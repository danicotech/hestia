package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

type fakeActivitySignup struct {
	registered []signup.RegisterParams
	regResult  *signup.RegisterResult
	regErr     error

	logins   []signup.LoginParams
	loginRes *tournament.Player
	loginErr error

	regenerated []signup.RegenerateParams
	regenRes    *signup.RegenerateResult
	regenErr    error

	bound   []signup.BindAccountParams
	bindRes *tournament.Player
	bindErr error
}

func (f *fakeActivitySignup) Register(
	_ context.Context, p signup.RegisterParams,
) (*signup.RegisterResult, error) {
	f.registered = append(f.registered, p)
	if f.regErr != nil {
		return nil, f.regErr
	}
	return f.regResult, nil
}

func (f *fakeActivitySignup) Login(
	_ context.Context, p signup.LoginParams,
) (*tournament.Player, error) {
	f.logins = append(f.logins, p)
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	return f.loginRes, nil
}

func (f *fakeActivitySignup) RegeneratePasscode(
	_ context.Context, p signup.RegenerateParams,
) (*signup.RegenerateResult, error) {
	f.regenerated = append(f.regenerated, p)
	if f.regenErr != nil {
		return nil, f.regenErr
	}
	return f.regenRes, nil
}

func (f *fakeActivitySignup) BindPlatformAccount(
	_ context.Context, p signup.BindAccountParams,
) (*tournament.Player, error) {
	f.bound = append(f.bound, p)
	if f.bindErr != nil {
		return nil, f.bindErr
	}
	return f.bindRes, nil
}

// signupDeps 組出一套可跑完選手流程的依賴。
func signupDeps() (ActivityDeps, *fakeActivitySignup, *fakeActivitySessions, *fakeActivityReader) {
	deps, _, reader := baseActivityDeps()
	player := testPlayer(testPlayerAID, bp.RankKaishan, nil)
	svc := &fakeActivitySignup{
		regResult: &signup.RegisterResult{
			Player:          player,
			ReturningFencer: true,
			PreviousRank:    bp.RankDuanshui,
		},
		loginRes: &player,
	}
	sessions := &fakeActivitySessions{
		token:     testSessionTok,
		expiresAt: time.Now().Add(6 * time.Hour),
		byToken: map[string]ActivityIdentity{
			testSessionTok: {TournamentSlug: testSlug, PlayerPublicID: testPlayerAID},
		},
	}
	deps.Signup = svc
	deps.Sessions = sessions
	return deps, svc, sessions, reader
}

// 報名不需要任何憑證 —— 那是這條路徑存在的全部理由(門檻要夠低)。
//
// 回應裡**不該有通行碼**(2026-09-13):登入只要遊戲ID,顯示一組沒有用途的
// 密碼只會讓人以為要保存它。proto 那個欄位已經 reserved,所以這裡改用
// 序列化後的內容確認 —— 有人把它加回來時,這條會紅。
func TestRegisterIsAnonymousAndReturnsNoPasscode(t *testing.T) {
	deps, svc, _, _ := signupDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	got, err := client.Register(context.Background(), connect.NewRequest(&activityv1.RegisterRequest{
		TournamentSlug: testSlug,
		GameId:         "御風羽",
		DiscordName:    "yufengyu",
		SelfRatedRank:  activityv1.Rank_RANK_FEIHUA,
		LadderRank:     "天梯三段",
		LadderScore:    1820,
	}))
	if err != nil {
		t.Fatalf("Register(匿名): %v", err)
	}
	if strings.Contains(got.Msg.String(), "passcode") {
		t.Fatalf("報名回應不該提到通行碼:%s", got.Msg.String())
	}
	if !got.Msg.GetReturningFencer() {
		t.Fatal("returning_fencer 應為 true")
	}
	if got.Msg.GetPreviousRank() != activityv1.Rank_RANK_DUANSHUI {
		t.Fatalf("previous_rank = %v", got.Msg.GetPreviousRank())
	}
	// 報名期的段位一律不外露。
	if got.Msg.GetPlayer().GetRank() != activityv1.Rank_RANK_UNSPECIFIED {
		t.Fatalf("報名回應不該帶段位,得到 %v", got.Msg.GetPlayer().GetRank())
	}
	if len(svc.registered) != 1 {
		t.Fatalf("領域層收到 %d 次報名", len(svc.registered))
	}
	if svc.registered[0].SelfRatedRank != bp.RankFeihua {
		t.Fatalf("自評段位轉換錯誤:%v", svc.registered[0].SelfRatedRank)
	}
	if svc.registered[0].LadderScore != 1820 {
		t.Fatalf("論劍積分 = %d", svc.registered[0].LadderScore)
	}
}

