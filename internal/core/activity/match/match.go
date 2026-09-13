// Package match 是「裁判帶著一場比賽走完生命週期」的編排。
//
// # 為什麼要有這一層
//
// 六個裁判動作(開盤、封盤、開打、判勝負、棄賽、設直播)每一個都同時牽動
// 三到五個套件:讓武(BP 預算與封盤)、下注(結算與退款)、賽程樹(晉級)、
// 跨屆戰績(wins/losses)、以及 Discord 公告(outbox)。
//
// 把這些連帶散在 transport handler 裡的後果不是「程式醜」,而是:
// 判了勝負但沒結算、結算了但沒晉級、晉級了但沒公告 —— 每一種都是真實可達的狀態,
// 而且全都要人工去資料庫修。所以編排必須有一個唯一的位置,而且**全部在同一個
// transaction 裡**:要嘛六件事都發生,要嘛一件都沒發生。
//
// # 為什麼 TX 是型別參數
//
// 與 betting 同一個理由:core 依分層規範不能 import pgx(depguard 會擋),
// 但「判勝負 + 派彩」的原子性又只能靠真正的 transaction。把 tx 做成型別參數,
// 由 infrastructure 用 pgx.Tx 實例化 —— core 一行 pgx 都沒有,實作端仍是靜態型別的,
// 而且 *betting.Service[pgx.Tx] 不需要任何包裝就滿足本套件的 Bets[pgx.Tx]。
//
// # 不可逆的三個動作
//
//	LockHandicap    封盤後選手不能再改讓武,且立刻公開並發公告
//	ReportResult    判定勝負會觸發下注結算與派彩
//	WithdrawPlayer  棄賽會判對手不戰而勝,並把該場注單全額退款
//
// 三者都要求 confirm = true 作為二次確認(judge.proto 的設計要求)。
// 誤觸的代價由人承擔,不由系統吞掉 —— 派彩一旦發出去就很難收回。
//
// # 多回合制(2026-09-13 增補,schemas/20)
//
// 一場比賽是 best_of 個回合(config.format),整場勝者 = 先拿到 ⌈best_of/2⌉ 勝的一方。
// 裁判動線因此多了「一場一次」的設定確認(ReviewSetup / ConfirmSetup)與
// 「每回合一次」的 StartRound / FinishRound;ReportResult 只在 best_of = 1 時仍可用,
// 而且走的是同一條回合路徑(開始第 1 回合 → 以該勝者結束它)。
// 整場勝者是回合推導的衍生值:寫入前重算核對,不符即 ErrRoundsInconsistent。
// 三條紀律各有應用層與 DB 層一道:封盤不可逆、未確認不得開打、整場勝者 = 回合推導。
//
// # 事件不是附加功能
//
// 使用者要的 Discord 公告有三類源自這條生命週期(開盤提醒、封盤公示、賽果晉級)。
// 事件與領域變更同 tx 寫 outbox,而且 payload 自己就夠渲染 ——
// renderer 不該為了組一則公告再回頭查資料庫,那等於讓公告的正確性取決於
// 「查的時候資料還在不在」。
package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// Status 是場次狀態,字面值即 activity.matches.status 的 CHECK 值域。
//
// 這裡是這個概念的權威位置:本套件是唯一會**寫**這個欄位的地方。
// betting.MatchStatus 是同一個值域的讀者副本(先於本套件存在,只回答
// 「現在能不能下注」),兩者語意不同但值域相同 —— 日後收斂的話,
// 方向應該是讓 betting 引用這裡,不是反過來。
type Status string

const (
	// StatusPending 對手未齊(等上一輪)。讓武 ✗ 下注 ✗
	StatusPending Status = "pending"
	// StatusReady 雙方確定、裁判已開盤。讓武 ✓ 下注 ✓
	StatusReady Status = "ready"
	// StatusLocked 讓武已封盤、內容公開。讓武 ✗ 下注 ✓
	StatusLocked Status = "locked"
	// StatusLive 比賽進行中。讓武 ✗ 下注 ✗
	StatusLive Status = "live"
	// StatusDone 已分勝負。
	StatusDone Status = "done"
)

// ResultKind 是勝負的產生方式,字面值即 matches.result_kind 的 CHECK 值域。
type ResultKind string

const (
	// ResultNormal 真的打完一場。
	ResultNormal ResultKind = "normal"
	// ResultWalkover 不戰而勝(對手棄賽)。賠率是按「真的打一場」算的,
	// 所以這種場次的注單走全額退款,不讓任何人贏錢。
	ResultWalkover ResultKind = "walkover"
)

