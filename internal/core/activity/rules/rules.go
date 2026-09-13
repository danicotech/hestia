// Package rules 是 activity.tournaments.config 的唯一契約(schemas/28)。
//
// 整個 package 只認標準庫:不碰 DB、不碰 context、不 import 任何活動層套件。
// bp / tournament / betting / match 都要讀這份設定,它若依賴其中任何一個
// 就會成環;而它自己需要的「段位」只是一個 level 整數,不必認識 bp.Rank。
//
// # 策略 + 參數,不是公式字串
//
// 每個規則區塊有一個 kind 選程式裡的一種實作,其餘欄位是那種實作的參數
// (schemas/28「要解的問題」)。kind 的登記表就在這個檔案裡,每個區塊一張;
// 未知的 kind 在 Validate 直接拒絕、在 Parse 退回預設並記診斷 ——
// 「改數字」零程式改動,「改形狀」要加一個 kind 並帶測試。
//
// # 兩種讀法,兩種態度
//
//	Parse     讀既存的 JSONB。壞欄位退回預設並包進 ErrMalformed,Config 仍可用。
//	Validate  寫入前的嚴格檢查。任何一項不合法就是錯,呼叫端必須中止。
//
// 兩者共用同一份規則(normalize):Parse 是「照規則修」,Validate 是「照規則擋」。
// 分開寫兩份的話,「vig_bps 可以是 0 但不可以是負數」這種判斷遲早有一邊漂掉。
package rules

import (
	"errors"
	"strconv"
)

// ErrMalformed 表示設定有欄位讀不懂或超出合理範圍。
//
// Parse 回它時 Config 仍然可用(壞欄位已退回預設);Validate 回它時是致命的。
// 同一個 sentinel 兩種態度,態度由呼叫端決定,不由錯誤型別決定 ——
// transport 對兩條路都映射成同一個 code,只是建立賽事那條會真的中止。
var ErrMalformed = errors.New("賽事設定不合法")

// CurrentVersion 是程式認得的 config 版本。
//
// Parse 讀到沒有 version 或 version 1 時在記憶體裡升成這個版本;
// Marshal 一律寫這個版本。
const CurrentVersion = 2

// ── kind 登記表 ──────────────────────────────────────────────────
//
// 每張表對應 schemas/28「各區塊的 kind 與程式對應」的一列。
// 加一種新 kind:在這裡登記 + 對應套件加實作 + 測試。少一步都算沒做完。

// BP 規則的 kind。
const (
	// BPKindLinearGap 低段者得 gap × per_rank_gap,高段者 0(core/activity/bp)。
	BPKindLinearGap = "linear_gap"
)

// 賠率規則的 kind。
const (
	// OddsKindVoteShare 投票驅動:p_i = (票_i + S) / (總票 + n·S);賠率 = (1 − vig) / p_i。
	OddsKindVoteShare = "vote_share"
)

// 盤口的 kind。
const (
	// MarketMatchWinner 整場勝負。
	MarketMatchWinner = "match_winner"
	// MarketRoundWinner 單回合勝負,每回合一個市場。
	MarketRoundWinner = "round_winner"
	// MarketDuration 回合時長大/小於 line_seconds,每回合一個市場。
	MarketDuration = "duration"
	// MarketScore 回合比數;結果數由 best_of 推導(見 Config.ScoreOutcomes)。
	MarketScore = "score"
)

// 計時起點的 kind。
const (
	// TimerRoundStart 每回合的「正式決鬥開始」起算;前置過程不計。
	TimerRoundStart = "round_start"
)

// 關盤時間點的 kind。
const (
	// CloseAtFirstRoundStart 第一回合正式決鬥開始即關盤。
	CloseAtFirstRoundStart = "first_round_start"
)

// PoolWuxue 是「隨機武學」的代抽清單名稱。handicap_items.params.draw 以它定址。
const PoolWuxue = "wuxue"

var (
	bpKinds      = set(BPKindLinearGap)
	oddsKinds    = set(OddsKindVoteShare)
	marketKinds  = set(MarketMatchWinner, MarketRoundWinner, MarketDuration, MarketScore)
	timerStarts  = set(TimerRoundStart)
	closeAtKinds = set(CloseAtFirstRoundStart)
)

