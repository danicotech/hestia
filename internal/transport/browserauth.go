package transport

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// 瀏覽器用的 Discord 登入流程。
//
// ## 為什麼 ConnectRPC 端點不夠
//
// AuthService.CompleteDiscordLogin 是一支 POST 的 RPC。但 Discord 完成授權後
// 做的事是**把使用者的瀏覽器重導到 redirect_uri** —— 那是一個普通的
// `GET /auth/discord/callback?code=...&state=...`,沒有 JSON body、沒有
// Connect 的 header、也不會有人替它組請求。少了這條路由,瀏覽器拿到的是 404,
// 登入流程根本走不完。兩者不是替代關係:RPC 給程式呼叫,這裡給瀏覽器導覽。
//
// ## 分工
//
// 這一層只做「搬運與導向」:讀 query、搬 cookie、轉成站內導向。
// 全部判定(state 比對、code 交換、session 簽發)仍在 identity;
// 這裡沒有任何一個安全決定,所以也沒有「忘了檢查」的空間。
//
// ## 錯誤一律導回站內,而且只帶封閉值域的錯誤碼
//
// 回呼的 query 是**任何人都能自由控制的字串**(那個 URL 誰都組得出來)。
// 把 provider 回的錯誤或內部訊息塞進導回網址,等於開一條反射通道,
// 而且會把內部細節印在使用者的網址列與瀏覽器歷史裡。
// 所以:錯誤先折成 errmap.go 那張表上的 reason,再放進 ?login_error=。
const (
	browserStartPath    = "/auth/discord/start"
	browserCallbackPath = "/auth/discord/callback"
	browserRefreshPath  = "/auth/refresh"
	browserLogoutPath   = "/auth/logout"
)

// loginErrorParam 是導回前端時帶錯誤碼的 query 參數。
const loginErrorParam = "login_error"

// siteRoot 是登入失敗時的導回目標。
//
// 刻意是 "/" 而不是掛載前綴:前綴是 **API** 的掛載點(/api),前端在站台根目錄。
// 把使用者導去 /api/ 只會讓他看到一個 404。
const siteRoot = "/"

var (
	// errOAuthDenied 是使用者在 Discord 的授權頁按了取消(?error=access_denied)。
	// 它**不是**系統錯誤:回 500 或紅色的「登入失敗」都在說謊,
	// 使用者只是改變了主意。前端該顯示的是「你取消了登入」。
	errOAuthDenied = errors.New("使用者取消了 Discord 授權")
	// errOAuthCallbackInvalid 是回呼缺 code 或 state(不是我們發出去的那個流程)。
	errOAuthCallbackInvalid = errors.New("OAuth 回呼參數不完整")
	// errLoginFailed 是**未映射錯誤**在導回網址上的統一代表。
	//
	// 為什麼需要它:reasonFor 對認不得的錯誤刻意回空字串(不洩漏內部分類),
	// 但網址上必須有一個值,否則前端分不出「登入失敗」與「沒登入過」。
	// 折成單一個 login_failed 既滿足前端,也沒有多告訴攻擊者任何事。
	errLoginFailed = errors.New("登入失敗")
)

// browserAuth 是四條瀏覽器路由的 handler。
type browserAuth struct {
	svc        AuthService
	state      stateCookieConfig
	session    sessionCookieConfig
	trustProxy bool
}

// register 把路由掛上 mux。路徑是**相對於掛載前綴**的 ——
// 前綴由 mountAt 在更外層剝掉,所以這裡永遠寫不帶前綴的形式,
// 「前綴改了忘了改路由」不可能發生。
//
// 方法寫死在 pattern 裡(Go 1.22 的 ServeMux):
//   - start / callback 是 GET —— 它們是瀏覽器導覽的落點,不可能是別的。
//   - refresh / logout 是 POST,理由見各自的說明。
//
// 方法不符會得到 405,而 405 在稽核裡折成 Unimplemented(httpaudit.go),
// 與「這條路不存在」同一個訊號。
func (h browserAuth) register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+browserStartPath, h.start)
	mux.HandleFunc("GET "+browserCallbackPath, h.callback)
	mux.HandleFunc("POST "+browserRefreshPath, h.refresh)
	mux.HandleFunc("POST "+browserLogoutPath, h.logout)
}

