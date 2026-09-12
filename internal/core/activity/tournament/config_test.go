package tournament

import (
	"errors"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
)

// TestParseConfigEmptyUsesDefaults 沒有設定時要退回一份完整可用的預設。
func TestParseConfigEmptyUsesDefaults(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{nil, []byte(""), []byte("{}")} {
		cfg, err := ParseConfig(raw)
		if err != nil {
			t.Fatalf("空設定不該回錯誤,得到 %v", err)
		}
		if cfg.BPPerRankGap != DefaultBPPerRankGap {
			t.Errorf("bp_per_rank_gap = %d, 要 %d", cfg.BPPerRankGap, DefaultBPPerRankGap)
		}
		if len(cfg.Ranks) != 4 {
			t.Fatalf("段位要有四段,得到 %d", len(cfg.Ranks))
		}
		if cfg.HandicapItemMaxQty != nil {
			t.Error("handicap_item_max_qty 預設應為不限制(nil)")
		}
	}
}

// TestParseConfigFull 一份完整合法的設定要原樣讀進來。
// 這份 JSON 直接抄自 schemas/20,不是從程式反推的。
func TestParseConfigFull(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
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
	}`)

	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("合法設定不該回錯誤:%v", err)
	}
	if cfg.BPPerRankGap != 8 {
		t.Errorf("bp_per_rank_gap=%d", cfg.BPPerRankGap)
	}
	if cfg.Odds != (OddsConfig{Smoothing: 5, VigBPS: 800, MinOddsMilli: 1050, MaxOddsMilli: 12000, MaxParlayMilli: 300000}) {
		t.Errorf("odds 讀錯:%+v", cfg.Odds)
	}
	if cfg.Prizes != (Prizes{Champion: 5000, RunnerUp: 3000, Third: 1000, Participation: 100}) {
		t.Errorf("prizes 讀錯:%+v", cfg.Prizes)
	}
	if cfg.HandicapItemMaxQty != nil {
		t.Error("null 應解成不限制")
	}
	wuwo, ok := cfg.Rank(bp.RankWuwo)
	if !ok || wuwo.Name != "無我" || wuwo.Title != "歷戰之境" {
		t.Errorf("無我讀錯:%+v ok=%v", wuwo, ok)
	}
	// 沒填 description 時要保留預設措辭,而不是變成空字串 ——
	// 規則頁是靠伺服器給文案的,空字串會在畫面上留下一塊空白。
	if wuwo.Description == "" {
		t.Error("沒填 description 時應保留預設描述")
	}
}

// TestParseConfigBadBPGapFallsBack 是這個函式存在的核心理由。
//
// 把 bp_per_rank_gap 寫成 0 或負數,如果照單全收,整場賽事的每一場
// 都會變成「沒有讓武」—— 而讓武是這屆活動的全部意義,對戰表上卻看不出異常。
// 所以壞值一律退回 8,同時把問題報出來。
func TestParseConfigBadBPGapFallsBack(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`{"bp_per_rank_gap": 0}`, `{"bp_per_rank_gap": -8}`} {
		cfg, err := ParseConfig([]byte(raw))
		if !errors.Is(err, ErrConfigMalformed) {
			t.Errorf("%s 要回 ErrConfigMalformed,得到 %v", raw, err)
		}
		if cfg.BPPerRankGap != DefaultBPPerRankGap {
			t.Errorf("%s 之後 bp_per_rank_gap = %d,要退回 %d", raw, cfg.BPPerRankGap, DefaultBPPerRankGap)
		}
	}
}

// TestParseConfigBrokenJSONStillUsable 整份 JSON 壞掉時仍要交出一份能跑的設定。
func TestParseConfigBrokenJSONStillUsable(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"bp_per_rank_gap": `))
	if !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("壞 JSON 要回 ErrConfigMalformed,得到 %v", err)
	}
	if cfg.BPPerRankGap != DefaultBPPerRankGap || len(cfg.Ranks) != 4 {
		t.Errorf("壞 JSON 之後設定必須仍然完整可用:%+v", cfg)
	}
}

// TestParseConfigPartialRanksMerge 只改一段的措辭時,其餘三段要保留預設。
func TestParseConfigPartialRanksMerge(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"ranks":[{"level":2,"name":"斷水改","description":"本屆新描述"}]}`))
	if err != nil {
		t.Fatalf("部分覆寫是合法的,不該回錯誤:%v", err)
	}
	if len(cfg.Ranks) != 4 {
		t.Fatalf("段位仍要有四段,得到 %d", len(cfg.Ranks))
	}
	duanshui, _ := cfg.Rank(bp.RankDuanshui)
	if duanshui.Name != "斷水改" || duanshui.Description != "本屆新描述" {
		t.Errorf("被覆寫的段位讀錯:%+v", duanshui)
	}
	// title 沒填,要保留預設而不是清空。
	if duanshui.Title != "初成之境" {
		t.Errorf("沒填的欄位要保留預設,得到 title=%q", duanshui.Title)
	}
	kaishan, _ := cfg.Rank(bp.RankKaishan)
	if kaishan.Name != "開山" {
		t.Errorf("沒被提到的段位不該變動,得到 %+v", kaishan)
	}
}

// TestParseConfigRanksOrdered 段位一律由低至高排序,不管 JSON 裡怎麼放。
// 規則頁直接照順序渲染,排序責任在伺服器。
func TestParseConfigRanksOrdered(t *testing.T) {
	t.Parallel()

	cfg, _ := ParseConfig([]byte(`{"ranks":[{"level":4,"name":"A"},{"level":1,"name":"B"}]}`))
	for i, r := range cfg.Ranks {
		if int(r.Rank) != i+1 {
			t.Fatalf("Ranks[%d].Rank = %d,應為 %d", i, r.Rank, i+1)
		}
	}
}

// TestParseConfigBadRankLevel 段位 level 超出 1..4 時整筆忽略,不污染其他段。
func TestParseConfigBadRankLevel(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"ranks":[{"level":9,"name":"傳說"},{"name":"缺level"}]}`))
	if !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("要回 ErrConfigMalformed,得到 %v", err)
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

