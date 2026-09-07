// Package shop 定義商店購買、權益(entitlements)、兌換工單(redemptions)與退款的
// 核心型別、錯誤與介面。
//
// 設計核心(schemas/08-shop.md):
//   - 三種履約型態、兩張表:auto_role / auto_perk 產生 entitlements(持續為真的狀態);
//     manual 產生 redemptions(一次性工單,pending → fulfilled / rejected / cancelled,單向)
//   - 退款政策全參數化:refund_window_seconds 逐商品設定(0 = 不可退),
//     refundable_until 在購買當下算好存欄位,不現算
//   - 退款一律是新的正數分錄(ledger-invariants):
//     短窗口自助退 reason='refund';工單被拒/取消 reason='redemption_refund'
//   - manual 商品購買即押款(reason='redemption_hold'),fulfilled 不再動錢,
//     rejected / cancelled 才全額退
//
// 錯誤透傳:餘額不足、冪等衝突等帳本層錯誤(ledger.ErrInsufficientBalance、
// ledger.ErrIdempotencyConflict、ledger.ErrInFlight)由實作原樣包裝上拋,
// 呼叫端用 errors.Is 判斷;本套件依分層規範只認標準庫,不重複定義同義錯誤。
package shop

import (
	"context"
	"errors"
	"time"
)

// Fulfillment 是商品履約型態的封閉枚舉(schemas/08)。
type Fulfillment string

const (
	// FulfillAutoRole 自動發 Discord 身分組(可限時)。
	FulfillAutoRole Fulfillment = "auto_role"
	// FulfillAutoPerk 自動生效的站內權益(可限時)。
	FulfillAutoPerk Fulfillment = "auto_perk"
	// FulfillManual 人工履約:產生工單,管理員核可後才算交付。
	FulfillManual Fulfillment = "manual"
)

// Auto 回報是否為自動履約(產生 entitlement 而非工單)。
func (f Fulfillment) Auto() bool { return f == FulfillAutoRole || f == FulfillAutoPerk }

// RedemptionStatus 是工單狀態的封閉枚舉,只允許 pending → 三個終態,單向不可逆。
type RedemptionStatus string

const (
	RedemptionPending   RedemptionStatus = "pending"
	RedemptionFulfilled RedemptionStatus = "fulfilled"
	RedemptionRejected  RedemptionStatus = "rejected"
	RedemptionCancelled RedemptionStatus = "cancelled"
)

// PurchaseParams 是一次購買請求。
type PurchaseParams struct {
	UserID       int64
	ItemPublicID string
	// IdempotencyKey 必填:購買沒有天然 UNIQUE 約束可擋
	// (同人可重複購買至 per_user_limit),API 冪等鍵是防連點的權威。
	IdempotencyKey string
}

