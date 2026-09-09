package main

import (
	"context"
	"testing"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 這支 CLI 的價值在於「跑完之後 bot 真的能記錄」,所以測試對真的 Postgres 跑,
// 驗的是資料庫裡的結果,不是印出來的字。

const (
	guildA   = "123456789012345678"
	guildB   = "876543210987654321"
	channelA = "111111111111111111"
)

func TestRequireSnowflake(t *testing.T) {
	t.Parallel()
	// 打錯一個字元不會有任何錯誤——只會建出一個永遠對不到的 space,
	// 然後每一則記錄都被拒。所以形狀在進 DB 之前就要擋下來。
	bad := []string{
		"", "abc", "12345", "12345678901234567890123",
		"1234567890123456789a", " 123456789012345678", "123456789012345678 ",
		"<#123456789012345678>", // 從 Discord 直接複製提及字串的常見錯誤
	}
	for _, v := range bad {
		if err := requireSnowflake("guild", v); err == nil {
			t.Errorf("%q 應該被拒絕", v)
		}
	}
	for _, v := range []string{guildA, "12345678901234567", "12345678901234567890"} {
		if err := requireSnowflake("guild", v); err != nil {
			t.Errorf("%q 應該被接受: %v", v, err)
		}
	}
}

func TestRegisterSpace_CreatesCommunityAndSpace(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()

	if err := registerSpace(ctx, pool, []string{"--guild", guildA, "--name", "測試伺服器"}); err != nil {
		t.Fatalf("register-space: %v", err)
	}

	q := db.New(pool)
	space, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	if err != nil {
		t.Fatalf("註冊後查不到 space: %v", err)
	}
	if space.PublicID == "" || len(space.PublicID) != 26 {
		t.Fatalf("public_id 應該是 26 字元 ULID,got %q", space.PublicID)
	}
	if space.Name == nil || *space.Name != "測試伺服器" {
		t.Fatalf("name = %v", space.Name)
	}

	communities, err := q.ListCommunities(ctx)
	if err != nil {
		t.Fatalf("ListCommunities: %v", err)
	}
	if len(communities) != 1 {
		t.Fatalf("應該剛好建一個社群,got %d", len(communities))
	}
	if communities[0].ID != space.CommunityID {
		t.Fatal("space 沒有掛在新建的社群下")
	}
	// xp_ruleset 留白是刻意的:M1 還沒有 ruleset,GetCommunityXpConfig 會
	// LEFT JOIN 回 NULL 並採安全預設(無冷卻、無上限)。
	if communities[0].XpRulesetID != nil {
		t.Fatal("不該自作主張綁 xp_ruleset")
	}
}

// 重跑是常態(忘了跑過、寫進部署腳本)。噴 UNIQUE violation 或
// 默默建第二個社群都是壞的失敗方式。
func TestRegisterSpace_IsIdempotent(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := registerSpace(ctx, pool, []string{"--guild", guildA, "--name", "測試伺服器"}); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}

	q := db.New(pool)
	spaces, err := q.ListSpaces(ctx)
	if err != nil {
		t.Fatalf("ListSpaces: %v", err)
	}
	if len(spaces) != 1 {
		t.Fatalf("space 應該只有一個,got %d", len(spaces))
	}
	communities, err := q.ListCommunities(ctx)
	if err != nil {
		t.Fatalf("ListCommunities: %v", err)
	}
	if len(communities) != 1 {
		t.Fatalf("社群應該只有一個,got %d —— 重跑不該一直長出空社群", len(communities))
	}
}

func TestRegisterSpace_JoinsExistingCommunity(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	q := db.New(pool)

	if err := registerSpace(ctx, pool, []string{"--guild", guildA, "--name", "主伺服器"}); err != nil {
		t.Fatalf("第一個: %v", err)
	}
	communities, err := q.ListCommunities(ctx)
	if err != nil {
		t.Fatalf("ListCommunities: %v", err)
	}
	existing := communities[0].PublicID

	// 這是「一個社群多個 Discord 伺服器」的路徑——多伺服器支援不是理論,
	// 是這條指令要能走通。
	if err := registerSpace(ctx, pool, []string{
		"--guild", guildB, "--name", "副伺服器", "--community", existing,
	}); err != nil {
		t.Fatalf("第二個: %v", err)
	}

	spaces, err := q.ListSpaces(ctx)
	if err != nil {
		t.Fatalf("ListSpaces: %v", err)
	}
	if len(spaces) != 2 {
		t.Fatalf("應該有兩個 space,got %d", len(spaces))
	}
	if spaces[0].CommunityID != spaces[1].CommunityID {
		t.Fatal("兩個 space 應該掛在同一個社群下")
	}
	if after, _ := q.ListCommunities(ctx); len(after) != 1 {
		t.Fatalf("不該多建社群,got %d", len(after))
	}
}

