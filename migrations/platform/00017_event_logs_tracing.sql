-- +goose Up
-- schemas/13 增補(2026-08-27 確認):簡化請求/回應摘要 + OTel 反查鍵
-- 分工:DB 存白名單摘要(業務稽核、可 JOIN);完整往返細節在 OTel(Tempo/Loki),用 trace_id 互跳。
ALTER TABLE platform.event_logs RENAME COLUMN payload TO request;
ALTER TABLE platform.event_logs
  ADD COLUMN response JSONB,                    -- 結果摘要:建立的 public_id、金額、錯誤細節(白名單)
  ADD COLUMN trace_id TEXT;                     -- W3C trace id;拿去 Grafana 看完整往返
CREATE INDEX ON platform.event_logs (trace_id); -- Grafana 看到慢 trace → 回 DB 查業務脈絡

-- +goose Down
DROP INDEX platform.event_logs_trace_id_idx;
ALTER TABLE platform.event_logs DROP COLUMN trace_id, DROP COLUMN response;
ALTER TABLE platform.event_logs RENAME COLUMN request TO payload;
