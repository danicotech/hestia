package playpg_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/play"
)

// 開箱是**水龍頭**:獎品由系統生出來。所以這裡測的重點是產出速率的控制 ——
// 有限獎池不能超發,而且每一次都要留痕。

func newBox(t *testing.T, communityID int64, cost int64) (id int64, publicID string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.loot_boxes (public_id, community_id, name, cost_currency, cost_amount)
		 VALUES (gen_random_uuid()::text, $1, '測試箱', 'coin', $2) RETURNING id, public_id`,
		communityID, cost).Scan(&id, &publicID)
	if err != nil {
		t.Fatalf("建箱子: %v", err)
	}
	return id, publicID
}

func addTokenPrize(t *testing.T, boxID int64, amount int64, weight int, remaining *int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO platform.loot_box_items (box_id, reward_kind, reward_ref, amount, weight, remaining)
		 VALUES ($1, 'tokens', 'coin', $2, $3, $4)`, boxID, amount, weight, remaining); err != nil {
		t.Fatalf("加獎品: %v", err)
	}
}

func addItemPrize(t *testing.T, boxID int64, category string, weight int, remaining *int) string {
	t.Helper()
	ctx := context.Background()
	var defPublic string
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.item_definitions (public_id, name, category, rarity)
		 VALUES (gen_random_uuid()::text, '獎品', $1, 'rare') RETURNING public_id`,
		category).Scan(&defPublic); err != nil {
		t.Fatalf("建物品定義: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.loot_box_items (box_id, reward_kind, reward_ref, weight, remaining)
		 VALUES ($1, 'item', $2, $3, $4)`, boxID, defPublic, weight, remaining); err != nil {
		t.Fatalf("加物品獎: %v", err)
	}
	return defPublic
}

func TestOpenBox_ChargesAndPays(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	boxID, boxPublic := newBox(t, c, 100)
	addTokenPrize(t, boxID, 250, 1, nil) // 唯一獎品,必中

	res, err := svc.OpenBox(ctx, play.OpenParams{
		UserID: u, BoxPublicID: boxPublic, IdempotencyKey: fmt.Sprintf("box-%d", u),
	})
	if err != nil {
		t.Fatalf("OpenBox: %v", err)
	}
	if res.Amount != 250 {
		t.Fatalf("應中 250,得到 %d", res.Amount)
	}
	// 1000 − 100(開箱費)+ 250(獎品)
	if got := balanceOf(t, u); got != 1150 {
		t.Fatalf("餘額應為 1150,得到 %d", got)
	}
}

