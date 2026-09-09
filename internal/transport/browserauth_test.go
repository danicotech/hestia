package transport_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/transport"
)

// 瀏覽器用的 Discord 登入流程(GET 導覽,不是 RPC)。
//
// 這些測試不需要真的 Discord:要驗的是入口層自己的責任 —— cookie 屬性、
// state 的搬運、失敗時**絕不建立 session**、錯誤碼只來自封閉值域、
// 前綴有沒有跟著走。判定本身(state 比對、code 交換)是 identity 的事,
// 所以替身用與 identity 相同的規則。

const (
	browserState        = "browser-state-token-xyz"
	browserAccessToken  = "ACCESS-TOKEN-NEVER-LOG-ME"
	browserRefreshToken = "REFRESH-TOKEN-NEVER-LOG-ME"
)

// fakeBrowserAuth 是 AuthService 的替身。
type fakeBrowserAuth struct {
	authorizeURL string
	redirect     string // 登入後要回到的站內路徑(state 帶進來的)
	startErr     error
	completeErr  error
	refreshErr   error
	logoutErr    error

	gotStartRedirect string
	gotCode          string
	gotState         string
	gotCookie        string
	gotRefreshToken  string
	gotLogoutAccess  string
	gotLogoutRefresh string
	completeCalls    int
}

func (f *fakeBrowserAuth) StartDiscordLogin(_ context.Context, redirectURI string) (string, string, error) {
	f.gotStartRedirect = redirectURI
	if f.startErr != nil {
		return "", "", f.startErr
	}
	u := f.authorizeURL
	if u == "" {
		u = "https://discord.com/oauth2/authorize?state=" + browserState
	}
	return u, browserState, nil
}

// CompleteDiscordLogin 複製 identity 的 state 判定:cookie 必須存在且相符。
func (f *fakeBrowserAuth) CompleteDiscordLogin(
	_ context.Context, code, state, fromCookie string, _ transport.DeviceInfo,
) (*transport.SessionView, *readmodel.ProfileView, error) {
	f.completeCalls++
	f.gotCode, f.gotState, f.gotCookie = code, state, fromCookie
	if f.completeErr != nil {
		return nil, nil, f.completeErr
	}
	if fromCookie == "" || fromCookie != state {
		return nil, nil, identity.ErrStateMismatch
	}
	return f.session(), &readmodel.ProfileView{PublicID: "U1"}, nil
}

func (f *fakeBrowserAuth) RefreshSession(
	_ context.Context, refreshToken string, _ transport.DeviceInfo,
) (*transport.SessionView, error) {
	f.gotRefreshToken = refreshToken
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.session(), nil
}

func (f *fakeBrowserAuth) Logout(_ context.Context, accessToken, refreshToken string) error {
	f.gotLogoutAccess, f.gotLogoutRefresh = accessToken, refreshToken
	return f.logoutErr
}

func (f *fakeBrowserAuth) session() *transport.SessionView {
	now := time.Now()
	return &transport.SessionView{
		AccessToken:           browserAccessToken,
		AccessTokenExpiresAt:  now.Add(identity.AccessTokenTTL),
		RefreshToken:          browserRefreshToken,
		RefreshTokenExpiresAt: now.Add(identity.RefreshTokenTTL),
		UserPublicID:          "U1",
		Redirect:              f.redirect,
	}
}

// ── 測試工具 ────────────────────────────────────────────────────

