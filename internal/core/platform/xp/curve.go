package xp

import (
	"fmt"
	"math"
)

// 等級曲線(schemas/24)。**使用者等級與寵物等級共用這一份** ——
// 兩條曲線會各自漂移,而「升到 10 級要多少」這個問題只該有一個答案。
//
// 等級是 XP 的純函數,不存欄位:衍生資料必須可重建,而存了就會有不一致的一天。

// 預設曲線。community 沒有指派 ruleset(M1 常態)時用它。
//
// exponent 1.5 是業界常見範圍(1.5–2.5)裡最平緩的一端,對社群而言那正是要的:
// 這裡的等級不解鎖任何功能,只是「我在這裡待多久」的顯示,不需要陡曲線控節奏。
// 調陡會讓輕度使用者看一眼就放棄,而社群最需要留下的恰恰是那些人。
const (
	DefaultCurveBase     = 100.0
	DefaultCurveExponent = 1.5
)

// Curve 是 xp_rulesets.config.curve 的 Go 形狀。
//
// 兩種模式:
//   - formula:累積 XP = round(base × (n-1)^exponent)
//   - table:  手調每級門檻,thresholds[i] = 升到第 i+1 級的累積 XP
//
// **沒有 max_level。** 等級無上限是刻意的:重度使用者三個月就摸到 100 級,
// 之後沒有東西可追;重量感應該由里程碑獎勵提供,不是由把曲線調陡提供。
type Curve struct {
	Type       string  `json:"type"`
	Base       float64 `json:"base"`
	Exponent   float64 `json:"exponent"`
	Thresholds []int64 `json:"thresholds"`
}

// Progress 是「目前等級與距離下一級的進度」。
//
// 給 UI 用:只給等級的話畫不出進度條,而每個呼叫端各自算一次就會有
// N 份「本級起點是多少」的實作,其中一定有一份算錯。
type Progress struct {
	Level int32
	// XPIntoLevel 是本級已累積多少(從本級起點算起)。
	XPIntoLevel int64
	// XPForLevel 是本級總共需要多少。下一級門檻 − 本級門檻。
	XPForLevel int64
	// NextLevelAt 是升到下一級所需的**累積** XP。
	NextLevelAt int64
}

// curveOf 取出設定裡的曲線,缺省補上預設值。
func (c Config) curveOf() Curve {
	cv := c.Curve
	if cv.Type == "" {
		cv.Type = "formula"
	}
	if cv.Base <= 0 {
		cv.Base = DefaultCurveBase
	}
	if cv.Exponent <= 0 {
		cv.Exponent = DefaultCurveExponent
	}
	return cv
}

// ThresholdFor 回傳「升到第 level 級」所需的累積 XP。level <= 1 一律是 0。
//
// 公式是 (n-1) 而不是 n:等級從 1 起算,而剛註冊的人是 1 級 0 XP。
// 用 n 的話 1 級要 100 XP,新人會顯示成 0 級 —— 那是 schemas/01 增補 A
// 原本的寫法,2026-09-11 修正。
func ThresholdFor(cv Curve, level int32) int64 {
	if level <= 1 {
		return 0
	}
	if cv.Type == "table" {
		i := int(level) - 2 // thresholds[0] = 升到 2 級
		if i < 0 || i >= len(cv.Thresholds) {
			// 表格用完了:沿用最後一個門檻,不要回 0(那會讓高等級的人
			// 突然變成「已達成」而無限升級)。
			if len(cv.Thresholds) == 0 {
				return 0
			}
			return cv.Thresholds[len(cv.Thresholds)-1]
		}
		return cv.Thresholds[i]
	}
	return int64(math.Round(cv.Base * math.Pow(float64(level-1), cv.Exponent)))
}

// LevelFor 由累積 XP 反推等級。負數 XP 視為 0(管理員修正可能扣到負,
// 但等級不該因此變成負數 —— 顯示 1 級比顯示 -3 級有意義)。
func (c Config) LevelFor(xp int64) int32 {
	return levelFor(c.curveOf(), xp)
}

func levelFor(cv Curve, xp int64) int32 {
	if xp <= 0 {
		return 1
	}
	if cv.Type == "table" {
		level := int32(1)
		for i, th := range cv.Thresholds {
			if xp < th {
				break
			}
			level = int32(i) + 2 //nolint:gosec // i 受 thresholds 長度限制,不會溢位
		}
		return level
	}
	// formula 的反函數:level = floor((xp/base)^(1/exponent)) + 1
	n := math.Floor(math.Pow(float64(xp)/cv.Base, 1/cv.Exponent)) + 1
	if n < 1 {
		return 1
	}
	// 浮點誤差可能讓門檻邊界差一級,兩邊各校正一次。
	// 不校正的話「剛好升級」那一刻會顯示成還差 1 點,而那正是使用者最會盯著看的時刻。
	level := int32(n) //nolint:gosec // 下面立刻做邊界校正
	for ThresholdFor(cv, level+1) <= xp {
		level++
	}
	for level > 1 && ThresholdFor(cv, level) > xp {
		level--
	}
	return level
}

// ProgressFor 算出等級與本級進度。
func (c Config) ProgressFor(xp int64) Progress {
	cv := c.curveOf()
	if xp < 0 {
		xp = 0
	}
	level := levelFor(cv, xp)
	cur := ThresholdFor(cv, level)
	next := ThresholdFor(cv, level+1)
	span := next - cur
	if span <= 0 {
		// table 模式走到表尾:沒有下一級可算,進度視為滿。
		return Progress{Level: level, XPIntoLevel: 0, XPForLevel: 0, NextLevelAt: next}
	}
	return Progress{
		Level:       level,
		XPIntoLevel: xp - cur,
		XPForLevel:  span,
		NextLevelAt: next,
	}
}

// ValidateCurve 在啟動或設定變更時檢查曲線可用。
// 曲線設壞的表現是「所有人都是 1 級」或「所有人都是 9999 級」,
// 而兩者都不會報錯 —— 所以要在寫入設定時就擋下來。
func ValidateCurve(cv Curve) error {
	switch cv.Type {
	case "", "formula":
		if cv.Base < 0 || cv.Exponent < 0 {
			return fmt.Errorf("曲線的 base 與 exponent 不可為負(base=%v exponent=%v)", cv.Base, cv.Exponent)
		}
	case "table":
		if len(cv.Thresholds) == 0 {
			return fmt.Errorf("曲線 type=table 但 thresholds 是空的")
		}
		prev := int64(-1)
		for i, th := range cv.Thresholds {
			if th <= prev {
				return fmt.Errorf("thresholds 必須嚴格遞增,第 %d 項 %d 沒有大於前一項 %d", i, th, prev)
			}
			prev = th
		}
	default:
		return fmt.Errorf("曲線 type %q 不認得,只能是 formula 或 table", cv.Type)
	}
	return nil
}
