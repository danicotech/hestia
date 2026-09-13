package betting

import (
	"slices"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/rules"
)

// 預設參數下的一組手算對照(兩路,n=2)。每一列的「期望」都是用公式在紙上算出來的,
// 不是跑一次程式抄下來的 —— 對照表如果是抄的,它就只會確認 bug 沒變。
//
//	odds = (1 − 0.08) × (total + 2·5) / (votes + 5),夾在 [1.050, 12.000]
//
// 這張表在公式從 2S 改成 n·S 之後**一個數字都不能變**:兩路是 n=2 的特例。
func TestOddsMilli對照表(t *testing.T) {
	cfg := OddsConfig{} // 全部走預設

	cases := []struct {
		name      string
		side, tot int64
		wantMilli int64
		hand      string
	}{
		{"零票兩邊等價", 0, 0, 1840, "0.92 × 10 / 5 = 1.84"},
		{"一面倒的強方", 10, 10, 1226, "0.92 × 20 / 15 = 1.2266…"},
		{"一面倒的弱方", 0, 10, 3680, "0.92 × 20 / 5 = 3.68"},
		{"五五波", 10, 20, 1840, "0.92 × 30 / 15 = 1.84"},
		{"小幅領先", 6, 10, 1672, "0.92 × 20 / 11 = 1.6727…"},
		{"極度弱方觸及上限", 0, 100, 12000, "0.92 × 110 / 5 = 20.24 → 夾到 12"},
		{"極度強方觸及下限", 100, 100, 1050, "0.92 × 110 / 105 = 0.9638 → 夾到 1.05"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OddsMilli(c.side, c.tot, 2, cfg); got != c.wantMilli {
				t.Fatalf("OddsMilli(%d,%d,n=2) = %d,want %d(%s)", c.side, c.tot, got, c.wantMilli, c.hand)
			}
		})
	}
}

// 四路(比分盤,best_of=3)的手算對照。
//
//	odds = (1 − 0.08) × (total + 4·5) / (votes + 5)
func TestOddsMilli四路對照表(t *testing.T) {
	cfg := OddsConfig{}
	cases := []struct {
		name      string
		votes, n  int64
		tot       int64
		wantMilli int64
		hand      string
	}{
		{"零票四路均等", 0, 4, 0, 3680, "0.92 × 20 / 5 = 3.68(schemas/21:四路 3.68 倍)"},
		{"十票全在一格_那格", 10, 4, 10, 1840, "0.92 × 30 / 15 = 1.84"},
		{"十票全在一格_其他格", 0, 4, 10, 5520, "0.92 × 30 / 5 = 5.52"},
		{"二十票平均分四格", 5, 4, 20, 3680, "0.92 × 40 / 10 = 3.68"},
		{"熱門格觸及下限", 200, 4, 200, 1050, "0.92 × 220 / 205 = 0.987 → 夾到 1.05"},
		{"冷門格觸及上限", 0, 4, 200, 12000, "0.92 × 220 / 5 = 40.48 → 夾到 12"},
		{"六路零票", 0, 6, 0, 5520, "best_of=5 的比分盤:0.92 × 30 / 5 = 5.52"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OddsMilli(c.votes, c.tot, int(c.n), cfg); got != c.wantMilli {
				t.Fatalf("OddsMilli(%d,%d,n=%d) = %d,want %d(%s)", c.votes, c.tot, c.n, got, c.wantMilli, c.hand)
			}
		})
	}
}

// Σ(1/賠率_i) = 1/(1−VIG):這是「抽水 8.7%」的定義,n=2 與 n=4 都要成立。
//
// 以毫分算:Σ(1000000 / odds_milli) ≈ 10000 × 1000 / 9200 = 1086.9。
// 每一項的整數捨去最多少 1,所以容差是 n;夾擠會破壞歸一,測試只用不會夾到的分佈。
func TestOddsMilli抽水歸一(t *testing.T) {
	cfg := OddsConfig{}
	const wantMilli = 1086 // 1000 × 10000 / 9200 無條件捨去
	cases := []struct {
		name  string
		votes []int64
	}{
		{"兩路零票", []int64{0, 0}},
		{"兩路五五波", []int64{10, 10}},
		{"兩路小幅領先", []int64{6, 4}},
		{"四路零票", []int64{0, 0, 0, 0}},
		{"四路平均", []int64{5, 5, 5, 5}},
		{"四路偏斜", []int64{8, 4, 2, 1}},
		{"六路零票", []int64{0, 0, 0, 0, 0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var total int64
			for _, v := range c.votes {
				total += v
			}
			var sum int64
			for _, v := range c.votes {
				o := OddsMilli(v, total, len(c.votes), cfg)
				if o == DefaultMinOddsMilli || o == DefaultMaxOddsMilli {
					t.Fatalf("這個分佈碰到夾擠(%d),測不出歸一;換一組票數", o)
				}
				sum += 1_000_000 / o
			}
			n := int64(len(c.votes))
			if sum > wantMilli+1 || sum < wantMilli-n {
				t.Fatalf("Σ(1/odds) = %d 毫分,want %d(±%d)", sum, wantMilli, n)
			}
		})
	}
}

