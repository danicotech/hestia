package match

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// fakeDB 同時扮演 Repository、Handicaps 與 Bets。
//
// 三個埠合在一個假物件裡不是偷懶:本套件的核心性質就是「六件事同生共死」,
// 拆成三個互不相干的 mock 就測不到那件事。它刻意模擬了三個真資料庫才有的行為:
//
//   - InTx 失敗時把**全部**狀態還原,用來驗證 rollback 後什麼都沒發生
//   - 結算/退款前會檢查場次是否真的已經是 done + 對應的 result_kind,
//     這正是「betting 必須讀到我們剛寫下的勝者」那條順序要求
//   - 場次的 P1/P2 每次都從 players 重新materialize(模擬 JOIN),
//     所以「先標 withdrawn 再讀場次」與反過來會得到不同結果 —— 那個差別要測得到
//
// 它**測不到**真正的鎖行為(FOR UPDATE 的等待與順序),那必須在 pg adapter
// 對真 Postgres 測;這裡只斷言「我們有依正確順序呼叫」。
type fakeTx struct{}

type fakeMatch struct {
	id           int64
	publicID     string
	tournamentID int64
	round, slot  int
	kind         MatchKind
	status       Status
	resultKind   ResultKind
	handicapOpen bool
	lockedAt     *time.Time
	streamURL    string
	startedAt    *time.Time
	finishedAt   *time.Time
	winnerID     int64
	p1ID, p2ID   int64
	confirmedAt  *time.Time
	confirmedBy  int64
}

// fakeRound 是 match_rounds 的一列。
type fakeRound struct {
	matchID    int64
	roundNo    int
	startedAt  time.Time
	finishedAt *time.Time
	winnerID   int64
}

// fakeMarket 是 activity.markets 的最小模型:只記開著沒有。
type fakeMarket struct {
	matchID int64
	kind    string
	roundNo int
	open    bool
}

type fakePlayer struct {
	id           int64
	publicID     string
	tournamentID int64
	fencerID     int64
	displayName  string
	rank         bp.Rank
	status       tournament.PlayerStatus
}

type fakeFencer struct {
	wins   int32
	losses int32
}

type fakeDB struct {
	tournaments map[int64]*Tournament
	matches     map[int64]*fakeMatch
	players     map[int64]*fakePlayer
	fencers     map[int64]*fakeFencer

	// 讓武:預設好的預算與選擇,GrantBudgetInTx / LockInTx 直接用。
	budgets    map[int64]*handicap.Budget
	selections map[int64][]handicap.Selection

	// 下注:每場尚未結算的注單數(整場盤口)、每場每回合的注單數(單回合盤口)、盤口。
	openBets      map[string]int
	openRoundBets map[string]map[int]int
	markets       []fakeMarket

	// 回合與違規,鍵是 match id。
	rounds     map[int64][]fakeRound
	violations map[int64][]Violation

	events []Event
	audits []JudgeAction

	// 觀測用,不參與 rollback —— 我們要看的是「呼叫了什麼」,不是「留下了什麼」。
	calls []string
	// failAppendEvents / failSettle / failAudit / failSettleRound 注入失敗,用來驗證整筆 rollback。
	failAppendEvents bool
	failSettle       bool
	failSettleRound  bool
	failAudit        bool

	// nextID 給季軍戰與違規紀錄配 id / public_id。
	nextID int64
	clock  time.Time
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		tournaments:   map[int64]*Tournament{},
		matches:       map[int64]*fakeMatch{},
		players:       map[int64]*fakePlayer{},
		fencers:       map[int64]*fakeFencer{},
		budgets:       map[int64]*handicap.Budget{},
		selections:    map[int64][]handicap.Selection{},
		openBets:      map[string]int{},
		openRoundBets: map[string]map[int]int{},
		rounds:        map[int64][]fakeRound{},
		violations:    map[int64][]Violation{},
		nextID:        1000,
		clock:         time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC),
	}
}

func (db *fakeDB) now() *time.Time {
	db.clock = db.clock.Add(time.Second)
	t := db.clock
	return &t
}

func (db *fakeDB) log(format string, args ...any) {
	db.calls = append(db.calls, fmt.Sprintf(format, args...))
}

// ── 快照與還原(模擬 transaction)──────────────────────────────

