package bettingpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/bettingpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants 第三條):
// 這一層要驗的正是資料庫的鎖與約束行為,mock 掉資料庫的併發測試沒有意義。
// core 那邊的 fake 測得了分支邏輯,測不了「兩個 session 同時結算會發生什麼」。
//
// 共用一個容器,各測試用獨立的使用者/賽事/場次隔離,不互相踩。

var (
	pool *pgxpool.Pool
	led  *ledgerpg.Service
	repo *bettingpg.Repository
	svc  *betting.Service[pgx.Tx]
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		led = ledgerpg.New(pool)
		repo = bettingpg.NewRepository(pool)
		// 直接注入 *ledgerpg.Service:它的 ApplyInTx 就是 betting.Ledger[pgx.Tx],
		// 中間沒有任何 adapter(見 bettingpg 的 package doc)。
		svc = bettingpg.New(pool, led)
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// arena 是一組隔離的測試資料:一屆賽事 + 一位下注者。
type arena struct {
	tournamentID int64
	slug         string
	user         int64
}

// newArena 建一屆 config 為 "{}"(v1 語意:只有勝負盤)的賽事。
func newArena(t *testing.T, initialBalance int64) arena {
	t.Helper()
	return newArenaWith(t, initialBalance, "{}")
}

// newFullArena 建一屆開發庫那種規則的賽事(三局兩勝、四種盤口、line 90)。
func newFullArena(t *testing.T, initialBalance int64) arena {
	t.Helper()
	return newArenaWith(t, initialBalance, fullConfig)
}

func newArenaWith(t *testing.T, initialBalance int64, config string) arena {
	t.Helper()
	id, slug := newTournament(t, config)
	u := newUser(t)
	if initialBalance > 0 {
		credit(t, u, initialBalance)
	}
	return arena{tournamentID: id, slug: slug, user: u}
}

// addMatch 在這屆加一場可下注的比賽(已開盤)。
func (a arena) addMatch(t *testing.T) mrec {
	t.Helper()
	p1 := newPlayer(t, a.tournamentID, nil)
	p2 := newPlayer(t, a.tournamentID, nil)
	return newMatch(t, a.tournamentID, p1, p2)
}

// tryLockBalance 從**另一條連線**試鎖某人的餘額列,回傳 SQLSTATE
// (空字串 = 鎖得到,"55P03" = 被別人持有)。
// 用 NOWAIT 而不是 sleep 觀察有沒有卡住:答案是確定的,不會因為機器快慢而偶發。
func tryLockBalance(t *testing.T, userID int64) string {
	t.Helper()
	var bal int64
	err := pool.QueryRow(context.Background(),
		`SELECT balance FROM platform.user_balances
		  WHERE user_id = $1 AND currency = 'coin' FOR UPDATE NOWAIT`, userID).Scan(&bal)
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	t.Fatalf("試鎖餘額列時的非預期錯誤: %v", err)
	return ""
}

// tryLockBet 從另一條連線試鎖一張注單列,回傳 SQLSTATE(空字串 = 鎖得到)。
func tryLockBet(t *testing.T, betID int64) string {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM activity.bets WHERE id = $1 FOR UPDATE NOWAIT`, betID).Scan(&id)
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	t.Fatalf("試鎖注單列時的非預期錯誤: %v", err)
	return ""
}

// place 下一注,每次都用新的冪等鍵(要測重放的測試自己傳同一把鍵)。
func place(t *testing.T, a arena, stake int64, legs ...betting.LegInput) *betting.PlaceBetResult {
	t.Helper()
	res, err := svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID:         a.user,
		TournamentSlug: a.slug,
		Stake:          stake,
		Legs:           legs,
		IdempotencyKey: newULID(t),
	})
	if err != nil {
		t.Fatalf("下注: %v", err)
	}
	return res
}

// ── 盤口 ──────────────────────────────────────────────────────────

// TestOpenMarketsCreatesEightRows:開發庫那種 config 要建出 1 + 3 + 3 + 1 = 8 個盤口,
// round_no 與 params 進了資料庫也對;同一場再開一次撞唯一索引,必須出聲。
func TestOpenMarketsCreatesEightRows(t *testing.T) {
	setup(t)
	a := newFullArena(t, 0)
	m := a.addMatch(t)
	if len(m.markets) != 8 {
		t.Fatalf("建了 %d 個盤口,應為 8", len(m.markets))
	}

	type row struct {
		kind   string
		round  *int32
		params string
		status string
	}
	rows, err := pool.Query(context.Background(),
		`SELECT kind, round_no, params::text, status FROM activity.markets
		  WHERE match_id = $1 ORDER BY kind, round_no NULLS FIRST`, m.id)
	if err != nil {
		t.Fatalf("讀盤口: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.round, &r.params, &r.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 8 {
		t.Fatalf("資料庫裡有 %d 列,應為 8", len(got))
	}
	count := map[string]int{}
	for _, r := range got {
		count[r.kind]++
		if r.status != "open" {
			t.Fatalf("剛開的盤口狀態 = %s", r.status)
		}
		perRound := r.kind == "round_winner" || r.kind == "duration"
		if perRound != (r.round != nil) {
			t.Fatalf("%s 的 round_no = %v,與 kind 不一致(markets_round_no_check)", r.kind, r.round)
		}
		if r.kind == "duration" && r.params != `{"line_seconds": 90}` {
			t.Fatalf("duration 的 params = %s,應為 line_seconds 90", r.params)
		}
		if r.kind != "duration" && r.params != `{}` {
			t.Fatalf("%s 的 params = %s,應為 {}", r.kind, r.params)
		}
	}
	if count["match_winner"] != 1 || count["round_winner"] != 3 || count["duration"] != 3 || count["score"] != 1 {
		t.Fatalf("各種盤口數 = %v,應為 1/3/3/1", count)
	}
	for _, mk := range m.markets {
		if mk.PublicID == "" || mk.Status != betting.MarketOpen {
			t.Fatalf("回傳的盤口 %+v 缺 public_id 或不是 open", mk)
		}
	}

	// 重複開盤:23505 → ErrMarketsExist,而且一列都不多。
	err = repo.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.OpenMarketsInTx(ctx, tx, m.pub)
		return err
	})
	if !errors.Is(err, betting.ErrMarketsExist) {
		t.Fatalf("重複開盤應回 ErrMarketsExist,實際 %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.markets WHERE match_id = $1`, m.id); n != 8 {
		t.Fatalf("重複開盤後有 %d 列,應仍為 8", n)
	}

	// 預設 config 只有勝負盤一個。
	b := newArena(t, 0)
	if mb := b.addMatch(t); len(mb.markets) != 1 || mb.markets[0].Kind != betting.MarketMatchWinner {
		t.Fatalf("預設 config 建了 %+v,應只有 match_winner", mb.markets)
	}
}

// TestCloseMarketsClosesAllOpen:關盤把 8 個 open 一起 closed;再關一次是 0。
func TestCloseMarketsClosesAllOpen(t *testing.T) {
	setup(t)
	a := newFullArena(t, 0)
	m := a.addMatch(t)
	if n := startMatch(t, m); n != 8 {
		t.Fatalf("關掉 %d 個盤口,應為 8", n)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.markets WHERE match_id = $1 AND status = 'closed'`, m.id); n != 8 {
		t.Fatalf("closed 的盤口 %d 個,應為 8", n)
	}
	var again int
	if err := repo.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		again, err = svc.CloseMarketsInTx(ctx, tx, m.pub)
		return err
	}); err != nil || again != 0 {
		t.Fatalf("第二次關盤 = (%d,%v),應為 (0,nil)", again, err)
	}
}

// ── 冪等 ──────────────────────────────────────────────────────────

// TestPlaceBetSameKeyConcurrent:同一把冪等鍵被 N 個 goroutine 同時送出,
// 只能建一張注單、只能扣一次款。
//
// 這是整個套件最重要的一條:下注沒有任何 UNIQUE 約束擋得住連點
// (同一人對同一場下兩注是合法的),擋住的只有 LockUser + 冪等鍵這對組合。
func TestPlaceBetSameKeyConcurrent(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m := a.addMatch(t)

	const n = 8
	key := newULID(t)
	results := make([]*betting.PlaceBetResult, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 盡量讓 N 個請求真的同時進資料庫
			results[i], errs[i] = svc.PlaceBet(context.Background(), betting.PlaceBetParams{
				UserID:         a.user,
				TournamentSlug: a.slug,
				Stake:          100,
				Legs:           []betting.LegInput{m.winner(t, betting.OutcomeP1)},
				IdempotencyKey: key,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	fresh, publicID := 0, ""
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 個請求失敗: %v", i, errs[i])
		}
		if !results[i].Replayed {
			fresh++
		}
		if publicID == "" {
			publicID = results[i].Bet.PublicID
		} else if results[i].Bet.PublicID != publicID {
			t.Fatalf("同一把鍵回了兩張不同的注單: %s vs %s", publicID, results[i].Bet.PublicID)
		}
	}
	if fresh != 1 {
		t.Fatalf("實際執行次數 = %d,應為 1", fresh)
	}

	if n := countRows(t, `SELECT count(*) FROM activity.bets WHERE user_id = $1`, a.user); n != 1 {
		t.Fatalf("注單數 = %d,應為 1", n)
	}
	if n := countRows(t,
		`SELECT count(*) FROM activity.bet_legs l JOIN activity.bets b ON b.id = l.bet_id
		  WHERE b.user_id = $1`, a.user); n != 1 {
		t.Fatalf("腿數 = %d,應為 1", n)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.token_entries
		  WHERE user_id = $1 AND reason = 'bet_stake'`, a.user); n != 1 {
		t.Fatalf("扣款分錄數 = %d,應為 1", n)
	}
	if bal := balanceOf(t, a.user); bal != 900 {
		t.Fatalf("餘額 = %d,應為 900(只扣一次)", bal)
	}
	assertReconciled(t, a.user)

	// 稽核鏈:注單指得到分錄,分錄指得回注單。
	b := readBet(t, publicID)
	if b.StakeEntryID == nil {
		t.Fatal("ledger_stake_entry_id 沒寫進去")
	}
	amount, reason, refType, refID := entryOf(t, *b.StakeEntryID)
	if amount != -100 || reason != "bet_stake" {
		t.Fatalf("扣款分錄 amount=%d reason=%s", amount, reason)
	}
	if refType == nil || *refType != "bet" || refID == nil || *refID != b.ID {
		t.Fatalf("扣款分錄沒指回注單: ref_type=%v ref_id=%v", refType, refID)
	}
}

