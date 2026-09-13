package tournament

import (
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/rules"
)

// tournaments.config 的契約在 rules 套件(schemas/28),這個檔案只做兩件事:
// 把型別與錯誤以別名接出來,讓既有呼叫端不必認識第二個套件名;
// 以及提供 NewConfig —— 開賽事時裁判填的旋鈕怎麼疊到預設值上,那是建立流程的事,
// 不是契約的事。
//
// 這裡**沒有**自己的解析邏輯。同一個概念只有一個權威位置:形狀、預設值、
// 值域規則全在 rules,這裡一行都不重複。

// Config 是 tournaments.config 解析後的形態,即 rules.Config。
type Config = rules.Config

// RankInfo / Prizes 是 transport 與 ConfigOverrides 會碰到的子型別,同樣是 rules 的別名。
type (
	RankInfo = rules.RankInfo
	Prizes   = rules.Prizes
)

// ErrConfigMalformed 表示賽事設定有欄位讀不懂或超出合理範圍。
//
// 兩個產生它的地方對它的態度**刻意相反**,各自的理由寫在自己的註解裡:
//
//	ParseConfig  讀既存的 JSONB。收到錯誤時 Config 仍然可用(壞欄位已退回預設),
//	             呼叫端該記 log 或在後台標紅,不是中止。
//	NewConfig    讀裁判此刻填的表單。收到錯誤必須中止 —— 替他猜一個值,
//	             等於開了一屆規則與他以為的不一樣的賽事。
var ErrConfigMalformed = rules.ErrMalformed

// DefaultConfig 是一份完全沒有設定時的賽事規則(rules.Default)。
func DefaultConfig() Config { return rules.Default() }

// ParseConfig 解析 tournaments.config(rules.Parse)。
//
// 回傳約定:**Config 永遠可用,err 只是診斷**。壞欄位逐一退回預設值,
// 同時把每一項都包進 ErrConfigMalformed 回給呼叫端去記 log 或在後台標紅。
// 呼叫端可以安全地忽略 err 繼續跑。理由見 rules.Parse。
func ParseConfig(data []byte) (Config, error) { return rules.Parse(data) }

// MarshalConfig 把一份設定寫成 tournaments.config 的 JSONB 內容(rules.Marshal)。
// 一律寫完整的 v2,與 ParseConfig 互為反函式;理由見 rules.Marshal。
func MarshalConfig(c Config) ([]byte, error) { return rules.Marshal(c) }

// RankOf 取某位選手段位的顯示資料。
//
// bp.Rank 與 rules.RankInfo.Level 是同一個數字(rank_level),但 rules 不能
// import bp(它只認標準庫),所以轉換在這一層做,呼叫端不必自己寫 int(r)。
func RankOf(c Config, r bp.Rank) (RankInfo, bool) {
	return c.Rank(int(r))
}

// ── 建立賽事時的設定 ──────────────────────────────────────────

// ConfigOverrides 是開一屆新賽事時裁判填的規則旋鈕。
//
// 每個欄位都是指標,nil = **用預設值**。填 0 與沒填在這裡是兩件事
// (vig_bps 填 0 是「不抽水」,沒填是「抽 8%」)。
//
// 刻意不含 Ranks、markets、draw_pools 這類整組結構:段位措辭要逐屆改寫是罕見情況,
// 而把它們全開成請求欄位會讓「開一屆賽事」這個動作多幾十個欄位。
// 真要改時走設定而不是建立流程 —— 那時候賽事已經存在,改起來風險也更低。
type ConfigOverrides struct {
	BPPerRankGap   *int64
	Smoothing      *int64
	VigBPS         *int64
	MinOddsMilli   *int64
	MaxOddsMilli   *int64
	MaxParlayMilli *int64
	// BestOf 幾局幾勝,必須是正奇數。nil = 單場定勝負。
	BestOf *int
	// ThirdPlaceMatch 準決賽敗者是否加打季軍戰。nil = 不打。
	ThirdPlaceMatch *bool
	// Prizes 整包 nil = 全部不發(0)。單項為 0 也是不發。
	Prizes *Prizes
	// HandicapItemMaxQty nil 為不限制;<= 0 會回 ErrConfigMalformed
	// ——「一項都不能買」不會是任何人的本意。
	HandicapItemMaxQty *int32
}

// NewConfig 把裁判填的旋鈕疊到預設值上,產生一份完整且**已通過 rules.Validate** 的賽事設定。
//
// # 與 ParseConfig 的回傳約定不同:這裡的錯誤是**致命的**
//
// ParseConfig 讀的是資料庫裡既存的設定,壞欄位只能退回預設然後把賽事開起來
// (理由見它的註解)。NewConfig 讀的是裁判**此刻**填的表單 —— 打錯了就當場說,
// 靜靜替他退回預設等於開了一屆規則與他以為的不一樣的賽事,而那要等到
// 第一場算 BP 時才有人看得出來。
//
// 所以呼叫端收到 err 必須中止;第一個回傳值在那種情況下是零值,不該被使用。
// Service.Create 走這裡,因此寫入前必然過了 rules.Validate。
func NewConfig(o ConfigOverrides) (Config, error) {
	cfg := rules.Default()
	if o.BPPerRankGap != nil {
		cfg.BP.PerRankGap = *o.BPPerRankGap
	}
	if o.Smoothing != nil {
		cfg.Betting.Odds.Smoothing = *o.Smoothing
	}
	if o.VigBPS != nil {
		cfg.Betting.Odds.VigBPS = *o.VigBPS
	}
	if o.MinOddsMilli != nil {
		cfg.Betting.Odds.MinOddsMilli = *o.MinOddsMilli
	}
	if o.MaxOddsMilli != nil {
		cfg.Betting.Odds.MaxOddsMilli = *o.MaxOddsMilli
	}
	if o.MaxParlayMilli != nil {
		cfg.Betting.Odds.MaxParlayMilli = *o.MaxParlayMilli
	}
	if o.BestOf != nil {
		cfg.Format.BestOf = *o.BestOf
	}
	if o.ThirdPlaceMatch != nil {
		cfg.Format.ThirdPlaceMatch = *o.ThirdPlaceMatch
	}
	if o.Prizes != nil {
		cfg.Prizes = *o.Prizes
	}
	if o.HandicapItemMaxQty != nil {
		q := *o.HandicapItemMaxQty
		cfg.Handicap.ItemMaxQty = &q
	}
	if err := rules.Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
