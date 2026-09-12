package transport

import (
	"context"
	"net/netip"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
)

// authHandler 只是契約的落點:svc 未注入時全部 RPC 一律回 Unimplemented,
// 實作(Discord OAuth、本地憑證、session 簽發)由 identity 層補上並在 main 注入。
//
// LocalLogin 多一道門檻:svc 還要滿足 LocalAuthService(選配能力),
// 沒有就跟未注入一樣是 Unimplemented。
//
// 認證攔截器對本服務整體豁免(publicProcedures),因為登入前本來就沒有 token。
type authHandler struct {
	svc     AuthService
	cookie  stateCookieConfig
	trusted []netip.Prefix
}

func (h authHandler) StartDiscordLogin(
	ctx context.Context, req *connect.Request[platformv1.StartDiscordLoginRequest],
) (*connect.Response[platformv1.StartDiscordLoginResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("AuthService.StartDiscordLogin")
	}
	url, state, err := h.svc.StartDiscordLogin(ctx, strings.TrimSpace(req.Msg.GetRedirectUri()))
	if err != nil {
		return nil, toConnectError(err)
	}
	res := connect.NewResponse(&platformv1.StartDiscordLoginResponse{
		AuthorizeUrl: url, State: state,
	})
	// state 同時寫進 HttpOnly cookie:回呼時兩邊必須相符,
	// 攻擊者無法替受害者的瀏覽器準備這個 cookie(login CSRF 防線)。
	setCookie(res, h.cookie.issue(state))
	return res, nil
}

func (h authHandler) CompleteDiscordLogin(
	ctx context.Context, req *connect.Request[platformv1.CompleteDiscordLoginRequest],
) (*connect.Response[platformv1.CompleteDiscordLoginResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("AuthService.CompleteDiscordLogin")
	}
	code := strings.TrimSpace(req.Msg.GetCode())
	state := strings.TrimSpace(req.Msg.GetState())
	// cookie 讀不到就是空字串,**照樣往下送**:
	// 「沒有 cookie 就跳過檢查」是 CSRF 防護最常見的破口。
	// 判定權威在 identity(它會回 ErrStateMismatch),入口層只負責搬運。
	fromCookie := stateFromCookie(req.Header())
	clear := h.cookie.clear()
	if code == "" || state == "" {
		return nil, clearCookieOnError(invalidArgument("code 與 state 必填"), clear)
	}
	dev := deviceInfo(req.Peer().Addr, req.Header(), h.trusted)
	session, profile, err := h.svc.CompleteDiscordLogin(ctx, code, state, fromCookie, dev)
	if err != nil {
		// 失敗也要清:一次性憑證留著只是多給一次重放機會。
		return nil, clearCookieOnError(toConnectError(err), clear)
	}
	res := connect.NewResponse(&platformv1.CompleteDiscordLoginResponse{
		Session: sessionToProto(session),
		Profile: profileToProto(profile),
	})
	setCookie(res, clear)
	return res, nil
}

// LocalLogin 是不經 Discord 的登入(裁判用,見 LocalAuthService)。
//
// 參數檢查刻意只到「兩個都不是空的」為止:再往下驗(長度、字元集)會讓
// 「這個登入名不可能存在」比「這個登入名不存在」早回、而且回不同的錯誤,
// 那就是一支不必猜密碼的帳號列舉器。形狀的判定留給實作端,它會連同
// 誘餌雜湊一起走完等量的路。
func (h authHandler) LocalLogin(
	ctx context.Context, req *connect.Request[platformv1.LocalLoginRequest],
) (*connect.Response[platformv1.LocalLoginResponse], error) {
	svc, ok := h.svc.(LocalAuthService)
	if h.svc == nil || !ok {
		return nil, unimplemented("AuthService.LocalLogin")
	}
	name := strings.TrimSpace(req.Msg.GetLoginName())
	passcode := strings.TrimSpace(req.Msg.GetPasscode())
	if name == "" || passcode == "" {
		return nil, invalidArgument("login_name 與 passcode 必填")
	}
	session, profile, err := svc.LocalLogin(ctx, name, passcode,
		deviceInfo(req.Peer().Addr, req.Header(), h.trusted))
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.LocalLoginResponse{
		Session: sessionToProto(session),
		Profile: profileToProto(profile),
	}), nil
}

func (h authHandler) RefreshSession(
	ctx context.Context, req *connect.Request[platformv1.RefreshSessionRequest],
) (*connect.Response[platformv1.RefreshSessionResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("AuthService.RefreshSession")
	}
	token := strings.TrimSpace(req.Msg.GetRefreshToken())
	if token == "" {
		return nil, invalidArgument("refresh_token 必填")
	}
	session, err := h.svc.RefreshSession(ctx, token, deviceInfo(req.Peer().Addr, req.Header(), h.trusted))
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.RefreshSessionResponse{
		Session: sessionToProto(session),
	}), nil
}

// Logout 兩種用法:帶 refresh_token 撤銷指定 session;不帶則撤銷
// 請求所帶憑證對應的當前 session(所以要讀 header / cookie)。
//
// 取憑證用 userCredential 而不是只讀 Authorization:瀏覽器的 session 在
// HttpOnly cookie 裡(前端讀不到,填不進 body),只讀 header 會讓網頁端
// 除了專用的 POST {base}/auth/logout 之外沒有第二條登出路徑。
// 順序仍是 Bearer 優先,與攔截器同一條規則。
func (h authHandler) Logout(
	ctx context.Context, req *connect.Request[platformv1.LogoutRequest],
) (*connect.Response[platformv1.LogoutResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("AuthService.Logout")
	}
	accessToken, _ := userCredential(req.Header())
	refreshToken := strings.TrimSpace(req.Msg.GetRefreshToken())
	if accessToken == "" && refreshToken == "" {
		return nil, invalidArgument("需要 Authorization: Bearer、access_token cookie 或 refresh_token 其一")
	}
	if err := h.svc.Logout(ctx, accessToken, refreshToken); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.LogoutResponse{}), nil
}