// TestPlaceBetInsufficientBalanceLeavesNoTrace:餘額不足時,注單、腿、分錄、
// 餘額、冪等鍵全部還原 —— 一次失敗的下注不該在任何一張表留下半個字。
//
// 這條驗的是 InTx 的 rollback 真的蓋得住 Ledger 在同一個 tx 裡寫的東西:
// 動錢是最後一步,失敗時前面建好的注單與腿必須一起消失。
func TestPlaceBetInsufficientBalanceLeavesNoTrace(t *testing.T) {
	setup(t)
	a := newArena(t, 50)
	m := a.addMatch(t)

	key := newULID(t)
	_, err := svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID:         a.user,
		TournamentSlug: a.slug,
		Stake:          100,
		Legs:           []betting.LegInput{m.winner(t, betting.OutcomeP1)},
		IdempotencyKey: key,
	})
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("應回餘額不足,實際 %v", err)
	}

	if n := countRows(t, `SELECT count(*) FROM activity.bets WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("留下了 %d 張注單", n)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.bet_legs WHERE match_id = $1`, m.id); n != 0 {
		t.Fatalf("留下了 %d 條腿", n)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.token_entries WHERE user_id = $1`, a.user); n != 1 {
		t.Fatalf("分錄數 = %d,應只剩入帳那一筆", n)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.idempotency_keys WHERE key LIKE '%' || $1`, key); n != 0 {
		t.Fatalf("留下了 %d 把冪等鍵(合法重試會被誤擋)", n)
	}
	if bal := balanceOf(t, a.user); bal != 50 {
		t.Fatalf("餘額 = %d,應原封不動 50", bal)
	}
	assertReconciled(t, a.user)

	// 錢進來之後,同一把鍵必須還能用 —— 失敗的請求不該永久燒掉一把鍵。
	credit(t, a.user, 100)
	res, err := svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID:         a.user,
		TournamentSlug: a.slug,
		Stake:          100,
		Legs:           []betting.LegInput{m.winner(t, betting.OutcomeP1)},
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("補錢後重試: %v", err)
	}
	if res.Replayed {
		t.Fatal("補錢後的重試被當成重放,錢沒真的扣")
	}
	assertReconciled(t, a.user)
}

// TestSelfBetBlockedBothPaths:「選手不得對自己參與的場次下注」的兩條比對路徑
// 都要擋得住。第二條(純活動報名但劍士本人綁過帳號)漏掉的話,整個檢查形同虛設。
func TestSelfBetBlockedBothPaths(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	other := newPlayer(t, a.tournamentID, nil)

	// 路徑一:只綁 tournament_players.user_id(fencers 那欄留空,才驗得出這條單獨成立)
	viaPlayer := newPlayerSplit(t, a.tournamentID, &a.user, nil)
	m1 := newMatch(t, a.tournamentID, viaPlayer, other)
	_, err := svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 10,
		Legs:           []betting.LegInput{m1.winner(t, betting.OutcomeP2)},
		IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrSelfBet) {
		t.Fatalf("路徑一應擋下自賭,實際 %v", err)
	}
	// 押時長盤也一樣:規則以場次為單位,不是以盤口。
	_, err = svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 10,
		Legs:           []betting.LegInput{m1.leg(t, betting.MarketDuration, 1, betting.OutcomeOver)},
		IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrSelfBet) {
		t.Fatalf("路徑一(時長盤)應擋下自賭,實際 %v", err)
	}

	// 路徑二:fencers.user_id(報名時沒綁,但劍士本人有平台帳號)
	viaFencer := newPlayerSplit(t, a.tournamentID, nil, &a.user)
	other2 := newPlayer(t, a.tournamentID, nil)
	m2 := newMatch(t, a.tournamentID, other2, viaFencer)
	_, err = svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 10,
		Legs:           []betting.LegInput{m2.winner(t, betting.OutcomeP1)},
		IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrSelfBet) {
		t.Fatalf("路徑二應擋下自賭,實際 %v", err)
	}
	if bal := balanceOf(t, a.user); bal != 1000 {
		t.Fatalf("餘額 = %d,一毛都不該動", bal)
	}
}

// ── 同一場只能一腿:應用層 + DB 兩層 ──────────────────────────────

// TestPlaceBetSameMatchTwoMarketsRejected:同一注單押同一場的兩個**不同**盤口,
// 應用層擋(ErrDuplicateMatchInParlay),一毛不扣、一列不留。
func TestPlaceBetSameMatchTwoMarketsRejected(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	_, err := svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 100,
		Legs: []betting.LegInput{
			m.winner(t, betting.OutcomeP1),
			m.leg(t, betting.MarketScore, 0, "p1_2_0"),
		},
		IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrDuplicateMatchInParlay) {
		t.Fatalf("應回 ErrDuplicateMatchInParlay,實際 %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.bets WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("留下了 %d 張注單", n)
	}
	if bal := balanceOf(t, a.user); bal != 1000 {
		t.Fatalf("餘額 = %d,應為 1000", bal)
	}
}

