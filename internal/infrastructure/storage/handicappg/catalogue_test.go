package handicappg_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
)

// 2026-09-13 增補(migration 00006):key / params 的落地、目錄同步、封盤抽選。
//
// 這些全部要對真 Postgres 跑:key 的 CHECK、params 的 JSONB 往返、
// draw_result IS NULL 的述詞、以及「池空時整個 tx 回滾」—— fake 只能模仿結論,
// 保證本身在資料庫裡。

// ── key 與 params 落地 ──────────────────────────────────────────

// 34 列寫進去之後 key 與 params 都對:抽最能代表的幾項釘住。
func TestInstallSeedItemsWritesKeyAndParams(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())

	n, err := handicap.New(repo).InstallSeedItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	if n != 34 {
		t.Fatalf("寫入 %d 列,想要 34", n)
	}

	want := map[string]handicap.ItemParams{
		"victory.survive_90s":  {Seconds: 90},
		"victory.survive_120s": {Seconds: 120},
		"weapon.random_art":    {Draw: handicap.DrawWuxue},
		"poison.scramble_keys": {Draw: handicap.DrawDirectionKeys},
		"weapon.armory_46":     {AppliesToBoth: true},
		"defense.no_jump":      {},
	}
	for key, params := range want {
		it, err := repo.GetItemByKey(ctx, f.tournamentID, key)
		if err != nil {
			t.Fatalf("GetItemByKey(%s): %v", key, err)
		}
		if it.Key != key || it.Params != params {
			t.Errorf("%s: key=%q params=%+v,想要 %+v", key, it.Key, it.Params, params)
		}
		if !strings.HasPrefix(key, string(it.Category)+".") {
			t.Errorf("%s 的分類 %q 與 key 前綴不一致", key, it.Category)
		}
	}

	// DB 側:沒有參數的項目存的是 {},不是三個零值(讀的人不必解讀 0 代表「沒有」)。
	var raw string
	if err := pool.QueryRow(ctx,
		`SELECT params::text FROM activity.handicap_items WHERE tournament_id = $1 AND key = 'defense.no_jump'`,
		f.tournamentID).Scan(&raw); err != nil {
		t.Fatalf("讀 params: %v", err)
	}
	if raw != "{}" {
		t.Errorf("無參數項目的 params = %s,想要 {}", raw)
	}

	// ListItems 同樣帶 key 與 params(三支項目查詢共用一份轉換)。
	items, err := repo.ListItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	keys := map[string]bool{}
	for _, it := range items {
		if it.Key == "" {
			t.Errorf("項目 %q 沒有 key", it.Name)
		}
		keys[it.Key] = true
	}
	if len(keys) != 34 {
		t.Errorf("唯一 key = %d,想要 34", len(keys))
	}
}

// key 的形狀由 DB 的 CHECK 守:前綴與分類不一致要被擋,而且整批都不進去。
func TestInsertItemsKeyCheckConstraint(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())

	bad := testSpecs()
	bad[1].Key = "weapon.wrong_prefix" // 分類是 skill
	if _, err := repo.InsertItems(ctx, f.tournamentID, bad); err == nil {
		t.Fatal("前綴與分類不一致的 key 應被 DB CHECK 擋下")
	}
	items, err := repo.ListItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("整批同一個敘述,失敗不該留下 %d 列", len(items))
	}
}

