package ledgerpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Service 是 ledger.Ledger 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

var _ ledger.Ledger = (*Service)(nil)

// New 建立帳本服務。
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

// Apply 原子執行一組分錄。流程(順序即鐵則):
//
//	驗證 → 開 tx → 先佔冪等鍵 → 依 user_id 升冪鎖餘額 → 檢查充足
//	→ 寫分錄 + 更新餘額 → 寫 outbox → 存冪等 response → commit
func (s *Service) Apply(ctx context.Context, p ledger.ApplyParams) (*ledger.ApplyResult, error) {
	if err := validate(p); err != nil {
		return nil, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// ── 冪等:tx 一開始就佔 key(response 先 NULL)──
	// 併發同 key 的第二個 tx 會在這裡阻塞,等第一個 commit 後撞 PK;
	// tx rollback 時 key 同步消失,合法重試不會被擋。
	err = qtx.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{
		Key:         p.IdempotencyKey,
		RequestHash: p.RequestHash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// key 已存在:先 rollback 釋放本 tx 的連線,再走重放路徑。
			// 不先釋放的話,goroutine 佔著廢棄連線又向池要第二條(hold-and-wait),
			// 併發重複請求 ≥ MaxConns 時整個連線池死鎖(QA 實測復現)。
			_ = tx.Rollback(ctx)
			return s.replay(ctx, p)
		}
		return nil, fmt.Errorf("佔冪等鍵: %w", err)
	}

	// ── 鎖餘額:收集 distinct (user, currency),依 user_id 升冪、再 currency 升冪 ──
	type pair struct {
		userID   int64
		currency string
	}
	seen := map[pair]int64{} // pair → 本次淨變動
	var order []pair
	for _, op := range p.Ops {
		k := pair{op.UserID, op.Currency}
		if _, ok := seen[k]; !ok {
			order = append(order, k)
		}
		seen[k] += op.Amount
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].userID != order[j].userID {
			return order[i].userID < order[j].userID
		}
		return order[i].currency < order[j].currency
	})

	locked := map[pair]int64{} // pair → 鎖定當下餘額
	for _, k := range order {
		if err := qtx.EnsureBalanceRow(ctx, db.EnsureBalanceRowParams{
			UserID: k.userID, Currency: k.currency,
		}); err != nil {
			return nil, fmt.Errorf("確保餘額列 user=%d: %w", k.userID, err)
		}
		bal, err := qtx.LockBalanceForUpdate(ctx, db.LockBalanceForUpdateParams{
			UserID: k.userID, Currency: k.currency,
		})
		if err != nil {
			return nil, fmt.Errorf("鎖餘額 user=%d: %w", k.userID, err)
		}
		locked[k] = bal
	}

	// ── 主要檢查:淨變動後不得為負(CHECK 約束是最後防線,不是這裡的替代品)──
	for _, k := range order {
		if locked[k]+seen[k] < 0 {
			return nil, fmt.Errorf("user=%d currency=%s 餘額 %d 不足以承受變動 %d: %w",
				k.userID, k.currency, locked[k], seen[k], ledger.ErrInsufficientBalance)
		}
	}

	// ── 寫分錄 + 更新餘額(同 tx)──
	result := &ledger.ApplyResult{}
	for _, op := range p.Ops {
		row, err := qtx.InsertTokenEntry(ctx, db.InsertTokenEntryParams{
			UserID:   op.UserID,
			Currency: op.Currency,
			Amount:   op.Amount,
			Reason:   string(op.Reason),
			RefType:  op.RefType,
			RefID:    op.RefID,
			ActorID:  op.ActorID,
		})
		if err != nil {
			return nil, fmt.Errorf("寫分錄: %w", err)
		}
		newBal, err := qtx.ApplyBalanceDelta(ctx, db.ApplyBalanceDeltaParams{
			UserID: op.UserID, Currency: op.Currency, Balance: op.Amount,
		})
		if err != nil {
			if isCheckViolation(err) { // 逐筆套用時中間值變負(淨額檢查抓不到的排列)
				return nil, fmt.Errorf("中間餘額為負: %w", ledger.ErrInsufficientBalance)
			}
			return nil, fmt.Errorf("更新餘額: %w", err)
		}
		result.Entries = append(result.Entries, ledger.EntryResult{
			EntryID: row.ID, UserID: op.UserID, Currency: op.Currency, NewBalance: newBal,
		})
	}

	// ── outbox(同 tx,鐵則第六條)──
	for _, ev := range p.Events {
		if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
			Topic: ev.Topic, Payload: ev.Payload,
		}); err != nil {
			return nil, fmt.Errorf("寫 outbox: %w", err)
		}
	}

	// ── 存冪等 response(同 tx:commit 成功 ⇔ key 帶著完整結果存在)──
	respJSON, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("序列化結果: %w", err)
	}
	if err := qtx.SetIdempotencyResponse(ctx, db.SetIdempotencyResponseParams{
		Key: p.IdempotencyKey, Response: respJSON,
	}); err != nil {
		return nil, fmt.Errorf("存冪等結果: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// replay 處理冪等鍵已存在的情況:內容相同 → 回上次結果;不同 → 衝突。
func (s *Service) replay(ctx context.Context, p ledger.ApplyParams) (*ledger.ApplyResult, error) {
	rec, err := s.q.GetIdempotencyKey(ctx, p.IdempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("讀冪等鍵: %w", err)
	}
	if rec.RequestHash != p.RequestHash {
		return nil, fmt.Errorf("key=%s: %w", p.IdempotencyKey, ledger.ErrIdempotencyConflict)
	}
	if rec.Response == nil {
		// key 在但 response 還沒寫:對方 tx 仍在進行(理論上撞 PK 前會先被鎖擋住,
		// 這裡是保險分支)。回可重試錯誤,不回成功。
		return nil, ledger.ErrInFlight
	}
	var result ledger.ApplyResult
	if err := json.Unmarshal(rec.Response, &result); err != nil {
		return nil, fmt.Errorf("反序列化冪等結果: %w", err)
	}
	result.Replayed = true
	return &result, nil
}

// Balance 查即時餘額;無餘額列視為 0。
func (s *Service) Balance(ctx context.Context, userID int64, currency string) (int64, error) {
	bal, err := s.q.GetBalance(ctx, db.GetBalanceParams{UserID: userID, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return bal, nil
}

// Reconcile 對帳:SUM(entries) 對 balance 的全量比對。
func (s *Service) Reconcile(ctx context.Context) ([]ledger.Mismatch, error) {
	rows, err := s.q.ReconcileBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Mismatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, ledger.Mismatch{
			UserID: r.UserID, Currency: r.Currency,
			Balance: r.Balance, EntryTotal: r.EntryTotal,
		})
	}
	return out, nil
}

func validate(p ledger.ApplyParams) error {
	if p.IdempotencyKey == "" {
		return fmt.Errorf("IdempotencyKey 必填: %w", ledger.ErrInvalidOp)
	}
	if p.RequestHash == "" {
		return fmt.Errorf("RequestHash 必填: %w", ledger.ErrInvalidOp)
	}
	if len(p.Ops) == 0 {
		return fmt.Errorf("至少要有一筆 Op: %w", ledger.ErrInvalidOp)
	}
	for i, op := range p.Ops {
		switch {
		case op.Amount == 0:
			return fmt.Errorf("ops[%d]: amount 不可為 0: %w", i, ledger.ErrInvalidOp)
		case op.UserID <= 0:
			return fmt.Errorf("ops[%d]: user_id 不合法: %w", i, ledger.ErrInvalidOp)
		case op.Currency == "":
			return fmt.Errorf("ops[%d]: currency 必填: %w", i, ledger.ErrInvalidOp)
		case !op.Reason.Valid():
			return fmt.Errorf("ops[%d]: reason %q 不在枚舉內: %w", i, op.Reason, ledger.ErrInvalidOp)
		}
	}
	for i, ev := range p.Events {
		if ev.Topic == "" {
			return fmt.Errorf("events[%d]: topic 必填: %w", i, ledger.ErrInvalidOp)
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
