package main

import (
	"context"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

const channelB = "222222222222222222"

// 設定頻道用途的價值在於「公告真的貼得到那裡」,所以測的是資料庫裡的結果。

func TestSetChannel_CreatesPurposeAndBackfillsChannel(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}

	// 刻意沒有先跑 register-channel:補建是這條指令的責任,
	// 不該為了設一個公告頻道還要記得先註冊。
	if err := setChannel(ctx, pool, []string{
		"--guild", guildA, "--purpose", "announcements", "--channel", channelA, "--name", "公告",
	}); err != nil {
		t.Fatalf("set-channel: %v", err)
	}

	q := db.New(pool)
	space, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	if err != nil {
		t.Fatalf("查 space: %v", err)
	}

	got, err := q.GetChannelForPurpose(ctx, db.GetChannelForPurposeParams{
		SpaceID: space.ID, Purpose: "announcements",
	})
	if err != nil {
		t.Fatalf("查用途: %v", err)
	}
	if got != channelA {
		t.Fatalf("頻道 = %q, want %q", got, channelA)
	}

	channels, err := q.ListSpaceChannels(ctx, space.ID)
	if err != nil {
		t.Fatalf("ListSpaceChannels: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("應自動補建一列 space_channels,got %d", len(channels))
	}
	// 補建用的是欄位預設,與「未註冊頻道」的既有行為相同:不記內容、計 XP。
	if channels[0].LogMessages {
		t.Fatal("補建不該順手打開訊息內容記錄")
	}
	if !channels[0].GrantXp {
		t.Fatal("補建不該關掉 XP")
	}
}

// 改公告頻道是常見操作(開了新頻道、舊的封存)。
// 重跑要覆蓋而不是噴 UNIQUE,也不能留下兩個公告頻道。
func TestSetChannel_RerunMoves(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	base := []string{"--guild", guildA, "--purpose", "announcements"}

	if err := setChannel(ctx, pool, append(append([]string{}, base...), "--channel", channelA)); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if err := setChannel(ctx, pool, append(append([]string{}, base...), "--channel", channelB)); err != nil {
		t.Fatalf("第二次: %v", err)
	}

	q := db.New(pool)
	space, _ := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	purposes, err := q.ListSpacePurposes(ctx, space.ID)
	if err != nil {
		t.Fatalf("ListSpacePurposes: %v", err)
	}
	if len(purposes) != 1 {
		t.Fatalf("同一個用途不該有兩列,got %d", len(purposes))
	}
	if purposes[0].ChannelExternalID != channelB {
		t.Fatalf("應指向新頻道 %q,got %q", channelB, purposes[0].ChannelExternalID)
	}
}

// 打錯用途是最容易發生的事。外鍵要擋下來,而不是寫進去之後公告安靜地不見。
func TestSetChannel_RejectsUnknownPurpose(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	err := setChannel(ctx, pool, []string{
		"--guild", guildA, "--purpose", "announcement", // 少了 s
		"--channel", channelA,
	})
	if err == nil {
		t.Fatal("不存在的用途應被拒絕")
	}
	// 錯誤訊息要告訴人有哪些可用,否則只能自己猜
	if want := "announcements"; !strings.Contains(err.Error(), want) {
		t.Fatalf("錯誤訊息應列出可用用途,got %q", err.Error())
	}
}

// 兩個不同用途可以指向同一個頻道(小伺服器常常只有一個公告區)。
func TestSetChannel_TwoPurposesSameChannel(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	for _, p := range []string{"announcements", "rules"} {
		if err := setChannel(ctx, pool, []string{
			"--guild", guildA, "--purpose", p, "--channel", channelA,
		}); err != nil {
			t.Fatalf("設定 %s: %v", p, err)
		}
	}
	q := db.New(pool)
	space, _ := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	purposes, err := q.ListSpacePurposes(ctx, space.ID)
	if err != nil {
		t.Fatalf("ListSpacePurposes: %v", err)
	}
	if len(purposes) != 2 {
		t.Fatalf("兩個用途都該在,got %d", len(purposes))
	}
}

func TestClearChannel(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	if err := setChannel(ctx, pool, []string{
		"--guild", guildA, "--purpose", "announcements", "--channel", channelA,
	}); err != nil {
		t.Fatalf("set-channel: %v", err)
	}
	if err := clearChannel(ctx, pool, []string{"--guild", guildA, "--purpose", "announcements"}); err != nil {
		t.Fatalf("clear-channel: %v", err)
	}

	q := db.New(pool)
	space, _ := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: guildA,
	})
	purposes, _ := q.ListSpacePurposes(ctx, space.ID)
	if len(purposes) != 0 {
		t.Fatalf("取消後不該還有用途,got %d", len(purposes))
	}
	// 取消用途不該連頻道註冊一起刪掉:那是另一件事(訊息記錄白名單)
	channels, _ := q.ListSpaceChannels(ctx, space.ID)
	if len(channels) != 1 {
		t.Fatalf("頻道註冊應保留,got %d", len(channels))
	}
}

// 沒設定過就取消不是錯誤(重跑部署腳本是常態)
func TestClearChannel_Idempotent(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	if err := registerSpace(ctx, pool, []string{"--guild", guildA}); err != nil {
		t.Fatalf("register-space: %v", err)
	}
	if err := clearChannel(ctx, pool, []string{"--guild", guildA, "--purpose", "rules"}); err != nil {
		t.Fatalf("沒設定過就取消不該報錯: %v", err)
	}
}

func TestListPurposes(t *testing.T) {
	pool := testdb.Start(t)
	if err := listPurposes(context.Background(), pool); err != nil {
		t.Fatalf("list-purposes: %v", err)
	}
}