// noRedirectClient 讓測試看得到 302 本身(預設的 client 會自己跟過去,
// 於是「導去哪裡」與「帶了什麼 cookie」都看不見)。
func noRedirectClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	return &http.Client{
		Transport: c.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// browserResp 是回應的快照:狀態碼、header、Set-Cookie。
//
// 刻意不回 *http.Response:body 在 doBrowser 裡就讀完關掉了,
// 交出一個還握著連線的物件只會讓每個呼叫端都得記得收尾
// (而那正是 bodyclose 這條 lint 在防的事)。
type browserResp struct {
	Status  int
	Header  http.Header
	Cookies []*http.Cookie
}

func doBrowser(
	t *testing.T, srv *httptest.Server, method, path string, cookies ...*http.Cookie,
) browserResp {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("建請求:%v", err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient(srv).Do(req)
	if err != nil {
		t.Fatalf("送請求:%v", err)
	}
	// body 當場讀完關掉:這些路由的斷言全在 header 與狀態碼上。
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("讀回應:%v", err)
	}
	return browserResp{Status: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies()}
}

func respCookie(t *testing.T, resp browserResp, name string) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// assertNoSession 是這一整組測試最重要的斷言:失敗路徑**絕不**建立 session。
func assertNoSession(t *testing.T, resp browserResp) {
	t.Helper()
	for _, name := range []string{"hestia_access_token", "hestia_refresh_token"} {
		c := respCookie(t, resp, name)
		if c != nil && c.Value != "" {
			t.Fatalf("失敗路徑竟然種下了 %s cookie:%+v", name, c)
		}
	}
}

// loginErrorOf 取導回網址上的錯誤碼。
func loginErrorOf(t *testing.T, resp browserResp) string {
	t.Helper()
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location 不是合法網址(%q):%v", loc, err)
	}
	if u.IsAbs() || u.Host != "" {
		t.Fatalf("失敗必須導回站內相對路徑,卻是 %q", loc)
	}
	return u.Query().Get("login_error")
}

// ── start ───────────────────────────────────────────────────────

func TestBrowserStart_SetsStateCookieAndRedirectsToDiscord(t *testing.T) {
	auth := &fakeBrowserAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})

	resp := doBrowser(t, srv, http.MethodGet, "/auth/discord/start?redirect=%2Ftournaments%2F1")
	if resp.Status != http.StatusFound {
		t.Fatalf("狀態碼 = %d,期望 302", resp.Status)
	}
	if got := resp.Header.Get("Location"); !strings.HasPrefix(got, "https://discord.com/") {
		t.Fatalf("沒有導去 Discord:%q", got)
	}
	if auth.gotStartRedirect != "/tournaments/1" {
		t.Fatalf("redirect 沒有原樣送進 identity:%q", auth.gotStartRedirect)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("帶 Set-Cookie 的回應必須 no-store,got %q", got)
	}

	c := respCookie(t, resp, "hestia_oauth_state")
	if c == nil {
		t.Fatal("沒有種下 state cookie —— 回呼一定會失敗")
	}
	if c.Value != browserState {
		t.Fatalf("cookie 值 = %q,期望 %q", c.Value, browserState)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie 屬性不對:%+v", c)
	}
	if c.MaxAge <= 0 || time.Duration(c.MaxAge)*time.Second > identity.StateTTL {
		t.Fatalf("Max-Age = %d,必須是正值且不超過 StateTTL", c.MaxAge)
	}
	if c.Path != "/" {
		t.Fatalf("沒有前綴時 Path 必須是 /,got %q", c.Path)
	}
}

// 沒帶 redirect 時預設站台根目錄(CleanRedirect 的既有規則)。
func TestBrowserStart_DefaultsToSiteRoot(t *testing.T) {
	auth := &fakeBrowserAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	if resp := doBrowser(t, srv, http.MethodGet, "/auth/discord/start"); resp.Status != http.StatusFound {
		t.Fatalf("狀態碼 = %d", resp.Status)
	}
	if auth.gotStartRedirect != "/" {
		t.Fatalf("預設 redirect = %q,期望 /", auth.gotStartRedirect)
	}
}

