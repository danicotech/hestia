package readpg_test

import (
	"context"
	"fmt"
	"testing"
)

// /profile 與 /leaderboard 的讀取側。對真 Postgres 跑:這裡的重點就是
// JOIN 條件與排序本身,mock 掉等於什麼都沒測。

// seedCommunity 建一個社群(不指派 ruleset,即 M1 常態:走預設曲線)。
func seedCommunity(t *testing.T, name string) (id int64, publicID string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.communities (public_id, name)
		 VALUES (gen_random_uuid()::text, $1) RETURNING id, public_id`, name,
	).Scan(&id, &publicID)
	if err != nil {
		t.Fatalf("建社群: %v", err)
	}
	return id, publicID
}

func seedXP(t *testing.T, userID, communityID, amount int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_xp (user_id, community_id, xp) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, community_id) DO UPDATE SET xp = EXCLUDED.xp`,
		userID, communityID, amount); err != nil {
		t.Fatalf("塞 XP: %v", err)
	}
}

// seedBadge 發一枚徽章給某人。徽章不是獨立系統,是 category='badge' 的物品。
func seedBadge(t *testing.T, userID int64, name string) {
	t.Helper()
	ctx := context.Background()
	var defID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO platform.item_definitions (public_id, name, category, rarity, bind_on_acquire)
		 VALUES (gen_random_uuid()::text, $1, 'badge', 'rare', true) RETURNING id`, name,
	).Scan(&defID)
	if err != nil {
		t.Fatalf("建徽章定義: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.item_instances (public_id, definition_id, owner_id, bound, acquired_at)
		 VALUES (gen_random_uuid()::text, $1, $2, true, now())`, defID, userID); err != nil {
		t.Fatalf("發徽章: %v", err)
	}
}

func TestSummary_IncludesXPAndLevel(t *testing.T) {
	ctx := context.Background()
	uid, _ := newUser(t, "summary-xp")
	cid, _ := seedCommunity(t, "summary-community")
	seedXP(t, uid, cid, 2700) // schemas/24 對照表:2700 = 10 級

	sum, err := svc.Summary(ctx, uid)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum.XP) != 1 {
		t.Fatalf("應有一個社群的 XP,得到 %d", len(sum.XP))
	}
	got := sum.XP[0]
	if got.Level != 10 {
		t.Fatalf("2700 XP 應是 10 級,得到 %d", got.Level)
	}
	// 進度條需要兩個數字才畫得出來;只給等級的話呈現層得自己算門檻,
	// 那就會有第二份曲線實作。
	if got.XPIntoLevel != 0 {
		t.Fatalf("剛好踩在門檻上,本級進度應為 0,得到 %d", got.XPIntoLevel)
	}
	if got.XPForLevel <= 0 {
		t.Fatalf("本級需求應為正數,得到 %d", got.XPForLevel)
	}
	if got.CommunityName != "summary-community" {
		t.Fatalf("社群名 = %q", got.CommunityName)
	}
}

// 沒指派 ruleset 的社群(M1 常態)照樣要算得出等級,不能整支失敗。
func TestSummary_CommunityWithoutRulesetStillHasLevel(t *testing.T) {
	ctx := context.Background()
	uid, _ := newUser(t, "summary-noruleset")
	cid, _ := seedCommunity(t, "no-ruleset")
	seedXP(t, uid, cid, 150)

	sum, err := svc.Summary(ctx, uid)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.XP[0].Level != 2 {
		t.Fatalf("150 XP 應是 2 級,得到 %d", sum.XP[0].Level)
	}
}

// 多數人一開始什麼都沒有。那不是錯誤,而且 /profile 必須照樣顯示得出來 ——
// 新人是最需要看到「我有帳號了」的那一群。
func TestSummary_EmptyUserIsNotAnError(t *testing.T) {
	ctx := context.Background()
	uid, publicID := newUser(t, "summary-empty")

	sum, err := svc.Summary(ctx, uid)
	if err != nil {
		t.Fatalf("空白使用者不該失敗: %v", err)
	}
	if sum.Profile.PublicID != publicID {
		t.Fatalf("檔案 public_id = %q,want %q", sum.Profile.PublicID, publicID)
	}
	if sum.Pet != nil {
		t.Fatal("沒有寵物時應為 nil")
	}
	if len(sum.Badges) != 0 || len(sum.XP) != 0 {
		t.Fatalf("不該憑空生出徽章或 XP:badges=%d xp=%d", len(sum.Badges), len(sum.XP))
	}
}