// 稽核紀錄的 action 值。與 topic 分開是因為兩者的讀者不同:
// 稽核是給人查「誰改了什麼」,事件是給 Discord 渲染公告。
const (
	ActionOpenHandicap = "match.open_handicap"
	ActionLockHandicap = "match.lock_handicap"
	// ActionRefundSelection 是裁判代退一筆讓武選擇(BP 退回該場預算)。
	// 與 lock 同樣走 RecordJudgeAction,理由見 JudgeAction 的註解。
	ActionRefundSelection = "match.refund_selection"
	ActionStartMatch      = "match.start"
	ActionReportResult    = "match.report_result"
	ActionSetStreamURL    = "match.set_stream_url"
	ActionWithdraw        = "player.withdraw"
	// ActionConfirmSetup 開賽前設定確認(schemas/20 裁判動線,grill Q1/Q9)。
	ActionConfirmSetup = "match.confirm_setup"
	// ActionStartRound / ActionFinishRound 是逐回合的兩個時刻(match_rounds 建列 / 填勝者)。
	ActionStartRound  = "match.start_round"
	ActionFinishRound = "match.finish_round"
	// ActionRecordViolation 是裁判記了一筆違規(schemas/27)。違規者本人不進稽核表
	// (選手不一定有平台帳號),這一筆記的是「裁判做了記錄」這個動作。
	ActionRecordViolation = "match.record_violation"
)

// AuditTargetMatch / AuditTargetPlayer 是 admin_audit_logs.target_type 的值。
//
// 導出是因為 adapter 必須填同樣的字串:RecordJudgeAction 由 service 帶進來,
// 另外五個動作的稽核是 adapter 自己掛的。不導出的話那邊只能再抄一份字面值,
// 而兩份字面值裡有一份會在某次改名時被忘記 —— 症狀是稽核查詢漏掉一半的紀錄,
// 而且要等到有人翻舊帳時才會發現。
const (
	AuditTargetMatch  = "activity.match"
	AuditTargetPlayer = "activity.tournament_player"
)

// ── 型別 ────────────────────────────────────────────────────────

// Player 是一位參賽者在本套件視角下的樣子。
//
// 同時用於「場上的一方」與「棄賽的對象」:兩者是同一個實體的同一份資料,
// 拆成兩個結構只會讓 adapter 寫兩份幾乎一樣的 SELECT(鐵則 9)。
// ID 為 0 表示該側尚未確定(對應 p1_player_id / p2_player_id 的 NULL)。
type Player struct {
	ID           int64
	PublicID     string
	TournamentID int64
	// FencerID 指向跨屆檔案。判完勝負要靠它增量更新 wins/losses。
	FencerID int64
	// DisplayName 是報名當下的快照 —— 公告要顯示的就是這個,
	// 不是現在的暱稱(對戰表公布之後改名不該讓舊公告對不上)。
	DisplayName string
	Rank        bp.Rank
	Status      tournament.PlayerStatus
}

// Seated 回報這一側是否已經確定。
func (p Player) Seated() bool { return p.ID != 0 }

// MatchKind 是場次在賽程裡的身分,字面值即 matches.kind 的 CHECK 值域。
type MatchKind string

const (
	// KindBracket 晉級樹上的一場:勝者往下一輪走。
	KindBracket MatchKind = "bracket"
	// KindThirdPlace 季軍戰:準決賽兩位敗者加打,**勝者不晉級**。
	// round 記為決賽那一輪、slot 另給(bracket.ThirdPlaceSlot),其餘流程與普通場次完全相同。
	KindThirdPlace MatchKind = "third_place"
)

