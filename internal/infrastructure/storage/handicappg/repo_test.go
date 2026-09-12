package handicappg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 整合測試對真 Postgres 跑(CLAUDE.md:mock 掉資料庫的併發測試沒有意義)。
//
// 這個 package 的測試存在的理由,正是 core 的 fake 測不到的那一半:
// fake 用一把 Go 的 mutex 近似 SELECT ... FOR UPDATE,而「連點兩次會不會同時
// 通過餘額檢查」「一邊買一邊封盤會不會漏掉一筆」只有真的資料庫答得出來。
//
// 共用一個容器,每個測試自建一屆賽事與一場比賽互相隔離 —— 讓武的所有狀態
// 都掛在場次底下,換一場就換一個世界,不必清表。

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// ── 測試資料 ────────────────────────────────────────────────────

// fixture 是一屆賽事 + 一場已開盤的比賽 + 兩名已評段的選手。
type fixture struct {
	tournamentID int64
	matchID      int64
	matchRef     string
	// holderID 是低段位方(拿 BP 的人);otherID 是高段位方(被施加限制的人)。
	holderID int64
	otherID  int64
	holderP1 bool
}

type fixtureOpt struct {
	p1Rank     int16
	p2Rank     int16
	status     string
	open       bool
	perRankGap string // 空字串 = config 不放這個鍵(驗證退回預設值的路徑)
}

func defaultOpt() fixtureOpt {
	return fixtureOpt{p1Rank: 1, p2Rank: 4, status: "ready", open: true, perRankGap: "8"}
}

func newFixture(t *testing.T, opt fixtureOpt) fixture {
	t.Helper()
	ctx := context.Background()

	config := "{}"
	if opt.perRankGap != "" {
		config = fmt.Sprintf(`{"bp_per_rank_gap": %s}`, opt.perRankGap)
	}
	var tournamentID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, config, phase)
		 VALUES ($1, $2, '測試賽事', 1, $3::jsonb, 'in_progress') RETURNING id`,
		newID(t), "t-"+newID(t), config,
	).Scan(&tournamentID); err != nil {
		t.Fatalf("建賽事: %v", err)
	}

	p1 := newPlayer(t, tournamentID, opt.p1Rank)
	p2 := newPlayer(t, tournamentID, opt.p2Rank)

	matchRef := newID(t)
	var matchID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO activity.matches
		   (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, status, handicap_open)
		 VALUES ($1, $2, 1, 0, $3, $4, $5, $6) RETURNING id`,
		matchRef, tournamentID, p1, p2, opt.status, opt.open,
	).Scan(&matchID); err != nil {
		t.Fatalf("建場次: %v", err)
	}

	f := fixture{
		tournamentID: tournamentID, matchID: matchID, matchRef: matchRef,
		holderID: p1, otherID: p2, holderP1: true,
	}
	if opt.p2Rank < opt.p1Rank {
		f.holderID, f.otherID, f.holderP1 = p2, p1, false
	}
	return f
}

// newPlayer 建一名選手。rank 為 0 時留 NULL(未評段)。
func newPlayer(t *testing.T, tournamentID int64, rank int16) int64 {
	t.Helper()
	var fencerID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.fencers (public_id, game_id) VALUES ($1, $2) RETURNING id`,
		newID(t), "g-"+newID(t),
	).Scan(&fencerID); err != nil {
		t.Fatalf("建選手檔案: %v", err)
	}
	var rankArg *int16
	if rank > 0 {
		rankArg = &rank
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.tournament_players
		   (public_id, tournament_id, fencer_id, display_name, passcode_hash, discord_name, rank_level)
		 VALUES ($1, $2, $3, '測試選手', 'x', 'd', $4) RETURNING id`,
		newID(t), tournamentID, fencerID, rankArg,
	).Scan(&id); err != nil {
		t.Fatalf("建參賽者: %v", err)
	}
	return id
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New()
	if err != nil {
		t.Fatalf("產生 ULID: %v", err)
	}
	return id
}

// note 讓 ItemSpec.RefereeNote 這個指標欄位在測試裡好寫。
func note(s string) *string { return &s }

