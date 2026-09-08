package healthz_test

import (
	"context"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/infrastructure/healthz"
)

// ── 1. 對帳 ────────────────────────────────────────────────────────────────

// 人為把餘額快取弄壞(直接改 user_balances),對帳要抓得到;修好後回零。
func TestLedgerMismatches(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	mis, err := c.LedgerMismatches(ctx)
	if err != nil {
		t.Fatalf("對帳: %v", err)
	}
	if len(mis) != 0 {
		t.Fatalf("起始狀態帳應該是平的,得到 %+v", mis)
	}

	user := newUser(t)
	mustExec(t, `INSERT INTO platform.user_balances (user_id, currency, balance)
	             VALUES ($1, 'coin', 500)`, user)

	mis, err = c.LedgerMismatches(ctx)
	if err != nil {
		t.Fatalf("對帳: %v", err)
	}
	found := false
	for _, m := range mis {
		if m.UserID == user {
			found = true
			if m.Balance != 500 || m.EntryTotal != 0 {
				t.Errorf("不一致的內容不對:balance=%d entries=%d", m.Balance, m.EntryTotal)
			}
		}
	}
	if !found {
		t.Fatalf("應偵測到 user=%d 的餘額與分錄不一致,得到 %+v", user, mis)
	}

	// 修好(把假餘額拿掉)→ 回零。token_entries 一列都沒動過,append-only 不受影響。
	mustExec(t, `DELETE FROM platform.user_balances WHERE user_id = $1`, user)
	mis, err = c.LedgerMismatches(ctx)
	if err != nil {
		t.Fatalf("對帳: %v", err)
	}
	if len(mis) != 0 {
		t.Fatalf("修好後應回零,得到 %+v", mis)
	}
}

// ── 2. outbox 積壓 ─────────────────────────────────────────────────────────

