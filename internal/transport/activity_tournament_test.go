package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 活動層 handler 的測試共用設施。
//
// 為什麼是 internal test(package transport)而不是 transport_test:
// 真實請求要帶著 callState 才有身分(那是最外層 middleware 的產物),
// 而 callState 是未匯出的。從外部測就只能整個 transport.New 起來,
// 但活動層還沒被 New 掛上去(組裝是主線的工作)——那會變成測一個
// 還不存在的接線,而不是測 handler 本身。
//
// 走真的 httptest + connect client 而不是直接呼叫方法,是因為兩件事
// 只有走完整條路徑才測得到:req.Spec().Procedure(裁判授權的判準)
// 與 Set-Cookie(活動層 session 的載體)。

const testUserHeader = "X-Test-User"

// testIdentityInterceptor 模擬正式環境的認證攔截器:建立便條、寫入身分。
//
// 它**只做身分注入**,不做任何授權判斷 —— 授權是 handler 的事,
// 而那正是這些測試要驗的東西。
func testIdentityInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			st := &callState{requestID: "test-request"}
			if v := req.Header().Get(testUserHeader); v != "" {
				id, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return nil, connect.NewError(connect.CodeInternal, err)
				}
				st.setUserID(id)
			}
			return next(withState(ctx, st), req)
		}
	}
}

