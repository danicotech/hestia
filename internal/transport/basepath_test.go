package transport_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/transport"
)

// 掛載前綴(HESTIA_BASE_PATH)。
//
// 要釘住的性質有四:
//
//  1. 前綴為空時**行為與從前一字不差**(這一整份既有測試就是那個證明)。
//  2. 設了前綴,全部端點都在前綴底下且可用。
//  3. cookie 的 Path 跟著前綴走 —— 寫死 "/" 或 "/auth" 都會壞。
//  4. **啟動時的存取層級斷言不受前綴影響。** 那份斷言守的是 fail-open 的方向,
//     被前綴弄失效的話,漏配置的服務 RPC 會變成任何登入使用者都能呼叫。

const testBase = "/api"

func TestNormalizeBasePath(t *testing.T) {
	// 只容忍兩種寫法差異:前後空白(env 從 compose / .env 讀進來常帶著)
	// 與結尾斜線。兩者都沒有第二種可能的意思,修正不會掩蓋任何錯誤。
	ok := map[string]string{
		"":         "",
		"/":        "",
		"/api":     "/api",
		"/api/":    "/api",
		"  /api  ": "/api",
		"/api\n":   "/api",
		"/a/b":     "/a/b",
		"/api/v1/": "/api/v1",
	}
	for in, want := range ok {
		got, err := transport.NormalizeBasePath(in)
		if err != nil {
			t.Errorf("NormalizeBasePath(%q) 意外失敗:%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeBasePath(%q) = %q,期望 %q", in, got, want)
		}
	}

	// 壞設定一律回錯。前綴寫錯不會有徵兆 —— 端點只是全部搬到別的位置,
	// 在測試環境看起來像 404,所以必須在啟動時就炸。
	bad := []string{
		"api",                          // 沒有開頭斜線
		"//api",                        // 空片段
		"/api//v1",                     // 空片段
		"/api/../etc",                  // 路徑穿越
		"/api?x=1",                     // query
		"/api#frag",                    // fragment
		"/ap\ni",                       // 控制字元(header 注入)
		"/api b",                       // 空白
		"/api\\x",                      // 反斜線
		"/%2e%2e",                      // 百分比編碼
		"http://evil.com/api",          // 絕對 URL
		"/" + strings.Repeat("a", 100), // 過長
	}
	for _, in := range bad {
		if got, err := transport.NormalizeBasePath(in); err == nil {
			t.Errorf("NormalizeBasePath(%q) 竟然通過,回了 %q", in, got)
		}
	}
}

// 壞設定要讓服務**起不來**,而不是靜靜地掛在奇怪的位置。
func TestNewRejectsBadBasePath(t *testing.T) {
	for _, bad := range []string{"api", "//api", "http://evil.com", "/ap\ni"} {
		if _, err := transport.New(transport.Deps{
			BasePath: bad,
			Logger:   slog.New(slog.DiscardHandler),
		}); err == nil {
			t.Fatalf("BasePath=%q 竟然開得起來", bad)
		}
	}
}

// newPrefixedServer 組出「前端在 /,API 在 /api」的部署形狀,
// 與 cmd/server 的掛法一致(healthz 與 ConnectRPC 共用同一個 mux)。
func newPrefixedServer(t *testing.T, deps transport.Deps) (*httptest.Server, *recordSink) {
	t.Helper()
	deps.BasePath = testBase
	sink := &recordSink{}
	if deps.EventLog == nil {
		deps.EventLog = sink
	}
	if deps.Authenticator == nil {
		deps.Authenticator = fakeAuth{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	h, err := transport.New(deps)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+testBase+"/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle(testBase+"/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = h.Shutdown(context.Background())
	})
	return srv, sink
}

