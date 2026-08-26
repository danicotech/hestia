package ledgerpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義)。
// 共用一個容器,各測試用獨立使用者隔離。

var (
	pool *pgxpool.Pool
	svc  *ledgerpg.Service
)

// TestMain 讓整個 package 共用一個容器(每個測試各起一個要多花 ~30 秒)。
func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = ledgerpg.New(pool)
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

func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

func credit(t *testing.T, userID, amount int64, key string) *ledger.ApplyResult {
	t.Helper()
	res, err := svc.Apply(context.Background(), ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "h-" + key,
		Ops: []ledger.Op{{UserID: userID, Currency: "coin", Amount: amount, Reason: ledger.ReasonAdminGrant}},
	})
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
	return res
}

// 基本流:入帳 → 扣款 → 餘額與帳本一致
func TestCreditDebitAndConsistency(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	credit(t, u, 1000, fmt.Sprintf("t1-credit-%d", u))
	res, err := svc.Apply(ctx, ledger.ApplyParams{
		IdempotencyKey: fmt.Sprintf("t1-debit-%d", u), RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -300, Reason: ledger.ReasonShopPurchase}},
	})
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if res.Entries[0].NewBalance != 700 {
		t.Fatalf("預期餘額 700,得到 %d", res.Entries[0].NewBalance)
	}
	bal, _ := svc.Balance(ctx, u, "coin")
	if bal != 700 {
		t.Fatalf("Balance() 預期 700,得到 %d", bal)
	}
}

// 冪等:同 key 重送回原結果不重複動錢;同 key 不同內容回衝突
func TestIdempotencyReplayAndConflict(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	key := fmt.Sprintf("t2-%d", u)

	p := ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "same-hash",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 500, Reason: ledger.ReasonAdminGrant}},
	}
	first, err := svc.Apply(ctx, p)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := svc.Apply(ctx, p)
	if err != nil {
		t.Fatalf("重送: %v", err)
	}
	if !second.Replayed {
		t.Fatal("重送應標記 Replayed")
	}
	if second.Entries[0].EntryID != first.Entries[0].EntryID {
		t.Fatal("重送應回同一筆分錄")
	}
	if bal, _ := svc.Balance(ctx, u, "coin"); bal != 500 {
		t.Fatalf("重送後餘額應仍為 500,得到 %d", bal)
	}

	p.RequestHash = "different-hash"
	if _, err := svc.Apply(ctx, p); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("同 key 不同內容應回 ErrIdempotencyConflict,得到 %v", err)
	}
}

// 併發冪等:同 key 同時打 8 次,只動一次錢
func TestIdempotencyConcurrent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	key := fmt.Sprintf("t3-%d", u)
	p := ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 100, Reason: ledger.ReasonDailyClaim}},
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc.Apply(ctx, p) }(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, ledger.ErrInFlight) {
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if bal, _ := svc.Balance(ctx, u, "coin"); bal != 100 {
		t.Fatalf("併發重送後餘額應為 100(只動一次),得到 %d", bal)
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u).Scan(&count)
	if count != 1 {
		t.Fatalf("應只有 1 筆分錄,得到 %d", count)
	}
}

// 迴歸:連線池耗盡死鎖(QA 發現的高嚴重度)。
// 撞冪等鍵後必須先釋放本 tx 連線再讀重放結果;
// 否則 MaxConns=2 + 同 key 6 併發 = hold-and-wait 全體卡死。
func TestIdempotencyConcurrentSmallPool(t *testing.T) {
	setup(t)
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 2
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("小池: %v", err)
	}
	defer small.Close()
	svc2 := ledgerpg.New(small)

	u := newUser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := ledger.ApplyParams{
		IdempotencyKey: fmt.Sprintf("t8-%d", u), RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 100, Reason: ledger.ReasonDailyClaim}},
	}
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc2.Apply(ctx, p) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("goroutine %d 逾時 —— 連線池死鎖回歸", i)
		}
		if err != nil && !errors.Is(err, ledger.ErrInFlight) {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if bal, _ := svc.Balance(context.Background(), u, "coin"); bal != 100 {
		t.Fatalf("錢應只動一次,餘額 %d", bal)
	}
}