// open redirect 的防線沿用 identity.CleanRedirect;拒絕而不是靜靜改成 "/"。
func TestBrowserStart_RejectsUnsafeRedirect(t *testing.T) {
	cases := map[string]string{
		"絕對 URL":            "https://evil.com/",
		"protocol-relative": "//evil.com",
		"反斜線":               "/\\evil.com",
		"javascript scheme": "javascript:alert(1)",
		"控制字元":              "/a\x01b",
		"不以斜線開頭":            "evil.com",
	}
	for name, redirect := range cases {
		t.Run(name, func(t *testing.T) {
			auth := &fakeBrowserAuth{}
			srv, _ := newServer(t, transport.Deps{Auth: auth})
			resp := doBrowser(t, srv, http.MethodGet,
				"/auth/discord/start?redirect="+url.QueryEscape(redirect))
			if resp.Status != http.StatusBadRequest {
				t.Fatalf("狀態碼 = %d,期望 400(%q 必須被拒)", resp.Status, redirect)
			}
			if auth.gotStartRedirect != "" {
				t.Fatalf("不合法的 redirect 竟然送進了 identity:%q", auth.gotStartRedirect)
			}
			if c := respCookie(t, resp, "hestia_oauth_state"); c != nil && c.Value != "" {
				t.Fatalf("被拒的請求不該種 state cookie:%+v", c)
			}
			if got := resp.Header.Get(transport.ErrorReasonHeader); got != "invalid_redirect_uri" {
				t.Fatalf("錯誤成因 = %q,期望 invalid_redirect_uri", got)
			}
		})
	}
}

// 導向站外是整個服務唯一一處把使用者送走的地方,所以在消費點再驗一次:
// 不是 https 絕對網址就不導(AuthService 的實作不在入口層控制之下)。
func TestBrowserStart_RejectsNonHTTPSAuthorizeURL(t *testing.T) {
	for _, bad := range []string{
		"http://discord.com/oauth2/authorize", // 明文
		"/oauth2/authorize",                   // 相對路徑
		"javascript:alert(1)",                 // 有 scheme 但不是 https
		"https:///x",                          // 沒有 host
	} {
		auth := &fakeBrowserAuth{authorizeURL: bad}
		srv, _ := newServer(t, transport.Deps{Auth: auth})
		resp := doBrowser(t, srv, http.MethodGet, "/auth/discord/start")
		if resp.Status != http.StatusInternalServerError {
			t.Fatalf("authorizeURL=%q 的狀態碼 = %d,期望 500", bad, resp.Status)
		}
		if c := respCookie(t, resp, "hestia_oauth_state"); c != nil && c.Value != "" {
			t.Fatalf("不導向就不該種 state cookie:%+v", c)
		}
	}
}

func TestBrowserStart_UnimplementedWithoutService(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{})
	if resp := doBrowser(t, srv, http.MethodGet, "/auth/discord/start"); resp.Status != http.StatusNotImplemented {
		t.Fatalf("沒接上 identity 時狀態碼 = %d,期望 501", resp.Status)
	}
}

// ── callback ────────────────────────────────────────────────────