// testSpecs 是測試用的項目定義。
//
// 名稱刻意避開 targetNoteRequired 的那八項:那些必須填指定內容,
// 而這裡測的是餘額與併發,不是表單驗證。
func testSpecs() []handicap.ItemSpec {
	return []handicap.ItemSpec{
		{Category: handicap.CategoryWeapon, Name: "測試_十點", Description: "值十點", Cost: 10, SortOrder: 1},
		{Category: handicap.CategorySkill, Name: "測試_四點", RefereeNote: note("裁判說明"), Cost: 4, Repeatable: true, SortOrder: 2},
		{Category: handicap.CategoryVictory, Name: "測試_勝利甲", Cost: 6, SortOrder: 3},
		{Category: handicap.CategoryVictory, Name: "測試_勝利乙", Cost: 6, SortOrder: 4},
	}
}

// installItems 把項目寫進某一屆並回傳 name → Item 的對照。
func installItems(t *testing.T, repo *handicappg.Repo, tournamentID int64, specs []handicap.ItemSpec) map[string]handicap.Item {
	t.Helper()
	n, err := repo.InsertItems(context.Background(), tournamentID, specs)
	if err != nil {
		t.Fatalf("寫入項目: %v", err)
	}
	if n != len(specs) {
		t.Fatalf("插入列數 = %d,想要 %d", n, len(specs))
	}
	items, err := repo.ListItems(context.Background(), tournamentID)
	if err != nil {
		t.Fatalf("列出項目: %v", err)
	}
	out := make(map[string]handicap.Item, len(items))
	for _, it := range items {
		out[it.Name] = it
	}
	return out
}

// ── 不變量斷言 ──────────────────────────────────────────────────

// assertSpentConsistent 驗證 match_budgets.spent 恆等於未作廢選擇的 cost 總和。
//
// 這是 migrations/activity/00002 對測試的明文要求(schemas/20 待確認 ②):
// spent 是衍生資料,冗餘存放只為了選購頁不必每次 SUM —— 一旦兩者分家,
// 之後每一次餘額檢查都是錯的,而且沒有任何地方會報錯。
func assertSpentConsistent(t *testing.T, matchID, playerID int64) {
	t.Helper()
	var spent, sum int64
	err := pool.QueryRow(context.Background(),
		`SELECT b.spent,
		        COALESCE((SELECT SUM(s.cost) FROM activity.handicap_selections s
		                   WHERE s.match_id = b.match_id AND s.player_id = b.player_id
		                     AND NOT s.voided), 0)
		   FROM activity.match_budgets b
		  WHERE b.match_id = $1 AND b.player_id = $2`,
		matchID, playerID,
	).Scan(&spent, &sum)
	if err != nil {
		t.Fatalf("讀 spent 與總和: %v", err)
	}
	if spent != sum {
		t.Fatalf("spent = %d,未作廢選擇總和 = %d(不一致)", spent, sum)
	}
}

func countSelections(t *testing.T, matchID, playerID int64, includeVoided bool) int {
	t.Helper()
	q := `SELECT count(*) FROM activity.handicap_selections
	       WHERE match_id = $1 AND player_id = $2 AND NOT voided`
	if includeVoided {
		q = `SELECT count(*) FROM activity.handicap_selections WHERE match_id = $1 AND player_id = $2`
	}
	var n int
	if err := pool.QueryRow(context.Background(), q, matchID, playerID).Scan(&n); err != nil {
		t.Fatalf("數選擇: %v", err)
	}
	return n
}

// ── 場次 ────────────────────────────────────────────────────────

