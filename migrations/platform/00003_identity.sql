-- +goose Up
CREATE TABLE platform.users (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id           TEXT        NOT NULL UNIQUE,     -- ULID,API 只露這個
  display_name        TEXT,
  avatar_url          TEXT,
  timezone            TEXT        NOT NULL DEFAULT 'Asia/Taipei',
  timezone_changed_at TIMESTAMPTZ,                     -- 變更冷卻(防一天簽兩次)
  email               TEXT,                            -- 非必填,無密碼登入
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at        TIMESTAMPTZ,
  deleted_at          TIMESTAMPTZ                      -- 軟刪除;真刪用匿名化
);

-- 一人多 provider,內部 users.id 是唯一權威(grill Q2)
-- token 加密:應用層 AES-256-GCM,密文格式 v<版本>:<nonce>:<ct>,金鑰在環境變數
CREATE TABLE platform.identities (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id           BIGINT      NOT NULL REFERENCES platform.users(id),
  provider          TEXT        NOT NULL,       -- discord | twitch | youtube | local
  provider_user_id  TEXT        NOT NULL,
  username          TEXT,
  access_token_enc  BYTEA,
  refresh_token_enc BYTEA,
  scope             TEXT,
  expires_at        TIMESTAMPTZ,
  linked_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_user_id)
);
CREATE INDEX ON platform.identities (user_id);

-- 兩級 optout(grill Q13/Q16):不記錄 ≠ 不入 AI 語料
CREATE TABLE platform.user_privacy_settings (
  user_id           BIGINT PRIMARY KEY REFERENCES platform.users(id),
  opt_out_logging   BOOLEAN NOT NULL DEFAULT false,
  opt_out_ai_corpus BOOLEAN NOT NULL DEFAULT false,
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE platform.user_privacy_settings;
DROP TABLE platform.identities;
DROP TABLE platform.users;