func TestBrowserCallback_Success(t *testing.T) {
	auth := &fakeBrowserAuth{redirect: "/tournaments/1"}
	srv, _ := newServer(t, transport.Deps{Auth: auth})

	resp := doBrowser(t, srv, http.MethodGet,
		"/auth/discord/callback?code=oauth-code&state="+browserState,
		&http.Cookie{Name: "hestia_oauth_state", Value: browserState})

	if resp.Status != http.StatusFound {
		t.Fatalf("狀態碼 = %d,期望 302", resp.Status)
	}
	if got := resp.Header.Get("Location"); got != "/tournaments/1" {
		t.Fatalf("導回 %q,期望 state 裡帶的 /tournaments/1", got)
	}
	if auth.gotCookie != browserState || auth.gotState != browserState || auth.gotCode != "oauth-code" {
		t.Fatalf("參數沒有原封不動送進 identity:code=%q state=%q cookie=%q",
			auth.gotCode, auth.gotState, auth.gotCookie)
	}

	// state cookie 是一次性的,成功後立刻清掉。
	if c := respCookie(t, resp, "hestia_oauth_state"); c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("成功後應清掉 state cookie,got %+v", c)
	}

	access := respCookie(t, resp, "hestia_access_token")
	if access == nil || access.Value != browserAccessToken {
		t.Fatalf("access_token cookie 不對:%+v", access)
	}
	if !access.HttpOnly || !access.Secure || access.SameSite != http.SameSiteLaxMode {
		t.Fatalf("access_token 屬性不對(HttpOnly/Secure/Lax 缺一不可):%+v", access)
	}
	if access.Path != "/" {
		t.Fatalf("access_token Path = %q,沒有前綴時應是 /", access.Path)
	}
	if access.MaxAge <= 0 || time.Duration(access.MaxAge)*time.Second > identity.AccessTokenTTL {
		t.Fatalf("access_token Max-Age = %d,必須等於 access token 壽命", access.MaxAge)
	}

	refresh := respCookie(t, resp, "hestia_refresh_token")
	if refresh == nil || refresh.Value != browserRefreshToken {
		t.Fatalf("refresh_token cookie 不對:%+v", refresh)
	}
	if !refresh.HttpOnly || !refresh.Secure || refresh.SameSite != http.SameSiteLaxMode {
		t.Fatalf("refresh_token 屬性不對:%+v", refresh)
	}
	// 刻意比 access 更窄:只有換發/登出需要 30 天的長效憑證。
	if refresh.Path != "/auth" {
		t.Fatalf("refresh_token Path = %q,期望 /auth(收窄到換發端點)", refresh.Path)
	}
	if refresh.MaxAge <= 0 || time.Duration(refresh.MaxAge)*time.Second > identity.RefreshTokenTTL {
		t.Fatalf("refresh_token Max-Age = %d,必須等於 refresh token 壽命", refresh.MaxAge)
	}
}

// state 沒帶 redirect(或帶了不合法的)時退回站台根目錄,而不是照樣導出去。
func TestBrowserCallback_LandingFallsBackToRoot(t *testing.T) {
	for name, redirect := range map[string]string{
		"空的":   "",
		"絕對網址": "https://evil.com/",
	} {
		t.Run(name, func(t *testing.T) {
			auth := &fakeBrowserAuth{redirect: redirect}
			srv, _ := newServer(t, transport.Deps{Auth: auth})
			resp := doBrowser(t, srv, http.MethodGet,
				"/auth/discord/callback?code=c&state="+browserState,
				&http.Cookie{Name: "hestia_oauth_state", Value: browserState})
			if got := resp.Header.Get("Location"); got != "/" {
				t.Fatalf("導向 %q,期望退回 /", got)
			}
		})
	}
}

// cookie 缺失 / 不符 / 兩者皆空 —— 全部被擋,而且**不建立 session**。
func TestBrowserCallback_StateMustMatchCookie(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		cookie     string
		wantReason string
		wantSvc    bool // 有沒有走到 identity
	}{
		{"cookie 缺失", "code=c&state=" + browserState, "", "oauth_state_mismatch", true},
		{"cookie 與 state 不符", "code=c&state=" + browserState, "attacker-state", "oauth_state_mismatch", true},
		{"state 空、cookie 有值", "code=c", browserState, "oauth_callback_invalid", false},
		{"兩者皆空", "code=c", "", "oauth_callback_invalid", false},
		{"缺 code", "state=" + browserState, browserState, "oauth_callback_invalid", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth := &fakeBrowserAuth{}
			srv, _ := newServer(t, transport.Deps{Auth: auth})
			var cookies []*http.Cookie
			if c.cookie != "" {
				cookies = append(cookies, &http.Cookie{Name: "hestia_oauth_state", Value: c.cookie})
			}
			resp := doBrowser(t, srv, http.MethodGet, "/auth/discord/callback?"+c.query, cookies...)

			if resp.Status != http.StatusFound {
				t.Fatalf("狀態碼 = %d,期望導回站內的 302", resp.Status)
			}
			if got := loginErrorOf(t, resp); got != c.wantReason {
				t.Fatalf("login_error = %q,期望 %q", got, c.wantReason)
			}
			assertNoSession(t, resp)
			// 失敗也要清掉一次性的 state cookie。
			if sc := respCookie(t, resp, "hestia_oauth_state"); sc == nil || sc.MaxAge >= 0 {
				t.Fatalf("失敗回應應清掉 state cookie,got %+v", sc)
			}
			if got := auth.completeCalls > 0; got != c.wantSvc {
				t.Fatalf("走到 identity = %v,期望 %v", got, c.wantSvc)
			}
		})
	}
}

