-- +goose Up
-- 可交易物品定義:tradable 預設 false,只有收藏品/消耗品開啟(grill Q8)
-- max_supply 讓「全世界只有 1 個」成立(grill Q9)
CREATE TABLE platform.item_definitions (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id       TEXT        NOT NULL UNIQUE,
  name            TEXT        NOT NULL,
  category        TEXT        NOT NULL,         -- collectible / consumable / badge / role / prize
  rarity          TEXT,
  icon_url        TEXT,
  tradable        BOOLEAN     NOT NULL DEFAULT false,
  bind_on_acquire BOOLEAN     NOT NULL DEFAULT false,
  stackable       BOOLEAN     NOT NULL DEFAULT false,
  max_supply      INT,                          -- NULL = 不限
  minted_count    INT         NOT NULL DEFAULT 0,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE platform.shop_items
  ADD CONSTRAINT shop_items_grants_fk
  FOREIGN KEY (grants_item_definition_id) REFERENCES platform.item_definitions(id);

-- 物品實例:owner 換手 = 交易完成
-- locked_by_trade_id 不可省:沒鎖就能把同一件同時賣給兩個人(grill Q7a)
CREATE TABLE platform.item_instances (
  id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id          TEXT        NOT NULL UNIQUE,
  definition_id      BIGINT      NOT NULL REFERENCES platform.item_definitions(id),
  owner_id           BIGINT      NOT NULL REFERENCES platform.users(id),
  bound              BOOLEAN     NOT NULL DEFAULT false,
  quantity           INT         NOT NULL DEFAULT 1 CHECK (quantity > 0),
  locked_by_trade_id BIGINT,
  acquired_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  acquired_via       TEXT                       -- shop / tournament / admin / market / trade
);
CREATE INDEX ON platform.item_instances (owner_id);
CREATE INDEX ON platform.item_instances (definition_id);

-- 掛單寄售(M1);面交 trade_sessions 排 M3 後段
CREATE TABLE platform.market_listings (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id        TEXT        NOT NULL UNIQUE,
  seller_id        BIGINT      NOT NULL REFERENCES platform.users(id),
  item_instance_id BIGINT      NOT NULL REFERENCES platform.item_instances(id),
  currency         TEXT        NOT NULL REFERENCES platform.currencies(code),
  price            BIGINT      NOT NULL CHECK (price > 0),
  status           TEXT        NOT NULL DEFAULT 'open',  -- open / sold / cancelled / expired
  listed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at       TIMESTAMPTZ
);
CREATE INDEX ON platform.market_listings (status, listed_at DESC) WHERE status = 'open';

-- 成交紀錄:稽核與淨流向報表(防洗點)的資料來源
CREATE TABLE platform.market_orders (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  listing_id BIGINT      NOT NULL REFERENCES platform.market_listings(id),
  buyer_id   BIGINT      NOT NULL REFERENCES platform.users(id),
  seller_id  BIGINT      NOT NULL REFERENCES platform.users(id),
  price      BIGINT      NOT NULL,
  fee        BIGINT      NOT NULL,
  settled_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON platform.market_orders (buyer_id, settled_at DESC);
CREATE INDEX ON platform.market_orders (seller_id, settled_at DESC);

-- +goose Down
DROP TABLE platform.market_orders;
DROP TABLE platform.market_listings;
DROP TABLE platform.item_instances;
ALTER TABLE platform.shop_items DROP CONSTRAINT shop_items_grants_fk;
DROP TABLE platform.item_definitions;
