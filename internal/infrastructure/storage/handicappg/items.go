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

// itemFields 是三支項目查詢共同的欄位形狀(List / GetByPublicID / GetByKey)。
//
// sqlc 為每支查詢各生一個 row 型別;轉成 core 型別只寫一份,理由與 matchFields 相同。
type itemFields struct {
	ID           int64
	PublicID     string
	TournamentID int64
	Key          string
	Category     string
	Name         string
	Description  *string
	RefereeNote  *string
	Cost         int64
	Repeatable   bool
	SortOrder    int32
	Params       []byte
}

// toItem 轉成 core 型別。params 在這裡驗:JSONB 不是垃圾桶,有人繞過程式直接改了
// 一個未登記的鍵,要在讀回時就出聲,而不是讓一個沒有人認得的參數混進封盤抽選。
func (f itemFields) toItem() (handicap.Item, error) {
	params, err := handicap.ParseItemParams(f.Params)
	if err != nil {
		return handicap.Item{}, fmt.Errorf("讓武項目 %s(tournament=%d): %w", f.Key, f.TournamentID, err)
	}
	return handicap.Item{
		ID:           f.ID,
		TournamentID: f.TournamentID,
		Ref:          f.PublicID,
		Key:          f.Key,
		Category:     handicap.Category(f.Category),
		Name:         f.Name,
		Description:  deref(f.Description),
		RefereeNote:  deref(f.RefereeNote),
		Cost:         f.Cost,
		Repeatable:   f.Repeatable,
		SortOrder:    f.SortOrder,
		Params:       params,
	}, nil
}