func TestMatchRoundTrip(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())

	m, err := repo.GetMatch(ctx, f.matchRef)
	if err != nil {
		t.Fatalf("GetMatch: %v", err)
	}
	switch {
	case m.ID != f.matchID:
		t.Errorf("ID = %d,想要 %d", m.ID, f.matchID)
	case m.Status != "ready":
		t.Errorf("Status = %q", m.Status)
	case !m.HandicapOpen:
		t.Error("HandicapOpen 應為 true")
	case m.LockedAt != nil:
		t.Error("尚未封盤,LockedAt 應為 nil")
	case m.P1Rank != bp.RankKaishan || m.P2Rank != bp.RankWuwo:
		t.Errorf("段位 = %d/%d,想要 1/4", m.P1Rank, m.P2Rank)
	case m.PerRankGap != 8:
		t.Errorf("PerRankGap = %d,想要 8", m.PerRankGap)
	case m.P1PlayerPublicID == "" || m.P2PlayerPublicID == "":
		t.Error("選手 public_id 不應為空")
	}

	if _, err := repo.GetMatch(ctx, "不存在"); !errors.Is(err, activityerr.ErrMatchNotFound) {
		t.Errorf("查不存在的場次 err = %v,想要 ErrMatchNotFound", err)
	}
}

// TestMatchNullColumns 驗證可空欄位攤成零值:對手未定、段位未評、config 缺鍵。
//
// 三個欄位在 core 型別裡都不是指標(P1PlayerID=0 / RankUnspecified / PerRankGap=0),
// 攤平的責任在 adapter —— 讀取端只要比 0,不必每處先判 nil。
func TestMatchNullColumns(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)

	var tournamentID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase)
		 VALUES ($1, $2, '空設定賽事', 1, 'in_progress') RETURNING id`,
		newID(t), "t-"+newID(t),
	).Scan(&tournamentID); err != nil {
		t.Fatalf("建賽事: %v", err)
	}
	p1 := newPlayer(t, tournamentID, 0) // 未評段
	matchRef := newID(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO activity.matches (public_id, tournament_id, round, slot, p1_player_id, status)
		 VALUES ($1, $2, 1, 0, $3, 'pending')`,
		matchRef, tournamentID, p1,
	); err != nil {
		t.Fatalf("建場次: %v", err)
	}

	m, err := repo.GetMatch(ctx, matchRef)
	if err != nil {
		t.Fatalf("GetMatch: %v", err)
	}
	if m.P2PlayerID != 0 || m.P2PlayerPublicID != "" {
		t.Errorf("對手未定應攤成零值,實際 %d / %q", m.P2PlayerID, m.P2PlayerPublicID)
	}
	if m.P1Rank != bp.RankUnspecified {
		t.Errorf("未評段應為 RankUnspecified,實際 %d", m.P1Rank)
	}
	if m.PerRankGap != 0 {
		t.Errorf("config 缺鍵時 PerRankGap 應為 0(由 bp 退回預設值),實際 %d", m.PerRankGap)
	}
}

// ── 項目 ────────────────────────────────────────────────────────

