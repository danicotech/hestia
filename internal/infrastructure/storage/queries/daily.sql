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