// start 起頭:產生 authorize URL 與 state、種下 state cookie、302 去 Discord。
//
//	GET {base}/auth/discord/start?redirect=/tournaments/1
func (h browserAuth) start(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.svc == nil {
		h.abort(w, r, http.StatusNotImplemented,
			platformv1connect.AuthServiceStartDiscordLoginProcedure, "")
		return
	}
	// redirect 用 identity 既有的 CleanRedirect 驗(open redirect 的唯一防線,
	// 不在這裡重寫一份規則)。**拒絕而不是靜靜改成 "/"**:
	// 帶絕對 URL 進來的請求不是使用者打錯字,是有人在試探;
	// 靜靜修正會讓那次試探看起來成功,也讓真正的設定錯誤永遠不被發現。
	redirect, err := identity.CleanRedirect(r.URL.Query().Get("redirect"))
	if err != nil {
		h.abort(w, r, http.StatusBadRequest,
			platformv1connect.AuthServiceStartDiscordLoginProcedure, reasonFor(err))
		return
	}
	authorizeURL, state, err := h.svc.StartDiscordLogin(r.Context(), redirect)
	if err != nil {
		h.abort(w, r, statusForLogin(err),
			platformv1connect.AuthServiceStartDiscordLoginProcedure, loginReason(err))
		return
	}
	// 在消費點驗一次「這真的是一個外部 https 端點」。authorizeURL 來自
	// AuthService 的實作(不在本套件控制之下),而這是整個服務唯一一處
	// 會把使用者導去**站外**的地方 —— 它必須是這條路徑上最不能出錯的一行。
	if err := checkExternalURL(authorizeURL); err != nil {
		h.abort(w, r, http.StatusInternalServerError,
			platformv1connect.AuthServiceStartDiscordLoginProcedure, loginReason(errLoginFailed))
		return
	}
	// state 同時進 HttpOnly cookie:回呼時兩邊必須相符。cookie 就是那份
	// 「這個流程屬於這個瀏覽器」的證明,攻擊者沒辦法替受害者的瀏覽器準備它。
	http.SetCookie(w, h.state.issue(state))
	noteAudit(r.Context(), platformv1connect.AuthServiceStartDiscordLoginProcedure,
		eventlog.StatusOK, "")
	// nolint:gosec // G710:authorizeURL 不是使用者輸入 —— 它由 identity 用
	// 部署設定的 provider 端點組出來(使用者能影響的只有簽在 state 裡的
	// 站內相對路徑,而那個值已經過 CleanRedirect)。上面的 checkExternalURL
	// 是消費點的第二道驗證:非 https 絕對網址一律不導。
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

// callback 是 Discord 導回來的落點。
//
//	GET {base}/auth/discord/callback?code=...&state=...
//	GET {base}/auth/discord/callback?error=access_denied&state=...
//
// 成功:清 state cookie → 種兩個 session cookie → 302 回 state 裡的相對路徑。
// 失敗:清 state cookie → 302 回 /?login_error=<reason>。
//
// 任何一條失敗路徑都**不會**種下 session cookie —— 那是這支 handler 唯一
// 不能出錯的事。
func (h browserAuth) callback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	const action = platformv1connect.AuthServiceCompleteDiscordLoginProcedure
	// state 是一次性的:不論成敗都先清掉。留著只是多給一次重放機會,
	// 而且「先清再判斷」保證沒有任何一條 return 路徑會忘記清。
	http.SetCookie(w, h.state.clear())

	if h.svc == nil {
		h.abort(w, r, http.StatusNotImplemented, action, "")
		return
	}
	q := r.URL.Query()
	// Discord 自己回報的錯誤(使用者按取消、scope 不合法、對方掛了)。
	// 這一條必須在 code/state 檢查**之前**:取消授權時本來就沒有 code,
	// 當成「參數不完整」會把使用者的正常選擇報成系統錯誤。
	if providerErr := q.Get("error"); providerErr != "" {
		h.failLogin(w, r, action, providerError(providerErr))
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		h.failLogin(w, r, action, errOAuthCallbackInvalid)
		return
	}
	// cookie 讀不到就是空字串,**照樣往下送**:比對權威在 identity
	// (它會回 ErrStateMismatch)。這裡沒有任何提前放行的分支。
	fromCookie := stateFromCookie(r.Header)
	dev := deviceInfo(r.RemoteAddr, r.Header, h.trustProxy)
	session, _, err := h.svc.CompleteDiscordLogin(r.Context(), code, state, fromCookie, dev)
	if err != nil {
		h.failLogin(w, r, action, err)
		return
	}
	h.session.setSession(w, session, time.Now())
	noteAudit(r.Context(), action, eventlog.StatusOK, "")
	// nolint:gosec // G710:landingPath 的回傳值必定通過 identity.CleanRedirect
	// (絕對 URL、protocol-relative、反斜線、含 scheme 者全部拒絕),
	// 驗不過就退回站台根目錄。這裡不可能導到站外。
	http.Redirect(w, r, landingPath(session.Redirect), http.StatusFound)
}

