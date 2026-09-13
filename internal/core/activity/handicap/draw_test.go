package handicap

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
)

// ── 方向鍵 ──────────────────────────────────────────────────────

// 結果格式固定:「上→下 下→左 左→右 右→上」—— 四段、空白分隔、順序上下左右,
// 每段原鍵→新鍵。前端與 Discord 公告直接貼這個字串,格式一變兩邊都要改。
var directionResultPattern = regexp.MustCompile(`^上→[上下左右] 下→[上下左右] 左→[上下左右] 右→[上下左右]$`)

func TestDrawDirectionKeys_FormatAndNeverIdentity(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	seen := map[string]bool{}
	for range 1000 {
		got := drawDirectionKeys(r)
		if !directionResultPattern.MatchString(got) {
			t.Fatalf("格式不對:%q", got)
		}
		if got == "上→上 下→下 左→左 右→右" {
			t.Fatalf("抽到了恆等排列:%q", got)
		}
		// 新鍵必須是一個排列:四個目標各出現一次。
		targets := map[string]int{}
		for _, part := range strings.Fields(got) {
			_, to, _ := strings.Cut(part, "→")
			targets[to]++
		}
		if len(targets) != 4 {
			t.Fatalf("不是排列(有鍵重複或缺席):%q", got)
		}
		seen[got] = true
	}
	// 24 − 1 = 23 種都抽得到(1000 次對 23 種,漏掉任何一種的機率 < 1e-18)。
	if len(seen) != 23 {
		t.Errorf("1000 次應覆蓋全部 23 種非恆等排列,實際 %d 種", len(seen))
	}
}

// 排列表本身:23 個、無恆等、無重複。「永不恆等」的保證就在這張表。
func TestDirectionPermutations(t *testing.T) {
	if len(directionPermutations) != 23 {
		t.Fatalf("排列數 = %d,期望 23", len(directionPermutations))
	}
	seen := map[string]bool{}
	for _, p := range directionPermutations {
		if isIdentity(p) {
			t.Fatalf("表裡有恆等排列 %v", p)
		}
		key := ""
		for _, v := range p {
			key += string(rune('0' + v))
		}
		if seen[key] {
			t.Fatalf("排列 %v 重複", p)
		}
		seen[key] = true
	}
}

// 固定種子 → 固定結果(與 bracket.Build 同一個約定)。
func TestDrawDirectionKeys_Deterministic(t *testing.T) {
	a := drawDirectionKeys(rand.New(rand.NewPCG(1, 2)))
	b := drawDirectionKeys(rand.New(rand.NewPCG(1, 2)))
	if a != b {
		t.Fatalf("同種子不同結果:%q / %q", a, b)
	}
}

// ── 武學池 ──────────────────────────────────────────────────────

func TestDrawFromPool(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	pools := fakePools{"wuxue": {"太虛劍意", "獨尊刀法", "鹿鳴劍"}}

	for range 50 {
		got, err := drawFromPool(r, pools, "wuxue")
		if err != nil {
			t.Fatalf("drawFromPool: %v", err)
		}
		if got != "太虛劍意" && got != "獨尊刀法" && got != "鹿鳴劍" {
			t.Fatalf("抽到池外的東西:%q", got)
		}
	}

	for name, p := range map[string]DrawPools{
		"池不存在": fakePools{},
		"池是空的": fakePools{"wuxue": {}},
		"沒有池":  nil,
	} {
		if _, err := drawFromPool(r, p, "wuxue"); !errors.Is(err, ErrDrawPoolEmpty) {
			t.Errorf("%s:期望 ErrDrawPoolEmpty,實際 %v", name, err)
		}
	}
}

// ── 封盤時抽選 ──────────────────────────────────────────────────

