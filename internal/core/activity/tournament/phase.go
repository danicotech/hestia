// Package tournament 是《百業試鋒》一屆賽事的階段狀態機、設定解析與抽籤。
//
// # 為什麼階段機要獨立成型別而不是幾個 if
//
// 七個階段之間只有六條合法邊,外加一條刻意保留的回頭路。用字串比大小或散在
// 各 handler 裡寫 `if phase != "signup"`,漏掉一處的代價是「報名期結束後還能報名」
// 或「抽完籤還能改段位」—— 後者會讓已發出去的 match_budgets 與段位對不上,
// 而那是整套讓武制度的公正性基礎。
//
// 所以階段判定收斂在這裡,對外只有 RequirePhase 一個守衛函式。
// 前端把按鈕變灰只是視覺,**伺服器端這道檢查才是權威**(schemas/20)。
package tournament

import (
	"errors"
	"fmt"
)

// Phase 是賽事生命週期。字面值刻意等於 tournaments.phase 的 CHECK 值域,
// 三處(DB / Go / proto enum)語意一致,轉換層不需要對照表。
type Phase string

const (
	// PhaseSignup 報名期。
	PhaseSignup Phase = "signup"
	// PhaseSignupClosed 報名截止,尚未開始評段。
	PhaseSignupClosed Phase = "signup_closed"
	// PhaseRanking 裁判逐人評定段位中。
	PhaseRanking Phase = "ranking"
	// PhaseRanked 段位已公布。刻意排在抽籤**之前**,讓選手有異議可提
	// (走 Discord 找裁判,不做線上申訴流程)。
	PhaseRanked Phase = "ranked"
	// PhaseDrawing 抽籤中。裁判確認前可重抽或交換籤位。
	PhaseDrawing Phase = "drawing"
	// PhaseInProgress 比賽中。進到這裡等於「抽籤已確認並鎖定」。
	PhaseInProgress Phase = "in_progress"
	// PhaseFinished 結束。
	PhaseFinished Phase = "finished"
)

// phaseOrder 是七階段的前後次序。用一張表而不是在 Phase 上掛數字,
// 是為了讓「合法的下一步」只由這個切片決定 —— 插入新階段時只改這裡。
var phaseOrder = []Phase{
	PhaseSignup,
	PhaseSignupClosed,
	PhaseRanking,
	PhaseRanked,
	PhaseDrawing,
	PhaseInProgress,
	PhaseFinished,
}

// 階段相關錯誤。transport 層用 errors.Is 分辨要回 409 還是 400。
var (
	// ErrUnknownPhase 是字串不在七階段之內 —— DB 的 CHECK 擋得住,
	// 但從 proto enum 轉過來的值不一定,所以這裡也要擋。
	ErrUnknownPhase = errors.New("未知的賽事階段")
	// ErrIllegalTransition 是跳階或倒退。階段只能一步一步往前,
	// 唯一例外是 drawing 退回 ranked。
	ErrIllegalTransition = errors.New("賽事階段轉換不合法")
	// ErrWrongPhase 是「這個操作在目前階段不開放」。
	// 與 ErrIllegalTransition 不同:那個是推進階段本身出錯,這個是別的操作被階段擋下。
	ErrWrongPhase = errors.New("目前賽事階段不允許這個操作")
)

// Valid 回報這個值是否為七階段之一。
func (p Phase) Valid() bool {
	return p.index() >= 0
}

// String 讓 Phase 可以直接進錯誤訊息與日誌。
func (p Phase) String() string { return string(p) }

// Label 是給人看的中文階段名。只用於錯誤訊息與後台;
// 對外顯示的文案由前端依 proto enum 決定,不要用這裡的。
func (p Phase) Label() string {
	switch p {
	case PhaseSignup:
		return "報名期"
	case PhaseSignupClosed:
		return "報名截止"
	case PhaseRanking:
		return "評段中"
	case PhaseRanked:
		return "段位已公布"
	case PhaseDrawing:
		return "抽籤中"
	case PhaseInProgress:
		return "比賽中"
	case PhaseFinished:
		return "已結束"
	default:
		return "未知階段"
	}
}

// index 回傳階段在 phaseOrder 裡的位置;不是合法階段時回 -1。
func (p Phase) index() int {
	for i, v := range phaseOrder {
		if v == p {
			return i
		}
	}
	return -1
}

// ParsePhase 把資料庫或契約層來的字串轉成 Phase。
func ParsePhase(s string) (Phase, error) {
	p := Phase(s)
	if !p.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownPhase, s)
	}
	return p, nil
}

// Phases 回傳七階段的正序副本,供後台顯示進度條用。
// 回副本而不是 phaseOrder 本身:呼叫端改動切片不該影響狀態機。
func Phases() []Phase {
	out := make([]Phase, len(phaseOrder))
	copy(out, phaseOrder)
	return out
}

// CanAdvance 判斷 from → to 是否為合法轉換。
//
// 規則只有兩條:
//
//	往前一步        signup → signup_closed → … → finished
//	drawing → ranked  唯一的回頭路
//
// 為什麼只留這一條回頭路:段位公布後仍可能有異議(走 Discord 找裁判),
// 而 BP 是依段位差算出來的,抽完籤才改段位會讓已發的 match_budgets 對不上。
// 於是流程被設計成「要改段位就退回 ranked、改完重抽」,退回時對戰表一併作廢。
//
// 其他方向一律拒絕,包含「原地不動」(from == to)—— 那通常是重複送出,
// 靜靜回成功會讓呼叫端以為自己推進了階段。
func CanAdvance(from, to Phase) error {
	fi, ti := from.index(), to.index()
	if fi < 0 {
		return fmt.Errorf("%w: from=%q", ErrUnknownPhase, from)
	}
	if ti < 0 {
		return fmt.Errorf("%w: to=%q", ErrUnknownPhase, to)
	}
	if ti == fi+1 {
		return nil
	}
	if from == PhaseDrawing && to == PhaseRanked {
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
}

// IsRollback 回報這次轉換是不是那條唯一的回頭路(drawing → ranked)。
//
// 存在的理由是退回時要順手清掉對戰表與籤位,而呼叫端不該自己再寫一次
// `from == PhaseDrawing && to == PhaseRanked` —— 判斷條件有兩個地方就會有兩個版本。
func IsRollback(from, to Phase) bool {
	return from == PhaseDrawing && to == PhaseRanked
}

// RequirePhase 是所有「這個操作只在某些階段開放」的唯一守衛。
//
// 報名、評段、下注、讓武選購全部經過它。allowed 為空時一律拒絕 ——
// 那代表呼叫端忘了填,預設放行會讓一個打錯的呼叫悄悄變成無限制。
func RequirePhase(current Phase, allowed ...Phase) error {
	if !current.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownPhase, current)
	}
	for _, a := range allowed {
		if current == a {
			return nil
		}
	}
	return fmt.Errorf("%w: 目前為 %s(%s)", ErrWrongPhase, current, current.Label())
}

// RanksEditable 回報現在還能不能改段位。
//
// 只有 ranking(裁判評段中)與 ranked(公布後的異議期)可改。
// 一旦進入 drawing 就鎖死 —— 這是 schemas/20 的實作連帶:
// BP 依段位差算出,抽籤後改段位會讓已發的預算對不上,要改必須先退回 ranked 重抽。
func RanksEditable(p Phase) bool {
	return p == PhaseRanking || p == PhaseRanked
}