// refresh 換發:讀 refresh cookie → 輪替 → 更新兩個 cookie。
//
//	POST {base}/auth/refresh
//
// ## 為什麼是 POST 而不是 GET
//
// 換發**改變狀態**:每次成功都會把舊 refresh token 標成 rotated 並發一把新的
// (schemas/02 增補 F 的輪替 + 重用偵測),而且刻意沒有 grace window ——
// 舊 token 再被用一次就判定為竊取,整條輪替鏈全撤、使用者被迫重新登入。
//
// GET 在 HTTP 的定義裡是安全且可重複的,於是任何東西都可以自作主張地發它:
// 瀏覽器預抓、`<img src>`、連結預覽、爬蟲、按上一頁。上面那個「沒有 grace
// window」的設計會把每一次這種意外重放都判成 token 被竊,結果是隨機登出。
//
// 第二個理由更硬:SameSite=Lax 對跨站的**頂層 GET 導覽**是放行的,對跨站
// POST 才是攔的。做成 GET 等於留一個誰都能觸發的換發端點(CSRF);
// 做成 POST,cookie 認證的 CSRF 防線才真的成立。
//
// 回應是 204 而不是 JSON:新的 token 只存在於 Set-Cookie,body 裡一個字都沒有。
// 前端本來就讀不到(HttpOnly),放進 body 只是多一份會被記進 log 的憑證副本。
func (h browserAuth) refresh(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	const action = platformv1connect.AuthServiceRefreshSessionProcedure
	if h.svc == nil {
		h.abort(w, r, http.StatusNotImplemented, action, "")
		return
	}
	token := cookieValue(r.Header, refreshCookieName)
	if token == "" {
		// 沒有 cookie(從未登入、已過期、或跨站送來所以 Lax 沒帶)一律
		// Unauthenticated,而且**不清 cookie** —— 這條路徑不知道發生了什麼,
		// 沒有理由替一個可能還有效的 session 做決定。
		h.abort(w, r, http.StatusUnauthorized, action, reasonFor(ErrUnauthenticated))
		return
	}
	session, err := h.svc.RefreshSession(r.Context(), token, deviceInfo(r.RemoteAddr, r.Header, h.trustProxy))
	if err != nil {
		// 換發失敗代表這個瀏覽器手上的 refresh token 已經沒用了
		// (過期、撤銷、重用偵測)。清掉兩個 cookie,讓下一次請求乾淨地
		// 走「未登入」而不是拿著死憑證反覆撞 401。
		h.session.clearSession(w)
		h.abort(w, r, statusForLogin(err), action, loginReason(err))
		return
	}
	h.session.setSession(w, session, time.Now())
	noteAudit(r.Context(), action, eventlog.StatusOK, "")
	w.WriteHeader(http.StatusNoContent)
}