func TestSummary_Badges(t *testing.T) {
	ctx := context.Background()
	uid, _ := newUser(t, "summary-badges")
	seedBadge(t, uid, "首次簽到")
	seedBadge(t, uid, "百日連續")

	sum, err := svc.Summary(ctx, uid)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum.Badges) != 2 {
		t.Fatalf("應有兩枚徽章,得到 %d", len(sum.Badges))
	}
	// 最新的在前:embed 會被截斷時,先被看到的該是剛拿到的那枚
	if sum.Badges[0].Name != "百日連續" {
		t.Fatalf("應依取得時間新到舊,得到 %q 在最前", sum.Badges[0].Name)
	}
	if sum.Badges[0].Rarity != "rare" {
		t.Fatalf("稀有度沒有帶出來:%q", sum.Badges[0].Rarity)
	}
}

// 別人的徽章不該出現在我的檔案裡。這條看起來理所當然,
// 但 owner_id 條件打錯的話所有人的檔案會長一樣,而且沒有人會立刻發現。
func TestSummary_BadgesAreScopedToOwner(t *testing.T) {
	ctx := context.Background()
	mine, _ := newUser(t, "summary-mine")
	theirs, _ := newUser(t, "summary-theirs")
	seedBadge(t, theirs, "別人的徽章")

	sum, err := svc.Summary(ctx, mine)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum.Badges) != 0 {
		t.Fatalf("不該看到別人的徽章,得到 %d 枚", len(sum.Badges))
	}
}

func TestLeaderboard_OrdersByXPDescending(t *testing.T) {
	ctx := context.Background()
	cid, cPublic := seedCommunity(t, "leaderboard-community")
	for i, amount := range []int64{500, 3000, 1200} {
		uid, _ := newUser(t, fmt.Sprintf("lb-%d", i))
		seedXP(t, uid, cid, amount)
	}

	entries, err := svc.Leaderboard(ctx, cPublic, 10)
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("應有三個人,得到 %d", len(entries))
	}
	if entries[0].XP != 3000 || entries[1].XP != 1200 || entries[2].XP != 500 {
		t.Fatalf("排序錯了:%d %d %d", entries[0].XP, entries[1].XP, entries[2].XP)
	}
	// 名次由後端給:各端自己數會在分頁時錯開
	if entries[0].Rank != 1 || entries[2].Rank != 3 {
		t.Fatalf("名次錯了:%d ... %d", entries[0].Rank, entries[2].Rank)
	}
	if entries[0].Level <= entries[2].Level {
		t.Fatalf("XP 高的等級應該較高:%d vs %d", entries[0].Level, entries[2].Level)
	}
}

// 別的社群的人不該出現。社群過濾漏掉的話,小社群的排行榜會被大社群洗掉。
func TestLeaderboard_ScopedToCommunity(t *testing.T) {
	ctx := context.Background()
	mineID, minePublic := seedCommunity(t, "lb-mine")
	otherID, _ := seedCommunity(t, "lb-other")

	u1, _ := newUser(t, "lb-in")
	seedXP(t, u1, mineID, 100)
	u2, _ := newUser(t, "lb-out")
	seedXP(t, u2, otherID, 99999)

	entries, err := svc.Leaderboard(ctx, minePublic, 10)
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("只該有自己社群的人,得到 %d", len(entries))
	}
	if entries[0].XP != 100 {
		t.Fatalf("撈到別的社群的人了:xp=%d", entries[0].XP)
	}
}

// limit 超出範圍夾到預設值,不讓一個顯示參數搞垮整個指令。
func TestLeaderboard_ClampsLimit(t *testing.T) {
	ctx := context.Background()
	cid, cPublic := seedCommunity(t, "lb-clamp")
	for i := 0; i < 15; i++ {
		uid, _ := newUser(t, fmt.Sprintf("lb-clamp-%d", i))
		seedXP(t, uid, cid, int64(i+1)*10)
	}
	for _, limit := range []int32{0, -5, 9999} {
		entries, err := svc.Leaderboard(ctx, cPublic, limit)
		if err != nil {
			t.Fatalf("limit=%d 不該失敗: %v", limit, err)
		}
		if len(entries) != 10 {
			t.Fatalf("limit=%d 應夾到預設 10,得到 %d", limit, len(entries))
		}
	}
}

// 空社群不是錯誤:剛建好的社群本來就沒有人。
func TestLeaderboard_EmptyCommunity(t *testing.T) {
	ctx := context.Background()
	_, cPublic := seedCommunity(t, "lb-empty")
	entries, err := svc.Leaderboard(ctx, cPublic, 10)
	if err != nil {
		t.Fatalf("空社群不該失敗: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("應為空,得到 %d", len(entries))
	}
}
