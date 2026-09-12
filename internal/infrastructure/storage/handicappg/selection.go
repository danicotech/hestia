package handicappg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// ListSelections 列出某人在某場**未作廢**的選擇(過濾寫在 SQL 裡)。
func (r *Repo) ListSelections(ctx context.Context, matchID, playerID int64) ([]handicap.Selection, error) {
	rows, err := r.q.ListHandicapSelections(ctx, db.ListHandicapSelectionsParams{
		MatchID:  matchID,
		PlayerID: playerID,
	})
	if err != nil {
		return nil, fmt.Errorf("列出讓武選擇 match=%d player=%d: %w", matchID, playerID, err)
	}
	out := make([]handicap.Selection, 0, len(rows))
	for _, row := range rows {
		out = append(out, selectionFields{
			ID:            row.ID,
			PublicID:      row.PublicID,
			MatchID:       row.MatchID,
			MatchPublicID: row.MatchPublicID,
			PlayerID:      row.PlayerID,
			ItemID:        row.ItemID,
			ItemRef:       row.ItemRef,
			ItemName:      row.ItemName,
			Category:      row.Category,
			Cost:          row.Cost,
			TargetNote:    row.TargetNote,
			Voided:        row.Voided,
			CreatedAt:     row.CreatedAt,
		}.toSelection())
	}
	return out, nil
}

// GetSelection 依 public_id 取單筆,**含已作廢者**。
//
// 不過濾 voided 是刻意的:退選路徑要分得出「已經退過」與「不存在」,
// 過濾掉的話重複退選會看起來像查無此筆。
func (r *Repo) GetSelection(ctx context.Context, selectionPublicID string) (*handicap.Selection, error) {
	row, err := r.q.GetHandicapSelection(ctx, selectionPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("selection=%s: %w", selectionPublicID, handicap.ErrSelectionNotFound)
		}
		return nil, fmt.Errorf("讀讓武選擇 %s: %w", selectionPublicID, err)
	}
	sel := selectionFields{
		ID:            row.ID,
		PublicID:      row.PublicID,
		MatchID:       row.MatchID,
		MatchPublicID: row.MatchPublicID,
		PlayerID:      row.PlayerID,
		ItemID:        row.ItemID,
		ItemRef:       row.ItemRef,
		ItemName:      row.ItemName,
		Category:      row.Category,
		Cost:          row.Cost,
		TargetNote:    row.TargetNote,
		Voided:        row.Voided,
		CreatedAt:     row.CreatedAt,
	}.toSelection()
	return &sel, nil
}

// InsertSelection 寫入一列並回傳完整內容。
//
// public_id(ULID)在這裡產生 —— core 不認識任何 ULID 套件,SQL 也生不出 ULID。
// 沒有 ON CONFLICT:重複購買同一項目是設計,每買一次就是新的一列。
func (r *Repo) InsertSelection(ctx context.Context, ns handicap.NewSelection) (*handicap.Selection, error) {
	pubID, err := ulid.New()
	if err != nil {
		return nil, fmt.Errorf("產生讓武選擇 public_id: %w", err)
	}
	row, err := r.q.InsertHandicapSelection(ctx, db.InsertHandicapSelectionParams{
		PublicID:   pubID,
		MatchID:    ns.MatchID,
		PlayerID:   ns.PlayerID,
		ItemID:     ns.ItemID,
		Cost:       ns.Cost,
		TargetNote: ns.TargetNote,
	})
	if err != nil {
		// 複合外鍵指向 match_budgets:沒有預算的人連一列都插不進來。
		// 規則層已經先查過預算,這裡是同一個判斷在 DB 側的備份 —— 翻成同一個
		// sentinel,呼叫端不必為「哪一層擋下的」寫兩套處理。
		if violates(err, codeForeignKeyViolation, constraintSelectionBudget) {
			return nil, fmt.Errorf("match=%d player=%d: %w", ns.MatchID, ns.PlayerID, handicap.ErrNoBudget)
		}
		return nil, fmt.Errorf("寫入讓武選擇 match=%d player=%d: %w", ns.MatchID, ns.PlayerID, err)
	}
	sel := selectionFields{
		ID:            row.ID,
		PublicID:      row.PublicID,
		MatchID:       row.MatchID,
		MatchPublicID: row.MatchPublicID,
		PlayerID:      row.PlayerID,
		ItemID:        row.ItemID,
		ItemRef:       row.ItemRef,
		ItemName:      row.ItemName,
		Category:      row.Category,
		Cost:          row.Cost,
		TargetNote:    row.TargetNote,
		Voided:        row.Voided,
		CreatedAt:     row.CreatedAt,
	}.toSelection()
	return &sel, nil
}

// MarkSelectionVoided 標記作廢。0 列 = 已經退過(SQL 的 AND NOT voided 擋掉)。
func (r *Repo) MarkSelectionVoided(ctx context.Context, selectionID int64) error {
	n, err := r.q.MarkHandicapSelectionVoided(ctx, selectionID)
	if err != nil {
		return fmt.Errorf("作廢讓武選擇 %d: %w", selectionID, err)
	}
	if n == 0 {
		return fmt.Errorf("selection=%d: %w", selectionID, handicap.ErrSelectionAlreadyVoided)
	}
	return nil
}

// selectionFields 是三支選擇查詢共同的欄位形狀。
//
// 三支查詢的 select list 逐字相同,sqlc 卻生出三個型別。轉換只寫一份的理由
// 與 matchFields 相同:三份拷貝遲早會有一份在加欄位時被漏掉。
type selectionFields struct {
	ID            int64
	PublicID      string
	MatchID       int64
	MatchPublicID string
	PlayerID      int64
	ItemID        int64
	ItemRef       string
	ItemName      string
	Category      string
	Cost          int64
	TargetNote    *string
	Voided        bool
	CreatedAt     time.Time
}

func (f selectionFields) toSelection() handicap.Selection {
	return handicap.Selection{
		ID:            f.ID,
		PublicID:      f.PublicID,
		MatchID:       f.MatchID,
		MatchPublicID: f.MatchPublicID,
		PlayerID:      f.PlayerID,
		ItemID:        f.ItemID,
		ItemRef:       f.ItemRef,
		ItemName:      f.ItemName,
		Category:      handicap.Category(f.Category),
		Cost:          f.Cost,
		TargetNote:    deref(f.TargetNote),
		Voided:        f.Voided,
		CreatedAt:     f.CreatedAt,
	}
}