// 只填遊戲ID 的報名要原樣送到領域層:入口層不補任何預設值,
// 「哪些欄位必填」的權威只有 signup.Service 一處。
func TestRegisterPassesThroughEmptyOptionalFields(t *testing.T) {
	deps, svc, _, _ := signupDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	if _, err := client.Register(context.Background(), connect.NewRequest(&activityv1.RegisterRequest{
		TournamentSlug: testSlug,
		GameId:         "御風羽",
	})); err != nil {
		t.Fatalf("只填遊戲ID 的報名: %v", err)
	}
	got := svc.registered[0]
	if got.GameID != "御風羽" {
		t.Fatalf("game_id = %q", got.GameID)
	}
	if got.DiscordName != "" || got.DisplayName != "" || got.LadderRank != "" ||
		got.ArtsNote != "" || got.AvailabilityNote != "" ||
		got.LadderScore != 0 || got.SelfRatedRank != bp.RankUnspecified {
		t.Fatalf("入口層不該替留空的欄位補值:%+v", got)
	}
}

func TestRegisterRequiresSlug(t *testing.T) {
	deps, _, _, _ := signupDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	_, err := client.Register(context.Background(), connect.NewRequest(&activityv1.RegisterRequest{
		GameId: "御風羽",
	}))
	requireCode(t, err, connect.CodeInvalidArgument)
}

