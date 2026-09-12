package handicap

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
)

// fakeRepo 是記憶體版 Repository。
//
// WithTx 用一把互斥鎖模擬 LockMatch 的 SELECT ... FOR UPDATE ——
// 讓武的所有寫入都以場次列為序列化點,fake 用整把鎖是它的保守近似:
// 真實的隔離保證要等 pg adapter 用真 Postgres 測(併發連點、一邊買一邊封盤)。
// 這裡測的是**規則**在序列化前提下正確,不是資料庫的隔離級別。
type fakeRepo struct {
	mu sync.Mutex

	matches    map[string]*Match
	items      []*Item
	budgets    map[[2]int64]*Budget
	selections []*Selection

	nextSelectionID int64
}

var _ Repository = (*fakeRepo)(nil)

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		matches: map[string]*Match{},
		budgets: map[[2]int64]*Budget{},
	}
}

func (f *fakeRepo) WithTx(ctx context.Context, fn func(context.Context, Repository) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	undo := f.snapshot()
	// 傳進去的是 fakeTx 而不是 f 自己:巢狀交易在這裡會拿到一個明確的錯誤,
	// 而不是在同一把不可重入的鎖上死等。
	if err := fn(ctx, fakeTx{f}); err != nil {
		undo() // 模擬 ROLLBACK:失敗的動作不留下半套寫入
		return err
	}
	return nil
}

// fakeTx 是交易內看到的 Repository。除了 WithTx 以外全部沿用 fakeRepo。
type fakeTx struct{ *fakeRepo }

func (fakeTx) WithTx(context.Context, func(context.Context, Repository) error) error {
	return errors.New("巢狀交易")
}

// snapshot 回傳一個把狀態還原到當下的函式。
func (f *fakeRepo) snapshot() func() {
	budgets := make(map[[2]int64]Budget, len(f.budgets))
	for k, v := range f.budgets {
		budgets[k] = *v
	}
	sels := make([]Selection, len(f.selections))
	for i, s := range f.selections {
		sels[i] = *s
	}
	matches := make(map[string]Match, len(f.matches))
	for k, v := range f.matches {
		matches[k] = *v
	}
	nextID := f.nextSelectionID
	return func() {
		f.budgets = map[[2]int64]*Budget{}
		for k, v := range budgets {
			f.budgets[k] = &v
		}
		f.selections = nil
		for i := range sels {
			f.selections = append(f.selections, &sels[i])
		}
		f.matches = map[string]*Match{}
		for k, v := range matches {
			f.matches[k] = &v
		}
		f.nextSelectionID = nextID
	}
}

func (f *fakeRepo) LockMatch(ctx context.Context, matchPublicID string) (*Match, error) {
	return f.GetMatch(ctx, matchPublicID)
}

func (f *fakeRepo) GetMatch(_ context.Context, matchPublicID string) (*Match, error) {
	m, ok := f.matches[matchPublicID]
	if !ok {
		return nil, activityerr.ErrMatchNotFound
	}
	cp := *m
	return &cp, nil
}