// MarketOddsOf 對每個結果各算一次,n 就是結果數 —— 比分盤四格零票都是 3.68。
func TestMarketOddsOf依結果數取n(t *testing.T) {
	r := Rules{Config: fullRulesConfig()}
	score := r.Outcomes(MarketScore)
	if len(score) != 4 {
		t.Fatalf("best_of=3 的比分盤該有 4 個結果,得到 %v", score)
	}
	odds := MarketOddsOf(nil, score, OddsConfig{})
	for _, o := range score {
		if odds[o] != 3680 {
			t.Fatalf("零票比分盤 %s = %d,want 3680", o, odds[o])
		}
	}
	two := MarketOddsOf(Tally{OutcomeP1: 3, OutcomeP2: 1}, r.Outcomes(MarketMatchWinner), OddsConfig{})
	// 與兩路對照表同一組數字:P1 三票 / 總四票 → 0.92 × 14 / 8 = 1.61;P2 → 0.92 × 14 / 6 = 2.146…
	if two[OutcomeP1] != 1610 || two[OutcomeP2] != 2146 {
		t.Fatalf("兩路 = %v,want p1=1610 p2=2146", two)
	}
}

func fullRulesConfig() rules.Config {
	cfg := rules.Default()
	cfg.Format.BestOf = 3
	cfg.Betting.Markets = []rules.MarketRule{
		{Kind: rules.MarketMatchWinner},
		{Kind: rules.MarketRoundWinner},
		{Kind: rules.MarketDuration, LineSeconds: 90},
		{Kind: rules.MarketScore},
	}
	return cfg
}

// 賠率永遠落在 [min,max] 之內,而且票數往哪邊跑都不會跑出界。
// 這是窮舉:0..60 票的所有分佈。
func TestOddsMilli窮舉夾擠(t *testing.T) {
	cfg := OddsConfig{}.Normalize()
	for total := int64(0); total <= 60; total++ {
		for side := int64(0); side <= total; side++ {
			o := OddsMilli(side, total, 2, cfg)
			if o < cfg.MinOddsMilli || o > cfg.MaxOddsMilli {
				t.Fatalf("side=%d total=%d → %d 超出 [%d,%d]", side, total, o, cfg.MinOddsMilli, cfg.MaxOddsMilli)
			}
			// odds_milli 必須 > 1000,否則寫進 bet_legs 會撞 CHECK。
			if o <= OddsScale {
				t.Fatalf("side=%d total=%d → %d 不符 bet_legs 的 CHECK(odds_milli > 1000)", side, total, o)
			}
		}
	}
}

// 票多的一方賠率必須比較低。這是賠率之所以是賠率的性質,不是實作細節。
func TestOddsMilli單調性(t *testing.T) {
	cfg := OddsConfig{}
	const total = 40
	prev := int64(1 << 62)
	for side := int64(0); side <= total; side++ {
		o := OddsMilli(side, total, 2, cfg)
		if o > prev {
			t.Fatalf("side=%d 的賠率 %d 高於 side=%d 的 %d,票愈多賠率該愈低", side, o, side-1, prev)
		}
		prev = o
	}
}

func TestNormalize把壞設定換成預設(t *testing.T) {
	got := OddsConfig{
		SmoothingVotes:     0,     // 會除以 0
		VigBps:             10000, // 抽水 100%,賠率會變 0
		MinOddsMilli:       1000,  // 等於 1.0,撞 bet_legs 的 CHECK
		MaxOddsMilli:       0,
		MaxParlayMilli:     -1,
		MaxStake:           0,
		PayoutToleranceBps: -5,
	}.Normalize()

	want := OddsConfig{
		SmoothingVotes:     DefaultSmoothingVotes,
		VigBps:             DefaultVigBps,
		MinOddsMilli:       DefaultMinOddsMilli,
		MaxOddsMilli:       DefaultMaxOddsMilli,
		MaxParlayMilli:     DefaultMaxParlayMilli,
		MaxStake:           DefaultMaxStake,
		PayoutToleranceBps: DefaultPayoutToleranceBps,
	}
	if got != want {
		t.Fatalf("Normalize() = %+v,want %+v", got, want)
	}
}

