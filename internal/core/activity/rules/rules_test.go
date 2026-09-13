package rules

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// v1JSON 是升版前 tournaments.config 的形狀(schemas/20 舊契約,MarshalConfig v1 的輸出)。
const v1JSON = `{
  "bp_per_rank_gap": 8,
  "ranks": [
    {"level": 1, "name": "開山", "title": "初試之境"},
    {"level": 2, "name": "斷水", "title": "初成之境"},
    {"level": 3, "name": "飛花", "title": "純熟之境"},
    {"level": 4, "name": "無我", "title": "歷戰之境"}
  ],
  "odds": {
    "smoothing": 5,
    "vig_bps": 800,
    "min_odds_milli": 1050,
    "max_odds_milli": 12000,
    "max_parlay_milli": 300000
  },
  "prizes": {"champion": 5000, "runner_up": 3000, "third": 1000, "participation": 100},
  "handicap_item_max_qty": null
}`

// migration00008JSON 是 migrations/activity/00008_config_v2.sql 兩句 UPDATE 對
// 《百業試鋒》這一屆產生的形狀:通用升版(jsonb_strip_nulls 拿掉了 item_max_qty)
// 再套第一屆的值(三局兩勝、季軍戰、四種盤口、duration 線 90 秒)。
// 這是開發庫此刻的真實資料,Parse 讀它必須零診斷。
const migration00008JSON = `{
  "version": 2,
  "ranks": [
    {"level": 1, "name": "開山", "title": "初試之境", "description": "初入 PVP、實戰場次較少。"},
    {"level": 2, "name": "斷水", "title": "初成之境", "description": "已具操作能力,對局觀念尚未成熟。"},
    {"level": 3, "name": "飛花", "title": "純熟之境", "description": "成熟的 PVP 實力與足夠實戰經驗。"},
    {"level": 4, "name": "無我", "title": "歷戰之境", "description": "頂尖實戰能力與大量對局經驗。"}
  ],
  "bp": {"kind": "linear_gap", "per_rank_gap": 8},
  "format": {"best_of": 3, "preamble_every_round": true, "third_place_match": true},
  "handicap": {"draw_pools": {"wuxue": []}},
  "timer": {"start": "round_start"},
  "betting": {
    "enabled": true,
    "markets": [
      {"kind": "match_winner"},
      {"kind": "round_winner"},
      {"kind": "duration", "line_seconds": 90},
      {"kind": "score"}
    ],
    "odds": {"smoothing": 5, "vig_bps": 800, "min_odds_milli": 1050, "max_odds_milli": 12000, "max_parlay_milli": 300000, "kind": "vote_share"},
    "parlay": {"legs_per_match": 1},
    "close_at": "first_round_start"
  },
  "prizes": {"champion": 0, "runner_up": 0, "third": 0, "participation": 0}
}`

// ── Parse ────────────────────────────────────────────────────────

// 沒有設定時要退回一份完整可用的預設,而且預設本身必須通過 Validate ——
// 否則「壞設定退回預設」退到的是另一份壞設定。
func TestParseEmptyIsDefaultAndValid(t *testing.T) {
	t.Parallel()
	for _, raw := range [][]byte{nil, []byte(""), []byte("{}")} {
		cfg, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) 不該回錯誤:%v", raw, err)
		}
		if !reflect.DeepEqual(cfg, Default()) {
			t.Errorf("Parse(%q) 應等於 Default():\n got %+v\nwant %+v", raw, cfg, Default())
		}
	}
	if err := Validate(Default()); err != nil {
		t.Fatalf("Default() 必須通過 Validate:%v", err)
	}
}

