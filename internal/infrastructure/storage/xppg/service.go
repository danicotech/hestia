// Package xppg 是 xp.Service 的 Postgres 實作(schemas/06,含增補 E)。
package xppg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Service 是 xp.Service 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

var _ xp.Service = (*Service)(nil)

// New 建立 XP 服務。
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

// Award 在單一 transaction 內完成一次 XP 入帳:
//
//	驗參數 → 開 tx → source 檢查(registry)→ 讀 ruleset config
//	→ 鎖 user_xp 列(串行化同 user 併發)→ 冷卻判斷 → daily_cap 裁量
//	→ INSERT xp_events + 更新 user_xp 投影 → commit
//
// 冷卻或 cap 攔下時不寫任何東西(tx rollback),last_xp_at 不動 ——
// 被攔的嘗試不重置冷卻計時。
func (s *Service) Award(ctx context.Context, p xp.AwardParams) (*xp.AwardResult, error) {
	if err := validate(p); err != nil {
		return nil, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// ── 1. source 檢查:FK 也擋得住,但先查 registry 才能給可讀錯誤 ──
	et, err := qtx.GetXpEventType(ctx, p.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("source %q: %w", p.Source, xp.ErrUnknownSource)
	}
	if err != nil {
		return nil, fmt.Errorf("查 xp_event_types: %w", err)
	}
	if !et.Enabled {
		return nil, fmt.Errorf("source %q: %w", p.Source, xp.ErrSourceDisabled)
	}

	// ── 2. 讀該社群的 ruleset config;沒有 ruleset(M1 可能)→ 安全預設 ──
	raw, err := qtx.GetCommunityXpConfig(ctx, p.CommunityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("community %d 不存在: %w", p.CommunityID, xp.ErrInvalidParams)
	}
	if err != nil {
		return nil, fmt.Errorf("查 xp_rulesets.config: %w", err)
	}
	cfg, err := xp.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	rule := cfg.Sources[p.Source] // 缺 key = 零值:無冷卻、無 cap

	// ── 3. 鎖投影列:同 user 同 community 的併發入帳在此串行化,
	//        冷卻與 cap 的讀寫因此無競態(schemas/06:冷卻直接用 DB,不上 Redis)──
	if err := qtx.EnsureUserXpRow(ctx, db.EnsureUserXpRowParams{
		UserID: p.UserID, CommunityID: p.CommunityID,
	}); err != nil {
		return nil, fmt.Errorf("確保 user_xp 列: %w", err)
	}
	row, err := qtx.LockUserXp(ctx, db.LockUserXpParams{
		UserID: p.UserID, CommunityID: p.CommunityID,
	})
	if err != nil {
		return nil, fmt.Errorf("鎖 user_xp: %w", err)
	}

	// ── 4. 冷卻:計時器 = 該 source 自己最近一筆 xp_events(QA 中1)。
	//        user_xp.last_xp_at 是跨 source 的「最後任何入帳」資訊欄位,拿來計時
	//        會被其他 source(voice 高頻、admin 修正)不斷重置而餓死本 source 的冷卻。
	//        比較兩端都用 DB 時鐘(LockUserXp 回傳 db_now),單一時鐘來源;
	//        已持有投影列鎖,同 user 併發已串行化,查詢無競態 ──
	if rule.CooldownSeconds > 0 {
		lastAt, err := qtx.LastXpEventAtBySource(ctx, db.LastXpEventAtBySourceParams{
			UserID: p.UserID, CommunityID: p.CommunityID, Source: p.Source,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// 該 source 從未入帳 → 無冷卻
		case err != nil:
			return nil, fmt.Errorf("查冷卻計時: %w", err)
		case row.DbNow.Sub(lastAt) < time.Duration(rule.CooldownSeconds)*time.Second:
			return &xp.AwardResult{OnCooldown: true, XP: row.Xp}, nil
		}
	}

	// ── 5. daily_cap:裁到 cap 部分入帳,而非整筆拒絕 ——
	//        對玩家較友善(當日最後一則發言仍拿得到剩餘額度,不因「差一點滿」
	//        整筆歸零),且入帳總量恰好收斂在 cap,行為由測試鎖住。
	//        只對正數裁量:負數(admin 手動扣減)不受 cap 邏輯影響。
	award := p.Amount
	capped := false
	if rule.DailyCap > 0 && p.Amount > 0 {
		today, err := qtx.SumXpTodayBySource(ctx, db.SumXpTodayBySourceParams{
			UserID: p.UserID, CommunityID: p.CommunityID, Source: p.Source,
		})
		if err != nil {
			return nil, fmt.Errorf("算當日累計: %w", err)
		}
		remaining := rule.DailyCap - today
		if remaining <= 0 {
			return &xp.AwardResult{Capped: true, XP: row.Xp}, nil
		}
		if award > remaining {
			award = remaining
			capped = true
		}
	}

	// ── 6. 事實 + 投影同 tx:XP 不進帳本、不經 Ledger(schemas/06 刻意設計)──
	if _, err := qtx.InsertXpEvent(ctx, db.InsertXpEventParams{
		UserID:      p.UserID,
		CommunityID: p.CommunityID,
		SpaceID:     p.SpaceID,
		Source:      p.Source,
		Amount:      award,
		RefID:       p.RefID,
	}); err != nil {
		return nil, fmt.Errorf("寫 xp_events: %w", err)
	}
	newXP, err := qtx.AddUserXp(ctx, db.AddUserXpParams{
		UserID: p.UserID, CommunityID: p.CommunityID, Xp: award,
	})
	if err != nil {
		return nil, fmt.Errorf("更新 user_xp: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &xp.AwardResult{Awarded: award, Capped: capped, XP: newXP}, nil
}

// Rebuild 從 xp_events 重算 user_xp(xp ← SUM、last_xp_at ← MAX(created_at))並覆寫。
// 先鎖列再重算:與並行 Award 串行化,不會用舊 SUM 蓋掉剛入帳的事件。
// level 不動(M1 恆 0,曲線上線後由等級服務負責)。
func (s *Service) Rebuild(ctx context.Context, userID, communityID int64) (int64, error) {
	if userID <= 0 || communityID <= 0 {
		return 0, fmt.Errorf("user_id / community_id 不合法: %w", xp.ErrInvalidParams)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.EnsureUserXpRow(ctx, db.EnsureUserXpRowParams{
		UserID: userID, CommunityID: communityID,
	}); err != nil {
		return 0, fmt.Errorf("確保 user_xp 列: %w", err)
	}
	if _, err := qtx.LockUserXp(ctx, db.LockUserXpParams{
		UserID: userID, CommunityID: communityID,
	}); err != nil {
		return 0, fmt.Errorf("鎖 user_xp: %w", err)
	}
	total, err := qtx.RebuildUserXp(ctx, db.RebuildUserXpParams{
		UserID: userID, CommunityID: communityID,
	})
	if err != nil {
		return 0, fmt.Errorf("重算 user_xp: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}

func validate(p xp.AwardParams) error {
	switch {
	case p.UserID <= 0:
		return fmt.Errorf("user_id 不合法: %w", xp.ErrInvalidParams)
	case p.CommunityID <= 0:
		return fmt.Errorf("community_id 不合法: %w", xp.ErrInvalidParams)
	case p.Source == "":
		return fmt.Errorf("source 必填: %w", xp.ErrInvalidParams)
	case p.Amount == 0:
		return fmt.Errorf("amount 不可為 0: %w", xp.ErrInvalidParams)
	case p.Amount < 0 && p.Source != xp.SourceAdmin:
		// 負數只允許 admin 手動修正(schemas/06 增補 E);一般 emitter 沒有扣 XP 的
		// 正當理由 —— XP 只增不減是設計核心,負數流入等於繞過它(QA 中2)
		return fmt.Errorf("source %q 不允許負數 amount: %w", p.Source, xp.ErrInvalidParams)
	}
	return nil
}