func TestNormalize上下限顛倒時以下限為準(t *testing.T) {
	got := OddsConfig{MinOddsMilli: 5000, MaxOddsMilli: 2000}.Normalize()
	if got.MaxOddsMilli != 5000 {
		t.Fatalf("MaxOddsMilli = %d,want 5000(顛倒時以下限為準)", got.MaxOddsMilli)
	}
	// 夾擠仍然成立:min == max 時所有輸入都得到同一個值。
	if o := OddsMilli(3, 10, 2, got); o != 5000 {
		t.Fatalf("夾擠後 = %d,want 5000", o)
	}
}

// 票數大到不真實時仍然不能溢位 —— 溢位的後果是憑空生出或蒸發代幣。
func TestOddsMilli極端票數不溢位(t *testing.T) {
	cfg := OddsConfig{}
	const huge = int64(1) << 62
	if o := OddsMilli(0, huge, 2, cfg); o != DefaultMaxOddsMilli {
		t.Fatalf("弱方 = %d,want %d", o, DefaultMaxOddsMilli)
	}
	if o := OddsMilli(huge, huge, 2, cfg); o != DefaultMinOddsMilli {
		t.Fatalf("強方 = %d,want %d", o, DefaultMinOddsMilli)
	}
	// 單邊票數大於總票數是資料異常,不該 panic 也不該算出界。
	if o := OddsMilli(100, 1, 2, cfg); o < DefaultMinOddsMilli || o > DefaultMaxOddsMilli {
		t.Fatalf("異常輸入算出 %d,超出夾擠範圍", o)
	}
	// 極端的平滑量 × 極端的結果數也不能溢位(n·S 那一項)。
	big := OddsConfig{SmoothingVotes: huge}
	for _, n := range []int{2, 4, 1 << 20} {
		if o := OddsMilli(0, 0, n, big); o < DefaultMinOddsMilli || o > DefaultMaxOddsMilli {
			t.Fatalf("n=%d S=huge 算出 %d,超出夾擠範圍(溢位?)", n, o)
		}
	}
	// n < 2 不是盤口,當兩路算,不讓分母縮到只剩平滑量。
	if OddsMilli(0, 0, 1, cfg) != OddsMilli(0, 0, 2, cfg) {
		t.Fatal("n=1 應與 n=2 同值")
	}
}

// ── 結果編碼與顯示名 ──────────────────────────────────────────────

func TestRulesOutcomes(t *testing.T) {
	r := Rules{Config: fullRulesConfig()}
	cases := []struct {
		kind MarketKind
		want []Outcome
	}{
		{MarketMatchWinner, []Outcome{"p1", "p2"}},
		{MarketRoundWinner, []Outcome{"p1", "p2"}},
		{MarketDuration, []Outcome{"over", "under"}},
		{MarketScore, []Outcome{"p1_2_0", "p1_2_1", "p2_2_1", "p2_2_0"}},
		{MarketKind("bogus"), nil},
	}
	for _, c := range cases {
		if got := r.Outcomes(c.kind); !slices.Equal(got, c.want) {
			t.Fatalf("Outcomes(%s) = %v,want %v", c.kind, got, c.want)
		}
	}
	if r.ValidOutcome(MarketScore, "p1_2_2") || r.ValidOutcome(MarketDuration, "p1") || r.ValidOutcome(MarketMatchWinner, "") {
		t.Fatal("不屬於該盤口的結果不該合法")
	}
	if !r.ValidOutcome(MarketScore, "p2_2_1") {
		t.Fatal("p2_2_1 是 best_of=3 的合法比分")
	}
}

