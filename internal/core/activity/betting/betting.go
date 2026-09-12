// Package betting 是觀眾投票與下注(schemas/21)。
//
// # 為什麼是投票驅動而不是彩池
//
// 彩池(pari-mutuel)做不了串關:彩池的賠率要等封盤後才算得出來,而串關必須在
// 下注當下就知道每一腿的賠率才能乘起來。所以改成觀眾投票推導隱含機率,
// 賠率在下注當下鎖定寫進 bet_legs —— 之後票數再怎麼跑都不影響已成立的注單。
//
// # 這裡全部是真錢
//
// 下注動的是平台代幣,所以本套件的每一條規則都比照 ledger-invariants:
//
//   - 扣款(platform)與建注單(activity)必須同一個 transaction,
//     靠 Ledger 埠的 ApplyInTx 把帳本寫入掛進呼叫端的 tx。
//   - PlaceBet 一律要冪等鍵。下注沒有任何 UNIQUE 約束擋得住連點
//     (同一人可以對同一場下很多注,那是合法的),所以 API 冪等鍵是唯一權威。
//   - 派彩與退款都是**新的正數分錄**,絕不改舊紀錄。
//   - 金額與賠率全程 int64。賠率參與串關連乘,浮點誤差會直接乘進派彩金額,
//     所以賠率存 odds_milli(×1000)、抽水存 vig_bps(萬分之一)。見 odds.go。
//
// # 兩條操守規則
//
//  1. 選手不得對自己參與的場次下注。伺服器擋,不是前端隱藏。
//  2. 投票只回票數,**永不回傳誰投給誰**。votes.user_id 只用來擋灌票;
//     投票直接動賠率,公開投票人等於公開可操縱的標的。
//
// # 為什麼 TX 是型別參數
//
// core 依分層規範不能 import pgx(depguard 會擋),但下注的原子性又必須靠
// 真正的 transaction。把 tx 做成型別參數,讓 infrastructure 層用 pgx.Tx 實例化:
// core 一行 pgx 都沒有,實作端卻仍是靜態型別的 —— 不需要 any 與型別斷言,
// *ledgerpg.Service 直接就滿足 Ledger[pgx.Tx]。
package betting

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// Side 是場上的一方。數值刻意等於資料庫的 SMALLINT 與 proto 的欄位值。
type Side int16

const (
	// SideNone = 未投票 / 未指定。votes.side 有 CHECK,這個值只會出現在回應裡。
	SideNone Side = 0
	SideP1   Side = 1
	SideP2   Side = 2
)

// Valid 回報是否為合法的下注/投票方。
func (s Side) Valid() bool { return s == SideP1 || s == SideP2 }

// MatchStatus 是場次狀態。讓武與下注是兩條不同的時間線(見 migration 00001):
//
//	pending 對手未齊        下注 ✗
//	ready   雙方確定已開盤  下注 ✓
//	locked  讓武封盤已公開  下注 ✓
//	live    比賽中          下注 ✗
//	done    已分勝負        下注 ✗
type MatchStatus string

const (
	MatchPending MatchStatus = "pending"
	MatchReady   MatchStatus = "ready"
	MatchLocked  MatchStatus = "locked"
	MatchLive    MatchStatus = "live"
	MatchDone    MatchStatus = "done"
)

// OpenForBets 回報現在能不能對這場下注。
//
// 下注開到開賽前一刻(live 才關),與讓武選購(只在 ready 開)刻意不同:
// 讓武要早收,對手需要時間適應;下注沒有這個問題。
func (s MatchStatus) OpenForBets() bool { return s == MatchReady || s == MatchLocked }

// OpenForVotes 回報現在能不能投票。
//
// 與下注同窗口,理由是票數的**唯一**用途就是推導賠率:開賽後再改票不會影響
// 任何已成立的注單,只會讓「目前賠率」這個數字繼續飄,對誰都沒有好處。
func (s MatchStatus) OpenForVotes() bool { return s.OpenForBets() }

// ResultKind 是勝負的產生方式。
type ResultKind string

const (
	// ResultNormal 真的打完一場。
	ResultNormal ResultKind = "normal"
	// ResultWalkover 不戰而勝。賠率是按「真的打一場」算的,所以這種場次
	// 不該讓任何人贏錢 —— 走 VoidMatch 退款,不走 SettleMatch。
	ResultWalkover ResultKind = "walkover"
)

// BetStatus 是注單狀態。
type BetStatus string

