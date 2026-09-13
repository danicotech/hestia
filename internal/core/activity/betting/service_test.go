package betting

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/rules"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// 測試場景:一屆賽事「試鋒」(三局兩勝、四種盤口、duration 線 90 秒),三場比賽。
//
//	M1  李璃(user 11)vs A冷(user 12)   —— 兩人都綁了平台帳號
//	M2  丙(純活動報名,fencer 綁 user 13)vs 丁(完全沒綁)
//	M3  戊 vs 己                          —— 都沒綁,用來組串關
//
// 每場 8 個盤口:match_winner、round_winner ×3、duration ×3、score。
// 下注者一律是 user 7,他不是任何一場的選手。
const (
	tourID   int64 = 1
	tourSlug       = "shifeng"
	bettor   int64 = 7
)

func setup(t *testing.T) (*Service[fakeTx], *fakeDB) {
	t.Helper()
	db := newFakeDB()
	db.addTournament(tourID, tourSlug, fullRules())
	db.addMatch(1, "M1", tourID, MatchReady, player(101, 11, "李璃"), player(102, 12, "A冷"))
	db.addMatch(2, "M2", tourID, MatchReady, unboundPlayer(103, 13, "丙"), anonPlayer(104, "丁"))
	db.addMatch(3, "M3", tourID, MatchReady, anonPlayer(105, "戊"), anonPlayer(106, "己"))
	db.credit(bettor, 10_000)

	svc := NewService[fakeTx](db, db, "")
	// 固定的 public_id 讓斷言讀得懂,也讓冪等測試看得出「回的是同一張注單」。
	var n atomic.Int64
	svc.newPublicID = func() (string, error) {
		return "BET" + strconv.FormatInt(n.Add(1), 10), nil
	}
	return svc, db
}

func mustPlace(t *testing.T, svc *Service[fakeTx], p PlaceBetParams) *PlaceBetResult {
	t.Helper()
	res, err := svc.PlaceBet(context.Background(), p)
	if err != nil {
		t.Fatalf("PlaceBet: %v", err)
	}
	return res
}

// leg 組一腿:某場某種盤口(逐回合盤口指定 roundNo)的某個結果。
func leg(db *fakeDB, matchID string, kind MarketKind, roundNo int, o Outcome) LegInput {
	mk := db.market(matchID, kind, roundNo)
	if mk == nil {
		panic("沒有這個盤口: " + matchID + " " + string(kind) + " " + strconv.Itoa(roundNo))
	}
	return LegInput{MatchPublicID: matchID, MarketPublicID: mk.PublicID, Outcome: o}
}

// winner 是最常用的一腿:整場勝負。
func winner(db *fakeDB, matchID string, o Outcome) []LegInput {
	return []LegInput{leg(db, matchID, MarketMatchWinner, 0, o)}
}

func vote(db *fakeDB, userID int64, matchID string, kind MarketKind, roundNo int, o Outcome) VoteParams {
	mk := db.market(matchID, kind, roundNo)
	return VoteParams{UserID: userID, MatchPublicID: matchID, MarketPublicID: mk.PublicID, Outcome: o}
}

// ── 投票 ──────────────────────────────────────────────────────────

func TestVote一盤口一票再投即改票(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()

	if _, err := svc.Vote(ctx, vote(db, 21, "M1", MarketMatchWinner, 0, OutcomeP1)); err != nil {
		t.Fatalf("第一次投票: %v", err)
	}
	odds, err := svc.Vote(ctx, vote(db, 21, "M1", MarketMatchWinner, 0, OutcomeP2))
	if err != nil {
		t.Fatalf("改票: %v", err)
	}
	if odds.Votes[OutcomeP1] != 0 || odds.Votes[OutcomeP2] != 1 {
		t.Fatalf("改票後票數 = %v,want p1=0 p2=1 —— 改票必須是覆蓋而不是再插一列", odds.Votes)
	}
	if odds.MyVote != OutcomeP2 {
		t.Fatalf("MyVote = %q,want p2", odds.MyVote)
	}
	if len(db.votes) != 1 {
		t.Fatalf("votes 有 %d 列,want 1(UNIQUE(market_id,user_id))", len(db.votes))
	}
	if odds.Kind != MarketMatchWinner || !slices.Equal(odds.Outcomes, []Outcome{OutcomeP1, OutcomeP2}) {
		t.Fatalf("回的盤口不對: %+v", odds.Market)
	}

	// 同一人對另一個盤口投票是另一票:一場每盤口一票。
	if _, err := svc.Vote(ctx, vote(db, 21, "M1", MarketScore, 0, "p1_2_0")); err != nil {
		t.Fatalf("投比分盤: %v", err)
	}
	if len(db.votes) != 2 {
		t.Fatalf("votes 有 %d 列,want 2(不同盤口各一票)", len(db.votes))
	}
}

// 投票會直接動賠率,所以回應裡只能有票數 —— 誰投給誰不可以離開伺服器。
func TestGetOdds只回自己的票(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()

	for _, u := range []int64{21, 22, 23} {
		if _, err := svc.Vote(ctx, vote(db, u, "M1", MarketMatchWinner, 0, OutcomeP1)); err != nil {
			t.Fatalf("投票: %v", err)
		}
	}
	if _, err := svc.Vote(ctx, vote(db, 24, "M1", MarketMatchWinner, 0, OutcomeP2)); err != nil {
		t.Fatalf("投票: %v", err)
	}

	// 24 號查詢:看得到總票數,看得到自己那一票,看不到是誰投了 P1。
	got, err := svc.GetOdds(ctx, GetOddsParams{MatchPublicIDs: []string{"M1"}, ViewerUserID: 24})
	if err != nil {
		t.Fatalf("GetOdds: %v", err)
	}
	if len(got) != 1 || len(got[0].Markets) != 8 {
		t.Fatalf("回了 %d 場,want 1 場 8 個盤口: %+v", len(got), got)
	}
	mw := marketOddsOf(t, got[0], MarketMatchWinner, 0)
	if mw.Votes[OutcomeP1] != 3 || mw.Votes[OutcomeP2] != 1 {
		t.Fatalf("票數 = %v,want p1=3 p2=1", mw.Votes)
	}
	if mw.MyVote != OutcomeP2 {
		t.Fatalf("MyVote = %q,want p2", mw.MyVote)
	}
	// 沒投過的盤口 MyVote 空、票數零、賠率均等(四路 3.68)。
	sc := marketOddsOf(t, got[0], MarketScore, 0)
	if sc.MyVote != OutcomeNone || sc.Votes["p1_2_0"] != 0 || sc.Odds["p1_2_0"] != 3680 {
		t.Fatalf("比分盤 = %+v,want 零票 3.68 倍", sc)
	}

	// 匿名查詢:MyVote 必須是空,不能因為「只有一個 P2 票」就推論出來。
	anon, err := svc.GetOdds(ctx, GetOddsParams{MatchPublicIDs: []string{"M1"}})
	if err != nil {
		t.Fatalf("GetOdds 匿名: %v", err)
	}
	amw := marketOddsOf(t, anon[0], MarketMatchWinner, 0)
	if amw.MyVote != OutcomeNone {
		t.Fatalf("匿名的 MyVote = %q,want 空", amw.MyVote)
	}
	// 賠率:P1 三票 / 總四票 → 0.92 × 14 / 8 = 1.61;P2 → 0.92 × 14 / 6 = 2.146…
	if amw.Odds[OutcomeP1] != 1610 || amw.Odds[OutcomeP2] != 2146 {
		t.Fatalf("賠率 = %v,want p1=1610 p2=2146", amw.Odds)
	}
	if !anon[0].OpenForBets {
		t.Fatal("ready 的場次應可下注")
	}
}

func marketOddsOf(t *testing.T, mo MatchOdds, kind MarketKind, roundNo int) MarketOdds {
	t.Helper()
	for _, mk := range mo.Markets {
		if mk.Kind == kind && mk.RoundNo == roundNo {
			return mk
		}
	}
	t.Fatalf("回應裡沒有 %s round=%d 的盤口", kind, roundNo)
	return MarketOdds{}
}

func TestVote開賽後不能投(t *testing.T) {
	svc, db := setup(t)
	db.matches["M1"].Status = MatchLive
	_, err := svc.Vote(context.Background(), vote(db, 21, "M1", MarketMatchWinner, 0, OutcomeP1))
	if !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("err = %v,want ErrVotingClosed", err)
	}
}

// 盤口關了就不能投,即使場次狀態還允許(狀態與盤口是兩層)。
func TestVote關盤的盤口不能投(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()
	if _, err := svc.CloseMarketsInTx(ctx, fakeTx{}, "M1"); err != nil {
		t.Fatalf("關盤: %v", err)
	}
	_, err := svc.Vote(ctx, vote(db, 21, "M1", MarketMatchWinner, 0, OutcomeP1))
	if !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("err = %v,want ErrVotingClosed", err)
	}
	if len(db.votes) != 0 {
		t.Fatal("關盤後仍寫入了票")
	}
}