// v1 輸入:零診斷,依 schemas/28 的升版表補成 v2,且 v2 才有的區塊是 v1 語意。
func TestParseV1UpgradesWithoutDiagnostics(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(v1JSON))
	if err != nil {
		t.Fatalf("合法的 v1 不該有診斷:%v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Errorf("version = %d,want %d", cfg.Version, CurrentVersion)
	}
	if cfg.BP != (BPRule{Kind: BPKindLinearGap, PerRankGap: 8}) {
		t.Errorf("bp = %+v", cfg.BP)
	}
	wantOdds := OddsRule{Kind: OddsKindVoteShare, Smoothing: 5, VigBPS: 800, MinOddsMilli: 1050, MaxOddsMilli: 12000, MaxParlayMilli: 300000}
	if cfg.Betting.Odds != wantOdds {
		t.Errorf("betting.odds = %+v,want %+v", cfg.Betting.Odds, wantOdds)
	}
	if cfg.Prizes != (Prizes{Champion: 5000, RunnerUp: 3000, Third: 1000, Participation: 100}) {
		t.Errorf("prizes = %+v", cfg.Prizes)
	}
	if cfg.Handicap.ItemMaxQty != nil {
		t.Error("handicap_item_max_qty: null 應解成不限制")
	}
	// v1 語意:單場定勝負、只有勝負盤、無季軍戰(schemas/28 那張表的最後兩列)。
	if cfg.Format != (FormatRule{BestOf: 1, PreambleEveryRound: true, ThirdPlaceMatch: false}) {
		t.Errorf("format = %+v,v1 升版必須是 v1 語意", cfg.Format)
	}
	if !reflect.DeepEqual(cfg.Betting.Markets, []MarketRule{{Kind: MarketMatchWinner}}) {
		t.Errorf("markets = %+v,v1 升版只有勝負盤", cfg.Betting.Markets)
	}
	if cfg.Betting.Parlay.LegsPerMatch != 1 || cfg.Betting.CloseAt != CloseAtFirstRoundStart || cfg.Timer.Start != TimerRoundStart {
		t.Errorf("v2 才有的區塊要補預設:parlay=%+v close_at=%q timer=%+v", cfg.Betting.Parlay, cfg.Betting.CloseAt, cfg.Timer)
	}
	wuwo, ok := cfg.Rank(4)
	if !ok || wuwo.Name != "無我" || wuwo.Title != "歷戰之境" {
		t.Errorf("無我讀錯:%+v ok=%v", wuwo, ok)
	}
	// 沒填 description 時要保留預設措辭,而不是變成空字串 ——
	// 規則頁是靠伺服器給文案的,空字串會在畫面上留下一塊空白。
	if wuwo.Description == "" {
		t.Error("沒填 description 時應保留預設描述")
	}
}

// 開發庫那屆的 v2 config:零診斷,值與 migration 寫的一致。
func TestParseMigration00008ShapeWithoutDiagnostics(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(migration00008JSON))
	if err != nil {
		t.Fatalf("00008 產生的 config 不該有診斷:%v", err)
	}
	if cfg.Format != (FormatRule{BestOf: 3, PreambleEveryRound: true, ThirdPlaceMatch: true}) {
		t.Errorf("format = %+v", cfg.Format)
	}
	wantMarkets := []MarketRule{
		{Kind: MarketMatchWinner}, {Kind: MarketRoundWinner},
		{Kind: MarketDuration, LineSeconds: 90}, {Kind: MarketScore},
	}
	if !reflect.DeepEqual(cfg.Betting.Markets, wantMarkets) {
		t.Errorf("markets = %+v,want %+v", cfg.Betting.Markets, wantMarkets)
	}
	if cfg.BP != (BPRule{Kind: BPKindLinearGap, PerRankGap: 8}) {
		t.Errorf("bp = %+v", cfg.BP)
	}
	if cfg.Betting.Odds.Kind != OddsKindVoteShare || cfg.Betting.Odds.VigBPS != 800 {
		t.Errorf("odds = %+v", cfg.Betting.Odds)
	}
	if cfg.Handicap.ItemMaxQty != nil {
		t.Error("jsonb_strip_nulls 拿掉的 item_max_qty 要解成不限制")
	}
	if pool, ok := cfg.DrawPool(PoolWuxue); !ok || len(pool) != 0 {
		t.Errorf("draw_pools.wuxue 應存在且為空,得到 %v ok=%v", pool, ok)
	}
	if cfg.WinsNeeded() != 2 {
		t.Errorf("三局兩勝 WinsNeeded = %d", cfg.WinsNeeded())
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("開發庫那屆的 config 必須通過 Validate:%v", err)
	}
}

