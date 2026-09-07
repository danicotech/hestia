-- 簽到:防連點靠 daily_claims 的 PK(user_id, claim_date),不用冪等鍵(ledger-invariants 第三條)

-- name: InsertDailyClaim :one
INSERT INTO platform.daily_claims (user_id, claim_date, timezone, streak, amount, source)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetDailyState :one
SELECT * FROM platform.user_daily_state WHERE user_id = $1;

-- name: UpsertDailyState :exec
INSERT INTO platform.user_daily_state (user_id, current_streak, best_streak, last_claim_date, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id) DO UPDATE
SET current_streak = EXCLUDED.current_streak,
    best_streak    = GREATEST(platform.user_daily_state.best_streak, EXCLUDED.best_streak),
    last_claim_date = EXCLUDED.last_claim_date,
    updated_at     = now();

-- name: GetLastDailyClaim :one
-- 最近一次簽到(依絕對時間),供改時區冷卻檢查(timezone_change_min_gap_hours)比對 claimed_at
SELECT * FROM platform.daily_claims
WHERE user_id = $1
ORDER BY claimed_at DESC
LIMIT 1;

-- name: LockUserForDaily :one
-- 串行化同一使用者的併發 Claim:跨當地午夜(或併發改時區)時兩個 tx 可能算出
-- 不同 claim_date,單靠 PK 擋不住(streak 誤算、20h 閘門可繞過)。
-- 先鎖 users 列,之後的冷卻/streak 讀取全在鎖後;PK 仍是防連點的最終防線。
SELECT timezone, timezone_changed_at FROM platform.users
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE;
