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
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
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
	ActionStartMatch   = "match.start"
	ActionReportResult = "match.report_result"
	ActionSetStreamURL = "match.set_stream_url"
	ActionWithdraw     = "player.withdraw"
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

// Match 是一場比賽的完整狀態。
type Match struct {
	ID               int64
	PublicID         string
	TournamentID     int64
	Round            int
	Slot             int
	Status           Status
	ResultKind       ResultKind
	HandicapOpen     bool
	HandicapLockedAt *time.Time
	StreamURL        string
	StartedAt        *time.Time
	FinishedAt       *time.Time
	// WinnerPlayerID 0 = 尚未分出勝負。
	WinnerPlayerID int64
	P1             Player
	P2             Player
}

// BothSeated 回報雙方是否都已確定。開盤的前提。
func (m Match) BothSeated() bool { return m.P1.Seated() && m.P2.Seated() }

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

// JudgeAction 是一筆稽核紀錄。
//
// **只有封盤走這裡**。封盤對 matches 的那句 UPDATE 屬於 handicap 套件
// (matches 的封盤寫入只能有一個權威),所以本套件在那個動作上沒有任何
// 自己的 UPDATE 可以順手掛上稽核。其餘五個動作的稽核都由各自的寫入方法
// 在同一個 tx 裡寫,不經這裡 —— 這個方法存在不代表可以改用它。
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

	// RecordJudgeAction 寫一筆 admin_audit_logs,見 JudgeAction 的註解。
	RecordJudgeAction(ctx context.Context, tx TX, a JudgeAction) error

	// AppendEvents 與領域變更同 tx 寫 outbox。events 為空時不做事。
	AppendEvents(ctx context.Context, tx TX, events []Event) error
}
