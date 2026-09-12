package betting

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// 測試場景:一屆賽事「試鋒」,三場比賽。
//
//	M1  李璃(user 11)vs A冷(user 12)   —— 兩人都綁了平台帳號
//	M2  丙(純活動報名,fencer 綁 user 13)vs 丁(完全沒綁)
//	M3  戊 vs 己                          —— 都沒綁,用來組串關
//
// 下注者一律是 user 7,他不是任何一場的選手。
const (
	tourID   int64 = 1
	tourSlug       = "shifeng"
	bettor   int64 = 7
)

func setup(t *testing.T) (*Service[fakeTx], *fakeDB) {
	t.Helper()
	db := newFakeDB()
	db.addTournament(tourID, tourSlug, OddsConfig{})
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

func single(matchID string, side Side) []LegInput {
	return []LegInput{{MatchPublicID: matchID, Side: side}}
}

// ── 投票 ──────────────────────────────────────────────────────────

func TestVote一場一票再投即改票(t *testing.T) {
	svc, db := setup(t)
	ctx := context.Background()

	if _, err := svc.Vote(ctx, VoteParams{UserID: 21, MatchPublicID: "M1", Side: SideP1}); err != nil {
		t.Fatalf("第一次投票: %v", err)
	}
	odds, err := svc.Vote(ctx, VoteParams{UserID: 21, MatchPublicID: "M1", Side: SideP2})
	if err != nil {
		t.Fatalf("改票: %v", err)
	}
	if odds.P1Votes != 0 || odds.P2Votes != 1 {
		t.Fatalf("改票後票數 = (%d,%d),want (0,1) —— 改票必須是覆蓋而不是再插一列",
			odds.P1Votes, odds.P2Votes)
	}
	if odds.MyVote != SideP2 {
		t.Fatalf("MyVote = %d,want %d", odds.MyVote, SideP2)
	}
	if len(db.votes) != 1 {
		t.Fatalf("votes 有 %d 列,want 1(UNIQUE(match_id,user_id))", len(db.votes))
	}
}

// 投票會直接動賠率,所以回應裡只能有票數 —— 誰投給誰不可以離開伺服器。
func TestGetOdds只回自己的票(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()

	for _, u := range []int64{21, 22, 23} {
		if _, err := svc.Vote(ctx, VoteParams{UserID: u, MatchPublicID: "M1", Side: SideP1}); err != nil {
			t.Fatalf("投票: %v", err)
		}
	}
	if _, err := svc.Vote(ctx, VoteParams{UserID: 24, MatchPublicID: "M1", Side: SideP2}); err != nil {
		t.Fatalf("投票: %v", err)
	}

	// 24 號查詢:看得到總票數,看得到自己那一票,看不到是誰投了 P1。
	got, err := svc.GetOdds(ctx, GetOddsParams{MatchPublicIDs: []string{"M1"}, ViewerUserID: 24})
	if err != nil {
		t.Fatalf("GetOdds: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("回了 %d 場,want 1", len(got))
	}
	if got[0].P1Votes != 3 || got[0].P2Votes != 1 {
		t.Fatalf("票數 = (%d,%d),want (3,1)", got[0].P1Votes, got[0].P2Votes)
	}
	if got[0].MyVote != SideP2 {
		t.Fatalf("MyVote = %d,want %d", got[0].MyVote, SideP2)
	}

	// 匿名查詢:MyVote 必須是 0,不能因為「只有一個 P2 票」就推論出來。
	anon, err := svc.GetOdds(ctx, GetOddsParams{MatchPublicIDs: []string{"M1"}})
	if err != nil {
		t.Fatalf("GetOdds 匿名: %v", err)
	}
	if anon[0].MyVote != SideNone {
		t.Fatalf("匿名的 MyVote = %d,want 0", anon[0].MyVote)
	}

	// 賠率:P1 三票 / 總四票 → 0.92 × 14 / 8 = 1.61;P2 → 0.92 × 14 / 6 = 2.146…
	if anon[0].P1OddsMilli != 1610 || anon[0].P2OddsMilli != 2146 {
		t.Fatalf("賠率 = (%d,%d),want (1610,2146)", anon[0].P1OddsMilli, anon[0].P2OddsMilli)
	}
}

func TestVote開賽後不能投(t *testing.T) {
	svc, db := setup(t)
	db.matches["M1"].Status = MatchLive
	_, err := svc.Vote(context.Background(), VoteParams{UserID: 21, MatchPublicID: "M1", Side: SideP1})
	if !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("err = %v,want ErrVotingClosed", err)
	}
}

// ── 下注 ──────────────────────────────────────────────────────────

func TestPlaceBet扣款與鎖定賠率(t *testing.T) {
	svc, db := setup(t)
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k1",
	})

	// 零票時兩邊都是 1.84 → 100 × 1.84 = 184。
	if res.Bet.PotentialPayout != 184 {
		t.Fatalf("potential_payout = %d,want 184", res.Bet.PotentialPayout)
	}
	if len(res.Bet.Legs) != 1 || res.Bet.Legs[0].OddsMilli != 1840 {
		t.Fatalf("腿 = %+v,want odds_milli 1840", res.Bet.Legs)
	}
	if res.Bet.Legs[0].SideDisplayName != "李璃" {
		t.Fatalf("side_display_name = %q,want 李璃", res.Bet.Legs[0].SideDisplayName)
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

	// 下注要能被通知到,不然使用者只會看到餘額莫名其妙變了。
}

