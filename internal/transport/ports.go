package transport

import (
	"context"
	"errors"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/authz"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// 入口層自己的 port。定義在使用端(Go 慣例)有兩個好處:
//   - transport 不能 import infrastructure(depguard 把關),所以「public_id →
//     內部 id」「讀取列表」這些查詢一定要以介面注入;
//   - 實作換掉(in-process → HTTP client)時 handler 一行不改。
//
// 檢視型別(ProfileView / ItemView / …)放在 core/platform/readmodel,不放這裡:
// 實作在 storage,而 storage 不能 import transport,Go 介面又是名目型別比對——
// 型別放 core 兩邊才接得起來。
//
// 已經有 core 介面的能力(daily / shop / adminecon / ledger)一律直接用 core 的,
// 不在這裡重新發明;這裡只補 core 目前沒有的讀取側與身分解析。

var (
	// ErrNotFound 是 port 實作回報「查無此物」的統一訊號,映射成 connect NotFound。
	ErrNotFound = errors.New("找不到指定的資源")
	// ErrUnauthenticated 是「沒有有效身分」的統一訊號,映射成 connect Unauthenticated。
	ErrUnauthenticated = errors.New("未認證")
	// ErrPermissionDenied 是「身分有效但無權執行」的統一訊號,映射成 connect PermissionDenied。
	//
	// 別名而不是另一個 var:授權實作(core/platform/authz)也要能回這個訊號,
	// 而它不能 import transport。兩邊各宣告一個同義 sentinel 會讓 errors.Is
	// 靜靜地失敗——授權被拒會被當成未映射錯誤而回 Internal(鐵則 9)。
	ErrPermissionDenied = authz.ErrPermissionDenied
)

// Authenticator 把 Authorization: Bearer 的 token 換成內部 user id。
// 真實實作(session / JWT 驗證)由 identity 層提供並在 main 注入;
// transport 只認這個介面,不知道 token 長什麼樣。
//
// 註:identity.Service 的 Authenticate 簽名與此完全一致,可直接注入。
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (userID int64, err error)
}

// UnauthenticatedStub 是還沒接上 identity 前的預設值:一律拒絕。
// 「預設拒絕」是刻意的——沒接認證就開放存取是最糟的失敗模式。
type UnauthenticatedStub struct{}

// Authenticate 一律回 ErrUnauthenticated。
func (UnauthenticatedStub) Authenticate(context.Context, string) (int64, error) {
	return 0, ErrUnauthenticated
}

// Authorizer 判定已認證的使用者能不能執行某個 procedure(schemas/03-authz.md)。
// 未注入時,需要授權的 procedure(AdminEconomyService 全部)一律拒絕:
// 授權沒接上就把管理端開著,是比功能缺席嚴重得多的問題。
type Authorizer interface {
	Authorize(ctx context.Context, userID int64, procedure string) error
}

// Directory 把對外 public_id 解成內部 BIGINT id,以及反向取回 public_id。
//
// 為什麼需要它:專案鐵則 5 要求對外只出現 public_id,而 core 的 shop / adminecon
// 介面收的是內部 id。解析只有一個位置(這個 port),不散落在各 handler。
type Directory interface {
	UserID(ctx context.Context, publicID string) (int64, error)
	UserPublicID(ctx context.Context, userID int64) (string, error)
	EntitlementID(ctx context.Context, publicID string) (int64, error)
	RedemptionID(ctx context.Context, publicID string) (int64, error)
}

// ProfileReader 是「我的檔案」的讀取 port。
type ProfileReader interface {
	Profile(ctx context.Context, userID int64) (*readmodel.ProfileView, error)
	// Balances 一次回全部幣別的餘額(前端要一次顯示,不該打 N 次)。
	Balances(ctx context.Context, userID int64) ([]readmodel.BalanceView, error)
}

// ProfileWriter 是「我的檔案」的寫入 port。
//
// 與 ProfileReader 分開是刻意的:讀寫能力不同、風險不同,handler 宣告自己
// 需要哪一種就只拿哪一種。同一個實作可以同時滿足兩者,main 傳同一個值兩次即可。
type ProfileWriter interface {
	// SetTimezone 更新時區並回傳更新後的檔案。
	// 時區字串的合法性由實作驗證(DB 是唯一真實來源),入口層只擋空字串。
	SetTimezone(ctx context.Context, userID int64, timezone string) (*readmodel.ProfileView, error)
}

