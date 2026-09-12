package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrPlayersNotSet)
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

		ev := ec.base(opened)
		ev.Handicap = ec.budgetInfo(opened, budget)
		if err := s.repo.AppendEvents(ctx, tx, []Event{event(TopicHandicapOpened, ev)}); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}
		out = &OpenHandicapResult{Match: *opened, Budget: budget}
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

// ── 開打 ────────────────────────────────────────────────────────

// StartMatch 標記開打:locked → live、started_at = now()。這一步關閉下注。
//
// 只從 locked 進來,ready 不行:每一場都必須走過封盤,即使同段對決沒有任何
// 讓武。封盤是「內容已公開」與下注視窗的唯一分界點,跳過它會讓觀眾看到
// 一場沒有人公示過條件的比賽。
func (s *Service[TX]) StartMatch(ctx context.Context, p StartMatchParams) (*Match, error) {
	if p.MatchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if p.ActorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}

	var out *Match
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, ec, err := s.load(ctx, tx, p.MatchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		switch {
		case m.Status == StatusDone:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		case m.Status != StatusLocked:
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotLocked)
		}

		live, err := s.repo.MarkLive(ctx, tx, LiveWrite{
			MatchID: m.ID, ActorUserID: p.ActorUserID, Reason: p.Reason,
		})
		if err != nil {
			return fmt.Errorf("開打 match=%s: %w", m.PublicID, err)
		}
		if err := s.repo.AppendEvents(ctx, tx, []Event{event(TopicMatchStarted, ec.base(live))}); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}
		out = live
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 判定勝負 ────────────────────────────────────────────────────

