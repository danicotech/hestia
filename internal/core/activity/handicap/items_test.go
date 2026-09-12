package handicap

import (
	"strings"
	"testing"
)

// seed 是賽事規則本身,不是設定 —— 少一項、打錯一個價格,場上就會出現
// 一個沒有人講得清楚的限制。所以這裡把整份清單當契約測。
func TestSeedItems_Shape(t *testing.T) {
	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	if len(items) != 34 {
		t.Fatalf("讓武項目應為 34 項,實際 %d 項", len(items))
	}

	byCategory := map[Category]int{}
	for _, it := range items {
		if !it.Category.Valid() {
			t.Errorf("項目 %q 的分類 %q 不合法", it.Name, it.Category)
		}
		if it.Cost <= 0 {
			t.Errorf("項目 %q 的 cost = %d,必須為正", it.Name, it.Cost)
		}
		if strings.TrimSpace(it.Description) == "" {
			t.Errorf("項目 %q 沒有給選手看的說明", it.Name)
		}
		byCategory[it.Category]++
	}

	// 數量對照 schemas/20 的六大類表。
	want := map[Category]int{
		CategoryWeapon:  5,
		CategorySkill:   7,
		CategoryDefense: 6,
		CategoryPoison:  4,
		CategoryVictory: 5,
		CategoryRule:    7,
	}
	for c, n := range want {
		if byCategory[c] != n {
			t.Errorf("%s 類應有 %d 項,實際 %d 項", c, n, byCategory[c])
		}
	}
}

// referee_note 目前全是 null(待御風羽補)。這不該讓載入失敗 ——
// 缺裁判說明是待辦事項,不是資料損壞。
func TestSeedItems_MissingRefereeNoteIsNotAnError(t *testing.T) {
	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	for _, it := range items {
		if it.RefereeNote != nil && strings.TrimSpace(*it.RefereeNote) == "" {
			t.Errorf("項目 %q 的 referee_note 是空字串 —— 未填請留 null,兩者語意不同", it.Name)
		}
	}
}

// 回傳的是複本:呼叫端改了不能污染下一位呼叫者。
func TestSeedItems_ReturnsCopy(t *testing.T) {
	a, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	a[0].Cost = 99999
	b, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	if b[0].Cost == 99999 {
		t.Fatal("SeedItems 回傳了共用的底層陣列")
	}
}

// targetNoteRequired 是用 (category, name) 當鍵的一份表,改名就會靜靜失效 ——
// 那時候「指定對手武學」會變成可以不指定武學。這個測試就是為了讓改名當場變紅。
func TestTargetNoteRequired_KeysExistInSeed(t *testing.T) {
	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	inSeed := map[string]bool{}
	for _, it := range items {
		inSeed[itemKey(it.Category, it.Name)] = true
	}
	for key := range targetNoteRequired {
		if !inSeed[key] {
			t.Errorf("targetNoteRequired 有一個 seed 裡不存在的項目 %q(被改名或刪掉了?)", key)
		}
	}
}

// 哪些項目需要指定內容是**判斷**,不是機械推導,所以把結論整份釘住:
// 判準是「效果需要買方指名一個對象,不指名就無法執行」。
func TestRequiresTargetNote_ExactSet(t *testing.T) {
	want := map[string]bool{
		"weapon/指定對手武學":         true,
		"skill/禁用兩種技能":          true,
		"skill/禁用一種技能":          true,
		"skill/指定並更改技能位子":       true,
		"skill/禁用任意奇術":          true,
		"poison/互換任意兩個按鍵":       true,
		"rule/指定對手外觀":           true,
		"rule/指定對手開局時講一句話":      true,
		"weapon/隨機武學":           false, // 隨機的意思正是不指定
		"skill/禁用奇術":            false, // 對象是全部
		"defense/禁用迴避":          false,
		"victory/撐過 90 秒即獲勝":    false, // 改寫的是判定方式,沒有可指名的對象
		"rule/對手必須在開賽前落下一句狠話":   false, // 狠話由對手自己想,那正是這項好玩的地方
		"rule/對手若輸了必須聽從勝者的一個要求": false, // 要求可以賽後再提
	}

	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	got := map[string]bool{}
	required := 0
	for _, it := range items {
		key := itemKey(it.Category, it.Name)
		got[key] = RequiresTargetNote(it.Category, it.Name)
		if got[key] {
			required++
		}
	}
	if required != 8 {
		t.Errorf("需要指定內容的項目應為 8 項,實際 %d 項", required)
	}
	for key, wantReq := range want {
		gotReq, ok := got[key]
		if !ok {
			t.Errorf("seed 裡找不到項目 %q", key)
			continue
		}
		if gotReq != wantReq {
			t.Errorf("%q 的 RequiresTargetNote = %v,期望 %v", key, gotReq, wantReq)
		}
	}
}

// 沒登記的項目預設不強制:漏擋的後果是裁判臨場問一句,誤擋的後果是選手買不了東西。
func TestRequiresTargetNote_UnknownItemDefaultsFalse(t *testing.T) {
	if RequiresTargetNote(CategoryRule, "裁判之後才加的新項目") {
		t.Fatal("未登記的項目不該強制填寫指定內容")
	}
}