// DB 裡塞了未登記的 params 鍵,讀回時要出聲 —— JSONB 不是垃圾桶,
// 而封盤抽選依 params.draw 決定要不要抽,一個沒驗過的值不能混進去。
func TestReadRejectsUnknownParamsKey(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	sel, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_items SET params = '{"bogus": 1}'::jsonb WHERE id = $1`,
		items["測試_十點"].ID); err != nil {
		t.Fatalf("塞壞 params: %v", err)
	}

	if _, err := repo.GetItem(ctx, f.tournamentID, items["測試_十點"].Ref); !errors.Is(err, handicap.ErrInvalidItemParams) {
		t.Errorf("GetItem err = %v,想要 ErrInvalidItemParams", err)
	}
	if _, err := repo.ListItems(ctx, f.tournamentID); !errors.Is(err, handicap.ErrInvalidItemParams) {
		t.Errorf("ListItems err = %v,想要 ErrInvalidItemParams", err)
	}
	if _, err := repo.GetSelection(ctx, sel.Selection.PublicID); !errors.Is(err, handicap.ErrInvalidItemParams) {
		t.Errorf("GetSelection err = %v,想要 ErrInvalidItemParams", err)
	}
	if _, err := repo.ListSelections(ctx, f.matchID, f.holderID); !errors.Is(err, handicap.ErrInvalidItemParams) {
		t.Errorf("ListSelections err = %v,想要 ErrInvalidItemParams", err)
	}
}

// 選擇的三支查詢都帶回 key / params / referee_note,而且剛買的那筆抽選欄位是 NULL。
func TestSelectionCarriesItemSnapshot(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	res, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_四點"].Ref,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	check := func(where string, s handicap.Selection) {
		t.Helper()
		if s.ItemKey != "skill.test_four" || s.ItemRefereeNote != "裁判說明" || s.ItemParams != (handicap.ItemParams{}) {
			t.Errorf("%s:項目快照不完整 %+v", where, s)
		}
		if s.DrawResult != nil || s.DrawnAt != nil {
			t.Errorf("%s:未封盤不該有抽選結果 %+v", where, s)
		}
	}
	check("Insert", res.Selection)
	got, err := repo.GetSelection(ctx, res.Selection.PublicID)
	if err != nil {
		t.Fatalf("GetSelection: %v", err)
	}
	check("Get", *got)
	list, err := repo.ListSelections(ctx, f.matchID, f.holderID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSelections: %v / %d 列", err, len(list))
	}
	check("List", list[0])
}

// ── 目錄同步 ────────────────────────────────────────────────────

// 無選擇時更新文字與參數、不動 cost。做法:先裝 seed,把 DB 裡一項改成「舊草稿」
// (文字、params、cost 全改),sync 之後文字與 params 回到 seed 的值,cost 維持改過的值。
func TestSyncCatalogueUpdatesTextNotCost(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	if _, err := svc.InstallSeedItems(ctx, f.tournamentID); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}

	const key = "victory.survive_90s"
	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_items
		    SET referee_note = '舊草稿', name = '舊名稱', params = '{}'::jsonb, cost = cost + 100
		  WHERE tournament_id = $1 AND key = $2`, f.tournamentID, key); err != nil {
		t.Fatalf("改成舊草稿: %v", err)
	}
	before, err := repo.GetItemByKey(ctx, f.tournamentID, key)
	if err != nil {
		t.Fatalf("GetItemByKey: %v", err)
	}

	rep, err := svc.SyncCatalogue(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("SyncCatalogue: %v", err)
	}
	if rep.Updated != 34 || len(rep.ExtraKeys) != 0 {
		t.Fatalf("report = %+v,想要 34 / 無缺 / 無多", rep)
	}

	seed, _ := handicap.SeedItems()
	var spec handicap.ItemSpec
	for _, s := range seed {
		if s.Key == key {
			spec = s
		}
	}
	after, err := repo.GetItemByKey(ctx, f.tournamentID, key)
	if err != nil {
		t.Fatalf("GetItemByKey: %v", err)
	}
	if after.RefereeNote != *spec.RefereeNote || after.Name != spec.Name || after.Params != spec.Params {
		t.Errorf("文字/參數沒同步回 seed:%+v", after)
	}
	if after.Cost != before.Cost {
		t.Errorf("cost 被動了:%d → %d", before.Cost, after.Cost)
	}
	if after.ID != before.ID || after.Ref != before.Ref {
		t.Errorf("同步不該換列或換 public_id")
	}
}

