package playpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/chance"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/play"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 抽獎活動(schemas/25)。與開箱的關鍵差別:**獎品是管理員先掏出來的**,
// 所以總量不變,只換口袋 —— 這不是水龍頭,不需要產出速率控制。
//
// 但它需要另一種保證:獎品在**建立時**就凍結,不是開獎時才扣。
// 否則管理員在開獎前把點數花光,抽獎就變成空頭支票,而那時參加者
// 已經投入了報名費。

// ListGiveaways 列出進行中的抽獎。
func (s *Service) ListGiveaways(ctx context.Context, communityID int64) ([]play.GiveawayView, error) {
	rows, err := s.q.ListOpenGiveaways(ctx, communityID)
	if err != nil {
		return nil, fmt.Errorf("列出抽獎: %w", err)
	}
	out := make([]play.GiveawayView, 0, len(rows))
	for _, r := range rows {
		out = append(out, play.GiveawayView{
			PublicID: r.PublicID, Title: r.Title, EntryCost: r.EntryCost,
			WinnerCount: r.WinnerCount, Entries: r.Entries, ClosesAt: r.ClosesAt,
		})
	}
	return out, nil
}

// EnterGiveaway 報名。收報名費(若有)與寫報名紀錄同一個 transaction。
func (s *Service) EnterGiveaway(ctx context.Context, userID int64, publicID string) error {
	g, err := s.q.GetGiveawayByPublicID(ctx, publicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("抽獎 %q: %w", publicID, play.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("查抽獎: %w", err)
	}
	if g.Status != "open" {
		return fmt.Errorf("抽獎 %q 狀態為 %s: %w", g.Title, g.Status, play.ErrClosed)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// 先寫報名紀錄再扣費:撞鍵(已經報名過)時整個 tx 回滾,不會重複收費。
	// 反過來的話,重複報名的人會被多扣一次錢才發現自己報過了。
	n, err := qtx.EnterGiveaway(ctx, db.EnterGiveawayParams{GiveawayID: g.ID, UserID: userID})
	if err != nil {
		return fmt.Errorf("報名: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("抽獎 %q: %w", g.Title, play.ErrAlreadyEntered)
	}

	if g.EntryCost > 0 {
		key := fmt.Sprintf("giveaway_entry:%d:%d", g.ID, userID)
		if _, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key,
			RequestHash:    key,
			Ops: []ledger.Op{{
				UserID: userID, Currency: defaultCurrency,
				Amount: -g.EntryCost, Reason: ledger.ReasonGiveawayEntry,
			}},
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DrawGiveaway 開獎:抽出贏家、派發獎品、結案。
//
// 整段在一個 transaction 且先 FOR UPDATE 鎖住抽獎列:兩個人同時按開獎
// 會抽出兩組贏家,而獎品只有一份。
func (s *Service) DrawGiveaway(ctx context.Context, publicID string) (*play.DrawResult, error) {
	g, err := s.q.GetGiveawayByPublicID(ctx, publicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("抽獎 %q: %w", publicID, play.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查抽獎: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	locked, err := qtx.LockGiveawayForDraw(ctx, g.ID)
	if err != nil {
		return nil, fmt.Errorf("鎖抽獎: %w", err)
	}
	// 鎖到之後**再檢查一次狀態**:上面那次讀是在鎖之前,
	// 兩個人同時開獎時其中一個會在這裡看到已經 drawn。
	if locked.Status != "open" {
		return nil, fmt.Errorf("抽獎 %q 狀態為 %s: %w", g.Title, locked.Status, play.ErrClosed)
	}

	winners, err := qtx.PickGiveawayWinners(ctx, db.PickGiveawayWinnersParams{
		GiveawayID: g.ID, Limit: locked.WinnerCount,
	})
	if err != nil {
		return nil, fmt.Errorf("抽贏家: %w", err)
	}
	if len(winners) == 0 {
		// 沒有人報名。結案但不發獎 —— 留著 open 的話它會永遠掛在清單上。
		if err := qtx.CloseGiveaway(ctx, g.ID); err != nil {
			return nil, fmt.Errorf("結案: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		return &play.DrawResult{Title: g.Title}, nil
	}

	ids := make([]int64, 0, len(winners))
	for _, w := range winners {
		ids = append(ids, w.UserID)
	}
	if _, err := qtx.MarkGiveawayWinners(ctx, db.MarkGiveawayWinnersParams{
		GiveawayID: g.ID, Column2: ids,
	}); err != nil {
		return nil, fmt.Errorf("標記贏家: %w", err)
	}

	for _, w := range winners {
		if err := s.payPrize(ctx, tx, qtx, locked, w.UserID); err != nil {
			return nil, err
		}
	}

	// 抽獎也留痕:它雖然不是水龍頭(獎品由管理員出),但「誰抽的、
	// 抽出誰」同樣需要能事後查證。
	outcome, err := json.Marshal(map[string]any{"winners": ids})
	if err != nil {
		return nil, fmt.Errorf("序列化結果: %w", err)
	}
	odds, err := json.Marshal(map[string]any{
		"winner_count": locked.WinnerCount, "method": "uniform_random_without_replacement",
	})
	if err != nil {
		return nil, fmt.Errorf("序列化賠率: %w", err)
	}
	_, seed, err := chance.RollBelow(1 << 32) //nolint:mnd // 只為了取一個種子留痕
	if err != nil {
		return nil, err
	}
	if _, err := qtx.InsertChanceDraw(ctx, db.InsertChanceDrawParams{
		CommunityID: locked.CommunityID, UserID: g.CreatedBy,
		Kind: string(chance.KindLootBox), Ref: g.PublicID,
		OddsSnapshot: odds, Seed: seed, Outcome: outcome,
	}); err != nil {
		return nil, fmt.Errorf("寫抽籤紀錄: %w", err)
	}

	if err := qtx.CloseGiveaway(ctx, g.ID); err != nil {
		return nil, fmt.Errorf("結案: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	out := &play.DrawResult{Title: g.Title}
	for _, w := range winners {
		out.Winners = append(out.Winners, play.Winner{UserPublicID: fmt.Sprintf("%d", w.UserID)})
	}
	return out, nil
}

// payPrize 發一位贏家的獎品。
func (s *Service) payPrize(
	ctx context.Context, tx pgx.Tx, qtx *db.Queries,
	g db.LockGiveawayForDrawRow, userID int64,
) error {
	key := fmt.Sprintf("giveaway_prize:%d:%d", g.ID, userID)
	switch g.PrizeKind {
	case "tokens":
		if g.PrizeAmount == nil || *g.PrizeAmount <= 0 {
			return fmt.Errorf("獎品金額不合法: %v", g.PrizeAmount)
		}
		_, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key, RequestHash: key,
			Ops: []ledger.Op{{
				UserID: userID, Currency: g.PrizeRef,
				Amount: *g.PrizeAmount, Reason: ledger.ReasonGiveawayPrize,
			}},
		})
		return err
	case "item":
		_, err := s.grantItem(ctx, qtx, userID, g.PrizeRef, "giveaway")
		return err
	default:
		return fmt.Errorf("獎品類型 %q 尚未接上發放路徑", g.PrizeKind)
	}
}
