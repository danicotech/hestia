// Package adminecon 定義管理員經濟操作(發點 / 扣點 / 萬能退款)的核心型別、錯誤與介面。
//
// 設計核心(schemas/05-ledger.md、schemas/03-authz.md):
//   - 三個操作都是「動錢 + admin_audit_logs 稽核」單一 transaction,要嘛都在要嘛都不在
//   - Reason 空字串一律拒絕 —— admin_audit_logs.reason NOT NULL 是刻意設計,
//     強迫管理員在動作當下寫理由
//   - 退款 = 沖銷分錄:新增正數分錄指向原分錄(ref_type='token_entry'),絕不改舊紀錄
//   - 同一原分錄只能退一次:權威是帳本的決定性冪等鍵 refund:entry:{id},
//     不做應用層先查後寫的第二套檢查(一個概念一個權威)
package adminecon

import (
	"context"
	"errors"
)

// AdjustParams 是 Grant / Deduct 共用的參數。Amount 一律為正數:
// Grant = 發多少、Deduct = 扣多少,方向由呼叫的方法決定,送進帳本時才帶正負號。
type AdjustParams struct {
	// IdempotencyKey 必填(API 動錢鐵則):同 key 重送錢只動一次。
	IdempotencyKey string
	ActorUserID    int64 // 執行操作的管理員;寫進 token_entries.actor_id 與 audit log
	UserID         int64 // 被發點 / 扣點的使用者
	Currency       string
	Amount         int64  // 必須 > 0
	Reason         string // 寫進 admin_audit_logs.reason;空字串拒絕(刻意)
}

// RefundParams 是萬能退款的參數。沒有 IdempotencyKey 欄位是刻意的:
// 退款的冪等權威是由 EntryID 導出的決定性鍵(refund:entry:{id}),
// 呼叫端再提供一把 key 就是第二套權威。
type RefundParams struct {
	ActorUserID int64  // 執行退款的管理員
	EntryID     int64  // 要沖銷的原分錄;必須存在且 amount < 0(只有扣款能退)
	Reason      string // 寫進 admin_audit_logs.reason;空字串拒絕(刻意)
}

// Result 是一次操作的結果。
type Result struct {
	EntryID    int64 // 本次新增的分錄 id(Replayed 時為原次的分錄)
	UserID     int64
	Currency   string
	Amount     int64 // 分錄實際金額:Grant 正、Deduct 負、Refund 正
	NewBalance int64 // 分錄寫入後的餘額(Replayed 時為原次執行當下的值)
	Replayed   bool  // true = 冪等重放,本次未實際動錢、未新增稽核紀錄
}

// 錯誤語意:呼叫端據此決定回應。餘額不足直接透傳 ledger.ErrInsufficientBalance。
var (
	// ErrReasonRequired 表示 Reason 是空字串。強迫寫理由是刻意設計(schemas/03)。
	ErrReasonRequired = errors.New("必須填寫操作理由")
	// ErrInvalidInput 表示其他參數不合法(金額非正數、缺 id、缺冪等鍵…)。
	ErrInvalidInput = errors.New("參數不合法")
	// ErrEntryNotFound 表示要退款的原分錄不存在。
	ErrEntryNotFound = errors.New("找不到原分錄")
	// ErrNotRefundable 表示原分錄不是扣款(amount >= 0),不能退。
	ErrNotRefundable = errors.New("只有扣款分錄能退款")
	// ErrAlreadyRefunded 表示同一原分錄已退過款(撞決定性冪等鍵)。
	ErrAlreadyRefunded = errors.New("此分錄已退過款")
)

// Service 是管理員經濟操作的對外介面。M1 為 in-process 實作(admineconpg)。
type Service interface {
	// Grant 發點:正數分錄 reason='admin_grant',audit action='grant_tokens'。
	Grant(ctx context.Context, p AdjustParams) (*Result, error)
	// Deduct 扣點:負數分錄 reason='admin_deduct',audit action='deduct_tokens'。
	// 餘額不足回 ledger.ErrInsufficientBalance,整個 tx 回滾(audit 也不留)。
	Deduct(ctx context.Context, p AdjustParams) (*Result, error)
	// Refund 萬能退款:讀原分錄(必須 amount < 0),新增等額正數分錄
	// reason='refund'、ref 指向原分錄。同一分錄第二次退款回 ErrAlreadyRefunded。
	Refund(ctx context.Context, p RefundParams) (*Result, error)
}
