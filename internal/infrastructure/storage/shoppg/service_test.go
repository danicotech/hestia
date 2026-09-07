package shoppg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/shoppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義)。
// 共用一個容器,各測試用獨立使用者與獨立商品隔離。

var (
	pool *pgxpool.Pool
	led  *ledgerpg.Service
	svc  *shoppg.Service
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
		svc = shoppg.New(pool, led)
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

func credit(t *testing.T, userID, amount int64) {
	t.Helper()
	key := fmt.Sprintf("shoptest-credit-%d-%d", userID, amount)
	_, err := led.Apply(context.Background(), ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "h-" + key,
		Ops: []ledger.Op{{UserID: userID, Currency: "coin", Amount: amount, Reason: ledger.ReasonAdminGrant}},
	})
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
}

// itemSpec 建測試商品的參數;零值 = 上架中、無期限、不限購、預設 600 秒退款窗口。
type itemSpec struct {
	fulfillment  string
	price        int64
	durationDays *int32
	perUserLimit *int32
	refundWindow int32 // 0 也會照傳(= 不可退);要預設 600 用 -1 以外的正值自己給
	unlisted     bool  // listed_at NULL
	futureListed bool  // listed_at 在未來
	delisted     bool  // delisted_at 已過
}

func newItem(t *testing.T, sp itemSpec) (id int64, publicID string) {
	t.Helper()
	listed := "now() - interval '1 hour'"
	if sp.unlisted {
		listed = "NULL"
	} else if sp.futureListed {
		listed = "now() + interval '1 hour'"
	}
	delisted := "NULL"
	if sp.delisted {
		delisted = "now() - interval '1 minute'"
	}
	q := fmt.Sprintf(`INSERT INTO platform.shop_items
		(public_id, name, fulfillment, currency, price, duration_days, per_user_limit,
		 refund_window_seconds, listed_at, delisted_at)
		VALUES (gen_random_uuid()::text, 'test item', $1, 'coin', $2, $3, $4, $5, %s, %s)
		RETURNING id, public_id`, listed, delisted)
	err := pool.QueryRow(context.Background(), q,
		sp.fulfillment, sp.price, sp.durationDays, sp.perUserLimit, sp.refundWindow,
	).Scan(&id, &publicID)
	if err != nil {
		t.Fatalf("建測試商品: %v", err)
	}
	return id, publicID
}

func balance(t *testing.T, userID int64) int64 {
	t.Helper()
	bal, err := led.Balance(context.Background(), userID, "coin")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return bal
}

