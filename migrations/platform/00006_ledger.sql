-- +goose Up
-- ══════════════ 帳本:整個系統的事實來源 ══════════════
-- append-only,永不 UPDATE/DELETE。錯帳用沖銷分錄。
-- 分區表的 PK 必須含分區鍵 → (id, created_at)
CREATE TABLE platform.token_entries (
  id         BIGINT GENERATED ALWAYS AS IDENTITY,
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  currency   TEXT        NOT NULL REFERENCES platform.currencies(code),
  amount     BIGINT      NOT NULL,              -- 正負皆可;絕不用 float
  reason     TEXT        NOT NULL,              -- 枚舉見 schema 文件 §5
  ref_type   TEXT,
  ref_id     BIGINT,
  actor_id   BIGINT      REFERENCES platform.users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX ON platform.token_entries (user_id, created_at DESC);
CREATE INDEX ON platform.token_entries (reason, created_at);

-- DEFAULT 分區當安全網:排程器漏建月分區時寫入不失敗
CREATE TABLE platform.token_entries_default PARTITION OF platform.token_entries DEFAULT;
SELECT platform.create_month_partition('platform.token_entries', now()::date);
SELECT platform.create_month_partition('platform.token_entries', (now() + interval '1 month')::date);

-- 餘額是快取,同 transaction 更新;獨立表讓多幣別零 migration(grill Q4)
CREATE TABLE platform.user_balances (
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  currency   TEXT        NOT NULL REFERENCES platform.currencies(code),
  balance    BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, currency)
);
CREATE INDEX ON platform.user_balances (currency, balance DESC);   -- 排行榜

-- 動錢 API 的冪等鍵(簽到除外:靠 daily_claims 的 UNIQUE)
CREATE TABLE platform.idempotency_keys (
  key          TEXT PRIMARY KEY,
  request_hash TEXT        NOT NULL,
  response     JSONB,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 領域變更與事件同 transaction;消費用 FOR UPDATE SKIP LOCKED
CREATE TABLE platform.outbox_events (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  topic         TEXT        NOT NULL,
  payload       JSONB       NOT NULL,
  status        TEXT        NOT NULL DEFAULT 'pending',
  attempts      INT         NOT NULL DEFAULT 0,
  next_retry_at TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON platform.outbox_events (status, next_retry_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE platform.outbox_events;
DROP TABLE platform.idempotency_keys;
DROP TABLE platform.user_balances;
DROP TABLE platform.token_entries;