// Match 是一場比賽的完整狀態。
type Match struct {
	ID           int64
	PublicID     string
	TournamentID int64
	Round        int
	Slot         int
	// Kind 空值視同 KindBracket(adapter 一律填;fake 與舊資料可能留空)。
	Kind             MatchKind
	Status           Status
	ResultKind       ResultKind
	HandicapOpen     bool
	HandicapLockedAt *time.Time
	StreamURL        string
	// StartedAt = 第一回合的正式決鬥開始。這一刻同時關下注、啟動計時。
	StartedAt  *time.Time
	FinishedAt *time.Time
	// WinnerPlayerID 0 = 尚未分出勝負。
	//
	// 多回合制下它是**衍生值但仍存**:等於 match_rounds 裡先拿到 WinsNeeded 勝的
	// 那一方。每次寫入前由 service 重算核對(ErrRoundsInconsistent),
	// 與 match_budgets.spent 同一套紀律。存它是因為建樹晉級與 FK 都要它。
	WinnerPlayerID int64
	// SetupConfirmedAt 是開賽前設定確認的時刻;nil = 還沒確認,**不能開打**。
	// DB 側另有 CHECK (started_at IS NULL OR setup_confirmed_at IS NOT NULL) 作最後一道。
	SetupConfirmedAt *time.Time
	// SetupConfirmedBy 是確認的裁判(platform.users.id,弱參照);0 = 尚未確認。
	SetupConfirmedBy int64
	P1               Player
	P2               Player
}

// BothSeated 回報雙方是否都已確定。開盤的前提。
func (m Match) BothSeated() bool { return m.P1.Seated() && m.P2.Seated() }

// IsThirdPlace 回報這是不是季軍戰。
func (m Match) IsThirdPlace() bool { return m.Kind == KindThirdPlace }

// PlayerByID 回傳場上 id 對應的一方;不是場上兩人之一時回零值。
func (m Match) PlayerByID(playerID int64) Player {
	if playerID == 0 {
		return Player{}
	}
	switch playerID {
	case m.P1.ID:
		return m.P1
	case m.P2.ID:
		return m.P2
	default:
		return Player{}
	}
}

// PlayerByPublicID 回傳場上 public_id 對應的一方;不是場上兩人之一時回零值。
func (m Match) PlayerByPublicID(publicID string) Player {
	if publicID == "" {
		return Player{}
	}
	switch publicID {
	case m.P1.PublicID:
		return m.P1
	case m.P2.PublicID:
		return m.P2
	default:
		return Player{}
	}
}

// ResolveRoundWinners 依場上雙方把每回合的 WinnerPlayerID 翻成 public_id(就地修改)。
//
// 回合列本身只存 winner_player_id(ListMatchRounds 不 JOIN 選手),而對外只能出現
// public_id。翻譯需要的資料就在 Match 上,所以由它來做 —— 讀取側(activityreadpg)
// 批次撈回合時手上沒有場次,回傳的 Round 其 WinnerPublicID 為空,呼叫端拿到
// 場次後用這支補齊。
func (m Match) ResolveRoundWinners(rounds []Round) {
	for i := range rounds {
		rounds[i].WinnerPublicID = m.PlayerByID(rounds[i].WinnerPlayerID).PublicID
	}
}

// ── 回合 ────────────────────────────────────────────────────────

// Round 是 activity.match_rounds 的一列:一回合的起訖與勝者。
//
// 回合列在裁判按「正式決鬥開始」時建,不預建 —— 所以「還沒開始的回合」= 沒有列,
// StartedAt 建列即有值。FinishedAt 與 WinnerPlayerID 同生共死(DB CHECK)。
type Round struct {
	RoundNo    int
	StartedAt  time.Time
	FinishedAt *time.Time
	// WinnerPlayerID 0 = 進行中。
	WinnerPlayerID int64
	// WinnerPublicID 是 WinnerPlayerID 的對外形式,由 Match.ResolveRoundWinners 填。
	WinnerPublicID string
}

// Finished 回報這一回合是否已經結束。
func (r Round) Finished() bool { return r.FinishedAt != nil }

// Score 是一場比賽到目前為止的回合比數。**衍生值**,由回合列數出來,不存。
type Score struct {
	P1Wins int
	P2Wins int
}

// ScoreOf 數出雙方各拿幾勝。不是場上兩人的勝者(資料異常)不計入任何一方。
func ScoreOf(m Match, rounds []Round) Score {
	var s Score
	for _, r := range rounds {
		if r.WinnerPlayerID == 0 {
			continue
		}
		switch r.WinnerPlayerID {
		case m.P1.ID:
			s.P1Wins++
		case m.P2.ID:
			s.P2Wins++
		}
	}
	return s
}

// String 是公告用的「2:1」形式(P1 在前)。
func (s Score) String() string { return fmt.Sprintf("%d:%d", s.P1Wins, s.P2Wins) }

