-- 管理端授權查詢(schemas/03-authz.md)。權限字串的權威在 Go
-- (core/platform/authz),這裡只問「這個人現在有沒有這個權限」。

-- name: UserHasPermission :one
-- 單一查詢完成三件事,因為它在每個管理請求的路徑上,不能 N+1:
--   ① 使用者仍存在且未軟刪除
--   ② 有一條未過期的 user_roles,其角色擁有該權限
--   ③ 沒有生效中的 full_ban
--
-- community_id 參數:
--   NULL = 不限範圍(M1 只有一個社群),全域角色與任何社群的角色都算;
--   有值 = 只認全域角色(user_roles.community_id IS NULL)與該社群的角色。
--
-- 刻意不 JOIN platform.roles:role_permissions 以 role_id 為鍵,
-- 多接一張表只是多一次探測,判定結果完全相同。
--
-- 三個條件都用 EXISTS 而不是 JOIN:一找到就短路,而且「有沒有」的語意
-- 不會因為多列而重複。停權與軟刪除放在同一句(而不是先查再判)是為了
-- 讓「檢查」與「授權」不可能被拆開漏掉其中一步。
SELECT (
  EXISTS (
    SELECT 1 FROM platform.users u
     WHERE u.id = sqlc.arg(user_id) AND u.deleted_at IS NULL
  )
  AND EXISTS (
    SELECT 1
      FROM platform.user_roles ur
      JOIN platform.role_permissions rp ON rp.role_id = ur.role_id
     WHERE ur.user_id = sqlc.arg(user_id)
       AND rp.permission = sqlc.arg(permission)
       AND (ur.expires_at IS NULL OR ur.expires_at > now())
       AND (
             ur.community_id IS NULL
          OR sqlc.narg(community_id)::bigint IS NULL
          OR ur.community_id = sqlc.narg(community_id)::bigint
       )
  )
  AND NOT EXISTS (
    SELECT 1 FROM platform.user_restrictions r
     WHERE r.user_id = sqlc.arg(user_id)
       AND r.kind = 'full_ban'
       AND r.lifted_at IS NULL
       AND (r.expires_at IS NULL OR r.expires_at > now())
  )
) AS allowed;