// TestPlaceBetResolvesMatchFromMarket:請求只帶 market_public_id(proto 的契約),
// 場次由盤口反推;帶錯的 match_public_id 被拒;同場兩個盤口一樣擋。
func TestPlaceBetResolvesMatchFromMarket(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m1, m2 := a.addMatch(t), a.addMatch(t)
	only := func(l betting.LegInput) betting.LegInput { l.MatchPublicID = ""; return l }

	res := place(t, a, 100, only(m1.winner(t, betting.OutcomeP1)), only(m2.leg(t, betting.MarketDuration, 1, betting.OutcomeUnder)))
	if len(res.Bet.Legs) != 2 || res.Bet.Legs[0].MatchPublicID != m1.pub || res.Bet.Legs[1].MatchPublicID != m2.pub {
		t.Fatalf("場次沒反推出來: %+v", res.Bet.Legs)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.bet_legs WHERE bet_id = $1 AND match_id = $2`, readBet(t, res.Bet.PublicID).ID, m2.id); n != 1 {
		t.Fatalf("m2 的腿沒帶對 match_id")
	}

	_, err := svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 10,
		Legs:           []betting.LegInput{only(m1.winner(t, betting.OutcomeP1)), only(m1.leg(t, betting.MarketScore, 0, "p1_2_0"))},
		IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrDuplicateMatchInParlay) {
		t.Fatalf("不帶場次的同場兩腿應回 ErrDuplicateMatchInParlay,實際 %v", err)
	}
	bad := m2.winner(t, betting.OutcomeP1)
	bad.MatchPublicID = m1.pub
	_, err = svc.PlaceBet(context.Background(), betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 10,
		Legs: []betting.LegInput{bad}, IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrMarketNotFound) {
		t.Fatalf("帶錯場次應回 ErrMarketNotFound,實際 %v", err)
	}
	if bal := balanceOf(t, a.user); bal != 900 {
		t.Fatalf("餘額 = %d,應為 900", bal)
	}
	// 投票只帶 market_public_id。
	if o, err := svc.Vote(context.Background(), betting.VoteParams{
		UserID: a.user, MarketPublicID: m1.market(t, betting.MarketScore, 0).PublicID, Outcome: "p2_2_0",
	}); err != nil || o.MatchID != m1.id || o.MyVote != "p2_2_0" {
		t.Fatalf("不帶場次投票 = (%+v,%v)", o, err)
	}
	// 不存在的盤口。
	if _, err := svc.Vote(context.Background(), betting.VoteParams{
		UserID: a.user, MarketPublicID: newULID(t), Outcome: betting.OutcomeP1,
	}); !errors.Is(err, betting.ErrMarketNotFound) {
		t.Fatalf("不存在的盤口應回 ErrMarketNotFound,實際 %v", err)
	}
}

// TestInsertLegsSameMatchTwoMarketsBubblesUp:繞過應用層直接插兩腿(同場不同盤口),
// bet_legs_bet_match_uq 必須讓 23505 冒上來。
//
// 為什麼不能 ON CONFLICT DO NOTHING 吞掉:少插一腿的注單會用**多一腿算出來的**
// 賠付派彩 —— 那是憑空多發錢。core 已經先擋掉重複,這裡驗的是最後一道 DB 兜底。
func TestInsertLegsSameMatchTwoMarketsBubblesUp(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	mw, sc := m.market(t, betting.MarketMatchWinner, 0), m.market(t, betting.MarketScore, 0)

	ctx := context.Background()
	err := repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		betID, _, err := repo.InsertBet(ctx, tx, betting.NewBet{
			PublicID: newULID(t), TournamentID: a.tournamentID,
			UserID: a.user, Stake: 100, PotentialPayout: 200,
		})
		if err != nil {
			return err
		}
		return repo.InsertLegs(ctx, tx, betID, []betting.NewLeg{
			{MatchID: m.id, MarketID: mw.ID, Outcome: betting.OutcomeP1, OddsMilli: 1500},
			{MatchID: m.id, MarketID: sc.ID, Outcome: "p1_2_0", OddsMilli: 3680},
		})
	})
	if !errors.Is(err, betting.ErrDuplicateMatchInParlay) {
		t.Fatalf("應回 ErrDuplicateMatchInParlay(23505 冒上來),實際 %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.bets WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("失敗的注單留下了 %d 列", n)
	}

	// 複合外鍵:腿的 match_id 與盤口的 match_id 對不上也要炸(bet_legs_market_match_fkey)。
	m2 := a.addMatch(t)
	err = repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		betID, _, err := repo.InsertBet(ctx, tx, betting.NewBet{
			PublicID: newULID(t), TournamentID: a.tournamentID,
			UserID: a.user, Stake: 100, PotentialPayout: 200,
		})
		if err != nil {
			return err
		}
		return repo.InsertLegs(ctx, tx, betID, []betting.NewLeg{
			{MatchID: m2.id, MarketID: mw.ID, Outcome: betting.OutcomeP1, OddsMilli: 1500},
		})
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("盤口不屬於那場應撞複合外鍵(23503),實際 %v", err)
	}
}

// ── 投票 ──────────────────────────────────────────────────────────

// TestVoteAndOdds:投票 UPSERT(一盤口一票、改票即覆蓋)與賠率推導。
// 回應裡只有票數與自己那一票,型別上就沒有「別人投給誰」可以洩漏。
func TestVoteAndOdds(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	mw := m.market(t, betting.MarketMatchWinner, 0)
	ctx := context.Background()

	vote := func(o betting.Outcome) (*betting.MarketOdds, error) {
		return svc.Vote(ctx, betting.VoteParams{
			UserID: a.user, MatchPublicID: m.pub, MarketPublicID: mw.PublicID, Outcome: o,
		})
	}
	if _, err := vote(betting.OutcomeP1); err != nil {
		t.Fatalf("投票: %v", err)
	}
	// 改票:覆蓋同一列,不是再插一列。
	odds, err := vote(betting.OutcomeP2)
	if err != nil {
		t.Fatalf("改票: %v", err)
	}
	if odds.Votes[betting.OutcomeP1] != 0 || odds.Votes[betting.OutcomeP2] != 1 {
		t.Fatalf("改票後票數 = %v,應為 p1=0 p2=1", odds.Votes)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.votes WHERE market_id = $1`, mw.ID); n != 1 {
		t.Fatalf("投票列數 = %d,改票不該新增列", n)
	}
	if odds.MyVote != betting.OutcomeP2 {
		t.Fatalf("MyVote = %q,應為 p2", odds.MyVote)
	}
	// 票多的那邊賠率低,這是隱含機率的直接結果。
	if odds.Odds[betting.OutcomeP2] >= odds.Odds[betting.OutcomeP1] {
		t.Fatalf("P2 有票賠率應較低: %v", odds.Odds)
	}

	castVotes(t, mw, map[betting.Outcome]int{betting.OutcomeP1: 4})
	got, err := svc.GetOdds(ctx, betting.GetOddsParams{
		MatchPublicIDs: []string{m.pub}, ViewerUserID: a.user,
	})
	if err != nil {
		t.Fatalf("查賠率: %v", err)
	}
	if len(got) != 1 || len(got[0].Markets) != 8 {
		t.Fatalf("應回 1 場 8 個盤口,實際 %+v", got)
	}
	var gmw, gsc betting.MarketOdds
	for _, mk := range got[0].Markets {
		switch mk.Kind {
		case betting.MarketMatchWinner:
			gmw = mk
		case betting.MarketScore:
			gsc = mk
		}
	}
	if gmw.Votes[betting.OutcomeP1] != 4 || gmw.Votes[betting.OutcomeP2] != 1 || gmw.MyVote != betting.OutcomeP2 {
		t.Fatalf("勝負盤票數不對: %+v", gmw)
	}
	// 沒人投的比分盤:四格零票 3.68 倍,而且 MyVote 空。
	if len(gsc.Outcomes) != 4 || gsc.Odds["p2_2_1"] != 3680 || gsc.MyVote != betting.OutcomeNone {
		t.Fatalf("比分盤不對: %+v", gsc)
	}
	if !got[0].OpenForBets {
		t.Fatal("ready 的場次應該可下注")
	}
	// 匿名查詢不該帶出任何人的票。
	anon, err := svc.GetOdds(ctx, betting.GetOddsParams{MatchPublicIDs: []string{m.pub}})
	if err != nil {
		t.Fatalf("匿名查賠率: %v", err)
	}
	for _, mk := range anon[0].Markets {
		if mk.MyVote != betting.OutcomeNone {
			t.Fatalf("匿名查詢回了 MyVote = %q", mk.MyVote)
		}
		if mk.Kind == betting.MarketMatchWinner && mk.Odds[betting.OutcomeP1] != gmw.Odds[betting.OutcomeP1] {
			t.Fatal("賠率對不同人不一樣,那就不是賠率了")
		}
	}
}

// TestVoteRejectedOnClosedMarketAndBadOutcome:對 closed 盤口投票被拒、outcome 不合法被拒,
// 而且兩者都不留下任何一列。
func TestVoteRejectedOnClosedMarketAndBadOutcome(t *testing.T) {
	setup(t)
	a := newFullArena(t, 0)
	m := a.addMatch(t)
	ctx := context.Background()
	sc := m.market(t, betting.MarketScore, 0)

	// outcome 不合法:best_of=3 沒有 p1_3_0;時長盤沒有 p1。
	for _, c := range []struct {
		mk betting.Market
		o  betting.Outcome
	}{
		{sc, "p1_3_0"},
		{m.market(t, betting.MarketDuration, 2), betting.OutcomeP1},
		{m.market(t, betting.MarketMatchWinner, 0), betting.OutcomeOver},
	} {
		_, err := svc.Vote(ctx, betting.VoteParams{
			UserID: 4242, MatchPublicID: m.pub, MarketPublicID: c.mk.PublicID, Outcome: c.o,
		})
		if !errors.Is(err, betting.ErrOutcomeInvalid) {
			t.Fatalf("%s/%s 應回 ErrOutcomeInvalid,實際 %v", c.mk.Kind, c.o, err)
		}
	}
	// 合法的比分照收。
	if _, err := svc.Vote(ctx, betting.VoteParams{
		UserID: 4242, MatchPublicID: m.pub, MarketPublicID: sc.PublicID, Outcome: "p2_2_1",
	}); err != nil {
		t.Fatalf("合法比分被拒: %v", err)
	}

	// 關盤之後(場次仍是 ready 也一樣):盤口 closed 就不能投。
	if err := repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.CloseMarketsInTx(ctx, tx, m.pub)
		return err
	}); err != nil {
		t.Fatalf("關盤: %v", err)
	}
	_, err := svc.Vote(ctx, betting.VoteParams{
		UserID: 4243, MatchPublicID: m.pub, MarketPublicID: sc.PublicID, Outcome: "p1_2_0",
	})
	if !errors.Is(err, betting.ErrVotingClosed) {
		t.Fatalf("closed 盤口應回 ErrVotingClosed,實際 %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.votes WHERE market_id = $1`, sc.ID); n != 1 {
		t.Fatalf("投票列數 = %d,應只有那一票合法的", n)
	}

	// 關盤後下注也被拒(ErrMarketClosed),即使場次狀態仍 ready。
	credit(t, a.user, 100)
	_, err = svc.PlaceBet(ctx, betting.PlaceBetParams{
		UserID: a.user, TournamentSlug: a.slug, Stake: 10,
		Legs:           []betting.LegInput{m.winner(t, betting.OutcomeP1)},
		IdempotencyKey: newULID(t),
	})
	if !errors.Is(err, betting.ErrMarketClosed) {
		t.Fatalf("closed 盤口下注應回 ErrMarketClosed,實際 %v", err)
	}
	if bal := balanceOf(t, a.user); bal != 100 {
		t.Fatalf("餘額 = %d,應為 100", bal)
	}
}

// ── 結算 ──────────────────────────────────────────────────────────

