package transport_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/transport"
)

// /login 是**前端**的網址,在 theatron 上線前由 hestia 暫時轉址。
// 它刻意不在 basePath 底下——放進 /api 就等於承認它是 API 的一部分,
// 而前端接手時網址就得改,已經發出去的 /bind 連結會全部壞掉。

func shimServer(t *testing.T, basePath string) *httptest.Server {
	t.Helper()
	srv, _ := newServer(t, transport.Deps{BasePath: basePath})
	return srv
}

// 不跟隨轉址:要驗的正是轉去哪,跟過去只會看到終點。
func noRedirect(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func TestLoginShim_RedirectsToOAuthStart(t *testing.T) {
	srv := shimServer(t, "/api")
	res, err := noRedirect(srv).Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("狀態碼 = %d, want 303", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/api/auth/discord/start" {
		t.Fatalf("Location = %q", got)
	}
}

// /bind 的連結會帶著使用者原本要去的頁面。掉了它,登入完成後每個人都落在首頁。
func TestLoginShim_KeepsQuery(t *testing.T) {
	srv := shimServer(t, "/api")
	res, err := noRedirect(srv).Get(srv.URL + "/login?redirect=%2Ftournaments%2F1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location 不是合法網址: %v", err)
	}
	if loc.Path != "/api/auth/discord/start" {
		t.Fatalf("路徑 = %q", loc.Path)
	}
	if got := loc.Query().Get("redirect"); got != "/tournaments/1" {
		t.Fatalf("redirect = %q", got)
	}
}

// 沒有前綴的部署(本機開發)一樣要能用。
func TestLoginShim_WorksWithoutBasePath(t *testing.T) {
	srv := shimServer(t, "")
	res, err := noRedirect(srv).Get(srv.URL + "/login")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get("Location"); got != "/auth/discord/start" {
		t.Fatalf("Location = %q", got)
	}
}

// 只接管 /login 這一條。多吃一個字元都會擋住前端之後要用的路徑。
func TestLoginShim_DoesNotSwallowOtherPaths(t *testing.T) {
	srv := shimServer(t, "/api")
	for _, path := range []string{"/login/", "/login/extra", "/loginx", "/"} {
		res, err := noRedirect(srv).Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusSeeOther {
			t.Fatalf("%s 不該被轉址", path)
		}
	}
}

// 轉址不是「送出資料」的入口:POST 進來要明確擋掉,
// 否則之後前端真的在這裡放表單時,行為會依部署階段而不同。
func TestLoginShim_RejectsNonGet(t *testing.T) {
	srv := shimServer(t, "/api")
	res, err := noRedirect(srv).Post(srv.URL+"/login", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("狀態碼 = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow = %q", got)
	}
}