func newActivityServer(t *testing.T, deps ActivityDeps) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	MountActivity(mux, deps, connect.WithInterceptors(testIdentityInterceptor()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// withUser 讓請求帶上「已登入的平台帳號」。
func withUser[T any](req *connect.Request[T], userID int64) *connect.Request[T] {
	req.Header().Set(testUserHeader, strconv.FormatInt(userID, 10))
	return req
}

// withPlayerSession 讓請求帶上活動層 session cookie。
func withPlayerSession[T any](req *connect.Request[T], token string) *connect.Request[T] {
	req.Header().Set("Cookie", activitySessionCookieName+"="+token)
	return req
}

// requireCode 斷言錯誤的 connect code。
func requireCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("預期錯誤 code=%s,實際成功", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("code = %s,want %s(err=%v)", got, want, err)
	}
}

// ── 領域服務與 port 的替身 ────────────────────────────────────

type fakeActivityTournament struct {
	view      *tournament.View
	getErr    error
	getSlugs  []string
	advanced  []tournament.AdvanceParams
	advErr    error
	setRank   []tournament.SetRankParams
	rankPlyr  *tournament.Player
	rankErr   error
	drawParam *tournament.DrawParams
	drawRes   *tournament.DrawResult
	drawErr   error
	swapParam *tournament.SwapParams
	swapErr   error
}

func (f *fakeActivityTournament) Get(_ context.Context, slug string) (*tournament.View, error) {
	f.getSlugs = append(f.getSlugs, slug)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.view, nil
}

func (f *fakeActivityTournament) AdvancePhase(
	_ context.Context, p tournament.AdvanceParams,
) (*tournament.View, error) {
	f.advanced = append(f.advanced, p)
	if f.advErr != nil {
		return nil, f.advErr
	}
	out := *f.view
	out.Tournament.Phase = p.To
	return &out, nil
}

func (f *fakeActivityTournament) SetRank(
	_ context.Context, p tournament.SetRankParams,
) (*tournament.Player, error) {
	f.setRank = append(f.setRank, p)
	if f.rankErr != nil {
		return nil, f.rankErr
	}
	return f.rankPlyr, nil
}

func (f *fakeActivityTournament) Draw(
	_ context.Context, p tournament.DrawParams,
) (*tournament.DrawResult, error) {
	f.drawParam = &p
	if f.drawErr != nil {
		return nil, f.drawErr
	}
	return f.drawRes, nil
}

func (f *fakeActivityTournament) SwapSeeds(
	_ context.Context, p tournament.SwapParams,
) (*tournament.Player, *tournament.Player, error) {
	f.swapParam = &p
	if f.swapErr != nil {
		return nil, nil, f.swapErr
	}
	return &tournament.Player{}, &tournament.Player{}, nil
}

type fakeActivityReader struct {
	tournamentByID     tournament.Tournament
	tournamentByPlayer tournament.Tournament
	byPlayerErr        error
	playerCount        int32
	players            []tournament.Player
	playersErr         error
	playerByPublic     map[string]tournament.Player
	playerErr          error
	matches            []match.Match
	matchByPublic      map[string]match.Match
	matchErr           error
	currentMatch       *match.Match
	fencers            map[int64]signup.Fencer
}

func (f *fakeActivityReader) TournamentByID(context.Context, int64) (tournament.Tournament, error) {
	return f.tournamentByID, nil
}

func (f *fakeActivityReader) TournamentByPlayer(context.Context, string) (tournament.Tournament, error) {
	if f.byPlayerErr != nil {
		return tournament.Tournament{}, f.byPlayerErr
	}
	return f.tournamentByPlayer, nil
}

func (f *fakeActivityReader) CountPlayers(context.Context, int64) (int32, error) {
	return f.playerCount, nil
}

func (f *fakeActivityReader) ListPlayers(context.Context, int64) ([]tournament.Player, error) {
	return f.players, f.playersErr
}

func (f *fakeActivityReader) PlayerByPublicID(
	_ context.Context, _ int64, publicID string,
) (tournament.Player, error) {
	if f.playerErr != nil {
		return tournament.Player{}, f.playerErr
	}
	p, ok := f.playerByPublic[publicID]
	if !ok {
		return tournament.Player{}, tournament.ErrPlayerNotFound
	}
	return p, nil
}

func (f *fakeActivityReader) ListMatches(context.Context, int64) ([]match.Match, error) {
	return f.matches, nil
}

func (f *fakeActivityReader) MatchByPublicID(_ context.Context, publicID string) (match.Match, error) {
	if f.matchErr != nil {
		return match.Match{}, f.matchErr
	}
	m, ok := f.matchByPublic[publicID]
	if !ok {
		return match.Match{}, match.ErrMatchNotFound
	}
	return m, nil
}

func (f *fakeActivityReader) CurrentMatchOfPlayer(context.Context, int64) (*match.Match, error) {
	return f.currentMatch, nil
}

func (f *fakeActivityReader) Fencers(context.Context, []int64) (map[int64]signup.Fencer, error) {
	return f.fencers, nil
}

// fakeActivityDirectory 只實作 Directory 裡活動層真的會用到的那一半。
// 其餘方法回 ErrNotFound —— 用不到卻回零值的話,誤用時看起來會像成功。
type fakeActivityDirectory struct {
	userPublicIDs map[int64]string
}

func (f fakeActivityDirectory) UserID(context.Context, string) (int64, error) {
	return 0, ErrNotFound
}

func (f fakeActivityDirectory) UserPublicID(_ context.Context, userID int64) (string, error) {
	if id, ok := f.userPublicIDs[userID]; ok {
		return id, nil
	}
	return "", ErrNotFound
}

func (f fakeActivityDirectory) EntitlementID(context.Context, string) (int64, error) {
	return 0, ErrNotFound
}

func (f fakeActivityDirectory) RedemptionID(context.Context, string) (int64, error) {
	return 0, ErrNotFound
}

type fakeActivitySessions struct {
	issued    []ActivityIdentity
	token     string
	expiresAt time.Time
	issueErr  error

	byToken   map[string]ActivityIdentity
	verifyErr error
}

func (f *fakeActivitySessions) Issue(
	_ context.Context, id ActivityIdentity,
) (string, time.Time, error) {
	f.issued = append(f.issued, id)
	if f.issueErr != nil {
		return "", time.Time{}, f.issueErr
	}
	return f.token, f.expiresAt, nil
}

func (f *fakeActivitySessions) Verify(_ context.Context, token string) (ActivityIdentity, error) {
	if f.verifyErr != nil {
		return ActivityIdentity{}, f.verifyErr
	}
	id, ok := f.byToken[token]
	if !ok {
		return ActivityIdentity{}, errors.New("token 不認得")
	}
	return id, nil
}

type fakeActivityAuthorizer struct {
	allow      bool
	procedures []string
	userIDs    []int64
}

func (f *fakeActivityAuthorizer) Authorize(_ context.Context, userID int64, procedure string) error {
	f.procedures = append(f.procedures, procedure)
	f.userIDs = append(f.userIDs, userID)
	if f.allow {
		return nil
	}
	return ErrPermissionDenied
}

// ── 測試資料 ────────────────────────────────────────────────────

const (
	testSlug       = "2026-baiye-shifeng"
	testPlayerAID  = "01PLAYERA"
	testPlayerBID  = "01PLAYERB"
	testMatchID    = "01MATCH001"
	testSessionTok = "test-activity-token"
	// 內部 id 刻意給兩個不同的值:同值的話,「勝者是誰」這類以 id 比對的
	// 程式碼會在測試裡永遠通過,而真實資料不會這麼巧。
	testPlayerAInternal = int64(11)
	testPlayerBInternal = int64(22)
	testUserID          = int64(42)
	testUserPublic      = "01USERPUBLIC"
	testTournamentI     = int64(7)
)

func testView(phase tournament.Phase) *tournament.View {
	return &tournament.View{
		Tournament: tournament.Tournament{
			ID:        testTournamentI,
			PublicID:  "01TOURNAMENT",
			Slug:      testSlug,
			Name:      "百業試鋒",
			Phase:     phase,
			CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
		Config: tournament.DefaultConfig(),
	}
}

func testPlayer(publicID string, rank bp.Rank, userID *int64) tournament.Player {
	id := testPlayerAInternal
	if publicID == testPlayerBID {
		id = testPlayerBInternal
	}
	return tournament.Player{
		ID:           id,
		PublicID:     publicID,
		TournamentID: testTournamentI,
		FencerID:     100,
		UserID:       userID,
		GameID:       "游俠" + publicID,
		DisplayName:  "選手" + publicID,
		DiscordName:  "disc#" + publicID,
		Rank:         rank,
		SeedNo:       1,
		Status:       tournament.PlayerActive,
	}
}

// baseActivityDeps 是一組「全部接上、可以跑完 happy path」的依賴。
func baseActivityDeps() (ActivityDeps, *fakeActivityTournament, *fakeActivityReader) {
	tsvc := &fakeActivityTournament{view: testView(tournament.PhaseRanked)}
	reader := &fakeActivityReader{
		tournamentByID:     testView(tournament.PhaseRanked).Tournament,
		tournamentByPlayer: testView(tournament.PhaseRanked).Tournament,
		playerCount:        2,
		players: []tournament.Player{
			testPlayer(testPlayerAID, bp.RankKaishan, nil),
			testPlayer(testPlayerBID, bp.RankWuwo, &[]int64{testUserID}[0]),
		},
		playerByPublic: map[string]tournament.Player{
			testPlayerAID: testPlayer(testPlayerAID, bp.RankKaishan, nil),
			testPlayerBID: testPlayer(testPlayerBID, bp.RankWuwo, &[]int64{testUserID}[0]),
		},
		fencers: map[int64]signup.Fencer{100: {
			LastRankLevel: bp.RankDuanshui, TournamentsPlayed: 3, Wins: 4, Losses: 2,
		}},
	}
	return ActivityDeps{
		Tournament: tsvc,
		Reader:     reader,
		Directory:  fakeActivityDirectory{userPublicIDs: map[int64]string{testUserID: testUserPublic}},
	}, tsvc, reader
}

// ── TournamentService ──────────────────────────────────────────

// 匿名可讀是這個服務的全部意義:觀眾不登入就要看得到賽事、段位與對戰表。
func TestTournamentServiceIsAnonymousReadable(t *testing.T) {
	deps, _, _ := baseActivityDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	got, err := client.GetTournament(ctx, connect.NewRequest(&activityv1.GetTournamentRequest{
		Slug: testSlug,
	}))
	if err != nil {
		t.Fatalf("GetTournament(匿名): %v", err)
	}
	if got.Msg.GetTournament().GetSlug() != testSlug {
		t.Fatalf("slug = %q", got.Msg.GetTournament().GetSlug())
	}
	if got.Msg.GetTournament().GetPhase() != activityv1.TournamentPhase_TOURNAMENT_PHASE_RANKED {
		t.Fatalf("phase = %v", got.Msg.GetTournament().GetPhase())
	}
	if got.Msg.GetTournament().GetPlayerCount() != 2 {
		t.Fatalf("player_count = %d", got.Msg.GetTournament().GetPlayerCount())
	}

	ranks, err := client.ListRanks(ctx, connect.NewRequest(&activityv1.ListRanksRequest{
		TournamentSlug: testSlug,
	}))
	if err != nil {
		t.Fatalf("ListRanks(匿名): %v", err)
	}
	if len(ranks.Msg.GetRanks()) != 4 {
		t.Fatalf("段位數 = %d,want 4", len(ranks.Msg.GetRanks()))
	}
	if ranks.Msg.GetBpPerRankGap() != bp.DefaultPerRankGap {
		t.Fatalf("bp_per_rank_gap = %d", ranks.Msg.GetBpPerRankGap())
	}

	players, err := client.ListPlayers(ctx, connect.NewRequest(&activityv1.ListPlayersRequest{
		TournamentSlug: testSlug,
	}))
	if err != nil {
		t.Fatalf("ListPlayers(匿名): %v", err)
	}
	if len(players.Msg.GetPlayers()) != 2 {
		t.Fatalf("選手數 = %d", len(players.Msg.GetPlayers()))
	}
}

// 段位未公布時連「誰已經被評過」都不該看得出來 —— 那是評段進度,不是公開資訊。
func TestListPlayersHidesRanksBeforePublication(t *testing.T) {
	deps, tsvc, _ := baseActivityDeps()
	tsvc.view = testView(tournament.PhaseRanking)
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	got, err := client.ListPlayers(context.Background(), connect.NewRequest(
		&activityv1.ListPlayersRequest{TournamentSlug: testSlug}))
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	if got.Msg.GetRanksPublished() {
		t.Fatal("ranks_published 在 ranking 階段應為 false")
	}
	for _, p := range got.Msg.GetPlayers() {
		if p.GetRank() != activityv1.Rank_RANK_UNSPECIFIED {
			t.Fatalf("%s 的段位在公布前就外洩了:%v", p.GetPublicId(), p.GetRank())
		}
	}
}

// 對外只出現 public_id:綁定的平台帳號要換成 user_public_id,未綁定是空字串。
func TestListPlayersNeverLeaksInternalUserID(t *testing.T) {
	deps, _, _ := baseActivityDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	got, err := client.ListPlayers(context.Background(), connect.NewRequest(
		&activityv1.ListPlayersRequest{TournamentSlug: testSlug}))
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	byID := map[string]*activityv1.Player{}
	for _, p := range got.Msg.GetPlayers() {
		byID[p.GetPublicId()] = p
	}
	if got := byID[testPlayerAID].GetUserPublicId(); got != "" {
		t.Fatalf("未綁定的選手不該有 user_public_id,得到 %q", got)
	}
	if got := byID[testPlayerBID].GetUserPublicId(); got != testUserPublic {
		t.Fatalf("user_public_id = %q,want %q", got, testUserPublic)
	}
}

func TestGetBracketBeforeDraw(t *testing.T) {
	deps, _, _ := baseActivityDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	got, err := client.GetBracket(context.Background(), connect.NewRequest(
		&activityv1.GetBracketRequest{TournamentSlug: testSlug}))
	if err != nil {
		t.Fatalf("GetBracket: %v", err)
	}
	if got.Msg.GetBracketPublished() {
		t.Fatal("尚未抽籤時 bracket_published 應為 false")
	}
	if len(got.Msg.GetRounds()) != 0 {
		t.Fatalf("尚未抽籤時不該有輪次,得到 %d", len(got.Msg.GetRounds()))
	}
}

func TestGetBracketGroupsRoundsWithLabels(t *testing.T) {
	deps, _, reader := baseActivityDeps()
	reader.matches = testBracketMatches()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	got, err := client.GetBracket(context.Background(), connect.NewRequest(
		&activityv1.GetBracketRequest{TournamentSlug: testSlug}))
	if err != nil {
		t.Fatalf("GetBracket: %v", err)
	}
	if !got.Msg.GetBracketPublished() {
		t.Fatal("bracket_published 應為 true")
	}
	rounds := got.Msg.GetRounds()
	if len(rounds) != 2 {
		t.Fatalf("輪次數 = %d,want 2", len(rounds))
	}
	if rounds[0].GetLabel() != "四強" {
		t.Fatalf("第一輪名稱 = %q,want 四強", rounds[0].GetLabel())
	}
	if rounds[1].GetLabel() != "決賽" {
		t.Fatalf("第二輪名稱 = %q,want 決賽", rounds[1].GetLabel())
	}
	// 對戰表裡只該出現真的在樹上的選手。
	if len(got.Msg.GetPlayers()) != 2 {
		t.Fatalf("對戰表選手數 = %d,want 2", len(got.Msg.GetPlayers()))
	}
}

func TestGetMatchExposesPublicHandicapBudget(t *testing.T) {
	deps, _, reader := baseActivityDeps()
	m := testBracketMatches()[0]
	reader.matchByPublic = map[string]match.Match{testMatchID: m}
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	got, err := client.GetMatch(context.Background(), connect.NewRequest(
		&activityv1.GetMatchRequest{MatchPublicId: testMatchID}))
	if err != nil {
		t.Fatalf("GetMatch: %v", err)
	}
	// 開山(1)對無我(4)= 差三段 × 8 = 24 BP,由低段位方取得。
	if got.Msg.GetHandicapBudget() != 24 {
		t.Fatalf("handicap_budget = %d,want 24", got.Msg.GetHandicapBudget())
	}
	if got.Msg.GetHandicapHolderPublicId() != testPlayerAID {
		t.Fatalf("holder = %q,want %q", got.Msg.GetHandicapHolderPublicId(), testPlayerAID)
	}
	if got.Msg.GetP1().GetGameId() == "" {
		t.Fatal("p1 應該是完整的 Player(含遊戲ID)")
	}
}

func TestGetMatchRequiresID(t *testing.T) {
	deps, _, _ := baseActivityDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	_, err := client.GetMatch(context.Background(), connect.NewRequest(
		&activityv1.GetMatchRequest{}))
	requireCode(t, err, connect.CodeInvalidArgument)
}

// testBracketMatches 是一張 4 人樹:首輪兩場(其中一場是我們關心的),決賽一場。
func testBracketMatches() []match.Match {
	p1 := match.Player{
		ID: testPlayerAInternal, PublicID: testPlayerAID,
		TournamentID: testTournamentI, DisplayName: "選手A",
		Rank: bp.RankKaishan, Status: tournament.PlayerActive,
	}
	p2 := match.Player{
		ID: testPlayerBInternal, PublicID: testPlayerBID,
		TournamentID: testTournamentI, DisplayName: "選手B",
		Rank: bp.RankWuwo, Status: tournament.PlayerActive,
	}
	return []match.Match{
		{
			ID: 1, PublicID: testMatchID, TournamentID: testTournamentI,
			Round: 1, Slot: 0, Status: match.StatusReady, HandicapOpen: true,
			P1: p1, P2: p2,
		},
		{
			ID: 2, PublicID: "01MATCH002", TournamentID: testTournamentI,
			Round: 2, Slot: 0, Status: match.StatusPending,
		},
	}
}

// ── 轉換函式 ────────────────────────────────────────────────────

// 階段在三處(DB 字串 / Go / proto enum)必須完全對得起來 ——
// 對照表打錯一格的症狀是「推進到某個階段後前端顯示另一個階段」。
func TestPhaseRoundTrip(t *testing.T) {
	for _, p := range tournament.Phases() {
		pb := phaseToProto(p)
		if pb == activityv1.TournamentPhase_TOURNAMENT_PHASE_UNSPECIFIED {
			t.Fatalf("%s 沒有對應的 proto 值", p)
		}
		back, ok := phaseFromProto(pb)
		if !ok || back != p {
			t.Fatalf("%s → %v → %s(ok=%v)", p, pb, back, ok)
		}
	}
	if _, ok := phaseFromProto(activityv1.TournamentPhase_TOURNAMENT_PHASE_UNSPECIFIED); ok {
		t.Fatal("UNSPECIFIED 不該被接受成一個階段")
	}
}

// 段位的列舉值刻意等於 rank_level:這個測試就是那條約定的守門員。
func TestRankValuesMatchRankLevel(t *testing.T) {
	for _, r := range []bp.Rank{bp.RankKaishan, bp.RankDuanshui, bp.RankFeihua, bp.RankWuwo} {
		pb := rankToProto(r)
		if int32(pb) != int32(r) {
			t.Fatalf("%s:proto=%d rank_level=%d,兩者必須相同", r, pb, r)
		}
		if back := rankFromProto(pb); back != r {
			t.Fatalf("%s 往返後變成 %s", r, back)
		}
	}
	if rankToProto(bp.RankUnspecified) != activityv1.Rank_RANK_UNSPECIFIED {
		t.Fatal("未評定必須是 UNSPECIFIED")
	}
	if rankToProto(bp.Rank(99)) != activityv1.Rank_RANK_UNSPECIFIED {
		t.Fatal("範圍外的段位必須折成 UNSPECIFIED,不能直接轉型送出去")
	}
}

// 勝者只以 public_id 出現;對不上場上兩人時留空,絕不吐內部 id。
func TestMatchToProtoWinnerUsesPublicID(t *testing.T) {
	m := testBracketMatches()[0]
	m.WinnerPlayerID = m.P2.ID
	m.Status = match.StatusDone
	m.ResultKind = match.ResultWalkover
	pb := matchToProto(m)
	if pb.GetWinnerPlayerPublicId() != testPlayerBID {
		t.Fatalf("winner = %q", pb.GetWinnerPlayerPublicId())
	}
	if pb.GetResultKind() != activityv1.MatchResultKind_MATCH_RESULT_KIND_WALKOVER {
		t.Fatalf("result_kind = %v", pb.GetResultKind())
	}

	m.WinnerPlayerID = 99999 // 資料異常:勝者不在場上
	if got := matchToProto(m).GetWinnerPlayerPublicId(); got != "" {
		t.Fatalf("勝者對不上時應留空,得到 %q", got)
	}
}

func TestMatchStatusMapping(t *testing.T) {
	cases := map[match.Status]activityv1.MatchStatus{
		match.StatusPending: activityv1.MatchStatus_MATCH_STATUS_PENDING,
		match.StatusReady:   activityv1.MatchStatus_MATCH_STATUS_READY,
		match.StatusLocked:  activityv1.MatchStatus_MATCH_STATUS_LOCKED,
		match.StatusLive:    activityv1.MatchStatus_MATCH_STATUS_LIVE,
		match.StatusDone:    activityv1.MatchStatus_MATCH_STATUS_DONE,
	}
	for in, want := range cases {
		if got := matchStatusToProto(in); got != want {
			t.Fatalf("%s → %v,want %v", in, got, want)
		}
		// 讓武那邊的 Status 是純字串(handicap 刻意不另造枚舉),共用同一條路。
		if got := matchStatusStringToProto(string(in)); got != want {
			t.Fatalf("字串 %s → %v,want %v", in, got, want)
		}
	}
	if got := matchStatusStringToProto("不存在的狀態"); got != activityv1.MatchStatus_MATCH_STATUS_UNSPECIFIED {
		t.Fatalf("未知狀態 → %v", got)
	}
}

func TestHandicapCategoryMapping(t *testing.T) {
	all := []handicap.Category{
		handicap.CategoryWeapon, handicap.CategorySkill, handicap.CategoryDefense,
		handicap.CategoryPoison, handicap.CategoryVictory, handicap.CategoryRule,
	}
	seen := map[activityv1.HandicapCategory]bool{}
	for _, c := range all {
		pb := handicapCategoryToProto(c)
		if pb == activityv1.HandicapCategory_HANDICAP_CATEGORY_UNSPECIFIED {
			t.Fatalf("%s 沒有對應的 proto 值", c)
		}
		if seen[pb] {
			t.Fatalf("%s 與其他分類撞到同一個 proto 值 %v", c, pb)
		}
		seen[pb] = true
	}
}

func TestBetStatusAndLegResultMapping(t *testing.T) {
	statuses := map[betting.BetStatus]activityv1.BetStatus{
		betting.BetOpen: activityv1.BetStatus_BET_STATUS_OPEN,
		betting.BetWon:  activityv1.BetStatus_BET_STATUS_WON,
		betting.BetLost: activityv1.BetStatus_BET_STATUS_LOST,
		betting.BetVoid: activityv1.BetStatus_BET_STATUS_VOID,
	}
	for in, want := range statuses {
		if got := betStatusToProto(in); got != want {
			t.Fatalf("%s → %v,want %v", in, got, want)
		}
	}
	legs := map[betting.LegResult]activityv1.LegResult{
		betting.LegPending: activityv1.LegResult_LEG_RESULT_PENDING,
		betting.LegWon:     activityv1.LegResult_LEG_RESULT_WON,
		betting.LegLost:    activityv1.LegResult_LEG_RESULT_LOST,
		betting.LegVoid:    activityv1.LegResult_LEG_RESULT_VOID,
	}
	for in, want := range legs {
		if got := legResultToProto(in); got != want {
			t.Fatalf("%s → %v,want %v", in, got, want)
		}
	}
}

// 抽籤種子必須位元不變地來回:重現一次抽籤是對戰表唯一的自證清白手段。
func TestDrawSeedRoundTrip(t *testing.T) {
	for _, raw := range []uint64{1, 0x0123456789abcdef, ^uint64(0)} {
		seed := tournament.DrawSeed(raw)
		wire := drawSeedToProto(seed)
		back := drawSeedFromProto(wire)
		if back != seed.String() {
			t.Fatalf("%016x 往返後成了 %q", raw, back)
		}
		parsed, err := tournament.ParseDrawSeed(back)
		if err != nil || parsed != seed {
			t.Fatalf("%016x 解析回來是 %016x(err=%v)", raw, uint64(parsed), err)
		}
	}
	if got := drawSeedFromProto(0); got != "" {
		t.Fatalf("0 代表「伺服器自己產」,應轉成空字串,得到 %q", got)
	}
}