// logout 撤銷這台裝置的 session 並清掉兩個 cookie。
//
//	POST {base}/auth/logout
//
// POST 的理由與 refresh 相同(改變狀態 + 跨站 GET 會帶 cookie);
// 一個 GET 的登出端點意味著任何網站放一張 `<img src=".../auth/logout">`
// 就能把使用者踢出去。
//
// **無論撤銷成功與否都清 cookie,而且一律回 204**:
// 登出最不能出現的結果是「瀏覽器以為還登著、伺服器以為已登出」或反過來。
// 從使用者的角度,按下登出之後這個瀏覽器就不該再持有憑證 —— 那件事這裡
// 一定做得到。撤銷失敗是伺服器端的問題,瀏覽器沒有任何可以據此做的事,
// 所以它進 event_logs(status=error + error_code),不進回應。
func (h browserAuth) logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	const action = platformv1connect.AuthServiceLogoutProcedure
	h.session.clearSession(w)

	access := cookieValue(r.Header, accessCookieName)
	refresh := cookieValue(r.Header, refreshCookieName)
	switch {
	case h.svc == nil, access == "" && refresh == "":
		// 沒有憑證可撤(從未登入、或已經登出過)。登出是冪等的,不是錯誤。
		noteAudit(r.Context(), action, eventlog.StatusOK, "")
	default:
		if err := h.svc.Logout(r.Context(), access, refresh); err != nil {
			// status / error_code 與 RPC 那條路徑同一套詞彙(statusOf),
			// 不是這裡另外判一次 —— 同一個錯誤在兩條路徑上必須長得一樣。
			noteAudit(r.Context(), action, statusForAudit(err), codeOfLogin(err))
		} else {
			noteAudit(r.Context(), action, eventlog.StatusOK, "")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// failLogin 是回呼的失敗出口:導回站內並帶封閉值域的錯誤碼。
//
// 不回 4xx/5xx 頁面是刻意的:這條路徑的使用者正在**瀏覽器導覽中**,
// 一個裸的錯誤頁沒有任何可以按的東西。導回前端才有「重試登入」的入口。
func (h browserAuth) failLogin(w http.ResponseWriter, r *http.Request, action string, err error) {
	reason := loginReason(err)
	noteAudit(r.Context(), action, statusForAudit(err), codeOfLogin(err))
	http.Redirect(w, r, siteRoot+"?"+url.Values{loginErrorParam: {reason}}.Encode(),
		http.StatusFound)
}

// abort 是非導覽路徑(參數不合法、服務未接上、換發失敗)的出口:
// 回狀態碼 + 機器可讀的成因 header,body 只有一行固定文案。
//
// 用的是同一個 Hestia-Error-Reason header,不是另一套 ——
// 呼叫端處理錯誤的方式因此與 ConnectRPC 完全一致。
func (h browserAuth) abort(w http.ResponseWriter, r *http.Request, status int, action, reason string) {
	if reason != "" {
		w.Header().Set(ErrorReasonHeader, reason)
	}
	noteAudit(r.Context(), action, statusFromHTTP(status), codeFromHTTPStatus(status))
	http.Error(w, http.StatusText(status), status)
}

// providerError 把 provider 回報的 OAuth error code 折成我們自己的錯誤。
//
// 值域封閉:對不上的一律當成「向 provider 換憑證失敗」。
// **絕不把 providerErr 的字串帶進任何回應或稽核** —— 那個值來自 URL query,
// 是攻擊者自由控制的輸入(RFC 6749 只約束 provider,不約束偽造回呼的人)。
func providerError(providerErr string) error {
	if providerErr == "access_denied" {
		return errOAuthDenied
	}
	return identity.ErrProviderExchange
}

// loginReason 取錯誤在導回網址上的代表值。
// 認不得的一律折成 login_failed:未映射錯誤的分類不是契約的一部分。
func loginReason(err error) string {
	if reason := reasonFor(err); reason != "" {
		return reason
	}
	return reasonFor(errLoginFailed)
}

// statusForLogin 把錯誤折成 HTTP 狀態碼,供非導覽路徑使用。
//
// 判定來源是 errmap 那張表(toConnectError)而不是逐個錯誤 switch:
// 同一個領域錯誤在 RPC 與這裡回不同的意思,對呼叫端是災難。
// 這裡只做「connect code → HTTP status」這一步,而那正是 connect 自己的規則。
func statusForLogin(err error) int {
	switch connect.CodeOf(toConnectError(err)) {
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// statusForAudit / codeOfLogin 讓導向路徑(HTTP 一律 302)在 event_logs 裡
// 仍然誠實:表上記的是這次登入的真實結果,不是「回了 302 所以是 ok」。
// 兩者共用 statusOf —— 與 RPC 那條路徑同一套 status / error_code 詞彙。
func statusForAudit(err error) string {
	status, _ := statusOf(toConnectError(err))
	return status
}

func codeOfLogin(err error) string {
	_, code := statusOf(toConnectError(err))
	return code
}

// checkExternalURL 確認這是一個可以把使用者送出去的外部端點。
//
// 這是整個服務唯一一處導向站外的地方,而值來自 AuthService 的實作
// (不在本套件控制之下)。三個條件缺一不可:
//
//	絕對 URL   相對路徑會把使用者留在站內的某個奇怪位置,登入靜默失敗
//	https      OAuth 的 authorization request 帶著 client_id 與 state,
//	           走明文等於把整段流程攤開給同網段的人看
//	有 host    "https:///x" 這種形狀在各家瀏覽器的解讀不一致
func checkExternalURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errLoginFailed
	}
	if u.Scheme != "https" || u.Host == "" {
		return errLoginFailed
	}
	return nil
}

// landingPath 決定成功後導去哪裡。
//
// 在消費點**再驗一次** CleanRedirect(簽發時 identity 已驗過):
// 這是 open redirect 唯一不依賴其他程式碼正確性的寫法 ——
// 就算哪天 state 的簽發端放寬了規則,舊 token 也不會變成一個外送門。
// 驗不過就回站台根目錄,而不是照樣導出去。
func landingPath(redirect string) string {
	clean, err := identity.CleanRedirect(redirect)
	if err != nil {
		return siteRoot
	}
	return clean
}

// noStore 讓任何帶 Set-Cookie 或導向的回應都不進快取。
// 共用快取(CDN、反向代理)存下一份帶 Set-Cookie 的回應,
// 就等於把某個人的 session 發給下一個造訪者。
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}