func TestVote結果不屬於盤口(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()
	cases := []struct {
		kind MarketKind
		o    Outcome
	}{
		{MarketMatchWinner, "over"},
		{MarketDuration, "p1"},
		{MarketScore, "p1_3_0"}, // best_of=3 沒有 3:0
		{MarketScore, "p1"},
	}
	for _, c := range cases {
		_, err := svc.Vote(ctx, vote(db, 21, "M1", c.kind, roundOf(c.kind), c.o))
		if !errors.Is(err, ErrOutcomeInvalid) {
			t.Fatalf("%s/%s err = %v,want ErrOutcomeInvalid", c.kind, c.o, err)
		}
	}
	// 合法的比分照收。
	if _, err := svc.Vote(ctx, vote(db, 21, "M1", MarketScore, 0, "p2_2_1")); err != nil {
		t.Fatalf("合法比分被拒: %v", err)
	}
	if len(db.votes) != 1 {
		t.Fatalf("votes 有 %d 列,want 1", len(db.votes))
	}
}

func roundOf(kind MarketKind) int {
	if kind.PerRound() {
		return 1
	}
	return 0
}

// 盤口必須屬於指定的場次:拿 M2 的盤口 id 配 M1 的場次 id 不能過。
func TestVote盤口不屬於場次(t *testing.T) {
	svc, db := setup(t)
	p := vote(db, 21, "M2", MarketMatchWinner, 0, OutcomeP1)
	p.MatchPublicID = "M1"
	_, err := svc.Vote(context.Background(), p)
	if !errors.Is(err, ErrMarketNotFound) {
		t.Fatalf("err = %v,want ErrMarketNotFound", err)
	}
}

// ── 盤口生命週期 ──────────────────────────────────────────────────

// 開發庫那種 config 要建出 1 + 3 + 3 + 1 = 8 個盤口;重複建要出聲。
func TestOpenMarketsInTx依規則建八個(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()
	db.matches["M9"] = &Match{ID: 9, PublicID: "M9", TournamentID: tourID, Status: MatchReady,
		P1: anonPlayer(901, "甲"), P2: anonPlayer(902, "乙")}

	markets, err := svc.OpenMarketsInTx(ctx, fakeTx{}, "M9")
	if err != nil {
		t.Fatalf("OpenMarketsInTx: %v", err)
	}
	if len(markets) != 8 {
		t.Fatalf("建了 %d 個盤口,want 8", len(markets))
	}
	count := map[MarketKind]int{}
	for _, mk := range markets {
		count[mk.Kind]++
		if mk.Status != MarketOpen || mk.PublicID == "" || mk.MatchID != 9 {
			t.Fatalf("盤口 %+v 不是 open / 缺 public_id / 場次錯", mk)
		}
		if mk.Kind == MarketDuration && mk.LineSeconds != 90 {
			t.Fatalf("duration 盤口 line = %d,want 90", mk.LineSeconds)
		}
		if mk.Kind.PerRound() != (mk.RoundNo != 0) {
			t.Fatalf("盤口 %+v 的 round_no 與 kind 不一致", mk)
		}
	}
	if count[MarketMatchWinner] != 1 || count[MarketRoundWinner] != 3 || count[MarketDuration] != 3 || count[MarketScore] != 1 {
		t.Fatalf("各種盤口數 = %v,want 1/3/3/1", count)
	}

	// 重複開盤撞唯一索引,必須出聲。
	if _, err := svc.OpenMarketsInTx(ctx, fakeTx{}, "M9"); !errors.Is(err, ErrMarketsExist) {
		t.Fatalf("重複開盤 err = %v,want ErrMarketsExist", err)
	}

	// 關盤:全部 open → closed,回 8;再關一次回 0。
	n, err := svc.CloseMarketsInTx(ctx, fakeTx{}, "M9")
	if err != nil || n != 8 {
		t.Fatalf("CloseMarketsInTx = (%d,%v),want (8,nil)", n, err)
	}
	if n, _ := svc.CloseMarketsInTx(ctx, fakeTx{}, "M9"); n != 0 {
		t.Fatalf("第二次關盤 = %d,want 0", n)
	}
	for _, mk := range db.markets {
		if mk.MatchID == 9 && mk.Status != MarketClosed {
			t.Fatalf("盤口 %+v 沒關", mk)
		}
	}
}

func TestOpenMarketsInTx不開盤的賽事不建(t *testing.T) {
	svc, db := setup(t)
	off := fullRules()
	off.Config.Betting.Enabled = false
	db.addTournament(2, "off", off)
	db.matches["M9"] = &Match{ID: 9, PublicID: "M9", TournamentID: 2, Status: MatchReady}
	markets, err := svc.OpenMarketsInTx(context.Background(), fakeTx{}, "M9")
	if err != nil || len(markets) != 0 {
		t.Fatalf("= (%v,%v),want 空", markets, err)
	}
}

// ── 下注 ──────────────────────────────────────────────────────────

func TestPlaceBet扣款與鎖定賠率(t *testing.T) {
	svc, db := setup(t)
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k1",
	})

	// 零票時兩邊都是 1.84 → 100 × 1.84 = 184。
	if res.Bet.PotentialPayout != 184 {
		t.Fatalf("potential_payout = %d,want 184", res.Bet.PotentialPayout)
	}
	if len(res.Bet.Legs) != 1 || res.Bet.Legs[0].OddsMilli != 1840 {
		t.Fatalf("腿 = %+v,want odds_milli 1840", res.Bet.Legs)
	}
	l := res.Bet.Legs[0]
	if l.OutcomeLabel != "李璃" || l.MarketKind != MarketMatchWinner || l.MarketPublicID == "" || l.Outcome != OutcomeP1 {
		t.Fatalf("腿的展示欄位 = %+v", l)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900", got)
	}
	if db.sumEntries(bettor) != -100 {
		t.Fatalf("SUM(entries) = %d,want -100", db.sumEntries(bettor))
	}

	// 稽核鏈:注單指向分錄、分錄指回注單。
	b := db.betByPublicID("BET1")
	entries := db.entriesFor(bettor)
	if len(entries) != 1 || entries[0].Reason != ledger.ReasonBetStake {
		t.Fatalf("分錄 = %+v,want 一筆 bet_stake", entries)
	}
	if b.LedgerStakeEntryID != entries[0].ID {
		t.Fatalf("ledger_stake_entry_id = %d,want %d", b.LedgerStakeEntryID, entries[0].ID)
	}
	if entries[0].RefType != refTypeBet || entries[0].RefID != b.ID {
		t.Fatalf("分錄 ref = (%s,%d),want (bet,%d)", entries[0].RefType, entries[0].RefID, b.ID)
	}
}

// 四種盤口各押一腿(跨四場才行,所以這裡是同場四個盤口各下一張單)。
func TestPlaceBet四種盤口的賠率各自算(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()
	// 比分盤灌 10 票到 p1_2_0,其他三格零票。
	for u := int64(100); u < 110; u++ {
		if _, err := svc.Vote(ctx, vote(db, u, "M1", MarketScore, 0, "p1_2_0")); err != nil {
			t.Fatalf("灌票: %v", err)
		}
	}
	cases := []struct {
		name string
		leg  LegInput
		want int64 // odds_milli
		lbl  string
	}{
		{"整場", leg(db, "M1", MarketMatchWinner, 0, OutcomeP2), 1840, "A冷"},
		{"第二回合勝負", leg(db, "M1", MarketRoundWinner, 2, OutcomeP1), 1840, "李璃"},
		{"第一回合時長", leg(db, "M1", MarketDuration, 1, OutcomeUnder), 1840, "90 秒內"},
		{"比分熱門格", leg(db, "M1", MarketScore, 0, "p1_2_0"), 1840, "2:0"}, // 0.92 × 30 / 15
		{"比分冷門格", leg(db, "M1", MarketScore, 0, "p2_2_1"), 5520, "1:2"}, // 0.92 × 30 / 5
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustPlace(t, svc, PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: []LegInput{c.leg}, IdempotencyKey: "k" + strconv.Itoa(i),
			})
			l := res.Bet.Legs[0]
			if l.OddsMilli != c.want || l.OutcomeLabel != c.lbl {
				t.Fatalf("腿 = %+v,want odds %d label %q", l, c.want, c.lbl)
			}
		})
	}
}

// 連點的核心測試:同一把鍵送兩次,只扣一次款、只有一張注單。
func TestPlaceBet冪等重送不重複扣款(t *testing.T) {
	svc, db := setup(t)
	p := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "same-key",
	}

	first := mustPlace(t, svc, p)
	second := mustPlace(t, svc, p)

	if first.Replayed {
		t.Fatal("第一次不該是重放")
	}
	if !second.Replayed {
		t.Fatal("第二次必須標成重放")
	}
	if first.Bet.PublicID != second.Bet.PublicID {
		t.Fatalf("兩次回了不同注單:%s vs %s", first.Bet.PublicID, second.Bet.PublicID)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900(只該扣一次)", got)
	}
	if len(db.bets) != 1 {
		t.Fatalf("注單有 %d 張,want 1", len(db.bets))
	}
	if len(db.entries) != 1 {
		t.Fatalf("分錄有 %d 筆,want 1", len(db.entries))
	}
	if db.sumEntries(bettor) != -100 {
		t.Fatalf("SUM(entries) = %d,want -100", db.sumEntries(bettor))
	}
}