// TestInsertItemsTransposition 驗證八個平行陣列真的逐欄對齊。
//
// 這是 unnest 批次插入最危險的失敗模式:陣列長度不一致時 Postgres 補 NULL
// 而不是報錯,錯位插入不會有任何徵兆 —— 只會有一項讓武的價格是別人的。
func TestInsertItemsTransposition(t *testing.T) {
	setup(t)
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	specs := testSpecs()
	items := installItems(t, repo, f.tournamentID, specs)

	if len(items) != len(specs) {
		t.Fatalf("項目數 = %d,想要 %d", len(items), len(specs))
	}
	for _, s := range specs {
		got, ok := items[s.Name]
		if !ok {
			t.Fatalf("找不到項目 %q", s.Name)
		}
		if got.Cost != s.Cost || got.Category != s.Category ||
			got.SortOrder != s.SortOrder || got.Repeatable != s.Repeatable ||
			got.Description != s.Description {
			t.Errorf("%q 欄位錯位:%+v,想要 %+v", s.Name, got, s)
		}
		wantNote := ""
		if s.RefereeNote != nil {
			wantNote = *s.RefereeNote
		}
		if got.RefereeNote != wantNote {
			t.Errorf("%q RefereeNote = %q,想要 %q", s.Name, got.RefereeNote, wantNote)
		}
		if len(got.Ref) != 26 {
			t.Errorf("%q 的 Ref = %q,應是 26 字元 ULID", s.Name, got.Ref)
		}
	}

	// referee_note 為 nil 的 spec 在 DB 裡必須是 NULL,不是空字串:
	// 「還沒寫」與「寫了空字串」在裁判手冊補齊的過程中是兩件事。
	var nullNotes int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity.handicap_items
		  WHERE tournament_id = $1 AND referee_note IS NULL`, f.tournamentID,
	).Scan(&nullNotes); err != nil {
		t.Fatalf("數 NULL referee_note: %v", err)
	}
	if nullNotes != 3 {
		t.Errorf("referee_note 為 NULL 的項目 = %d,想要 3", nullNotes)
	}
}

// TestInsertItemsRerunInsertsNothing 驗證重跑 seed 不改價格。
func TestInsertItemsRerunInsertsNothing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())

	svc := handicap.New(repo)
	first, err := svc.InstallSeedItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	seed, err := handicap.SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	if first != len(seed) {
		t.Fatalf("首次安裝 = %d,想要 %d", first, len(seed))
	}

	// 改一項的價格,重跑之後價格必須原封不動 —— 改價是裁判的明確動作。
	items, err := repo.ListItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	target := items[0]
	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_items SET cost = cost + 100 WHERE id = $1`, target.ID); err != nil {
		t.Fatalf("改價: %v", err)
	}

	second, err := svc.InstallSeedItems(ctx, f.tournamentID)
	if err != nil {
		t.Fatalf("重跑 InstallSeedItems: %v", err)
	}
	if second != 0 {
		t.Errorf("重跑插入 = %d,想要 0", second)
	}
	again, err := repo.GetItem(ctx, f.tournamentID, target.Ref)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if again.Cost != target.Cost+100 {
		t.Errorf("重跑後 cost = %d,想要 %d(不該被 seed 蓋回去)", again.Cost, target.Cost+100)
	}
}

// TestGetItemIsTournamentScoped 驗證跨屆引用是「找不到」。
func TestGetItemIsTournamentScoped(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	a := newFixture(t, defaultOpt())
	b := newFixture(t, defaultOpt())
	items := installItems(t, repo, a.tournamentID, testSpecs())
	ref := items["測試_十點"].Ref

	if _, err := repo.GetItem(ctx, b.tournamentID, ref); !errors.Is(err, handicap.ErrItemNotFound) {
		t.Errorf("拿別屆的 ref err = %v,想要 ErrItemNotFound", err)
	}
}

// ── 預算 ────────────────────────────────────────────────────────

func TestGrantBudget(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)

	t.Run("依段位差發放且可重複呼叫", func(t *testing.T) {
		f := newFixture(t, defaultOpt())
		b, err := svc.GrantBudget(ctx, f.matchRef)
		if err != nil {
			t.Fatalf("GrantBudget: %v", err)
		}
		if b == nil || b.Budget != 24 || b.PlayerID != f.holderID {
			t.Fatalf("預算 = %+v,想要 24 BP 給 player=%d", b, f.holderID)
		}
		if b.PlayerPublicID == "" {
			t.Error("PlayerPublicID 不應為空(對外不露內部 id)")
		}
		again, err := svc.GrantBudget(ctx, f.matchRef)
		if err != nil {
			t.Fatalf("重複 GrantBudget: %v", err)
		}
		if again.Budget != 24 {
			t.Errorf("重複發放 = %d,想要 24", again.Budget)
		}
	})

	t.Run("同段對決不建列", func(t *testing.T) {
		opt := defaultOpt()
		opt.p2Rank = 1
		f := newFixture(t, opt)
		b, err := svc.GrantBudget(ctx, f.matchRef)
		if err != nil || b != nil {
			t.Fatalf("同段應回 nil, nil,實際 %+v / %v", b, err)
		}
		if _, err := repo.FindMatchBudget(ctx, f.matchID); !errors.Is(err, handicap.ErrNoBudget) {
			t.Errorf("不該有預算列,err = %v", err)
		}
	})

	t.Run("抽籤後改段位要失敗出聲", func(t *testing.T) {
		f := newFixture(t, defaultOpt())
		if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
			t.Fatalf("GrantBudget: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE activity.tournament_players SET rank_level = 2 WHERE id = $1`, f.holderID); err != nil {
			t.Fatalf("改段位: %v", err)
		}
		_, err := svc.GrantBudget(ctx, f.matchRef)
		if !errors.Is(err, handicap.ErrBudgetConflict) {
			t.Errorf("err = %v,想要 ErrBudgetConflict", err)
		}
	})
}

