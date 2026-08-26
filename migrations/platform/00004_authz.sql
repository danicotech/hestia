-- +goose Up
-- 權限權威在自家 DB,第三方身分只是綁定來源(grill Q10)
CREATE TABLE platform.roles (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key        TEXT        NOT NULL UNIQUE,       -- owner | admin | moderator | auditor
  name       TEXT        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 權限常數寫在程式碼,此表只存對應(不做 UI 可編輯的 permission)
CREATE TABLE platform.role_permissions (
  role_id    BIGINT NOT NULL REFERENCES platform.roles(id),
  permission TEXT   NOT NULL,
  PRIMARY KEY (role_id, permission)
);

-- source 是關鍵欄位:同步 Discord 身分組只撤 provider_sync,手動授予不受影響
-- 註:原設計 PK 含 COALESCE(community_id,0),PG 不允許表達式主鍵 → 改代理鍵 + 表達式唯一索引
CREATE TABLE platform.user_roles (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id      BIGINT      NOT NULL REFERENCES platform.users(id),
  role_id      BIGINT      NOT NULL REFERENCES platform.roles(id),
  community_id BIGINT      REFERENCES platform.communities(id),   -- NULL = 全域
  source       TEXT        NOT NULL,            -- manual | provider_sync
  granted_by   BIGINT      REFERENCES platform.users(id),
  granted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX user_roles_uniq
  ON platform.user_roles (user_id, role_id, COALESCE(community_id, 0));

-- 角色 ↔ 第三方身分映射,方向:provider → 系統
CREATE TABLE platform.role_provider_bindings (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  role_id          BIGINT NOT NULL REFERENCES platform.roles(id),
  space_id         BIGINT NOT NULL REFERENCES platform.community_spaces(id),
  provider         TEXT   NOT NULL,
  external_role_id TEXT   NOT NULL,
  sync_mode        TEXT   NOT NULL DEFAULT 'import',
  UNIQUE (space_id, external_role_id, role_id)
);

-- reason 刻意 NOT NULL:強迫在動作當下寫理由(grill Q10)
CREATE TABLE platform.admin_audit_logs (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_user_id BIGINT      NOT NULL REFERENCES platform.users(id),
  action        TEXT        NOT NULL,
  target_type   TEXT,
  target_id     BIGINT,
  before        JSONB,
  after         JSONB,
  reason        TEXT        NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON platform.admin_audit_logs (actor_user_id, created_at DESC);

-- 停權三級:no_trade / no_earn / full_ban(grill Q10:疑似洗點只凍交易,不趕人)
CREATE TABLE platform.user_restrictions (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  kind       TEXT        NOT NULL,
  reason     TEXT        NOT NULL,
  created_by BIGINT      NOT NULL REFERENCES platform.users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ,
  lifted_at  TIMESTAMPTZ
);
CREATE INDEX ON platform.user_restrictions (user_id) WHERE lifted_at IS NULL;

-- +goose Down
DROP TABLE platform.user_restrictions;
DROP TABLE platform.admin_audit_logs;
DROP TABLE platform.role_provider_bindings;
DROP TABLE platform.user_roles;
DROP TABLE platform.role_permissions;
DROP TABLE platform.roles;
