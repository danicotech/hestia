// Package admineconpg 是 adminecon.Service 的 Postgres 實作。
//
// 每個操作的全部效果——token_entries、user_balances、idempotency_keys、
// admin_audit_logs——都在同一個 transaction,要嘛都在要嘛都不在。
// 動作沒發生就不留稽核紀錄:Deduct 餘額不足時整個 tx 回滾,audit 一併消失。
package admineconpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// audit action 常數(schemas/03 的清單成員)。
const (
	actionGrant  = "grant_tokens"
	actionDeduct = "deduct_tokens"
	actionRefund = "refund"
)

// refTypeTokenEntry 是沖銷分錄的 ref_type(schemas/05:沖銷時指向原分錄)。
const refTypeTokenEntry = "token_entry"

// txLedger 是本服務對帳本的最小依賴:與 audit 寫入同 tx 動錢。
// 實際注入 *ledgerpg.Service(ApplyInTx 刻意不在 ledger.Ledger interface 上)。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// Service 是 adminecon.Service 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	led  txLedger
}

var _ adminecon.Service = (*Service)(nil)

// New 建立管理員經濟操作服務。led 傳 ledgerpg.New(pool) 的結果。
func New(pool *pgxpool.Pool, led txLedger) *Service {
	return &Service{pool: pool, q: db.New(pool), led: led}
}

// Grant 發點:正數分錄 + audit,同 tx。
func (s *Service) Grant(ctx context.Context, p adminecon.AdjustParams) (*adminecon.Result, error) {
	return s.adjust(ctx, p, +1, ledger.ReasonAdminGrant, actionGrant)
}

// Deduct 扣點:負數分錄 + audit,同 tx。餘額不足透傳 ledger.ErrInsufficientBalance。
func (s *Service) Deduct(ctx context.Context, p adminecon.AdjustParams) (*adminecon.Result, error) {
	return s.adjust(ctx, p, -1, ledger.ReasonAdminDeduct, actionDeduct)
}

