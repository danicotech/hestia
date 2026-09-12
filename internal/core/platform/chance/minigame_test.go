package chance_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/chance"
)

// 小遊戲的核心約束是**期望值 < 1**:它們是水槽不是水龍頭。
// 那個性質沒有測試就守不住 —— 賠率是一個數字,誰都能順手調高,
// 而調高之後不會有任何東西報錯,只會在幾個月後變成通膨。

// 樣本數。大數法則要收斂到 1% 以內需要這個量級;純 CPU,跑得很快。
const trials = 200_000

// evOf 實際跑 N 局,回傳實測期望值(總回收 / 總投注)。
func evOf(t *testing.T, game chance.Game, choice string) float64 {
	t.Helper()
	const stake = int64(100)
	var payout int64
	for i := 0; i < trials; i++ {
		p, err := chance.Resolve(game, choice, stake)
		if err != nil {
			t.Fatalf("%s: %v", game, err)
		}
		payout += p.Payout
	}
	return float64(payout) / float64(trials*int(stake))
}

func TestExpectedValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		game   chance.Game
		choice string
		want   float64
	}{
		{chance.GameRPS, "rock", 0.95},
		{chance.GameDice, "big", 0.975},
		{chance.GameGuess, "42", 0.5},
		{chance.GameRoulette, "red", 0.95},
		{chance.GameRoulette, "7", 0.95}, // 35/37 —— 與押顏色相等是刻意的設計
	}
	for _, c := range cases {
		got := evOf(t, c.game, c.choice)
		if got > 1.0 {
			t.Errorf("%s(%s) 期望值 %.4f > 1 —— 這款會變成水龍頭", c.game, c.choice, got)
		}
		if diff := got - c.want; diff > 0.02 || diff < -0.02 {
			t.Errorf("%s(%s) 期望值 %.4f,預期約 %.3f", c.game, c.choice, got, c.want)
		}
	}
}

// 每一款都必須有玩家操作。純隨機的話玩家會覺得被機器擺布,
// 而同樣的期望值下,有操作的體感完全不同(schemas/25)。
func TestEveryGameTakesAChoice(t *testing.T) {
	t.Parallel()
	for _, g := range chance.Games() {
		if _, err := chance.Resolve(g, "", 100); err == nil {
			t.Errorf("%s 沒有選擇也能玩,那就不是「機率加上自己決定的操作」", g)
		}
	}
}

func TestRPS_TieRefundsStake(t *testing.T) {
	t.Parallel()
	// 平手退本金不是慷慨:三種結果各 1/3,平手也算輸的話期望值掉到 0.63,
	// 那不是遊戲是懲罰。跑到看見平手為止。
	for i := 0; i < 200; i++ {
		p, err := chance.Resolve(chance.GameRPS, "rock", 100)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if p.Choice == p.Result {
			if p.Payout != 100 {
				t.Fatalf("平手應退回本金 100,得到 %d", p.Payout)
			}
			return
		}
	}
	t.Skip("200 局沒抽到平手(機率約 10^-35),不判定失敗")
}

func TestDice_OnlyBigOrSmall(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"medium", "7", "", "BIGGER"} {
		if _, err := chance.Resolve(chance.GameDice, bad, 100); err == nil {
			t.Errorf("%q 應該被拒絕", bad)
		}
	}
	for _, ok := range []string{"big", "BIG", " small "} {
		if _, err := chance.Resolve(chance.GameDice, ok, 100); err != nil {
			t.Errorf("%q 應該被接受: %v", ok, err)
		}
	}
}

func TestGuess_RejectsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"0", "101", "-3", "abc", ""} {
		if _, err := chance.Resolve(chance.GameGuess, bad, 100); err == nil {
			t.Errorf("%q 應該被拒絕", bad)
		}
	}
}

func TestRoulette_GreenZeroTakesAll(t *testing.T) {
	t.Parallel()
	// 0 是綠色,押紅黑都不中 —— 那正是輪盤期望值低於 1 的來源。
	// 沒有它的話押顏色會變成公平賭局(期望值剛好 1),整款就不再是水槽。
	sawZero := false
	for i := 0; i < 5000; i++ {
		p, err := chance.Resolve(chance.GameRoulette, "red", 100)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if strings.HasPrefix(p.Result, "0(") {
			sawZero = true
			if p.Payout != 0 {
				t.Fatalf("開出 0 時押顏色應全輸,得到 %d", p.Payout)
			}
		}
	}
	if !sawZero {
		t.Skip("5000 局沒開出 0,不判定失敗")
	}
}

func TestResolve_RejectsNonPositiveStake(t *testing.T) {
	t.Parallel()
	for _, stake := range []int64{0, -1, -100} {
		_, err := chance.Resolve(chance.GameRPS, "rock", stake)
		if !errors.Is(err, chance.ErrInvalidStake) {
			t.Errorf("stake=%d 應回 ErrInvalidStake,得到 %v", stake, err)
		}
	}
}

