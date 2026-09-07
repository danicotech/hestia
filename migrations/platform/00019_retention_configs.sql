-- +goose Up
-- schemas/14:資料膨脹治理的兩個清理窗口(seed 契約:created_by NULL)
INSERT INTO platform.economy_configs (key, value, note) VALUES
  ('outbox_done_retention_days',  '30', 'outbox done/failed 事件保留(schemas/14)'),
  ('idempotency_retention_days',  '30', '冪等鍵保留;超過窗口的重送不再識別為重放(schemas/14)');

-- +goose Down
DELETE FROM platform.economy_configs
 WHERE created_by IS NULL
   AND key IN ('outbox_done_retention_days','idempotency_retention_days');
