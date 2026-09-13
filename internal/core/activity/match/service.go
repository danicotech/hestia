package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/bracket"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// Service 是六個裁判動作的編排。TX 由 infrastructure 實例化成 pgx.Tx。
//
// 每個動作都是「讀場次取列鎖 → 檢查閘門 → 寫領域變更 → 連帶(讓武 / 下注 /
// 晉級 / 戰績)→ 發事件」,而且整段跑在同一個 transaction 裡。
// 順序不是風格問題:結算要讀得到剛寫下的勝者,事件要與變更同生共死。
type Service[TX any] struct {
	repo Repository[TX]
	hcap Handicaps[TX]
	bets Bets[TX]
}

// NewService 建立服務。三個相依都是必要的 —— 沒有讓武就不能開盤封盤,
// 沒有下注就不能判勝負(判了不結算等於把錢留在系統裡沒有人負責)。
func NewService[TX any](repo Repository[TX], hcap Handicaps[TX], bets Bets[TX]) *Service[TX] {
	return &Service[TX]{repo: repo, hcap: hcap, bets: bets}
}

// actor 是執行動作的裁判。內部型別,只為了不讓每個 helper 都多兩個參數。
type actor struct {
	userID int64
	reason string
}

// ── 請求與結果 ──────────────────────────────────────────────────

// OpenHandicapParams 是開盤:把場次從 pending 推到 ready,並依段位差發 BP。
type OpenHandicapParams struct {
	MatchPublicID string
	ActorUserID   int64
	// Reason 進稽核紀錄。開盤不是破壞性動作,所以允許留空。
	Reason string
}

// OpenHandicapResult 是開盤的結果。
type OpenHandicapResult struct {
	Match Match
	// Budget 為 nil 表示**本場無讓武**(同段對決)。
	// 這不是失敗:有沒有讓武在資料上就該分得出來,不是靠 budget = 0 去解讀。
	Budget *handicap.Budget
	// Markets 是同一個 tx 內建好的盤口(config.betting.markets);本屆不開放下注時為空。
	Markets []betting.Market
}

// LockHandicapParams 是封盤。**不可逆**。
type LockHandicapParams struct {
	MatchPublicID string
	// Confirm 必須為 true。封盤後選手不能再改,而且立刻公開並發 Discord 公告。
	Confirm     bool
	ActorUserID int64
	Reason      string
}

// LockHandicapResult 是封盤的結果。
type LockHandicapResult struct {
	Match Match
	// Handicaps 是封盤當下的完整清單,即公告發出去的內容。
	Handicaps *handicap.MatchHandicaps
}

// StartMatchParams 是標記開打。這一步同時關閉下注。
type StartMatchParams struct {
	MatchPublicID string
	ActorUserID   int64
	Reason        string
}

// ReportResultParams 是判定勝負。**不可逆**:會觸發晉級與派彩。
type ReportResultParams struct {
	MatchPublicID        string
	WinnerPlayerPublicID string
	Confirm              bool
	ActorUserID          int64
	// Reason 是裁決理由。爭議場次尤其要寫 —— 它是日後唯一查得到的說法。
	Reason string
}

// ReportResultOutcome 是判定勝負的完整結果。
type ReportResultOutcome struct {
	Match Match
	// AdvancedMatches 是因此而成形的後續場次:勝者被寫進去的那一場,
	// 以及(對手早已棄賽時)被連帶判成不戰而勝的場次。
	AdvancedMatches []Match
	// ThirdPlaceMatch 非 nil = 這是第二場判完的準決賽,季軍戰在同一個 tx 內成形了
	// (config.format.third_place_match)。它之後走與其他場次完全相同的流程。
	ThirdPlaceMatch *Match
	// SettledBetCount 是本次結算影響的注單數,給裁判「確實發生了」的回饋。
	SettledBetCount int
	// VoidedBetCount 是連帶的不戰而勝場次退掉的注單數。正常情況為 0。
	VoidedBetCount int
	// ChampionPlayerPublicID 非空 = 這是決賽,勝者即冠軍。
	ChampionPlayerPublicID string
}

// WithdrawPlayerParams 是選手棄賽。**不可逆**:對手不戰而勝、注單全額退款。
type WithdrawPlayerParams struct {
	PlayerPublicID string
	Confirm        bool
	ActorUserID    int64
	Reason         string
}

// WithdrawResult 是棄賽的結果。
type WithdrawResult struct {
	Player Player
	// WalkoverMatches 是因此被判不戰而勝的場次(含連鎖判定的後續輪次)。
	WalkoverMatches []Match
	// AdvancedMatches 是對手照常晉級後成形、但仍在等待的場次。
	AdvancedMatches []Match
	// ThirdPlaceMatch 非 nil = 這次不戰而勝判掉的是第二場準決賽,季軍戰因此成形。
	ThirdPlaceMatch *Match
	// VoidedBetCount 是因此作廢退款的注單數。
	VoidedBetCount int
	// ChampionPlayerPublicID 非空 = 棄賽發生在決賽,對手不戰而勝奪冠。
	ChampionPlayerPublicID string
}

// SetStreamURLParams 是設定直播連結。StreamURL 為空字串 = 清掉。
type SetStreamURLParams struct {
	MatchPublicID string
	StreamURL     string
	ActorUserID   int64
	Reason        string
}

// ── 開盤 ────────────────────────────────────────────────────────