const (
	BetOpen BetStatus = "open"
	BetWon  BetStatus = "won"
	BetLost BetStatus = "lost"
	// BetVoid 作廢退款:整場棄賽的單場注單,或每一腿都作廢的串關。
	BetVoid BetStatus = "void"
)

// LegResult 是串關單腿的結果。
type LegResult string

const (
	LegPending LegResult = "pending"
	LegWon     LegResult = "won"
	LegLost    LegResult = "lost"
	// LegVoid 該場不戰而勝,這一腿不成立 —— 從乘積中移除,
	// 剩餘腿仍用各自下注當下鎖定的 odds_milli 重算。
	LegVoid LegResult = "void"
)

// MaxLegsPerBet 是一張注單的腿數上限。
//
// schema 沒有這條約束,這是應用層的自我保護:串關腿數不設限等於讓單一請求
// 能拉進任意多場比賽的鎖,而 MAX_PARLAY 封頂之後多押幾腿對賠付也毫無意義。
const MaxLegsPerBet = 10

// Participant 是場上一方的身分快照。
//
// 兩個 user_id 欄位是刻意的,對應「選手不得賭自己」那條規則的兩條比對路徑:
//
//	PlayerUserID  tournament_players.user_id —— 純活動報名者為 0(NULL)
//	FencerUserID  fencers.user_id            —— 該劍士曾綁過平台帳號就有值
//
// 只比對前者會漏:沒綁平台帳號的選手用自己的平台帳號賭自己輸,前者比不到。
// 兩者都沒綁時擋不住,那是「純活動報名」的固有代價,只能靠裁判事後稽核。
// 用 0 而不是指標,是因為 0 不是合法的 id,而這個套件不做序列化。
type Participant struct {
	PlayerID     int64
	DisplayName  string
	PlayerUserID int64
	FencerUserID int64
}

// IsUser 回報某個平台帳號是不是這位選手本人。0 永遠回 false ——
// 「兩邊都沒綁帳號」不能被當成「所有人都是這位選手」。
func (p Participant) IsUser(userID int64) bool {
	if userID <= 0 {
		return false
	}
	return p.PlayerUserID == userID || p.FencerUserID == userID
}

// Match 是一場比賽在下注視角下的樣子。
type Match struct {
	ID           int64
	PublicID     string
	TournamentID int64
	Round        int
	Slot         int
	Status       MatchStatus
	ResultKind   ResultKind
	// WinnerPlayerID 0 = 尚未分出勝負。
	WinnerPlayerID int64
	P1             Participant
	P2             Participant
}

// WinningSide 回報勝方。0 = 還沒有勝者,或勝者不是場上兩人之一(資料異常)。
func (m Match) WinningSide() Side {
	switch m.WinnerPlayerID {
	case 0:
		return SideNone
	case m.P1.PlayerID:
		return SideP1
	case m.P2.PlayerID:
		return SideP2
	default:
		return SideNone
	}
}

// Participant 回傳指定一方的身分快照。
func (m Match) Participant(s Side) Participant {
	if s == SideP2 {
		return m.P2
	}
	return m.P1
}

// Tournament 是一屆賽事在下注視角下需要的部分。
type Tournament struct {
	ID   int64
	Slug string
}

// Tally 是一場比賽的票數。只有票數 —— 誰投的不在這裡,也不在任何回應裡。
type Tally struct {
	P1 int64
	P2 int64
}

// Total 是總票數。
func (t Tally) Total() int64 { return t.P1 + t.P2 }

// Votes 回傳指定一方的票數。
func (t Tally) Votes(s Side) int64 {
	if s == SideP2 {
		return t.P2
	}
	return t.P1
}

// MatchOdds 是一場比賽的即時賠率與票數。
type MatchOdds struct {
	MatchPublicID string
	P1Votes       int64
	P2Votes       int64
	P1OddsMilli   int64
	P2OddsMilli   int64
	OpenForBets   bool
	// MyVote 是**查詢者自己**投給誰(SideNone = 沒投或未登入)。
	// 這是唯一會離開伺服器的投票歸屬資訊,而且永遠只有自己那一筆。
	MyVote Side
}