// 未知的 kind:Parse 記診斷並退回預設,Config 仍可用。每張登記表各一筆。
func TestParseUnknownKindFallsBack(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		raw   string
		check func(Config) bool
	}{
		"bp.kind": {
			`{"version":2,"bp":{"kind":"quadratic","per_rank_gap":9}}`,
			func(c Config) bool { return c.BP.Kind == BPKindLinearGap && c.BP.PerRankGap == 9 },
		},
		"betting.odds.kind": {
			`{"version":2,"betting":{"odds":{"kind":"bookmaker"}}}`,
			func(c Config) bool { return c.Betting.Odds.Kind == OddsKindVoteShare },
		},
		"betting.markets[].kind": {
			`{"version":2,"betting":{"markets":[{"kind":"match_winner"},{"kind":"first_blood"}]}}`,
			func(c Config) bool {
				return reflect.DeepEqual(c.Betting.Markets, []MarketRule{{Kind: MarketMatchWinner}})
			},
		},
		"timer.start": {
			`{"version":2,"timer":{"start":"match_start"}}`,
			func(c Config) bool { return c.Timer.Start == TimerRoundStart },
		},
		"betting.close_at": {
			`{"version":2,"betting":{"close_at":"never"}}`,
			func(c Config) bool { return c.Betting.CloseAt == CloseAtFirstRoundStart },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(tc.raw))
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("未知 kind 要回 ErrMalformed,得到 %v", err)
			}
			if !tc.check(cfg) {
				t.Errorf("退回預設後的設定不對:%+v", cfg)
			}
			if verr := Validate(cfg); verr != nil {
				t.Errorf("退回預設後的 Config 必須合法:%v", verr)
			}
		})
	}
}

// bp.per_rank_gap 寫成 0 或負數,如果照單全收,整場賽事的每一場都會變成「沒有讓武」——
// 而讓武是這屆活動的全部意義,對戰表上卻看不出異常。所以壞值一律退回 8,同時報出來。
func TestParseBadPerRankGapFallsBack(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"bp_per_rank_gap": 0}`, `{"bp_per_rank_gap": -8}`,
		`{"version":2,"bp":{"per_rank_gap":0}}`,
	} {
		cfg, err := Parse([]byte(raw))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s 要回 ErrMalformed,得到 %v", raw, err)
		}
		if cfg.BP.PerRankGap != DefaultPerRankGap {
			t.Errorf("%s 之後 per_rank_gap = %d,要退回 %d", raw, cfg.BP.PerRankGap, DefaultPerRankGap)
		}
	}
}

// 整份 JSON 壞掉時仍要交出一份能跑的設定。
func TestParseBrokenJSONStillUsable(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"bp_per_rank_gap": `))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("壞 JSON 要回 ErrMalformed,得到 %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("壞 JSON 之後設定必須是完整預設:%+v", cfg)
	}
}

