package healthz

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// 分區健康的兩個問題,對應兩種不同的災難:
//
//   - 下個月的分區沒預建 → 月初開始,所有寫入都會落進 DEFAULT。系統不會報錯
//     (DEFAULT 就是為了不報錯而存在的),但那個月的資料從此不能整分區 DROP,
//     而且之後要補建該月分區時得先把列搬回來。這是「安靜的故障」,只能靠檢查看見。
//   - DEFAULT 分區裡有資料 → 有東西沒被正確路由。對到期 DROP 的表來說,
//     那些列會永遠躲過保留期清理。
//
// 兩支查詢都走系統目錄(pg_partitioned_table / pg_inherits),sqlc 解析不了,
// 因此留在套件內當 raw SQL——與 maintenance/partition.go 同一個先例、同一個理由。

// nextMonthMissingSQL:找出「沒有任何非 DEFAULT 分區涵蓋下個月第一天」的分區表。
//
// 刻意從 catalog 解析上下界而不是比對分區命名:命名是慣例,bound 才是事實。
// 上下界解析不出來時 regexp_match 回 NULL、比較為 NULL → 該分區不算數 →
// 這張表會被報成缺分區。方向是對的:看不懂的分區不能當作安全。
const nextMonthMissingSQL = `
WITH nm AS (SELECT date_trunc('month', now() + interval '1 month') AS m),
parents AS (
  SELECT pt.partrelid AS oid, (n.nspname || '.' || c.relname)::text AS name
  FROM pg_partitioned_table pt
  JOIN pg_class c ON c.oid = pt.partrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'platform'
)
SELECT p.name
FROM parents p, nm
WHERE NOT EXISTS (
  SELECT 1
  FROM pg_inherits i
  JOIN pg_class ch ON ch.oid = i.inhrelid
  WHERE i.inhparent = p.oid
    AND pg_get_expr(ch.relpartbound, ch.oid) <> 'DEFAULT'
    AND (regexp_match(pg_get_expr(ch.relpartbound, ch.oid), 'FROM \(''([^'']+)''\)'))[1]::timestamptz <= nm.m
    AND (regexp_match(pg_get_expr(ch.relpartbound, ch.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz > nm.m
)
ORDER BY p.name`

// defaultRowsSQL:數每個 DEFAULT 分區裡的列。
//
// 分區名是動態的,count 沒辦法用固定 SQL 表達;query_to_xml 讓「動態查詢」留在
// 同一句 SQL 裡完成(不必在 Go 端組字串再跑第二輪)。名字直接來自 catalog 且經
// format 的 %I 消毒。LIMIT 讓每張表的工作量有上界——見 DefaultPartitionSampleLimit 的說明。
const defaultRowsSQL = `
SELECT t.parent, t.partition, t.row_count FROM (
  SELECT (pn.nspname || '.' || pc.relname)::text AS parent,
         (n.nspname  || '.' || c.relname)::text  AS partition,
         (xpath('/row/c/text()',
            query_to_xml(
              format('SELECT count(*) AS c FROM (SELECT 1 FROM %I.%I LIMIT %s) s',
                     n.nspname, c.relname, $1::int),
              false, true, '')
          ))[1]::text::bigint AS row_count
  FROM pg_inherits i
  JOIN pg_class c       ON c.oid  = i.inhrelid
  JOIN pg_namespace n   ON n.oid  = c.relnamespace
  JOIN pg_class pc      ON pc.oid = i.inhparent
  JOIN pg_namespace pn  ON pn.oid = pc.relnamespace
  WHERE pn.nspname = 'platform'
    AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'
) t
WHERE t.row_count > 0
ORDER BY t.parent`

// DefaultPartitionRows:某張表的 DEFAULT 分區裡有多少列(數到取樣上限為止)。
type DefaultPartitionRows struct {
	Parent    string
	Partition string
	RowCount  int64 // 上限為 DefaultPartitionSampleLimit;達到上限代表「至少這麼多」
}

type PartitionHealth struct {
	// NextMonthMissing:下個月分區還沒預建的表(空 = 全部備妥)
	NextMonthMissing []string
	// DefaultWithRows:DEFAULT 分區裡有資料的表(空 = 路由正確)
	DefaultWithRows []DefaultPartitionRows
}

func (p PartitionHealth) OK() bool {
	return len(p.NextMonthMissing) == 0 && len(p.DefaultWithRows) == 0
}

// Partitions 跑上面兩支查詢。
func (c *Checker) Partitions(ctx context.Context) (PartitionHealth, error) {
	var h PartitionHealth

	rows, err := c.pool.Query(ctx, nextMonthMissingSQL)
	if err != nil {
		return h, fmt.Errorf("查下月分區: %w", err)
	}
	h.NextMonthMissing, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return h, fmt.Errorf("查下月分區: %w", err)
	}

	limit := c.DefaultPartitionSampleLimit
	if limit < 1 {
		limit = 1
	}
	rows, err = c.pool.Query(ctx, defaultRowsSQL, limit)
	if err != nil {
		return h, fmt.Errorf("查 DEFAULT 分區列數: %w", err)
	}
	h.DefaultWithRows, err = pgx.CollectRows(rows, pgx.RowToStructByPos[DefaultPartitionRows])
	if err != nil {
		return h, fmt.Errorf("查 DEFAULT 分區列數: %w", err)
	}
	return h, nil
}
