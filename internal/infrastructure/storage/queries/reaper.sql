-- 限時權益到期回收(schemas/14 entitlement_reaper、schemas/08 部分索引)。
-- 本檔只有到期掃描與撤銷;outbox 寫入沿用 ledger.sql 的 InsertOutboxEvent
-- (一個概念一個權威,不重複)。entitlements 之外的表僅共享讀。

-- name: LockExpiredEntitlements :many
-- FOR UPDATE OF e SKIP LOCKED:多實例回收互不重複,也不與退款互擋——
-- shoppg 退款持有的 entitlement 列鎖讓本輪直接跳過,下一輪再看
-- (屆時要嘛已 revoked 不再匹配,要嘛退款窗口已過照常回收,兩者皆正確)。
-- OF e:只鎖權益列,不鎖共享讀的 shop_items。
-- WHERE 條件正中既有部分索引 (expires_at) WHERE revoked_at IS NULL AND expires_at IS NOT NULL。
-- external_role_id:auto_role 商品的 Discord 身分組,消費端收回身分組要用。
SELECT e.id, e.user_id, e.item_id, i.external_role_id
FROM platform.entitlements e
JOIN platform.shop_items i ON i.id = e.item_id
WHERE e.revoked_at IS NULL AND e.expires_at IS NOT NULL AND e.expires_at <= now()
ORDER BY e.expires_at
FOR UPDATE OF e SKIP LOCKED
LIMIT $1;

-- name: RevokeExpiredEntitlements :execrows
-- 呼叫前必須已 LockExpiredEntitlements 持鎖;revoked_at IS NULL 再守一層,
-- 影響列數 ≠ 輸入數即狀態矛盾,呼叫端失敗出聲。時間用 DB 時鐘(單一時鐘來源)。
UPDATE platform.entitlements SET revoked_at = now()
WHERE id = ANY(sqlc.arg(ids)::bigint[]) AND revoked_at IS NULL;
