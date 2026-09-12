package transport

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/betting"
)

type fakeActivityBetting struct {
	votes   []betting.VoteParams
	voteRes *betting.MatchOdds
	voteErr error

	oddsQueries []betting.GetOddsParams
	odds        []betting.MatchOdds

	placed   []betting.PlaceBetParams
	placeRes *betting.PlaceBetResult
	placeErr error

	listed []betting.ListMyBetsParams
	myBets []betting.Bet
}

func (f *fakeActivityBetting) Vote(_ context.Context, p betting.VoteParams) (*betting.MatchOdds, error) {
	f.votes = append(f.votes, p)
	if f.voteErr != nil {
		return nil, f.voteErr
	}
	return f.voteRes, nil
}

func (f *fakeActivityBetting) GetOdds(
	_ context.Context, p betting.GetOddsParams,
) ([]betting.MatchOdds, error) {
	f.oddsQueries = append(f.oddsQueries, p)
	return f.odds, nil
}

func (f *fakeActivityBetting) PlaceBet(
	_ context.Context, p betting.PlaceBetParams,
) (*betting.PlaceBetResult, error) {
	f.placed = append(f.placed, p)
	if f.placeErr != nil {
		return nil, f.placeErr
	}
	return f.placeRes, nil
}

func (f *fakeActivityBetting) ListMyBets(
	_ context.Context, p betting.ListMyBetsParams,
) ([]betting.Bet, error) {
	f.listed = append(f.listed, p)
	return f.myBets, nil
}

func bettingDeps() (ActivityDeps, *fakeActivityBetting) {
	deps, _, _ := baseActivityDeps()
	odds := betting.MatchOdds{
		MatchPublicID: testMatchID,
		P1Votes:       7, P2Votes: 3,
		P1OddsMilli: 1430, P2OddsMilli: 2860,
		OpenForBets: true, MyVote: betting.SideP1,
	}
	settled := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	svc := &fakeActivityBetting{
		voteRes: &odds,
		odds:    []betting.MatchOdds{odds},
		placeRes: &betting.PlaceBetResult{
			Bet: betting.Bet{
				PublicID: "01BET", Stake: 100, PotentialPayout: 286,
				Status: betting.BetOpen,
				Legs: []betting.Leg{{
					MatchPublicID: testMatchID, Round: 1, Slot: 0,
					Side: betting.SideP2, SideDisplayName: "選手B",
					OddsMilli: 2860, Result: betting.LegPending,
				}},
				CreatedAt: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC),
			},
			Replayed: true,
		},
		myBets: []betting.Bet{{
			PublicID: "01BETOLD", Stake: 50, PotentialPayout: 0,
			Status: betting.BetLost, SettledAt: &settled,
		}},
	}
	deps.Betting = svc
	return deps, svc
}

// 賠率是公開資訊:不登入就看得到,viewer 是 0。
func TestGetOddsIsAnonymousReadable(t *testing.T) {
	deps, svc := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	got, err := client.GetOdds(ctx, connect.NewRequest(&activityv1.GetOddsRequest{
		MatchPublicIds: []string{testMatchID},
	}))
	if err != nil {
		t.Fatalf("GetOdds(匿名): %v", err)
	}
	if len(got.Msg.GetOdds()) != 1 {
		t.Fatalf("賠率筆數 = %d", len(got.Msg.GetOdds()))
	}
	if got.Msg.GetOdds()[0].GetP1OddsMilli() != 1430 {
		t.Fatalf("p1_odds_milli = %d", got.Msg.GetOdds()[0].GetP1OddsMilli())
	}
	if svc.oddsQueries[0].ViewerUserID != 0 {
		t.Fatalf("匿名查詢的 viewer = %d,want 0", svc.oddsQueries[0].ViewerUserID)
	}

	// 已登入:viewer 是自己,只影響 my_vote(賠率對所有人都一樣)。
	if _, err := client.GetOdds(ctx, withUser(connect.NewRequest(&activityv1.GetOddsRequest{
		MatchPublicIds: []string{testMatchID},
	}), testUserID)); err != nil {
		t.Fatalf("GetOdds(已登入): %v", err)
	}
	if svc.oddsQueries[1].ViewerUserID != testUserID {
		t.Fatalf("已登入的 viewer = %d,want %d", svc.oddsQueries[1].ViewerUserID, testUserID)
	}
}

