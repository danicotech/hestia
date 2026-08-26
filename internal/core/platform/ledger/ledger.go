// Package ledger 是代幣帳本的唯一寫入入口。
//
// 鐵則(完整版見 .claude/skills/ledger-invariants):
//   - token_entries append-only,錯帳用沖銷分錄
//   - 分錄 + 餘額 + outbox 同一個 transaction
//   - 動錢一律冪等(Idempotency-Key)
//   - 多方操作鎖順序 = user_id 升冪
//
// activity 層(下注、派彩)不直接寫帳本,只依賴這裡的 Ledger interface(規劃書 §37)。
package ledger

import (
	"context"
	"encoding/json"
	"errors"
)

// Reason 是分錄事由的封閉枚舉(grill Q12 定案)。不要發明新的近義詞;
// 要加新事由先更新 schema 文件 §5 與這份清單。
type Reason string

const (
	ReasonSignupBonus      Reason = "signup_bonus"
	ReasonDailyClaim       Reason = "daily_claim"
	ReasonDailyMakeup      Reason = "daily_makeup"
	ReasonAdminGrant       Reason = "admin_grant"
	ReasonAdminDeduct      Reason = "admin_deduct"
	ReasonShopPurchase     Reason = "shop_purchase"
	ReasonRefund           Reason = "refund"
	ReasonRedemptionHold   Reason = "redemption_hold"
	ReasonRedemptionRefund Reason = "redemption_refund"
	ReasonMarketPurchase   Reason = "market_purchase"
	ReasonMarketSaleIncome Reason = "market_sale_income"
	ReasonMarketFee        Reason = "market_fee"
	ReasonTradeSettle      Reason = "trade_settle"
	ReasonBetStake         Reason = "bet_stake"
	ReasonBetPayout        Reason = "bet_payout"
	ReasonBetRefund        Reason = "bet_refund"
	ReasonTournamentPrize  Reason = "tournament_prize"
	ReasonEventReward      Reason = "event_reward"
)

var validReasons = map[Reason]struct{}{
	ReasonSignupBonus: {}, ReasonDailyClaim: {}, ReasonDailyMakeup: {},
	ReasonAdminGrant: {}, ReasonAdminDeduct: {}, ReasonShopPurchase: {},
	ReasonRefund: {}, ReasonRedemptionHold: {}, ReasonRedemptionRefund: {},
	ReasonMarketPurchase: {}, ReasonMarketSaleIncome: {}, ReasonMarketFee: {},
	ReasonTradeSettle: {}, ReasonBetStake: {}, ReasonBetPayout: {},
	ReasonBetRefund: {}, ReasonTournamentPrize: {}, ReasonEventReward: {},
}

// Valid 回報 reason 是否在枚舉內。
func (r Reason) Valid() bool { _, ok := validReasons[r]; return ok }

// Op 是一筆分錄操作。Amount 正 = 入帳,負 = 扣款。
type Op struct {
	UserID   int64
	Currency string
	Amount   int64 // 絕不使用浮點;0 不合法
	Reason   Reason
	RefType  *string
	RefID    *int64
	ActorID  *int64 // 管理員操作時填
}

// Event 是要與本次動錢同 transaction 寫入的 outbox 事件。
type Event struct {
	Topic   string
	Payload json.RawMessage
}

// ApplyParams 是一次原子動錢請求:全部 Op + 全部 Event 要嘛都成立要嘛都不成立。
type ApplyParams struct {
	// IdempotencyKey 必填。同 key 重送直接回上次結果;同 key 不同內容回 ErrIdempotencyConflict。
	IdempotencyKey string
	// RequestHash 由呼叫端對請求內容取雜湊,用於偵測同 key 不同內容。
	RequestHash string
	Ops         []Op
	Events      []Event
}

// EntryResult 是一筆已寫入分錄的結果。
type EntryResult struct {
	EntryID    int64  `json:"entry_id"`
	UserID     int64  `json:"user_id"`
	Currency   string `json:"currency"`
	NewBalance int64  `json:"new_balance"`
}

// ApplyResult 是一次動錢的完整結果;冪等重放時原樣回傳。
type ApplyResult struct {
	Entries  []EntryResult `json:"entries"`
	Replayed bool          `json:"-"` // true = 冪等重放,未實際動錢
}

// Mismatch 是對帳發現的不一致。
type Mismatch struct {
	UserID     int64
	Currency   string
	Balance    int64 // user_balances 快取值
	EntryTotal int64 // SUM(token_entries) 真值
}

// 錯誤語意:呼叫端據此決定回 4xx 還是重試。
var (
	ErrInsufficientBalance = errors.New("餘額不足")
	ErrIdempotencyConflict = errors.New("冪等鍵已存在但請求內容不同")
	ErrInvalidOp           = errors.New("操作不合法")
	ErrInFlight            = errors.New("同一冪等鍵的請求正在處理中")
)

// Ledger 是帳本的對外介面。M1 為 in-process 實作;
// 未來拆服務時換成 HTTP client,呼叫端(activity 層)一行不改。
type Ledger interface {
	// Apply 原子執行一組分錄(+ outbox 事件),冪等。
	Apply(ctx context.Context, p ApplyParams) (*ApplyResult, error)
	// Balance 查詢即時餘額(無餘額列視為 0)。
	Balance(ctx context.Context, userID int64, currency string) (int64, error)
	// Reconcile 對帳:回傳所有 SUM(entries) != balance 的組合。空清單 = 帳是平的。
	Reconcile(ctx context.Context) ([]Mismatch, error)
}
