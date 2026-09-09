package transport

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
)

// 掛載前綴與稽核 action 的內部測試。
//
// 這裡驗的是兩件外部測試看不到的事:前綴剝除的邊界條件,以及
// **啟動斷言在有前綴時照樣會炸**。後者是重點 —— 那份斷言守的是 fail-open
// 的方向,它若因為前綴而失效,漏配置的服務 RPC 會變成任何登入使用者都能呼叫,
// 而且完全沒有徵兆。

func TestStripBasePath(t *testing.T) {
	cases := []struct {
		path, base string
		want       string
		wantOK     bool
	}{
		{"/hestia.platform.v1.MeService/GetProfile", "", "/hestia.platform.v1.MeService/GetProfile", true},
		{"/api/hestia.platform.v1.MeService/GetProfile", "/api", "/hestia.platform.v1.MeService/GetProfile", true},
		{"/api", "/api", "/", true},
		{"/api/", "/api", "/", true},
		{"/api/auth/refresh", "/api", "/auth/refresh", true},
		// 邊界必須落在路徑片段上:/api 不該吃掉 /apifoo。
		{"/apifoo", "/api", "/apifoo", false},
		{"/other/x", "/api", "/other/x", false},
	}
	for _, c := range cases {
		got, ok := stripBasePath(c.path, c.base)
		if got != c.want || ok != c.wantOK {
			t.Errorf("stripBasePath(%q, %q) = (%q, %v),期望 (%q, %v)",
				c.path, c.base, got, ok, c.want, c.wantOK)
		}
	}
}

// action 必須是 procedure 全名,不帶前綴 —— 同一支 RPC 在不同部署下
// 不該在稽核表上長成兩個值(專案第 9 條)。
func TestAuditActionStripsBasePath(t *testing.T) {
	const procedure = platformv1connect.MeServiceGetProfileProcedure
	cases := []struct {
		path, base, want string
	}{
		{procedure, "", procedure},
		{"/api" + procedure, "/api", procedure},
		// 前綴外的探測流量:不原樣入庫(event_logs 保留 180 天)。
		{"/wp-login.php", "/api", unknownAction},
		{"/api/.env", "/api", unknownAction},
		{procedure, "/api", unknownAction}, // 設了前綴卻沒帶 = 不是我們的 RPC
	}
	for _, c := range cases {
		if got := auditAction(c.path, c.base); got != c.want {
			t.Errorf("auditAction(%q, %q) = %q,期望 %q", c.path, c.base, got, c.want)
		}
	}
}

// 超長的假 procedure 照樣截斷(前綴剝除之後,規則不變)。
func TestAuditActionTruncatesUnderBasePath(t *testing.T) {
	long := "/api/" + string(protoPackage) + "." + strings.Repeat("A", 4000)
	got := auditAction(long, "/api")
	if len(got) > summaryMaxStringBytes {
		t.Fatalf("action 長度 %d 超過上限 %d", len(got), summaryMaxStringBytes)
	}
	if strings.HasPrefix(got, "/api/") {
		t.Fatalf("前綴沒被剝掉:%q", got)
	}
}

// **壞的存取層級設定 + 前綴 → 服務照樣起不來。**
//
// 前綴一度是這道斷言最可能的破口:若 procedure 改成從 URL 解析,
// 這裡就會靜靜通過。它現在讀的是 proto descriptor,與 URL 無關。
func TestAccessLevelAssertionStillFiresUnderBasePath(t *testing.T) {
	defer swapStrings(&serviceIdentityServices, nil)()
	defer swapStrings(&subjectlessServices, nil)()

	_, err := New(Deps{BasePath: "/api", Logger: slog.New(slog.DiscardHandler)})
	if err == nil {
		t.Fatal("漏登記卻在有前綴時通過了 —— 這道斷言守的正是 fail open 的方向")
	}
	if !strings.Contains(err.Error(), "serviceIdentityServices") {
		t.Fatalf("錯誤訊息沒指出問題在哪:%v", err)
	}
}

// 出貨設定 + 前綴要開得起來(上面那條測試才證明得了「炸的是壞設定」)。
func TestShippedConfigStartsUnderBasePath(t *testing.T) {
	srv, err := New(Deps{BasePath: "/api", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("出貨設定 + /api 應該開得起來:%v", err)
	}
	if srv == nil {
		t.Fatal("New 回了 nil server")
	}
}

// state cookie 的 Path 推導:明確指定 > 掛載前綴 > "/"。
func TestStateCookiePathPrecedence(t *testing.T) {
	cases := []struct {
		explicit, base, want string
	}{
		{"", "", "/"},
		{"", "/api", "/api"},
		{"/custom", "/api", "/custom"},
		{"/custom", "", "/custom"},
	}
	for _, c := range cases {
		if got := newStateCookieConfig(c.explicit, c.base).path; got != c.want {
			t.Errorf("newStateCookieConfig(%q, %q) = %q,期望 %q",
				c.explicit, c.base, got, c.want)
		}
	}
	// state cookie 必須涵蓋**兩個**回呼入口:瀏覽器路由與 ConnectRPC。
	// 兩者只在掛載前綴那一層有共同祖先,所以 Path 就是前綴,不能再窄。
	const base = "/api"
	path := newStateCookieConfig("", base).path
	for _, entry := range []string{
		base + browserCallbackPath,
		base + platformv1connect.AuthServiceCompleteDiscordLoginProcedure,
	} {
		if !strings.HasPrefix(entry, path) {
			t.Fatalf("cookie Path %q 涵蓋不到回呼入口 %q", path, entry)
		}
	}
}

// session cookie 的 Path 也跟著前綴走,而且 refresh 比 access 窄。
func TestSessionCookiePaths(t *testing.T) {
	root := newSessionCookieConfig("")
	if root.accessPath() != "/" || root.refreshPath() != "/auth" {
		t.Fatalf("沒有前綴時:access=%q refresh=%q", root.accessPath(), root.refreshPath())
	}
	api := newSessionCookieConfig("/api")
	if api.accessPath() != "/api" || api.refreshPath() != "/api/auth" {
		t.Fatalf("有前綴時:access=%q refresh=%q", api.accessPath(), api.refreshPath())
	}
	// refresh 必須是 access 的真子集:30 天的長效憑證不該跟著每一次 API 請求走。
	if !strings.HasPrefix(api.refreshPath(), api.accessPath()) ||
		api.refreshPath() == api.accessPath() {
		t.Fatalf("refresh 的範圍沒有比 access 窄:%q vs %q", api.refreshPath(), api.accessPath())
	}
	// 換發端點確實落在 refresh cookie 的範圍內(否則它永遠收不到那把 token)。
	for _, p := range []string{"/api" + browserRefreshPath, "/api" + browserLogoutPath} {
		if !strings.HasPrefix(p, api.refreshPath()) {
			t.Fatalf("%q 收不到 refresh cookie(Path=%q)", p, api.refreshPath())
		}
	}
}
