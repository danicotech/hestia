// Package daily 定義每日簽到的核心型別、錯誤與介面。
//
// 設計核心(schemas/07-daily.md):
//   - 日界線以使用者自己的時區計算(claim_date = 當地日期)
//   - 防連點的最終保證是 daily_claims 的 PK (user_id, claim_date),不靠應用層檢查
//   - 防改時區濫用:改時區後下次簽到仍需距上次 ≥ timezone_change_min_gap_hours
package daily

import (
	"context"
	"errors"
)

// Result 是一次成功簽到的結果,呼叫端(Bot / 前端)直接顯示。
type Result struct {
	Amount int64 // 本次發放金額
	Streak int32 // 更新後的連續天數
}

// 錯誤語意:呼叫端據此決定回應文案(都是 4xx 級,不該重試)。
var (
	// ErrAlreadyClaimed 表示當地今天已簽過。
	// 權威判定是 daily_claims 的 PK 撞鍵,不是應用層先查後寫。
	ErrAlreadyClaimed = errors.New("今天已簽到")
	// ErrTimezoneChangeCooldown 表示改過時區後,距上次簽到未滿最小間隔
	// (economy_configs 的 timezone_change_min_gap_hours,預設 20h)。
	ErrTimezoneChangeCooldown = errors.New("改時區後簽到冷卻中")
	// ErrLedgerStateConflict 表示兩個防重權威矛盾:daily_claims 的 PK 判定首簽,
	// 帳本卻已有同一天的冪等鍵(replay)。異常狀態必須失敗出聲,不能回成功但錢沒動。
	ErrLedgerStateConflict = errors.New("簽到紀錄與帳本狀態矛盾")
	// ErrInvalidConfig 表示 economy_configs 的簽到參數不合法(負值、cap < base、
	// 算出金額 ≤ 0)。壞設定要拒絕出聲,不是默默動錢。
	ErrInvalidConfig = errors.New("簽到經濟設定不合法")
)

// Service 是簽到的對外介面。M1 為 in-process 實作(dailypg)。
type Service interface {
	// Claim 為使用者執行「當地今日」簽到:寫簽到紀錄、更新 streak、經帳本發幣,
	// 全部在單一 transaction 內完成。回傳發放金額與連續天數。
	Claim(ctx context.Context, userID int64) (*Result, error)
}