// OpenHandicap 開盤:pending → ready、handicap_open = true,並依段位差發 BP。
//
// 發 BP 與改狀態必須同一個 tx:少了前者,選手看到「可以選讓武」卻沒有預算;
// 少了後者,預算發下去卻買不了東西。兩種都要人工修。
//
// 重複開盤回 ErrAlreadyOpen 而不是默默成功 —— 開盤會發提醒公告,
// 默默成功等於每呼叫一次就再吵一次低段位者。
func (s *Service[TX]) OpenHandicap(ctx context.Context, p OpenHandicapParams) (*OpenHandicapResult, error) {
	if p.MatchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if p.ActorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}

	var out *OpenHandicapResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, ec, err := s.load(ctx, tx, p.MatchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		switch {
		case m.Status == StatusDone:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		case m.Status != StatusPending:
			// ready / locked / live 都是「已經開過盤」。分得更細沒有意義:
			// 對裁判來說要做的事都一樣 —— 這個按鈕已經按過了。
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrAlreadyOpen)
		case !m.BothSeated():
			return fmt.Errorf("match=%s: %w", m.PublicID, activityerr.ErrPlayersNotSet)
		case m.P1.Status == tournament.PlayerWithdrawn || m.P2.Status == tournament.PlayerWithdrawn:
			// 對已棄賽的一方開盤,發下去的 BP 永遠花不掉,而且會發一則
			// 沒有意義的提醒。這種場次該走 WithdrawPlayer 判不戰而勝。
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrPlayerWithdrawn)
		}

		opened, err := s.repo.MarkReady(ctx, tx, ReadyWrite{
			MatchID: m.ID, ActorUserID: p.ActorUserID, Reason: p.Reason,
		})
		if err != nil {
			return fmt.Errorf("開盤 match=%s: %w", m.PublicID, err)
		}

		budget, err := s.hcap.GrantBudgetInTx(ctx, tx, m.PublicID)
		if err != nil {
			return fmt.Errorf("發 BP 預算 match=%s: %w", m.PublicID, err)
		}

		// 盤口在 ready 時建(schemas/21):下注視窗與讓武選購同一刻打開。
		// 與改狀態同一個 tx —— 少了它,觀眾看到「可以下注」卻沒有任何盤口可押。
		markets, err := s.bets.OpenMarketsInTx(ctx, tx, m.PublicID)
		if err != nil {
			return fmt.Errorf("建盤口 match=%s: %w", m.PublicID, err)
		}

		ev := ec.base(opened)
		ev.Handicap = ec.budgetInfo(opened, budget)
		if err := s.repo.AppendEvents(ctx, tx, []Event{event(TopicHandicapOpened, ev)}); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}
		out = &OpenHandicapResult{Match: *opened, Budget: budget, Markets: markets}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 封盤 ────────────────────────────────────────────────────────

// lockAudit 是封盤寫進 admin_audit_logs.after 的內容。
//
// 記的是**公示出去的那份清單**而不是「封盤了」三個字:日後有人質疑
// 「我明明沒買這一項」時,要查的就是這裡。
type lockAudit struct {
	Status   Status        `json:"status"`
	LockedAt *time.Time    `json:"handicap_locked_at,omitempty"`
	Handicap *HandicapInfo `json:"handicap,omitempty"`
}

// LockHandicap 封盤。**不可逆**,要求 confirm = true。
//
// matches 的封盤 UPDATE 由 handicap 套件執行(那句 SQL 只能有一個權威),
// 本套件負責的是閘門、稽核與公告。公告 payload 帶完整讓武清單 ——
// 那是對手與觀眾在這一刻唯一的資訊來源,renderer 不該再回頭查一次。
func (s *Service[TX]) LockHandicap(ctx context.Context, p LockHandicapParams) (*LockHandicapResult, error) {
	if p.MatchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if p.ActorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}
	if !p.Confirm {
		return nil, fmt.Errorf("封盤後選手不能再更動讓武: %w", ErrConfirmationRequired)
	}

	var out *LockHandicapResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, ec, err := s.load(ctx, tx, p.MatchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		switch {
		case m.Status == StatusDone:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		case m.Status == StatusLocked, m.Status == StatusLive:
			// 重複封盤用讓武套件的錯誤,不在這裡造一個同義詞:
			// 「已經封盤」的權威語意屬於 handicap,transport 只該認一套。
			return fmt.Errorf("match=%s: %w", m.PublicID, handicap.ErrAlreadyLocked)
		case m.Status != StatusReady:
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotReady)
		}

		view, err := s.hcap.LockInTx(ctx, tx, m.PublicID)
		if err != nil {
			// handicap.ErrAlreadyLocked 等原樣上拋:那些錯誤的語意在讓武套件
			// 已經定義過,在這裡再包一層同義詞只會讓 transport 要認兩套。
			return fmt.Errorf("封盤 match=%s: %w", m.PublicID, err)
		}

		// 重讀:剛才那句 UPDATE 是讓武套件下的,本 tx 手上的副本已經過時。
		locked, err := s.repo.GetMatch(ctx, tx, m.PublicID)
		if err != nil {
			return fmt.Errorf("重讀場次 match=%s: %w", m.PublicID, err)
		}

		info := ec.lockedInfo(locked, view)
		after, _ := json.Marshal(lockAudit{
			Status: locked.Status, LockedAt: locked.HandicapLockedAt, Handicap: info,
		})
		if err := s.repo.RecordJudgeAction(ctx, tx, JudgeAction{
			ActorUserID: p.ActorUserID,
			Action:      ActionLockHandicap,
			TargetType:  AuditTargetMatch,
			TargetID:    locked.ID,
			After:       after,
			Reason:      p.Reason,
		}); err != nil {
			return fmt.Errorf("寫稽核紀錄: %w", err)
		}

		ev := ec.base(locked)
		ev.Handicap = info
		if err := s.repo.AppendEvents(ctx, tx, []Event{event(TopicHandicapLocked, ev)}); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}
		out = &LockHandicapResult{Match: *locked, Handicaps: view}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 裁判代退讓武 ────────────────────────────────────────────────

