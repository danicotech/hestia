package betting

import (
	"context"
	"fmt"
)

// 盤口的生命週期(schemas/21「什麼時候建、什麼時候關」)。
//
// 兩支都是 *InTx:呼叫端是 match 套件的狀態轉移(進 ready、開打),盤口的建與關
// 必須與那句 UPDATE matches 同生共死 —— 建了盤口但場次沒進 ready,或場次開打了
// 盤口還開著,都是觀眾看得到的矛盾。

// OpenMarketsInTx 在場次進入 ready 時依 rules 建盤口。
//
// match_winner / score 各一個;round_winner / duration 為 best_of 個回合各一個
// (Rules.MarketsFor)。betting.enabled = false 的賽事回空清單、不建列。
// 同一場建第二次撞 markets_match_kind_line_uq,回 ErrMarketsExist ——
// 不吞:兩次開盤代表狀態機走岔了,要出聲。
func (s *Service[TX]) OpenMarketsInTx(ctx context.Context, tx TX, matchPublicID string) ([]Market, error) {
	m, err := s.oneMatch(ctx, tx, matchPublicID)
	if err != nil {
		return nil, err
	}
	r, err := s.rules(ctx, tx, m.TournamentID)
	if err != nil {
		return nil, err
	}
	plan := r.MarketsFor()
	if len(plan) == 0 {
		return nil, nil
	}
	for i := range plan {
		id, err := s.newPublicID()
		if err != nil {
			return nil, fmt.Errorf("產生盤口 public_id: %w", err)
		}
		plan[i].PublicID = id
	}
	markets, err := s.repo.InsertMarkets(ctx, tx, m.ID, plan)
	if err != nil {
		return nil, fmt.Errorf("match=%s 建盤口: %w", m.PublicID, err)
	}
	return markets, nil
}

// CloseMarketsInTx 在第一回合正式決鬥開始時把該場所有 open 的盤口一起關掉,
// 回關掉的數量(grill Q11/Q20:不逐回合重開,第二回合開始前也不能押第二回合)。
//
// 0 不是錯誤:重複呼叫、或這屆本來就不開盤,都會是 0。
func (s *Service[TX]) CloseMarketsInTx(ctx context.Context, tx TX, matchPublicID string) (int, error) {
	m, err := s.oneMatch(ctx, tx, matchPublicID)
	if err != nil {
		return 0, err
	}
	n, err := s.repo.CloseMarkets(ctx, tx, m.ID)
	if err != nil {
		return 0, fmt.Errorf("match=%s 關盤: %w", m.PublicID, err)
	}
	return n, nil
}