func count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// assertLedgerConsistent 驗證該使用者 SUM(entries) = balance(帳本鐵則的核心性質)。
func assertLedgerConsistent(t *testing.T, userID int64) {
	t.Helper()
	var sum, bal int64
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE((SELECT SUM(amount) FROM platform.token_entries WHERE user_id=$1 AND currency='coin'), 0),
		       COALESCE((SELECT balance FROM platform.user_balances WHERE user_id=$1 AND currency='coin'), 0)`,
		userID).Scan(&sum, &bal)
	if err != nil {
		t.Fatalf("對帳查詢: %v", err)
	}
	if sum != bal {
		t.Fatalf("SUM(entries)=%d != balance=%d", sum, bal)
	}
}

func ptr[T any](v T) *T { return &v }

// ── auto 購買 ──

// 限時 + 可退商品:扣款、entitlement 時間欄位、分錄 ref、outbox、SUM=balance
func TestAutoPurchase(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	_, pub := newItem(t, itemSpec{fulfillment: "auto_role", price: 300,
		durationDays: ptr(int32(30)), refundWindow: 600})

	res, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("ap-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if res.EntitlementID == 0 || res.RedemptionID != 0 {
		t.Fatalf("auto 購買應產生 entitlement 而非工單: %+v", res)
	}
	if bal := balance(t, u); bal != 700 {
		t.Fatalf("餘額應為 700,得到 %d", bal)
	}
	// expires_at / refundable_until 與 granted_at 同一個 now(),差值必須精確
	var dDays, dSecs int64
	if err := pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM (expires_at - granted_at))::bigint,
		       EXTRACT(EPOCH FROM (refundable_until - granted_at))::bigint
		FROM platform.entitlements WHERE id=$1`, res.EntitlementID).Scan(&dDays, &dSecs); err != nil {
		t.Fatalf("讀權益: %v", err)
	}
	if dDays != 30*86400 {
		t.Fatalf("expires_at 應為 granted_at+30 天,差 %d 秒", dDays)
	}
	if dSecs != 600 {
		t.Fatalf("refundable_until 應為 granted_at+600 秒,差 %d 秒", dSecs)
	}
	// 扣款分錄 ref 指向 entitlement(退款憑此找回)
	if n := count(t, `SELECT count(*) FROM platform.token_entries
		WHERE user_id=$1 AND reason='shop_purchase' AND amount=-300
		  AND ref_type='entitlement' AND ref_id=$2`, u, res.EntitlementID); n != 1 {
		t.Fatalf("應有 1 筆 ref 指向 entitlement 的扣款分錄,得到 %d", n)
	}
	if n := count(t, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='shop.purchased' AND payload->>'entitlement_id'=$1::text`,
		fmt.Sprint(res.EntitlementID)); n != 1 {
		t.Fatalf("應有 1 筆 shop.purchased outbox,得到 %d", n)
	}
	assertLedgerConsistent(t, u)
}

// 永久 + 不可退商品:expires_at 與 refundable_until 皆 NULL
func TestAutoPurchasePermanentNonRefundable(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 500)
	_, pub := newItem(t, itemSpec{fulfillment: "auto_perk", price: 500, refundWindow: 0})

	res, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("perm-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if res.ExpiresAt != nil || res.RefundableUntil != nil {
		t.Fatalf("永久不可退商品兩個時間都應為 nil: %+v", res)
	}
	var expiresNull, refundNull bool
	if err := pool.QueryRow(ctx, `SELECT expires_at IS NULL, refundable_until IS NULL
		FROM platform.entitlements WHERE id=$1`, res.EntitlementID).Scan(&expiresNull, &refundNull); err != nil {
		t.Fatalf("讀權益: %v", err)
	}
	if !expiresNull || !refundNull {
		t.Fatal("DB 中 expires_at / refundable_until 應為 NULL")
	}
	assertLedgerConsistent(t, u)
}

// 免費商品:產生權益但不產生任何分錄
func TestFreePurchaseNoLedgerEntry(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t) // 不入金:price=0 不該碰餘額
	_, pub := newItem(t, itemSpec{fulfillment: "auto_perk", price: 0, refundWindow: 600})

	res, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("free-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if res.EntitlementID == 0 || res.LedgerEntryID != 0 {
		t.Fatalf("免費購買應有權益、無分錄: %+v", res)
	}
	if n := count(t, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u); n != 0 {
		t.Fatalf("免費購買不應有分錄,得到 %d", n)
	}
	// 免費購買仍可在窗口內「退款」:只撤銷權益,不動帳本
	rres, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: res.EntitlementID, ByUserID: u, IdempotencyKey: fmt.Sprintf("free-rf-%d", u),
	})
	if err != nil {
		t.Fatalf("退免費購買: %v", err)
	}
	if rres.RefundEntryID != 0 || rres.Amount != 0 {
		t.Fatalf("免費退款不應動帳本: %+v", rres)
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements
		WHERE id=$1 AND revoked_at IS NOT NULL`, res.EntitlementID); n != 1 {
		t.Fatal("權益應已撤銷")
	}
}