// DecidedWinner 依比數推導整場勝者:先拿到 winsNeeded 勝的一方。
//
// 0 = 尚未分出勝負。兩方同時達標是資料異常(回合列被動過),同樣回 0 ——
// 呼叫端接著會撞上 ErrRoundsInconsistent,那正是要的。
func (s Score) DecidedWinner(m Match, winsNeeded int) int64 {
	if winsNeeded <= 0 {
		return 0
	}
	p1, p2 := s.P1Wins >= winsNeeded, s.P2Wins >= winsNeeded
	switch {
	case p1 && !p2:
		return m.P1.ID
	case p2 && !p1:
		return m.P2.ID
	default:
		return 0
	}
}

// ── 違規(schemas/27)────────────────────────────────────────────

// Ruling 是裁判對一筆違規的判法,字面值即 match_violations.ruling 的 CHECK 值域。
//
// **只是紀錄,不是觸發器**:判該回合 = 裁判把該回合勝者填成對方(FinishRound),
// 判整場 = 走正常的回合路徑。這個值唯一的程式用途是賽果公告要不要帶上它。
type Ruling string

const (
	RulingWarning   Ruling = "warning"
	RulingRoundLoss Ruling = "round_loss"
	RulingMatchLoss Ruling = "match_loss"
	RulingNone      Ruling = "none"
)

// Valid 回報是否為合法值。
func (r Ruling) Valid() bool {
	switch r {
	case RulingWarning, RulingRoundLoss, RulingMatchLoss, RulingNone:
		return true
	default:
		return false
	}
}

// Announced 回報這筆判法要不要寫進賽果公告(grill Q14:只有判負的才帶原因)。
func (r Ruling) Announced() bool { return r == RulingRoundLoss || r == RulingMatchLoss }

// Violation 是 activity.match_violations 的一列,連同顯示用的快照。
//
// 只有 public_id 對外;內部 id 一個都不帶(鐵則 5)。
type Violation struct {
	PublicID      string
	MatchPublicID string
	// RoundNo nil = 開賽前(例:設定確認時發現沒改到)。
	RoundNo *int
	// 違規者,可以是任一方(grill Q6)。
	PlayerPublicID    string
	PlayerDisplayName string
	// 違反哪一項;全部為空 = 不對應特定項目(違反通則)。
	ItemPublicID string
	ItemKey      string
	ItemName     string
	Ruling       Ruling
	Note         string
	// RecordedBy 是裁判(platform.users.id)。
	RecordedBy int64
	CreatedAt  time.Time
}

// Opponent 回傳 playerID 在本場的對手;找不到或對手未定時回零值。
func (m Match) Opponent(playerID int64) Player {
	switch playerID {
	case m.P1.ID:
		return m.P2
	case m.P2.ID:
		return m.P1
	default:
		return Player{}
	}
}

// Tournament 是一屆賽事在本套件視角下需要的部分。
type Tournament struct {
	ID        int64
	PublicID  string
	Slug      string
	Name      string
	Phase     tournament.Phase
	ConfigRaw []byte
	// TotalRounds 是對戰表的總輪數,即 MAX(matches.round);0 = 尚未抽籤。
	//
	// 放在賽事上而不是另開一支查詢:晉級目標與「這是不是決賽」都要用它,
	// 而它在一屆賽事裡是常數(抽籤確認後對戰表不再變形)。
	TotalRounds int
}

// Event 是要與領域變更同 transaction 寫入 outbox 的事件。
type Event struct {
	Topic   string
	Payload json.RawMessage
}

