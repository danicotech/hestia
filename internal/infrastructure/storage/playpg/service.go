// Package playpg 是 play.Service 的 Postgres 實作(schemas/25)。
//
// 動錢的部分全部走 ledger.ApplyInTx,與抽籤留痕同一個 transaction:
// 「抽了但沒扣錢」或「扣了錢但沒留痕」都會讓產出速率的統計失真,
// 而那個統計正是唯一能在通膨之前發現通膨的東西。
package playpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/chance"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/play"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// txLedger 是本套件對帳本的最小依賴:與抽籤留痕同 tx 動錢。
// 實際注入 *ledgerpg.Service(與 dailypg / xppg 同樣的宣告方式)。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// Service 實作 play.Service。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	led  txLedger
}

var _ play.Service = (*Service)(nil)

// New 建立服務。led 傳 ledgerpg.New(pool) 的結果。
func New(pool *pgxpool.Pool, led txLedger) *Service {
	return &Service{pool: pool, q: db.New(pool), led: led}
}

// defaultCurrency 是小遊戲與抽獎預設使用的幣別。
//
// 寫死是暫時的:多幣別上線後這要變成社群設定(economy_configs)。
// 留在這裡而不是散在各處,是為了那天只改一個地方。
const defaultCurrency = "coin"

// PlayGame 跑一局小遊戲:扣款 → 判定 → 派彩 → 留痕,全部同一個 transaction。
//
// 冪等鍵是必填的(動錢一律冪等,帳本鐵則三)。Discord 的 interaction id
// 是天然的鍵:同一次點擊重送只會結算一次,而使用者連點是常態不是例外。
func (s *Service) PlayGame(ctx context.Context, p play.GameParams) (*play.GameResult, error) {
	if p.UserID <= 0 || p.CommunityID <= 0 {
		return nil, fmt.Errorf("user / community 不合法: %w", play.ErrInvalidParams)
	}
	if p.IdempotencyKey == "" {
		return nil, fmt.Errorf("缺少冪等鍵: %w", play.ErrInvalidParams)
	}

	// 先判定再開 tx:遊戲邏輯是純函數,不需要資料庫,而參數錯誤
	// (押了不存在的選項)在扣款之前就該回絕。
	result, err := chance.Resolve(chance.Game(p.Game), p.Choice, p.Stake)
	if err != nil {
		if errors.Is(err, chance.ErrInvalidStake) {
			return nil, fmt.Errorf("%w: %w", play.ErrInvalidParams, err)
		}
		return nil, fmt.Errorf("%w: %w", play.ErrInvalidParams, err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := s.checkDailyLimit(ctx, qtx, p.UserID); err != nil {
		return nil, err
	}

	// 投注與回收分成兩筆分錄,不記淨額:對帳時「這個月遊戲收了多少、
	// 吐了多少」要分得出來,只記淨額的話那兩個數字永遠算不出來。
	ops := []ledger.Op{{
		UserID: p.UserID, Currency: defaultCurrency,
		Amount: -p.Stake, Reason: ledger.ReasonGameStake,
	}}
	if result.Payout > 0 {
		ops = append(ops, ledger.Op{
			UserID: p.UserID, Currency: defaultCurrency,
			Amount: result.Payout, Reason: ledger.ReasonGamePayout,
		})
	}
	led, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: p.IdempotencyKey,
		RequestHash:    p.IdempotencyKey,
		Ops:            ops,
	})
	if err != nil {
		return nil, err // 餘額不足等語意由帳本的 sentinel 帶出去
	}

	odds, err := json.Marshal(result.Odds)
	if err != nil {
		return nil, fmt.Errorf("序列化賠率: %w", err)
	}
	outcome, err := json.Marshal(map[string]any{
		"choice": result.Choice, "result": result.Result, "payout": result.Payout,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化結果: %w", err)
	}
	if _, err := qtx.InsertChanceDraw(ctx, db.InsertChanceDrawParams{
		CommunityID:  p.CommunityID,
		UserID:       p.UserID,
		Kind:         string(chance.KindMinigame),
		Ref:          p.Game,
		OddsSnapshot: odds,
		Seed:         result.Seed,
		Outcome:      outcome,
		Stake:        p.Stake,
		Payout:       result.Payout,
	}); err != nil {
		return nil, fmt.Errorf("寫抽籤紀錄: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	balance := int64(0)
	if n := len(led.Entries); n > 0 {
		balance = led.Entries[n-1].NewBalance
	}
	return &play.GameResult{
		Game: p.Game, Choice: result.Choice, Result: result.Result,
		Stake: p.Stake, Payout: result.Payout, Balance: balance, Seed: result.Seed,
	}, nil
}

// checkDailyLimit 擋每日次數上限。
//
// 三個賭博管道(小遊戲、開箱、賽事下注)共用同一個計數,不是各算各的:
// 分開算的話上限就變成三倍,而設這個上限的理由(經濟失控與觀感)
// 不會因為換一個玩法就消失。
func (s *Service) checkDailyLimit(ctx context.Context, qtx *db.Queries, userID int64) error {
	n, err := qtx.CountDrawsToday(ctx, userID)
	if err != nil {
		return fmt.Errorf("查今日次數: %w", err)
	}
	if n >= play.MaxDrawsPerDay {
		return fmt.Errorf("今天已經玩了 %d 次(上限 %d): %w",
			n, play.MaxDrawsPerDay, play.ErrDailyLimit)
	}
	return nil
}
