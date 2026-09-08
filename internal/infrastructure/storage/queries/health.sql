-- 維運監控指標(schemas/14「監控與告警」)。每支查詢都是單一 SQL,
-- 目的是讓 HTTP handler 或排程 job 都能便宜地叫,不做應用層彙總。
--
-- 對帳(SUM(entries) = balance)不在這裡:ledger.Reconcile() 已經是全量比對的權威,
-- healthz 直接呼叫它,不寫第二套 SQL(專案規則 9)。
-- 分區健康走 pg_inherits / pg_partitioned_table,sqlc 解析不了系統目錄,
-- 留在 healthz/partition.go 當 raw SQL(與 maintenance/partition.go 同一個先例)。

-- name: OutboxBacklog :one
-- outbox 積壓:pending 數、最舊 pending 的年齡(秒)、failed 數。
-- 一次掃描三個數字——分三句查會在三個時點看到三份不一致的快照。
-- 沒有 pending 時年齡回 0(NULL 會逼呼叫端處理一個沒有意義的空值)。
SELECT
  count(*) FILTER (WHERE status = 'pending')::bigint AS pending,
  count(*) FILTER (WHERE status = 'failed')::bigint  AS failed,
  COALESCE(EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE status = 'pending')), 0)::bigint
    AS oldest_pending_age_seconds
FROM platform.outbox_events;

-- name: JobLastSuccess :many
-- 排程器健康:各 job 最近一次「成功」執行的時間(schemas/13 的 job.run)。
-- 只認 status='ok'——一直失敗的 job 有在跑但沒在做事,不能算健康。
-- 沒跑過的 job 不會出現在結果裡,由呼叫端判定為異常(缺席才是最嚴重的那種)。
SELECT action::text AS job_name, max(created_at)::timestamptz AS last_success_at
FROM platform.event_logs
WHERE channel = 'system' AND kind = 'job.run' AND status = 'ok'
  AND action = ANY(sqlc.arg(job_names)::text[])
GROUP BY action;

-- name: RecentSessionReuse :one
-- session 異常:近期因重用偵測而撤銷的數量。已輪替的 token 再被使用 = token 被竊
-- (schemas/02 增補 F),這個數字從 0 變正就是安全事件,不是效能指標。
SELECT count(*)::bigint
FROM platform.sessions
WHERE revoked_reason = 'reuse_detected'
  AND revoked_at >= now() - make_interval(hours => sqlc.arg(window_hours)::int);
