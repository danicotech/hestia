-- +goose Up
-- 語音是可靠的參與訊號,可發點數(grill Q13);保留 90 天,按月分區
CREATE TABLE platform.voice_sessions (
  id               BIGINT GENERATED ALWAYS AS IDENTITY,
  user_id          BIGINT      NOT NULL REFERENCES platform.users(id),
  space_id         BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  channel_id       TEXT        NOT NULL,
  joined_at        TIMESTAMPTZ NOT NULL,
  left_at          TIMESTAMPTZ,
  duration_seconds INT,
  self_muted       BOOLEAN,
  self_deafened    BOOLEAN,
  peer_count_avg   REAL,                        -- 防掛機:需 >= 2 人才計時
  PRIMARY KEY (id, joined_at)
) PARTITION BY RANGE (joined_at);
CREATE INDEX ON platform.voice_sessions (user_id, joined_at DESC);
CREATE TABLE platform.voice_sessions_default PARTITION OF platform.voice_sessions DEFAULT;
SELECT platform.create_month_partition('platform.voice_sessions', now()::date);
SELECT platform.create_month_partition('platform.voice_sessions', (now() + interval '1 month')::date);

-- presence 存「區間」不存「事件」:統計不用做狀態機配對(grill Q13)
-- 注意:presence 不可靠(特權 intent、Bot 重啟漏資料),不可拿來發點數
CREATE TABLE platform.presence_spans (
  id         BIGINT GENERATED ALWAYS AS IDENTITY,
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  space_id   BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  status     TEXT        NOT NULL,              -- online / idle / dnd / offline
  started_at TIMESTAMPTZ NOT NULL,
  ended_at   TIMESTAMPTZ,
  PRIMARY KEY (id, started_at)
) PARTITION BY RANGE (started_at);
CREATE INDEX ON platform.presence_spans (user_id, started_at DESC);
CREATE TABLE platform.presence_spans_default PARTITION OF platform.presence_spans DEFAULT;
SELECT platform.create_month_partition('platform.presence_spans', now()::date);
SELECT platform.create_month_partition('platform.presence_spans', (now() + interval '1 month')::date);

-- 彙總層:永久保留,原始事件刪掉後統計仍在(寫入時彙總,grill Q15)
CREATE TABLE platform.activity_daily (
  user_id        BIGINT NOT NULL REFERENCES platform.users(id),
  community_id   BIGINT NOT NULL REFERENCES platform.communities(id),
  date           DATE   NOT NULL,
  messages       INT    NOT NULL DEFAULT 0,
  reactions      INT    NOT NULL DEFAULT 0,
  voice_seconds  INT    NOT NULL DEFAULT 0,
  online_seconds INT    NOT NULL DEFAULT 0,
  first_seen_at  TIMESTAMPTZ,
  last_seen_at   TIMESTAMPTZ,
  xp_gained      BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, community_id, date)
);

-- +goose Down
DROP TABLE platform.activity_daily;
DROP TABLE platform.presence_spans;
DROP TABLE platform.voice_sessions;