type snapshot struct {
	matches       map[int64]fakeMatch
	players       map[int64]fakePlayer
	fencers       map[int64]fakeFencer
	budgets       map[int64]handicap.Budget
	selections    map[int64][]handicap.Selection
	openBets      map[string]int
	openRoundBets map[string]map[int]int
	markets       []fakeMarket
	rounds        map[int64][]fakeRound
	violations    map[int64][]Violation
	events        []Event
	audits        []JudgeAction
	nextID        int64
}

func (db *fakeDB) snapshot() snapshot {
	s := snapshot{
		matches:       map[int64]fakeMatch{},
		players:       map[int64]fakePlayer{},
		fencers:       map[int64]fakeFencer{},
		budgets:       map[int64]handicap.Budget{},
		selections:    map[int64][]handicap.Selection{},
		openBets:      map[string]int{},
		openRoundBets: map[string]map[int]int{},
		markets:       append([]fakeMarket{}, db.markets...),
		rounds:        map[int64][]fakeRound{},
		violations:    map[int64][]Violation{},
		events:        append([]Event{}, db.events...),
		audits:        append([]JudgeAction{}, db.audits...),
		nextID:        db.nextID,
	}
	for k, v := range db.rounds {
		s.rounds[k] = append([]fakeRound{}, v...)
	}
	for k, v := range db.violations {
		s.violations[k] = append([]Violation{}, v...)
	}
	for k, v := range db.openRoundBets {
		inner := map[int]int{}
		for r, n := range v {
			inner[r] = n
		}
		s.openRoundBets[k] = inner
	}
	for k, v := range db.matches {
		s.matches[k] = *v
	}
	for k, v := range db.players {
		s.players[k] = *v
	}
	for k, v := range db.fencers {
		s.fencers[k] = *v
	}
	for k, v := range db.budgets {
		s.budgets[k] = *v
	}
	for k, v := range db.selections {
		s.selections[k] = append([]handicap.Selection{}, v...)
	}
	for k, v := range db.openBets {
		s.openBets[k] = v
	}
	return s
}

func (db *fakeDB) restore(s snapshot) {
	db.matches = map[int64]*fakeMatch{}
	for k, v := range s.matches {
		m := v
		db.matches[k] = &m
	}
	db.players = map[int64]*fakePlayer{}
	for k, v := range s.players {
		p := v
		db.players[k] = &p
	}
	db.fencers = map[int64]*fakeFencer{}
	for k, v := range s.fencers {
		f := v
		db.fencers[k] = &f
	}
	db.budgets = map[int64]*handicap.Budget{}
	for k, v := range s.budgets {
		b := v
		db.budgets[k] = &b
	}
	db.selections = s.selections
	db.openBets = s.openBets
	db.openRoundBets = s.openRoundBets
	db.markets = s.markets
	db.rounds = s.rounds
	db.violations = s.violations
	db.events = s.events
	db.audits = s.audits
	db.nextID = s.nextID
}

// ── Repository ─────────────────────────────────────────────────

func (db *fakeDB) InTx(ctx context.Context, fn func(ctx context.Context, tx fakeTx) error) error {
	before := db.snapshot()
	if err := fn(ctx, fakeTx{}); err != nil {
		db.restore(before)
		return err
	}
	return nil
}

// view 把內部表示 materialize 成 core 看到的 Match(模擬 JOIN tournament_players)。
func (db *fakeDB) view(m *fakeMatch) *Match {
	kind := m.kind
	if kind == "" {
		kind = KindBracket
	}
	out := &Match{
		ID:               m.id,
		PublicID:         m.publicID,
		TournamentID:     m.tournamentID,
		Round:            m.round,
		Slot:             m.slot,
		Kind:             kind,
		Status:           m.status,
		ResultKind:       m.resultKind,
		HandicapOpen:     m.handicapOpen,
		HandicapLockedAt: m.lockedAt,
		StreamURL:        m.streamURL,
		StartedAt:        m.startedAt,
		FinishedAt:       m.finishedAt,
		WinnerPlayerID:   m.winnerID,
		SetupConfirmedAt: m.confirmedAt,
		SetupConfirmedBy: m.confirmedBy,
	}
	if p := db.players[m.p1ID]; p != nil {
		out.P1 = *db.playerView(p)
	}
	if p := db.players[m.p2ID]; p != nil {
		out.P2 = *db.playerView(p)
	}
	return out
}