// 免費商品同 key 重放:回原結果、不重複建權益
func TestFreePurchaseIdempotentReplay(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	itemID, pub := newItem(t, itemSpec{fulfillment: "auto_perk", price: 0, refundWindow: 600})
	p := shop.PurchaseParams{UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("fr-%d", u)}

	first, err := svc.Purchase(ctx, p)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := svc.Purchase(ctx, p)
	if err != nil {
		t.Fatalf("重送: %v", err)
	}
	if !second.Replayed || second.EntitlementID != first.EntitlementID {
		t.Fatalf("重送應回原結果並標記 Replayed: first=%+v second=%+v", first, second)
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements WHERE user_id=$1 AND item_id=$2`, u, itemID); n != 1 {
		t.Fatalf("應只有 1 筆權益,得到 %d", n)
	}
	if n := count(t, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u); n != 0 {
		t.Fatalf("免費重放不應有任何分錄,得到 %d", n)
	}
}

// 攻擊迴歸(QA 高風險):用字面 "shop.redemption:<id>:refund" 當 API 鍵搶佔
// 內部決定性鍵後,該工單的 reject / cancel 仍必須成功退款。
// 命名空間前綴讓客戶端可控字串在結構上撞不到內部鍵。
func TestInternalKeySquattingCannotBlockRefund(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, admin := newUser(t), newUser(t)
	credit(t, u, 1000)
	r1 := buyManual(t, u, 111) // 之後被 reject
	r2 := buyManual(t, u, 222) // 之後被 cancel

	// 攻擊:免費商品零成本,用兩張工單的內部退款鍵當 API 冪等鍵各買一次
	_, freePub := newItem(t, itemSpec{fulfillment: "auto_perk", price: 0})
	for _, rid := range []int64{r1.RedemptionID, r2.RedemptionID} {
		if _, err := svc.Purchase(ctx, shop.PurchaseParams{
			UserID: u, ItemPublicID: freePub,
			IdempotencyKey: fmt.Sprintf("shop.redemption:%d:refund", rid),
		}); err != nil {
			t.Fatalf("搶佔購買本身應成功(鍵已被命名空間隔離): %v", err)
		}
	}

	// reject r1:退款不得被搶佔的鍵擋住
	hres, err := svc.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: r1.RedemptionID, HandlerUserID: admin, Approve: false,
	})
	if err != nil {
		t.Fatalf("搶佔後 reject 仍應成功: %v", err)
	}
	if hres.RefundAmount != 111 {
		t.Fatalf("reject 應全額退 111,得到 %d", hres.RefundAmount)
	}
	// cancel r2:同樣不得被擋
	cres, err := svc.CancelRedemption(ctx, shop.CancelParams{
		RedemptionID: r2.RedemptionID, ByUserID: u,
	})
	if err != nil {
		t.Fatalf("搶佔後 cancel 仍應成功: %v", err)
	}
	if cres.RefundAmount != 222 {
		t.Fatalf("cancel 應全額退 222,得到 %d", cres.RefundAmount)
	}
	if bal := balance(t, u); bal != 1000 {
		t.Fatalf("兩筆押款全退後餘額應回 1000,得到 %d", bal)
	}
	assertLedgerConsistent(t, u)
}

// ── manual 購買 ──

func TestManualPurchase(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	_, pub := newItem(t, itemSpec{fulfillment: "manual", price: 400, refundWindow: 600})

	res, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("mp-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if res.RedemptionID == 0 || res.EntitlementID != 0 {
		t.Fatalf("manual 購買應產生工單而非權益: %+v", res)
	}
	if len(res.RedemptionPublicID) != 26 {
		t.Fatalf("工單 public_id 應為 26 字元 ULID,得到 %q", res.RedemptionPublicID)
	}
	if bal := balance(t, u); bal != 600 {
		t.Fatalf("押款後餘額應為 600,得到 %d", bal)
	}
	if n := count(t, `SELECT count(*) FROM platform.redemptions
		WHERE id=$1 AND status='pending'`, res.RedemptionID); n != 1 {
		t.Fatal("工單應為 pending")
	}
	if n := count(t, `SELECT count(*) FROM platform.token_entries
		WHERE user_id=$1 AND reason='redemption_hold' AND amount=-400
		  AND ref_type='redemption' AND ref_id=$2`, u, res.RedemptionID); n != 1 {
		t.Fatal("應有 1 筆 ref 指向 redemption 的押款分錄")
	}
	if n := count(t, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='redemption.created' AND payload->>'redemption_public_id'=$1`,
		res.RedemptionPublicID); n != 1 {
		t.Fatal("應有 1 筆 redemption.created outbox")
	}
	assertLedgerConsistent(t, u)
}