// ListItems 列出某屆的全部讓武項目(排序由 SQL 決定:category, sort_order, id)。
func (r *Repo) ListItems(ctx context.Context, tournamentID int64) ([]handicap.Item, error) {
	rows, err := r.q.ListHandicapItems(ctx, tournamentID)
	if err != nil {
		return nil, fmt.Errorf("列出讓武項目 tournament=%d: %w", tournamentID, err)
	}
	out := make([]handicap.Item, 0, len(rows))
	for _, row := range rows {
		it, err := itemFields(row).toItem()
		if err != nil {
			return nil, err
		}
		out = append(out, it)
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
	it, err := itemFields(row).toItem()
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// GetItemByKey 依穩定識別取單一項目(目錄同步、測試用)。tournament_id 一起進 WHERE 同上。
func (r *Repo) GetItemByKey(ctx context.Context, tournamentID int64, key string) (*handicap.Item, error) {
	row, err := r.q.GetHandicapItemByKey(ctx, db.GetHandicapItemByKeyParams{
		TournamentID: tournamentID,
		Key:          key,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("tournament=%d key=%s: %w", tournamentID, key, handicap.ErrItemNotFound)
		}
		return nil, fmt.Errorf("讀讓武項目 %s: %w", key, err)
	}
	it, err := itemFields(row).toItem()
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// InsertItems 把項目定義實例化到某一屆,回實際插入的列數。
//
// ON CONFLICT (tournament_id, category, name) DO NOTHING —— 重跑回 0,不改價格。
// key 撞到(改了名稱卻用 install 而不是 sync)是 23505,原樣冒上來:那是要的。
func (r *Repo) InsertItems(ctx context.Context, tournamentID int64, specs []handicap.ItemSpec) (int, error) {
	if len(specs) == 0 {
		return 0, nil
	}

	n := len(specs)
	arg := db.InsertHandicapItemsParams{
		TournamentID: tournamentID,
		PublicIds:    make([]string, 0, n),
		Keys:         make([]string, 0, n),
		Categories:   make([]string, 0, n),
		Names:        make([]string, 0, n),
		Descriptions: make([]string, 0, n),
		RefereeNotes: make([]string, 0, n),
		Costs:        make([]int64, 0, n),
		Repeatables:  make([]bool, 0, n),
		SortOrders:   make([]int32, 0, n),
		Params:       make([]string, 0, n),
	}

	// 十個陣列一定要在**同一個迴圈**裡組出來。
	//
	// 這不是風格問題:Postgres 的多個 unnest 併排展開時,長度不一致的那些會被補 NULL
	// 而不是報錯 —— 少 append 一次 cost,結果是一列 cost 為 NULL 被 NOT NULL 擋下,
	// 少 append 一次 description 則會安靜地插進一筆沒有說明的項目。
	// 分成十個迴圈寫,任何一次 continue / 提早 break 都能製造出這種錯位。
	for _, s := range specs {
		pubID, err := ulid.New()
		if err != nil {
			return 0, fmt.Errorf("產生讓武項目 public_id: %w", err)
		}
		params, err := s.Params.JSON()
		if err != nil {
			return 0, fmt.Errorf("項目 %s: %w", s.Key, err)
		}
		arg.PublicIds = append(arg.PublicIds, pubID)
		arg.Keys = append(arg.Keys, s.Key)
		arg.Categories = append(arg.Categories, string(s.Category))
		arg.Names = append(arg.Names, s.Name)
		arg.Descriptions = append(arg.Descriptions, s.Description)
		arg.RefereeNotes = append(arg.RefereeNotes, refereeNote(s))
		arg.Costs = append(arg.Costs, s.Cost)
		arg.Repeatables = append(arg.Repeatables, s.Repeatable)
		arg.SortOrders = append(arg.SortOrders, s.SortOrder)
		// params 以 JSON 文字傳入,SQL 那側 ::jsonb(pgx 對 jsonb[] 的編碼不穩定)。
		arg.Params = append(arg.Params, params)
	}

	inserted, err := r.q.InsertHandicapItems(ctx, arg)
	if err != nil {
		return 0, fmt.Errorf("寫入讓武項目 tournament=%d: %w", tournamentID, err)
	}
	return int(inserted), nil
}

// SyncItems 以 key 匹配,把目錄的文字與參數寫進該屆既有的項目,回影響列數。
//
// **不動 cost**(SQL 的 SET 清單裡沒有它),也不補建 key 不存在的項目。
// 前置條件(該屆沒有選擇)與影響列數的核對都在 handicap.Service.SyncCatalogue,
// 這裡只負責把七個陣列對齊 —— 同一個迴圈,理由同 InsertItems。
func (r *Repo) SyncItems(ctx context.Context, tournamentID int64, specs []handicap.ItemSpec) (int, error) {
	if len(specs) == 0 {
		return 0, nil
	}
	n := len(specs)
	arg := db.SyncHandicapItemsParams{
		TournamentID: tournamentID,
		Keys:         make([]string, 0, n),
		Names:        make([]string, 0, n),
		Descriptions: make([]string, 0, n),
		RefereeNotes: make([]string, 0, n),
		Repeatables:  make([]bool, 0, n),
		SortOrders:   make([]int32, 0, n),
		Params:       make([]string, 0, n),
	}
	for _, s := range specs {
		params, err := s.Params.JSON()
		if err != nil {
			return 0, fmt.Errorf("項目 %s: %w", s.Key, err)
		}
		arg.Keys = append(arg.Keys, s.Key)
		arg.Names = append(arg.Names, s.Name)
		arg.Descriptions = append(arg.Descriptions, s.Description)
		arg.RefereeNotes = append(arg.RefereeNotes, refereeNote(s))
		arg.Repeatables = append(arg.Repeatables, s.Repeatable)
		arg.SortOrders = append(arg.SortOrders, s.SortOrder)
		arg.Params = append(arg.Params, params)
	}
	updated, err := r.q.SyncHandicapItems(ctx, arg)
	if err != nil {
		return 0, fmt.Errorf("同步讓武項目 tournament=%d: %w", tournamentID, err)
	}
	return int(updated), nil
}

// CountSelections 數該屆指向任何讓武項目的選擇,含已作廢(SQL 不濾 voided)。
func (r *Repo) CountSelections(ctx context.Context, tournamentID int64) (int64, error) {
	n, err := r.q.CountHandicapSelectionsInTournament(ctx, tournamentID)
	if err != nil {
		return 0, fmt.Errorf("數讓武選擇 tournament=%d: %w", tournamentID, err)
	}
	return n, nil
}

// refereeNote 把 ItemSpec 的可空 referee_note 攤成字串。
//
// RefereeNote 是指標,因為「還沒寫」與「寫了空字串」在裁判手冊補齊的過程中
// 是兩件事。但 text[] 的元素表達不了 NULL,所以 nil 先攤成空字串,
// 由 SQL 那側的 NULLIF(..., ”) 還原成 NULL —— 兩端要一起看才完整。
func refereeNote(s handicap.ItemSpec) string {
	if s.RefereeNote == nil {
		return ""
	}
	return *s.RefereeNote
}