// Catalog 是商店與個人持有物的讀取側 port(core 目前只有寫入側介面)。
type Catalog interface {
	Items(ctx context.Context, includeDelisted bool) ([]readmodel.ItemView, error)
	Entitlements(ctx context.Context, userID int64, includeRevoked bool) ([]readmodel.EntitlementView, error)
	// Redemptions 依狀態過濾;status 為空字串 = 全部。
	Redemptions(ctx context.Context, userID int64, status shop.RedemptionStatus) ([]readmodel.RedemptionView, error)
}

// LedgerReader 是帳本的**管理端**讀取 port。
//
// 為什麼存在:AdminEconomyService.Refund 用 entry_ref 定址,但在這之前沒有任何
// RPC 產出 entry_ref——管理員只能去翻資料庫才叫得動退款,契約自相矛盾。
// 這個 port 補上缺口(對應 AdminEconomyService.ListEntries)。
//
// 實作提示:PageToken 請用 keyset 游標(created_at, id),不要 OFFSET——
// token_entries 是分區表且量大,OFFSET 分頁會越翻越慢。
type LedgerReader interface {
	ListEntries(ctx context.Context, q readmodel.LedgerEntriesQuery) (*readmodel.LedgerEntriesPage, error)
}

// SessionView 是一次登入產生的憑證。這是整個 transport 唯一碰到 token 的型別,
// event_logs 的摘要對 AuthService 一律不記任何欄位值。
type SessionView struct {
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
	UserPublicID          string
}

// DeviceInfo 是發 session 當下的裝置足跡,寫進 sessions.user_agent / ip。
//
// 用途只有兩個:裝置清單顯示與稽核。**不參與任何判定**——
// 拿 UA/IP 當安全條件是經典誤區(兩者都可偽造,而且行動網路的 IP 一直在變)。
//
// 兩個欄位都由入口層負責清理:UserAgent 截斷(client 可控字串),
// IP 預設取連線對端(不信任可偽造的 X-Forwarded-For,除非明確開啟)。
type DeviceInfo struct {
	UserAgent string
	IP        string
}

// AuthService 是 Discord OAuth 登入流程的 port。
// 未注入時 AuthService 的四個 RPC 一律回 Unimplemented。
//
// state 綁瀏覽器(login CSRF / session fixation 防線):StartDiscordLogin 回傳的
// state 由入口層寫進 HttpOnly cookie,回呼時原封不動一起交回來。
// **比對在實作內部做**(identity 層),入口層只負責搬運——
// 這樣「忘了比對」不可能發生,而不是靠每個呼叫端自律。
type AuthService interface {
	StartDiscordLogin(ctx context.Context, redirectURI string) (authorizeURL, state string, err error)
	// CompleteDiscordLogin 的 stateFromCookie 是瀏覽器 cookie 裡的值,
	// 可能為空(cookie 缺失)——**空值不代表跳過檢查**,實作必須照樣拒絕。
	// dev 只寫進 sessions 供裝置清單顯示,不參與任何判定。
	CompleteDiscordLogin(ctx context.Context, code, state, stateFromCookie string, dev DeviceInfo) (*SessionView, *readmodel.ProfileView, error)
	// RefreshSession 換發時同樣記錄裝置足跡(輪替後的 session 是新的一列)。
	RefreshSession(ctx context.Context, refreshToken string, dev DeviceInfo) (*SessionView, error)
	// Logout 撤銷 session。refreshToken 為空時撤銷 accessToken 對應的當前 session。
	//
	// 為什麼收 token 而不是 session public_id:AuthService 對認證攔截器整體豁免,
	// 所以這裡沒有「已驗證的 ctx」可取——而且**access token 過期時仍必須能登出**
	// (只帶 refresh token 那條路),要求先通過認證會把使用者鎖在登入狀態裡。
	// 實作端的 VerifyAccess 是這條路徑上唯一一次驗證,不構成重複。
	Logout(ctx context.Context, accessToken, refreshToken string) error
}
