-- +goose Up
-- schemas/13:入口層操作紀錄(2026-08-27 確認:統一表 / 180 天 / optout 不涵蓋 / 含 system)
-- 所有進到系統的動作(API / gRPC / Discord bot / Twitch / YT / 排程)都在這留痕。
-- 內容不存這裡(訊息=message_logs、錢=token_entries、管理=admin_audit_logs)——這張只記「發生過什麼操作、結果如何」。
CREATE TABLE platform.event_logs (
  id         BIGINT GENERATED ALWAYS AS IDENTITY,
  request_id TEXT,                              -- ULID,跨層追蹤;冪等鍵/outbox 可引用
  channel    TEXT        NOT NULL,              -- api | grpc | discord | twitch | youtube | system
  kind       TEXT        NOT NULL,              -- http.request | grpc.call | discord.command | discord.component | twitch.event | youtube.event | job.run
  action     TEXT        NOT NULL,              -- POST /v1/shop/purchase | /daily claim | channel.follow | entitlement_reaper
  user_id    BIGINT,                            -- 弱參照(未登入/系統為 NULL;不設 FK,量大且到期整分區 DROP)
  space_id   BIGINT,                            -- 弱參照,來源空間
  status     TEXT,                              -- ok | error | denied | rate_limited
  error_code TEXT,
  latency_ms INT,
  payload    JSONB,                             -- 白名單摘要參數;絕不存 token / 密碼 / 訊息內容
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX ON platform.event_logs (user_id, created_at DESC);
CREATE INDEX ON platform.event_logs (channel, kind, created_at DESC);
CREATE INDEX ON platform.event_logs (request_id);
CREATE TABLE platform.event_logs_default PARTITION OF platform.event_logs DEFAULT;
SELECT platform.create_month_partition('platform.event_logs', now()::date);
SELECT platform.create_month_partition('platform.event_logs', (now() + interval '1 month')::date);

-- 保留期可調(seed 契約:created_by NULL)
INSERT INTO platform.economy_configs (key, value, note)
VALUES ('event_log_retention_days', '180', '入口層操作紀錄保留(2026-08-27 定案)');

-- +goose Down
DELETE FROM platform.economy_configs
 WHERE created_by IS NULL AND key = 'event_log_retention_days';
DROP TABLE platform.event_logs;
