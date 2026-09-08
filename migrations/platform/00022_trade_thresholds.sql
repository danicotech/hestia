-- +goose Up
-- schemas/09 的防洗點門檻(市集實作需要,seed 契約:created_by NULL)。
-- 前綴用 trade_ 而非 market_:M3 後段的面交會沿用同一組值,不該有第二份權威。
--
-- ⚠️ trade_min_level 必須是 0:schemas/06 明定 M1 的 level 恆為 0(等級功能不上線,
-- 只收 XP 資料)。設成任何正數會讓**所有人**都無法交易——這個門檻要等等級機制
-- 真的上線才有意義。
INSERT INTO platform.economy_configs (key, value, note) VALUES
  ('trade_min_level',            '0',    '交易等級門檻;M1 的 level 恆為 0,設正數會擋住所有人(schemas/06)'),
  ('trade_min_member_days',      '3',    '入群天數門檻,自首次加入起算(離群不歸零)'),
  ('trade_min_account_age_days', '7',    '平台帳號年齡門檻(users.created_at 起算,非 Discord 帳號年齡)'),
  ('trade_daily_volume_cap',     '5000', '單日交易額上限(買+賣合計),日界線用使用者自己的時區');

-- +goose Down
DELETE FROM platform.economy_configs
 WHERE created_by IS NULL
   AND key IN ('trade_min_level','trade_min_member_days',
               'trade_min_account_age_days','trade_daily_volume_cap');
