package playpg_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/play"
)

// 抽獎不是水龍頭:獎品由管理員先掏出來,總量不變。
// 所以這裡測的是另一組性質 —— 一人一次、開獎只能一次、獎品不會發兩份。

func newGiveaway(t *testing.T, communityID int64, prize, entryCost int64, winners int) string {
	t.Helper()
	ctx := context.Background()
	// space 是 NOT NULL,建一個給它掛
	var spaceID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.community_spaces (public_id, community_id, provider, external_id)
		 VALUES (gen_random_uuid()::text, $1, 'discord', gen_random_uuid()::text) RETURNING id`,
		communityID).Scan(&spaceID); err != nil {
		t.Fatalf("建 space: %v", err)
	}
	var creator int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id, display_name)
		 VALUES (gen_random_uuid()::text, 'host') RETURNING id`).Scan(&creator); err != nil {
		t.Fatalf("建主辦人: %v", err)
	}
	var publicID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.giveaways
		   (public_id, community_id, space_id, title, prize_kind, prize_ref, prize_amount,
		    winner_count, entry_cost, opens_at, closes_at, created_by)
		 VALUES (gen_random_uuid()::text, $1, $2, '測試抽獎', 'tokens', 'coin', $3,
		         $4, $5, now(), now() + interval '1 day', $6)
		 RETURNING public_id`,
		communityID, spaceID, prize, winners, entryCost, creator).Scan(&publicID); err != nil {
		t.Fatalf("建抽獎: %v", err)
	}
	return publicID
}

func TestEnterGiveaway_ChargesEntryFee(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	g := newGiveaway(t, c, 5000, 100, 1)

	if err := svc.EnterGiveaway(ctx, u, g); err != nil {
		t.Fatalf("EnterGiveaway: %v", err)
	}
	if got := balanceOf(t, u); got != 900 {
		t.Fatalf("報名費應扣 100,餘額得到 %d", got)
	}
}

// 重複報名要被擋,而且**不能重複收費**。
// 先寫報名紀錄再扣費就是為了這個:順序反過來的話,重複報名的人
// 會先被多扣一次錢才發現自己報過了。
func TestEnterGiveaway_NoDoubleCharge(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	g := newGiveaway(t, c, 5000, 100, 1)

	if err := svc.EnterGiveaway(ctx, u, g); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	err := svc.EnterGiveaway(ctx, u, g)
	if !errors.Is(err, play.ErrAlreadyEntered) {
		t.Fatalf("重複報名應回 ErrAlreadyEntered,得到 %v", err)
	}
	if got := balanceOf(t, u); got != 900 {
		t.Fatalf("重複報名不該再扣費,餘額得到 %d", got)
	}
}

func TestEnterGiveaway_FreeEntry(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	g := newGiveaway(t, c, 5000, 0, 1) // 免費參加,使用者連餘額都沒有

	if err := svc.EnterGiveaway(ctx, u, g); err != nil {
		t.Fatalf("免費抽獎應該不需要餘額: %v", err)
	}
}

func TestDrawGiveaway_PaysWinners(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := newCommunity(t)
	g := newGiveaway(t, c, 5000, 0, 2)

	users := make([]int64, 5)
	for i := range users {
		users[i] = newUser(t)
		if err := svc.EnterGiveaway(ctx, users[i], g); err != nil {
			t.Fatalf("報名: %v", err)
		}
	}

	res, err := svc.DrawGiveaway(ctx, g)
	if err != nil {
		t.Fatalf("DrawGiveaway: %v", err)
	}
	if len(res.Winners) != 2 {
		t.Fatalf("應抽出 2 位,得到 %d", len(res.Winners))
	}

	paid := 0
	for _, u := range users {
		if balanceOf(t, u) == 5000 {
			paid++
		}
	}
	if paid != 2 {
		t.Fatalf("應有 2 人拿到獎金,得到 %d", paid)
	}
}

// 開獎只能一次。兩個人同時按,會抽出兩組贏家而獎品只有一份 ——
// FOR UPDATE + 鎖後再檢查狀態就是為了這個。
func TestDrawGiveaway_OnlyOnce(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := newCommunity(t)
	g := newGiveaway(t, c, 1000, 0, 1)
	u := newUser(t)
	if err := svc.EnterGiveaway(ctx, u, g); err != nil {
		t.Fatalf("報名: %v", err)
	}

	if _, err := svc.DrawGiveaway(ctx, g); err != nil {
		t.Fatalf("第一次開獎: %v", err)
	}
	_, err := svc.DrawGiveaway(ctx, g)
	if !errors.Is(err, play.ErrClosed) {
		t.Fatalf("重複開獎應回 ErrClosed,得到 %v", err)
	}
	if got := balanceOf(t, u); got != 1000 {
		t.Fatalf("獎金只該發一次,餘額得到 %d", got)
	}
}

func TestDrawGiveaway_Concurrent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := newCommunity(t)
	g := newGiveaway(t, c, 1000, 0, 1)
	u := newUser(t)
	if err := svc.EnterGiveaway(ctx, u, g); err != nil {
		t.Fatalf("報名: %v", err)
	}

	const racers = 5
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.DrawGiveaway(ctx, g)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("同時開獎應只有一次成功,得到 %d", ok)
	}
	if got := balanceOf(t, u); got != 1000 {
		t.Fatalf("獎金只該發一次,餘額得到 %d", got)
	}
}

// 沒有人報名也要能結案。留著 open 的話它會永遠掛在清單上。
func TestDrawGiveaway_NoEntries(t *testing.T) {
	setup(t)
	ctx := context.Background()
	g := newGiveaway(t, newCommunity(t), 1000, 0, 1)

	res, err := svc.DrawGiveaway(ctx, g)
	if err != nil {
		t.Fatalf("沒人報名不該失敗: %v", err)
	}
	if len(res.Winners) != 0 {
		t.Fatalf("不該憑空生出贏家,得到 %d", len(res.Winners))
	}
	// 已結案 → 不會再出現在進行中清單
	if _, err := svc.DrawGiveaway(ctx, g); !errors.Is(err, play.ErrClosed) {
		t.Fatalf("應已結案,得到 %v", err)
	}
}

func TestListGiveaways_ShowsEntryCount(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := newCommunity(t)
	g := newGiveaway(t, c, 1000, 0, 1)
	for i := 0; i < 3; i++ {
		if err := svc.EnterGiveaway(ctx, newUser(t), g); err != nil {
			t.Fatalf("報名: %v", err)
		}
	}

	list, err := svc.ListGiveaways(ctx, c)
	if err != nil {
		t.Fatalf("ListGiveaways: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("應有一場進行中,得到 %d", len(list))
	}
	if list[0].Entries != 3 {
		t.Fatalf("參加人數應為 3,得到 %d", list[0].Entries)
	}
	if !list[0].ClosesAt.After(time.Now()) {
		t.Fatal("進行中的抽獎截止時間應在未來")
	}
}
