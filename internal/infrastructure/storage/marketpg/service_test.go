package marketpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/market"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/marketpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義,
// 要測的正是資料庫的鎖行為與約束)。共用一個容器,各測試用獨立使用者/物品隔離。

var (
	pool *pgxpool.Pool
	led  *ledgerpg.Service
	svc  *marketpg.Service
	seq  atomic.Int64 // 冪等鍵去重用
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
		svc = marketpg.New(pool, led)
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

func key(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.Add(1))
}

// ── 測試資料 ──

// 防洗點門檻的真實 seed 值(migration 00022)。測試**不改這些設定**——
// 要驗的正是真實設定下的行為;該由測試造出來的是「前提資料」
// (帳號年齡、入群時間),不是把門檻調成 0 來繞過。
const (
	seedMinMemberDays     = 3    // trade_min_member_days
	seedMinAccountAgeDays = 7    // trade_min_account_age_days
	seedDailyVolumeCap    = 5000 // trade_daily_volume_cap
)

// newRookie 建一個全新帳號:created_at = now()、沒有任何入群紀錄。
// 這種帳號**不符合**交易門檻,只用於不涉及交易的場景與門檻測試的反例。
func newRookie(t *testing.T) int64 {
	t.Helper()
	return newUserAged(t, 0)
}

// newTrader 建一個有資格交易的使用者:帳號 30 天、入群 30 天(遠高於門檻)。
// 交易路徑的測試一律用它——真實使用者本來就得先在社群待一段時間才能交易。
func newTrader(t *testing.T) int64 {
	t.Helper()
	u := newUserAged(t, 30)
	joinSpace(t, u, 30)
	return u
}

// newUserAged 建立一個「帳號年齡 = ageDays 天」的使用者(不含入群紀錄)。
func newUserAged(t *testing.T, ageDays int) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id, created_at)
		 VALUES (gen_random_uuid()::text, now() - make_interval(days => $1)) RETURNING id`, ageDays,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

func credit(t *testing.T, userID, amount int64) {
	t.Helper()
	k := key("credit")
	if _, err := led.Apply(context.Background(), ledger.ApplyParams{
		IdempotencyKey: k, RequestHash: k,
		Ops: []ledger.Op{{UserID: userID, Currency: "coin", Amount: amount, Reason: ledger.ReasonAdminGrant}},
	}); err != nil {
		t.Fatalf("入帳: %v", err)
	}
}

func balance(t *testing.T, userID int64) int64 {
	t.Helper()
	b, err := led.Balance(context.Background(), userID, "coin")
	if err != nil {
		t.Fatalf("讀餘額: %v", err)
	}
	return b
}

// newDef 建一個物品定義。maxSupply nil = 不限量。
func newDef(t *testing.T, category string, tradable, bindOnAcquire bool, maxSupply *int32) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.item_definitions (public_id, name, category, tradable, bind_on_acquire, max_supply)
		 VALUES (gen_random_uuid()::text, '測試物品', $1, $2, $3, $4) RETURNING id`,
		category, tradable, bindOnAcquire, maxSupply,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建物品定義: %v", err)
	}
	return id
}

// tradableItem 是最常見的組合:可交易的收藏品,鑄一件給 owner。
func tradableItem(t *testing.T, owner int64) *market.MintResult {
	t.Helper()
	def := newDef(t, string(market.CategoryCollectible), true, false, nil)
	res, err := svc.MintItem(context.Background(), market.MintParams{
		DefinitionID: def, OwnerID: owner,
		AcquiredVia: market.AcquiredViaAdmin, IdempotencyKey: key("mint"),
	})
	if err != nil {
		t.Fatalf("發放物品: %v", err)
	}
	return res
}

func listing(t *testing.T, seller int64, itemPublicID string, price int64) *market.Listing {
	t.Helper()
	l, err := svc.CreateListing(context.Background(), market.CreateListingParams{
		SellerID: seller, ItemInstancePublicID: itemPublicID, Currency: "coin", Price: price,
	})
	if err != nil {
		t.Fatalf("掛單: %v", err)
	}
	return l
}

// setConfig 寫入一筆 economy_configs(調整 = 插新列),測試結束刪掉,不影響其他測試。
func setConfig(t *testing.T, k string, v int64) {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO platform.economy_configs (key, value, note) VALUES ($1, $2::jsonb, 'test') RETURNING id`,
		k, strconv.FormatInt(v, 10),
	).Scan(&id)
	if err != nil {
		t.Fatalf("寫設定 %s: %v", k, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM platform.economy_configs WHERE id = $1`, id)
	})
}

func restrict(t *testing.T, userID int64, kind string) {
	t.Helper()
	admin := newRookie(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_restrictions (user_id, kind, reason, created_by)
		 VALUES ($1, $2, '測試', $3)`, userID, kind, admin)
	if err != nil {
		t.Fatalf("寫限制: %v", err)
	}
}

// 全套測試共用一個社群 + 一個 Discord 空間(M1 就是這個形狀:單一社群共用 XP 池)。
// 每個使用者一列 space_members,入群時間各自回填。
var (
	spaceOnce                    sync.Once
	testCommunityID, testSpaceID int64
	spaceErr                     error
)

func testSpace(t *testing.T) int64 {
	t.Helper()
	spaceOnce.Do(func() {
		ctx := context.Background()
		if err := pool.QueryRow(ctx,
			`INSERT INTO platform.communities (public_id, name)
			 VALUES (gen_random_uuid()::text, '測試社群') RETURNING id`,
		).Scan(&testCommunityID); err != nil {
			spaceErr = fmt.Errorf("建社群: %w", err)
			return
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO platform.community_spaces (public_id, community_id, provider, external_id)
			 VALUES (gen_random_uuid()::text, $1, 'discord', gen_random_uuid()::text) RETURNING id`,
			testCommunityID,
		).Scan(&testSpaceID); err != nil {
			spaceErr = fmt.Errorf("建空間: %w", err)
		}
	})
	if spaceErr != nil {
		t.Fatalf("%v", spaceErr)
	}
	return testSpaceID
}

