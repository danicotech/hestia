package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// ── 線上形態 ─────────────────────────────────────────────────────
//
// 每個欄位都是指標,才分得出「沒填」與「填了 0」:
// 填 0 是設定錯誤要報出來,沒填是正常情況靜靜用預設值。
//
// 同一組結構同時服務 v1 與 v2:v1 的鍵(bp_per_rank_gap / odds /
// handicap_item_max_qty)只在讀取時看,Marshal 永遠不寫它們。

type wireRank struct {
	Level       *int    `json:"level"`
	Name        *string `json:"name"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

type wireBP struct {
	Kind       *string `json:"kind"`
	PerRankGap *int64  `json:"per_rank_gap"`
}

type wireFormat struct {
	BestOf             *int  `json:"best_of"`
	PreambleEveryRound *bool `json:"preamble_every_round"`
	ThirdPlaceMatch    *bool `json:"third_place_match"`
}

type wireHandicap struct {
	// ItemMaxQty 用 json.RawMessage 而不是 *int32:null 與缺鍵在這裡同義(不限制),
	// 但「填了非數字」要能報出來,而 *int32 對 "abc" 會讓整份 JSON 解析失敗。
	ItemMaxQty json.RawMessage     `json:"item_max_qty"`
	DrawPools  map[string][]string `json:"draw_pools"`
}

type wireTimer struct {
	Start *string `json:"start"`
}

type wireMarket struct {
	Kind        *string `json:"kind"`
	LineSeconds *int64  `json:"line_seconds,omitempty"`
}

type wireOdds struct {
	Kind           *string `json:"kind"`
	Smoothing      *int64  `json:"smoothing"`
	VigBPS         *int64  `json:"vig_bps"`
	MinOddsMilli   *int64  `json:"min_odds_milli"`
	MaxOddsMilli   *int64  `json:"max_odds_milli"`
	MaxParlayMilli *int64  `json:"max_parlay_milli"`
}

type wireParlay struct {
	LegsPerMatch *int `json:"legs_per_match"`
}

type wireBetting struct {
	Enabled *bool        `json:"enabled"`
	Markets []wireMarket `json:"markets"`
	Odds    *wireOdds    `json:"odds"`
	Parlay  *wireParlay  `json:"parlay"`
	CloseAt *string      `json:"close_at"`
}

type wirePrizes struct {
	Champion      *int64 `json:"champion"`
	RunnerUp      *int64 `json:"runner_up"`
	Third         *int64 `json:"third"`
	Participation *int64 `json:"participation"`
}

// wire 是 config JSONB 的線上形態,v1 與 v2 的鍵都在。
type wire struct {
	Version *int `json:"version,omitempty"`

	// v2
	Ranks    []wireRank    `json:"ranks"`
	BP       *wireBP       `json:"bp,omitempty"`
	Format   *wireFormat   `json:"format,omitempty"`
	Handicap *wireHandicap `json:"handicap,omitempty"`
	Timer    *wireTimer    `json:"timer,omitempty"`
	Betting  *wireBetting  `json:"betting,omitempty"`
	Prizes   *wirePrizes   `json:"prizes,omitempty"`

	// v1(只讀;Marshal 不寫)
	BPPerRankGap       *int64          `json:"bp_per_rank_gap,omitempty"`
	OddsV1             *wireOdds       `json:"odds,omitempty"`
	HandicapItemMaxQty json.RawMessage `json:"handicap_item_max_qty,omitempty"`
}

// Parse 解析 tournaments.config。
//
// # 回傳約定:Config 永遠可用,err 只是診斷
//
// 設定是裁判手打進 JSONB 的,打錯一個欄位名或多一個負號都不是稀奇事。
// 若在壞設定時回零值,呼叫端一 return 就是整場賽事打不開;若靜靜吞掉錯誤,
// 一份把 per_rank_gap 寫成 0 的設定會讓所有比賽變成「沒有讓武」——
// 而讓武是這屆活動的全部意義,沒有人會從對戰表上看出異常。
//
// 所以取第三條路:壞欄位逐一退回預設值,同時把每一項都包進 ErrMalformed
// 回給呼叫端去記 log 或在後台標紅。呼叫端可以安全地忽略 err 繼續跑。
//
// # 升版
//
// 沒有 version 或 version 1 的 JSON 依 schemas/28「從 version 1 升版」在記憶體裡
// 補成 v2:v1 的鍵搬到 v2 的位置,v2 才有的區塊用 v1 語意的預設。
// **不回寫 DB**(讀取不該有副作用);寫回由 migration 00008 做過一次。
func Parse(data []byte) (Config, error) {
	if len(data) == 0 {
		return Default(), nil
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		// 整份 JSON 都讀不懂:退回全預設。這比逐欄位壞掉更嚴重,但處理方式一樣 ——
		// 賽事還是要能開,只是規則是預設的那份。
		return Default(), fmt.Errorf("%w: config 不是合法 JSON: %w", ErrMalformed, err)
	}

	var problems []error
	switch {
	case w.Version == nil || *w.Version == 1:
		upgradeV1(&w)
	case *w.Version == CurrentVersion:
	default:
		// 比程式新的版本:當成 v2 讀,認得的鍵照讀、不認得的鍵不會進來。
		// 這是唯一合理的降級 —— 退回全預設會把一份大部分正確的設定整個丟掉。
		problems = append(problems, fmt.Errorf("version=%d 不是程式認得的版本(目前 %d),以 v2 形狀讀取",
			*w.Version, CurrentVersion))
	}

	cfg, structural := fromWire(w)
	problems = append(problems, structural...)
	problems = append(problems, normalize(&cfg)...)
	if len(problems) > 0 {
		return cfg, fmt.Errorf("%w: %w", ErrMalformed, errors.Join(problems...))
	}
	return cfg, nil
}

// ParseBetting 只解析 config 的 betting 區塊(BettingOddsConfig 那條查詢把它整包取出來)。
//
// 回傳約定與 Parse 相同:壞欄位退回預設、錯誤只是診斷。
// 給下注那條路徑用,讓它不必為了拿盤口清單再讀一次完整 config。
func ParseBetting(data []byte) (BettingRule, error) {
	if len(data) == 0 {
		return Default().Betting, nil
	}
	var wb wireBetting
	if err := json.Unmarshal(data, &wb); err != nil {
		return Default().Betting, fmt.Errorf("%w: betting 不是合法 JSON: %w", ErrMalformed, err)
	}
	cfg := Default()
	structural := applyBetting(&cfg, &wb)
	problems := slices.Concat(structural, normalize(&cfg))
	if len(problems) > 0 {
		return cfg.Betting, fmt.Errorf("%w: %w", ErrMalformed, errors.Join(problems...))
	}
	return cfg.Betting, nil
}

// upgradeV1 把 v1 的鍵搬到 v2 的位置(schemas/28 那張表)。
//
// 只搬有值的:v2 才有的區塊(format / timer / markets / parlay / close_at)留 nil,
// 由 fromWire 補上 Default() —— 而 Default 就是 v1 語意,這正是那張表的意思。
func upgradeV1(w *wire) {
	if w.BPPerRankGap != nil {
		w.BP = &wireBP{PerRankGap: w.BPPerRankGap}
	}
	if w.OddsV1 != nil {
		w.Betting = &wireBetting{Odds: w.OddsV1}
	}
	if len(w.HandicapItemMaxQty) > 0 {
		w.Handicap = &wireHandicap{ItemMaxQty: w.HandicapItemMaxQty}
	}
	w.BPPerRankGap, w.OddsV1, w.HandicapItemMaxQty = nil, nil, nil
}

// fromWire 把線上形態疊到 Default() 上。
//
// 這裡只做「有填就覆寫」與**結構性**的問題(ranks 缺 level、item_max_qty 不是數字);
// 值域的檢查一律交給 normalize,那是 Parse 與 Validate 共用的唯一一份。
func fromWire(w wire) (Config, []error) {
	cfg := Default()
	var problems []error

	cfg.Ranks, problems = mergeRanks(w.Ranks, problems)

	if w.BP != nil {
		setStr(&cfg.BP.Kind, w.BP.Kind)
		setI64(&cfg.BP.PerRankGap, w.BP.PerRankGap)
	}
	if w.Format != nil {
		setInt(&cfg.Format.BestOf, w.Format.BestOf)
		setBool(&cfg.Format.PreambleEveryRound, w.Format.PreambleEveryRound)
		setBool(&cfg.Format.ThirdPlaceMatch, w.Format.ThirdPlaceMatch)
	}
	if w.Handicap != nil {
		qty, err := parseItemMaxQty(w.Handicap.ItemMaxQty)
		if err != nil {
			problems = append(problems, err)
		} else {
			cfg.Handicap.ItemMaxQty = qty
		}
		if w.Handicap.DrawPools != nil {
			cfg.Handicap.DrawPools = clonePools(w.Handicap.DrawPools)
		}
	}
	if w.Timer != nil {
		setStr(&cfg.Timer.Start, w.Timer.Start)
	}
	if w.Betting != nil {
		problems = append(problems, applyBetting(&cfg, w.Betting)...)
	}
	if w.Prizes != nil {
		setI64(&cfg.Prizes.Champion, w.Prizes.Champion)
		setI64(&cfg.Prizes.RunnerUp, w.Prizes.RunnerUp)
		setI64(&cfg.Prizes.Third, w.Prizes.Third)
		setI64(&cfg.Prizes.Participation, w.Prizes.Participation)
	}
	return cfg, problems
}

// applyBetting 把 betting 區塊疊上去;抽出來是因為 ParseBetting 也走這裡。
func applyBetting(cfg *Config, wb *wireBetting) []error {
	var problems []error
	setBool(&cfg.Betting.Enabled, wb.Enabled)
	if wb.Markets != nil {
		cfg.Betting.Markets = make([]MarketRule, 0, len(wb.Markets))
		for i, m := range wb.Markets {
			if m.Kind == nil {
				problems = append(problems, fmt.Errorf("betting.markets[%d] 缺 kind,整筆忽略", i))
				continue
			}
			mr := MarketRule{Kind: *m.Kind}
			setI64(&mr.LineSeconds, m.LineSeconds)
			cfg.Betting.Markets = append(cfg.Betting.Markets, mr)
		}
	}
	if wb.Odds != nil {
		setStr(&cfg.Betting.Odds.Kind, wb.Odds.Kind)
		setI64(&cfg.Betting.Odds.Smoothing, wb.Odds.Smoothing)
		setI64(&cfg.Betting.Odds.VigBPS, wb.Odds.VigBPS)
		setI64(&cfg.Betting.Odds.MinOddsMilli, wb.Odds.MinOddsMilli)
		setI64(&cfg.Betting.Odds.MaxOddsMilli, wb.Odds.MaxOddsMilli)
		setI64(&cfg.Betting.Odds.MaxParlayMilli, wb.Odds.MaxParlayMilli)
	}
	if wb.Parlay != nil {
		setInt(&cfg.Betting.Parlay.LegsPerMatch, wb.Parlay.LegsPerMatch)
	}
	setStr(&cfg.Betting.CloseAt, wb.CloseAt)
	return problems
}

// mergeRanks 逐段位覆寫措辭。
//
// 刻意做**逐欄位**合併而不是「ranks 陣列不完整就整組退回預設」:
// 裁判只想改一段的描述時,不該被迫把另外三段也抄一遍 ——
// 抄一遍就是四份權威,其中一份遲早會忘記跟著改。
func mergeRanks(raws []wireRank, problems []error) ([]RankInfo, []error) {
	out := defaultRanks()
	byLevel := make(map[int]*RankInfo, len(out))
	for i := range out {
		byLevel[out[i].Level] = &out[i]
	}
	for i, r := range raws {
		if r.Level == nil {
			problems = append(problems, fmt.Errorf("ranks[%d] 缺 level,整筆忽略", i))
			continue
		}
		target, ok := byLevel[*r.Level]
		if !ok {
			problems = append(problems, fmt.Errorf("ranks[%d] 的 level=%d 不在 1..%d,整筆忽略", i, *r.Level, len(out)))
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
	sort.Slice(out, func(i, j int) bool { return out[i].Level < out[j].Level })
	return out, problems
}

// parseItemMaxQty 讀 item_max_qty:缺鍵 / null = 不限制;數字交給 normalize 檢查值域;
// 其餘一律是結構錯誤。
func parseItemMaxQty(raw json.RawMessage) (*int32, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var v int32
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("handicap.item_max_qty 必須是整數或 null,得到 %s,退回不限制", raw)
	}
	return &v, nil
}

// Marshal 把一份設定寫成 tournaments.config 的 JSONB 內容。
//
// # 為什麼寫**完整**的一份,而不是只寫與預設值不同的欄位
//
// 一屆賽事開下去之後,它的規則就該凍結。只寫差異的話,哪天有人調整 Default
// 的數字,所有進行中的賽事會在半途換一套 BP 級距或賠率上限 —— 而且沒有任何
// 紀錄顯示發生過這件事。
//
// 代價是每一列都存了一份看起來像預設值的內容,那是刻意付的:
// 「這屆用的是哪一套規則」必須在資料上有答案,不是在當時的程式碼裡。
//
// 一律寫 v2、一律寫 version。與 Parse 互為反函式(測試釘住往返,含 v1 → v2)。
func Marshal(c Config) ([]byte, error) {
	version := CurrentVersion
	w := wire{
		Version: &version,
		BP:      &wireBP{Kind: &c.BP.Kind, PerRankGap: &c.BP.PerRankGap},
		Format: &wireFormat{
			BestOf:             &c.Format.BestOf,
			PreambleEveryRound: &c.Format.PreambleEveryRound,
			ThirdPlaceMatch:    &c.Format.ThirdPlaceMatch,
		},
		Handicap: &wireHandicap{
			ItemMaxQty: marshalItemMaxQty(c.Handicap.ItemMaxQty),
			DrawPools:  marshalPools(c.Handicap.DrawPools),
		},
		Timer: &wireTimer{Start: &c.Timer.Start},
		Betting: &wireBetting{
			Enabled: &c.Betting.Enabled,
			Markets: make([]wireMarket, 0, len(c.Betting.Markets)),
			Odds: &wireOdds{
				Kind:           &c.Betting.Odds.Kind,
				Smoothing:      &c.Betting.Odds.Smoothing,
				VigBPS:         &c.Betting.Odds.VigBPS,
				MinOddsMilli:   &c.Betting.Odds.MinOddsMilli,
				MaxOddsMilli:   &c.Betting.Odds.MaxOddsMilli,
				MaxParlayMilli: &c.Betting.Odds.MaxParlayMilli,
			},
			Parlay:  &wireParlay{LegsPerMatch: &c.Betting.Parlay.LegsPerMatch},
			CloseAt: &c.Betting.CloseAt,
		},
		Prizes: &wirePrizes{
			Champion:      &c.Prizes.Champion,
			RunnerUp:      &c.Prizes.RunnerUp,
			Third:         &c.Prizes.Third,
			Participation: &c.Prizes.Participation,
		},
	}
	w.Ranks = make([]wireRank, 0, len(c.Ranks))
	for i := range c.Ranks {
		ri := c.Ranks[i]
		w.Ranks = append(w.Ranks, wireRank{
			Level: &ri.Level, Name: &ri.Name, Title: &ri.Title, Description: &ri.Description,
		})
	}
	for i := range c.Betting.Markets {
		m := c.Betting.Markets[i]
		wm := wireMarket{Kind: &m.Kind}
		// line_seconds 只對 duration 有意義;其餘 kind 不寫,讀回來就是 0。
		if m.LineSeconds != 0 {
			wm.LineSeconds = &m.LineSeconds
		}
		w.Betting.Markets = append(w.Betting.Markets, wm)
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("序列化賽事設定: %w", err)
	}
	return data, nil
}

// marshalItemMaxQty 寫 null 而不是省略鍵:schemas/28 的完整結構就是這樣寫的,
// 「有這個旋鈕、目前不限制」與「這份 config 不知道有這個旋鈕」要看得出差別。
func marshalItemMaxQty(q *int32) json.RawMessage {
	if q == nil {
		return json.RawMessage("null")
	}
	data, _ := json.Marshal(*q) // int32 不可能序列化失敗
	return data
}

// marshalPools 保證 nil 清單寫成 [] 而不是 null:讀回來的形狀要跟寫出去的一樣。
func marshalPools(pools map[string][]string) map[string][]string {
	out := make(map[string][]string, len(pools))
	for name, list := range pools {
		if list == nil {
			list = []string{}
		}
		out[name] = list
	}
	return out
}

// clonePools 複製一份,nil 清單一律換成空切片(與 marshalPools 同一個理由:
// 往返之後的形狀要一樣)。
func clonePools(pools map[string][]string) map[string][]string {
	out := make(map[string][]string, len(pools))
	for name, list := range pools {
		cp := make([]string, len(list))
		copy(cp, list)
		out[name] = cp
	}
	return out
}

func setStr(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

func setI64(dst *int64, v *int64) {
	if v != nil {
		*dst = *v
	}
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}