// ── 錯誤 ────────────────────────────────────────────────────────
//
// 全部是 package 層級 sentinel,transport 以 errors.Is 分辨後決定狀態碼與文案。
// 階段錯誤不在這裡重複定義:那是 tournament.ErrWrongPhase,原樣上拋。
var (
	// ErrInvalidRequest 表示請求參數不合法(缺 public_id 等)。
	ErrInvalidRequest = errors.New("請求參數不合法")
	// ErrConfirmationRequired 表示不可逆動作沒帶 confirm。
	ErrConfirmationRequired = errors.New("這是不可逆的動作,需要二次確認")
	// ErrPlayerWithdrawn 表示場上有一方已棄賽,這場不該再走正常流程。
	ErrPlayerWithdrawn = errors.New("場上有一方已棄賽")
	// ErrAlreadyOpen 表示重複開盤。開盤會發提醒公告,默默成功等於再公告一次。
	ErrAlreadyOpen = errors.New("此場次已經開盤")
	// ErrNotReady 表示場次不在 ready,不能封盤。
	ErrNotReady = errors.New("此場次不在讓武選購階段,無法封盤")
	// ErrNotLocked 表示還沒封盤就要開打。
	//
	// 每一場都必須走過封盤,即使同段對決沒有任何讓武:封盤是下注視窗與
	// 「內容已公開」的唯一分界點,跳過它會讓觀眾看到一場沒有人公示過條件的比賽。
	ErrNotLocked = errors.New("讓武尚未封盤,不能開打")
	// ErrNotLive 表示還沒開打就要判勝負。
	ErrNotLive = errors.New("此場次尚未開打,不能判定勝負")
	// ErrMatchFinished 表示這場已經結束,不能再動。
	ErrMatchFinished = errors.New("此場次已完賽")
	// ErrWinnerNotInMatch 表示指定的勝者不是場上兩人之一。
	// matches_winner_is_participant_check 也擋得住,但在這裡擋才給得出人話。
	ErrWinnerNotInMatch = errors.New("勝者必須是本場的選手")
	// ErrAlreadyWithdrawn 表示這位選手已經棄賽。
	// 重複棄賽要出聲:第一次已經判了不戰而勝並退了款,第二次沒有任何事情可做。
	ErrAlreadyWithdrawn = errors.New("此選手已經棄賽")
	// ErrBracketMissing 表示這屆還沒有對戰表(TotalRounds = 0),算不出晉級目標。
	ErrBracketMissing = errors.New("這屆賽事尚未抽籤")
	// ErrAdvanceTargetMissing 表示算得出晉級目標、但那一場不存在。
	//
	// 這是資料完整性失效(對戰表被刪了一半),不是使用者錯誤。
	// 必須整筆失敗:繼續下去的後果是勝者從對戰表上消失,而且沒有人會發現。
	ErrAdvanceTargetMissing = errors.New("晉級目標場次不存在")

	// ErrSetupNotConfirmed 表示還沒做開賽前設定確認就要開打(第一回合)。
	// 應用層先擋、給得出人話;matches_started_requires_setup_check 是最後一道。
	ErrSetupNotConfirmed = errors.New("開賽前設定尚未確認,不能開打")
	// ErrSetupAlreadyConfirmed 表示重複確認。第二次會蓋掉第一次的時間與人,所以要出聲。
	ErrSetupAlreadyConfirmed = errors.New("開賽前設定已經確認過")
	// ErrMatchDecided 表示回合結果已經分出整場勝者,不能再開新回合。
	ErrMatchDecided = errors.New("此場次勝負已由回合結果決定")
	// ErrRoundInProgress 表示上一回合還沒填勝者,不能開始下一回合(或同一回合已經開始過)。
	ErrRoundInProgress = errors.New("上一回合尚未結束")
	// ErrRoundNotFound 表示指定的回合不存在(還沒開始)。
	ErrRoundNotFound = errors.New("回合不存在")
	// ErrRoundAlreadyFinished 表示這一回合已經填過勝者。
	// 回合結果會結算單回合盤口,填第二次等於派彩兩次,必須失敗。
	ErrRoundAlreadyFinished = errors.New("此回合已經結束")
	// ErrRoundsInconsistent 表示要寫入的整場勝者與回合推導的不一致,或回合列與場次狀態對不上。
	//
	// 這是資料完整性失效,不是使用者錯誤。整場勝者是衍生值,寫入前重算核對
	// (與 match_budgets.spent 同一套紀律);不符就整筆失敗,絕不寫一個推導不出來的勝者。
	ErrRoundsInconsistent = errors.New("回合紀錄與場次勝負不一致")
	// ErrMultiRoundMatch 表示這屆是多回合制(best_of > 1),整場勝負要由逐回合結果推導,
	// 不能一次判定 —— 裁判要用 FinishRound。
	ErrMultiRoundMatch = errors.New("多回合制的場次要逐回合判定")
	// ErrPlayerNotInMatch 表示指定的選手不是場上兩人之一(違規紀錄)。
	ErrPlayerNotInMatch = errors.New("選手不是本場的選手")
	// ErrInvalidRuling 表示判法不在四個固定值內。
	ErrInvalidRuling = errors.New("違規判法不合法")
)

