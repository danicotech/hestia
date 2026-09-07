package reaper_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/reaper"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(mock 掉資料庫就測不到 SKIP LOCKED 與列鎖行為)。
// 共用一個容器;測試循序執行(不開 t.Parallel),斷言 processed 前先 drain 清場。

var (
	pool *pgxpool.Pool
	rp   *reaper.Reaper
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
		rp = reaper.New(pool)
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

// drain 把資料庫裡既有的到期權益收乾淨,讓接下來的 processed 斷言只算本測試的列。
func drain(t *testing.T) {
	t.Helper()
	if _, err := rp.Reap(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
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

// newItem 建測試商品;externalRoleID 為 nil 時 external_role_id 為 NULL。
func newItem(t *testing.T, externalRoleID *string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.shop_items
		   (public_id, name, fulfillment, currency, price, duration_days, external_role_id, listed_at)
		 VALUES (gen_random_uuid()::text, 'reaper test item', 'auto_role', 'coin', 0, 7, $1, now())
		 RETURNING id`, externalRoleID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試商品: %v", err)
	}
	return id
}

// newEntitlement 直接插權益列以精準控制時間欄位。
// expiresExpr / revokedExpr 是 SQL 時間表達式(如 "now() - interval '1 minute'")或 "NULL"。
func newEntitlement(t *testing.T, userID, itemID int64, expiresExpr, revokedExpr string) int64 {
	t.Helper()
	var id int64
	q := fmt.Sprintf(`INSERT INTO platform.entitlements (user_id, item_id, expires_at, revoked_at)
		VALUES ($1, $2, %s, %s) RETURNING id`, expiresExpr, revokedExpr)
	if err := pool.QueryRow(context.Background(), q, userID, itemID).Scan(&id); err != nil {
		t.Fatalf("建測試權益: %v", err)
	}
	return id
}

func isRevoked(t *testing.T, entID int64) bool {
	t.Helper()
	var revoked bool
	err := pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM platform.entitlements WHERE id = $1`, entID,
	).Scan(&revoked)
	if err != nil {
		t.Fatalf("查權益 %d: %v", entID, err)
	}
	return revoked
}

// outboxCount 數某權益的 entitlement.expired 事件數(恰一筆是核心驗收)。
func outboxCount(t *testing.T, entID int64) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform.outbox_events
		 WHERE topic = 'entitlement.expired' AND (payload->>'entitlement_id')::bigint = $1`, entID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("數 outbox: %v", err)
	}
	return n
}

// TestReapExpired:到期的被 revoke + outbox 恰一筆,payload 齊全;
// 未到期、永久、已 revoked 的一律不動。
func TestReapExpired(t *testing.T) {
	setup(t)
	drain(t)
	ctx := context.Background()
	user := newUser(t)
	roleID := "discord-role-42"
	itemWithRole := newItem(t, &roleID)
	itemNoRole := newItem(t, nil)

	expired := newEntitlement(t, user, itemWithRole, `now() - interval '1 minute'`, "NULL")
	expiredNoRole := newEntitlement(t, user, itemNoRole, `now() - interval '1 minute'`, "NULL")
	future := newEntitlement(t, user, itemWithRole, `now() + interval '1 hour'`, "NULL")
	permanent := newEntitlement(t, user, itemWithRole, "NULL", "NULL")
	alreadyRevoked := newEntitlement(t, user, itemWithRole, `now() - interval '1 hour'`, `now() - interval '30 minutes'`)

	processed, err := rp.Reap(ctx)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if processed != 2 {
		t.Fatalf("processed = %d,要 2(只有兩筆到期)", processed)
	}

	// 到期的:revoked + outbox 恰一筆,payload 帶 external_role_id
	if !isRevoked(t, expired) {
		t.Error("到期權益沒被 revoke")
	}
	if n := outboxCount(t, expired); n != 1 {
		t.Errorf("到期權益 outbox = %d,要恰 1", n)
	}
	var raw []byte
	err = pool.QueryRow(ctx, `SELECT payload FROM platform.outbox_events
		WHERE topic = 'entitlement.expired' AND (payload->>'entitlement_id')::bigint = $1`, expired).Scan(&raw)
	if err != nil {
		t.Fatalf("讀 payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解 payload: %v", err)
	}
	if got := int64(payload["user_id"].(float64)); got != user {
		t.Errorf("payload user_id = %d,要 %d", got, user)
	}
	if got := int64(payload["item_id"].(float64)); got != itemWithRole {
		t.Errorf("payload item_id = %d,要 %d", got, itemWithRole)
	}
	if got, _ := payload["external_role_id"].(string); got != roleID {
		t.Errorf("payload external_role_id = %q,要 %q", got, roleID)
	}

	// 商品沒 external_role_id:事件照發,但 payload 不帶該鍵
	if !isRevoked(t, expiredNoRole) {
		t.Error("無身分組商品的到期權益沒被 revoke")
	}
	err = pool.QueryRow(ctx, `SELECT payload FROM platform.outbox_events
		WHERE topic = 'entitlement.expired' AND (payload->>'entitlement_id')::bigint = $1`, expiredNoRole).Scan(&raw)
	if err != nil {
		t.Fatalf("讀無身分組 payload: %v", err)
	}
	payload = nil
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解無身分組 payload: %v", err)
	}
	if _, ok := payload["external_role_id"]; ok {
		t.Error("商品沒有 external_role_id,payload 不該帶這個鍵")
	}

	// 不動的三類
	for name, id := range map[string]int64{"未到期": future, "永久": permanent} {
		if isRevoked(t, id) {
			t.Errorf("%s權益不該被 revoke", name)
		}
		if n := outboxCount(t, id); n != 0 {
			t.Errorf("%s權益 outbox = %d,要 0", name, n)
		}
	}
	if n := outboxCount(t, alreadyRevoked); n != 0 {
		t.Errorf("已 revoked 權益 outbox = %d,要 0", n)
	}
	var revokedAgo bool
	err = pool.QueryRow(ctx, `SELECT revoked_at < now() - interval '20 minutes'
		FROM platform.entitlements WHERE id = $1`, alreadyRevoked).Scan(&revokedAgo)
	if err != nil {
		t.Fatalf("查已 revoked 權益: %v", err)
	}
	if !revokedAgo {
		t.Error("已 revoked 權益的 revoked_at 被改動了")
	}
}

// TestReapEmpty:無到期權益時 processed=0 無錯。
func TestReapEmpty(t *testing.T) {
	setup(t)
	drain(t)
	processed, err := rp.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if processed != 0 {
		t.Fatalf("processed = %d,要 0", processed)
	}
}

// TestReapBatchBoundary:超過一批的量(250 筆、LIMIT 100)迴圈收完。
func TestReapBatchBoundary(t *testing.T) {
	setup(t)
	drain(t)
	ctx := context.Background()
	user := newUser(t)
	item := newItem(t, nil)
	const total = 250
	_, err := pool.Exec(ctx, `INSERT INTO platform.entitlements (user_id, item_id, expires_at)
		SELECT $1, $2, now() - interval '1 minute' FROM generate_series(1, $3::int)`,
		user, item, total)
	if err != nil {
		t.Fatalf("建 %d 筆權益: %v", total, err)
	}

	processed, err := rp.Reap(ctx) // BatchSize 預設 100 → 100+100+50+空
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if processed != total {
		t.Fatalf("processed = %d,要 %d", processed, total)
	}

	var remaining, events int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM platform.entitlements
		WHERE item_id = $1 AND revoked_at IS NULL`, item).Scan(&remaining)
	if err != nil {
		t.Fatalf("數殘留: %v", err)
	}
	if remaining != 0 {
		t.Errorf("還有 %d 筆沒收", remaining)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
		WHERE topic = 'entitlement.expired' AND (payload->>'item_id')::bigint = $1`, item).Scan(&events)
	if err != nil {
		t.Fatalf("數事件: %v", err)
	}
	if events != total {
		t.Errorf("outbox 事件 %d 筆,要 %d", events, total)
	}
}

// TestReapConcurrent:兩個 Reap 並發,每筆到期權益恰被處理一次
// (revoked 一次、outbox 恰一筆),總 processed 不多不少。
func TestReapConcurrent(t *testing.T) {
	setup(t)
	drain(t)
	ctx := context.Background()
	user := newUser(t)
	item := newItem(t, nil)
	const total = 60
	_, err := pool.Exec(ctx, `INSERT INTO platform.entitlements (user_id, item_id, expires_at)
		SELECT $1, $2, now() - interval '1 minute' FROM generate_series(1, $3::int)`,
		user, item, total)
	if err != nil {
		t.Fatalf("建 %d 筆權益: %v", total, err)
	}

	// 小批次逼出交錯:兩個實例都要跑好幾批,SKIP LOCKED 互相跳過對方鎖住的列。
	a, b := reaper.New(pool), reaper.New(pool)
	a.BatchSize, b.BatchSize = 7, 7

	var wg sync.WaitGroup
	counts := make([]int, 2)
	errs := make([]error, 2)
	for i, r := range []*reaper.Reaper{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counts[i], errs[i] = r.Reap(ctx)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Reap #%d: %v", i, err)
		}
	}
	if sum := counts[0] + counts[1]; sum != total {
		t.Fatalf("兩實例合計 processed = %d(%d+%d),要 %d", sum, counts[0], counts[1], total)
	}

	// 每筆恰一次:沒有殘留、沒有重複事件、事件涵蓋全部權益
	var remaining, dup, distinct int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM platform.entitlements
		WHERE item_id = $1 AND revoked_at IS NULL`, item).Scan(&remaining)
	if err != nil {
		t.Fatalf("數殘留: %v", err)
	}
	if remaining != 0 {
		t.Errorf("還有 %d 筆沒收", remaining)
	}
	err = pool.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT payload->>'entitlement_id' FROM platform.outbox_events
		WHERE topic = 'entitlement.expired' AND (payload->>'item_id')::bigint = $1
		GROUP BY 1 HAVING count(*) <> 1) d`, item).Scan(&dup)
	if err != nil {
		t.Fatalf("數重複事件: %v", err)
	}
	if dup != 0 {
		t.Errorf("有 %d 筆權益的事件數 ≠ 1", dup)
	}
	err = pool.QueryRow(ctx, `SELECT count(DISTINCT payload->>'entitlement_id')
		FROM platform.outbox_events
		WHERE topic = 'entitlement.expired' AND (payload->>'item_id')::bigint = $1`, item).Scan(&distinct)
	if err != nil {
		t.Fatalf("數事件涵蓋: %v", err)
	}
	if distinct != total {
		t.Errorf("事件涵蓋 %d 筆權益,要 %d", distinct, total)
	}
}

// TestReapSkipsLockedRows:被別的 tx 鎖住的列(模擬退款中)本輪跳過不阻塞,
// 鎖釋放後下輪照常處理。
func TestReapSkipsLockedRows(t *testing.T) {
	setup(t)
	drain(t)
	ctx := context.Background()
	user := newUser(t)
	item := newItem(t, nil)
	locked := newEntitlement(t, user, item, `now() - interval '1 minute'`, "NULL")
	free := newEntitlement(t, user, item, `now() - interval '1 minute'`, "NULL")

	// 模擬 shoppg 退款:另一條連線鎖住 entitlement 列不放
	refundTx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("開退款 tx: %v", err)
	}
	defer func() { _ = refundTx.Rollback(ctx) }()
	var lockedID int64
	err = refundTx.QueryRow(ctx,
		`SELECT id FROM platform.entitlements WHERE id = $1 FOR UPDATE`, locked).Scan(&lockedID)
	if err != nil {
		t.Fatalf("鎖權益列: %v", err)
	}

	// 本輪:不阻塞(逾時保護),只收沒被鎖的那筆
	reapCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	processed, err := rp.Reap(reapCtx)
	if err != nil {
		t.Fatalf("Reap(有列被鎖): %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d,要 1(鎖住的要跳過)", processed)
	}
	if !isRevoked(t, free) {
		t.Error("沒被鎖的權益該被收")
	}
	if isRevoked(t, locked) {
		t.Error("被鎖的權益本輪不該被動到")
	}
	if n := outboxCount(t, locked); n != 0 {
		t.Errorf("被鎖權益 outbox = %d,要 0", n)
	}

	// 鎖釋放(退款 rollback,權益仍到期未撤銷)→ 下輪照常收
	if err := refundTx.Rollback(ctx); err != nil {
		t.Fatalf("放鎖: %v", err)
	}
	processed, err = rp.Reap(ctx)
	if err != nil {
		t.Fatalf("Reap(鎖已釋放): %v", err)
	}
	if processed != 1 {
		t.Fatalf("放鎖後 processed = %d,要 1", processed)
	}
	if !isRevoked(t, locked) {
		t.Error("放鎖後權益該被收")
	}
	if n := outboxCount(t, locked); n != 1 {
		t.Errorf("放鎖後 outbox = %d,要恰 1", n)
	}
}
