-- +goose Up
-- 幣別是資料不是欄位:新增幣別 = INSERT 一列,零 migration(grill Q4)
CREATE TABLE platform.currencies (
  code       TEXT PRIMARY KEY,                  -- M1 只有 coin
  name       TEXT NOT NULL,
  symbol     TEXT,
  tradable   BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 所有經濟數值存 DB 可調;不覆寫舊值,以 effective_at 切換留歷史(grill Q5b)
CREATE TABLE platform.economy_configs (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key          TEXT        NOT NULL,
  value        JSONB       NOT NULL,
  effective_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by   BIGINT      REFERENCES platform.users(id),
  note         TEXT,
  UNIQUE (key, effective_at)
);
CREATE INDEX ON platform.economy_configs (key, effective_at DESC);

-- +goose Down
DROP TABLE platform.economy_configs;
DROP TABLE platform.currencies;
