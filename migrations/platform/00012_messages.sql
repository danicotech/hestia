-- +goose Up
-- ══ 事實層(短保留,按月分區)══
-- 訊息摘要:保留 365 天(grill Q17)。excerpt 截斷長度由 economy_configs 控制
CREATE TABLE platform.message_logs (
  id             BIGINT GENERATED ALWAYS AS IDENTITY,
  user_id        BIGINT      NOT NULL REFERENCES platform.users(id),
  space_id       BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  channel_id     TEXT        NOT NULL,
  channel_kind   TEXT        NOT NULL,          -- text / forum / thread / voice_text
  thread_id      TEXT,
  message_id     TEXT        NOT NULL,
  excerpt        TEXT,
  full_length    INT,                           -- 原始長度:檢討截斷是否太短
  has_attachment BOOLEAN     NOT NULL DEFAULT false,
  reply_to       TEXT,
  created_at     TIMESTAMPTZ NOT NULL,
  deleted_at     TIMESTAMPTZ,
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE UNIQUE INDEX ON platform.message_logs (message_id, created_at);
CREATE INDEX ON platform.message_logs (user_id, created_at DESC);
CREATE TABLE platform.message_logs_default PARTITION OF platform.message_logs DEFAULT;
SELECT platform.create_month_partition('platform.message_logs', now()::date);
SELECT platform.create_month_partition('platform.message_logs', (now() + interval '1 month')::date);

-- 已刪除/編輯的舊版本:保留 90 天、僅 admin 可查、查詢寫 audit log(grill Q15 三道約束)
CREATE TABLE platform.message_revisions (
  id          BIGINT GENERATED ALWAYS AS IDENTITY,
  message_id  TEXT        NOT NULL,
  user_id     BIGINT      NOT NULL REFERENCES platform.users(id),
  excerpt     TEXT,
  kind        TEXT        NOT NULL,             -- edited / deleted
  captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, captured_at)
) PARTITION BY RANGE (captured_at);
CREATE TABLE platform.message_revisions_default PARTITION OF platform.message_revisions DEFAULT;
SELECT platform.create_month_partition('platform.message_revisions', now()::date);
SELECT platform.create_month_partition('platform.message_revisions', (now() + interval '1 month')::date);

-- Reaction:量小、不涉內容隱私、比發言難刷(grill Q15)
CREATE TABLE platform.reaction_events (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  message_id TEXT        NOT NULL,
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  space_id   BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  emoji      TEXT        NOT NULL,
  added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  removed_at TIMESTAMPTZ
);
CREATE INDEX ON platform.reaction_events (message_id);

-- ══ 衍生層(永久,體積小;寫入時彙總)══
CREATE TABLE platform.user_term_counts (
  user_id BIGINT NOT NULL REFERENCES platform.users(id),
  period  TEXT   NOT NULL,                      -- 2026-08 / 2026 / all
  term    TEXT   NOT NULL,
  count   INT    NOT NULL,
  PRIMARY KEY (user_id, period, term)
);

CREATE TABLE platform.message_stats (
  message_id     TEXT PRIMARY KEY,
  user_id        BIGINT NOT NULL REFERENCES platform.users(id),
  reaction_count INT    NOT NULL DEFAULT 0,
  reply_count    INT    NOT NULL DEFAULT 0,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 月度精選:excerpt 是快照,不依賴原文是否還在
CREATE TABLE platform.highlights (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  period     TEXT        NOT NULL,
  message_id TEXT        NOT NULL,
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  excerpt    TEXT        NOT NULL,
  score      INT         NOT NULL,
  vote_count INT         NOT NULL DEFAULT 0,
  selected   BOOLEAN     NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (period, message_id)
);

-- +goose Down
DROP TABLE platform.highlights;
DROP TABLE platform.message_stats;
DROP TABLE platform.user_term_counts;
DROP TABLE platform.reaction_events;
DROP TABLE platform.message_revisions;
DROP TABLE platform.message_logs;