// 只改一段的措辭時,其餘三段要保留預設;不管 JSON 裡怎麼放,一律由低至高排序。
func TestParsePartialRanksMergeAndOrder(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"version":2,"ranks":[{"level":4,"name":"無我改"},{"level":2,"name":"斷水改","description":"本屆新描述"}]}`))
	if err != nil {
		t.Fatalf("部分覆寫是合法的,不該回錯誤:%v", err)
	}
	if len(cfg.Ranks) != 4 {
		t.Fatalf("段位仍要有四段,得到 %d", len(cfg.Ranks))
	}
	for i, r := range cfg.Ranks {
		if r.Level != i+1 {
			t.Fatalf("Ranks[%d].Level = %d,應為 %d", i, r.Level, i+1)
		}
	}
	duanshui, _ := cfg.Rank(2)
	if duanshui.Name != "斷水改" || duanshui.Description != "本屆新描述" || duanshui.Title != "初成之境" {
		t.Errorf("被覆寫的段位讀錯(title 沒填要保留預設):%+v", duanshui)
	}
	if kaishan, _ := cfg.Rank(1); kaishan.Name != "開山" {
		t.Errorf("沒被提到的段位不該變動:%+v", kaishan)
	}
}

// 段位 level 超出 1..4 或缺 level 時整筆忽略,不污染其他段。
func TestParseBadRankLevelIgnored(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"version":2,"ranks":[{"level":9,"name":"傳說"},{"name":"缺level"}]}`))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("要回 ErrMalformed,得到 %v", err)
	}
	if len(cfg.Ranks) != 4 {
		t.Fatalf("段位仍要有四段,得到 %d", len(cfg.Ranks))
	}
	for _, r := range cfg.Ranks {
		if r.Name == "傳說" || r.Name == "缺level" {
			t.Errorf("不合法的段位定義不該進到結果:%+v", r)
		}
	}
}

// 賠率上下限反了要兩個都退回預設;抽水 0 合法、負數與 100% 不合法。
func TestParseOddsBounds(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"odds":{"min_odds_milli":20000,"max_odds_milli":3000}}`))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("要回 ErrMalformed,得到 %v", err)
	}
	if cfg.Betting.Odds.MinOddsMilli != DefaultMinOddsMilli || cfg.Betting.Odds.MaxOddsMilli != DefaultMaxOddsMilli {
		t.Errorf("上下限反了要兩個都退回預設,得到 min=%d max=%d", cfg.Betting.Odds.MinOddsMilli, cfg.Betting.Odds.MaxOddsMilli)
	}

	cfg, err = Parse([]byte(`{"version":2,"betting":{"odds":{"vig_bps":0}}}`))
	if err != nil || cfg.Betting.Odds.VigBPS != 0 {
		t.Errorf("vig_bps=0 是合法的(不抽水):err=%v vig=%d", err, cfg.Betting.Odds.VigBPS)
	}
	for _, raw := range []string{`{"odds":{"vig_bps":-1}}`, `{"odds":{"vig_bps":10000}}`} {
		cfg, err := Parse([]byte(raw))
		if !errors.Is(err, ErrMalformed) || cfg.Betting.Odds.VigBPS != DefaultVigBPS {
			t.Errorf("%s:err=%v vig=%d,要 ErrMalformed 且退回 %d", raw, err, cfg.Betting.Odds.VigBPS, DefaultVigBPS)
		}
	}
}

// duration 沒有線在 Parse 是整筆忽略;非 duration 帶了線是清掉但保留盤口。
func TestParseMarketLineSeconds(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"version":2,"betting":{"markets":[
		{"kind":"duration"},
		{"kind":"duration","line_seconds":90},
		{"kind":"duration","line_seconds":120},
		{"kind":"score","line_seconds":5},
		{"kind":"duration","line_seconds":90}
	]}}`))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("要回 ErrMalformed,得到 %v", err)
	}
	want := []MarketRule{{Kind: MarketDuration, LineSeconds: 90}, {Kind: MarketDuration, LineSeconds: 120}, {Kind: MarketScore}}
	if !reflect.DeepEqual(cfg.Betting.Markets, want) {
		t.Errorf("markets = %+v,want %+v", cfg.Betting.Markets, want)
	}
}

