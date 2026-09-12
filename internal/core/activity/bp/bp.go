// Package bp 是讓武調平點數(Balance Point)的計算規則。
//
// 整個 package 沒有任何依賴 —— 不碰 DB、不碰 context、不碰 Ledger。
// 這是刻意的:BP 算錯,整場賽事的公正性就沒有了,所以它必須能被徹底測試,
// 而純函數是唯一能做到「窮舉所有輸入」的形式。四個段位的全部組合只有 16 種。
//
// # 為什麼 BP 不是貨幣
//
// BP 看起來很像代幣,但它的四個性質沒有一個符合貨幣:
//
//	每輪依段位差重新發放 → 帳本是 append-only,「重新發」只能靠一堆正負分錄湊
//	沒花完即作廢         → 帳本沒有「作廢」這個動作
//	不可交易、不可累積   → 貨幣的核心性質一個都沒有
//	不需冪等             → 重複扣一次的後果是數字不對,不是對不上帳
//
// 所以它完全活在 activity 層,不經過 Ledger。
//
// # 讓武不是放水
//
// 低段位者花 BP 換到的是「高手必須遵守的正式比賽條件」,不是要求高手故意失誤。
// 受限方仍應以取勝為目的全力應戰 —— 這是整套制度最重要的觀念,
// 也是為什麼這裡只有「計算能施加多少限制」,沒有任何關於勝負的邏輯。
package bp

import "fmt"

// Rank 是試鋒段位。數值刻意等於資料庫的 rank_level 與 proto 的 enum 值,
// 三處一致才不會在轉換層出錯。
type Rank int8

const (
	// RankUnspecified = 尚未評定。未評定者不得進入抽籤。
	RankUnspecified Rank = 0
	// RankKaishan 開山|初試之境。
	RankKaishan Rank = 1
	// RankDuanshui 斷水|初成之境。
	RankDuanshui Rank = 2
	// RankFeihua 飛花|純熟之境。
	RankFeihua Rank = 3
	// RankWuwo 無我|歷戰之境。
	RankWuwo Rank = 4
)

// DefaultPerRankGap 是每差一段給多少 BP。
// 逐屆可在 tournaments.config 覆寫,這裡只是預設值。
const DefaultPerRankGap int64 = 8

// MinRank 與 MaxRank 界定有效的已評定段位。
const (
	MinRank = RankKaishan
	MaxRank = RankWuwo
)

// ErrUnranked 表示至少有一方尚未評定段位,無法計算 BP。
//
// 這不是可以用零值帶過的情況:未評段就配對代表流程出錯了(裁判還沒評完就抽籤),
// 靜靜回 0 會讓一場本該有 24 BP 的比賽變成沒有讓武,而且沒有人會發現。
var ErrUnranked = fmt.Errorf("段位尚未評定")

// Valid 回報這個段位是否為有效的已評定值。
func (r Rank) Valid() bool {
	return r >= MinRank && r <= MaxRank
}

// String 回傳段位名稱。只用於日誌與錯誤訊息 —— 給使用者看的名稱由
// tournaments.config 供應(逐屆可調措辭),不要用這裡的。
func (r Rank) String() string {
	switch r {
	case RankKaishan:
		return "開山"
	case RankDuanshui:
		return "斷水"
	case RankFeihua:
		return "飛花"
	case RankWuwo:
		return "無我"
	default:
		return "未評定"
	}
}

// Budget 算出 self 在對上 opponent 時取得的 BP。
//
//	gap    = opponent − self
//	budget = gap > 0 ? gap × perGap : 0
//
// BP 永遠只由**較低段位**的一方取得,高段位者不會因任何理由獲得反向補償。
// 同段對決雙方都是 0 —— 沒有實力差距就沒有需要調平的東西。
//
// perGap <= 0 時退回 DefaultPerRankGap:設定檔壞掉時,寧可用預設值也不要
// 讓整場賽事變成沒有讓武。
func Budget(self, opponent Rank, perGap int64) (int64, error) {
	if !self.Valid() || !opponent.Valid() {
		return 0, fmt.Errorf("%w: self=%d opponent=%d", ErrUnranked, self, opponent)
	}
	if perGap <= 0 {
		perGap = DefaultPerRankGap
	}
	gap := int64(opponent) - int64(self)
	if gap <= 0 {
		return 0, nil
	}
	return gap * perGap, nil
}

// Holder 回報一場對決中由誰取得 BP、取得多少。
//
// 回傳的 holderIsP1 在 budget 為 0 時沒有意義(同段對決無人取得)。
// 呼叫端必須先看 budget 再看 holderIsP1,不要反過來。
func Holder(p1, p2 Rank, perGap int64) (budget int64, holderIsP1 bool, err error) {
	if !p1.Valid() || !p2.Valid() {
		return 0, false, fmt.Errorf("%w: p1=%d p2=%d", ErrUnranked, p1, p2)
	}
	switch {
	case p1 < p2:
		b, err := Budget(p1, p2, perGap)
		return b, true, err
	case p2 < p1:
		b, err := Budget(p2, p1, perGap)
		return b, false, err
	default:
		return 0, false, nil
	}
}

// CanAfford 回報剩餘預算夠不夠買一項 cost 的讓武。
//
// 存在的理由是把「餘額檢查」收斂成一個地方。分散在各處寫 spent+cost <= budget
// 遲早會有人寫成 < 而不是 <=,而那個 off-by-one 只會在「剛好花光」時顯現。
func CanAfford(budget, spent, cost int64) bool {
	if cost <= 0 {
		return false
	}
	if spent < 0 || budget < 0 {
		return false
	}
	return spent+cost <= budget
}

// Remaining 是剩餘可用 BP,永不為負。
func Remaining(budget, spent int64) int64 {
	r := budget - spent
	if r < 0 {
		return 0
	}
	return r
}