// ── 拒絕條件 ──

func TestPurchaseRejections(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 10000)

	cases := []struct {
		name string
		spec itemSpec
		want error
	}{
		{"未上架", itemSpec{fulfillment: "auto_perk", price: 100, unlisted: true}, shop.ErrItemNotListed},
		{"上架時間未到", itemSpec{fulfillment: "auto_perk", price: 100, futureListed: true}, shop.ErrItemNotListed},
		{"已下架", itemSpec{fulfillment: "auto_perk", price: 100, delisted: true}, shop.ErrItemNotListed},
	}
	for i, c := range cases {
		_, pub := newItem(t, c.spec)
		_, err := svc.Purchase(ctx, shop.PurchaseParams{
			UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("rej-%d-%d", u, i),
		})
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: 應回 %v,得到 %v", c.name, c.want, err)
		}
	}
	// 商品不存在
	_, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: "no-such-item", IdempotencyKey: fmt.Sprintf("rej-nf-%d", u),
	})
	if !errors.Is(err, shop.ErrItemNotFound) {
		t.Fatalf("不存在商品應回 ErrItemNotFound,得到 %v", err)
	}
	if bal := balance(t, u); bal != 10000 {
		t.Fatalf("被拒的購買不應動錢,餘額 %d", bal)
	}
}

// per_user_limit:達上限拒絕;退款撤銷後釋放額度可再買
func TestPerUserLimit(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	_, pub := newItem(t, itemSpec{fulfillment: "auto_perk", price: 100,
		perUserLimit: ptr(int32(1)), refundWindow: 600})

	first, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("lim1-%d", u),
	})
	if err != nil {
		t.Fatalf("第一次購買: %v", err)
	}
	_, err = svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("lim2-%d", u),
	})
	if !errors.Is(err, shop.ErrPerUserLimitReached) {
		t.Fatalf("達上限應回 ErrPerUserLimitReached,得到 %v", err)
	}
	if bal := balance(t, u); bal != 900 {
		t.Fatalf("被拒的第二次不應扣款,餘額 %d", bal)
	}
	// 退款撤銷 → 額度釋放 → 可再買
	if _, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: first.EntitlementID, ByUserID: u, IdempotencyKey: fmt.Sprintf("lim-rf-%d", u),
	}); err != nil {
		t.Fatalf("退款: %v", err)
	}
	if _, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("lim3-%d", u),
	}); err != nil {
		t.Fatalf("退款後應可再買: %v", err)
	}
	assertLedgerConsistent(t, u)
}

// 餘額不足:透傳 ErrInsufficientBalance 且零寫入
func TestInsufficientBalanceZeroWrites(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 100)
	itemID, pub := newItem(t, itemSpec{fulfillment: "auto_role", price: 500, refundWindow: 600})
	key := fmt.Sprintf("poor-%d", u)

	_, err := svc.Purchase(ctx, shop.PurchaseParams{UserID: u, ItemPublicID: pub, IdempotencyKey: key})
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("應透傳 ErrInsufficientBalance,得到 %v", err)
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements WHERE user_id=$1 AND item_id=$2`, u, itemID); n != 0 {
		t.Fatal("失敗的購買不應留下權益")
	}
	if n := count(t, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1 AND reason<>'admin_grant'`, u); n != 0 {
		t.Fatal("失敗的購買不應留下分錄")
	}
	if n := count(t, `SELECT count(*) FROM platform.idempotency_keys
		WHERE key='shop.purchase:api:'||$1 OR key='shop.purchase:api:'||$1||':ledger'`, key); n != 0 {
		t.Fatal("rollback 後冪等鍵不應殘留(合法重試不能被擋)")
	}
	if bal := balance(t, u); bal != 100 {
		t.Fatalf("餘額應保持 100,得到 %d", bal)
	}
}

