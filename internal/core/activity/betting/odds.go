package betting

// 賠率與賠付的計算。整個檔案沒有一個浮點數,也沒有任何 I/O ——
// 這是刻意的:賠率會參與串關連乘,誤差會直接乘進派彩金額,而派彩是真錢。
// 純函數也才有辦法窮舉測邊界(零票、一面倒、夾擠點、封頂點)。

// 刻度。賠率存毫分、抽水存基點,兩者都是「金額一律 BIGINT」的直接延伸。
const (
	// OddsScale 是賠率的放大倍率:1.43 → 1430。
	OddsScale int64 = 1000
	// bpsScale 是基點的分母:8% → 800 / 10000。
	bpsScale int64 = 10000
)

// 預設參數(schemas/21)。逐屆可在 tournaments.config 覆寫,這裡只是預設值。
const (
	DefaultSmoothingVotes int64 = 5
	DefaultVigBps         int64 = 800
	DefaultMinOddsMilli   int64 = 1050
	DefaultMaxOddsMilli   int64 = 12000
	DefaultMaxParlayMilli int64 = 300_000
	DefaultMaxStake       int64 = 500
	// DefaultPayoutToleranceBps 是 expected_payout 比對的容差(2%)。
	// schema 沒有訂這個數字 —— 容差太緊會讓正常的票數波動一直退單,
	// 太鬆等於沒擋。2% 約等於「一場十幾票的比賽多一票」的量級。
	DefaultPayoutToleranceBps int64 = 200
)

// 防溢位的硬上限。這些數字都遠超任何真實情境,存在只是為了讓整數乘法在
// **任何**輸入下都不可能溢位 —— 金額計算溢位的後果是憑空生出或蒸發代幣。
const (
	maxSafeVotes       int64 = 1 << 40
	maxSafeOddsMilli   int64 = 1_000_000_000
	maxSafeParlayMilli int64 = 1_000_000_000
	maxSafeStake       int64 = 1_000_000_000
)

// OddsConfig 是賠率與下注上限參數。
//
// 前五項來自 tournaments.config(逐屆可調);MaxStake 來自 platform 的
// economy_configs.max_stake —— 單筆上限是平台級風險控制,不該由活動自己調。
// 零值一律由 Normalize 補成預設值:設定檔壞掉時寧可用預設值,
// 也不要讓賠率變成 0 或除以 0。
type OddsConfig struct {
	// SmoothingVotes 是拉普拉斯平滑的票數。它決定「零票時賠率是多少」,
	// 也決定前幾票能把賠率推多遠 —— 沒有它,第一票就會讓另一邊變成無限賠率。
	SmoothingVotes int64
	// VigBps 是抽水(莊家優勢),萬分之一。8% → 800。
	VigBps int64
	// MinOddsMilli / MaxOddsMilli 夾住賠率。下限存在的理由是 bet_legs 有
	// CHECK (odds_milli > 1000):賠率 ≤ 1.0 代表「贏了還倒賠」,那不是賠率。
	// 上限存在的理由是一面倒的票數會讓弱方賠率飆到天上,而票是免費的。
	MinOddsMilli int64
	MaxOddsMilli int64
	// MaxParlayMilli 是串關總倍數的封頂 ×1000(300 倍 → 300000)。
	MaxParlayMilli int64
	// MaxStake 是單筆下注上限。
	MaxStake int64
	// PayoutToleranceBps 是 expected_payout 比對容差,萬分之一。
	PayoutToleranceBps int64
}