// healthz 與 ConnectRPC 都在前綴底下且可用;前綴外一律 404。
func TestBasePath_EndpointsMoveUnderPrefix(t *testing.T) {
	srv, _ := newPrefixedServer(t, transport.Deps{
		Catalog: fakeCatalog{items: []readmodel.ItemView{{PublicID: "I1"}}},
	})

	if resp := doBrowser(t, srv, http.MethodGet, testBase+"/healthz"); resp.Status != http.StatusOK {
		t.Fatalf("%s/healthz = %d", testBase, resp.Status)
	}
	if resp := doBrowser(t, srv, http.MethodGet, "/healthz"); resp.Status == http.StatusOK {
		t.Fatal("前綴外的 /healthz 不該還在")
	}

	// ConnectRPC:client 的 base URL 帶前綴,procedure 照常接在後面。
	client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL+testBase)
	res, err := client.ListItems(context.Background(),
		connect.NewRequest(&platformv1.ListItemsRequest{}))
	if err != nil {
		t.Fatalf("前綴底下的 ConnectRPC 應該可用:%v", err)
	}
	if len(res.Msg.GetItems()) != 1 {
		t.Fatalf("回應不對:%v", res.Msg)
	}

	// 前綴外的同一支 RPC 不存在(它整個搬走了)。
	root := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	if _, err := root.ListItems(context.Background(),
		connect.NewRequest(&platformv1.ListItemsRequest{})); err == nil {
		t.Fatal("前綴外還打得到 RPC —— 前綴根本沒生效")
	}
}

// 四條瀏覽器登入路由也在前綴底下,而且 cookie 的 Path 跟著前綴走。
func TestBasePath_BrowserAuthRoutesAndCookiePaths(t *testing.T) {
	auth := &fakeBrowserAuth{redirect: "/tournaments/1"}
	srv, _ := newPrefixedServer(t, transport.Deps{Auth: auth})

	// start
	start := doBrowser(t, srv, http.MethodGet, testBase+"/auth/discord/start")
	if start.Status != http.StatusFound {
		t.Fatalf("start = %d", start.Status)
	}
	state := respCookie(t, start, "hestia_oauth_state")
	if state == nil || state.Path != testBase {
		t.Fatalf("state cookie 的 Path 沒跟著前綴走:%+v", state)
	}

	// callback
	cb := doBrowser(t, srv, http.MethodGet,
		testBase+"/auth/discord/callback?code=c&state="+browserState,
		&http.Cookie{Name: "hestia_oauth_state", Value: browserState})
	if cb.Status != http.StatusFound {
		t.Fatalf("callback = %d", cb.Status)
	}
	// 導回的是**前端**的路徑,不帶 API 的前綴 —— 前端在站台根目錄。
	if got := cb.Header.Get("Location"); got != "/tournaments/1" {
		t.Fatalf("導回 %q,期望 /tournaments/1(前端不在 API 前綴底下)", got)
	}
	access := respCookie(t, cb, "hestia_access_token")
	if access == nil || access.Path != testBase {
		t.Fatalf("access_token 的 Path 必須是 %q:%+v", testBase, access)
	}
	refresh := respCookie(t, cb, "hestia_refresh_token")
	if refresh == nil || refresh.Path != testBase+"/auth" {
		t.Fatalf("refresh_token 的 Path 必須是 %q:%+v", testBase+"/auth", refresh)
	}

	// refresh / logout
	rf := doBrowser(t, srv, http.MethodPost, testBase+"/auth/refresh",
		&http.Cookie{Name: "hestia_refresh_token", Value: "old"})
	if rf.Status != http.StatusNoContent {
		t.Fatalf("refresh = %d", rf.Status)
	}
	lo := doBrowser(t, srv, http.MethodPost, testBase+"/auth/logout",
		&http.Cookie{Name: "hestia_access_token", Value: "at"})
	if lo.Status != http.StatusNoContent {
		t.Fatalf("logout = %d", lo.Status)
	}
	if got := respCookie(t, lo, "hestia_refresh_token").Path; got != testBase+"/auth" {
		t.Fatalf("清除指令的 Path = %q,與種下時不同就刪不掉", got)
	}
}

