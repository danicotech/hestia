// Package dailypg 是 daily.Service 的 Postgres 實作。
//
// 一次 Claim 的全部效果——daily_claims、token_entries、user_balances、
// outbox_events、user_daily_state——都在同一個 transaction,要嘛都在要嘛都不在。
package dailypg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	// Windows 上系統不一定有 tz 資料庫,內嵌確保 time.LoadLocation 一定可用。
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// currency 是簽到發放的幣別。M1 只有 coin(schemas/04);多幣別是資料層面的擴充,
// 屆時發什麼幣改這裡或改成設定,不影響資料模型。
const currency = "coin"

// txLedger 是本服務對帳本的最小依賴:與簽到同 tx 動錢。
// 實際注入 *ledgerpg.Service(ApplyInTx 刻意不在 ledger.Ledger interface 上)。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// Service 是 daily.Service 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	led  txLedger
}

var _ daily.Service = (*Service)(nil)

// New 建立簽到服務。led 傳 ledgerpg.New(pool) 的結果。
func New(pool *pgxpool.Pool, led txLedger) *Service {
	return &Service{pool: pool, q: db.New(pool), led: led}
}

// Claim 在單一 transaction 內完成簽到。流程:
//
//	讀使用者時區 → 算當地今天 → 改時區冷卻檢查 → 算 streak 與金額(設定全從 DB 讀)
//	→ INSERT daily_claims(PK 撞鍵 = 已簽到,防連點的最終保證)
//	→ ApplyInTx 發幣 + daily.claimed outbox 事件 → 更新 user_daily_state → commit
func (s *Service) Claim(ctx context.Context, userID int64) (*daily.Result, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)

	// FOR UPDATE 鎖 users 列,串行化同一使用者的併發 Claim:
	// 跨當地午夜(或併發改時區)時兩個 tx 可能算出不同 claim_date,單靠 PK 擋不住
	// (streak 誤算、20h 閘門可繞過、last_claim_date 倒退)。之後的冷卻/streak 讀取全在鎖後。
	user, err := qtx.LockUserForDaily(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("鎖使用者 %d: %w", userID, err)
	}
	// 邊界:LoadLocation 對 "" 與 "Local" 不回 error(靜默用 UTC / 伺服器時區)。
	// users.timezone 有 NOT NULL DEFAULT 'Asia/Taipei',正常資料不會是空字串;
	// 時區字串的白名單驗證是時區更新入口(UpdateUserTimezone 的呼叫端)的責任,不在這裡。
	loc, err := time.LoadLocation(user.Timezone)
	if err != nil {
		return nil, fmt.Errorf("使用者 %d 的時區 %q 無效: %w", userID, user.Timezone, err)
	}
	now := time.Now()
	claimDate := dateOf(now.In(loc)) // 當地今天

	// ── 防改時區濫用(schemas/07):只在「改過時區、且改在最近一次簽到之後」才檢查
	// 最小間隔。一般情況不檢查——同時區連續兩個當地日期可能只差幾分鐘,
	// 全域 20h gap 會誤殺正常使用。
	lastClaim, err := qtx.GetLastDailyClaim(ctx, userID)
	hasLast := true
	if errors.Is(err, pgx.ErrNoRows) {
		hasLast = false
	} else if err != nil {
		return nil, fmt.Errorf("讀最近簽到: %w", err)
	}
	if hasLast && user.TimezoneChangedAt != nil && user.TimezoneChangedAt.After(lastClaim.ClaimedAt) {
		gapHours, err := s.configInt(ctx, qtx, "timezone_change_min_gap_hours")
		if err != nil {
			return nil, err
		}
		if now.Sub(lastClaim.ClaimedAt) < time.Duration(gapHours)*time.Hour {
			return nil, fmt.Errorf("距上次簽到未滿 %d 小時: %w",
				gapHours, daily.ErrTimezoneChangeCooldown)
		}
	}

	// ── streak:昨天(當地)簽過 → +1,否則歸 1;best_streak 由 upsert 的 GREATEST 保護
	streak := int32(1)
	state, err := qtx.GetDailyState(ctx, userID)
	switch {
	case err == nil:
		if state.LastClaimDate.Valid && sameDate(state.LastClaimDate.Time, claimDate.AddDate(0, 0, -1)) {
			streak = state.CurrentStreak + 1
		}
	case errors.Is(err, pgx.ErrNoRows): // 首簽,streak = 1
	default:
		return nil, fmt.Errorf("讀簽到狀態: %w", err)
	}

	// ── 金額 = min(base + step×(streak−1), cap),全從 economy_configs 讀,不寫死
	base, err := s.configInt(ctx, qtx, "daily_base")
	if err != nil {
		return nil, err
	}
	step, err := s.configInt(ctx, qtx, "daily_streak_step")
	if err != nil {
		return nil, err
	}
	capAmount, err := s.configInt(ctx, qtx, "daily_streak_cap")
	if err != nil {
		return nil, err
	}
	// 值域驗證:壞設定(負值、cap < base)要拒絕出聲,不能默默反向扣錢。
	if base < 0 || step < 0 || capAmount < base {
		return nil, fmt.Errorf("daily_base=%d daily_streak_step=%d daily_streak_cap=%d: %w",
			base, step, capAmount, daily.ErrInvalidConfig)
	}
	amount := base + step*int64(streak-1)
	if amount > capAmount {
		amount = capAmount
	}
	if amount <= 0 {
		return nil, fmt.Errorf("算出金額 %d ≤ 0: %w", amount, daily.ErrInvalidConfig)
	}

	// ── INSERT daily_claims:PK (user_id, claim_date) 撞鍵即已簽到。
	// 這是防連點的最終保證——不先 SELECT 檢查,讓資料庫擋(schemas/07)。
	// 併發時後到的 INSERT 會等先到的 tx commit 後收到 unique violation。
	if _, err := qtx.InsertDailyClaim(ctx, db.InsertDailyClaimParams{
		UserID:    userID,
		ClaimDate: pgtype.Date{Time: claimDate, Valid: true},
		Timezone:  user.Timezone, // 當下時區快照,改時區後仍可稽核重算
		Streak:    streak,
		Amount:    amount,
		Source:    "normal",
	}); err != nil {
		if isDailyPKViolation(err) {
			return nil, fmt.Errorf("user=%d date=%s: %w",
				userID, claimDate.Format(time.DateOnly), daily.ErrAlreadyClaimed)
		}
		return nil, fmt.Errorf("寫簽到紀錄: %w", err)
	}

	// ── 發幣 + outbox 事件,經帳本、同 tx(ledger-invariants 第二、六條)。
	// 冪等鍵用決定性 key:防重的權威是 daily_claims 的 PK(上面撞鍵就到不了這裡),
	// 這個 key 只是滿足 Ledger 介面的必填欄位——同人同日必然同 key 同 hash,
	// 天然冪等,不構成第二套檢查(一個概念一個權威)。
	idemKey := fmt.Sprintf("daily:%d:%s", userID, claimDate.Format(time.DateOnly))
	payload, err := json.Marshal(map[string]any{
		"user_id":    userID,
		"amount":     amount,
		"streak":     streak,
		"claim_date": claimDate.Format(time.DateOnly),
	})
	if err != nil {
		return nil, fmt.Errorf("序列化事件: %w", err)
	}
	ledRes, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: idemKey,
		RequestHash:    idemKey,
		Ops: []ledger.Op{{
			UserID:   userID,
			Currency: currency,
			Amount:   amount,
			Reason:   ledger.ReasonDailyClaim,
		}},
		Events: []ledger.Event{{Topic: "daily.claimed", Payload: payload}},
	})
	if err != nil {
		return nil, fmt.Errorf("簽到發幣: %w", err)
	}
	// 到得了這裡代表 daily_claims 的 PK 判定首簽;帳本若回 replay(同 key 已有結果),
	// 兩個防重權威矛盾——回成功但錢沒動是最壞結果,必須失敗出聲讓整個 tx rollback。
	if ledRes.Replayed {
		return nil, fmt.Errorf("key=%s 帳本已有結果但無簽到紀錄: %w", idemKey, daily.ErrLedgerStateConflict)
	}

	// ── 更新 streak 狀態(同 tx;best_streak 用 GREATEST 保護,見 daily.sql)
	if err := qtx.UpsertDailyState(ctx, db.UpsertDailyStateParams{
		UserID:        userID,
		CurrentStreak: streak,
		BestStreak:    streak,
		LastClaimDate: pgtype.Date{Time: claimDate, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("更新簽到狀態: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &daily.Result{Amount: amount, Streak: streak}, nil
}

// configInt 讀 economy_configs 目前生效值(effective_at <= now() 最新一筆)。
// 值不存在或不是整數都是部署錯誤,直接回傳錯誤,不用預設值掩蓋。
func (s *Service) configInt(ctx context.Context, qtx *db.Queries, key string) (int64, error) {
	raw, err := qtx.GetCurrentConfig(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("讀設定 %s: %w", key, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("設定 %s 不是整數 (%s): %w", key, raw, err)
	}
	return v, nil
}

// dateOf 取 t 的年月日(在 t 自己的時區),回傳 UTC 午夜的 time.Time。
// pgtype.Date 只取日期部分,統一用 UTC 午夜避免時區再度干擾比較。
func dateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// sameDate 比較兩個 time 的年月日是否相同(忽略時刻與時區偏移)。
func sameDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// isDailyPKViolation 只認 daily_claims 的 PK:其他 unique 約束(如冪等鍵)不得誤判為已簽到。
func isDailyPKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "daily_claims_pkey"
}
