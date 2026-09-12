package match

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

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
	status       Status
	resultKind   ResultKind
	handicapOpen bool
	lockedAt     *time.Time
	streamURL    string
	startedAt    *time.Time
	finishedAt   *time.Time
	winnerID     int64
	p1ID, p2ID   int64
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

	// 下注:每場尚未結算的注單數。
	openBets map[string]int

	events []Event
	audits []JudgeAction

	// 觀測用,不參與 rollback —— 我們要看的是「呼叫了什麼」,不是「留下了什麼」。
	calls []string
	// failAppendEvents / failSettle 注入失敗,用來驗證整筆 rollback。
	failAppendEvents bool
	failSettle       bool

	clock time.Time
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		tournaments: map[int64]*Tournament{},
		matches:     map[int64]*fakeMatch{},
		players:     map[int64]*fakePlayer{},
		fencers:     map[int64]*fakeFencer{},
		budgets:     map[int64]*handicap.Budget{},
		selections:  map[int64][]handicap.Selection{},
		openBets:    map[string]int{},
		clock:       time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC),
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
	matches    map[int64]fakeMatch
	players    map[int64]fakePlayer
	fencers    map[int64]fakeFencer
	budgets    map[int64]handicap.Budget
	selections map[int64][]handicap.Selection
	openBets   map[string]int
	events     []Event
	audits     []JudgeAction
}

func (db *fakeDB) snapshot() snapshot {
	s := snapshot{
		matches:    map[int64]fakeMatch{},
		players:    map[int64]fakePlayer{},
		fencers:    map[int64]fakeFencer{},
		budgets:    map[int64]handicap.Budget{},
		selections: map[int64][]handicap.Selection{},
		openBets:   map[string]int{},
		events:     append([]Event{}, db.events...),
		audits:     append([]JudgeAction{}, db.audits...),
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
	db.events = s.events
	db.audits = s.audits
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
	out := &Match{
		ID:               m.id,
		PublicID:         m.publicID,
		TournamentID:     m.tournamentID,
		Round:            m.round,
		Slot:             m.slot,
		Status:           m.status,
		ResultKind:       m.resultKind,
		HandicapOpen:     m.handicapOpen,
		HandicapLockedAt: m.lockedAt,
		StreamURL:        m.streamURL,
		StartedAt:        m.startedAt,
		FinishedAt:       m.finishedAt,
		WinnerPlayerID:   m.winnerID,
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
		return nil, ErrMatchNotFound
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
	return nil, ErrMatchNotFound
}

func (db *fakeDB) GetMatch(_ context.Context, _ fakeTx, publicID string) (*Match, error) {
	m := db.findMatch(publicID)
	if m == nil {
		return nil, ErrMatchNotFound
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
	return nil, ErrPlayerNotFound
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
		return nil, ErrMatchNotFound
	}
	m.status, m.handicapOpen = StatusReady, true
	db.audit(w.ActorUserID, ActionOpenHandicap, auditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) MarkLive(_ context.Context, _ fakeTx, w LiveWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, ErrMatchNotFound
	}
	m.status, m.startedAt = StatusLive, db.now()
	db.audit(w.ActorUserID, ActionStartMatch, auditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) MarkFinished(_ context.Context, _ fakeTx, w FinishWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, ErrMatchNotFound
	}
	// 模擬 matches_winner_is_participant_check:勝者必須是場上兩人之一。
	if w.WinnerPlayerID != m.p1ID && w.WinnerPlayerID != m.p2ID {
		return nil, errors.New("winner 不是本場選手(CHECK 約束)")
	}
	m.status, m.winnerID, m.resultKind = StatusDone, w.WinnerPlayerID, w.ResultKind
	m.finishedAt = db.now()
	db.log("MarkFinished(%s,winner=%d,kind=%s)", m.publicID, w.WinnerPlayerID, w.ResultKind)
	db.audit(w.ActorUserID, ActionReportResult, auditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) SetStreamURL(_ context.Context, _ fakeTx, w StreamWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, ErrMatchNotFound
	}
	m.streamURL = w.StreamURL
	db.audit(w.ActorUserID, ActionSetStreamURL, auditTargetMatch, m.id, w.Reason)
	return db.view(m), nil
}

func (db *fakeDB) SeatPlayer(_ context.Context, _ fakeTx, w SeatWrite) (*Match, error) {
	m := db.matches[w.MatchID]
	if m == nil {
		return nil, ErrMatchNotFound
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
		return nil, ErrPlayerNotFound
	}
	p.status = w.Status
	db.log("SetPlayerStatus(%s,%s)", p.publicID, w.Status)
	if w.Status == tournament.PlayerWithdrawn {
		db.audit(w.ActorUserID, ActionWithdraw, auditTargetPlayer, p.id, w.Reason)
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
		return nil, handicap.ErrMatchNotFound
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
		return nil, handicap.ErrMatchNotFound
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

// ── Bets ───────────────────────────────────────────────────────

// requireDone 模擬 betting 的前置檢查:場次必須已經是 done,而且 result_kind
// 要對得上。這是本套件「先寫 matches 再結算」那條順序要求的守門員 ——
// 順序寫反的話,這裡就會失敗。
func (db *fakeDB) requireDone(matchPublicID string, kind ResultKind) (*fakeMatch, error) {
	m := db.findMatch(matchPublicID)
	if m == nil {
		return nil, betting.ErrMatchNotFound
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
