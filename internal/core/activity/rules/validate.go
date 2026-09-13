package rules

import (
	"errors"
	"fmt"
	"strings"
)

// Validate 是寫入前的嚴格檢查(schemas/28「驗證」)。
//
// 與 Parse 的回傳約定相反:這裡的錯誤是**致命的**。Parse 讀的是既存的設定,
// 壞欄位只能退回預設然後把賽事開起來;Validate 面對的是此刻要寫進去的那一份 ——
// 靜靜替裁判退回預設,等於開了一屆規則與他以為的不一樣的賽事,而那要等到
// 第一場算 BP 時才有人看得出來。
//
// 判定規則與 Parse 是同一份(normalize):Parse 會修的,這裡就會擋;
// Parse 不會動的,這裡也放行。
func Validate(c Config) error {
	c = clone(c)
	if problems := normalize(&c); len(problems) > 0 {
		return fmt.Errorf("%w: %w", ErrMalformed, errors.Join(problems...))
	}
	return nil
}

// LockedAfter 回報進入某個賽事階段之後哪些欄位唯讀(schemas/28「各階段可改範圍」)。
//
// 回傳的是 JSON 路徑(如 "bp"、"format.best_of"),給日後「改 config 的 RPC」
// 在寫入前比對用。phase 是 tournament.Phase 的字面值;這裡收字串是因為
// rules 不能 import tournament。
//
// TODO(schemas/28):目前只涵蓋以**階段**為鎖點的那幾列(進入 drawing 之後)。
// 其餘幾列的鎖點是場次狀態,不是賽事階段 —— betting.markets / odds / parlay
// 鎖在「第一場進入 ready」、handicap.draw_pools 鎖在「第一場封盤」、prizes 鎖在
// 「發獎之後」—— 需要讀 matches 才判斷得出,等改 config 的 RPC 真的出現時
// 再與它一起做。改 config 的 RPC 目前不存在,所以這個入口還沒有呼叫端。
func LockedAfter(phase string) []string {
	switch phase {
	case "drawing", "in_progress", "finished":
		return []string{"ranks", "bp", "format.best_of", "format.third_place_match"}
	default:
		return nil
	}
}

