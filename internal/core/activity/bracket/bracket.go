// Package bracket 是單淘汰梯狀對戰表的建立與晉級規則。
//
// 跟 bp 一樣,整個 package 是純函數:不碰 DB、不碰 context、不碰時間。
// 建樹出錯的後果是有人被排了兩場、或有人憑空消失,而那種 bug 在人工檢查
// 一張 32 人的對戰表時幾乎看不出來 —— 所以它必須靠測試,而不是靠眼睛。
//
// # 人數不設上限,也不假設是 2 的冪次
//
// 第一屆沒譜,所以任意人數都要能排。做法是把人數補到最近的 2 的冪次,
// 差額就是輪空(bye)名額。
//
// 輪空**不會產生一場空比賽** —— 那個人直接出現在第二輪。
// 若為了結構整齊而建一場「A vs 無人」的比賽,對戰表上就會出現一場永遠
// 不會發生的對決,而觀眾與下注系統都得為它特判。
//
// # 籤位與輪空的分配
//
// 抽籤是隨機的,沒有種子保護,所以輪空落在誰頭上也是隨機的。
// 分配方式是把前 byes 個籤位做成單人籤位,其餘兩兩成對 ——
// 這保證每一組最多一個輪空,不會出現「兩個人都輪空」的空洞。
package bracket

import (
	"errors"
	"fmt"
	"math/rand/v2"
)

// ErrNoPlayers 表示沒有任何參賽者,無法建樹。
var ErrNoPlayers = errors.New("沒有參賽者")

// Match 是樹上的一個節點。
//
// P1 / P2 為 0 代表「尚未確定」—— 等上一輪打完才知道是誰。
// 這對應資料庫的 NULL,用 0 而不是指標是因為這個 package 不做序列化,
// 而 0 不是合法的 player id。
type Match struct {
	// Round 1 = 首輪。
	Round int
	// Slot 是該輪內位置,0-based。
	Slot int
	P1   int64
	P2   int64
}

// Bracket 是一張完整的對戰表。
type Bracket struct {
	// Size 是補齊後的樹寬度,必為 2 的冪次。
	Size int
	// TotalRounds = log2(Size)。
	TotalRounds int
	// Byes 是首輪輪空、直接進入第二輪的選手。
	Byes []int64
	// Matches 是全部場次,依 (Round, Slot) 排序。
	// 首輪只包含真的要打的場次;輪空不在裡面。
	Matches []Match
	// Seeds 是每位選手抽到的籤位(1-based),對應 tournament_players.seed_no。
	Seeds map[int64]int
}

// Build 隨機排出一張單淘汰對戰表。
//
// r 決定抽籤結果。傳入固定種子的 *rand.Rand 會得到固定的表 ——
// 裁判需要向人證明「這張表確實是這個種子抽出來的」時就靠這個。
//
// 場次總數恆為 len(playerIDs) − 1,這是單淘汰的數學性質:
// 每場淘汰剛好一人,要從 N 人選出 1 人就要淘汰 N−1 人。
//
// 只有一人報名是退化情況:樹寬 1、零輪、零場,那個人直接是冠軍,
// 既不打首輪也不算輪空。這不是錯誤 —— 其他人全部棄賽時就會走到這裡。
func Build(playerIDs []int64, r *rand.Rand) (*Bracket, error) {
	n := len(playerIDs)
	if n == 0 {
		return nil, ErrNoPlayers
	}
	if err := checkDistinct(playerIDs); err != nil {
		return nil, err
	}

	// 抽籤:複製一份再洗,不動呼叫端的切片。
	order := make([]int64, n)
	copy(order, playerIDs)
	r.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })

	size := nextPowerOfTwo(n)
	totalRounds := log2(size)
	pairs := size / 2
	byes := size - n

	b := &Bracket{
		Size:        size,
		TotalRounds: totalRounds,
		Seeds:       make(map[int64]int, n),
	}
	for i, id := range order {
		b.Seeds[id] = i + 1
	}

	// 首輪:前 byes 個籤位是單人籤位(輪空),其餘兩兩成對。
	// 這樣分配保證每組最多一個輪空 —— 若把輪空隨機灑在 size 個位置上,
	// 有可能兩個輪空落在同一組,那一組就變成一場沒有人的比賽。
	next := 0
	for slot := range pairs {
		if slot < byes {
			b.Byes = append(b.Byes, order[next])
			next++
			continue
		}
		b.Matches = append(b.Matches, Match{
			Round: 1,
			Slot:  slot,
			P1:    order[next],
			P2:    order[next+1],
		})
		next += 2
	}

	// 後續各輪:全部先建成待定。輪空者的位置在這裡就填好,
	// 因為他不必打首輪,第二輪的對手欄位一開始就該有他。
	for round := 2; round <= totalRounds; round++ {
		slots := size >> round
		for slot := range slots {
			b.Matches = append(b.Matches, Match{Round: round, Slot: slot})
		}
	}
	for slot, id := range b.Byes {
		ns, isP1 := feedsInto(slot)
		m := b.find(2, ns)
		if m == nil {
			// 只有在 size 計算錯誤時才可能發生;寧可炸也不要靜靜漏掉一個人。
			return nil, fmt.Errorf("輪空者 %d 找不到晉級目標 (round=2 slot=%d)", id, ns)
		}
		if isP1 {
			m.P1 = id
		} else {
			m.P2 = id
		}
	}

	return b, nil
}