func TestOutboxBacklog(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	got, err := c.Outbox(ctx)
	if err != nil {
		t.Fatalf("查 outbox: %v", err)
	}
	if got.Pending != 0 || got.Failed != 0 || got.OldestPendingAge != 0 {
		t.Fatalf("起始狀態應該是空的,得到 %+v", got)
	}

	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM platform.outbox_events`) })
	mustExec(t, `INSERT INTO platform.outbox_events (topic, payload, status, created_at)
	             VALUES ('healthz.test', '{}', 'pending', now() - interval '45 minutes')`)
	mustExec(t, `INSERT INTO platform.outbox_events (topic, payload, status, created_at)
	             VALUES ('healthz.test', '{}', 'pending', now() - interval '1 minute')`)
	mustExec(t, `INSERT INTO platform.outbox_events (topic, payload, status, created_at)
	             VALUES ('healthz.test', '{}', 'failed', now() - interval '2 hours')`)
	mustExec(t, `INSERT INTO platform.outbox_events (topic, payload, status, created_at)
	             VALUES ('healthz.test', '{}', 'done', now() - interval '3 hours')`)

	got, err = c.Outbox(ctx)
	if err != nil {
		t.Fatalf("查 outbox: %v", err)
	}
	if got.Pending != 2 {
		t.Errorf("pending 應為 2,得到 %d", got.Pending)
	}
	if got.Failed != 1 {
		t.Errorf("failed 應為 1,得到 %d", got.Failed)
	}
	// 最舊的 pending 是 45 分鐘那筆;done/failed 更舊但不該影響這個數字
	if got.OldestPendingAge < 44*time.Minute || got.OldestPendingAge > 47*time.Minute {
		t.Errorf("最舊 pending 年齡應約 45 分鐘,得到 %s", got.OldestPendingAge)
	}
	if !got.Stale() {
		t.Errorf("45 分鐘 > 門檻 %s,應判定為積壓", got.StaleAfter)
	}
}

// ── 3. 排程器健康 ──────────────────────────────────────────────────────────

func TestJobHealth(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	insertJobRun(t, "healthz_fresh_job", "ok", 30)         // 30 分鐘前成功
	insertJobRun(t, "healthz_stale_job", "ok", 5*24*60)    // 5 天前成功
	insertJobRun(t, "healthz_failing_job", "error", 10)    // 有在跑但一直失敗
	insertJobRun(t, "healthz_failing_job", "error", 24*60) //
	expect := []healthz.JobExpectation{
		{Name: "healthz_fresh_job", Every: 24 * time.Hour},
		{Name: "healthz_stale_job", Every: 24 * time.Hour},
		{Name: "healthz_failing_job", Every: time.Hour},
		{Name: "healthz_never_ran_job", Every: time.Hour},
	}

	got, err := c.Jobs(ctx, expect)
	if err != nil {
		t.Fatalf("查 job 健康: %v", err)
	}
	if len(got) != len(expect) {
		t.Fatalf("每個被監控的 job 都要有結果,得到 %+v", got)
	}

	if j := jobHealth(t, got, "healthz_fresh_job"); j.Stale {
		t.Errorf("30 分鐘前跑過的每日 job 不該算 stale:%+v", j)
	}
	if j := jobHealth(t, got, "healthz_stale_job"); !j.Stale {
		t.Errorf("5 天沒跑的每日 job 應判定為 stale:%+v", j)
	} else if j.Age < 4*24*time.Hour {
		t.Errorf("年齡算錯:%s", j.Age)
	}
	// 只認 status='ok':一直失敗的 job 有在跑,但等同沒跑
	if j := jobHealth(t, got, "healthz_failing_job"); !j.Stale || !j.LastSuccessAt.IsZero() {
		t.Errorf("只失敗過的 job 應等同從未成功:%+v", j)
	}
	if j := jobHealth(t, got, "healthz_never_ran_job"); !j.Stale || !j.LastSuccessAt.IsZero() {
		t.Errorf("從未跑過的 job 應判定為異常:%+v", j)
	}

	// 攤成異常清單時,「從未跑過」與「太久沒跑」是可分辨的兩種
	rep := healthz.Report{Jobs: got}
	an := rep.Anomalies()
	if !hasAnomaly(an, "job_never_ran") || !hasAnomaly(an, "job_stale") {
		t.Errorf("異常分類不完整:%+v", an)
	}
}

// ── 4. 分區健康 ────────────────────────────────────────────────────────────

// 沒預建下個月的分區 → 偵測得到(月初起所有寫入會落進 DEFAULT)。
func TestPartitionHealth_NextMonthMissing(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	got, err := c.Partitions(ctx)
	if err != nil {
		t.Fatalf("查分區健康: %v", err)
	}
	if len(got.NextMonthMissing) != 0 {
		t.Fatalf("migration 已預建下月分區,起始狀態不該有缺:%v", got.NextMonthMissing)
	}

	part := nextMonthPartitionName("platform.message_logs")
	mustExec(t, `DROP TABLE `+part)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`SELECT platform.create_month_partition('platform.message_logs'::regclass,
			         (date_trunc('month', now()) + interval '1 month')::date)`)
	})

	got, err = c.Partitions(ctx)
	if err != nil {
		t.Fatalf("查分區健康: %v", err)
	}
	if !contains(got.NextMonthMissing, "platform.message_logs") {
		t.Fatalf("應偵測到 message_logs 缺下月分區,得到 %v", got.NextMonthMissing)
	}
	rep := healthz.Report{Partitions: got}
	if !hasAnomaly(rep.Anomalies(), "partition_next_month_missing") {
		t.Errorf("應產生 partition_next_month_missing 異常:%+v", rep.Anomalies())
	}

	// 補建回來 → 回到乾淨
	mustExec(t, `SELECT platform.create_month_partition('platform.message_logs'::regclass,
	              (date_trunc('month', now()) + interval '1 month')::date)`)
	got, err = c.Partitions(ctx)
	if err != nil {
		t.Fatalf("查分區健康: %v", err)
	}
	if len(got.NextMonthMissing) != 0 {
		t.Fatalf("補建後應回到乾淨,得到 %v", got.NextMonthMissing)
	}
}

// DEFAULT 分區有資料 → 偵測得到(有列沒被正確路由,會躲過保留期清理)。
func TestPartitionHealth_DefaultHasRows(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	got, err := c.Partitions(ctx)
	if err != nil {
		t.Fatalf("查分區健康: %v", err)
	}
	if len(got.DefaultWithRows) != 0 {
		t.Fatalf("起始狀態 DEFAULT 應該都是空的,得到 %+v", got.DefaultWithRows)
	}

	// 五年後的時間沒有對應月分區 → 一定落進 DEFAULT
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM platform.event_logs WHERE action = 'healthz_default_probe'`)
	})
	mustExec(t, `INSERT INTO platform.event_logs (channel, kind, action, created_at)
	             VALUES ('api', 'http.request', 'healthz_default_probe', now() + interval '5 years')`)

	got, err = c.Partitions(ctx)
	if err != nil {
		t.Fatalf("查分區健康: %v", err)
	}
	if len(got.DefaultWithRows) != 1 ||
		got.DefaultWithRows[0].Parent != "platform.event_logs" ||
		got.DefaultWithRows[0].RowCount != 1 {
		t.Fatalf("應偵測到 event_logs 的 DEFAULT 有 1 列,得到 %+v", got.DefaultWithRows)
	}
	if got.DefaultWithRows[0].Partition != "platform.event_logs_default" {
		t.Errorf("分區名不對:%s", got.DefaultWithRows[0].Partition)
	}
	rep := healthz.Report{Partitions: got}
	if !hasAnomaly(rep.Anomalies(), "partition_default_has_rows") {
		t.Errorf("應產生 partition_default_has_rows 異常:%+v", rep.Anomalies())
	}
}

