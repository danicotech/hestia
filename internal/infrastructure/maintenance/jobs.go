package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/chunker"
	"github.com/danicotech/hestia/internal/infrastructure/healthz"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
)

// 內建 job 名(advisory lock 與 event_logs.action 都用它)。
const (
	JobPartitionMaintenance = "partition_maintenance"
	JobOutboxCleanup        = "outbox_cleanup"
	JobIdempotencyCleanup   = "idempotency_cleanup"
	JobBuildChunks          = "build_chunks"
	JobHealthCheck          = "health_check"
)

// defaultEvery 是內建 job 的執行間隔。註冊(RegisterDefaults)與健康檢查
// (healthz 判斷「太久沒跑」)讀同一份,間隔的數字只有這一個權威位置。
var defaultEvery = map[string]time.Duration{
	JobPartitionMaintenance: 24 * time.Hour,
	JobOutboxCleanup:        24 * time.Hour,
	JobIdempotencyCleanup:   24 * time.Hour,
	JobSessionCleanup:       24 * time.Hour,
	JobBuildChunks:          10 * time.Minute,
	JobHealthCheck:          time.Hour,
}

// monitoredJobs 是 health_check 要盯的 job 清單:內建 job 全都盯,唯獨不盯自己——
// health_check 沒跑的話也不會有人來報告它沒跑,那一層屬於外部存活監控。
func monitoredJobs() []healthz.JobExpectation {
	out := make([]healthz.JobExpectation, 0, len(defaultEvery)-1)
	for name, every := range defaultEvery {
		if name == JobHealthCheck {
			continue
		}
		out = append(out, healthz.JobExpectation{Name: name, Every: every})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RegisterDefaults 註冊 schemas/14 的內建每日 job(cmd/server 接線時呼叫)。
func RegisterDefaults(r *Runner, pool *pgxpool.Pool) {
	r.Register(JobPartitionMaintenance, defaultEvery[JobPartitionMaintenance], PartitionMaintenance(pool))
	r.Register(JobOutboxCleanup, defaultEvery[JobOutboxCleanup], OutboxCleanup(pool))
	r.Register(JobIdempotencyCleanup, defaultEvery[JobIdempotencyCleanup], IdempotencyCleanup(pool))
	r.Register(JobSessionCleanup, defaultEvery[JobSessionCleanup], SessionCleanup(pool))
	// M6:對話 chunk 聚合(schemas/12)。10 分鐘一輪 —— 比 chunk_gap_minutes(20)短,
	// 對話一封閉就會在下一輪被收走,不會累積成一大批。
	r.Register(JobBuildChunks, defaultEvery[JobBuildChunks], chunker.New(pool).BuildChunks)
	// M7:維運監控(schemas/14 監控與告警)。異常寫 slog.Error,processed = 異常項數。
	r.Register(JobHealthCheck, defaultEvery[JobHealthCheck],
		healthz.New(pool, ledgerpg.New(pool)).HealthCheck(monitoredJobs()))
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