// Normalize 把不合法或未設定的參數換成預設值,並修正彼此矛盾的組合。
//
// 不回錯誤是刻意的:設定檔是裁判在後台填的,一個打錯的數字不該讓整屆賽事
// 完全不能下注。但每一條修正都必須是「保守的方向」—— 不會因為設定壞掉
// 而讓平台多付錢,也不會寫出資料庫 CHECK 擋不住的值。
func (c OddsConfig) Normalize() OddsConfig {
	if c.SmoothingVotes <= 0 {
		// 0 平滑會讓零票時分母為 0。這不是「沒設定」,是會爆炸。
		c.SmoothingVotes = DefaultSmoothingVotes
	}
	if c.SmoothingVotes > maxSafeVotes {
		c.SmoothingVotes = maxSafeVotes
	}
	if c.VigBps <= 0 || c.VigBps >= bpsScale {
		// 0 一律視為「沒設定」而不是「零抽水」:tournaments.config 少填一個鍵
		// 就等於平台白送 8% 出去,這個方向的錯誤太貴。真的要零抽水就填 1
		// (0.01%,比賠率的最小刻度還小,實務上就是 0)。
		// 上界是因為抽水 ≥ 100% 會讓賠率變成 0 或負數。
		c.VigBps = DefaultVigBps
	}
	if c.MinOddsMilli <= OddsScale {
		c.MinOddsMilli = DefaultMinOddsMilli
	}
	if c.MinOddsMilli > maxSafeOddsMilli {
		c.MinOddsMilli = maxSafeOddsMilli
	}
	if c.MaxOddsMilli <= OddsScale {
		c.MaxOddsMilli = DefaultMaxOddsMilli
	}
	if c.MaxOddsMilli > maxSafeOddsMilli {
		c.MaxOddsMilli = maxSafeOddsMilli
	}
	if c.MaxOddsMilli < c.MinOddsMilli {
		// 上下限顛倒時以下限為準:賠率至少要是個合法賠率。
		c.MaxOddsMilli = c.MinOddsMilli
	}
	if c.MaxParlayMilli <= OddsScale {
		c.MaxParlayMilli = DefaultMaxParlayMilli
	}
	if c.MaxParlayMilli > maxSafeParlayMilli {
		c.MaxParlayMilli = maxSafeParlayMilli
	}
	if c.MaxStake <= 0 {
		c.MaxStake = DefaultMaxStake
	}
	if c.MaxStake > maxSafeStake {
		c.MaxStake = maxSafeStake
	}
	if c.PayoutToleranceBps < 0 {
		c.PayoutToleranceBps = DefaultPayoutToleranceBps
	}
	if c.PayoutToleranceBps > bpsScale {
		c.PayoutToleranceBps = bpsScale
	}
	return c
}

// OddsMilli 算出某一方的賠率 ×1000。
//
//	隱含機率 = (該方票數 + S) / (總票數 + 2S)
//	賠率     = (1 − VIG) / 隱含機率,夾在 [MIN_ODDS, MAX_ODDS]
//
// 代入後展開成一次整數除法:
//
//	odds_milli = (10000 − vig_bps) × (total + 2S) × 1000 / (10000 × (side + S))
//
// 1000/10000 先約掉成 1/10,分子小一個數量級,溢位餘裕就多一個數量級。
// 除法無條件捨去:方向固定且永遠對平台有利(每筆最多少 0.001 倍),
// 重點不是誰佔便宜,而是**同一組輸入永遠得到同一個答案** ——
// 下注當下算的乘積,和棄賽重算時用的乘積,必須出自同一條規則。
func OddsMilli(sideVotes, totalVotes int64, c OddsConfig) int64 {
	c = c.Normalize()

	if sideVotes < 0 {
		sideVotes = 0
	}
	if totalVotes < sideVotes {
		// 總票數小於單邊票數是資料異常。就低不就高:寧可算出偏保守的賠率。
		totalVotes = sideVotes
	}
	if totalVotes > maxSafeVotes {
		totalVotes = maxSafeVotes
		if sideVotes > totalVotes {
			sideVotes = totalVotes
		}
	}

	s := c.SmoothingVotes
	num := (bpsScale - c.VigBps) * (totalVotes + 2*s)
	den := (bpsScale / OddsScale) * (sideVotes + s)
	odds := num / den

	if odds < c.MinOddsMilli {
		return c.MinOddsMilli
	}
	if odds > c.MaxOddsMilli {
		return c.MaxOddsMilli
	}
	return odds
}

// MatchOddsOf 算出一場比賽兩邊的賠率。
func MatchOddsOf(t Tally, c OddsConfig) (p1, p2 int64) {
	total := t.Total()
	return OddsMilli(t.P1, total, c), OddsMilli(t.P2, total, c)
}

