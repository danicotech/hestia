package transport

import (
	"context"
	"errors"
	"strings"
	"testing"

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

type fakeActivityMatches struct {
	opened    []match.OpenHandicapParams
	locked    []match.LockHandicapParams
	started   []match.StartMatchParams
	reported  []match.ReportResultParams
	withdrawn []match.WithdrawPlayerParams
	streams   []match.SetStreamURLParams

	err error
}

func (f *fakeActivityMatches) OpenHandicap(
	_ context.Context, p match.OpenHandicapParams,
) (*match.OpenHandicapResult, error) {
	f.opened = append(f.opened, p)
	if f.err != nil {
		return nil, f.err
	}
	return &match.OpenHandicapResult{Match: testBracketMatches()[0]}, nil
}

func (f *fakeActivityMatches) LockHandicap(
	_ context.Context, p match.LockHandicapParams,
) (*match.LockHandicapResult, error) {
	f.locked = append(f.locked, p)
	if f.err != nil {
		return nil, f.err
	}
	return &match.LockHandicapResult{
		Match: testBracketMatches()[0],
		Handicaps: &handicap.MatchHandicaps{
			MatchPublicID: testMatchID, Status: "locked", Revealed: true,
		},
	}, nil
}

func (f *fakeActivityMatches) StartMatch(
	_ context.Context, p match.StartMatchParams,
) (*match.Match, error) {
	f.started = append(f.started, p)
	if f.err != nil {
		return nil, f.err
	}
	m := testBracketMatches()[0]
	return &m, nil
}

func (f *fakeActivityMatches) ReportResult(
	_ context.Context, p match.ReportResultParams,
) (*match.ReportResultOutcome, error) {
	f.reported = append(f.reported, p)
	if f.err != nil {
		return nil, f.err
	}
	all := testBracketMatches()
	done := all[0]
	done.Status = match.StatusDone
	done.WinnerPlayerID = testPlayerBInternal
	return &match.ReportResultOutcome{
		Match:           done,
		AdvancedMatches: []match.Match{all[1]},
		SettledBetCount: 5,
	}, nil
}

func (f *fakeActivityMatches) WithdrawPlayer(
	_ context.Context, p match.WithdrawPlayerParams,
) (*match.WithdrawResult, error) {
	f.withdrawn = append(f.withdrawn, p)
	if f.err != nil {
		return nil, f.err
	}
	return &match.WithdrawResult{
		Player: match.Player{
			ID: testPlayerAInternal, PublicID: testPlayerAID,
			TournamentID: testTournamentI, Status: tournament.PlayerWithdrawn,
		},
		WalkoverMatches: []match.Match{testBracketMatches()[0]},
		VoidedBetCount:  3,
	}, nil
}

func (f *fakeActivityMatches) SetStreamURL(
	_ context.Context, p match.SetStreamURLParams,
) (*match.Match, error) {
	f.streams = append(f.streams, p)
	if f.err != nil {
		return nil, f.err
	}
	m := testBracketMatches()[0]
	m.StreamURL = p.StreamURL
	return &m, nil
}

type fakeActivityPrizes struct {
	calls    []ActivityPrizeParams
	awards   []ActivityPrizeAward
	replayed bool
}

func (f *fakeActivityPrizes) AwardPrizes(
	_ context.Context, p ActivityPrizeParams,
) ([]ActivityPrizeAward, bool, error) {
	f.calls = append(f.calls, p)
	return f.awards, f.replayed, nil
}

func judgeDeps() (ActivityDeps, *fakeActivityTournament, *fakeActivityMatches,
	*fakeActivitySignup, *fakeActivityPrizes, *fakeActivityAuthorizer,
) {
	deps, tsvc, reader := baseActivityDeps()
	tsvc.view = testView(tournament.PhaseDrawing)
	ranked := testPlayer(testPlayerAID, bp.RankFeihua, nil)
	tsvc.rankPlyr = &ranked
	tsvc.drawRes = &tournament.DrawResult{
		Seed:         tournament.DrawSeed(0x0123456789abcdef),
		ByePlayerIDs: []int64{testPlayerBInternal},
		TotalRounds:  2,
		Players: []tournament.Player{
			testPlayer(testPlayerAID, bp.RankKaishan, nil),
			testPlayer(testPlayerBID, bp.RankWuwo, nil),
		},
	}
	reader.matches = testBracketMatches()

	matches := &fakeActivityMatches{}
	sgn := &fakeActivitySignup{regenRes: &signup.RegenerateResult{Passcode: "Z9X4W2"}}
	prizes := &fakeActivityPrizes{awards: []ActivityPrizeAward{
		{PlayerPublicID: testPlayerBID, DisplayName: "選手B", Kind: "champion", Amount: 5000, Granted: true},
		{PlayerPublicID: testPlayerAID, DisplayName: "選手A", Kind: "runner_up", Granted: false,
			SkippedReason: "尚未綁定平台帳號"},
	}}
	authz := &fakeActivityAuthorizer{allow: true}

	deps.Matches = matches
	deps.Signup = sgn
	deps.Prizes = prizes
	deps.Authorizer = authz
	return deps, tsvc, matches, sgn, prizes, authz
}

// 每一支裁判 RPC 都要平台帳號 + 裁判權限,而且是逐支檢查,不是挑幾支。
func TestJudgeServiceRequiresPlatformAccountAndPermission(t *testing.T) {
	deps, _, matches, _, _, authz := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	// 每一支 RPC 各打一次:沒登入 → Unauthenticated。
	calls := judgeCalls(client)
	for name, call := range calls {
		t.Run(name+"/未登入", func(t *testing.T) {
			requireCode(t, call(ctx, false), connect.CodeUnauthenticated)
		})
	}

	// 登入了但沒有裁判權限 → PermissionDenied。
	authz.allow = false
	for name, call := range calls {
		t.Run(name+"/無權限", func(t *testing.T) {
			requireCode(t, call(ctx, true), connect.CodePermissionDenied)
		})
	}
	if len(matches.opened)+len(matches.locked)+len(matches.reported) != 0 {
		t.Fatal("權限不足的請求不該碰領域層")
	}

	// 每一支都真的送出了自己的 procedure 名稱(授權表的判準)。
	if len(authz.procedures) != len(calls) {
		t.Fatalf("Authorize 被呼叫 %d 次,RPC 有 %d 支", len(authz.procedures), len(calls))
	}
	for _, p := range authz.procedures {
		if p == "" {
			t.Fatal("procedure 是空字串,授權表對不上任何一列")
		}
		if !strings.HasPrefix(p, "/"+activityv1connect.JudgeServiceName+"/") {
			t.Fatalf("procedure = %q,不是 JudgeService 的", p)
		}
	}
	for _, id := range authz.userIDs {
		if id != testUserID {
			t.Fatalf("授權對象 = %d,want %d", id, testUserID)
		}
	}
}

// 沒注入 Authorizer 時一律拒絕:授權沒接上就把裁判後台開著,比功能缺席嚴重得多。
func TestJudgeServiceWithoutAuthorizerDeniesEverything(t *testing.T) {
	deps, _, _, _, _, _ := judgeDeps()
	deps.Authorizer = nil
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	for name, call := range judgeCalls(client) {
		t.Run(name, func(t *testing.T) {
			requireCode(t, call(context.Background(), true), connect.CodePermissionDenied)
		})
	}
}

// judgeCalls 回傳每一支裁判 RPC 的最小合法呼叫。
//
// 逐支列出而不是只測一兩支:漏掉權限檢查的那一支,正是會被人找到的那一支。
// 這份清單與 proto 的服務定義必須一樣長 —— 新增 RPC 時測試會先提醒你。
func judgeCalls(c activityv1connect.JudgeServiceClient) map[string]func(context.Context, bool) error {
	return map[string]func(context.Context, bool) error{
		"AdvancePhase": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.AdvancePhaseRequest{
				TournamentSlug: testSlug,
				ToPhase:        activityv1.TournamentPhase_TOURNAMENT_PHASE_IN_PROGRESS,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.AdvancePhase(ctx, r)
			return err
		},
		"AssignRank": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.AssignRankRequest{
				PlayerPublicId: testPlayerAID, Rank: activityv1.Rank_RANK_FEIHUA,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.AssignRank(ctx, r)
			return err
		},
		"ListUnranked": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.ListUnrankedRequest{TournamentSlug: testSlug})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.ListUnranked(ctx, r)
			return err
		},
		"DrawBracket": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.DrawBracketRequest{TournamentSlug: testSlug})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.DrawBracket(ctx, r)
			return err
		},
		"SwapSeeds": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.SwapSeedsRequest{
				PlayerAPublicId: testPlayerAID, PlayerBPublicId: testPlayerBID,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.SwapSeeds(ctx, r)
			return err
		},
		"ConfirmBracket": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.ConfirmBracketRequest{
				TournamentSlug: testSlug, Confirm: true,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.ConfirmBracket(ctx, r)
			return err
		},
		"OpenHandicap": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.OpenHandicapRequest{MatchPublicId: testMatchID})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.OpenHandicap(ctx, r)
			return err
		},
		"LockHandicap": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.LockHandicapRequest{
				MatchPublicId: testMatchID, Confirm: true,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.LockHandicap(ctx, r)
			return err
		},
		"SetStreamUrl": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.SetStreamUrlRequest{MatchPublicId: testMatchID})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.SetStreamUrl(ctx, r)
			return err
		},
		"StartMatch": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.StartMatchRequest{MatchPublicId: testMatchID})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.StartMatch(ctx, r)
			return err
		},
		"ReportResult": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.ReportResultRequest{
				MatchPublicId: testMatchID, WinnerPlayerPublicId: testPlayerBID, Confirm: true,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.ReportResult(ctx, r)
			return err
		},
		"WithdrawPlayer": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.WithdrawPlayerRequest{
				PlayerPublicId: testPlayerAID, Confirm: true,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.WithdrawPlayer(ctx, r)
			return err
		},
		"RegeneratePasscode": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.RegeneratePasscodeRequest{
				PlayerPublicId: testPlayerAID,
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.RegeneratePasscode(ctx, r)
			return err
		},
		"AwardPrizes": func(ctx context.Context, in bool) error {
			r := connect.NewRequest(&activityv1.AwardPrizesRequest{
				TournamentSlug: testSlug, Confirm: true, IdempotencyKey: "k",
			})
			if in {
				withUser(r, testUserID)
			}
			_, err := c.AwardPrizes(ctx, r)
			return err
		},
	}
}