// Shape 建出「只有形狀、沒有選手」的對戰表。
//
// 存在的理由:晉級與輪次名稱只取決於樹的大小,不取決於誰在裡面。
// 比賽結束時要問「勝者進哪一場」「這輪叫什麼」,呼叫端手上通常只有
// 「這屆總共幾輪」(從 matches 的 MAX(round) 得知),沒有完整的 Bracket。
//
// 沒有這個建構子,呼叫端就得自己湊 &Bracket{Size: 1<<n, TotalRounds: n} ——
// 那是在複製這裡的不變式(Size 必須是 2^TotalRounds),而複製出去的不變式
// 不會跟著這裡一起改。
//
// totalRounds <= 0 回 nil:零輪的賽事沒有任何場次,問晉級是呼叫端的 bug。
func Shape(totalRounds int) *Bracket {
	if totalRounds <= 0 {
		return nil
	}
	return &Bracket{
		Size:        1 << totalRounds,
		TotalRounds: totalRounds,
		Seeds:       map[int64]int{},
	}
}

// Advance 回報某場的勝者該進到哪裡。
//
// 回傳 ok = false 代表這是決賽,沒有下一場 —— 勝者即冠軍。
func (b *Bracket) Advance(round, slot int) (nextRound, nextSlot int, isP1, ok bool) {
	if round < 1 || round >= b.TotalRounds {
		return 0, 0, false, false
	}
	ns, p1 := feedsInto(slot)
	return round + 1, ns, p1, true
}

// RoundLabel 是一輪的顯示名稱,如「首輪」「八強」「四強」「決賽」。
//
// 由伺服器算而不是讓前端寫死:同一個 round 編號在 8 人賽是「四強」、
// 在 32 人賽是「三十二強」,前端沒有足夠資訊自己判斷。
func (b *Bracket) RoundLabel(round int) string {
	if round < 1 || round > b.TotalRounds {
		return ""
	}
	remaining := b.Size >> (round - 1)
	switch remaining {
	case 2:
		return "決賽"
	case 4:
		return "四強"
	case 8:
		return "八強"
	case 16:
		return "十六強"
	case 32:
		return "三十二強"
	case 64:
		return "六十四強"
	}
	if round == 1 {
		return "首輪"
	}
	return fmt.Sprintf("第 %d 輪", round)
}

// MatchCount 是場次總數,恆為參賽人數 − 1。
func (b *Bracket) MatchCount() int { return len(b.Matches) }

// find 取得指定位置的場次指標,供 Build 內部填輪空用。
func (b *Bracket) find(round, slot int) *Match {
	for i := range b.Matches {
		if b.Matches[i].Round == round && b.Matches[i].Slot == slot {
			return &b.Matches[i]
		}
	}
	return nil
}

// feedsInto 回報某一輪的 slot 匯進下一輪的哪個位置。
//
// 兩個相鄰的 slot 匯進同一場:slot 0 與 1 → 下一輪 slot 0,
// 偶數進 P1、奇數進 P2。這個映射與輪次無關,所以不需要 round 參數。
func feedsInto(slot int) (nextSlot int, isP1 bool) {
	return slot / 2, slot%2 == 0
}

// nextPowerOfTwo 回傳 >= n 的最小 2 的冪次。
func nextPowerOfTwo(n int) int {
	size := 1
	for size < n {
		size <<= 1
	}
	return size
}

// log2 回傳 2 的冪次的指數。
func log2(size int) int {
	r := 0
	for size > 1 {
		size >>= 1
		r++
	}
	return r
}

// checkDistinct 擋掉同一個人出現兩次。
//
// 資料庫有 UNIQUE (tournament_id, fencer_id) 擋重複報名,但建樹拿到的是
// 呼叫端組出來的切片,那層保證到不了這裡。同一人出現兩次會讓他自己打自己,
// 或在對戰表上出現兩次 —— 都是人工檢查看不出來的。
func checkDistinct(ids []int64) error {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return errors.New("參賽者 id 不可為 0")
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("參賽者 %d 重複出現", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
