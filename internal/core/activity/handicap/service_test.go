package handicap

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
)

const (
	testTournamentID = int64(1)
	testMatchID      = int64(100)
	testMatchPub     = "M0001"
	p1ID             = int64(11)
	p2ID             = int64(12)
)

var testClock = func() time.Time { return time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC) }

type fixture struct {
	repo *fakeRepo
	svc  *Service
	// holder 是本場拿到 BP 的一方;無讓武時為 0。
	holder int64
	budget int64
}

// newFixture 建一場 p1 vs p2 的比賽,狀態 ready、已開盤、項目已安裝、預算已發。
func newFixture(t *testing.T, p1Rank, p2Rank bp.Rank) *fixture {
	t.Helper()
	return newFixtureWithGap(t, p1Rank, p2Rank, bp.DefaultPerRankGap)
}

// newFixtureWithGap 同上,但可指定 config.bp_per_rank_gap —— 逐屆可調正是它存在的理由。
func newFixtureWithGap(t *testing.T, p1Rank, p2Rank bp.Rank, perGap int64) *fixture {
	t.Helper()
	repo := newFakeRepo()
	repo.matches[testMatchPub] = &Match{
		ID:               testMatchID,
		PublicID:         testMatchPub,
		TournamentID:     testTournamentID,
		Status:           statusReady,
		HandicapOpen:     true,
		P1PlayerID:       p1ID,
		P2PlayerID:       p2ID,
		P1PlayerPublicID: playerPublicID(p1ID),
		P2PlayerPublicID: playerPublicID(p2ID),
		P1Rank:           p1Rank,
		P2Rank:           p2Rank,
		PerRankGap:       perGap,
	}
	svc := New(repo, WithClock(testClock))
	if _, err := svc.InstallSeedItems(context.Background(), testTournamentID); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	f := &fixture{repo: repo, svc: svc}
	b, err := svc.GrantBudget(context.Background(), testMatchPub)
	if err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	if b != nil {
		f.holder, f.budget = b.PlayerID, b.Budget
	}
	return f
}

func (f *fixture) ref(t *testing.T, c Category, name string) string {
	t.Helper()
	for _, it := range f.repo.items {
		if it.Category == c && it.Name == name {
			return it.Ref
		}
	}
	t.Fatalf("seed 裡沒有項目 %s/%s", c, name)
	return ""
}

func (f *fixture) buy(t *testing.T, c Category, name, note string) *SelectResult {
	t.Helper()
	res, err := f.svc.Select(context.Background(), SelectParams{
		MatchPublicID: testMatchPub,
		PlayerID:      f.holder,
		ItemRef:       f.ref(t, c, name),
		TargetNote:    note,
	})
	if err != nil {
		t.Fatalf("買 %s 失敗: %v", name, err)
	}
	f.assertSpentInvariant(t)
	return res
}

// assertSpentInvariant 驗證 schemas/20 待確認 ② 要求的恆等式:
// match_budgets.spent == SUM(cost) WHERE NOT voided。每個寫入動作後都要成立。
func (f *fixture) assertSpentInvariant(t *testing.T) {
	t.Helper()
	if f.holder == 0 {
		return
	}
	b, err := f.repo.GetBudget(context.Background(), testMatchID, f.holder)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if sum := f.repo.sumSelections(testMatchID, f.holder); b.Spent != sum {
		t.Fatalf("spent = %d,未作廢選擇總和 = %d,兩者必須恆等", b.Spent, sum)
	}
	if b.Spent > b.Budget {
		t.Fatalf("spent = %d 超過 budget = %d", b.Spent, b.Budget)
	}
}

// ── 發預算 ──────────────────────────────────────────────────────

func TestGrantBudget_OnlyLowerRankGetsRow(t *testing.T) {
	t.Run("低段位在 p1", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		if f.holder != p1ID || f.budget != 24 {
			t.Fatalf("holder=%d budget=%d,期望 p1 取得 24 BP", f.holder, f.budget)
		}
		if _, err := f.repo.GetBudget(context.Background(), testMatchID, p2ID); !errors.Is(err, ErrNoBudget) {
			t.Fatal("高段位方不該有預算列")
		}
	})

	t.Run("低段位在 p2", func(t *testing.T) {
		f := newFixture(t, bp.RankWuwo, bp.RankDuanshui)
		if f.holder != p2ID || f.budget != 16 {
			t.Fatalf("holder=%d budget=%d,期望 p2 取得 16 BP", f.holder, f.budget)
		}
	})

	t.Run("同段不建列", func(t *testing.T) {
		f := newFixture(t, bp.RankFeihua, bp.RankFeihua)
		if f.holder != 0 {
			t.Fatalf("同段對決不該有人拿到 BP,實際 holder=%d", f.holder)
		}
		if len(f.repo.budgets) != 0 {
			t.Fatalf("同段對決不該建任何預算列,實際 %d 列", len(f.repo.budgets))
		}
	})
}

