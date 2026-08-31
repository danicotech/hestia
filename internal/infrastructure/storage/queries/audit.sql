-- name: InsertAdminAudit :one
-- reason NOT NULL 是刻意的:強迫動作當下寫理由(schemas/03)
INSERT INTO platform.admin_audit_logs
  (actor_user_id, action, target_type, target_id, before, after, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id;

-- name: ListAdminAuditByActor :many
SELECT * FROM platform.admin_audit_logs
WHERE actor_user_id = $1
ORDER BY created_at DESC
LIMIT $2;
