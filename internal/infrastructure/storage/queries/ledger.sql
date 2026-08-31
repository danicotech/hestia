-- 帳本 query。使用規則見 .claude/skills/ledger-invariants:
-- append-only、同 transaction 更新餘額、鎖依 user_id 升冪、動錢一律冪等。
-- 這裡刻意「沒有」UPDATE/DELETE token_entries 的 query —— 不要新增。

-- name: LockBalanceForUpdate :one
SELECT balance FROM platform.user_balances
WHERE user_id = $1 AND currency = $2
FOR UPDATE;

-- name: EnsureBalanceRow :exec
INSERT INTO platform.user_balances (user_id, currency, balance)
VALUES ($1, $2, 0)
ON CONFLICT (user_id, currency) DO NOTHING;

-- name: InsertTokenEntry :one
INSERT INTO platform.token_entries (user_id, currency, amount, reason, ref_type, ref_id, actor_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, created_at;

-- name: ApplyBalanceDelta :one
-- 呼叫前必須已 LockBalanceForUpdate;CHECK(balance >= 0) 是最後防線,不是主要檢查
UPDATE platform.user_balances
SET balance = balance + $3, updated_at = now()
WHERE user_id = $1 AND currency = $2
RETURNING balance;

-- name: GetBalance :one
SELECT balance FROM platform.user_balances
WHERE user_id = $1 AND currency = $2;

-- name: SumEntriesForUser :one
-- 對帳 job 用:驗證 SUM(entries) = balance
SELECT COALESCE(SUM(amount), 0)::bigint AS total
FROM platform.token_entries
WHERE user_id = $1 AND currency = $2;

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
SELECT
  COALESCE(b.user_id, s.user_id)::bigint   AS user_id,
  COALESCE(b.currency, s.currency)::text   AS currency,
  COALESCE(b.balance, 0)::bigint           AS balance,
  COALESCE(s.total, 0)::bigint             AS entry_total
FROM platform.user_balances b
FULL JOIN (
  SELECT user_id, currency, SUM(amount) AS total
  FROM platform.token_entries
  GROUP BY user_id, currency
) s ON s.user_id = b.user_id AND s.currency = b.currency
WHERE COALESCE(b.balance, 0) <> COALESCE(s.total, 0);

-- name: InsertOutboxEvent :one
INSERT INTO platform.outbox_events (topic, payload)
VALUES ($1, $2)
RETURNING id;

-- name: ClaimPendingOutbox :many
-- 多實例安全消費:SKIP LOCKED
SELECT * FROM platform.outbox_events
WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= now())
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT $1;

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