// 餘額不足:整筆失敗,無任何殘留
func TestInsufficientBalanceAtomicity(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 100, fmt.Sprintf("t4-%d", u))

	_, err := svc.Apply(ctx, ledger.ApplyParams{
		IdempotencyKey: fmt.Sprintf("t4-over-%d", u), RequestHash: "h",
		Ops: []ledger.Op{
			{UserID: u, Currency: "coin", Amount: -80, Reason: ledger.ReasonShopPurchase},
			{UserID: u, Currency: "coin", Amount: -80, Reason: ledger.ReasonShopPurchase}, // 淨 -160 > 100
		},
	})
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("應回 ErrInsufficientBalance,得到 %v", err)
	}
	// 原子性:失敗後分錄只有最初的入帳、餘額不變、冪等鍵未殘留(可合法重試)
	var entries int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u).Scan(&entries)
	if entries != 1 {
		t.Fatalf("失敗的 tx 不應留下分錄,得到 %d 筆", entries)
	}
	if bal, _ := svc.Balance(ctx, u, "coin"); bal != 100 {
		t.Fatalf("餘額應保持 100,得到 %d", bal)
	}
	var keys int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.idempotency_keys WHERE key=$1`,
		fmt.Sprintf("t4-over-%d", u)).Scan(&keys)
	if keys != 0 {
		t.Fatal("rollback 後冪等鍵不應殘留")
	}
}

// 死鎖測試:A↔B 反向轉帳高併發,鎖順序保證完成且總量守恆
func TestOpposingTransfersNoDeadlock(t *testing.T) {
	setup(t)
	ctx := context.Background()
	a, b := newUser(t), newUser(t)
	credit(t, a, 10000, fmt.Sprintf("t5-a-%d", a))
	credit(t, b, 10000, fmt.Sprintf("t5-b-%d", b))

	transfer := func(from, to int64, key string) error {
		_, err := svc.Apply(ctx, ledger.ApplyParams{
			IdempotencyKey: key, RequestHash: "h",
			Ops: []ledger.Op{
				{UserID: from, Currency: "coin", Amount: -10, Reason: ledger.ReasonTradeSettle},
				{UserID: to, Currency: "coin", Amount: 10, Reason: ledger.ReasonTradeSettle},
			},
		})
		return err
	}

	const rounds = 25
	var wg sync.WaitGroup
	errsAB := make([]error, rounds)
	errsBA := make([]error, rounds)
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); errsAB[i] = transfer(a, b, fmt.Sprintf("t5-ab-%d-%d", a, i)) }(i)
		go func(i int) { defer wg.Done(); errsBA[i] = transfer(b, a, fmt.Sprintf("t5-ba-%d-%d", b, i)) }(i)
	}
	wg.Wait()

	for i := 0; i < rounds; i++ {
		if errsAB[i] != nil {
			t.Fatalf("A→B 第 %d 次失敗: %v", i, errsAB[i])
		}
		if errsBA[i] != nil {
			t.Fatalf("B→A 第 %d 次失敗: %v", i, errsBA[i])
		}
	}
	balA, _ := svc.Balance(ctx, a, "coin")
	balB, _ := svc.Balance(ctx, b, "coin")
	if balA+balB != 20000 {
		t.Fatalf("總量不守恆: %d + %d != 20000", balA, balB)
	}
}

// 市集式三筆分錄 + outbox 同 tx
func TestMultiLegWithOutbox(t *testing.T) {
	setup(t)
	ctx := context.Background()
	buyer, seller := newUser(t), newUser(t)
	credit(t, buyer, 1000, fmt.Sprintf("t6-%d", buyer))

	fee := int64(80)
	price := int64(1000)
	_, err := svc.Apply(ctx, ledger.ApplyParams{
		IdempotencyKey: fmt.Sprintf("t6-settle-%d", buyer), RequestHash: "h",
		Ops: []ledger.Op{
			{UserID: buyer, Currency: "coin", Amount: -price, Reason: ledger.ReasonMarketPurchase},
			{UserID: seller, Currency: "coin", Amount: price - fee, Reason: ledger.ReasonMarketSaleIncome},
			// 手續費銷毀:買方付全額、賣方收 price-fee,差額即銷毀,分錄留痕在上面兩筆的差
		},
		Events: []ledger.Event{{Topic: "market.settled", Payload: []byte(`{"price":1000,"fee":80}`)}},
	})
	if err != nil {
		t.Fatalf("成交: %v", err)
	}
	balB, _ := svc.Balance(ctx, buyer, "coin")
	balS, _ := svc.Balance(ctx, seller, "coin")
	if balB != 0 || balS != 920 {
		t.Fatalf("結算後餘額錯誤: buyer=%d seller=%d", balB, balS)
	}
	var events int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events WHERE topic='market.settled'`).Scan(&events)
	if events != 1 {
		t.Fatalf("outbox 應有 1 筆 market.settled,得到 %d", events)
	}
}

// 對帳:正常狀態帳是平的;人為弄壞快取後 Reconcile 要抓得到
func TestReconcile(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 777, fmt.Sprintf("t7-%d", u))

	mis, err := svc.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(mis) != 0 {
		t.Fatalf("正常狀態應無不一致,得到 %+v", mis)
	}

	// 人為破壞快取(繞過帳本直接改 user_balances —— 僅測試允許)
	if _, err := pool.Exec(ctx,
		`UPDATE platform.user_balances SET balance = balance + 1 WHERE user_id=$1`, u); err != nil {
		t.Fatalf("弄壞快取: %v", err)
	}
	mis, _ = svc.Reconcile(ctx)
	found := false
	for _, m := range mis {
		if m.UserID == u && m.Balance == 778 && m.EntryTotal == 777 {
			found = true
		}
	}
	if !found {
		t.Fatalf("Reconcile 應抓到 user=%d 的不一致,得到 %+v", u, mis)
	}
	// 修回去,避免影響其他測試
	_, _ = pool.Exec(ctx, `UPDATE platform.user_balances SET balance = balance - 1 WHERE user_id=$1`, u)
}
