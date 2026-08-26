-- name: CreateUser :one
INSERT INTO platform.users (public_id, display_name, avatar_url)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM platform.users WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByPublicID :one
SELECT * FROM platform.users WHERE public_id = $1 AND deleted_at IS NULL;

-- name: GetUserByIdentity :one
SELECT u.* FROM platform.users u
JOIN platform.identities i ON i.user_id = u.id
WHERE i.provider = $1 AND i.provider_user_id = $2 AND u.deleted_at IS NULL;

-- name: CreateIdentity :one
INSERT INTO platform.identities
  (user_id, provider, provider_user_id, username, access_token_enc, refresh_token_enc, scope, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateIdentityTokens :exec
UPDATE platform.identities
SET access_token_enc = $2, refresh_token_enc = $3, scope = $4, expires_at = $5
WHERE id = $1;

-- name: UpdateUserTimezone :one
UPDATE platform.users
SET timezone = $2, timezone_changed_at = now()
WHERE id = $1
RETURNING timezone, timezone_changed_at;

-- name: TouchUserLastSeen :exec
UPDATE platform.users SET last_seen_at = now() WHERE id = $1;
