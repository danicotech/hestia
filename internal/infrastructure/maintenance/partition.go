package maintenance

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// retentionTables:schemas/14 盤點的五張「分區到期 DROP」表。
// 保留期沿用既有 economy_configs key,不新增重複概念(專案規則 9)。
var retentionTables = []struct {
	table     string
	configKey string
}{
	{"platform.voice_sessions", "raw_activity_retention_days"},
	{"platform.presence_spans", "raw_activity_retention_days"},
	{"platform.message_revisions", "revision_log_retention_days"},
	{"platform.event_logs", "event_log_retention_days"},
	{"platform.message_logs", "message_log_retention_days"},
}

// PartitionMaintenance 每日分區維護:
//  1. ensure_month_partitions() 預建本月與下月分區(migration 00001 的函式,動態涵蓋所有分區表);
//  2. 對五張到期 DROP 表,卸掉「整個分區都超出保留期」的月分區。
//
// processed = 卸掉的分區數。
func PartitionMaintenance(pool *pgxpool.Pool) JobFunc {
	return func(ctx context.Context) (int, error) {
		q := db.New(pool)
		if err := q.EnsureMonthPartitions(ctx); err != nil {
			return 0, fmt.Errorf("ensure_month_partitions: %w", err)
		}
		processed := 0
		for _, rt := range retentionTables {
			days, err := configDays(ctx, q, rt.configKey)
			if err != nil {
				return processed, err
			}
			parts, err := listExpiredMonthPartitions(ctx, pool, rt.table, days)
			if err != nil {
				return processed, fmt.Errorf("找 %s 過期分區: %w", rt.table, err)
			}
			for _, p := range parts {
				if err := dropPartition(ctx, pool, rt.table, p); err != nil {
					return processed, fmt.Errorf("卸分區 %s.%s: %w", p.Schema, p.Name, err)
				}
				processed++
			}
		}
		return processed, nil
	}
}

// partitionRef 是待卸分區的 (schema, 名稱):直接取自 pg_namespace / pg_class 兩欄,
// 不經 regclass::text 再拆字——那條路依賴 search_path,且含引號/點的名稱會拆錯。
type partitionRef struct {
	Schema string
	Name   string
}

// listExpiredMonthPartitions 從 catalog 解析各分區的上界,不依賴分區命名。
// sqlc 解析不了 pg_inherits 等系統目錄,只能留在這裡當 raw SQL(僅此一處,與動態 DROP 配對)。
//
// 安全設計:
//   - DEFAULT 分區的 bound 表達式是字面 'DEFAULT',永遠排除;
//   - 上界解析不出來(如 TO (MAXVALUE))時 regexp_match 回 NULL,比較為 NULL → 不入選
//     ——解析失敗的安全方向是「不刪」;
//   - 上界 <= now() − 保留天數 ⇒ 分區內所有列都早於截止線,可整個 DROP。
const listExpiredSQL = `
SELECT n.nspname::text, c.relname::text
FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE i.inhparent = $1::regclass
  AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
  AND (regexp_match(pg_get_expr(c.relpartbound, c.oid),
        'TO \(''([^'']+)''\)'))[1]::timestamptz
      <= now() - make_interval(days => $2::int)`

func listExpiredMonthPartitions(ctx context.Context, pool *pgxpool.Pool, parent string, retentionDays int32) ([]partitionRef, error) {
	rows, err := pool.Query(ctx, listExpiredSQL, parent, retentionDays)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[partitionRef])
}

// dropPartition 卸掉單一分區。名字來自 catalog(非使用者輸入),仍過 pgx.Identifier
// 消毒——DDL 不能參數化,這是唯一的組字點。
//
// 鎖序:同 tx 先對父表取 ACCESS EXCLUSIVE,再 DROP 子分區——與寫入路徑(經父表
// 路由:先父後子)及 migration 00001 的 CREATE 路徑同向,結構上不可能死鎖。
// 直接 DROP 子分區是「先子後父」,撞上高峰期寫入會互相死鎖。
func dropPartition(ctx context.Context, pool *pgxpool.Pool, parent string, p partitionRef) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// parent 來自本檔案的靜態 retentionTables 清單(非外部輸入),可直接內插
	if _, err := tx.Exec(ctx, "LOCK TABLE "+parent+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("鎖父表: %w", err)
	}
	// IF EXISTS:列舉與 DROP 之間分區可能已被別的路徑卸掉(TOCTOU),消失即目的已達
	ident := pgx.Identifier{p.Schema, p.Name}
	if _, err := tx.Exec(ctx, "DROP TABLE IF EXISTS "+ident.Sanitize()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