// 併發連點。fake 的 InTx 是互斥的,所以這測的是「兩個 tx 不交錯時冪等是否正確」,
// **不是**資料庫的鎖行為 —— 後者必須在 bettingpg 對真 Postgres 測。
func TestPlaceBet併發同一把鍵只扣一次(t *testing.T) {
	svc, db := setup(t)
	p := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "race",
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]*PlaceBetResult, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.PlaceBet(context.Background(), p)
		}()
	}
	wg.Wait()

	replayed := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("第 %d 次: %v", i, errs[i])
		}
		if results[i].Replayed {
			replayed++
		}
		if results[i].Bet.PublicID != results[0].Bet.PublicID {
			t.Fatalf("第 %d 次回了不同注單", i)
		}
	}
	if replayed != n-1 {
		t.Fatalf("重放了 %d 次,want %d", replayed, n-1)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900", got)
	}
	if len(db.bets) != 1 || len(db.entries) != 1 {
		t.Fatalf("注單 %d 張、分錄 %d 筆,want 各 1", len(db.bets), len(db.entries))
	}
}

func TestPlaceBet同鍵不同內容回衝突(t *testing.T) {
	svc, db := setup(t)
	p := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	}
	mustPlace(t, svc, p)

	p.Stake = 200 // 同一把鍵,不同金額
	_, err := svc.PlaceBet(context.Background(), p)
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("err = %v,want ErrIdempotencyConflict", err)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900", got)
	}
}

// 選手不得賭自己:第一條路徑,tournament_players.user_id 比對得到。
func TestPlaceBet選手不能賭自己_報名時已綁帳號(t *testing.T) {
	svc, db := setup(t)
	db.credit(11, 1_000)

	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: 11, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP2), IdempotencyKey: "k", // 賭自己輸
	})
	if !errors.Is(err, ErrSelfBet) {
		t.Fatalf("err = %v,want ErrSelfBet", err)
	}
	// 押時長盤也一樣擋:規則是「場次」不是「盤口」。
	_, err = svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: 11, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketDuration, 1, OutcomeOver)}, IdempotencyKey: "k2",
	})
	if !errors.Is(err, ErrSelfBet) {
		t.Fatalf("時長盤 err = %v,want ErrSelfBet", err)
	}
	if got := db.balance(11); got != 1_000 {
		t.Fatalf("餘額 = %d,want 1000", got)
	}
}

// 第二條路徑:純活動報名(tournament_players.user_id IS NULL),
// 只有 fencers.user_id 比對得到。漏掉這條就等於這個檢查形同虛設。
func TestPlaceBet選手不能賭自己_純活動報名但劍士綁過帳號(t *testing.T) {
	svc, db := setup(t)
	db.credit(13, 1_000)

	// 先確認前提:這位選手的 tournament_players.user_id 真的是空的。
	if db.matches["M2"].P1.PlayerUserID != 0 {
		t.Fatal("測試前提錯了:P1 不該有 tournament_players.user_id")
	}
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: 13, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M2", OutcomeP2), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrSelfBet) {
		t.Fatalf("err = %v,want ErrSelfBet(fencers.user_id 路徑)", err)
	}
}

// 已知缺口,不是 bug:兩邊都沒綁平台帳號時擋不住,只能靠裁判事後稽核。
// 這個測試存在的目的是讓「缺口被無意間補上或擴大」時有人會發現。
func TestPlaceBet兩邊都沒綁帳號時擋不住(t *testing.T) {
	svc, db := setup(t)
	db.credit(99, 1_000)

	res, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: 99, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M3", OutcomeP1), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("PlaceBet: %v", err)
	}
	if res.Bet.PublicID == "" {
		t.Fatal("應該成立注單 —— 這是雙軌身分的固有代價")
	}
}

// 同一注單同一場只能一腿:同一盤口押兩邊、或**不同盤口**(整場 + 比分)都不行。
func TestPlaceBet同注單同場只能一腿(t *testing.T) {
	cases := []struct {
		name string
		legs func(db *fakeDB) []LegInput
	}{
		{"同盤口押兩邊", func(db *fakeDB) []LegInput {
			return []LegInput{leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), leg(db, "M1", MarketMatchWinner, 0, OutcomeP2)}
		}},
		{"整場加比分", func(db *fakeDB) []LegInput {
			return []LegInput{leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), leg(db, "M1", MarketScore, 0, "p1_2_0")}
		}},
		{"兩個回合的時長", func(db *fakeDB) []LegInput {
			return []LegInput{leg(db, "M1", MarketDuration, 1, OutcomeOver), leg(db, "M1", MarketDuration, 2, OutcomeOver)}
		}},
		{"隔著別場再回來", func(db *fakeDB) []LegInput {
			return []LegInput{leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), leg(db, "M3", MarketMatchWinner, 0, OutcomeP1), leg(db, "M1", MarketRoundWinner, 1, OutcomeP1)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, db := setup(t)
			_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: c.legs(db), IdempotencyKey: "k",
			})
			if !errors.Is(err, ErrDuplicateMatchInParlay) {
				t.Fatalf("err = %v,want ErrDuplicateMatchInParlay", err)
			}
			if got := db.balance(bettor); got != 10_000 {
				t.Fatalf("餘額 = %d,want 10000", got)
			}
			// 應用層擋在建注單之前。DB 的 UNIQUE 是兜底,不是這條檢查的替身 ——
			// 少了應用層那一句,fake 的 InsertLegs 會回同一個錯、rollback 也會把注單抹掉,
			// 唯一看得出差別的是「有沒有呼叫過 InsertBet」(觀測值,不參與 rollback)。
			if db.insertBetCalls != 0 {
				t.Fatalf("重複押同場應在建注單前擋下,卻呼叫了 InsertBet %d 次", db.insertBetCalls)
			}
			// 客戶端有帶 match_public_id 時連資料庫都不碰(validate 就擋);沒帶時反推場次後擋。
			legs := c.legs(db)
			if legs[0].MatchPublicID != "" && (len(db.lockedUsers) != 0 || len(db.idem) != 0) {
				t.Fatalf("帶了 match_public_id 的重複應在碰 DB 前擋下(lockedUsers=%v)", db.lockedUsers)
			}
		})
	}
}

// 請求只帶 market_public_id(proto 的契約):場次由盤口反推,同場兩腿一樣擋。
func TestPlaceBet不帶場次也能下注與擋重複(t *testing.T) {
	svc, db := setup(t)
	strip := func(ls []LegInput) []LegInput {
		for i := range ls {
			ls[i].MatchPublicID = ""
		}
		return ls
	}
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs:           strip([]LegInput{leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), leg(db, "M3", MarketScore, 0, "p1_2_0")}),
		IdempotencyKey: "k",
	})
	if len(res.Bet.Legs) != 2 || res.Bet.Legs[0].MatchPublicID != "M1" || res.Bet.Legs[1].MatchPublicID != "M3" {
		t.Fatalf("腿的場次沒反推出來: %+v", res.Bet.Legs)
	}
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs:           strip([]LegInput{leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), leg(db, "M1", MarketScore, 0, "p1_2_0")}),
		IdempotencyKey: "k2",
	})
	if !errors.Is(err, ErrDuplicateMatchInParlay) {
		t.Fatalf("err = %v,want ErrDuplicateMatchInParlay", err)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900", got)
	}
	// 反推場次後的重複也是在建注單前擋(第一張合法的注單呼叫過一次)。
	if db.insertBetCalls != 1 {
		t.Fatalf("同場兩腿應在建注單前擋下,InsertBet 被呼叫 %d 次,want 1", db.insertBetCalls)
	}
	// 帶錯的 match_public_id(盤口是 M2 的)→ ErrMarketNotFound。
	bad := leg(db, "M2", MarketMatchWinner, 0, OutcomeP1)
	bad.MatchPublicID = "M1"
	if _, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100, Legs: []LegInput{bad}, IdempotencyKey: "k3",
	}); !errors.Is(err, ErrMarketNotFound) {
		t.Fatalf("err = %v,want ErrMarketNotFound", err)
	}
	// 投票同樣不需要場次。
	v := vote(db, 21, "M1", MarketDuration, 2, OutcomeOver)
	v.MatchPublicID = ""
	if odds, err := svc.Vote(context.Background(), v); err != nil || odds.MatchID != 1 || odds.MyVote != OutcomeOver {
		t.Fatalf("不帶場次投票 = (%+v,%v)", odds, err)
	}
}

// 應用層的檢查被繞過時,DB 的 UNIQUE (bet_id, match_id) 要兜底 —— fake 模擬那條約束。
func TestInsertLegs同場兩腿撞唯一約束(t *testing.T) {
	_, db := setup(t)
	mw, sc := db.market("M1", MarketMatchWinner, 0), db.market("M1", MarketScore, 0)
	err := db.InsertLegs(context.Background(), fakeTx{}, 1, []NewLeg{
		{MatchID: 1, MarketID: mw.ID, Outcome: OutcomeP1, OddsMilli: 1840},
		{MatchID: 1, MarketID: sc.ID, Outcome: "p1_2_0", OddsMilli: 3680},
	})
	if !errors.Is(err, ErrDuplicateMatchInParlay) {
		t.Fatalf("err = %v,want ErrDuplicateMatchInParlay", err)
	}
}

func TestPlaceBet封盤狀態(t *testing.T) {
	cases := []struct {
		status  MatchStatus
		wantErr error
	}{
		{MatchPending, ErrBettingClosed},
		{MatchReady, nil},
		{MatchLocked, nil}, // 讓武封盤了,下注還開著
		{MatchLive, ErrBettingClosed},
		{MatchDone, ErrBettingClosed},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			svc, db := setup(t)
			db.matches["M1"].Status = c.status
			_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
			})
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v,want nil", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v,want %v", err, c.wantErr)
			}
		})
	}
}

