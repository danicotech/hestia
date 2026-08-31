// Package outbox 消費 platform.outbox_events 並派送給註冊的 handler。
//
// 消費模型(帳本鐵則六):
//   - 領域變更與 outbox 同 tx 寫入(寫入端在各 service,不在這裡)
//   - 消費用 FOR UPDATE SKIP LOCKED,多實例不重複處理
//   - 失敗指數退避重試;超過上限標 failed——毒訊息不能卡住整條佇列
//
// 純 ticker + SKIP LOCKED,不上 asynq(開發待辦清單 1-4 定案)。
//
// 交付語意是 at-least-once:handler 執行後若 commit 失敗,事件會重派。
// 因此 handler 必須冪等(重發通知可接受;動錢一律走 Ledger 的冪等鍵)。
package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Handler 處理單一事件。回傳 error 會進退避重試;nil 即標 done。
type Handler func(ctx context.Context, topic string, payload []byte) error

type Consumer struct {
	pool     *pgxpool.Pool
	handlers map[string]Handler

	// 以下皆有預設值,測試可覆寫。
	BatchSize    int32         // 每輪最多取幾筆(handler 在持鎖的 tx 內執行,批次要小)
	PollInterval time.Duration // Run 的輪詢間隔
	MaxAttempts  int32         // 含首次;達到即標 failed
	BackoffBase  time.Duration // 第 n 次失敗後等 BackoffBase × 2^n(封頂 1 小時)

	now func() time.Time // 測試注入
}

func NewConsumer(pool *pgxpool.Pool) *Consumer {
	return &Consumer{
		pool:         pool,
		handlers:     map[string]Handler{},
		BatchSize:    10,
		PollInterval: time.Second,
		MaxAttempts:  8,
		BackoffBase:  30 * time.Second,
		now:          time.Now,
	}
}

// Handle 註冊 topic 的處理者。未註冊的 topic 走一般失敗路徑
// (退避重試→failed):handler 可能晚於事件部署,直接丟棄會漏通知。
func (c *Consumer) Handle(topic string, h Handler) {
	c.handlers[topic] = h
}

// Run 輪詢直到 ctx 取消。佇列非空時連續處理,空了才等下一個 tick。
func (c *Consumer) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.PollInterval)
	defer ticker.Stop()
	for {
		n, err := c.ProcessOnce(ctx)
		if err != nil && ctx.Err() == nil {
			// 整批層級的錯誤(連線斷等):記入下一輪重試,不讓消費者死掉
			n = 0
		}
		if n > 0 {
			continue // 還有貨,不等 tick
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ProcessOnce 取一批、逐筆派送、標記結果,單一 transaction。
// 回傳本輪處理(含失敗)的事件數。
func (c *Consumer) ProcessOnce(ctx context.Context) (int, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := db.New(tx)
	events, err := q.ClaimPendingOutbox(ctx, c.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	for _, ev := range events {
		herr := c.dispatch(ctx, ev)
		switch {
		case herr == nil:
			err = q.MarkOutboxDone(ctx, ev.ID)
		case ev.Attempts+1 >= c.MaxAttempts:
			err = q.MarkOutboxFailed(ctx, ev.ID)
		default:
			retryAt := c.now().Add(c.backoff(ev.Attempts))
			err = q.MarkOutboxRetry(ctx, db.MarkOutboxRetryParams{ID: ev.ID, NextRetryAt: &retryAt})
		}
		if err != nil {
			return 0, fmt.Errorf("mark event %d: %w", ev.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(events), nil
}

// dispatch 把 handler 的 panic 收斂成 error:一筆毒訊息只影響自己的重試,
// 不能把整個消費者(和同批其他事件)拖下水。
func (c *Consumer) dispatch(ctx context.Context, ev db.PlatformOutboxEvent) (err error) {
	h, ok := c.handlers[ev.Topic]
	if !ok {
		return fmt.Errorf("no handler for topic %q", ev.Topic)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, ev.Topic, ev.Payload)
}

func (c *Consumer) backoff(attempts int32) time.Duration {
	const cap = time.Hour
	d := c.BackoffBase
	for i := int32(0); i < attempts && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	return d
}
