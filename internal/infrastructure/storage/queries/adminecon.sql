-- 管理員經濟操作 query。動錢一律走 ledger 的 ApplyInTx,這裡只有讀取;
-- audit 寫入用 audit.sql 的 InsertAdminAudit,不重複定義。

-- name: GetTokenEntryByID :one
-- 退款前讀原分錄(分區表,依 id 掃全分區;管理操作低頻,可接受)
SELECT * FROM platform.token_entries WHERE id = $1;
