package transport_test

import (
	"context"
	"errors"
	"net/http"
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

// OAuth state 綁瀏覽器的入口層測試(login CSRF / session fixation)。
//
// 這裡驗的是**搬運**:cookie 有沒有種下、有沒有被原封不動送進 identity、
// 失敗時有沒有清掉、值有沒有外洩到稽核。比對本身是 identity 的事,
// 所以 fakeOAuth 用與 identity 相同的規則(相等且非空)當替身。

const oauthState = "state-token-abc123"

type fakeOAuth struct {
	gotState  string
	gotCookie string
	gotDevice transport.DeviceInfo
	seen      bool
}

func (f *fakeOAuth) StartDiscordLogin(context.Context, string) (string, string, error) {
	return "https://discord.com/oauth2/authorize?state=" + oauthState, oauthState, nil
}

// CompleteDiscordLogin 複製 identity 的判定:cookie 必須存在且與 state 相同。
func (f *fakeOAuth) CompleteDiscordLogin(_ context.Context, _, state, fromCookie string, dev transport.DeviceInfo) (
	*transport.SessionView, *readmodel.ProfileView, error,
) {
	f.seen = true
	f.gotState, f.gotCookie, f.gotDevice = state, fromCookie, dev
	if fromCookie == "" || fromCookie != state {
		return nil, nil, identity.ErrStateMismatch
	}
	return &transport.SessionView{AccessToken: "at", RefreshToken: "rt", UserPublicID: "U1"},
		&readmodel.ProfileView{PublicID: "U1"}, nil
}

func (f *fakeOAuth) RefreshSession(_ context.Context, _ string, dev transport.DeviceInfo) (*transport.SessionView, error) {
	f.seen = true
	f.gotDevice = dev
	return &transport.SessionView{}, nil
}

func (f *fakeOAuth) Logout(context.Context, string, string) error { return nil }

func parseSetCookie(t *testing.T, header http.Header, name string) *http.Cookie {
	t.Helper()
	resp := http.Response{Header: header}
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// StartDiscordLogin 必須把 state 種進 HttpOnly cookie,屬性要對。
func TestStartDiscordLogin_SetsHardenedStateCookie(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Auth: &fakeOAuth{}})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	res, err := client.StartDiscordLogin(context.Background(),
		connect.NewRequest(&platformv1.StartDiscordLoginRequest{}))
	if err != nil {
		t.Fatalf("StartDiscordLogin: %v", err)
	}
	c := parseSetCookie(t, res.Header(), "hestia_oauth_state")
	if c == nil {
		t.Fatal("沒有種下 state cookie,登入流程會靜默失敗")
	}
	if c.Value != res.Msg.GetState() {
		t.Fatalf("cookie 值必須與回應的 state 一致:%q vs %q", c.Value, res.Msg.GetState())
	}
	if !c.HttpOnly {
		t.Fatal("必須 HttpOnly(否則 XSS 就能偷走)")
	}
	if !c.Secure {
		t.Fatal("預設必須 Secure")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v,OAuth 回呼需要 Lax", c.SameSite)
	}
	if c.MaxAge <= 0 || time.Duration(c.MaxAge)*time.Second > identity.StateTTL {
		t.Fatalf("Max-Age = %d,必須是正值且不超過 StateTTL", c.MaxAge)
	}
	// Path 必須涵蓋回呼那支 RPC,否則瀏覽器根本不會送出來。
	callback := platformv1connect.AuthServiceCompleteDiscordLoginProcedure
	path := c.Path
	if !strings.HasPrefix(callback, path) {
		t.Fatalf("cookie Path %q 涵蓋不到回呼 procedure %q", path, callback)
	}
}

