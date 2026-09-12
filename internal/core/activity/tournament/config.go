package tournament

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/danicotech/hestia/internal/core/activity/bp"
)

// ErrConfigMalformed 表示 tournaments.config 有欄位讀不懂或超出合理範圍。
//
// **收到這個錯誤時 Config 仍然可用** —— ParseConfig 保證回傳的一定是一份
// 每個欄位都補過預設值的設定。理由寫在 ParseConfig 的註解裡。
var ErrConfigMalformed = errors.New("賽事設定不合法")

// 設定的預設值。全部集中在這裡,是為了讓「壞設定會退回什麼」這件事
// 只有一個答案可查 —— 散在各 default 分支裡的話,沒有人說得出完整的退化行為。
const (
	// DefaultBPPerRankGap 每差一段給多少 BP。與 bp.DefaultPerRankGap 同源,
	// 不另外寫一個 8:同一個概念兩個字面值遲早會有一邊被改掉。
	DefaultBPPerRankGap = bp.DefaultPerRankGap
	// DefaultMaxStake 單注上限(平台代幣)。
	DefaultMaxStake int64 = 500
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
)

// OddsConfig 是賠率與抽水參數。
//
// 全部是整數:bps 是萬分之一、milli 是千分之一。
// 這是「金額不用浮點」那條鐵則往賠率上的延伸 —— 賠率乘注額就是金額,
// 中間只要出現一次 float64,兩個人算同一張注單就可能差一塊錢。
type OddsConfig struct {
	// Smoothing 是拉普拉斯平滑量:賠率的分母加上它,
	// 否則第一個下注的人會拿到無限大的賠率。
	Smoothing int64
	// VigBPS 是抽水,萬分之一。800 = 8%。
	VigBPS int64
	// MinOddsMilli 是賠率下限,千分之一。
	MinOddsMilli int64
	// MaxOddsMilli 是賠率上限,壓住「一面倒的比賽開出天價賠率」的尾端風險。
	MaxOddsMilli int64
	// MaxParlayMilli 是串關總賠率上限。
	MaxParlayMilli int64
}

// Prizes 是獎金表,單位為平台代幣。0 = 不發。
//
// 第一屆的數字未定,預設全 0 —— 讓一份沒填獎金的設定變成「不發獎」,
// 而不是「發一個隨便猜的數字」。
type Prizes struct {
	Champion      int64
	RunnerUp      int64
	Third         int64
	Participation int64
}

// RankInfo 是段位的顯示資料。
//
// 段位的權威表示法是 rank_level 數字(1..4),名稱與境界描述只是**措辭**,
// 逐屆可調,所以放 config 由伺服器給,前端不要寫死中文(schemas/20)。
type RankInfo struct {
	Rank        bp.Rank
	Name        string
	Title       string
	Description string
}

// Config 是 tournaments.config 解析後的形態。
type Config struct {
	// BPPerRankGap 每差一段給多少 BP。恆為正數。
	BPPerRankGap int64
	// Ranks 是四個段位的顯示資料,依 level 由低至高排序,長度恆為 4。
	Ranks []RankInfo
	Odds  OddsConfig
	// MaxStake 單注上限。恆為正數。
	MaxStake int64
	Prizes   Prizes
	// HandicapItemMaxQty 是單一讓武項目的重複購買上限。
	// nil = **不限制**,這是 09-12 grill 的定案值。
	// 預留這個旋鈕是為了日後實戰發現失衡時能逐屆設上限,不必改表。
	HandicapItemMaxQty *int32
}

// Rank 取某個段位的顯示資料。段位不合法時回零值與 false。
func (c Config) Rank(r bp.Rank) (RankInfo, bool) {
	for _, ri := range c.Ranks {
		if ri.Rank == r {
			return ri, true
		}
	}
	return RankInfo{}, false
}

// defaultRanks 是四段的預設措辭,抄自 schemas/20 與 common.proto 的 enum 註解。
// 用函式而不是套件層變數:回傳新的切片,呼叫端改動不會污染下一次解析。
func defaultRanks() []RankInfo {
	return []RankInfo{
		{Rank: bp.RankKaishan, Name: "開山", Title: "初試之境", Description: "初入 PVP、實戰場次較少。"},
		{Rank: bp.RankDuanshui, Name: "斷水", Title: "初成之境", Description: "已具操作能力,對局觀念尚未成熟。"},
		{Rank: bp.RankFeihua, Name: "飛花", Title: "純熟之境", Description: "成熟的 PVP 實力與足夠實戰經驗。"},
		{Rank: bp.RankWuwo, Name: "無我", Title: "歷戰之境", Description: "頂尖實戰能力與大量對局經驗。"},
	}
}