func (f *fakeRepo) matchByID(id int64) *Match {
	for _, m := range f.matches {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (f *fakeRepo) ListItems(_ context.Context, tournamentID int64) ([]Item, error) {
	var out []Item
	for _, it := range f.items {
		if it.TournamentID == tournamentID {
			out = append(out, *it)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetItem(_ context.Context, tournamentID int64, ref string) (*Item, error) {
	for _, it := range f.items {
		if it.Ref == ref && it.TournamentID == tournamentID {
			cp := *it
			return &cp, nil
		}
	}
	return nil, ErrItemNotFound
}

func (f *fakeRepo) InsertItems(_ context.Context, tournamentID int64, specs []ItemSpec) (int, error) {
	exists := map[string]bool{}
	for _, it := range f.items {
		if it.TournamentID == tournamentID {
			exists[itemKey(it.Category, it.Name)] = true
		}
	}
	inserted := 0
	for _, sp := range specs {
		if exists[itemKey(sp.Category, sp.Name)] {
			continue // ON CONFLICT DO NOTHING
		}
		id := int64(len(f.items) + 1)
		note := ""
		if sp.RefereeNote != nil {
			note = *sp.RefereeNote
		}
		f.items = append(f.items, &Item{
			ID:           id,
			TournamentID: tournamentID,
			Ref:          strconv.FormatInt(id, 10),
			Category:     sp.Category,
			Name:         sp.Name,
			Description:  sp.Description,
			RefereeNote:  note,
			Cost:         sp.Cost,
			Repeatable:   sp.Repeatable,
			SortOrder:    sp.SortOrder,
		})
		exists[itemKey(sp.Category, sp.Name)] = true
		inserted++
	}
	return inserted, nil
}

func (f *fakeRepo) GetBudget(_ context.Context, matchID, playerID int64) (*Budget, error) {
	b, ok := f.budgets[[2]int64{matchID, playerID}]
	if !ok {
		return nil, ErrNoBudget
	}
	cp := *b
	return &cp, nil
}

func (f *fakeRepo) FindMatchBudget(_ context.Context, matchID int64) (*Budget, error) {
	for k, b := range f.budgets {
		if k[0] == matchID {
			cp := *b
			return &cp, nil
		}
	}
	return nil, ErrNoBudget
}

func (f *fakeRepo) InsertBudget(_ context.Context, matchID, playerID, budget int64) (*Budget, error) {
	key := [2]int64{matchID, playerID}
	if _, ok := f.budgets[key]; ok {
		return nil, ErrBudgetExists
	}
	b := &Budget{
		MatchID:        matchID,
		PlayerID:       playerID,
		PlayerPublicID: playerPublicID(playerID),
		Budget:         budget,
	}
	f.budgets[key] = b
	cp := *b
	return &cp, nil
}

func (f *fakeRepo) SetSpent(_ context.Context, matchID, playerID, spent int64) error {
	b, ok := f.budgets[[2]int64{matchID, playerID}]
	if !ok {
		return ErrNoBudget
	}
	// 對應 DB 的 match_budgets_spent_check —— 應用層擋了,DB 還是說了算。
	if spent < 0 || spent > b.Budget {
		return fmt.Errorf("違反 spent CHECK: spent=%d budget=%d", spent, b.Budget)
	}
	b.Spent = spent
	return nil
}

func (f *fakeRepo) ListSelections(_ context.Context, matchID, playerID int64) ([]Selection, error) {
	var out []Selection
	for _, s := range f.selections {
		if s.MatchID == matchID && s.PlayerID == playerID && !s.Voided {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetSelection(_ context.Context, publicID string) (*Selection, error) {
	for _, s := range f.selections {
		if s.PublicID == publicID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, ErrSelectionNotFound
}

func (f *fakeRepo) InsertSelection(ctx context.Context, ns NewSelection) (*Selection, error) {
	var item *Item
	for _, it := range f.items {
		if it.ID == ns.ItemID {
			item = it
			break
		}
	}
	if item == nil {
		return nil, ErrItemNotFound
	}
	m := f.matchByID(ns.MatchID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	// 對應 handicap_selections_budget_fkey:沒有預算的人連一列都插不進來。
	if _, err := f.GetBudget(ctx, ns.MatchID, ns.PlayerID); err != nil {
		return nil, err
	}
	f.nextSelectionID++
	s := &Selection{
		ID:            f.nextSelectionID,
		PublicID:      fmt.Sprintf("SEL%04d", f.nextSelectionID),
		MatchID:       ns.MatchID,
		MatchPublicID: m.PublicID,
		PlayerID:      ns.PlayerID,
		ItemID:        item.ID,
		ItemRef:       item.Ref,
		ItemName:      item.Name,
		Category:      item.Category,
		Cost:          ns.Cost,
		TargetNote:    ns.TargetNote,
		CreatedAt:     time.Unix(1_800_000_000+f.nextSelectionID, 0).UTC(),
	}
	f.selections = append(f.selections, s)
	cp := *s
	return &cp, nil
}

func (f *fakeRepo) MarkSelectionVoided(_ context.Context, selectionID int64) error {
	for _, s := range f.selections {
		if s.ID == selectionID {
			s.Voided = true
			return nil
		}
	}
	return ErrSelectionNotFound
}

func (f *fakeRepo) LockHandicaps(_ context.Context, matchID int64, at time.Time) error {
	m := f.matchByID(matchID)
	if m == nil {
		return activityerr.ErrMatchNotFound
	}
	m.HandicapOpen = false
	m.LockedAt = &at
	m.Status = statusLocked
	return nil
}

// sumSelections 是測試用的權威口徑:未作廢選擇的 cost 總和。
func (f *fakeRepo) sumSelections(matchID, playerID int64) int64 {
	var sum int64
	for _, s := range f.selections {
		if s.MatchID == matchID && s.PlayerID == playerID && !s.Voided {
			sum += s.Cost
		}
	}
	return sum
}

func playerPublicID(id int64) string { return fmt.Sprintf("P%03d", id) }
