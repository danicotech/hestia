package handicap

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// seedJSON 是《百業試鋒》讓武項目總表,也是**唯一**的那一份。
//
// 內嵌而不是讀檔:這份清單是賽事規則本身,不是設定。跟 binary 一起走才保證
// 「部署到哪裡,規則就是哪一份」—— 少一個「正式機上那個 JSON 是哪個版本」的問題。
//
// 這份清單是**目錄不是資料**:開一屆賽事時整份複製進 activity.handicap_items,
// 逐屆一套(價格要能逐屆調,而上屆的選購紀錄必須永遠指向上屆的價格)。
// 複製的動作在 tournament.Service.Create,與建立賽事同一個 transaction。
//
//go:embed catalogue.json
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
	seenKey := make(map[string]struct{}, len(f.Items))
	seenName := make(map[string]struct{}, len(f.Items))
	for i, it := range f.Items {
		if !it.Category.Valid() {
			return nil, fmt.Errorf("第 %d 項 %q 的分類 %q 不是六大類之一", i, it.Name, it.Category)
		}
		if it.Name == "" {
			return nil, fmt.Errorf("第 %d 項沒有名稱", i)
		}
		if err := validateKey(it.Key, it.Category); err != nil {
			return nil, fmt.Errorf("第 %d 項 %q: %w", i, it.Name, err)
		}
		// cost > 0 是 DB 的 CHECK;在這裡先擋住,免得 seed 打錯字要等到 migration 才炸。
		if it.Cost <= 0 {
			return nil, fmt.Errorf("項目 %q 的 cost 必須為正,實際是 %d", it.Name, it.Cost)
		}
		// params 的鍵與值域已在 ItemParams.UnmarshalJSON 驗過;這裡只剩唯一性。
		if _, dup := seenKey[it.Key]; dup {
			// 對應 UNIQUE (tournament_id, key)。
			return nil, fmt.Errorf("項目 key %s 重複", it.Key)
		}
		seenKey[it.Key] = struct{}{}
		name := string(it.Category) + "/" + it.Name
		if _, dup := seenName[name]; dup {
			// 對應 UNIQUE (tournament_id, category, name):InsertItems 的衝突鍵仍是它。
			return nil, fmt.Errorf("項目 %s 重複", name)
		}
		seenName[name] = struct{}{}
	}
	return f.Items, nil
})

// keyPattern 是 key 的形狀,與 DB 的 handicap_items_key_format_check 同一條正規式。
var keyPattern = regexp.MustCompile(`^[a-z]+\.[a-z0-9_]+$`)

// validateKey 檢查 key 的形狀且前綴等於分類。
//
// 兩個地方各寫一次分類(key 的前綴、category 欄位)本來就是重複,
// 這條檢查讓漂移在載入 seed 時就被擋下,而不是等 DB 的 CHECK。
func validateKey(key string, c Category) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("key %q 不符合 <category>.<snake_case>", key)
	}
	if prefix, _, _ := strings.Cut(key, "."); prefix != string(c) {
		return fmt.Errorf("key %q 的前綴 %q 與分類 %q 不一致", key, prefix, c)
	}
	return nil
}

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

// targetNoteRequired 列出「必須填指定內容」的項目,以 key 定址。
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
//
// 以 key 而不是 (category, name) 定址:2026-09-13 審定時「16 級武庫」改成「46 級」,
// 名稱會變、key 不會。items_test.go 驗每一個 key 都存在於 seed,刪項時測試會紅。
var targetNoteRequired = map[string]struct{}{
	"weapon.designated_art":      {}, // 指定對手武學
	"skill.ban_two":              {}, // 禁用兩種技能
	"skill.ban_one":              {}, // 禁用一種技能
	"skill.remap":                {}, // 指定並更改技能位子
	"skill.ban_arcane_one":       {}, // 禁用任意奇術
	"poison.swap_keys":           {}, // 互換任意兩個按鍵
	"rule.designated_appearance": {}, // 指定對手外觀
	"rule.opening_line":          {}, // 指定對手開局時講一句話
}

// RequiresTargetNote 回報某個項目(以 key 指定)是否必須填指定內容。
func RequiresTargetNote(key string) bool {
	_, ok := targetNoteRequired[key]
	return ok
}