// ── 冪等 ──

// 同 key 重送:回原結果、錢與權益只動一次;同 key 不同內容:衝突
func TestPurchaseIdempotencyReplayAndConflict(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	itemID, pub := newItem(t, itemSpec{fulfillment: "auto_role", price: 300, refundWindow: 600})
	key := fmt.Sprintf("idem-%d", u)
	p := shop.PurchaseParams{UserID: u, ItemPublicID: pub, IdempotencyKey: key}

	first, err := svc.Purchase(ctx, p)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := svc.Purchase(ctx, p)
	if err != nil {
		t.Fatalf("重送: %v", err)
	}
	if !second.Replayed || second.EntitlementID != first.EntitlementID {
		t.Fatalf("重送應回原結果並標記 Replayed: first=%+v second=%+v", first, second)
	}
	if bal := balance(t, u); bal != 700 {
		t.Fatalf("重送後餘額應仍為 700,得到 %d", bal)
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements WHERE user_id=$1 AND item_id=$2`, u, itemID); n != 1 {
		t.Fatalf("應只有 1 筆權益,得到 %d", n)
	}
	// 同 key 買別的商品 → 衝突
	_, pub2 := newItem(t, itemSpec{fulfillment: "auto_perk", price: 100})
	if _, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub2, IdempotencyKey: key,
	}); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("同 key 不同內容應回 ErrIdempotencyConflict,得到 %v", err)
	}
}

// 併發同 key:錢只動一次、權益只有一筆
func TestConcurrentPurchaseSameKey(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	itemID, pub := newItem(t, itemSpec{fulfillment: "auto_role", price: 300, refundWindow: 600})
	p := shop.PurchaseParams{UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("cc-%d", u)}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc.Purchase(ctx, p) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ledger.ErrInFlight) {
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if bal := balance(t, u); bal != 700 {
		t.Fatalf("併發重送錢應只動一次,餘額 %d", bal)
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements WHERE user_id=$1 AND item_id=$2`, u, itemID); n != 1 {
		t.Fatalf("應只有 1 筆權益,得到 %d", n)
	}
	if n := count(t, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1 AND reason='shop_purchase'`, u); n != 1 {
		t.Fatalf("應只有 1 筆扣款分錄,得到 %d", n)
	}
	assertLedgerConsistent(t, u)
}

// 併發不同 key 搶限購:恰一成功
func TestConcurrentPurchaseLimitRace(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	itemID, pub := newItem(t, itemSpec{fulfillment: "auto_perk", price: 100,
		perUserLimit: ptr(int32(1)), refundWindow: 600})

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Purchase(ctx, shop.PurchaseParams{
				UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("lr-%d-%d", u, i),
			})
		}(i)
	}
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, shop.ErrPerUserLimitReached): // 預期
		default:
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("限購 1 的併發搶購應恰一成功,成功了 %d 次", ok)
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements WHERE user_id=$1 AND item_id=$2`, u, itemID); n != 1 {
		t.Fatalf("應只有 1 筆權益,得到 %d", n)
	}
	if bal := balance(t, u); bal != 900 {
		t.Fatalf("應只扣一次款,餘額 %d", bal)
	}
	assertLedgerConsistent(t, u)
}

// ── 退款 ──

