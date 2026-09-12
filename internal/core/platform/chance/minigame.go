package chance

import (
	"fmt"
	"strings"
)

// 小遊戲(schemas/25)。四款的期望值全部 < 1 —— 它們是**水槽不是水龍頭**,
// 玩越多流通量越少。這是舊 grill 就定下的約束,不要為了「讓玩家開心」調高。
//
// 每一款都有玩家操作,不是純隨機。使用者的原則:「以機率和可控性為準,
// 也希望有像猜拳這種機率加上自己決定的操作」。同樣期望值下,有操作的體感
// 完全不同 —— 玩家會覺得結果跟自己有關,而不是被機器擺布。
//
// 刻意**不用變動比率獎勵**(不知道何時會中的那種)。研究顯示它留存最強,
// 但所有來源都同時警告它「機制與賭博相同」,而這個平台已經有真實點數與
// 賽事下注,不該再疊一層。

// Game 是小遊戲代號。
type Game string

const (
	GameRPS      Game = "rps"      // 猜拳
	GameDice     Game = "dice"     // 擲骰比大小
	GameGuess    Game = "guess"    // 猜數字
	GameRoulette Game = "roulette" // 輪盤
)

// Games 是全部可玩的遊戲(排序固定,供指令選單與測試斷言)。
func Games() []Game { return []Game{GameRPS, GameDice, GameGuess, GameRoulette} }

// Play 是一次遊戲的結果。
type Play struct {
	Game Game
	// Choice 是玩家的選擇(原樣保留,寫進 chance_draws.outcome 供對帳)。
	Choice string
	// Result 是這局開出來的東西(對手出什麼、骰出幾點、輪盤停在哪)。
	Result string
	// Payout 是回給玩家的總額,**含本金**。0 = 全輸。
	//
	// 含本金是刻意的:分開記「退本金」與「贏獎金」會讓帳本出現兩筆
	// 語意重疊的分錄,而對帳時沒有人分得出哪一筆是哪一筆。
	Payout int64
	Seed   string
	// Odds 是這局用的賠率快照,寫進 chance_draws.odds_snapshot。
	Odds map[string]any
}

// Resolve 跑一局遊戲。stake 必須為正。
//
// 回傳的 Payout 是「要付給玩家多少」,扣款由呼叫端在同一個 transaction 做:
// 扣 stake、加 Payout。淨變動 = Payout - stake。
func Resolve(game Game, choice string, stake int64) (*Play, error) {
	if stake <= 0 {
		return nil, fmt.Errorf("投注額必須為正: %w", ErrInvalidStake)
	}
	switch game {
	case GameRPS:
		return resolveRPS(choice, stake)
	case GameDice:
		return resolveDice(choice, stake)
	case GameGuess:
		return resolveGuess(choice, stake)
	case GameRoulette:
		return resolveRoulette(choice, stake)
	default:
		return nil, fmt.Errorf("不認得的遊戲 %q", game)
	}
}

// ── 猜拳:押 100 贏拿 190,平手退回本金。期望值 0.95 ──────────────
//
// 平手退本金不是慷慨,是規則本身:三種結果各 1/3,若平手也算輸,
// 期望值會掉到 0.63,那不是遊戲是懲罰。

var rpsChoices = []string{"rock", "paper", "scissors"}

func resolveRPS(choice string, stake int64) (*Play, error) {
	mine, err := indexOf(rpsChoices, choice)
	if err != nil {
		return nil, err
	}
	roll, seed, err := RollBelow(3)
	if err != nil {
		return nil, err
	}
	theirs := int(roll)

	var payout int64
	switch {
	case mine == theirs:
		payout = stake // 平手退回
	case (mine+1)%3 == theirs:
		payout = 0 // 輸
	default:
		payout = stake * 19 / 10 // 贏:1.9 倍
	}
	return &Play{
		Game: GameRPS, Choice: rpsChoices[mine], Result: rpsChoices[theirs],
		Payout: payout, Seed: seed,
		Odds: map[string]any{"win_multiplier": 1.9, "tie": "refund", "expected_value": 0.95},
	}, nil
}

// ── 擲骰比大小:押 100 贏拿 195。期望值 0.975 ─────────────────────
//
// 1–6 各半,沒有和局(3 以下小、4 以上大),所以贏面剛好 1/2。

func resolveDice(choice string, stake int64) (*Play, error) {
	c := strings.ToLower(strings.TrimSpace(choice))
	if c != "big" && c != "small" {
		return nil, fmt.Errorf("只能押 big 或 small,拿到 %q", choice)
	}
	roll, seed, err := RollBelow(6)
	if err != nil {
		return nil, err
	}
	pip := roll + 1
	isBig := pip >= 4

	var payout int64
	if (isBig && c == "big") || (!isBig && c == "small") {
		payout = stake * 195 / 100
	}
	return &Play{
		Game: GameDice, Choice: c, Result: fmt.Sprintf("%d", pip),
		Payout: payout, Seed: seed,
		Odds: map[string]any{"win_multiplier": 1.95, "expected_value": 0.975},
	}, nil
}