func TestGrantBudget_RepeatIsIdempotent(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankFeihua)
	b, err := f.svc.GrantBudget(context.Background(), testMatchPub)
	if err != nil {
		t.Fatalf("重複發預算應該是安全的: %v", err)
	}
	if b.Budget != 16 || len(f.repo.budgets) != 1 {
		t.Fatalf("重複發預算不該多建列或改金額: budget=%d 列數=%d", b.Budget, len(f.repo.budgets))
	}
}

// 抽籤後改段位會讓已發的預算對不上。這種情況必須失敗出聲,不能默默覆蓋。
func TestGrantBudget_RankChangedAfterGrantConflicts(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankFeihua)
	f.repo.matches[testMatchPub].P2Rank = bp.RankWuwo
	_, err := f.svc.GrantBudget(context.Background(), testMatchPub)
	if !errors.Is(err, ErrBudgetConflict) {
		t.Fatalf("期望 ErrBudgetConflict,實際 %v", err)
	}
}

func TestGrantBudget_Rejections(t *testing.T) {
	base := func() *fakeRepo {
		repo := newFakeRepo()
		repo.matches[testMatchPub] = &Match{
			ID: testMatchID, PublicID: testMatchPub, TournamentID: testTournamentID,
			Status: statusPending, P1PlayerID: p1ID, P2PlayerID: p2ID,
			P1Rank: bp.RankKaishan, P2Rank: bp.RankWuwo, PerRankGap: bp.DefaultPerRankGap,
		}
		return repo
	}

	t.Run("對手未定", func(t *testing.T) {
		repo := base()
		repo.matches[testMatchPub].P2PlayerID = 0
		_, err := New(repo).GrantBudget(context.Background(), testMatchPub)
		if !errors.Is(err, ErrPlayersNotSet) {
			t.Fatalf("期望 ErrPlayersNotSet,實際 %v", err)
		}
	})

	t.Run("尚未評段", func(t *testing.T) {
		repo := base()
		repo.matches[testMatchPub].P2Rank = bp.RankUnspecified
		_, err := New(repo).GrantBudget(context.Background(), testMatchPub)
		if !errors.Is(err, bp.ErrUnranked) {
			t.Fatalf("期望 bp.ErrUnranked,實際 %v", err)
		}
	})

	t.Run("已封盤", func(t *testing.T) {
		repo := base()
		repo.matches[testMatchPub].Status = statusLocked
		_, err := New(repo).GrantBudget(context.Background(), testMatchPub)
		if !errors.Is(err, ErrHandicapLocked) {
			t.Fatalf("期望 ErrHandicapLocked,實際 %v", err)
		}
	})

	t.Run("場次不存在", func(t *testing.T) {
		_, err := New(newFakeRepo()).GrantBudget(context.Background(), "沒這場")
		if !errors.Is(err, ErrMatchNotFound) {
			t.Fatalf("期望 ErrMatchNotFound,實際 %v", err)
		}
	})
}

// ── 選購 ────────────────────────────────────────────────────────

// 同一項目重複購買是設計,不是 bug(09-12 定案)。
func TestSelect_SameItemRepeatedly(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo) // 24 BP
	for i := 1; i <= 3; i++ {
		res := f.buy(t, CategoryDefense, "禁用迴避", "") // 8 BP
		if res.Budget.Spent != int64(8*i) {
			t.Fatalf("第 %d 次購買後 spent = %d,期望 %d", i, res.Budget.Spent, 8*i)
		}
		if res.Advisory != "" {
			t.Fatalf("非 victory 類不該有 advisory,實際 %q", res.Advisory)
		}
	}
	sels, _ := f.repo.ListSelections(context.Background(), testMatchID, f.holder)
	if len(sels) != 3 {
		t.Fatalf("重複購買應產生 3 列,實際 %d 列", len(sels))
	}
	// 每一列都是獨立的價格快照,不是一列 qty=3。
	for _, s := range sels {
		if s.Cost != 8 || s.ItemName != "禁用迴避" {
			t.Fatalf("非預期的選擇內容: %+v", s)
		}
	}
}

