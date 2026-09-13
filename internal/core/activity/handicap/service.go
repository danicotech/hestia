package handicap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/bp"
)

// Service 是讓武選購的全部規則。
//
// 所有寫入路徑都跑在 Repository.WithTx 裡,而且第一件事一定是 LockMatch ——
// 以場次列為序列化點,讓「連點兩次買同一項」「一邊買一邊封盤」這兩種競態
// 在資料庫層就排成序,而不是靠應用層的檢查順序去賭。
type Service struct {
	repo Repository
	now  func() time.Time
	// rng 是封盤抽選的亂數來源;預設走 math/rand/v2 的全域函式(見 draw.go)。
	rng randomSource
}

// Option 用來替換預設相依:時鐘(測試要可預期的封盤時間)與亂數(WithRand)。
type Option func(*Service)

// WithClock 換掉時鐘。
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New 建立 Service。
func New(repo Repository, opts ...Option) *Service {
	s := &Service{repo: repo, now: time.Now, rng: globalRand{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ── 項目 ────────────────────────────────────────────────────────

// InstallSeedItems 把內嵌的 34 項讓武項目實例化到某一屆賽事,回實際新增的列數。
//
// 逐屆一套是刻意的:價格要能逐屆調,而上一屆的選購紀錄必須永遠指向上一屆的價格。
// 重跑只會補上缺的項目,不會改已存在項目的價格(見 Repository.InsertItems)。
func (s *Service) InstallSeedItems(ctx context.Context, tournamentID int64) (int, error) {
	if tournamentID <= 0 {
		return 0, fmt.Errorf("%w: 缺少 tournament_id", ErrInvalidRequest)
	}
	specs, err := SeedItems()
	if err != nil {
		return 0, err
	}
	return s.repo.InsertItems(ctx, tournamentID, specs)
}

// SyncCatalogue 把內嵌目錄的**文字與參數**同步進既有的一屆(cmd/admin catalogue-sync)。
//
// 以 key 匹配;不動 cost(價格逐屆定案),不補建缺的項目(那是 InstallSeedItems)。
// 三件事在同一個 tx 內:
//
//  1. 該屆已有任何選擇(含已退掉)→ ErrCatalogueInUse。已有人依舊文字買過,
//     改文字等於改比賽條件。
//  2. UPDATE,回影響列數。
//  3. 影響列數必須等於「seed 中在該屆有對應 key 的項數」,否則 ErrCatalogueMismatch ——
//     對不上代表目錄與該屆的項目不一致,而 UPDATE 已經做了一半,整個 tx 回滾。
//
// 稽核不在這裡寫:本套件只認標準庫,呼叫端(CLI)在同一個 tx 上寫 admin_audit_logs。
func (s *Service) SyncCatalogue(ctx context.Context, tournamentID int64) (*SyncReport, error) {
	if tournamentID <= 0 {
		return nil, fmt.Errorf("%w: 缺少 tournament_id", ErrInvalidRequest)
	}
	specs, err := SeedItems()
	if err != nil {
		return nil, err
	}
	var out SyncReport
	err = s.repo.WithTx(ctx, func(ctx context.Context, r Repository) error {
		n, err := r.CountSelections(ctx, tournamentID)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: tournament=%d 已有 %d 筆選擇", ErrCatalogueInUse, tournamentID, n)
		}

		existing, err := r.ListItems(ctx, tournamentID)
		if err != nil {
			return err
		}
		inDB := make(map[string]bool, len(existing))
		for _, it := range existing {
			inDB[it.Key] = true
		}
		inSeed := make(map[string]bool, len(specs))
		var missing []string
		for _, sp := range specs {
			inSeed[sp.Key] = true
			if !inDB[sp.Key] {
				missing = append(missing, sp.Key)
			}
		}
		// 目錄有、該屆沒有:同步只更新既有項目,不補建 —— 補建是「開一屆賽事」那條路的事。
		// 但也不能靜靜跳過:跑完顯示成功、該屆卻缺項目,要到選手找不到那一項才會被發現。
		if len(missing) > 0 {
			return fmt.Errorf("%w: 目錄有但這一屆沒有的項目 %v", ErrCatalogueMismatch, missing)
		}
		expected := len(specs)
		for _, it := range existing {
			if !inSeed[it.Key] {
				out.ExtraKeys = append(out.ExtraKeys, it.Key)
			}
		}

		updated, err := r.SyncItems(ctx, tournamentID, specs)
		if err != nil {
			return err
		}
		if updated != expected {
			return fmt.Errorf("%w: 更新 %d 列,預期 %d 列(tournament=%d)",
				ErrCatalogueMismatch, updated, expected, tournamentID)
		}
		out.TournamentID = tournamentID
		out.Updated = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListItems 列出某屆的可購買項目。
func (s *Service) ListItems(ctx context.Context, tournamentID int64) ([]Item, error) {
	if tournamentID <= 0 {
		return nil, fmt.Errorf("%w: 缺少 tournament_id", ErrInvalidRequest)
	}
	return s.repo.ListItems(ctx, tournamentID)
}

// ── 發預算 ──────────────────────────────────────────────────────

// GrantBudget 在一場比賽雙方都確定時發放 BP 預算。
//
// 回傳 nil 且 err 為 nil 表示**本場無讓武**(同段對決):此時刻意不建列,
// 而不是建一筆 budget = 0。有沒有讓武這件事應該在資料上就分得出來,
// 否則「查不到列」與「查到 0」兩種情況會在每個讀取端各被解讀一次。
//
// 重複呼叫是安全的:預算已存在且金額與當下段位差算出來的一致時,原樣回傳。
// 金額不一致代表抽籤後有人改了段位,那必須失敗出聲(ErrBudgetConflict)——
// 默默覆蓋會讓已經花掉的 spent 對不上新的 budget。
func (s *Service) GrantBudget(ctx context.Context, matchPublicID string) (*Budget, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	var out *Budget
	err := s.repo.WithTx(ctx, func(ctx context.Context, r Repository) error {
		m, err := r.LockMatch(ctx, matchPublicID)
		if err != nil {
			return err
		}
		if m.P1PlayerID == 0 || m.P2PlayerID == 0 {
			return activityerr.ErrPlayersNotSet
		}
		// 已封盤之後才來發預算,代表叫用順序錯了。這時候發下去的 BP 永遠花不掉,
		// 而前端會顯示一筆「有 16 BP 但不能用」的預算,比直接失敗更難查。
		if isLocked(m) {
			return ErrHandicapLocked
		}

		amount, holderIsP1, err := bp.Holder(m.P1Rank, m.P2Rank, m.PerRankGap)
		if err != nil {
			return err // bp.ErrUnranked:還沒評完段就配對,流程出錯了
		}
		if amount == 0 {
			return nil // 同段對決,本場無讓武
		}
		holderID := m.P2PlayerID
		if holderIsP1 {
			holderID = m.P1PlayerID
		}

		b, err := r.InsertBudget(ctx, m.ID, holderID, amount)
		switch {
		case err == nil:
			out = b
			return nil
		case errors.Is(err, ErrBudgetExists):
			existing, gerr := r.GetBudget(ctx, m.ID, holderID)
			if gerr != nil {
				return gerr
			}
			if existing.Budget != amount {
				return fmt.Errorf("%w: 既有 %d BP,依當下段位應為 %d BP",
					ErrBudgetConflict, existing.Budget, amount)
			}
			out = existing
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 選購 ────────────────────────────────────────────────────────

// Select 買一項讓武。
//
// 同一項目可以重複購買,不設上限 —— 這是 09-12 定案的設計:「點數夠 都可以購買
// 同個商品」,所以 handicap_selections 刻意沒有 UNIQUE 約束。
//
// 系統也**不做互斥檢查**。victory 類買到第二項時回 AdvisoryVictoryConflict,
// 但購買照樣成立:互斥組合由裁判臨場判,系統擋一半比完全不擋更危險。
func (s *Service) Select(ctx context.Context, p SelectParams) (*SelectResult, error) {
	switch {
	case p.MatchPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	case p.ItemRef == "":
		return nil, fmt.Errorf("%w: 缺少 item ref", ErrInvalidRequest)
	case p.PlayerID <= 0:
		return nil, fmt.Errorf("%w: 缺少選手身分", ErrInvalidRequest)
	}
	note := strings.TrimSpace(p.TargetNote)
	if utf8.RuneCountInString(note) > maxTargetNoteRunes {
		return nil, fmt.Errorf("%w: 指定內容不得超過 %d 字", ErrInvalidRequest, maxTargetNoteRunes)
	}

	var out SelectResult
	err := s.repo.WithTx(ctx, func(ctx context.Context, r Repository) error {
		m, err := r.LockMatch(ctx, p.MatchPublicID)
		if err != nil {
			return err
		}
		if err := requireSelectable(m); err != nil {
			return err
		}

		// 沒有預算列 = 同段、或請求者是高段位方、或根本不是這場的選手。
		// 三者都回 ErrNoBudget:對呼叫端是同一件事,而分得更細等於告訴外人
		// 「這場的預算持有者不是你」以外的資訊。
		budget, err := r.GetBudget(ctx, m.ID, p.PlayerID)
		if err != nil {
			return err
		}

		item, err := r.GetItem(ctx, m.TournamentID, p.ItemRef)
		if err != nil {
			return err
		}
		if RequiresTargetNote(item.Key) && note == "" {
			return fmt.Errorf("%w: %s", ErrTargetNoteRequired, item.Name)
		}

		existing, err := r.ListSelections(ctx, m.ID, p.PlayerID)
		if err != nil {
			return err
		}
		spent, err := reconcileSpent(budget, existing)
		if err != nil {
			return err
		}
		if !bp.CanAfford(budget.Budget, spent, item.Cost) {
			return fmt.Errorf("%w: 需要 %d BP,只剩 %d BP",
				ErrInsufficientBP, item.Cost, bp.Remaining(budget.Budget, spent))
		}

		sel, err := r.InsertSelection(ctx, NewSelection{
			MatchID:    m.ID,
			PlayerID:   p.PlayerID,
			ItemID:     item.ID,
			Cost:       item.Cost,
			TargetNote: note,
		})
		if err != nil {
			return err
		}
		// spent 是覆寫而不是累加:權威值永遠是「未作廢選擇的 cost 總和」,
		// 這裡把剛成立的這一筆加進剛剛核對過的總和,兩者一致才寫回去。
		if err := r.SetSpent(ctx, m.ID, p.PlayerID, spent+item.Cost); err != nil {
			return err
		}

		budget.Spent = spent + item.Cost
		out = SelectResult{
			Selection: *sel,
			Budget:    *budget,
			Advisory:  advisoryFor(item.Category, existing),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// VoidSelection 退掉一項,BP 退回預算。
//
// 只在封盤前可行 —— 封盤後對手已經照著這份清單準備了,這時候抽掉一項
// 等於改了比賽條件。退費純粹是 BP 內部的事(標 voided + 改 spent),不經 Ledger。
func (s *Service) VoidSelection(ctx context.Context, p VoidParams) (*Budget, error) {
	switch {
	case p.SelectionPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 selection_public_id", ErrInvalidRequest)
	case p.PlayerID <= 0:
		return nil, fmt.Errorf("%w: 缺少選手身分", ErrInvalidRequest)
	}

	var out Budget
	err := s.repo.WithTx(ctx, func(ctx context.Context, r Repository) error {
		res, err := s.voidSelection(ctx, r, p.SelectionPublicID,
			voidAuthority{ownerID: p.PlayerID})
		if err != nil {
			return err
		}
		out = res.Budget
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RefundSelection 是**裁判代退**一筆選擇,BP 退回該場預算。
//
// 存在的理由是目錄裡的規則本身:「禁用奇術」的 referee_note 寫著「若同時買了
// 禁用任意奇術,將經裁判確認後返還禁用任意奇術的 BP」。那句話沒有 VoidSelection
// 可走 —— 它只認選擇的本人,而裁判不是本人。
//
// 與 VoidSelection 的差別**只有擁有者檢查**。其餘三件事一字不差:
//
//	封盤後拒絕    封盤不可逆是既有定案,裁判也不例外(對手已經照那份清單準備了)
//	不經 Ledger   BP 不是代幣,退點是標 voided + 改 spent,沒有冪等鍵
//	spent 重算    恆等於未作廢選擇的 cost 總和,由 reconcileSpent 核對
//
// 稽核紀錄不在這裡寫:本套件只認標準庫與 activity/bp,寫 admin_audit_logs 的
// 權威在 match.Service(它與這一步同 tx)。
func (s *Service) RefundSelection(ctx context.Context, selectionPublicID string) (*RefundResult, error) {
	if selectionPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 selection_public_id", ErrInvalidRequest)
	}
	var out RefundResult
	err := s.repo.WithTx(ctx, func(ctx context.Context, r Repository) error {
		res, err := s.voidSelection(ctx, r, selectionPublicID,
			voidAuthority{referee: true})
		if err != nil {
			return err
		}
		out = *res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// voidAuthority 說明這次退選憑什麼成立。
//
// 刻意不用「ownerID 為 0 就跳過檢查」那種哨兵值:授權方向的預設值一旦是
// 放行,任何取不到身分而回 0 的新呼叫端都會靜默失去擁有者檢查。這裡的零值
// 是「自助退選但沒帶身分」,那是無效請求,會被擋下來。
type voidAuthority struct {
	// ownerID 是自助退選者的 id,必須與選擇的擁有者相符。
	ownerID int64
	// referee 為 true 表示裁判代退,不檢查擁有者。把關在別處:
	// transport 的 requireJudge + authz.PermTournamentJudge。在這裡多擺一個
	// 「裁判是誰」的參數只會讓本套件對權限有意見,而它沒有資格有意見。
	referee bool
}

// voidSelection 是退選的唯一實作,選手自助與裁判代退共用。
//
// 呼叫端必須已經在 tx 裡(r 是綁在該 tx 上的 Repository)。
func (s *Service) voidSelection(
	ctx context.Context, r Repository, selectionPublicID string, a voidAuthority,
) (*RefundResult, error) {
	// 先無鎖讀一次只為了知道它屬於哪一場,取得序列化點之後再重讀一次 ——
	// 兩次之間可能有另一個請求把它退掉了,以鎖後那次為準。
	sel, err := r.GetSelection(ctx, selectionPublicID)
	if err != nil {
		return nil, err
	}
	m, err := r.LockMatch(ctx, sel.MatchPublicID)
	if err != nil {
		return nil, err
	}
	sel, err = r.GetSelection(ctx, selectionPublicID)
	if err != nil {
		return nil, err
	}
	switch {
	case a.referee:
		// 裁判代退:不比對擁有者。
	case a.ownerID <= 0:
		return nil, fmt.Errorf("%w: 缺少選手身分", ErrInvalidRequest)
	case sel.PlayerID != a.ownerID:
		return nil, ErrNotSelectionOwner
	}
	if isLocked(m) {
		return nil, ErrHandicapLocked
	}
	if sel.Voided {
		return nil, ErrSelectionAlreadyVoided
	}

	// 預算查的是**選擇的擁有者**,不是請求者:裁判代退時兩者不同,
	// 而 BP 永遠只退回當初花掉它的那個人的預算。
	budget, err := r.GetBudget(ctx, m.ID, sel.PlayerID)
	if err != nil {
		return nil, err
	}
	existing, err := r.ListSelections(ctx, m.ID, sel.PlayerID)
	if err != nil {
		return nil, err
	}
	spent, err := reconcileSpent(budget, existing)
	if err != nil {
		return nil, err
	}

	if err := r.MarkSelectionVoided(ctx, sel.ID); err != nil {
		return nil, err
	}
	if err := r.SetSpent(ctx, m.ID, sel.PlayerID, spent-sel.Cost); err != nil {
		return nil, err
	}
	budget.Spent = spent - sel.Cost
	sel.Voided = true
	return &RefundResult{
		MatchID:       m.ID,
		MatchPublicID: m.PublicID,
		Selection:     *sel,
		Budget:        *budget,
	}, nil
}

// Lock 封盤。**不可逆**,回傳封盤後的完整清單(此時已對所有人公開)。
//
// 重複封盤回 ErrAlreadyLocked 而不是默默成功:封盤同時會觸發 Discord 公告,
// 默默成功等於每呼叫一次就再公告一次。
//
// 封盤 UPDATE 之後、回傳清單之前,對持有者每一筆要抽的選擇抽一次(drawAtLock);
// 抽不出來整個封盤失敗。回傳的 Selections 因此帶著 DrawResult —— 公示是完整的。
func (s *Service) Lock(ctx context.Context, matchPublicID string) (*MatchHandicaps, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	var out MatchHandicaps
	err := s.repo.WithTx(ctx, func(ctx context.Context, r Repository) error {
		m, err := r.LockMatch(ctx, matchPublicID)
		if err != nil {
			return err
		}
		if isLocked(m) {
			return ErrAlreadyLocked
		}
		if m.Status != statusReady {
			return ErrHandicapClosed
		}
		at := s.now().UTC()
		if err := r.LockHandicaps(ctx, m.ID, at); err != nil {
			return err
		}

		// 就地把記憶體中的場次推進到封盤後的樣子,再組公開檢視 ——
		// 重新查一次只會拿到同一份資料,而且要多一趟往返。
		m.LockedAt = &at
		m.HandicapOpen = false
		m.Status = statusLocked
		if err := s.drawAtLock(ctx, r, m, at); err != nil {
			return err
		}
		view, err := s.buildMatchHandicaps(ctx, r, m, viewer{})
		if err != nil {
			return err
		}
		out = *view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// drawAtLock 對持有者每一筆未作廢、要抽、且尚未抽過的選擇抽一次並寫回。
//
// 呼叫端已經在場次鎖下、封盤 UPDATE 之後。at 是封盤時間,drawn_at 寫**同一個值**
// (不是再取一次時鐘):「抽選時間 = 封盤時間」是語意,不該靠時鐘巧合。
//
// 已抽過的(DrawResult 非 nil)在記憶體裡跳過;真正的「只抽一次」由
// SetSelectionDraw 的 draw_result IS NULL 守 —— 0 列是 ErrAlreadyDrawn,不靜靜跳過。
func (s *Service) drawAtLock(ctx context.Context, r Repository, m *Match, at time.Time) error {
	budget, err := r.FindMatchBudget(ctx, m.ID)
	if errors.Is(err, ErrNoBudget) {
		return nil // 本場無讓武,沒有東西要抽
	}
	if err != nil {
		return err
	}
	sels, err := r.ListSelections(ctx, m.ID, budget.PlayerID)
	if err != nil {
		return err
	}

	// 池只在真的有人買了要抽池的項目時才去讀:大多數場次不需要,
	// 而且讀 config 的失敗不該擋住一場沒有「隨機武學」的封盤。
	var pools DrawPools
	for _, sel := range sels {
		if sel.Voided || sel.ItemParams.Draw == "" || sel.DrawResult != nil {
			continue
		}
		if sel.ItemParams.Draw == DrawWuxue && pools == nil {
			if pools, err = r.DrawPools(ctx, m.TournamentID); err != nil {
				return err
			}
		}
		result, err := s.draw(sel, pools)
		if err != nil {
			return err
		}
		if err := r.SetSelectionDraw(ctx, sel.ID, result, at); err != nil {
			return err
		}
	}
	return nil
}

// ── 查詢 ────────────────────────────────────────────────────────

// MyBudget 回傳請求者在某場的預算與已選項目。
//
// 沒有預算不是錯誤:同段對決與高段位方本來就沒有,此時 HasBudget = false
// 且 LockedReason 說明原因,前端顯示「本場無讓武」。
func (s *Service) MyBudget(ctx context.Context, matchPublicID string, playerID int64) (*MyBudget, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if playerID <= 0 {
		return nil, fmt.Errorf("%w: 缺少選手身分", ErrInvalidRequest)
	}
	m, err := s.repo.GetMatch(ctx, matchPublicID)
	if err != nil {
		return nil, err
	}
	budget, err := s.repo.GetBudget(ctx, m.ID, playerID)
	if errors.Is(err, ErrNoBudget) {
		return &MyBudget{Editable: false, LockedReason: "本場沒有讓武 —— 同段對決,或你是段位較高的一方"}, nil
	}
	if err != nil {
		return nil, err
	}
	sels, err := s.repo.ListSelections(ctx, m.ID, playerID)
	if err != nil {
		return nil, err
	}
	out := &MyBudget{HasBudget: true, Budget: *budget, Selections: sels}
	out.Editable = editable(m)
	if !out.Editable {
		out.LockedReason = lockedReason(m)
	}
	return out, nil
}

// MatchHandicaps 回傳一場比賽的讓武全貌,**依請求者身分決定回傳內容**。
//
// viewerPlayerID 為 0 表示匿名觀眾。封盤前只有施加者本人看得到自己買的;
// 封盤後雙方與觀眾都看得到全部。這條界線在這裡守,不是前端隱藏。
func (s *Service) MatchHandicaps(ctx context.Context, matchPublicID string, viewerPlayerID int64) (*MatchHandicaps, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	m, err := s.repo.GetMatch(ctx, matchPublicID)
	if err != nil {
		return nil, err
	}
	return s.buildMatchHandicaps(ctx, s.repo, m, viewer{playerID: viewerPlayerID})
}

// RefereeMatchHandicaps 是**裁判端**的讓武全貌:封盤前也看得到內容。
//
// 為什麼要繞過那條界線:裁判要執行的規則就寫在買下的項目裡(如「禁用奇術」的
// referee_note 要求裁判確認後返還另一項的 BP),而封盤前看不到內容的話,
// 那些規則在場上沒有人執行得了 —— 裁判的 viewerPlayerID 恆為 0,
// 走 MatchHandicaps 拿到的永遠是空清單。
//
// 它不改 Revealed:Revealed 的意思是「已封盤、對所有人公開」,不是
// 「這次呼叫看得到內容」。裁判看得到不代表公眾看得到,兩者混成一個布林值,
// 前端就會拿它去決定要不要把內容貼出去。
//
// 誰可以呼叫由 transport 的裁判權限把關(authz.PermTournamentJudge)。
// 本套件不認識「裁判」這個身分 —— 它只接受「這次要不要套用揭露限制」。
func (s *Service) RefereeMatchHandicaps(ctx context.Context, matchPublicID string) (*MatchHandicaps, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	m, err := s.repo.GetMatch(ctx, matchPublicID)
	if err != nil {
		return nil, err
	}
	return s.buildMatchHandicaps(ctx, s.repo, m, viewer{referee: true})
}

// viewer 是「誰在看」,只決定封盤前的揭露程度。
//
// 做成一個型別而不是多一個 bool 參數:呼叫點會變成 build(..., 0, true) 這種
// 沒有人讀得懂的東西,而這裡每加一個揭露條件都是在動可見性界線 ——
// 那正是最該讓呼叫點自己說清楚的地方。
type viewer struct {
	// playerID 0 = 匿名觀眾。
	playerID int64
	// referee = 裁判端檢視,不套用封盤前的揭露限制。
	referee bool
}

func (s *Service) buildMatchHandicaps(ctx context.Context, r Repository, m *Match, v viewer) (*MatchHandicaps, error) {
	out := &MatchHandicaps{
		MatchPublicID: m.PublicID,
		Status:        m.Status,
		LockedAt:      m.LockedAt,
		Revealed:      isLocked(m),
	}

	budget, err := r.FindMatchBudget(ctx, m.ID)
	if errors.Is(err, ErrNoBudget) {
		return out, nil // 本場無讓武:雙方同段,沒有人拿到 BP
	}
	if err != nil {
		return nil, err
	}
	out.HolderPlayerPublicID = budget.PlayerPublicID
	out.ConstrainedPlayerPublicID = m.P1PlayerPublicID
	if budget.PlayerID == m.P1PlayerID {
		out.ConstrainedPlayerPublicID = m.P2PlayerPublicID
	}

	// 封盤前,除了施加者本人與裁判一律看不到內容 —— 連花了多少都不給,
	// 那會反推出對方還剩幾 BP,等於提前洩漏還會被加上什麼限制。
	if !out.Revealed && !v.referee && v.playerID != budget.PlayerID {
		return out, nil
	}
	sels, err := r.ListSelections(ctx, m.ID, budget.PlayerID)
	if err != nil {
		return nil, err
	}
	out.Budget = budget
	out.Selections = sels
	return out, nil
}

// ── 內部判斷 ────────────────────────────────────────────────────

// isLocked 回報讓武是否已經收掉。
//
// 兩個條件任一成立即為已封盤:時間戳非空,或場次已經走過 ready。
// 看兩個而不是只看時間戳,是因為場次可能因不戰而勝之類的原因直接跳到 done ——
// 那時候沒有 handicap_locked_at,但顯然也不能再買。
func isLocked(m *Match) bool {
	if m.LockedAt != nil {
		return true
	}
	switch m.Status {
	case statusLocked, statusLive, statusDone:
		return true
	default:
		return false
	}
}

// requireSelectable 檢查場次是否處於可選購狀態。
func requireSelectable(m *Match) error {
	if isLocked(m) {
		return ErrHandicapLocked
	}
	if m.Status != statusReady {
		return ErrHandicapClosed
	}
	if !m.HandicapOpen {
		return ErrHandicapNotOpen
	}
	return nil
}

// editable 回報現在還能不能增減讓武。
//
// 刻意轉呼叫 requireSelectable 而不是把條件再寫一次:
// 「能不能改」只能有一個權威,兩份條件遲早會有一份被改漏。
func editable(m *Match) bool { return requireSelectable(m) == nil }

// lockedReason 是給選手看的人話說明。
func lockedReason(m *Match) string {
	switch {
	case isLocked(m):
		return "讓武已封盤,內容已公開,不可再更動"
	case m.Status == statusPending:
		return "對手尚未確定,要等上一輪打完"
	case !m.HandicapOpen:
		return "裁判尚未開盤,請稍候"
	default:
		return "此場次目前不開放讓武選購"
	}
}

// reconcileSpent 重算未作廢選擇的 cost 總和,並與 match_budgets.spent 核對。
//
// 對不上時整個動作失敗(ErrBudgetInconsistent),不自動修正:
// spent 是衍生資料,對不上代表寫入路徑有 bug,而繼續算下去只會讓錯誤擴散到
// 下一次購買的餘額檢查。這是 schemas/20 待確認 ② 要求的那條驗證,
// 寫在每次寫入的路徑上而不是只寫在測試裡。
func reconcileSpent(b *Budget, sels []Selection) (int64, error) {
	var sum int64
	for _, s := range sels {
		sum += s.Cost
	}
	if sum != b.Spent {
		return 0, fmt.Errorf("%w: spent=%d 選擇總和=%d(match=%d player=%d)",
			ErrBudgetInconsistent, b.Spent, sum, b.MatchID, b.PlayerID)
	}
	return sum, nil
}

// advisoryFor 決定要不要附上非阻擋式提醒。
//
// 目前唯一會觸發的情況:victory 類已經有一項,現在又買一項。
// 兩項同時生效可能互相矛盾(「撐過 90 秒即獲勝」+「時間結束即勝利」),
// 但**不擋** —— 系統列不完所有矛盾組合,擋一半會讓選手以為沒被擋的都相容。
func advisoryFor(c Category, existing []Selection) string {
	if c != CategoryVictory {
		return ""
	}
	for _, s := range existing {
		if s.Category == CategoryVictory {
			return AdvisoryVictoryConflict
		}
	}
	return ""
}