// ── Repository port ────────────────────────────────────────────
//
// 以下是本套件對儲存層的全部需求。實作在 storage/*pg;本套件只認標準庫、
// context 與其他 activity 契約套件。
//
// 每個方法都收 tx,而且**沒有一個例外**:六個動作的原子性不是「盡量」而是
// 硬需求(判了勝負卻沒派彩是真錢問題)。讀取也走同一個 tx,因為結算與晉級
// 的判斷必須看得到本 tx 內剛寫下的狀態。
//
// # 稽核紀錄由實作在同一句 SQL 的鄰居裡寫
//
// 帶 ActorUserID / Reason 的寫入參數全部是裁判的破壞性操作,依 schemas/20
// 必須寫進 platform.admin_audit_logs。刻意**不**另開一個 Auditor 讓 service
// 事後補寫:那樣「資料改了但稽核沒記上」就是一個能真實發生的狀態。
// 唯一的例外是 RecordJudgeAction,理由寫在它自己的註解裡。

// ReadyWrite 是開盤的寫入參數:status pending → ready、handicap_open = true。
//
// 「Write」字尾是為了與服務層的 OpenHandicapParams 區分 ——
// 前者是 adapter 要落成 SQL 的東西,後者是裁判送進來的請求。
type ReadyWrite struct {
	MatchID     int64
	ActorUserID int64
	Reason      string
}

// LiveWrite 是開打的寫入參數:status locked → live、started_at = now()。
//
// 時間由資料庫的 now() 決定,core 不該有第二個時鐘(與 bets.settled_at 同一個理由)。
type LiveWrite struct {
	MatchID     int64
	ActorUserID int64
	Reason      string
}

// FinishWrite 是判定勝負的寫入參數。
//
// 四個欄位必須同一句 UPDATE 寫完:winner_player_id、status = 'done'、
// finished_at = now()、result_kind。分成兩句的話,中間那一瞬間的
// 「status = done 但沒有勝者」會被 betting 的結算路徑讀成資料異常。
type FinishWrite struct {
	MatchID        int64
	WinnerPlayerID int64
	ResultKind     ResultKind
	ActorUserID    int64
	Reason         string
}

// StreamWrite 是設定直播連結的寫入參數。URL 為空字串即清成 NULL。
type StreamWrite struct {
	MatchID     int64
	StreamURL   string
	ActorUserID int64
	Reason      string
}

// SeatWrite 是把晉級者寫進下一場的某一側。
//
// 刻意不附 ActorUserID:晉級是判定勝負的**後果**而不是一個獨立的裁判動作,
// 它的理由已經在 FinishWrite 那筆稽核裡了。為後果各記一筆,只會讓稽核表
// 一半的列沒有資訊量,真正要查的那筆反而更難找。
type SeatWrite struct {
	MatchID  int64
	PlayerID int64
	// IsP1 決定寫進 p1_player_id 還是 p2_player_id,由 bracket.Advance 算出。
	IsP1 bool
}

// PlayerStatusWrite 是改變參賽者狀態。
//
// 實作**只在 Status = withdrawn 時寫稽核**:棄賽是裁判的動作,而 eliminated
// 是判定勝負的後果(已被 FinishWrite 的稽核涵蓋)。這條規則是確定的,
// adapter 不需要再判斷任何東西。
type PlayerStatusWrite struct {
	PlayerID    int64
	Status      tournament.PlayerStatus
	ActorUserID int64
	Reason      string
}

// FencerRecordWrite 是跨屆戰績的增量更新。
//
// 口徑由 queries/activity_tournament.sql 的 RecalcFencerStats 定死:
//
//	wins    winner_player_id 指到自己的場次數
//	losses  自己有上場、已分勝負、而勝者不是自己(棄賽造成的 walkover 也算一敗)
//
// 所以每判定一場就 ±1,不在別處補算。用增量而不是先讀後寫:增量在 SQL 裡
// 是原子的,不需要為它多一道列鎖,而真值隨時可用 RecalcFencerStats 對帳。
// 鎖序是 tournament_players → fencers(與報名、評段路徑一致)。
type FencerRecordWrite struct {
	FencerID    int64
	WinsDelta   int32
	LossesDelta int32
}

// SetupConfirmWrite 是開賽前設定確認的寫入參數:setup_confirmed_at = now()、
// setup_confirmed_by = 裁判。要求 status = locked 且尚未確認過(同一句 UPDATE 守)。
//
// Checklist 是裁判按下確認時看到的那份清單,進稽核的 after:清單是推導值,
// 不另存,而「確認時清單長什麼樣」正是日後爭議時要查的東西。
type SetupConfirmWrite struct {
	MatchID     int64
	ActorUserID int64
	Checklist   []handicap.ChecklistEntry
	Reason      string
}

