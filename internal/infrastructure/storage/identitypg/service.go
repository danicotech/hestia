// Package identitypg 是 identity.Service 的 Postgres 實作:
// Discord OAuth2 登入、身分綁定、登入 session 的簽發與輪替。
//
// 一次首登的全部效果——users、identities、user_privacy_settings、
// token_entries + user_balances(註冊禮)、sessions——都在同一個 transaction,
// 要嘛都在要嘛都不在。
package identitypg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// currency 是註冊禮發放的幣別。M1 只有 coin(schemas/04)。
const currency = "coin"

// refreshTokenBytes 是 refresh token 的熵。32 bytes = 256 bit,
// 與 SHA-256 同級;比對用的是雜湊,明文只在回應中出現一次。
const refreshTokenBytes = 32

// userAgentMaxLen 是 user_agent 的截斷長度(schemas/02:裝置清單顯示用,200 字元)。
const userAgentMaxLen = 200

// revokeChainTimeout 是重用偵測後撤鏈的自有時限。撤鏈不跟隨呼叫端 ctx,
// 但也不能無限期佔著連線。
const revokeChainTimeout = 10 * time.Second

// txLedger 是本服務對帳本的最小依賴:註冊禮與建帳同 tx。
// 實際注入 *ledgerpg.Service(ApplyInTx 刻意不在 ledger.Ledger interface 上)。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// Config 是身分服務的組裝參數。
//
// client_id / secret / redirect_uri / 金鑰全部由**建構子注入**,
// 套件內不讀環境變數:讀 env 的位置只有 cmd/server 一處,
// 測試才能不靠環境就跑,也不會有第二份「設定從哪來」的答案。
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	// Scopes 預設 ["identify"];要拿 email 就加 "email"。
	Scopes []string
	// Endpoints 預設 DiscordEndpoints();測試指向 httptest.Server。
	Endpoints Endpoints
	// HTTPClient 預設 10 秒逾時的 client(絕不用 http.DefaultClient:它沒有逾時)。
	HTTPClient *http.Client
	// TokenEncKey 是 identities.*_token_enc 的 AES-256 金鑰,必須 32 bytes
	// (PLATFORM_TOKEN_ENC_KEY)。
	TokenEncKey []byte
	// Logger 預設 slog.Default();目前只用來記「偵測到重用但撤鏈失敗」這個安全事件。
	Logger *slog.Logger
}

// Service 是 identity.Service 的 Postgres 實作。
type Service struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	led    txLedger
	signer *identity.Signer
	prov   oauthProvider
	enc    *tokenCipher
	log    *slog.Logger
}

var _ identity.Service = (*Service)(nil)

// New 建立身分服務。signer 由 PLATFORM_JWT_SECRET 建立(identity.NewSigner)。
func New(pool *pgxpool.Pool, led txLedger, signer *identity.Signer, cfg Config) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("pool 必填")
	case led == nil:
		return nil, errors.New("ledger 必填")
	case signer == nil:
		return nil, errors.New("signer 必填")
	case cfg.ClientID == "":
		return nil, errors.New("ClientID 必填")
	case cfg.ClientSecret == "":
		return nil, errors.New("ClientSecret 必填")
	case cfg.RedirectURI == "":
		return nil, errors.New("RedirectURI 必填")
	}
	enc, err := newTokenCipher(cfg.TokenEncKey)
	if err != nil {
		return nil, err
	}
	ep := cfg.Endpoints
	if ep.Authorize == "" {
		ep = DiscordEndpoints()
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"identify"}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		pool:   pool,
		q:      db.New(pool),
		led:    led,
		signer: signer,
		enc:    enc,
		log:    logger,
		prov: &discordProvider{
			clientID:     cfg.ClientID,
			clientSecret: cfg.ClientSecret,
			redirectURI:  cfg.RedirectURI,
			scopes:       scopes,
			ep:           ep,
			hc:           hc,
		},
	}, nil
}

// StartLogin 產生 authorize URL 與無狀態 CSRF state。
func (s *Service) StartLogin(redirectPath string) (string, string, error) {
	state, err := s.signer.SignState(redirectPath, time.Now())
	if err != nil {
		return "", "", err
	}
	return s.prov.AuthorizeURL(state), state, nil
}