// TestParseConfigOddsBounds 賠率上下限反了要兩個都退回預設。
func TestParseConfigOddsBounds(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"odds":{"min_odds_milli":20000,"max_odds_milli":3000}}`))
	if !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("要回 ErrConfigMalformed,得到 %v", err)
	}
	if cfg.Odds.MinOddsMilli != DefaultMinOddsMilli || cfg.Odds.MaxOddsMilli != DefaultMaxOddsMilli {
		t.Errorf("上下限反了要兩個都退回預設,得到 min=%d max=%d",
			cfg.Odds.MinOddsMilli, cfg.Odds.MaxOddsMilli)
	}
}

// TestParseConfigVigRange 抽水可以是 0(不抽),但不能是負數也不能到 100%。
func TestParseConfigVigRange(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"odds":{"vig_bps":0}}`))
	if err != nil {
		t.Errorf("vig_bps=0 是合法的(不抽水),不該回錯誤:%v", err)
	}
	if cfg.Odds.VigBPS != 0 {
		t.Errorf("vig_bps 應為 0,得到 %d", cfg.Odds.VigBPS)
	}

	for _, raw := range []string{`{"odds":{"vig_bps":-1}}`, `{"odds":{"vig_bps":10000}}`} {
		cfg, err := ParseConfig([]byte(raw))
		if !errors.Is(err, ErrConfigMalformed) {
			t.Errorf("%s 要回 ErrConfigMalformed,得到 %v", raw, err)
		}
		if cfg.Odds.VigBPS != DefaultVigBPS {
			t.Errorf("%s 之後 vig_bps = %d,要退回 %d", raw, cfg.Odds.VigBPS, DefaultVigBPS)
		}
	}
}

// TestParseConfigNegativePrizes 獎金不可為負(那等於向得獎者收錢)。
func TestParseConfigNegativePrizes(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"prizes":{"champion":-100,"runner_up":50}}`))
	if !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("要回 ErrConfigMalformed,得到 %v", err)
	}
	if cfg.Prizes.Champion != 0 {
		t.Errorf("負獎金要退回 0,得到 %d", cfg.Prizes.Champion)
	}
	if cfg.Prizes.RunnerUp != 50 {
		t.Errorf("同一組裡合法的欄位不該被牽連,得到 %d", cfg.Prizes.RunnerUp)
	}
}

// TestParseConfigHandicapItemMaxQty null / 正數 / 非正數三種情況。
func TestParseConfigHandicapItemMaxQty(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"handicap_item_max_qty":3}`))
	if err != nil {
		t.Fatalf("正數是合法的:%v", err)
	}
	if cfg.HandicapItemMaxQty == nil || *cfg.HandicapItemMaxQty != 3 {
		t.Errorf("上限應為 3,得到 %v", cfg.HandicapItemMaxQty)
	}

	// 0 代表「一項都不能買」,沒有人會刻意這樣設定,而照單全收會讓
	// 整場賽事的讓武全部買不下去 —— 所以當成打錯,退回不限制。
	cfg, err = ParseConfig([]byte(`{"handicap_item_max_qty":0}`))
	if !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("0 要回 ErrConfigMalformed,得到 %v", err)
	}
	if cfg.HandicapItemMaxQty != nil {
		t.Errorf("0 之後應退回不限制,得到 %v", *cfg.HandicapItemMaxQty)
	}
}

// TestParseConfigReportsEveryProblem 多個壞欄位時要一次全部報出來。
// 只報第一個的話,裁判會修一個、再跑一次、再修一個。
func TestParseConfigReportsEveryProblem(t *testing.T) {
	t.Parallel()

	_, err := ParseConfig([]byte(`{"bp_per_rank_gap":-1,"handicap_item_max_qty":0,"prizes":{"third":-5}}`))
	if err == nil {
		t.Fatal("應該回錯誤")
	}
	msg := err.Error()
	for _, want := range []string{"bp_per_rank_gap", "handicap_item_max_qty", "prizes.third"} {
		if !strings.Contains(msg, want) {
			t.Errorf("錯誤訊息應提到 %s:%v", want, err)
		}
	}
}

// TestDefaultConfigIsIsolated 兩次呼叫拿到的段位切片不可共用底層陣列。
func TestDefaultConfigIsIsolated(t *testing.T) {
	t.Parallel()

	a := DefaultConfig()
	a.Ranks[0].Name = "被改過"
	if DefaultConfig().Ranks[0].Name != "開山" {
		t.Error("DefaultConfig() 的段位切片被共用了,一次改動會污染所有賽事")
	}
}
