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
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 開箱(schemas/25)。與小遊戲的關鍵差別:**這是水龍頭不是水槽** ——
// 獎品由系統生出來,所以產出速率要看得見、要控制得住。
//
// 控制手段有兩個,都在這裡:
//   - 有限獎池(loot_box_items.remaining):抽完就沒了,是 max_supply 在
//     抽獎場景的實現
//   - 每次留痕(chance_draws):隨時算得出「這個月吐了多少」

// ListBoxes 列出這個社群啟用中的箱子。
func (s *Service) ListBoxes(ctx context.Context, communityID int64) ([]play.LootBoxView, error) {
	rows, err := s.q.ListLootBoxes(ctx, communityID)
	if err != nil {
		return nil, fmt.Errorf("列出箱子: %w", err)
	}
	out := make([]play.LootBoxView, 0, len(rows))
	for _, r := range rows {
		v := play.LootBoxView{
			PublicID: r.PublicID, Name: r.Name,
			Currency: r.CostCurrency, Cost: r.CostAmount,
		}
		if r.DailyLimit != nil {
			v.DailyLimit = *r.DailyLimit
		}
		out = append(out, v)
	}
	return out, nil
}

// OpenBox 開一次箱子。
//
// 順序刻意是「先鎖獎池、再扣款、最後發獎」:
//   - 先鎖獎池,兩個人同時抽最後一件才不會都拿到
//   - 扣款在發獎之前,失敗時沒有東西要收回
func (s *Service) OpenBox(ctx context.Context, p play.OpenParams) (*play.OpenResult, error) {
	if p.UserID <= 0 || p.BoxPublicID == "" {
		return nil, fmt.Errorf("參數不完整: %w", play.ErrInvalidParams)
	}
	if p.IdempotencyKey == "" {
		return nil, fmt.Errorf("缺少冪等鍵: %w", play.ErrInvalidParams)
	}

	box, err := s.q.GetLootBoxByPublicID(ctx, p.BoxPublicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("箱子 %q: %w", p.BoxPublicID, play.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("查箱子: %w", err)
	}
	if !box.Enabled {
		return nil, fmt.Errorf("箱子 %q: %w", box.Name, play.ErrDisabled)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := s.checkDailyLimit(ctx, qtx, p.UserID); err != nil {
		return nil, err
	}

	// FOR UPDATE:remaining 要在同一個 transaction 裡讀了再扣,
	// 否則兩個人同時抽最後一件,兩個人都會拿到。
	items, err := qtx.LockLootBoxItems(ctx, box.ID)
	if err != nil {
		return nil, fmt.Errorf("鎖獎池: %w", err)
	}
	outcomes := make([]chance.Outcome, 0, len(items))
	for _, it := range items {
		o := chance.Outcome{Ref: fmt.Sprintf("%d", it.ID), Weight: int64(it.Weight), Remaining: -1}
		if it.Remaining != nil {
			o.Remaining = int(*it.Remaining)
		}
		outcomes = append(outcomes, o)
	}
	idx, seed, err := chance.Pick(outcomes)
	if err != nil {
		if errors.Is(err, chance.ErrNoOutcomes) {
			return nil, fmt.Errorf("箱子 %q: %w", box.Name, play.ErrPoolEmpty)
		}
		return nil, err
	}
	won := items[idx]

	// 有限項目要扣庫存。條件帶 remaining > 0,所以就算上面算錯也不會變負數。
	if won.Remaining != nil {
		n, err := qtx.DecrementLootBoxItem(ctx, won.ID)
		if err != nil {
			return nil, fmt.Errorf("扣獎池庫存: %w", err)
		}
		if n == 0 {
			// 讀到的時候還有,扣的時候沒了 —— 理論上 FOR UPDATE 擋得住,
			// 這是保險分支。回「抽完了」而不是硬發,寧可少發也不能超發。
			return nil, fmt.Errorf("箱子 %q: %w", box.Name, play.ErrPoolEmpty)
		}
	}

	// 扣款 + 發獎在同一次帳本呼叫:tokens 類獎勵也是分錄,
	// 所以開箱的淨變動一次算完,不會出現「扣了但獎沒發」的中間狀態。
	ops := []ledger.Op{{
		UserID: p.UserID, Currency: box.CostCurrency,
		Amount: -box.CostAmount, Reason: ledger.ReasonBoxOpen,
	}}
	rewardName := won.RewardRef
	var amount int64
	if won.RewardKind == "tokens" {
		if won.Amount == nil || *won.Amount <= 0 {
			return nil, fmt.Errorf("獎池項目 %d 的點數金額不合法", won.ID)
		}
		amount = *won.Amount
		ops = append(ops, ledger.Op{
			UserID: p.UserID, Currency: won.RewardRef,
			Amount: amount, Reason: ledger.ReasonBoxReward,
		})
		rewardName = fmt.Sprintf("%d %s", amount, won.RewardRef)
	}

	led, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: p.IdempotencyKey,
		RequestHash:    p.IdempotencyKey,
		Ops:            ops,
	})
	if err != nil {
		return nil, err
	}

	if won.RewardKind == "item" {
		name, err := s.grantItem(ctx, qtx, p.UserID, won.RewardRef, "loot_box")
		if err != nil {
			return nil, err
		}
		rewardName = name
	}
	if won.RewardKind == "entitlement" {
		// 與里程碑獎勵同樣的處置:明確失敗而不是靜默跳過。
		// 設定了獎勵卻什麼都沒發生,是最難被發現的壞法。
		return nil, errors.New("entitlement 類獎勵尚未接上發放路徑")
	}

	odds, err := json.Marshal(map[string]any{"items": outcomes})
	if err != nil {
		return nil, fmt.Errorf("序列化賠率: %w", err)
	}
	outcome, err := json.Marshal(map[string]any{
		"item_id": won.ID, "kind": won.RewardKind, "ref": won.RewardRef, "amount": amount,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化結果: %w", err)
	}
	if _, err := qtx.InsertChanceDraw(ctx, db.InsertChanceDrawParams{
		CommunityID:  box.CommunityID,
		UserID:       p.UserID,
		Kind:         string(chance.KindLootBox),
		Ref:          box.PublicID,
		OddsSnapshot: odds,
		Seed:         seed,
		Outcome:      outcome,
		Stake:        box.CostAmount,
		Payout:       amount,
	}); err != nil {
		return nil, fmt.Errorf("寫抽籤紀錄: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	balance := int64(0)
	if n := len(led.Entries); n > 0 {
		balance = led.Entries[n-1].NewBalance
	}
	return &play.OpenResult{
		BoxName: box.Name, RewardKind: won.RewardKind, RewardName: rewardName,
		Amount: amount, Balance: balance, Seed: seed,
	}, nil
}

// grantItem 發一件物品,回傳顯示用名稱。
//
// 抽到寵物時一併建 pet_states:少了那一列,寵物看得到卻養不了,
// 而且 /pet 會查不到它 —— 那是「發了但等於沒發」。
func (s *Service) grantItem(
	ctx context.Context, qtx *db.Queries, userID int64, definitionPublicID, via string,
) (string, error) {
	row, err := qtx.GrantItemToUser(ctx, db.GrantItemToUserParams{
		PublicID:    mustULID(),
		OwnerID:     userID,
		PublicID_2:  definitionPublicID,
		AcquiredVia: &via,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("物品定義 %q 不存在: %w", definitionPublicID, play.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("發物品: %w", err)
	}
	if row.Category == "pet" {
		if err := qtx.CreatePetState(ctx, db.CreatePetStateParams{
			ItemInstanceID: row.ID, OwnerID: userID,
		}); err != nil {
			return "", fmt.Errorf("建寵物狀態: %w", err)
		}
	}
	return definitionPublicID, nil
}

// mustULID 產生 public_id。crypto/rand 失敗是系統級異常,
// 在這條路徑上沒有合理的降級方式(可預測的 public_id 等於可枚舉)。
func mustULID() string {
	id, err := ulid.New()
	if err != nil {
		panic(fmt.Sprintf("產生 ULID 失敗(crypto/rand 異常): %v", err))
	}
	return id
}
