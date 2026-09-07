package maintenance_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/infrastructure/maintenance"
)

// TestPartitionMaintenance:造舊分區 + 舊資料,跑完後過期分區消失、
// 窗口內分區與 DEFAULT 完好、近期資料仍可查、下月分區已預建。
func TestPartitionMaintenance(t *testing.T) {
	setup(t)
	ctx := context.Background()
	userID := newUser(t)
	spaceID := newSpace(t)

	// 2020-01 對五張表的保留期(90/180/365 天)都是「完全過期」,且不隨測試執行日期漂移
	oldMonth := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	oldTS := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	tables := []string{
		"platform.voice_sessions",
		"platform.presence_spans",
		"platform.message_revisions",
		"platform.event_logs",
		"platform.message_logs",
	}
	for _, tb := range tables {
		createMonthPartition(t, tb, oldMonth)
	}
	// 窗口內的上月分區(上界=本月一號,任何保留期都 >= now-90d)——必須存活
	now := time.Now().UTC()
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonth := firstOfMonth.AddDate(0, -1, 0)
	createMonthPartition(t, "platform.voice_sessions", prevMonth)
	prevName := "platform.voice_sessions_" + prevMonth.Format("2006_01")

	// 舊資料(進 2020-01 分區)+ 近期資料(進本月分區/上月分區)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("插測試資料: %v\nSQL: %s", err, sql)
		}
	}
	mustExec(`INSERT INTO platform.voice_sessions (user_id, space_id, channel_id, joined_at)
	          VALUES ($1, $2, 'ch', $3), ($1, $2, 'ch', $4), ($1, $2, 'ch', now())`,
		userID, spaceID, oldTS, prevMonth.Add(48*time.Hour))
	mustExec(`INSERT INTO platform.presence_spans (user_id, space_id, status, started_at)
	          VALUES ($1, $2, 'online', $3), ($1, $2, 'online', now())`, userID, spaceID, oldTS)
	mustExec(`INSERT INTO platform.message_revisions (message_id, user_id, kind, captured_at)
	          VALUES ('mt-old', $1, 'deleted', $2), ('mt-new', $1, 'edited', now())`, userID, oldTS)
	mustExec(`INSERT INTO platform.event_logs (channel, kind, action, created_at)
	          VALUES ('system', 'test.row', 'mt-partition-old', $1),
	                 ('system', 'test.row', 'mt-partition-new', now())`, oldTS)
	mustExec(`INSERT INTO platform.message_logs (user_id, space_id, channel_id, channel_kind, message_id, created_at)
	          VALUES ($1, $2, 'ch', 'text', gen_random_uuid()::text, $3),
	                 ($1, $2, 'ch', 'text', gen_random_uuid()::text, now())`, userID, spaceID, oldTS)

	processed, err := maintenance.PartitionMaintenance(pool)(ctx)
	if err != nil {
		t.Fatalf("partition_maintenance: %v", err)
	}
	if processed < 5 {
		t.Errorf("processed = %d,至少應卸掉 5 個 2020-01 分區", processed)
	}

	for _, tb := range tables {
		oldPart := tb + "_2020_01"
		if regclassExists(t, oldPart) {
			t.Errorf("過期分區 %s 應被卸掉", oldPart)
		}
		if !regclassExists(t, tb+"_default") {
			t.Errorf("DEFAULT 分區 %s_default 不得被動到", tb)
		}
	}
	if !regclassExists(t, prevName) {
		t.Errorf("窗口內分區 %s 不得被卸掉", prevName)
	}

	// 下月分區已由 ensure_month_partitions 預建(下月以 DB 時鐘為準)
	var nextSuffix string
	if err := pool.QueryRow(ctx,
		`SELECT to_char(date_trunc('month', now()) + interval '1 month', 'YYYY_MM')`,
	).Scan(&nextSuffix); err != nil {
		t.Fatalf("算下月字尾: %v", err)
	}
	if !regclassExists(t, "platform.voice_sessions_"+nextSuffix) {
		t.Errorf("下月分區 platform.voice_sessions_%s 應已預建", nextSuffix)
	}

	// 舊資料隨分區消失,近期資料仍可查(經父表查,證明路由完好)
	var oldCount, newCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.voice_sessions WHERE user_id = $1 AND joined_at < '2020-02-01'`,
		userID).Scan(&oldCount); err != nil {
		t.Fatalf("查舊資料: %v", err)
	}
	if oldCount != 0 {
		t.Errorf("2020-01 的舊資料應隨分區消失,還剩 %d 列", oldCount)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.voice_sessions WHERE user_id = $1`, userID).Scan(&newCount); err != nil {
		t.Fatalf("查近期資料: %v", err)
	}
	if newCount != 2 {
		t.Errorf("窗口內的 2 列(上月+本月)應完好,查到 %d 列", newCount)
	}
	var revCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.message_revisions WHERE message_id = 'mt-new'`).Scan(&revCount); err != nil {
		t.Fatalf("查 message_revisions: %v", err)
	}
	if revCount != 1 {
		t.Errorf("近期 revision 應完好,查到 %d 列", revCount)
	}

	// 冪等:再跑一次,沒有東西可卸,也不能出錯
	processed, err = maintenance.PartitionMaintenance(pool)(ctx)
	if err != nil {
		t.Fatalf("第二次 partition_maintenance: %v", err)
	}
	if processed != 0 {
		t.Errorf("第二次執行不該再卸分區,processed = %d", processed)
	}
}

// TestOutboxCleanup:只刪過期的 done/failed;pending 與新資料不動。
func TestOutboxCleanup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	const topic = "maintenance.test.outbox"
	_, err := pool.Exec(ctx, `
		INSERT INTO platform.outbox_events (topic, payload, status, created_at) VALUES
		  ($1, '{}', 'done',    now() - interval '40 days'),
		  ($1, '{}', 'failed',  now() - interval '40 days'),
		  ($1, '{}', 'done',    now() - interval '1 day'),
		  ($1, '{}', 'pending', now() - interval '40 days'),
		  ($1, '{}', 'pending', now() - interval '1 hour')`, topic)
	if err != nil {
		t.Fatalf("插 outbox 測試資料: %v", err)
	}

	processed, err := maintenance.OutboxCleanup(pool)(ctx)
	if err != nil {
		t.Fatalf("outbox_cleanup: %v", err)
	}
	if processed < 2 {
		t.Errorf("processed = %d,至少應刪掉本測試的 2 筆過期終態事件", processed)
	}

	counts := map[string]int{}
	rows, err := pool.Query(ctx,
		`SELECT status, count(*) FROM platform.outbox_events WHERE topic = $1 GROUP BY status`, topic)
	if err != nil {
		t.Fatalf("查殘留: %v", err)
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[s] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := map[string]int{"done": 1, "pending": 2}
	for s, n := range want {
		if counts[s] != n {
			t.Errorf("status=%s 應剩 %d 筆,實際 %d(全貌:%v)", s, n, counts[s], counts)
		}
	}
	if counts["failed"] != 0 {
		t.Errorf("過期的 failed 應被刪掉,還剩 %d 筆", counts["failed"])
	}
}

// TestIdempotencyCleanup:只刪過期鍵,新鍵不動。
func TestIdempotencyCleanup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	oldKey := fmt.Sprintf("mt-old-%d", time.Now().UnixNano())
	newKey := fmt.Sprintf("mt-new-%d", time.Now().UnixNano())
	_, err := pool.Exec(ctx, `
		INSERT INTO platform.idempotency_keys (key, request_hash, created_at) VALUES
		  ($1, 'h', now() - interval '40 days'),
		  ($2, 'h', now() - interval '1 day')`, oldKey, newKey)
	if err != nil {
		t.Fatalf("插冪等鍵測試資料: %v", err)
	}

	processed, err := maintenance.IdempotencyCleanup(pool)(ctx)
	if err != nil {
		t.Fatalf("idempotency_cleanup: %v", err)
	}
	if processed < 1 {
		t.Errorf("processed = %d,至少應刪掉本測試的過期鍵", processed)
	}

	var oldLeft, newLeft int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE key = $1), count(*) FILTER (WHERE key = $2)
		 FROM platform.idempotency_keys WHERE key IN ($1, $2)`, oldKey, newKey,
	).Scan(&oldLeft, &newLeft); err != nil {
		t.Fatalf("查殘留: %v", err)
	}
	if oldLeft != 0 {
		t.Errorf("過期鍵應被刪掉")
	}
	if newLeft != 1 {
		t.Errorf("窗口內的鍵不得被刪")
	}
}