// 剛好花光的邊界:bp.CanAfford 用的是 <=,寫成 < 只會在這裡現形。
func TestSelect_ExactlySpendsBudget(t *testing.T) {
	f := newFixture(t, bp.RankFeihua, bp.RankWuwo) // 8 BP
	res := f.buy(t, CategoryDefense, "禁用迴避", "")   // 8 BP
	if res.Budget.Remaining() != 0 {
		t.Fatalf("剛好花光後 remaining = %d,期望 0", res.Budget.Remaining())
	}

	// 花光之後連 1 BP 的項目都買不起。
	_, err := f.svc.Select(context.Background(), SelectParams{
		MatchPublicID: testMatchPub, PlayerID: f.holder,
		ItemRef: f.ref(t, CategoryRule, "指定對手外觀"), TargetNote: "全身白衣",
	})
	if !errors.Is(err, ErrInsufficientBP) {
		t.Fatalf("期望 ErrInsufficientBP,實際 %v", err)
	}
	f.assertSpentInvariant(t)
}

func TestSelect_OverBudgetRejectedAndNothingWritten(t *testing.T) {
	f := newFixture(t, bp.RankFeihua, bp.RankWuwo) // 8 BP
	_, err := f.svc.Select(context.Background(), SelectParams{
		MatchPublicID: testMatchPub, PlayerID: f.holder,
		ItemRef: f.ref(t, CategoryWeapon, "指定對手武學"), TargetNote: "太虛劍意", // 20 BP
	})
	if !errors.Is(err, ErrInsufficientBP) {
		t.Fatalf("期望 ErrInsufficientBP,實際 %v", err)
	}
	sels, _ := f.repo.ListSelections(context.Background(), testMatchID, f.holder)
	if len(sels) != 0 {
		t.Fatalf("被擋下的購買不該留下任何列,實際 %d 列", len(sels))
	}
	f.assertSpentInvariant(t)
}

// victory 類第二項要回 advisory,但**購買仍然成立** —— 互斥由裁判判,不由系統擋。
func TestSelect_VictorySecondItemAdvisesButStillSucceeds(t *testing.T) {
	// 買三項 victory 要 44 BP,所以這一屆的 bp_per_rank_gap 調成 20(差三段 = 60 BP)。
	f := newFixtureWithGap(t, bp.RankKaishan, bp.RankWuwo, 20)

	first := f.buy(t, CategoryVictory, "時間結束即勝利", "") // 10 BP
	if first.Advisory != "" {
		t.Fatalf("victory 第一項不該有 advisory,實際 %q", first.Advisory)
	}

	second := f.buy(t, CategoryVictory, "打出守關元即勝利", "") // 10 BP
	if second.Advisory != AdvisoryVictoryConflict {
		t.Fatalf("victory 第二項應回 advisory,實際 %q", second.Advisory)
	}
	if second.Selection.PublicID == "" {
		t.Fatal("回了 advisory 仍然必須成功購買")
	}
	if second.Budget.Spent != 20 {
		t.Fatalf("spent = %d,期望 20", second.Budget.Spent)
	}

	// 第三項一樣照買。
	third := f.buy(t, CategoryVictory, "撐過 90 秒即獲勝", "")
	_ = third
	sels, _ := f.repo.ListSelections(context.Background(), testMatchID, f.holder)
	if len(sels) != 3 {
		t.Fatalf("victory 類應累積 3 列,實際 %d 列", len(sels))
	}
}

// 同一類但不同項目之外,其他類買第二項不該有 advisory。
func TestSelect_NonVictoryCategoryNeverAdvises(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	f.buy(t, CategoryDefense, "禁用卸勢", "")
	res := f.buy(t, CategoryDefense, "禁用防禦", "")
	if res.Advisory != "" {
		t.Fatalf("defense 類不該有 advisory,實際 %q", res.Advisory)
	}
}