// joinSpace 讓 userID 在 daysAgo 天前加入共用測試空間(入群天數 = MIN(joined_at) 起算)。
func joinSpace(t *testing.T, userID int64, daysAgo int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO platform.space_members (space_id, user_id, joined_at)
		 VALUES ($1, $2, now() - make_interval(days => $3))`, testSpace(t), userID, daysAgo); err != nil {
		t.Fatalf("加入空間: %v", err)
	}
}

// setLevel 寫使用者在共用社群的等級(M1 的 level 恆為 0,只有等級門檻測試會用到)。
func setLevel(t *testing.T, userID int64, level int32) {
	t.Helper()
	testSpace(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_xp (user_id, community_id, xp, level) VALUES ($1, $2, 0, $3)
		 ON CONFLICT (user_id, community_id) DO UPDATE SET level = EXCLUDED.level`,
		userID, testCommunityID, level); err != nil {
		t.Fatalf("寫等級: %v", err)
	}
}

// ── 斷言工具 ──

type entry struct {
	UserID int64
	Amount int64
	Reason string
}

// entriesOf 讀一張成交單的全部分錄(三筆分錄都以 ref 指向 market_orders)。
func entriesOf(t *testing.T, orderID int64) []entry {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT user_id, amount, reason FROM platform.token_entries
		 WHERE ref_type = 'market_order' AND ref_id = $1 ORDER BY id`, orderID)
	if err != nil {
		t.Fatalf("讀分錄: %v", err)
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.UserID, &e.Amount, &e.Reason); err != nil {
			t.Fatalf("掃分錄: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀分錄: %v", err)
	}
	return out
}

// totalSupply 是全體流通量:SUM(所有 coin 分錄)。手續費銷毀時它會下降 fee。
func totalSupply(t *testing.T) int64 {
	t.Helper()
	var v int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM platform.token_entries WHERE currency = 'coin'`,
	).Scan(&v); err != nil {
		t.Fatalf("讀流通量: %v", err)
	}
	return v
}

func itemState(t *testing.T, itemPublicID string) (ownerID int64, lockedBy *int64, bound bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT owner_id, locked_by_trade_id, bound FROM platform.item_instances WHERE public_id = $1`,
		itemPublicID).Scan(&ownerID, &lockedBy, &bound); err != nil {
		t.Fatalf("讀物品狀態: %v", err)
	}
	return
}

func listingStatus(t *testing.T, listingPublicID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM platform.market_listings WHERE public_id = $1`, listingPublicID).Scan(&s); err != nil {
		t.Fatalf("讀掛單狀態: %v", err)
	}
	return s
}

func countRows(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("計數: %v", err)
	}
	return n
}

// assertReconciled 全域對帳:SUM(entries) 必須等於 user_balances(帳本鐵則第二條)。
func assertReconciled(t *testing.T) {
	t.Helper()
	mm, err := led.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("對帳: %v", err)
	}
	if len(mm) != 0 {
		t.Fatalf("帳不平:%+v", mm)
	}
}

// ══ 成交主流程 ══