// 獎金不可為負;同一組裡合法的欄位不該被牽連。
func TestParseNegativePrizes(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"prizes":{"champion":-100,"runner_up":50}}`))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("要回 ErrMalformed,得到 %v", err)
	}
	if cfg.Prizes.Champion != 0 || cfg.Prizes.RunnerUp != 50 {
		t.Errorf("prizes = %+v", cfg.Prizes)
	}
}

// item_max_qty:null / 正數 / 非正數 / 非數字。
func TestParseItemMaxQty(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"version":2,"handicap":{"item_max_qty":3}}`))
	if err != nil || cfg.Handicap.ItemMaxQty == nil || *cfg.Handicap.ItemMaxQty != 3 {
		t.Errorf("正數是合法的:err=%v qty=%v", err, cfg.Handicap.ItemMaxQty)
	}
	cfg, err = Parse([]byte(`{"handicap_item_max_qty":2}`))
	if err != nil || cfg.Handicap.ItemMaxQty == nil || *cfg.Handicap.ItemMaxQty != 2 {
		t.Errorf("v1 的 handicap_item_max_qty 要搬到 handicap.item_max_qty:err=%v qty=%v", err, cfg.Handicap.ItemMaxQty)
	}
	for _, raw := range []string{`{"version":2,"handicap":{"item_max_qty":0}}`, `{"version":2,"handicap":{"item_max_qty":"many"}}`} {
		cfg, err = Parse([]byte(raw))
		if !errors.Is(err, ErrMalformed) || cfg.Handicap.ItemMaxQty != nil {
			t.Errorf("%s:err=%v qty=%v,要 ErrMalformed 且退回不限制", raw, err, cfg.Handicap.ItemMaxQty)
		}
	}
}

