package betting

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// fakeDB 同時扮演 Repository 與 Ledger,因為下注的核心性質是
// 「注單側寫入與帳本寫入同生共死」—— 拆成兩個互不相干的假物件就測不到那件事。
//
// 它刻意實作了三個真資料庫才有的行為:
//   - InTx 失敗時把**全部**狀態(含餘額與分錄)還原,用來驗證 rollback 後餘額不變
//   - ApplyInTx 的 savepoint 語意:失敗不留下任何分錄,也不毒化呼叫端的 tx
//   - InTx 全域互斥,讓併發測試至少能驗證「兩個 tx 不會交錯」下的冪等行為
//
// 它**測不到**的是真正的鎖行為(advisory lock、FOR UPDATE 的等待與順序)。
// 那必須在 bettingpg 對真 Postgres 測,這裡只斷言「我們有依正確順序呼叫」。
type fakeTx struct{}

type fakeEntry struct {
	ID      int64
	UserID  int64
	Amount  int64
	Reason  ledger.Reason
	RefType string
	RefID   int64
}

type voteKey struct{ matchID, userID int64 }

type fakeDB struct {
	mu sync.Mutex

	tournaments map[string]*Tournament
	cfgs        map[int64]OddsConfig
	matches     map[string]*Match
	votes       map[voteKey]Side
	idem        map[string]*IdempotencyRecord
	bets        map[int64]*Bet
	legs        map[int64]*Leg
	events      []Event

	balances   map[int64]int64
	entries    []fakeEntry
	ledgerKeys map[string]*ledger.ApplyResult

	nextBetID, nextLegID, nextEntryID int64
	now                               time.Time

	// 觀測用,不參與 rollback —— 我們要看的是「呼叫了什麼」,不是「留下了什麼」。
	advisoryLocks []int64
	lockedUsers   []int64
	lockedBets    [][]int64
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		tournaments: map[string]*Tournament{},
		cfgs:        map[int64]OddsConfig{},
		matches:     map[string]*Match{},
		votes:       map[voteKey]Side{},
		idem:        map[string]*IdempotencyRecord{},
		bets:        map[int64]*Bet{},
		legs:        map[int64]*Leg{},
		balances:    map[int64]int64{},
		ledgerKeys:  map[string]*ledger.ApplyResult{},
		now:         time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC),
	}
}

// ── rollback 模擬 ────────────────────────────────────────────────

type dbState struct {
	votes      map[voteKey]Side
	idem       map[string]*IdempotencyRecord
	bets       map[int64]*Bet
	legs       map[int64]*Leg
	events     []Event
	balances   map[int64]int64
	entries    []fakeEntry
	ledgerKeys map[string]*ledger.ApplyResult
	nextBetID  int64
	nextLegID  int64
}

func (d *fakeDB) snapshot() dbState {
	return dbState{
		votes:      maps.Clone(d.votes),
		idem:       cloneIdem(d.idem),
		bets:       clonePtrMap(d.bets),
		legs:       clonePtrMap(d.legs),
		events:     slices.Clone(d.events),
		balances:   maps.Clone(d.balances),
		entries:    slices.Clone(d.entries),
		ledgerKeys: maps.Clone(d.ledgerKeys),
		nextBetID:  d.nextBetID,
		nextLegID:  d.nextLegID,
	}
}

func (d *fakeDB) restore(s dbState) {
	d.votes, d.idem, d.bets, d.legs = s.votes, s.idem, s.bets, s.legs
	d.events, d.balances, d.entries, d.ledgerKeys = s.events, s.balances, s.entries, s.ledgerKeys
	d.nextBetID, d.nextLegID = s.nextBetID, s.nextLegID
}

