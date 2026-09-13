package tournament

import (
	"errors"
	"reflect"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/rules"
)

// 形狀、預設值、值域規則的測試都在 rules 套件;這裡只測這一層自己的東西:
// 委派沒有走樣、NewConfig 的疊加與致命錯誤、bp.Rank 到 level 的轉換。

// ParseConfig / MarshalConfig / DefaultConfig 必須是 rules 的同一份,
// 不能在委派時多做或少做任何事。
func TestConfigDelegatesToRules(t *testing.T) {
	t.Parallel()

	if !reflect.DeepEqual(DefaultConfig(), rules.Default()) {
		t.Error("DefaultConfig 與 rules.Default 不同")
	}
	raw := []byte(`{"bp_per_rank_gap":12,"odds":{"vig_bps":-1}}`)
	got, gotErr := ParseConfig(raw)
	want, wantErr := rules.Parse(raw)
	if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) {
		t.Errorf("ParseConfig 與 rules.Parse 結果不同:%+v / %v vs %+v / %v", got, gotErr, want, wantErr)
	}
	// 診斷錯誤要能用這個套件的 sentinel 判斷(transport 的 errmap 靠它)。
	if !errors.Is(gotErr, ErrConfigMalformed) || !errors.Is(gotErr, rules.ErrMalformed) {
		t.Errorf("ErrConfigMalformed 必須就是 rules.ErrMalformed:%v", gotErr)
	}
	a, _ := MarshalConfig(want)
	b, _ := rules.Marshal(want)
	if string(a) != string(b) {
		t.Error("MarshalConfig 與 rules.Marshal 輸出不同")
	}
}

// RankOf 把 bp.Rank 轉成 level 去查措辭;未評定(0)與超界回 false。
func TestRankOf(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	for r := bp.MinRank; r <= bp.MaxRank; r++ {
		ri, ok := RankOf(cfg, r)
		if !ok || ri.Level != int(r) || ri.Name != r.String() {
			t.Errorf("RankOf(%d) = %+v ok=%v", r, ri, ok)
		}
	}
	if _, ok := RankOf(cfg, bp.RankUnspecified); ok {
		t.Error("未評定不該查得到措辭")
	}
	if _, ok := RankOf(cfg, bp.Rank(5)); ok {
		t.Error("超界不該查得到措辭")
	}
}

// 沒填任何旋鈕就是預設;填了的要落在 v2 的位置上,沒填的維持預設。
func TestNewConfigOverlays(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig(ConfigOverrides{})
	if err != nil {
		t.Fatalf("空旋鈕不該失敗:%v", err)
	}
	if !reflect.DeepEqual(cfg, rules.Default()) {
		t.Errorf("空旋鈕應等於預設:%+v", cfg)
	}

	gap, zero, bestOf, third, qty := int64(12), int64(0), 3, true, int32(3)
	cfg, err = NewConfig(ConfigOverrides{
		BPPerRankGap:       &gap,
		VigBPS:             &zero,
		BestOf:             &bestOf,
		ThirdPlaceMatch:    &third,
		Prizes:             &Prizes{Champion: 5000, RunnerUp: 2000},
		HandicapItemMaxQty: &qty,
	})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if cfg.BP.PerRankGap != 12 || cfg.BP.Kind != rules.BPKindLinearGap {
		t.Errorf("bp = %+v", cfg.BP)
	}
	if cfg.Betting.Odds.VigBPS != 0 {
		t.Errorf("vig_bps = %d,填 0 是不抽水,不是沒填", cfg.Betting.Odds.VigBPS)
	}
	if cfg.Betting.Odds.MinOddsMilli != rules.DefaultMinOddsMilli {
		t.Errorf("沒填的 min_odds_milli 要維持預設,得到 %d", cfg.Betting.Odds.MinOddsMilli)
	}
	if cfg.Format != (rules.FormatRule{BestOf: 3, PreambleEveryRound: true, ThirdPlaceMatch: true}) {
		t.Errorf("format = %+v", cfg.Format)
	}
	if cfg.Prizes != (Prizes{Champion: 5000, RunnerUp: 2000}) {
		t.Errorf("prizes = %+v", cfg.Prizes)
	}
	if cfg.Handicap.ItemMaxQty == nil || *cfg.Handicap.ItemMaxQty != 3 {
		t.Errorf("item_max_qty = %v", cfg.Handicap.ItemMaxQty)
	}
	// 疊出來的一定要能通過寫入前的驗證,而且寫出去要能解回同一份。
	if err := rules.Validate(cfg); err != nil {
		t.Errorf("NewConfig 的結果必須通過 Validate:%v", err)
	}
	raw, _ := MarshalConfig(cfg)
	back, err := ParseConfig(raw)
	if err != nil || !reflect.DeepEqual(back, cfg) {
		t.Errorf("往返不等:err=%v\n got %+v\nwant %+v", err, back, cfg)
	}
}

// 壞旋鈕在建立時是**致命的**:回錯誤、第一個回傳值是零值,不做任何「幫他修」。
func TestNewConfigBadValuesAreFatal(t *testing.T) {
	t.Parallel()

	neg, bigMin, even := int64(-1), int64(99999), 2
	cases := map[string]ConfigOverrides{
		"bp_per_rank_gap 為負":       {BPPerRankGap: &neg},
		"vig_bps 為負":               {VigBPS: &neg},
		"下限高於上限":                   {MinOddsMilli: &bigMin},
		"best_of 偶數":               {BestOf: &even},
		"獎金為負":                     {Prizes: &Prizes{Champion: -1}},
		"handicap_item_max_qty 為負": {HandicapItemMaxQty: ptrOf(int32(-1))},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := NewConfig(o)
			if !errors.Is(err, ErrConfigMalformed) {
				t.Fatalf("err = %v,想要 ErrConfigMalformed", err)
			}
			if !reflect.DeepEqual(cfg, Config{}) {
				t.Errorf("失敗時不該交出一份「修過的」設定:%+v", cfg)
			}
		})
	}
}

// NewConfig 與 ParseConfig 共用同一份規則(rules.normalize),所以同一個壞值
// 在兩邊的判定必須一致 —— 只是處置不同(中止 vs 退回預設)。
func TestNewConfigSharesValidationWithParseConfig(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	if _, err := NewConfig(ConfigOverrides{BPPerRankGap: &zero}); !errors.Is(err, ErrConfigMalformed) {
		t.Fatalf("NewConfig err = %v,想要 ErrConfigMalformed", err)
	}
	cfg, err := ParseConfig([]byte(`{"bp_per_rank_gap":0}`))
	if !errors.Is(err, ErrConfigMalformed) {
		t.Fatalf("ParseConfig err = %v,想要 ErrConfigMalformed", err)
	}
	if cfg.BP.PerRankGap != bp.DefaultPerRankGap {
		t.Errorf("ParseConfig 退化後的 per_rank_gap = %d,want %d", cfg.BP.PerRankGap, bp.DefaultPerRankGap)
	}
}
