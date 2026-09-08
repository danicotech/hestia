package transport_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/transport"
)

// 裝置足跡(sessions.user_agent / ip)的入口層責任:取值、清理、不外流。
// 這兩個欄位只給裝置清單與稽核看,不參與任何判定。

// completeLogin 走一次成功的回呼,回傳 fake 收到的裝置資訊。
func completeLogin(t *testing.T, deps transport.Deps, auth *fakeOAuth, headers map[string]string) transport.DeviceInfo {
	t.Helper()
	deps.Auth = auth
	srv, _ := newServer(t, deps)
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.CompleteDiscordLoginRequest{
		Code: "oauth-code", State: oauthState,
	})
	req.Header().Set("Cookie", (&http.Cookie{Name: "hestia_oauth_state", Value: oauthState}).String())
	for k, v := range headers {
		req.Header().Set(k, v)
	}
	if _, err := client.CompleteDiscordLogin(context.Background(), req); err != nil {
		t.Fatalf("CompleteDiscordLogin: %v", err)
	}
	return auth.gotDevice
}

// User-Agent 是 client 完全可控且無長度限制的字串:不截斷就等於
// 讓任何人決定 sessions 表裡一列有多大(和 event_logs 摘要同一類問題)。
func TestDeviceInfo_UserAgentTruncated(t *testing.T) {
	auth := &fakeOAuth{}
	huge := strings.Repeat("U", 50_000)
	dev := completeLogin(t, transport.Deps{}, auth, map[string]string{"User-Agent": huge})
	if dev.UserAgent == "" {
		t.Fatal("UA 應該有值")
	}
	if len(dev.UserAgent) > 200 {
		t.Fatalf("UA 沒被截斷:%d bytes", len(dev.UserAgent))
	}
	if !strings.HasPrefix(huge, dev.UserAgent) {
		t.Fatal("截斷後應是原字串的前綴")
	}
}

// 多位元組 UA 不能被切成半個 rune(壞字元會一路寫進 DB)。
func TestDeviceInfo_UserAgentKeepsRuneBoundary(t *testing.T) {
	auth := &fakeOAuth{}
	dev := completeLogin(t, transport.Deps{}, auth,
		map[string]string{"User-Agent": strings.Repeat("瀏", 500)})
	if len(dev.UserAgent) > 200 {
		t.Fatalf("UA 沒被截斷:%d bytes", len(dev.UserAgent))
	}
	if strings.ContainsRune(dev.UserAgent, '�') || !strings.HasPrefix(strings.Repeat("瀏", 500), dev.UserAgent) {
		t.Fatalf("截斷切壞了字元:%q", dev.UserAgent)
	}
}

// 預設不信任 X-Forwarded-For:它是純請求標頭,誰都能填。
// 直接採信等於讓稽核紀錄的來源 IP 變成攻擊者可自選的欄位。
func TestDeviceInfo_ForwardedHeaderIgnoredByDefault(t *testing.T) {
	auth := &fakeOAuth{}
	dev := completeLogin(t, transport.Deps{}, auth, map[string]string{
		"X-Forwarded-For": "203.0.113.7",
	})
	if dev.IP == "203.0.113.7" {
		t.Fatal("預設不該相信 X-Forwarded-For")
	}
	if dev.IP != "127.0.0.1" && dev.IP != "::1" {
		t.Fatalf("應該用連線對端位址,got %q", dev.IP)
	}
}

// 明確開啟後才採用最左值(前提是服務只接受可信代理的連線)。
func TestDeviceInfo_ForwardedHeaderHonoredWhenTrusted(t *testing.T) {
	auth := &fakeOAuth{}
	dev := completeLogin(t, transport.Deps{TrustProxyHeaders: true}, auth, map[string]string{
		"X-Forwarded-For": "203.0.113.7, 70.41.3.18, 150.172.238.178",
	})
	if dev.IP != "203.0.113.7" {
		t.Fatalf("應取最左值,got %q", dev.IP)
	}
}

// 開了信任但 header 是垃圾:退回連線對端,不把來路不明的字串寫進 DB。
func TestDeviceInfo_GarbageForwardedFallsBackToPeer(t *testing.T) {
	auth := &fakeOAuth{}
	dev := completeLogin(t, transport.Deps{TrustProxyHeaders: true}, auth, map[string]string{
		"X-Forwarded-For": "不是IP, 203.0.113.7",
	})
	if dev.IP != "127.0.0.1" && dev.IP != "::1" {
		t.Fatalf("垃圾值應退回連線對端,got %q", dev.IP)
	}
}

// 連線對端要去掉 port:sessions.ip 存的是位址,不是連線。
func TestDeviceInfo_PeerPortStripped(t *testing.T) {
	auth := &fakeOAuth{}
	dev := completeLogin(t, transport.Deps{}, auth, nil)
	if strings.Contains(dev.IP, ":") && dev.IP != "::1" {
		t.Fatalf("IP 不該帶 port:%q", dev.IP)
	}
}

// RefreshSession 換發後是新的一列 session,同樣要記裝置。
func TestDeviceInfo_RefreshSessionAlsoRecordsDevice(t *testing.T) {
	auth := &fakeOAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.RefreshSessionRequest{RefreshToken: "rt"})
	req.Header().Set("User-Agent", "hestia-test-agent/1.0")
	if _, err := client.RefreshSession(context.Background(), req); err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	if auth.gotDevice.UserAgent != "hestia-test-agent/1.0" {
		t.Fatalf("UA = %q", auth.gotDevice.UserAgent)
	}
	if auth.gotDevice.IP == "" {
		t.Fatal("IP 應該有值")
	}
}

// UA 與 IP 不得進 event_logs:UA 是自由文字、IP 是個資,
// 而 sessions 表已經是它們的權威位置(一個概念一個位置)。
func TestDeviceInfo_NotAudited(t *testing.T) {
	const ua = "Mozilla/5.0 (X11; TestOS) 稽核不該有我"
	auth := &fakeOAuth{}
	srv, sink := newServer(t, transport.Deps{Auth: auth, TrustProxyHeaders: true})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.CompleteDiscordLoginRequest{
		Code: "oauth-code", State: oauthState,
	})
	req.Header().Set("Cookie", (&http.Cookie{Name: "hestia_oauth_state", Value: oauthState}).String())
	req.Header().Set("User-Agent", ua)
	req.Header().Set("X-Forwarded-For", "203.0.113.7")
	if _, err := client.CompleteDiscordLogin(context.Background(), req); err != nil {
		t.Fatalf("CompleteDiscordLogin: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	blob := string(rec.Request) + string(rec.Response)
	for _, secret := range []string{ua, "稽核不該有我", "203.0.113.7", "127.0.0.1", "Mozilla"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("裝置資訊外洩到 event_logs(%q):%s", secret, blob)
		}
	}
}