// 窗口內退款:錢退回、權益撤銷、退款分錄 ref 原分錄;重複退擋住;同 key 重放
func TestRefundWithinWindow(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 1000)
	_, pub := newItem(t, itemSpec{fulfillment: "auto_role", price: 300, refundWindow: 600})

	pres, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("rf-p-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	key := fmt.Sprintf("rf-%d", u)
	rres, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: pres.EntitlementID, ByUserID: u, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if rres.Amount != 300 {
		t.Fatalf("應全額退 300,得到 %d", rres.Amount)
	}
	if bal := balance(t, u); bal != 1000 {
		t.Fatalf("退款後餘額應回到 1000,得到 %d", bal)
	}
	// 退款是新的正數分錄,ref 指向原扣款分錄(append-only)
	if n := count(t, `SELECT count(*) FROM platform.token_entries
		WHERE user_id=$1 AND reason='refund' AND amount=300
		  AND ref_type='token_entry' AND ref_id=$2`, u, pres.LedgerEntryID); n != 1 {
		t.Fatal("退款分錄應 ref 原扣款分錄")
	}
	if n := count(t, `SELECT count(*) FROM platform.entitlements
		WHERE id=$1 AND revoked_at IS NOT NULL`, pres.EntitlementID); n != 1 {
		t.Fatal("權益應已撤銷")
	}
	// 同 key 重送 → 重放,不重複退錢
	again, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: pres.EntitlementID, ByUserID: u, IdempotencyKey: key,
	})
	if err != nil || !again.Replayed {
		t.Fatalf("同 key 重送應重放: res=%+v err=%v", again, err)
	}
	// 不同 key 再退同一權益 → 已撤銷
	if _, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: pres.EntitlementID, ByUserID: u, IdempotencyKey: key + "-2",
	}); !errors.Is(err, shop.ErrAlreadyRevoked) {
		t.Fatalf("重複退款應回 ErrAlreadyRevoked,得到 %v", err)
	}
	if bal := balance(t, u); bal != 1000 {
		t.Fatalf("餘額應仍為 1000,得到 %d", bal)
	}
	assertLedgerConsistent(t, u)
}