// 成交:三筆分錄金額正確、餘額與帳本一致、物品換手、掛單 sold、成交單落地、outbox 一筆。
func TestBuySettlesEverythingInOneTx(t *testing.T) {
	setup(t)
	ctx := context.Background()
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 5000)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 1000)

	before := totalSupply(t)
	res, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
	})
	if err != nil {
		t.Fatalf("成交: %v", err)
	}

	if res.Price != 1000 || res.Fee != 80 { // 800 bps = 8%
		t.Fatalf("預期 price=1000 fee=80,得到 price=%d fee=%d", res.Price, res.Fee)
	}
	if got := balance(t, buyer); got != 4000 {
		t.Fatalf("買方餘額應為 4000(付了 price),得到 %d", got)
	}
	if got := balance(t, seller); got != 920 {
		t.Fatalf("賣方餘額應為 920(price − fee),得到 %d", got)
	}
	if delta := totalSupply(t) - before; delta != -80 {
		t.Fatalf("流通量應恰好減少 fee=80(銷毀),實際變動 %d", delta)
	}

	es := entriesOf(t, res.OrderID)
	if len(es) != 3 {
		t.Fatalf("預期三筆分錄,得到 %d 筆:%+v", len(es), es)
	}
	want := []entry{
		{buyer, -1000, string(ledger.ReasonMarketPurchase)},
		{seller, 1000, string(ledger.ReasonMarketSaleIncome)},
		{seller, -80, string(ledger.ReasonMarketFee)},
	}
	for i, w := range want {
		if es[i] != w {
			t.Fatalf("分錄[%d] 預期 %+v,得到 %+v", i, w, es[i])
		}
	}
	assertReconciled(t)

	owner, lockedBy, _ := itemState(t, item.InstancePublicID)
	if owner != buyer {
		t.Fatalf("物品應換手給買方 %d,實際 owner=%d", buyer, owner)
	}
	if lockedBy != nil {
		t.Fatalf("成交後應清鎖,實際 locked_by_trade_id=%d", *lockedBy)
	}
	if s := listingStatus(t, l.PublicID); s != string(market.ListingSold) {
		t.Fatalf("掛單狀態應為 sold,得到 %s", s)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.market_orders WHERE id = $1 AND buyer_id = $2 AND seller_id = $3
		   AND price = 1000 AND fee = 80`, res.OrderID, buyer, seller); n != 1 {
		t.Fatalf("成交紀錄應恰一筆且金額相符,得到 %d", n)
	}
	if n := countRows(t,
		`SELECT count(*) FROM platform.outbox_events
		 WHERE topic = 'market.sold' AND (payload->>'order_id')::bigint = $1`, res.OrderID); n != 1 {
		t.Fatalf("outbox 應恰一筆 market.sold,得到 %d", n)
	}
	// 物品取得脈絡更新為 market(稽核用)
	if n := countRows(t,
		`SELECT count(*) FROM platform.item_instances WHERE public_id = $1 AND acquired_via = 'market'`,
		item.InstancePublicID); n != 1 {
		t.Fatalf("換手後 acquired_via 應為 market")
	}
}

// 手續費整數運算:無條件捨去;捨去後為 0 時不寫手續費分錄(帳本禁止 0 金額)。
func TestFeeRoundingTruncatesDown(t *testing.T) {
	setup(t)
	ctx := context.Background()

	// 105 × 800 / 10000 = 8.4 → 8(對賣家有利的方向)
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 1000)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 105)
	res, err := svc.Buy(ctx, market.BuyParams{BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy")})
	if err != nil {
		t.Fatalf("成交: %v", err)
	}
	if res.Fee != 8 {
		t.Fatalf("105 的 8%% 應捨去為 8,得到 %d", res.Fee)
	}
	if got := balance(t, seller); got != 97 {
		t.Fatalf("賣方應實收 97,得到 %d", got)
	}

	// price=1 → fee=0 → 只有兩筆分錄,流通量不變
	buyer2, seller2 := newTrader(t), newTrader(t)
	credit(t, buyer2, 10)
	item2 := tradableItem(t, seller2)
	l2 := listing(t, seller2, item2.InstancePublicID, 1)
	before := totalSupply(t)
	res2, err := svc.Buy(ctx, market.BuyParams{BuyerID: buyer2, ListingPublicID: l2.PublicID, IdempotencyKey: key("buy")})
	if err != nil {
		t.Fatalf("成交: %v", err)
	}
	if res2.Fee != 0 || res2.FeeEntryID != 0 {
		t.Fatalf("fee 捨去後為 0 時不應寫分錄,得到 fee=%d entry=%d", res2.Fee, res2.FeeEntryID)
	}
	if es := entriesOf(t, res2.OrderID); len(es) != 2 {
		t.Fatalf("fee=0 時應只有兩筆分錄,得到 %d", len(es))
	}
	if delta := totalSupply(t) - before; delta != 0 {
		t.Fatalf("fee=0 時流通量不該變,變動 %d", delta)
	}
	assertReconciled(t)
}

// ══ 併發 ══

// 併發同一掛單兩個買家:恰一成功,物品只換手一次,錢只動一次。
func TestConcurrentBuyersSameListing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	seller := newTrader(t)
	b1, b2 := newTrader(t), newTrader(t)
	credit(t, b1, 5000)
	credit(t, b2, 5000)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 1000)

	type outcome struct {
		buyer int64
		res   *market.Purchase
		err   error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	for i, b := range []int64{b1, b2} {
		wg.Add(1)
		go func(i int, b int64) {
			defer wg.Done()
			res, err := svc.Buy(ctx, market.BuyParams{
				BuyerID: b, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
			})
			results[i] = outcome{b, res, err}
		}(i, b)
	}
	wg.Wait()

	var winner int64
	ok := 0
	for _, r := range results {
		if r.err == nil {
			ok++
			winner = r.buyer
			continue
		}
		if !errors.Is(r.err, market.ErrListingNotOpen) {
			t.Fatalf("敗者應收到 ErrListingNotOpen,得到 %v", r.err)
		}
	}
	if ok != 1 {
		t.Fatalf("應恰一人成交,實際成功 %d 人", ok)
	}

	owner, lockedBy, _ := itemState(t, item.InstancePublicID)
	if owner != winner {
		t.Fatalf("物品應屬於成交者 %d,實際 %d", winner, owner)
	}
	if lockedBy != nil {
		t.Fatal("成交後鎖應清空")
	}
	if got := balance(t, seller); got != 920 {
		t.Fatalf("賣方只該收一次 920,得到 %d", got)
	}
	loser := b1 + b2 - winner
	if got := balance(t, loser); got != 5000 {
		t.Fatalf("敗者不該被扣錢,餘額 %d", got)
	}
	if n := countRows(t, `SELECT count(*) FROM platform.market_orders WHERE listing_id =
		(SELECT id FROM platform.market_listings WHERE public_id = $1)`, l.PublicID); n != 1 {
		t.Fatalf("成交紀錄應恰一筆,得到 %d", n)
	}
	assertReconciled(t)
}

// 併發互買(A 買 B 的掛單、B 買 A 的掛單同時發生):鎖序固定 → 不死鎖,兩筆都成交。
func TestConcurrentCrossBuyNoDeadlock(t *testing.T) {
	setup(t)
	ctx := context.Background()
	a, b := newTrader(t), newTrader(t)
	credit(t, a, 5000)
	credit(t, b, 5000)
	itemA := tradableItem(t, a)
	itemB := tradableItem(t, b)
	listA := listing(t, a, itemA.InstancePublicID, 1000)
	listB := listing(t, b, itemB.InstancePublicID, 1000)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = svc.Buy(ctx, market.BuyParams{BuyerID: a, ListingPublicID: listB.PublicID, IdempotencyKey: key("buy")})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = svc.Buy(ctx, market.BuyParams{BuyerID: b, ListingPublicID: listA.PublicID, IdempotencyKey: key("buy")})
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("互買[%d] 不該失敗(尤其不該死鎖): %v", i, err)
		}
	}
	if ownerA, _, _ := itemState(t, itemA.InstancePublicID); ownerA != b {
		t.Fatalf("A 的物品應換手給 B")
	}
	if ownerB, _, _ := itemState(t, itemB.InstancePublicID); ownerB != a {
		t.Fatalf("B 的物品應換手給 A")
	}
	// 各付 1000、各收 920:5000 − 1000 + 920 = 4920
	if got := balance(t, a); got != 4920 {
		t.Fatalf("A 餘額應為 4920,得到 %d", got)
	}
	if got := balance(t, b); got != 4920 {
		t.Fatalf("B 餘額應為 4920,得到 %d", got)
	}
	assertReconciled(t)
}

// 同 IdempotencyKey 重送(循序 + 併發):錢只動一次,回同一張成交單。
func TestBuyIdempotent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 5000)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 1000)
	k := key("buy")

	first, err := svc.Buy(ctx, market.BuyParams{BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: k})
	if err != nil {
		t.Fatalf("第一次成交: %v", err)
	}
	second, err := svc.Buy(ctx, market.BuyParams{BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: k})
	if err != nil {
		t.Fatalf("重送: %v", err)
	}
	if !second.Replayed {
		t.Fatal("重送應標記 Replayed")
	}
	if second.OrderID != first.OrderID {
		t.Fatalf("重送應回同一張成交單,%d != %d", second.OrderID, first.OrderID)
	}
	if got := balance(t, buyer); got != 4000 {
		t.Fatalf("重送後買方餘額應仍為 4000,得到 %d", got)
	}
	if n := countRows(t, `SELECT count(*) FROM platform.market_orders WHERE id = $1`, first.OrderID); n != 1 {
		t.Fatal("成交單不該重複")
	}

	// 併發同 key:另一張掛單,8 個 goroutine 打同一個鍵
	buyer2, seller2 := newTrader(t), newTrader(t)
	credit(t, buyer2, 5000)
	item2 := tradableItem(t, seller2)
	l2 := listing(t, seller2, item2.InstancePublicID, 1000)
	k2 := key("buy")

	const n = 8
	res := make([]*market.Purchase, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = svc.Buy(ctx, market.BuyParams{
				BuyerID: buyer2, ListingPublicID: l2.PublicID, IdempotencyKey: k2,
			})
		}(i)
	}
	wg.Wait()

	fresh := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("併發重送[%d] 不該失敗: %v", i, errs[i])
		}
		if !res[i].Replayed {
			fresh++
		}
		if res[i].OrderID != res[0].OrderID {
			t.Fatal("併發重送應全部回同一張成交單")
		}
	}
	if fresh != 1 {
		t.Fatalf("應恰一次真正成交,實際 %d 次", fresh)
	}
	if got := balance(t, buyer2); got != 4000 {
		t.Fatalf("併發重送後買方餘額應為 4000,得到 %d", got)
	}
	assertReconciled(t)
}

// ══ 掛單規則 ══

// 掛單時物品被鎖:同一件物品不能同時掛兩張單(併發也不行)。
func TestListingLocksItem(t *testing.T) {
	setup(t)
	ctx := context.Background()
	seller := newTrader(t)
	item := tradableItem(t, seller)

	listing(t, seller, item.InstancePublicID, 500)
	if _, lockedBy, _ := itemState(t, item.InstancePublicID); lockedBy == nil {
		t.Fatal("掛單後物品應被鎖住")
	}
	_, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: seller, ItemInstancePublicID: item.InstancePublicID, Currency: "coin", Price: 600,
	})
	if !errors.Is(err, market.ErrItemLocked) {
		t.Fatalf("第二張單應回 ErrItemLocked,得到 %v", err)
	}

	// 併發掛單:恰一張成立
	seller2 := newTrader(t)
	item2 := tradableItem(t, seller2)
	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.CreateListing(ctx, market.CreateListingParams{
				SellerID: seller2, ItemInstancePublicID: item2.InstancePublicID, Currency: "coin", Price: 700,
			})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, market.ErrItemLocked):
		default:
			t.Fatalf("併發掛單的敗者應回 ErrItemLocked,得到 %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("併發掛單應恰一張成立,實際 %d", ok)
	}
	if got := countRows(t, `SELECT count(*) FROM platform.market_listings
		WHERE item_instance_id = (SELECT id FROM platform.item_instances WHERE public_id = $1)`,
		item2.InstancePublicID); got != 1 {
		t.Fatalf("同一物品只該有一張掛單,得到 %d", got)
	}
}

// 掛單的四道檢查:非本人物品、綁定物品、不可交易分類、幣別不存在。
func TestCreateListingRejections(t *testing.T) {
	setup(t)
	ctx := context.Background()
	seller, other := newTrader(t), newTrader(t)

	item := tradableItem(t, seller)
	if _, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: other, ItemInstancePublicID: item.InstancePublicID, Currency: "coin", Price: 100,
	}); !errors.Is(err, market.ErrNotItemOwner) {
		t.Fatalf("非本人物品應回 ErrNotItemOwner,得到 %v", err)
	}

	// 綁定物品(bind_on_acquire)
	boundDef := newDef(t, string(market.CategoryCollectible), true, true, nil)
	bound, err := svc.MintItem(ctx, market.MintParams{
		DefinitionID: boundDef, OwnerID: seller, AcquiredVia: market.AcquiredViaTournament, IdempotencyKey: key("mint"),
	})
	if err != nil {
		t.Fatalf("發放綁定物品: %v", err)
	}
	if !bound.Bound {
		t.Fatal("bind_on_acquire 的定義,實例應出生即綁定")
	}
	if _, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: seller, ItemInstancePublicID: bound.InstancePublicID, Currency: "coin", Price: 100,
	}); !errors.Is(err, market.ErrItemBound) {
		t.Fatalf("綁定物品應回 ErrItemBound,得到 %v", err)
	}

	// 不可交易的分類:身分組 / 徽章 / 實體獎品(schemas/09:tradable 預設 false)
	for _, cat := range []market.Category{market.CategoryRole, market.CategoryBadge, market.CategoryPrize} {
		def := newDef(t, string(cat), false, false, nil)
		inst, err := svc.MintItem(ctx, market.MintParams{
			DefinitionID: def, OwnerID: seller, AcquiredVia: market.AcquiredViaAdmin, IdempotencyKey: key("mint"),
		})
		if err != nil {
			t.Fatalf("發放 %s: %v", cat, err)
		}
		if _, err := svc.CreateListing(ctx, market.CreateListingParams{
			SellerID: seller, ItemInstancePublicID: inst.InstancePublicID, Currency: "coin", Price: 100,
		}); !errors.Is(err, market.ErrItemNotTradable) {
			t.Fatalf("%s 應回 ErrItemNotTradable,得到 %v", cat, err)
		}
	}

	// 幣別不存在(currencies FK)
	item2 := tradableItem(t, seller)
	if _, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: seller, ItemInstancePublicID: item2.InstancePublicID, Currency: "gem", Price: 100,
	}); !errors.Is(err, market.ErrInvalidRequest) {
		t.Fatalf("不存在的幣別應回 ErrInvalidRequest,得到 %v", err)
	}
	// 失敗的掛單不得留下鎖
	if _, lockedBy, _ := itemState(t, item2.InstancePublicID); lockedBy != nil {
		t.Fatal("掛單失敗不該留下物品鎖")
	}
}

// 取消掛單:解鎖物品、狀態 cancelled、可再次掛單;非本人與重複取消都拒絕。
func TestCancelListing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	seller, other := newTrader(t), newTrader(t)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 500)

	if _, err := svc.CancelListing(ctx, market.CancelListingParams{
		ListingPublicID: l.PublicID, ByUserID: other,
	}); !errors.Is(err, market.ErrNotListingOwner) {
		t.Fatalf("非賣家取消應回 ErrNotListingOwner,得到 %v", err)
	}

	if _, err := svc.CancelListing(ctx, market.CancelListingParams{
		ListingPublicID: l.PublicID, ByUserID: seller,
	}); err != nil {
		t.Fatalf("取消掛單: %v", err)
	}
	if s := listingStatus(t, l.PublicID); s != string(market.ListingCancelled) {
		t.Fatalf("狀態應為 cancelled,得到 %s", s)
	}
	if _, lockedBy, _ := itemState(t, item.InstancePublicID); lockedBy != nil {
		t.Fatal("取消後應解鎖物品")
	}
	if _, err := svc.CancelListing(ctx, market.CancelListingParams{
		ListingPublicID: l.PublicID, ByUserID: seller,
	}); !errors.Is(err, market.ErrListingNotOpen) {
		t.Fatalf("重複取消應回 ErrListingNotOpen,得到 %v", err)
	}
	// 解鎖後可以重新掛
	l2 := listing(t, seller, item.InstancePublicID, 600)

	// 已取消的掛單不能買
	buyer := newTrader(t)
	credit(t, buyer, 5000)
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
	}); !errors.Is(err, market.ErrListingNotOpen) {
		t.Fatalf("買已取消的掛單應回 ErrListingNotOpen,得到 %v", err)
	}
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l2.PublicID, IdempotencyKey: key("buy"),
	}); err != nil {
		t.Fatalf("重新掛的單應能成交: %v", err)
	}
}

// ══ 成交拒絕路徑 ══

// 買自己的掛單一律拒絕(左手換右手是洗點的最短路徑)。
func TestBuyOwnListingRejected(t *testing.T) {
	setup(t)
	seller := newTrader(t)
	credit(t, seller, 5000)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 100)

	if _, err := svc.Buy(context.Background(), market.BuyParams{
		BuyerID: seller, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
	}); !errors.Is(err, market.ErrSelfPurchase) {
		t.Fatalf("應回 ErrSelfPurchase,得到 %v", err)
	}
	if s := listingStatus(t, l.PublicID); s != string(market.ListingOpen) {
		t.Fatalf("掛單應仍 open,得到 %s", s)
	}
}

// 餘額不足:零寫入——物品不換手、掛單仍 open、鎖仍在、沒有成交單/分錄/outbox。
// 補足餘額後同一個冪等鍵可以重試成功(失敗的 tx 不該把鍵留下來擋人)。
func TestInsufficientBalanceLeavesNoTrace(t *testing.T) {
	setup(t)
	ctx := context.Background()
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 100)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 1000)
	k := key("buy")

	_, err := svc.Buy(ctx, market.BuyParams{BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: k})
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("應回 ErrInsufficientBalance,得到 %v", err)
	}
	owner, lockedBy, _ := itemState(t, item.InstancePublicID)
	if owner != seller {
		t.Fatal("失敗的成交不該換手")
	}
	if lockedBy == nil {
		t.Fatal("失敗的成交不該解鎖物品")
	}
	if s := listingStatus(t, l.PublicID); s != string(market.ListingOpen) {
		t.Fatalf("掛單應仍 open,得到 %s", s)
	}
	if n := countRows(t, `SELECT count(*) FROM platform.market_orders WHERE listing_id =
		(SELECT id FROM platform.market_listings WHERE public_id = $1)`, l.PublicID); n != 0 {
		t.Fatal("不該留下成交單")
	}
	if n := countRows(t, `SELECT count(*) FROM platform.idempotency_keys WHERE key LIKE '%' || $1`, k); n != 0 {
		t.Fatal("rollback 後冪等鍵不該留下(否則合法重試被擋)")
	}
	if got := balance(t, buyer); got != 100 {
		t.Fatalf("買方餘額不該變動,得到 %d", got)
	}

	credit(t, buyer, 2000)
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: k,
	}); err != nil {
		t.Fatalf("補錢後同鍵重試應成功: %v", err)
	}
	assertReconciled(t)
}

// 過期掛單不能買;賣家仍可取消把物品拿回來。
func TestExpiredListingRejected(t *testing.T) {
	setup(t)
	ctx := context.Background()
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 5000)
	item := tradableItem(t, seller)
	past := time.Now().Add(-time.Minute)
	l, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: seller, ItemInstancePublicID: item.InstancePublicID,
		Currency: "coin", Price: 100, ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("掛單: %v", err)
	}
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
	}); !errors.Is(err, market.ErrListingExpired) {
		t.Fatalf("應回 ErrListingExpired,得到 %v", err)
	}
	if _, err := svc.CancelListing(ctx, market.CancelListingParams{
		ListingPublicID: l.PublicID, ByUserID: seller,
	}); err != nil {
		t.Fatalf("過期掛單應仍可取消: %v", err)
	}
	if _, lockedBy, _ := itemState(t, item.InstancePublicID); lockedBy != nil {
		t.Fatal("取消後應解鎖")
	}
}

// ══ 防洗點 ══

// no_trade:買賣雙方任一有此限制都拒絕(買方在成交、賣方在掛單與成交都擋)。
func TestNoTradeRestriction(t *testing.T) {
	setup(t)
	ctx := context.Background()

	// 買方被限制
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 5000)
	item := tradableItem(t, seller)
	l := listing(t, seller, item.InstancePublicID, 100)
	restrict(t, buyer, "no_trade")
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
	}); !errors.Is(err, market.ErrTradeRestricted) {
		t.Fatalf("買方 no_trade 應回 ErrTradeRestricted,得到 %v", err)
	}

	// 賣方在掛單後才被限制:成交也要擋
	buyer2, seller2 := newTrader(t), newTrader(t)
	credit(t, buyer2, 5000)
	item2 := tradableItem(t, seller2)
	l2 := listing(t, seller2, item2.InstancePublicID, 100)
	restrict(t, seller2, "no_trade")
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer2, ListingPublicID: l2.PublicID, IdempotencyKey: key("buy"),
	}); !errors.Is(err, market.ErrTradeRestricted) {
		t.Fatalf("賣方 no_trade 應回 ErrTradeRestricted,得到 %v", err)
	}
	// 被限制的人也不能掛新單
	item3 := tradableItem(t, seller2)
	if _, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: seller2, ItemInstancePublicID: item3.InstancePublicID, Currency: "coin", Price: 100,
	}); !errors.Is(err, market.ErrTradeRestricted) {
		t.Fatalf("no_trade 不該能掛單,得到 %v", err)
	}

	// 已解除的限制不影響交易
	if _, err := pool.Exec(ctx,
		`UPDATE platform.user_restrictions SET lifted_at = now() WHERE user_id = $1`, seller2); err != nil {
		t.Fatalf("解除限制: %v", err)
	}
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer2, ListingPublicID: l2.PublicID, IdempotencyKey: key("buy"),
	}); err != nil {
		t.Fatalf("解除限制後應可成交: %v", err)
	}
}

// 交易門檻:用 migration 00022 的**真實 seed 值**驗證(入群 3 天 / 帳號 7 天 / 等級 0),
// 測試不調整這三個設定——要驗的正是正式設定下的行為。每個案例只讓一項不達標,
// 才能證明每一項都各自擋得住(全部不達標只能證明「有東西擋了」)。
func TestTradeThresholds(t *testing.T) {
	setup(t)
	ctx := context.Background()

	// 賣家是合格老手(掛單同樣要過門檻)
	seller := newTrader(t)
	l := listing(t, seller, tradableItem(t, seller).InstancePublicID, 100)
	buy := func(buyer int64) error {
		_, err := svc.Buy(ctx, market.BuyParams{
			BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
		})
		return err
	}
	fund := func(u int64) int64 { credit(t, u, 5000); return u }

	// ① 全新帳號:帳號年齡 0 天、入群 0 天 —— 兩項都不達標
	if err := buy(fund(newRookie(t))); !errors.Is(err, market.ErrTradeThresholdNotMet) {
		t.Fatalf("全新帳號應回 ErrTradeThresholdNotMet,得到 %v", err)
	}

	// ② 只有帳號年齡不達標:帳號 6 天(< 7)、入群 5 天(>= 3)
	young := newUserAged(t, seedMinAccountAgeDays-1)
	joinSpace(t, young, seedMinAccountAgeDays-2)
	if err := buy(fund(young)); !errors.Is(err, market.ErrTradeThresholdNotMet) {
		t.Fatalf("帳號年齡 %d 天應被擋,得到 %v", seedMinAccountAgeDays-1, err)
	}

	// ③ 只有入群天數不達標:帳號 30 天、入群 2 天(< 3)
	almost := newUserAged(t, 30)
	joinSpace(t, almost, seedMinMemberDays-1)
	if err := buy(fund(almost)); !errors.Is(err, market.ErrTradeThresholdNotMet) {
		t.Fatalf("入群 %d 天應被擋,得到 %v", seedMinMemberDays-1, err)
	}

	// ④ 從未入群(帳號夠老):入群天數視為 0
	never := fund(newUserAged(t, 30))
	if err := buy(never); !errors.Is(err, market.ErrTradeThresholdNotMet) {
		t.Fatalf("從未入群應被擋,得到 %v", err)
	}

	// ⑤ 回填到剛好達標(帳號 7 天、入群 3 天)→ 放行。
	// 兩個時間都是先寫入才比對,實際差距略大於整數天,不會卡在邊界上。
	eligible := newUserAged(t, seedMinAccountAgeDays)
	joinSpace(t, eligible, seedMinMemberDays)
	if err := buy(fund(eligible)); err != nil {
		t.Fatalf("剛好達標者應能成交: %v", err)
	}

	// ⑥ 同一個人補上入群紀錄後就放行 —— 證明擋下 ④ 的確實是入群天數這一項
	joinSpace(t, never, seedMinMemberDays)
	l2 := listing(t, seller, tradableItem(t, seller).InstancePublicID, 100)
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: never, ListingPublicID: l2.PublicID, IdempotencyKey: key("buy"),
	}); err != nil {
		t.Fatalf("補上入群紀錄後應能成交: %v", err)
	}
}

// 等級門檻的機制測試。
//
// seed 值是 0(migration 00022):schemas/06 明定 M1 的 level 恆為 0,等級功能不上線,
// 門檻設正數會擋住所有人 —— 也就是說這一項在 M1 是**刻意不啟用**的。
// 但「不啟用」不等於「沒實作」,所以這裡暫時把 trade_min_level 調高來驗證機制是活的:
// 這是唯一需要動設定的門檻(M1 沒有任何真實途徑讓 level > 0),
// 其餘三項一律用真實 seed 值測(見 TestTradeThresholds)。
func TestMinLevelThreshold(t *testing.T) {
	setup(t)
	ctx := context.Background()

	seller := newTrader(t)
	buyer := newTrader(t) // 其餘門檻都達標,level 為 0
	credit(t, buyer, 5000)
	// 兩張單都在調高門檻**之前**掛好:掛單同樣要過門檻,門檻調高後賣家自己也掛不了。
	l1 := listing(t, seller, tradableItem(t, seller).InstancePublicID, 100)
	l2 := listing(t, seller, tradableItem(t, seller).InstancePublicID, 100)

	// 未調整前(seed = 0):level 0 也放行 —— 證明 M1 的設定不會誤擋人
	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l1.PublicID, IdempotencyKey: key("buy"),
	}); err != nil {
		t.Fatalf("trade_min_level=0 時 level 0 應放行: %v", err)
	}

	setConfig(t, market.ConfigMinLevel, 5)
	buyL2 := func() error {
		_, err := svc.Buy(ctx, market.BuyParams{
			BuyerID: buyer, ListingPublicID: l2.PublicID, IdempotencyKey: key("buy"),
		})
		return err
	}

	// 買方 level 0 → 擋(錯誤訊息前綴標明是哪一方,順便驗買賣雙方各自都被檢查)
	err := buyL2()
	if !errors.Is(err, market.ErrTradeThresholdNotMet) || !strings.Contains(err.Error(), "買方") {
		t.Fatalf("買方 level 0 < 門檻 5 應被擋,得到 %v", err)
	}

	// 買方升級後換賣方擋:兩邊都要過門檻
	setLevel(t, buyer, 5)
	err = buyL2()
	if !errors.Is(err, market.ErrTradeThresholdNotMet) || !strings.Contains(err.Error(), "賣方") {
		t.Fatalf("賣方 level 0 < 門檻 5 應被擋,得到 %v", err)
	}

	// 雙方都達標 → 放行
	setLevel(t, seller, 5)
	if err := buyL2(); err != nil {
		t.Fatalf("雙方都達門檻等級後應放行: %v", err)
	}
}

// 單日交易額上限:用真實 seed 值 5000 驗證邊界 —— 剛好等於上限放行、超過一單位拒絕。
// 上限對買賣雙方各自累計(當日買 + 賣合計)。
func TestDailyVolumeCap(t *testing.T) {
	setup(t)
	ctx := context.Background()
	seller, buyer := newTrader(t), newTrader(t)
	credit(t, buyer, 20000)
	sell := func(price int64) string {
		return listing(t, seller, tradableItem(t, seller).InstancePublicID, price).PublicID
	}
	buy := func(buyer int64, listingID string) error {
		_, err := svc.Buy(ctx, market.BuyParams{
			BuyerID: buyer, ListingPublicID: listingID, IdempotencyKey: key("buy"),
		})
		return err
	}

	// 3000 → 雙方當日各累計 3000
	if err := buy(buyer, sell(3000)); err != nil {
		t.Fatalf("額度內第一筆應成交: %v", err)
	}
	// 再 2000 → 恰好等於上限 5000:邊界是「超過才擋」,所以放行
	if err := buy(buyer, sell(seedDailyVolumeCap-3000)); err != nil {
		t.Fatalf("累計恰好等於上限 %d 應放行: %v", seedDailyVolumeCap, err)
	}

	// 再 1 → 5001 > 5000,超過一單位就擋
	overByOne := sell(1)
	if err := buy(buyer, overByOne); !errors.Is(err, market.ErrDailyLimitExceeded) {
		t.Fatalf("超過上限一單位應回 ErrDailyLimitExceeded,得到 %v", err)
	}
	if s := listingStatus(t, overByOne); s != string(market.ListingOpen) {
		t.Fatalf("被上限擋下的掛單應仍 open,得到 %s", s)
	}

	// 換一個當日額度全新的買家買同一張單:這次超限的是**賣方**(當日也已 5000)
	buyer2 := newTrader(t)
	credit(t, buyer2, 20000)
	if err := buy(buyer2, overByOne); !errors.Is(err, market.ErrDailyLimitExceeded) {
		t.Fatalf("賣方超過單日上限也該擋,得到 %v", err)
	}

	// 買賣雙方當日額度都還在 → 同樣價格就能成交,證明擋的是額度而非別的原因
	seller2 := newTrader(t)
	if err := buy(buyer2, listing(t, seller2, tradableItem(t, seller2).InstancePublicID, 1).PublicID); err != nil {
		t.Fatalf("雙方額度都夠時應成交: %v", err)
	}
	assertReconciled(t)
}

// ══ 發放與超發保護 ══

// max_supply=1 的定義併發鑄造 10 次:恰一成功,minted_count 不會超過上限。
func TestMintSupplyCapUnderConcurrency(t *testing.T) {
	setup(t)
	ctx := context.Background()
	one := int32(1)
	def := newDef(t, string(market.CategoryCollectible), true, false, &one)
	owner := newRookie(t)

	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.MintItem(ctx, market.MintParams{
				DefinitionID: def, OwnerID: owner,
				AcquiredVia: market.AcquiredViaTournament, IdempotencyKey: key("mint"),
			})
		}(i)
	}
	wg.Wait()

	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, market.ErrSupplyExhausted):
		default:
			t.Fatalf("鑄造[%d] 應成功或回 ErrSupplyExhausted,得到 %v", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("max_supply=1 應恰一次成功,實際 %d 次", ok)
	}
	if got := countRows(t, `SELECT count(*) FROM platform.item_instances WHERE definition_id = $1`, def); got != 1 {
		t.Fatalf("應只存在 1 件實例,得到 %d", got)
	}
	if got := countRows(t, `SELECT minted_count FROM platform.item_definitions WHERE id = $1`, def); got != 1 {
		t.Fatalf("minted_count 應為 1,得到 %d", got)
	}
	// 額度用完後再鑄一樣拒絕
	if _, err := svc.MintItem(ctx, market.MintParams{
		DefinitionID: def, OwnerID: owner, AcquiredVia: market.AcquiredViaAdmin, IdempotencyKey: key("mint"),
	}); !errors.Is(err, market.ErrSupplyExhausted) {
		t.Fatalf("額度用完應回 ErrSupplyExhausted,得到 %v", err)
	}
}

// 發放冪等:同鍵重送不會多鑄一件(併發亦然);不存在的定義回 ErrDefinitionNotFound。
func TestMintIdempotentAndLookup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	limit := int32(5)
	def := newDef(t, string(market.CategoryCollectible), true, false, &limit)
	owner := newRookie(t)
	k := key("mint")
	p := market.MintParams{
		DefinitionID: def, OwnerID: owner, AcquiredVia: market.AcquiredViaAdmin, IdempotencyKey: k,
	}

	first, err := svc.MintItem(ctx, p)
	if err != nil {
		t.Fatalf("第一次鑄造: %v", err)
	}
	const n = 5
	res := make([]*market.MintResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = svc.MintItem(ctx, p)
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("重送[%d]: %v", i, errs[i])
		}
		if !res[i].Replayed || res[i].InstanceID != first.InstanceID {
			t.Fatalf("重送應回同一件物品且標記 Replayed,得到 %+v", res[i])
		}
	}
	if got := countRows(t, `SELECT count(*) FROM platform.item_instances WHERE definition_id = $1`, def); got != 1 {
		t.Fatalf("同鍵重送不該多鑄,實際 %d 件", got)
	}
	if got := countRows(t, `SELECT minted_count FROM platform.item_definitions WHERE id = $1`, def); got != 1 {
		t.Fatalf("minted_count 應為 1,得到 %d", got)
	}

	// public_id 路徑 + 不存在的定義
	var pubID string
	if err := pool.QueryRow(ctx,
		`SELECT public_id FROM platform.item_definitions WHERE id = $1`, def).Scan(&pubID); err != nil {
		t.Fatalf("讀 public_id: %v", err)
	}
	if _, err := svc.MintItem(ctx, market.MintParams{
		DefinitionPublicID: pubID, OwnerID: owner,
		AcquiredVia: market.AcquiredViaShop, IdempotencyKey: key("mint"),
	}); err != nil {
		t.Fatalf("用 public_id 鑄造: %v", err)
	}
	if _, err := svc.MintItem(ctx, market.MintParams{
		DefinitionPublicID: "不存在", OwnerID: owner,
		AcquiredVia: market.AcquiredViaShop, IdempotencyKey: key("mint"),
	}); !errors.Is(err, market.ErrDefinitionNotFound) {
		t.Fatalf("應回 ErrDefinitionNotFound,得到 %v", err)
	}
}

// 使用者物品清單:含定義資訊與鎖狀態,成交後從賣家清單消失、出現在買家清單。
func TestListUserItems(t *testing.T) {
	setup(t)
	ctx := context.Background()
	buyer, seller := newTrader(t), newTrader(t)
	credit(t, buyer, 5000)
	item := tradableItem(t, seller)

	items, err := svc.ListUserItems(ctx, seller)
	if err != nil {
		t.Fatalf("讀清單: %v", err)
	}
	if len(items) != 1 || items[0].PublicID != item.InstancePublicID {
		t.Fatalf("賣家應有 1 件物品,得到 %+v", items)
	}
	if items[0].Category != market.CategoryCollectible || !items[0].Tradable || items[0].Locked {
		t.Fatalf("定義資訊或鎖狀態不對:%+v", items[0])
	}

	l := listing(t, seller, item.InstancePublicID, 200)
	items, _ = svc.ListUserItems(ctx, seller)
	if !items[0].Locked {
		t.Fatal("掛單後清單應顯示 locked")
	}

	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: buyer, ListingPublicID: l.PublicID, IdempotencyKey: key("buy"),
	}); err != nil {
		t.Fatalf("成交: %v", err)
	}
	if items, _ = svc.ListUserItems(ctx, seller); len(items) != 0 {
		t.Fatalf("成交後賣家清單應清空,得到 %+v", items)
	}
	items, _ = svc.ListUserItems(ctx, buyer)
	if len(items) != 1 || items[0].PublicID != item.InstancePublicID || items[0].Locked {
		t.Fatalf("買家應持有該物品且未被鎖:%+v", items)
	}
}

// 參數檢查:缺欄位、價格非正數一律 ErrInvalidRequest;不存在的掛單/物品有明確錯誤。
func TestInvalidRequests(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newTrader(t)

	cases := []struct {
		name string
		run  func() error
	}{
		{"缺冪等鍵", func() error {
			_, err := svc.Buy(ctx, market.BuyParams{BuyerID: u, ListingPublicID: "x"})
			return err
		}},
		{"價格為 0", func() error {
			_, err := svc.CreateListing(ctx, market.CreateListingParams{
				SellerID: u, ItemInstancePublicID: "x", Currency: "coin", Price: 0})
			return err
		}},
		{"價格為負", func() error {
			_, err := svc.CreateListing(ctx, market.CreateListingParams{
				SellerID: u, ItemInstancePublicID: "x", Currency: "coin", Price: -5})
			return err
		}},
		{"鑄造未指定定義", func() error {
			_, err := svc.MintItem(ctx, market.MintParams{OwnerID: u, IdempotencyKey: "k"})
			return err
		}},
		{"acquired_via 不在枚舉", func() error {
			_, err := svc.MintItem(ctx, market.MintParams{
				DefinitionID: 1, OwnerID: u, AcquiredVia: "怪管道", IdempotencyKey: "k"})
			return err
		}},
	}
	for _, c := range cases {
		if err := c.run(); !errors.Is(err, market.ErrInvalidRequest) {
			t.Errorf("%s:應回 ErrInvalidRequest,得到 %v", c.name, err)
		}
	}

	if _, err := svc.Buy(ctx, market.BuyParams{
		BuyerID: u, ListingPublicID: "沒有這張單", IdempotencyKey: key("buy"),
	}); !errors.Is(err, market.ErrListingNotFound) {
		t.Errorf("應回 ErrListingNotFound,得到 %v", err)
	}
	if _, err := svc.CreateListing(ctx, market.CreateListingParams{
		SellerID: u, ItemInstancePublicID: "沒有這件物品", Currency: "coin", Price: 10,
	}); !errors.Is(err, market.ErrItemNotFound) {
		t.Errorf("應回 ErrItemNotFound,得到 %v", err)
	}
}

// 手續費計算本身(純函式):捨去方向、邊界與非法輸入。
func TestFeeFor(t *testing.T) {
	cases := []struct {
		price, bps, want int64
		wantErr          bool
	}{
		{1000, 800, 80, false},
		{105, 800, 8, false},     // 8.4 → 8(無條件捨去)
		{1, 800, 0, false},       // 0.08 → 0
		{999, 10000, 999, false}, // 100% 是合法邊界
		{1000, 0, 0, false},
		{1000, 10001, 0, true},
		{1000, -1, 0, true},
	}
	for _, c := range cases {
		got, err := market.FeeFor(c.price, c.bps)
		if c.wantErr {
			if err == nil {
				t.Errorf("FeeFor(%d,%d) 應回錯", c.price, c.bps)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("FeeFor(%d,%d) = %d,%v;預期 %d", c.price, c.bps, got, err, c.want)
		}
	}
}