// RefundSelectionParams 是裁判退掉某一筆讓武選擇。
type RefundSelectionParams struct {
	// SelectionPublicID 指名要退哪一筆。對外只出現 public_id(鐵則 5)。
	SelectionPublicID string
	ActorUserID       int64
	// Reason **必填**,與其餘動作不同。
	//
	// 這一步拿走的是選手已經買到手的東西,而它唯一的正當理由來自目錄裡的規則
	// (如「禁用奇術」的 referee_note)。不寫理由的退點,事後沒有人分得出
	// 「裁判依規則返還」與「裁判把不喜歡的項目刪掉」—— 而那正是選手會質疑的事。
	//
	// 不可逆動作用的 confirm 這裡沒有,因為退點不是不可逆:封盤前選手能拿
	// 退回來的 BP 重買同一項。要求寫理由已經足以擋住誤觸。
	Reason string
}

// RefundSelectionResult 是裁判代退的結果。
type RefundSelectionResult struct {
	MatchPublicID string
	// Selection 是被退掉的那一筆(Voided 已為 true)。
	Selection handicap.Selection
	// Budget 是退款後的預算,BP 已經回到裡面。
	Budget handicap.Budget
}

// refundAudit 是代退寫進 admin_audit_logs.after 的內容。
//
// 記的是「退了哪一筆、值多少 BP、退完剩多少」而不是「退了一筆」:
// 日後選手質疑「我的 BP 為什麼少了/多了」時,要查的就是這裡。
type refundAudit struct {
	MatchPublicID     string `json:"match_public_id"`
	SelectionPublicID string `json:"selection_public_id"`
	PlayerPublicID    string `json:"player_public_id"`
	ItemName          string `json:"item_name"`
	RefundedBP        int64  `json:"refunded_bp"`
	Spent             int64  `json:"spent"`
	Remaining         int64  `json:"remaining"`
}

