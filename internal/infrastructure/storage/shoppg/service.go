// Package shoppg 是 shop.Service 的 Postgres 實作。
//
// 每個操作的全部效果——商品側寫入(entitlements / redemptions)、帳本分錄、餘額、
// outbox、冪等鍵——都在同一個 transaction,要嘛都在要嘛都不在。
//
// 冪等設計(一個概念一個權威):
//   - Purchase / RefundPurchase 沒有天然 UNIQUE 約束可擋(同人可重複購買至上限),
//     API 冪等鍵是權威:tx 內先鎖定串行化列(購買鎖 users 列、退款鎖 entitlement 列),
//     再查鍵——查到即重放,查不到就佔鍵並在 commit 前存入完整結果。
//   - HandleRedemption / CancelRedemption 的權威是「pending → 終態」的單向轉移
//     (列鎖下檢查 status),不另設 API 冪等鍵;重複處理回 ErrRedemptionNotPending。
//   - 帳本介面必填冪等鍵,故傳派生鍵(命名空間化的 API 鍵 + ":ledger",
//     或工單的決定性鍵)。派生鍵與商店側寫入同 tx 共存亡,只是滿足介面,
//     不構成第二套防重權威;若帳本回報 Replayed(鍵已有結果但商店側判定是首次),
//     即兩個權威矛盾,失敗出聲(ErrLedgerStateConflict),絕不回成功但錢不對。
//
// 冪等鍵命名空間(QA 高風險修正):idempotency_keys 是全平台單一 PK,客戶端
// 提供的鍵一律在服務端加前綴(purchaseKeyPrefix / refundKeyPrefix)後才佔鍵/查鍵/
// 進帳本。否則攻擊者可用字面 "shop.redemption:<id>:refund" 當 API 鍵搶佔內部
// 決定性鍵(工單 id 連續可推算、免費商品零成本),讓該工單的 reject/cancel
// 永遠撞鍵失敗、押款凍結。結構性隔離後,客戶端可控字串不可能撞到內部鍵
// (同一手法見 admineconpg 的 "adminecon:" 前綴)。
//
// 鎖序(全系統同向,避免死鎖):users → entitlements/redemptions → user_balances。
package shoppg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 分錄 ref_type 值:購買分錄指向履約物(退款憑此精確找回原分錄),退款分錄指向原分錄。
const (
	refTypeEntitlement = "entitlement"
	refTypeRedemption  = "redemption"
	refTypeTokenEntry  = "token_entry"
)

// 冪等鍵命名空間(見 package doc):客戶端鍵加前綴後才進 idempotency_keys,
// 與內部決定性鍵("shop.redemption:<id>:refund")結構性隔離。
const (
	purchaseKeyPrefix = "shop.purchase:api:"
	refundKeyPrefix   = "shop.refund:api:"
	// ledgerKeySuffix 是帳本派生鍵的字尾(派生鍵不構成第二套權威)。
	ledgerKeySuffix = ":ledger"
)

// txLedger 是本服務對帳本的最小依賴:與商店側寫入同 tx 動錢。
// 實際注入 *ledgerpg.Service(ApplyInTx 刻意不在 ledger.Ledger interface 上)。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// Service 是 shop.Service 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	led  txLedger
}

var _ shop.Service = (*Service)(nil)

// New 建立商店服務。led 傳 ledgerpg.New(pool) 的結果。
func New(pool *pgxpool.Pool, led txLedger) *Service {
	return &Service{pool: pool, q: db.New(pool), led: led}
}