// ── 猜數字:1–100 猜一個,中了 50 倍。期望值 0.5 ──────────────────
//
// 期望值最低的一款,但它的吸引力在於單次回報最高 —— 這是刻意的配置:
// 想要大回報就得接受差的期望值,而那個取捨對玩家是**看得見**的
// (賠率寫在指令說明裡),不是藏起來的。

const guessRange = 100

func resolveGuess(choice string, stake int64) (*Play, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(choice), "%d", &n); err != nil {
		return nil, fmt.Errorf("請給 1 到 %d 的數字,拿到 %q", guessRange, choice)
	}
	if n < 1 || n > guessRange {
		return nil, fmt.Errorf("數字要在 1 到 %d 之間,拿到 %d", guessRange, n)
	}
	roll, seed, err := RollBelow(guessRange)
	if err != nil {
		return nil, err
	}
	answer := int(roll) + 1

	var payout int64
	if answer == n {
		payout = stake * 50
	}
	return &Play{
		Game: GameGuess, Choice: fmt.Sprintf("%d", n), Result: fmt.Sprintf("%d", answer),
		Payout: payout, Seed: seed,
		Odds: map[string]any{"range": guessRange, "win_multiplier": 50, "expected_value": 0.5},
	}, nil
}

// ── 輪盤:押顏色 1.95 倍、押數字 35 倍。兩種都是期望值 0.95 ───────
//
// 0–36 共 37 格,0 是綠色(莊家通吃)。勝率:顏色 18/37、數字 1/37。
//
// **賠率刻意不是真實輪盤的 2 倍 / 36 倍。** 那組數字給的是 36/37 ≈ 0.973,
// 對一個要當水槽的玩法來說太薄。這裡調成 1.95 / 35,讓兩種下注都是 0.95 ——
// 與其他三款一致,也是使用者定案的數字。
//
// 兩種下注的期望值**必須相等**,這點沿用真實輪盤的設計原則:
// 不相等的話,選高風險玩法的人會被額外懲罰,而那個懲罰是隱形的。
// (最初寫成 2 倍 / 30 倍時,兩者是 0.973 與 0.811 —— 差了 16%,
// 而公開說明只寫一個 0.95,等於對其中一種下注說了謊。測試抓到的。)

const rouletteSlots = 37

func resolveRoulette(choice string, stake int64) (*Play, error) {
	c := strings.ToLower(strings.TrimSpace(choice))
	roll, seed, err := RollBelow(rouletteSlots)
	if err != nil {
		return nil, err
	}
	slot := int(roll)
	color := rouletteColor(slot)

	var payout int64
	switch c {
	case "red", "black":
		if color == c {
			payout = stake * 195 / 100
		}
	default:
		var n int
		if _, err := fmt.Sscanf(c, "%d", &n); err != nil {
			return nil, fmt.Errorf("押 red / black 或 0-%d 的數字,拿到 %q", rouletteSlots-1, choice)
		}
		if n < 0 || n >= rouletteSlots {
			return nil, fmt.Errorf("數字要在 0 到 %d 之間,拿到 %d", rouletteSlots-1, n)
		}
		if n == slot {
			payout = stake * 35
		}
	}
	return &Play{
		Game: GameRoulette, Choice: c, Result: fmt.Sprintf("%d(%s)", slot, color),
		Payout: payout, Seed: seed,
		Odds: map[string]any{
			"slots": rouletteSlots, "color_multiplier": 1.95, "number_multiplier": 35,
			"expected_value": 0.95,
		},
	}, nil
}

// rouletteColor 用標準輪盤的紅黑配置。0 是綠色,莊家通吃。
func rouletteColor(slot int) string {
	if slot == 0 {
		return "green"
	}
	reds := map[int]bool{
		1: true, 3: true, 5: true, 7: true, 9: true, 12: true, 14: true, 16: true, 18: true,
		19: true, 21: true, 23: true, 25: true, 27: true, 30: true, 32: true, 34: true, 36: true,
	}
	if reds[slot] {
		return "red"
	}
	return "black"
}

func indexOf(options []string, choice string) (int, error) {
	c := strings.ToLower(strings.TrimSpace(choice))
	for i, o := range options {
		if o == c {
			return i, nil
		}
	}
	return 0, fmt.Errorf("只能選 %s,拿到 %q", strings.Join(options, " / "), choice)
}