// 回呼:cookie 缺失 / 不符 / 兩者皆空都必須被擋。
// 「沒有 cookie 就跳過檢查」是 CSRF 防護最常見的破口。
func TestCompleteDiscordLogin_StateMustMatchCookie(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		cookie string
		want   connect.Code
	}{
		{"cookie 缺失", oauthState, "", connect.CodePermissionDenied},
		{"cookie 與 state 不符", oauthState, "attacker-state", connect.CodePermissionDenied},
		{"state 空、cookie 有值", "", oauthState, connect.CodeInvalidArgument},
		{"兩者皆空", "", "", connect.CodeInvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth := &fakeOAuth{}
			srv, sink := newServer(t, transport.Deps{Auth: auth})
			client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
			req := connect.NewRequest(&platformv1.CompleteDiscordLoginRequest{
				Code: "oauth-code", State: c.state,
			})
			if c.cookie != "" {
				req.Header().Set("Cookie", (&http.Cookie{
					Name: "hestia_oauth_state", Value: c.cookie,
				}).String())
			}
			_, err := client.CompleteDiscordLogin(context.Background(), req)
			if got := codeOf(err); got != c.want {
				t.Fatalf("code = %v, 期望 %v(err=%v)", got, c.want, err)
			}
			// 失敗也要把一次性的 state cookie 清掉。
			var ce *connect.Error
			if errors.As(err, &ce) {
				if cookie := parseSetCookie(t, ce.Meta(), "hestia_oauth_state"); cookie == nil || cookie.MaxAge >= 0 {
					t.Fatalf("失敗回應應清掉 state cookie,got %+v", cookie)
				}
			}
			// state / cookie 的值都不能進稽核(10 分鐘內是有效憑證)。
			rec := waitRecords(t, sink, 1)[0]
			blob := string(rec.Request) + string(rec.Response)
			for _, secret := range []string{c.state, c.cookie, oauthState} {
				if secret != "" && strings.Contains(blob, secret) {
					t.Fatalf("state 值外洩到 event_logs:%s", blob)
				}
			}
		})
	}
}

// 成功路徑:cookie 值原封不動送進 identity,且回應立刻清掉 cookie。
func TestCompleteDiscordLogin_PassesCookieThroughAndClears(t *testing.T) {
	auth := &fakeOAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: auth})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.CompleteDiscordLoginRequest{
		Code: "oauth-code", State: oauthState,
	})
	req.Header().Set("Cookie", (&http.Cookie{Name: "hestia_oauth_state", Value: oauthState}).String())
	res, err := client.CompleteDiscordLogin(context.Background(), req)
	if err != nil {
		t.Fatalf("CompleteDiscordLogin: %v", err)
	}
	if !auth.seen {
		t.Fatal("沒有呼叫到 identity")
	}
	if auth.gotCookie != oauthState || auth.gotState != oauthState {
		t.Fatalf("state/cookie 沒有原封不動送進去:%q / %q", auth.gotState, auth.gotCookie)
	}
	if res.Msg.GetSession().GetAccessToken() != "at" {
		t.Fatalf("回應不對:%v", res.Msg)
	}
	c := parseSetCookie(t, res.Header(), "hestia_oauth_state")
	if c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("成功後應立刻清掉一次性 cookie,got %+v", c)
	}
}

// Deps.StateCookiePath 可收窄 cookie 範圍(服務掛在前綴下時要用)。
func TestStateCookiePathIsConfigurable(t *testing.T) {
	const path = "/hestia.platform.v1.AuthService"
	srv, _ := newServer(t, transport.Deps{Auth: &fakeOAuth{}, StateCookiePath: path})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	res, err := client.StartDiscordLogin(context.Background(),
		connect.NewRequest(&platformv1.StartDiscordLoginRequest{}))
	if err != nil {
		t.Fatalf("StartDiscordLogin: %v", err)
	}
	c := parseSetCookie(t, res.Header(), "hestia_oauth_state")
	if c == nil || c.Path != path {
		t.Fatalf("Path = %+v,期望 %q", c, path)
	}
	if !c.Secure {
		t.Fatal("Secure 永遠是開的,沒有關掉的開關")
	}
}