// normalize 是 Parse 與 Validate 共用的**唯一一份**值域規則。
//
// 每一條都是「不合法 → 記一筆問題 → 退回預設」。Parse 直接用修過的結果,
// Validate 只看有沒有問題。同一個壞值在兩邊的判定因此必然一致,只是處置不同。
func normalize(c *Config) []error {
	var problems []error
	d := Default()

	if c.Version != CurrentVersion {
		problems = append(problems, fmt.Errorf("version 必須是 %d,得到 %d", CurrentVersion, c.Version))
		c.Version = CurrentVersion
	}

	problems = append(problems, normalizeRanks(c, d)...)

	if _, ok := bpKinds[c.BP.Kind]; !ok {
		problems = append(problems, fmt.Errorf("bp.kind=%q 不是已登記的 kind,退回 %q", c.BP.Kind, d.BP.Kind))
		c.BP.Kind = d.BP.Kind
	}
	if c.BP.PerRankGap <= 0 {
		problems = append(problems, fmt.Errorf("bp.per_rank_gap 必須為正數,得到 %d,退回 %d", c.BP.PerRankGap, d.BP.PerRankGap))
		c.BP.PerRankGap = d.BP.PerRankGap
	}

	// 幾局幾勝必須是正奇數:偶數會有平手,而單淘汰沒有平手這回事。
	if c.Format.BestOf < 1 || c.Format.BestOf%2 == 0 {
		problems = append(problems, fmt.Errorf("format.best_of 必須是正奇數,得到 %d,退回 %d", c.Format.BestOf, d.Format.BestOf))
		c.Format.BestOf = d.Format.BestOf
	}

	// null / 缺欄位 = 不限制,這是定案的預設。<= 0 則是打錯了:0 代表「一項都不能買」,
	// 沒有人會刻意這樣設定,而把它當真會讓整場賽事的讓武全部買不下去。
	if q := c.Handicap.ItemMaxQty; q != nil && *q <= 0 {
		problems = append(problems, fmt.Errorf("handicap.item_max_qty 必須為正數或 null,得到 %d,退回不限制", *q))
		c.Handicap.ItemMaxQty = nil
	}
	if c.Handicap.DrawPools == nil {
		c.Handicap.DrawPools = clonePools(d.Handicap.DrawPools)
	}
	for name, list := range c.Handicap.DrawPools {
		if strings.TrimSpace(name) == "" {
			problems = append(problems, errors.New("handicap.draw_pools 有空白的清單名稱,整組忽略"))
			delete(c.Handicap.DrawPools, name)
			continue
		}
		kept := list[:0]
		for _, item := range list {
			if strings.TrimSpace(item) == "" {
				problems = append(problems, fmt.Errorf("handicap.draw_pools.%s 有空白項目,已忽略", name))
				continue
			}
			kept = append(kept, item)
		}
		c.Handicap.DrawPools[name] = kept
	}

	if _, ok := timerStarts[c.Timer.Start]; !ok {
		problems = append(problems, fmt.Errorf("timer.start=%q 不是已登記的 kind,退回 %q", c.Timer.Start, d.Timer.Start))
		c.Timer.Start = d.Timer.Start
	}

	problems = append(problems, normalizeMarkets(c)...)
	problems = append(problems, normalizeOdds(&c.Betting.Odds, d.Betting.Odds)...)

	if c.Betting.Parlay.LegsPerMatch < 1 {
		problems = append(problems, fmt.Errorf("betting.parlay.legs_per_match 必須 >= 1,得到 %d,退回 %d",
			c.Betting.Parlay.LegsPerMatch, d.Betting.Parlay.LegsPerMatch))
		c.Betting.Parlay.LegsPerMatch = d.Betting.Parlay.LegsPerMatch
	}
	if _, ok := closeAtKinds[c.Betting.CloseAt]; !ok {
		problems = append(problems, fmt.Errorf("betting.close_at=%q 不是已登記的 kind,退回 %q", c.Betting.CloseAt, d.Betting.CloseAt))
		c.Betting.CloseAt = d.Betting.CloseAt
	}

	// 獎金可以是 0(不發),但不能是負數(那等於向得獎者收錢)。
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"prizes.champion", &c.Prizes.Champion},
		{"prizes.runner_up", &c.Prizes.RunnerUp},
		{"prizes.third", &c.Prizes.Third},
		{"prizes.participation", &c.Prizes.Participation},
	} {
		if *f.v < 0 {
			problems = append(problems, fmt.Errorf("%s 不可為負數,得到 %d,退回 0", f.name, *f.v))
			*f.v = 0
		}
	}
	return problems
}

// normalizeRanks 要求 level 恰為 1..N 連續且唯一、每段有名稱。
//
// 任何一項不成立就整組退回預設:段位表是一個整體(BP 依 level 差算),
// 只修一段等於替裁判猜他想要幾段。
func normalizeRanks(c *Config, d Config) []error {
	var problems []error
	if len(c.Ranks) == 0 {
		problems = append(problems, errors.New("ranks 不可為空,退回預設四段"))
		c.Ranks = d.Ranks
		return problems
	}
	for i, ri := range c.Ranks {
		switch {
		case ri.Level != i+1:
			problems = append(problems, fmt.Errorf("ranks 的 level 必須是 1..%d 連續且唯一,第 %d 筆是 level=%d,整組退回預設",
				len(c.Ranks), i, ri.Level))
		case strings.TrimSpace(ri.Name) == "":
			problems = append(problems, fmt.Errorf("ranks level=%d 缺名稱,整組退回預設", ri.Level))
		default:
			continue
		}
		c.Ranks = d.Ranks
		return problems
	}
	return nil
}

// normalizeMarkets 逐筆檢查盤口:未知 kind 丟掉、duration 沒有線丟掉、
// 非 duration 帶了線清掉、(kind, line_seconds) 重複的丟掉。
func normalizeMarkets(c *Config) []error {
	var problems []error
	seen := make(map[MarketRule]struct{}, len(c.Betting.Markets))
	kept := make([]MarketRule, 0, len(c.Betting.Markets))
	for i, m := range c.Betting.Markets {
		if _, ok := marketKinds[m.Kind]; !ok {
			problems = append(problems, fmt.Errorf("betting.markets[%d].kind=%q 不是已登記的 kind,整筆忽略", i, m.Kind))
			continue
		}
		if m.Kind == MarketDuration {
			if m.LineSeconds <= 0 {
				problems = append(problems, fmt.Errorf("betting.markets[%d] 是 duration 但 line_seconds=%d,必須 > 0,整筆忽略", i, m.LineSeconds))
				continue
			}
		} else if m.LineSeconds != 0 {
			problems = append(problems, fmt.Errorf("betting.markets[%d] 是 %s,不該有 line_seconds(%d),已清掉", i, m.Kind, m.LineSeconds))
			m.LineSeconds = 0
		}
		if _, dup := seen[m]; dup {
			problems = append(problems, fmt.Errorf("betting.markets[%d] 與前面的 %s 重複,整筆忽略", i, describeMarket(m)))
			continue
		}
		seen[m] = struct{}{}
		kept = append(kept, m)
	}
	c.Betting.Markets = kept
	return problems
}

