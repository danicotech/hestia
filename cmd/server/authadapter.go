package main

import (
	"context"
	"fmt"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/transport"
)

// authAdapter 把 identity.Service 接到 transport.AuthService。
//
// 需要 adapter 的原因只有兩個形狀差異,不是設計不合:
//   - transport 回傳的是對外檢視(SessionView + ProfileView),identity 回傳的是
//     內部 Session(含內部 user id);轉換在這裡做,內部 id 不會流到入口層。
//   - Logout 收的是 token,identity 收的是 session public_id;access token 的
//     sid claim 就是它,用 signer 解出來即可(AuthService 對認證攔截器豁免,
//     所以這是這條路徑上唯一一次驗證,不是重複驗)。
type authAdapter struct {
	svc      identity.Service
	signer   *identity.Signer
	profiles transport.ProfileReader
	dir      transport.Directory
}

var _ transport.AuthService = (*authAdapter)(nil)

func (a *authAdapter) StartDiscordLogin(_ context.Context, redirectURI string) (string, string, error) {
	return a.svc.StartLogin(redirectURI)
}

func (a *authAdapter) CompleteDiscordLogin(
	ctx context.Context, code, state, stateFromCookie string, dev transport.DeviceInfo,
) (*transport.SessionView, *readmodel.ProfileView, error) {
	sess, err := a.svc.CompleteLogin(ctx, code, state, stateFromCookie, toIdentityDevice(dev))
	if err != nil {
		return nil, nil, err
	}
	profile, err := a.profiles.Profile(ctx, sess.UserID)
	if err != nil {
		return nil, nil, fmt.Errorf("讀登入後的使用者檔案: %w", err)
	}
	return toSessionView(sess), profile, nil
}

func (a *authAdapter) RefreshSession(
	ctx context.Context, refreshToken string, dev transport.DeviceInfo,
) (*transport.SessionView, error) {
	sess, err := a.svc.Refresh(ctx, refreshToken, toIdentityDevice(dev))
	if err != nil {
		return nil, err
	}
	return toSessionView(sess), nil
}

// Logout 撤銷 access token 指向的那個 session(連同它的輪替鏈)。
// 刻意不要求 access token 仍在有效期內以外的任何條件——過期就登不出去是很糟的行為,
// 但簽章仍必須正確(否則任何人都能登出別人)。
func (a *authAdapter) Logout(ctx context.Context, accessToken, _ string) error {
	claims, err := a.signer.VerifyAccess(accessToken, nowFunc())
	if err != nil {
		return err
	}
	userID, err := a.dir.UserID(ctx, claims.Sub)
	if err != nil {
		return err
	}
	return a.svc.Logout(ctx, claims.Sid, userID)
}

func toIdentityDevice(d transport.DeviceInfo) identity.DeviceInfo {
	return identity.DeviceInfo{UserAgent: d.UserAgent, IP: d.IP}
}

func toSessionView(s *identity.Session) *transport.SessionView {
	return &transport.SessionView{
		AccessToken:           s.AccessToken,
		AccessTokenExpiresAt:  s.AccessExpiresAt,
		RefreshToken:          s.RefreshToken,
		RefreshTokenExpiresAt: s.ExpiresAt,
		UserPublicID:          s.UserPublicID,
		// 登入時帶進 state 的站內相對路徑。瀏覽器回呼路由靠它把人送回原本
		// 那一頁;不轉過去的話,每次登入都只能落在首頁。
		Redirect: s.Redirect,
	}
}