// 使用者在 Discord 按了取消:導回站內帶可辨識的錯誤碼,不是 500。
func TestBrowserCallback_ProviderError(t *testing.T) {
	cases := map[string]string{
		"access_denied":           "oauth_denied",
		"server_error":            "provider_exchange_failed",
		"temporarily_unavailable": "provider_exchange_failed",
		// 攻擊者自己組的回呼:值域封閉,絕不反射回網址。
		"<script>alert(1)</script>": "provider_exchange_failed",
	}
	for providerErr, wantReason := range cases {
		t.Run(providerErr, func(t *testing.T) {
			auth := &fakeBrowserAuth{}
			srv, _ := newServer(t, transport.Deps{Auth: auth})
			resp := doBrowser(t, srv, http.MethodGet,
				"/auth/discord/callback?error="+url.QueryEscape(providerErr)+"&state="+browserState,
				&http.Cookie{Name: "hestia_oauth_state", Value: browserState})

			if resp.Status != http.StatusFound {
				t.Fatalf("狀態碼 = %d,使用者取消不是系統錯誤", resp.Status)
			}
			// 導回網址的形狀是完全固定的:站內根目錄 + 封閉值域的錯誤碼。
			// 逐字比對是刻意的 —— 只要 provider 的字串有任何一部分被反射
			// 進去(哪怕只是拼接),這裡就會失敗。
			want := "/?login_error=" + wantReason
			if got := resp.Header.Get("Location"); got != want {
				t.Fatalf("Location = %q,期望 %q(provider 的字串絕不反射)", got, want)
			}
			if got := loginErrorOf(t, resp); got != wantReason {
				t.Fatalf("login_error = %q,期望 %q", got, wantReason)
			}
			assertNoSession(t, resp)
			if auth.completeCalls != 0 {
				t.Fatal("provider 回報錯誤時不該再去換 code")
			}
		})
	}
}

// token 的值絕不進 event_logs 的任何欄位(憑證與稽核的分界)。
func TestBrowserCallback_TokensNeverReachEventLogs(t *testing.T) {
	auth := &fakeBrowserAuth{redirect: "/"}
	srv, sink := newServer(t, transport.Deps{Auth: auth})
	doBrowser(t, srv, http.MethodGet,
		"/auth/discord/callback?code=super-secret-oauth-code&state="+browserState,
		&http.Cookie{Name: "hestia_oauth_state", Value: browserState})

	rec := waitRecords(t, sink, 1)[0]
	blob := rec.Action + rec.Status + rec.ErrorCode + string(rec.Request) + string(rec.Response)
	for _, secret := range []string{
		browserAccessToken, browserRefreshToken, browserState, "super-secret-oauth-code",
	} {
		if strings.Contains(blob, secret) {
			t.Fatalf("憑證外洩到 event_logs(%q):%s", secret, blob)
		}
	}
	// action 用 procedure 全名:同一件事在稽核表上不該有兩種寫法。
	if rec.Action != platformv1connect.AuthServiceCompleteDiscordLoginProcedure {
		t.Fatalf("action = %q,期望 procedure 全名", rec.Action)
	}
}