func describeMarket(m MarketRule) string {
	if m.Kind == MarketDuration {
		return fmt.Sprintf("%s(line_seconds=%d)", m.Kind, m.LineSeconds)
	}
	return m.Kind
}

// normalizeOdds 檢查賠率參數(schemas/28:smoothing >= 1、0 <= vig_bps < 10000、
// min_odds_milli >= 1000、max_odds_milli > min_odds_milli)。
func normalizeOdds(o *OddsRule, d OddsRule) []error {
	var problems []error
	if _, ok := oddsKinds[o.Kind]; !ok {
		problems = append(problems, fmt.Errorf("betting.odds.kind=%q 不是已登記的 kind,退回 %q", o.Kind, d.Kind))
		o.Kind = d.Kind
	}
	if o.Smoothing < 1 {
		problems = append(problems, fmt.Errorf("betting.odds.smoothing 必須 >= 1,得到 %d,退回 %d", o.Smoothing, d.Smoothing))
		o.Smoothing = d.Smoothing
	}
	// 抽水可以是 0(不抽),但不能是負數(等於倒貼)也不能到 100%(全吃)。
	if o.VigBPS < 0 || o.VigBPS >= maxVigBPS {
		problems = append(problems, fmt.Errorf("betting.odds.vig_bps 必須在 0..%d,得到 %d,退回 %d", maxVigBPS-1, o.VigBPS, d.VigBPS))
		o.VigBPS = d.VigBPS
	}
	if o.MinOddsMilli < minOddsFloorMilli {
		problems = append(problems, fmt.Errorf("betting.odds.min_odds_milli 必須 >= %d,得到 %d,退回 %d", minOddsFloorMilli, o.MinOddsMilli, d.MinOddsMilli))
		o.MinOddsMilli = d.MinOddsMilli
	}
	if o.MaxOddsMilli <= 0 {
		problems = append(problems, fmt.Errorf("betting.odds.max_odds_milli 必須為正數,得到 %d,退回 %d", o.MaxOddsMilli, d.MaxOddsMilli))
		o.MaxOddsMilli = d.MaxOddsMilli
	}
	if o.MaxParlayMilli <= 0 {
		problems = append(problems, fmt.Errorf("betting.odds.max_parlay_milli 必須為正數,得到 %d,退回 %d", o.MaxParlayMilli, d.MaxParlayMilli))
		o.MaxParlayMilli = d.MaxParlayMilli
	}
	// 上下限反了會讓「夾在 min 與 max 之間」這個動作沒有解。
	// 兩個都退回預設而不是只修一邊 —— 只修一邊等於替裁判猜他想改的是哪一個。
	if o.MinOddsMilli >= o.MaxOddsMilli {
		problems = append(problems, fmt.Errorf("betting.odds.min_odds_milli(%d) 必須小於 max_odds_milli(%d),兩者都退回預設",
			o.MinOddsMilli, o.MaxOddsMilli))
		o.MinOddsMilli, o.MaxOddsMilli = d.MinOddsMilli, d.MaxOddsMilli
	}
	return problems
}

// clone 深複製切片與 map,讓 Validate 的 normalize 不會動到呼叫端的那一份。
func clone(c Config) Config {
	if c.Ranks != nil {
		ranks := make([]RankInfo, len(c.Ranks))
		copy(ranks, c.Ranks)
		c.Ranks = ranks
	}
	if c.Betting.Markets != nil {
		markets := make([]MarketRule, len(c.Betting.Markets))
		copy(markets, c.Betting.Markets)
		c.Betting.Markets = markets
	}
	if c.Handicap.DrawPools != nil {
		c.Handicap.DrawPools = clonePools(c.Handicap.DrawPools)
	}
	if c.Handicap.ItemMaxQty != nil {
		q := *c.Handicap.ItemMaxQty
		c.Handicap.ItemMaxQty = &q
	}
	return c
}