// 連點的核心測試:同一把鍵送兩次,只扣一次款、只有一張注單。
func TestPlaceBet冪等重送不重複扣款(t *testing.T) {
	svc, db := setup(t)
	p := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "same-key",
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
	// 重放不該再發一次通知。
}

// 併發連點。fake 的 InTx 是互斥的,所以這測的是「兩個 tx 不交錯時冪等是否正確」,
// **不是**資料庫的鎖行為 —— 後者必須在 bettingpg 對真 Postgres 測。
func TestPlaceBet併發同一把鍵只扣一次(t *testing.T) {
	svc, db := setup(t)
	p := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "race",
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
		Legs: single("M1", SideP1), IdempotencyKey: "k",
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
		Legs: single("M1", SideP2), IdempotencyKey: "k", // 賭自己輸
	})
	if !errors.Is(err, ErrSelfBet) {
		t.Fatalf("err = %v,want ErrSelfBet", err)
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
		Legs: single("M2", SideP2), IdempotencyKey: "k",
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
		Legs: single("M3", SideP1), IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatalf("PlaceBet: %v", err)
	}
	if res.Bet.PublicID == "" {
		t.Fatal("應該成立注單 —— 這是雙軌身分的固有代價")
	}
}

func TestPlaceBet同注單不可重押同場(t *testing.T) {
	svc, db := setup(t)
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			{MatchPublicID: "M1", Side: SideP1},
			{MatchPublicID: "M1", Side: SideP2}, // 押完 p1 再押 p2 = 穩賺
		},
		IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrDuplicateLeg) {
		t.Fatalf("err = %v,want ErrDuplicateLeg", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
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
				Legs: single("M1", SideP1), IdempotencyKey: "k",
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

func TestPlaceBet賠率跑掉就拒絕(t *testing.T) {
	svc, db := setup(t)
	// 客戶端以為是 5 倍(500),伺服器算出來是 1.84 倍(184)。
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k", ExpectedPayout: 500,
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
		Legs: single("M1", SideP1), IdempotencyKey: "k2", ExpectedPayout: 184,
	})
	if res.Bet.PotentialPayout != 184 {
		t.Fatalf("potential_payout = %d,want 184", res.Bet.PotentialPayout)
	}
}

func TestPlaceBet超過單筆上限(t *testing.T) {
	svc, db := setup(t)
	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: DefaultMaxStake + 1,
		Legs: single("M1", SideP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrStakeTooLarge) {
		t.Fatalf("err = %v,want ErrStakeTooLarge", err)
	}
	if got := db.balance(bettor); got != 10_000 {
		t.Fatalf("餘額 = %d,want 10000", got)
	}
}

// rollback 的關鍵驗證:餘額不足時,注單、腿、分錄、餘額、outbox 全部不留痕跡。
func TestPlaceBet餘額不足時整筆消失(t *testing.T) {
	svc, db := setup(t)
	db.balances[bettor] = 50

	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k",
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
	for m := int64(1); m <= 3; m++ {
		for u := int64(100); u < 160; u++ {
			db.votes[voteKey{m, u}] = SideP1
		}
	}
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: []LegInput{
			{MatchPublicID: "M1", Side: SideP2},
			{MatchPublicID: "M2", Side: SideP2},
			{MatchPublicID: "M3", Side: SideP2},
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
	base := PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k",
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
		{"side 不合法", mut(func(p *PlaceBetParams) { p.Legs[0].Side = 3 }), ErrInvalidRequest},
		{"腿數超過上限", mut(func(p *PlaceBetParams) {
			p.Legs = make([]LegInput, MaxLegsPerBet+1)
			for i := range p.Legs {
				p.Legs[i] = LegInput{MatchPublicID: "M" + strconv.Itoa(i), Side: SideP1}
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
		})
	}
}

func TestPlaceBet場次不屬於這屆(t *testing.T) {
	svc, db := setup(t)
	db.addTournament(2, "other", OddsConfig{})
	db.addMatch(9, "M9", 2, MatchReady, anonPlayer(901, "甲"), anonPlayer(902, "乙"))

	_, err := svc.PlaceBet(context.Background(), PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M9", SideP1), IdempotencyKey: "k",
	})
	if !errors.Is(err, ErrMatchNotInTournament) {
		t.Fatalf("err = %v,want ErrMatchNotInTournament", err)
	}
}

func TestListMyBets只列自己的(t *testing.T) {
	svc, db := setup(t)
	db.credit(8, 1_000)
	mustPlace(t, svc, PlaceBetParams{UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "a"})
	mustPlace(t, svc, PlaceBetParams{UserID: 8, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP2), IdempotencyKey: "b"})

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

// ── 結算 ──────────────────────────────────────────────────────────

func TestSettleMatch全腿贏就派彩(t *testing.T) {
	svc, db := setup(t)
	res := mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k",
	})
	payout := res.Bet.PotentialPayout

	db.finish("M1", SideP1, ResultNormal)
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
	if b := db.betByPublicID("BET1"); b.SettledAt == nil {
		t.Fatal("settled_at 仍是 NULL,違反 bets_settled_at_check")
	}
	if !slices.Equal(db.advisoryLocks, []int64{1}) {
		t.Fatalf("advisory lock = %v,want [1]", db.advisoryLocks)
	}
}