// 導向路徑一律 302,但稽核必須記真實結果 —— 否則表上「登入永遠成功」。
func TestBrowserCallback_FailureIsAuditedAsDenied(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{Auth: &fakeBrowserAuth{}})
	doBrowser(t, srv, http.MethodGet, "/auth/discord/callback?code=c&state="+browserState)

	rec := waitRecords(t, sink, 1)[0]
	if rec.Status != "denied" {
		t.Fatalf("status = %q,state 不符是 denied,不是 ok", rec.Status)
	}
	if rec.ErrorCode != connect.CodePermissionDenied.String() {
		t.Fatalf("error_code = %q", rec.ErrorCode)
	}
	if rec.Action != platformv1connect.AuthServiceCompleteDiscordLoginProcedure {
		t.Fatalf("action = %q", rec.Action)
	}
}

// ── refresh ─────────────────────────────────────────────────────

func TestBrowserRefresh_RotatesCookies(t *testing.T) {
	auth := &fakeBrowserAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	resp := doBrowser(t, srv, http.MethodPost, "/auth/refresh",
		&http.Cookie{Name: "hestia_refresh_token", Value: "old-refresh"})

	if resp.Status != http.StatusNoContent {
		t.Fatalf("狀態碼 = %d,期望 204", resp.Status)
	}
	if auth.gotRefreshToken != "old-refresh" {
		t.Fatalf("cookie 沒有原樣送進 identity:%q", auth.gotRefreshToken)
	}
	if c := respCookie(t, resp, "hestia_access_token"); c == nil || c.Value != browserAccessToken {
		t.Fatalf("換發後沒有更新 access_token:%+v", c)
	}
	if c := respCookie(t, resp, "hestia_refresh_token"); c == nil || c.Value != browserRefreshToken {
		t.Fatalf("換發後沒有更新 refresh_token:%+v", c)
	}
}

// 換發是**改變狀態**的操作,所以只收 POST。GET 會被瀏覽器預抓、被
// <img src> 觸發,而 SameSite=Lax 對跨站頂層 GET 是放行的。
func TestBrowserRefresh_RejectsGET(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Auth: &fakeBrowserAuth{}})
	resp := doBrowser(t, srv, http.MethodGet, "/auth/refresh",
		&http.Cookie{Name: "hestia_refresh_token", Value: "old-refresh"})
	if resp.Status != http.StatusMethodNotAllowed {
		t.Fatalf("狀態碼 = %d,換發不能是 GET(CSRF + 誤觸重放)", resp.Status)
	}
}

// 沒有 cookie(從未登入、或跨站送來所以 Lax 沒帶)→ 401,而且不動 cookie。
func TestBrowserRefresh_WithoutCookieIsUnauthenticated(t *testing.T) {
	auth := &fakeBrowserAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	resp := doBrowser(t, srv, http.MethodPost, "/auth/refresh")
	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("狀態碼 = %d,期望 401", resp.Status)
	}
	if got := resp.Header.Get(transport.ErrorReasonHeader); got != "unauthenticated" {
		t.Fatalf("錯誤成因 = %q", got)
	}
	if auth.gotRefreshToken != "" {
		t.Fatal("沒有 cookie 卻呼叫了 identity")
	}
	if len(resp.Cookies) != 0 {
		t.Fatalf("這條路徑不該動任何 cookie:%+v", resp.Cookies)
	}
}

// 換發失敗(重用偵測 / 撤銷 / 過期)→ 清掉兩個 cookie,
// 不讓瀏覽器拿著死憑證反覆撞 401。
func TestBrowserRefresh_FailureClearsCookies(t *testing.T) {
	auth := &fakeBrowserAuth{refreshErr: identity.ErrTokenReuseDetected}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	resp := doBrowser(t, srv, http.MethodPost, "/auth/refresh",
		&http.Cookie{Name: "hestia_refresh_token", Value: "stolen"})

	if resp.Status != http.StatusUnauthorized {
		t.Fatalf("狀態碼 = %d,期望 401", resp.Status)
	}
	if got := resp.Header.Get(transport.ErrorReasonHeader); got != "token_reuse_detected" {
		t.Fatalf("錯誤成因 = %q,期望 token_reuse_detected", got)
	}
	for _, name := range []string{"hestia_access_token", "hestia_refresh_token"} {
		c := respCookie(t, resp, name)
		if c == nil || c.MaxAge >= 0 || c.Value != "" {
			t.Fatalf("%s 應被清掉,got %+v", name, c)
		}
	}
}