// 多個壞欄位時要一次全部報出來。只報第一個的話,裁判會修一個、再跑一次、再修一個。
func TestParseReportsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"version":2,"bp":{"per_rank_gap":-1},"format":{"best_of":2},"prizes":{"third":-5}}`))
	if err == nil {
		t.Fatal("應該回錯誤")
	}
	for _, want := range []string{"bp.per_rank_gap", "format.best_of", "prizes.third"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("錯誤訊息應提到 %s:%v", want, err)
		}
	}
}

// 比程式新的版本:當成 v2 讀,但要報出來。
func TestParseUnknownVersion(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`{"version":3,"bp":{"kind":"linear_gap","per_rank_gap":10}}`))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("要回 ErrMalformed,得到 %v", err)
	}
	if cfg.Version != CurrentVersion || cfg.BP.PerRankGap != 10 {
		t.Errorf("認得的鍵仍要讀進來:%+v", cfg)
	}
}

// Default() 兩次呼叫拿到的切片與 map 不可共用底層。
func TestDefaultIsIsolated(t *testing.T) {
	t.Parallel()
	a := Default()
	a.Ranks[0].Name = "被改過"
	a.Betting.Markets[0].Kind = "被改過"
	a.Handicap.DrawPools[PoolWuxue] = append(a.Handicap.DrawPools[PoolWuxue], "被改過")
	b := Default()
	if b.Ranks[0].Name != "開山" || b.Betting.Markets[0].Kind != MarketMatchWinner || len(b.Handicap.DrawPools[PoolWuxue]) != 0 {
		t.Error("Default() 的切片或 map 被共用了,一次改動會污染所有賽事")
	}
}

// ParseBetting 只讀 betting 區塊,約定與 Parse 相同。
func TestParseBetting(t *testing.T) {
	t.Parallel()
	b, err := ParseBetting([]byte(`{"enabled":true,"markets":[{"kind":"score"},{"kind":"duration","line_seconds":90}],"odds":{"vig_bps":500,"kind":"vote_share"},"parlay":{"legs_per_match":2},"close_at":"first_round_start"}`))
	if err != nil {
		t.Fatalf("合法的 betting 不該有診斷:%v", err)
	}
	if b.Odds.VigBPS != 500 || b.Odds.Smoothing != DefaultOddsSmoothing || b.Parlay.LegsPerMatch != 2 || len(b.Markets) != 2 {
		t.Errorf("betting = %+v", b)
	}
	b, err = ParseBetting([]byte(`{"markets":[{"kind":"nope"}]}`))
	if !errors.Is(err, ErrMalformed) || len(b.Markets) != 0 {
		t.Errorf("未知盤口要報診斷並丟掉:err=%v markets=%+v", err, b.Markets)
	}
	if b, err := ParseBetting(nil); err != nil || !reflect.DeepEqual(b, Default().Betting) {
		t.Errorf("空 betting 要是預設:err=%v %+v", err, b)
	}
}

// ── Validate ─────────────────────────────────────────────────────

func fullConfig() Config {
	qty := int32(3)
	return Config{
		Version: CurrentVersion,
		Ranks: []RankInfo{
			{Level: 1, Name: "一", Title: "壹", Description: "甲"},
			{Level: 2, Name: "二", Title: "貳", Description: "乙"},
			{Level: 3, Name: "三", Title: "參", Description: "丙"},
			{Level: 4, Name: "四", Title: "肆", Description: "丁"},
		},
		BP:     BPRule{Kind: BPKindLinearGap, PerRankGap: 12},
		Format: FormatRule{BestOf: 5, PreambleEveryRound: false, ThirdPlaceMatch: true},
		Handicap: HandicapRule{
			ItemMaxQty: &qty,
			DrawPools:  map[string][]string{PoolWuxue: {"太極劍", "八卦掌"}, "other": {}},
		},
		Timer: TimerRule{Start: TimerRoundStart},
		Betting: BettingRule{
			Enabled: false,
			Markets: []MarketRule{
				{Kind: MarketMatchWinner}, {Kind: MarketRoundWinner},
				{Kind: MarketDuration, LineSeconds: 90}, {Kind: MarketDuration, LineSeconds: 120},
				{Kind: MarketScore},
			},
			Odds:    OddsRule{Kind: OddsKindVoteShare, Smoothing: 7, VigBPS: 0, MinOddsMilli: 1100, MaxOddsMilli: 9000, MaxParlayMilli: 200000},
			Parlay:  ParlayRule{LegsPerMatch: 2},
			CloseAt: CloseAtFirstRoundStart,
		},
		Prizes: Prizes{Champion: 5000, RunnerUp: 2000, Third: 0, Participation: 100},
	}
}

// 驗收條件 3:每一條規則各一個反例,且都要是 ErrMalformed。
func TestValidateRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"best_of=2(偶數)":             func(c *Config) { c.Format.BestOf = 2 },
		"best_of=0":                 func(c *Config) { c.Format.BestOf = 0 },
		"duration 缺 line_seconds":   func(c *Config) { c.Betting.Markets = []MarketRule{{Kind: MarketDuration}} },
		"非 duration 帶 line_seconds": func(c *Config) { c.Betting.Markets = []MarketRule{{Kind: MarketScore, LineSeconds: 5}} },
		"min_odds >= max_odds":      func(c *Config) { c.Betting.Odds.MinOddsMilli, c.Betting.Odds.MaxOddsMilli = 9000, 9000 },
		"min_odds < 1000":           func(c *Config) { c.Betting.Odds.MinOddsMilli = 999 },
		"smoothing=0":               func(c *Config) { c.Betting.Odds.Smoothing = 0 },
		"vig_bps=10000":             func(c *Config) { c.Betting.Odds.VigBPS = 10000 },
		"vig_bps 負數":                func(c *Config) { c.Betting.Odds.VigBPS = -1 },
		"max_parlay=0":              func(c *Config) { c.Betting.Odds.MaxParlayMilli = 0 },
		"未知 market kind":            func(c *Config) { c.Betting.Markets = []MarketRule{{Kind: "first_blood"}} },
		"重複的 market":                func(c *Config) { c.Betting.Markets = []MarketRule{{Kind: MarketScore}, {Kind: MarketScore}} },
		"未知 bp kind":                func(c *Config) { c.BP.Kind = "quadratic" },
		"空 bp kind":                 func(c *Config) { c.BP.Kind = "" },
		"未知 odds kind":              func(c *Config) { c.Betting.Odds.Kind = "bookmaker" },
		"未知 timer.start":            func(c *Config) { c.Timer.Start = "match_start" },
		"未知 close_at":               func(c *Config) { c.Betting.CloseAt = "never" },
		"per_rank_gap=0":            func(c *Config) { c.BP.PerRankGap = 0 },
		"ranks level 不連續(1,2,4)": func(c *Config) {
			c.Ranks = []RankInfo{{Level: 1, Name: "a"}, {Level: 2, Name: "b"}, {Level: 4, Name: "d"}}
		},
		"ranks level 不從 1 開始": func(c *Config) { c.Ranks = []RankInfo{{Level: 2, Name: "b"}} },
		"ranks level 重複":      func(c *Config) { c.Ranks = []RankInfo{{Level: 1, Name: "a"}, {Level: 1, Name: "a2"}} },
		"ranks 沒排序":           func(c *Config) { c.Ranks = []RankInfo{{Level: 2, Name: "b"}, {Level: 1, Name: "a"}} },
		"ranks 為空":            func(c *Config) { c.Ranks = nil },
		"ranks 缺名稱":           func(c *Config) { c.Ranks[2].Name = " " },
		"legs_per_match=0":    func(c *Config) { c.Betting.Parlay.LegsPerMatch = 0 },
		"item_max_qty=0":      func(c *Config) { z := int32(0); c.Handicap.ItemMaxQty = &z },
		"獎金為負":                func(c *Config) { c.Prizes.Participation = -1 },
		"version 不對":          func(c *Config) { c.Version = 1 },
		"draw_pools 有空白項目":    func(c *Config) { c.Handicap.DrawPools[PoolWuxue] = []string{"太極劍", " "} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := fullConfig()
			mutate(&c)
			if err := Validate(c); !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v,想要 ErrMalformed", err)
			}
		})
	}
}

// 合法的設定要放行,而且 Validate 不可改動呼叫端的那一份。
func TestValidateAcceptsAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	c := fullConfig()
	before := fullConfig()
	if err := Validate(c); err != nil {
		t.Fatalf("合法設定被拒:%v", err)
	}
	if !reflect.DeepEqual(c, before) {
		t.Errorf("Validate 改動了呼叫端的設定:\n got %+v\nwant %+v", c, before)
	}
	// 每個壞值 Validate 擋、Parse 修:兩邊共用同一份規則,修過的一定合法。
	c.Format.BestOf = 4
	c.Betting.Markets = append(c.Betting.Markets, MarketRule{Kind: "nope"})
	raw, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, perr := Parse(raw)
	if !errors.Is(perr, ErrMalformed) {
		t.Fatalf("Parse 要診斷出 Validate 會擋的東西:%v", perr)
	}
	if err := Validate(parsed); err != nil {
		t.Errorf("Parse 修過的設定必須通過 Validate:%v", err)
	}
}

// ── Marshal / 往返 ────────────────────────────────────────────────

// Parse(Marshal(c)) == c:這是 Create 的前提 —— 它把算出來的那一份寫進 config,
// 之後每一次讀取都走 Parse。兩者一旦分歧,開出來的規則與讀回來的規則會不同,
// 而且沒有任何地方會報錯。
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]Config{"全預設": Default(), "全部填滿": fullConfig()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := Marshal(want)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("往返不等:\n got %+v\nwant %+v\nraw %s", got, want, raw)
			}
		})
	}
}

// Marshal(Parse(x)) 對 v1 與 v2 輸入都要是完整的 v2 形狀,而且 v1 的鍵不能再出現。
func TestMarshalWritesFullV2(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{"v1": v1JSON, "v2(00008)": migration00008JSON, "空": "{}"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(in))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(raw, &top); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"version", "ranks", "bp", "format", "handicap", "timer", "betting", "prizes"} {
				if _, ok := top[key]; !ok {
					t.Errorf("完整 v2 必須有 %q:%s", key, raw)
				}
			}
			for _, key := range []string{"bp_per_rank_gap", "odds", "handicap_item_max_qty"} {
				if _, ok := top[key]; ok {
					t.Errorf("v1 的鍵 %q 不該再出現:%s", key, raw)
				}
			}
			if string(top["version"]) != "2" {
				t.Errorf("version = %s,want 2", top["version"])
			}
			// 「有這個旋鈕、目前不限制」要寫成 null,不是省略。
			var handicap map[string]json.RawMessage
			if err := json.Unmarshal(top["handicap"], &handicap); err != nil {
				t.Fatal(err)
			}
			if string(handicap["item_max_qty"]) != "null" {
				t.Errorf("item_max_qty 要寫 null,得到 %s", handicap["item_max_qty"])
			}
			// 再讀一次要得到同一份。
			again, err := Parse(raw)
			if err != nil {
				t.Fatalf("Marshal 出來的東西 Parse 不該有診斷:%v", err)
			}
			if !reflect.DeepEqual(again, cfg) {
				t.Errorf("Parse(Marshal(Parse(x))) != Parse(x):\n got %+v\nwant %+v", again, cfg)
			}
		})
	}
}

// v1 與「v1 升版後的 v2」寫出來必須位元相同:升版是純函式,沒有第二種答案。
func TestV1AndUpgradedV2MarshalIdentically(t *testing.T) {
	t.Parallel()
	fromV1, err := Parse([]byte(v1JSON))
	if err != nil {
		t.Fatal(err)
	}
	rawV1, _ := Marshal(fromV1)
	fromV2, err := Parse(rawV1)
	if err != nil {
		t.Fatal(err)
	}
	rawV2, _ := Marshal(fromV2)
	if string(rawV1) != string(rawV2) {
		t.Errorf("兩次序列化不同:\n%s\n%s", rawV1, rawV2)
	}
}

// ── 衍生值 ───────────────────────────────────────────────────────

// 驗收條件 5:順序釘住,它是 markets 的 outcome 定址依據。
func TestScoreOutcomes(t *testing.T) {
	t.Parallel()
	cases := map[int][]string{
		1: {"p1_1_0", "p2_1_0"},
		3: {"p1_2_0", "p1_2_1", "p2_2_1", "p2_2_0"},
		5: {"p1_3_0", "p1_3_1", "p1_3_2", "p2_3_2", "p2_3_1", "p2_3_0"},
	}
	for bestOf, want := range cases {
		c := Default()
		c.Format.BestOf = bestOf
		if got := c.ScoreOutcomes(); !reflect.DeepEqual(got, want) {
			t.Errorf("best_of=%d: %v,want %v", bestOf, got, want)
		}
		if got := c.WinsNeeded(); got != (bestOf+1)/2 {
			t.Errorf("best_of=%d: WinsNeeded = %d", bestOf, got)
		}
	}
}

func TestRankAndDrawPool(t *testing.T) {
	t.Parallel()
	c := Default()
	if _, ok := c.Rank(0); ok {
		t.Error("level 0 不存在")
	}
	if ri, ok := c.Rank(3); !ok || ri.Name != "飛花" {
		t.Errorf("Rank(3) = %+v ok=%v", ri, ok)
	}
	if pool, ok := c.DrawPool(PoolWuxue); !ok || pool == nil {
		t.Errorf("預設的 wuxue 清單要存在且非 nil:%v ok=%v", pool, ok)
	}
	if _, ok := c.DrawPool("direction_keys"); ok {
		t.Error("沒設的清單要回 false")
	}
}

func TestLockedAfter(t *testing.T) {
	t.Parallel()
	if got := LockedAfter("signup"); got != nil {
		t.Errorf("報名期不鎖任何欄位,得到 %v", got)
	}
	for _, phase := range []string{"drawing", "in_progress", "finished"} {
		got := LockedAfter(phase)
		for _, want := range []string{"ranks", "bp", "format.best_of", "format.third_place_match"} {
			found := false
			for _, g := range got {
				found = found || g == want
			}
			if !found {
				t.Errorf("%s 之後 %q 要鎖住,得到 %v", phase, want, got)
			}
		}
	}
}
