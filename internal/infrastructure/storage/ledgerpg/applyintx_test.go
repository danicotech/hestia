package ledgerpg_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// ApplyInTx 的核心價值:呼叫端把「動錢」和「自己的領域寫入」綁進同一個 tx
// (schemas/21 已確認 ③:下注 = 扣款 + 建注單同 tx)。這裡用一張暫存表模擬注單。

func begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

// 呼叫端 rollback → 動錢與領域寫入一起消失,冪等鍵不殘留可重試
func TestApplyInTxAtomicWithCallerRollback(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	credit(t, u, 500, fmt.Sprintf("x1-%d", u))

	tx := begin(t)
	_, err := svc.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: fmt.Sprintf("x1-bet-%d", u), RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -200, Reason: ledger.ReasonBetStake}},
	})
	if err != nil {
		t.Fatalf("ApplyInTx: %v", err)
	}
	// 模擬:領域寫入(注單)在同一 tx
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS fake_bets(user_id bigint)`); err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fake_bets VALUES ($1)`, u); err != nil {
		t.Fatalf("insert bet: %v", err)
	}
	// 呼叫端反悔
	_ = tx.Rollback(ctx)

	if bal, _ := svc.Balance(ctx, u, "coin"); bal != 500 {
		t.Fatalf("rollback 後餘額應回 500,得到 %d", bal)
	}
	var keys int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.idempotency_keys WHERE key=$1`,
		fmt.Sprintf("x1-bet-%d", u)).Scan(&keys)
	if keys != 0 {
		t.Fatal("rollback 後冪等鍵不應殘留")
	}
	// 同 key 重試要能成功(合法重試)
	tx2 := begin(t)
	if _, err := svc.ApplyInTx(ctx, tx2, ledger.ApplyParams{
		IdempotencyKey: fmt.Sprintf("x1-bet-%d", u), RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: -200, Reason: ledger.ReasonBetStake}},
	}); err != nil {
		t.Fatalf("重試應成功: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if bal, _ := svc.Balance(ctx, u, "coin"); bal != 300 {
		t.Fatalf("commit 後餘額應為 300,得到 %d", bal)
	}
}

// 撞已提交的冪等鍵:savepoint 回滾,重放結果經呼叫端 tx 讀回,呼叫端 tx 不被毒化
func TestApplyInTxReplayDoesNotPoisonCallerTx(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	key := fmt.Sprintf("x2-%d", u)

	// 先用一般 Apply 提交一筆
	first, err := svc.Apply(ctx, ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 100, Reason: ledger.ReasonDailyClaim}},
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	tx := begin(t)
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := svc.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "h",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 100, Reason: ledger.ReasonDailyClaim}},
	})
	if err != nil {
		t.Fatalf("重放: %v", err)
	}
	if !res.Replayed || res.Entries[0].EntryID != first.Entries[0].EntryID {
		t.Fatal("應回原分錄的重放結果")
	}
	// tx 沒被毒化:後續操作照常
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("caller tx 被毒化: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if bal, _ := svc.Balance(ctx, u, "coin"); bal != 100 {
		t.Fatalf("錢只能動一次,餘額 %d", bal)
	}
}

// 同 key 不同內容在 ApplyInTx 也要回衝突
func TestApplyInTxConflict(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	key := fmt.Sprintf("x3-%d", u)
	if _, err := svc.Apply(ctx, ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "hash-a",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 50, Reason: ledger.ReasonAdminGrant}},
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	tx := begin(t)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err := svc.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "hash-b",
		Ops: []ledger.Op{{UserID: u, Currency: "coin", Amount: 50, Reason: ledger.ReasonAdminGrant}},
	})
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("應回 ErrIdempotencyConflict,得到 %v", err)
	}
}