func (db *fakeDB) playerView(p *fakePlayer) *Player {
	return &Player{
		ID:           p.id,
		PublicID:     p.publicID,
		TournamentID: p.tournamentID,
		FencerID:     p.fencerID,
		DisplayName:  p.displayName,
		Rank:         p.rank,
		Status:       p.status,
	}
}

func (db *fakeDB) findMatch(publicID string) *fakeMatch {
	for _, m := range db.matches {
		if m.publicID == publicID {
			return m
		}
	}
	return nil
}

func (db *fakeDB) LockMatch(_ context.Context, _ fakeTx, publicID string) (*Match, error) {
	db.log("LockMatch(%s)", publicID)
	m := db.findMatch(publicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	return db.view(m), nil
}

func (db *fakeDB) LockMatchAt(_ context.Context, _ fakeTx, tournamentID int64, round, slot int) (*Match, error) {
	db.log("LockMatchAt(%d,%d,%d)", tournamentID, round, slot)
	for _, m := range db.matches {
		if m.tournamentID == tournamentID && m.round == round && m.slot == slot {
			return db.view(m), nil
		}
	}
	return nil, activityerr.ErrMatchNotFound
}

func (db *fakeDB) GetMatch(_ context.Context, _ fakeTx, publicID string) (*Match, error) {
	m := db.findMatch(publicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	return db.view(m), nil
}

func (db *fakeDB) TournamentByID(_ context.Context, _ fakeTx, id int64) (*Tournament, error) {
	t := db.tournaments[id]
	if t == nil {
		return nil, tournament.ErrTournamentNotFound
	}
	c := *t
	return &c, nil
}

func (db *fakeDB) LockPlayer(_ context.Context, _ fakeTx, publicID string) (*Player, error) {
	db.log("LockPlayer(%s)", publicID)
	for _, p := range db.players {
		if p.publicID == publicID {
			return db.playerView(p), nil
		}
	}
	return nil, tournament.ErrPlayerNotFound
}

func (db *fakeDB) LockUnfinishedMatchesOfPlayer(_ context.Context, _ fakeTx, playerID int64) ([]Match, error) {
	db.log("LockUnfinishedMatchesOfPlayer(%d)", playerID)
	var out []Match
	for _, m := range db.matches {
		if m.status == StatusDone {
			continue
		}
		if m.p1ID != playerID && m.p2ID != playerID {
			continue
		}
		out = append(out, *db.view(m))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Round != out[j].Round {
			return out[i].Round < out[j].Round
		}
		return out[i].Slot < out[j].Slot
	})
	return out, nil
}

func (db *fakeDB) MarkReady(_ context.Context, _ fakeTx, w ReadyWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	m.status, m.handicapOpen = StatusReady, true
	db.audit(w.ActorUserID, ActionOpenHandicap, AuditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) MarkLive(_ context.Context, _ fakeTx, w LiveWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	// 呼叫先記再守門:測試要分得出「service 擋在寫入之前」與「靠 DB 擋下來」。
	db.log("MarkLive(%s)", m.publicID)
	// 模擬 MarkMatchLive 的兩道 WHERE 守門(status = locked、setup_confirmed_at IS NOT NULL)
	// 與 matches_started_requires_setup_check:service 漏擋時這裡要出聲,不能默默寫進去。
	switch {
	case m.status != StatusLocked:
		return nil, ErrNotLocked
	case m.confirmedAt == nil:
		return nil, ErrSetupNotConfirmed
	}
	m.status, m.startedAt = StatusLive, db.now()
	db.audit(w.ActorUserID, ActionStartMatch, AuditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) ConfirmSetup(_ context.Context, _ fakeTx, w SetupConfirmWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	// 模擬 ConfirmMatchSetup 的 WHERE:status = locked AND setup_confirmed_at IS NULL。
	switch {
	case m.confirmedAt != nil:
		return nil, ErrSetupAlreadyConfirmed
	case m.status != StatusLocked:
		return nil, ErrNotLocked
	}
	m.confirmedAt, m.confirmedBy = db.now(), w.ActorUserID
	db.log("ConfirmSetup(%s)", m.publicID)
	db.audit(w.ActorUserID, ActionConfirmSetup, AuditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

// ── 回合 ───────────────────────────────────────────────────────

func (db *fakeDB) roundView(r fakeRound) Round {
	return Round{RoundNo: r.roundNo, StartedAt: r.startedAt, FinishedAt: r.finishedAt, WinnerPlayerID: r.winnerID}
}

func (db *fakeDB) ListRounds(_ context.Context, _ fakeTx, matchID int64) ([]Round, error) {
	out := make([]Round, 0, len(db.rounds[matchID]))
	for _, r := range db.rounds[matchID] {
		out = append(out, db.roundView(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoundNo < out[j].RoundNo })
	return out, nil
}

func (db *fakeDB) StartRound(_ context.Context, _ fakeTx, w RoundStartWrite) (*Round, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	// 模擬 match_rounds_match_round_uq。
	for _, r := range db.rounds[m.id] {
		if r.roundNo == w.RoundNo {
			return nil, ErrRoundInProgress
		}
	}
	r := fakeRound{matchID: m.id, roundNo: w.RoundNo, startedAt: *db.now()}
	db.rounds[m.id] = append(db.rounds[m.id], r)
	db.log("StartRound(%s,%d)", m.publicID, w.RoundNo)
	db.audit(w.ActorUserID, ActionStartRound, AuditTargetMatch, m.id, w.Reason)
	v := db.roundView(r)
	return &v, nil
}

func (db *fakeDB) FinishRound(_ context.Context, _ fakeTx, w RoundFinishWrite) (*Round, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	rs := db.rounds[m.id]
	for i := range rs {
		if rs[i].roundNo != w.RoundNo {
			continue
		}
		// 模擬 FinishMatchRound 的 finished_at IS NULL 守門。
		if rs[i].finishedAt != nil {
			return nil, ErrRoundAlreadyFinished
		}
		rs[i].finishedAt, rs[i].winnerID = db.now(), w.WinnerPlayerID
		db.log("FinishRound(%s,%d,winner=%d)", m.publicID, w.RoundNo, w.WinnerPlayerID)
		db.audit(w.ActorUserID, ActionFinishRound, AuditTargetMatch, m.id, w.Reason)
		v := db.roundView(rs[i])
		return &v, nil
	}
	return nil, ErrRoundNotFound
}

// ── 季軍戰 ─────────────────────────────────────────────────────

func (db *fakeDB) InsertThirdPlaceMatch(_ context.Context, _ fakeTx, w ThirdPlaceWrite) (*Match, error) {
	// 模擬 UNIQUE (tournament_id, round, slot)。
	for _, m := range db.matches {
		if m.tournamentID == w.TournamentID && m.round == w.Round && m.slot == w.Slot {
			return nil, errors.New("季軍戰位置已有場次(UNIQUE 約束)")
		}
	}
	db.nextID++
	m := &fakeMatch{
		id: db.nextID, publicID: fmt.Sprintf("M-3RD-%d", db.nextID), tournamentID: w.TournamentID,
		round: w.Round, slot: w.Slot, kind: KindThirdPlace, status: StatusPending, resultKind: ResultNormal,
		p1ID: w.P1PlayerID, p2ID: w.P2PlayerID,
	}
	db.matches[m.id] = m
	db.log("InsertThirdPlaceMatch(%d,%d,p1=%d,p2=%d)", w.Round, w.Slot, w.P1PlayerID, w.P2PlayerID)
	return db.view(m), nil
}

func (db *fakeDB) FindThirdPlaceMatch(_ context.Context, _ fakeTx, tournamentID int64) (*Match, error) {
	for _, m := range db.matches {
		if m.tournamentID == tournamentID && m.kind == KindThirdPlace {
			return db.view(m), nil
		}
	}
	return nil, nil
}

// ── 違規 ───────────────────────────────────────────────────────

func (db *fakeDB) RecordViolation(_ context.Context, _ fakeTx, w ViolationWrite) (*Violation, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	// 呼叫先記再守門(理由同 MarkLive)。
	db.log("RecordViolation(%s,player=%d,%s)", m.publicID, w.PlayerID, w.Ruling)
	p := db.players[w.PlayerID]
	if p == nil || (w.PlayerID != m.p1ID && w.PlayerID != m.p2ID) {
		return nil, ErrPlayerNotInMatch
	}
	if w.Note == "" {
		return nil, errors.New("note 為空(CHECK 約束)")
	}
	db.nextID++
	v := Violation{
		PublicID: fmt.Sprintf("V-%d", db.nextID), MatchPublicID: m.publicID, RoundNo: w.RoundNo,
		PlayerPublicID: p.publicID, PlayerDisplayName: p.displayName,
		ItemPublicID: w.ItemPublicID, Ruling: w.Ruling, Note: w.Note,
		RecordedBy: w.ActorUserID, CreatedAt: *db.now(),
	}
	if w.ItemPublicID != "" {
		v.ItemName = "項目 " + w.ItemPublicID
	}
	db.violations[m.id] = append(db.violations[m.id], v)
	db.audit(w.ActorUserID, ActionRecordViolation, AuditTargetMatch, m.id, w.Note)
	return &v, nil
}

func (db *fakeDB) ListViolations(_ context.Context, _ fakeTx, matchID int64) ([]Violation, error) {
	return append([]Violation{}, db.violations[matchID]...), nil
}

func (db *fakeDB) MarkFinished(_ context.Context, _ fakeTx, w FinishWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	// 模擬 matches_winner_is_participant_check:勝者必須是場上兩人之一。
	if w.WinnerPlayerID != m.p1ID && w.WinnerPlayerID != m.p2ID {
		return nil, errors.New("winner 不是本場選手(CHECK 約束)")
	}
	m.status, m.winnerID, m.resultKind = StatusDone, w.WinnerPlayerID, w.ResultKind
	m.finishedAt = db.now()
	db.log("MarkFinished(%s,winner=%d,kind=%s)", m.publicID, w.WinnerPlayerID, w.ResultKind)
	db.audit(w.ActorUserID, ActionReportResult, AuditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) SetStreamURL(_ context.Context, _ fakeTx, w StreamWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	m.streamURL = w.StreamURL
	db.audit(w.ActorUserID, ActionSetStreamURL, AuditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) SeatPlayer(_ context.Context, _ fakeTx, w SeatWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	if w.IsP1 {
		m.p1ID = w.PlayerID
	} else {
		m.p2ID = w.PlayerID
	}
	db.log("SeatPlayer(%s,p1=%v,player=%d)", m.publicID, w.IsP1, w.PlayerID)
	return db.view(m), nil
}

func (db *fakeDB) SetPlayerStatus(_ context.Context, _ fakeTx, w PlayerStatusWrite) (*Player, error) {
	p := db.players[w.PlayerID]
	if p == nil {
		return nil, tournament.ErrPlayerNotFound
	}
	p.status = w.Status
	db.log("SetPlayerStatus(%s,%s)", p.publicID, w.Status)
	if w.Status == tournament.PlayerWithdrawn {
		db.audit(w.ActorUserID, ActionWithdraw, AuditTargetPlayer, p.id, w.Reason)
	}
	return db.playerView(p), nil
}

func (db *fakeDB) BumpFencerRecord(_ context.Context, _ fakeTx, w FencerRecordWrite) error {
	f := db.fencers[w.FencerID]
	if f == nil {
		f = &fakeFencer{}
		db.fencers[w.FencerID] = f
	}
	f.wins += w.WinsDelta
	f.losses += w.LossesDelta
	return nil
}

func (db *fakeDB) RecordJudgeAction(_ context.Context, _ fakeTx, a JudgeAction) error {
	if db.failAudit {
		return errors.New("稽核寫入失敗(注入)")
	}
	db.audits = append(db.audits, a)
	return nil
}

func (db *fakeDB) AppendEvents(_ context.Context, _ fakeTx, events []Event) error {
	if db.failAppendEvents {
		return errors.New("outbox 寫入失敗(注入)")
	}
	db.events = append(db.events, events...)
	return nil
}

func (db *fakeDB) audit(actorUserID int64, action, targetType string, targetID int64, reason string) {
	db.audits = append(db.audits, JudgeAction{
		ActorUserID: actorUserID, Action: action,
		TargetType: targetType, TargetID: targetID, Reason: reason,
	})
}

// ── Handicaps ──────────────────────────────────────────────────

func (db *fakeDB) GrantBudgetInTx(_ context.Context, _ fakeTx, matchPublicID string) (*handicap.Budget, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	db.log("GrantBudget(%s)", matchPublicID)
	b := db.budgets[m.id]
	if b == nil {
		return nil, nil // 同段對決,本場無讓武
	}
	c := *b
	return &c, nil
}

func (db *fakeDB) LockInTx(_ context.Context, _ fakeTx, matchPublicID string) (*handicap.MatchHandicaps, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	if m.status != StatusReady {
		return nil, handicap.ErrHandicapClosed
	}
	m.status, m.handicapOpen, m.lockedAt = StatusLocked, false, db.now()
	db.log("LockHandicaps(%s)", matchPublicID)

	out := &handicap.MatchHandicaps{
		MatchPublicID: matchPublicID,
		Status:        string(StatusLocked),
		LockedAt:      m.lockedAt,
		Revealed:      true,
	}
	b := db.budgets[m.id]
	if b == nil {
		return out, nil
	}
	holder, constrained := db.players[m.p1ID], db.players[m.p2ID]
	if b.PlayerID == m.p2ID {
		holder, constrained = constrained, holder
	}
	c := *b
	out.HolderPlayerPublicID = holder.publicID
	out.ConstrainedPlayerPublicID = constrained.publicID
	out.Budget = &c
	out.Selections = db.selections[m.id]
	return out, nil
}

// RefundSelectionInTx 模擬讓武套件的代退:找到那一筆、檢查封盤與重複退、
// 標 voided 並把 BP 加回預算。
//
// 它刻意重現真實作的三個性質(封盤即拒、已退即拒、spent 恆等於未作廢總和),
// 因為本套件的測試要斷言的是「稽核有沒有跟著同一個 tx 一起成立/一起消失」——
// 而那只有在退點本身真的會失敗時才驗得到。
func (db *fakeDB) RefundSelectionInTx(
	_ context.Context, _ fakeTx, selectionPublicID string,
) (*handicap.RefundResult, error) {
	for matchID, sels := range db.selections {
		for i := range sels {
			if sels[i].PublicID != selectionPublicID {
				continue
			}
			m := db.matches[matchID]
			if m == nil {
				return nil, activityerr.ErrMatchNotFound
			}
			if m.lockedAt != nil || m.status == StatusLocked ||
				m.status == StatusLive || m.status == StatusDone {
				return nil, handicap.ErrHandicapLocked
			}
			if sels[i].Voided {
				return nil, handicap.ErrSelectionAlreadyVoided
			}
			b := db.budgets[matchID]
			if b == nil {
				return nil, handicap.ErrNoBudget
			}
			db.log("RefundSelection(%s)", selectionPublicID)
			sels[i].Voided = true
			b.Spent -= sels[i].Cost
			return &handicap.RefundResult{
				MatchID:       matchID,
				MatchPublicID: m.publicID,
				Selection:     sels[i],
				Budget:        *b,
			}, nil
		}
	}
	return nil, handicap.ErrSelectionNotFound
}

// RefereeViewInTx 模擬裁判端檢視:封盤前後都給完整清單(不套用揭露限制)。
func (db *fakeDB) RefereeViewInTx(_ context.Context, _ fakeTx, matchPublicID string) (*handicap.MatchHandicaps, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	db.log("RefereeView(%s)", matchPublicID)
	out := &handicap.MatchHandicaps{
		MatchPublicID: matchPublicID,
		Status:        string(m.status),
		LockedAt:      m.lockedAt,
		Revealed:      m.lockedAt != nil,
	}
	b := db.budgets[m.id]
	if b == nil {
		return out, nil
	}
	c := *b
	out.Budget = &c
	out.Selections = db.selections[m.id]
	return out, nil
}

// ── Bets ───────────────────────────────────────────────────────

// OpenMarketsInTx 模擬開盤建盤口:整場勝負盤 + (best_of 由測試決定)三個單回合盤。
// 場次必須已是 ready —— 那是「先寫 matches 再建盤」的順序要求。
func (db *fakeDB) OpenMarketsInTx(_ context.Context, _ fakeTx, matchPublicID string) ([]betting.Market, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	if m.status != StatusReady {
		return nil, betting.ErrBettingClosed
	}
	db.log("OpenMarkets(%s)", matchPublicID)
	created := []fakeMarket{{matchID: m.id, kind: "match_winner", open: true}}
	for r := 1; r <= 3; r++ {
		created = append(created, fakeMarket{matchID: m.id, kind: "round_winner", roundNo: r, open: true})
	}
	db.markets = append(db.markets, created...)
	out := make([]betting.Market, 0, len(created))
	for _, c := range created {
		out = append(out, betting.Market{MatchID: c.matchID, Kind: betting.MarketKind(c.kind), RoundNo: c.roundNo})
	}
	return out, nil
}

// CloseMarketsInTx 關掉這場所有還開著的盤口,回關掉的數量(重複呼叫 = 0)。
func (db *fakeDB) CloseMarketsInTx(_ context.Context, _ fakeTx, matchPublicID string) (int, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return 0, activityerr.ErrMatchNotFound
	}
	db.log("CloseMarkets(%s)", matchPublicID)
	n := 0
	for i := range db.markets {
		if db.markets[i].matchID == m.id && db.markets[i].open {
			db.markets[i].open = false
			n++
		}
	}
	return n, nil
}

// openMarketCount 是觀測用:這場還開著幾個盤口。
func (db *fakeDB) openMarketCount(matchID int64) int {
	n := 0
	for _, mk := range db.markets {
		if mk.matchID == matchID && mk.open {
			n++
		}
	}
	return n
}

// SettleRoundInTx 結算單回合盤口。回合必須已經 finished —— 那是
// 「先寫 match_rounds 再結算」的順序要求,順序寫反時這裡直接失敗。
func (db *fakeDB) SettleRoundInTx(_ context.Context, _ fakeTx, matchPublicID string, roundNo int) (*betting.SettleResult, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	var found *fakeRound
	for i := range db.rounds[m.id] {
		if db.rounds[m.id][i].roundNo == roundNo {
			found = &db.rounds[m.id][i]
		}
	}
	if found == nil || found.finishedAt == nil || found.winnerID == 0 {
		return nil, betting.ErrRoundNotFinished
	}
	if db.failSettleRound {
		return nil, betting.ErrLedgerStateConflict
	}
	db.log("SettleRound(%s,%d)", matchPublicID, roundNo)
	n := db.openRoundBets[m.publicID][roundNo]
	if db.openRoundBets[m.publicID] != nil {
		db.openRoundBets[m.publicID][roundNo] = 0
	}
	return &betting.SettleResult{MatchPublicID: matchPublicID, Bets: make([]betting.BetOutcome, n)}, nil
}

// requireDone 模擬 betting 的前置檢查:場次必須已經是 done,而且 result_kind
// 要對得上。這是本套件「先寫 matches 再結算」那條順序要求的守門員 ——
// 順序寫反的話,這裡就會失敗。
func (db *fakeDB) requireDone(matchPublicID string, kind ResultKind) (*fakeMatch, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, activityerr.ErrMatchNotFound
	}
	if m.status != StatusDone || m.winnerID == 0 {
		return nil, betting.ErrMatchNotDecided
	}
	if kind == ResultNormal && m.resultKind == ResultWalkover {
		return nil, betting.ErrWalkoverMatch
	}
	if kind == ResultWalkover && m.resultKind != ResultWalkover {
		return nil, betting.ErrNotWalkover
	}
	return m, nil
}

func (db *fakeDB) SettleMatchInTx(_ context.Context, _ fakeTx, matchPublicID string) (*betting.SettleResult, error) {
	m, err := db.requireDone(matchPublicID, ResultNormal)
	if err != nil {
		return nil, err
	}
	if db.failSettle {
		return nil, betting.ErrLedgerStateConflict
	}
	db.log("SettleMatch(%s)", matchPublicID)
	n := db.openBets[m.publicID]
	db.openBets[m.publicID] = 0
	return &betting.SettleResult{MatchPublicID: matchPublicID, Bets: make([]betting.BetOutcome, n)}, nil
}

func (db *fakeDB) VoidMatchInTx(_ context.Context, _ fakeTx, matchPublicID string) (*betting.SettleResult, error) {
	m, err := db.requireDone(matchPublicID, ResultWalkover)
	if err != nil {
		return nil, err
	}
	db.log("VoidMatch(%s)", matchPublicID)
	n := db.openBets[m.publicID]
	db.openBets[m.publicID] = 0
	return &betting.SettleResult{MatchPublicID: matchPublicID, LegsVoid: n, Bets: make([]betting.BetOutcome, n)}, nil
}
