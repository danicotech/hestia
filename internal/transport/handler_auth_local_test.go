package transport_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/transport"
)

// AuthService.LocalLogin(裁判的本地登入)在入口層的三件事:
// 未注入 → Unimplemented、缺參數 → InvalidArgument、失敗 → 固定 reason。
//
// LocalAuthService 是**選配**能力:注入的 AuthService 沒實作它時,
// 回的必須是 Unimplemented 而不是 panic 或 Internal。

// discordOnlyAuth 只實作 AuthService,刻意**不**實作 LocalAuthService。
type discordOnlyAuth struct{}

func (discordOnlyAuth) StartDiscordLogin(context.Context, string) (string, string, error) {
	return "https://discord.test/authorize", "state", nil
}

func (discordOnlyAuth) CompleteDiscordLogin(
	context.Context, string, string, string, transport.DeviceInfo,
) (*transport.SessionView, *readmodel.ProfileView, error) {
	return nil, nil, errors.New("不該被呼叫")
}

func (discordOnlyAuth) RefreshSession(
	context.Context, string, transport.DeviceInfo,
) (*transport.SessionView, error) {
	return nil, errors.New("不該被呼叫")
}

func (discordOnlyAuth) Logout(context.Context, string, string) error { return nil }

// localAuth 兩者都實作。err 非 nil 時 LocalLogin 一律失敗。
type localAuth struct {
	discordOnlyAuth
	err   error
	calls []string // 依序記下收到的 (login_name, passcode)
}

func (a *localAuth) LocalLogin(
	_ context.Context, loginName, passcode string, _ transport.DeviceInfo,
) (*transport.SessionView, *readmodel.ProfileView, error) {
	a.calls = append(a.calls, loginName, passcode)
	if a.err != nil {
		return nil, nil, a.err
	}
	return &transport.SessionView{
			AccessToken:  "access",
			RefreshToken: "refresh",
			UserPublicID: "01JUDGE0000000000000000000",
		},
		&readmodel.ProfileView{PublicID: "01JUDGE0000000000000000000"}, nil
}

func TestLocalLoginUnimplementedWithoutCapability(t *testing.T) {
	for name, svc := range map[string]transport.AuthService{
		"完全未注入":       nil,
		"只實作 Discord": discordOnlyAuth{},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := newServer(t, transport.Deps{Auth: svc})
			client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
			_, err := client.LocalLogin(context.Background(),
				connect.NewRequest(&platformv1.LocalLoginRequest{LoginName: "lin", Passcode: "ABCD2345EFGH"}))
			if codeOf(err) != connect.CodeUnimplemented {
				t.Fatalf("應回 unimplemented,got %v", err)
			}
		})
	}
}

func TestLocalLoginRequiresBothFields(t *testing.T) {
	svc := &localAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: svc})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)

	for name, req := range map[string]*platformv1.LocalLoginRequest{
		"缺登入名":   {Passcode: "ABCD2345EFGH"},
		"缺通行碼":   {LoginName: "lin"},
		"兩個都空":   {},
		"只有空白字元": {LoginName: "   ", Passcode: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.LocalLogin(context.Background(), connect.NewRequest(req))
			if codeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("應回 invalid_argument,got %v", err)
			}
		})
	}
	if len(svc.calls) != 0 {
		t.Fatalf("空欄位不該打到實作:%v", svc.calls)
	}
}

// 成功路徑:session 與 profile 都轉得出去,而且入口層不動輸入值
// (正規化的權威在實作端 —— 兩邊各做一次就會有兩個答案)。
func TestLocalLoginPassesThroughAndReturnsSession(t *testing.T) {
	svc := &localAuth{}
	srv, _ := newServer(t, transport.Deps{Auth: svc})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)

	res, err := client.LocalLogin(context.Background(),
		connect.NewRequest(&platformv1.LocalLoginRequest{LoginName: "  LIN  ", Passcode: " abcd2345efgh "}))
	if err != nil {
		t.Fatalf("LocalLogin: %v", err)
	}
	if res.Msg.GetSession().GetAccessToken() != "access" {
		t.Errorf("session 沒帶回來:%v", res.Msg.GetSession())
	}
	if res.Msg.GetProfile().GetPublicId() != "01JUDGE0000000000000000000" {
		t.Errorf("profile 沒帶回來:%v", res.Msg.GetProfile())
	}
	// 入口層只做 TrimSpace(擋空字串用),大小寫原樣往下送。
	if got := svc.calls; len(got) != 2 || got[0] != "LIN" || got[1] != "abcd2345efgh" {
		t.Errorf("實作收到 %v,入口層不該改寫大小寫", got)
	}
}

// 失敗一律是同一個 code + 同一個 reason:前端不必(也不該)分辨成因。
func TestLocalLoginFailureCarriesStableReason(t *testing.T) {
	svc := &localAuth{err: identity.ErrInvalidCredentials}
	srv, _ := newServer(t, transport.Deps{Auth: svc})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)

	_, err := client.LocalLogin(context.Background(),
		connect.NewRequest(&platformv1.LocalLoginRequest{LoginName: "nobody", Passcode: "WRONG2345XYZQ"}))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("應回 unauthenticated,got %v", err)
	}
	if got := transport.ErrorReason(err); got != "local_invalid_credentials" {
		t.Fatalf("reason = %q,要 local_invalid_credentials", got)
	}
}
