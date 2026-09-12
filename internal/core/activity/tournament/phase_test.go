package tournament

import (
	"errors"
	"strings"
	"testing"
)

// TestCanAdvanceExhaustive 窮舉七階段的全部 49 種轉換。
//
// 用窮舉而不是挑幾個案例:階段機的價值全在「不該發生的轉換真的不會發生」,
// 而漏測的那一格永遠是日後出事的那一格。合法的只有七格 ——
// 六條往前的邊,加上 drawing → ranked 那條唯一的回頭路。
func TestCanAdvanceExhaustive(t *testing.T) {
	t.Parallel()

	all := Phases()
	legal := map[[2]Phase]bool{
		{PhaseSignup, PhaseSignupClosed}:  true,
		{PhaseSignupClosed, PhaseRanking}: true,
		{PhaseRanking, PhaseRanked}:       true,
		{PhaseRanked, PhaseDrawing}:       true,
		{PhaseDrawing, PhaseInProgress}:   true,
		{PhaseInProgress, PhaseFinished}:  true,
		{PhaseDrawing, PhaseRanked}:       true, // 唯一的回頭路
	}

	for _, from := range all {
		for _, to := range all {
			err := CanAdvance(from, to)
			want := legal[[2]Phase{from, to}]
			switch {
			case want && err != nil:
				t.Errorf("CanAdvance(%s, %s) 應該合法,卻回 %v", from, to, err)
			case !want && err == nil:
				t.Errorf("CanAdvance(%s, %s) 應該被擋下,卻放行了", from, to)
			case !want && !errors.Is(err, ErrIllegalTransition):
				t.Errorf("CanAdvance(%s, %s) 的錯誤要是 ErrIllegalTransition,得到 %v", from, to, err)
			}
		}
	}
}

// TestCanAdvanceRejectsSelf 確認「原地不動」也被擋。
//
// 單獨測是因為它最容易被當成「無害」而放行:重複送出的請求靜靜回成功,
// 呼叫端會以為自己推進了階段,而實際上什麼都沒發生。
func TestCanAdvanceRejectsSelf(t *testing.T) {
	t.Parallel()

	for _, p := range Phases() {
		if err := CanAdvance(p, p); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("CanAdvance(%s, %s) 要回 ErrIllegalTransition,得到 %v", p, p, err)
		}
	}
}

// TestCanAdvanceRejectsBackward 確認除了 drawing → ranked 之外,任何倒退都被擋。
func TestCanAdvanceRejectsBackward(t *testing.T) {
	t.Parallel()

	all := Phases()
	for i, from := range all {
		for j, to := range all {
			if j >= i {
				continue
			}
			if IsRollback(from, to) {
				continue
			}
			if err := CanAdvance(from, to); err == nil {
				t.Errorf("CanAdvance(%s, %s) 是倒退,不該放行", from, to)
			}
		}
	}
}

// TestCanAdvanceRejectsSkipping 確認一次跳兩階以上都被擋。
func TestCanAdvanceRejectsSkipping(t *testing.T) {
	t.Parallel()

	all := Phases()
	for i, from := range all {
		for j := i + 2; j < len(all); j++ {
			if err := CanAdvance(from, all[j]); !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("CanAdvance(%s, %s) 跳階,要回 ErrIllegalTransition,得到 %v", from, all[j], err)
			}
		}
	}
}

func TestCanAdvanceUnknownPhase(t *testing.T) {
	t.Parallel()

	if err := CanAdvance(Phase("preparation"), PhaseSignup); !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("來源階段未知時要回 ErrUnknownPhase,得到 %v", err)
	}
	if err := CanAdvance(PhaseSignup, Phase("shop_open")); !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("目標階段未知時要回 ErrUnknownPhase,得到 %v", err)
	}
}

func TestIsRollback(t *testing.T) {
	t.Parallel()

	if !IsRollback(PhaseDrawing, PhaseRanked) {
		t.Error("drawing → ranked 應該是回頭路")
	}
	if IsRollback(PhaseRanked, PhaseDrawing) {
		t.Error("ranked → drawing 是往前一步,不是回頭路")
	}
	if IsRollback(PhaseInProgress, PhaseDrawing) {
		t.Error("in_progress → drawing 不是合法的回頭路")
	}
}