// 封盤後,「隨機武學」與「打亂方向鍵」兩筆都有結果,drawn_at 等於封盤時間;
// 不抽的項目維持 nil;回傳的清單就帶著結果(公示完整)。
func TestLock_DrawsAtLock(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWithGap(t, bp.RankKaishan, bp.RankWuwo, 20) // 60 BP
	f.repo.pools[testTournamentID] = fakePools{"wuxue": {"太虛劍意", "獨尊刀法"}}
	f.svc = New(f.repo, WithClock(testClock), WithRand(rand.New(rand.NewPCG(5, 6))))

	art := f.buy(t, CategoryWeapon, "隨機武學", "")     // draw=wuxue
	keys := f.buy(t, CategoryPoison, "打亂上下左右鍵", "") // draw=direction_keys
	plain := f.buy(t, CategoryDefense, "禁跳躍", "")   // 不抽

	view, err := f.svc.Lock(ctx, testMatchPub)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	byPub := map[string]Selection{}
	for _, s := range view.Selections {
		byPub[s.PublicID] = s
	}

	got := byPub[art.Selection.PublicID]
	if got.DrawResult == nil || (*got.DrawResult != "太虛劍意" && *got.DrawResult != "獨尊刀法") {
		t.Fatalf("隨機武學沒有抽到池裡的東西:%+v", got.DrawResult)
	}
	if got.DrawnAt == nil || !got.DrawnAt.Equal(testClock()) {
		t.Fatalf("drawn_at 應等於封盤時間 %v,實際 %v", testClock(), got.DrawnAt)
	}

	got = byPub[keys.Selection.PublicID]
	if got.DrawResult == nil || !directionResultPattern.MatchString(*got.DrawResult) {
		t.Fatalf("方向鍵結果格式不對:%+v", got.DrawResult)
	}
	if got.DrawnAt == nil || !got.DrawnAt.Equal(*view.LockedAt) {
		t.Fatalf("drawn_at 應與 locked_at 同一個值")
	}

	if got = byPub[plain.Selection.PublicID]; got.DrawResult != nil || got.DrawnAt != nil {
		t.Fatalf("不抽的項目不該有結果:%+v", got)
	}

	// 抽選結果落在 repo 裡(不是只在回傳值上)。
	stored, _ := f.repo.GetSelection(ctx, art.Selection.PublicID)
	if stored.DrawResult == nil {
		t.Fatal("抽選結果沒有寫回")
	}
}

// 武學池空 → 整個封盤失敗,場次仍未封盤(tx 回滾)。
func TestLock_EmptyPoolFailsWholeLock(t *testing.T) {
	ctx := context.Background()
	for name, pools := range map[string]fakePools{
		"config 沒有池": nil,
		"池是空的":       {"wuxue": {}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixtureWithGap(t, bp.RankKaishan, bp.RankWuwo, 20) // 60 BP
			if pools != nil {
				f.repo.pools[testTournamentID] = pools
			}
			f.buy(t, CategoryWeapon, "隨機武學", "")    // 12 BP
			f.buy(t, CategoryPoison, "打亂上下左右鍵", "") // 24 BP

			_, err := f.svc.Lock(ctx, testMatchPub)
			if !errors.Is(err, ErrDrawPoolEmpty) {
				t.Fatalf("期望 ErrDrawPoolEmpty,實際 %v", err)
			}
			m, _ := f.repo.GetMatch(ctx, testMatchPub)
			if m.LockedAt != nil || m.Status != statusReady {
				t.Fatalf("封盤失敗後場次不該是封盤狀態:%+v", m)
			}
			// 方向鍵那筆即使先抽到了,也要跟著回滾。
			sels, _ := f.repo.ListSelections(ctx, testMatchID, f.holder)
			for _, s := range sels {
				if s.DrawResult != nil {
					t.Fatalf("封盤失敗後仍留有抽選結果:%+v", s)
				}
			}
			// 還買得了東西:沒封盤。
			f.buy(t, CategoryDefense, "禁跳躍", "")
		})
	}
}