// 盤口關了就不能押,即使場次狀態(ready)還允許。
func TestPlaceBet關盤的盤口不能押(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()
	if _, err := svc.CloseMarketsInTx(ctx, fakeTx{}, "M1"); err != nil {
		t.Fatalf("關盤: %v", err)
	}
	_, err := svc.PlaceBet(ctx, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrMarketClosed) {
		t.Fatalf("err = %v,want ErrMarketClosed", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
	}
}

func TestPlaceBet結果不屬於盤口(t *testing.T) {
	svc, db := setup(t)
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketDuration, 1, OutcomeP1)}, IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrOutcomeInvalid) {
		t.Fatalf("err = %v,want ErrOutcomeInvalid", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
	}
}

func TestPlaceBet盤口不屬於場次(t *testing.T) {
	svc, db := setup(t)
	l := leg(db, "M2", MarketMatchWinner, 0, OutcomeP1)
	l.MatchPublicID = "M1"
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{l}, IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrMarketNotFound) {
		t.Fatalf("err = %v,want ErrMarketNotFound", err)
	}
}

func TestPlaceBet不開盤的賽事(t *testing.T) {
	svc, db := setup(t)
	off := fullRules()
	off.Config.Betting.Enabled = false
	db.rules[tourID] = off
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrBettingClosed) {
		t.Fatalf("err = %v,want ErrBettingClosed", err)
	}
}

func TestPlaceBet賠率跑掉就拒絕(t *testing.T) {
	svc, db := setup(t)
	// 客戶端以為是 5 倍(500),伺服器算出來是 1.84 倍(184)。
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k", ExpectedPayout: 500,
	})
	if !errors.Is(err, ErrOddsMoved) {
		t.Fatalf("err = %v,want ErrOddsMoved", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
	}

	// 算得準的話照樣成立。
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k2", ExpectedPayout: 184,
	})
	if res.Bet.PotentialPayout != 184 {
		t.Fatalf("potential_payout = %d,want 184", res.Bet.PotentialPayout)
	}
}

func TestPlaceBet超過單筆上限(t *testing.T) {
	svc, db := setup(t)
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: DefaultMaxStake + 1,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrStakeTooLarge) {
		t.Fatalf("err = %v,want ErrStakeTooLarge", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
	}
}

// rollback 的關鍵驗證:餘額不足時,注單、腿、分錄、餘額、冪等鍵全部不留痕跡。
func TestPlaceBet餘額不足時整筆消失(t *testing.T) {
	svc, db := setup(t)
	db.balances[bettor] = 50

	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("err = %v,want ErrInsufficientBalance", err)
	}
	if got := db.balance(bettor); got != 50 {
		t.Fatalf("餘額 = %d,want 50", got)
	}
	if len(db.entries) != 0 {
		t.Fatalf("留下了 %d 筆分錄,want 0", len(db.entries))
	}
	if len(db.bets) != 0 || len(db.legs) != 0 {
		t.Fatalf("留下了 %d 張注單 / %d 條腿,want 0", len(db.bets), len(db.legs))
	}
	// 冪等鍵也必須一起消失,否則合法的重試會被當成重放而永遠下不了注。
	if len(db.idem) != 0 {
		t.Fatalf("留下了 %d 把冪等鍵,want 0", len(db.idem))
	}
}

func TestPlaceBet串關連乘與封頂(t *testing.T) {
	svc, db := setup(t)
	// 把三場都推成一面倒,讓每腿都吃到 12 倍上限:12³ = 1728 倍 → 封 300 倍。
	// 60 票才夠把弱方推過上限:0.92 × 70 / 5 = 12.88 > 12。
	for _, m := range []string{"M1", "M2", "M3"} {
		mk := db.market(m, MarketMatchWinner, 0)
		for u := int64(100); u < 160; u++ {
			db.votes[voteKey{mk.ID, u}] = OutcomeP1
		}
	}
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			leg(db, "M1", MarketMatchWinner, 0, OutcomeP2),
			leg(db, "M2", MarketMatchWinner, 0, OutcomeP2),
			leg(db, "M3", MarketMatchWinner, 0, OutcomeP2),
		},
		IdempotencyKey: "k",
	})
	for _, l := range res.Bet.Legs {
		if l.OddsMilli != DefaultMaxOddsMilli {
			t.Fatalf("腿 %s 賠率 = %d,want %d", l.MatchPublicID, l.OddsMilli, DefaultMaxOddsMilli)
		}
	}
	if want := int64(100) * DefaultMaxParlayMilli / OddsScale; res.Bet.PotentialPayout != want {
		t.Fatalf("potential_payout = %d,want %d(封頂 300 倍)", res.Bet.PotentialPayout, want)
	}
}

func TestPlaceBet參數檢查(t *testing.T) {
	_, db0 := setup(t)
	base := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db0, "M1", OutcomeP1), IdempotencyKey: "k",
	}
	mut := func(f func(*PlaceBetParams)) PlaceBetParams {
		p := base
		p.Legs = slices.Clone(base.Legs)
		f(&p)
		return p
	}
	cases := []struct {
		name string
		p    PlaceBetParams
		want error
	}{
		{"沒有平台帳號", mut(func(p *PlaceBetParams) { p.UserID = 0 }), ErrInvalidRequest},
		{"沒帶冪等鍵", mut(func(p *PlaceBetParams) { p.IdempotencyKey = "" }), ErrInvalidRequest},
		{"本金非正", mut(func(p *PlaceBetParams) { p.Stake = 0 }), ErrInvalidRequest},
		{"一腿都沒有", mut(func(p *PlaceBetParams) { p.Legs = nil }), ErrInvalidRequest},
		{"缺 outcome", mut(func(p *PlaceBetParams) { p.Legs[0].Outcome = "" }), ErrInvalidRequest},
		{"缺 market", mut(func(p *PlaceBetParams) { p.Legs[0].MarketPublicID = "" }), ErrInvalidRequest},
		{"同一盤口兩次", mut(func(p *PlaceBetParams) { p.Legs = append(p.Legs, p.Legs[0]) }), ErrDuplicateMatchInParlay},
		// 不帶場次時,同一盤口兩次也要在碰 DB 前擋(盤口相同必然同場,不必反推)。
		{"同一盤口兩次_不帶場次", mut(func(p *PlaceBetParams) {
			p.Legs[0].MatchPublicID = ""
			p.Legs = append(p.Legs, p.Legs[0])
		}), ErrDuplicateMatchInParlay},
		{"腿數超過上限", mut(func(p *PlaceBetParams) {
			p.Legs = make([]LegInput, MaxLegsPerBet+1)
			for i := range p.Legs {
				p.Legs[i] = LegInput{MatchPublicID: "M" + strconv.Itoa(i), MarketPublicID: "X", Outcome: OutcomeP1}
			}
		}), ErrTooManyLegs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, db := setup(t)
			if _, err := svc.PlaceBet(context.Background(), c.p); !errors.Is(err, c.want) {
				t.Fatalf("err = %v,want %v", err, c.want)
			}
			if got := db.balance(bettor); got != 10_000 {
				t.Fatalf("餘額 = %d,want 10000", got)
			}
			// 這些都是不必碰資料庫就判得出的錯:validate 在 InTx 之前擋,連使用者列都不該鎖。
			if len(db.lockedUsers) != 0 {
				t.Fatalf("參數錯誤應在碰 DB 前擋下,卻鎖了使用者 %v", db.lockedUsers)
			}
		})
	}
}

// 查無盤口:投票與下注都要回 ErrMarketNotFound,而不是把零值的參照往下傳。
func TestPlaceBet與Vote查無盤口(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()
	_, err := svc.Vote(ctx, VoteParams{UserID: 21, MarketPublicID: "nope", Outcome: OutcomeP1})
	if !errors.Is(err, ErrMarketNotFound) {
		t.Fatalf("Vote err = %v,want ErrMarketNotFound", err)
	}
	_, err = svc.PlaceBet(ctx, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs:           []LegInput{leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), {MarketPublicID: "nope", Outcome: OutcomeP1}},
		IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrMarketNotFound) {
		t.Fatalf("PlaceBet err = %v,want ErrMarketNotFound", err)
	}
	if db.insertBetCalls != 0 || db.balance(bettor) != 10_000 {
		t.Fatalf("查無盤口不該走到建注單(calls=%d balance=%d)", db.insertBetCalls, db.balance(bettor))
	}
}

func TestPlaceBet場次不屬於這屆(t *testing.T) {
	svc, db := setup(t)
	db.addTournament(2, "other", fullRules())
	db.addMatch(9, "M9", 2, MatchReady, anonPlayer(901, "甲"), anonPlayer(902, "乙"))

	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M9", OutcomeP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrMatchNotInTournament) {
		t.Fatalf("err = %v,want ErrMatchNotInTournament", err)
	}
}

