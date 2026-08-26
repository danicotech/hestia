-- +goose Up
-- XP/等級規則集;多個 community 可指向同一套(grill Q4b)
CREATE TABLE platform.xp_rulesets (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        TEXT        NOT NULL,
  config      JSONB       NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 邏輯社群 = XP 池的邊界。M1 只有一列(grill Q4c:共用池)
CREATE TABLE platform.communities (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id     TEXT        NOT NULL UNIQUE,
  name          TEXT        NOT NULL,
  xp_ruleset_id BIGINT      REFERENCES platform.xp_rulesets(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 實體空間:Discord guild / Twitch 頻道 / YT 頻道
-- 未來接 Twitch = 插一列,自動共用 XP 池,不改程式
CREATE TABLE platform.community_spaces (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id    TEXT        NOT NULL UNIQUE,
  community_id BIGINT      NOT NULL REFERENCES platform.communities(id),
  provider     TEXT        NOT NULL,            -- discord | twitch | youtube
  external_id  TEXT        NOT NULL,            -- snowflake 一律 TEXT(JS 精度)
  name         TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (provider, external_id)
);

-- +goose Down
DROP TABLE platform.community_spaces;
DROP TABLE platform.communities;
DROP TABLE platform.xp_rulesets;