// TestSettlePayoutIsNewPositiveEntry:派彩是**新的正數分錄**,舊紀錄一個字都沒改,
// 三個稽核鏈欄位都寫得進去。
func TestSettlePayoutIsNewPositiveEntry(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m := a.addMatch(t)

	res := place(t, a, 100, m.winner(t, betting.OutcomeP1))
	before := readBet(t, res.Bet.PublicID)
	stakeEntry := *before.StakeEntryID
	stakeAmount, _, _, _ := entryOf(t, stakeEntry)

	playRound(t, m, 1, m.p1, 100)
	finishMatch(t, m, m.p1)
	out, err := svc.SettleMatch(context.Background(), m.pub)
	if err != nil {
		t.Fatalf("結算: %v", err)
	}
	if out.LegsWon != 1 || out.LegsLost != 0 || len(out.Bets) != 1 || out.MarketsSettled != 1 {
		t.Fatalf("結算結果不符: %+v", out)
	}

	after := readBet(t, res.Bet.PublicID)
	if after.Status != "won" {
		t.Fatalf("注單狀態 = %s,應為 won", after.Status)
	}
	if after.SettledAt == nil {
		t.Fatal("settled_at 沒寫(bets_settled_at_check 要求離開 open 就要有值)")
	}
	if after.PayoutEntryID == nil {
		t.Fatal("ledger_payout_entry_id 沒寫進去(稽核鏈斷一半)")
	}
	payAmount, payReason, payRefType, payRefID := entryOf(t, *after.PayoutEntryID)
	if payAmount != after.PotentialPayout || payAmount <= 0 {
		t.Fatalf("派彩分錄 amount=%d,應等於 potential_payout=%d 且為正", payAmount, after.PotentialPayout)
	}
	if payReason != "bet_payout" {
		t.Fatalf("派彩 reason = %s", payReason)
	}
	if payRefType == nil || *payRefType != "bet" || payRefID == nil || *payRefID != after.ID {
		t.Fatalf("派彩分錄沒指回注單: %v %v", payRefType, payRefID)
	}
	// append-only:原扣款分錄與它的 id 都沒被動過。
	if *after.StakeEntryID != stakeEntry {
		t.Fatal("ledger_stake_entry_id 被改寫了")
	}
	if nowAmount, _, _, _ := entryOf(t, stakeEntry); nowAmount != stakeAmount {
		t.Fatalf("舊分錄金額被改:%d → %d", stakeAmount, nowAmount)
	}
	if bal := balanceOf(t, a.user); bal != 1000-100+payAmount {
		t.Fatalf("餘額 = %d,應為 %d", bal, 1000-100+payAmount)
	}
	if s := marketStatus(t, m.markets[0].ID); s != "settled" {
		t.Fatalf("盤口狀態 = %s,應為 settled", s)
	}
	assertReconciled(t, a.user)
}

// TestSettleLoserKeepsMoneyGone:輸的注單不動錢(本金在下注當下就扣掉了)。
func TestSettleLoserKeepsMoneyGone(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m := a.addMatch(t)

	res := place(t, a, 100, m.winner(t, betting.OutcomeP1))
	playRound(t, m, 1, m.p2, 100)
	finishMatch(t, m, m.p2)
	if _, err := svc.SettleMatch(context.Background(), m.pub); err != nil {
		t.Fatalf("結算: %v", err)
	}

	b := readBet(t, res.Bet.PublicID)
	if b.Status != "lost" || b.PayoutEntryID != nil || b.RefundEntryID != nil {
		t.Fatalf("輸的注單不該有任何金流: %+v", b)
	}
	if bal := balanceOf(t, a.user); bal != 900 {
		t.Fatalf("餘額 = %d,應為 900", bal)
	}
	assertReconciled(t, a.user)
}

// TestSettleBestOf3TwoZeroVoidsThirdRound:三局兩勝 2:0 結束,第三回合的兩個盤口被 void,
// 押它們的單場注單**全額退款**(Ledger 有 bet_refund 分錄),不是 lost;
// 其餘六個盤口 settled,比分盤 2:0 那一格派彩。
func TestSettleBestOf3TwoZeroVoidsThirdRound(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	r3w, r3d := m.market(t, betting.MarketRoundWinner, 3), m.market(t, betting.MarketDuration, 3)

	betR3W := place(t, a, 100, m.leg(t, betting.MarketRoundWinner, 3, betting.OutcomeP2))
	betR3D := place(t, a, 70, m.leg(t, betting.MarketDuration, 3, betting.OutcomeOver))
	betScore := place(t, a, 50, m.leg(t, betting.MarketScore, 0, "p1_2_0"))
	betScoreMiss := place(t, a, 50, m.leg(t, betting.MarketScore, 0, "p1_2_1"))

	if n := startMatch(t, m); n != 8 {
		t.Fatalf("關掉 %d 個盤口,應為 8", n)
	}
	finishBestOf3(t, m, []int64{m.p1, m.p1}, []int64{120, 60})
	out, err := svc.SettleMatch(context.Background(), m.pub)
	if err != nil {
		t.Fatalf("結算: %v", err)
	}
	if out.MarketsSettled != 6 || out.MarketsVoided != 2 || out.LegsVoid != 2 || out.LegsWon != 1 || out.LegsLost != 1 {
		t.Fatalf("結算結果不符: %+v", out)
	}
	if marketStatus(t, r3w.ID) != "void" || marketStatus(t, r3d.ID) != "void" {
		t.Fatal("第三回合的盤口應為 void")
	}
	if n := countRows(t, `SELECT count(*) FROM activity.markets WHERE match_id = $1 AND status = 'settled'`, m.id); n != 6 {
		t.Fatalf("settled 的盤口 %d 個,應為 6", n)
	}

	for _, r := range []struct {
		res   *betting.PlaceBetResult
		stake int64
		mk    betting.Market
	}{{betR3W, 100, r3w}, {betR3D, 70, r3d}} {
		b := readBet(t, r.res.Bet.PublicID)
		if b.Status != "void" {
			t.Fatalf("押第三回合的注單狀態 = %s,應為 void(不是 lost)", b.Status)
		}
		if b.RefundEntryID == nil {
			t.Fatal("沒有退款分錄")
		}
		amount, reason, _, refID := entryOf(t, *b.RefundEntryID)
		if amount != r.stake || reason != "bet_refund" || refID == nil || *refID != b.ID {
			t.Fatalf("退款分錄 amount=%d reason=%s ref=%v,應為 +%d bet_refund 指回注單", amount, reason, refID, r.stake)
		}
		if b.PayoutRecalculated {
			t.Fatal("單場全退沒有重算金額,payout_recalculated 不該是 true")
		}
		if legResult(t, b.ID, r.mk.ID) != "void" {
			t.Fatal("腿應為 void")
		}
	}
	hit, miss := readBet(t, betScore.Bet.PublicID), readBet(t, betScoreMiss.Bet.PublicID)
	if hit.Status != "won" || hit.PayoutEntryID == nil || hit.PotentialPayout != 184 { // 50 × 3.68
		t.Fatalf("押 2:0 的注單 = %+v,應為 won 派 184", hit)
	}
	if miss.Status != "lost" || miss.PayoutEntryID != nil || miss.RefundEntryID != nil {
		t.Fatalf("押 2:1 的注單 = %+v,應為 lost 且無金流", miss)
	}
	// 1000 − 270 本金 + 170 退款 + 184 派彩。
	if bal := balanceOf(t, a.user); bal != 1000-270+170+184 {
		t.Fatalf("餘額 = %d,應為 %d", bal, 1000-270+170+184)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.token_entries WHERE user_id = $1 AND reason = 'bet_refund'`, a.user); n != 2 {
		t.Fatalf("退款分錄數 = %d,應為 2", n)
	}
	assertReconciled(t, a.user)

	// 重跑:所有盤口都已離開 open/closed,什麼都不做、不重複退款。
	again, err := svc.SettleMatch(context.Background(), m.pub)
	if err != nil || len(again.Bets) != 0 || again.MarketsSettled != 0 || again.MarketsVoided != 0 {
		t.Fatalf("重跑 = (%+v,%v),應為空", again, err)
	}
	assertReconciled(t, a.user)
}

// TestScoreMarketSettlesBothSides:比分盤 2:1 兩邊各驗一次,押中的一格贏、其他三格輸。
func TestScoreMarketSettlesBothSides(t *testing.T) {
	setup(t)
	cases := []struct {
		name string
		play func(m mrec) []int64 // 逐回合勝者
		hit  betting.Outcome
	}{
		{"p1 2:1", func(m mrec) []int64 { return []int64{m.p1, m.p2, m.p1} }, "p1_2_1"},
		{"p2 2:1", func(m mrec) []int64 { return []int64{m.p2, m.p1, m.p2} }, "p2_2_1"},
		{"p2 2:0", func(m mrec) []int64 { return []int64{m.p2, m.p2} }, "p2_2_0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newFullArena(t, 1000)
			m := a.addMatch(t)
			all := []betting.Outcome{"p1_2_0", "p1_2_1", "p2_2_1", "p2_2_0"}
			bets := map[betting.Outcome]*betting.PlaceBetResult{}
			for _, o := range all {
				bets[o] = place(t, a, 100, m.leg(t, betting.MarketScore, 0, o))
			}
			startMatch(t, m)
			winners := c.play(m)
			finishBestOf3(t, m, winners, []int64{100, 100, 100}[:len(winners)])
			if _, err := svc.SettleMatch(context.Background(), m.pub); err != nil {
				t.Fatalf("結算: %v", err)
			}
			for _, o := range all {
				b := readBet(t, bets[o].Bet.PublicID)
				want := "lost"
				if o == c.hit {
					want = "won"
				}
				if b.Status != want {
					t.Fatalf("押 %s 的注單狀態 = %s,應為 %s", o, b.Status, want)
				}
			}
			if bal := balanceOf(t, a.user); bal != 1000-400+368 {
				t.Fatalf("餘額 = %d,應為 %d", bal, 1000-400+368)
			}
			assertReconciled(t, a.user)
		})
	}
}

// TestSettleRoundSettlesOnlyThatRound:逐回合結算只動該回合的兩個盤口;
// duration **等於 line 算 over**,差一毫秒就是 under。
func TestSettleRoundSettlesOnlyThatRound(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	r1w, r1d := m.market(t, betting.MarketRoundWinner, 1), m.market(t, betting.MarketDuration, 1)
	r2w := m.market(t, betting.MarketRoundWinner, 2)

	over := place(t, a, 100, m.leg(t, betting.MarketDuration, 1, betting.OutcomeOver))
	under := place(t, a, 100, m.leg(t, betting.MarketDuration, 1, betting.OutcomeUnder))
	r1 := place(t, a, 100, m.leg(t, betting.MarketRoundWinner, 1, betting.OutcomeP2))
	r2 := place(t, a, 100, m.leg(t, betting.MarketRoundWinner, 2, betting.OutcomeP2))
	mw := place(t, a, 100, m.winner(t, betting.OutcomeP2))
	startMatch(t, m)

	// 回合還沒結束就結算:錯,而且什麼都不動。
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO activity.match_rounds (match_id, round_no) VALUES ($1, 1)`, m.id); err != nil {
		t.Fatalf("開第一回合: %v", err)
	}
	if _, err := svc.SettleRound(context.Background(), m.pub, 1); !errors.Is(err, betting.ErrRoundNotFinished) {
		t.Fatalf("回合未結束應回 ErrRoundNotFinished,實際 %v", err)
	}
	if marketStatus(t, r1w.ID) != "closed" {
		t.Fatal("失敗的結算不該動到盤口")
	}

	// 第一回合:p2 贏,時長**剛好 90 秒** → over。
	if _, err := pool.Exec(context.Background(),
		`UPDATE activity.match_rounds
		    SET winner_player_id = $2, finished_at = started_at + interval '90 seconds'
		  WHERE match_id = $1 AND round_no = 1`, m.id, m.p2); err != nil {
		t.Fatalf("結束第一回合: %v", err)
	}
	out, err := svc.SettleRound(context.Background(), m.pub, 1)
	if err != nil {
		t.Fatalf("結算第一回合: %v", err)
	}
	if out.MarketsSettled != 2 || out.MarketsVoided != 0 || out.LegsWon != 2 || out.LegsLost != 1 {
		t.Fatalf("結算結果不符: %+v", out)
	}
	if marketStatus(t, r1w.ID) != "settled" || marketStatus(t, r1d.ID) != "settled" {
		t.Fatal("第一回合的兩個盤口應為 settled")
	}
	if marketStatus(t, r2w.ID) != "closed" {
		t.Fatal("第二回合的盤口不該被動到")
	}
	want := map[string]string{
		over.Bet.PublicID: "won", under.Bet.PublicID: "lost", r1.Bet.PublicID: "won",
		r2.Bet.PublicID: "open", mw.Bet.PublicID: "open",
	}
	for pub, st := range want {
		if b := readBet(t, pub); b.Status != st {
			t.Fatalf("注單 %s 狀態 = %s,應為 %s", pub, b.Status, st)
		}
	}
	if bal := balanceOf(t, a.user); bal != 1000-500+184*2 {
		t.Fatalf("餘額 = %d,應為 %d", bal, 1000-500+184*2)
	}
	assertReconciled(t, a.user)

	// 重跑同一回合:兩個盤口已結算,無事。
	again, err := svc.SettleRound(context.Background(), m.pub, 1)
	if err != nil || len(again.Bets) != 0 || again.MarketsSettled != 0 {
		t.Fatalf("重跑 = (%+v,%v),應為空", again, err)
	}

	// 第二回合 p2 贏、89.999 秒 → under;然後整場 2:0 由 p2 拿下。
	// 整場結算要補結第二回合(裁判沒逐回合呼叫)、作廢第三回合、結算整場與比分。
	playRoundExact(t, m, 2, m.p2, 90*time.Second-time.Millisecond)
	finishMatch(t, m, m.p2)
	fin, err := svc.SettleMatch(context.Background(), m.pub)
	if err != nil {
		t.Fatalf("整場結算: %v", err)
	}
	if fin.MarketsSettled != 4 || fin.MarketsVoided != 2 {
		t.Fatalf("整場結算 settled=%d void=%d,應為 4/2", fin.MarketsSettled, fin.MarketsVoided)
	}
	if b := readBet(t, r2.Bet.PublicID); b.Status != "won" {
		t.Fatalf("第二回合注單狀態 = %s,應為 won", b.Status)
	}
	if b := readBet(t, mw.Bet.PublicID); b.Status != "won" {
		t.Fatalf("整場注單狀態 = %s,應為 won", b.Status)
	}
	// 沒有人押第二回合的時長盤,但盤口仍要 settled;第三回合兩個盤口 void。
	if marketStatus(t, m.market(t, betting.MarketDuration, 2).ID) != "settled" {
		t.Fatal("第二回合時長盤應為 settled")
	}
	if marketStatus(t, m.market(t, betting.MarketDuration, 3).ID) != "void" {
		t.Fatal("第三回合時長盤應為 void")
	}
	assertReconciled(t, a.user)
}