// 開發庫那種 config(四種、best_of 3、line 90)要展開成 1 + 3 + 3 + 1 = 8 個盤口。
func TestRulesMarketsFor(t *testing.T) {
	r := Rules{Config: fullRulesConfig()}
	got := r.MarketsFor()
	want := []NewMarket{
		{Kind: MarketMatchWinner},
		{Kind: MarketRoundWinner, RoundNo: 1}, {Kind: MarketRoundWinner, RoundNo: 2}, {Kind: MarketRoundWinner, RoundNo: 3},
		{Kind: MarketDuration, RoundNo: 1, LineSeconds: 90}, {Kind: MarketDuration, RoundNo: 2, LineSeconds: 90}, {Kind: MarketDuration, RoundNo: 3, LineSeconds: 90},
		{Kind: MarketScore},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("MarketsFor = %+v,want %+v", got, want)
	}

	// 預設(v1 語意)只有勝負盤一個。
	if got := (Rules{Config: rules.Default()}).MarketsFor(); len(got) != 1 || got[0].Kind != MarketMatchWinner {
		t.Fatalf("預設規則 = %+v,want 只有 match_winner", got)
	}
	// 不開盤就不建。
	off := fullRulesConfig()
	off.Betting.Enabled = false
	if got := (Rules{Config: off}).MarketsFor(); got != nil {
		t.Fatalf("enabled=false 仍展開了 %+v", got)
	}
}

func TestScoreOutcome(t *testing.T) {
	cases := []struct {
		p1, p2 int
		want   Outcome
	}{
		{2, 0, "p1_2_0"}, {2, 1, "p1_2_1"}, {1, 2, "p2_2_1"}, {0, 2, "p2_2_0"},
		{3, 1, "p1_3_1"}, {0, 0, OutcomeNone}, {1, 1, OutcomeNone},
	}
	for _, c := range cases {
		if got := ScoreOutcome(c.p1, c.p2); got != c.want {
			t.Fatalf("ScoreOutcome(%d,%d) = %q,want %q", c.p1, c.p2, got, c.want)
		}
	}
}

func TestOutcomeLabel(t *testing.T) {
	cases := []struct {
		kind MarketKind
		o    Outcome
		want string
	}{
		{MarketMatchWinner, "p1", "李璃"},
		{MarketMatchWinner, "p2", "A冷"},
		{MarketRoundWinner, "p2", "A冷"},
		{MarketDuration, "over", "超過 90 秒"},
		{MarketDuration, "under", "90 秒內"},
		{MarketScore, "p1_2_0", "2:0"},
		{MarketScore, "p1_2_1", "2:1"},
		{MarketScore, "p2_2_1", "1:2"}, // 固定 p1:p2 順序
		{MarketScore, "p2_2_0", "0:2"},
		{MarketScore, "p1_3_2", "3:2"},
		{MarketScore, "garbage", "garbage"}, // 認不得就原樣,不要空字串
		{MarketMatchWinner, "over", "over"},
	}
	for _, c := range cases {
		if got := OutcomeLabel(c.kind, c.o, "李璃", "A冷", 90); got != c.want {
			t.Fatalf("OutcomeLabel(%s,%s) = %q,want %q", c.kind, c.o, got, c.want)
		}
	}
}

func TestParlayMultiplier連乘與封頂(t *testing.T) {
	cases := []struct {
		name string
		odds []int64
		cap  int64
		want int64
		hand string
	}{
		{"空清單就是一倍", nil, DefaultMaxParlayMilli, 1000, "沒有腿 → 賠付 = 本金"},
		{"單腿原值", []int64{1840}, DefaultMaxParlayMilli, 1840, "1.84"},
		{"兩腿相乘", []int64{1840, 1226}, DefaultMaxParlayMilli, 2255, "1.84 × 1.226 = 2.25584 → 逐步捨去 2255"},
		{"三腿相乘", []int64{2000, 2000, 2000}, DefaultMaxParlayMilli, 8000, "2 × 2 × 2 = 8"},
		{"封頂", []int64{12000, 12000, 12000}, DefaultMaxParlayMilli, DefaultMaxParlayMilli, "1728 倍 → 封 300 倍"},
		{"剛好在封頂線下", []int64{12000, 12000}, DefaultMaxParlayMilli, 144000, "144 倍 < 300"},
		{"壞掉的賠率當一倍略過", []int64{1000, 2000}, DefaultMaxParlayMilli, 2000, "≤1.0 的腿不該把賠付吃掉"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParlayMultiplierMilli(c.odds, c.cap); got != c.want {
				t.Fatalf("= %d,want %d(%s)", got, c.want, c.hand)
			}
		})
	}
}

