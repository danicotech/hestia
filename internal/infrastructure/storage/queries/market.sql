-- 物品與市集 query(schemas/09-items-market.md)。動錢一律經 ledger(marketpg 用 ApplyInTx
-- 同 tx 綁定),這裡只有物品 / 掛單 / 成交紀錄側的讀寫;token_entries 一律不碰(append-only 鐵則)。
--
-- 全域鎖序(與 shoppg / dailypg 同向,結構上不可能死鎖):
--   users → market_listings → item_instances → user_balances(user_id 升冪)

-- name: LockUserForTrade :one
-- 交易雙方的 users 列鎖:同一使用者的成交全序列化。
-- 單日交易額上限與 API 冪等鍵的重放判定,都必須在「前一筆成交 commit 之後」
-- 才有意義(READ COMMITTED 下先讀後寫會踩到舊快照)。呼叫端依 user_id 升冪呼叫。
SELECT id FROM platform.users
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE;

-- ══ 物品定義與發放 ══

-- name: GetItemDefinitionByID :one
SELECT id, public_id, name, category, tradable, bind_on_acquire, max_supply, minted_count
FROM platform.item_definitions
WHERE id = $1;

-- name: GetItemDefinitionByPublicID :one
SELECT id, public_id, name, category, tradable, bind_on_acquire, max_supply, minted_count
FROM platform.item_definitions
WHERE public_id = $1;

-- name: MintDefinitionSupply :one
-- 超發保護:檢查與遞增是**同一個敘述**,不是先查後寫。
-- 併發時後到者在 UPDATE 取得列鎖後會依最新版本重跑 WHERE(READ COMMITTED 的
-- EvalPlanQual),額度用完就是 0 rows —— 呼叫端據此回 ErrSupplyExhausted。
-- max_supply IS NULL = 不限量,仍然遞增 minted_count(發行量要可查)。
UPDATE platform.item_definitions
SET minted_count = minted_count + 1
WHERE id = $1
  AND (max_supply IS NULL OR minted_count < max_supply)
RETURNING id, public_id, name, category, tradable, bind_on_acquire, max_supply, minted_count;

-- name: InsertItemInstance :one
-- quantity 固定 1:M1 不做 stackable 疊加(schemas/09 有欄位但沒有合併/拆分語意)。
INSERT INTO platform.item_instances (public_id, definition_id, owner_id, bound, quantity, acquired_via)
VALUES (sqlc.arg(public_id), sqlc.arg(definition_id), sqlc.arg(owner_id), sqlc.arg(bound), 1,
        sqlc.narg(acquired_via))
RETURNING id, public_id, acquired_at;

-- name: ListUserItemInstances :many
-- 使用者的物品清單(含定義資訊);locked 讓前端知道「正在賣,不能再掛」。
SELECT i.public_id, i.bound, i.quantity, i.acquired_at, i.acquired_via,
       (i.locked_by_trade_id IS NOT NULL)::bool AS locked,
       d.public_id AS definition_public_id, d.name, d.category, d.rarity, d.icon_url, d.tradable
FROM platform.item_instances i
JOIN platform.item_definitions d ON d.id = i.definition_id
WHERE i.owner_id = $1
ORDER BY i.acquired_at DESC, i.id DESC;

-- name: LockItemInstanceByPublicID :one
-- FOR UPDATE OF i:只鎖實例列(同一件物品的掛單串行化),不鎖共享讀的定義列。
SELECT i.id, i.public_id, i.definition_id, i.owner_id, i.bound, i.locked_by_trade_id,
       d.public_id AS definition_public_id, d.category, d.tradable, d.bind_on_acquire
FROM platform.item_instances i
JOIN platform.item_definitions d ON d.id = i.definition_id
WHERE i.public_id = $1
FOR UPDATE OF i;

-- name: LockItemInstanceByID :one
SELECT i.id, i.public_id, i.definition_id, i.owner_id, i.bound, i.locked_by_trade_id,
       d.public_id AS definition_public_id, d.category, d.tradable, d.bind_on_acquire
FROM platform.item_instances i
JOIN platform.item_definitions d ON d.id = i.definition_id
WHERE i.id = $1
FOR UPDATE OF i;

-- name: LockItemInstanceForTrade :execrows
-- 掛單成立時把物品鎖住;WHERE 的 IS NULL 是最後防線(呼叫端已持有列鎖並檢查過)。
UPDATE platform.item_instances
SET locked_by_trade_id = sqlc.arg(trade_id)::bigint
WHERE id = sqlc.arg(id) AND locked_by_trade_id IS NULL;

-- name: UnlockItemInstance :execrows
-- 取消掛單時解鎖;只解本掛單放的鎖,不會誤解別人的。
UPDATE platform.item_instances
SET locked_by_trade_id = NULL
WHERE id = sqlc.arg(id) AND locked_by_trade_id = sqlc.arg(trade_id)::bigint;