// 沒有人買要抽的項目時不讀池:config 沒有池也能封盤。
func TestLock_NoDrawItemsDoesNotNeedPool(t *testing.T) {
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	f.buy(t, CategoryDefense, "禁跳躍", "")
	if _, err := f.svc.Lock(context.Background(), testMatchPub); err != nil {
		t.Fatalf("Lock: %v", err)
	}
}

// 已退掉的「隨機武學」不抽:作廢的選擇不在封盤清單裡,也不該消耗池或報池空。
func TestLock_VoidedDrawItemIsSkipped(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	art := f.buy(t, CategoryWeapon, "隨機武學", "")
	if _, err := f.svc.VoidSelection(ctx, VoidParams{SelectionPublicID: art.Selection.PublicID, PlayerID: f.holder}); err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	// 池是空的 —— 若作廢的那筆被抽,這裡會 ErrDrawPoolEmpty。
	if _, err := f.svc.Lock(ctx, testMatchPub); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	stored, _ := f.repo.GetSelection(ctx, art.Selection.PublicID)
	if stored.DrawResult != nil {
		t.Fatal("作廢的選擇不該被抽")
	}
}

// 同一筆只抽一次:SetSelectionDraw 的 draw_result IS NULL 守;0 列是錯,不是跳過。
func TestSetSelectionDraw_SecondTimeIsAnError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
	f.repo.pools[testTournamentID] = fakePools{"wuxue": {"太虛劍意"}}
	art := f.buy(t, CategoryWeapon, "隨機武學", "")
	if _, err := f.svc.Lock(ctx, testMatchPub); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	err := f.repo.SetSelectionDraw(ctx, art.Selection.ID, "獨尊刀法", testClock())
	if !errors.Is(err, ErrAlreadyDrawn) {
		t.Fatalf("期望 ErrAlreadyDrawn,實際 %v", err)
	}
	stored, _ := f.repo.GetSelection(ctx, art.Selection.PublicID)
	if *stored.DrawResult != "太虛劍意" {
		t.Fatalf("第二次抽覆蓋了結果:%q", *stored.DrawResult)
	}
}

// ── 設定確認清單 ────────────────────────────────────────────────