// 十腿滿賠率是最容易溢位的輸入:12^10 ≈ 6.2×10^10 倍。
func TestParlayMultiplier十腿不溢位(t *testing.T) {
	odds := slices.Repeat([]int64{12000}, MaxLegsPerBet)
	if got := ParlayMultiplierMilli(odds, DefaultMaxParlayMilli); got != DefaultMaxParlayMilli {
		t.Fatalf("= %d,want %d", got, DefaultMaxParlayMilli)
	}
	// 連封頂值本身都極端時也不能溢位。
	if got := ParlayMultiplierMilli(odds, 1<<62); got <= 0 {
		t.Fatalf("= %d,溢位成非正數", got)
	}
}

func TestParlayPayout(t *testing.T) {
	cases := []struct {
		name  string
		stake int64
		odds  []int64
		want  int64
	}{
		{"單場", 100, []int64{1840}, 184},
		{"捨去到整數", 33, []int64{1226}, 40},                   // 33 × 1.226 = 40.458
		{"串關封頂", 100, []int64{12000, 12000, 12000}, 30000}, // 封 300 倍
		{"本金非正回 0", 0, []int64{1840}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParlayPayout(c.stake, c.odds, DefaultMaxParlayMilli); got != c.want {
				t.Fatalf("= %d,want %d", got, c.want)
			}
		})
	}
}

// 賠付必定大於本金:最低賠率 1.05 且 stake ≥ 1。
// 這同時保證 bets 的 CHECK (potential_payout > 0) 不會被我們自己踩爆。
func TestParlayPayout恆大於本金(t *testing.T) {
	for stake := int64(1); stake <= 500; stake++ {
		p := ParlayPayout(stake, []int64{DefaultMinOddsMilli}, DefaultMaxParlayMilli)
		if p <= 0 {
			t.Fatalf("stake=%d → payout=%d,違反 potential_payout > 0", stake, p)
		}
	}
}

func TestWithinTolerance(t *testing.T) {
	cases := []struct {
		name             string
		expected, actual int64
		want             bool
	}{
		{"沒填就不檢查", 0, 999999, true},
		{"完全一致", 1000, 1000, true},
		{"剛好在容差內", 1000, 1020, true}, // 2%
		{"超出容差", 1000, 1021, false},
		{"往有利方向也一樣擋", 1000, 979, false},
		{"看到 5 倍送達剩 2 倍", 5000, 2000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WithinTolerance(c.expected, c.actual, DefaultPayoutToleranceBps); got != c.want {
				t.Fatalf("= %v,want %v", got, c.want)
			}
		})
	}
}

func TestEvaluateBet(t *testing.T) {
	leg := func(r LegResult) Leg { return Leg{Result: r} }

	cases := []struct {
		name string
		legs []Leg
		want BetStatus
	}{
		{"單場待判定", []Leg{leg(LegPending)}, BetOpen},
		{"單場贏", []Leg{leg(LegWon)}, BetWon},
		{"單場輸", []Leg{leg(LegLost)}, BetLost},
		{"單場作廢", []Leg{leg(LegVoid)}, BetVoid},
		{"串關全贏", []Leg{leg(LegWon), leg(LegWon)}, BetWon},
		{"串關一腿輸就是輸", []Leg{leg(LegWon), leg(LegLost)}, BetLost},
		{"串關還有待判定", []Leg{leg(LegWon), leg(LegPending)}, BetOpen},
		{"輸先於待判定判定", []Leg{leg(LegLost), leg(LegPending)}, BetLost},
		{"作廢腿不參與判定_剩餘全贏", []Leg{leg(LegVoid), leg(LegWon)}, BetWon},
		{"作廢腿不參與判定_剩餘還有待判定", []Leg{leg(LegVoid), leg(LegPending)}, BetOpen},
		{"作廢腿不救已經輸掉的注單", []Leg{leg(LegVoid), leg(LegLost)}, BetLost},
		{"全部作廢", []Leg{leg(LegVoid), leg(LegVoid)}, BetVoid},
		{"沒有腿", nil, BetVoid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EvaluateBet(c.legs); got != c.want {
				t.Fatalf("= %s,want %s", got, c.want)
			}
		})
	}
}

func TestRemainingOdds移除作廢腿(t *testing.T) {
	legs := []Leg{
		{OddsMilli: 1500, Result: LegWon},
		{OddsMilli: 2000, Result: LegVoid},
		{OddsMilli: 3000, Result: LegPending},
	}
	got := RemainingOdds(legs)
	want := []int64{1500, 3000}
	if !slices.Equal(got, want) {
		t.Fatalf("= %v,want %v", got, want)
	}
}
