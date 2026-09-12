package playpg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/play"
)

// 寵物(schemas/25)。擁有權在 item_instances、狀態在 pet_states,
// 所以這裡測的是狀態那一半:出戰、改名、以及「別人的寵物碰不到」。

// givePet 直接發一隻寵物給某人,回傳它的 public_id。
func givePet(t *testing.T, userID int64, name string) string {
	t.Helper()
	ctx := context.Background()
	var defID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.item_definitions (public_id, name, category, rarity)
		 VALUES (gen_random_uuid()::text, $1, 'pet', 'common') RETURNING id`, name,
	).Scan(&defID); err != nil {
		t.Fatalf("建寵物定義: %v", err)
	}
	var instID int64
	var publicID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.item_instances (public_id, definition_id, owner_id, acquired_at)
		 VALUES (gen_random_uuid()::text, $1, $2, now()) RETURNING id, public_id`, defID, userID,
	).Scan(&instID, &publicID); err != nil {
		t.Fatalf("發寵物: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.pet_states (item_instance_id, owner_id) VALUES ($1, $2)`,
		instID, userID); err != nil {
		t.Fatalf("建寵物狀態: %v", err)
	}
	return publicID
}

func TestDeployPet_OnlyOneAtATime(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	a := givePet(t, u, "橘貓")
	b := givePet(t, u, "白貓")

	if err := svc.DeployPet(ctx, u, a); err != nil {
		t.Fatalf("出戰 A: %v", err)
	}
	// 換一隻。舊的要自動收起來 —— 部分唯一索引擋得住,但直接撞它
	// 使用者看到的會是「發生未知錯誤」。
	if err := svc.DeployPet(ctx, u, b); err != nil {
		t.Fatalf("換出戰 B: %v", err)
	}

	pets, err := svc.ListPets(ctx, u)
	if err != nil {
		t.Fatalf("ListPets: %v", err)
	}
	deployed := 0
	for _, p := range pets {
		if p.Deployed {
			deployed++
			if p.PublicID != b {
				t.Fatalf("出戰的應該是 B,得到 %s", p.PublicID)
			}
		}
	}
	if deployed != 1 {
		t.Fatalf("同時只能有一隻出戰,得到 %d", deployed)
	}
}

// 別人的寵物不能動。owner 檢查漏掉的話,任何人都能把別人的寵物改名或換下場。
func TestDeployPet_RejectsOtherOwner(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mine, theirs := newUser(t), newUser(t)
	pet := givePet(t, theirs, "別人的貓")

	if err := svc.DeployPet(ctx, mine, pet); !errors.Is(err, play.ErrNotOwner) {
		t.Fatalf("應回 ErrNotOwner,得到 %v", err)
	}
	if err := svc.RenamePet(ctx, mine, pet, "改你的名"); !errors.Is(err, play.ErrNotOwner) {
		t.Fatalf("改名也應回 ErrNotOwner,得到 %v", err)
	}
}

func TestRenamePet(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	pet := givePet(t, u, "原本的名字")

	if err := svc.RenamePet(ctx, u, pet, "小橘"); err != nil {
		t.Fatalf("改名: %v", err)
	}
	pets, _ := svc.ListPets(ctx, u)
	if pets[0].Name != "小橘" {
		t.Fatalf("名字 = %q", pets[0].Name)
	}

	// 空字串 = 還原成物品定義的名稱
	if err := svc.RenamePet(ctx, u, pet, "  "); err != nil {
		t.Fatalf("還原: %v", err)
	}
	pets, _ = svc.ListPets(ctx, u)
	if pets[0].Name != "原本的名字" {
		t.Fatalf("還原後名字 = %q", pets[0].Name)
	}
}

// 暱稱是外界完全可控的字串。不限長度的話,一個人可以用暱稱
// 把 /profile 的 embed 撐爆 —— 與 event_logs 的摘要截斷是同一類問題。
func TestRenamePet_RejectsOverlongNickname(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	pet := givePet(t, u, "貓")

	err := svc.RenamePet(ctx, u, pet, strings.Repeat("長", 200))
	if !errors.Is(err, play.ErrInvalidParams) {
		t.Fatalf("超長暱稱應回 ErrInvalidParams,得到 %v", err)
	}
	// 多位元組字要按字數算不是位元組數:24 個中文字應該過
	if err := svc.RenamePet(ctx, u, pet, strings.Repeat("貓", 24)); err != nil {
		t.Fatalf("24 個中文字應該可以: %v", err)
	}
}

func TestListPets_DeployedFirst(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	givePet(t, u, "第一隻")
	second := givePet(t, u, "第二隻")
	if err := svc.DeployPet(ctx, u, second); err != nil {
		t.Fatalf("出戰: %v", err)
	}

	pets, err := svc.ListPets(ctx, u)
	if err != nil {
		t.Fatalf("ListPets: %v", err)
	}
	if len(pets) != 2 {
		t.Fatalf("應有兩隻,得到 %d", len(pets))
	}
	if !pets[0].Deployed {
		t.Fatal("出戰中的應該排最前面")
	}
}

func TestDeployPet_UnknownPet(t *testing.T) {
	setup(t)
	err := svc.DeployPet(context.Background(), newUser(t), "沒有這隻")
	if !errors.Is(err, play.ErrNotFound) {
		t.Fatalf("應回 ErrNotFound,得到 %v", err)
	}
}