func TestChecklist(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWithGap(t, bp.RankKaishan, bp.RankWuwo, 20) // 60 BP
	f.repo.pools[testTournamentID] = fakePools{"wuxue": {"太虛劍意"}}

	// 購買順序刻意打亂分類與目錄順序:清單要依分類重排,分類內依**目錄順序**(sort_order),
	// 不是誰先買誰在前 —— 裁判逐項對照時看到的順序要跟目錄一致。
	f.buy(t, CategoryRule, "指定對手外觀", "全身白衣")                  // rule   #1
	armory := f.buy(t, CategoryWeapon, "雙方使用 46 級武庫武學裝備", "") // weapon #1(applies_to_both)
	voided := f.buy(t, CategoryDefense, "禁跳躍", "")            // defense,稍後退掉
	f.buy(t, CategoryRule, "獲得先手權", "")                       // rule   #2
	art := f.buy(t, CategoryWeapon, "隨機武學", "")               // weapon #2(抽)
	f.buy(t, CategoryDefense, "禁用迴避", "")                     // defense #1

	if _, err := f.svc.VoidSelection(ctx, VoidParams{SelectionPublicID: voided.Selection.PublicID, PlayerID: f.holder}); err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	view, err := f.svc.Lock(ctx, testMatchPub)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	got := view.Checklist()
	wantKeys := []string{
		"defense.no_dodge",           // defense(sort 30)
		"rule.first_move",            // rule(sort 30;雖然比外觀晚買)
		"rule.designated_appearance", // rule(sort 40)
		"weapon.random_art",          // weapon(sort 20;雖然比武庫晚買)
		"weapon.armory_46",           // weapon(sort 40)
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("清單 %d 列,期望 %d 列:%+v", len(got), len(wantKeys), got)
	}
	for i, e := range got {
		if e.ItemKey != wantKeys[i] {
			t.Errorf("第 %d 列 = %s,期望 %s", i, e.ItemKey, wantKeys[i])
		}
		if e.SelectionPublicID == "" || e.ItemName == "" {
			t.Errorf("第 %d 列缺識別或名稱:%+v", i, e)
		}
		if e.RefereeNote == "" {
			t.Errorf("第 %d 列(%s)沒有帶到 referee_note", i, e.ItemKey)
		}
		if e.SelectionPublicID == voided.Selection.PublicID {
			t.Errorf("作廢的選擇出現在清單裡")
		}
	}
	// 逐列內容。
	if got[2].TargetNote != "全身白衣" {
		t.Errorf("指定對手外觀的 TargetNote = %q", got[2].TargetNote)
	}
	if !got[4].AppliesToBoth || got[4].SelectionPublicID != armory.Selection.PublicID {
		t.Errorf("46 級武庫應標 AppliesToBoth:%+v", got[4])
	}
	if got[3].DrawResult == nil || *got[3].DrawResult != "太虛劍意" || got[3].SelectionPublicID != art.Selection.PublicID {
		t.Errorf("隨機武學應帶抽選結果:%+v", got[3])
	}
	for i := range got {
		if i != 3 && got[i].DrawResult != nil {
			t.Errorf("第 %d 列不該有抽選結果", i)
		}
		if i != 4 && got[i].AppliesToBoth {
			t.Errorf("第 %d 列不該是 AppliesToBoth", i)
		}
	}

	// 作廢的即使混進 Selections 也要被濾掉(Checklist 自己守,不靠 adapter 過濾)。
	mh := &MatchHandicaps{Selections: []Selection{
		{PublicID: "A", Category: CategoryRule, ItemKey: "rule.x"},
		{PublicID: "B", Category: CategoryDefense, ItemKey: "defense.y", Voided: true},
	}}
	if cl := mh.Checklist(); len(cl) != 1 || cl[0].SelectionPublicID != "A" {
		t.Errorf("作廢的選擇沒有被濾掉:%+v", cl)
	}
	if cl := (&MatchHandicaps{}).Checklist(); len(cl) != 0 {
		t.Errorf("空的 MatchHandicaps 應回空清單")
	}
}

// ── 目錄同步 ────────────────────────────────────────────────────