// Purchase 在單一 transaction 內完成購買。流程:
//
//	鎖 users 列(同人購買串行化)→ 冪等重放檢查 → 佔冪等鍵
//	→ 讀商品 + 上架檢查(DB 時鐘)→ per_user_limit 檢查
//	→ 履約(INSERT entitlement / redemption)→ 扣款經 ApplyInTx(分錄 ref 指向履約物;
//	  price=0 跳過帳本)→ outbox → 存冪等 response → commit
//
// 履約刻意先於扣款:分錄的 ref_id 需要履約物的 id,同 tx 內順序不影響原子性,
// 而「扣款分錄精確指向 entitlement/redemption」讓退款不必靠「最近一筆」猜測。
func (s *Service) Purchase(ctx context.Context, p shop.PurchaseParams) (*shop.PurchaseResult, error) {
	if p.UserID <= 0 || p.ItemPublicID == "" || p.IdempotencyKey == "" {
		return nil, fmt.Errorf("user_id / item_public_id / idempotency_key 必填: %w", shop.ErrInvalidRequest)
	}
	hash := requestHash("shop.purchase", strconv.FormatInt(p.UserID, 10), p.ItemPublicID)
	// 客戶端鍵先收進服務端命名空間,結構上不可能撞到內部決定性鍵(見 package doc)。
	key := purchaseKeyPrefix + p.IdempotencyKey

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// ── 鎖 users 列:同一使用者的購買全序列化。per_user_limit 計數與冪等重放判定
	// 都要在前一筆購買 commit 之後才有意義(同 key 必同 user,見 requestHash)。
	if _, err := qtx.LockUserForShop(ctx, p.UserID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("user=%d: %w", p.UserID, shop.ErrUserNotFound)
		}
		return nil, fmt.Errorf("鎖使用者 %d: %w", p.UserID, err)
	}

	// ── 冪等:查到鍵即重放(在 user 鎖之後,前一次同 key 的 tx 已 commit 或已消失)
	if res, done, err := replayFromKey[shop.PurchaseResult](ctx, qtx, key, hash); done {
		return res, err
	}
	if err := qtx.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{
		Key: key, RequestHash: hash,
	}); err != nil {
		if isUniqueViolation(err, "idempotency_keys_pkey") {
			// user 鎖已擋掉同 user 的競態;走到這裡是同 key 被「不同 user/內容」重用
			// (hash 含 user 與 item,必然不同)→ 衝突。
			return nil, fmt.Errorf("key=%s: %w", key, ledger.ErrIdempotencyConflict)
		}
		return nil, fmt.Errorf("佔冪等鍵: %w", err)
	}

	// ── 商品:必須存在且上架中(is_listed 由 DB 時鐘計算)
	item, err := qtx.GetShopItemForPurchase(ctx, p.ItemPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("item=%s: %w", p.ItemPublicID, shop.ErrItemNotFound)
		}
		return nil, fmt.Errorf("讀商品 %s: %w", p.ItemPublicID, err)
	}
	if !item.IsListed {
		return nil, fmt.Errorf("item=%s: %w", p.ItemPublicID, shop.ErrItemNotListed)
	}
	if item.Price < 0 { // schema CHECK 已擋,防禦性再驗
		return nil, fmt.Errorf("item=%s price=%d: %w", p.ItemPublicID, item.Price, shop.ErrInvalidRequest)
	}

	// ── per_user_limit(全域 stock 在 M1 不啟用,schemas/08:搶購併發是白工)
	if item.PerUserLimit != nil {
		acquired, err := qtx.CountUserItemAcquisitions(ctx, db.CountUserItemAcquisitionsParams{
			UserID: p.UserID, ItemID: item.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("計算限購: %w", err)
		}
		if acquired >= int64(*item.PerUserLimit) {
			return nil, fmt.Errorf("item=%s 已持有 %d/上限 %d: %w",
				p.ItemPublicID, acquired, *item.PerUserLimit, shop.ErrPerUserLimitReached)
		}
	}

	// ── 履約 + 扣款 + outbox
	result := &shop.PurchaseResult{
		ItemPublicID: item.PublicID,
		Fulfillment:  shop.Fulfillment(item.Fulfillment),
		Currency:     item.Currency,
		Price:        item.Price,
	}
	switch f := shop.Fulfillment(item.Fulfillment); {
	case f.Auto():
		entPublicID, err := newULID()
		if err != nil {
			return nil, fmt.Errorf("產生權益 public_id: %w", err)
		}
		ent, err := qtx.InsertEntitlement(ctx, db.InsertEntitlementParams{
			PublicID: entPublicID,
			UserID:   p.UserID, ItemID: item.ID,
			DurationDays:        item.DurationDays,
			RefundWindowSeconds: item.RefundWindowSeconds,
		})
		if err != nil {
			return nil, fmt.Errorf("寫入權益: %w", err)
		}
		result.EntitlementID = ent.ID
		result.EntitlementPublicID = entPublicID
		result.ExpiresAt = ent.ExpiresAt
		result.RefundableUntil = ent.RefundableUntil

		if item.Price > 0 {
			entry, err := s.charge(ctx, tx, key+ledgerKeySuffix, hash, ledger.Op{
				UserID: p.UserID, Currency: item.Currency, Amount: -item.Price,
				Reason:  ledger.ReasonShopPurchase,
				RefType: ptr(refTypeEntitlement), RefID: ptr(ent.ID),
			})
			if err != nil {
				return nil, err
			}
			result.LedgerEntryID = entry.EntryID
		}
		if err := s.emit(ctx, qtx, "shop.purchased", map[string]any{
			"user_id":        p.UserID,
			"item_public_id": item.PublicID,
			"fulfillment":    item.Fulfillment,
			"entitlement_id": ent.ID,
			"currency":       item.Currency,
			"price":          item.Price,
		}); err != nil {
			return nil, err
		}

	case f == shop.FulfillManual:
		pubID, err := newULID()
		if err != nil {
			return nil, err
		}
		red, err := qtx.InsertRedemption(ctx, db.InsertRedemptionParams{
			PublicID: pubID, UserID: p.UserID, ItemID: item.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("寫入工單: %w", err)
		}
		result.RedemptionID = red.ID
		result.RedemptionPublicID = red.PublicID

		if item.Price > 0 {
			// 人工履約先押款:rejected / cancelled 才退(reason='redemption_refund')
			entry, err := s.charge(ctx, tx, key+ledgerKeySuffix, hash, ledger.Op{
				UserID: p.UserID, Currency: item.Currency, Amount: -item.Price,
				Reason:  ledger.ReasonRedemptionHold,
				RefType: ptr(refTypeRedemption), RefID: ptr(red.ID),
			})
			if err != nil {
				return nil, err
			}
			result.LedgerEntryID = entry.EntryID
		}
		if err := s.emit(ctx, qtx, "redemption.created", map[string]any{
			"redemption_public_id": red.PublicID,
			"user_id":              p.UserID,
			"item_public_id":       item.PublicID,
			"currency":             item.Currency,
			"price":                item.Price,
		}); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("item=%s 履約型態 %q 不在枚舉內: %w",
			p.ItemPublicID, item.Fulfillment, shop.ErrInvalidRequest)
	}

	if err := s.saveResponse(ctx, qtx, key, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// RefundPurchase 短窗口自助退款。流程:
//
//	鎖 entitlement 列(同一權益的退款串行化)→ 持有人檢查 → 冪等重放檢查 → 佔冪等鍵
//	→ 未撤銷 + 窗口內檢查(DB 時鐘)→ 找原扣款分錄(憑 ref_type='entitlement')
//	→ 退款 = 新正數分錄 reason='refund' ref 原分錄 → 撤銷權益 → outbox → 存 response → commit
//
// 退款金額以原分錄為準(-entry.amount),不看商品現價——商品價格事後調整不影響退款額。
// 免費購買(無分錄)只撤銷權益,不動帳本。
func (s *Service) RefundPurchase(ctx context.Context, p shop.RefundParams) (*shop.RefundResult, error) {
	if p.EntitlementID <= 0 || p.ByUserID <= 0 || p.IdempotencyKey == "" {
		return nil, fmt.Errorf("entitlement_id / by_user_id / idempotency_key 必填: %w", shop.ErrInvalidRequest)
	}
	hash := requestHash("shop.refund",
		strconv.FormatInt(p.EntitlementID, 10), strconv.FormatInt(p.ByUserID, 10))
	// 客戶端鍵先收進服務端命名空間(見 package doc)。
	key := refundKeyPrefix + p.IdempotencyKey

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	ent, err := qtx.LockEntitlementForRefund(ctx, p.EntitlementID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("entitlement=%d: %w", p.EntitlementID, shop.ErrEntitlementNotFound)
		}
		return nil, fmt.Errorf("鎖權益 %d: %w", p.EntitlementID, err)
	}
	if ent.UserID != p.ByUserID {
		return nil, fmt.Errorf("entitlement=%d 屬於 user=%d 而非 %d: %w",
			p.EntitlementID, ent.UserID, p.ByUserID, shop.ErrNotEntitlementOwner)
	}

	// ── 冪等重放檢查必須先於 revoked 檢查:前一次成功退款已撤銷權益,
	// 同 key 重試要回原結果,不能誤回 ErrAlreadyRevoked。
	if res, done, err := replayFromKey[shop.RefundResult](ctx, qtx, key, hash); done {
		return res, err
	}
	if err := qtx.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{
		Key: key, RequestHash: hash,
	}); err != nil {
		if isUniqueViolation(err, "idempotency_keys_pkey") {
			return nil, fmt.Errorf("key=%s: %w", key, ledger.ErrIdempotencyConflict)
		}
		return nil, fmt.Errorf("佔冪等鍵: %w", err)
	}

	if ent.RevokedAt != nil {
		return nil, fmt.Errorf("entitlement=%d: %w", p.EntitlementID, shop.ErrAlreadyRevoked)
	}
	// refundable_until NULL = 不可退(refund_window_seconds=0 的商品);窗口比對用 DB 時鐘。
	if ent.RefundableUntil == nil || !ent.DbNow.Before(*ent.RefundableUntil) {
		return nil, fmt.Errorf("entitlement=%d: %w", p.EntitlementID, shop.ErrRefundWindowClosed)
	}

	result := &shop.RefundResult{EntitlementID: ent.ID}

	// ── 原扣款分錄:Purchase 讓分錄 ref 指向 entitlement,一筆精確對應(見 shop.sql)。
	entry, err := qtx.GetPurchaseEntryForEntitlement(ctx, ptr(ent.ID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// 免費購買沒有分錄:無錢可退,僅撤銷權益。
	case err != nil:
		return nil, fmt.Errorf("找原扣款分錄: %w", err)
	default:
		amount := -entry.Amount
		if amount <= 0 {
			return nil, fmt.Errorf("entitlement=%d 原分錄 %d 金額 %d 非扣款: %w",
				ent.ID, entry.ID, entry.Amount, shop.ErrLedgerStateConflict)
		}
		res, err := s.charge(ctx, tx, key+ledgerKeySuffix, hash, ledger.Op{
			UserID: ent.UserID, Currency: entry.Currency, Amount: amount,
			Reason:  ledger.ReasonRefund,
			RefType: ptr(refTypeTokenEntry), RefID: ptr(entry.ID),
		})
		if err != nil {
			return nil, err
		}
		result.RefundEntryID = res.EntryID
		result.Amount = amount
		result.Currency = entry.Currency
	}

	if _, err := qtx.RevokeEntitlement(ctx, ent.ID); err != nil {
		return nil, fmt.Errorf("撤銷權益: %w", err)
	}
	// outbox:撤銷後下游要收回 Discord 身分組/站內權益,與到期回收共用消費端。
	if err := s.emit(ctx, qtx, "shop.refunded", map[string]any{
		"entitlement_id": ent.ID,
		"user_id":        ent.UserID,
		"item_public_id": ent.ItemPublicID,
		"amount":         result.Amount,
		"currency":       result.Currency,
	}); err != nil {
		return nil, err
	}
	if err := s.saveResponse(ctx, qtx, key, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// HandleRedemption 管理員處理工單:pending → fulfilled(不動錢,押款即最終扣款)
// 或 rejected(全額退)。防重權威是列鎖下的狀態檢查,非 pending 一律拒絕。
func (s *Service) HandleRedemption(ctx context.Context, p shop.HandleParams) (*shop.HandleResult, error) {
	if p.RedemptionID <= 0 || p.HandlerUserID <= 0 {
		return nil, fmt.Errorf("redemption_id / handler_user_id 必填: %w", shop.ErrInvalidRequest)
	}
	status := shop.RedemptionRejected
	if p.Approve {
		status = shop.RedemptionFulfilled
	}
	return s.settleRedemption(ctx, p.RedemptionID, status, p.HandlerUserID, p.Note, nil)
}

// CancelRedemption 使用者自行取消自己的 pending 工單 → cancelled + 全額退。
// handled_by 記為取消者本人(工單的終結者),與管理員處理共用欄位語意:誰結束了這張單。
func (s *Service) CancelRedemption(ctx context.Context, p shop.CancelParams) (*shop.HandleResult, error) {
	if p.RedemptionID <= 0 || p.ByUserID <= 0 {
		return nil, fmt.Errorf("redemption_id / by_user_id 必填: %w", shop.ErrInvalidRequest)
	}
	return s.settleRedemption(ctx, p.RedemptionID, shop.RedemptionCancelled, p.ByUserID, nil, &p.ByUserID)
}

// settleRedemption 是工單終結的共同流程(fulfilled / rejected / cancelled):
//
//	鎖 redemption 列 → (取消時)持有人檢查 → status='pending' 檢查(狀態機單向)
//	→ rejected/cancelled 時全額退(reason='redemption_refund' ref 原押款分錄,
//	  帳本鍵用決定性派生鍵 "shop.redemption:<id>:refund"——一張單終生至多退一次)
//	→ 更新 status/handled_by/handled_at → outbox 'redemption.handled' → commit
//
// mustOwner 非 nil 時要求工單屬於該使用者(自取消路徑)。
func (s *Service) settleRedemption(ctx context.Context, id int64, status shop.RedemptionStatus,
	handlerID int64, note *string, mustOwner *int64) (*shop.HandleResult, error) {

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	red, err := qtx.LockRedemptionForHandle(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("redemption=%d: %w", id, shop.ErrRedemptionNotFound)
		}
		return nil, fmt.Errorf("鎖工單 %d: %w", id, err)
	}
	if mustOwner != nil && red.UserID != *mustOwner {
		return nil, fmt.Errorf("redemption=%d 屬於 user=%d 而非 %d: %w",
			id, red.UserID, *mustOwner, shop.ErrNotRedemptionOwner)
	}
	if red.Status != string(shop.RedemptionPending) {
		return nil, fmt.Errorf("redemption=%d 狀態 %s: %w", id, red.Status, shop.ErrRedemptionNotPending)
	}

	result := &shop.HandleResult{
		RedemptionID:       red.ID,
		RedemptionPublicID: red.PublicID,
		Status:             status,
	}

	// ── rejected / cancelled → 全額退;fulfilled 不動錢(押款即最終扣款)。
	// 免費 manual 商品(無押款分錄)一律不動帳本。
	if status == shop.RedemptionRejected || status == shop.RedemptionCancelled {
		hold, err := qtx.GetHoldEntryForRedemption(ctx, ptr(red.ID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// 免費工單,無押款可退。
		case err != nil:
			return nil, fmt.Errorf("找押款分錄: %w", err)
		default:
			amount := -hold.Amount
			if amount <= 0 {
				return nil, fmt.Errorf("redemption=%d 押款分錄 %d 金額 %d 非扣款: %w",
					red.ID, hold.ID, hold.Amount, shop.ErrLedgerStateConflict)
			}
			// 決定性帳本鍵:一張單只可能發生一次退款(pending → 終態單向)。
			key := fmt.Sprintf("shop.redemption:%d:refund", red.ID)
			res, err := s.charge(ctx, tx, key, key, ledger.Op{
				UserID: red.UserID, Currency: hold.Currency, Amount: amount,
				Reason:  ledger.ReasonRedemptionRefund,
				RefType: ptr(refTypeTokenEntry), RefID: ptr(hold.ID),
				ActorID: ptr(handlerID),
			})
			if err != nil {
				return nil, err
			}
			result.RefundEntryID = res.EntryID
			result.RefundAmount = amount
		}
	}

	if _, err := qtx.UpdateRedemptionStatus(ctx, db.UpdateRedemptionStatusParams{
		ID: red.ID, Status: string(status), Note: note, HandledBy: ptr(handlerID),
	}); err != nil {
		return nil, fmt.Errorf("更新工單狀態: %w", err)
	}
	// cancelled 也走同一個 topic:消費端只認「工單已終結 + 終態」,不用分家(一個概念一個權威)。
	if err := s.emit(ctx, qtx, "redemption.handled", map[string]any{
		"redemption_public_id": red.PublicID,
		"user_id":              red.UserID,
		"item_public_id":       red.ItemPublicID,
		"status":               string(status),
		"handled_by":           handlerID,
		"refund_amount":        result.RefundAmount,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// ── 內部工具 ──

// charge 經帳本動一筆錢(同 tx)。ledKey 是完整帳本鍵:API 路徑傳「API 鍵 + ledgerKeySuffix」,
// 工單退款傳決定性鍵。帳本回 Replayed 代表鍵已有結果、商店側卻判定首次 → 兩個權威矛盾,失敗出聲。
func (s *Service) charge(ctx context.Context, tx pgx.Tx, ledKey, hash string, op ledger.Op) (*ledger.EntryResult, error) {
	res, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: ledKey,
		RequestHash:    hash,
		Ops:            []ledger.Op{op},
	})
	if err != nil {
		return nil, fmt.Errorf("帳本動錢: %w", err) // ErrInsufficientBalance 等原樣包裝透傳
	}
	if res.Replayed {
		return nil, fmt.Errorf("帳本鍵 %s 已有結果但商店側為首次: %w", ledKey, shop.ErrLedgerStateConflict)
	}
	return &res.Entries[0], nil
}

// replayFromKey 在已取得串行化鎖之後檢查 API 冪等鍵:
// done=true 表示不需(也不可)繼續執行——回重放結果或錯誤。
func replayFromKey[T any](ctx context.Context, qtx *db.Queries, key, hash string) (*T, bool, error) {
	rec, err := qtx.GetIdempotencyKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("讀冪等鍵: %w", err)
	}
	if rec.RequestHash != hash {
		return nil, true, fmt.Errorf("key=%s: %w", key, ledger.ErrIdempotencyConflict)
	}
	if rec.Response == nil {
		// 理論上到不了:同 key 的前一個 tx 若還在跑,我們會被串行化鎖擋在外面。保險分支。
		return nil, true, ledger.ErrInFlight
	}
	var result T
	if err := json.Unmarshal(rec.Response, &result); err != nil {
		return nil, true, fmt.Errorf("反序列化冪等結果: %w", err)
	}
	markReplayed(&result)
	return &result, true, nil
}

// markReplayed 設定結果的 Replayed 旗標(兩種結果型別都有)。
func markReplayed(v any) {
	switch r := v.(type) {
	case *shop.PurchaseResult:
		r.Replayed = true
	case *shop.RefundResult:
		r.Replayed = true
	}
}

// saveResponse 在 commit 前存入完整結果:commit 成功 ⇔ 鍵帶著結果存在。
func (s *Service) saveResponse(ctx context.Context, qtx *db.Queries, key string, result any) error {
	respJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("序列化結果: %w", err)
	}
	if err := qtx.SetIdempotencyResponse(ctx, db.SetIdempotencyResponseParams{
		Key: key, Response: respJSON,
	}); err != nil {
		return fmt.Errorf("存冪等結果: %w", err)
	}
	return nil
}

// emit 與領域變更同 tx 寫 outbox(ledger-invariants 第六條)。
func (s *Service) emit(ctx context.Context, qtx *db.Queries, topic string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化事件 %s: %w", topic, err)
	}
	if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{Topic: topic, Payload: raw}); err != nil {
		return fmt.Errorf("寫 outbox %s: %w", topic, err)
	}
	return nil
}

// requestHash 對請求內容取決定性雜湊(sha256 hex),偵測同 key 不同內容的重用。
func requestHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func ptr[T any](v T) *T { return &v }