func TestSettleMatch押錯邊就是輸而且不動錢(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP2), IdempotencyKey: "k",
	})

	db.finish("M1", SideP1, ResultNormal)
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

// 重複結算同一場不可以再派一次彩。狀態檢查是這件事的權威,advisory lock 只擋併發。
func TestSettleMatch重複呼叫不重複派彩(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k",
	})
	db.finish("M1", SideP1, ResultNormal)

	if _, err := svc.SettleMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	after := db.balance(bettor)
	entries := len(db.entries)

	second, err := svc.SettleMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if len(second.Bets) != 0 {
		t.Fatalf("第二次動了 %d 張注單,want 0", len(second.Bets))
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
			{MatchPublicID: "M1", Side: SideP1},
			{MatchPublicID: "M3", Side: SideP1},
		},
		IdempotencyKey: "k",
	})
	payout := res.Bet.PotentialPayout // 100 × 1.84 × 1.84 = 338(逐步捨去)
	if payout != 338 {
		t.Fatalf("potential_payout = %d,want 338", payout)
	}

	db.finish("M1", SideP1, ResultNormal)
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

	db.finish("M3", SideP1, ResultNormal)
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
}

// 注單列鎖必須依 id 升冪取 —— 這是全系統的防死鎖慣例。
func TestSettleMatch鎖注單依id升冪(t *testing.T) {
	svc, db := setup(t)
	for i, key := range []string{"a", "b", "c"} {
		side := SideP1
		if i == 1 {
			side = SideP2
		}
		mustPlace(t, svc, PlaceBetParams{
			UserID: bettor, TournamentSlug: tourSlug, Stake: 10,
			Legs: single("M1", side), IdempotencyKey: key,
		})
	}
	db.finish("M1", SideP1, ResultNormal)
	if _, err := svc.SettleMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("SettleMatch: %v", err)
	}

	if len(db.lockedBets) != 1 {
		t.Fatalf("取鎖 %d 次,want 1", len(db.lockedBets))
	}
	got := db.lockedBets[0]
	if !slices.IsSorted(got) {
		t.Fatalf("取鎖順序 = %v,不是升冪", got)
	}
	if !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("取鎖 = %v,want [1 2 3]", got)
	}
}

