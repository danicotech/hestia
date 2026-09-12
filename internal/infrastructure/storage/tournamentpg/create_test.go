package tournamentpg_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// newCommunity 建一個真的 platform.communities 列。
//
// 不能拿一個隨便的數字當 community_id:InsertTournament 是由 public_id 反查的,
// 查不到就撞 NOT NULL,而那正是 ErrCommunityNotFound 的來源。
func newCommunity(t *testing.T) string {
	t.Helper()
	var publicID string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.communities (public_id, name)
		 VALUES (gen_random_uuid()::text, '測試社群') RETURNING public_id`,
	).Scan(&publicID)
	if err != nil {
		t.Fatalf("建測試社群: %v", err)
	}
	return publicID
}

// createParams 是一次合法的建立請求(呼叫端要自己填 ActorUserID)。
func createParams(t *testing.T, community string) tournament.CreateTournamentParams {
	t.Helper()
	items, err := handicap.SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	cfg, err := tournament.MarshalConfig(tournament.DefaultConfig())
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	return tournament.CreateTournamentParams{
		CommunityPublicID: community,
		Slug:              fmt.Sprintf("test-%d", nextSeq()),
		Name:              "測試賽事",
		SignupBonus:       100,
		ConfigRaw:         cfg,
		HandicapItems:     items,
		Reason:            "測試",
	}
}

// countItems 數某一屆的讓武項目。
func countItems(t *testing.T, tournamentID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity.handicap_items WHERE tournament_id = $1`, tournamentID,
	).Scan(&n); err != nil {
		t.Fatalf("數讓武項目: %v", err)
	}
	return n
}