// CompleteLogin 完成 OAuth 回呼。
//
// 順序即安全性:先驗 state(含與瀏覽器 cookie 的比對,見 Signer.VerifyState)
// → 才拿 code 去換 token。反過來的話,攻擊者能用偽造的回呼讓我們對 provider 發請求。
//
// stateFromCookie 由入口層從 HttpOnly + SameSite=Lax cookie 讀出;
// 它是 login CSRF / session fixation 的唯一防線,缺了就一律拒絕。
func (s *Service) CompleteLogin(
	ctx context.Context, code, state, stateFromCookie string, dev identity.DeviceInfo,
) (*identity.Session, error) {
	st, err := s.signer.VerifyState(state, stateFromCookie, time.Now())
	if err != nil {
		return nil, err
	}
	if code == "" {
		return nil, fmt.Errorf("%w: 缺 code", identity.ErrProviderExchange)
	}

	tok, err := s.prov.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	pu, err := s.prov.Me(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}

	// 併發首登(同一 Discord 帳號、兩個不同 code)時,兩個 tx 都會走「建新使用者」路徑,
	// 但 UNIQUE(provider, provider_user_id) 只讓一個成立;輸的那個整個 tx 回滾
	// (連同它剛建的 user 與註冊禮),重跑一次就會走到「既有綁定」路徑。
	// 「一個 Discord 帳號只對應一個 user、註冊禮只發一次」的權威就是這個唯一鍵。
	for attempt := 0; ; attempt++ {
		sess, err := s.completeLoginTx(ctx, tok, pu, dev)
		if err != nil {
			if attempt == 0 && isConstraintViolation(err, "identities_provider_provider_user_id_key") {
				continue
			}
			return nil, err
		}
		sess.Redirect = st.Redirect
		return sess, nil
	}
}

// completeLoginTx 是 CompleteLogin 的單一 transaction 部分。
func (s *Service) completeLoginTx(
	ctx context.Context, tok *providerToken, pu *providerUser, dev identity.DeviceInfo,
) (*identity.Session, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	var (
		userID int64
		isNew  bool
	)
	row, err := qtx.GetLoginIdentity(ctx, db.GetLoginIdentityParams{
		Provider:       string(identity.ProviderDiscord),
		ProviderUserID: pu.ID,
	})
	switch {
	case err == nil:
		if row.UserDeletedAt != nil {
			return nil, identity.ErrAccountDeleted
		}
		userID = row.UserID
		accessEnc, refreshEnc, err := s.encryptProviderTokens(userID, pu.ID, tok)
		if err != nil {
			return nil, err
		}
		if err := qtx.UpdateLoginIdentity(ctx, db.UpdateLoginIdentityParams{
			ID:              row.IdentityID,
			Username:        strPtr(pu.Username),
			AccessTokenEnc:  accessEnc,
			RefreshTokenEnc: refreshEnc,
			Scope:           strPtr(tok.Scope),
			ExpiresAt:       tok.ExpiresAt,
		}); err != nil {
			return nil, fmt.Errorf("更新身分綁定: %w", err)
		}
		if err := qtx.TouchLoginUser(ctx, userID); err != nil {
			return nil, fmt.Errorf("更新 last_seen_at: %w", err)
		}

	case errors.Is(err, pgx.ErrNoRows):
		isNew = true
		pid, err := ulid.New()
		if err != nil {
			return nil, err
		}
		u, err := qtx.InsertLoginUser(ctx, db.InsertLoginUserParams{
			PublicID:    pid,
			DisplayName: strPtr(pu.Username),
			AvatarUrl:   strPtr(pu.AvatarURL),
			Email:       strPtr(pu.Email),
		})
		if err != nil {
			return nil, fmt.Errorf("建立使用者: %w", err)
		}
		userID = u.ID
		accessEnc, refreshEnc, err := s.encryptProviderTokens(userID, pu.ID, tok)
		if err != nil {
			return nil, err
		}

		// 唯一鍵閘門放在最前面(在動錢之前):撞鍵時該回滾的東西越少越好。
		if _, err := qtx.InsertLoginIdentity(ctx, db.InsertLoginIdentityParams{
			UserID:          userID,
			Provider:        string(identity.ProviderDiscord),
			ProviderUserID:  pu.ID,
			Username:        strPtr(pu.Username),
			AccessTokenEnc:  accessEnc,
			RefreshTokenEnc: refreshEnc,
			Scope:           strPtr(tok.Scope),
			ExpiresAt:       tok.ExpiresAt,
		}); err != nil {
			return nil, fmt.Errorf("建立身分綁定: %w", err)
		}
		if err := qtx.EnsurePrivacySettings(ctx, userID); err != nil {
			return nil, fmt.Errorf("建立隱私設定: %w", err)
		}
		if err := s.grantSignupBonus(ctx, tx, qtx, userID); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("查身分綁定: %w", err)
	}

	sess, err := s.issueSession(ctx, qtx, userID, dev, nil)
	if err != nil {
		return nil, err
	}
	sess.IsNewUser = isNew

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return sess, nil
}

