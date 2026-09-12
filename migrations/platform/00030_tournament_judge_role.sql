-- +goose Up
-- 裁判權限與 judge 角色(《百業試鋒》的御風羽)。
--
-- 為什麼新增一個角色而不是掛到 moderator:辦賽事的人與管理伺服器的人是兩群人。
-- 御風羽負責評段、抽籤、判勝負,但不需要刪訊息或禁言;反過來,版主也不該
-- 因為能管秩序就能判誰贏。把兩者綁在同一個角色上,就是強迫其中一邊拿到
-- 他不需要的能力 —— 而權限最小化的價值正是在出事時能縮小懷疑範圍。
--
-- 為什麼發獎**不另立權限**:AwardPrizes 做的事就是發點,而 economy.grant
-- 已經是「能動平台代幣」的權限。同一個概念不該有第二個權限名字(規則 9)——
-- 否則「誰能發錢」這個問題會有兩個答案,而審核時沒有人說得出該看哪一個。
-- 裁判要發獎就得另外拿到 economy.grant,那是刻意的摩擦:發錢與判勝負是
-- 兩件事,同一個人能做不代表該用同一把鑰匙。
--
-- is_dangerous = true:裁判不直接動餘額,但判定勝負會觸發下注派彩,等於
-- 決定真錢的流向,而且封盤與判定都不可逆。授予時該跳警告。
INSERT INTO platform.permissions (key, name, description, is_dangerous) VALUES
  ('tournament.judge', '賽事裁判', '評段、抽籤、開封盤、判勝負、棄賽、補發通行碼;判定勝負會觸發下注派彩', true);

-- public_id 沿用 00025 的內建角色慣例:固定值而非隨機 ULID。它們在每個部署
-- 裡都是同一批東西,可預測不洩漏任何規模資訊,而固定值讓文件與測試引用得到。
-- owner=01 admin=02 moderator=03 auditor=04,judge 接 05。
INSERT INTO platform.roles (key, name, public_id) VALUES
  ('judge', '裁判', '00000000000000000000000005');

-- owner 與 admin 一併拿到:他們本來就有全部管理能力,裁判權限缺席只會製造
-- 「明明是管理員卻按不下判定勝者」這種要另外解釋的狀態。
INSERT INTO platform.role_permissions (role_id, permission)
SELECT r.id, v.perm
FROM platform.roles r
JOIN LATERAL (VALUES
  ('owner', 'tournament.judge'),
  ('admin', 'tournament.judge'),
  ('judge', 'tournament.judge')
) AS v(role_key, perm) ON v.role_key = r.key;

-- +goose Down
DELETE FROM platform.role_permissions WHERE permission = 'tournament.judge';
DELETE FROM platform.roles WHERE key = 'judge';
DELETE FROM platform.permissions WHERE key = 'tournament.judge';