func clonePtrMap[K comparable, V any](m map[K]*V) map[K]*V {
	out := make(map[K]*V, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func cloneIdem(m map[string]*IdempotencyRecord) map[string]*IdempotencyRecord {
	out := make(map[string]*IdempotencyRecord, len(m))
	for k, v := range m {
		cp := *v
		cp.Response = slices.Clone(v.Response)
		out[k] = &cp
	}
	return out
}

// ── Repository ───────────────────────────────────────────────────

func (d *fakeDB) InTx(ctx context.Context, fn func(context.Context, fakeTx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	saved := d.snapshot()
	if err := fn(ctx, fakeTx{}); err != nil {
		d.restore(saved)
		return err
	}
	return nil
}

func (d *fakeDB) LockMatchSettle(_ context.Context, _ fakeTx, matchID int64) error {
	d.advisoryLocks = append(d.advisoryLocks, matchID)
	return nil
}

func (d *fakeDB) LockUser(_ context.Context, _ fakeTx, userID int64) error {
	d.lockedUsers = append(d.lockedUsers, userID)
	return nil
}

func (d *fakeDB) LockBets(_ context.Context, _ fakeTx, betIDs []int64) error {
	d.lockedBets = append(d.lockedBets, slices.Clone(betIDs))
	return nil
}

func (d *fakeDB) TournamentBySlug(_ context.Context, _ fakeTx, slug string) (*Tournament, error) {
	t, ok := d.tournaments[slug]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (d *fakeDB) OddsConfig(_ context.Context, _ fakeTx, tournamentID int64) (OddsConfig, error) {
	return d.cfgs[tournamentID], nil
}

func (d *fakeDB) MatchesByPublicIDs(_ context.Context, _ fakeTx, publicIDs []string) ([]Match, error) {
	out := make([]Match, 0, len(publicIDs))
	for _, id := range publicIDs {
		if m, ok := d.matches[id]; ok {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (d *fakeDB) VoteTallies(_ context.Context, _ fakeTx, matchIDs []int64) (map[int64]Tally, error) {
	out := make(map[int64]Tally, len(matchIDs))
	for _, id := range matchIDs {
		out[id] = Tally{}
	}
	for k, side := range d.votes {
		t, ok := out[k.matchID]
		if !ok {
			continue
		}
		if side == SideP1 {
			t.P1++
		} else {
			t.P2++
		}
		out[k.matchID] = t
	}
	return out, nil
}

func (d *fakeDB) UpsertVote(_ context.Context, _ fakeTx, matchID, userID int64, side Side) error {
	d.votes[voteKey{matchID, userID}] = side
	return nil
}

func (d *fakeDB) MyVotes(_ context.Context, _ fakeTx, matchIDs []int64, userID int64) (map[int64]Side, error) {
	out := map[int64]Side{}
	for _, id := range matchIDs {
		if s, ok := d.votes[voteKey{id, userID}]; ok {
			out[id] = s
		}
	}
	return out, nil
}

func (d *fakeDB) GetIdempotency(_ context.Context, _ fakeTx, key string) (*IdempotencyRecord, error) {
	rec, ok := d.idem[key]
	if !ok {
		return nil, nil
	}
	cp := *rec
	return &cp, nil
}

func (d *fakeDB) InsertIdempotency(_ context.Context, _ fakeTx, key, requestHash string) error {
	d.idem[key] = &IdempotencyRecord{RequestHash: requestHash}
	return nil
}

func (d *fakeDB) SaveIdempotencyResponse(_ context.Context, _ fakeTx, key string, response []byte) error {
	rec, ok := d.idem[key]
	if !ok {
		return errFake("冪等鍵不存在")
	}
	rec.Response = json.RawMessage(slices.Clone(response))
	return nil
}

func (d *fakeDB) InsertBet(_ context.Context, _ fakeTx, b NewBet) (int64, time.Time, error) {
	d.nextBetID++
	d.bets[d.nextBetID] = &Bet{
		ID:              d.nextBetID,
		PublicID:        b.PublicID,
		UserID:          b.UserID,
		TournamentID:    b.TournamentID,
		Stake:           b.Stake,
		PotentialPayout: b.PotentialPayout,
		Status:          BetOpen,
		CreatedAt:       d.now,
	}
	return d.nextBetID, d.now, nil
}

func (d *fakeDB) InsertLegs(_ context.Context, _ fakeTx, betID int64, legs []NewLeg) error {
	for _, l := range legs {
		// bet_legs 的 UNIQUE (bet_id, match_id)。服務層應該在這之前就擋掉。
		for _, ex := range d.legs {
			if ex.BetID == betID && ex.MatchID == l.MatchID {
				return errFake("撞到 bet_legs_bet_match_uq")
			}
		}
		if l.OddsMilli <= 1000 {
			return errFake("撞到 bet_legs_odds_check")
		}
		d.nextLegID++
		d.legs[d.nextLegID] = &Leg{
			ID: d.nextLegID, BetID: betID, MatchID: l.MatchID,
			Side: l.Side, OddsMilli: l.OddsMilli, Result: LegPending,
		}
	}
	return nil
}

func (d *fakeDB) SetBetStakeEntry(_ context.Context, _ fakeTx, betID, entryID int64) error {
	b, ok := d.bets[betID]
	if !ok {
		return errFake("注單不存在")
	}
	b.LedgerStakeEntryID = entryID
	return nil
}

func (d *fakeDB) PendingLegsByMatch(_ context.Context, _ fakeTx, matchID int64) ([]Leg, error) {
	var out []Leg
	for _, l := range d.legs {
		if l.MatchID == matchID && l.Result == LegPending {
			out = append(out, *l)
		}
	}
	slices.SortFunc(out, func(a, b Leg) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *fakeDB) UpdateLegResults(_ context.Context, _ fakeTx, legIDs []int64, result LegResult) error {
	for _, id := range legIDs {
		l, ok := d.legs[id]
		if !ok {
			return errFake("腿不存在")
		}
		l.Result = result
	}
	return nil
}

func (d *fakeDB) BetsByIDs(_ context.Context, _ fakeTx, betIDs []int64) ([]Bet, error) {
	out := make([]Bet, 0, len(betIDs))
	for _, id := range betIDs {
		if b, ok := d.bets[id]; ok {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (d *fakeDB) LegsByBetIDs(_ context.Context, _ fakeTx, betIDs []int64) (map[int64][]Leg, error) {
	want := make(map[int64]struct{}, len(betIDs))
	for _, id := range betIDs {
		want[id] = struct{}{}
	}
	out := map[int64][]Leg{}
	for _, l := range d.legs {
		if _, ok := want[l.BetID]; ok {
			out[l.BetID] = append(out[l.BetID], *l)
		}
	}
	for k := range out {
		slices.SortFunc(out[k], func(a, b Leg) int { return cmp.Compare(a.ID, b.ID) })
	}
	return out, nil
}

func (d *fakeDB) UpdateBet(_ context.Context, _ fakeTx, u BetUpdate) error {
	b, ok := d.bets[u.BetID]
	if !ok {
		return errFake("注單不存在")
	}
	b.Status = u.Status
	if u.PotentialPayout != nil {
		b.PotentialPayout = *u.PotentialPayout
		b.PayoutRecalculated = true
	}
	if u.LedgerRefundEntryID != nil {
		b.LedgerRefundEntryID = *u.LedgerRefundEntryID
	}
	// bets_settled_at_check:status='open' ⇔ settled_at IS NULL。
	if u.Status == BetOpen {
		b.SettledAt = nil
	} else {
		t := d.now
		b.SettledAt = &t
	}
	return nil
}

func (d *fakeDB) BetsByUser(_ context.Context, tx fakeTx, tournamentID, userID int64, openOnly bool) ([]Bet, error) {
	var ids []int64
	for id, b := range d.bets {
		if b.TournamentID != tournamentID || b.UserID != userID {
			continue
		}
		if openOnly && b.Status != BetOpen {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	legs, _ := d.LegsByBetIDs(context.Background(), tx, ids)
	out := make([]Bet, 0, len(ids))
	for _, id := range ids {
		b := *d.bets[id]
		b.Legs = legs[id]
		out = append(out, b)
	}
	return out, nil
}

func (d *fakeDB) AppendEvents(_ context.Context, _ fakeTx, events []Event) error {
	d.events = append(d.events, events...)
	return nil
}

// ── Ledger ───────────────────────────────────────────────────────

func (d *fakeDB) ApplyInTx(_ context.Context, _ fakeTx, p ledger.ApplyParams) (*ledger.ApplyResult, error) {
	if p.IdempotencyKey == "" {
		return nil, ledger.ErrInvalidOp
	}
	if prev, ok := d.ledgerKeys[p.IdempotencyKey]; ok {
		r := *prev
		r.Replayed = true
		return &r, nil
	}

	// savepoint 語意:任何一個 op 失敗,這次呼叫不留下任何痕跡,
	// 但呼叫端的 tx 仍然可用(由呼叫端決定要不要 rollback)。
	nextBal := maps.Clone(d.balances)
	var entries []fakeEntry
	res := &ledger.ApplyResult{}
	for _, op := range p.Ops {
		if op.Amount == 0 || !op.Reason.Valid() {
			return nil, ledger.ErrInvalidOp
		}
		bal := nextBal[op.UserID] + op.Amount
		if bal < 0 {
			return nil, ledger.ErrInsufficientBalance
		}
		nextBal[op.UserID] = bal
		d.nextEntryID++
		e := fakeEntry{ID: d.nextEntryID, UserID: op.UserID, Amount: op.Amount, Reason: op.Reason}
		if op.RefType != nil {
			e.RefType = *op.RefType
		}
		if op.RefID != nil {
			e.RefID = *op.RefID
		}
		entries = append(entries, e)
		res.Entries = append(res.Entries, ledger.EntryResult{
			EntryID: e.ID, UserID: op.UserID, Currency: op.Currency, NewBalance: bal,
		})
	}
	d.balances = nextBal
	d.entries = append(d.entries, entries...)
	d.ledgerKeys[p.IdempotencyKey] = res
	return res, nil
}

// ── 測試用的建構與查詢輔助 ────────────────────────────────────────

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
func errFake(s string) error    { return fakeErr(s) }

func (d *fakeDB) addTournament(id int64, slug string, cfg OddsConfig) {
	d.tournaments[slug] = &Tournament{ID: id, Slug: slug}
	d.cfgs[id] = cfg
}

func (d *fakeDB) addMatch(id int64, publicID string, tournamentID int64, status MatchStatus, p1, p2 Participant) {
	d.matches[publicID] = &Match{
		ID: id, PublicID: publicID, TournamentID: tournamentID,
		Round: 1, Slot: int(id), Status: status, ResultKind: ResultNormal,
		P1: p1, P2: p2,
	}
}

// finish 把場次標成已分勝負。kind 決定走結算還是退款路徑。
func (d *fakeDB) finish(publicID string, winner Side, kind ResultKind) {
	m := d.matches[publicID]
	m.Status = MatchDone
	m.ResultKind = kind
	m.WinnerPlayerID = m.Participant(winner).PlayerID
}

func (d *fakeDB) credit(userID, amount int64) { d.balances[userID] += amount }

func (d *fakeDB) balance(userID int64) int64 { return d.balances[userID] }

func (d *fakeDB) betByPublicID(publicID string) *Bet {
	for _, b := range d.bets {
		if b.PublicID == publicID {
			return b
		}
	}
	return nil
}

func (d *fakeDB) entriesFor(userID int64) []fakeEntry {
	var out []fakeEntry
	for _, e := range d.entries {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	return out
}

// sumEntries 是「餘額 = SUM(分錄)」的重算。每個動錢的測試都該用它對一次帳。
func (d *fakeDB) sumEntries(userID int64) int64 {
	var sum int64
	for _, e := range d.entries {
		if e.UserID == userID {
			sum += e.Amount
		}
	}
	return sum
}

func (d *fakeDB) topics() []string {
	out := make([]string, 0, len(d.events))
	for _, e := range d.events {
		out = append(out, e.Topic)
	}
	return out
}

// player 是綁了平台帳號報名的選手(tournament_players.user_id 有值)。
func player(playerID, userID int64, name string) Participant {
	return Participant{PlayerID: playerID, DisplayName: name, PlayerUserID: userID, FencerUserID: userID}
}

// unboundPlayer 是純活動報名、但劍士本人綁過平台帳號的選手 ——
// tournament_players.user_id 是 NULL,只有 fencers.user_id 比對得到。
func unboundPlayer(playerID, fencerUserID int64, name string) Participant {
	return Participant{PlayerID: playerID, DisplayName: name, FencerUserID: fencerUserID}
}

// anonPlayer 是兩邊都沒綁平台帳號的選手。擋不住他賭自己,這是已知的缺口。
func anonPlayer(playerID int64, name string) Participant {
	return Participant{PlayerID: playerID, DisplayName: name}
}

// 編譯期確認 fakeDB 真的滿足兩個埠。
var (
	_ Repository[fakeTx] = (*fakeDB)(nil)
	_ Ledger[fakeTx]     = (*fakeDB)(nil)
)