// 過窗口 / 不可退商品 / 非本人:各回專屬錯誤
func TestRefundRejections(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	other := newUser(t)
	credit(t, u, 1000)

	// 不可退商品(window=0 → refundable_until NULL)
	_, pubNoRefund := newItem(t, itemSpec{fulfillment: "auto_perk", price: 100, refundWindow: 0})
	p1, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pubNoRefund, IdempotencyKey: fmt.Sprintf("rr1-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: p1.EntitlementID, ByUserID: u, IdempotencyKey: fmt.Sprintf("rr1-rf-%d", u),
	}); !errors.Is(err, shop.ErrRefundWindowClosed) {
		t.Fatalf("不可退商品應回 ErrRefundWindowClosed,得到 %v", err)
	}

	// 過窗口(把 refundable_until 改到過去——僅測試允許的直接操作)
	_, pubShort := newItem(t, itemSpec{fulfillment: "auto_perk", price: 100, refundWindow: 600})
	p2, err := svc.Purchase(ctx, shop.PurchaseParams{
		UserID: u, ItemPublicID: pubShort, IdempotencyKey: fmt.Sprintf("rr2-%d", u),
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE platform.entitlements
		SET refundable_until = now() - interval '1 second' WHERE id=$1`, p2.EntitlementID); err != nil {
		t.Fatalf("調整窗口: %v", err)
	}
	if _, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: p2.EntitlementID, ByUserID: u, IdempotencyKey: fmt.Sprintf("rr2-rf-%d", u),
	}); !errors.Is(err, shop.ErrRefundWindowClosed) {
		t.Fatalf("過窗口應回 ErrRefundWindowClosed,得到 %v", err)
	}

	// 非本人
	if _, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: p2.EntitlementID, ByUserID: other, IdempotencyKey: fmt.Sprintf("rr3-%d", u),
	}); !errors.Is(err, shop.ErrNotEntitlementOwner) {
		t.Fatalf("非本人應回 ErrNotEntitlementOwner,得到 %v", err)
	}

	// 權益不存在
	if _, err := svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: 1 << 40, ByUserID: u, IdempotencyKey: fmt.Sprintf("rr4-%d", u),
	}); !errors.Is(err, shop.ErrEntitlementNotFound) {
		t.Fatalf("不存在應回 ErrEntitlementNotFound,得到 %v", err)
	}

	if bal := balance(t, u); bal != 800 {
		t.Fatalf("兩筆購買後餘額應為 800(退款全被拒),得到 %d", bal)
	}
	assertLedgerConsistent(t, u)
}

// ── 工單處理 ──

func buyManual(t *testing.T, u int64, price int64) *shop.PurchaseResult {
	t.Helper()
	_, pub := newItem(t, itemSpec{fulfillment: "manual", price: price, refundWindow: 600})
	res, err := svc.Purchase(context.Background(), shop.PurchaseParams{
		UserID: u, ItemPublicID: pub, IdempotencyKey: fmt.Sprintf("bm-%d-%d", u, price),
	})
	if err != nil {
		t.Fatalf("manual 購買: %v", err)
	}
	return res
}

// approve → fulfilled:不動錢,押款即最終扣款;重複處理拒絕
func TestHandleRedemptionApprove(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, admin := newUser(t), newUser(t)
	credit(t, u, 1000)
	pres := buyManual(t, u, 400)

	hres, err := svc.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: pres.RedemptionID, HandlerUserID: admin, Approve: true, Note: ptr("done"),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if hres.Status != shop.RedemptionFulfilled || hres.RefundEntryID != 0 {
		t.Fatalf("fulfilled 不應退款: %+v", hres)
	}
	if bal := balance(t, u); bal != 600 {
		t.Fatalf("fulfilled 不動錢,餘額應為 600,得到 %d", bal)
	}
	if n := count(t, `SELECT count(*) FROM platform.redemptions
		WHERE id=$1 AND status='fulfilled' AND handled_by=$2 AND handled_at IS NOT NULL AND note='done'`,
		pres.RedemptionID, admin); n != 1 {
		t.Fatal("工單應為 fulfilled 且記錄處理人/時間/備註")
	}
	if n := count(t, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='redemption.handled' AND payload->>'redemption_public_id'=$1`,
		pres.RedemptionPublicID); n != 1 {
		t.Fatal("應有 1 筆 redemption.handled outbox")
	}
	// 非 pending 重複處理拒絕(狀態機單向)
	if _, err := svc.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: pres.RedemptionID, HandlerUserID: admin, Approve: false,
	}); !errors.Is(err, shop.ErrRedemptionNotPending) {
		t.Fatalf("重複處理應回 ErrRedemptionNotPending,得到 %v", err)
	}
	assertLedgerConsistent(t, u)
}

// reject → rejected:全額退且只退一次
func TestHandleRedemptionReject(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, admin := newUser(t), newUser(t)
	credit(t, u, 1000)
	pres := buyManual(t, u, 400)

	hres, err := svc.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: pres.RedemptionID, HandlerUserID: admin, Approve: false, Note: ptr("out of stock"),
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if hres.Status != shop.RedemptionRejected || hres.RefundAmount != 400 {
		t.Fatalf("rejected 應全額退 400: %+v", hres)
	}
	if bal := balance(t, u); bal != 1000 {
		t.Fatalf("退款後餘額應回 1000,得到 %d", bal)
	}
	// 退款分錄 ref 原押款分錄,且只有一筆
	if n := count(t, `SELECT count(*) FROM platform.token_entries
		WHERE user_id=$1 AND reason='redemption_refund' AND amount=400
		  AND ref_type='token_entry' AND ref_id=$2`, u, pres.LedgerEntryID); n != 1 {
		t.Fatal("應恰有 1 筆 ref 原押款的退款分錄")
	}
	// 再處理 → 拒絕,不重複退
	if _, err := svc.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: pres.RedemptionID, HandlerUserID: admin, Approve: false,
	}); !errors.Is(err, shop.ErrRedemptionNotPending) {
		t.Fatalf("重複處理應回 ErrRedemptionNotPending,得到 %v", err)
	}
	if bal := balance(t, u); bal != 1000 {
		t.Fatalf("只能退一次,餘額 %d", bal)
	}
	assertLedgerConsistent(t, u)
}