// ParlayMultiplierMilli 是各腿賠率的連乘 ×1000,受 MAX_PARLAY 封頂。
//
// 每一步都先封頂再相乘,而不是乘完最後才封頂 —— 因為每腿最高 12 倍,
// 十腿就是 6×10^10 倍,乘完才封頂會在中途溢位。先封頂不改變結果:
// 每腿賠率都 > 1.0,已經到頂的乘積再乘只會更大,最後一樣是頂。
//
// 每一步的捨去會讓總倍數略低於理論值(同方向、可重現),這是整數運算的
// 必然代價;相對地,任何浮點方案都無法保證兩次計算得到同一個數字。
func ParlayMultiplierMilli(oddsMilli []int64, maxParlayMilli int64) int64 {
	if maxParlayMilli <= OddsScale {
		maxParlayMilli = DefaultMaxParlayMilli
	}
	if maxParlayMilli > maxSafeParlayMilli {
		maxParlayMilli = maxSafeParlayMilli
	}

	m := OddsScale
	for _, o := range oddsMilli {
		if o <= OddsScale {
			// 資料庫 CHECK 擋著,理論上到不了。真的發生時當作 1.0 倍略過,
			// 而不是讓一個壞掉的賠率把整張注單的賠付吃掉。
			continue
		}
		if o > maxSafeOddsMilli {
			o = maxSafeOddsMilli
		}
		if m > maxParlayMilli {
			m = maxParlayMilli
		}
		m = m * o / OddsScale
	}
	if m > maxParlayMilli {
		m = maxParlayMilli
	}
	if m < OddsScale {
		// 空清單:沒有任何一腿,倍數就是 1.0(賠付 = 本金)。
		m = OddsScale
	}
	return m
}

// ParlayPayout 是下注當下算定的賠付(含本金)= stake × ∏(各腿賠率),受封頂。
//
// 這個數字寫死在 bets.potential_payout,結算時直接發,不重算 ——
// 使用者看到的「可能贏得 X」必須等於實際入帳,這是信任問題不是技術問題。
func ParlayPayout(stake int64, oddsMilli []int64, maxParlayMilli int64) int64 {
	if stake <= 0 {
		return 0
	}
	if stake > maxSafeStake {
		stake = maxSafeStake
	}
	return stake * ParlayMultiplierMilli(oddsMilli, maxParlayMilli) / OddsScale
}

// WithinTolerance 回報伺服器算出的賠付與客戶端預期是否夠接近。
//
// expected = 0 代表客戶端沒填,不檢查(proto 明文允許)。
// 比較用交叉相乘,不做除法 —— 除法會在小額時把容差整個捨去掉。
func WithinTolerance(expected, actual, toleranceBps int64) bool {
	if expected <= 0 {
		return true
	}
	if toleranceBps < 0 {
		toleranceBps = DefaultPayoutToleranceBps
	}
	diff := actual - expected
	if diff < 0 {
		diff = -diff
	}
	// diff/expected ≤ tol/10000 ⇔ diff×10000 ≤ expected×tol
	return diff*bpsScale <= expected*toleranceBps
}

// EvaluateBet 由各腿的結果推出注單該是什麼狀態。
//
// void 的腿**不參與判定**,這是棄賽退款路徑的核心:那一腿從來沒有成立過,
// 所以既不算贏也不算輸,直接從乘積與判定中移除。
//
//	沒有任何非 void 的腿 → void(全額退)
//	有任一腿 lost        → lost(棄賽不救已經輸掉的注單)
//	還有腿 pending       → open(等其他場次)
//	其餘(全 won)        → won
//
// 順序很重要:lost 必須排在 pending 之前判 —— 一張已經輸掉的串關不必等
// 剩下的場次打完,那只是讓使用者多空等幾天。
func EvaluateBet(legs []Leg) BetStatus {
	remaining := 0
	pending := false
	for _, l := range legs {
		if l.Result == LegVoid {
			continue
		}
		if l.Result == LegLost {
			return BetLost
		}
		if l.Result == LegPending {
			pending = true
		}
		remaining++
	}
	switch {
	case remaining == 0:
		return BetVoid
	case pending:
		return BetOpen
	default:
		return BetWon
	}
}

// RemainingOdds 取出非 void 腿的賠率,順序照給定的順序。
// 棄賽重算 potential_payout 時用它:剩餘每一腿仍用自己下注當下鎖定的
// odds_milli,沒有任何一腿的賠率被改動。
func RemainingOdds(legs []Leg) []int64 {
	out := make([]int64, 0, len(legs))
	for _, l := range legs {
		if l.Result == LegVoid {
			continue
		}
		out = append(out, l.OddsMilli)
	}
	return out
}
