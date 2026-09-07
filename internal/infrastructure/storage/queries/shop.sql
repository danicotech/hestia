-- 商店 query(schemas/08-shop.md)。動錢一律經 ledger(shoppg 用 ApplyInTx 同 tx 綁定),
-- 這裡只有商品 / 權益 / 工單側的讀寫;token_entries 僅有 SELECT(append-only 鐵則)。

-- name: LockUserForShop :one
-- 同一使用者的購買以 users 列鎖串行化:per_user_limit 計數與 API 冪等鍵的重放判定,
-- 都必須在「前一筆購買 commit 之後」才有意義(READ COMMITTED 下先讀後寫會踩到舊快照)。
-- 鎖序:users → user_balances(與 dailypg 同向,不會與帳本互鎖)。
SELECT id FROM platform.users
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE;

-- name: GetShopItemForPurchase :one
-- is_listed 用 DB 時鐘計算(單一時鐘來源):listed_at 非空且已到、delisted_at 空或未到。
SELECT i.*,
  (i.listed_at IS NOT NULL AND i.listed_at <= now()
   AND (i.delisted_at IS NULL OR i.delisted_at > now()))::bool AS is_listed
FROM platform.shop_items i
WHERE i.public_id = $1;

-- name: CountUserItemAcquisitions :one
-- per_user_limit 的計數口徑(schemas/08):未撤銷的 entitlements + 非 cancelled/rejected
-- 的 redemptions。退款(撤銷)與被拒/取消的工單釋放額度。
SELECT (
  (SELECT count(*) FROM platform.entitlements e
     WHERE e.user_id = sqlc.arg(user_id) AND e.item_id = sqlc.arg(item_id)
       AND e.revoked_at IS NULL)
+ (SELECT count(*) FROM platform.redemptions r
     WHERE r.user_id = sqlc.arg(user_id) AND r.item_id = sqlc.arg(item_id)
       AND r.status NOT IN ('cancelled', 'rejected'))
)::bigint AS acquired;

-- name: InsertEntitlement :one
-- expires_at / refundable_until 在購買當下用 DB 時鐘算好「存欄位」
-- (schemas/08:窗口逐商品調,存值不現算)。
-- duration_days NULL → expires_at NULL = 永久;refund_window_seconds <= 0 → NULL = 不可退。
INSERT INTO platform.entitlements (public_id, user_id, item_id, expires_at, refundable_until)
VALUES (
  sqlc.arg(public_id), $1, $2,
  CASE WHEN sqlc.narg(duration_days)::int IS NULL THEN NULL
       ELSE now() + make_interval(days => sqlc.narg(duration_days)::int) END,
  CASE WHEN sqlc.arg(refund_window_seconds)::int <= 0 THEN NULL
       ELSE now() + make_interval(secs => sqlc.arg(refund_window_seconds)::int) END
)
RETURNING *;

-- name: InsertRedemption :one
INSERT INTO platform.redemptions (public_id, user_id, item_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: LockEntitlementForRefund :one
-- FOR UPDATE OF e:只鎖權益列(同一權益的退款串行化),不鎖共享讀的商品列。
-- db_now 一併回傳:退款窗口比對用 DB 時鐘,避免 app/DB 時鐘偏移誤判(同 outbox 的教訓)。
SELECT e.id, e.user_id, e.item_id, e.granted_at, e.expires_at, e.refundable_until, e.revoked_at,
       i.public_id AS item_public_id,
       now()::timestamptz AS db_now
FROM platform.entitlements e
JOIN platform.shop_items i ON i.id = e.item_id
WHERE e.id = $1
FOR UPDATE OF e;

-- name: GetPurchaseEntryForEntitlement :one
-- Purchase 把扣款分錄的 ref 指向 entitlement(ref_type='entitlement', ref_id=權益 id),
-- 退款由此精確找回原分錄——不靠「該 user 該 item 最近一筆」猜測,也不改 schema。
-- 免費商品(price=0)購買時沒有分錄 → no rows,呼叫端視為無錢可退、僅撤銷權益。
SELECT id, currency, amount FROM platform.token_entries
WHERE reason = 'shop_purchase' AND ref_type = 'entitlement' AND ref_id = $1
ORDER BY created_at DESC
LIMIT 1;

-- name: RevokeEntitlement :one
-- 呼叫前必須已 LockEntitlementForRefund 且確認 revoked_at IS NULL。
UPDATE platform.entitlements SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING revoked_at;

-- name: LockRedemptionForHandle :one
-- FOR UPDATE OF r:同一工單的 approve / reject / cancel 串行化,
-- 後到者看到非 pending 即拒絕(狀態機單向)。
SELECT r.id, r.public_id, r.user_id, r.item_id, r.status, r.note, r.created_at,
       i.public_id AS item_public_id
FROM platform.redemptions r
JOIN platform.shop_items i ON i.id = r.item_id
WHERE r.id = $1
FOR UPDATE OF r;

-- name: GetHoldEntryForRedemption :one
-- manual 購買的押款分錄 ref 指向 redemption;免費 manual 商品沒有分錄 → no rows。
SELECT id, currency, amount FROM platform.token_entries
WHERE reason = 'redemption_hold' AND ref_type = 'redemption' AND ref_id = $1
ORDER BY created_at DESC
LIMIT 1;

-- name: UpdateRedemptionStatus :one
-- 呼叫前必須已 LockRedemptionForHandle 且確認 status='pending'。
UPDATE platform.redemptions
SET status = sqlc.arg(status),
    note = COALESCE(sqlc.narg(note)::text, note),
    handled_by = sqlc.arg(handled_by),
    handled_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;