// 使用者自取消:cancelled + 全額退;非本人拒絕
func TestCancelRedemption(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, other := newUser(t), newUser(t)
	credit(t, u, 1000)
	pres := buyManual(t, u, 250)

	if _, err := svc.CancelRedemption(ctx, shop.CancelParams{
		RedemptionID: pres.RedemptionID, ByUserID: other,
	}); !errors.Is(err, shop.ErrNotRedemptionOwner) {
		t.Fatalf("非本人取消應回 ErrNotRedemptionOwner,得到 %v", err)
	}
	cres, err := svc.CancelRedemption(ctx, shop.CancelParams{
		RedemptionID: pres.RedemptionID, ByUserID: u,
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cres.Status != shop.RedemptionCancelled || cres.RefundAmount != 250 {
		t.Fatalf("cancelled 應全額退 250: %+v", cres)
	}
	if bal := balance(t, u); bal != 1000 {
		t.Fatalf("取消後餘額應回 1000,得到 %d", bal)
	}
	// 已取消後管理員再處理 → 拒絕
	if _, err := svc.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: pres.RedemptionID, HandlerUserID: other, Approve: true,
	}); !errors.Is(err, shop.ErrRedemptionNotPending) {
		t.Fatalf("已取消的工單應回 ErrRedemptionNotPending,得到 %v", err)
	}
	assertLedgerConsistent(t, u)
}

// 併發 approve + reject:恰一生效;若 reject 勝出,退款恰一次
func TestConcurrentHandleRedemption(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, admin := newUser(t), newUser(t)
	credit(t, u, 1000)
	pres := buyManual(t, u, 400)

	var wg sync.WaitGroup
	var errApprove, errReject error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errApprove = svc.HandleRedemption(ctx, shop.HandleParams{
			RedemptionID: pres.RedemptionID, HandlerUserID: admin, Approve: true,
		})
	}()
	go func() {
		defer wg.Done()
		_, errReject = svc.HandleRedemption(ctx, shop.HandleParams{
			RedemptionID: pres.RedemptionID, HandlerUserID: admin, Approve: false,
		})
	}()
	wg.Wait()

	okCount := 0
	for _, err := range []error{errApprove, errReject} {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, shop.ErrRedemptionNotPending): // 輸的一方
		default:
			t.Fatalf("非預期錯誤: approve=%v reject=%v", errApprove, errReject)
		}
	}
	if okCount != 1 {
		t.Fatalf("併發 approve+reject 應恰一生效,生效了 %d 個", okCount)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM platform.redemptions WHERE id=$1`,
		pres.RedemptionID).Scan(&status); err != nil {
		t.Fatalf("讀工單: %v", err)
	}
	refunds := count(t, `SELECT count(*) FROM platform.token_entries
		WHERE user_id=$1 AND reason='redemption_refund'`, u)
	switch status {
	case "fulfilled":
		if refunds != 0 || balance(t, u) != 600 {
			t.Fatalf("fulfilled 不應退款: refunds=%d bal=%d", refunds, balance(t, u))
		}
	case "rejected":
		if refunds != 1 || balance(t, u) != 1000 {
			t.Fatalf("rejected 應恰退一次: refunds=%d bal=%d", refunds, balance(t, u))
		}
	default:
		t.Fatalf("終態應為 fulfilled 或 rejected,得到 %s", status)
	}
	assertLedgerConsistent(t, u)
}

// 全域對帳:所有測試跑完前的最後防線(SUM(entries) = balance 對每一組都成立)
func TestReconcileAll(t *testing.T) {
	setup(t)
	mis, err := led.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(mis) != 0 {
		t.Fatalf("帳應是平的,發現不一致: %+v", mis)
	}
}
