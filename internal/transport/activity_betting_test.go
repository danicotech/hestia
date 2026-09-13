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

const testMarketID = "01MARKETWINNER0000000000"

type fakeActivityBetting struct {
	votes   []betting.VoteParams
	voteRes *betting.MarketOdds
	voteErr error

	oddsQueries []betting.GetOddsParams
	odds        []betting.MatchOdds

	placed   []betting.PlaceBetParams
	placeRes *betting.PlaceBetResult
	placeErr error

	listed []betting.ListMyBetsParams
	myBets []betting.Bet
}

func (f *fakeActivityBetting) Vote(_ context.Context, p betting.VoteParams) (*betting.MarketOdds, error) {
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

// testMarketOdds 是一場的兩個盤口:整場勝負(開著)與第 2 回合時長(已關)。
// 兩種狀態並存,轉換層的 open_for_bets 才驗得出「場次開著 ≠ 盤口開著」。
func testMarketOdds() betting.MatchOdds {
	winner := betting.MarketOdds{
		Market: betting.Market{
			ID: 1, PublicID: testMarketID, MatchID: 1,
			Kind: betting.MarketMatchWinner, Status: betting.MarketOpen,
		},
		MatchPublicID: testMatchID,
		Outcomes:      []betting.Outcome{betting.OutcomeP1, betting.OutcomeP2},
		Odds:          map[betting.Outcome]int64{betting.OutcomeP1: 1430, betting.OutcomeP2: 2860},
		Votes:         map[betting.Outcome]int64{betting.OutcomeP1: 7, betting.OutcomeP2: 3},
		Labels:        map[betting.Outcome]string{betting.OutcomeP1: "選手A", betting.OutcomeP2: "選手B"},
		MyVote:        betting.OutcomeP1,
	}
	duration := betting.MarketOdds{
		Market: betting.Market{
			ID: 2, PublicID: "01MARKETDURATION00000000", MatchID: 1,
			Kind: betting.MarketDuration, RoundNo: 2, LineSeconds: 90, Status: betting.MarketClosed,
		},
		MatchPublicID: testMatchID,
		Outcomes:      []betting.Outcome{betting.OutcomeOver, betting.OutcomeUnder},
		Odds:          map[betting.Outcome]int64{betting.OutcomeOver: 1840, betting.OutcomeUnder: 1840},
		Votes:         map[betting.Outcome]int64{},
		Labels:        map[betting.Outcome]string{betting.OutcomeOver: "超過 90 秒", betting.OutcomeUnder: "90 秒內"},
	}
	return betting.MatchOdds{
		MatchPublicID: testMatchID, OpenForBets: true,
		Markets: []betting.MarketOdds{winner, duration},
	}
}

func bettingDeps() (ActivityDeps, *fakeActivityBetting) {
	deps, _, _ := baseActivityDeps()
	odds := testMarketOdds()
	settled := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	svc := &fakeActivityBetting{
		voteRes: &odds.Markets[0],
		odds:    []betting.MatchOdds{odds},
		placeRes: &betting.PlaceBetResult{
			Bet: betting.Bet{
				PublicID: "01BET", Stake: 100, PotentialPayout: 286,
				Status: betting.BetOpen,
				Legs: []betting.Leg{{
					MatchPublicID: testMatchID, Round: 1, Slot: 0,
					MarketPublicID: testMarketID, MarketKind: betting.MarketMatchWinner,
					Outcome: betting.OutcomeP2, OutcomeLabel: "選手B",
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
	markets := got.Msg.GetOdds()[0].GetMarkets()
	if len(markets) != 2 {
		t.Fatalf("盤口數 = %d,want 2", len(markets))
	}
	winner := markets[0]
	if winner.GetKind() != activityv1.MarketKind_MARKET_KIND_MATCH_WINNER || winner.GetMarketPublicId() != testMarketID {
		t.Fatalf("第一個盤口:%v", winner)
	}
	if got := winner.GetOutcomes(); len(got) != 2 || got[0].GetOutcome() != "p1" ||
		got[0].GetOddsMilli() != 1430 || got[0].GetVotes() != 7 || got[0].GetLabel() != "選手A" {
		t.Fatalf("結果列表:%v", got)
	}
	if !winner.GetOpenForBets() {
		t.Fatal("開著的盤口 + 開著的場次 → open_for_bets 應為 true")
	}
	// 場次開著但盤口已關:open_for_bets 看的是兩層 AND。
	duration := markets[1]
	if duration.GetOpenForBets() || duration.GetStatus() != activityv1.MarketStatus_MARKET_STATUS_CLOSED {
		t.Fatalf("已關的盤口不該可下注:%v", duration)
	}
	if duration.GetRoundNo() != 2 || duration.GetLineSeconds() != 90 {
		t.Fatalf("時長盤的回合與線:%v", duration)
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
		MarketPublicId: testMarketID, Outcome: "p1",
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 只有選手 session 也不行。
	_, err = client.Vote(ctx, withPlayerSession(connect.NewRequest(&activityv1.VoteRequest{
		MarketPublicId: testMarketID, Outcome: "p1",
	}), testSessionTok))
	requireCode(t, err, connect.CodeUnauthenticated)

	_, err = client.PlaceBet(ctx, connect.NewRequest(&activityv1.PlaceBetRequest{
		TournamentSlug: testSlug, Stake: 100,
		Legs:           []*activityv1.BetLegInput{{MarketPublicId: testMarketID, Outcome: "p1"}},
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

func TestVoteRequiresMarketAndOutcome(t *testing.T) {
	deps, svc := bettingDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	// outcome 沒填(proto3 沒有 required,漏填就是空字串 = 領域層的「未投票」)。
	_, err := client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		MarketPublicId: testMarketID,
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	_, err = client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		MarketPublicId: testMarketID, Outcome: "   ",
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	_, err = client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		Outcome: "p1",
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)

	if len(svc.votes) != 0 {
		t.Fatal("組不起來的請求不該進到領域層")
	}

	got, err := client.Vote(ctx, withUser(connect.NewRequest(&activityv1.VoteRequest{
		MarketPublicId: testMarketID, Outcome: " p2 ",
	}), testUserID))
	if err != nil {
		t.Fatalf("Vote: %v", err)
	}
	if svc.votes[0].Outcome != betting.OutcomeP2 || svc.votes[0].UserID != testUserID ||
		svc.votes[0].MarketPublicID != testMarketID {
		t.Fatalf("投票參數錯誤:%+v", svc.votes[0])
	}
	// 投完一併回該盤口的賠率:票數變了賠率就會變。
	odds := got.Msg.GetOdds()
	if odds.GetMarketPublicId() != testMarketID || odds.GetMatchPublicId() != testMatchID {
		t.Fatalf("回的盤口:%v", odds)
	}
	if odds.GetOutcomes()[0].GetVotes() != 7 {
		t.Fatalf("p1 votes = %d", odds.GetOutcomes()[0].GetVotes())
	}
	if odds.GetMyVote() != "p1" {
		t.Fatalf("my_vote = %q", odds.GetMyVote())
	}
	if !odds.GetOpenForBets() {
		t.Fatal("投票能成功就表示場次還開著;盤口 open → open_for_bets 應為 true")
	}
}

// 「結果屬不屬於這個盤口」是領域層的判斷(要看 kind 與 best_of),入口層放行。
func TestVoteLeavesOutcomeValidationToDomain(t *testing.T) {
	deps, svc := bettingDeps()
	svc.voteErr = betting.ErrOutcomeInvalid
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewBettingServiceClient(srv.Client(), srv.URL)

	_, err := client.Vote(context.Background(), withUser(connect.NewRequest(&activityv1.VoteRequest{
		MarketPublicId: testMarketID, Outcome: "over",
	}), testUserID))
	requireCode(t, err, connect.CodeInvalidArgument)
	if len(svc.votes) != 1 {
		t.Fatalf("領域層應被呼叫一次,實際 %d 次", len(svc.votes))
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
			Legs: []*activityv1.BetLegInput{{MarketPublicId: testMarketID, Outcome: "p1"}},
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
			Legs: []*activityv1.BetLegInput{{MarketPublicId: testMarketID, Outcome: "p1"}},
		}},
		{"沒有腿", &activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100, IdempotencyKey: "k",
		}},
		{"某一腿缺盤口", &activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100, IdempotencyKey: "k",
			Legs: []*activityv1.BetLegInput{{Outcome: "p1"}},
		}},
		{"某一腿缺結果", &activityv1.PlaceBetRequest{
			TournamentSlug: testSlug, Stake: 100, IdempotencyKey: "k",
			Legs: []*activityv1.BetLegInput{{MarketPublicId: testMarketID}},
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
				{MarketPublicId: " " + testMarketID + " ", Outcome: "p2"},
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
	if len(p.Legs) != 1 || p.Legs[0].Outcome != betting.OutcomeP2 || p.Legs[0].MarketPublicID != testMarketID {
		t.Fatalf("腿轉換錯誤:%+v", p.Legs)
	}
	if !got.Msg.GetReplayed() {
		t.Fatal("replayed 應原樣回傳")
	}
	leg := got.Msg.GetBet().GetLegs()[0]
	if leg.GetOddsMilli() != 2860 {
		t.Fatalf("odds_milli = %d(必須是下注當下鎖定的值)", leg.GetOddsMilli())
	}
	if leg.GetMarketPublicId() != testMarketID || leg.GetKind() != activityv1.MarketKind_MARKET_KIND_MATCH_WINNER ||
		leg.GetOutcome() != "p2" || leg.GetOutcomeLabel() != "選手B" {
		t.Fatalf("腿的盤口與結果:%v", leg)
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
