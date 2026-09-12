package xp_test

import (
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/xp"
)

// 等級是 XP 的純函數。這組測試守的是兩件事:
//   1. 門檻與反推**互為反函數**(算出來的等級,其門檻不會高於手上的 XP)
//   2. 邊界剛好那一點的行為 —— 那正是使用者盯著看的時刻

func defaultConfig() xp.Config { return xp.Config{} }

// schemas/24 的對照表。這幾個數字是文件裡寫給使用者看的,
// 對不上就是文件在騙人。
func TestThreshold_MatchesPublishedTable(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cases := []struct {
		level int32
		want  int64
	}{
		{1, 0}, // 剛註冊 = 1 級 0 XP。用 n^1.5 的話這裡會是 100,新人變成 0 級
		{2, 100},
		{3, 283},
		{5, 800},
		{10, 2700},
		{20, 8282},
		{50, 34300},
		{100, 98504},
	}
	for _, c := range cases {
		got := cfg.ProgressFor(c.want).Level
		if got != c.level {
			t.Errorf("累積 %d XP 應是 %d 級,得到 %d", c.want, c.level, got)
		}
	}
}

// 沒有 max_level。舊設定寫 100 級封頂,重度使用者三個月就摸到,之後沒東西追。
func TestLevel_HasNoCap(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	if got := cfg.ProgressFor(98504).Level; got != 100 {
		t.Fatalf("98504 XP 應是 100 級,得到 %d", got)
	}
	if got := cfg.ProgressFor(1_000_000).Level; got <= 100 {
		t.Fatalf("百萬 XP 應該遠超過 100 級,得到 %d", got)
	}
}

// 反函數必須自洽:每一級的門檻餵回去,拿到的就是那一級。
// 浮點的 pow/round 在邊界很容易差一級,而差一級的表現是
// 「明明升級了卻還顯示舊等級」——最難被回報、也最傷的那種 bug。
func TestLevel_IsInverseOfThreshold(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cv := xp.Curve{Type: "formula", Base: xp.DefaultCurveBase, Exponent: xp.DefaultCurveExponent}
	for level := int32(1); level <= 500; level++ {
		at := xp.ThresholdFor(cv, level)
		if got := cfg.ProgressFor(at).Level; got != level {
			t.Fatalf("第 %d 級門檻 %d,反推卻是 %d 級", level, at, got)
		}
		// 門檻前一點一定還是上一級
		if level > 1 {
			if got := cfg.ProgressFor(at - 1).Level; got != level-1 {
				t.Fatalf("門檻 %d 差 1 應仍是 %d 級,得到 %d", at, level-1, got)
			}
		}
	}
}

func TestProgress_WithinLevel(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	// L2 起點 100、L3 起點 283 → 本級需要 183
	p := cfg.ProgressFor(150)
	if p.Level != 2 {
		t.Fatalf("150 XP 應是 2 級,得到 %d", p.Level)
	}
	if p.XPIntoLevel != 50 {
		t.Fatalf("本級已累積應為 50,得到 %d", p.XPIntoLevel)
	}
	if p.XPForLevel != 183 {
		t.Fatalf("本級需要應為 183,得到 %d", p.XPForLevel)
	}
	if p.NextLevelAt != 283 {
		t.Fatalf("下一級門檻應為 283,得到 %d", p.NextLevelAt)
	}
}

// 管理員修正可以扣到負數。顯示 -3 級沒有意義,也會讓進度條畫不出來。
func TestProgress_NegativeXPClampsToLevelOne(t *testing.T) {
	t.Parallel()
	p := defaultConfig().ProgressFor(-500)
	if p.Level != 1 || p.XPIntoLevel != 0 {
		t.Fatalf("負 XP 應是 1 級 0 進度,得到 %+v", p)
	}
}

// 沒指派 ruleset 的 community(M1 常態)照樣要算得出等級。
func TestProgress_EmptyConfigUsesDefaults(t *testing.T) {
	t.Parallel()
	var empty xp.Config
	if got := empty.ProgressFor(2700).Level; got != 10 {
		t.Fatalf("空設定應套用預設曲線,2700 XP 期望 10 級,得到 %d", got)
	}
}

func TestProgress_TableMode(t *testing.T) {
	t.Parallel()
	cfg := xp.Config{Curve: xp.Curve{Type: "table", Thresholds: []int64{50, 150, 400}}}
	for _, c := range []struct {
		xp    int64
		level int32
	}{{0, 1}, {49, 1}, {50, 2}, {149, 2}, {150, 3}, {400, 4}, {9999, 4}} {
		if got := cfg.ProgressFor(c.xp).Level; got != c.level {
			t.Errorf("table 模式 %d XP 應是 %d 級,得到 %d", c.xp, c.level, got)
		}
	}
}

// 表格用完之後不能回 0 門檻 —— 那會讓最高等的人每一點 XP 都在「升級」。
func TestProgress_TableBeyondLastThreshold(t *testing.T) {
	t.Parallel()
	cfg := xp.Config{Curve: xp.Curve{Type: "table", Thresholds: []int64{50, 150}}}
	p := cfg.ProgressFor(10_000)
	if p.Level != 3 {
		t.Fatalf("超過表尾應停在最後一級,得到 %d", p.Level)
	}
	if p.XPForLevel != 0 {
		t.Fatalf("沒有下一級時本級需求應為 0(進度視為滿),得到 %d", p.XPForLevel)
	}
}

// 曲線設壞的表現是「所有人都 1 級」或「所有人都 9999 級」,兩者都不報錯。
// 所以要在寫入設定時就擋。
func TestValidateCurve(t *testing.T) {
	t.Parallel()
	ok := []xp.Curve{
		{},
		{Type: "formula", Base: 100, Exponent: 1.5},
		{Type: "table", Thresholds: []int64{10, 20, 30}},
	}
	for _, cv := range ok {
		if err := xp.ValidateCurve(cv); err != nil {
			t.Errorf("%+v 應該合法: %v", cv, err)
		}
	}
	bad := []xp.Curve{
		{Type: "table"}, // 空表
		{Type: "table", Thresholds: []int64{10, 10}},    // 沒有嚴格遞增
		{Type: "table", Thresholds: []int64{30, 20}},    // 倒退
		{Type: "formula", Base: -1},                     // 負 base
		{Type: "formula", Base: 100, Exponent: -1},      // 負 exponent
		{Type: "exponential", Base: 100, Exponent: 1.5}, // 不認得的型別
	}
	for _, cv := range bad {
		if err := xp.ValidateCurve(cv); err == nil {
			t.Errorf("%+v 應該被拒絕", cv)
		}
	}
}