// TestConfigDaysFailures:保留天數設定壞掉(缺 key、0、負數、非整數)時 job 必須
// 回報錯誤且不執行清理——保留期歸零等於「全刪」,絕不能用預設值掩蓋設定錯誤。
func TestConfigDaysFailures(t *testing.T) {
	setup(t)
	ctx := context.Background()
	const key = "outbox_done_retention_days"
	const topic = "maintenance.test.configdays"

	// 哨兵:一筆「照正常設定本該被刪」的過期 done 事件——設定壞掉時它必須毫髮無傷
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.outbox_events (topic, payload, status, created_at)
		VALUES ($1, '{}', 'done', now() - interval '400 days')`, topic); err != nil {
		t.Fatalf("插哨兵資料: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM platform.outbox_events WHERE topic = $1`, topic)
	})
	sentinelIntact := func(t *testing.T) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM platform.outbox_events WHERE topic = $1`, topic).Scan(&n); err != nil {
			t.Fatalf("查哨兵: %v", err)
		}
		if n != 1 {
			t.Errorf("設定壞掉時不得執行清理,哨兵剩 %d 列(應為 1)", n)
		}
	}
	// override:插一列 effective_at=now() 的設定蓋過 seed,子測試結束即還原
	override := func(t *testing.T, value string) {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO platform.economy_configs (key, value, note)
			VALUES ($1, $2::jsonb, 'maintenance 測試覆寫') RETURNING id`, key, value).Scan(&id); err != nil {
			t.Fatalf("覆寫設定: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM platform.economy_configs WHERE id = $1`, id)
		})
	}
	runExpectError := func(t *testing.T, wantInErr string) {
		t.Helper()
		processed, err := maintenance.OutboxCleanup(pool)(ctx)
		if err == nil {
			t.Fatalf("設定壞掉應回報錯誤,卻成功了(processed=%d)", processed)
		}
		if wantInErr != "" && !strings.Contains(err.Error(), wantInErr) {
			t.Errorf("錯誤訊息應包含 %q,實際:%v", wantInErr, err)
		}
		if processed != 0 {
			t.Errorf("失敗時 processed 應為 0,實際 %d", processed)
		}
		sentinelIntact(t)
	}

	t.Run("zero", func(t *testing.T) {
		override(t, `0`)
		runExpectError(t, "超出合理範圍")
	})
	t.Run("negative", func(t *testing.T) {
		override(t, `-5`)
		runExpectError(t, "超出合理範圍")
	})
	t.Run("non_integer", func(t *testing.T) {
		override(t, `"abc"`)
		runExpectError(t, "不是整數")
	})
	t.Run("missing_key", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`DELETE FROM platform.economy_configs WHERE key = $1`, key); err != nil {
			t.Fatalf("移除設定: %v", err)
		}
		t.Cleanup(func() {
			// 還原 seed 值(00019 的預設 30 天),不影響後續測試
			_, _ = pool.Exec(ctx, `
				INSERT INTO platform.economy_configs (key, value, note)
				VALUES ($1, '30', 'maintenance 測試還原')`, key)
		})
		runExpectError(t, "讀設定")
	})

	// 設定修好後清理恢復正常:哨兵這次要被刪掉
	processed, err := maintenance.OutboxCleanup(pool)(ctx)
	if err != nil {
		t.Fatalf("設定還原後清理應成功: %v", err)
	}
	if processed < 1 {
		t.Errorf("設定還原後應刪掉哨兵,processed = %d", processed)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.outbox_events WHERE topic = $1`, topic).Scan(&n); err != nil {
		t.Fatalf("查哨兵: %v", err)
	}
	if n != 0 {
		t.Errorf("設定還原後哨兵應被刪掉,還剩 %d 列", n)
	}
}