// Leg 是注單的一腿。
//
// MatchPublicID / Round / Slot / SideDisplayName 是列注單時 join 出來的展示欄位,
// bet_legs 本身沒有這些欄位 —— 結算路徑用不到,實作可以留空。
type Leg struct {
	ID              int64
	BetID           int64
	MatchID         int64
	MatchPublicID   string
	Round           int
	Slot            int
	Side            Side
	SideDisplayName string
	// OddsMilli 是下注當下鎖定的賠率 ×1000。之後票數再跑都不改這個值。
	OddsMilli int64
	Result    LegResult
}

// Bet 是一張注單。
type Bet struct {
	ID       int64
	PublicID string
	// UserID 是下注者。下注要動平台代幣,所以這裡必定有值
	// (對比 tournament_players.user_id 允許 NULL:參賽不需帳號,動錢才需要)。
	UserID          int64
	TournamentID    int64
	Stake           int64
	PotentialPayout int64
	Status          BetStatus
	// PayoutRecalculated = 有腿因棄賽作廢而重算過 PotentialPayout。
	PayoutRecalculated bool
	// 稽核鏈:注單 ↔ 帳本雙向可查(0 = 還沒有那筆分錄)。
	// 不設 FK 是因為 token_entries 是按月分區表,分區表無法由 DB 保證全域唯一。
	LedgerStakeEntryID  int64
	LedgerRefundEntryID int64
	LedgerPayoutEntryID int64
	Legs                []Leg
	CreatedAt           time.Time
	// SettledAt nil = 尚未結算(對應 bets_settled_at_check:status='open' ⇔ NULL)。
	SettledAt *time.Time
}

// NewBet 是要寫入的注單。ledger_stake_entry_id 在扣款之後才補上 ——
// 分錄要指向注單、注單要指向分錄,兩邊的 id 不可能同時先有。
type NewBet struct {
	PublicID        string
	TournamentID    int64
	UserID          int64
	Stake           int64
	PotentialPayout int64
}

// NewLeg 是要寫入的一腿。
type NewLeg struct {
	MatchID   int64
	Side      Side
	OddsMilli int64
}

// BetUpdate 是結算/退款對注單的一次更新。
//
// 刻意沒有 settled_at 欄位:時間由資料庫的 now() 決定(status 離開 open 時寫入、
// 留在 open 時維持 NULL,對應 bets_settled_at_check)。core 不該有第二個時鐘。
type BetUpdate struct {
	BetID  int64
	Status BetStatus
	// PotentialPayout 非 nil = 有腿因棄賽 void 而重算過,實作必須同時把
	// payout_recalculated 設為 true —— 對帳時要看得出「這筆金額為什麼跟原始承諾不同」。
	PotentialPayout *int64
	// LedgerRefundEntryID 非 nil = 本次退款的分錄 id(稽核鏈)。
	LedgerRefundEntryID *int64
	// LedgerPayoutEntryID 非 nil = 本次派彩的分錄 id(稽核鏈)。
	//
	// 三種金流都存分錄 id,是為了讓「拿著注單跳到帳本」對三者都成立。
	// 派彩是金額最大、也最可能被質疑的一筆(「我明明中了為什麼沒收到」),
	// 偏偏原本是唯一需要從 token_entries 反查的 —— 那個不對稱在對帳時最傷。
	LedgerPayoutEntryID *int64
}

// IdempotencyRecord 是一把已存在的冪等鍵。
type IdempotencyRecord struct {
	RequestHash string
	// Response 為 nil 表示同一把鍵的前一個請求還沒完成(或已 rollback 消失)。
	Response json.RawMessage
}

// 這裡刻意沒有 outbox 事件。
//
// 曾經有過 bet.placed / bet.settled / bet.voided 三個 topic,拆掉的理由有兩層:
//
//  1. 沒有消費者。它們既不在 notification 的 Discord 清單也不在 in-process 保留清單,
//     而 outbox 的 pending 列**永不刪除**(CleanupOutboxEvents 只清 done/failed)——
//     發了就是永久累積。
//  2. 就算接上也不該接。它們是 per-bet 的:一場比賽結算會產生每張注單各一則,
//     發到公開頻道就是洗版。公開的那一則應該是**彙總**,而那屬於 match.finished
//     (它已經帶 settled_bet_count 與 voided_bet_count)。
//
// 日後若要做站內通知(對本人說「你中了 480」),事實已經完整記在 bets 與
// token_entries 裡,不必為此先發事件 —— 到時候再依那個功能的需要決定形狀。