// TestDurationJustUnderLineIsUnder:89.999 秒押 over 是輸的 —— 邊界只在 ≥ 90.000。
func TestDurationJustUnderLineIsUnder(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	over := place(t, a, 100, m.leg(t, betting.MarketDuration, 1, betting.OutcomeOver))
	startMatch(t, m)
	playRoundExact(t, m, 1, m.p1, 90*time.Second-time.Millisecond)
	if _, err := svc.SettleRound(context.Background(), m.pub, 1); err != nil {
		t.Fatalf("結算: %v", err)
	}
	if b := readBet(t, over.Bet.PublicID); b.Status != "lost" {
		t.Fatalf("89.999 秒押 over 狀態 = %s,應為 lost", b.Status)
	}
}

// TestAdvisoryLockBlocksCrossSession:同一場的結算鎖真的跨 session 互斥。
//
// 用 pg_try_advisory_xact_lock 從另一條連線驗,而不是靠 sleep 觀察有沒有卡住 ——
// try 版本立刻回 true/false,結果是確定的,不會因為機器快慢而偶發。
func TestAdvisoryLockBlocksCrossSession(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m := a.addMatch(t)
	ctx := context.Background()

	tryLock := func(matchID int64) bool {
		var ok bool
		err := pool.QueryRow(ctx,
			`SELECT pg_try_advisory_xact_lock(hashtext('match_settle'), ($1::bigint)::int)`, matchID,
		).Scan(&ok)
		if err != nil {
			t.Fatalf("試取鎖: %v", err)
		}
		return ok
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 tx: %v", err)
	}
	if err := repo.LockMatchSettle(ctx, tx, m.id); err != nil {
		t.Fatalf("取結算鎖: %v", err)
	}
	// 另一條連線(pool.QueryRow 自己拿一條)應該拿不到同一場的鎖。
	if tryLock(m.id) {
		t.Fatal("第二個 session 也拿到了同一場的結算鎖 —— 同場可被重複結算")
	}
	// 不同場互不干擾:namespace 與 match_id 各佔一半。
	m2 := a.addMatch(t)
	if !tryLock(m2.id) {
		t.Fatal("不同場的結算被互相擋住了")
	}

	// 鎖隨 tx 結束自動釋放,不需要也不該手動解鎖。
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !tryLock(m.id) {
		t.Fatal("tx 結束後鎖沒有釋放")
	}
}

// TestConcurrentSettleSameMatchPaysOnce:四個 goroutine 同時結算同一場(四種盤口都有注單),
// 只能派彩一次、盤口只能結算一次。第二個拿到鎖時盤口已不在未結算清單裡,自然什麼都不做。
func TestConcurrentSettleSameMatchPaysOnce(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	res := place(t, a, 100, m.winner(t, betting.OutcomeP1))
	sc := place(t, a, 100, m.leg(t, betting.MarketScore, 0, "p1_2_0"))
	startMatch(t, m)
	finishBestOf3(t, m, []int64{m.p1, m.p1}, []int64{100, 100})

	const n = 4
	outs := make([]*betting.SettleResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], errs[i] = svc.SettleMatch(context.Background(), m.pub)
		}(i)
	}
	close(start)
	wg.Wait()

	settled := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 次結算失敗: %v", i, errs[i])
		}
		if len(outs[i].Bets) > 0 || outs[i].MarketsSettled > 0 {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("有 %d 次結算真的動了東西,應為 1", settled)
	}

	b, s := readBet(t, res.Bet.PublicID), readBet(t, sc.Bet.PublicID)
	if n := countRows(t,
		`SELECT count(*) FROM platform.token_entries
		  WHERE user_id = $1 AND reason = 'bet_payout'`, a.user); n != 2 {
		t.Fatalf("派彩分錄數 = %d,應為 2(兩張注單各一次)", n)
	}
	if bal := balanceOf(t, a.user); bal != 800+b.PotentialPayout+s.PotentialPayout {
		t.Fatalf("餘額 = %d,應為 %d", bal, 800+b.PotentialPayout+s.PotentialPayout)
	}
	assertReconciled(t, a.user)
}

