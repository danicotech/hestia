// Package xppg 是 xp.Service 的 Postgres 實作(schemas/06,含增補 E)。
package xppg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/notification"
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

	res, wrote, err := s.run(ctx, s.q.WithTx(tx), p)
	if err != nil {
		return nil, err
	}
	if !wrote {
		// 冷卻/cap 攔下:什麼都不留(連 EnsureUserXpRow 建的空列也回滾),
		// defer 的 Rollback 負責 —— 被攔的嘗試不該在 DB 留下任何痕跡。
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// AwardInTx 在呼叫端持有的 tx 內入帳,語意與 Award 完全相同,差別只在
// 「誰決定 commit」。給活動記錄那類「事實 + 彙總 + XP 必須同生共死」的呼叫端用
// (activitylogpg):沒有它就只能「事實 commit 後另開 tx 發 XP」,中間掛掉會留下
// 「事實在、XP 沒發」的窗口。
//
// 三個必須說清楚的性質:
//
//   - **原子性由呼叫端決定**:呼叫端 rollback,本次的 xp_events 與 user_xp 一併消失。
//     照 ledgerpg.ApplyInTx 的既有範式。
//   - **失敗用 savepoint 隔離**:source 不存在、cap 攔下等路徑只回滾 savepoint,
//     不毒化呼叫端的 tx —— 呼叫端仍可自行決定要不要繼續(例如照樣記事實)。
//   - **併發語意不變**:一樣鎖 user_xp 列、一樣用 DB 單一時鐘(LockUserXp 回傳的
//     db_now)判冷卻。注意 savepoint rollback **不釋放已取得的 row lock**:
//     本函式回來之後,呼叫端的 tx 仍持有該 user_xp 列的鎖直到 tx 結束,
//     所以請盡快收尾,別拿著它做別的長工。
//
// 刻意放在具體 *Service 而非 xp.Service interface:interface 要保持可攜
// (未來的 HTTP 版沒有 tx 可傳),同 repo 需要 tx 組合的呼叫端依賴具體型別
// 或自己宣告最小介面(同 ledger.ApplyInTx 的理由)。
func (s *Service) AwardInTx(ctx context.Context, tx pgx.Tx, p xp.AwardParams) (*xp.AwardResult, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	sp, err := tx.Begin(ctx) // pgx:巢狀 Begin = SAVEPOINT
	if err != nil {
		return nil, fmt.Errorf("開 savepoint: %w", err)
	}
	res, wrote, err := s.run(ctx, s.q.WithTx(sp), p)
	if err != nil || !wrote {
		// 沒入帳(冷卻/cap)也要回滾:與 Award 一致,不留 EnsureUserXpRow 的空列。
		_ = sp.Rollback(ctx)
		if err != nil {
			return nil, err
		}
		return res, nil
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, fmt.Errorf("release savepoint: %w", err)
	}
	return res, nil
}

// run 是入帳核心,在給定的 query 執行環境(tx 或 savepoint)內完成
// source 檢查 → ruleset → 鎖投影列 → 冷卻 → daily_cap → 寫事實 + 更新投影。
//
// wrote 回報「這次有沒有寫東西」:冷卻與 cap 攔下時是正常結果(不是錯誤),
// 但呼叫端必須據此回滾,否則會留下 EnsureUserXpRow 建出來的空投影列。
func (s *Service) run(
	ctx context.Context, qtx *db.Queries, p xp.AwardParams,
) (res *xp.AwardResult, wrote bool, err error) {
	// ── 1. source 檢查:FK 也擋得住,但先查 registry 才能給可讀錯誤 ──
	et, err := qtx.GetXpEventType(ctx, p.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("source %q: %w", p.Source, xp.ErrUnknownSource)
	}
	if err != nil {
		return nil, false, fmt.Errorf("查 xp_event_types: %w", err)
	}
	if !et.Enabled {
		return nil, false, fmt.Errorf("source %q: %w", p.Source, xp.ErrSourceDisabled)
	}

	// ── 2. 讀該社群的 ruleset config;沒有 ruleset(M1 可能)→ 安全預設 ──
	raw, err := qtx.GetCommunityXpConfig(ctx, p.CommunityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("community %d 不存在: %w", p.CommunityID, xp.ErrInvalidParams)
	}
	if err != nil {
		return nil, false, fmt.Errorf("查 xp_rulesets.config: %w", err)
	}
	cfg, err := xp.ParseConfig(raw)
	if err != nil {
		return nil, false, err
	}
	rule := cfg.Sources[p.Source] // 缺 key = 零值:無冷卻、無 cap

	// ── 3. 鎖投影列:同 user 同 community 的併發入帳在此串行化,
	//        冷卻與 cap 的讀寫因此無競態(schemas/06:冷卻直接用 DB,不上 Redis)──
	if err := qtx.EnsureUserXpRow(ctx, db.EnsureUserXpRowParams{
		UserID: p.UserID, CommunityID: p.CommunityID,
	}); err != nil {
		return nil, false, fmt.Errorf("確保 user_xp 列: %w", err)
	}
	row, err := qtx.LockUserXp(ctx, db.LockUserXpParams{
		UserID: p.UserID, CommunityID: p.CommunityID,
	})
	if err != nil {
		return nil, false, fmt.Errorf("鎖 user_xp: %w", err)
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
			return nil, false, fmt.Errorf("查冷卻計時: %w", err)
		case row.DbNow.Sub(lastAt) < time.Duration(rule.CooldownSeconds)*time.Second:
			return &xp.AwardResult{OnCooldown: true, XP: row.Xp}, false, nil
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
			return nil, false, fmt.Errorf("算當日累計: %w", err)
		}
		remaining := rule.DailyCap - today
		if remaining <= 0 {
			return &xp.AwardResult{Capped: true, XP: row.Xp}, false, nil
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
		return nil, false, fmt.Errorf("寫 xp_events: %w", err)
	}
	newXP, err := qtx.AddUserXp(ctx, db.AddUserXpParams{
		UserID: p.UserID, CommunityID: p.CommunityID, Xp: award,
	})
	if err != nil {
		return nil, false, fmt.Errorf("更新 user_xp: %w", err)
	}

	// ── 7. 升級偵測(schemas/24)──
	//
	// 等級是 XP 的純函數,所以「升級了沒」就是入帳前後各算一次。
	// 用區間而不是單一新等級:語音一次入帳可能跨兩級以上,
	// 只看新等級會漏掉中間那些里程碑。
	from := cfg.ProgressFor(row.Xp).Level
	to := cfg.ProgressFor(newXP).Level
	if to > from {
		if err := s.emitLevelUp(ctx, qtx, p, from, to); err != nil {
			return nil, false, err
		}
	}

	return &xp.AwardResult{
		Awarded: award, Capped: capped, XP: newXP,
		FromLevel: from, ToLevel: to,
	}, true, nil
}

// emitLevelUp 在同一個 transaction 寫出兩則 outbox 事件。
//
// **兩則而不是一則**:公告由閘道投遞、發獎由 in-process 消費者處理,
// 兩者的失敗模式完全不同 —— Discord 限流時公告會重試,但那不該讓
// 里程碑獎勵跟著卡住;反過來,獎勵設定寫壞了也不該讓升級訊息貼不出去。
//
// 同 tx 是鐵則第六條:升級這件事與「有人會知道它」必須同生共死。
func (s *Service) emitLevelUp(
	ctx context.Context, qtx *db.Queries, p xp.AwardParams, from, to int32,
) error {
	payload, err := json.Marshal(map[string]any{
		"user_id":      p.UserID,
		"community_id": p.CommunityID,
		"subject":      "user",
		"from_level":   from,
		"to_level":     to,
	})
	if err != nil {
		return fmt.Errorf("序列化升級事件: %w", err)
	}
	for _, topic := range []string{notification.TopicLevelUp, notification.TopicLevelReward} {
		if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
			Topic: topic, Payload: payload,
		}); err != nil {
			return fmt.Errorf("寫 %s 事件: %w", topic, err)
		}
	}
	return nil
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