func TestListMyBets只列自己的(t *testing.T) {
	svc, db := setup(t)
	db.credit(8, 1_000)
	mustPlace(t, svc, PlaceBetParams{UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "a"})
	mustPlace(t, svc, PlaceBetParams{UserID: 8, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP2), IdempotencyKey: "b"})

	got, err := svc.ListMyBets(context.Background(), ListMyBetsParams{UserID: bettor, TournamentSlug: tourSlug})
	if err != nil {
		t.Fatalf("ListMyBets: %v", err)
	}
	if len(got) != 1 || got[0].UserID != bettor {
		t.Fatalf("列出了 %d 張注單 %+v,want 只有自己的", len(got), got)
	}
	if len(got[0].Legs) != 1 {
		t.Fatalf("腿數 = %d,want 1", len(got[0].Legs))
	}
}

// ── 整場結算 ──────────────────────────────────────────────────────

// playBestOf3 記一場三局兩勝的回合:seconds 是每回合時長,winners 是每回合勝者。
func playRounds(db *fakeDB, matchID string, winners []Outcome, seconds []int64) {
	for i, w := range winners {
		db.playRound(matchID, i+1, w, seconds[i])
	}
}

func TestSettleMatch全腿贏就派彩(t *testing.T) {
	svc, db := setup(t)
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	payout := res.Bet.PotentialPayout

	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M1", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}

	if len(out.Bets) != 1 || out.Bets[0].Status != BetWon || out.Bets[0].Amount != payout {
		t.Fatalf("結果 = %+v,want 一張 won 派彩 %d", out.Bets, payout)
	}
	if got := db.balance(bettor); got != 10_000-100+payout {
		t.Fatalf("餘額 = %d,want %d", got, 10_000-100+payout)
	}
	if db.sumEntries(bettor) != db.balance(bettor)-10_000 {
		t.Fatal("SUM(entries) 與餘額變動對不上")
	}
	// 派彩必須是新的正數分錄,而不是改掉原本的扣款。
	entries := db.entriesFor(bettor)
	if len(entries) != 2 {
		t.Fatalf("分錄 %d 筆,want 2(扣款 + 派彩)", len(entries))
	}
	if entries[0].Amount != -100 || entries[1].Amount != payout {
		t.Fatalf("分錄金額 = (%d,%d),want (-100,%d)", entries[0].Amount, entries[1].Amount, payout)
	}
	if entries[1].Reason != ledger.ReasonBetPayout {
		t.Fatalf("reason = %s,want bet_payout", entries[1].Reason)
	}
	b := db.betByPublicID("BET1")
	if b.SettledAt == nil {
		t.Fatal("settled_at 仍是 NULL,違反 bets_settled_at_check")
	}
	if b.LedgerPayoutEntryID != entries[1].ID {
		t.Fatalf("ledger_payout_entry_id = %d,want %d", b.LedgerPayoutEntryID, entries[1].ID)
	}
	if !slices.Equal(db.advisoryLocks, []int64{1}) {
		t.Fatalf("advisory lock = %v,want [1]", db.advisoryLocks)
	}
	// 整場 done 時所有盤口都離開 open/closed:2:0 → 第三回合的兩個盤口 void,其餘 settled。
	if out.MarketsSettled != 6 || out.MarketsVoided != 2 {
		t.Fatalf("盤口 settled=%d void=%d,want 6/2", out.MarketsSettled, out.MarketsVoided)
	}
}

func TestSettleMatch押錯邊就是輸而且不動錢(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP2), IdempotencyKey: "k",
	})

	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M1", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}
	if len(out.Bets) != 1 || out.Bets[0].Status != BetLost || out.Bets[0].Amount != 0 {
		t.Fatalf("結果 = %+v,want 一張 lost 且不動錢", out.Bets)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900", got)
	}
	if len(db.entries) != 1 {
		t.Fatalf("分錄 %d 筆,want 1(輸掉不產生新分錄)", len(db.entries))
	}
}

// 比分盤:2:0 與 2:1、兩邊各驗一次。押中的一格贏,其他三格輸。
func TestSettleMatch比分盤(t *testing.T) {
	cases := []struct {
		name    string
		winners []Outcome
		winner  Outcome
		hit     Outcome
	}{
		{"p1 兩比零", []Outcome{OutcomeP1, OutcomeP1}, OutcomeP1, "p1_2_0"},
		{"p1 兩比一", []Outcome{OutcomeP1, OutcomeP2, OutcomeP1}, OutcomeP1, "p1_2_1"},
		{"p2 兩比一", []Outcome{OutcomeP2, OutcomeP1, OutcomeP2}, OutcomeP2, "p2_2_1"},
		{"p2 兩比零", []Outcome{OutcomeP2, OutcomeP2}, OutcomeP2, "p2_2_0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, db := setup(t)
			all := []Outcome{"p1_2_0", "p1_2_1", "p2_2_1", "p2_2_0"}
			for i, o := range all {
				mustPlace(t, svc, PlaceBetParams{
					UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
					Legs: []LegInput{leg(db, "M1", MarketScore, 0, o)}, IdempotencyKey: "k" + strconv.Itoa(i),
				})
			}
			playRounds(db, "M1", c.winners, []int64{100, 100, 100}[:len(c.winners)])
			db.finish("M1", c.winner, ResultNormal)
			out, err := svc.SettleMatch(context.Background(), "M1")
			if err != nil {
				t.Fatalf("SettleMatch: %v", err)
			}
			won, lost := 0, 0
			for _, oc := range out.Bets {
				switch oc.Status {
				case BetWon:
					won++
					if oc.Amount != 368 { // 100 × 3.68
						t.Fatalf("派彩 = %d,want 368", oc.Amount)
					}
				case BetLost:
					lost++
				default:
					t.Fatalf("注單 %s 狀態 = %s", oc.BetPublicID, oc.Status)
				}
			}
			if won != 1 || lost != 3 {
				t.Fatalf("won=%d lost=%d,want 1/3", won, lost)
			}
			for i, o := range all {
				b := db.betByPublicID("BET" + strconv.Itoa(i+1))
				want := BetLost
				if o == c.hit {
					want = BetWon
				}
				if b.Status != want {
					t.Fatalf("押 %s 的注單狀態 = %s,want %s", o, b.Status, want)
				}
			}
			if got := db.balance(bettor); got != 10_000-400+368 {
				t.Fatalf("餘額 = %d,want %d", got, 10_000-400+368)
			}
		})
	}
}

// 三局兩勝 2:0 結束:第三回合的兩個盤口沒有結果 → void,押它們的單場注單全額退款(不是 lost)。
func TestSettleMatch沒打到的回合作廢退款(t *testing.T) {
	svc, db := setup(t)
	r3w := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketRoundWinner, 3, OutcomeP2)}, IdempotencyKey: "a",
	})
	r3d := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 70,
		Legs: []LegInput{leg(db, "M1", MarketDuration, 3, OutcomeOver)}, IdempotencyKey: "b",
	})
	// 對照:第二回合的盤口有結果,照結算。
	r2d := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 50,
		Legs: []LegInput{leg(db, "M1", MarketDuration, 2, OutcomeUnder)}, IdempotencyKey: "c",
	})

	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{120, 60})
	db.finish("M1", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}
	if out.MarketsVoided != 2 || out.MarketsSettled != 6 || out.LegsVoid != 2 {
		t.Fatalf("盤口 void=%d settled=%d 腿 void=%d,want 2/6/2", out.MarketsVoided, out.MarketsSettled, out.LegsVoid)
	}
	for _, r := range []*PlaceBetResult{r3w, r3d} {
		b := db.betByPublicID(r.Bet.PublicID)
		if b.Status != BetVoid {
			t.Fatalf("押第三回合的注單 %s 狀態 = %s,want void(不是 lost)", b.PublicID, b.Status)
		}
		if b.LedgerRefundEntryID == 0 {
			t.Fatalf("注單 %s 沒有退款分錄", b.PublicID)
		}
		if b.PayoutRecalculated {
			t.Fatal("單場全退沒有重算金額,payout_recalculated 不該是 true")
		}
	}
	if b := db.betByPublicID(r2d.Bet.PublicID); b.Status != BetWon {
		t.Fatalf("押第二回合 60 秒內的注單狀態 = %s,want won", b.Status)
	}
	for _, mk := range db.markets {
		if mk.MatchID != 1 {
			continue
		}
		want := MarketSettled
		if mk.RoundNo == 3 {
			want = MarketVoid
		}
		if mk.Status != want {
			t.Fatalf("盤口 %s round=%d 狀態 = %s,want %s", mk.Kind, mk.RoundNo, mk.Status, want)
		}
	}
	// 退了 100 + 70,派了 50 × 1.84 = 92。
	if got := db.balance(bettor); got != 10_000-220+170+92 {
		t.Fatalf("餘額 = %d,want %d", got, 10_000-220+170+92)
	}
	if db.sumEntries(bettor) != db.balance(bettor)-10_000 {
		t.Fatal("SUM(entries) 與餘額變動對不上")
	}
	refunds := 0
	for _, e := range db.entriesFor(bettor) {
		if e.Reason == ledger.ReasonBetRefund {
			refunds++
		}
	}
	if refunds != 2 {
		t.Fatalf("退款分錄 %d 筆,want 2", refunds)
	}
}