// TestConcurrentSettleSharedParlayNoDeadlock:兩場比賽同時結算、共用同一批串關注單。
//
// advisory lock 各鎖各的場,擋不住彼此;真正擋住的是 activity.bets 的列鎖,
// 而它必須依 id 升冪取 —— 順序被打亂就是死鎖(40P01)。
// 兩張注單各押兩場(不同盤口種類),讓兩個結算 tx 必然爭同一批列。
func TestConcurrentSettleSharedParlayNoDeadlock(t *testing.T) {
	setup(t)
	a := newFullArena(t, 10000)
	u2 := newUser(t)
	credit(t, u2, 10000)
	b2 := arena{tournamentID: a.tournamentID, slug: a.slug, user: u2}

	mA := a.addMatch(t)
	mB := a.addMatch(t)

	// 兩人各下一張同樣的串關(兩張注單的 id 一前一後):A 押整場、B 押比分。
	betX := place(t, a, 100, mA.winner(t, betting.OutcomeP1), mB.leg(t, betting.MarketScore, 0, "p1_2_0"))
	betY := place(t, b2, 100, mA.winner(t, betting.OutcomeP1), mB.leg(t, betting.MarketScore, 0, "p1_2_0"))

	startMatch(t, mA)
	startMatch(t, mB)
	finishBestOf3(t, mA, []int64{mA.p1, mA.p1}, []int64{100, 100})
	finishBestOf3(t, mB, []int64{mB.p1, mB.p1}, []int64{100, 100})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, pub := range []string{mA.pub, mB.pub} {
		wg.Add(1)
		go func(i int, pub string) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.SettleMatch(context.Background(), pub)
		}(i, pub)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("第 %d 場結算死鎖(取鎖順序被打亂): %v", i, err)
			}
			t.Fatalf("第 %d 場結算失敗: %v", i, err)
		}
	}

	for _, r := range []*betting.PlaceBetResult{betX, betY} {
		b := readBet(t, r.Bet.PublicID)
		if b.Status != "won" {
			t.Fatalf("注單 %s 狀態 = %s,兩場都贏了應為 won(卡在 open 代表兩邊都沒看到對方標好的腿)",
				r.Bet.PublicID, b.Status)
		}
		if b.PayoutEntryID == nil {
			t.Fatalf("注單 %s 沒有派彩分錄", r.Bet.PublicID)
		}
		if b.PayoutRecalculated {
			t.Fatalf("注單 %s 沒有腿作廢卻標了 payout_recalculated", r.Bet.PublicID)
		}
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.token_entries
		  WHERE user_id = ANY($1::bigint[]) AND reason = 'bet_payout'`,
		[]int64{a.user, u2}); n != 2 {
		t.Fatalf("派彩分錄數 = %d,應為 2(一張注單一次)", n)
	}
	assertReconciled(t, a.user, u2)
}

// TestLockBetsAcquiresInAscendingOrder:實際下去的取鎖順序真的是 id 升冪。
//
// 光看 SQL 的 ORDER BY 不夠 —— 要證明的是「LockRows 節點在 Sort 之上」這件事在
// 這個版本的 planner 上成立,鎖是照輸出順序一列一列取的。
//
// 做法:先由別人鎖住 id 大的那張,再叫 LockBets([小, 大])。
// 升冪的話它會**先拿到小的**再卡在大的;降冪的話它會立刻卡在大的,小的永遠拿不到。
// 所以「小的變成被鎖住」就是升冪的證據。
func TestLockBetsAcquiresInAscendingOrder(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m1 := a.addMatch(t)
	m2 := a.addMatch(t)
	betA := readBet(t, place(t, a, 100, m1.winner(t, betting.OutcomeP1)).Bet.PublicID)
	betB := readBet(t, place(t, a, 100, m2.winner(t, betting.OutcomeP1)).Bet.PublicID)

	lo, hi := betA.ID, betB.ID
	if lo > hi {
		lo, hi = hi, lo
	}
	ctx := context.Background()

	// 別人先鎖住 id 大的那張。
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 blocker tx: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	var tmp int64
	if err := blocker.QueryRow(ctx,
		`SELECT id FROM activity.bets WHERE id = $1 FOR UPDATE`, hi).Scan(&tmp); err != nil {
		t.Fatalf("blocker 取鎖: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			// 呼叫端給的順序就是升冪(core 的 betIDsOf 已排序)。
			return repo.LockBets(ctx, tx, []int64{lo, hi})
		})
	}()

	// 等到「小的那張被鎖住」為止。降冪取鎖的話這裡會等到逾時。
	locked := false
	for i := 0; i < 60 && !locked; i++ {
		if tryLockBet(t, lo) == "55P03" {
			locked = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("LockBets 不該在 blocker 放手前就結束: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !locked {
		t.Fatal("LockBets 卡在大的那張、小的一直沒被鎖住 —— 取鎖順序不是 id 升冪")
	}
	// 而且它確實還在等大的那張(還沒回來)。
	select {
	case err := <-done:
		t.Fatalf("LockBets 竟然沒被 blocker 擋住: %v", err)
	default:
	}

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("放開 blocker: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("LockBets: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocker 放手後 LockBets 仍未完成")
	}
}

// TestIdempotencyKeyReuseConflicts:同一把鍵被拿去送不同內容的請求 → 409,不是默默下另一張單。
func TestIdempotencyKeyReuseConflicts(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m := a.addMatch(t)
	key := newULID(t)
	ctx := context.Background()

	mk := func(stake int64) betting.PlaceBetParams {
		return betting.PlaceBetParams{
			UserID: a.user, TournamentSlug: a.slug, Stake: stake,
			Legs:           []betting.LegInput{m.winner(t, betting.OutcomeP1)},
			IdempotencyKey: key,
		}
	}
	if _, err := svc.PlaceBet(ctx, mk(100)); err != nil {
		t.Fatalf("第一次下注: %v", err)
	}
	if _, err := svc.PlaceBet(ctx, mk(200)); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("同鍵不同內容應回 409,實際 %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.bets WHERE user_id = $1`, a.user); n != 1 {
		t.Fatalf("注單數 = %d,應為 1", n)
	}
	if bal := balanceOf(t, a.user); bal != 900 {
		t.Fatalf("餘額 = %d,只該扣 100", bal)
	}
	assertReconciled(t, a.user)
}

// TestSettleMarketTwiceFails:盤口只能結算一次 —— 第二次 SettleMarket 影響 0 列,adapter 出聲。
func TestSettleMarketTwiceFails(t *testing.T) {
	setup(t)
	a := newArena(t, 0)
	m := a.addMatch(t)
	mk := m.markets[0]
	ctx := context.Background()
	if err := repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return repo.SettleMarket(ctx, tx, mk.ID, betting.MarketSettled)
	}); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	err := repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return repo.SettleMarket(ctx, tx, mk.ID, betting.MarketVoid)
	})
	if !errors.Is(err, betting.ErrMarketSettled) {
		t.Fatalf("第二次應回 ErrMarketSettled,實際 %v", err)
	}
	if marketStatus(t, mk.ID) != "settled" {
		t.Fatal("第二次不該把 settled 改成 void")
	}
	// 不合法的目標狀態在 Go 這側就擋。
	err = repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return repo.SettleMarket(ctx, tx, mk.ID, betting.MarketOpen)
	})
	if !errors.Is(err, betting.ErrInvalidRequest) {
		t.Fatalf("標回 open 應回 ErrInvalidRequest,實際 %v", err)
	}
}

// ── 棄賽的四個分支 ────────────────────────────────────────────────

// TestVoidMatchSingleLegRefunds:單場注單碰上棄賽 → 全額退,退款是新的正數分錄;
// 該場**所有**盤口 void。
func TestVoidMatchSingleLegRefunds(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m := a.addMatch(t)
	res := place(t, a, 100, m.leg(t, betting.MarketDuration, 2, betting.OutcomeUnder))

	walkoverMatch(t, m)
	out, err := svc.VoidMatch(context.Background(), m.pub)
	if err != nil {
		t.Fatalf("棄賽退款: %v", err)
	}
	if out.LegsVoid != 1 || len(out.Bets) != 1 || out.MarketsVoided != 8 || out.MarketsSettled != 0 {
		t.Fatalf("結果不符: %+v", out)
	}

	b := readBet(t, res.Bet.PublicID)
	if b.Status != "void" {
		t.Fatalf("狀態 = %s,應為 void", b.Status)
	}
	if b.RefundEntryID == nil {
		t.Fatal("ledger_refund_entry_id 沒寫進去")
	}
	amount, reason, _, refID := entryOf(t, *b.RefundEntryID)
	if amount != 100 || reason != "bet_refund" {
		t.Fatalf("退款分錄 amount=%d reason=%s,應為 +100 / bet_refund", amount, reason)
	}
	if refID == nil || *refID != b.ID {
		t.Fatalf("退款分錄沒指回注單: %v", refID)
	}
	if bal := balanceOf(t, a.user); bal != 1000 {
		t.Fatalf("餘額 = %d,應回到 1000", bal)
	}
	if n := countRows(t, `SELECT count(*) FROM activity.markets WHERE match_id = $1 AND status = 'void'`, m.id); n != 8 {
		t.Fatalf("void 的盤口 %d 個,應為 8", n)
	}
	assertReconciled(t, a.user)
}

// TestVoidMatchParlayRemainingWon:串關的一腿棄賽、剩餘全贏
// → 依剩餘腿各自鎖定的賠率重算派彩,payout_recalculated = true。
func TestVoidMatchParlayRemainingWon(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	mA := a.addMatch(t)
	mB := a.addMatch(t)

	res := place(t, a, 100, mA.winner(t, betting.OutcomeP1), mB.winner(t, betting.OutcomeP1))
	original := readBet(t, res.Bet.PublicID)

	// 先把 A 打完(贏),再讓 B 棄賽。
	playRound(t, mA, 1, mA.p1, 100)
	finishMatch(t, mA, mA.p1)
	if _, err := svc.SettleMatch(context.Background(), mA.pub); err != nil {
		t.Fatalf("結算 A: %v", err)
	}
	if b := readBet(t, res.Bet.PublicID); b.Status != "open" {
		t.Fatalf("A 贏之後還有 B 沒打,狀態應維持 open,實際 %s", b.Status)
	}

	walkoverMatch(t, mB)
	if _, err := svc.VoidMatch(context.Background(), mB.pub); err != nil {
		t.Fatalf("B 棄賽: %v", err)
	}

	b := readBet(t, res.Bet.PublicID)
	if b.Status != "won" {
		t.Fatalf("狀態 = %s,剩餘腿全贏應為 won", b.Status)
	}
	if !b.PayoutRecalculated {
		t.Fatal("payout_recalculated 應為 true —— 對帳要看得出金額為什麼跟原始承諾不同")
	}
	if b.PotentialPayout >= original.PotentialPayout {
		t.Fatalf("移掉一腿後賠付 %d 應小於原始的 %d", b.PotentialPayout, original.PotentialPayout)
	}
	// 重算是把 void 的腿從乘積裡拿掉,剩餘腿的賠率一個都沒改。
	var oddsA int64
	if err := pool.QueryRow(context.Background(),
		`SELECT odds_milli FROM activity.bet_legs WHERE bet_id = $1 AND match_id = $2`,
		b.ID, mA.id).Scan(&oddsA); err != nil {
		t.Fatalf("讀腿賠率: %v", err)
	}
	if want := betting.ParlayPayout(100, []int64{oddsA}, betting.DefaultMaxParlayMilli); b.PotentialPayout != want {
		t.Fatalf("重算賠付 = %d,應為 %d(只剩 A 那一腿)", b.PotentialPayout, want)
	}
	if b.PayoutEntryID == nil {
		t.Fatal("沒有派彩分錄")
	}
	amount, reason, _, _ := entryOf(t, *b.PayoutEntryID)
	if amount != b.PotentialPayout || reason != "bet_payout" {
		t.Fatalf("派彩分錄 amount=%d reason=%s", amount, reason)
	}
	assertReconciled(t, a.user)
}