// TestInsertBudgetConflictSurfaces 驗證 23505 沒有被吞掉。
func TestInsertBudgetConflictSurfaces(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())

	if _, err := repo.InsertBudget(ctx, f.matchID, f.holderID, 24); err != nil {
		t.Fatalf("InsertBudget: %v", err)
	}
	_, err := repo.InsertBudget(ctx, f.matchID, f.holderID, 99)
	if !errors.Is(err, handicap.ErrBudgetExists) {
		t.Fatalf("err = %v,想要 ErrBudgetExists", err)
	}
	got, err := repo.GetBudget(ctx, f.matchID, f.holderID)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if got.Budget != 24 {
		t.Errorf("budget = %d,想要 24(衝突不得覆蓋)", got.Budget)
	}
}

// TestSetSpentTranslatesFailures 驗證兩種失敗都有明確的 sentinel,
// 而且被 CHECK 擋下的那次不留半套寫入。
func TestSetSpentTranslatesFailures(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	item := items["測試_十點"]

	if err := repo.SetSpent(ctx, f.matchID, f.holderID, 1); !errors.Is(err, handicap.ErrNoBudget) {
		t.Errorf("沒有預算列時 err = %v,想要 ErrNoBudget", err)
	}
	if _, err := repo.InsertBudget(ctx, f.matchID, f.holderID, 24); err != nil {
		t.Fatalf("InsertBudget: %v", err)
	}

	// 在同一個 tx 裡先插一列再超支寫入:CHECK 擋下之後整個 tx 回滾,
	// 不能留下「選擇已存在但 spent 沒跟上」的半套狀態。
	err := repo.WithTx(ctx, func(ctx context.Context, r handicap.Repository) error {
		if _, err := r.InsertSelection(ctx, handicap.NewSelection{
			MatchID: f.matchID, PlayerID: f.holderID, ItemID: item.ID, Cost: item.Cost,
		}); err != nil {
			return err
		}
		return r.SetSpent(ctx, f.matchID, f.holderID, 25)
	})
	if !errors.Is(err, handicap.ErrBudgetInconsistent) {
		t.Fatalf("超支 err = %v,想要 ErrBudgetInconsistent", err)
	}
	if n := countSelections(t, f.matchID, f.holderID, true); n != 0 {
		t.Errorf("回滾後仍有 %d 列選擇,想要 0", n)
	}
	assertSpentConsistent(t, f.matchID, f.holderID)
}

// ── 選購與退選 ──────────────────────────────────────────────────