// 錯誤語意:呼叫端據此決定回 4xx 還是重試。帳本層的錯誤
// (ledger.ErrInsufficientBalance / ErrIdempotencyConflict / ErrInFlight)
// 原樣上拋,不在這裡重複定義同義詞。
var (
	// ErrInvalidRequest 表示請求參數不合法(缺冪等鍵、stake ≤ 0、side 不是 1/2 等)。
	ErrInvalidRequest = errors.New("請求參數不合法")
	// ErrMatchNotInTournament 表示場次不屬於這一屆賽事。
	ErrMatchNotInTournament = errors.New("場次不屬於這屆賽事")
	// ErrVotingClosed 表示這場已經不能投票(未開盤或已開賽)。
	ErrVotingClosed = errors.New("這場已不開放投票")
	// ErrBettingClosed 表示這場已經不能下注(live 之後不行)。
	ErrBettingClosed = errors.New("這場已封盤")
	// ErrDuplicateLeg 表示同一張注單押了同一場兩次。
	// 押完 p1 再押 p2 等於穩賺,必須擋死(bet_legs 也有 UNIQUE 兜底)。
	ErrDuplicateLeg = errors.New("同一注單不可重複押同一場")
	// ErrTooManyLegs 表示串關腿數超過 MaxLegsPerBet。
	ErrTooManyLegs = errors.New("串關腿數過多")
	// ErrSelfBet 表示下注者是本場選手。
	ErrSelfBet = errors.New("選手不得對自己參與的場次下注")
	// ErrStakeTooLarge 表示超過單筆下注上限(platform 的 max_stake)。
	ErrStakeTooLarge = errors.New("超過單筆下注上限")
	// ErrOddsMoved 表示伺服器算出的賠付與客戶端預期差太多 ——
	// 使用者看到 5.0 倍按下去、送達時票數已經跑到 2.0 倍,這種單不該默默成立。
	ErrOddsMoved = errors.New("賠率已變動,請重新確認")
	// ErrMatchNotDecided 表示場次還沒分出勝負,不能結算。
	ErrMatchNotDecided = errors.New("場次尚未分出勝負")
	// ErrWalkoverMatch 表示這場是不戰而勝,要走 VoidMatch 而不是 SettleMatch。
	ErrWalkoverMatch = errors.New("不戰而勝的場次應走退款路徑")
	// ErrNotWalkover 表示這場不是不戰而勝,不該走 VoidMatch。
	ErrNotWalkover = errors.New("場次不是不戰而勝")
	// ErrLedgerStateConflict 表示注單紀錄與帳本狀態矛盾。
	// 異常狀態必須失敗出聲,不能回成功但錢不對。
	ErrLedgerStateConflict = errors.New("注單紀錄與帳本狀態矛盾")
)