// tournamentExists 回報這個 slug 有沒有留下一屆賽事。
func tournamentExists(t *testing.T, slug string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity.tournaments WHERE slug = $1`, slug,
	).Scan(&n); err != nil {
		t.Fatalf("查賽事: %v", err)
	}
	return n > 0
}

// 建立成功後:賽事停在報名期,而且該屆**恰好**有目錄那幾項讓武,
// 每一項的分類、價格、裁判說明都與目錄逐字相同。
//
// 逐項比對而不是只數 34:少一項會被數量抓到,但「價格全部寫成 0」不會 ——
// 而那會讓整屆的讓武變成免費。
func TestCreate_InstallsFullCatalogue(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	p := createParams(t, newCommunity(t))
	p.ActorUserID = actor

	got, err := svc.Create(ctx, p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Phase != tournament.PhaseSignup {
		t.Errorf("phase = %s,新賽事必須停在報名期", got.Phase)
	}
	if got.PublicID == "" || got.ID == 0 {
		t.Errorf("賽事沒有 id / public_id:%+v", got)
	}
	if got.Slug != p.Slug || got.Name != p.Name || got.SignupBonus != 100 {
		t.Errorf("寫進去的欄位對不上:%+v", got)
	}
	if _, err := tournament.ParseConfig(got.ConfigRaw); err != nil {
		t.Errorf("寫進去的 config 解不回來: %v", err)
	}

	if n := countItems(t, got.ID); n != len(p.HandicapItems) {
		t.Fatalf("該屆有 %d 項讓武,目錄是 %d 項", n, len(p.HandicapItems))
	}

	rows, err := pool.Query(ctx,
		`SELECT category, name, description, referee_note, cost, repeatable, sort_order, public_id
		   FROM activity.handicap_items WHERE tournament_id = $1`, got.ID)
	if err != nil {
		t.Fatalf("讀讓武項目: %v", err)
	}
	defer rows.Close()

	type row struct {
		desc, note         *string
		cost               int64
		repeatable         bool
		sortOrder          int32
		publicID, category string
	}
	stored := map[string]row{}
	for rows.Next() {
		var category, name, publicID string
		var r row
		if err := rows.Scan(&category, &name, &r.desc, &r.note,
			&r.cost, &r.repeatable, &r.sortOrder, &publicID); err != nil {
			t.Fatalf("掃讓武項目: %v", err)
		}
		r.publicID, r.category = publicID, category
		stored[category+"/"+name] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀讓武項目: %v", err)
	}

	seenIDs := map[string]bool{}
	for _, spec := range p.HandicapItems {
		key := string(spec.Category) + "/" + spec.Name
		r, ok := stored[key]
		if !ok {
			t.Errorf("目錄裡的 %q 沒有進資料庫", key)
			continue
		}
		if r.cost != spec.Cost {
			t.Errorf("%q 的 cost = %d,目錄是 %d", key, r.cost, spec.Cost)
		}
		if r.repeatable != spec.Repeatable || r.sortOrder != spec.SortOrder {
			t.Errorf("%q 的 repeatable/sort_order = %v/%d,目錄是 %v/%d",
				key, r.repeatable, r.sortOrder, spec.Repeatable, spec.SortOrder)
		}
		if text(r.desc) != spec.Description {
			t.Errorf("%q 的說明 = %q,目錄是 %q", key, text(r.desc), spec.Description)
		}
		wantNote := ""
		if spec.RefereeNote != nil {
			wantNote = *spec.RefereeNote
		}
		if text(r.note) != wantNote {
			t.Errorf("%q 的 referee_note = %q,目錄是 %q", key, text(r.note), wantNote)
		}
		// public_id 是 ULID,而且逐列不同 —— 對外只出現它(鐵則 5)。
		if len(r.publicID) != 26 {
			t.Errorf("%q 的 public_id = %q,不是 26 字元的 ULID", key, r.publicID)
		}
		if seenIDs[r.publicID] {
			t.Errorf("public_id %q 重複", r.publicID)
		}
		seenIDs[r.publicID] = true
	}

	// 建賽事是裁判的破壞性操作,必須留痕。
	if n := auditCount(t, "tournament.created", got.ID); n != 1 {
		t.Errorf("稽核紀錄 %d 筆,想要 1 筆", n)
	}
}

// 代號撞了要回 ErrSlugTaken 而不是 500 —— 那是使用者輸入造成的結果。
// 而且失敗的那一次不該留下任何東西。
func TestCreate_DuplicateSlug(t *testing.T) {
	setup(t)
	ctx := context.Background()
	community := newCommunity(t)
	actor := newUser(t)

	first := createParams(t, community)
	first.ActorUserID = actor
	created, err := svc.Create(ctx, first)
	if err != nil {
		t.Fatalf("第一次 Create: %v", err)
	}

	second := createParams(t, community)
	second.ActorUserID = actor
	second.Slug = first.Slug
	if _, err := svc.Create(ctx, second); !errors.Is(err, tournament.ErrSlugTaken) {
		t.Fatalf("err = %v,想要 ErrSlugTaken", err)
	}
	// 第二次失敗不該替第一屆多裝一份項目,也不該多一筆稽核。
	if n := countItems(t, created.ID); n != len(first.HandicapItems) {
		t.Errorf("第一屆的項目變成 %d 項", n)
	}
	if n := auditCount(t, "tournament.created", created.ID); n != 1 {
		t.Errorf("稽核紀錄 %d 筆,想要 1 筆", n)
	}
}

// 社群不存在時子查詢回 NULL,失敗發生在 community_id 的 NOT NULL 上。
// 分辨得出來才能回一句裁判看得懂的話。
func TestCreate_CommunityNotFound(t *testing.T) {
	setup(t)
	p := createParams(t, "01J0DOESNOTEXIST")
	p.ActorUserID = newUser(t)
	if _, err := svc.Create(context.Background(), p); !errors.Is(err, tournament.ErrCommunityNotFound) {
		t.Fatalf("err = %v,想要 ErrCommunityNotFound", err)
	}
	if tournamentExists(t, p.Slug) {
		t.Fatal("社群不存在卻留下了一屆賽事")
	}
}

// 項目寫入中途失敗 → 賽事也不能存在。
//
// 這是 Create 唯一真正要靠 transaction 保證的事:一屆有賽事列卻沒有讓武項目
// 的賽事不會有任何東西出聲,它要等到選手走到選讓武那一步才顯現,
// 而那時候已經報名、評段、抽籤完了。
func TestCreate_ItemFailureLeavesNothing(t *testing.T) {
	setup(t)
	community := newCommunity(t)
	actor := newUser(t)

	cases := map[string]func([]handicap.ItemSpec) []handicap.ItemSpec{
		// CHECK (cost > 0):資料庫直接擋下,整筆 tx 進 aborted。
		"價格為 0": func(items []handicap.ItemSpec) []handicap.ItemSpec {
			items[7].Cost = 0
			return items
		},
		// CHECK (category IN (...)):同上,失敗在 INSERT 那一句。
		"分類不合法": func(items []handicap.ItemSpec) []handicap.ItemSpec {
			items[3].Category = "不存在的分類"
			return items
		},
		// UNIQUE (tournament_id, category, name) + ON CONFLICT DO NOTHING:
		// 這一種**不會**讓 SQL 報錯,只會少插一列。要靠「插入數必須等於目錄長度」
		// 那道檢查把它變成失敗 —— 否則就是一屆悄悄少一項讓武的賽事。
		"目錄裡有重複的項目": func(items []handicap.ItemSpec) []handicap.ItemSpec {
			items[5] = items[4]
			return items
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := createParams(t, community)
			p.ActorUserID = actor
			p.HandicapItems = mutate(p.HandicapItems)

			if _, err := svc.Create(context.Background(), p); err == nil {
				t.Fatal("壞掉的目錄卻建立成功了")
			}
			if tournamentExists(t, p.Slug) {
				t.Fatal("項目沒裝成,賽事卻留了下來 —— 那正是 transaction 要擋的半套")
			}
		})
	}
}

// 稽核寫不進去(actor 不存在於 platform.users)時,賽事與項目一起消失。
//
// admin_audit_logs.actor_user_id 是 FK,假 id 會讓 insert 失敗。這條路徑
// 驗的是「稽核與資料變更同一個 tx」這個承諾 —— 少了它,會存在一屆
// 沒有人知道是誰開的賽事。
func TestCreate_AuditFailureRollsBackEverything(t *testing.T) {
	setup(t)
	p := createParams(t, newCommunity(t))
	p.ActorUserID = 1 << 40 // 不存在的使用者

	if _, err := svc.Create(context.Background(), p); err == nil {
		t.Fatal("稽核寫不進去卻建立成功了")
	}
	if tournamentExists(t, p.Slug) {
		t.Fatal("稽核失敗,賽事卻留了下來")
	}
}

// 沒有裁判身分一律拒絕:這個動作要進 admin_audit_logs,沒有 actor 就記不出
// 「是誰開的」。擋在進資料庫之前。
func TestCreate_ActorRequired(t *testing.T) {
	setup(t)
	p := createParams(t, newCommunity(t))
	if _, err := svc.Create(context.Background(), p); !errors.Is(err, tournament.ErrActorRequired) {
		t.Fatalf("err = %v,想要 ErrActorRequired", err)
	}
	if tournamentExists(t, p.Slug) {
		t.Fatal("沒有裁判身分卻建立了賽事")
	}
}

// 兩個裁判同時用同一個代號開賽事:只能有一屆成立,輸的那個要拿到
// ErrSlugTaken 而不是一個看不懂的資料庫錯誤。
//
// 這一段必須對真的 Postgres 跑:擋住第二個的是 UNIQUE 約束本身,
// 而不是任何應用層的檢查(後者在並發下擋不住)。
func TestCreate_ConcurrentSameSlug(t *testing.T) {
	setup(t)
	ctx := context.Background()
	community := newCommunity(t)
	actor := newUser(t)
	slug := fmt.Sprintf("race-%d", nextSeq())

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	ids := make([]int64, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := createParams(t, community)
			p.ActorUserID = actor
			p.Slug = slug
			got, err := svc.Create(ctx, p)
			results[i] = err
			ids[i] = got.ID
		}()
	}
	wg.Wait()

	won := 0
	var winnerID int64
	for i, err := range results {
		switch {
		case err == nil:
			won++
			winnerID = ids[i]
		case errors.Is(err, tournament.ErrSlugTaken):
		default:
			t.Errorf("第 %d 次的錯誤是 %v,想要 ErrSlugTaken", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d 次並發建立成功了 %d 次,想要 1 次", n, won)
	}
	items, err := handicap.SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	// 贏的那一屆要拿到完整的目錄:輸的那幾個 tx 全部 rollback,
	// 不該在它身上留下半份項目。
	if got := countItems(t, winnerID); got != len(items) {
		t.Fatalf("勝出的賽事有 %d 項讓武,想要 %d 項", got, len(items))
	}
}

// text 取可空字串欄位的值;NULL 視為空字串(ItemSpec 那側的約定相同)。
func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