// TestSelectSameItemThreeTimes 驗證重複購買同一項目成立,且三列各自獨立。
func TestSelectSameItemThreeTimes(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}

	refs := map[string]bool{}
	for i := 1; i <= 3; i++ {
		res, err := svc.Select(ctx, handicap.SelectParams{
			MatchPublicID: f.matchRef, PlayerID: f.holderID,
			ItemRef: items["測試_四點"].Ref, TargetNote: fmt.Sprintf("第 %d 次", i),
		})
		if err != nil {
			t.Fatalf("第 %d 次購買: %v", i, err)
		}
		if refs[res.Selection.PublicID] {
			t.Fatalf("第 %d 次的 public_id 與先前重複", i)
		}
		refs[res.Selection.PublicID] = true
		if res.Selection.TargetNote != fmt.Sprintf("第 %d 次", i) {
			t.Errorf("target_note = %q", res.Selection.TargetNote)
		}
		if res.Selection.ItemRef != items["測試_四點"].Ref {
			t.Errorf("ItemRef = %q,想要項目的 public_id", res.Selection.ItemRef)
		}
		if res.Budget.Spent != int64(4*i) {
			t.Errorf("spent = %d,想要 %d", res.Budget.Spent, 4*i)
		}
		assertSpentConsistent(t, f.matchID, f.holderID)
	}

	sels, err := repo.ListSelections(ctx, f.matchID, f.holderID)
	if err != nil {
		t.Fatalf("ListSelections: %v", err)
	}
	if len(sels) != 3 {
		t.Fatalf("選擇數 = %d,想要 3", len(sels))
	}

	// 退掉中間那一列:只有它變成 voided,另外兩列不受影響。
	if _, err := svc.VoidSelection(ctx, handicap.VoidParams{
		SelectionPublicID: sels[1].PublicID, PlayerID: f.holderID,
	}); err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	assertSpentConsistent(t, f.matchID, f.holderID)
	if n := countSelections(t, f.matchID, f.holderID, false); n != 2 {
		t.Errorf("未作廢選擇 = %d,想要 2", n)
	}
	if n := countSelections(t, f.matchID, f.holderID, true); n != 3 {
		t.Errorf("含作廢共 %d 列,想要 3(退選不刪列)", n)
	}

	// 重複退選必須回 ErrSelectionAlreadyVoided,不是默默成功也不是查無此筆。
	_, err = svc.VoidSelection(ctx, handicap.VoidParams{
		SelectionPublicID: sels[1].PublicID, PlayerID: f.holderID,
	})
	if !errors.Is(err, handicap.ErrSelectionAlreadyVoided) {
		t.Errorf("重複退選 err = %v,想要 ErrSelectionAlreadyVoided", err)
	}
	if err := repo.MarkSelectionVoided(ctx, sels[1].ID); !errors.Is(err, handicap.ErrSelectionAlreadyVoided) {
		t.Errorf("MarkSelectionVoided 重複 err = %v,想要 ErrSelectionAlreadyVoided", err)
	}
}

// TestSelectionWithoutBudgetBlockedByFK 驗證外鍵是應用層檢查之外的第二道保險。
func TestSelectionWithoutBudgetBlockedByFK(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())

	_, err := repo.InsertSelection(ctx, handicap.NewSelection{
		MatchID: f.matchID, PlayerID: f.holderID,
		ItemID: items["測試_四點"].ID, Cost: 4,
	})
	if !errors.Is(err, handicap.ErrNoBudget) {
		t.Errorf("沒有預算卻插得進去,err = %v,想要 ErrNoBudget", err)
	}
}

// TestSelectExhaustsBudget 驗證 BP 不足由規則層擋下,spent 停在正確的值。
func TestSelectExhaustsBudget(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := svc.Select(ctx, handicap.SelectParams{
			MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
		}); err != nil {
			t.Fatalf("第 %d 次購買: %v", i+1, err)
		}
	}
	_, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
	})
	if !errors.Is(err, handicap.ErrInsufficientBP) {
		t.Fatalf("第三次購買 err = %v,想要 ErrInsufficientBP", err)
	}
	assertSpentConsistent(t, f.matchID, f.holderID)
	if n := countSelections(t, f.matchID, f.holderID, true); n != 2 {
		t.Errorf("選擇列數 = %d,想要 2(失敗的那次不留痕跡)", n)
	}
}

// ── 封盤與可見性 ────────────────────────────────────────────────

