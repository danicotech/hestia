package transport

import (
	"errors"
	"net/http"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// OAuth state 的瀏覽器綁定(login CSRF / session fixation 防線)。
//
// 為什麼光有簽章不夠:自簽的 state 只證明「這是我們發的」,不證明「它屬於
// 發起登入的那個瀏覽器」。攻擊者拿一個合法 state 配上自己的 authorization
// code 誘導受害者造訪回呼,受害者的瀏覽器就被種上**攻擊者帳號**的 session。
// 把 state 同時放進 HttpOnly cookie,回呼時兩邊必須相符,攻擊者就無法替
// 受害者的瀏覽器準備那個 cookie。
//
// 分工:入口層只負責**搬運**(寫 cookie、讀 cookie、原樣交給 identity),
// 比對在 identity 內部做——「忘了比對」因此不可能發生。
const stateCookieName = "hestia_oauth_state"

// defaultStateCookiePath 是 "/" 而不是 "/auth"。
//
// 這是刻意偏離一般寫法:ConnectRPC 的路徑是 procedure 全名
// (/hestia.platform.v1.AuthService/CompleteDiscordLogin),不是 /auth。
// cookie 的 Path 若設 /auth,瀏覽器就**不會**把它送到回呼那支 RPC,
// 整個登入流程會靜默失敗。要收窄的話請用 Deps.StateCookiePath 明確指定
// 服務實際掛載的前綴(例如 /hestia.platform.v1.AuthService)。
const defaultStateCookiePath = "/"

// stateCookieConfig 是 cookie 的屬性,由 Deps 決定。
//
// 刻意沒有「關掉 Secure」的開關:Secure 一旦可設定,遲早有人在正式環境把它
// 關掉。本機開發也不需要——現代瀏覽器把 http://localhost 當成 secure context,
// Secure cookie 照樣運作。少一個旋鈕,就少一種把憑證送上明文連線的方式。
type stateCookieConfig struct {
	path string
}

// newStateCookieConfig 決定 state cookie 的 Path,優先序是:
//
//	Deps.StateCookiePath  明確指定(最權威,呼叫端知道自己在做什麼)
//	掛載前綴              服務實際掛在哪就發到哪(/api → Path=/api)
//	"/"                   掛在根時的唯一正確答案
//
// 為什麼前綴要參與:這個 cookie 必須同時送達兩個回呼入口 ——
// 瀏覽器路由 {base}/auth/discord/callback 與 ConnectRPC 的
// {base}/hestia.platform.v1.AuthService/CompleteDiscordLogin。
// 兩者只在 {base} 這一層有共同祖先,所以 Path 就是 {base},不能再窄。
func newStateCookieConfig(explicit, basePath string) stateCookieConfig {
	switch {
	case explicit != "":
		return stateCookieConfig{path: explicit}
	case basePath != "":
		return stateCookieConfig{path: basePath}
	default:
		return stateCookieConfig{path: defaultStateCookiePath}
	}
}

// issue 產生「種下 state」的 cookie。
//
//	HttpOnly  JS 讀不到,XSS 也偷不走
//	SameSite=Lax  OAuth 回呼是跨站導轉回來的 GET/POST,Strict 會讓 cookie 送不出去
//	Secure    永遠開(localhost 也是 secure context,開發不受影響)
//	Max-Age   等同 identity.StateTTL(10 分鐘),過期即失效
func (c stateCookieConfig) issue(state string) *http.Cookie {
	return &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     c.path,
		MaxAge:   int(identity.StateTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// clear 產生「立刻清掉」的 cookie。state 是一次性的:
// 用過(或用失敗)就該消失,留著只是多一次被重放的機會。
func (c stateCookieConfig) clear() *http.Cookie {
	return &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     c.path,
		MaxAge:   -1, // 立即刪除
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// stateFromCookie 從請求 header 讀出 cookie 值。
// 讀不到就回空字串,**不做任何提前放行**——空值照樣往下送,
// 由 identity 判定(它會回 ErrStateMismatch)。
func stateFromCookie(header http.Header) string {
	return cookieValue(header, stateCookieName)
}

// setCookie 把 Set-Cookie 掛到成功的回應上。
func setCookie[T any](res *connect.Response[T], c *http.Cookie) {
	if v := c.String(); v != "" {
		res.Header().Add("Set-Cookie", v)
	}
}

// clearCookieOnError 讓失敗的回呼也把 state cookie 清掉。
// 失敗才更要清:留著那把一次性憑證只是給攻擊者第二次機會。
func clearCookieOnError(err error, c *http.Cookie) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	if v := c.String(); v != "" {
		ce.Meta().Add("Set-Cookie", v)
	}
	return err
}