// PurchaseResult 是一次購買的完整結果;冪等重放時原樣回傳(JSON 序列化存於冪等鍵)。
type PurchaseResult struct {
	ItemPublicID string      `json:"item_public_id"`
	Fulfillment  Fulfillment `json:"fulfillment"`
	Currency     string      `json:"currency"`
	Price        int64       `json:"price"`
	// LedgerEntryID 是扣款分錄 id;0 = 免費商品未動帳本。
	LedgerEntryID int64 `json:"ledger_entry_id"`

	// auto_* 履約:
	EntitlementID       int64      `json:"entitlement_id"`                  // 內部 id;冪等 response 序列化需要,對外遮蔽是 transport DTO 的責任
	EntitlementPublicID string     `json:"entitlement_public_id,omitempty"` // 對外定址用;manual 購買為空
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`            // nil = 永久
	RefundableUntil     *time.Time `json:"refundable_until,omitempty"`      // nil = 不可退

	// manual 履約:
	RedemptionID       int64  `json:"redemption_id"` // 0 = auto 購買
	RedemptionPublicID string `json:"redemption_public_id,omitempty"`

	// Replayed = true 表示冪等重放,本次未實際動錢/寫入。
	Replayed bool `json:"-"`
}

// RefundParams 是短窗口自助退款請求(僅限 auto_* 產生的 entitlement)。
type RefundParams struct {
	EntitlementID  int64
	ByUserID       int64 // 必須是權益持有人
	IdempotencyKey string
}

// RefundResult 是一次退款的結果;冪等重放時原樣回傳。
type RefundResult struct {
	EntitlementID int64 `json:"entitlement_id"`
	// RefundEntryID 是退款(正數)分錄 id;0 = 免費購買無錢可退,僅撤銷權益。
	RefundEntryID int64  `json:"refund_entry_id"`
	Amount        int64  `json:"amount"` // 退回金額(≥ 0)
	Currency      string `json:"currency,omitempty"`
	Replayed      bool   `json:"-"`
}

// HandleParams 是管理員處理工單的請求。
// 刻意沒有冪等鍵:pending → 終態的單向轉移在列鎖下本身就是防重權威,
// 再加冪等鍵就是同一概念兩個權威(專案第 9 條)。重複處理回 ErrRedemptionNotPending。
type HandleParams struct {
	RedemptionID  int64
	HandlerUserID int64
	Approve       bool    // true → fulfilled(不動錢,押款即最終扣款);false → rejected(全額退)
	Note          *string // 選填,覆寫工單備註
}

// CancelParams 是使用者自行取消 pending 工單的請求(全額退)。同樣不需冪等鍵。
type CancelParams struct {
	RedemptionID int64
	ByUserID     int64 // 必須是工單建立者
}

// HandleResult 是工單處理/取消的結果。
type HandleResult struct {
	RedemptionID       int64
	RedemptionPublicID string
	Status             RedemptionStatus
	// RefundEntryID / RefundAmount:rejected / cancelled 時的退款分錄;fulfilled 恆為 0。
	RefundEntryID int64
	RefundAmount  int64
}

// 錯誤語意:呼叫端據此決定回應文案(都是 4xx 級,不該盲目重試)。
var (
	// ErrInvalidRequest 表示請求參數不合法(缺 id、缺冪等鍵等)。
	ErrInvalidRequest = errors.New("請求參數不合法")
	// ErrUserNotFound 表示使用者不存在或已刪除。
	ErrUserNotFound = errors.New("使用者不存在")
	// ErrItemNotFound 表示商品不存在。
	ErrItemNotFound = errors.New("商品不存在")
	// ErrItemNotListed 表示商品尚未上架、上架時間未到或已下架(絕版制)。
	ErrItemNotListed = errors.New("商品未上架或已下架")
	// ErrPerUserLimitReached 表示已達每人限購上限
	// (口徑:未撤銷的 entitlements + 非 cancelled/rejected 的 redemptions)。
	ErrPerUserLimitReached = errors.New("已達每人限購上限")
	// ErrEntitlementNotFound 表示權益不存在。
	ErrEntitlementNotFound = errors.New("權益不存在")
	// ErrNotEntitlementOwner 表示權益不屬於請求的使用者。
	ErrNotEntitlementOwner = errors.New("權益不屬於該使用者")
	// ErrAlreadyRevoked 表示權益已撤銷(含已退款),不能重複退。
	ErrAlreadyRevoked = errors.New("權益已撤銷")
	// ErrRefundWindowClosed 表示已過退款窗口(或商品設定為不可退)。
	ErrRefundWindowClosed = errors.New("已過退款窗口")
	// ErrRedemptionNotFound 表示工單不存在。
	ErrRedemptionNotFound = errors.New("兌換工單不存在")
	// ErrNotRedemptionOwner 表示工單不屬於請求的使用者(自取消時)。
	ErrNotRedemptionOwner = errors.New("兌換工單不屬於該使用者")
	// ErrRedemptionNotPending 表示工單已離開 pending(狀態機單向,重複處理一律拒絕)。
	ErrRedemptionNotPending = errors.New("兌換工單已處理")
	// ErrLedgerStateConflict 表示商店紀錄與帳本狀態矛盾(如:工單 pending 但退款
	// 冪等鍵已有結果)。異常狀態必須失敗出聲,不能回成功但錢不對。
	ErrLedgerStateConflict = errors.New("商店紀錄與帳本狀態矛盾")
)

// Service 是商店的對外介面。M1 為 in-process 實作(shoppg)。
// 每個方法的全部效果——商品側寫入、帳本分錄、餘額、outbox——都在單一 transaction。
type Service interface {
	// Purchase 購買商品:上架與限購檢查 → 履約(entitlement 或 pending 工單)
	// → 經帳本扣款(auto_* reason='shop_purchase';manual reason='redemption_hold';
	// 免費商品不動帳本)→ outbox(shop.purchased / redemption.created)。
	Purchase(ctx context.Context, p PurchaseParams) (*PurchaseResult, error)

	// RefundPurchase 短窗口自助退款:權益須屬於該使用者、未撤銷、仍在
	// refundable_until 窗口內。退款是新的正數分錄(reason='refund',ref 指向
	// 原扣款分錄),同 tx 撤銷權益(revoked_at)。過窗口回 ErrRefundWindowClosed。
	RefundPurchase(ctx context.Context, p RefundParams) (*RefundResult, error)

	// HandleRedemption 管理員處理 pending 工單:
	// Approve=true → fulfilled(不動錢,購買時的押款即最終扣款);
	// Approve=false → rejected(全額退,reason='redemption_refund',ref 指向原押款分錄)。
	// 同 tx 更新 status/handled_by/handled_at + outbox 'redemption.handled'。
	HandleRedemption(ctx context.Context, p HandleParams) (*HandleResult, error)

	// CancelRedemption 使用者自行取消自己的 pending 工單 → cancelled + 全額退
	// (同 reason='redemption_refund')。
	CancelRedemption(ctx context.Context, p CancelParams) (*HandleResult, error)
}
