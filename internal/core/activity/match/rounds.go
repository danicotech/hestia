package match

import (
	"context"
	"fmt"
	"strings"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 本檔是 2026-09-13 增補的裁判動線(schemas/20「裁判動線」+ schemas/27):
//
//	【一場一次】ReviewSetup → ConfirmSetup
//	【每回合一次】StartRound → FinishRound(整場勝者由回合推導)
//	【隨時】RecordViolation / ListMatchViolations(只記錄,不觸發任何後果)
//
// 每一支都跑在與其他裁判動作相同的鎖序裡(advisory lock → matches 列鎖 → 賽事 FOR SHARE),
// 稽核與 outbox / 推播與領域變更同一個 tx。

// ── 設定確認 ────────────────────────────────────────────────────

// SetupReview 是開賽前設定確認的檢視:裁判按「都確認了」之前看的那一頁。
type SetupReview struct {
	Match Match
	// Handicaps 是裁判端的讓武檢視(封盤後才有內容);本場無讓武時 Selections 為空。
	Handicaps *handicap.MatchHandicaps
	// Checklist 是確認清單:每一筆未作廢的讓武項目的 referee_note + 抽選結果。
	// **推導值**(handicap.MatchHandicaps.Checklist),不另存。
	Checklist []handicap.ChecklistEntry
	// Rounds 是已經打過的回合(重看設定時可能已經打到第二回合)。
	Rounds []Round
	// WinsNeeded 是本屆整場勝者要先拿到幾勝(⌈best_of/2⌉)。
	WinsNeeded int
}

// ReviewSetup 唯讀:回場次、確認清單與既有回合。
//
// 只有 status ≥ locked 才有清單:封盤前讓武內容還在變,沒有一份定案的東西可確認
// (matches_setup_confirmed_status_check 也是同一條線)。封盤前呼叫回的是只有 Match 的檢視,
// 不是錯誤 —— 裁判頁在任何狀態都要打得開。
func (s *Service[TX]) ReviewSetup(ctx context.Context, matchPublicID string) (*SetupReview, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	var out *SetupReview
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		// 無鎖讀:這一支不寫任何東西,取列鎖只會讓裁判頁的每次重整都與選購互相排隊。
		m, err := s.repo.GetMatch(ctx, tx, matchPublicID)
		if err != nil {
			return fmt.Errorf("讀場次 %s: %w", matchPublicID, err)
		}
		t, err := s.repo.TournamentByID(ctx, tx, m.TournamentID)
		if err != nil {
			return fmt.Errorf("讀賽事 id=%d: %w", m.TournamentID, err)
		}
		ec := newEventCtx(t)
		out = &SetupReview{Match: *m, WinsNeeded: ec.winsNeeded()}
		switch m.Status {
		case StatusLocked, StatusLive, StatusDone:
		default:
			return nil
		}
		view, err := s.hcap.RefereeViewInTx(ctx, tx, m.PublicID)
		if err != nil {
			return fmt.Errorf("讀裁判端讓武檢視 match=%s: %w", m.PublicID, err)
		}
		rounds, err := s.rounds(ctx, tx, m)
		if err != nil {
			return err
		}
		out.Handicaps = view
		out.Checklist = view.Checklist()
		out.Rounds = rounds
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ConfirmSetup 開賽前設定確認:裁判看完整張清單按一次「都確認了」。
//
// 要求 status = locked(封盤後才有定案的清單)且尚未確認過(ErrSetupAlreadyConfirmed:
// 第二次會蓋掉第一次的時間與人)。這一步是**開打的前提** —— StartRound 的第一回合
// 在應用層檢查 SetupConfirmedAt,DB 的 CHECK 是最後一道。
//
// 稽核的 after 帶當下的清單(SetupConfirmWrite.Checklist):日後有人問「裁判確認時
// 清單上有沒有這一項」,查的就是那裡 —— 清單是推導值,目錄同步不會改寫這份快照。
// 不發公告:確認是裁判與選手之間的事,觀眾要看的是封盤公示與賽果。
func (s *Service[TX]) ConfirmSetup(ctx context.Context, matchPublicID string, actorUserID int64) (*Match, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if actorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}
	var out *Match
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, _, err := s.load(ctx, tx, matchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		switch {
		case m.Status == StatusDone:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		case m.SetupConfirmedAt != nil:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrSetupAlreadyConfirmed)
		case m.Status != StatusLocked:
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotLocked)
		}
		view, err := s.hcap.RefereeViewInTx(ctx, tx, m.PublicID)
		if err != nil {
			return fmt.Errorf("讀裁判端讓武檢視 match=%s: %w", m.PublicID, err)
		}
		confirmed, err := s.repo.ConfirmSetup(ctx, tx, SetupConfirmWrite{
			MatchID: m.ID, ActorUserID: actorUserID, Checklist: view.Checklist(),
		})
		if err != nil {
			return fmt.Errorf("設定確認 match=%s: %w", m.PublicID, err)
		}
		out = confirmed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 回合 ────────────────────────────────────────────────────────

// RoundStart 是「正式決鬥開始」的結果。
type RoundStart struct {
	Match Match
	// Round 是剛建的那一回合(StartedAt = 計時起點)。
	Round Round
	// Rounds 是含這一回合的全部回合。
	Rounds []Round
	// ClosedMarkets 是第一回合開始時關掉的盤口數;之後的回合恆為 0。
	ClosedMarkets int
}

// StartRound 開始下一回合(裁判按「正式決鬥開始」)。
//
// 第一回合 = 開打:要求 status = locked 且已做設定確認(否則 ErrSetupNotConfirmed,
// 應用層先給人話,matches_started_requires_setup_check 是最後一道)→ 場次進 live、
// started_at = now()、關掉所有盤口(schemas/21「第一回合開打即關」)→ 建第 1 回合列。
//
// 之後的回合:要求 status = live、上一回合已填勝者(ErrRoundInProgress)、
// 整場尚未有人達到 WinsNeeded(ErrMatchDecided)→ 建第 n+1 回合列。
//
// 回合列**不預建**:三局兩勝可能只打兩回合。started_at 由 DB 的 now() 給,
// 前端的計時器算 now() − started_at,伺服器不存倒數狀態。
func (s *Service[TX]) StartRound(ctx context.Context, matchPublicID string, actorUserID int64) (*RoundStart, error) {
	return s.startRound(ctx, matchPublicID, actorUserID, "")
}

func (s *Service[TX]) startRound(ctx context.Context, matchPublicID string, actorUserID int64, reason string) (*RoundStart, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	if actorUserID <= 0 {
		return nil, tournament.ErrActorRequired
	}
	var out *RoundStart
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, ec, err := s.load(ctx, tx, matchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		rounds, err := s.rounds(ctx, tx, m)
		if err != nil {
			return err
		}
		score := ScoreOf(*m, rounds)
		// 先看回合再看狀態:2:0 之後場次已是 done,但裁判要聽到的是「勝負已定」,
		// 不是「已完賽」—— 前者告訴他為什麼不能再開一回合。
		if score.DecidedWinner(*m, ec.winsNeeded()) != 0 {
			return fmt.Errorf("match=%s 比數 %s: %w", m.PublicID, score, ErrMatchDecided)
		}
		if m.Status == StatusDone {
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		}

		next := len(rounds) + 1
		closed := 0
		if next == 1 {
			switch {
			case m.Status == StatusLive:
				// 已經 live 卻沒有回合列(舊資料):這一按就是第一回合,補建即可。
			case m.Status != StatusLocked:
				return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotLocked)
			case m.SetupConfirmedAt == nil:
				return fmt.Errorf("match=%s: %w", m.PublicID, ErrSetupNotConfirmed)
			default:
				live, err := s.repo.MarkLive(ctx, tx, LiveWrite{
					MatchID: m.ID, ActorUserID: actorUserID, Reason: reason,
				})
				if err != nil {
					return fmt.Errorf("開打 match=%s: %w", m.PublicID, err)
				}
				m = live
			}
			// 第一回合的正式決鬥開始 = 關盤(schemas/21)。已經 live 的場次照樣關:
			// CloseMarketsInTx 只動還開著的盤口,重複呼叫是 0 列,不是錯誤。
			n, err := s.bets.CloseMarketsInTx(ctx, tx, m.PublicID)
			if err != nil {
				return fmt.Errorf("關盤 match=%s: %w", m.PublicID, err)
			}
			closed = n
		} else {
			if m.Status != StatusLive {
				return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotLive)
			}
			if last := rounds[len(rounds)-1]; !last.Finished() {
				return fmt.Errorf("match=%s round=%d: %w", m.PublicID, last.RoundNo, ErrRoundInProgress)
			}
		}

		r, err := s.repo.StartRound(ctx, tx, RoundStartWrite{
			MatchID: m.ID, RoundNo: next, ActorUserID: actorUserID, Reason: reason,
		})
		if err != nil {
			return fmt.Errorf("開始第 %d 回合 match=%s: %w", next, m.PublicID, err)
		}
		rounds = append(rounds, *r)

		var events []Event
		if next == 1 {
			events = append(events, event(TopicMatchStarted, ec.base(m)))
		}
		ev := ec.base(m)
		ev.CurrentRound = ec.roundInfo(m, *r, rounds)
		events = append(events, event(TopicRoundStarted, ev))
		if err := s.repo.AppendEvents(ctx, tx, events); err != nil {
			return fmt.Errorf("寫 outbox: %w", err)
		}
		out = &RoundStart{Match: *m, Round: *r, Rounds: rounds, ClosedMarkets: closed}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RoundFinish 是填該回合勝者的結果。
type RoundFinish struct {
	Match Match
	// Round 是剛結束的那一回合。
	Round Round
	// Rounds 是全部回合;Score 由它們數出來。
	Rounds []Round
	Score  Score
	// RoundSettledBetCount 是本回合單回合盤口結算的注單數。
	RoundSettledBetCount int
	// Decided = 整場因此定案;Outcome 非 nil,內容與 ReportResult 相同。
	Decided bool
	Outcome *ReportResultOutcome
}

// FinishRoundParams 是 FinishRound 的輸入。
type FinishRoundParams struct {
	MatchPublicID  string
	RoundNo        int
	WinnerPublicID string
	ActorUserID    int64
	// Note 進稽核的 reason;判該回合違規時就在這裡寫「為什麼」。
	Note string
	// Confirm 是二次確認;沒帶就是 ErrConfirmationRequired。
	Confirm bool
}

// FinishRound 填該回合勝者。**不可逆**:回合結果會結算單回合盤口;若因此
// 有人先拿到 WinsNeeded 勝,整場當場定案(晉級、派彩、季軍戰、公告全在同一個 tx)。
//
// 勝者必須是場上兩人之一(ErrWinnerNotInMatch);回合必須存在且未結束
// (ErrRoundNotFound / ErrRoundAlreadyFinished,後者由 finished_at IS NULL 在 DB 側同守)。
// Confirm 必須為 true(ErrConfirmationRequired):與 LockHandicap / ReportResult /
// WithdrawPlayer 一樣,二次確認的權威在這裡,入口層只是把旗標傳下來 ——
// 任何繞過 transport 的呼叫端(CLI、測試、第二個入口)都得過同一道門。
func (s *Service[TX]) FinishRound(ctx context.Context, p FinishRoundParams) (*RoundFinish, error) {
	matchPublicID, roundNo, winnerPublicID := p.MatchPublicID, p.RoundNo, p.WinnerPublicID
	actorUserID, note := p.ActorUserID, p.Note
	switch {
	case matchPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	case roundNo < 1:
		return nil, fmt.Errorf("%w: round_no 必須 >= 1", ErrInvalidRequest)
	case winnerPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 winner_player_public_id", ErrInvalidRequest)
	case actorUserID <= 0:
		return nil, tournament.ErrActorRequired
	case !p.Confirm:
		return nil, fmt.Errorf("填回合勝者不可逆: %w", ErrConfirmationRequired)
	}
	var out *RoundFinish
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, ec, err := s.load(ctx, tx, matchPublicID, tournament.PhaseInProgress)
		if err != nil {
			return err
		}
		switch {
		case m.Status == StatusDone:
			return fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchFinished)
		case m.Status != StatusLive:
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrNotLive)
		}
		winner := m.PlayerByPublicID(winnerPublicID)
		if !winner.Seated() {
			return fmt.Errorf("match=%s winner=%s: %w", m.PublicID, winnerPublicID, ErrWinnerNotInMatch)
		}
		res, err := s.finishRound(ctx, tx, ec, m, roundNo, winner, actor{userID: actorUserID, reason: note})
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// finishRound 是 FinishRound 與(單場定勝負的)ReportResult 共用的核心:
// 填勝者 → 結算單回合盤口 → 重數比數 → 達標就整場定案 → 事件。
//
// 呼叫端已經持有列鎖、驗過狀態與勝者。roundNo 對應的回合必須存在且未結束。
func (s *Service[TX]) finishRound(
	ctx context.Context, tx TX, ec eventCtx, m *Match, roundNo int, winner Player, act actor,
) (*RoundFinish, error) {
	rounds, err := s.rounds(ctx, tx, m)
	if err != nil {
		return nil, err
	}
	var target *Round
	for i := range rounds {
		if rounds[i].RoundNo == roundNo {
			target = &rounds[i]
		}
	}
	switch {
	case target == nil:
		return nil, fmt.Errorf("match=%s round=%d: %w", m.PublicID, roundNo, ErrRoundNotFound)
	case target.Finished():
		return nil, fmt.Errorf("match=%s round=%d: %w", m.PublicID, roundNo, ErrRoundAlreadyFinished)
	}

	r, err := s.repo.FinishRound(ctx, tx, RoundFinishWrite{
		MatchID: m.ID, RoundNo: roundNo, WinnerPlayerID: winner.ID,
		ActorUserID: act.userID, Reason: act.reason,
	})
	if err != nil {
		return nil, fmt.Errorf("結束第 %d 回合 match=%s: %w", roundNo, m.PublicID, err)
	}
	r.WinnerPublicID = winner.PublicID
	*target = *r

	// 單回合盤口在回合寫完之後結算:betting 要讀到的是我們剛寫下的 finished + winner。
	settled, err := s.bets.SettleRoundInTx(ctx, tx, m.PublicID, roundNo)
	if err != nil {
		return nil, fmt.Errorf("結算第 %d 回合 match=%s: %w", roundNo, m.PublicID, err)
	}

	score := ScoreOf(*m, rounds)
	out := &RoundFinish{
		Match: *m, Round: *r, Rounds: rounds, Score: score,
		RoundSettledBetCount: len(settled.Bets),
	}

	ev := ec.base(m)
	ev.CurrentRound = ec.roundInfo(m, *r, rounds)
	events := []Event{event(TopicRoundFinished, ev)}

	if decided := score.DecidedWinner(*m, ec.winsNeeded()); decided != 0 {
		if decided != winner.ID {
			// 剛填的勝者不是達標的那一方:只可能是對手早就達標了卻沒定案(資料被動過)。
			return nil, fmt.Errorf("match=%s 比數 %s: %w", m.PublicID, score, ErrRoundsInconsistent)
		}
		res := &resolution{}
		if err := s.resolve(ctx, tx, ec, m, winner, m.Opponent(winner.ID), ResultNormal, act, res); err != nil {
			return nil, err
		}
		events = append(events, res.events...)
		out.Match = res.concluded[0]
		out.Decided = true
		out.Outcome = res.outcome()
	}
	if err := s.repo.AppendEvents(ctx, tx, events); err != nil {
		return nil, fmt.Errorf("寫 outbox: %w", err)
	}
	return out, nil
}