func TestSelect_OnlyBudgetHolderCanBuy(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo) // holder = p1
	for _, who := range []struct {
		name string
		id   int64
	}{
		{"高段位的對手", p2ID},
		{"完全不相干的選手", int64(999)},
	} {
		t.Run(who.name, func(t *testing.T) {
			_, err := f.svc.Select(context.Background(), SelectParams{
				MatchPublicID: testMatchPub, PlayerID: who.id,
				ItemRef: f.ref(t, CategoryDefense, "禁跳躍"),
			})
			if !errors.Is(err, ErrNoBudget) {
				t.Fatalf("期望 ErrNoBudget,實際 %v", err)
			}
		})
	}
}

func TestSelect_TargetNote(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)

	t.Run("需要指定內容卻留空", func(t *testing.T) {
		_, err := f.svc.Select(context.Background(), SelectParams{
			MatchPublicID: testMatchPub, PlayerID: f.holder,
			ItemRef: f.ref(t, CategoryWeapon, "指定對手武學"),
		})
		if !errors.Is(err, ErrTargetNoteRequired) {
			t.Fatalf("期望 ErrTargetNoteRequired,實際 %v", err)
		}
	})

	t.Run("只填空白等同留空", func(t *testing.T) {
		_, err := f.svc.Select(context.Background(), SelectParams{
			MatchPublicID: testMatchPub, PlayerID: f.holder,
			ItemRef: f.ref(t, CategoryWeapon, "指定對手武學"), TargetNote: "  \n\t ",
		})
		if !errors.Is(err, ErrTargetNoteRequired) {
			t.Fatalf("期望 ErrTargetNoteRequired,實際 %v", err)
		}
	})

	t.Run("填了就成立且去頭尾空白", func(t *testing.T) {
		res := f.buy(t, CategoryWeapon, "指定對手武學", "  太虛劍意  ")
		if res.Selection.TargetNote != "太虛劍意" {
			t.Fatalf("target_note = %q,期望去掉頭尾空白", res.Selection.TargetNote)
		}
	})

	t.Run("不需要指定內容的項目留空也成立", func(t *testing.T) {
		res := f.buy(t, CategoryDefense, "禁跳躍", "")
		if res.Selection.TargetNote != "" {
			t.Fatalf("target_note = %q,期望空字串", res.Selection.TargetNote)
		}
	})

	t.Run("過長被擋", func(t *testing.T) {
		_, err := f.svc.Select(context.Background(), SelectParams{
			MatchPublicID: testMatchPub, PlayerID: f.holder,
			ItemRef:    f.ref(t, CategoryWeapon, "指定對手武學"),
			TargetNote: strings.Repeat("霸", maxTargetNoteRunes+1),
		})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("期望 ErrInvalidRequest,實際 %v", err)
		}
	})
}

func TestSelect_ItemFromAnotherTournamentRejected(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	// 另一屆也安裝了同一份 seed,價格逐屆可調 —— 拿別屆的項目買這場必須失敗。
	if _, err := f.svc.InstallSeedItems(context.Background(), int64(2)); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	var otherRef string
	for _, it := range f.repo.items {
		if it.TournamentID == 2 && it.Name == "禁跳躍" {
			otherRef = it.Ref
		}
	}
	_, err := f.svc.Select(context.Background(), SelectParams{
		MatchPublicID: testMatchPub, PlayerID: f.holder, ItemRef: otherRef,
	})
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("期望 ErrItemNotFound,實際 %v", err)
	}
}

func TestSelect_MatchStateGates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m *Match)
		wantErr error
	}{
		{"對手未齊", func(m *Match) { m.Status = statusPending }, ErrHandicapClosed},
		{"裁判尚未開盤", func(m *Match) { m.HandicapOpen = false }, ErrHandicapNotOpen},
		{"已封盤", func(m *Match) { m.Status = statusLocked }, ErrHandicapLocked},
		{"比賽中", func(m *Match) { m.Status = statusLive }, ErrHandicapLocked},
		{"已分勝負", func(m *Match) { m.Status = statusDone }, ErrHandicapLocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
			tc.mutate(f.repo.matches[testMatchPub])
			_, err := f.svc.Select(context.Background(), SelectParams{
				MatchPublicID: testMatchPub, PlayerID: f.holder,
				ItemRef: f.ref(t, CategoryDefense, "禁跳躍"),
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v,實際 %v", tc.wantErr, err)
			}
		})
	}
}

func TestSelect_InvalidParams(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	cases := []SelectParams{
		{PlayerID: f.holder, ItemRef: "1"},
		{MatchPublicID: testMatchPub, PlayerID: f.holder},
		{MatchPublicID: testMatchPub, ItemRef: "1"},
	}
	for _, p := range cases {
		if _, err := f.svc.Select(context.Background(), p); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%+v 期望 ErrInvalidRequest,實際 %v", p, err)
		}
	}
}