// 每一局都要留下種子與賠率快照,否則 chance_draws 就證明不了任何事。
func TestPlay_CarriesSeedAndOdds(t *testing.T) {
	t.Parallel()
	for _, g := range chance.Games() {
		choice := map[chance.Game]string{
			chance.GameRPS: "rock", chance.GameDice: "big",
			chance.GameGuess: "7", chance.GameRoulette: "black",
		}[g]
		p, err := chance.Resolve(g, choice, 100)
		if err != nil {
			t.Fatalf("%s: %v", g, err)
		}
		if len(p.Seed) != 32 {
			t.Errorf("%s 的種子長度 = %d,應為 32(16 bytes hex)", g, len(p.Seed))
		}
		if p.Odds["expected_value"] == nil {
			t.Errorf("%s 沒有帶賠率快照", g)
		}
	}
}

// ── 獎池抽籤 ────────────────────────────────────────────────────

func TestPick_RespectsWeights(t *testing.T) {
	t.Parallel()
	outcomes := []chance.Outcome{
		{Ref: "common", Weight: 90, Remaining: -1},
		{Ref: "rare", Weight: 10, Remaining: -1},
	}
	counts := map[string]int{}
	for i := 0; i < 50_000; i++ {
		idx, _, err := chance.Pick(outcomes)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[outcomes[idx].Ref]++
	}
	ratio := float64(counts["rare"]) / 50_000
	if ratio < 0.08 || ratio > 0.12 {
		t.Fatalf("權重 10/100 的項目實測比例 %.4f,偏離 0.10 太多", ratio)
	}
}

// 有限獎池是 max_supply 在抽獎場景的實現:抽完就不該再出現。
// 不擋的話「全世界只有 1 個」這個設計形同虛設。
func TestPick_SkipsExhausted(t *testing.T) {
	t.Parallel()
	outcomes := []chance.Outcome{
		{Ref: "sold-out", Weight: 1000, Remaining: 0},
		{Ref: "available", Weight: 1, Remaining: 5},
	}
	for i := 0; i < 100; i++ {
		idx, _, err := chance.Pick(outcomes)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if outcomes[idx].Ref != "available" {
			t.Fatalf("抽到已經賣完的項目")
		}
	}
}

func TestPick_EmptyPool(t *testing.T) {
	t.Parallel()
	_, _, err := chance.Pick([]chance.Outcome{{Ref: "x", Weight: 10, Remaining: 0}})
	if !errors.Is(err, chance.ErrNoOutcomes) {
		t.Fatalf("全部抽完應回 ErrNoOutcomes,得到 %v", err)
	}
}

// 種子不可重複:重複代表亂數源有問題,而那會讓結果可預測。
func TestSeedsAreUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		_, seed, err := chance.RollBelow(100)
		if err != nil {
			t.Fatalf("RollBelow: %v", err)
		}
		if seen[seed] {
			t.Fatalf("第 %d 次出現重複種子 %s", i, seed)
		}
		seen[seed] = true
	}
}

func TestRollBelow_StaysInRange(t *testing.T) {
	t.Parallel()
	for i := 0; i < 10_000; i++ {
		v, _, err := chance.RollBelow(6)
		if err != nil {
			t.Fatalf("RollBelow: %v", err)
		}
		if v < 0 || v > 5 {
			t.Fatalf("RollBelow(6) 回了 %d", v)
		}
	}
	if _, _, err := chance.RollBelow(0); err == nil {
		t.Fatal("範圍 0 應該回錯")
	}
}

// 猜數字的 50 倍賠率:中一次要能真的拿到 50 倍,不是 50 倍減本金。
func TestGuess_PayoutIsFiftyTimes(t *testing.T) {
	t.Parallel()
	for i := 0; i < 20_000; i++ {
		p, err := chance.Resolve(chance.GameGuess, "42", 100)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if p.Result == "42" {
			if p.Payout != 5000 {
				t.Fatalf("猜中應回 5000(含本金),得到 %d", p.Payout)
			}
			return
		}
	}
	t.Skip("20000 局沒猜中(機率約 10^-88),不判定失敗")
}

// 賠率表上的數字要跟實際發出去的一致 —— 寫一套、算另一套是最糟的情況,
// 因為公開的說明會變成謊言。
func TestOddsSnapshotMatchesPayout(t *testing.T) {
	t.Parallel()
	p, err := chance.Resolve(chance.GameDice, "big", 1000)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	mult, ok := p.Odds["win_multiplier"].(float64)
	if !ok {
		t.Fatalf("賠率快照裡沒有 win_multiplier:%v", p.Odds)
	}
	if p.Payout != 0 && p.Payout != int64(1000*mult) {
		t.Fatalf("回收 %d 與賠率 %.2f 不符", p.Payout, mult)
	}
	// 順帶確認結果是 1-6 的點數
	pip, err := strconv.Atoi(p.Result)
	if err != nil || pip < 1 || pip > 6 {
		t.Fatalf("骰子結果 %q 不合法", p.Result)
	}
}