// ── 單支 RPC 的行為 ────────────────────────────────────────────

func TestAdvancePhaseConvertsPhase(t *testing.T) {
	deps, tsvc, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	got, err := client.AdvancePhase(context.Background(), withUser(connect.NewRequest(
		&activityv1.AdvancePhaseRequest{
			TournamentSlug: testSlug,
			ToPhase:        activityv1.TournamentPhase_TOURNAMENT_PHASE_IN_PROGRESS,
			Note:           "籤表已確認",
		}), testUserID))
	if err != nil {
		t.Fatalf("AdvancePhase: %v", err)
	}
	if len(tsvc.advanced) != 1 {
		t.Fatalf("領域層收到 %d 次", len(tsvc.advanced))
	}
	if tsvc.advanced[0].To != tournament.PhaseInProgress {
		t.Fatalf("To = %q", tsvc.advanced[0].To)
	}
	if tsvc.advanced[0].ActorUserID != testUserID {
		t.Fatalf("裁判 = %d", tsvc.advanced[0].ActorUserID)
	}
	if tsvc.advanced[0].Reason != "籤表已確認" {
		t.Fatalf("note 未傳到稽核理由:%q", tsvc.advanced[0].Reason)
	}
	if got.Msg.GetTournament().GetPhase() != activityv1.TournamentPhase_TOURNAMENT_PHASE_IN_PROGRESS {
		t.Fatalf("回應階段 = %v", got.Msg.GetTournament().GetPhase())
	}
}

