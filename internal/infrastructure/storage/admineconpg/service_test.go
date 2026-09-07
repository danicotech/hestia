package admineconpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/admineconpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義)。
// 共用一個容器,各測試用獨立使用者隔離。

var (
	pool *pgxpool.Pool
	svc  *admineconpg.Service
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
		svc = admineconpg.New(pool, ledgerpg.New(pool))
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

func grant(t *testing.T, actor, user, amount int64, key string) *adminecon.Result {
	t.Helper()
	res, err := svc.Grant(context.Background(), adminecon.AdjustParams{
		IdempotencyKey: key, ActorUserID: actor, UserID: user,
		Currency: "coin", Amount: amount, Reason: "測試發點",
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return res
}

// balanceOf 直接讀 user_balances 快取。
func balanceOf(t *testing.T, user int64) int64 {
	t.Helper()
	var bal int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT balance FROM platform.user_balances
		   WHERE user_id=$1 AND currency='coin'), 0)`, user).Scan(&bal)
	if err != nil {
		t.Fatalf("讀餘額: %v", err)
	}
	return bal
}

// assertLedgerConsistent 驗證 SUM(entries) = balance(帳本鐵則的核心性質)。
func assertLedgerConsistent(t *testing.T, user int64) {
	t.Helper()
	var sum int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0) FROM platform.token_entries
		 WHERE user_id=$1 AND currency='coin'`, user).Scan(&sum)
	if err != nil {
		t.Fatalf("SUM(entries): %v", err)
	}
	if bal := balanceOf(t, user); sum != bal {
		t.Fatalf("帳不平: SUM(entries)=%d, balance=%d", sum, bal)
	}
}

type auditRow struct {
	Action     string
	TargetType string
	TargetID   int64
	Before     []byte
	After      []byte
	Reason     string
}

