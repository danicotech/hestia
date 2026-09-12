package transport

import (
	"context"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 報名與選手登入 —— 活動層**雙軌身分**裡的選手那一軌。
//
// # 為什麼活動層要自己發一組 session
//
// 報名不需要平台帳號:百業的成員未必都綁過 Discord OAuth,而「先去登入再回來
// 報名」會直接勸退一部分人(grill Q12)。代價是活動層得自己發憑證,
// 於是同一個服務上有兩種身分並存:
//
//	選手      遊戲ID + 通行碼 → 活動層 session cookie(本檔案)
//	下注/投票  平台帳號 Bearer   → 既有的認證攔截器
//
// 兩者可以是同一人:選手事後 BindPlatformAccount 即可,而那支 RPC **同時**
// 需要兩種憑證 —— 綁定決定獎金匯到誰的帳戶,要由本人雙重證明。
//
// # cookie 的機制沿用平台 session,沒有第二套規則
//
// HttpOnly + Secure + SameSite=Lax + Path 跟著掛載前綴走,理由與
// sessioncookie.go 寫的完全相同(XSS 偷不走、跨站 POST 不帶憑證、
// Path 寫死 "/" 會讓每個靜態資源都帶上憑證)。這裡只是換一個 cookie 名字
// 與一個 token 用途,連清除用的 clearCookieAt 與讀取用的 cookieValue 都是同一個。
//
// # 簽發本身是注入的,不是在這裡實作
//
// 「我們自己簽的 token 長什麼樣」的權威是 identity.Signer(v1.<claims>.<sig>,
// 用途混進被簽訊息所以跨用途一定簽章不符)。在入口層再寫一份 HMAC 就是
// 同一個概念的第二個權威位置(專案鐵則 9),所以這裡只宣告 ActivitySessions
// 這個 port,實作由組裝端給。

// activitySessionCookieName 是活動層 session 的 cookie 名稱。
//
// 刻意與 hestia_access_token 不同名:兩者是不同的身分體系,混用一個名字
// 會讓「這個瀏覽器登入的是選手還是平台帳號」變成要靠解 token 才答得出來的問題,
// 而 BindPlatformAccount 正需要兩者同時存在。
const activitySessionCookieName = "hestia_activity_session"

// activitySessionFallbackTTL 只在 ActivitySessions 實作沒填到期時間時使用。
//
// 不做「無條件退回這個值」:實作填了就以實作為準,那才是 token 真正的壽命。
// 兩個地方各寫一份壽命,遲早會有一邊比另一邊長,而那個差值就是
// 「cookie 還在但 token 已死」的窗口(與 maxAgeFor 的說明同一件事)。
const activitySessionFallbackTTL = 12 * time.Hour

// ActivityIdentity 是一個活動層 session 證明的身分。
//
// 只有兩個欄位,而且**都是對外識別字**:選手的內部 BIGINT id 不進 token
// (鐵則 5 —— token 由客戶端持有,內部 id 進去就等於進了對外契約)。
// 內部 id 由 handler 每次用 public_id 反查,那也順便讓「選手被刪掉/棄賽後
// 舊 token 還能用」不可能發生。
type ActivityIdentity struct {
	// TournamentSlug 是這個 session 屬於哪一屆。
	//
	// 必要而不是冗餘:通行碼是逐屆發的,一個 session 只該對一屆有效。
	// 少了它,舊屆的 token 就能拿來操作新一屆的讓武。
	TournamentSlug string
	// PlayerPublicID 是選手的 public_id(ULID)。
	PlayerPublicID string
}

// Valid 回報這是不是一個完整的身分。兩個欄位缺一不可 ——
// 只有一半的身分不該被任何地方當成「已登入」。
func (id ActivityIdentity) Valid() bool {
	return id.TournamentSlug != "" && id.PlayerPublicID != ""
}

// ActivitySessions 簽發與驗證活動層 session。
//
// 為什麼是 port 而不是在這裡實作:見檔案開頭。建議的實作是在
// identity.Signer 上補一個 activity 用途(它的 typ 已經做了用途分離,
// 加一個用途不會讓既有 token 變得可互換),由 cmd/server 注入。
//
// Verify 失敗一律回 identity 的憑證類 sentinel(ErrInvalidToken /
// ErrTokenExpired),errmap 已經把它們映射成 Unauthenticated ——
// 「請重新登入」對呼叫端是同一件事,不需要第二套錯誤字典。
type ActivitySessions interface {
	// Issue 簽發一個 session,回傳 token 與到期時間。
	Issue(ctx context.Context, id ActivityIdentity) (token string, expiresAt time.Time, err error)
	// Verify 驗證 token 並取回身分。
	Verify(ctx context.Context, token string) (ActivityIdentity, error)
}

// ActivitySignup 是報名與選手身分的領域服務(*signup.Service 直接滿足)。
type ActivitySignup interface {
	Register(ctx context.Context, p signup.RegisterParams) (*signup.RegisterResult, error)
	Login(ctx context.Context, p signup.LoginParams) (*tournament.Player, error)
	RegeneratePasscode(ctx context.Context, p signup.RegenerateParams) (*signup.RegenerateResult, error)
	BindPlatformAccount(ctx context.Context, p signup.BindAccountParams) (*tournament.Player, error)
}

// activitySessionCookie 是活動層 session cookie 的屬性。
//
// 與 sessionCookieConfig 同一套規則,只是少了 refresh 那一半:
// 活動層 session 沒有輪替 —— 它的壽命以「一屆賽事的一次使用」為尺度,
// 而通行碼隨時可以重新登入。少一個 refresh token 就少一個長效憑證要保護。
type activitySessionCookie struct {
	basePath string // 已由 NormalizeBasePath 正規化;"" = 掛在根
}

func newActivitySessionCookie(basePath string) activitySessionCookie {
	return activitySessionCookie{basePath: basePath}
}

// path 與平台 access token 同範圍:活動層的每一支 RPC 都可能需要它。
func (c activitySessionCookie) path() string {
	if c.basePath == "" {
		return "/"
	}
	return c.basePath
}

func (c activitySessionCookie) issue(token string, expiresAt, now time.Time) *http.Cookie {
	maxAge := maxAgeFor(expiresAt, activitySessionFallbackTTL, now)
	if token == "" || maxAge <= 0 {
		// 沒有值、或壽命已經是負的:種下去只會是一個立刻失效的憑證。
		// 直接送刪除指令,不留一個「看起來有登入」的假象。
		return c.clear()
	}
	return &http.Cookie{
		Name:     activitySessionCookieName,
		Value:    token,
		Path:     c.path(),
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// clear 產生刪除指令。Path 必須與種下時完全相同,否則瀏覽器會當成另一個
// cookie 而留著原來的 ——「登出了但 cookie 還在」是最糟的一種靜默失敗。
func (c activitySessionCookie) clear() *http.Cookie {
	return clearCookieAt(activitySessionCookieName, c.path())
}

// activitySessionToken 讀出請求帶的活動層 session;讀不到是空字串。
//
// 讀不到就回空字串**照樣往下送**,由驗證那一層判定 ——
// 「沒有 cookie 就跳過檢查」是所有 cookie 認證最常見的破口。
func activitySessionToken(header http.Header) string {
	return cookieValue(header, activitySessionCookieName)
}

// requireActivityIdentity 是「這支 RPC 需要選手身分」的統一入口。
//
// sessions 未注入時回 Unimplemented 而不是放行:沒接上驗證就把選手端開著,
// 是比功能缺席嚴重得多的問題(與 Authenticator 的 UnauthenticatedStub 同一個判斷)。
func requireActivityIdentity(
	ctx context.Context, sessions ActivitySessions, header http.Header,
) (ActivityIdentity, error) {
	if sessions == nil {
		return ActivityIdentity{}, unimplemented("活動層 session")
	}
	token := activitySessionToken(header)
	if token == "" {
		return ActivityIdentity{}, toConnectError(ErrUnauthenticated)
	}
	id, err := sessions.Verify(ctx, token)
	if err != nil {
		return ActivityIdentity{}, toConnectError(err)
	}
	if !id.Valid() {
		// 實作回了「成功但身分不完整」——當作驗證失敗,不讓半個身分過關
		// (與認證攔截器對 userID == 0 的處置同一道防線)。
		return ActivityIdentity{}, toConnectError(ErrUnauthenticated)
	}
	return id, nil
}

// optionalActivityIdentity 給「匿名也讀得到、但登入者看得比較多」的 RPC 用
// (目前只有 HandicapService.GetMatchHandicaps)。
//
// 任何失敗都退化成匿名,不回錯:過期的 cookie 不該讓觀眾連公開的封盤內容
// 都看不到。可見性的權威在領域層,它收到的 viewerPlayerID 是 0 就當匿名處理。
func optionalActivityIdentity(
	ctx context.Context, sessions ActivitySessions, header http.Header,
) (ActivityIdentity, bool) {
	if sessions == nil {
		return ActivityIdentity{}, false
	}
	token := activitySessionToken(header)
	if token == "" {
		return ActivityIdentity{}, false
	}
	id, err := sessions.Verify(ctx, token)
	if err != nil || !id.Valid() {
		return ActivityIdentity{}, false
	}
	return id, true
}

// activitySignupHandler 是選手側的報名與身分。
//
// 除了 BindPlatformAccount,沒有任何一支需要平台帳號 —— 這是這個服務
// 存在的全部理由,不要因為「其他服務都要 requireUser」而加上去。
type activitySignupHandler struct {
	svc         ActivitySignup
	tournaments ActivityTournament
	reader      ActivityReader
	sessions    ActivitySessions
	cookie      activitySessionCookie
	directory   Directory
}

// Register 報名。
//
// 刻意沒有冪等鍵:防連點的權威是 UNIQUE (tournament_id, fencer_id)
// (與每日簽到同一個判斷,帳本鐵則 4 的延伸)。重複報名由 DB 撞鍵擋下,
// 對外表現為 signup.ErrAlreadyRegistered。
func (h activitySignupHandler) Register(
	ctx context.Context, req *connect.Request[activityv1.RegisterRequest],
) (*connect.Response[activityv1.RegisterResponse], error) {
	if h.svc == nil || h.directory == nil {
		return nil, unimplemented("SignupService.Register")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	res, err := h.svc.Register(ctx, signup.RegisterParams{
		TournamentSlug:   slug,
		GameID:           req.Msg.GetGameId(),
		DisplayName:      req.Msg.GetDisplayName(),
		DiscordName:      req.Msg.GetDiscordName(),
		SelfRatedRank:    rankFromProto(req.Msg.GetSelfRatedRank()),
		LadderRank:       req.Msg.GetLadderRank(),
		LadderScore:      req.Msg.GetLadderScore(),
		ArtsNote:         req.Msg.GetArtsNote(),
		AvailabilityNote: req.Msg.GetAvailabilityNote(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// 報名當下一定還在 signup 階段,段位必為未評定 —— showRank 傳 false
	// 不會少掉任何資訊,而且讓「段位只在公布後才出現」這條規則沒有例外。
	player, err := playerWithAccountToProto(ctx, h.directory, res.Player, false)
	if err != nil {
		return nil, err
	}
	// 明碼通行碼**只在這裡出現一次**。它不進日誌、不進稽核紀錄、
	// 不進任何其他回應 —— 這個性質是刻意維持的,加任何 log 前請先想清楚。
	return connect.NewResponse(&activityv1.RegisterResponse{
		Player:          player,
		Passcode:        res.Passcode,
		ReturningFencer: res.ReturningFencer,
		PreviousRank:    rankToProto(res.PreviousRank),
	}), nil
}

// Login 用遊戲ID + 通行碼換發活動層 session。
//
// 回應 body 裡**沒有** token:它只在 HttpOnly cookie 裡(與平台的瀏覽器登入
// 同一個判斷 —— 交給 JS 存等於把憑證交給每一個第三方腳本)。
func (h activitySignupHandler) Login(
	ctx context.Context, req *connect.Request[activityv1.LoginRequest],
) (*connect.Response[activityv1.LoginResponse], error) {
	if h.svc == nil || h.tournaments == nil || h.sessions == nil || h.directory == nil {
		return nil, unimplemented("SignupService.Login")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	player, err := h.svc.Login(ctx, signup.LoginParams{
		TournamentSlug: slug,
		GameID:         req.Msg.GetGameId(),
		Passcode:       req.Msg.GetPasscode(),
	})
	if err != nil {
		// 「查無此遊戲ID」與「通行碼錯誤」在領域層就已經折成同一個
		// ErrInvalidCredentials,這裡不做任何額外分流 ——
		// 分得出來的話,登入頁就變成一份「誰報了名」的查詢介面。
		return nil, toConnectError(err)
	}
	view, err := h.tournaments.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	token, expiresAt, err := h.sessions.Issue(ctx, ActivityIdentity{
		TournamentSlug: slug, PlayerPublicID: player.PublicID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	pb, err := playerWithAccountToProto(ctx, h.directory, *player, ranksPublished(view.Tournament.Phase))
	if err != nil {
		return nil, err
	}
	res := connect.NewResponse(&activityv1.LoginResponse{Player: pb})
	setCookie(res, h.cookie.issue(token, expiresAt, time.Now()))
	return res, nil
}

// Logout 結束活動層 session。
//
// 只清 cookie:活動層 session 是無狀態簽章,沒有伺服器端的撤銷清單。
// 少一張清單就少一個會忘記清的地方(與通行碼「換發即失效」同一個判斷)。
//
// 不帶 cookie 也回成功:登出的語意是「結束後不該有憑證」,而那在
// 一開始就沒有憑證時已經成立。回錯只會讓前端要為一個無害的狀態寫分支。
func (h activitySignupHandler) Logout(
	_ context.Context, _ *connect.Request[activityv1.LogoutRequest],
) (*connect.Response[activityv1.LogoutResponse], error) {
	res := connect.NewResponse(&activityv1.LogoutResponse{})
	setCookie(res, h.cookie.clear())
	return res, nil
}

// GetMyPlayer 取目前登入的選手、本屆狀態與待打的場次。
//
// 三樣一起回是因為前端要用它們決定顯示哪個畫面(還在報名期?該去選讓武了?
// 已經出局?),分成三支 RPC 的結果是那三個答案來自三個不同的時間點。
func (h activitySignupHandler) GetMyPlayer(
	ctx context.Context, req *connect.Request[activityv1.GetMyPlayerRequest],
) (*connect.Response[activityv1.GetMyPlayerResponse], error) {
	if h.tournaments == nil || h.reader == nil || h.directory == nil {
		return nil, unimplemented("SignupService.GetMyPlayer")
	}
	id, err := requireActivityIdentity(ctx, h.sessions, req.Header())
	if err != nil {
		return nil, err
	}
	view, player, err := resolveActivityPlayer(ctx, h.tournaments, h.reader, id)
	if err != nil {
		return nil, err
	}
	showRank := ranksPublished(view.Tournament.Phase)
	pbPlayer, err := playerWithAccountToProto(ctx, h.directory, player, showRank)
	if err != nil {
		return nil, err
	}
	count, err := h.reader.CountPlayers(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	res := &activityv1.GetMyPlayerResponse{
		Player:     pbPlayer,
		Tournament: tournamentToProto(view, count),
	}
	current, err := h.reader.CurrentMatchOfPlayer(ctx, player.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	if current != nil {
		res.CurrentMatch = matchToProto(*current)
	}
	return connect.NewResponse(res), nil
}

// BindPlatformAccount 把活動層身分接到平台帳號(領獎前必做)。
//
// **同時**需要兩種憑證:活動層 session 證明「你是這位選手」,平台 Bearer
// 證明「你是這個帳號」。綁定決定獎金匯到誰的帳戶,所以刻意不讓裁判代綁,
// 也刻意不接受只有其中一半的請求。
//
// 兩道檢查的順序是有意義的:先要平台身分(那是認證攔截器已經做完的事),
// 再要活動層身分 —— 反過來的話,沒登入平台的人會先看到「請用通行碼登入」,
// 而那不是他該做的事。
func (h activitySignupHandler) BindPlatformAccount(
	ctx context.Context, req *connect.Request[activityv1.BindPlatformAccountRequest],
) (*connect.Response[activityv1.BindPlatformAccountResponse], error) {
	if h.svc == nil || h.tournaments == nil || h.directory == nil {
		return nil, unimplemented("SignupService.BindPlatformAccount")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	id, err := requireActivityIdentity(ctx, h.sessions, req.Header())
	if err != nil {
		return nil, err
	}
	player, err := h.svc.BindPlatformAccount(ctx, signup.BindAccountParams{
		TournamentSlug: id.TournamentSlug,
		PlayerPublicID: id.PlayerPublicID,
		UserID:         userID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	view, err := h.tournaments.Get(ctx, id.TournamentSlug)
	if err != nil {
		return nil, toConnectError(err)
	}
	pb, err := playerWithAccountToProto(ctx, h.directory, *player, ranksPublished(view.Tournament.Phase))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.BindPlatformAccountResponse{Player: pb}), nil
}

// resolveActivityPlayer 把一個活動層身分解成「這屆賽事」與「這位選手」。
//
// 每次請求都重解一次而不是把內部 id 放進 token:棄賽、被刪、換屆之後,
// 舊 token 會在這一步自然失效,不需要另一張撤銷清單。
func resolveActivityPlayer(
	ctx context.Context, tournaments ActivityTournament, reader ActivityReader, id ActivityIdentity,
) (*tournament.View, tournament.Player, error) {
	view, err := tournaments.Get(ctx, id.TournamentSlug)
	if err != nil {
		return nil, tournament.Player{}, toConnectError(err)
	}
	player, err := reader.PlayerByPublicID(ctx, view.Tournament.ID, id.PlayerPublicID)
	if err != nil {
		return nil, tournament.Player{}, toConnectError(err)
	}
	return view, player, nil
}