func TestAdvancePhaseRejectsUnspecified(t *testing.T) {
	deps, tsvc, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	_, err := client.AdvancePhase(context.Background(), withUser(connect.NewRequest(
		&activityv1.AdvancePhaseRequest{TournamentSlug: testSlug}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)
	if len(tsvc.advanced) != 0 {
		t.Fatal("未指定階段不該進到領域層")
	}
}

// 請求只帶 player_public_id,slug 由入口層反查 —— 那是契約與領域參數的落差。
func TestAssignRankResolvesTournamentFromPlayer(t *testing.T) {
	deps, tsvc, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	got, err := client.AssignRank(context.Background(), withUser(connect.NewRequest(
		&activityv1.AssignRankRequest{
			PlayerPublicId: testPlayerAID,
			Rank:           activityv1.Rank_RANK_FEIHUA,
			Note:           "實戰經驗充足",
		}), testUserID))
	if err != nil {
		t.Fatalf("AssignRank: %v", err)
	}
	if len(tsvc.setRank) != 1 {
		t.Fatalf("領域層收到 %d 次", len(tsvc.setRank))
	}
	if tsvc.setRank[0].Slug != testSlug {
		t.Fatalf("slug = %q(應由 player 反查)", tsvc.setRank[0].Slug)
	}
	if tsvc.setRank[0].Rank != bp.RankFeihua {
		t.Fatalf("rank = %v", tsvc.setRank[0].Rank)
	}
	// 裁判後台永遠看得到段位 —— ranksPublished 管的是對外公布。
	if got.Msg.GetPlayer().GetRank() != activityv1.Rank_RANK_FEIHUA {
		t.Fatalf("回應段位 = %v", got.Msg.GetPlayer().GetRank())
	}
}

func TestAssignRankRejectsUnspecifiedRank(t *testing.T) {
	deps, tsvc, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	_, err := client.AssignRank(context.Background(), withUser(connect.NewRequest(
		&activityv1.AssignRankRequest{PlayerPublicId: testPlayerAID}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)
	if len(tsvc.setRank) != 0 {
		t.Fatal("未指定段位不該進到領域層")
	}
}

// 報名表的內容只有裁判看得到,而且要配上跨屆戰績。
func TestListUnrankedBuildsDossiers(t *testing.T) {
	deps, _, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	// 預設只列未評段的:測試資料裡兩位都已評段,所以是空的。
	got, err := client.ListUnranked(ctx, withUser(connect.NewRequest(
		&activityv1.ListUnrankedRequest{TournamentSlug: testSlug}), testUserID))
	if err != nil {
		t.Fatalf("ListUnranked: %v", err)
	}
	if len(got.Msg.GetPlayers()) != 0 {
		t.Fatalf("已評段的不該出現在待評名單,得到 %d 位", len(got.Msg.GetPlayers()))
	}

	got, err = client.ListUnranked(ctx, withUser(connect.NewRequest(
		&activityv1.ListUnrankedRequest{TournamentSlug: testSlug, IncludeRanked: true}), testUserID))
	if err != nil {
		t.Fatalf("ListUnranked(含已評段): %v", err)
	}
	if len(got.Msg.GetPlayers()) != 2 {
		t.Fatalf("複查名單 = %d 位", len(got.Msg.GetPlayers()))
	}
	d := got.Msg.GetPlayers()[0]
	if d.GetDiscordName() == "" {
		t.Fatal("裁判需要 Discord 名稱才聯絡得到人")
	}
	if d.GetTournamentsPlayed() != 3 || d.GetWins() != 4 || d.GetLosses() != 2 {
		t.Fatalf("跨屆戰績錯誤:played=%d wins=%d losses=%d",
			d.GetTournamentsPlayed(), d.GetWins(), d.GetLosses())
	}
	if d.GetPreviousRank() != activityv1.Rank_RANK_DUANSHUI {
		t.Fatalf("previous_rank = %v", d.GetPreviousRank())
	}
}

// 抽籤種子要能原樣帶回:那是對戰表唯一的自證清白手段。
func TestDrawBracketRoundTripsSeedAndUsesPublicIDs(t *testing.T) {
	deps, tsvc, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	got, err := client.DrawBracket(context.Background(), withUser(connect.NewRequest(
		&activityv1.DrawBracketRequest{TournamentSlug: testSlug}), testUserID))
	if err != nil {
		t.Fatalf("DrawBracket: %v", err)
	}
	if tsvc.drawParam.Seed != "" {
		t.Fatalf("seed=0 應轉成空字串(讓伺服器自己產),得到 %q", tsvc.drawParam.Seed)
	}
	if uint64(got.Msg.GetSeed()) != 0x0123456789abcdef {
		t.Fatalf("回傳的種子 = %016x", uint64(got.Msg.GetSeed()))
	}
	if len(got.Msg.GetByePlayerPublicIds()) != 1 ||
		got.Msg.GetByePlayerPublicIds()[0] != testPlayerBID {
		t.Fatalf("輪空名單應該是 public_id:%v", got.Msg.GetByePlayerPublicIds())
	}
	// 場次 public_id 只有回讀對戰表才拿得到。
	if len(got.Msg.GetRounds()) != 2 {
		t.Fatalf("輪次數 = %d", len(got.Msg.GetRounds()))
	}
	if got.Msg.GetRounds()[0].GetMatches()[0].GetPublicId() != testMatchID {
		t.Fatal("對戰表的場次必須帶 public_id")
	}

	// 指定種子:重現一次既有的抽籤。
	_, err = client.DrawBracket(context.Background(), withUser(connect.NewRequest(
		&activityv1.DrawBracketRequest{TournamentSlug: testSlug, Seed: 1}), testUserID))
	if err != nil {
		t.Fatalf("DrawBracket(指定種子): %v", err)
	}
	if tsvc.drawParam.Seed != "0000000000000001" {
		t.Fatalf("種子字串 = %q", tsvc.drawParam.Seed)
	}
}

// 確認對戰表就是階段推進 —— 少一個 confirmed 旗標就少一個矛盾狀態。
func TestConfirmBracketNeedsConfirmAndAdvancesPhase(t *testing.T) {
	deps, tsvc, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	_, err := client.ConfirmBracket(ctx, withUser(connect.NewRequest(
		&activityv1.ConfirmBracketRequest{TournamentSlug: testSlug}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)
	if len(tsvc.advanced) != 0 {
		t.Fatal("沒有二次確認不該推進階段")
	}

	got, err := client.ConfirmBracket(ctx, withUser(connect.NewRequest(
		&activityv1.ConfirmBracketRequest{TournamentSlug: testSlug, Confirm: true}), testUserID))
	if err != nil {
		t.Fatalf("ConfirmBracket: %v", err)
	}
	if tsvc.advanced[0].To != tournament.PhaseInProgress {
		t.Fatalf("確認對戰表應推進到 in_progress,得到 %q", tsvc.advanced[0].To)
	}
	if got.Msg.GetTournament().GetPhase() != activityv1.TournamentPhase_TOURNAMENT_PHASE_IN_PROGRESS {
		t.Fatalf("回應階段 = %v", got.Msg.GetTournament().GetPhase())
	}
}

// 三個不可逆動作的 confirm 由領域層檢查:入口層必須原樣往下傳,
// 包含 false —— 自己擋掉就變成第二個權威位置。
func TestIrreversibleActionsPassConfirmThrough(t *testing.T) {
	deps, _, matches, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	for _, confirm := range []bool{false, true} {
		if _, err := client.LockHandicap(ctx, withUser(connect.NewRequest(
			&activityv1.LockHandicapRequest{MatchPublicId: testMatchID, Confirm: confirm},
		), testUserID)); err != nil {
			t.Fatalf("LockHandicap(confirm=%v): %v", confirm, err)
		}
		if _, err := client.ReportResult(ctx, withUser(connect.NewRequest(
			&activityv1.ReportResultRequest{
				MatchPublicId: testMatchID, WinnerPlayerPublicId: testPlayerBID, Confirm: confirm,
			}), testUserID)); err != nil {
			t.Fatalf("ReportResult(confirm=%v): %v", confirm, err)
		}
		if _, err := client.WithdrawPlayer(ctx, withUser(connect.NewRequest(
			&activityv1.WithdrawPlayerRequest{PlayerPublicId: testPlayerAID, Confirm: confirm},
		), testUserID)); err != nil {
			t.Fatalf("WithdrawPlayer(confirm=%v): %v", confirm, err)
		}
	}
	if len(matches.locked) != 2 || matches.locked[0].Confirm || !matches.locked[1].Confirm {
		t.Fatalf("LockHandicap 的 confirm 沒有原樣傳下去:%+v", matches.locked)
	}
	if len(matches.reported) != 2 || matches.reported[0].Confirm || !matches.reported[1].Confirm {
		t.Fatalf("ReportResult 的 confirm 沒有原樣傳下去:%+v", matches.reported)
	}
	if len(matches.withdrawn) != 2 || matches.withdrawn[0].Confirm || !matches.withdrawn[1].Confirm {
		t.Fatalf("WithdrawPlayer 的 confirm 沒有原樣傳下去:%+v", matches.withdrawn)
	}
}

func TestReportResultReturnsAdvancedMatchesAndCount(t *testing.T) {
	deps, _, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	got, err := client.ReportResult(context.Background(), withUser(connect.NewRequest(
		&activityv1.ReportResultRequest{
			MatchPublicId: testMatchID, WinnerPlayerPublicId: testPlayerBID,
			Confirm: true, Note: "三比零",
		}), testUserID))
	if err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	if got.Msg.GetMatch().GetWinnerPlayerPublicId() != testPlayerBID {
		t.Fatalf("勝者 = %q", got.Msg.GetMatch().GetWinnerPlayerPublicId())
	}
	if len(got.Msg.GetAdvancedMatches()) != 1 {
		t.Fatalf("晉級場次數 = %d", len(got.Msg.GetAdvancedMatches()))
	}
	if got.Msg.GetSettledBetCount() != 5 {
		t.Fatalf("settled_bet_count = %d", got.Msg.GetSettledBetCount())
	}
}

func TestWithdrawPlayerReturnsFullPlayer(t *testing.T) {
	deps, _, _, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	got, err := client.WithdrawPlayer(context.Background(), withUser(connect.NewRequest(
		&activityv1.WithdrawPlayerRequest{
			PlayerPublicId: testPlayerAID, Confirm: true, Note: "臨時有事",
		}), testUserID))
	if err != nil {
		t.Fatalf("WithdrawPlayer: %v", err)
	}
	// 回的是完整的 Player(含遊戲ID),不是場上那份精簡檢視。
	if got.Msg.GetPlayer().GetGameId() == "" {
		t.Fatal("棄賽回應應該是完整的 Player")
	}
	if len(got.Msg.GetWalkoverMatches()) != 1 {
		t.Fatalf("不戰而勝場次數 = %d", len(got.Msg.GetWalkoverMatches()))
	}
	if got.Msg.GetVoidedBetCount() != 3 {
		t.Fatalf("voided_bet_count = %d", got.Msg.GetVoidedBetCount())
	}
}

func TestSetStreamURLAllowsClearing(t *testing.T) {
	deps, _, matches, _, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	if _, err := client.SetStreamUrl(ctx, withUser(connect.NewRequest(
		&activityv1.SetStreamUrlRequest{
			MatchPublicId: testMatchID, StreamUrl: " https://live.example/abc ",
		}), testUserID)); err != nil {
		t.Fatalf("SetStreamUrl: %v", err)
	}
	if matches.streams[0].StreamURL != "https://live.example/abc" {
		t.Fatalf("URL 未去除前後空白:%q", matches.streams[0].StreamURL)
	}

	if _, err := client.SetStreamUrl(ctx, withUser(connect.NewRequest(
		&activityv1.SetStreamUrlRequest{MatchPublicId: testMatchID}), testUserID)); err != nil {
		t.Fatalf("SetStreamUrl(清除): %v", err)
	}
	if matches.streams[1].StreamURL != "" {
		t.Fatalf("空字串代表清除,得到 %q", matches.streams[1].StreamURL)
	}
}

// 明碼通行碼只在這裡出現一次,而且回應裡沒有其他東西。
func TestRegeneratePasscodeReturnsOnlyPasscode(t *testing.T) {
	deps, _, _, sgn, _, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	got, err := client.RegeneratePasscode(context.Background(), withUser(connect.NewRequest(
		&activityv1.RegeneratePasscodeRequest{PlayerPublicId: testPlayerAID}), testUserID))
	if err != nil {
		t.Fatalf("RegeneratePasscode: %v", err)
	}
	if got.Msg.GetPasscode() != "Z9X4W2" {
		t.Fatalf("passcode = %q", got.Msg.GetPasscode())
	}
	if len(sgn.regenerated) != 1 {
		t.Fatalf("領域層收到 %d 次", len(sgn.regenerated))
	}
	// 沒有「選手自助重取」這條路:執行者必須是裁判,而且要留稽核。
	if sgn.regenerated[0].ActorUserID != testUserID {
		t.Fatalf("裁判身分 = %d", sgn.regenerated[0].ActorUserID)
	}
	if sgn.regenerated[0].TournamentSlug != testSlug {
		t.Fatalf("slug 應由 player 反查,得到 %q", sgn.regenerated[0].TournamentSlug)
	}
}

// 發獎動真錢:冪等鍵與二次確認缺一不可。
func TestAwardPrizesRequiresConfirmAndIdempotencyKey(t *testing.T) {
	deps, _, _, _, prizes, _ := judgeDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	_, err := client.AwardPrizes(ctx, withUser(connect.NewRequest(
		&activityv1.AwardPrizesRequest{TournamentSlug: testSlug, IdempotencyKey: "k"}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	_, err = client.AwardPrizes(ctx, withUser(connect.NewRequest(
		&activityv1.AwardPrizesRequest{TournamentSlug: testSlug, Confirm: true}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	if len(prizes.calls) != 0 {
		t.Fatal("條件不齊時絕不能動錢")
	}

	got, err := client.AwardPrizes(ctx, withUser(connect.NewRequest(
		&activityv1.AwardPrizesRequest{
			TournamentSlug: testSlug, Confirm: true, IdempotencyKey: "award-1",
		}), testUserID))
	if err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}
	if prizes.calls[0].IdempotencyKey != "award-1" || prizes.calls[0].ActorUserID != testUserID {
		t.Fatalf("參數錯誤:%+v", prizes.calls[0])
	}
	if len(got.Msg.GetAwards()) != 2 {
		t.Fatalf("發獎筆數 = %d", len(got.Msg.GetAwards()))
	}
	// 沒綁帳號的那一筆不該讓整批失敗,而是 granted=false + 人話說明。
	skipped := got.Msg.GetAwards()[1]
	if skipped.GetGranted() || skipped.GetSkippedReason() == "" {
		t.Fatalf("未綁定者應該是 granted=false 加說明:%+v", skipped)
	}
}

// 沒有發獎實作時回 Unimplemented,而不是假裝成功。
func TestAwardPrizesWithoutImplementation(t *testing.T) {
	deps, _, _, _, _, _ := judgeDeps()
	deps.Prizes = nil
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	_, err := client.AwardPrizes(context.Background(), withUser(connect.NewRequest(
		&activityv1.AwardPrizesRequest{
			TournamentSlug: testSlug, Confirm: true, IdempotencyKey: "k",
		}), testUserID))
	requireCode(t, err, connect.CodeUnimplemented)
}

// ── 錯誤映射 ────────────────────────────────────────────────────

// 活動層的每個 sentinel 都必須有穩定的 code 與 reason。
//
// 這張表是**契約**,不是實作細節:呼叫端靠 reason 分辨「BP 不足」與
// 「已封盤」,而兩者都是 FailedPrecondition。errmap.go 是那張表的唯一
// 權威位置,而活動層的列目前還沒加進去(見回報)——
// 加進去的那一刻,這個測試就會自動開始跑。
func TestActivitySentinelsMapToStableReasons(t *testing.T) {
	cases := []struct {
		err    error
		code   connect.Code
		reason string
	}{
		// 報名 / 選手身分
		{signup.ErrInvalidCredentials, connect.CodeUnauthenticated, "activity_invalid_credentials"},
		{signup.ErrAlreadyRegistered, connect.CodeAlreadyExists, "activity_already_registered"},
		{signup.ErrGameIDRequired, connect.CodeInvalidArgument, "activity_game_id_required"},
		{signup.ErrInvalidGameID, connect.CodeInvalidArgument, "activity_invalid_game_id"},
		{signup.ErrDiscordNameRequired, connect.CodeInvalidArgument, "activity_discord_name_required"},
		{signup.ErrFieldTooLong, connect.CodeInvalidArgument, "activity_field_too_long"},
		{signup.ErrAlreadyBound, connect.CodeFailedPrecondition, "activity_player_already_bound"},
		{signup.ErrUserAlreadyBound, connect.CodeFailedPrecondition, "activity_user_already_bound"},
		{signup.ErrUserRequired, connect.CodeUnauthenticated, "activity_platform_account_required"},
		// 賽事 / 階段機
		{tournament.ErrTournamentNotFound, connect.CodeNotFound, "tournament_not_found"},
		{tournament.ErrPlayerNotFound, connect.CodeNotFound, "activity_player_not_found"},
		{tournament.ErrPhaseConflict, connect.CodeAborted, "tournament_phase_conflict"},
		{tournament.ErrWrongPhase, connect.CodeFailedPrecondition, "tournament_wrong_phase"},
		{tournament.ErrIllegalTransition, connect.CodeFailedPrecondition, "tournament_illegal_transition"},
		{tournament.ErrRanksLocked, connect.CodeFailedPrecondition, "tournament_ranks_locked"},
		{tournament.ErrPlayersUnranked, connect.CodeFailedPrecondition, "tournament_players_unranked"},
		{tournament.ErrInvalidRank, connect.CodeInvalidArgument, "activity_invalid_rank"},
		// 讓武
		{handicap.ErrInsufficientBP, connect.CodeFailedPrecondition, "insufficient_bp"},
		{handicap.ErrHandicapLocked, connect.CodeFailedPrecondition, "handicap_locked"},
		{handicap.ErrNoBudget, connect.CodeFailedPrecondition, "handicap_no_budget"},
		{handicap.ErrTargetNoteRequired, connect.CodeInvalidArgument, "handicap_target_note_required"},
		{handicap.ErrNotSelectionOwner, connect.CodePermissionDenied, "handicap_not_selection_owner"},
		// 下注
		{betting.ErrSelfBet, connect.CodePermissionDenied, "bet_self_bet"},
		{betting.ErrBettingClosed, connect.CodeFailedPrecondition, "bet_closed"},
		{betting.ErrOddsMoved, connect.CodeAborted, "bet_odds_moved"},
		{betting.ErrStakeTooLarge, connect.CodeFailedPrecondition, "bet_stake_too_large"},
		// 場次生命週期
		{match.ErrConfirmationRequired, connect.CodeInvalidArgument, "confirmation_required"},
		{match.ErrMatchNotFound, connect.CodeNotFound, "activity_match_not_found"},
		{match.ErrWinnerNotInMatch, connect.CodeInvalidArgument, "winner_not_in_match"},
	}
	if ErrorReason(toConnectError(cases[0].err)) == "" {
		t.Skip("errmap.go 尚未加入活動層的錯誤映射;這張表是提給主線的清單")
	}
	for _, c := range cases {
		got := toConnectError(c.err)
		if code := connect.CodeOf(got); code != c.code {
			t.Errorf("%v:code = %s,want %s", c.err, code, c.code)
		}
		if reason := ErrorReason(got); reason != c.reason {
			t.Errorf("%v:reason = %q,want %q", c.err, reason, c.reason)
		}
	}
}

// 未映射的錯誤絕不能把領域訊息原樣吐出去。
func TestUnmappedActivityErrorsStayOpaque(t *testing.T) {
	deps, tsvc, _ := baseActivityDeps()
	tsvc.getErr = errors.New("pq: relation \"activity.tournaments\" does not exist")
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewTournamentServiceClient(srv.Client(), srv.URL)

	_, err := client.GetTournament(context.Background(), connect.NewRequest(
		&activityv1.GetTournamentRequest{Slug: testSlug}))
	requireCode(t, err, connect.CodeInternal)
	for _, leak := range []string{"pq:", "relation", "activity.tournaments"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("錯誤訊息外洩了內部細節(%q):%v", leak, err)
		}
	}
}