// ── 退選 ────────────────────────────────────────────────────────

func TestVoidSelection_RefundsBP(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo) // 24 BP
	a := f.buy(t, CategoryDefense, "禁用迴避", "")      // 8
	f.buy(t, CategoryDefense, "禁用解控", "")           // 8

	b, err := f.svc.VoidSelection(context.Background(), VoidParams{
		SelectionPublicID: a.Selection.PublicID, PlayerID: f.holder,
	})
	if err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	if b.Spent != 8 || b.Remaining() != 16 {
		t.Fatalf("退選後 spent=%d remaining=%d,期望 8 / 16", b.Spent, b.Remaining())
	}
	f.assertSpentInvariant(t)

	// 退回來的 BP 要真的能再花出去。
	f.buy(t, CategorySkill, "禁用奇術", "") // 12 BP,退選前只剩 8 BP 買不起
}

func TestVoidSelection_Rejections(t *testing.T) {
	t.Run("不是本人買的", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		s := f.buy(t, CategoryDefense, "禁跳躍", "")
		_, err := f.svc.VoidSelection(context.Background(), VoidParams{
			SelectionPublicID: s.Selection.PublicID, PlayerID: p2ID,
		})
		if !errors.Is(err, ErrNotSelectionOwner) {
			t.Fatalf("期望 ErrNotSelectionOwner,實際 %v", err)
		}
	})

	t.Run("重複退", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		s := f.buy(t, CategoryDefense, "禁跳躍", "")
		p := VoidParams{SelectionPublicID: s.Selection.PublicID, PlayerID: f.holder}
		if _, err := f.svc.VoidSelection(context.Background(), p); err != nil {
			t.Fatalf("第一次退選應成功: %v", err)
		}
		_, err := f.svc.VoidSelection(context.Background(), p)
		if !errors.Is(err, ErrSelectionAlreadyVoided) {
			t.Fatalf("期望 ErrSelectionAlreadyVoided,實際 %v", err)
		}
		f.assertSpentInvariant(t)
	})

	t.Run("不存在", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		_, err := f.svc.VoidSelection(context.Background(), VoidParams{
			SelectionPublicID: "SEL9999", PlayerID: f.holder,
		})
		if !errors.Is(err, ErrSelectionNotFound) {
			t.Fatalf("期望 ErrSelectionNotFound,實際 %v", err)
		}
	})
}

// ── 封盤 ────────────────────────────────────────────────────────

func TestLock_IsIrreversibleAndFreezesEverything(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	s := f.buy(t, CategoryDefense, "禁用迴避", "")

	view, err := f.svc.Lock(context.Background(), testMatchPub)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !view.Revealed || view.LockedAt == nil || !view.LockedAt.Equal(testClock()) {
		t.Fatalf("封盤後應公開且有封盤時間: %+v", view)
	}
	if len(view.Selections) != 1 {
		t.Fatalf("封盤回傳應含全部選擇,實際 %d 列", len(view.Selections))
	}

	t.Run("重複封盤要出聲", func(t *testing.T) {
		if _, err := f.svc.Lock(context.Background(), testMatchPub); !errors.Is(err, ErrAlreadyLocked) {
			t.Fatalf("期望 ErrAlreadyLocked,實際 %v", err)
		}
	})

	t.Run("封盤後不能再買", func(t *testing.T) {
		_, err := f.svc.Select(context.Background(), SelectParams{
			MatchPublicID: testMatchPub, PlayerID: f.holder,
			ItemRef: f.ref(t, CategoryDefense, "禁跳躍"),
		})
		if !errors.Is(err, ErrHandicapLocked) {
			t.Fatalf("期望 ErrHandicapLocked,實際 %v", err)
		}
	})

	t.Run("封盤後不能退", func(t *testing.T) {
		_, err := f.svc.VoidSelection(context.Background(), VoidParams{
			SelectionPublicID: s.Selection.PublicID, PlayerID: f.holder,
		})
		if !errors.Is(err, ErrHandicapLocked) {
			t.Fatalf("期望 ErrHandicapLocked,實際 %v", err)
		}
	})
}

