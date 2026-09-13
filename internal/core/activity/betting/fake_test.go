package betting

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/rules"
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

type voteKey struct{ marketID, userID int64 }

type fakeDB struct {
	mu sync.Mutex

	tournaments map[string]*Tournament
	rules       map[int64]Rules
	matches     map[string]*Match
	rounds      map[int64][]RoundResult
	markets     map[int64]*Market
	votes       map[voteKey]Outcome
	idem        map[string]*IdempotencyRecord
	bets        map[int64]*Bet
	legs        map[int64]*Leg

	balances   map[int64]int64
	entries    []fakeEntry
	ledgerKeys map[string]*ledger.ApplyResult

	nextBetID, nextLegID, nextEntryID, nextMarketID int64
	now                                             time.Time

	// 觀測用,不參與 rollback —— 我們要看的是「呼叫了什麼」,不是「留下了什麼」。
	advisoryLocks  []int64
	lockedUsers    []int64
	lockedBets     [][]int64
	insertBetCalls int
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		tournaments: map[string]*Tournament{},
		rules:       map[int64]Rules{},
		matches:     map[string]*Match{},
		rounds:      map[int64][]RoundResult{},
		markets:     map[int64]*Market{},
		votes:       map[voteKey]Outcome{},
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
	markets      map[int64]*Market
	votes        map[voteKey]Outcome
	idem         map[string]*IdempotencyRecord
	bets         map[int64]*Bet
	legs         map[int64]*Leg
	balances     map[int64]int64
	entries      []fakeEntry
	ledgerKeys   map[string]*ledger.ApplyResult
	nextBetID    int64
	nextLegID    int64
	nextMarketID int64
}

func (d *fakeDB) snapshot() dbState {
	return dbState{
		markets:      clonePtrMap(d.markets),
		votes:        maps.Clone(d.votes),
		idem:         cloneIdem(d.idem),
		bets:         clonePtrMap(d.bets),
		legs:         clonePtrMap(d.legs),
		balances:     maps.Clone(d.balances),
		entries:      slices.Clone(d.entries),
		ledgerKeys:   maps.Clone(d.ledgerKeys),
		nextBetID:    d.nextBetID,
		nextLegID:    d.nextLegID,
		nextMarketID: d.nextMarketID,
	}
}