func TestGetOddsRejectsEmptyAndOversizedQueries(t *testing.T) {
	deps, _ := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	_, err := client.GetOdds(ctx, connect.NewRequest(&activityv1.GetOddsRequest{}))
	requireCode(t, err, connect.CodeInvalidArgument)

	// 只有空白的 id 等於沒給。
	_, err = client.GetOdds(ctx, connect.NewRequest(&activityv1.GetOddsRequest{
		MatchPublicIds: []string{"  ", ""},
	}))
	requireCode(t, err, connect.CodeInvalidArgument)

	tooMany := make([]string, maxOddsQueryIDs+1)
	for i := range tooMany {
		tooMany[i] = testMatchID
	}
	_, err = client.GetOdds(ctx, connect.NewRequest(&activityv1.GetOddsRequest{
		MatchPublicIds: tooMany,
	}))
	requireCode(t, err, connect.CodeInvalidArgument)
}

// 投票與下注動的是真錢與賠率,一律要平台帳號 —— 活動層 session 在這裡不算數。
func TestBettingWritesRequirePlatformAccount(t *testing.T) {
	deps, svc := bettingDeps()
	deps.Sessions = &fakeActivitySessions{byToken: map[string]ActivityIdentity{
		testSessionTok: {TournamentSlug: testSlug, PlayerPublicID: testPlayerAID},
	}}
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	_, err := client.Vote(ctx, connect.NewRequest(&activityv1.VoteRequest{
		MatchPublicId: testMatchID, Side: 1,
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 只有選手 session 也不行。
	_, err = client.Vote(ctx, withPlayerSession(connect.NewRequest(&activityv1.VoteRequest{
		MatchPublicId: testMatchID, Side: 1,
	}), testSessionTok))
	requireCode(t, err, connect.CodeUnauthenticated)

	_, err = client.PlaceBet(ctx, connect.NewRequest(&activityv1.PlaceBetRequest{
		TournamentSlug: testSlug, Stake: 100,
		Legs:           []*activityv1.BetLegInput{{MatchPublicId: testMatchID, Side: 1}},
		IdempotencyKey: "k1",
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	_, err = client.ListMyBets(ctx, connect.NewRequest(&activityv1.ListMyBetsRequest{
		TournamentSlug: testSlug,
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	if len(svc.votes)+len(svc.placed)+len(svc.listed) != 0 {
		t.Fatal("未認證的請求不該碰領域層")
	}
}

func TestVoteRequiresValidSide(t *testing.T) {
	deps, svc := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	// side 沒填(proto3 沒有 required,漏填就是 0)。
	_, err := client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		MatchPublicId: testMatchID,
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	_, err = client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		MatchPublicId: testMatchID, Side: 3,
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	_, err = client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		Side: 1,
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	if len(svc.votes) != 0 {
		t.Fatal("不合法的 side 不該進到領域層")
	}

	got, err := client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		MatchPublicId: testMatchID, Side: 2,
	}), testUserID))
	if err != nil {
		t.Fatalf("Vote: %v", err)
	}
	if svc.votes[0].Side != betting.SideP2 || svc.votes[0].UserID != testUserID {
		t.Fatalf("投票參數錯誤:%+v", svc.votes[0])
	}
	// 投完一併回賠率:票數變了賠率就會變。
	if got.Msg.GetOdds().GetP1Votes() != 7 {
		t.Fatalf("p1_votes = %d", got.Msg.GetOdds().GetP1Votes())
	}
	if got.Msg.GetOdds().GetMyVote() != int32(betting.SideP1) {
		t.Fatalf("my_vote = %d", got.Msg.GetOdds().GetMyVote())
	}
}

// 動錢的 RPC 一律要冪等鍵:下注沒有任何 UNIQUE 約束擋得住連點。
func TestPlaceBetRequiresIdempotencyKey(t *testing.T) {
	deps, svc := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)

	_, err := client.PlaceBet(context.Background(), withUser(connect.NewRequest(
		&activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100,
			Legs: []*activityv1.BetLegInput{{MatchPublicId: testMatchID, Side: 1}},
		}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)
	if len(svc.placed) != 0 {
		t.Fatal("沒有冪等鍵的下注絕不能進到領域層")
	}
}

func TestPlaceBetRejectsMalformedRequests(t *testing.T) {
	deps, _ := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *activityv1.PlaceBetRequest
	}{
		{"沒有 slug", &activityv1.PlaceBetRequest{
			Stake: 100, IdempotencyKey: "k",
			Legs: []*activityv1.BetLegInput{{MatchPublicId: testMatchID, Side: 1}},
		}},
		{"沒有腿", &activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100, IdempotencyKey: "k",
		}},
		{"某一腿缺 match", &activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100, IdempotencyKey: "k",
			Legs: []*activityv1.BetLegInput{{Side: 1}},
		}},
		{"某一腿的 side 不合法", &activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100, IdempotencyKey: "k",
			Legs: []*activityv1.BetLegInput{{MatchPublicId: testMatchID, Side: 9}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := client.PlaceBet(ctx, withUser(connect.NewRequest(c.req), testUserID))
			requireCode(t, err, connect.CodeInvalidArgument)
		})
	}
}

func TestPlaceBetPassesEverythingDown(t *testing.T) {
	deps, svc := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)

	got, err := client.PlaceBet(context.Background(), withUser(connect.NewRequest(
		&activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100,
			Legs: []*activityv1.BetLegInput{
				{MatchPublicId: testMatchID, Side: 2},
			},
			IdempotencyKey: " key-1 ",
			ExpectedPayout: 286,
		}), testUserID))
	if err != nil {
		t.Fatalf("PlaceBet: %v", err)
	}
	p := svc.placed[0]
	if p.UserID != testUserID || p.Stake != 100 || p.ExpectedPayout != 286 {
		t.Fatalf("參數錯誤:%+v", p)
	}
	if p.IdempotencyKey != "key-1" {
		t.Fatalf("冪等鍵應去掉前後空白:%q", p.IdempotencyKey)
	}
	if len(p.Legs) != 1 || p.Legs[0].Side != betting.SideP2 {
		t.Fatalf("腿轉換錯誤:%+v", p.Legs)
	}
	if !got.Msg.GetReplayed() {
		t.Fatal("replayed 應原樣回傳")
	}
	leg := got.Msg.GetBet().GetLegs()[0]
	if leg.GetOddsMilli() != 2860 {
		t.Fatalf("odds_milli = %d(必須是下注當下鎖定的值)", leg.GetOddsMilli())
	}
	if leg.GetMatchLabel() == "" {
		t.Fatal("match_label 應該有東西可顯示")
	}
}