// 該屆已有選擇(即使已退掉)→ 拒絕,且 DB 文字一個字都沒動。
func TestSyncCatalogueRejectedWhenSelectionsExist(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	if _, err := svc.InstallSeedItems(ctx, f.tournamentID); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	item, err := repo.GetItemByKey(ctx, f.tournamentID, "defense.no_jump")
	if err != nil {
		t.Fatalf("GetItemByKey: %v", err)
	}
	res, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: item.Ref,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// 退掉也算:「有人依這份文字買過」。
	if _, err := svc.VoidSelection(ctx, handicap.VoidParams{
		SelectionPublicID: res.Selection.PublicID, PlayerID: f.holderID,
	}); err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_items SET referee_note = '舊草稿' WHERE tournament_id = $1 AND key = 'defense.no_jump'`,
		f.tournamentID); err != nil {
		t.Fatalf("改成舊草稿: %v", err)
	}

	_, err = svc.SyncCatalogue(ctx, f.tournamentID)
	if !errors.Is(err, handicap.ErrCatalogueInUse) {
		t.Fatalf("err = %v,想要 ErrCatalogueInUse", err)
	}
	again, err := repo.GetItemByKey(ctx, f.tournamentID, "defense.no_jump")
	if err != nil {
		t.Fatalf("GetItemByKey: %v", err)
	}
	if again.RefereeNote != "舊草稿" {
		t.Errorf("被拒的同步不該動到文字,實際 %q", again.RefereeNote)
	}

	// 別屆的選擇不影響這屆。
	other := newFixture(t, defaultOpt())
	if _, err := svc.InstallSeedItems(ctx, other.tournamentID); err != nil {
		t.Fatalf("InstallSeedItems(別屆): %v", err)
	}
	if _, err := svc.SyncCatalogue(ctx, other.tournamentID); err != nil {
		t.Errorf("別屆同步不該被這屆的選擇擋下:%v", err)
	}
}

// 影響列數對不上 → ErrCatalogueMismatch,而且已經做了一半的 UPDATE 跟著 tx 回滾。
//
// 真 SQL 的影響列數天生等於預期(同一個 tx、以 key 匹配),所以用一層只改
// SyncItems 回傳值的包裝製造不符;UPDATE 本身照樣打進 Postgres —— 這個測試
// 驗的正是「出錯時那個 UPDATE 有沒有被收回」,那只有真的 DB 答得出來。
func TestSyncCatalogueMismatchRollsBack(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	if _, err := handicap.New(repo).InstallSeedItems(ctx, f.tournamentID); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_items SET referee_note = '舊草稿' WHERE tournament_id = $1 AND key = 'defense.no_jump'`,
		f.tournamentID); err != nil {
		t.Fatalf("改成舊草稿: %v", err)
	}

	_, err := handicap.New(shortSync{repo}).SyncCatalogue(ctx, f.tournamentID)
	if !errors.Is(err, handicap.ErrCatalogueMismatch) {
		t.Fatalf("err = %v,想要 ErrCatalogueMismatch", err)
	}
	it, err := repo.GetItemByKey(ctx, f.tournamentID, "defense.no_jump")
	if err != nil {
		t.Fatalf("GetItemByKey: %v", err)
	}
	if it.RefereeNote != "舊草稿" {
		t.Errorf("列數不符時 UPDATE 應回滾,實際 referee_note = %q", it.RefereeNote)
	}
}

// shortSync 讓 SyncItems 少報一列;其餘原樣委派給真 adapter(含 WithTx 內的那一層)。
type shortSync struct{ handicap.Repository }

func (s shortSync) WithTx(ctx context.Context, fn func(context.Context, handicap.Repository) error) error {
	return s.Repository.WithTx(ctx, func(ctx context.Context, r handicap.Repository) error {
		return fn(ctx, shortSync{r})
	})
}

func (s shortSync) SyncItems(ctx context.Context, tournamentID int64, specs []handicap.ItemSpec) (int, error) {
	n, err := s.Repository.SyncItems(ctx, tournamentID, specs)
	return n - 1, err
}

// ── 封盤抽選 ────────────────────────────────────────────────────

var directionResultPattern = regexp.MustCompile(`^上→[上下左右] 下→[上下左右] 左→[上下左右] 右→[上下左右]$`)

// seedFixture 是一場已裝 seed、已發預算的比賽,回 key → Item。
func seedFixture(t *testing.T, repo *handicappg.Repo, opt fixtureOpt) (fixture, map[string]handicap.Item) {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t, opt)
	if _, err := handicap.New(repo).InstallSeedItems(ctx, f.tournamentID); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	if _, err := handicap.New(repo).GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	items, err := repo.ListItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	byKey := make(map[string]handicap.Item, len(items))
	for _, it := range items {
		byKey[it.Key] = it
	}
	return f, byKey
}

func buyKey(t *testing.T, svc *handicap.Service, f fixture, items map[string]handicap.Item, key string) handicap.Selection {
	t.Helper()
	res, err := svc.Select(context.Background(), handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items[key].Ref,
	})
	if err != nil {
		t.Fatalf("買 %s: %v", key, err)
	}
	return res.Selection
}