func TestSyncCatalogue(t *testing.T) {
	ctx := context.Background()

	t.Run("無選擇時更新文字與參數但不動 cost", func(t *testing.T) {
		repo := newFakeRepo()
		svc := New(repo)
		if _, err := svc.InstallSeedItems(ctx, testTournamentID); err != nil {
			t.Fatalf("InstallSeedItems: %v", err)
		}
		// 模擬「舊草稿」:把 DB 裡的文字改掉、價格也改掉。
		var target *Item
		for _, it := range repo.items {
			if it.Key == "victory.survive_90s" {
				target = it
			}
		}
		target.RefereeNote, target.Name, target.Params, target.Cost = "舊草稿", "舊名稱", ItemParams{}, target.Cost+100
		wantCost := target.Cost

		rep, err := svc.SyncCatalogue(ctx, testTournamentID)
		if err != nil {
			t.Fatalf("SyncCatalogue: %v", err)
		}
		if rep.Updated != 34 || len(rep.ExtraKeys) != 0 {
			t.Fatalf("report = %+v,期望更新 34 列、無缺無多", rep)
		}
		seed, _ := SeedItems()
		var spec ItemSpec
		for _, s := range seed {
			if s.Key == "victory.survive_90s" {
				spec = s
			}
		}
		if target.RefereeNote != *spec.RefereeNote || target.Name != spec.Name || target.Params != spec.Params {
			t.Errorf("文字/參數沒有同步:%+v", target)
		}
		if target.Cost != wantCost {
			t.Errorf("cost 被動了:%d → %d", wantCost, target.Cost)
		}
	})

	t.Run("有選擇時拒絕", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo)
		s := f.buy(t, CategoryDefense, "禁跳躍", "")
		// 退掉也算:「有人依這份文字買過」。
		if _, err := f.svc.VoidSelection(ctx, VoidParams{SelectionPublicID: s.Selection.PublicID, PlayerID: f.holder}); err != nil {
			t.Fatalf("VoidSelection: %v", err)
		}
		_, err := f.svc.SyncCatalogue(ctx, testTournamentID)
		if !errors.Is(err, ErrCatalogueInUse) {
			t.Fatalf("期望 ErrCatalogueInUse,實際 %v", err)
		}
	})

	t.Run("別屆的選擇不影響這屆", func(t *testing.T) {
		f := newFixture(t, bp.RankKaishan, bp.RankWuwo) // 第 1 屆有選擇
		f.buy(t, CategoryDefense, "禁跳躍", "")
		if _, err := f.svc.InstallSeedItems(ctx, 2); err != nil {
			t.Fatalf("InstallSeedItems: %v", err)
		}
		if _, err := f.svc.SyncCatalogue(ctx, 2); err != nil {
			t.Fatalf("第 2 屆同步不該被第 1 屆的選擇擋下:%v", err)
		}
	})

	t.Run("該屆多出來的項目只回報不動手", func(t *testing.T) {
		repo := newFakeRepo()
		svc := New(repo)
		if _, err := svc.InstallSeedItems(ctx, testTournamentID); err != nil {
			t.Fatalf("InstallSeedItems: %v", err)
		}
		repo.items = append(repo.items, &Item{ID: 999, TournamentID: testTournamentID, Ref: "999",
			Key: "rule.custom", Category: CategoryRule, Name: "這屆自己加的", Cost: 1})

		rep, err := svc.SyncCatalogue(ctx, testTournamentID)
		if err != nil {
			t.Fatalf("SyncCatalogue: %v", err)
		}
		if rep.Updated != 34 {
			t.Errorf("Updated = %d,期望 34", rep.Updated)
		}
		if len(rep.ExtraKeys) != 1 || rep.ExtraKeys[0] != "rule.custom" {
			t.Errorf("ExtraKeys = %v", rep.ExtraKeys)
		}
		if len(repo.items) != 35 {
			t.Errorf("同步不該補建或刪除項目,實際 %d 項", len(repo.items))
		}
	})

	t.Run("目錄有而該屆沒有的項目就拒絕", func(t *testing.T) {
		repo := newFakeRepo()
		svc := New(repo)
		seed, _ := SeedItems()
		if _, err := repo.InsertItems(ctx, testTournamentID, seed[:30]); err != nil {
			t.Fatalf("InsertItems: %v", err)
		}
		before := repo.items[0].RefereeNote

		_, err := svc.SyncCatalogue(ctx, testTournamentID)
		if !errors.Is(err, ErrCatalogueMismatch) {
			t.Fatalf("期望 ErrCatalogueMismatch,實際 %v", err)
		}
		// 拒絕在任何 UPDATE 之前:沒有「做了一半」。
		if repo.items[0].RefereeNote != before {
			t.Error("拒絕同步時不該改到任何項目")
		}
	})

	t.Run("影響列數不符整個失敗", func(t *testing.T) {
		repo := newFakeRepo()
		svc := New(repo)
		if _, err := svc.InstallSeedItems(ctx, testTournamentID); err != nil {
			t.Fatalf("InstallSeedItems: %v", err)
		}
		repo.syncShortfall = 1
		_, err := svc.SyncCatalogue(ctx, testTournamentID)
		if !errors.Is(err, ErrCatalogueMismatch) {
			t.Fatalf("期望 ErrCatalogueMismatch,實際 %v", err)
		}
	})

	t.Run("缺 tournament_id", func(t *testing.T) {
		if _, err := New(newFakeRepo()).SyncCatalogue(ctx, 0); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("期望 ErrInvalidRequest,實際 %v", err)
		}
	})
}