func (d *fakeDB) restore(s dbState) {
	d.markets, d.votes, d.idem, d.bets, d.legs = s.markets, s.votes, s.idem, s.bets, s.legs
	d.balances, d.entries, d.ledgerKeys = s.balances, s.entries, s.ledgerKeys
	d.nextBetID, d.nextLegID, d.nextMarketID = s.nextBetID, s.nextLegID, s.nextMarketID
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

func (d *fakeDB) Rules(_ context.Context, _ fakeTx, tournamentID int64) (Rules, error) {
	return d.rules[tournamentID], nil
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

func (d *fakeDB) RoundsByMatch(_ context.Context, _ fakeTx, matchID int64) ([]RoundResult, error) {
	out := slices.Clone(d.rounds[matchID])
	slices.SortFunc(out, func(a, b RoundResult) int { return cmp.Compare(a.RoundNo, b.RoundNo) })
	return out, nil
}

func (d *fakeDB) InsertMarkets(_ context.Context, _ fakeTx, matchID int64, ms []NewMarket) ([]Market, error) {
	out := make([]Market, 0, len(ms))
	for _, nm := range ms {
		// markets_match_kind_line_uq (match_id, kind, COALESCE(round_no,0), line_seconds)。
		for _, ex := range d.markets {
			if ex.MatchID == matchID && ex.Kind == nm.Kind && ex.RoundNo == nm.RoundNo && ex.LineSeconds == nm.LineSeconds {
				return nil, ErrMarketsExist
			}
		}
		// markets_round_no_check:(kind IN (round_winner,duration)) = (round_no IS NOT NULL)。
		if nm.Kind.PerRound() != (nm.RoundNo != 0) {
			return nil, errFake("撞到 markets_round_no_check")
		}
		d.nextMarketID++
		mk := &Market{
			ID: d.nextMarketID, PublicID: nm.PublicID, MatchID: matchID,
			Kind: nm.Kind, RoundNo: nm.RoundNo, LineSeconds: nm.LineSeconds, Status: MarketOpen,
		}
		d.markets[mk.ID] = mk
		out = append(out, *mk)
	}
	return out, nil
}

func (d *fakeDB) MarketsByMatches(_ context.Context, _ fakeTx, matchIDs []int64) ([]Market, error) {
	var out []Market
	for _, mk := range d.markets {
		if slices.Contains(matchIDs, mk.MatchID) {
			out = append(out, *mk)
		}
	}
	slices.SortFunc(out, func(a, b Market) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *fakeDB) MarketsByPublicIDs(_ context.Context, _ fakeTx, publicIDs []string) ([]MarketRef, error) {
	var out []MarketRef
	for _, mk := range d.markets {
		if !slices.Contains(publicIDs, mk.PublicID) {
			continue
		}
		ref := MarketRef{Market: *mk}
		for _, m := range d.matches {
			if m.ID == mk.MatchID {
				ref.MatchPublicID = m.PublicID
			}
		}
		out = append(out, ref)
	}
	return out, nil
}

func (d *fakeDB) UnsettledMarketsByMatch(_ context.Context, _ fakeTx, matchID int64) ([]Market, error) {
	var out []Market
	for _, mk := range d.markets {
		if mk.MatchID == matchID && (mk.Status == MarketOpen || mk.Status == MarketClosed) {
			out = append(out, *mk)
		}
	}
	slices.SortFunc(out, func(a, b Market) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *fakeDB) CloseMarkets(_ context.Context, _ fakeTx, matchID int64) (int, error) {
	n := 0
	for _, mk := range d.markets {
		if mk.MatchID == matchID && mk.Status == MarketOpen {
			mk.Status = MarketClosed
			n++
		}
	}
	return n, nil
}

func (d *fakeDB) SettleMarket(_ context.Context, _ fakeTx, marketID int64, status MarketStatus) error {
	mk, ok := d.markets[marketID]
	if !ok || (mk.Status != MarketOpen && mk.Status != MarketClosed) {
		return ErrMarketSettled
	}
	mk.Status = status
	return nil
}

func (d *fakeDB) VoteTallies(_ context.Context, _ fakeTx, marketIDs []int64) (map[int64]Tally, error) {
	out := make(map[int64]Tally, len(marketIDs))
	for k, o := range d.votes {
		if !slices.Contains(marketIDs, k.marketID) {
			continue
		}
		t := out[k.marketID]
		if t == nil {
			t = Tally{}
			out[k.marketID] = t
		}
		t[o]++
	}
	return out, nil
}

func (d *fakeDB) UpsertVote(_ context.Context, _ fakeTx, marketID, userID int64, o Outcome) error {
	d.votes[voteKey{marketID, userID}] = o
	return nil
}

func (d *fakeDB) MyVotes(_ context.Context, _ fakeTx, marketIDs []int64, userID int64) (map[int64]Outcome, error) {
	out := map[int64]Outcome{}
	for _, id := range marketIDs {
		if o, ok := d.votes[voteKey{id, userID}]; ok {
			out[id] = o
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
	d.insertBetCalls++
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
				return ErrDuplicateMatchInParlay
			}
		}
		// bet_legs_market_match_fkey (market_id, match_id) → markets (id, match_id)。
		mk, ok := d.markets[l.MarketID]
		if !ok || mk.MatchID != l.MatchID {
			return errFake("撞到 bet_legs_market_match_fkey")
		}
		if l.OddsMilli <= 1000 {
			return errFake("撞到 bet_legs_odds_check")
		}
		d.nextLegID++
		d.legs[d.nextLegID] = &Leg{
			ID: d.nextLegID, BetID: betID, MatchID: l.MatchID, MarketID: l.MarketID,
			Outcome: l.Outcome, OddsMilli: l.OddsMilli, Result: LegPending,
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

func (d *fakeDB) PendingLegsByMarket(_ context.Context, _ fakeTx, marketID int64) ([]Leg, error) {
	var out []Leg
	for _, l := range d.legs {
		if l.MarketID == marketID && l.Result == LegPending {
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
		if l.Result != LegPending {
			return ErrLedgerStateConflict
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
	if b.Status != BetOpen {
		return ErrLedgerStateConflict
	}
	b.Status = u.Status
	if u.PotentialPayout != nil {
		b.PotentialPayout = *u.PotentialPayout
		b.PayoutRecalculated = true
	}
	if u.LedgerRefundEntryID != nil {
		b.LedgerRefundEntryID = *u.LedgerRefundEntryID
	}
	if u.LedgerPayoutEntryID != nil {
		b.LedgerPayoutEntryID = *u.LedgerPayoutEntryID
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

// fullRules 是開發庫那種 config:三局兩勝、四種盤口、duration 線 90 秒。
func fullRules() Rules {
	cfg := rules.Default()
	cfg.Format.BestOf = 3
	cfg.Betting.Markets = []rules.MarketRule{
		{Kind: rules.MarketMatchWinner},
		{Kind: rules.MarketRoundWinner},
		{Kind: rules.MarketDuration, LineSeconds: 90},
		{Kind: rules.MarketScore},
	}
	return Rules{Config: cfg, MaxStake: DefaultMaxStake}
}

func (d *fakeDB) addTournament(id int64, slug string, r Rules) {
	d.tournaments[slug] = &Tournament{ID: id, Slug: slug}
	d.rules[id] = r
}

// addMatch 建一場 ready 的比賽並依該屆規則建好盤口(與 OpenMarketsInTx 同一條路)。
func (d *fakeDB) addMatch(id int64, publicID string, tournamentID int64, status MatchStatus, p1, p2 Participant) {
	d.matches[publicID] = &Match{
		ID: id, PublicID: publicID, TournamentID: tournamentID,
		Round: 1, Slot: int(id), Status: status, ResultKind: ResultNormal,
		P1: p1, P2: p2,
	}
	plan := d.rules[tournamentID].MarketsFor()
	for i := range plan {
		plan[i].PublicID = publicID + "-" + string(plan[i].Kind) + "-" + strconv.Itoa(plan[i].RoundNo)
	}
	if _, err := d.InsertMarkets(context.Background(), fakeTx{}, id, plan); err != nil {
		panic(err)
	}
}

// market 找某場某種盤口(逐回合盤口指定 roundNo,其餘傳 0)。
func (d *fakeDB) market(matchPublicID string, kind MarketKind, roundNo int) *Market {
	m := d.matches[matchPublicID]
	for _, mk := range d.markets {
		if mk.MatchID == m.ID && mk.Kind == kind && mk.RoundNo == roundNo {
			return mk
		}
	}
	return nil
}

// playRound 記一回合:seconds 是時長,winner 是 p1 / p2。
func (d *fakeDB) playRound(matchPublicID string, roundNo int, winner Outcome, seconds int64) {
	m := d.matches[matchPublicID]
	start := d.now.Add(time.Duration(roundNo) * time.Hour)
	end := start.Add(time.Duration(seconds) * time.Second)
	var winnerID int64
	if winner == OutcomeP1 {
		winnerID = m.P1.PlayerID
	} else {
		winnerID = m.P2.PlayerID
	}
	d.rounds[m.ID] = append(d.rounds[m.ID], RoundResult{
		RoundNo: roundNo, StartedAt: start, FinishedAt: &end, WinnerPlayerID: winnerID,
	})
}

// finish 把場次標成已分勝負。kind 決定走結算還是退款路徑。
func (d *fakeDB) finish(publicID string, winner Outcome, kind ResultKind) {
	m := d.matches[publicID]
	m.Status = MatchDone
	m.ResultKind = kind
	switch winner {
	case OutcomeP1:
		m.WinnerPlayerID = m.P1.PlayerID
	case OutcomeP2:
		m.WinnerPlayerID = m.P2.PlayerID
	}
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