// RoundStartWrite 是「正式決鬥開始」:建一列 match_rounds,started_at 由 DB 的 now() 給。
//
// RoundNo 由 service 依既有回合數算出(n+1);UNIQUE (match_id, round_no) 是防併發的第二道。
type RoundStartWrite struct {
	MatchID     int64
	RoundNo     int
	ActorUserID int64
	Reason      string
}

// RoundFinishWrite 是填該回合勝者。finished_at IS NULL 是「一回合只結束一次」的 DB 側保證。
type RoundFinishWrite struct {
	MatchID        int64
	RoundNo        int
	WinnerPlayerID int64
	ActorUserID    int64
	Reason         string
}

// ThirdPlaceWrite 是建季軍戰:kind = third_place、雙方 = 兩位準決賽敗者、status pending。
//
// 刻意不附 ActorUserID:季軍戰是準決賽判定的**後果**,理由已經在那筆判定的稽核裡
// (與 SeatWrite 同一個判斷)。public_id 由 adapter 產生。
type ThirdPlaceWrite struct {
	TournamentID int64
	Round        int
	Slot         int
	P1PlayerID   int64
	P2PlayerID   int64
}

// ViolationWrite 是記一筆違規。
//
// ItemPublicID 為空 = 不對應特定項目;非空時 adapter 在本屆內找項目,查無回
// handicap.ErrItemNotFound(那句話的權威在讓武套件)。
type ViolationWrite struct {
	MatchID  int64
	RoundNo  *int
	PlayerID int64
	// ItemPublicID 是讓武項目的 public_id(對外只認這個);空 = 違反通則。
	ItemPublicID string
	Ruling       Ruling
	Note         string
	ActorUserID  int64
}

// JudgeAction 是一筆稽核紀錄。
//
// **只有封盤與代退讓武走這裡**,而且是同一個理由:那兩個動作對資料庫的
// 寫入(matches 的封盤 UPDATE、handicap_selections 標 voided + match_budgets
// 改 spent)全部屬於 handicap 套件 —— 那些 SQL 只能有一個權威 —— 所以本套件
// 在這兩個動作上沒有任何自己的 UPDATE 可以順手掛上稽核。
//
// 其餘動作的稽核都由各自的寫入方法在同一個 tx 裡寫,不經這裡。
// 這個方法存在不代表可以改用它:有自己的 UPDATE 就掛在那句 SQL 的鄰居上,
// 分兩步寫等於讓「資料改了但稽核沒記上」變成一個能真實發生的狀態。
type JudgeAction struct {
	ActorUserID int64
	Action      string
	TargetType  string
	TargetID    int64
	// After 是動作之後的狀態快照,進 admin_audit_logs.after。
	After  json.RawMessage
	Reason string
}