// ReportResult 判定勝負。**不可逆**,要求 confirm = true。
//
// 一次 tx 裡發生七件事,少任何一件都要人工修資料庫:
//
//	寫 winner / status='done' / finished_at → 敗方標 eliminated
//	→ 雙方跨屆戰績 ±1 → 下注結算派彩 → 勝者晉級 → 賽果公告 →(決賽時)冠軍公告
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
		}

		var winner, loser Player
		switch p.WinnerPlayerPublicID {
		case m.P1.PublicID:
			winner, loser = m.P1, m.P2
		case m.P2.PublicID:
			winner, loser = m.P2, m.P1
		default:
			return fmt.Errorf("match=%s winner=%s: %w",
				m.PublicID, p.WinnerPlayerPublicID, ErrWinnerNotInMatch)
		}

		res := &resolution{}
		act := actor{userID: p.ActorUserID, reason: p.Reason}
		if err := s.resolve(ctx, tx, ec, m, winner, loser, ResultNormal, act, res); err != nil {
			return err
		}
		if err := s.repo.AppendEvents(ctx, tx, res.events); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}

		out = &ReportResultOutcome{
			Match:           res.concluded[0],
			AdvancedMatches: append(append([]Match{}, res.concluded[1:]...), res.seated...),
			SettledBetCount: res.settled,
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
type resolution struct {
	concluded []Match
	seated    []Match
	settled   int
	voided    int
	events    []Event
	champion  *Player
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
func (s *Service[TX]) resolve(
	ctx context.Context, tx TX, ec eventCtx,
	m *Match, winner, loser Player, kind ResultKind, act actor, out *resolution,
) error {
	fin, settled, voided, err := s.finish(ctx, tx, m, winner, loser, kind, act)
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

	ev := ec.base(fin)
	ev.Result = &ResultInfo{
		Kind:            kind,
		Winner:          ec.side(winner),
		Loser:           ec.side(loser),
		SettledBetCount: settled,
		VoidedBetCount:  voided,
	}
	if next != nil {
		ev.Result.NextMatchPublicID = next.PublicID
		ev.Result.NextRoundLabel = ec.b.RoundLabel(next.Round)
	}
	out.events = append(out.events, event(TopicMatchFinished, ev))

	if next == nil {
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
		return nil
	}

	// 下一場雙方到齊時**不自動開盤** —— 開盤是裁判的另一個動作(要發 BP、
	// 要發提醒公告),自動做等於替裁判決定了比賽節奏。狀態維持 pending。
	opp := next.Opponent(winner.ID)
	if opp.Seated() && opp.Status == tournament.PlayerWithdrawn {
		return s.resolve(ctx, tx, ec, next, winner, opp, ResultWalkover, act, out)
	}
	out.seated = append(out.seated, *next)
	return nil
}

// finish 寫下一場比賽的結局,並處理敗方狀態、跨屆戰績與注單。
//
// 回傳的兩個計數分別是結算與退款影響的注單數(同一場只會有一種)。
func (s *Service[TX]) finish(
	ctx context.Context, tx TX, m *Match, winner, loser Player, kind ResultKind, act actor,
) (fin *Match, settled, voided int, err error) {
	fin, err = s.repo.MarkFinished(ctx, tx, FinishWrite{
		MatchID:        m.ID,
		WinnerPlayerID: winner.ID,
		ResultKind:     kind,
		ActorUserID:    act.userID,
		Reason:         act.reason,
	})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("判定勝負 match=%s: %w", m.PublicID, err)
	}

	// 棄賽者維持 withdrawn:覆寫成 eliminated 會讓「他是自己退出的」
	// 從資料上消失,而生涯頁要靠這個區別顯示。
	if loser.Status == tournament.PlayerActive {
		if _, err := s.repo.SetPlayerStatus(ctx, tx, PlayerStatusWrite{
			PlayerID:    loser.ID,
			Status:      tournament.PlayerEliminated,
			ActorUserID: act.userID,
			Reason:      act.reason,
		}); err != nil {
			return nil, 0, 0, fmt.Errorf("標記淘汰 player=%s: %w", loser.PublicID, err)
		}
	}

	// 戰績口徑由 RecalcFencerStats 定死:勝者 wins +1、敗者 losses +1,
	// 不戰而勝也照算(schemas/26 待確認 ④)。
	if err := s.repo.BumpFencerRecord(ctx, tx, FencerRecordWrite{
		FencerID: winner.FencerID, WinsDelta: 1,
	}); err != nil {
		return nil, 0, 0, fmt.Errorf("更新勝者戰績 fencer=%d: %w", winner.FencerID, err)
	}
	if err := s.repo.BumpFencerRecord(ctx, tx, FencerRecordWrite{
		FencerID: loser.FencerID, LossesDelta: 1,
	}); err != nil {
		return nil, 0, 0, fmt.Errorf("更新敗者戰績 fencer=%d: %w", loser.FencerID, err)
	}

	// 動錢排在最後、而且一定在 MarkFinished 之後:
	// betting 會重讀這一列,它要看到的是我們剛寫下的 done + winner + result_kind。
	if kind == ResultWalkover {
		r, verr := s.bets.VoidMatchInTx(ctx, tx, fin.PublicID)
		if verr != nil {
			return nil, 0, 0, fmt.Errorf("退款 match=%s: %w", fin.PublicID, verr)
		}
		return fin, 0, len(r.Bets), nil
	}
	r, serr := s.bets.SettleMatchInTx(ctx, tx, fin.PublicID)
	if serr != nil {
		return nil, 0, 0, fmt.Errorf("結算 match=%s: %w", fin.PublicID, serr)
	}
	return fin, len(r.Bets), 0, nil
}

// advance 把勝者寫進下一場。回傳 (nil, nil) 表示這是決賽。
//
// 位置由 bracket.Advance 算,不在這裡自己寫一次 slot/2 的映射:
// 那個映射的權威在 bracket 套件,寫第二份就會有第二個版本,
// 而「晉級到錯的位置」是人工看對戰表看不出來的 bug。
func (s *Service[TX]) advance(ctx context.Context, tx TX, ec eventCtx, m *Match, winner Player) (*Match, error) {
	if ec.t.TotalRounds <= 0 {
		return nil, fmt.Errorf("tournament=%s: %w", ec.t.Slug, ErrBracketMissing)
	}
	nr, ns, isP1, ok := ec.b.Advance(m.Round, m.Slot)
	if !ok {
		return nil, nil
	}
	next, err := s.repo.LockMatchAt(ctx, tx, m.TournamentID, nr, ns)
	if err != nil {
		if errors.Is(err, ErrMatchNotFound) {
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