// ── logout ──────────────────────────────────────────────────────

func TestBrowserLogout_ClearsBothCookies(t *testing.T) {
	auth := &fakeBrowserAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	resp := doBrowser(t, srv, http.MethodPost, "/auth/logout",
		&http.Cookie{Name: "hestia_access_token", Value: "at"},
		&http.Cookie{Name: "hestia_refresh_token", Value: "rt"})

	if resp.Status != http.StatusNoContent {
		t.Fatalf("狀態碼 = %d,期望 204", resp.Status)
	}
	if auth.gotLogoutAccess != "at" || auth.gotLogoutRefresh != "rt" {
		t.Fatalf("兩把 token 都要送去撤銷:%q / %q", auth.gotLogoutAccess, auth.gotLogoutRefresh)
	}
	for _, name := range []string{"hestia_access_token", "hestia_refresh_token"} {
		c := respCookie(t, resp, name)
		if c == nil || c.MaxAge >= 0 || c.Value != "" {
			t.Fatalf("%s 應被清掉,got %+v", name, c)
		}
	}
	// Path 必須與種下時相同,否則瀏覽器會留著原來那個。
	if got := respCookie(t, resp, "hestia_refresh_token").Path; got != "/auth" {
		t.Fatalf("清除指令的 Path = %q,與種下時不同就刪不掉", got)
	}
}

// 撤銷失敗也要清 cookie:登出最不能出現的結果是「瀏覽器以為還登著」。
// 失敗本身進 event_logs,不進回應(瀏覽器對它沒有任何能做的事)。
func TestBrowserLogout_ClearsCookiesEvenWhenRevokeFails(t *testing.T) {
	auth := &fakeBrowserAuth{logoutErr: identity.ErrSessionNotFound}
	srv, sink := newServer(t, transport.Deps{Auth: auth})
	resp := doBrowser(t, srv, http.MethodPost, "/auth/logout",
		&http.Cookie{Name: "hestia_access_token", Value: "at"})

	if resp.Status != http.StatusNoContent {
		t.Fatalf("狀態碼 = %d,期望 204", resp.Status)
	}
	for _, name := range []string{"hestia_access_token", "hestia_refresh_token"} {
		if c := respCookie(t, resp, name); c == nil || c.MaxAge >= 0 {
			t.Fatalf("%s 應被清掉,got %+v", name, c)
		}
	}
	rec := waitRecords(t, sink, 1)[0]
	// 與 RPC 路徑同一套 status / error_code 詞彙(Unauthenticated → denied)。
	if rec.Status != "denied" || rec.ErrorCode != connect.CodeUnauthenticated.String() {
		t.Fatalf("撤銷失敗必須留在稽核裡:status=%q code=%q", rec.Status, rec.ErrorCode)
	}
}

// 沒有任何憑證時登出是冪等的,不是錯誤。
func TestBrowserLogout_IsIdempotent(t *testing.T) {
	auth := &fakeBrowserAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	resp := doBrowser(t, srv, http.MethodPost, "/auth/logout")
	if resp.Status != http.StatusNoContent {
		t.Fatalf("狀態碼 = %d,期望 204", resp.Status)
	}
	if auth.gotLogoutAccess != "" || auth.gotLogoutRefresh != "" {
		t.Fatal("沒有憑證卻呼叫了撤銷")
	}
}

// ── cookie 認證 ─────────────────────────────────────────────────