// DefaultConfig 是一份完全沒有設定時的賽事規則。
func DefaultConfig() Config {
	return Config{
		BPPerRankGap: DefaultBPPerRankGap,
		Ranks:        defaultRanks(),
		Odds: OddsConfig{
			Smoothing:      DefaultOddsSmoothing,
			VigBPS:         DefaultVigBPS,
			MinOddsMilli:   DefaultMinOddsMilli,
			MaxOddsMilli:   DefaultMaxOddsMilli,
			MaxParlayMilli: DefaultMaxParlayMilli,
		},
		MaxStake: DefaultMaxStake,
	}
}

// rawRank 是 config.ranks 的單筆線上形態。
type rawRank struct {
	Level       *int8   `json:"level"`
	Name        *string `json:"name"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// rawOdds 是 config.odds 的線上形態。
type rawOdds struct {
	Smoothing      *int64 `json:"smoothing"`
	VigBPS         *int64 `json:"vig_bps"`
	MinOddsMilli   *int64 `json:"min_odds_milli"`
	MaxOddsMilli   *int64 `json:"max_odds_milli"`
	MaxParlayMilli *int64 `json:"max_parlay_milli"`
}

// rawPrizes 是 config.prizes 的線上形態。
type rawPrizes struct {
	Champion      *int64 `json:"champion"`
	RunnerUp      *int64 `json:"runner_up"`
	Third         *int64 `json:"third"`
	Participation *int64 `json:"participation"`
}

// rawConfig 對應 config JSONB 的線上形態。
//
// 每個欄位都是指標,才分得出「沒填」與「填了 0」——
// 填 0 是設定錯誤要報出來,沒填是正常情況靜靜用預設值。
type rawConfig struct {
	BPPerRankGap       *int64     `json:"bp_per_rank_gap"`
	Ranks              []rawRank  `json:"ranks"`
	Odds               *rawOdds   `json:"odds"`
	MaxStake           *int64     `json:"max_stake"`
	Prizes             *rawPrizes `json:"prizes"`
	HandicapItemMaxQty *int32     `json:"handicap_item_max_qty"`
}

// ParseConfig 解析 tournaments.config。
//
// # 回傳約定:Config 永遠可用,err 只是診斷
//
// 這是整個套件裡唯一「回了錯誤但第一個回傳值仍然有效」的函式,刻意如此。
//
// 設定是**裁判手打進 JSONB 的**,打錯一個欄位名或多一個負號都不是稀奇事。
// 若讓 ParseConfig 在壞設定時回零值,呼叫端一 return 就是整場賽事打不開;
// 若讓它靜靜吞掉錯誤,一份把 bp_per_rank_gap 寫成 0 的設定會讓所有比賽
// 變成「沒有讓武」—— 而讓武是這屆活動的全部意義,沒有人會從對戰表上看出異常。
//
// 所以取第三條路:壞欄位逐一退回預設值,同時把每一項都包進 ErrConfigMalformed
// 回給呼叫端去記 log 或在後台標紅。呼叫端可以安全地忽略 err 繼續跑。
func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(data) == 0 {
		return cfg, nil
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		// 整份 JSON 都讀不懂:退回全預設。這比逐欄位壞掉更嚴重,但處理方式一樣 ——
		// 賽事還是要能開,只是規則是預設的那份。
		return cfg, fmt.Errorf("%w: config 不是合法 JSON: %w", ErrConfigMalformed, err)
	}

	var problems []error
	if raw.BPPerRankGap != nil {
		if *raw.BPPerRankGap > 0 {
			cfg.BPPerRankGap = *raw.BPPerRankGap
		} else {
			problems = append(problems, fmt.Errorf("bp_per_rank_gap 必須為正數,得到 %d,退回 %d",
				*raw.BPPerRankGap, DefaultBPPerRankGap))
		}
	}

	cfg.Ranks, problems = mergeRanks(raw.Ranks, problems)

	if raw.Odds != nil {
		cfg.Odds, problems = mergeOdds(cfg.Odds, raw.Odds, problems)
	}

	if raw.MaxStake != nil {
		if *raw.MaxStake > 0 {
			cfg.MaxStake = *raw.MaxStake
		} else {
			problems = append(problems, fmt.Errorf("max_stake 必須為正數,得到 %d,退回 %d",
				*raw.MaxStake, DefaultMaxStake))
		}
	}

	if raw.Prizes != nil {
		cfg.Prizes.Champion, problems = nonNegative("prizes.champion", raw.Prizes.Champion, problems)
		cfg.Prizes.RunnerUp, problems = nonNegative("prizes.runner_up", raw.Prizes.RunnerUp, problems)
		cfg.Prizes.Third, problems = nonNegative("prizes.third", raw.Prizes.Third, problems)
		cfg.Prizes.Participation, problems = nonNegative("prizes.participation", raw.Prizes.Participation, problems)
	}

	// null / 缺欄位 = 不限制,這是定案的預設。
	// <= 0 則是打錯了:0 代表「一項都不能買」,沒有人會刻意這樣設定,
	// 而把它當真會讓整場賽事的讓武全部買不下去。
	if raw.HandicapItemMaxQty != nil {
		if *raw.HandicapItemMaxQty > 0 {
			q := *raw.HandicapItemMaxQty
			cfg.HandicapItemMaxQty = &q
		} else {
			problems = append(problems, fmt.Errorf("handicap_item_max_qty 必須為正數或 null,得到 %d,退回不限制",
				*raw.HandicapItemMaxQty))
		}
	}

	if len(problems) > 0 {
		return cfg, fmt.Errorf("%w: %w", ErrConfigMalformed, errors.Join(problems...))
	}
	return cfg, nil
}

// mergeRanks 逐段位覆寫措辭。
//
// 刻意做**逐欄位**合併而不是「ranks 陣列不完整就整組退回預設」:
// 裁判只想改一段的描述時,不該被迫把另外三段也抄一遍 ——
// 抄一遍就是四份權威,其中一份遲早會忘記跟著改。
func mergeRanks(raws []rawRank, problems []error) ([]RankInfo, []error) {
	out := defaultRanks()
	byRank := make(map[bp.Rank]*RankInfo, len(out))
	for i := range out {
		byRank[out[i].Rank] = &out[i]
	}

	for i, r := range raws {
		if r.Level == nil {
			problems = append(problems, fmt.Errorf("ranks[%d] 缺 level,整筆忽略", i))
			continue
		}
		target, ok := byRank[bp.Rank(*r.Level)]
		if !ok {
			problems = append(problems, fmt.Errorf("ranks[%d] 的 level=%d 不在 1..4,整筆忽略", i, *r.Level))
			continue
		}
		if r.Name != nil && *r.Name != "" {
			target.Name = *r.Name
		}
		if r.Title != nil && *r.Title != "" {
			target.Title = *r.Title
		}
		if r.Description != nil && *r.Description != "" {
			target.Description = *r.Description
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out, problems
}

// mergeOdds 逐項覆寫賠率參數,壞值退回預設。
func mergeOdds(cur OddsConfig, raw *rawOdds, problems []error) (OddsConfig, []error) {
	cur.Smoothing, problems = positive("odds.smoothing", raw.Smoothing, cur.Smoothing, problems)
	cur.MinOddsMilli, problems = positive("odds.min_odds_milli", raw.MinOddsMilli, cur.MinOddsMilli, problems)
	cur.MaxOddsMilli, problems = positive("odds.max_odds_milli", raw.MaxOddsMilli, cur.MaxOddsMilli, problems)
	cur.MaxParlayMilli, problems = positive("odds.max_parlay_milli", raw.MaxParlayMilli, cur.MaxParlayMilli, problems)

	// 抽水可以是 0(不抽),但不能是負數(等於倒貼)也不能到 100%(全吃)。
	if raw.VigBPS != nil {
		if *raw.VigBPS >= 0 && *raw.VigBPS < 10000 {
			cur.VigBPS = *raw.VigBPS
		} else {
			problems = append(problems, fmt.Errorf("odds.vig_bps 必須在 0..9999,得到 %d,退回 %d",
				*raw.VigBPS, DefaultVigBPS))
		}
	}

	// 上下限反了會讓「夾在 min 與 max 之間」這個動作沒有解。
	// 兩個都退回預設而不是只修一邊 —— 只修一邊等於替裁判猜他想改的是哪一個。
	if cur.MinOddsMilli >= cur.MaxOddsMilli {
		problems = append(problems, fmt.Errorf("odds.min_odds_milli(%d) 必須小於 max_odds_milli(%d),兩者都退回預設",
			cur.MinOddsMilli, cur.MaxOddsMilli))
		cur.MinOddsMilli = DefaultMinOddsMilli
		cur.MaxOddsMilli = DefaultMaxOddsMilli
	}
	return cur, problems
}

// positive 取正數欄位;沒填就保留 fallback,填了非正數就記一筆問題並保留 fallback。
func positive(field string, v *int64, fallback int64, problems []error) (int64, []error) {
	if v == nil {
		return fallback, problems
	}
	if *v <= 0 {
		return fallback, append(problems, fmt.Errorf("%s 必須為正數,得到 %d,退回 %d", field, *v, fallback))
	}
	return *v, problems
}

// nonNegative 取非負欄位(獎金可以是 0 = 不發,但不能是負數)。
func nonNegative(field string, v *int64, problems []error) (int64, []error) {
	if v == nil {
		return 0, problems
	}
	if *v < 0 {
		return 0, append(problems, fmt.Errorf("%s 不可為負數,得到 %d,退回 0", field, *v))
	}
	return *v, problems
}
