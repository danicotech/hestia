-- +goose Up
-- ══ M1 種子資料:用 migration 管理,不手工 INSERT(可重現原則,grill Q11)══

-- 幣別:M1 只發行一種
INSERT INTO platform.currencies (code, name, symbol, tradable) VALUES
  ('coin', '社群幣', '🪙', true);

-- 角色(權限常數本身寫在 Go 程式碼,這裡只建角色與對應)
INSERT INTO platform.roles (key, name) VALUES
  ('owner',     '擁有者'),
  ('admin',     '管理員'),
  ('moderator', '版主'),
  ('auditor',   '稽核(唯讀)');

INSERT INTO platform.role_permissions (role_id, permission)
SELECT r.id, p.perm
FROM platform.roles r
JOIN LATERAL (
  VALUES
    ('owner',     'economy.grant'), ('owner',     'economy.refund'), ('owner',     'economy.config'),
    ('owner',     'shop.manage'),   ('owner',     'redemption.handle'),
    ('owner',     'user.restrict'), ('owner',     'roles.manage'),
    ('owner',     'logs.read'),     ('owner',     'logs.read_deleted'),
    ('admin',     'economy.grant'), ('admin',     'economy.refund'), ('admin',     'economy.config'),
    ('admin',     'shop.manage'),   ('admin',     'redemption.handle'),
    ('admin',     'user.restrict'),
    ('admin',     'logs.read'),     ('admin',     'logs.read_deleted'),
    ('moderator', 'redemption.handle'),
    ('moderator', 'user.restrict'),
    ('moderator', 'logs.read'),
    ('auditor',   'logs.read')
) AS p(role_key, perm) ON p.role_key = r.key;

-- 經濟數值(grill Q5b 價目表;之後調整 = 插新列,不改舊列)
INSERT INTO platform.economy_configs (key, value, note) VALUES
  ('daily_base',                    '100',  '每日簽到基礎額'),
  ('daily_streak_step',             '10',   '連續每天 +10'),
  ('daily_streak_cap',              '200',  '第 7 天封頂'),
  ('signup_bonus',                  '1000', '新人註冊禮(約 10 天簽到)'),
  ('max_stake',                     '500',  '單注上限'),
  ('market_fee_bps',                '800',  '市集手續費 8%(銷毀)'),
  ('refund_window_seconds_default', '600',  '購買後 10 分鐘可全額退'),
  ('timezone_change_min_gap_hours', '20',   '改時區後下次簽到仍需距上次 20 小時(防一天兩簽)'),
  ('voice_xp_min_peers',            '2',    '語音計時需頻道內至少 2 人'),
  ('message_excerpt_max_chars',     '200',  '訊息摘要截斷長度'),
  ('message_log_retention_days',    '365',  '訊息原文保留(grill Q17)'),
  ('revision_log_retention_days',   '90',   '已刪除訊息保留'),
  ('raw_activity_retention_days',   '90',   '語音/上線原始事件保留'),
  ('chunk_gap_minutes',             '20',   '對話 chunk 的斷開時間窗');

-- +goose Down
-- 只清 seed 寫入的列,不動 runtime 產生的資料
-- 契約:runtime 調整 economy_configs 一律經 admin API 且必帶 created_by;seed 列 created_by 為 NULL
DELETE FROM platform.economy_configs
 WHERE created_by IS NULL
   AND key IN ('daily_base','daily_streak_step','daily_streak_cap','signup_bonus',
               'max_stake','market_fee_bps','refund_window_seconds_default',
               'timezone_change_min_gap_hours','voice_xp_min_peers',
               'message_excerpt_max_chars','message_log_retention_days',
               'revision_log_retention_days','raw_activity_retention_days',
               'chunk_gap_minutes');
-- 只刪 seed 的 21 組配對;runtime 加給 seed 角色的自訂權限不受影響
DELETE FROM platform.role_permissions rp
 USING platform.roles r,
       (VALUES
          ('owner','economy.grant'),('owner','economy.refund'),('owner','economy.config'),
          ('owner','shop.manage'),('owner','redemption.handle'),
          ('owner','user.restrict'),('owner','roles.manage'),
          ('owner','logs.read'),('owner','logs.read_deleted'),
          ('admin','economy.grant'),('admin','economy.refund'),('admin','economy.config'),
          ('admin','shop.manage'),('admin','redemption.handle'),
          ('admin','user.restrict'),
          ('admin','logs.read'),('admin','logs.read_deleted'),
          ('moderator','redemption.handle'),('moderator','user.restrict'),('moderator','logs.read'),
          ('auditor','logs.read')
       ) AS s(role_key, perm)
 WHERE r.id = rp.role_id AND r.key = s.role_key AND rp.permission = s.perm;
DELETE FROM platform.roles WHERE key IN ('owner','admin','moderator','auditor');
DELETE FROM platform.currencies WHERE code = 'coin';
