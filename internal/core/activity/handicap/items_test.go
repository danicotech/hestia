package handicap

import (
	"encoding/json"
	"errors"
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

// 目錄裡的 referee_note 目前 34 項都有內容,但缺了不該讓載入失敗 ——
// 新增一項而裁判說明還沒寫是待辦事項,不是資料損壞。
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

// 34 個 key 唯一、形狀合法、前綴等於分類 —— 與 DB 的 handicap_items_key_format_check
// 同一條規則。loadSeed 已經驗過(SeedItems 回 nil 錯就是證據),這裡再逐項釘住
// 是為了讓「哪一項壞了」在測試輸出裡直接看得到,而不是一句「seed 解析失敗」。
func TestSeedItems_KeysUniqueAndWellFormed(t *testing.T) {
	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	seen := map[string]bool{}
	for _, it := range items {
		if !keyPattern.MatchString(it.Key) {
			t.Errorf("項目 %q 的 key %q 不符合 ^[a-z]+\\.[a-z0-9_]+$", it.Name, it.Key)
		}
		if !strings.HasPrefix(it.Key, string(it.Category)+".") {
			t.Errorf("項目 %q 的 key %q 前綴與分類 %q 不一致", it.Name, it.Key, it.Category)
		}
		if seen[it.Key] {
			t.Errorf("key %q 重複", it.Key)
		}
		seen[it.Key] = true
	}
	if len(seen) != 34 {
		t.Errorf("唯一 key 應為 34 個,實際 %d 個", len(seen))
	}

	// migration 00006 的補值對照表用的正是這 34 個 key;抽幾個釘住,
	// 改 seed 的 key 而沒改 migration 時這裡會紅。
	for _, key := range []string{
		"weapon.designated_art", "weapon.random_art", "weapon.armory_46",
		"poison.scramble_keys", "victory.survive_90s", "victory.survive_120s",
		"rule.opening_line", "rule.loser_request",
	} {
		if !seen[key] {
			t.Errorf("seed 裡找不到 key %q(migration 00006 的對照表有它)", key)
		}
	}
}

// validateKey 是 DB CHECK 在程式側的鏡像,壞形狀要在載入 seed 時就擋下。
func TestValidateKey(t *testing.T) {
	cases := []struct {
		key string
		cat Category
		ok  bool
	}{
		{"weapon.designated_art", CategoryWeapon, true},
		{"victory.survive_90s", CategoryVictory, true},
		{"weapon.designated_art", CategorySkill, false},  // 前綴與分類不一致
		{"Weapon.designated_art", CategoryWeapon, false}, // 大寫
		{"weapon.Designated", CategoryWeapon, false},
		{"weapon", CategoryWeapon, false},          // 沒有點
		{"weapon.", CategoryWeapon, false},         // 點後為空
		{"weapon.a.b", CategoryWeapon, false},      // 兩個點
		{"weapon.has-dash", CategoryWeapon, false}, // 連字號
		{"", CategoryWeapon, false},
	}
	for _, tc := range cases {
		err := validateKey(tc.key, tc.cat)
		if (err == nil) != tc.ok {
			t.Errorf("validateKey(%q, %s) err = %v,期望 ok=%v", tc.key, tc.cat, err, tc.ok)
		}
	}
}

// params 的四項帶參數的項目值要對,其餘 30 項必須是零值(seed 不該偷塞東西)。
func TestSeedItems_Params(t *testing.T) {
	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	want := map[string]ItemParams{
		"weapon.random_art":    {Draw: DrawWuxue},
		"weapon.armory_46":     {AppliesToBoth: true},
		"poison.scramble_keys": {Draw: DrawDirectionKeys},
		"victory.survive_90s":  {Seconds: 90},
		"victory.survive_120s": {Seconds: 120},
	}
	for _, it := range items {
		w, ok := want[it.Key]
		if !ok {
			w = ItemParams{}
		}
		if it.Params != w {
			t.Errorf("%s 的 params = %+v,期望 %+v", it.Key, it.Params, w)
		}
	}
}

// ItemParams 的解析是 params 鍵的唯一權威:未知鍵、壞值域都要回錯,JSONB 不是垃圾桶。
func TestItemParams_Parse(t *testing.T) {
	t.Run("合法", func(t *testing.T) {
		cases := map[string]ItemParams{
			``:                               {},
			`{}`:                             {},
			`{"seconds": 90}`:                {Seconds: 90},
			`{"draw": "wuxue"}`:              {Draw: DrawWuxue},
			`{"draw": "direction_keys"}`:     {Draw: DrawDirectionKeys},
			`{"applies_to_both": true}`:      {AppliesToBoth: true},
			`{"seconds":120,"draw":"wuxue"}`: {Seconds: 120, Draw: DrawWuxue},
		}
		for in, want := range cases {
			got, err := ParseItemParams([]byte(in))
			if err != nil {
				t.Errorf("%s: %v", in, err)
				continue
			}
			if got != want {
				t.Errorf("%s = %+v,期望 %+v", in, got, want)
			}
		}
	})

	t.Run("不合法", func(t *testing.T) {
		for _, in := range []string{
			`{"second": 90}`,          // 打錯鍵
			`{"seconds": 90, "x": 1}`, // 多一個未登記的鍵
			`{"draw": "coin"}`,        // 未知的抽選類型
			`{"seconds": -1}`,         // 負數
			`{"seconds": "90"}`,       // 型別錯
			`[]`,                      // 不是物件
			`{} {}`,                   // 多餘內容
		} {
			if _, err := ParseItemParams([]byte(in)); !errors.Is(err, ErrInvalidItemParams) {
				t.Errorf("%s 應回 ErrInvalidItemParams,實際 %v", in, err)
			}
		}
	})

	t.Run("JSON 往返", func(t *testing.T) {
		for _, p := range []ItemParams{{}, {Seconds: 90}, {Draw: DrawWuxue, AppliesToBoth: true}} {
			s, err := p.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			back, err := ParseItemParams([]byte(s))
			if err != nil {
				t.Fatalf("ParseItemParams(%s): %v", s, err)
			}
			if back != p {
				t.Errorf("%+v → %s → %+v", p, s, back)
			}
		}
		if s, _ := (ItemParams{}).JSON(); s != "{}" {
			t.Errorf("零值應序列化成 {},實際 %s", s)
		}
	})

	// seed 走的是同一條解析:塞一個未知鍵進 ItemSpec 的 params 必須失敗。
	t.Run("經 ItemSpec 也擋", func(t *testing.T) {
		var sp ItemSpec
		err := json.Unmarshal([]byte(`{"category":"weapon","key":"weapon.x","name":"x","cost":1,"params":{"bogus":1}}`), &sp)
		if !errors.Is(err, ErrInvalidItemParams) {
			t.Fatalf("期望 ErrInvalidItemParams,實際 %v", err)
		}
	})
}

// targetNoteRequired 以 key 定址,刪項就會靜靜失效 —— 那時候「指定對手武學」
// 會變成可以不指定武學。這個測試就是為了讓刪項當場變紅。
func TestTargetNoteRequired_KeysExistInSeed(t *testing.T) {
	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	inSeed := map[string]bool{}
	for _, it := range items {
		inSeed[it.Key] = true
	}
	for key := range targetNoteRequired {
		if !inSeed[key] {
			t.Errorf("targetNoteRequired 有一個 seed 裡不存在的 key %q(被刪掉了?)", key)
		}
	}
}

// 哪些項目需要指定內容是**判斷**,不是機械推導,所以把結論整份釘住:
// 判準是「效果需要買方指名一個對象,不指名就無法執行」。
func TestRequiresTargetNote_ExactSet(t *testing.T) {
	want := map[string]bool{
		"weapon.designated_art":      true,
		"skill.ban_two":              true,
		"skill.ban_one":              true,
		"skill.remap":                true,
		"skill.ban_arcane_one":       true,
		"poison.swap_keys":           true,
		"rule.designated_appearance": true,
		"rule.opening_line":          true,
		"weapon.random_art":          false, // 隨機的意思正是不指定
		"skill.ban_arcane_all":       false, // 對象是全部
		"defense.no_dodge":           false,
		"victory.survive_90s":        false, // 改寫的是判定方式,沒有可指名的對象
		"rule.trash_talk":            false, // 狠話由對手自己想,那正是這項好玩的地方
		"rule.loser_request":         false, // 要求可以賽後再提
	}

	items, err := SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	got := map[string]bool{}
	required := 0
	for _, it := range items {
		got[it.Key] = RequiresTargetNote(it.Key)
		if got[it.Key] {
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
	// Item 上的方法與函式是同一個權威。
	if !(Item{Key: "weapon.designated_art"}).RequiresTargetNote() {
		t.Error("Item.RequiresTargetNote 沒有走 key")
	}
}

// 沒登記的項目預設不強制:漏擋的後果是裁判臨場問一句,誤擋的後果是選手買不了東西。
func TestRequiresTargetNote_UnknownItemDefaultsFalse(t *testing.T) {
	if RequiresTargetNote("rule.added_later") {
		t.Fatal("未登記的項目不該強制填寫指定內容")
	}
}
