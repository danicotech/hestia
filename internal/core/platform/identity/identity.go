// Package identity 是身分層的契約:OAuth2 登入、provider 綁定、登入 session。
//
// 設計核心(schemas/02):登入 = OAuth2(第一個 provider 是 Discord),
// **內部自生 user id 是唯一權威**,provider 帳號只是掛在上面的憑證。無密碼登入。
//
// session 模型(schemas/02 增補 F)是雙 token:
//   - access token:15 分鐘、無狀態簽章、驗證不打 DB(讀取路徑不多一次往返)
//   - refresh token:30 天、只以 SHA-256 存在 platform.sessions,每次使用即輪替
//
// 撤銷延遲上限 = access token 壽命(15 分鐘),這是刻意取捨。
//
// 本套件只認標準庫(契約層鐵則);Postgres 實作在 storage/identitypg。
package identity

import (
	"context"
	"errors"
	"time"
)

// Provider 是外部身分來源。M1 只有 Discord,路線圖有 Twitch / YouTube。
type Provider string

// ProviderDiscord 是 M1 唯一啟用的 provider。
const ProviderDiscord Provider = "discord"

// AccessTokenTTL / RefreshTokenTTL / StateTTL 是 schemas/02 增補 F 定的壽命。
// 公開成常數是為了讓呼叫端(cookie Max-Age、前端排程換發)有唯一權威可讀,
// 不要各自寫死一份。
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 30 * 24 * time.Hour
	StateTTL        = 10 * time.Minute
)

// RevokeReason 是 sessions.revoked_reason 的封閉枚舉(migration 00021 的註解即權威)。
type RevokeReason string

const (
	// RevokeLogout 是使用者主動登出這台裝置。
	RevokeLogout RevokeReason = "logout"
	// RevokeRotated 是正常輪替:舊 refresh token 已被新的取代。
	RevokeRotated RevokeReason = "rotated"
	// RevokeAdminBan 是管理端封鎖時全撤。
	RevokeAdminBan RevokeReason = "admin_ban"
	// RevokeReuseDetected 是偵測到已輪替的 token 再度被使用(token 被竊)。
	RevokeReuseDetected RevokeReason = "reuse_detected"
	// RevokeAccountDeleted 是帳號軟刪除時一併撤掉所有裝置。
	RevokeAccountDeleted RevokeReason = "account_deleted"
)

// Valid 回報 reason 是否在枚舉內(與 migration 00021 的 CHECK 值域一致)。
func (r RevokeReason) Valid() bool {
	switch r {
	case RevokeLogout, RevokeRotated, RevokeAdminBan, RevokeReuseDetected, RevokeAccountDeleted:
		return true
	}
	return false
}

// DeviceInfo 是發 session 當下的裝置足跡,只作裝置清單顯示與稽核,不參與任何判定。
type DeviceInfo struct {
	UserAgent string
	IP        string
}

// Session 是一次登入/換發的結果。
//
// RefreshToken 是**唯一一次**能拿到明文的機會(DB 只有 SHA-256);
// 呼叫端負責放進 HttpOnly cookie,之後就再也取不回來。
type Session struct {
	UserID          int64  // 內部權威 id,絕不對外
	UserPublicID    string // ULID,對外用
	SessionPublicID string // ULID,裝置清單與「登出這台」用
	AccessToken     string
	RefreshToken    string
	AccessExpiresAt time.Time
	ExpiresAt       time.Time // refresh token 到期
	// IsNewUser 為 true 表示這次登入建立了新使用者(並已發註冊禮)。
	IsNewUser bool
	// Redirect 是 StartLogin 當時帶進 state 的相對路徑,已驗證過不是絕對 URL。
	Redirect string
}

// Device 是裝置清單的一列(未撤銷且未過期的 session)。
type Device struct {
	PublicID   string
	UserAgent  string
	IP         string
	IssuedAt   time.Time
	LastUsedAt *time.Time
	ExpiresAt  time.Time
}