func TestLock_RejectsNotReadyMatch(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	f.repo.matches[testMatchPub].Status = statusPending
	if _, err := f.svc.Lock(context.Background(), testMatchPub); !errors.Is(err, ErrHandicapClosed) {
		t.Fatalf("期望 ErrHandicapClosed,實際 %v", err)
	}
}

// ── 可見性 ──────────────────────────────────────────────────────

// 封盤前只有施加者本人看得到;封盤後雙方與觀眾都看得到。這條界線由伺服器守。
func TestMatchHandicaps_VisibilityBoundary(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	f.buy(t, CategoryDefense, "禁用迴避", "")
	f.buy(t, CategoryVictory, "時間結束即勝利", "")

	t.Run("封盤前:施加者本人看得到全部", func(t *testing.T) {
		v, err := f.svc.MatchHandicaps(context.Background(), testMatchPub, f.holder)
		if err != nil {
			t.Fatalf("MatchHandicaps: %v", err)
		}
		if v.Revealed || len(v.Selections) != 2 || v.Budget == nil {
			t.Fatalf("本人應看得到自己的 2 項與預算: %+v", v)
		}
	})

	for _, viewer := range []struct {
		name string
		id   int64
	}{
		{"對手", p2ID},
		{"匿名觀眾", 0},
		{"別場的選手", 999},
	} {
		t.Run("封盤前:"+viewer.name+"看不到", func(t *testing.T) {
			v, err := f.svc.MatchHandicaps(context.Background(), testMatchPub, viewer.id)
			if err != nil {
				t.Fatalf("MatchHandicaps: %v", err)
			}
			if len(v.Selections) != 0 {
				t.Fatalf("封盤前不該看到任何選擇,實際 %d 列", len(v.Selections))
			}
			// 連花了多少都不給 —— 那會反推出對方還剩幾 BP。
			if v.Budget != nil {
				t.Fatalf("封盤前不該看到預算細節: %+v", v.Budget)
			}
			// 誰是施加者、誰被限制是公開資訊(段位本來就公開)。
			if v.HolderPlayerPublicID != playerPublicID(p1ID) ||
				v.ConstrainedPlayerPublicID != playerPublicID(p2ID) {
				t.Fatalf("施加者/受限方應為公開資訊: %+v", v)
			}
		})
	}

	if _, err := f.svc.Lock(context.Background(), testMatchPub); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	for _, viewer := range []struct {
		name string
		id   int64
	}{
		{"施加者", p1ID},
		{"對手", p2ID},
		{"匿名觀眾", 0},
	} {
		t.Run("封盤後:"+viewer.name+"看得到全部", func(t *testing.T) {
			v, err := f.svc.MatchHandicaps(context.Background(), testMatchPub, viewer.id)
			if err != nil {
				t.Fatalf("MatchHandicaps: %v", err)
			}
			if !v.Revealed || len(v.Selections) != 2 || v.Budget == nil {
				t.Fatalf("封盤後應全部公開: %+v", v)
			}
		})
	}
}

func TestMatchHandicaps_NoHandicapMatch(t *testing.T) {
	f := newFixture(t, bp.RankFeihua, bp.RankFeihua)
	v, err := f.svc.MatchHandicaps(context.Background(), testMatchPub, p1ID)
	if err != nil {
		t.Fatalf("MatchHandicaps: %v", err)
	}
	if v.HolderPlayerPublicID != "" || v.Budget != nil || len(v.Selections) != 0 {
		t.Fatalf("同段對決本場無讓武: %+v", v)
	}
}

// 退掉的選擇不該出現在封盤後的公開清單裡。
func TestMatchHandicaps_ExcludesVoided(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	a := f.buy(t, CategoryDefense, "禁用迴避", "")
	f.buy(t, CategoryDefense, "禁用解控", "")
	if _, err := f.svc.VoidSelection(context.Background(), VoidParams{
		SelectionPublicID: a.Selection.PublicID, PlayerID: f.holder,
	}); err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	v, err := f.svc.Lock(context.Background(), testMatchPub)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if len(v.Selections) != 1 || v.Selections[0].ItemName != "禁用解控" {
		t.Fatalf("已退掉的項目不該公開: %+v", v.Selections)
	}
}

// ── 自己的預算頁 ────────────────────────────────────────────────

