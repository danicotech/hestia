package handicap

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// seedJSON 是《百業試鋒》讓武項目總表。
//
// 內嵌而不是讀檔:這份清單是賽事規則本身,不是設定。跟 binary 一起走才保證
// 「部署到哪裡,規則就是哪一份」—— 少一個「正式機上那個 JSON 是哪個版本」的問題。
//
//go:embed seed/handicap-items.json
var seedJSON []byte

// seedFile 是 JSON 的外層結構。$comment 與 version 刻意不解析:
// 前者是給人看的,後者目前沒有任何程式依賴它(真要版本化時是 migration 的事)。
type seedFile struct {
	Items []ItemSpec `json:"items"`
}

// loadSeed 只跑一次。解析失敗是編譯期就該發現的錯(檔案是內嵌的),
// 但仍然回 error 而不是 panic —— 讓呼叫端決定要不要讓整個服務起不來。
var loadSeed = sync.OnceValues(func() ([]ItemSpec, error) {
	var f seedFile
	if err := json.Unmarshal(seedJSON, &f); err != nil {
		return nil, fmt.Errorf("解析讓武項目 seed: %w", err)
	}
	if len(f.Items) == 0 {
		return nil, fmt.Errorf("讓武項目 seed 是空的")
	}
	seen := make(map[string]struct{}, len(f.Items))
	for i, it := range f.Items {
		if !it.Category.Valid() {
			return nil, fmt.Errorf("第 %d 項 %q 的分類 %q 不是六大類之一", i, it.Name, it.Category)
		}
		if it.Name == "" {
			return nil, fmt.Errorf("第 %d 項沒有名稱", i)
		}
		// cost > 0 是 DB 的 CHECK;在這裡先擋住,免得 seed 打錯字要等到 migration 才炸。
		if it.Cost <= 0 {
			return nil, fmt.Errorf("項目 %q 的 cost 必須為正,實際是 %d", it.Name, it.Cost)
		}
		key := itemKey(it.Category, it.Name)
		if _, dup := seen[key]; dup {
			// 對應 UNIQUE (tournament_id, category, name)。
			return nil, fmt.Errorf("項目 %s 重複", key)
		}
		seen[key] = struct{}{}
	}
	return f.Items, nil
})

// SeedItems 回傳內嵌的讓武項目定義。
//
// 回的是複本:呼叫端改了不會污染下一位呼叫者,而 sync.OnceValues 只解析一次。
func SeedItems() ([]ItemSpec, error) {
	items, err := loadSeed()
	if err != nil {
		return nil, err
	}
	out := make([]ItemSpec, len(items))
	copy(out, items)
	return out, nil
}

func itemKey(c Category, name string) string { return string(c) + "/" + name }

// targetNoteRequired 列出「必須填指定內容」的項目。
//
// 為什麼是程式裡的一份表而不是 handicap_items 的欄位:這個判斷是項目**語意**
// 的一部分(「指定對手武學」不填武學名就沒有意義),不是逐屆可調的設定。
// 放欄位的話,裁判在後台把它改掉就會讓一個沒有內容的限制進到場上,
// 而那要等到開賽前一刻裁判看著清單發呆時才會被發現。
//
// 判準只有一條:**這一項的效果需要買方指名一個對象,不指名就無法執行**。
//
//	指定對手武學          → 要指名是哪個武學
//	禁用一種 / 兩種技能   → 要指名是哪一種(兩種)
//	指定並更改技能位子    → 要指名哪個技能移到哪個鍵位
//	禁用任意奇術          → 「任意」的意思正是由買方指定一個
//	互換任意兩個按鍵      → 要指名是哪兩個鍵
//	指定對手外觀          → 要指名外觀
//	指定對手開局時講一句話 → 要寫出那八個字
//
// 反過來,下面這些**不**需要,因為對象是固定的或由對手自行決定:
//
//	隨機武學              → 隨機就是不指定,指定了反而變成另一個(20 BP)的項目
//	禁用奇術 / 普攻 / 處決 → 對象是「全部」
//	defense 全類          → 對象是固定的操作
//	victory 全類          → 改寫的是勝負判定,沒有可指名的對象
//	開賽前落下一句狠話    → 狠話的內容是對手自己想的,那正是這一項好玩的地方
//	輸了聽從勝者一個要求  → 要求可以賽後再提,不必在購買當下寫死
//
// 新增項目時若忘了登記,預設是 false(不強制)—— 選擇寬鬆的預設是因為
// 漏擋的後果是裁判臨場問一句,誤擋的後果是選手買不了東西。
var targetNoteRequired = map[string]struct{}{
	itemKeyWeaponDesignatedArt:   {},
	itemKeySkillBanTwo:           {},
	itemKeySkillBanOne:           {},
	itemKeySkillRemap:            {},
	itemKeySkillBanOneArcane:     {},
	itemKeyPoisonSwapKeys:        {},
	itemKeyRuleDesignateAppear:   {},
	itemKeyRuleDesignateOpenLine: {},
}

// 這些 key 與 seed JSON 的 (category, name) 必須逐字相同 ——
// items_test.go 會驗證每一個 key 都真的存在於 seed 裡,改名時測試會紅。
const (
	itemKeyWeaponDesignatedArt   = "weapon/指定對手武學"
	itemKeySkillBanTwo           = "skill/禁用兩種技能"
	itemKeySkillBanOne           = "skill/禁用一種技能"
	itemKeySkillRemap            = "skill/指定並更改技能位子"
	itemKeySkillBanOneArcane     = "skill/禁用任意奇術"
	itemKeyPoisonSwapKeys        = "poison/互換任意兩個按鍵"
	itemKeyRuleDesignateAppear   = "rule/指定對手外觀"
	itemKeyRuleDesignateOpenLine = "rule/指定對手開局時講一句話"
)

// RequiresTargetNote 回報某個項目是否必須填指定內容。
func RequiresTargetNote(c Category, name string) bool {
	_, ok := targetNoteRequired[itemKey(c, name)]
	return ok
}