// Repository 是下注需要的資料存取埠。TX 由實作端決定(infrastructure 傳 pgx.Tx)。
//
// 每個方法都收 tx:下注的原子性不是「盡量」而是硬需求,沒有一個寫入可以落在
// transaction 外面。讀取也走同一個 tx,因為結算時的判斷必須看到本 tx 內剛寫下的腿。
type Repository[TX any] interface {
	// InTx 開一個 transaction 執行 fn。fn 回錯即 rollback(含帳本寫入一併消失)。
	InTx(ctx context.Context, fn func(ctx context.Context, tx TX) error) error

	// LockMatchSettle 取 pg_advisory_xact_lock(hashtext('match_settle'), matchID),
	// 防同一場被同時結算兩次。鎖隨 tx 結束自動釋放,不需要也不該手動解鎖。
	LockMatchSettle(ctx context.Context, tx TX, matchID int64) error
	// LockUser 鎖住使用者列,把同一人的下注串行化 ——
	// 冪等重放的判定要在前一筆同鍵請求 commit 之後才有意義。
	LockUser(ctx context.Context, tx TX, userID int64) error
	// LockBets 依 betIDs **升冪**取列鎖。呼叫端已排序;實作不得改順序。
	// 存在的理由見 Service.settleLegs:同一張串關的兩腿分屬兩場比賽時,
	// 兩場各自的 advisory lock 擋不住彼此,只有注單列鎖擋得住。
	LockBets(ctx context.Context, tx TX, betIDs []int64) error

	// TournamentBySlug 找不到回 (nil, nil) —— 由 Service 轉成 tournament.ErrTournamentNotFound,
	// 實作端不必知道 core 的錯誤語意。
	TournamentBySlug(ctx context.Context, tx TX, slug string) (*Tournament, error)
	// OddsConfig 讀 tournaments.config 的賠率參數。缺值可回零值,由 Normalize 補預設。
	OddsConfig(ctx context.Context, tx TX, tournamentID int64) (OddsConfig, error)
	// MatchesByPublicIDs 批次讀場次,回傳順序不拘(呼叫端自行索引)。
	MatchesByPublicIDs(ctx context.Context, tx TX, publicIDs []string) ([]Match, error)

	// VoteTallies 數票:每場每邊各幾票。
	VoteTallies(ctx context.Context, tx TX, matchIDs []int64) (map[int64]Tally, error)
	// UpsertVote 一場一票,改票即覆蓋(votes 的 UNIQUE(match_id,user_id) 是權威)。
	UpsertVote(ctx context.Context, tx TX, matchID, userID int64, side Side) error
	// MyVotes 只查 userID 自己的票。**不要**加一個查別人的版本。
	MyVotes(ctx context.Context, tx TX, matchIDs []int64, userID int64) (map[int64]Side, error)

	// GetIdempotency 查不到回 (nil, nil)。
	GetIdempotency(ctx context.Context, tx TX, key string) (*IdempotencyRecord, error)
	// InsertIdempotency 佔鍵(response 留 NULL)。
	InsertIdempotency(ctx context.Context, tx TX, key, requestHash string) error
	// SaveIdempotencyResponse 在 commit 前存入完整結果:commit 成功 ⇔ 鍵帶著結果存在。
	SaveIdempotencyResponse(ctx context.Context, tx TX, key string, response []byte) error

	// InsertBet 回傳新注單的內部 id 與資料庫寫下的 created_at。
	// 時間由資料庫給,core 不該有第二個時鐘。
	InsertBet(ctx context.Context, tx TX, b NewBet) (int64, time.Time, error)
	// InsertLegs 寫入全部腿。
	InsertLegs(ctx context.Context, tx TX, betID int64, legs []NewLeg) error
	// SetBetStakeEntry 補上扣款分錄 id(稽核鏈:注單 ↔ 帳本雙向可查)。
	SetBetStakeEntry(ctx context.Context, tx TX, betID, entryID int64) error

	// PendingLegsByMatch 撈出所有押到這場、還沒判定的腿。
	PendingLegsByMatch(ctx context.Context, tx TX, matchID int64) ([]Leg, error)
	// UpdateLegResults 把指定的腿標成同一個結果。legIDs 為空時不做事。
	UpdateLegResults(ctx context.Context, tx TX, legIDs []int64, result LegResult) error
	// BetsByIDs 讀注單本體(不含腿)。
	BetsByIDs(ctx context.Context, tx TX, betIDs []int64) ([]Bet, error)
	// LegsByBetIDs 讀這些注單的全部腿(含已判定的)。
	LegsByBetIDs(ctx context.Context, tx TX, betIDs []int64) (map[int64][]Leg, error)
	// UpdateBet 套用結算結果。
	UpdateBet(ctx context.Context, tx TX, u BetUpdate) error

	// BetsByUser 列某人在某屆的注單(含腿),依 created_at 由新到舊。
	BetsByUser(ctx context.Context, tx TX, tournamentID, userID int64, openOnly bool) ([]Bet, error)
}

// Ledger 是本套件對帳本的最小依賴:與注單寫入同 tx 動錢。
//
// 實際注入 *ledgerpg.Service。ApplyInTx 刻意不在 ledger.Ledger interface 上
// (那個 interface 要保持可攜,未來的 HTTP 版帳本沒有 tx 可傳),所以需要同 tx
// 組合的呼叫端自己宣告這個最小介面 —— 與 shoppg / xppg / marketpg 同一個模式。
//
// 三個性質決定了本套件的寫法:
//  1. 原子性由呼叫端決定:我們 rollback,動錢一併消失。
//  2. 失敗用 savepoint 隔離,不會毒化我們的 tx。
//  3. 失敗後**餘額列的鎖會被釋放**(列鎖寫在 tuple 的 xmax,子交易 abort
//     會讓那個 subxid 失效,別的 session 就看不到那把鎖了)。
//
// 第 3 點原本寫的是相反的,2026-09-12 對真 Postgres 實測推翻 ——
// 行為釘在 bettingpg 的 TestApplyInTxContract。但**動錢排在流程最後這個寫法不變**:
// 理由不需要靠那個(已被推翻的)機制,見 placeBetInTx 的註解。
type Ledger[TX any] interface {
	ApplyInTx(ctx context.Context, tx TX, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}
