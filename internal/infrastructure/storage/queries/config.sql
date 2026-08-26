-- 經濟設定:不覆寫舊值,讀取取「生效時間最新」的一筆

-- name: GetCurrentConfig :one
SELECT value FROM platform.economy_configs
WHERE key = $1 AND effective_at <= now()
ORDER BY effective_at DESC
LIMIT 1;

-- name: ListCurrentConfigs :many
SELECT DISTINCT ON (key) key, value, effective_at
FROM platform.economy_configs
WHERE effective_at <= now()
ORDER BY key, effective_at DESC;

-- name: InsertConfig :one
-- 調整 = 插新列(必帶 created_by;seed 列 created_by 為 NULL)
INSERT INTO platform.economy_configs (key, value, effective_at, created_by, note)
VALUES ($1, $2, COALESCE(sqlc.narg('effective_at')::timestamptz, now()), $3, $4)
RETURNING *;