// TestRequirePhaseEmptyAllowedDenies 是這個守衛最重要的一條性質。
//
// allowed 為空時必須拒絕。預設放行的話,一個忘了填參數的呼叫會悄悄
// 變成「任何階段都能做」,而那個 bug 在功能測試裡完全看不出來 ——
// 所有正常流程都會照常通過。
func TestRequirePhaseEmptyAllowedDenies(t *testing.T) {
	t.Parallel()

	for _, p := range Phases() {
		if err := RequirePhase(p); !errors.Is(err, ErrWrongPhase) {
			t.Errorf("RequirePhase(%s) 沒給 allowed 時要拒絕,得到 %v", p, err)
		}
	}
}

func TestRequirePhase(t *testing.T) {
	t.Parallel()

	if err := RequirePhase(PhaseSignup, PhaseSignup); err != nil {
		t.Errorf("階段相符不該回錯誤,得到 %v", err)
	}
	if err := RequirePhase(PhaseRanked, PhaseRanking, PhaseRanked); err != nil {
		t.Errorf("階段在允許清單內不該回錯誤,得到 %v", err)
	}
	if err := RequirePhase(PhaseDrawing, PhaseRanking, PhaseRanked); !errors.Is(err, ErrWrongPhase) {
		t.Errorf("階段不在清單內要回 ErrWrongPhase,得到 %v", err)
	}
	if err := RequirePhase(Phase("preparation"), PhaseSignup); !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("未知階段要回 ErrUnknownPhase,得到 %v", err)
	}
}

// TestRequirePhaseErrorDoesNotLeakAllowed 確認錯誤訊息只講「現在是什麼階段」。
//
// 目前階段是公開資訊(對戰表頁就看得到),但把「這個操作在哪幾個階段開放」
// 也寫進去,等於把後台的流程圖送給任何一個亂打 API 的人。
func TestRequirePhaseErrorDoesNotLeakAllowed(t *testing.T) {
	t.Parallel()

	err := RequirePhase(PhaseSignup, PhaseDrawing, PhaseInProgress)
	if err == nil {
		t.Fatal("應該回錯誤")
	}
	for _, leaked := range []Phase{PhaseDrawing, PhaseInProgress} {
		if strings.Contains(err.Error(), string(leaked)) {
			t.Errorf("錯誤訊息不該提到允許的階段 %s:%v", leaked, err)
		}
	}
}

// TestRanksEditable 段位只在評段中與公布後的異議期可改。
//
// 進入 drawing 之後鎖死,是因為 BP 依段位差算出 ——
// 抽完籤才改段位會讓已發的 match_budgets 與段位對不上,而沒有任何約束會擋。
func TestRanksEditable(t *testing.T) {
	t.Parallel()

	want := map[Phase]bool{
		PhaseSignup:       false,
		PhaseSignupClosed: false,
		PhaseRanking:      true,
		PhaseRanked:       true,
		PhaseDrawing:      false,
		PhaseInProgress:   false,
		PhaseFinished:     false,
	}
	for p, w := range want {
		if got := RanksEditable(p); got != w {
			t.Errorf("RanksEditable(%s) = %v, 要 %v", p, got, w)
		}
	}
}

func TestParsePhase(t *testing.T) {
	t.Parallel()

	for _, p := range Phases() {
		got, err := ParsePhase(string(p))
		if err != nil || got != p {
			t.Errorf("ParsePhase(%q) = %v, %v", p, got, err)
		}
	}
	// preparation 是舊草稿的階段,已隨 loadouts 一起刪除。
	// 舊資料或舊前端送上來時必須被擋,不能因為「看起來像階段」就放行。
	if _, err := ParsePhase("preparation"); !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("preparation 已廢除,要回 ErrUnknownPhase,得到 %v", err)
	}
	if _, err := ParsePhase(""); !errors.Is(err, ErrUnknownPhase) {
		t.Errorf("空字串要回 ErrUnknownPhase,得到 %v", err)
	}
}

// TestPhasesReturnsCopy 確認 Phases() 不會把內部的次序表交出去。
func TestPhasesReturnsCopy(t *testing.T) {
	t.Parallel()

	got := Phases()
	got[0] = Phase("tampered")
	if Phases()[0] != PhaseSignup {
		t.Error("呼叫端改動回傳的切片後,狀態機的次序表被污染了")
	}
}