func set(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// ── 預設值 ───────────────────────────────────────────────────────
//
// 全部集中在這裡,是為了讓「壞設定會退回什麼」只有一個答案可查。
//
// 預設是 **v1 的語意**(單場定勝負、只有勝負盤、無季軍戰),不是第一屆定案的值:
// schemas/28「從 version 1 升版」把缺欄位對到 v1 語意,而第一屆的三局兩勝、
// 四種盤口是資料遷移明確套上去的(migration 00008 第二句)。預設若是第一屆的值,
// 「缺欄位」與「升版」就會各有一套答案。

const (
	// DefaultPerRankGap 每差一段給多少 BP。bp.DefaultPerRankGap 引用它,
	// 不另外寫一個 8:同一個概念兩個字面值遲早會有一邊被改掉。
	DefaultPerRankGap int64 = 8
	// DefaultBestOf 單場定勝負。
	DefaultBestOf = 1
	// DefaultOddsSmoothing 是賠率的拉普拉斯平滑量,避免冷門方分母為 0。
	DefaultOddsSmoothing int64 = 5
	// DefaultVigBPS 是抽水,萬分之一為單位(800 = 8%)。
	DefaultVigBPS int64 = 800
	// DefaultMinOddsMilli 是單場賠率下限,千分之一為單位(1050 = 1.05 倍)。
	DefaultMinOddsMilli int64 = 1050
	// DefaultMaxOddsMilli 是單場賠率上限。
	DefaultMaxOddsMilli int64 = 12000
	// DefaultMaxParlayMilli 是串關總賠率上限。
	DefaultMaxParlayMilli int64 = 300000
	// DefaultLegsPerMatch 同一場只能一腿;串關只能跨場。
	DefaultLegsPerMatch = 1
)

// 驗證用的界線(schemas/28「驗證」)。
const (
	// minOddsFloorMilli 賠率下限不得低於 1.0 倍:低於本金的賠率是倒貼。
	minOddsFloorMilli int64 = 1000
	// maxVigBPS 抽水不得到 100%。
	maxVigBPS int64 = 10000
)

// ── 型別 ─────────────────────────────────────────────────────────

// RankInfo 是段位的顯示資料。
//
// 段位的權威表示法是 Level(1..N);名稱與境界描述只是措辭,逐屆可調,
// 所以放 config 由伺服器給,前端不要寫死中文。
// Level 是 int 而不是 bp.Rank:這個 package 不能 import bp(見套件註解),
// 轉換在 tournament 層做。
type RankInfo struct {
	Level       int
	Name        string
	Title       string
	Description string
}

// BPRule 是讓武 BP 怎麼發。
type BPRule struct {
	// Kind 目前唯一值 BPKindLinearGap。
	Kind string
	// PerRankGap 每差一段給多少 BP,恆為正數。
	PerRankGap int64
}

// FormatRule 是賽制。
type FormatRule struct {
	// BestOf 幾局幾勝,正奇數。1 = 單場定勝負。
	BestOf int
	// PreambleEveryRound 讓守關元 / 讓半血這類開局效果每回合都做。
	PreambleEveryRound bool
	// ThirdPlaceMatch 準決賽兩位敗者加打一場。
	ThirdPlaceMatch bool
}

// HandicapRule 是讓武項目層級的旋鈕。
type HandicapRule struct {
	// ItemMaxQty 同一項目的重複購買上限。nil = 不限制(09-12 grill 定案)。
	ItemMaxQty *int32
	// DrawPools 系統代抽用的候選清單,鍵是 handicap_items.params.draw 的值。
	DrawPools map[string][]string
}

// TimerRule 是計時規則。
type TimerRule struct {
	// Start 目前唯一值 TimerRoundStart。
	Start string
}

// MarketRule 是一種盤口。
type MarketRule struct {
	Kind string
	// LineSeconds 只有 MarketDuration 用;其餘 kind 必須為 0。
	LineSeconds int64
}

// OddsRule 是賠率與抽水參數。
//
// 全部是整數:bps 是萬分之一、milli 是千分之一。賠率乘注額就是金額,
// 中間只要出現一次 float64,兩個人算同一張注單就可能差一塊錢。
type OddsRule struct {
	// Kind 目前唯一值 OddsKindVoteShare。
	Kind string
	// Smoothing 拉普拉斯平滑量,>= 1。
	Smoothing int64
	// VigBPS 抽水,0 <= x < 10000。
	VigBPS int64
	// MinOddsMilli 賠率下限,>= 1000。
	MinOddsMilli int64
	// MaxOddsMilli 賠率上限,> MinOddsMilli。
	MaxOddsMilli int64
	// MaxParlayMilli 串關總賠率上限,> 0。
	MaxParlayMilli int64
}

// ParlayRule 是串關規則。
type ParlayRule struct {
	// LegsPerMatch 同一場最多幾腿,>= 1。
	LegsPerMatch int
}

// BettingRule 是下注規則。
type BettingRule struct {
	Enabled bool
	// Markets 每場開盤時依這份清單建 activity.markets。(Kind, LineSeconds) 不重複。
	Markets []MarketRule
	Odds    OddsRule
	Parlay  ParlayRule
	// CloseAt 目前唯一值 CloseAtFirstRoundStart。
	CloseAt string
}

// Prizes 是獎金表,單位為平台代幣。0 = 不發。
type Prizes struct {
	Champion      int64
	RunnerUp      int64
	Third         int64
	Participation int64
}

// Config 是 tournaments.config 解析後的形態(schemas/28 version 2)。
//
// # 這裡刻意沒有 MaxStake
//
// 單注上限的權威是 platform.economy_configs 的 max_stake(schemas/21)。
// 它是平台層的風險控制,不是逐屆的玩法旋鈕;曾經兩邊都有過,症狀是
// 「同一顆按鈕在不同賽事有不同上限,而沒有人說得出為什麼」。
type Config struct {
	// Version 恆為 CurrentVersion(Parse 升版、Marshal 寫死)。
	Version int
	// Ranks 依 Level 由低至高,Level 為 1..len(Ranks) 連續。
	Ranks    []RankInfo
	BP       BPRule
	Format   FormatRule
	Handicap HandicapRule
	Timer    TimerRule
	Betting  BettingRule
	Prizes   Prizes
}

// defaultRanks 是四段的預設措辭,抄自 schemas/20 與 common.proto 的 enum 註解。
// 用函式而不是套件層變數:回傳新的切片,呼叫端改動不會污染下一次解析。
func defaultRanks() []RankInfo {
	return []RankInfo{
		{Level: 1, Name: "開山", Title: "初試之境", Description: "初入 PVP、實戰場次較少。"},
		{Level: 2, Name: "斷水", Title: "初成之境", Description: "已具操作能力,對局觀念尚未成熟。"},
		{Level: 3, Name: "飛花", Title: "純熟之境", Description: "成熟的 PVP 實力與足夠實戰經驗。"},
		{Level: 4, Name: "無我", Title: "歷戰之境", Description: "頂尖實戰能力與大量對局經驗。"},
	}
}

// Default 是一份完全沒有設定時的賽事規則(v1 語意,理由見預設值那一段)。
func Default() Config {
	return Config{
		Version: CurrentVersion,
		Ranks:   defaultRanks(),
		BP:      BPRule{Kind: BPKindLinearGap, PerRankGap: DefaultPerRankGap},
		Format: FormatRule{
			BestOf:             DefaultBestOf,
			PreambleEveryRound: true,
			ThirdPlaceMatch:    false,
		},
		Handicap: HandicapRule{
			ItemMaxQty: nil,
			DrawPools:  map[string][]string{PoolWuxue: {}},
		},
		Timer: TimerRule{Start: TimerRoundStart},
		Betting: BettingRule{
			Enabled: true,
			Markets: []MarketRule{{Kind: MarketMatchWinner}},
			Odds: OddsRule{
				Kind:           OddsKindVoteShare,
				Smoothing:      DefaultOddsSmoothing,
				VigBPS:         DefaultVigBPS,
				MinOddsMilli:   DefaultMinOddsMilli,
				MaxOddsMilli:   DefaultMaxOddsMilli,
				MaxParlayMilli: DefaultMaxParlayMilli,
			},
			Parlay:  ParlayRule{LegsPerMatch: DefaultLegsPerMatch},
			CloseAt: CloseAtFirstRoundStart,
		},
	}
}

// ── 衍生值 ───────────────────────────────────────────────────────

// WinsNeeded 是整場勝者要先拿到幾勝:⌈BestOf/2⌉。
func (c Config) WinsNeeded() int {
	return (c.Format.BestOf + 1) / 2
}

// ScoreOutcomes 是比分盤的全部結果,由 BestOf 推導。
//
// 順序固定:先列 p1 獲勝的比數(對手勝場由少到多),再列 p2 獲勝的比數
// (對手勝場由多到少),讓清單從「p1 完勝」一路排到「p2 完勝」——
// best_of=3 → [p1_2_0, p1_2_1, p2_2_1, p2_2_0]。
// 這個順序是 markets 的 outcome 定址依據(schemas/21),測試釘住,不可改。
func (c Config) ScoreOutcomes() []string {
	w := c.WinsNeeded()
	if w <= 0 {
		return nil
	}
	out := make([]string, 0, 2*w)
	for lost := 0; lost < w; lost++ {
		out = append(out, scoreOutcome("p1", w, lost))
	}
	for lost := w - 1; lost >= 0; lost-- {
		out = append(out, scoreOutcome("p2", w, lost))
	}
	return out
}

func scoreOutcome(side string, won, lost int) string {
	return side + "_" + strconv.Itoa(won) + "_" + strconv.Itoa(lost)
}

// Rank 取某個 level 的段位顯示資料。不存在時回零值與 false。
func (c Config) Rank(level int) (RankInfo, bool) {
	for _, ri := range c.Ranks {
		if ri.Level == level {
			return ri, true
		}
	}
	return RankInfo{}, false
}

// DrawPool 取某個代抽清單。不存在時回 nil 與 false;存在但為空回空切片與 true ——
// 兩者要分得出來:前者是 config 沒設,後者是設了但御風羽還沒給清單。
func (c Config) DrawPool(name string) ([]string, bool) {
	pool, ok := c.Handicap.DrawPools[name]
	return pool, ok
}