// TestVoidMatchParlayRemainingPending:剩餘腿還沒打 → 維持 open、不動錢,
// 但 potential_payout 已經重算過(旗標要立起來)。
func TestVoidMatchParlayRemainingPending(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	mA := a.addMatch(t)
	mB := a.addMatch(t)

	res := place(t, a, 100, mA.winner(t, betting.OutcomeP1), mB.winner(t, betting.OutcomeP1))
	original := readBet(t, res.Bet.PublicID)

	walkoverMatch(t, mB)
	if _, err := svc.VoidMatch(context.Background(), mB.pub); err != nil {
		t.Fatalf("B 棄賽: %v", err)
	}

	b := readBet(t, res.Bet.PublicID)
	if b.Status != "open" {
		t.Fatalf("狀態 = %s,A 還沒打應維持 open", b.Status)
	}
	if b.SettledAt != nil {
		t.Fatal("還在 open 就寫了 settled_at(bets_settled_at_check 的反面)")
	}
	if !b.PayoutRecalculated || b.PotentialPayout >= original.PotentialPayout {
		t.Fatalf("賠付應已重算變小: recalculated=%v payout=%d → %d",
			b.PayoutRecalculated, original.PotentialPayout, b.PotentialPayout)
	}
	if b.PayoutEntryID != nil || b.RefundEntryID != nil {
		t.Fatal("維持 open 不該有任何金流")
	}
	if bal := balanceOf(t, a.user); bal != 900 {
		t.Fatalf("餘額 = %d,應仍是 900", bal)
	}
	assertReconciled(t, a.user)
}

// TestVoidMatchParlayAlreadyLost:剩餘腿已經輸了 → 棄賽不救,不退不派。
func TestVoidMatchParlayAlreadyLost(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	mA := a.addMatch(t)
	mB := a.addMatch(t)

	res := place(t, a, 100, mA.winner(t, betting.OutcomeP1), mB.winner(t, betting.OutcomeP1))

	playRound(t, mA, 1, mA.p2, 100)
	finishMatch(t, mA, mA.p2) // 押 P1,結果 P2 贏 → 這張串關已經輸了
	if _, err := svc.SettleMatch(context.Background(), mA.pub); err != nil {
		t.Fatalf("結算 A: %v", err)
	}
	if b := readBet(t, res.Bet.PublicID); b.Status != "lost" {
		t.Fatalf("一腿輸掉就該直接 lost,實際 %s", b.Status)
	}

	walkoverMatch(t, mB)
	if _, err := svc.VoidMatch(context.Background(), mB.pub); err != nil {
		t.Fatalf("B 棄賽: %v", err)
	}

	b := readBet(t, res.Bet.PublicID)
	if b.Status != "lost" {
		t.Fatalf("狀態 = %s,已輸的注單不該被棄賽救回", b.Status)
	}
	if b.RefundEntryID != nil || b.PayoutEntryID != nil {
		t.Fatalf("已輸的注單不該有金流: refund=%v payout=%v", b.RefundEntryID, b.PayoutEntryID)
	}
	if bal := balanceOf(t, a.user); bal != 900 {
		t.Fatalf("餘額 = %d,應仍是 900", bal)
	}
	// 該腿仍要標成 void(事實要記下來),只是不改變注單的結局。
	if r := legResult(t, b.ID, mB.markets[0].ID); r != "void" {
		t.Fatalf("棄賽那一腿 result = %s,應為 void", r)
	}
	assertReconciled(t, a.user)
}