// 串關裡一腿押到沒打的回合:那一腿 void、移除後依剩餘腿重算。
func TestSettleMatch串關含沒打到的回合就重算(t *testing.T) {
	svc, db := setup(t)
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			leg(db, "M1", MarketRoundWinner, 3, OutcomeP1), // 會 void
			leg(db, "M3", MarketMatchWinner, 0, OutcomeP1), // 之後贏
		},
		IdempotencyKey: "k",
	})
	if res.Bet.PotentialPayout != 338 {
		t.Fatalf("原始 potential_payout = %d,want 338", res.Bet.PotentialPayout)
	}
	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M1", OutcomeP1, ResultNormal)
	if _, err := svc.SettleMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("結算 M1: %v", err)
	}
	b := db.betByPublicID("BET1")
	if b.Status != BetOpen || !b.PayoutRecalculated || b.PotentialPayout != 184 {
		t.Fatalf("注單 = %+v,want open、已重算、184", b)
	}

	playRounds(db, "M3", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M3", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M3")
	if err != nil {
		t.Fatalf("結算 M3: %v", err)
	}
	if len(out.Bets) != 1 || out.Bets[0].Status != BetWon || out.Bets[0].Amount != 184 {
		t.Fatalf("結果 = %+v,want won 派 184(重算後的金額)", out.Bets)
	}
}

// 重複結算同一場不可以再派一次彩。狀態檢查是這件事的權威,advisory lock 只擋併發。
func TestSettleMatch重複呼叫不重複派彩(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M1", OutcomeP1, ResultNormal)

	if _, err := svc.SettleMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	after := db.balance(bettor)
	entries := len(db.entries)

	second, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if len(second.Bets) != 0 || second.MarketsSettled != 0 || second.MarketsVoided != 0 {
		t.Fatalf("第二次動了東西 %+v,want 什麼都不做", second)
	}
	if db.balance(bettor) != after {
		t.Fatalf("餘額變成 %d,want %d", db.balance(bettor), after)
	}
	if len(db.entries) != entries {
		t.Fatalf("分錄變成 %d 筆,want %d", len(db.entries), entries)
	}
}

// 串關:一腿判完之後注單仍是 open,兩腿都判完才派彩。
func TestSettleMatch串關要兩場都贏(t *testing.T) {
	svc, db := setup(t)
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			leg(db, "M1", MarketMatchWinner, 0, OutcomeP1),
			leg(db, "M3", MarketMatchWinner, 0, OutcomeP1),
		},
		IdempotencyKey: "k",
	})
	payout := res.Bet.PotentialPayout // 100 × 1.84 × 1.84 = 338(逐步捨去)
	if payout != 338 {
		t.Fatalf("potential_payout = %d,want 338", payout)
	}

	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M1", OutcomeP1, ResultNormal)
	first, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("結算 M1: %v", err)
	}
	if len(first.Bets) != 0 {
		t.Fatalf("第一場就動了注單 %+v,want 維持 open", first.Bets)
	}
	if db.balance(bettor) != 9_900 {
		t.Fatalf("餘額 = %d,want 9900", db.balance(bettor))
	}

	playRounds(db, "M3", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M3", OutcomeP1, ResultNormal)
	second, err := svc.SettleMatch(context.Background(), "M3")
	if err != nil {
		t.Fatalf("結算 M3: %v", err)
	}
	if len(second.Bets) != 1 || second.Bets[0].Status != BetWon || second.Bets[0].Amount != payout {
		t.Fatalf("結果 = %+v,want won 派彩 %d", second.Bets, payout)
	}
	if want := 10_000 - 100 + payout; db.balance(bettor) != want {
		t.Fatalf("餘額 = %d,want %d", db.balance(bettor), want)
	}
	// 沒有腿作廢,金額不該被標成重算過。
	if db.betByPublicID("BET1").PayoutRecalculated {
		t.Fatal("沒有腿作廢卻標了 payout_recalculated")
	}
}

// 注單列鎖必須依 id 升冪取 —— 這是全系統的防死鎖慣例。
// 一次結算要動多個盤口,但取鎖只有一次、涵蓋全部盤口的注單。
func TestSettleMatch鎖注單依id升冪且只鎖一次(t *testing.T) {
	svc, db := setup(t)
	legs := [][]LegInput{
		winner(db, "M1", OutcomeP1),
		{leg(db, "M1", MarketScore, 0, "p2_2_0")},
		{leg(db, "M1", MarketDuration, 1, OutcomeOver)},
	}
	for i, ls := range legs {
		mustPlace(t, svc, PlaceBetParams{
			UserID: bettor, TournamentSlug: tourSlug, Stake: 10,
			Legs: ls, IdempotencyKey: "k" + strconv.Itoa(i),
		})
	}
	playRounds(db, "M1", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M1", OutcomeP1, ResultNormal)
	if _, err := svc.SettleMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}

	if len(db.lockedBets) != 1 {
		t.Fatalf("取鎖 %d 次,want 1", len(db.lockedBets))
	}
	got := db.lockedBets[0]
	if !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("取鎖 = %v,want [1 2 3]", got)
	}
}

func TestSettleMatch拒絕棄賽場次(t *testing.T) {
	svc, db := setup(t)
	db.finish("M1", OutcomeP1, ResultWalkover)
	_, err := svc.SettleMatch(context.Background(), "M1")
	if !errors.Is(err, ErrWalkoverMatch) {
		t.Fatalf("err = %v,want ErrWalkoverMatch", err)
	}
}

func TestSettleMatch尚未分出勝負(t *testing.T) {
	svc, _ := setup(t)
	_, err := svc.SettleMatch(context.Background(), "M1")
	if !errors.Is(err, ErrMatchNotDecided) {
		t.Fatalf("err = %v,want ErrMatchNotDecided", err)
	}
}

// 沒有任何回合紀錄卻已 done(資料異常):比分盤作廢退款,不猜、也不擋住整場結算。
func TestSettleMatch比分算不出來就作廢(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketScore, 0, "p1_2_0")}, IdempotencyKey: "k",
	})
	db.finish("M1", OutcomeP1, ResultNormal) // 沒有 playRounds
	out, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}
	// match_winner settled;score + 三回合 ×2 = 7 個 void。
	if out.MarketsSettled != 1 || out.MarketsVoided != 7 {
		t.Fatalf("settled=%d void=%d,want 1/7", out.MarketsSettled, out.MarketsVoided)
	}
	if b := db.betByPublicID("BET1"); b.Status != BetVoid {
		t.Fatalf("比分注單狀態 = %s,want void", b.Status)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000(全退)", got)
	}
}

// ── 逐回合結算 ────────────────────────────────────────────────────

func TestSettleRound回合勝負與時長(t *testing.T) {
	svc, db := setup(t)
	bets := map[string]*PlaceBetResult{}
	for key, l := range map[string]LegInput{
		"r1w-p1":    leg(db, "M1", MarketRoundWinner, 1, OutcomeP1),
		"r1w-p2":    leg(db, "M1", MarketRoundWinner, 1, OutcomeP2),
		"r1d-over":  leg(db, "M1", MarketDuration, 1, OutcomeOver),
		"r1d-under": leg(db, "M1", MarketDuration, 1, OutcomeUnder),
		"r2w-p1":    leg(db, "M1", MarketRoundWinner, 2, OutcomeP1), // 不該被動到
		"mw-p1":     leg(db, "M1", MarketMatchWinner, 0, OutcomeP1), // 不該被動到
	} {
		bets[key] = mustPlace(t, svc, PlaceBetParams{
			UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
			Legs: []LegInput{l}, IdempotencyKey: key,
		})
	}
	db.matches["M1"].Status = MatchLive
	if _, err := svc.CloseMarketsInTx(context.Background(), fakeTx{}, "M1"); err != nil {
		t.Fatalf("關盤: %v", err)
	}
	db.playRound("M1", 1, OutcomeP2, 130) // p2 贏、130 秒 > 90 → over

	out, err := svc.SettleRound(context.Background(), "M1", 1)
	if err != nil {
		t.Fatalf("SettleRound: %v", err)
	}
	if out.MarketsSettled != 2 || out.MarketsVoided != 0 || out.LegsWon != 2 || out.LegsLost != 2 {
		t.Fatalf("結果 = %+v,want 2 盤口 settled、2 腿贏 2 腿輸", out)
	}
	want := map[string]BetStatus{
		"r1w-p1": BetLost, "r1w-p2": BetWon, "r1d-over": BetWon, "r1d-under": BetLost,
		"r2w-p1": BetOpen, "mw-p1": BetOpen,
	}
	for key, st := range want {
		if b := db.betByPublicID(bets[key].Bet.PublicID); b.Status != st {
			t.Fatalf("%s 狀態 = %s,want %s", key, b.Status, st)
		}
	}
	if got := db.balance(bettor); got != 10_000-600+184*2 {
		t.Fatalf("餘額 = %d,want %d", got, 10_000-600+184*2)
	}
	if mk := db.market("M1", MarketRoundWinner, 2); mk.Status != MarketClosed {
		t.Fatalf("第二回合盤口狀態 = %s,want 仍 closed", mk.Status)
	}

	// 重跑同一回合:兩個盤口已結算,什麼都不做。
	again, err := svc.SettleRound(context.Background(), "M1", 1)
	if err != nil || len(again.Bets) != 0 || again.MarketsSettled != 0 {
		t.Fatalf("重跑 = (%+v,%v),want 空", again, err)
	}
}

