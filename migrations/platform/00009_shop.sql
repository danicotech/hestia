-- +goose Up
-- 商店:三種履約(auto_role / auto_perk / manual),政策全參數化(grill Q6/Q6a)
CREATE TABLE platform.shop_items (
  id                        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id                 TEXT        NOT NULL UNIQUE,
  name                      TEXT        NOT NULL,
  description               TEXT,
  fulfillment               TEXT        NOT NULL,   -- auto_role | auto_perk | manual
  currency                  TEXT        NOT NULL REFERENCES platform.currencies(code),
  price                     BIGINT      NOT NULL CHECK (price >= 0),
  duration_days             INT,                    -- NULL = 永久
  stock                     INT,                    -- NULL = 不限量(M1 不啟用全域限量)
  per_user_limit            INT,                    -- NULL = 不限
  refund_window_seconds     INT         NOT NULL DEFAULT 600,  -- 0 = 不可退
  grants_item_definition_id BIGINT,                 -- FK 於 00010 補上(item_definitions 在後)
  external_role_id          TEXT,                   -- auto_role 用的 Discord role id
  listed_at                 TIMESTAMPTZ,
  delisted_at               TIMESTAMPTZ,            -- 絕版制:下架後不再產出(grill Q9)
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- soulbound 權益(身分組/稱號):持續為真的狀態
-- refundable_until 存欄位不現算 —— 窗口長度會逐商品調(grill Q6a)
CREATE TABLE platform.entitlements (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id          BIGINT      NOT NULL REFERENCES platform.users(id),
  item_id          BIGINT      NOT NULL REFERENCES platform.shop_items(id),
  granted_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at       TIMESTAMPTZ,                 -- NULL = 永久
  refundable_until TIMESTAMPTZ,
  revoked_at       TIMESTAMPTZ
);
CREATE INDEX ON platform.entitlements (user_id) WHERE revoked_at IS NULL;
CREATE INDEX ON platform.entitlements (expires_at)
  WHERE revoked_at IS NULL AND expires_at IS NOT NULL;   -- 到期回收排程器掃描用

-- 人工履約的一次性工單(與 entitlements 分表:生命週期不同,grill Q6)
CREATE TABLE platform.redemptions (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id  TEXT        NOT NULL UNIQUE,
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  item_id    BIGINT      NOT NULL REFERENCES platform.shop_items(id),
  status     TEXT        NOT NULL DEFAULT 'pending',  -- pending / fulfilled / rejected / cancelled
  note       TEXT,
  handled_by BIGINT      REFERENCES platform.users(id),
  handled_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON platform.redemptions (status) WHERE status = 'pending';

-- +goose Down
DROP TABLE platform.redemptions;
DROP TABLE platform.entitlements;
DROP TABLE platform.shop_items;
