package transport

import (
	"net/http"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// 瀏覽器 session 的兩個 cookie。
//
// ## 為什麼是 cookie,不是把 token 丟進網址 fragment 給前端自己存
//
// 「回呼後把 token 放進 #access_token=... 讓 JS 撈走存進 localStorage」是很常見的
// 寫法,而它把**全部**憑證交給 JS —— 任何一個 XSS(自己的、第三方腳本的、
// 依賴鏈上的)就能把 refresh token 整個帶走,而 refresh token 是 30 天的長效憑證。
// HttpOnly cookie 讓 JS 讀不到:XSS 仍然能以使用者身分發請求(那擋不住),
// 但**偷不走可攜帶的憑證**,攻擊隨著分頁關閉而結束。token 也不會經過網址,
// 因此不會進瀏覽器歷史、Referer、反向代理的存取日誌。
//
// ## cookie 認證的 CSRF 防線是 SameSite=Lax
//
// 一旦攔截器接受 cookie,瀏覽器就會**自動**替跨站請求附上憑證,CSRF 從此是
// 這個系統的實際威脅。SameSite=Lax 的意思是:跨站的 POST / fetch 一律不帶
// 這個 cookie,只有頂層 GET 導覽會帶。而本服務所有 ConnectRPC 都是 POST,
// 換發與登出也刻意是 POST —— 於是「跨站送來的請求拿不到 cookie」,
// 在攔截器眼中就等同「沒有憑證」,直接 Unauthenticated。
//
// 這也是為什麼 /auth/refresh 不能是 GET(見 browserauth.go 的說明)。
//
// ## Secure 沒有開關
//
// 與 state cookie 同一個理由:一旦可設定,遲早有人在正式環境把它關掉。
// 本機開發不受影響 —— 現代瀏覽器把 http://localhost 當成 secure context。
const (
	// accessCookieName 是短效 access token(15 分鐘)。
	accessCookieName = "hestia_access_token"
	// refreshCookieName 是長效 refresh token(30 天)。
	refreshCookieName = "hestia_refresh_token"
)

// sessionCookieConfig 依掛載前綴決定兩個 cookie 的 Path。
//
// Path 不是裝飾:它決定瀏覽器把憑證送給誰。同一個 host 上還有前端(以及
// 未來可能的其他應用),把 Path 寫死 "/" 等於讓每一個靜態資源請求都帶上
// access token。前綴一改而 Path 沒跟著改,則是反過來:cookie 送不到 API,
// 登入看起來成功但每一支 RPC 都 401。
type sessionCookieConfig struct {
	basePath string // 已由 NormalizeBasePath 正規化;"" = 掛在根
}

func newSessionCookieConfig(basePath string) sessionCookieConfig {
	return sessionCookieConfig{basePath: basePath}
}

// accessPath 是 access token 的作用範圍:整個 API 掛載點。
// 每一支 RPC 都需要它,所以不能再收窄。
func (c sessionCookieConfig) accessPath() string {
	if c.basePath == "" {
		return "/"
	}
	return c.basePath
}

// refreshPath 刻意比 accessPath 更窄:只有 {base}/auth 底下的換發與登出需要
// refresh token,其餘上百次 API 請求都不該把 30 天的長效憑證帶在身上。
//
// 收窄的效益是實際的:少一個 cookie 在網路上、在反向代理的緩衝區、在任何
// 一次誤把 header 記進 log 的地方出現。它不是「多一道防線」的形式主義,
// 而是把長效憑證的曝光面壓到真正需要它的那幾個端點。
func (c sessionCookieConfig) refreshPath() string {
	return c.basePath + "/auth"
}

// issueAccess 產生 access token 的 cookie。Max-Age 取自 token 自己的到期時間
// (SessionView.AccessTokenExpiresAt),不是另外寫死一個數字 ——
// 兩個地方各寫一份壽命,遲早會有一邊比另一邊長,那個差值就是「cookie 還在
// 但 token 已死」或反過來的窗口。
func (c sessionCookieConfig) issueAccess(token string, expiresAt, now time.Time) *http.Cookie {
	return c.issue(accessCookieName, c.accessPath(), token,
		maxAgeFor(expiresAt, identity.AccessTokenTTL, now))
}

// issueRefresh 產生 refresh token 的 cookie。
func (c sessionCookieConfig) issueRefresh(token string, expiresAt, now time.Time) *http.Cookie {
	return c.issue(refreshCookieName, c.refreshPath(), token,
		maxAgeFor(expiresAt, identity.RefreshTokenTTL, now))
}

func (c sessionCookieConfig) issue(name, path, value string, maxAge int) *http.Cookie {
	if value == "" || maxAge <= 0 {
		// 沒有值、或壽命已經是負的:種下去只會是一個立刻失效的憑證。
		// 直接送刪除指令,不留一個「看起來有登入」的假象。
		return clearCookieAt(name, path)
	}
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// clearAccess / clearRefresh 產生刪除指令。
//
// Path 必須與種下時**完全相同**,否則瀏覽器會當成另一個 cookie 而留著原來的
// ——「登出了但 cookie 還在」是最糟的一種靜默失敗。
func (c sessionCookieConfig) clearAccess() *http.Cookie {
	return clearCookieAt(accessCookieName, c.accessPath())
}

func (c sessionCookieConfig) clearRefresh() *http.Cookie {
	return clearCookieAt(refreshCookieName, c.refreshPath())
}

func clearCookieAt(name, path string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1, // 立即刪除
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// setSession 把一次登入/換發的兩個 cookie 掛上回應。
func (c sessionCookieConfig) setSession(w http.ResponseWriter, s *SessionView, now time.Time) {
	http.SetCookie(w, c.issueAccess(s.AccessToken, s.AccessTokenExpiresAt, now))
	http.SetCookie(w, c.issueRefresh(s.RefreshToken, s.RefreshTokenExpiresAt, now))
}

// clearSession 清掉兩個 cookie(登出、以及任何「這個瀏覽器不該再持有憑證」的時刻)。
func (c sessionCookieConfig) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, c.clearAccess())
	http.SetCookie(w, c.clearRefresh())
}

// maxAgeFor 由到期時間算出 Max-Age 秒數;到期時間缺席(port 實作沒填)時
// 退回契約常數。**不做無條件退回**:實作填了就以實作為準,那才是真正的壽命。
func maxAgeFor(expiresAt time.Time, fallback time.Duration, now time.Time) int {
	d := fallback
	if !expiresAt.IsZero() {
		d = expiresAt.Sub(now)
	}
	secs := int(d.Seconds())
	if secs <= 0 {
		return -1
	}
	return secs
}

// cookieValue 讀出指定 cookie 的值;讀不到回空字串。
//
// 回空字串而不是回 error 是刻意的:呼叫端一律把「沒有」與「空的」當成
// 同一件事往下送,由權威層判定。任何「沒有 cookie 就跳過檢查」的分支
// 都是 CSRF 防護最常見的破口。
func cookieValue(header http.Header, name string) string {
	r := http.Request{Header: header}
	c, err := r.Cookie(name)
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}