// 只帶 cookie 也能通過受保護的 RPC(瀏覽器讀不到 HttpOnly cookie,
// 沒有這條路徑網頁端就只能把 token 交給 JS 保管)。
func TestCookieAuth_AcceptedOnProtectedRPC(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Profiles: fakeProfiles{view: &readmodel.ProfileView{PublicID: "U1"}}})
	client := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.GetProfileRequest{})
	req.Header().Set("Cookie", (&http.Cookie{Name: "hestia_access_token", Value: testToken}).String())
	res, err := client.GetProfile(context.Background(), req)
	if err != nil {
		t.Fatalf("只帶 cookie 應該通過:%v", err)
	}
	if res.Msg.GetProfile().GetPublicId() != "U1" {
		t.Fatalf("回應不對:%v", res.Msg)
	}
}

// 兩者都沒有 → Unauthenticated。這是 SameSite=Lax 擋下跨站請求後的等價保證:
// 跨站送來的請求不帶 cookie,在攔截器眼中就是「沒有憑證」。
func TestCookieAuth_MissingCredentialIsUnauthenticated(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Profiles: fakeProfiles{view: &readmodel.ProfileView{PublicID: "U1"}}})
	client := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	_, err := client.GetProfile(context.Background(), connect.NewRequest(&platformv1.GetProfileRequest{}))
	if got := codeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v,期望 Unauthenticated", got)
	}
	// 無關的 cookie 也一樣:名字不對就是沒有憑證。
	req := connect.NewRequest(&platformv1.GetProfileRequest{})
	req.Header().Set("Cookie", (&http.Cookie{Name: "other", Value: testToken}).String())
	if _, err := client.GetProfile(context.Background(), req); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("不相干的 cookie 竟然通過了:%v", err)
	}
}

// Bearer 與 cookie 同時存在時以 Bearer 為準。
//
// 順序反過來的後果是「一個恰好還留在瀏覽器裡的舊 cookie 蓋掉呼叫端明確
// 附上的憑證」——請求以另一個身分執行,而且沒有任何徵兆。
func TestCookieAuth_BearerWinsOverCookie(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Profiles: fakeProfiles{view: &readmodel.ProfileView{PublicID: "U1"}}})
	client := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)

	// 有效 Bearer + 無效 cookie → 通過(Bearer 為準)。
	ok := connect.NewRequest(&platformv1.GetProfileRequest{})
	ok.Header().Set("Authorization", "Bearer "+testToken)
	ok.Header().Set("Cookie", (&http.Cookie{Name: "hestia_access_token", Value: "garbage"}).String())
	if _, err := client.GetProfile(context.Background(), ok); err != nil {
		t.Fatalf("Bearer 有效就該通過,cookie 不該有發言權:%v", err)
	}

	// 無效 Bearer + 有效 cookie → 被拒(cookie 不得替補)。
	bad := connect.NewRequest(&platformv1.GetProfileRequest{})
	bad.Header().Set("Authorization", "Bearer garbage")
	bad.Header().Set("Cookie", (&http.Cookie{Name: "hestia_access_token", Value: testToken}).String())
	if _, err := client.GetProfile(context.Background(), bad); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v,cookie 不該替補失敗的 Bearer", codeOf(err))
	}
}

// 服務憑證 + cookie 也算混合憑證:與帶 Bearer 時同一條規則。
func TestCookieAuth_MixedWithServiceTokenIsDenied(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		Profiles:      fakeProfiles{view: &readmodel.ProfileView{PublicID: "U1"}},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: strings.Repeat("k", 32)}},
	})
	client := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.GetProfileRequest{})
	req.Header().Set("X-Service-Token", strings.Repeat("k", 32))
	req.Header().Set("Cookie", (&http.Cookie{Name: "hestia_access_token", Value: testToken}).String())
	_, err := client.GetProfile(context.Background(), req)
	if got := codeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 PermissionDenied", got)
	}
	if got := transport.ErrorReason(err); got != "mixed_credentials" {
		t.Fatalf("reason = %q,期望 mixed_credentials", got)
	}
}
