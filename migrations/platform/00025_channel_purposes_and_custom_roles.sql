-- +goose Up
-- schemas/22 落地:頻道用途註冊表、逐空間落點、權限註冊表、社群自訂角色。
--
-- 這份 migration 推翻兩處既有設計,兩處都已在 schemas/22 逐項確認:
--   1. schemas/01 增補 D 的「公告落點頻道 id 放 community_spaces.config」
--      → 改用獨立表。JSONB 裡的 channel id 沒有外鍵,打錯字不報錯,
--        只會安靜地貼不出去。config 保留,但只放沒有結構的東西(locale)。
--   2. schemas/03 的「不做 UI 可編輯的 permission 表」
--      → 權限改為可自由指派給任何角色。使用者在知悉風險後重申此決定;
--        保留的緩衝是 permissions.is_dangerous(只警告、不阻擋)。

-- ── 頻道用途註冊表 ────────────────────────────────────────────
--
-- 形狀刻意與 xp_event_types 一致:同一個問題(「有哪些值是合法的」)
-- 不該有兩種解法。用註冊表而非 Go const 的理由也相同——
-- 加一個新用途不該要改程式碼、重編、重部署。
CREATE TABLE platform.channel_purposes (
  key         TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  description TEXT,
  -- 停用只影響後台選單,既有設定照常運作(拔掉選項不該讓公告突然消失)
  enabled     BOOLEAN NOT NULL DEFAULT true,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform.channel_purposes (key, name, description) VALUES
  ('announcements',      '一般公告',   '系統公告、活動宣布'),
  ('rules',              '規則說明',   '社群規則、資料記錄告知'),
  ('tournament_results', '賽事結果',   '對戰結果與名次'),
  ('bot_log',            '機器人紀錄', '管理操作與異常;建議設成只有管理員看得到的頻道'),
  ('welcome',            '新成員歡迎', '新成員加入時的歡迎訊息'),
  ('level_up',           '升級訊息',   '里程碑升級通知(schemas/24)');

-- ── 逐空間的頻道落點 ──────────────────────────────────────────
CREATE TABLE platform.space_channel_purposes (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  space_id            BIGINT NOT NULL REFERENCES platform.community_spaces(id),
  purpose             TEXT   NOT NULL REFERENCES platform.channel_purposes(key),
  channel_external_id TEXT   NOT NULL,
  updated_by          BIGINT REFERENCES platform.users(id),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- 一個空間的「公告區」只能有一個。沒有這條就會出現兩個公告頻道,
  -- 而程式只會挑到其中一個,另一個永遠是空的——而且沒有任何錯誤訊息。
  CONSTRAINT space_channel_purposes_uq UNIQUE (space_id, purpose),

  -- 頻道必須先在 space_channels 註冊過。這是選獨立表而非 JSONB 的全部意義:
  -- 打錯 channel id 在寫入當下就被擋掉,而不是等到貼不出去才發現。
  --
  -- 代價是 space_channels 的語意擴大成「這個空間認得的頻道清單」,
  -- 而不只是「訊息記錄白名單」。管理指令在設定用途時會自動補建缺少的列
  -- (log_messages=false, grant_xp=true,即欄位預設),使用者不會感覺多一步。
  CONSTRAINT space_channel_purposes_channel_fkey
    FOREIGN KEY (space_id, channel_external_id)
    REFERENCES platform.space_channels (space_id, external_id)
);

-- 反查「這個頻道被設成了什麼用途」——刪頻道或排查時會用到
CREATE INDEX space_channel_purposes_channel_idx
  ON platform.space_channel_purposes (space_id, channel_external_id);

-- ── 權限註冊表 ────────────────────────────────────────────────
--
-- 9 個權限字串原本只存在於 role_permissions 的 seed 裡,沒有地方說明
-- 它們各是什麼。後台要列出「有哪些權限可以勾」就需要這張表。
--
-- is_dangerous 只影響顯示:勾選時跳警告,但不阻擋。這是 grill Q5 的定案——
-- 9 個權限全部可以指派給任何角色(含由 Discord 身分組同步而來的),
-- 標記存在的目的是讓設定的人知道自己在做什麼,而不是替他決定。
CREATE TABLE platform.permissions (
  key          TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  description  TEXT NOT NULL,
  is_dangerous BOOLEAN NOT NULL DEFAULT false,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform.permissions (key, name, description, is_dangerous) VALUES
  ('logs.read',         '查詢紀錄',     '查詢操作紀錄與活動紀錄', false),
  ('logs.read_deleted', '查看已刪訊息', '查看使用者已刪除的訊息內容;此動作本身也會進稽核紀錄', true),
  ('redemption.handle', '處理兌換',     '核可或退回實體獎品的兌換申請', false),
  ('user.restrict',     '限制使用者',   '凍結交易、停止賺取、停權', false),
  ('shop.manage',       '管理商店',     '商品上下架與定價', false),
  ('roles.manage',      '管理角色',     '建立角色、指派權限、綁定 Discord 身分組;等於能給出任何其他權限', true),
  ('economy.config',    '修改經濟設定', '簽到額度、手續費率、XP 規則等數值', true),
  ('economy.grant',     '發放點數',     '直接增減任何使用者的餘額', true),
  ('economy.refund',    '退款',         '對既有分錄開立沖銷', true);

-- seed 必須先進去,外鍵才加得上(既有 21 組 role_permissions 都在這 9 個裡)
ALTER TABLE platform.role_permissions
  ADD CONSTRAINT role_permissions_permission_fkey
  FOREIGN KEY (permission) REFERENCES platform.permissions(key);

-- ── 社群自訂角色 ──────────────────────────────────────────────
ALTER TABLE platform.roles
  ADD COLUMN public_id    TEXT,
  -- NULL = 內建角色,所有社群共用
  ADD COLUMN community_id BIGINT REFERENCES platform.communities(id),
  ADD COLUMN created_by   BIGINT REFERENCES platform.users(id);

-- 內建 4 個角色的 public_id 刻意是固定值,不是隨機 ULID。
--
-- 它們在每個部署裡都是同一批東西(不是使用者資料),所以「可預測」
-- 沒有任何可洩漏的規模資訊;反過來,固定值讓文件、測試與救援腳本
-- 引用得到它們。長度仍是 26,字元仍在 Crockford base32 集合內,
-- 所以對外格式與一般 public_id 無法區分。
UPDATE platform.roles SET public_id = '00000000000000000000000001' WHERE key = 'owner';
UPDATE platform.roles SET public_id = '00000000000000000000000002' WHERE key = 'admin';
UPDATE platform.roles SET public_id = '00000000000000000000000003' WHERE key = 'moderator';
UPDATE platform.roles SET public_id = '00000000000000000000000004' WHERE key = 'auditor';

ALTER TABLE platform.roles
  ALTER COLUMN public_id SET NOT NULL,
  ADD CONSTRAINT roles_public_id_key UNIQUE (public_id);

-- key 從全域唯一改成「每個社群一套」。
--
-- 沒有這個改動,第二個社群就建不出自己的 vip —— 只能叫 vip2,
-- 而那個名字會一路出現在管理介面與 Discord 身分組上。
ALTER TABLE platform.roles DROP CONSTRAINT roles_key_key;

-- COALESCE 而不是把 community_id 設成 NOT NULL DEFAULT 0:
-- 0 不是合法的 communities.id,設 NOT NULL 就沒辦法加外鍵。
-- 寫法與 user_roles 現有的唯一索引一致(schemas/03:「PG 不支援表達式主鍵」)。
CREATE UNIQUE INDEX roles_scope_key_uq
  ON platform.roles (COALESCE(community_id, 0), key);

CREATE INDEX roles_community_idx
  ON platform.roles (community_id) WHERE community_id IS NOT NULL;

COMMENT ON COLUMN platform.roles.community_id IS
  'NULL = 內建角色(owner/admin/moderator/auditor),管理端不得刪除或改權限:'
  '它們是系統啟動與緊急救援的依據,刪掉之後沒有人能修好自己的權限。'
  '社群要不同的權限組合,複製一份成自己的角色。';

-- +goose Down
DROP INDEX platform.roles_community_idx;
DROP INDEX platform.roles_scope_key_uq;
ALTER TABLE platform.roles ADD CONSTRAINT roles_key_key UNIQUE (key);
ALTER TABLE platform.roles
  DROP CONSTRAINT roles_public_id_key,
  DROP COLUMN created_by,
  DROP COLUMN community_id,
  DROP COLUMN public_id;
ALTER TABLE platform.role_permissions DROP CONSTRAINT role_permissions_permission_fkey;
DROP TABLE platform.permissions;
DROP TABLE platform.space_channel_purposes;
DROP TABLE platform.channel_purposes;
