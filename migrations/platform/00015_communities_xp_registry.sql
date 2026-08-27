-- +goose Up
-- schemas/01 增補 A–D 與 schemas/06 增補 E(2026-08-27 逐點確認)

-- D:space 層設定(公告/通知落點頻道 id、locale)——通知管線消費 outbox 時的目的地
ALTER TABLE platform.community_spaces
  ADD COLUMN config JSONB NOT NULL DEFAULT '{}';

-- B:成員在籍史。一段在籍一列;退群補 left_at,重進插新列
-- 入群天數(交易門檻)= 該 user 的 MIN(joined_at),離群不歸零
CREATE TABLE platform.space_members (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  space_id   BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  joined_at  TIMESTAMPTZ NOT NULL,
  left_at    TIMESTAMPTZ,                       -- NULL = 在籍
  leave_kind TEXT                               -- leave | kick | ban;事件拿不到就 NULL
);
CREATE UNIQUE INDEX space_members_active_uq
  ON platform.space_members (space_id, user_id) WHERE left_at IS NULL;
CREATE INDEX space_members_user_idx ON platform.space_members (user_id);

-- C:頻道註冊 = 訊息白名單的權威(schemas/11 的白名單存這裡)
-- AI 語料(schemas/12)頻道範圍跟隨 log_messages,不另設旗標
CREATE TABLE platform.space_channels (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  space_id     BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  external_id  TEXT        NOT NULL,            -- channel id(TEXT,JS 精度)
  name         TEXT,                            -- 快照,顯示用
  kind         TEXT        NOT NULL,            -- text | forum | voice | announcement
  log_messages BOOLEAN     NOT NULL DEFAULT false, -- 白名單本體:false 不寫 message_logs
  grant_xp     BOOLEAN     NOT NULL DEFAULT true,  -- 指令區/刷頻區可關
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at  TIMESTAMPTZ,
  UNIQUE (space_id, external_id)
);

-- E:事件類型是資料(同 currencies 哲學)——新增事件 = INSERT,全域開關 = UPDATE
-- registry 只管「存在與開關」;每社群數值在 xp_rulesets.config.sources[key](schemas/01 A)
CREATE TABLE platform.xp_event_types (
  key         TEXT        PRIMARY KEY,
  name        TEXT        NOT NULL,
  description TEXT,
  enabled     BOOLEAN     NOT NULL DEFAULT true, -- 關掉即停止入帳,不用部署
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform.xp_event_types (key, name, description) VALUES
  ('discord_message', '發言',       '冷卻與每日上限在 xp_rulesets.config'),
  ('discord_voice',   '語音時長',   '需 ≥2 人未靜音(economy_configs.voice_xp_min_peers)'),
  ('admin',           '管理員調整', '手動增減,ref_id 記操作脈絡');

-- source 值域由 DB 保證(原自由 TEXT)
ALTER TABLE platform.xp_events
  ADD CONSTRAINT xp_events_source_fkey
  FOREIGN KEY (source) REFERENCES platform.xp_event_types(key);

-- +goose Down
ALTER TABLE platform.xp_events DROP CONSTRAINT xp_events_source_fkey;
DROP TABLE platform.xp_event_types;
DROP TABLE platform.space_channels;
DROP TABLE platform.space_members;
ALTER TABLE platform.community_spaces DROP COLUMN config;