// 有限獎池就是 max_supply 在抽獎場景的實現。超發的話「全世界只有 1 個」
// 這個設計形同虛設,而且發出去的東西收不回來。
func TestOpenBox_FinitePoolCannotOverDraw(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 10_000)
	boxID, boxPublic := newBox(t, c, 10)
	one := 1
	// 有限獎池只能放物品:00028 的 CHECK 不允許點數限量,
	// 因為「剩 30 個 100 點」在語意上很奇怪(點數沒有稀缺性)。
	addItemPrize(t, boxID, "collectible", 1, &one) // 全世界只有一個

	if _, err := svc.OpenBox(ctx, play.OpenParams{
		UserID: u, BoxPublicID: boxPublic, IdempotencyKey: fmt.Sprintf("fin-%d-1", u),
	}); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	_, err := svc.OpenBox(ctx, play.OpenParams{
		UserID: u, BoxPublicID: boxPublic, IdempotencyKey: fmt.Sprintf("fin-%d-2", u),
	})
	if !errors.Is(err, play.ErrPoolEmpty) {
		t.Fatalf("抽完應回 ErrPoolEmpty,得到 %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT remaining FROM platform.loot_box_items WHERE box_id = $1`, boxID,
	).Scan(&remaining); err != nil {
		t.Fatalf("讀庫存: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("庫存應為 0,得到 %d —— 負數代表超發了", remaining)
	}
}

// 兩個人同時抽最後一件。FOR UPDATE 沒有生效的話兩個人都會拿到,
// 而這種 bug 只在有人真的同時按下去時才出現。
func TestOpenBox_ConcurrentLastItem(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := newCommunity(t)
	boxID, boxPublic := newBox(t, c, 10)
	one := 1
	addItemPrize(t, boxID, "collectible", 1, &one)

	const racers = 6
	users := make([]int64, racers)
	for i := range users {
		users[i] = newUser(t)
		fund(t, users[i], 1000)
	}

	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = svc.OpenBox(ctx, play.OpenParams{
				UserID: users[i], BoxPublicID: boxPublic,
				IdempotencyKey: fmt.Sprintf("race-%d-%d", boxID, i),
			})
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("只有一件獎品,卻有 %d 個人抽到", wins)
	}
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT remaining FROM platform.loot_box_items WHERE box_id = $1`, boxID).Scan(&remaining); err != nil {
		t.Fatalf("讀庫存: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("庫存應為 0,得到 %d", remaining)
	}
}

// 抽到寵物時要一併建 pet_states。少了那一列,寵物看得到卻養不了,
// 而且 /pet 查不到它 —— 那是「發了但等於沒發」。
func TestOpenBox_PetGetsState(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	boxID, boxPublic := newBox(t, c, 10)
	addItemPrize(t, boxID, "pet", 1, nil)

	if _, err := svc.OpenBox(ctx, play.OpenParams{
		UserID: u, BoxPublicID: boxPublic, IdempotencyKey: fmt.Sprintf("pet-%d", u),
	}); err != nil {
		t.Fatalf("OpenBox: %v", err)
	}
	pets, err := svc.ListPets(ctx, u)
	if err != nil {
		t.Fatalf("ListPets: %v", err)
	}
	if len(pets) != 1 {
		t.Fatalf("抽到寵物後應該看得到牠,得到 %d 隻", len(pets))
	}
	if pets[0].Level != 1 {
		t.Fatalf("新寵物應為 1 級,得到 %d", pets[0].Level)
	}
}

// 一般物品不該長出寵物狀態 —— 那會讓背包裡的消耗品出現在 /pet 清單上。
func TestOpenBox_NonPetItemHasNoPetState(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	boxID, boxPublic := newBox(t, c, 10)
	addItemPrize(t, boxID, "collectible", 1, nil)

	if _, err := svc.OpenBox(ctx, play.OpenParams{
		UserID: u, BoxPublicID: boxPublic, IdempotencyKey: fmt.Sprintf("coll-%d", u),
	}); err != nil {
		t.Fatalf("OpenBox: %v", err)
	}
	pets, err := svc.ListPets(ctx, u)
	if err != nil {
		t.Fatalf("ListPets: %v", err)
	}
	if len(pets) != 0 {
		t.Fatalf("收藏品不該變成寵物,得到 %d 隻", len(pets))
	}
}

func TestOpenBox_DisabledBox(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	boxID, boxPublic := newBox(t, c, 10)
	addTokenPrize(t, boxID, 50, 1, nil)
	if _, err := pool.Exec(ctx,
		`UPDATE platform.loot_boxes SET enabled = false WHERE id = $1`, boxID); err != nil {
		t.Fatalf("停用箱子: %v", err)
	}

	_, err := svc.OpenBox(ctx, play.OpenParams{
		UserID: u, BoxPublicID: boxPublic, IdempotencyKey: fmt.Sprintf("dis-%d", u),
	})
	if !errors.Is(err, play.ErrDisabled) {
		t.Fatalf("停用的箱子應回 ErrDisabled,得到 %v", err)
	}
}

func TestOpenBox_UnknownBox(t *testing.T) {
	setup(t)
	_, err := svc.OpenBox(context.Background(), play.OpenParams{
		UserID: newUser(t), BoxPublicID: "沒有這個箱子", IdempotencyKey: "x",
	})
	if !errors.Is(err, play.ErrNotFound) {
		t.Fatalf("應回 ErrNotFound,得到 %v", err)
	}
}