// Repository 是本套件對儲存層的全部需求。
type Repository[TX any] interface {
	// InTx 開一個 transaction 執行 fn。fn 回錯即 rollback ——
	// 連同讓武封盤、下注派彩、outbox 事件一併消失。
	InTx(ctx context.Context, fn func(ctx context.Context, tx TX) error) error

	// LockMatch 讀一場比賽並鎖住該列(SELECT ... FOR UPDATE)。
	//
	// matches 列是整條生命週期的**序列化點**:裁判連點兩次「判定勝負」、
	// 或一邊開打一邊封盤,都在這裡排成序。與 handicap 用的是同一列鎖,
	// 所以「一邊買讓武一邊封盤」也一併被擋住。查無回 activityerr.ErrMatchNotFound。
	LockMatch(ctx context.Context, tx TX, matchPublicID string) (*Match, error)
	// LockMatchAt 依位置讀晉級目標並鎖住該列。查無回 activityerr.ErrMatchNotFound。
	//
	// 取鎖順序恆為 round 遞增(晉級只會往後走),不會與自己形成環。
	LockMatchAt(ctx context.Context, tx TX, tournamentID int64, round, slot int) (*Match, error)
	// GetMatch 無鎖重讀。只在本 tx 已經持有該列鎖之後使用
	// (封盤的 UPDATE 由 handicap 執行,回來要看到它寫下的樣子)。
	GetMatch(ctx context.Context, tx TX, matchPublicID string) (*Match, error)

	// TournamentByID 讀賽事,含 TotalRounds。查無回 tournament.ErrTournamentNotFound。
	TournamentByID(ctx context.Context, tx TX, tournamentID int64) (*Tournament, error)

	// LockPlayer 依 public_id 讀參賽者並鎖住該列。查無回 tournament.ErrPlayerNotFound。
	LockPlayer(ctx context.Context, tx TX, playerPublicID string) (*Player, error)

	// LockUnfinishedMatchesOfPlayer 列出這位選手所有**尚未完賽**的場次並鎖住那些列,
	// 依 (round, slot) 遞增排序。
	//
	// 排序是防死鎖的:兩位選手同時棄賽時,兩個 tx 會以同一個順序取鎖。
	// 回傳含對手未定的場次 —— 呼叫端要知道它們存在才判斷得出「這場還不能判」。
	LockUnfinishedMatchesOfPlayer(ctx context.Context, tx TX, playerID int64) ([]Match, error)

	// MarkReady 開盤,回傳更新後的場次。
	MarkReady(ctx context.Context, tx TX, w ReadyWrite) (*Match, error)
	// MarkLive 開打,回傳更新後的場次。
	MarkLive(ctx context.Context, tx TX, w LiveWrite) (*Match, error)
	// MarkFinished 判定勝負,回傳更新後的場次。
	MarkFinished(ctx context.Context, tx TX, w FinishWrite) (*Match, error)
	// SetStreamURL 設定或清除直播連結,回傳更新後的場次。
	SetStreamURL(ctx context.Context, tx TX, w StreamWrite) (*Match, error)
	// SeatPlayer 把晉級者寫進目標場次的一側,回傳更新後的場次。
	SeatPlayer(ctx context.Context, tx TX, w SeatWrite) (*Match, error)
	// SetPlayerStatus 改參賽者狀態,回傳更新後的選手。
	SetPlayerStatus(ctx context.Context, tx TX, w PlayerStatusWrite) (*Player, error)
	// BumpFencerRecord 增量更新跨屆戰績。
	BumpFencerRecord(ctx context.Context, tx TX, w FencerRecordWrite) error

	// ConfirmSetup 開賽前設定確認,回傳更新後的場次,並寫稽核。
	// 0 列時重讀分辨:ErrMatchNotFound / ErrNotLocked / ErrSetupAlreadyConfirmed。
	ConfirmSetup(ctx context.Context, tx TX, w SetupConfirmWrite) (*Match, error)

	// ListRounds 讀一場的全部回合,依 round_no 遞增。無鎖:寫入路徑已持有 matches 列鎖。
	// 回傳的 Round 只帶 WinnerPlayerID;WinnerPublicID 由呼叫端用 Match.ResolveRoundWinners 補
	// (那一支 query 不 JOIN 選手,翻譯需要的資料在場次上)。
	ListRounds(ctx context.Context, tx TX, matchID int64) ([]Round, error)
	// StartRound 建一列回合,回傳它,並寫稽核。同一回合重複建列回 ErrRoundInProgress。
	StartRound(ctx context.Context, tx TX, w RoundStartWrite) (*Round, error)
	// FinishRound 填該回合勝者,回傳更新後的回合,並寫稽核。
	// 0 列時重讀分辨:ErrRoundNotFound / ErrRoundAlreadyFinished。
	FinishRound(ctx context.Context, tx TX, w RoundFinishWrite) (*Round, error)

	// InsertThirdPlaceMatch 建季軍戰,回傳新場次。**不寫稽核**(判定勝負的後果)。
	InsertThirdPlaceMatch(ctx context.Context, tx TX, w ThirdPlaceWrite) (*Match, error)
	// FindThirdPlaceMatch 讀本屆的季軍戰;沒有時回 (nil, nil) —— 那不是錯誤。
	FindThirdPlaceMatch(ctx context.Context, tx TX, tournamentID int64) (*Match, error)

	// RecordViolation 記一筆違規並寫稽核。**不觸發任何後果**(schemas/27)。
	RecordViolation(ctx context.Context, tx TX, w ViolationWrite) (*Violation, error)
	// ListViolations 讀一場的全部違規,依發生順序。
	ListViolations(ctx context.Context, tx TX, matchID int64) ([]Violation, error)

	// RecordJudgeAction 寫一筆 admin_audit_logs,見 JudgeAction 的註解。
	RecordJudgeAction(ctx context.Context, tx TX, a JudgeAction) error

	// AppendEvents 與領域變更同 tx 寫 outbox。events 為空時不做事。
	AppendEvents(ctx context.Context, tx TX, events []Event) error
}