// ── 5. session 異常 ────────────────────────────────────────────────────────

func TestSessionReuse(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	n, err := c.SessionReuse(ctx)
	if err != nil {
		t.Fatalf("查 session 重用: %v", err)
	}
	if n != 0 {
		t.Fatalf("起始狀態不該有重用偵測,得到 %d", n)
	}

	user := newUser(t)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM platform.sessions WHERE user_id = $1`, user) })
	// 一個被判定重用而撤銷的、一個正常登出的、一個窗口外的
	mustExec(t, `INSERT INTO platform.sessions
	   (public_id, user_id, refresh_token_hash, expires_at, revoked_at, revoked_reason)
	 VALUES (gen_random_uuid()::text, $1, sha256(gen_random_uuid()::text::bytea),
	         now() + interval '30 days', now() - interval '1 hour', 'reuse_detected')`, user)
	mustExec(t, `INSERT INTO platform.sessions
	   (public_id, user_id, refresh_token_hash, expires_at, revoked_at, revoked_reason)
	 VALUES (gen_random_uuid()::text, $1, sha256(gen_random_uuid()::text::bytea),
	         now() + interval '30 days', now() - interval '2 hours', 'logout')`, user)
	mustExec(t, `INSERT INTO platform.sessions
	   (public_id, user_id, refresh_token_hash, expires_at, revoked_at, revoked_reason)
	 VALUES (gen_random_uuid()::text, $1, sha256(gen_random_uuid()::text::bytea),
	         now() + interval '30 days', now() - interval '10 days', 'reuse_detected')`, user)

	n, err = c.SessionReuse(ctx)
	if err != nil {
		t.Fatalf("查 session 重用: %v", err)
	}
	if n != 1 {
		t.Fatalf("24 小時窗口內應只算到 1 筆 reuse_detected,得到 %d", n)
	}
	rep := healthz.Report{SessionReuse: n, SessionReuseWindow: 24 * time.Hour}
	if !hasAnomaly(rep.Anomalies(), "session_reuse_detected") {
		t.Errorf("重用偵測的門檻就是 0,應產生異常:%+v", rep.Anomalies())
	}
}

// ── health_check job ───────────────────────────────────────────────────────

// 全部正常時 processed=0。放在最後跑:前面的測試都在 t.Cleanup 清掉自己造的異常,
// 這個測試驗的正是「乾淨狀態下不誤報」。
func TestHealthCheckJob_CleanSystem(t *testing.T) {
	c := setup(t)
	ctx := context.Background()

	expect := []healthz.JobExpectation{
		{Name: "healthz_job_a", Every: 24 * time.Hour},
		{Name: "healthz_job_b", Every: time.Hour},
	}
	for _, e := range expect {
		insertJobRun(t, e.Name, "ok", 1)
	}

	job := c.HealthCheck(expect)
	processed, err := job(ctx)
	if err != nil {
		t.Fatalf("health_check job: %v", err)
	}
	if processed != 0 {
		rep, _ := c.Check(ctx, expect)
		t.Fatalf("全部正常時 processed 應為 0,得到 %d:%+v", processed, rep.Anomalies())
	}

	// 造一個異常 → processed 要跟著變成 1(job 本身仍算成功執行)
	mustExec(t, `INSERT INTO platform.outbox_events (topic, payload, status, created_at)
	             VALUES ('healthz.test', '{}', 'pending', now() - interval '3 hours')`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM platform.outbox_events`) })

	processed, err = job(ctx)
	if err != nil {
		t.Fatalf("health_check job: %v", err)
	}
	if processed != 1 {
		rep, _ := c.Check(ctx, expect)
		t.Fatalf("應回報 1 項異常,得到 %d:%+v", processed, rep.Anomalies())
	}
}