// RefundSelection 退掉指定的一筆讓武選擇,BP 回到該場預算。
//
// # 為什麼需要它
//
// 目錄裡的規則自己要求:「禁用奇術」的 referee_note 寫著「對手全場無法使用任何
// 奇術,若同時買了禁用任意奇術,將經裁判確認後返還禁用任意奇術的 BP」。
// 選手自助的 VoidSelection 只認本人,裁判走不了那條路,所以那句規則在這支 RPC
// 出現之前是**執行不了的**。
//
// # 封盤後一律拒絕
//
// 閘門在讓武套件裡(handicap.ErrHandicapLocked),本套件不重寫一次,也不在上面
// 補一個「裁判例外」。封盤後對手已經照著那份公開清單準備了,這時候抽掉一項
// 等於偷偷改了比賽條件 —— 裁判要更正只能走賽後裁決,不是回頭改資料。
//
// # BP 不經 Ledger
//
// 退的是 BP 不是平台代幣:標 voided + 改 spent 就是全部,沒有分錄、沒有冪等鍵
// (理由見 handicap 套件說明)。所以這裡也沒有 IdempotencyKey 欄位 ——
// 重複呼叫由 handicap.ErrSelectionAlreadyVoided 擋,不是靠鍵去重。
func (s *Service[TX]) RefundSelection(ctx context.Context, p RefundSelectionParams) (*RefundSelectionResult, error) {
	if p.SelectionPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 selection_public_id", ErrInvalidRequest)
	}
	if p.ActorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: 代退必須寫明理由", ErrInvalidRequest)
	}

	var out *RefundSelectionResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		// 讓武套件自己會依 selection 找到場次、取該列的鎖、檢查封盤。
		// 這裡刻意不先 LockMatch 一次:請求只帶 selection_public_id,
		// 要先鎖就得先查一次它屬於哪一場,而那份「哪一場」的權威已經在
		// voidSelection 的鎖後重讀裡了 —— 在這裡再查一次只會多一個
		// 會過期的副本,以及一個與它不同步的第二版封盤判斷。
		res, err := s.hcap.RefundSelectionInTx(ctx, tx, p.SelectionPublicID)
		if err != nil {
			return fmt.Errorf("代退讓武 selection=%s: %w", p.SelectionPublicID, err)
		}

		after, _ := json.Marshal(refundAudit{
			MatchPublicID:     res.MatchPublicID,
			SelectionPublicID: res.Selection.PublicID,
			PlayerPublicID:    res.Budget.PlayerPublicID,
			ItemName:          res.Selection.ItemName,
			RefundedBP:        res.Selection.Cost,
			Spent:             res.Budget.Spent,
			Remaining:         res.Budget.Remaining(),
		})
		// target 是**場次**而不是選擇:爭議發生時人問的是「這一場裁判動過什麼」,
		// 而 admin_audit_logs 沒有 activity.handicap_selection 這個 target_type。
		// 退了哪一筆寫在 after 裡,查得到。
		if err := s.repo.RecordJudgeAction(ctx, tx, JudgeAction{
			ActorUserID: p.ActorUserID,
			Action:      ActionRefundSelection,
			TargetType:  AuditTargetMatch,
			TargetID:    res.MatchID,
			After:       after,
			Reason:      reason,
		}); err != nil {
			return fmt.Errorf("寫稽核紀錄: %w", err)
		}

		// 沒有 outbox 事件:封盤前的讓武內容只有施加者與裁判看得到,
		// 發一則公告等於把「他買了什麼」廣播出去,那正是封盤前要守住的東西。
		out = &RefundSelectionResult{
			MatchPublicID: res.MatchPublicID,
			Selection:     res.Selection,
			Budget:        res.Budget,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 開打 ────────────────────────────────────────────────────────

// StartMatch 標記開打 = **開始第一回合**(2026-09-13 起)。
//
// matches.started_at 的定義就是「第一回合的正式決鬥開始」(schemas/20),所以這支
// 與 StartRound 是同一個動作,不是兩個:它只是回傳形狀不同(舊契約只要場次)。
// 閘門(必須 locked、必須已做設定確認)、關盤、建回合列、稽核、推播全在 StartRound。
func (s *Service[TX]) StartMatch(ctx context.Context, p StartMatchParams) (*Match, error) {
	rs, err := s.startRound(ctx, p.MatchPublicID, p.ActorUserID, p.Reason)
	if err != nil {
		return nil, err
	}
	return &rs.Match, nil
}

// ── 判定勝負 ────────────────────────────────────────────────────

// ReportResult 一次判定整場勝負。**不可逆**,要求 confirm = true。
//
// 只在 best_of = 1 時可用;多回合制回 ErrMultiRoundMatch —— 整場勝者是回合推導的,
// 裁判要逐回合 FinishRound。單場定勝負時它走的也是**同一條回合路徑**:
// 等價於「開始第 1 回合(若還沒有)→ 以該勝者結束第 1 回合」,所以回合列一定存在,
// 整場勝者一樣是重算核對後才寫。
//
// 一次 tx 裡發生的事,少任何一件都要人工修資料庫:
//
//	填回合勝者 → 結算單回合盤口 → 寫 winner / status='done' / finished_at
//	→ 敗方標 eliminated → 雙方跨屆戰績 ±1 → 整場盤口結算派彩 → 勝者晉級
//	→ 季軍戰檢查 → 賽果公告 →(決賽時)冠軍公告
//
// 結算刻意排在寫 matches 之後:betting 要讀到的是我們剛寫下的 done + winner。
func (s *Service[TX]) ReportResult(ctx context.Context, p ReportResultParams) (*ReportResultOutcome, error) {
	switch {
	case p.MatchPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	case p.WinnerPlayerPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 winner_player_public_id", ErrInvalidRequest)
	case p.ActorUserID <= 0:
		return nil, tournament.ErrActorRequired
	case !p.Confirm:
		return nil, fmt.Errorf("判定勝負會觸發晉級與派彩: %w", ErrConfirmationRequired)
	}

	var out *ReportResultOutcome
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, ec, err := s.load(ctx, tx, p.MatchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		switch {
		case m.Status == StatusDone:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		case m.Status != StatusLive:
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotLive)
		case ec.cfg.Format.BestOf > 1:
			return fmt.Errorf("match=%s best_of=%d: %w", m.PublicID, ec.cfg.Format.BestOf, ErrMultiRoundMatch)
		}
		winner := m.PlayerByPublicID(p.WinnerPlayerPublicID)
		if !winner.Seated() {
			return fmt.Errorf("match=%s winner=%s: %w",
				m.PublicID, p.WinnerPlayerPublicID, ErrWinnerNotInMatch)
		}
		act := actor{userID: p.ActorUserID, reason: p.Reason}

		rounds, err := s.rounds(ctx, tx, m)
		if err != nil {
			return err
		}
		var current Round
		switch {
		case len(rounds) == 0:
			// 場次已經 live 卻沒有回合列:第一回合的列就在這裡建。
			// 這條路只在「開打」與「判定」是同一次動作時走到(單場定勝負的裁判
			// 不需要分開按),回合的起訖因此同一瞬間 —— 那正確反映了裁判做的事。
			r, err := s.repo.StartRound(ctx, tx, RoundStartWrite{
				MatchID: m.ID, RoundNo: 1, ActorUserID: act.userID, Reason: act.reason,
			})
			if err != nil {
				return fmt.Errorf("建第 1 回合 match=%s: %w", m.PublicID, err)
			}
			current = *r
		case !rounds[len(rounds)-1].Finished():
			current = rounds[len(rounds)-1]
		default:
			// 單場定勝負而回合已經結束、場次卻還是 live:回合推導得出勝者但沒寫進場次,
			// 那是不一致,不能在這裡再結束一次回合。
			return fmt.Errorf("match=%s: 回合已結束但場次未定案: %w", m.PublicID, ErrRoundsInconsistent)
		}

		res, err := s.finishRound(ctx, tx, ec, m, current.RoundNo, winner, act)
		if err != nil {
			return err
		}
		if !res.Decided {
			// best_of = 1 時一回合就定案;走到這裡代表 WinsNeeded 算出來不是 1。
			return fmt.Errorf("match=%s: 單場定勝負卻未定案: %w", m.PublicID, ErrRoundsInconsistent)
		}
		out = res.Outcome
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 棄賽 ────────────────────────────────────────────────────────

// WithdrawPlayer 選手棄賽。**不可逆**,要求 confirm = true。
//
// 三件連帶:
//
//	選手標 withdrawn → 所有「對手已確定」的未完賽場次判對手不戰而勝
//	→ 每一場全額退款(賠率是按真的打一場算的,不戰而勝不該讓任何人贏錢)
//
// 棄賽者每被判一場就 losses +1(schemas/26 待確認 ④:戰績要誠實反映結果,
// 生涯頁再用 result_kind 區分顯示)。口徑與 RecalcFencerStats 一致 ——
// 那支對帳 SQL 數的就是「自己有上場、已分勝負、而勝者不是自己」的場次。
//
// 刻意**不設階段閘門**:報名期、評段期都可能有人退出,而抽籤前棄賽
// 根本沒有場次要判。對手未確定的場次也刻意不動 —— 那一場要等勝者產生,
// 到時候由晉級鏈接手判成不戰而勝(見 resolve)。
//
// # 它同時是「鎖住這個人」的手段
//
// 2026-09-13 起選手登入只要遊戲ID,沒有密碼可換,所以把一個人擋在系統外面的
// 唯一槓桿就是他的狀態 —— signup.Service.Login 只放行 status = active。
// 標成 withdrawn 的那一刻,這個遊戲ID 就再也登不進來了。
//
// 所以這支不只影響賽程:對「有人冒用某個遊戲ID」的檢舉,棄賽就是處置本身 ——
// 它同時擋住之後的登入,並讓對方現在還活著的 session 當場失效。
// 機制見 internal/core/activity/session 套件註解的「撤銷」一節。
func (s *Service[TX]) WithdrawPlayer(ctx context.Context, p WithdrawPlayerParams) (*WithdrawResult, error) {
	switch {
	case p.PlayerPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 player_public_id", ErrInvalidRequest)
	case p.ActorUserID <= 0:
		return nil, tournament.ErrActorRequired
	case !p.Confirm:
		return nil, fmt.Errorf("棄賽會判對手不戰而勝並退掉該場注單: %w", ErrConfirmationRequired)
	}

	var out *WithdrawResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		pl, err := s.repo.LockPlayer(ctx, tx, p.PlayerPublicID)
		if err != nil {
			return fmt.Errorf("讀選手 %s: %w", p.PlayerPublicID, err)
		}
		if pl.Status == tournament.PlayerWithdrawn {
			return fmt.Errorf("player=%s: %w", pl.PublicID, ErrAlreadyWithdrawn)
		}
		t, err := s.repo.TournamentByID(ctx, tx, pl.TournamentID)
		if err != nil {
			return fmt.Errorf("讀賽事 id=%d: %w", pl.TournamentID, err)
		}
		ec := newEventCtx(t)

		// 先標狀態再讀場次:順序反過來的話,讀到的場次快照裡他還是 active,
		// 判定時就會把他從 withdrawn 覆寫成 eliminated,
		// 「他是自己退出的」這件事便從資料上消失了。
		withdrawn, err := s.repo.SetPlayerStatus(ctx, tx, PlayerStatusWrite{
			PlayerID:    pl.ID,
			Status:      tournament.PlayerWithdrawn,
			ActorUserID: p.ActorUserID,
			Reason:      p.Reason,
		})
		if err != nil {
			return fmt.Errorf("標記棄賽 player=%s: %w", pl.PublicID, err)
		}

		pending, err := s.repo.LockUnfinishedMatchesOfPlayer(ctx, tx, pl.ID)
		if err != nil {
			return fmt.Errorf("讀未完賽場次 player=%s: %w", pl.PublicID, err)
		}

		act := actor{userID: p.ActorUserID, reason: p.Reason}
		res := &resolution{}
		handled := make(map[int64]bool, len(pending))
		for i := range pending {
			m := &pending[i]
			if handled[m.ID] {
				continue // 已經被上一場的晉級鏈判掉了
			}
			opp := m.Opponent(pl.ID)
			if !opp.Seated() {
				// 對手還沒產生。留著等上一輪打完,由 resolve 的晉級鏈接手 ——
				// 現在判不出勝者(matches_winner_is_participant_check 也不允許)。
				continue
			}
			before := len(res.concluded)
			if err := s.resolve(ctx, tx, ec, m, opp, *withdrawn, ResultWalkover, act, res); err != nil {
				return err
			}
			for _, c := range res.concluded[before:] {
				handled[c.ID] = true
			}
		}

		if err := s.repo.AppendEvents(ctx, tx, res.events); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}
		out = &WithdrawResult{
			Player:          *withdrawn,
			WalkoverMatches: res.concluded,
			AdvancedMatches: res.seated,
			ThirdPlaceMatch: res.thirdPlace,
			VoidedBetCount:  res.voided,
		}
		if res.champion != nil {
			out.ChampionPlayerPublicID = res.champion.PublicID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 直播連結 ────────────────────────────────────────────────────

// SetStreamURL 設定直播連結;空字串 = 清掉。
//
// 這是六個動作裡唯一可逆的一個:不要求 confirm,也不發公告
// (連結會變、會補、會打錯,每改一次就廣播一次只會變成洗頻)。
// finished 階段仍然允許設定 —— 賽後補上 VOD 連結是常態。
func (s *Service[TX]) SetStreamURL(ctx context.Context, p SetStreamURLParams) (*Match, error) {
	if p.MatchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if p.ActorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}

	var out *Match
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, _, err := s.load(ctx, tx, p.MatchPublicID,
			tournament.PhaseInProgress, tournament.PhaseFinished)
		if err != nil {
			return err
		}
		updated, err := s.repo.SetStreamURL(ctx, tx, StreamWrite{
			MatchID:     m.ID,
			StreamURL:   p.StreamURL,
			ActorUserID: p.ActorUserID,
			Reason:      p.Reason,
		})
		if err != nil {
			return fmt.Errorf("設定直播連結 match=%s: %w", m.PublicID, err)
		}
		out = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 內部:載入與判定 ────────────────────────────────────────────

// load 取場次列鎖、讀賽事、驗階段,並備好組公告要用的上下文。
//
// 五個動作的開頭都一樣,所以收斂在這裡:漏掉階段檢查的那一個動作
// 不會自己冒出來,它只會在某天被人用來在報名期改一場不存在的比賽。
func (s *Service[TX]) load(ctx context.Context, tx TX, matchPublicID string, allowed ...tournament.Phase) (*Match, eventCtx, error) {
	m, err := s.repo.LockMatch(ctx, tx, matchPublicID)
	if err != nil {
		return nil, eventCtx{}, fmt.Errorf("讀場次 %s: %w", matchPublicID, err)
	}
	t, err := s.repo.TournamentByID(ctx, tx, m.TournamentID)
	if err != nil {
		return nil, eventCtx{}, fmt.Errorf("讀賽事 id=%d: %w", m.TournamentID, err)
	}
	if err := tournament.RequirePhase(t.Phase, allowed...); err != nil {
		return nil, eventCtx{}, err
	}
	return m, newEventCtx(t), nil
}

// resolution 是一次判定連鎖的累積結果。
//
// concluded[0] 是本次直接判定的那一場;其後是連鎖判定(對手早已棄賽)的場次。
// seated 是鏈的末端 —— 勝者已寫進去、但還在等另一半的場次。
// thirdPlace 是準決賽判定後成形的季軍戰(至多一場)。
type resolution struct {
	concluded  []Match
	seated     []Match
	thirdPlace *Match
	settled    int
	voided     int
	events     []Event
	champion   *Player
}

// outcome 把連鎖結果整理成對外的形狀。
func (r *resolution) outcome() *ReportResultOutcome {
	out := &ReportResultOutcome{
		Match:           r.concluded[0],
		AdvancedMatches: append(append([]Match{}, r.concluded[1:]...), r.seated...),
		ThirdPlaceMatch: r.thirdPlace,
		SettledBetCount: r.settled,
		VoidedBetCount:  r.voided,
	}
	if r.champion != nil {
		out.ChampionPlayerPublicID = r.champion.PublicID
	}
	return out
}

// resolve 判定一場比賽,然後沿著晉級鏈往下走。
//
// # 為什麼要「往下走」
//
// 棄賽者可能坐在一個對手尚未確定的位置上(輪空直接進第二輪就會這樣)。
// 那一場在棄賽當下判不了 —— 沒有勝者可寫。等另一半終於由上一輪產生時,
// 如果這裡不接手,那一場就永遠停在 pending:裁判沒有任何動作可以推動它,
// 因為棄賽者已經棄賽過了。所以晉級寫入之後要檢查對手,必要時直接判成
// 不戰而勝,再繼續往下 —— 遞迴深度以對戰表輪數為界。
//
// 季軍戰是同一個機制的另一條支線:準決賽兩場都判完時建它;若兩位敗者之一
// 早已棄賽,它也在這裡直接判成不戰而勝,不留一場沒有人推得動的比賽。
func (s *Service[TX]) resolve(
	ctx context.Context, tx TX, ec eventCtx,
	m *Match, winner, loser Player, kind ResultKind, act actor, out *resolution,
) error {
	fin, score, settled, voided, err := s.finish(ctx, tx, ec, m, winner, loser, kind, act)
	if err != nil {
		return err
	}
	out.concluded = append(out.concluded, *fin)
	out.settled += settled
	out.voided += voided

	next, err := s.advance(ctx, tx, ec, fin, winner)
	if err != nil {
		return err
	}

	third, err := s.thirdPlace(ctx, tx, ec, fin, loser)
	if err != nil {
		return err
	}

	violations, err := s.repo.ListViolations(ctx, tx, fin.ID)
	if err != nil {
		return fmt.Errorf("讀違規紀錄 match=%s: %w", fin.PublicID, err)
	}

	ev := ec.base(fin)
	ev.Result = &ResultInfo{
		Kind:            kind,
		Winner:          ec.side(winner),
		Loser:           ec.side(loser),
		Violations:      violationInfos(violations),
		SettledBetCount: settled,
		VoidedBetCount:  voided,
	}
	if score != nil {
		si := ec.scoreInfo(*score)
		ev.Result.Score = &si
	}
	if next != nil {
		ev.Result.NextMatchPublicID = next.PublicID
		ev.Result.NextRoundLabel = ec.b.RoundLabel(next.Round)
	}
	if third != nil {
		ev.Result.ThirdPlaceMatchPublicID = third.PublicID
	}
	out.events = append(out.events, event(TopicMatchFinished, ev))

	switch {
	case next == nil && fin.IsThirdPlace():
		// 季軍戰沒有下一場,也沒有冠軍:它的結論(季軍)由 AwardPrizes 從
		// FindThirdPlaceMatch 取,不在這裡另發公告。
	case next == nil:
		// 決賽。冠軍另發一則:讓它混在某一場的賽果公告裡會被滑過去,
		// 而那是一屆賽事的結論。
		champion := winner
		out.champion = &champion
		out.events = append(out.events, event(TopicChampion, ChampionEvent{
			TournamentSlug: ec.t.Slug,
			TournamentName: ec.t.Name,
			Champion:       *ec.side(winner),
			RunnerUp:       ec.side(loser),
			FinalMatchID:   fin.PublicID,
			TotalRounds:    ec.t.TotalRounds,
		}))
	default:
		// 下一場雙方到齊時**不自動開盤** —— 開盤是裁判的另一個動作(要發 BP、
		// 要發提醒公告),自動做等於替裁判決定了比賽節奏。狀態維持 pending。
		opp := next.Opponent(winner.ID)
		if opp.Seated() && opp.Status == tournament.PlayerWithdrawn {
			if err := s.resolve(ctx, tx, ec, next, winner, opp, ResultWalkover, act, out); err != nil {
				return err
			}
		} else {
			out.seated = append(out.seated, *next)
		}
	}
	return s.resolveThirdPlace(ctx, tx, ec, third, act, out)
}

// resolveThirdPlace 把剛成形的季軍戰記進結果;若一方早已棄賽,當場判成不戰而勝。
//
// 兩位敗者都棄賽的情況在 thirdPlace 就不會建(回 nil),所以這裡最多一方棄賽。
func (s *Service[TX]) resolveThirdPlace(
	ctx context.Context, tx TX, ec eventCtx, third *Match, act actor, out *resolution,
) error {
	if third == nil {
		return nil
	}
	out.thirdPlace = third
	for _, side := range []Player{third.P1, third.P2} {
		if side.Status != tournament.PlayerWithdrawn {
			continue
		}
		return s.resolve(ctx, tx, ec, third, third.Opponent(side.ID), side, ResultWalkover, act, out)
	}
	return nil
}

// finish 寫下一場比賽的結局,並處理敗方狀態、跨屆戰績與注單。
//
// 回傳的兩個計數分別是結算與退款影響的注單數(同一場只會有一種);
// score 只在 ResultNormal 時非 nil。
//
// # 整場勝者是衍生值,寫入前重算核對
//
// ResultNormal 的勝者必須等於回合推導的結果(先拿到 WinsNeeded 勝的一方)。
// 呼叫端算過一次,這裡**再算一次**而不是相信參數:與 match_budgets.spent 同一套紀律 ——
// 衍生值存進表裡的那一刻,要能證明它與來源一致,否則日後對帳對不上時
// 分不出是回合列被動過還是當時就寫錯了。不戰而勝沒有回合,跳過。
func (s *Service[TX]) finish(
	ctx context.Context, tx TX, ec eventCtx, m *Match, winner, loser Player, kind ResultKind, act actor,
) (fin *Match, score *Score, settled, voided int, err error) {
	if kind == ResultNormal {
		rounds, rerr := s.rounds(ctx, tx, m)
		if rerr != nil {
			return nil, nil, 0, 0, rerr
		}
		sc := ScoreOf(*m, rounds)
		if derived := sc.DecidedWinner(*m, ec.winsNeeded()); derived != winner.ID {
			return nil, nil, 0, 0, fmt.Errorf("match=%s 比數 %s 推導勝者=%d 寫入勝者=%d: %w",
				m.PublicID, sc, derived, winner.ID, ErrRoundsInconsistent)
		}
		score = &sc
	}

	fin, err = s.repo.MarkFinished(ctx, tx, FinishWrite{
		MatchID:        m.ID,
		WinnerPlayerID: winner.ID,
		ResultKind:     kind,
		ActorUserID:    act.userID,
		Reason:         act.reason,
	})
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("判定勝負 match=%s: %w", m.PublicID, err)
	}

	// 棄賽者維持 withdrawn:覆寫成 eliminated 會讓「他是自己退出的」
	// 從資料上消失,而生涯頁要靠這個區別顯示。
	// 季軍戰的敗者早在準決賽就標過 eliminated,這裡的條件自然不成立。
	if loser.Status == tournament.PlayerActive {
		if _, err := s.repo.SetPlayerStatus(ctx, tx, PlayerStatusWrite{
			PlayerID:    loser.ID,
			Status:      tournament.PlayerEliminated,
			ActorUserID: act.userID,
			Reason:      act.reason,
		}); err != nil {
			return nil, nil, 0, 0, fmt.Errorf("標記淘汰 player=%s: %w", loser.PublicID, err)
		}
	}

	// 戰績口徑由 RecalcFencerStats 定死:勝者 wins +1、敗者 losses +1,
	// 不戰而勝也照算(schemas/26 待確認 ④)。
	if err := s.repo.BumpFencerRecord(ctx, tx, FencerRecordWrite{
		FencerID: winner.FencerID, WinsDelta: 1,
	}); err != nil {
		return nil, nil, 0, 0, fmt.Errorf("更新勝者戰績 fencer=%d: %w", winner.FencerID, err)
	}
	if err := s.repo.BumpFencerRecord(ctx, tx, FencerRecordWrite{
		FencerID: loser.FencerID, LossesDelta: 1,
	}); err != nil {
		return nil, nil, 0, 0, fmt.Errorf("更新敗者戰績 fencer=%d: %w", loser.FencerID, err)
	}

	// 動錢排在最後、而且一定在 MarkFinished 之後:
	// betting 會重讀這一列,它要看到的是我們剛寫下的 done + winner + result_kind。
	if kind == ResultWalkover {
		r, verr := s.bets.VoidMatchInTx(ctx, tx, fin.PublicID)
		if verr != nil {
			return nil, nil, 0, 0, fmt.Errorf("退款 match=%s: %w", fin.PublicID, verr)
		}
		return fin, nil, 0, len(r.Bets), nil
	}
	r, serr := s.bets.SettleMatchInTx(ctx, tx, fin.PublicID)
	if serr != nil {
		return nil, nil, 0, 0, fmt.Errorf("結算 match=%s: %w", fin.PublicID, serr)
	}
	return fin, score, len(r.Bets), 0, nil
}

// advance 把勝者寫進下一場。回傳 (nil, nil) 表示沒有下一場(決賽或季軍戰)。
//
// 位置由 bracket.Advance 算,不在這裡自己寫一次 slot/2 的映射:
// 那個映射的權威在 bracket 套件,寫第二份就會有第二個版本,
// 而「晉級到錯的位置」是人工看對戰表看不出來的 bug。
func (s *Service[TX]) advance(ctx context.Context, tx TX, ec eventCtx, m *Match, winner Player) (*Match, error) {
	if ec.t.TotalRounds <= 0 {
		return nil, fmt.Errorf("tournament=%s: %w", ec.t.Slug, ErrBracketMissing)
	}
	if m.IsThirdPlace() {
		// 季軍戰的勝者不晉級。bracket.Advance 對它的位置也會回 ok=false,
		// 但那是「樹上沒有這個位置」的巧合;不晉級是 kind 的定義,在這裡明說。
		return nil, nil
	}
	nr, ns, isP1, ok := ec.b.Advance(m.Round, m.Slot)
	if !ok {
		return nil, nil
	}
	next, err := s.repo.LockMatchAt(ctx, tx, m.TournamentID, nr, ns)
	if err != nil {
		if errors.Is(err, activityerr.ErrMatchNotFound) {
			return nil, fmt.Errorf("round=%d slot=%d: %w", nr, ns, ErrAdvanceTargetMissing)
		}
		return nil, fmt.Errorf("讀晉級目標 round=%d slot=%d: %w", nr, ns, err)
	}
	seated, err := s.repo.SeatPlayer(ctx, tx, SeatWrite{
		MatchID: next.ID, PlayerID: winner.ID, IsP1: isP1,
	})
	if err != nil {
		return nil, fmt.Errorf("寫入晉級 match=%s: %w", next.PublicID, err)
	}
	return seated, nil
}

// thirdPlace 在準決賽判定後檢查要不要建季軍戰(grill Q15)。回 (nil, nil) = 這次不建。
//
// 條件全部成立才建:本屆開季軍戰、這場是晉級樹上的準決賽、另一場準決賽也已 done、
// 季軍戰還沒建過。位置由 bracket.ThirdPlaceSlot 給;p1/p2 依準決賽的 slot 順序排
// (slot 0 的敗者是 p1),讓同一屆重跑得到同一張表。
//
// 兩位敗者都已棄賽時不建:那會是一場誰都推不動的比賽(開盤會被 ErrPlayerWithdrawn
// 擋,棄賽也已經發生過),而季軍本來就沒有人可以得。
func (s *Service[TX]) thirdPlace(ctx context.Context, tx TX, ec eventCtx, fin *Match, loser Player) (*Match, error) {
	if !ec.cfg.Format.ThirdPlaceMatch || fin.IsThirdPlace() {
		return nil, nil
	}
	semis := ec.b.SemifinalPositions()
	var otherPos *bracket.Position
	for i := range semis {
		if semis[i].Round == fin.Round && semis[i].Slot == fin.Slot {
			other := semis[len(semis)-1-i]
			otherPos = &other
		}
	}
	if otherPos == nil {
		return nil, nil // 不是準決賽
	}
	existing, err := s.repo.FindThirdPlaceMatch(ctx, tx, fin.TournamentID)
	if err != nil {
		return nil, fmt.Errorf("讀季軍戰 tournament=%d: %w", fin.TournamentID, err)
	}
	if existing != nil {
		return nil, nil
	}
	other, err := s.repo.LockMatchAt(ctx, tx, fin.TournamentID, otherPos.Round, otherPos.Slot)
	if err != nil {
		if errors.Is(err, activityerr.ErrMatchNotFound) {
			return nil, fmt.Errorf("round=%d slot=%d: %w", otherPos.Round, otherPos.Slot, ErrAdvanceTargetMissing)
		}
		return nil, fmt.Errorf("讀另一場準決賽 round=%d slot=%d: %w", otherPos.Round, otherPos.Slot, err)
	}
	if other.Status != StatusDone {
		return nil, nil // 等另一場打完,由它那次判定來建
	}
	otherLoser := other.Opponent(other.WinnerPlayerID)
	if !otherLoser.Seated() {
		return nil, fmt.Errorf("match=%s 已完賽但敗者不明: %w", other.PublicID, ErrRoundsInconsistent)
	}
	if loser.Status == tournament.PlayerWithdrawn && otherLoser.Status == tournament.PlayerWithdrawn {
		return nil, nil
	}

	round, slot, ok := ec.b.ThirdPlaceSlot()
	if !ok {
		return nil, nil
	}
	p1, p2 := loser, otherLoser
	if fin.Slot > other.Slot {
		p1, p2 = otherLoser, loser
	}
	created, err := s.repo.InsertThirdPlaceMatch(ctx, tx, ThirdPlaceWrite{
		TournamentID: fin.TournamentID, Round: round, Slot: slot,
		P1PlayerID: p1.ID, P2PlayerID: p2.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("建季軍戰 tournament=%d: %w", fin.TournamentID, err)
	}
	return created, nil
}

// rounds 讀一場的回合並把勝者翻成 public_id。
func (s *Service[TX]) rounds(ctx context.Context, tx TX, m *Match) ([]Round, error) {
	rounds, err := s.repo.ListRounds(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀回合 match=%s: %w", m.PublicID, err)
	}
	m.ResolveRoundWinners(rounds)
	return rounds, nil
}