func TestRegisterSpace_UnknownCommunityFails(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()

	err := registerSpace(ctx, pool, []string{
		"--guild", guildA, "--community", "01ZZZZZZZZZZZZZZZZZZZZZZZZ",
	})
	if err == nil {
		t.Fatal("不存在的社群應該回錯")
	}
	// 失敗時不該留下任何東西:半途建好的 space 會讓下一次重跑
	// 走進「已經註冊過了」的分支,把錯誤永久化。
	q := db.New(pool)
	if spaces, _ := q.ListSpaces(ctx); len(spaces) != 0 {
		t.Fatalf("失敗後不該留下 space,got %d", len(spaces))
	}
	if communities, _ := q.ListCommunities(ctx); len(communities) != 0 {
		t.Fatalf("失敗後不該留下社群,got %d", len(communities))
	}
}

func TestRegisterChannel(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}

	if err := registerChannel(ctx, pool, []string{
		"--guild", guildA, "--channel", channelA, "--kind", "text",
		"--name", "閒聊", "--log-messages",
	}); err != nil {
		t.Fatalf("register-channel: %v", err)
	}

	q := db.New(pool)
	space, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	if err != nil {
		t.Fatalf("查 space: %v", err)
	}
	channels, err := q.ListSpaceChannels(ctx, space.ID)
	if err != nil {
		t.Fatalf("ListSpaceChannels: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("應該有一個頻道,got %d", len(channels))
	}
	if !channels[0].LogMessages {
		t.Fatal("--log-messages 沒有生效")
	}
	if !channels[0].GrantXp {
		t.Fatal("預設應該計 XP")
	}
}

// 重跑帶不同旗標的語意是「改成這樣」,不是「已經有了就不管」。
func TestRegisterChannel_RerunOverwritesFlags(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	base := []string{"--guild", guildA, "--channel", channelA, "--kind", "text"}

	if err := registerChannel(ctx, pool, append(append([]string{}, base...), "--log-messages")); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if err := registerChannel(ctx, pool, append(append([]string{}, base...), "--no-xp")); err != nil {
		t.Fatalf("第二次: %v", err)
	}

	q := db.New(pool)
	space, _ := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	channels, err := q.ListSpaceChannels(ctx, space.ID)
	if err != nil {
		t.Fatalf("ListSpaceChannels: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("重跑不該長出第二列,got %d", len(channels))
	}
	if channels[0].LogMessages {
		t.Fatal("第二次沒帶 --log-messages,應該關掉")
	}
	if channels[0].GrantXp {
		t.Fatal("--no-xp 沒有生效")
	}
}

func TestRegisterChannel_RequiresSpace(t *testing.T) {
	pool := testdb.Start(t)
	err := registerChannel(context.Background(), pool, []string{
		"--guild", guildA, "--channel", channelA,
	})
	if err == nil {
		t.Fatal("space 不存在時應該回錯,而不是靜靜建一列孤兒頻道")
	}
}

func TestRegisterChannel_RejectsUnknownKind(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	err := registerChannel(ctx, pool, []string{
		"--guild", guildA, "--channel", channelA, "--kind", "stage",
	})
	if err == nil {
		t.Fatal("不合法的 kind 應該被拒絕")
	}
}

// list 在空資料庫上要給出下一步,而不是印出空白讓人不知道發生了什麼。
func TestListAll_EmptyDatabase(t *testing.T) {
	pool := testdb.Start(t)
	if err := listAll(context.Background(), pool); err != nil {
		t.Fatalf("list: %v", err)
	}
}

func TestListAll_AfterRegistration(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA, "--name", "測試"}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	if err := registerChannel(ctx, pool, []string{
		"--guild", guildA, "--channel", channelA, "--kind", "voice",
	}); err != nil {
		t.Fatalf("register-channel: %v", err)
	}
	if err := listAll(ctx, pool); err != nil {
		t.Fatalf("list: %v", err)
	}
}