// encryptProviderTokens 把 provider 憑證加密,AAD 綁在這個身分列上
// (見 tokenAAD:密文被搬到別的 identity 列就解不開)。
func (s *Service) encryptProviderTokens(userID int64, providerUserID string, tok *providerToken) (accessEnc, refreshEnc []byte, err error) {
	aad := tokenAAD(userID, string(identity.ProviderDiscord), providerUserID)
	if accessEnc, err = s.enc.encrypt(tok.AccessToken, aad); err != nil {
		return nil, nil, err
	}
	if refreshEnc, err = s.enc.encrypt(tok.RefreshToken, aad); err != nil {
		return nil, nil, err
	}
	return accessEnc, refreshEnc, nil
}

// grantSignupBonus 發註冊禮,與建帳同 tx(ledger-invariants 第二條)。
//
// 冪等鍵用決定性的 signup:{userID}:防重的**權威是 UNIQUE(provider, provider_user_id)**
// ——併發首登時輸的那個 tx 連同它的分錄一起回滾,所以一個 Discord 帳號只會拿到一次。
// 這個 key 只是滿足 Ledger 介面的必填欄位(users.id 由 IDENTITY 產生、永不重用,
// 同一個 user 必然同 key 同 hash),不構成第二套檢查(專案第 9 條)。
func (s *Service) grantSignupBonus(ctx context.Context, tx pgx.Tx, qtx *db.Queries, userID int64) error {
	bonus, err := configInt(ctx, qtx, "signup_bonus")
	if err != nil {
		return err
	}
	if bonus < 0 {
		return fmt.Errorf("signup_bonus=%d 不可為負: %w", bonus, identity.ErrInvalidConfig)
	}
	if bonus == 0 {
		return nil // 設成 0 = 明確關閉註冊禮,不寫金額為 0 的分錄(帳本不接受 0)
	}
	key := fmt.Sprintf("signup:%d", userID)
	res, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: key,
		RequestHash:    key,
		Ops: []ledger.Op{{
			UserID:   userID,
			Currency: currency,
			Amount:   bonus,
			Reason:   ledger.ReasonSignupBonus,
		}},
	})
	if err != nil {
		return fmt.Errorf("發註冊禮: %w", err)
	}
	// user 是本 tx 剛建的、id 不可能被重用,帳本卻說這個 key 已有結果 ⇒ 狀態矛盾。
	// 回成功但錢沒動是最壞結果,必須失敗出聲讓整個 tx 回滾。
	if res.Replayed {
		return fmt.Errorf("key=%s 帳本已有結果但使用者是新建的: %w", key, identity.ErrInvalidConfig)
	}
	return nil
}

// Refresh 以 refresh token 換一組新的 access + refresh。
func (s *Service) Refresh(ctx context.Context, refreshToken string, dev identity.DeviceInfo) (*identity.Session, error) {
	if refreshToken == "" {
		return nil, identity.ErrSessionNotFound
	}
	hash := hashRefreshToken(refreshToken)

	sess, err := s.rotate(ctx, hash, dev)
	var reuse *reuseError
	if errors.As(err, &reuse) {
		// 撤鏈**不能**跟著呼叫端的 ctx 一起被取消:最容易觸發重用偵測的場景
		// (行動網路換手、回應遺失後重送)正是 ctx 最容易被取消的場景,
		// 客戶端送完就斷線就會變成「偵測到了,鏈卻還活著」。
		// 先讓 rotate 的 tx 回滾(defer 已執行)再撤鏈:兩者都要拿同一把 advisory lock。
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeChainTimeout)
		defer cancel()
		if rerr := s.revokeChain(revokeCtx, reuse.userID, reuse.sessionID); rerr != nil {
			// 撤鏈失敗要吵,但**絕不**把安全事件降級成普通 DB 錯誤:
			// 回傳值必須讓呼叫端知道「這個 token 被重用了,請重新登入」。
			s.log.Error("refresh token 重用:撤銷輪替鏈失敗,鏈可能仍有效",
				"user_id", reuse.userID, "session_id", reuse.sessionID, "err", rerr)
		}
		return nil, identity.ErrTokenReuseDetected
	}
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// reuseError 是內部訊號:偵測到 refresh token 重用,帶著撤鏈需要的座標。
// 訊息刻意不含任何 token。
type reuseError struct {
	userID    int64
	sessionID int64
}