// 封盤後 random_art 與 scramble_keys 都有 draw_result 與 drawn_at(= locked_at);
// 不抽的項目維持 NULL;回傳的清單就帶著結果。
func TestLockDrawsBothKinds(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	opt := defaultOpt()
	opt.perRankGap = "20" // 60 BP:隨機武學 12 + 打亂方向鍵 24 + 禁跳躍 2
	opt.wuxuePool = []string{"太虛劍意", "獨尊刀法", "鹿鳴劍"}
	f, items := seedFixture(t, repo, opt)

	lockedAt := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	svc := handicap.New(repo,
		handicap.WithClock(func() time.Time { return lockedAt }),
		handicap.WithRand(rand.New(rand.NewPCG(1, 2))))
	art := buyKey(t, svc, f, items, "weapon.random_art")
	keys := buyKey(t, svc, f, items, "poison.scramble_keys")
	plain := buyKey(t, svc, f, items, "defense.no_jump")

	view, err := svc.Lock(ctx, f.matchRef)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if view.LockedAt == nil || !view.LockedAt.Equal(lockedAt) {
		t.Fatalf("LockedAt = %v", view.LockedAt)
	}
	byPub := map[string]handicap.Selection{}
	for _, s := range view.Selections {
		byPub[s.PublicID] = s
	}
	if got := byPub[art.PublicID]; got.DrawResult == nil ||
		(*got.DrawResult != "太虛劍意" && *got.DrawResult != "獨尊刀法" && *got.DrawResult != "鹿鳴劍") {
		t.Errorf("隨機武學的結果不在池裡:%v", got.DrawResult)
	}
	if got := byPub[keys.PublicID]; got.DrawResult == nil || !directionResultPattern.MatchString(*got.DrawResult) {
		t.Errorf("方向鍵結果格式不對:%v", got.DrawResult)
	}
	if got := byPub[plain.PublicID]; got.DrawResult != nil || got.DrawnAt != nil {
		t.Errorf("不抽的項目不該有結果:%+v", got)
	}

	// DB 側:drawn_at 與 handicap_locked_at 是同一個值;不抽的那筆兩欄都 NULL。
	rows, err := pool.Query(ctx,
		`SELECT s.public_id, s.draw_result, s.drawn_at, m.handicap_locked_at
		   FROM activity.handicap_selections s JOIN activity.matches m ON m.id = s.match_id
		  WHERE s.match_id = $1`, f.matchID)
	if err != nil {
		t.Fatalf("查抽選結果: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var pub string
		var result *string
		var drawnAt, lockedAtDB *time.Time
		if err := rows.Scan(&pub, &result, &drawnAt, &lockedAtDB); err != nil {
			t.Fatalf("掃描: %v", err)
		}
		seen++
		if lockedAtDB == nil || !lockedAtDB.Equal(lockedAt) {
			t.Errorf("handicap_locked_at = %v,想要 %v", lockedAtDB, lockedAt)
		}
		switch pub {
		case art.PublicID, keys.PublicID:
			if result == nil || drawnAt == nil || !drawnAt.Equal(*lockedAtDB) {
				t.Errorf("%s:draw_result=%v drawn_at=%v,想要 drawn_at = locked_at", pub, result, drawnAt)
			}
		case plain.PublicID:
			if result != nil || drawnAt != nil {
				t.Errorf("%s 不該有抽選結果", pub)
			}
		}
	}
	if seen != 3 {
		t.Errorf("選擇列數 = %d,想要 3", seen)
	}

	// 設定確認清單:三列、依分類排、抽選結果跟著。
	cl := view.Checklist()
	if len(cl) != 3 || cl[0].ItemKey != "defense.no_jump" || cl[1].ItemKey != "poison.scramble_keys" || cl[2].ItemKey != "weapon.random_art" {
		t.Errorf("清單順序不對:%+v", cl)
	}
	if cl[2].DrawResult == nil || cl[1].DrawResult == nil || cl[0].DrawResult != nil {
		t.Errorf("清單的抽選結果不對:%+v", cl)
	}
	for _, e := range cl {
		if e.RefereeNote == "" {
			t.Errorf("%s 沒帶 referee_note", e.ItemKey)
		}
	}
}

// 武學池空 → 整個封盤失敗:handicap_locked_at 仍為 NULL、status 仍 ready、
// 先抽到的方向鍵結果也跟著回滾。之後還買得了東西。
func TestLockEmptyPoolRollsBackWholeLock(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)

	for name, poolOpt := range map[string][]string{
		"池是空的":       {},
		"config 沒有池": nil,
	} {
		t.Run(name, func(t *testing.T) {
			opt := defaultOpt()
			opt.perRankGap = "20"
			opt.wuxuePool = poolOpt
			f, items := seedFixture(t, repo, opt)
			svc := handicap.New(repo)
			buyKey(t, svc, f, items, "poison.scramble_keys") // 先抽這筆(方向鍵不需要池)
			buyKey(t, svc, f, items, "weapon.random_art")    // 這筆抽不出來

			_, err := svc.Lock(ctx, f.matchRef)
			if !errors.Is(err, handicap.ErrDrawPoolEmpty) {
				t.Fatalf("err = %v,想要 ErrDrawPoolEmpty", err)
			}

			var status string
			var lockedAt *time.Time
			if err := pool.QueryRow(ctx,
				`SELECT status, handicap_locked_at FROM activity.matches WHERE id = $1`, f.matchID,
			).Scan(&status, &lockedAt); err != nil {
				t.Fatalf("讀場次: %v", err)
			}
			if lockedAt != nil || status != "ready" {
				t.Fatalf("封盤失敗後 status=%q locked_at=%v,想要 ready / NULL", status, lockedAt)
			}
			var drawn int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM activity.handicap_selections WHERE match_id = $1 AND draw_result IS NOT NULL`,
				f.matchID).Scan(&drawn); err != nil {
				t.Fatalf("數抽選: %v", err)
			}
			if drawn != 0 {
				t.Errorf("封盤失敗後仍留有 %d 筆抽選結果(方向鍵那筆沒有回滾)", drawn)
			}
			// 沒封盤:還買得了。
			buyKey(t, svc, f, items, "defense.no_jump")
		})
	}
}

// 同一筆只抽一次由 draw_result IS NULL 守;第二次是 ErrAlreadyDrawn 且結果不被覆蓋。
func TestSetSelectionDrawOnlyOnce(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	opt := defaultOpt()
	opt.wuxuePool = []string{"太虛劍意"}
	f, items := seedFixture(t, repo, opt)
	svc := handicap.New(repo)
	art := buyKey(t, svc, f, items, "weapon.random_art")

	view, err := svc.Lock(ctx, f.matchRef)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	err = repo.SetSelectionDraw(ctx, art.ID, "獨尊刀法", *view.LockedAt)
	if !errors.Is(err, handicap.ErrAlreadyDrawn) {
		t.Fatalf("err = %v,想要 ErrAlreadyDrawn", err)
	}
	got, err := repo.GetSelection(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("GetSelection: %v", err)
	}
	if got.DrawResult == nil || *got.DrawResult != "太虛劍意" {
		t.Errorf("第二次抽覆蓋了結果:%v", got.DrawResult)
	}
	// 另外:DB 的 CHECK 不讓 draw_result 與 drawn_at 分家。
	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_selections SET drawn_at = NULL WHERE id = $1`, art.ID); err == nil {
		t.Error("draw_result 有值而 drawn_at 為 NULL 應被 handicap_selections_draw_check 擋下")
	}
}

// 兩個裁判同時封盤:一個成功、一個 ErrAlreadyLocked,抽選恰好一次。
//
// 抽選跟在封盤 UPDATE 後面、同一個場次鎖底下,所以不可能兩邊都抽;
// 這條驗的是那個前提在真 Postgres 上成立,而不是 fake 的一把 mutex。
func TestConcurrentLockDrawsExactlyOnce(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	opt := defaultOpt()
	opt.wuxuePool = []string{"太虛劍意", "獨尊刀法"}
	f, items := seedFixture(t, repo, opt)
	svc := handicap.New(repo)
	art := buyKey(t, svc, f, items, "weapon.random_art")

	const judges = 6
	var wg sync.WaitGroup
	errs := make([]error, judges)
	start := make(chan struct{})
	for i := range judges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = svc.Lock(ctx, f.matchRef)
		}()
	}
	close(start)
	wg.Wait()

	ok, already := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, handicap.ErrAlreadyLocked):
			already++
		default:
			t.Errorf("非預期的錯誤:%v", err)
		}
	}
	if ok != 1 || already != judges-1 {
		t.Fatalf("成功 %d / 已封 %d,想要 1 / %d", ok, already, judges-1)
	}
	got, err := repo.GetSelection(ctx, art.PublicID)
	if err != nil {
		t.Fatalf("GetSelection: %v", err)
	}
	if got.DrawResult == nil || got.DrawnAt == nil {
		t.Fatal("封盤成功的那次沒有抽")
	}
}