// auditsBy 讀某管理員的全部稽核紀錄(測試用 actor 隔離,不會撈到別的測試)。
func auditsBy(t *testing.T, actor int64) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT action, COALESCE(target_type,''), COALESCE(target_id,0), before, after, reason
		 FROM platform.admin_audit_logs WHERE actor_user_id=$1 ORDER BY id`, actor)
	if err != nil {
		t.Fatalf("讀 audit: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.Before, &r.After, &r.Reason); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// Grant:餘額、分錄欄位、audit 全對,且 SUM(entries)=balance
func TestGrant(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)

	res, err := svc.Grant(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("g1-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 500,
		Reason: "活動補償",
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if res.NewBalance != 500 || res.Amount != 500 || res.Replayed {
		t.Fatalf("結果錯誤: %+v", res)
	}
	if got := balanceOf(t, user); got != 500 {
		t.Fatalf("餘額應為 500,得到 %d", got)
	}

	var reason string
	var actorID int64
	err = pool.QueryRow(ctx,
		`SELECT reason, actor_id FROM platform.token_entries WHERE id=$1`, res.EntryID,
	).Scan(&reason, &actorID)
	if err != nil {
		t.Fatalf("讀分錄: %v", err)
	}
	if reason != "admin_grant" || actorID != actor {
		t.Fatalf("分錄欄位錯誤: reason=%s actor_id=%d", reason, actorID)
	}

	audits := auditsBy(t, actor)
	if len(audits) != 1 {
		t.Fatalf("應有 1 筆 audit,得到 %d", len(audits))
	}
	a := audits[0]
	if a.Action != "grant_tokens" || a.TargetType != "user" || a.TargetID != user {
		t.Fatalf("audit 欄位錯誤: %+v", a)
	}
	if a.Reason == "" {
		t.Fatal("audit reason 不得為空")
	}
	assertLedgerConsistent(t, user)
}

// 同 IdempotencyKey 重送 Grant:錢只動一次,audit 也只有一筆
func TestGrantIdempotentReplay(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	p := adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("g2-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 300,
		Reason: "補點",
	}
	first, err := svc.Grant(ctx, p)
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := svc.Grant(ctx, p)
	if err != nil {
		t.Fatalf("重送: %v", err)
	}
	if !second.Replayed {
		t.Fatal("重送應標記 Replayed")
	}
	if second.EntryID != first.EntryID {
		t.Fatal("重送應回同一筆分錄")
	}
	if got := balanceOf(t, user); got != 300 {
		t.Fatalf("重送後餘額應仍為 300,得到 %d", got)
	}
	var entries int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, user).Scan(&entries)
	if entries != 1 {
		t.Fatalf("應只有 1 筆分錄,得到 %d", entries)
	}
	if audits := auditsBy(t, actor); len(audits) != 1 {
		t.Fatalf("重放不應重寫 audit,得到 %d 筆", len(audits))
	}
	assertLedgerConsistent(t, user)
}

// Deduct:餘額、負數分錄、audit 全對
func TestDeduct(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	grant(t, actor, user, 1000, fmt.Sprintf("d1-fund-%d", user))

	res, err := svc.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("d1-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 300,
		Reason: "違規扣點",
	})
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}
	if res.NewBalance != 700 || res.Amount != -300 {
		t.Fatalf("結果錯誤: %+v", res)
	}

	var amount int64
	var reason string
	var actorID int64
	err = pool.QueryRow(ctx,
		`SELECT amount, reason, actor_id FROM platform.token_entries WHERE id=$1`, res.EntryID,
	).Scan(&amount, &reason, &actorID)
	if err != nil {
		t.Fatalf("讀分錄: %v", err)
	}
	if amount != -300 || reason != "admin_deduct" || actorID != actor {
		t.Fatalf("分錄欄位錯誤: amount=%d reason=%s actor_id=%d", amount, reason, actorID)
	}

	audits := auditsBy(t, actor)
	if len(audits) != 2 { // 前面的 grant + 這次的 deduct
		t.Fatalf("應有 2 筆 audit,得到 %d", len(audits))
	}
	a := audits[1]
	if a.Action != "deduct_tokens" || a.TargetType != "user" || a.TargetID != user || a.Reason == "" {
		t.Fatalf("audit 欄位錯誤: %+v", a)
	}
	assertLedgerConsistent(t, user)
}

// Deduct 超過餘額:拒絕且零寫入 —— 分錄、餘額、冪等鍵、audit 全部不留
func TestDeductInsufficientZeroWrites(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	grant(t, actor, user, 100, fmt.Sprintf("d2-fund-%d", user))

	key := fmt.Sprintf("d2-over-%d", user)
	_, err := svc.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: key,
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 200,
		Reason: "扣太多",
	})
	if !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("應透傳 ErrInsufficientBalance,得到 %v", err)
	}
	if got := balanceOf(t, user); got != 100 {
		t.Fatalf("餘額應保持 100,得到 %d", got)
	}
	var entries int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, user).Scan(&entries)
	if entries != 1 { // 只有 fund 的那筆
		t.Fatalf("失敗的扣點不應留分錄,得到 %d 筆", entries)
	}
	var keys int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.idempotency_keys WHERE key=$1`, key).Scan(&keys)
	if keys != 0 {
		t.Fatal("rollback 後冪等鍵不應殘留")
	}
	// 動作沒發生就不該有稽核紀錄:只剩 grant 那筆
	audits := auditsBy(t, actor)
	if len(audits) != 1 || audits[0].Action != "grant_tokens" {
		t.Fatalf("失敗的扣點不應留 audit,得到 %+v", audits)
	}
	assertLedgerConsistent(t, user)
}

// Refund:沖銷分錄欄位、餘額回復、audit before/after
func TestRefund(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	grant(t, actor, user, 1000, fmt.Sprintf("r1-fund-%d", user))
	ded, err := svc.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("r1-ded-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 400,
		Reason: "誤扣",
	})
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}

	res, err := svc.Refund(ctx, adminecon.RefundParams{
		ActorUserID: actor, EntryID: ded.EntryID, Reason: "扣錯人,退回",
	})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.Amount != 400 || res.NewBalance != 1000 || res.UserID != user {
		t.Fatalf("結果錯誤: %+v", res)
	}
	if got := balanceOf(t, user); got != 1000 {
		t.Fatalf("退款後餘額應為 1000,得到 %d", got)
	}

	var amount int64
	var reason, refType string
	var refID, actorID int64
	err = pool.QueryRow(ctx,
		`SELECT amount, reason, ref_type, ref_id, actor_id
		 FROM platform.token_entries WHERE id=$1`, res.EntryID,
	).Scan(&amount, &reason, &refType, &refID, &actorID)
	if err != nil {
		t.Fatalf("讀沖銷分錄: %v", err)
	}
	if amount != 400 || reason != "refund" || refType != "token_entry" ||
		refID != ded.EntryID || actorID != actor {
		t.Fatalf("沖銷分錄欄位錯誤: amount=%d reason=%s ref=%s/%d actor=%d",
			amount, reason, refType, refID, actorID)
	}
	// 原分錄絕未被修改(append-only)
	var origAmount int64
	_ = pool.QueryRow(ctx, `SELECT amount FROM platform.token_entries WHERE id=$1`, ded.EntryID).Scan(&origAmount)
	if origAmount != -400 {
		t.Fatalf("原分錄被改動: amount=%d", origAmount)
	}

	audits := auditsBy(t, actor)
	last := audits[len(audits)-1]
	if last.Action != "refund" || last.TargetType != "token_entry" || last.TargetID != ded.EntryID {
		t.Fatalf("refund audit 欄位錯誤: %+v", last)
	}
	if last.Reason == "" {
		t.Fatal("audit reason 不得為空")
	}
	if !strings.Contains(string(last.Before), `"admin_deduct"`) {
		t.Fatalf("audit before 應帶原分錄摘要,得到 %s", last.Before)
	}
	if !strings.Contains(string(last.After), `"ref_type"`) {
		t.Fatalf("audit after 應帶沖銷分錄摘要,得到 %s", last.After)
	}
	assertLedgerConsistent(t, user)
}