func TestSettleMatch拒絕棄賽場次(t *testing.T) {
	svc, db := setup(t)
	db.finish("M1", SideP1, ResultWalkover)
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

// ── 棄賽退款 ──────────────────────────────────────────────────────

func TestVoidMatch單場全額退(t *testing.T) {
	svc, db := setup(t)
	mustPlace(t, svc, PlaceBetParams{
		UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
		Legs: single("M1", SideP1), IdempotencyKey: "k",
	})

	db.finish("M1", SideP1, ResultWalkover)
	out, err := svc.VoidMatch(context.Background(), "M1")
	if err != nil {
		t.Fatalf("VoidMatch: %v", err)
	}
	if len(out.Bets) != 1 || out.Bets[0].Status != BetVoid || out.Bets[0].Amount != 100 {
		t.Fatalf("結果 = %+v,want 一張 void 退 100", out.Bets)
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
}

// 串關的四種分支。每一種都從同一張兩腿注單出發,只差在另一腿的狀態。
func TestVoidMatch串關重算的四種分支(t *testing.T) {
	// 另一腿先被判成什麼:pending 就不碰、won/lost 就先結算 M3。
	cases := []struct {
		name       string
		otherLeg   LegResult
		wantStatus BetStatus
		wantPaid   int64 // 本次退款/派彩金額
		wantRecalc bool
	}{
		{"剩餘腿還有待判定_維持open並重算", LegPending, BetOpen, 0, true},
		{"剩餘腿全贏_依重算金額派彩", LegWon, BetWon, 184, true},
		{"剩餘腿已輸_棄賽不救", LegLost, BetLost, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, db := setup(t)
			res := mustPlace(t, svc, PlaceBetParams{
				UserID: bettor, TournamentSlug: tourSlug, Stake: 100,
				Legs: []LegInput{
					{MatchPublicID: "M1", Side: SideP1},
					{MatchPublicID: "M3", Side: SideP1},
				},
				IdempotencyKey: "k",
			})
			if res.Bet.PotentialPayout != 338 {
				t.Fatalf("原始 potential_payout = %d,want 338", res.Bet.PotentialPayout)
			}

			// 先把 M3 判掉(如果這個分支需要)。
			switch c.otherLeg {
			case LegWon:
				db.finish("M3", SideP1, ResultNormal)
				if _, err := svc.SettleMatch(context.Background(), "M3"); err != nil {
					t.Fatalf("結算 M3: %v", err)
				}
			case LegLost:
				db.finish("M3", SideP2, ResultNormal)
				if _, err := svc.SettleMatch(context.Background(), "M3"); err != nil {
					t.Fatalf("結算 M3: %v", err)
				}
			case LegPending, LegVoid:
			}

			before := db.balance(bettor)
			db.finish("M1", SideP1, ResultWalkover)
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
			if oc.Status != c.wantStatus {
				t.Fatalf("status = %s,want %s", oc.Status, c.wantStatus)
			}
			if oc.Amount != c.wantPaid {
				t.Fatalf("動錢 = %d,want %d", oc.Amount, c.wantPaid)
			}
			if oc.PayoutRecalculated != c.wantRecalc {
				t.Fatalf("payout_recalculated = %v,want %v", oc.PayoutRecalculated, c.wantRecalc)
			}

			// 重算後的賠付只剩 M3 那一腿的賠率:100 × 1.84 = 184。
			b := db.betByPublicID("BET1")
			if b.PotentialPayout != 184 {
				t.Fatalf("重算後 potential_payout = %d,want 184(只剩 M3 的 1.84 倍)", b.PotentialPayout)
			}
			if !b.PayoutRecalculated {
				t.Fatal("payout_recalculated 必須是 true,對帳才看得出金額為什麼變了")
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
			{MatchPublicID: "M1", Side: SideP1},
			{MatchPublicID: "M3", Side: SideP1},
		},
		IdempotencyKey: "k",
	})

	db.finish("M1", SideP1, ResultWalkover)
	if _, err := svc.VoidMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("VoidMatch: %v", err)
	}

	db.finish("M3", SideP1, ResultNormal)
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
			{MatchPublicID: "M1", Side: SideP1},
			{MatchPublicID: "M3", Side: SideP1},
		},
		IdempotencyKey: "k",
	})

	db.finish("M1", SideP1, ResultWalkover)
	if _, err := svc.VoidMatch(context.Background(), "M1"); err != nil {
		t.Fatalf("VoidMatch M1: %v", err)
	}
	db.finish("M3", SideP1, ResultWalkover)
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
		Legs: single("M1", SideP1), IdempotencyKey: "k",
	})
	db.finish("M1", SideP1, ResultWalkover)

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
	db.finish("M1", SideP1, ResultNormal)
	_, err := svc.VoidMatch(context.Background(), "M1")
	if !errors.Is(err, ErrNotWalkover) {
		t.Fatalf("err = %v,want ErrNotWalkover", err)
	}
}

// ── 雜項 ──────────────────────────────────────────────────────────

// 冪等的判準不能含賠率:賠率隨票數浮動,含進去的話每次重試都變成「不同請求」。
func TestRequestHash不含賠率但含關鍵欄位(t *testing.T) {
	base := PlaceBetParams{UserID: 7, TournamentSlug: "a", Stake: 100,
		Legs: single("M1", SideP1), ExpectedPayout: 184}
	same := base
	same.ExpectedPayout = 999 // 客戶端當時看到的賠率不同,但下的是同一注
	if placeRequestHash(base) != placeRequestHash(same) {
		t.Fatal("expected_payout 不該影響請求雜湊")
	}

	for _, mod := range []func(*PlaceBetParams){
		func(p *PlaceBetParams) { p.UserID = 8 },
		func(p *PlaceBetParams) { p.TournamentSlug = "b" },
		func(p *PlaceBetParams) { p.Stake = 101 },
		func(p *PlaceBetParams) { p.Legs = single("M2", SideP1) },
		func(p *PlaceBetParams) { p.Legs = single("M1", SideP2) },
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