// ── 違規 ────────────────────────────────────────────────────────

// RecordViolationParams 是記一筆違規(schemas/27)。
type RecordViolationParams struct {
	MatchPublicID string
	// RoundNo nil = 開賽前(例:設定確認時發現沒改到)。
	RoundNo *int
	// PlayerPublicID 是違規者,可以是任一方(grill Q6)。
	PlayerPublicID string
	// ItemPublicID 是違反哪一項讓武;空 = 不對應特定項目(違反通則)。
	ItemPublicID string
	Ruling       Ruling
	// Note **必填**:發生了什麼、為什麼這樣判。沒有理由的紀錄事後分不出
	// 「依規則」與「裁判不喜歡」(與 RefundSelection 的 reason 同一個理由)。
	Note        string
	ActorUserID int64
}

// RecordViolation 記一筆違規。**不觸發任何後果**:判該回合 = 裁判把該回合勝者填成
// 對方(FinishRound),判整場 = 走正常的回合路徑;這一筆只是那些判決的說明。
//
// 只寫 match_violations 與稽核(actor = 裁判),不發公告:違規本身不單發,
// ruling 是判負時賽果公告會帶上它(grill Q14)。任何場次狀態都可以記 ——
// 開賽前、比賽中、賽後補記都是真實會發生的事。
func (s *Service[TX]) RecordViolation(ctx context.Context, p RecordViolationParams) (*Violation, error) {
	switch {
	case p.MatchPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	case p.PlayerPublicID == "":
		return nil, fmt.Errorf("%w: 缺少 player_public_id", ErrInvalidRequest)
	case p.ActorUserID <= 0:
		return nil, tournament.ErrActorRequired
	case !p.Ruling.Valid():
		return nil, fmt.Errorf("ruling=%q: %w", p.Ruling, ErrInvalidRuling)
	case p.RoundNo != nil && *p.RoundNo < 1:
		return nil, fmt.Errorf("%w: round_no 必須 >= 1", ErrInvalidRequest)
	}
	note := strings.TrimSpace(p.Note)
	if note == "" {
		return nil, fmt.Errorf("%w: 違規紀錄必須寫明備註", ErrInvalidRequest)
	}

	var out *Violation
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, _, err := s.load(ctx, tx, p.MatchPublicID, tournament.PhaseInProgress, tournament.PhaseFinished)
		if err != nil {
			return err
		}
		player := m.PlayerByPublicID(p.PlayerPublicID)
		if !player.Seated() {
			return fmt.Errorf("match=%s player=%s: %w", m.PublicID, p.PlayerPublicID, ErrPlayerNotInMatch)
		}
		v, err := s.repo.RecordViolation(ctx, tx, ViolationWrite{
			MatchID: m.ID, RoundNo: p.RoundNo, PlayerID: player.ID,
			ItemPublicID: p.ItemPublicID, Ruling: p.Ruling, Note: note, ActorUserID: p.ActorUserID,
		})
		if err != nil {
			return fmt.Errorf("記錄違規 match=%s: %w", m.PublicID, err)
		}
		out = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListMatchViolations 列出一場的全部違規(裁判端)。
//
// 觀眾端**不揭露**:item 會間接洩漏誰買了什麼;選手端只該讀自己的,那條路由 transport
// 依 session 過濾這份清單即可,不另開一支。
func (s *Service[TX]) ListMatchViolations(ctx context.Context, matchPublicID string) ([]Violation, error) {
	if matchPublicID == "" {
		return nil, fmt.Errorf("%w: 缺少 match_public_id", ErrInvalidRequest)
	}
	var out []Violation
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, err := s.repo.GetMatch(ctx, tx, matchPublicID)
		if err != nil {
			return fmt.Errorf("讀場次 %s: %w", matchPublicID, err)
		}
		vs, err := s.repo.ListViolations(ctx, tx, m.ID)
		if err != nil {
			return fmt.Errorf("讀違規紀錄 match=%s: %w", m.PublicID, err)
		}
		out = vs
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Violation{}
	}
	return out, nil
}