func TestListMyBetsAlwaysScopedToCaller(t *testing.T) {
	deps, svc := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)

	got, err := client.ListMyBets(context.Background(), withUser(connect.NewRequest(
		&activityv1.ListMyBetsRequest{TournamentSlug: testSlug, OpenOnly: true}), testUserID))
	if err != nil {
		t.Fatalf("ListMyBets: %v", err)
	}
	if svc.listed[0].UserID != testUserID || !svc.listed[0].OpenOnly {
		t.Fatalf("查詢參數錯誤:%+v", svc.listed[0])
	}
	if len(got.Msg.GetBets()) != 1 {
		t.Fatalf("注單數 = %d", len(got.Msg.GetBets()))
	}
	if got.Msg.GetBets()[0].GetStatus() != activityv1.BetStatus_BET_STATUS_LOST {
		t.Fatalf("status = %v", got.Msg.GetBets()[0].GetStatus())
	}
	if got.Msg.GetBets()[0].GetSettledAt() == nil {
		t.Fatal("已結算的注單要有 settled_at")
	}
}

// 票數在領域層是 int64,契約是 int32。靜默截斷會讓票數變成負的,
// 而負票數會讓賠率算出荒謬的值。
func TestClampInt32(t *testing.T) {
	cases := map[int64]int32{
		0: 0, 7: 7, -1: 0,
		int64(1) << 40: 2147483647,
	}
	for in, want := range cases {
		if got := clampInt32(in); got != want {
			t.Fatalf("clampInt32(%d) = %d,want %d", in, got, want)
		}
	}
}
