package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// Service 簽發與驗證選手 session。
//
// 相依只有兩個:簽章器與一個查 passcode_issued_at 的 port。
// 這裡沒有任何自己的狀態 —— session 不落儲存(見套件註解),
// 所以同一個 Service 可以被所有請求共用。
type Service struct {
	signer *identity.Signer
	repo   Repo
	now    func() time.Time
}

// Option 用來替換預設相依,目前只有時鐘(測試要能把時間推過到期點)。
type Option func(*Service)

// WithClock 換掉時鐘。
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New 建立 Service。
//
// signer 刻意是 **identity.Signer 的具體型別**而不是一個本地介面:
// 簽章格式只該有一個權威(鐵則 9),而假的簽章器能讓測試全綠卻在正式環境
// 簽出驗不過的 token。真正需要被替換的相依是資料庫與時鐘,那兩個才是 port。
func New(signer *identity.Signer, repo Repo, opts ...Option) *Service {
	s := &Service{signer: signer, repo: repo, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Issue 簽發一張選手 session,回傳 token 與到期時間。
//
// 會多打一次資料庫讀 passcode_issued_at。這是划算的:登入一個晚上只發生
// 一次,而換來的是每一張 token 都帶著「我是哪一輪換發之後發出來的」這個事實。
//
// # 一個已知且刻意接受的窗口
//
// 呼叫端的流程是「signup.Login 查選手 → session.Issue 讀 pat」,兩者之間
// 裁判剛好按下重新產生的話,這張 token 會帶著**新**的 pat —— 裁判以為自己把
// 這個人踢光了,卻正好發了一張活的給他。窗口是那兩次查詢之間的幾毫秒,
// 而且要求對方恰好在裁判按鈕的那一瞬間完成登入。
//
// 能關掉它的唯一辦法是讓登入與簽發共用同一次讀取(把 pat 從 Login 的結果
// 一路傳到這裡),但那要求 transport 的 ActivitySessions port 多帶一個
// 活動層專屬的欄位 —— 為了幾毫秒的窗口,把「簽發 session」這個介面綁死在
// 「passcode_issued_at」這個實作細節上,不划算。真的要確保踢乾淨,
// 裁判還有 WithdrawPlayer(擋住之後的登入)可以一起用。
// 寫在這裡是為了讓下一個人知道這是判斷過的取捨,不是漏掉的。
func (s *Service) Issue(ctx context.Context, id Identity) (string, time.Time, error) {
	if !id.Valid() {
		return "", time.Time{}, fmt.Errorf("%w: 缺 slug 或選手 public_id", ErrInvalidIdentity)
	}
	issuedAt, err := s.passcodeIssuedAt(ctx, id)
	if err != nil {
		return "", time.Time{}, err
	}

	now := s.now()
	// claims 存的是秒,回傳值就用同一個秒 —— 兩邊各自取一次會差幾奈秒,
	// 而那個差值正是「cookie 還在但 token 已死」的窗口。
	exp := time.Unix(now.Add(TTL).Unix(), 0)
	tok, err := s.signer.Sign(TokenType, claims{
		Tnt: id.TournamentSlug,
		Plr: id.PlayerPublicID,
		Pat: issuedAt.UnixMicro(),
		Iat: now.Unix(),
		Exp: exp.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// Verify 驗證 token 並取回身分。
//
// 順序是有意義的,每一步都比下一步便宜:
//
//  1. 簽章(identity.Verify:長度上限 → 版本 → 常數時間比簽章 → 才解析)
//  2. 到期
//  3. claims 完整性
//  4. 才打資料庫對 passcode_issued_at
//
// 資料庫排在最後,是因為這支函式跑在**未認證**的請求上:順序反過來的話,
// 任何人送一串垃圾字串都能叫我們查一次資料庫。
//
// # 第 4 步就是「換發通行碼即失效」
//
// 裁判重新產生通行碼時,UPDATE 會同時改 passcode_hash 與 passcode_issued_at。
// 登入已經不比對雜湊了(2026-09-13 起只要遊戲ID),所以那個動作現在**全部**的
// 效果就在這裡:token 裡的 pat 是簽發當下的 issued_at,對不上現在這一列就當場失效。
//
// 比「相等」而不是「不大於」:換發只會讓時間往前走,但萬一資料被改回舊值
// (還原備份、手動改資料),那正是最需要保守的時候。相等的語意是
// 「這張 token 是現在這一輪換發之後發出來的」,沒有第二種解釋。
func (s *Service) Verify(ctx context.Context, token string) (Identity, error) {
	var c claims
	if err := s.signer.Verify(TokenType, token, &c); err != nil {
		return Identity{}, err
	}
	if s.now().Unix() >= c.Exp {
		return Identity{}, identity.ErrTokenExpired
	}
	id := Identity{TournamentSlug: c.Tnt, PlayerPublicID: c.Plr}
	if !id.Valid() || c.Pat == 0 {
		// 簽章是對的但內容不完整 —— 只可能是我們自己簽壞了。
		// 當成無效憑證處理:半個身分不該被任何地方當成「已登入」。
		return Identity{}, fmt.Errorf("claims 不完整: %w", identity.ErrInvalidToken)
	}

	issuedAt, err := s.passcodeIssuedAt(ctx, id)
	if err != nil {
		return Identity{}, err
	}
	if issuedAt.UnixMicro() != c.Pat {
		return Identity{}, ErrPasscodeRotated
	}
	return id, nil
}

// passcodeIssuedAt 是 Issue 與 Verify 共用的那一次查詢。
//
// 兩邊必須用完全相同的讀法與錯誤折法:一邊多做或少做一件事,
// 就會出現「發得出來卻驗不過」的 session。
func (s *Service) passcodeIssuedAt(ctx context.Context, id Identity) (time.Time, error) {
	t, err := s.repo.PasscodeIssuedAt(ctx, id.TournamentSlug, id.PlayerPublicID)
	if err != nil {
		if errors.Is(err, tournament.ErrPlayerNotFound) {
			// 賽事不存在與選手不存在折成同一個答案(見 Repo 的註解)。
			return time.Time{}, ErrPlayerGone
		}
		// 其他錯誤(資料庫掛了、逾時)原樣往上丟。
		// **絕不**折成憑證錯誤:那會讓一次故障表現成「全站選手被登出」,
		// 而所有人重新登入只會讓那個故障更嚴重。
		return time.Time{}, err
	}
	if t.IsZero() {
		return time.Time{}, ErrPasscodeTimeUnavailable
	}
	return t, nil
}
