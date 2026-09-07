-- maintenance 排程器(schemas/14):job 執行紀錄、advisory lock、分區與列級清理。
-- 分區到期偵測(pg_inherits catalog,sqlc 解析不了系統目錄)與 DROP TABLE(動態 DDL)
-- 無法走 sqlc,實作在 maintenance 套件內(partition.go)。

-- name: InsertJobRunLog :exec
-- 每次 job 執行寫一列 event_logs(schemas/13 的 job.run 在此兌現;失敗也要記)
INSERT INTO platform.event_logs (channel, kind, action, status, latency_ms, response)
VALUES ('system', 'job.run', sqlc.arg(action), sqlc.arg(status), sqlc.arg(latency_ms), sqlc.arg(response));

-- name: TryMaintenanceLock :one
-- session-level advisory lock:同名 job 多實例只有一個能跑,拿不到就跳過本輪。
-- 必須在同一條連線上執行 TryMaintenanceLock / MaintenanceUnlock(排程器用 pool.Acquire 釘住連線)。
SELECT pg_try_advisory_lock(hashtext('maintenance'), hashtext(sqlc.arg(job_name)::text));

-- name: MaintenanceUnlock :exec
SELECT pg_advisory_unlock(hashtext('maintenance'), hashtext(sqlc.arg(job_name)::text));

-- name: EnsureMonthPartitions :exec
-- 為 platform schema 所有分區表補齊本月與下月分區(migration 00001 的函式)
SELECT platform.ensure_month_partitions();

-- name: CleanupOutboxEvents :execrows
-- 終態事件(done/failed)逾保留期即刪;pending 永不動(schemas/14)
DELETE FROM platform.outbox_events
WHERE status IN ('done', 'failed')
  AND created_at < now() - make_interval(days => sqlc.arg(retention_days)::int);

-- name: CleanupIdempotencyKeys :execrows
-- 語意代價(接受):超過窗口的重送不再被識別為重放——30 天遠大於任何合法重試窗口
DELETE FROM platform.idempotency_keys
WHERE created_at < now() - make_interval(days => sqlc.arg(retention_days)::int);