func TestMyBudget(t *testing.T) {
	t.Run("持有者可編輯", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		f.buy(t, CategoryDefense, "禁跳躍", "")
		mb, err := f.svc.MyBudget(context.Background(), testMatchPub, f.holder)
		if err != nil {
			t.Fatalf("MyBudget: %v", err)
		}
		if !mb.HasBudget || !mb.Editable || mb.Budget.Remaining() != 22 || len(mb.Selections) != 1 {
			t.Fatalf("非預期的預算頁: %+v", mb)
		}
	})

	t.Run("高段位方沒有預算但不是錯誤", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		mb, err := f.svc.MyBudget(context.Background(), testMatchPub, p2ID)
		if err != nil {
			t.Fatalf("MyBudget: %v", err)
		}
		if mb.HasBudget || mb.Editable || mb.LockedReason == "" {
			t.Fatalf("應回「本場無讓武」而不是錯誤: %+v", mb)
		}
	})

	t.Run("尚未開盤說得出原因", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		f.repo.matches[testMatchPub].HandicapOpen = false
		mb, err := f.svc.MyBudget(context.Background(), testMatchPub, f.holder)
		if err != nil {
			t.Fatalf("MyBudget: %v", err)
		}
		if mb.Editable || !strings.Contains(mb.LockedReason, "開盤") {
			t.Fatalf("非預期的說明: %+v", mb)
		}
	})

	t.Run("封盤後不可編輯", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		if _, err := f.svc.Lock(context.Background(), testMatchPub); err != nil {
			t.Fatalf("Lock: %v", err)
		}
		mb, err := f.svc.MyBudget(context.Background(), testMatchPub, f.holder)
		if err != nil {
			t.Fatalf("MyBudget: %v", err)
		}
		if mb.Editable || !strings.Contains(mb.LockedReason, "封盤") {
			t.Fatalf("非預期的說明: %+v", mb)
		}
	})
}

// ── spent 恆等式 ────────────────────────────────────────────────

// 一連串買買退退之後,spent 仍然等於未作廢選擇的總和。
func TestSpentInvariant_ThroughMixedOperations(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo) // 24 BP
	ctx := context.Background()

	a := f.buy(t, CategoryDefense, "禁用迴避", "")   // +8  = 8
	b := f.buy(t, CategoryDefense, "禁用解控", "")   // +8  = 16
	f.buy(t, CategoryRule, "獲得先手權", "")          // +2  = 18
	f.buy(t, CategoryRule, "指定對手外觀", "全身白衣")     // +1  = 19
	f.buy(t, CategorySkill, "禁用任意奇術", "踏雪無痕")    // +3  = 22
	f.buy(t, CategoryRule, "對手必須在開賽前落下一句狠話", "") // +1  = 23

	for _, pub := range []string{a.Selection.PublicID, b.Selection.PublicID} {
		if _, err := f.svc.VoidSelection(ctx, VoidParams{SelectionPublicID: pub, PlayerID: f.holder}); err != nil {
			t.Fatalf("VoidSelection: %v", err)
		}
		f.assertSpentInvariant(t)
	}

	// 退回 16 BP 之後買得起 12 BP 的項目。
	f.buy(t, CategorySkill, "禁用奇術", "") // 12
	bud, _ := f.repo.GetBudget(ctx, testMatchID, f.holder)
	if bud.Spent != 19 {
		t.Fatalf("spent = %d,期望 19(23 − 8 − 8 + 12)", bud.Spent)
	}
}

// spent 與選擇總和對不上時必須整個動作失敗,不自動修正 ——
// 那代表寫入路徑有 bug,靜靜修好只會讓錯誤擴散到下一次餘額檢查。
func TestSelect_InconsistentSpentFailsLoudly(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	f.buy(t, CategoryDefense, "禁用迴避", "")
	f.repo.budgets[[2]int64{testMatchID, f.holder}].Spent = 3 // 人為破壞

	_, err := f.svc.Select(context.Background(), SelectParams{
		MatchPublicID: testMatchPub, PlayerID: f.holder,
		ItemRef: f.ref(t, CategoryDefense, "禁跳躍"),
	})
	if !errors.Is(err, ErrBudgetInconsistent) {
		t.Fatalf("期望 ErrBudgetInconsistent,實際 %v", err)
	}

	_, err = f.svc.VoidSelection(context.Background(), VoidParams{
		SelectionPublicID: "SEL0001", PlayerID: f.holder,
	})
	if !errors.Is(err, ErrBudgetInconsistent) {
		t.Fatalf("退選也應擋下來,實際 %v", err)
	}
}

// ── 併發 ────────────────────────────────────────────────────────

