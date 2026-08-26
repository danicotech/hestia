-- +goose Up
-- 原始事件:曲線一定會調,有這張表才能重算歷史(grill Q4b)
CREATE TABLE platform.xp_events (
  id           BIGINT GENERATED ALWAYS AS IDENTITY,
  user_id      BIGINT      NOT NULL REFERENCES platform.users(id),
  community_id BIGINT      NOT NULL REFERENCES platform.communities(id),
  space_id     BIGINT      REFERENCES platform.community_spaces(id),
  source       TEXT        NOT NULL,            -- discord_message | discord_voice | twitch_watch | admin
  amount       BIGINT      NOT NULL,
  ref_id       TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX ON platform.xp_events (user_id, created_at DESC);
CREATE TABLE platform.xp_events_default PARTITION OF platform.xp_events DEFAULT;
SELECT platform.create_month_partition('platform.xp_events', now()::date);
SELECT platform.create_month_partition('platform.xp_events', (now() + interval '1 month')::date);

-- 投影表,可從 xp_events 重算;last_xp_at 做冷卻(不上 Redis)
CREATE TABLE platform.user_xp (
  user_id      BIGINT      NOT NULL REFERENCES platform.users(id),
  community_id BIGINT      NOT NULL REFERENCES platform.communities(id),
  xp           BIGINT      NOT NULL DEFAULT 0,
  level        INT         NOT NULL DEFAULT 0,
  last_xp_at   TIMESTAMPTZ,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, community_id)
);
CREATE INDEX ON platform.user_xp (community_id, xp DESC);   -- 排行榜

-- +goose Down
DROP TABLE platform.user_xp;
DROP TABLE platform.xp_events;
