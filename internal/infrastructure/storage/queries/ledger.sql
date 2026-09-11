-- 帳本 query。使用規則見 .claude/skills/ledger-invariants:
-- append-only、同 transaction 更新餘額、鎖依 user_id 升冪、動錢一律冪等。
-- 這裡刻意「沒有」UPDATE/DELETE token_entries 的 query —— 不要新增。

-- 餘額與分錄都帶 community_id(schemas/23):NULL = 全域幣。
-- 每一支都用 COALESCE(community_id, 0) 比對,因為 NULL = NULL 為 NULL 而不是 true——
-- 直接寫 community_id = $3 會讓全域幣的每一次查詢都查不到列,
-- 而那個 bug 的表現是「餘額突然變 0」,不是報錯。
--
-- scope 一律由 currencies 現查,不由呼叫端傳:它必須等於 currencies.scope
-- (複合外鍵擋著),讓呼叫端傳就是給了一個唯一的錯法。

-- name: LockBalanceForUpdate :one
SELECT balance FROM platform.user_balances
WHERE user_id = $1 AND currency = $2
  AND COALESCE(community_id, 0) = COALESCE(sqlc.narg(community_id)::bigint, 0)
FOR UPDATE;

-- name: EnsureBalanceRow :exec
INSERT INTO platform.user_balances (user_id, currency, community_id, scope, balance)
SELECT $1, $2, sqlc.narg(community_id)::bigint, c.scope, 0
FROM platform.currencies c WHERE c.code = $2
ON CONFLICT (user_id, currency, COALESCE(community_id, 0)) DO NOTHING;

-- name: InsertTokenEntry :one
INSERT INTO platform.token_entries
  (user_id, currency, community_id, scope, amount, reason, ref_type, ref_id, actor_id)
SELECT $1, $2, sqlc.narg(community_id)::bigint, c.scope, $3, $4, $5, $6, $7
FROM platform.currencies c WHERE c.code = $2
RETURNING id, created_at;

-- name: ApplyBalanceDelta :one
-- 呼叫前必須已 LockBalanceForUpdate;CHECK(balance >= 0) 是最後防線,不是主要檢查
UPDATE platform.user_balances
SET balance = balance + $3, updated_at = now()
WHERE user_id = $1 AND currency = $2
  AND COALESCE(community_id, 0) = COALESCE(sqlc.narg(community_id)::bigint, 0)
RETURNING balance;

-- name: GetBalance :one
SELECT balance FROM platform.user_balances
WHERE user_id = $1 AND currency = $2
  AND COALESCE(community_id, 0) = COALESCE(sqlc.narg(community_id)::bigint, 0);

-- name: SumEntriesForUser :one
-- 對帳 job 用:驗證 SUM(entries) = balance
SELECT COALESCE(SUM(amount), 0)::bigint AS total
FROM platform.token_entries
WHERE user_id = $1 AND currency = $2
  AND COALESCE(community_id, 0) = COALESCE(sqlc.narg(community_id)::bigint, 0);

-- name: ListEntriesByUser :many
SELECT * FROM platform.token_entries
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetIdempotencyKey :one
SELECT * FROM platform.idempotency_keys WHERE key = $1;

-- name: InsertIdempotencyKey :exec
-- 與動錢同一個 transaction 寫入(見 ledger-invariants 第三條)。
-- 在 tx 開頭先插(response 先 NULL),讓併發同 key 的第二個 tx 直接撞 PK;
-- tx 若 rollback,key 同步消失,合法重試不會被擋。
INSERT INTO platform.idempotency_keys (key, request_hash, response)
VALUES ($1, $2, $3);

-- name: SetIdempotencyResponse :exec
UPDATE platform.idempotency_keys SET response = $2 WHERE key = $1;

-- name: ReconcileBalances :many
-- 對帳:找出 SUM(entries) 與 balance 不一致的每一組(含只有分錄沒有餘額列、或反之)
--
-- **分組必須含 community**:少了它,SUM 會把同一個人在所有社群的分錄加在一起,
-- 而那個總和幾乎永遠不等於任何單一列的 balance —— 對帳會變成全表誤報,
-- 比不對帳更糟(沒有人會再相信它)。
SELECT
  COALESCE(b.user_id, s.user_id)::bigint        AS user_id,
  COALESCE(b.currency, s.currency)::text        AS currency,
  -- 補 0 而不是讓它是 NULL:community id 從 1 起算,0 明確代表全域幣。
  -- 留 NULL 的話這一欄要變成指標型別,而對帳報告的每個使用點都得多一次解參照,
  -- 換來的只是重述「全域」這件 scope 已經說過的事。
  COALESCE(b.community_id, s.community_id, 0)::bigint AS community_key,
  COALESCE(b.balance, 0)::bigint                AS balance,
  COALESCE(s.total, 0)::bigint                  AS entry_total
FROM platform.user_balances b
FULL JOIN (
  SELECT user_id, currency, community_id, SUM(amount) AS total
  FROM platform.token_entries
  GROUP BY user_id, currency, community_id
) s ON s.user_id = b.user_id
   AND s.currency = b.currency
   AND COALESCE(s.community_id, 0) = COALESCE(b.community_id, 0)
WHERE COALESCE(b.balance, 0) <> COALESCE(s.total, 0);

-- name: InsertOutboxEvent :one
INSERT INTO platform.outbox_events (topic, payload)
VALUES ($1, $2)
RETURNING id;

-- name: ClaimPendingOutbox :many
-- 多實例安全消費:SKIP LOCKED
--
-- excluded_topics 是「由別人負責投遞」的 topic(目前是閘道經 NotificationService
-- 拉取的公告)。少了這段,in-process 消費者會認領它們、找不到 handler、
-- 退避重試到上限後標 failed —— 閘道離線一小時,公告就被燒光了。
-- 傳空陣列 = 全部都歸這個消費者(既有行為)。
SELECT * FROM platform.outbox_events
WHERE status = 'pending'
  AND (next_retry_at IS NULL OR next_retry_at <= now())
  -- COALESCE 不可省:topic <> ALL(NULL) 求值為 NULL 而非 true,
  -- 呼叫端沒傳清單時會變成「一筆都認領不到」,消費者靜默停擺。
  AND topic <> ALL(COALESCE(sqlc.narg(excluded_topics)::text[], ARRAY[]::text[]))
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT sqlc.arg(row_limit);

-- name: MarkOutboxDone :exec
UPDATE platform.outbox_events SET status = 'done' WHERE id = $1;

-- name: MarkOutboxRetry :exec
-- 退避時間由 DB 時鐘計算(單一時鐘來源,QA:app/DB 時鐘偏移會讓事件被提前取走)
UPDATE platform.outbox_events
SET attempts = attempts + 1,
    next_retry_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::int)
WHERE id = sqlc.arg(id);

-- name: MarkOutboxFailed :exec
-- 毒訊息終態:超過重試上限,不能卡住整條佇列
UPDATE platform.outbox_events
SET status = 'failed', attempts = attempts + 1
WHERE id = $1;