// TestVoidMatchAllLegsVoid:每一腿都棄賽 → void,全額退一次(不是一腿退一次)。
func TestVoidMatchAllLegsVoid(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	mA := a.addMatch(t)
	mB := a.addMatch(t)

	res := place(t, a, 100, mA.winner(t, betting.OutcomeP1), mB.winner(t, betting.OutcomeP1))

	walkoverMatch(t, mA)
	if _, err := svc.VoidMatch(context.Background(), mA.pub); err != nil {
		t.Fatalf("A 棄賽: %v", err)
	}
	walkoverMatch(t, mB)
	if _, err := svc.VoidMatch(context.Background(), mB.pub); err != nil {
		t.Fatalf("B 棄賽: %v", err)
	}

	b := readBet(t, res.Bet.PublicID)
	if b.Status != "void" {
		t.Fatalf("狀態 = %s,全部腿作廢應為 void", b.Status)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.token_entries
		  WHERE user_id = $1 AND reason = 'bet_refund'`, a.user); n != 1 {
		t.Fatalf("退款分錄數 = %d,應為 1", n)
	}
	if bal := balanceOf(t, a.user); bal != 1000 {
		t.Fatalf("餘額 = %d,應回到 1000", bal)
	}
	assertReconciled(t, a.user)
}

// TestSettleWalkoverRejected:不戰而勝不能走結算路徑(賠率是按真的打一場算的)。
func TestSettleWalkoverRejected(t *testing.T) {
	setup(t)
	a := newArena(t, 1000)
	m := a.addMatch(t)
	place(t, a, 100, m.winner(t, betting.OutcomeP1))
	walkoverMatch(t, m)

	if _, err := svc.SettleMatch(context.Background(), m.pub); !errors.Is(err, betting.ErrWalkoverMatch) {
		t.Fatalf("應回 ErrWalkoverMatch,實際 %v", err)
	}
	if _, err := svc.SettleRound(context.Background(), m.pub, 1); !errors.Is(err, betting.ErrWalkoverMatch) {
		t.Fatalf("逐回合也應回 ErrWalkoverMatch,實際 %v", err)
	}
	if _, err := svc.VoidMatch(context.Background(), m.pub); err != nil {
		t.Fatalf("退款路徑應可用: %v", err)
	}
}

// ── 查詢 ──────────────────────────────────────────────────────────

// TestListMyBetsOpenOnly:openOnly 分派到兩支不同的 query(部分索引 bets_open_idx),
// 兩支都要回對的東西,而且都要帶齊腿與盤口的展示欄位。
func TestListMyBetsOpenOnly(t *testing.T) {
	setup(t)
	a := newFullArena(t, 1000)
	m1 := a.addMatch(t)
	m2 := a.addMatch(t)

	done := place(t, a, 100, m1.winner(t, betting.OutcomeP1))
	// 串關:m2 還沒打(pending)+ m1 押 2:0(等下會贏)→ 結算 m1 之後仍是 open。
	open := place(t, a, 100, m2.leg(t, betting.MarketDuration, 2, betting.OutcomeOver), m1.leg(t, betting.MarketScore, 0, "p1_2_0"))

	startMatch(t, m1)
	finishBestOf3(t, m1, []int64{m1.p1, m1.p1}, []int64{100, 100})
	if _, err := svc.SettleMatch(context.Background(), m1.pub); err != nil {
		t.Fatalf("結算: %v", err)
	}

	ctx := context.Background()
	all, err := svc.ListMyBets(ctx, betting.ListMyBetsParams{
		UserID: a.user, TournamentSlug: a.slug,
	})
	if err != nil {
		t.Fatalf("列全部: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("全部注單數 = %d,應為 2", len(all))
	}
	// 新到舊:第二張在前。
	if all[0].PublicID != open.Bet.PublicID {
		t.Fatalf("排序不對,第一張應是最新的 %s,實際 %s", open.Bet.PublicID, all[0].PublicID)
	}
	if len(all[0].Legs) != 2 || len(all[1].Legs) != 1 {
		t.Fatalf("腿沒帶齊: %d / %d", len(all[0].Legs), len(all[1].Legs))
	}
	// 展示欄位要 join 得出來(對外只出現 public_id),顯示名由 core 依 kind 組。
	for _, l := range all[0].Legs {
		switch l.MarketKind {
		case betting.MarketDuration:
			if l.MatchPublicID != m2.pub || l.MarketRoundNo != 2 || l.LineSeconds != 90 || l.OutcomeLabel != "超過 90 秒" {
				t.Fatalf("時長腿的展示欄位不對: %+v", l)
			}
		case betting.MarketScore:
			if l.MatchPublicID != m1.pub || l.OutcomeLabel != "2:0" || l.Result != betting.LegWon {
				t.Fatalf("比分腿的展示欄位不對: %+v", l)
			}
		default:
			t.Fatalf("多出來的腿: %+v", l)
		}
		if l.MarketPublicID == "" || l.MarketID == 0 {
			t.Fatalf("腿缺盤口識別: %+v", l)
		}
	}
	if l := all[1].Legs[0]; l.OutcomeLabel == "" || l.MarketKind != betting.MarketMatchWinner {
		t.Fatalf("勝負腿的展示欄位不對: %+v", l)
	}
	if all[1].PublicID != done.Bet.PublicID || all[1].Status != betting.BetWon {
		t.Fatalf("已結算的注單狀態不對: %+v", all[1])
	}
	if all[1].LedgerStakeEntryID == 0 || all[1].LedgerPayoutEntryID == 0 {
		t.Fatalf("稽核鏈欄位沒讀出來: %+v", all[1])
	}

	onlyOpen, err := svc.ListMyBets(ctx, betting.ListMyBetsParams{
		UserID: a.user, TournamentSlug: a.slug, OpenOnly: true,
	})
	if err != nil {
		t.Fatalf("列未結算: %v", err)
	}
	if len(onlyOpen) != 1 || onlyOpen[0].PublicID != open.Bet.PublicID {
		t.Fatalf("openOnly 結果不對: %+v", onlyOpen)
	}
	if len(onlyOpen[0].Legs) != 2 {
		t.Fatalf("openOnly 也要帶腿,實際 %d 條", len(onlyOpen[0].Legs))
	}
}

// TestRulesReadsConfigAndMaxStake 釘住規則的兩個來源:
// 逐屆旋鈕來自 tournaments.config(rules.Parse),max_stake 來自 platform.economy_configs(JSONB)。
//
// economy_configs.value 是 **JSONB** 不是 TEXT。取裸值用 #>> '{}' 而不是 ::text:
// seed 的 500 是 JSON 數字,但裁判在後台手打成 "500" JSON 字串也完全可能,
// ::text 會把引號一起帶出來、落回預設值 —— 而預設值剛好也是 500,所以這個錯
// 永遠不會有人發現。用 777 才驗得出差別。
func TestRulesReadsConfigAndMaxStake(t *testing.T) {
	setup(t)
	ctx := context.Background()
	a := newFullArena(t, 0)

	const note = "測試用 max_stake"
	clear := func() {
		if _, err := pool.Exec(ctx,
			`DELETE FROM platform.economy_configs WHERE key = 'max_stake' AND note = $1`, note); err != nil {
			t.Fatalf("清掉測試用 max_stake: %v", err)
		}
	}
	// 一定要還原:這張表是全平台共用的,留下去會讓後面每一個下注測試的上限都不一樣。
	t.Cleanup(clear)
	setMaxStake := func(raw string) {
		t.Helper()
		clear() // 同 key 靠 effective_at 取最新一筆,先刪掉舊的測試列才不會撞 UNIQUE
		if _, err := pool.Exec(ctx,
			`INSERT INTO platform.economy_configs (key, value, note)
			 VALUES ('max_stake', $1::jsonb, $2)`, raw, note); err != nil {
			t.Fatalf("寫入測試用 max_stake %s: %v", raw, err)
		}
	}
	read := func() betting.Rules {
		t.Helper()
		var r betting.Rules
		if err := repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = repo.Rules(ctx, tx, a.tournamentID)
			return err
		}); err != nil {
			t.Fatalf("讀規則: %v", err)
		}
		return r
	}

	// migration 00014 seed 的那一筆(JSON 數字)。
	if got := read().MaxStake; got != 500 {
		t.Fatalf("seed 的 max_stake 應讀成 500,得到 %d", got)
	}
	for _, raw := range []string{`777`, `"777"`} {
		setMaxStake(raw)
		if got := read().MaxStake; got != 777 {
			t.Fatalf("max_stake = %s 應讀成 777,得到 %d", raw, got)
		}
	}
	// 逐屆旋鈕:config 是 fullConfig → best_of 3、四種盤口、line 90;odds 缺鍵 → rules 的預設。
	r := read()
	if r.Config.Format.BestOf != 3 || len(r.Config.Betting.Markets) != 4 || r.Config.Betting.Markets[2].LineSeconds != 90 {
		t.Fatalf("config 沒讀對: %+v", r.Config)
	}
	if r.Config.Betting.Odds.VigBPS != betting.DefaultVigBps || r.Odds().MaxStake != 777 {
		t.Fatalf("賠率參數 = %+v / MaxStake %d", r.Config.Betting.Odds, r.Odds().MaxStake)
	}
	// 壞掉的 config 不會讓下注整個死掉:退回預設。
	if _, err := pool.Exec(ctx, `UPDATE activity.tournaments SET config = '{"format": {"best_of": "three"}}' WHERE id = $1`, a.tournamentID); err != nil {
		t.Fatalf("寫壞 config: %v", err)
	}
	if got := read().Config.Format.BestOf; got != 1 {
		t.Fatalf("壞 config 的 best_of 應退回預設 1,得到 %d", got)
	}
}

// ── ApplyInTx 的三個性質(betting 對帳本的全部假設)────────────────

// TestApplyInTxContract 實測 ledgerpg.Service.ApplyInTx 註解宣稱的三點。
// 下注流程的寫法(動錢排在最後、失敗立刻收尾)完全建立在這三點上,
// 所以在**真資料庫**上驗一次,而不是相信註解。
func TestApplyInTxContract(t *testing.T) {
	setup(t)
	ctx := context.Background()

	t.Run("呼叫端 rollback 動錢一併消失", func(t *testing.T) {
		u := newUser(t)
		credit(t, u, 500)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("開 tx: %v", err)
		}
		key := newULID(t)
		if _, err := led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key, RequestHash: key,
			Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -100, Reason: ledger.ReasonBetStake}},
		}); err != nil {
			t.Fatalf("動錢: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if bal := balanceOf(t, u); bal != 500 {
			t.Fatalf("rollback 後餘額 = %d,應為 500", bal)
		}
		if n := countRows(t,
			`SELECT count(*) FROM platform.token_entries WHERE user_id = $1 AND reason = 'bet_stake'`,
			u); n != 0 {
			t.Fatalf("rollback 後仍有 %d 筆扣款分錄", n)
		}
		if n := countRows(t, `SELECT count(*) FROM platform.idempotency_keys WHERE key = $1`, key); n != 0 {
			t.Fatalf("rollback 後仍有 %d 把冪等鍵", n)
		}
	})

	t.Run("失敗用 savepoint 隔離不毒化呼叫端的 tx", func(t *testing.T) {
		u := newUser(t)
		credit(t, u, 50)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("開 tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		key := newULID(t)
		_, err = led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key, RequestHash: key,
			Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -100, Reason: ledger.ReasonBetStake}},
		})
		if !errors.Is(err, ledger.ErrInsufficientBalance) {
			t.Fatalf("應回餘額不足,實際 %v", err)
		}
		// tx 仍然可用:再下一句不該撞 "current transaction is aborted"。
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("savepoint 失敗後 tx 被毒化了: %v", err)
		}
		// 而且還能成功動一筆小額的錢,然後 commit。
		key2 := newULID(t)
		if _, err := led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key2, RequestHash: key2,
			Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -10, Reason: ledger.ReasonBetStake}},
		}); err != nil {
			t.Fatalf("失敗後再動錢: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if bal := balanceOf(t, u); bal != 40 {
			t.Fatalf("餘額 = %d,應為 40", bal)
		}
		assertReconciled(t, u)
	})

	// 第三點:ledgerpg 的註解說「savepoint rollback **不釋放**已取得的 row lock」。
	//
	// 實測結果相反 —— PostgreSQL 的列鎖不是 heavyweight lock,而是寫在 tuple 的
	// xmax 上;子交易 abort 會把那個 subxid 標成 aborted 並從 proc array 移除,
	// 於是別的 session 再也看不到那把鎖。註解描述的行為在這個版本上不成立。
	//
	// 這不改變 betting 的寫法:「動錢排在流程最後」本來就還有另外兩個更硬的理由
	// (所有會拒絕這張單的檢查都該在碰錢之前做完;失敗即 rollback 整個 tx),
	// 而且此時 tx 仍握著 platform.users 的列鎖(那是在 savepoint 外面取的)。
	// 但既然鎖真的放掉了,就把實際行為釘在測試裡,避免下一個人照著錯的註解做決定。
	t.Run("savepoint rollback 會釋放餘額列鎖(與註解相反)", func(t *testing.T) {
		u := newUser(t)
		credit(t, u, 50) // 先讓餘額列存在,否則它是在 savepoint 裡才被建出來的
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("開 tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// 對照組:先證明 NOWAIT 這個偵測手段真的有效 ——
		// 同一列由外層 tx 直接鎖住時,另一條連線必須撞 55P03。
		var held int64
		if err := tx.QueryRow(ctx,
			`SELECT balance FROM platform.user_balances
			  WHERE user_id = $1 AND currency = 'coin' FOR UPDATE`, u).Scan(&held); err != nil {
			t.Fatalf("外層 tx 取列鎖: %v", err)
		}
		if code := tryLockBalance(t, u); code != "55P03" {
			t.Fatalf("對照組失敗:外層 tx 持鎖時 NOWAIT 應回 55P03,實際 %q", code)
		}
		// 放掉對照組的鎖(整個 tx 重來,才不會蓋住待測的行為)。
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback 對照組: %v", err)
		}
		tx, err = pool.Begin(ctx)
		if err != nil {
			t.Fatalf("重開 tx: %v", err)
		}

		key := newULID(t)
		_, err = led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key, RequestHash: key,
			Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -100, Reason: ledger.ReasonBetStake}},
		})
		if !errors.Is(err, ledger.ErrInsufficientBalance) {
			t.Fatalf("應回餘額不足,實際 %v", err)
		}
		if code := tryLockBalance(t, u); code != "" {
			t.Fatalf("savepoint rollback 之後列鎖仍被持有(%s)—— 行為與這個測試的前提不同,"+
				"請重讀 ledgerpg.ApplyInTx 的註解再決定 betting 的流程順序", code)
		}
	})
}