// duration 的邊界:等於 line 算 over(schemas/21 待確認 ①);差一奈秒就是 under。
func TestSettleRound時長等於線算over(t *testing.T) {
	cases := []struct {
		name    string
		seconds int64
		want    BetStatus // 押 over 的注單
	}{
		{"等於 90 秒", 90, BetWon},
		{"89 秒", 89, BetLost},
		{"91 秒", 91, BetWon},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, db := setup(t)
			over := mustPlace(t, svc, PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: []LegInput{leg(db, "M1", MarketDuration, 1, OutcomeOver)}, IdempotencyKey: "o",
			})
			under := mustPlace(t, svc, PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: []LegInput{leg(db, "M1", MarketDuration, 1, OutcomeUnder)}, IdempotencyKey: "u",
			})
			db.playRound("M1", 1, OutcomeP1, c.seconds)
			if _, err := svc.SettleRound(context.Background(), "M1", 1); err != nil {
				t.Fatalf("SettleRound: %v", err)
			}
			if b := db.betByPublicID(over.Bet.PublicID); b.Status != c.want {
				t.Fatalf("押 over 狀態 = %s,want %s", b.Status, c.want)
			}
			wantUnder := BetLost
			if c.want == BetLost {
				wantUnder = BetWon
			}
			if b := db.betByPublicID(under.Bet.PublicID); b.Status != wantUnder {
				t.Fatalf("押 under 狀態 = %s,want %s", b.Status, wantUnder)
			}
		})
	}

	// 真正的邊界:89.999… 秒是 under,只有 ≥ 90.000 才是 over。直接比 Duration,不截成整數秒。
	t.Run("差一奈秒", func(t *testing.T) {
		svc, db := setup(t)
		over := mustPlace(t, svc, PlaceBetParams{
			UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
			Legs: []LegInput{leg(db, "M1", MarketDuration, 1, OutcomeOver)}, IdempotencyKey: "o",
		})
		m := db.matches["M1"]
		start := db.now
		end := start.Add(90e9 - 1) // 89.999999999 秒
		db.rounds[m.ID] = []RoundResult{{RoundNo: 1, StartedAt: start, FinishedAt: &end, WinnerPlayerID: m.P1.PlayerID}}
		if _, err := svc.SettleRound(context.Background(), "M1", 1); err != nil {
			t.Fatalf("SettleRound: %v", err)
		}
		if b := db.betByPublicID(over.Bet.PublicID); b.Status != BetLost {
			t.Fatalf("89.999999999 秒押 over 狀態 = %s,want lost", b.Status)
		}
	})
}

func TestSettleRound回合未結束(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketRoundWinner, 1, OutcomeP1)}, IdempotencyKey: "k",
	})
	// 沒有回合列。
	if _, err := svc.SettleRound(context.Background(), "M1", 1); !errors.Is(err, ErrRoundNotFinished) {
		t.Fatalf("沒有回合 err = %v,want ErrRoundNotFinished", err)
	}
	// 有列但還沒結束。
	m := db.matches["M1"]
	db.rounds[m.ID] = []RoundResult{{RoundNo: 1, StartedAt: db.now}}
	if _, err := svc.SettleRound(context.Background(), "M1", 1); !errors.Is(err, ErrRoundNotFinished) {
		t.Fatalf("進行中 err = %v,want ErrRoundNotFinished", err)
	}
	if _, err := svc.SettleRound(context.Background(), "M1", 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("round=0 err = %v,want ErrInvalidRequest", err)
	}
	if got := db.balance(bettor); got != 9_900 {
		t.Fatalf("餘額 = %d,want 9900(什麼都不該動)", got)
	}
}

func TestSettleRound拒絕棄賽場次(t *testing.T) {
	svc, db := setup(t)
	db.finish("M1", OutcomeP1, ResultWalkover)
	if _, err := svc.SettleRound(context.Background(), "M1", 1); !errors.Is(err, ErrWalkoverMatch) {
		t.Fatalf("err = %v,want ErrWalkoverMatch", err)
	}
}

// 逐回合結算過的盤口,整場結算時不再碰;沒逐回合結算的,整場結算補結。
func TestSettleMatch補結沒逐回合結算的盤口(t *testing.T) {
	svc, db := setup(t)
	r1 := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketRoundWinner, 1, OutcomeP1)}, IdempotencyKey: "a",
	})
	r2 := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{leg(db, "M1", MarketRoundWinner, 2, OutcomeP2)}, IdempotencyKey: "b",
	})
	db.playRound("M1", 1, OutcomeP1, 100)
	if _, err := svc.SettleRound(context.Background(), "M1", 1); err != nil {
		t.Fatalf("結算第一回合: %v", err)
	}
	afterR1 := db.balance(bettor)
	db.playRound("M1", 2, OutcomeP1, 100) // 第二回合沒呼叫 SettleRound
	db.finish("M1", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}
	// 第一回合的兩個盤口已 settled,這次動的是:match_winner、score、r2 ×2 settled;r3 ×2 void。
	if out.MarketsSettled != 4 || out.MarketsVoided != 2 {
		t.Fatalf("settled=%d void=%d,want 4/2", out.MarketsSettled, out.MarketsVoided)
	}
	if b := db.betByPublicID(r1.Bet.PublicID); b.Status != BetWon {
		t.Fatalf("第一回合注單狀態 = %s,want won", b.Status)
	}
	if b := db.betByPublicID(r2.Bet.PublicID); b.Status != BetLost {
		t.Fatalf("第二回合注單狀態 = %s,want lost", b.Status)
	}
	if db.balance(bettor) != afterR1 {
		t.Fatalf("餘額 = %d,want %d(第一回合已派過,不重派)", db.balance(bettor), afterR1)
	}
}

// ── 棄賽退款 ──────────────────────────────────────────────────────

func TestVoidMatch單場全額退(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})

	db.finish("M1", OutcomeP1, ResultWalkover)
	out, err := svc.VoidMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("VoidMatch: %v", err)
	}
	if len(out.Bets) != 1 || out.Bets[0].Status != BetVoid || out.Bets[0].Amount != 100 {
		t.Fatalf("結果 = %+v,want 一張 void 退 100", out.Bets)
	}
	if out.MarketsVoided != 8 || out.MarketsSettled != 0 || out.LegsVoid != 1 {
		t.Fatalf("盤口 void=%d settled=%d,want 全部 8 個 void", out.MarketsVoided, out.MarketsSettled)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000(全額退回)", got)
	}

	// 退款是新的正數分錄,舊的扣款分錄原封不動。
	entries := db.entriesFor(bettor)
	if len(entries) != 2 || entries[0].Amount != -100 || entries[1].Amount != 100 {
		t.Fatalf("分錄 = %+v,want (-100,+100)", entries)
	}
	if entries[1].Reason != ledger.ReasonBetRefund {
		t.Fatalf("reason = %s,want bet_refund", entries[1].Reason)
	}
	b := db.betByPublicID("BET1")
	if b.LedgerRefundEntryID != entries[1].ID {
		t.Fatalf("ledger_refund_entry_id = %d,want %d", b.LedgerRefundEntryID, entries[1].ID)
	}
	if b.PayoutRecalculated {
		t.Fatal("單場全退沒有重算金額,payout_recalculated 不該是 true")
	}
	for _, mk := range db.markets {
		if mk.MatchID == 1 && mk.Status != MarketVoid {
			t.Fatalf("盤口 %+v 沒作廢", mk)
		}
	}
}

// 串關的三種分支。每一種都從同一張兩腿注單出發,只差在另一腿的狀態。
func TestVoidMatch串關重算的三種分支(t *testing.T) {
	cases := []struct {
		name       string
		otherLeg   LegResult
		wantStatus BetStatus
		wantPaid   int64 // 本次退款/派彩金額
	}{
		{"剩餘腿還有待判定_維持open並重算", LegPending, BetOpen, 0},
		{"剩餘腿全贏_依重算金額派彩", LegWon, BetWon, 184},
		{"剩餘腿已輸_棄賽不救", LegLost, BetLost, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, db := setup(t)
			res := mustPlace(t, svc, PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: []LegInput{
					leg(db, "M1", MarketMatchWinner, 0, OutcomeP1),
					leg(db, "M3", MarketMatchWinner, 0, OutcomeP1),
				},
				IdempotencyKey: "k",
			})
			if res.Bet.PotentialPayout != 338 {
				t.Fatalf("原始 potential_payout = %d,want 338", res.Bet.PotentialPayout)
			}

			// 先把 M3 判掉(如果這個分支需要)。
			switch c.otherLeg {
			case LegWon:
				playRounds(db, "M3", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
				db.finish("M3", OutcomeP1, ResultNormal)
				if _, err := svc.SettleMatch(context.Background(), "M3"); err != nil {
					t.Fatalf("結算 M3: %v", err)
				}
			case LegLost:
				playRounds(db, "M3", []Outcome{OutcomeP2, OutcomeP2}, []int64{100, 100})
				db.finish("M3", OutcomeP2, ResultNormal)
				if _, err := svc.SettleMatch(context.Background(), "M3"); err != nil {
					t.Fatalf("結算 M3: %v", err)
				}
			case LegPending, LegVoid:
			}

			before := db.balance(bettor)
			db.finish("M1", OutcomeP1, ResultWalkover)
			out, err := svc.VoidMatch(context.Background(), "M1")
			if err != nil {
				t.Fatalf("VoidMatch: %v", err)
			}

			// M3 先輸掉的話,那張注單在結算 M3 時就已經定案為 lost,
			// 之後的 VoidMatch 不該再碰它(status 已經不是 open)。
			if c.otherLeg == LegLost {
				if len(out.Bets) != 0 {
					t.Fatalf("動了 %d 張注單,want 0(已經定案)", len(out.Bets))
				}
				if db.betByPublicID("BET1").Status != BetLost {
					t.Fatalf("注單狀態 = %s,want lost", db.betByPublicID("BET1").Status)
				}
				return
			}

			if len(out.Bets) != 1 {
				t.Fatalf("動了 %d 張注單,want 1", len(out.Bets))
			}
			oc := out.Bets[0]
			if oc.Status != c.wantStatus || oc.Amount != c.wantPaid || !oc.PayoutRecalculated {
				t.Fatalf("結果 = %+v,want status %s 動錢 %d 已重算", oc, c.wantStatus, c.wantPaid)
			}

			// 重算後的賠付只剩 M3 那一腿的賠率:100 × 1.84 = 184。
			b := db.betByPublicID("BET1")
			if b.PotentialPayout != 184 || !b.PayoutRecalculated {
				t.Fatalf("重算後 = (%d,%v),want (184,true)", b.PotentialPayout, b.PayoutRecalculated)
			}
			if want := before + c.wantPaid; db.balance(bettor) != want {
				t.Fatalf("餘額 = %d,want %d", db.balance(bettor), want)
			}
			if c.wantStatus == BetOpen && b.SettledAt != nil {
				t.Fatal("仍是 open 卻寫了 settled_at,違反 bets_settled_at_check")
			}
		})
	}
}

