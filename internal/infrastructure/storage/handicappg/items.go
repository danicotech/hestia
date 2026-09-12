package handicappg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// ListItems 列出某屆的全部讓武項目(排序由 SQL 決定:category, sort_order, id)。
func (r *Repo) ListItems(ctx context.Context, tournamentID int64) ([]handicap.Item, error) {
	rows, err := r.q.ListHandicapItems(ctx, tournamentID)
	if err != nil {
		return nil, fmt.Errorf("列出讓武項目 tournament=%d: %w", tournamentID, err)
	}
	out := make([]handicap.Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, handicap.Item{
			ID:           row.ID,
			TournamentID: row.TournamentID,
			Ref:          row.PublicID,
			Category:     handicap.Category(row.Category),
			Name:         row.Name,
			Description:  deref(row.Description),
			RefereeNote:  deref(row.RefereeNote),
			Cost:         row.Cost,
			Repeatable:   row.Repeatable,
			SortOrder:    row.SortOrder,
		})
	}
	return out, nil
}

// GetItem 依 Ref(= handicap_items.public_id)取單一項目。
//
// tournament_id 一起進 WHERE,所以「拿上一屆的 ref 買這一屆」是 ErrItemNotFound
// 而不是拿到一筆別屆的價格。
func (r *Repo) GetItem(ctx context.Context, tournamentID int64, ref string) (*handicap.Item, error) {
	row, err := r.q.GetHandicapItemByPublicID(ctx, db.GetHandicapItemByPublicIDParams{
		TournamentID: tournamentID,
		PublicID:     ref,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("tournament=%d item=%s: %w", tournamentID, ref, handicap.ErrItemNotFound)
		}
		return nil, fmt.Errorf("讀讓武項目 %s: %w", ref, err)
	}
	return &handicap.Item{
		ID:           row.ID,
		TournamentID: row.TournamentID,
		Ref:          row.PublicID,
		Category:     handicap.Category(row.Category),
		Name:         row.Name,
		Description:  deref(row.Description),
		RefereeNote:  deref(row.RefereeNote),
		Cost:         row.Cost,
		Repeatable:   row.Repeatable,
		SortOrder:    row.SortOrder,
	}, nil
}

// InsertItems 把項目定義實例化到某一屆,回實際插入的列數。
//
// ON CONFLICT (tournament_id, category, name) DO NOTHING —— 重跑回 0,不改價格。
func (r *Repo) InsertItems(ctx context.Context, tournamentID int64, specs []handicap.ItemSpec) (int, error) {
	if len(specs) == 0 {
		return 0, nil
	}

	n := len(specs)
	arg := db.InsertHandicapItemsParams{
		TournamentID: tournamentID,
		PublicIds:    make([]string, 0, n),
		Categories:   make([]string, 0, n),
		Names:        make([]string, 0, n),
		Descriptions: make([]string, 0, n),
		RefereeNotes: make([]string, 0, n),
		Costs:        make([]int64, 0, n),
		Repeatables:  make([]bool, 0, n),
		SortOrders:   make([]int32, 0, n),
	}

	// 八個陣列一定要在**同一個迴圈**裡組出來。
	//
	// 這不是風格問題:Postgres 的多個 unnest 併排展開時,長度不一致的那些會被補 NULL
	// 而不是報錯 —— 少 append 一次 cost,結果是一列 cost 為 NULL 被 NOT NULL 擋下,
	// 少 append 一次 description 則會安靜地插進一筆沒有說明的項目。
	// 分成八個迴圈寫,任何一次 continue / 提早 break 都能製造出這種錯位。
	for _, s := range specs {
		pubID, err := ulid.New()
		if err != nil {
			return 0, fmt.Errorf("產生讓武項目 public_id: %w", err)
		}
		arg.PublicIds = append(arg.PublicIds, pubID)
		arg.Categories = append(arg.Categories, string(s.Category))
		arg.Names = append(arg.Names, s.Name)
		arg.Descriptions = append(arg.Descriptions, s.Description)
		// RefereeNote 是指標,因為「還沒寫」與「寫了空字串」在裁判手冊補齊的過程中
		// 是兩件事。但 text[] 的元素表達不了 NULL,所以 nil 先攤成空字串,
		// 由 SQL 那側的 NULLIF(..., '') 還原成 NULL —— 兩端要一起看才完整。
		note := ""
		if s.RefereeNote != nil {
			note = *s.RefereeNote
		}
		arg.RefereeNotes = append(arg.RefereeNotes, note)
		arg.Costs = append(arg.Costs, s.Cost)
		arg.Repeatables = append(arg.Repeatables, s.Repeatable)
		arg.SortOrders = append(arg.SortOrders, s.SortOrder)
	}

	inserted, err := r.q.InsertHandicapItems(ctx, arg)
	if err != nil {
		return 0, fmt.Errorf("寫入讓武項目 tournament=%d: %w", tournamentID, err)
	}
	return int(inserted), nil
}