// TestLockIsIrreversible 驗證封盤不可逆:第二次回 ErrAlreadyLocked,不是默默成功。
func TestLockIsIrreversible(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	if _, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
	}); err != nil {
		t.Fatalf("Select: %v", err)
	}

	view, err := svc.Lock(ctx, f.matchRef)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !view.Revealed || view.LockedAt == nil || len(view.Selections) != 1 {
		t.Fatalf("封盤後的檢視不正確:%+v", view)
	}
	if view.HolderPlayerPublicID == "" || view.ConstrainedPlayerPublicID == "" {
		t.Error("封盤後雙方 public_id 都應該有值")
	}

	if _, err := svc.Lock(ctx, f.matchRef); !errors.Is(err, handicap.ErrAlreadyLocked) {
		t.Errorf("重複封盤 err = %v,想要 ErrAlreadyLocked", err)
	}
	if err := repo.LockHandicaps(ctx, f.matchID, *view.LockedAt); !errors.Is(err, handicap.ErrAlreadyLocked) {
		t.Errorf("LockHandicaps 重複 err = %v,想要 ErrAlreadyLocked", err)
	}

	// 封盤後 status 必須一起變成 locked(matches_locked_at_check 逼出來的)。
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM activity.matches WHERE id = $1`, f.matchID).Scan(&status); err != nil {
		t.Fatalf("讀 status: %v", err)
	}
	if status != "locked" {
		t.Errorf("status = %q,想要 locked", status)
	}

	// 封盤後不可再增減。
	if _, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_四點"].Ref,
	}); !errors.Is(err, handicap.ErrHandicapLocked) {
		t.Errorf("封盤後購買 err = %v,想要 ErrHandicapLocked", err)
	}
	if _, err := svc.VoidSelection(ctx, handicap.VoidParams{
		SelectionPublicID: view.Selections[0].PublicID, PlayerID: f.holderID,
	}); !errors.Is(err, handicap.ErrHandicapLocked) {
		t.Errorf("封盤後退選 err = %v,想要 ErrHandicapLocked", err)
	}
}

// TestVisibilityBeforeAndAfterLock 驗證封盤前非本人拿不到 Budget 與清單。
//
// 這條界線由伺服器守,不是前端隱藏:封盤前連「花了多少」都不給對手,
// 那會反推出對方還剩幾 BP,等於提前洩漏還會被加上什麼限制。
func TestVisibilityBeforeAndAfterLock(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}
	if _, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
	}); err != nil {
		t.Fatalf("Select: %v", err)
	}

	// 對手視角:知道有讓武、知道是誰施加,但看不到內容與預算。
	opponent, err := svc.MatchHandicaps(ctx, f.matchRef, f.otherID)
	if err != nil {
		t.Fatalf("MatchHandicaps(對手): %v", err)
	}
	if opponent.Budget != nil || len(opponent.Selections) != 0 || opponent.Revealed {
		t.Fatalf("封盤前對手看到了內容:%+v", opponent)
	}
	if opponent.HolderPlayerPublicID == "" {
		t.Error("施加者的 public_id 封盤前就該公開(前端要顯示「對方有讓武」)")
	}
	// 匿名觀眾同樣看不到。
	anon, err := svc.MatchHandicaps(ctx, f.matchRef, 0)
	if err != nil {
		t.Fatalf("MatchHandicaps(觀眾): %v", err)
	}
	if anon.Budget != nil || len(anon.Selections) != 0 {
		t.Fatalf("封盤前觀眾看到了內容:%+v", anon)
	}
	// 本人看得到自己的。
	mine, err := svc.MyBudget(ctx, f.matchRef, f.holderID)
	if err != nil {
		t.Fatalf("MyBudget: %v", err)
	}
	if !mine.HasBudget || len(mine.Selections) != 1 || !mine.Editable {
		t.Fatalf("本人看不到自己的讓武:%+v", mine)
	}
	// 高段位方沒有預算,而且這件事不是錯誤。
	theirs, err := svc.MyBudget(ctx, f.matchRef, f.otherID)
	if err != nil {
		t.Fatalf("MyBudget(對手): %v", err)
	}
	if theirs.HasBudget || theirs.LockedReason == "" {
		t.Fatalf("高段位方不該有預算:%+v", theirs)
	}

	if _, err := svc.Lock(ctx, f.matchRef); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	revealed, err := svc.MatchHandicaps(ctx, f.matchRef, f.otherID)
	if err != nil {
		t.Fatalf("MatchHandicaps(封盤後): %v", err)
	}
	if !revealed.Revealed || revealed.Budget == nil || len(revealed.Selections) != 1 {
		t.Fatalf("封盤後對手仍看不到內容:%+v", revealed)
	}
}