// 錯誤語意:呼叫端據此決定回 401 / 400 還是「請重新登入」。
//
// 這些錯誤的訊息永遠不含 token、secret 或 authorization code——
// 錯誤字串會進 log、進 APM、可能被回給客戶端,任何一處外洩都等於憑證外洩。
var (
	// ErrInvalidToken 是簽章不符、格式錯誤、版本前綴不對。
	ErrInvalidToken = errors.New("token 無效")
	// ErrTokenExpired 是簽章正確但已過期。
	ErrTokenExpired = errors.New("token 已過期")
	// ErrInvalidState 是 OAuth state 無效(CSRF 防線)。
	ErrInvalidState = errors.New("OAuth state 無效")
	// ErrStateExpired 是 OAuth state 已過期(10 分鐘)。
	ErrStateExpired = errors.New("OAuth state 已過期")
	// ErrStateMismatch 是回呼帶的 state 與該瀏覽器 cookie 裡的不符(或 cookie 缺失)。
	// 這是 login CSRF / session fixation 的防線:簽章只證明 state 是我們簽的,
	// 這個比對才證明它屬於**發起這次請求的那個瀏覽器**。
	ErrStateMismatch = errors.New("OAuth state 與瀏覽器 cookie 不符")
	// ErrInvalidRedirect 是登入後導回路徑不是相對路徑(open redirect 防線)。
	ErrInvalidRedirect = errors.New("導回路徑必須是站內相對路徑")
	// ErrProviderExchange 是向 provider 換 token / 取使用者資料失敗。
	ErrProviderExchange = errors.New("向身分提供者換取憑證失敗")
	// ErrSessionNotFound 是 refresh token 查無對應 session。
	ErrSessionNotFound = errors.New("session 不存在")
	// ErrSessionRevoked 是 session 已被撤銷(登出 / 封鎖 / 重用偵測)。
	ErrSessionRevoked = errors.New("session 已撤銷")
	// ErrSessionExpired 是 session 已逾 30 天。
	ErrSessionExpired = errors.New("session 已過期")
	// ErrTokenReuseDetected 是已輪替的 refresh token 再度被使用:
	// 整條輪替鏈已全部撤銷,呼叫端應要求使用者重新登入。
	ErrTokenReuseDetected = errors.New("refresh token 重用,該裝置的 session 已全部撤銷")
	// ErrAccountDeleted 是該 provider 帳號綁在已軟刪除的使用者上。
	ErrAccountDeleted = errors.New("帳號已刪除")
	// ErrInvalidConfig 是經濟設定缺失或不合法(如 signup_bonus)。
	ErrInvalidConfig = errors.New("經濟設定不合法")
)

// Service 是身分層的對外介面。
//
// 為什麼 Authenticate 不打 DB:雙 token 模型的重點就在這裡——
// 每個請求都查一次 session 表會讓所有讀取路徑多一次往返,
// 代價是撤銷有最多 15 分鐘的延遲(schemas/02 增補 F 已接受)。
type Service interface {
	// StartLogin 產生 provider 的 authorize URL 與無狀態 CSRF state。
	// redirectPath 必須是站內相對路徑,否則回 ErrInvalidRedirect。
	StartLogin(redirectPath string) (authorizeURL, state string, err error)

	// CompleteLogin 驗 state、用 code 換 provider 憑證、建立或更新身分綁定,
	// 並在同一個 transaction 內發 session(新使用者另發註冊禮)。
	//
	// stateFromCookie 是入口層從 HttpOnly + SameSite=Lax cookie 讀回的同一個 state,
	// 必填且必須與 state 相符,否則回 ErrStateMismatch(見 Signer.VerifyState)。
	CompleteLogin(ctx context.Context, code, state, stateFromCookie string, dev DeviceInfo) (*Session, error)

	// Refresh 以 refresh token 換一組新的 access + refresh(輪替)。
	// 已輪替過的 token 再度被使用會撤銷整條鏈並回 ErrTokenReuseDetected。
	Refresh(ctx context.Context, refreshToken string, dev DeviceInfo) (*Session, error)

	// Authenticate 純簽章驗證 access token,回內部 user id。不打 DB。
	Authenticate(ctx context.Context, accessToken string) (userID int64, err error)

	// Logout 撤銷指定裝置的 session。userID 是防越權的必要條件,不是多餘參數。
	Logout(ctx context.Context, sessionPublicID string, userID int64) error

	// RevokeAllForUser 撤銷該使用者所有未撤銷的 session(封鎖、改密、疑似外洩用)。
	RevokeAllForUser(ctx context.Context, userID int64, reason RevokeReason) (int, error)

	// ListDevices 列出未撤銷且未過期的 session。
	ListDevices(ctx context.Context, userID int64) ([]Device, error)
}