// adjust 是 Grant / Deduct 的共同流程:
//
//	驗證 → 開 tx → ApplyInTx 動錢(冪等)→ 重放則直接回(不重寫 audit)
//	→ InsertAdminAudit(target 'user'/user_id)→ commit
func (s *Service) adjust(ctx context.Context, p adminecon.AdjustParams, sign int64,
	reason ledger.Reason, action string) (*adminecon.Result, error) {

	if err := validateAdjust(p); err != nil {
		return nil, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	amount := sign * p.Amount
	// 服務端加命名空間前綴:呼叫端的 key 不得污染 Refund 的保留鍵空間
	// refund:entry:{id}(QA 發現——否則惡意 key 可讓某分錄永久不可退)
	ledRes, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: "adminecon:" + p.IdempotencyKey,
		// hash 蓋住全部語意欄位:同 key 換內容(含換理由)→ ErrIdempotencyConflict
		RequestHash: requestHash(action, p),
		Ops: []ledger.Op{{
			UserID:   p.UserID,
			Currency: p.Currency,
			Amount:   amount,
			Reason:   reason,
			ActorID:  &p.ActorUserID,
		}},
	})
	if err != nil {
		return nil, err // 含 ledger.ErrInsufficientBalance 透傳;defer rollback → 零寫入
	}
	entry := ledRes.Entries[0]
	result := &adminecon.Result{
		EntryID:    entry.EntryID,
		UserID:     p.UserID,
		Currency:   p.Currency,
		Amount:     amount,
		NewBalance: entry.NewBalance,
		Replayed:   ledRes.Replayed,
	}
	if ledRes.Replayed {
		// 冪等重放:錢沒動、audit 原次已寫,本 tx 無任何寫入,rollback 即可。
		return result, nil
	}

	after, err := json.Marshal(map[string]any{
		"entry_id":    entry.EntryID,
		"user_id":     p.UserID,
		"currency":    p.Currency,
		"amount":      amount,
		"new_balance": entry.NewBalance,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化 audit after: %w", err)
	}
	targetType := "user"
	if _, err := s.q.WithTx(tx).InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: p.ActorUserID,
		Action:      action,
		TargetType:  &targetType,
		TargetID:    &p.UserID,
		After:       after,
		Reason:      p.Reason,
	}); err != nil {
		return nil, fmt.Errorf("寫稽核紀錄: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// Refund 萬能退款 = 沖銷分錄。流程:
//
//	驗證 → 開 tx → 讀原分錄(必須存在且 amount < 0)
//	→ ApplyInTx 寫正數分錄(決定性冪等鍵 refund:entry:{id},ref 指向原分錄)
//	→ Replayed = 已退過 → ErrAlreadyRefunded
//	→ InsertAdminAudit(target 'token_entry'/entry_id,before 帶原分錄摘要)→ commit
//
// 併發兩個退款:第二個的冪等鍵 INSERT 會等第一個 commit 後撞 PK → 重放 → 明確拒絕,
// 恰一成功由 idempotency_keys 的 PK 保證,不靠應用層先查後寫。
func (s *Service) Refund(ctx context.Context, p adminecon.RefundParams) (*adminecon.Result, error) {
	switch {
	case p.Reason == "":
		return nil, fmt.Errorf("refund entry=%d: %w", p.EntryID, adminecon.ErrReasonRequired)
	case p.ActorUserID <= 0:
		return nil, fmt.Errorf("actor_user_id 不合法: %w", adminecon.ErrInvalidInput)
	case p.EntryID <= 0:
		return nil, fmt.Errorf("entry_id 不合法: %w", adminecon.ErrInvalidInput)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	orig, err := qtx.GetTokenEntryByID(ctx, p.EntryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("entry=%d: %w", p.EntryID, adminecon.ErrEntryNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("讀原分錄: %w", err)
	}
	if orig.Amount >= 0 {
		return nil, fmt.Errorf("entry=%d amount=%d: %w",
			p.EntryID, orig.Amount, adminecon.ErrNotRefundable)
	}
	refundAmount := -orig.Amount // 取絕對值,退回正數分錄

	// 決定性冪等鍵:同一原分錄永遠同 key。這是「一分錄只退一次」的唯一權威;
	// hash 用同一字串(dailypg 前例)——第二次退款無論理由為何都走重放,不是 409。
	idemKey := fmt.Sprintf("refund:entry:%d", p.EntryID)
	refType := refTypeTokenEntry
	ledRes, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: idemKey,
		RequestHash:    idemKey,
		Ops: []ledger.Op{{
			UserID:   orig.UserID,
			Currency: orig.Currency,
			Amount:   refundAmount,
			Reason:   ledger.ReasonRefund,
			RefType:  &refType,
			RefID:    &p.EntryID,
			ActorID:  &p.ActorUserID,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("退款動錢: %w", err)
	}
	if ledRes.Replayed {
		// 撞決定性鍵 = 這筆分錄已經退過。錢沒動、本 tx 零寫入,回明確錯誤。
		return nil, fmt.Errorf("entry=%d: %w", p.EntryID, adminecon.ErrAlreadyRefunded)
	}
	entry := ledRes.Entries[0]

	before, err := json.Marshal(map[string]any{ // 原分錄摘要
		"entry_id":   orig.ID,
		"user_id":    orig.UserID,
		"currency":   orig.Currency,
		"amount":     orig.Amount,
		"reason":     orig.Reason,
		"created_at": orig.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化 audit before: %w", err)
	}
	after, err := json.Marshal(map[string]any{ // 沖銷分錄摘要
		"entry_id":    entry.EntryID,
		"amount":      refundAmount,
		"new_balance": entry.NewBalance,
		"ref_type":    refTypeTokenEntry,
		"ref_id":      p.EntryID,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化 audit after: %w", err)
	}
	targetType := refTypeTokenEntry
	if _, err := qtx.InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: p.ActorUserID,
		Action:      actionRefund,
		TargetType:  &targetType,
		TargetID:    &p.EntryID,
		Before:      before,
		After:       after,
		Reason:      p.Reason,
	}); err != nil {
		return nil, fmt.Errorf("寫稽核紀錄: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &adminecon.Result{
		EntryID:    entry.EntryID,
		UserID:     orig.UserID,
		Currency:   orig.Currency,
		Amount:     refundAmount,
		NewBalance: entry.NewBalance,
	}, nil
}

func validateAdjust(p adminecon.AdjustParams) error {
	switch {
	case p.Reason == "":
		return fmt.Errorf("user=%d: %w", p.UserID, adminecon.ErrReasonRequired)
	case p.IdempotencyKey == "":
		return fmt.Errorf("IdempotencyKey 必填: %w", adminecon.ErrInvalidInput)
	case p.ActorUserID <= 0:
		return fmt.Errorf("actor_user_id 不合法: %w", adminecon.ErrInvalidInput)
	case p.UserID <= 0:
		return fmt.Errorf("user_id 不合法: %w", adminecon.ErrInvalidInput)
	case p.Currency == "":
		return fmt.Errorf("currency 必填: %w", adminecon.ErrInvalidInput)
	case p.Amount <= 0:
		return fmt.Errorf("amount=%d 必須為正數: %w", p.Amount, adminecon.ErrInvalidInput)
	}
	return nil
}

// requestHash 蓋住請求全部語意欄位:同 key 不同內容 → ledger.ErrIdempotencyConflict。
func requestHash(action string, p adminecon.AdjustParams) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d|%s|%d|%s",
		action, p.ActorUserID, p.UserID, p.Currency, p.Amount, p.Reason))
	return hex.EncodeToString(h[:])
}