// 連點:同一個人同時送出多筆購買,總花費不得超過預算。
//
// fake 用一把鎖模擬 LockMatch 的列鎖,所以這裡驗的是「規則在序列化前提下正確」——
// 真正的隔離保證要用真 Postgres 測,那是 pg adapter 的責任。
func TestSelect_ConcurrentClicksCannotOverspend(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo) // 24 BP
	ref := f.ref(t, CategorySkill, "禁用奇術")          // 12 BP → 最多買 2 次

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Select(context.Background(), SelectParams{
				MatchPublicID: testMatchPub, PlayerID: f.holder, ItemRef: ref,
			})
			results[i] = err
		}()
	}
	wg.Wait()

	ok, insufficient := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInsufficientBP):
			insufficient++
		default:
			t.Fatalf("非預期的錯誤: %v", err)
		}
	}
	if ok != 2 || insufficient != attempts-2 {
		t.Fatalf("成功 %d 次、BP 不足 %d 次,期望 2 / %d", ok, insufficient, attempts-2)
	}
	f.assertSpentInvariant(t)
	if b, _ := f.repo.GetBudget(context.Background(), testMatchID, f.holder); b.Spent != 24 {
		t.Fatalf("spent = %d,期望剛好花光的 24", b.Spent)
	}
}

// 一邊買一邊封盤:封盤是不可逆的分界,不能有任何選擇落在它之後。
func TestSelect_ConcurrentWithLock(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	ref := f.ref(t, CategoryDefense, "禁跳躍") // 2 BP

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = f.svc.Lock(context.Background(), testMatchPub)
	}()
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Select(context.Background(), SelectParams{
				MatchPublicID: testMatchPub, PlayerID: f.holder, ItemRef: ref,
			})
			if err != nil && !errors.Is(err, ErrHandicapLocked) {
				t.Errorf("非預期的錯誤: %v", err)
			}
		}()
	}
	wg.Wait()

	f.assertSpentInvariant(t)
	m, _ := f.repo.GetMatch(context.Background(), testMatchPub)
	if m.LockedAt == nil {
		t.Fatal("封盤應該成功")
	}
	// 封盤之後的清單就是最終清單,不會再變。
	before, _ := f.repo.ListSelections(context.Background(), testMatchID, f.holder)
	_, err := f.svc.Select(context.Background(), SelectParams{
		MatchPublicID: testMatchPub, PlayerID: f.holder, ItemRef: ref,
	})
	if !errors.Is(err, ErrHandicapLocked) {
		t.Fatalf("期望 ErrHandicapLocked,實際 %v", err)
	}
	after, _ := f.repo.ListSelections(context.Background(), testMatchID, f.holder)
	if len(before) != len(after) {
		t.Fatalf("封盤後清單仍在變動: %d → %d", len(before), len(after))
	}
}

// ── seed 安裝 ───────────────────────────────────────────────────

func TestInstallSeedItems(t *testing.T) {
	repo := newFakeRepo()
	svc := New(repo)
	ctx := context.Background()

	n, err := svc.InstallSeedItems(ctx, testTournamentID)
	if err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	if n != 34 {
		t.Fatalf("首次安裝應建 34 項,實際 %d 項", n)
	}

	// 重跑不該多建列,也不該改價格。
	again, err := svc.InstallSeedItems(ctx, testTournamentID)
	if err != nil {
		t.Fatalf("重跑 InstallSeedItems: %v", err)
	}
	if again != 0 {
		t.Fatalf("重跑應新增 0 項,實際 %d 項", again)
	}

	items, err := svc.ListItems(ctx, testTournamentID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 34 {
		t.Fatalf("該屆應有 34 項,實際 %d 項", len(items))
	}

	// 逐屆一套:另一屆各自有自己的 34 列,價格可以各自調。
	if _, err := svc.InstallSeedItems(ctx, int64(2)); err != nil {
		t.Fatalf("第二屆 InstallSeedItems: %v", err)
	}
	if got, _ := svc.ListItems(ctx, int64(2)); len(got) != 34 {
		t.Fatalf("第二屆應有 34 項,實際 %d 項", len(got))
	}
}

func TestInstallSeedItems_RejectsMissingTournament(t *testing.T) {
	if _, err := New(newFakeRepo()).InstallSeedItems(context.Background(), 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("期望 ErrInvalidRequest,實際 %v", err)
	}
}