// **這是本組最重要的一條**:前綴不得讓存取層級的判定失效。
//
// 攔截器比對的是 req.Spec().Procedure(生成碼裡的常數),不是 URL。
// 若哪天它改成從 URL 解析,下面每一條都會落空 —— 而那個方向是 fail open。
func TestBasePath_AccessLevelsStillEnforced(t *testing.T) {
	srv, _ := newPrefixedServer(t, transport.Deps{
		Profiles: fakeProfiles{view: &readmodel.ProfileView{PublicID: "U1"}},
		Catalog:  fakeCatalog{},
		AdminEcon: &fakeAdmin{
			res: nil,
		},
	})

	// 1. 需要登入的 RPC:沒憑證照樣 Unauthenticated。
	me := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL+testBase)
	if _, err := me.GetProfile(context.Background(),
		connect.NewRequest(&platformv1.GetProfileRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v,期望 Unauthenticated", codeOf(err))
	}

	// 2. 只收服務身分的 RPC:使用者 token 打進去照樣被擋,而且是同一個 reason。
	act := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL+testBase)
	_, err := act.RecordMessages(context.Background(),
		withToken(connect.NewRequest(&platformv1.RecordMessagesRequest{})))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 PermissionDenied", codeOf(err))
	}
	if got := transport.ErrorReason(err); got != "user_on_service_rpc" {
		t.Fatalf("reason = %q,期望 user_on_service_rpc —— 服務前綴比對失效了", got)
	}

	// 3. 需要授權的管理 RPC:沒有 Authorizer 一律 PermissionDenied。
	admin := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL+testBase)
	_, err = admin.Grant(context.Background(),
		withToken(connect.NewRequest(&platformv1.GrantRequest{})))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 PermissionDenied", codeOf(err))
	}

	// 4. 公開白名單照常成立(白名單的鍵也是 procedure 全名)。
	shop := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL+testBase)
	if _, err := shop.ListItems(context.Background(),
		connect.NewRequest(&platformv1.ListItemsRequest{})); err != nil {
		t.Fatalf("公開型錄在前綴底下應該免登入:%v", err)
	}
}

// event_logs 的 action 維持 procedure 全名:前綴是部署細節,
// 讓它滲進資料表會使同一支 RPC 在不同部署下長成兩個值(專案第 9 條)。
func TestBasePath_EventLogActionKeepsProcedureName(t *testing.T) {
	srv, sink := newPrefixedServer(t, transport.Deps{Catalog: fakeCatalog{}})
	client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL+testBase)
	if _, err := client.ListItems(context.Background(),
		connect.NewRequest(&platformv1.ListItemsRequest{})); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Action != platformv1connect.ShopServiceListItemsProcedure {
		t.Fatalf("action = %q,期望 %q(不帶前綴)",
			rec.Action, platformv1connect.ShopServiceListItemsProcedure)
	}
}

// 前綴底下的探測流量照樣留痕,而且不把攻擊者可控的路徑原樣存進表。
func TestBasePath_ProbeTrafficStillAudited(t *testing.T) {
	srv, sink := newPrefixedServer(t, transport.Deps{})
	resp := doBrowser(t, srv, http.MethodGet, testBase+"/.env")
	if resp.Status != http.StatusNotFound {
		t.Fatalf("狀態碼 = %d,期望 404", resp.Status)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Action != "(non-rpc path)" {
		t.Fatalf("action = %q,探測路徑不該原樣入庫", rec.Action)
	}
}

// 前綴的邊界必須落在路徑片段上:/api 不該吃掉 /apifoo。
func TestBasePath_DoesNotMatchPartialSegment(t *testing.T) {
	srv, _ := newPrefixedServer(t, transport.Deps{Auth: &fakeBrowserAuth{}})
	resp := doBrowser(t, srv, http.MethodGet, "/apifoo/auth/discord/start")
	if resp.Status == http.StatusFound {
		t.Fatal("/apifoo 被當成 /api 的前綴匹配了")
	}
}
