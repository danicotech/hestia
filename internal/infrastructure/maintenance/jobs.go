package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 內建 job 名(advisory lock 與 event_logs.action 都用它)。
const (
	JobPartitionMaintenance = "partition_maintenance"
	JobOutboxCleanup        = "outbox_cleanup"
	JobIdempotencyCleanup   = "idempotency_cleanup"
)

// RegisterDefaults 註冊 schemas/14 的三個內建每日 job(cmd/server 接線時呼叫)。
func RegisterDefaults(r *Runner, pool *pgxpool.Pool) {
	r.Register(JobPartitionMaintenance, 24*time.Hour, PartitionMaintenance(pool))
	r.Register(JobOutboxCleanup, 24*time.Hour, OutboxCleanup(pool))
	r.Register(JobIdempotencyCleanup, 24*time.Hour, IdempotencyCleanup(pool))
}

// OutboxCleanup 刪除逾保留期的終態(done/failed)outbox 事件;pending 永不動。
func OutboxCleanup(pool *pgxpool.Pool) JobFunc {
	return func(ctx context.Context) (int, error) {
		q := db.New(pool)
		days, err := configDays(ctx, q, "outbox_done_retention_days")
		if err != nil {
			return 0, err
		}
		n, err := q.CleanupOutboxEvents(ctx, days)
		if err != nil {
			return 0, fmt.Errorf("清理 outbox_events: %w", err)
		}
		return int(n), nil
	}
}

// IdempotencyCleanup 刪除逾保留期的冪等鍵。
// 語意代價(schemas/14 接受):超過窗口的重送不再被識別為重放——窗口遠大於任何合法重試。
func IdempotencyCleanup(pool *pgxpool.Pool) JobFunc {
	return func(ctx context.Context) (int, error) {
		q := db.New(pool)
		days, err := configDays(ctx, q, "idempotency_retention_days")
		if err != nil {
			return 0, err
		}
		n, err := q.CleanupIdempotencyKeys(ctx, days)
		if err != nil {
			return 0, fmt.Errorf("清理 idempotency_keys: %w", err)
		}
		return int(n), nil
	}
}

// configDays 讀 economy_configs 目前生效的保留天數(effective_at <= now() 最新一筆)。
// 值缺失、非整數、非正數都是設定錯誤,直接回報——保留期歸零等於「全刪」,不能用預設值掩蓋。
func configDays(ctx context.Context, q *db.Queries, key string) (int32, error) {
	raw, err := q.GetCurrentConfig(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("讀設定 %s: %w", key, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("設定 %s 不是整數 (%s): %w", key, raw, err)
	}
	if v < 1 || v > 1<<31-1 {
		return 0, fmt.Errorf("設定 %s 超出合理範圍: %d", key, v)
	}
	return int32(v), nil
}