// 剩餘腿還在 pending 時重算過的金額,必須真的被後續結算採用 ——
// 否則「重算」只是寫進資料庫給人看的,實際派彩還是舊數字。
func TestVoidMatch重算後的金額會被後續派彩採用(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			leg(db, "M1", MarketMatchWinner, 0, OutcomeP1),
			leg(db, "M3", MarketMatchWinner, 0, OutcomeP1),
		},
		IdempotencyKey: "k",
	})

	db.finish("M1", OutcomeP1, ResultWalkover)
	if _, err := svc.VoidMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("VoidMatch: %v", err)
	}

	playRounds(db, "M3", []Outcome{OutcomeP1, OutcomeP1}, []int64{100, 100})
	db.finish("M3", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M3")
	if err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}
	if len(out.Bets) != 1 || out.Bets[0].Status != BetWon {
		t.Fatalf("結果 = %+v,want won", out.Bets)
	}
	if out.Bets[0].Amount != 184 {
		t.Fatalf("派彩 = %d,want 184(重算後的金額,不是原本的 338)", out.Bets[0].Amount)
	}
	if want := int64(10_000 - 100 + 184); db.balance(bettor) != want {
		t.Fatalf("餘額 = %d,want %d", db.balance(bettor), want)
	}
}

// 兩腿都棄賽 → 全額退,而不是用「剩下零腿」的乘積派彩。
func TestVoidMatch所有腿都作廢就全額退(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			leg(db, "M1", MarketMatchWinner, 0, OutcomeP1),
			leg(db, "M3", MarketMatchWinner, 0, OutcomeP1),
		},
		IdempotencyKey: "k",
	})

	db.finish("M1", OutcomeP1, ResultWalkover)
	if _, err := svc.VoidMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("VoidMatch M1: %v", err)
	}
	db.finish("M3", OutcomeP1, ResultWalkover)
	out, err := svc.VoidMatch(context.Background(), "M3")
	if err != nil {
		t.Fatalf("VoidMatch M3: %v", err)
	}

	if len(out.Bets) != 1 || out.Bets[0].Status != BetVoid || out.Bets[0].Amount != 100 {
		t.Fatalf("結果 = %+v,want void 全額退 100", out.Bets)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
	}
	if db.sumEntries(bettor) != 0 {
		t.Fatalf("SUM(entries) = %d,want 0(扣了又全退)", db.sumEntries(bettor))
	}
}

func TestVoidMatch重複呼叫不重複退款(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: winner(db, "M1", OutcomeP1), IdempotencyKey: "k",
	})
	db.finish("M1", OutcomeP1, ResultWalkover)

	if _, err := svc.VoidMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if _, err := svc.VoidMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000(只該退一次)", got)
	}
	if len(db.entries) != 2 {
		t.Fatalf("分錄 %d 筆,want 2", len(db.entries))
	}
}

func TestVoidMatch拒絕非棄賽場次(t *testing.T) {
	svc, db := setup(t)
	db.finish("M1", OutcomeP1, ResultNormal)
	_, err := svc.VoidMatch(context.Background(), "M1")
	if !errors.Is(err, ErrNotWalkover) {
		t.Fatalf("err = %v,want ErrNotWalkover", err)
	}
}

// ── 雜項 ──────────────────────────────────────────────────────────

// 冪等的判準不能含賠率:賠率隨票數浮動,含進去的話每次重試都變成「不同請求」。
func TestRequestHash不含賠率但含關鍵欄位(t *testing.T) {
	one := func(mk string, o Outcome) []LegInput {
		return []LegInput{{MarketPublicID: mk, Outcome: o}}
	}
	base := PlaceBetParams{UserID: 7, TournamentSlug: "a", Stake: 100,
		Legs: one("K1", OutcomeP1), ExpectedPayout: 184}
	same := base
	same.ExpectedPayout = 999 // 客戶端當時看到的賠率不同,但下的是同一注
	if placeRequestHash(base) != placeRequestHash(same) {
		t.Fatal("expected_payout 不該影響請求雜湊")
	}
	// match_public_id 只是可選的一致性檢查,盤口已經決定了場次 —— 帶不帶都是同一注。
	withMatch := base
	withMatch.Legs = []LegInput{{MatchPublicID: "M1", MarketPublicID: "K1", Outcome: OutcomeP1}}
	if placeRequestHash(base) != placeRequestHash(withMatch) {
		t.Fatal("match_public_id 不該影響請求雜湊")
	}

	for _, mod := range []func(*PlaceBetParams){
		func(p *PlaceBetParams) { p.UserID = 8 },
		func(p *PlaceBetParams) { p.TournamentSlug = "b" },
		func(p *PlaceBetParams) { p.Stake = 101 },
		func(p *PlaceBetParams) { p.Legs = one("K2", OutcomeP1) },
		func(p *PlaceBetParams) { p.Legs = one("K1", OutcomeP2) },
	} {
		p := base
		p.Legs = slices.Clone(base.Legs)
		mod(&p)
		if placeRequestHash(base) == placeRequestHash(p) {
			t.Fatalf("改了關鍵欄位卻是同一個雜湊: %+v", p)
		}
	}
}

func TestMatchStatus下注與投票窗口(t *testing.T) {
	for _, c := range []struct {
		s    MatchStatus
		open bool
	}{
		{MatchPending, false}, {MatchReady, true}, {MatchLocked, true},
		{MatchLive, false}, {MatchDone, false},
	} {
		if got := c.s.OpenForBets(); got != c.open {
			t.Fatalf("%s.OpenForBets() = %v,want %v", c.s, got, c.open)
		}
		if got := c.s.OpenForVotes(); got != c.open {
			t.Fatalf("%s.OpenForVotes() = %v,want %v", c.s, got, c.open)
		}
	}
}

// 0 不是合法的 user_id。沒綁帳號的選手不能讓「所有未登入的人」都變成他。
func TestParticipantIsUser(t *testing.T) {
	p := unboundPlayer(1, 13, "丙")
	if !p.IsUser(13) {
		t.Fatal("fencers.user_id 應該比對得到")
	}
	if p.IsUser(0) {
		t.Fatal("user_id 0 永遠不該比對成功")
	}
	if anonPlayer(2, "丁").IsUser(0) {
		t.Fatal("兩邊都沒綁時,0 仍然不該比對成功")
	}
}

// 預設規則(v1 語意)只有勝負盤:舊版的 config 進來也要能開盤、下注、結算。
func TestDefaultRules只有勝負盤(t *testing.T) {
	svc, db := setup(t)
	db.addTournament(2, "legacy", Rules{Config: rules.Default(), MaxStake: DefaultMaxStake})
	db.addMatch(9, "M9", 2, MatchReady, anonPlayer(901, "甲"), anonPlayer(902, "乙"))
	got, err := svc.GetOdds(context.Background(), GetOddsParams{MatchPublicIDs: []string{"M9"}})
	if err != nil || len(got) != 1 || len(got[0].Markets) != 1 || got[0].Markets[0].Kind != MarketMatchWinner {
		t.Fatalf("= (%+v,%v),want 一個 match_winner 盤口", got, err)
	}
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: "legacy", Stake: 100,
		Legs: winner(db, "M9", OutcomeP1), IdempotencyKey: "k",
	})
	db.playRound("M9", 1, OutcomeP1, 100)
	db.finish("M9", OutcomeP1, ResultNormal)
	out, err := svc.SettleMatch(context.Background(), "M9")
	if err != nil || len(out.Bets) != 1 || out.Bets[0].Status != BetWon || out.Bets[0].Amount != res.Bet.PotentialPayout {
		t.Fatalf("= (%+v,%v),want 派彩 %d", out, err, res.Bet.PotentialPayout)
	}
}