func (e *reuseError) Error() string { return "refresh token 重用" }

// rotate 是輪替的單一 transaction 部分。
//
// 「一個 session 只能被輪替一次」的**權威是 sessions_rotated_from_uq**,不是前置讀取:
// 併發下先查後寫必然有窗口,唯一索引沒有。撞鍵 = 同一個 refresh token 被用了第二次。
func (s *Service) rotate(ctx context.Context, hash []byte, dev identity.DeviceInfo) (*identity.Session, error) {
	pre, err := s.q.GetSessionByRefreshHash(ctx, hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, identity.ErrSessionNotFound
	case err != nil:
		return nil, fmt.Errorf("查 session: %w", err)
	}
	if pre.RevokedAt != nil {
		if pre.RevokedReason != nil && *pre.RevokedReason == string(identity.RevokeRotated) {
			return nil, &reuseError{userID: pre.UserID, sessionID: pre.ID}
		}
		return nil, identity.ErrSessionRevoked
	}
	if pre.IsExpired {
		return nil, identity.ErrSessionExpired
	}
	// 軟刪除的帳號其 session 仍在表裡且未過期,DB 約束表達不了跨表條件——
	// 這個檢查漏掉就是「已刪除的帳號可以無限續期」。
	if pre.UserDeletedAt != nil {
		return nil, identity.ErrAccountDeleted
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	sess, err := s.issueSession(ctx, qtx, pre.UserID, dev, &pre.ID)
	if err != nil {
		if isConstraintViolation(err, "sessions_rotated_from_uq") {
			return nil, &reuseError{userID: pre.UserID, sessionID: pre.ID}
		}
		return nil, err
	}

	// 舊列標 rotated;0 列 = 前置讀取之後它被登出/封鎖撤銷了,
	// 整個 tx 必須回滾——新 session 不能從一條已死的鏈上長出來。
	n, err := qtx.MarkSessionRotated(ctx, pre.ID)
	if err != nil {
		return nil, fmt.Errorf("標記舊 session: %w", err)
	}
	if n == 0 {
		return nil, identity.ErrSessionRevoked
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return sess, nil
}

// revokeChain 沿輪替鏈把整台裝置**還活著的** session 全撤(reuse_detected)。
// 複合 FK 保證鏈不跨使用者,所以「整條鏈」就是「這台裝置」;
// CHECK(rotated_from < id)保證鏈不可能成環,沿鏈走訪必終止。
//
// 已撤銷的列維持原本的 reason(撤銷不可逆,見 migration 00021 的 trigger):
// 它們早已失效,安全效果不變,而保留「當時為什麼被撤」才答得出事後的稽核問題。
func (s *Service) revokeChain(ctx context.Context, userID, sessionID int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.LockUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("取 session 鎖: %w", err)
	}
	ids, err := qtx.SessionChainIDs(ctx, sessionID) // 升冪
	if err != nil {
		return fmt.Errorf("展開輪替鏈: %w", err)
	}
	if len(ids) == 0 {
		return tx.Commit(ctx)
	}
	if _, err := qtx.RevokeSessionsByID(ctx, db.RevokeSessionsByIDParams{
		Ids:           ids,
		RevokedReason: string(identity.RevokeReuseDetected),
	}); err != nil {
		return fmt.Errorf("撤銷輪替鏈: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Authenticate 驗證 access token 並回傳內部 user id。
//
// **不查 sessions 表**——這正是雙 token 模型的重點:撤銷有最多一個 access token
// 壽命(15 分鐘)的延遲,換來每個請求少一次 session 往返(schemas/02 增補 F 已接受)。
//
// 仍需一次 users 的唯一索引查詢:token 的 sub 是 public_id 而非內部 BIGINT id
// (專案鐵則 5——進了 token 就等於進了對外契約)。這次查詢順帶讓軟刪除的帳號
// 立刻失效,不必等 token 過期。
func (s *Service) Authenticate(ctx context.Context, accessToken string) (int64, error) {
	claims, err := s.signer.VerifyAccess(accessToken, time.Now())
	if err != nil {
		return 0, err
	}
	userID, err := s.q.GetUserIDByPublicID(ctx, claims.Sub)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, identity.ErrAccountDeleted
	}
	if err != nil {
		return 0, fmt.Errorf("解析使用者: %w", err)
	}
	return userID, nil
}

// Logout 撤銷指定裝置。
//
// 撤的是**整條輪替鏈**而不是 public_id 對應的那一列:裝置清單上拿到的 public_id
// 只要經過一次背景 refresh 就會變成已 rotated 的舊列,單撤它等於什麼都沒做,
// 而該裝置靠輪替出來的新 token 繼續有效。「一台裝置 = 一條鏈」是本表既有的表達,
// 裝置級撤銷(登出、重用偵測)一律走它。
func (s *Service) Logout(ctx context.Context, sessionPublicID string, userID int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.LockUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("取 session 鎖: %w", err)
	}
	// 越權防線在這裡:public_id 必須同時屬於這個 user_id 才解得出 id。
	sessionID, err := qtx.GetSessionIDByPublicID(ctx, db.GetSessionIDByPublicIDParams{
		PublicID: sessionPublicID,
		UserID:   userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 不存在、或不屬於這個使用者——一律回同一個錯誤,
		// 不讓呼叫端據此探測別人的 session public_id 是否存在。
		return identity.ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("查 session: %w", err)
	}

	ids, err := qtx.SessionChainIDs(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("展開輪替鏈: %w", err)
	}
	n, err := qtx.RevokeSessionsByID(ctx, db.RevokeSessionsByIDParams{
		Ids:           ids,
		RevokedReason: string(identity.RevokeLogout),
	})
	if err != nil {
		return fmt.Errorf("撤銷 session: %w", err)
	}
	if n == 0 {
		return identity.ErrSessionNotFound // 整條鏈都已撤銷:重複登出
	}
	return tx.Commit(ctx)
}

// RevokeAllForUser 撤銷該使用者所有未撤銷的 session(封鎖 / 刪帳號 / 疑似外洩)。
func (s *Service) RevokeAllForUser(ctx context.Context, userID int64, reason identity.RevokeReason) (int, error) {
	if !reason.Valid() {
		return 0, fmt.Errorf("撤銷理由 %q 不在枚舉內: %w", reason, identity.ErrInvalidConfig)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// 先拿 advisory lock 再列出:否則「列出之後、更新之前」併發輪替出來的新 session
	// 會躲過這次全撤(封鎖漏一台裝置就等於沒封鎖)。
	if err := qtx.LockUserSessions(ctx, userID); err != nil {
		return 0, fmt.Errorf("取 session 鎖: %w", err)
	}
	ids, err := qtx.ListActiveSessionIDs(ctx, userID) // 升冪
	if err != nil {
		return 0, fmt.Errorf("列出有效 session: %w", err)
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}
	n, err := qtx.RevokeSessionsByID(ctx, db.RevokeSessionsByIDParams{
		Ids:           ids,
		RevokedReason: string(reason),
	})
	if err != nil {
		return 0, fmt.Errorf("撤銷 session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int(n), nil
}

// ListDevices 列出未撤銷且未過期的 session。
func (s *Service) ListDevices(ctx context.Context, userID int64) ([]identity.Device, error) {
	rows, err := s.q.ListUserDevices(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("列出裝置: %w", err)
	}
	out := make([]identity.Device, 0, len(rows))
	for _, r := range rows {
		d := identity.Device{
			PublicID:   r.PublicID,
			IP:         r.Ip,
			IssuedAt:   r.IssuedAt,
			LastUsedAt: r.LastUsedAt,
			ExpiresAt:  r.ExpiresAt,
		}
		if r.UserAgent != nil {
			d.UserAgent = *r.UserAgent
		}
		out = append(out, d)
	}
	return out, nil
}

// issueSession 在給定 tx 內發一組新 session。
//
// refresh token:32 bytes crypto/rand,**只有雜湊進 DB**(DB 外洩 ≠ session 外洩),
// 明文只在這個回傳值裡出現一次,之後永遠取不回來。
func (s *Service) issueSession(
	ctx context.Context, qtx *db.Queries,
	userID int64, dev identity.DeviceInfo, rotatedFrom *int64,
) (*identity.Session, error) {
	if err := qtx.LockUserSessions(ctx, userID); err != nil {
		return nil, fmt.Errorf("取 session 鎖: %w", err)
	}
	// 取鎖**之後**一律重讀(GetUserPublicID 帶 deleted_at IS NULL),不因為
	// 「呼叫端剛剛查過」就跳過:那個查詢在取鎖之前,中間足夠讓另一個 tx
	// commit 軟刪除,然後我們就會替一個已刪除的帳號發出 30 天的憑證。
	userPublicID, err := qtx.GetUserPublicID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, identity.ErrAccountDeleted
	}
	if err != nil {
		return nil, fmt.Errorf("讀使用者 public_id: %w", err)
	}

	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand 失敗絕不降級:可預測的 refresh token 等於沒有 refresh token。
		return nil, fmt.Errorf("讀亂數: %w", err)
	}
	refreshToken := base64.RawURLEncoding.EncodeToString(raw)

	sessionPublicID, err := ulid.New()
	if err != nil {
		return nil, err
	}
	ua, ip := deviceParams(dev)
	row, err := qtx.InsertSession(ctx, db.InsertSessionParams{
		PublicID:         sessionPublicID,
		UserID:           userID,
		RefreshTokenHash: hashRefreshToken(refreshToken),
		TtlDays:          int32(identity.RefreshTokenTTL / (24 * time.Hour)),
		UserAgent:        ua,
		Ip:               ip,
		RotatedFrom:      rotatedFrom,
	})
	if err != nil {
		return nil, fmt.Errorf("建立 session: %w", err) // 撞唯一鍵由呼叫端判讀
	}

	access, accessExp, err := s.signer.SignAccess(userPublicID, row.PublicID, time.Now())
	if err != nil {
		return nil, err
	}
	return &identity.Session{
		UserID:          userID,
		UserPublicID:    userPublicID,
		SessionPublicID: row.PublicID,
		AccessToken:     access,
		RefreshToken:    refreshToken,
		AccessExpiresAt: accessExp,
		ExpiresAt:       row.ExpiresAt,
	}, nil
}

// hashRefreshToken 是 refresh_token_hash 的唯一算法:對**呈現形式**(base64url 字串)
// 取 SHA-256。固定用字串而非解碼後的位元組,避免 padding 變體造成同一 token 兩個雜湊。
func hashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// deviceParams 把裝置足跡整成可入庫的形態:user_agent 截斷、ip 正規化。
// 兩者都是顯示與稽核用,任何一個不合法都只是存 NULL,絕不因此拒絕登入。
func deviceParams(dev identity.DeviceInfo) (*string, *netip.Addr) {
	var ua *string
	if dev.UserAgent != "" {
		r := []rune(dev.UserAgent)
		if len(r) > userAgentMaxLen {
			r = r[:userAgentMaxLen]
		}
		s := string(r)
		ua = &s
	}
	return ua, parseIP(dev.IP)
}

// parseIP 接受純 IP 或 "host:port"(net/http 的 RemoteAddr 是後者)。
func parseIP(raw string) *netip.Addr {
	if raw == "" {
		return nil
	}
	if a, err := netip.ParseAddr(raw); err == nil {
		a = a.Unmap()
		return &a
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			a = a.Unmap()
			return &a
		}
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// configInt 讀 economy_configs 目前生效值(effective_at <= now() 最新一筆)。
// 值不存在或不是整數都是部署錯誤,直接回傳錯誤,不用預設值掩蓋。
func configInt(ctx context.Context, qtx *db.Queries, key string) (int64, error) {
	raw, err := qtx.GetCurrentConfig(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("讀設定 %s: %w", key, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("設定 %s 不是整數 (%s): %w", key, raw, err)
	}
	return v, nil
}

// isConstraintViolation 只認指定名稱的唯一約束:
// 未來新增別的 UNIQUE 時撞到不會被誤判成「已存在的綁定」或「token 重用」。
func isConstraintViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