// 同一分錄退兩次:第二次 ErrAlreadyRefunded,錢只退一次
func TestRefundTwice(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	grant(t, actor, user, 500, fmt.Sprintf("r2-fund-%d", user))
	ded, err := svc.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("r2-ded-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 200,
		Reason: "扣點",
	})
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}

	if _, err := svc.Refund(ctx, adminecon.RefundParams{
		ActorUserID: actor, EntryID: ded.EntryID, Reason: "第一次退",
	}); err != nil {
		t.Fatalf("第一次退款: %v", err)
	}
	_, err = svc.Refund(ctx, adminecon.RefundParams{
		ActorUserID: actor, EntryID: ded.EntryID, Reason: "第二次退(換個理由也不行)",
	})
	if !errors.Is(err, adminecon.ErrAlreadyRefunded) {
		t.Fatalf("第二次應回 ErrAlreadyRefunded,得到 %v", err)
	}
	if got := balanceOf(t, user); got != 500 {
		t.Fatalf("錢應只退一次,餘額 %d", got)
	}
	var refunds int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.token_entries
		 WHERE user_id=$1 AND reason='refund'`, user).Scan(&refunds)
	if refunds != 1 {
		t.Fatalf("應只有 1 筆退款分錄,得到 %d", refunds)
	}
	assertLedgerConsistent(t, user)
}

// 正數分錄(admin_grant)不能退
func TestRefundPositiveEntryRejected(t *testing.T) {
	setup(t)
	actor, user := newUser(t), newUser(t)
	g := grant(t, actor, user, 100, fmt.Sprintf("r3-%d", user))

	_, err := svc.Refund(context.Background(), adminecon.RefundParams{
		ActorUserID: actor, EntryID: g.EntryID, Reason: "想退發點",
	})
	if !errors.Is(err, adminecon.ErrNotRefundable) {
		t.Fatalf("退正數分錄應回 ErrNotRefundable,得到 %v", err)
	}
	if got := balanceOf(t, user); got != 100 {
		t.Fatalf("餘額不應變動,得到 %d", got)
	}
}

// 原分錄不存在
func TestRefundEntryNotFound(t *testing.T) {
	setup(t)
	actor := newUser(t)
	_, err := svc.Refund(context.Background(), adminecon.RefundParams{
		ActorUserID: actor, EntryID: 999999999, Reason: "不存在的分錄",
	})
	if !errors.Is(err, adminecon.ErrEntryNotFound) {
		t.Fatalf("應回 ErrEntryNotFound,得到 %v", err)
	}
}

// 併發:同一分錄 8 個併發退款,恰一成功,其餘 ErrAlreadyRefunded
func TestRefundConcurrent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	grant(t, actor, user, 1000, fmt.Sprintf("r4-fund-%d", user))
	ded, err := svc.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("r4-ded-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 600,
		Reason: "扣點",
	})
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Refund(ctx, adminecon.RefundParams{
				ActorUserID: actor, EntryID: ded.EntryID,
				Reason: fmt.Sprintf("併發退款 %d", i),
			})
		}(i)
	}
	wg.Wait()

	success := 0
	for i, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, adminecon.ErrAlreadyRefunded):
		default:
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if success != 1 {
		t.Fatalf("應恰一成功,得到 %d", success)
	}
	if got := balanceOf(t, user); got != 1000 {
		t.Fatalf("錢應只退一次,餘額 %d", got)
	}
	var refunds int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.token_entries
		 WHERE user_id=$1 AND reason='refund'`, user).Scan(&refunds)
	if refunds != 1 {
		t.Fatalf("應只有 1 筆退款分錄,得到 %d", refunds)
	}
	var refundAudits int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.admin_audit_logs
		 WHERE actor_user_id=$1 AND action='refund'`, actor).Scan(&refundAudits)
	if refundAudits != 1 {
		t.Fatalf("應只有 1 筆 refund audit,得到 %d", refundAudits)
	}
	assertLedgerConsistent(t, user)
}

// 併發:同 IdempotencyKey 的 Grant 同時打,錢只動一次
func TestGrantConcurrentSameKey(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	p := adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("g3-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 250,
		Reason: "併發發點",
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc.Grant(ctx, p) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ledger.ErrInFlight) {
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if got := balanceOf(t, user); got != 250 {
		t.Fatalf("併發重送後餘額應為 250,得到 %d", got)
	}
	var entries int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, user).Scan(&entries)
	if entries != 1 {
		t.Fatalf("應只有 1 筆分錄,得到 %d", entries)
	}
	if audits := auditsBy(t, actor); len(audits) != 1 {
		t.Fatalf("應只有 1 筆 audit,得到 %d", len(audits))
	}
	assertLedgerConsistent(t, user)
}

// 空 Reason:三個操作全拒絕,零寫入
func TestEmptyReasonRejected(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)

	if _, err := svc.Grant(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("e1-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 100,
	}); !errors.Is(err, adminecon.ErrReasonRequired) {
		t.Fatalf("Grant 空 reason 應拒絕,得到 %v", err)
	}
	if _, err := svc.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("e2-%d", user),
		ActorUserID:    actor, UserID: user, Currency: "coin", Amount: 100,
	}); !errors.Is(err, adminecon.ErrReasonRequired) {
		t.Fatalf("Deduct 空 reason 應拒絕,得到 %v", err)
	}
	if _, err := svc.Refund(ctx, adminecon.RefundParams{
		ActorUserID: actor, EntryID: 1,
	}); !errors.Is(err, adminecon.ErrReasonRequired) {
		t.Fatalf("Refund 空 reason 應拒絕,得到 %v", err)
	}
	if got := balanceOf(t, user); got != 0 {
		t.Fatalf("不應動錢,餘額 %d", got)
	}
	if audits := auditsBy(t, actor); len(audits) != 0 {
		t.Fatalf("不應留 audit,得到 %d 筆", len(audits))
	}
}

// 金額非正數拒絕
func TestNonPositiveAmountRejected(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor, user := newUser(t), newUser(t)
	for _, amount := range []int64{0, -50} {
		if _, err := svc.Grant(ctx, adminecon.AdjustParams{
			IdempotencyKey: fmt.Sprintf("n1-%d-%d", user, amount),
			ActorUserID:    actor, UserID: user, Currency: "coin", Amount: amount,
			Reason: "非法金額",
		}); !errors.Is(err, adminecon.ErrInvalidInput) {
			t.Fatalf("amount=%d 應拒絕,得到 %v", amount, err)
		}
	}
}

// QA:呼叫端 key 不得污染 refund:entry:{id} 保留鍵空間——
// 惡意 Grant 用字面 "refund:entry:N" 當 key 後,該分錄的正當退款仍要能執行
func TestKeyNamespaceIsolation(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	// 先造一筆可退的扣款
	grant(t, newUser(t), u, 500, fmt.Sprintf("ns-grant-%d", u))
	res, err := svc.Deduct(ctx, adminecon.AdjustParams{
		ActorUserID: newUser(t), UserID: u, Currency: "coin", Amount: 200,
		Reason: "測試扣款", IdempotencyKey: fmt.Sprintf("ns-deduct-%d", u),
	})
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}
	entryID := res.EntryID

	// 攻擊:用保留命名空間的字面值當 Grant 的 key
	if _, err := svc.Grant(ctx, adminecon.AdjustParams{
		ActorUserID: newUser(t), UserID: u, Currency: "coin", Amount: 1,
		Reason: "污染嘗試", IdempotencyKey: fmt.Sprintf("refund:entry:%d", entryID),
	}); err != nil {
		t.Fatalf("grant with hostile key: %v", err)
	}

	// 正當退款必須不受影響
	if _, err := svc.Refund(ctx, adminecon.RefundParams{
		ActorUserID: newUser(t), EntryID: entryID, Reason: "正當退款",
	}); err != nil {
		t.Fatalf("退款被污染的鍵空間擋住: %v", err)
	}
}