-- name: TransferItemInstance :execrows
-- 成交換手:owner 換人 + 清鎖 + 更新取得脈絡,單一敘述。
-- WHERE 同時比對原持有人與鎖:鎖不指向本掛單就換不動(ledger-invariants 第五條)。
-- bound 依定義的 bind_on_acquire 重新決定:交易後綁定的物品,新主人手上就是綁定的。
UPDATE platform.item_instances
SET owner_id = sqlc.arg(new_owner_id),
    locked_by_trade_id = NULL,
    bound = sqlc.arg(bound),
    acquired_at = now(),
    acquired_via = 'market'
WHERE id = sqlc.arg(id)
  AND owner_id = sqlc.arg(seller_id)
  AND locked_by_trade_id = sqlc.arg(trade_id)::bigint;

-- ══ 掛單 ══

-- name: InsertMarketListing :one
INSERT INTO platform.market_listings (public_id, seller_id, item_instance_id, currency, price, expires_at)
VALUES (sqlc.arg(public_id), sqlc.arg(seller_id), sqlc.arg(item_instance_id),
        sqlc.arg(currency), sqlc.arg(price), sqlc.narg(expires_at)::timestamptz)
RETURNING id, public_id, seller_id, currency, price, status, listed_at, expires_at;

-- name: GetListingByPublicID :one
-- 不加鎖:先讀出賣家是誰,才知道要鎖哪兩個 users 列(鎖序需要先知道鎖的集合)。
-- 讀到的內容一律在取得列鎖後重新驗證,不當作判斷依據。
SELECT id, public_id, seller_id, item_instance_id, currency, price, status, listed_at, expires_at
FROM platform.market_listings
WHERE public_id = $1;

-- name: LockListingByID :one
-- 同一張掛單的成交/取消串行化,後到者看到非 open 即拒絕。
-- db_now 一併回傳:過期比對用 DB 時鐘,避免 app/DB 時鐘偏移誤判。
SELECT id, public_id, seller_id, item_instance_id, currency, price, status, listed_at, expires_at,
       now()::timestamptz AS db_now
FROM platform.market_listings
WHERE id = $1
FOR UPDATE;

-- name: LockListingByPublicID :one
SELECT id, public_id, seller_id, item_instance_id, currency, price, status, listed_at, expires_at,
       now()::timestamptz AS db_now
FROM platform.market_listings
WHERE public_id = $1
FOR UPDATE;

-- name: UpdateListingStatus :execrows
-- 呼叫前必須已鎖住掛單列;WHERE status='open' 是狀態機單向的最後防線。
UPDATE platform.market_listings
SET status = sqlc.arg(status)
WHERE id = sqlc.arg(id) AND status = 'open';

-- ══ 成交紀錄 ══

-- name: InsertMarketOrder :one
-- 買賣雙方 + 金額 + 手續費全額記帳:任兩人的資金淨流向可查(防洗點稽核的資料來源)。
INSERT INTO platform.market_orders (listing_id, buyer_id, seller_id, price, fee)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, settled_at;

-- ══ 防洗點:交易門檻 / 單日上限 / no_trade ══

-- name: GetTradeEligibility :one
-- 一次算完一個人的交易資格(schemas/09「防洗點」四項的資料來源),全部用 DB 時鐘:
--   level            該使用者在各社群的最高等級(M1 只有一個社群;user_xp 是投影表)
--   member_days      入群天數 = now() − MIN(space_members.joined_at),離群不歸零(schemas/01 增補 B)
--   account_age_days 帳號年齡 = now() − users.created_at
--   no_trade         有無生效中的 no_trade 限制(未解除且未到期)
--   traded_today     當日已成交額(買 + 賣合計,同幣別):日界線用該使用者自己的時區,
--                    與每日簽到的「一天」同義,不另立第二套日曆
SELECT
  u.id,
  COALESCE((SELECT MAX(x.level) FROM platform.user_xp x WHERE x.user_id = u.id), 0)::int AS level,
  COALESCE((SELECT (EXTRACT(EPOCH FROM (now() - MIN(m.joined_at))) / 86400)::int
            FROM platform.space_members m WHERE m.user_id = u.id), 0)::int AS member_days,
  (EXTRACT(EPOCH FROM (now() - u.created_at)) / 86400)::int AS account_age_days,
  (EXISTS (SELECT 1 FROM platform.user_restrictions r
            WHERE r.user_id = u.id AND r.kind = 'no_trade'
              AND r.lifted_at IS NULL
              AND (r.expires_at IS NULL OR r.expires_at > now())))::bool AS no_trade,
  COALESCE((SELECT SUM(o.price)
            FROM platform.market_orders o
            JOIN platform.market_listings l ON l.id = o.listing_id
            WHERE (o.buyer_id = u.id OR o.seller_id = u.id)
              AND l.currency = sqlc.arg(currency)
              AND o.settled_at >= date_trunc('day', now() AT TIME ZONE u.timezone) AT TIME ZONE u.timezone
           ), 0)::bigint AS traded_today
FROM platform.users u
WHERE u.id = sqlc.arg(user_id) AND u.deleted_at IS NULL;