// 登入換發的是 cookie,不是回應 body 裡的字串。
func TestLoginIssuesHardenedCookieAndNoTokenInBody(t *testing.T) {
	deps, svc, sessions, _ := signupDeps()
	deps.BasePath = "/api"
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	res, err := client.Login(context.Background(), connect.NewRequest(&activityv1.LoginRequest{
		TournamentSlug: testSlug, GameId: "御風羽",
	}))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// 登入只要遊戲ID(2026-09-13):原樣送到領域層,入口層不做任何整理。
	if len(svc.logins) != 1 || svc.logins[0].GameID != "御風羽" {
		t.Fatalf("遊戲ID 未原樣傳到領域層:%+v", svc.logins)
	}
	if len(sessions.issued) != 1 {
		t.Fatalf("簽發次數 = %d", len(sessions.issued))
	}
	if sessions.issued[0].TournamentSlug != testSlug || sessions.issued[0].PlayerPublicID != testPlayerAID {
		t.Fatalf("session 身分錯誤:%+v", sessions.issued[0])
	}

	cookie := findSetCookie(t, res.Header(), activitySessionCookieName)
	if cookie.Value != testSessionTok {
		t.Fatalf("cookie 值 = %q", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Fatal("必須是 HttpOnly:JS 讀得到就等於 XSS 偷得走")
	}
	if !cookie.Secure {
		t.Fatal("必須是 Secure")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v,want Lax(這是 cookie 認證唯一的 CSRF 防線)", cookie.SameSite)
	}
	if cookie.Path != "/api" {
		t.Fatalf("Path = %q,want /api(掛載前綴)", cookie.Path)
	}
	if cookie.MaxAge <= 0 {
		t.Fatalf("MaxAge = %d,應取自 token 自己的到期時間", cookie.MaxAge)
	}
	// 回應 body 裡不該有任何看起來像 token 的東西。
	if strings.Contains(res.Msg.String(), testSessionTok) {
		t.Fatal("token 不該出現在回應 body 裡")
	}
}

// 登入失敗不種 cookie —— 那是這條路徑最不能出錯的一件事。
func TestLoginFailureIssuesNoCookie(t *testing.T) {
	deps, svc, _, _ := signupDeps()
	svc.loginErr = signup.ErrInvalidCredentials
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	_, err := client.Login(context.Background(), connect.NewRequest(&activityv1.LoginRequest{
		TournamentSlug: testSlug, GameId: "不存在",
	}))
	if err == nil {
		t.Fatal("預期登入失敗")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("非 connect 錯誤:%v", err)
	}
	if v := ce.Meta().Values("Set-Cookie"); len(v) != 0 {
		t.Fatalf("失敗路徑不該種 cookie,得到 %v", v)
	}
}

func TestGetMyPlayerRequiresActivitySession(t *testing.T) {
	deps, _, _, _ := signupDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	// 沒有 cookie:未認證。
	_, err := client.GetMyPlayer(context.Background(), connect.NewRequest(&activityv1.GetMyPlayerRequest{}))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 只有平台帳號也不行 —— 兩軌身分不能互相頂替。
	_, err = client.GetMyPlayer(context.Background(),
		withUser(connect.NewRequest(&activityv1.GetMyPlayerRequest{}), testUserID))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 壞掉的 token 同樣是未認證(不洩漏是哪一步錯的)。
	_, err = client.GetMyPlayer(context.Background(),
		withPlayerSession(connect.NewRequest(&activityv1.GetMyPlayerRequest{}), "偽造的-token"))
	requireCode(t, err, connect.CodeUnauthenticated)
}

func TestGetMyPlayerReturnsPlayerTournamentAndCurrentMatch(t *testing.T) {
	deps, _, _, reader := signupDeps()
	m := testBracketMatches()[0]
	reader.currentMatch = &m
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	got, err := client.GetMyPlayer(context.Background(),
		withPlayerSession(connect.NewRequest(&activityv1.GetMyPlayerRequest{}), testSessionTok))
	if err != nil {
		t.Fatalf("GetMyPlayer: %v", err)
	}
	if got.Msg.GetPlayer().GetPublicId() != testPlayerAID {
		t.Fatalf("player = %q", got.Msg.GetPlayer().GetPublicId())
	}
	if got.Msg.GetTournament().GetSlug() != testSlug {
		t.Fatalf("tournament = %q", got.Msg.GetTournament().GetSlug())
	}
	if got.Msg.GetCurrentMatch().GetPublicId() != testMatchID {
		t.Fatalf("current_match = %q", got.Msg.GetCurrentMatch().GetPublicId())
	}
}

func TestGetMyPlayerWithoutCurrentMatch(t *testing.T) {
	deps, _, _, _ := signupDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	got, err := client.GetMyPlayer(context.Background(),
		withPlayerSession(connect.NewRequest(&activityv1.GetMyPlayerRequest{}), testSessionTok))
	if err != nil {
		t.Fatalf("GetMyPlayer: %v", err)
	}
	if got.Msg.CurrentMatch != nil {
		t.Fatal("沒有待打場次時 current_match 應該缺席,而不是一個空的 Match")
	}
}

// 綁定需要**兩種**憑證,缺任何一半都不行 —— 它決定獎金匯到誰的帳戶。
func TestBindPlatformAccountNeedsBothIdentities(t *testing.T) {
	deps, svc, _, _ := signupDeps()
	bound := testPlayer(testPlayerAID, bp.RankKaishan, &[]int64{testUserID}[0])
	svc.bindRes = &bound
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	// 什麼都沒有。
	_, err := client.BindPlatformAccount(ctx, connect.NewRequest(&activityv1.BindPlatformAccountRequest{}))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 只有活動層 session:缺平台帳號。
	_, err = client.BindPlatformAccount(ctx,
		withPlayerSession(connect.NewRequest(&activityv1.BindPlatformAccountRequest{}), testSessionTok))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 只有平台帳號:缺活動層 session。
	_, err = client.BindPlatformAccount(ctx,
		withUser(connect.NewRequest(&activityv1.BindPlatformAccountRequest{}), testUserID))
	requireCode(t, err, connect.CodeUnauthenticated)
	if len(svc.bound) != 0 {
		t.Fatalf("憑證不齊時不該呼叫領域層,卻呼叫了 %d 次", len(svc.bound))
	}

	// 兩者齊備。
	req := connect.NewRequest(&activityv1.BindPlatformAccountRequest{})
	withUser(req, testUserID)
	withPlayerSession(req, testSessionTok)
	got, err := client.BindPlatformAccount(ctx, req)
	if err != nil {
		t.Fatalf("BindPlatformAccount: %v", err)
	}
	if got.Msg.GetPlayer().GetUserPublicId() != testUserPublic {
		t.Fatalf("綁定後 user_public_id = %q", got.Msg.GetPlayer().GetUserPublicId())
	}
	if len(svc.bound) != 1 {
		t.Fatalf("領域層收到 %d 次綁定", len(svc.bound))
	}
	// 選手身分來自 session,平台身分來自 Bearer —— 兩者都不從請求 body 讀。
	if svc.bound[0].PlayerPublicID != testPlayerAID || svc.bound[0].TournamentSlug != testSlug {
		t.Fatalf("選手身分應取自 session:%+v", svc.bound[0])
	}
	if svc.bound[0].UserID != testUserID {
		t.Fatalf("平台身分 = %d,want %d", svc.bound[0].UserID, testUserID)
	}
}

// 登出永遠成功並清 cookie:「結束後不該有憑證」在沒有憑證時已經成立。
func TestLogoutClearsCookieEvenWithoutOne(t *testing.T) {
	deps, _, _, _ := signupDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	res, err := client.Logout(context.Background(), connect.NewRequest(&activityv1.LogoutRequest{}))
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	cookie := findSetCookie(t, res.Header(), activitySessionCookieName)
	if cookie.MaxAge >= 0 {
		t.Fatalf("MaxAge = %d,登出必須是立即刪除(負值)", cookie.MaxAge)
	}
	if cookie.Value != "" {
		t.Fatalf("刪除指令不該帶值,得到 %q", cookie.Value)
	}
}

// 沒接上 session 實作時回 Unimplemented,而不是靜靜放行。
func TestActivitySessionNotWiredIsUnimplementedNotOpen(t *testing.T) {
	deps, _, _, _ := signupDeps()
	deps.Sessions = nil
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewSignupServiceClient(srv.Client(), srv.URL)

	_, err := client.GetMyPlayer(context.Background(),
		withPlayerSession(connect.NewRequest(&activityv1.GetMyPlayerRequest{}), testSessionTok))
	requireCode(t, err, connect.CodeUnimplemented)
}

// findSetCookie 從回應標頭裡挑出指定的 Set-Cookie。
func findSetCookie(t *testing.T, header http.Header, name string) *http.Cookie {
	t.Helper()
	res := http.Response{Header: header}
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("回應裡沒有 %s 這個 Set-Cookie(有的是 %v)", name, header.Values("Set-Cookie"))
	return nil
}
