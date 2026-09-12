package prize

import (
	"context"
	"sync"

	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// ── Repository ───────────────────────────────────────────────────

type fakeRepo struct {
	tournaments map[string]tournament.Tournament
	players     map[int64][]tournament.Player
	matches     map[int64][]match.Match
	// failMatches 讓測試驗證「用不到對戰表時就不該去讀它」。
	failMatches error
}

func (r *fakeRepo) TournamentBySlug(_ context.Context, slug string) (tournament.Tournament, error) {
	t, ok := r.tournaments[slug]
	if !ok {
		return tournament.Tournament{}, tournament.ErrTournamentNotFound
	}
	return t, nil
}

func (r *fakeRepo) ListPlayers(_ context.Context, tournamentID int64) ([]tournament.Player, error) {
	return append([]tournament.Player(nil), r.players[tournamentID]...), nil
}

func (r *fakeRepo) ListMatches(_ context.Context, tournamentID int64) ([]match.Match, error) {
	if r.failMatches != nil {
		return nil, r.failMatches
	}
	return append([]match.Match(nil), r.matches[tournamentID]...), nil
}

// ── Ledger ───────────────────────────────────────────────────────

// fakeLedger 複製 ledgerpg 的冪等語意:
// 同鍵同內容回放、同鍵不同內容衝突、佔了鍵但還沒有結果時 ErrInFlight。
//
// 全程持鎖 —— 真實的 ledgerpg 靠 idempotency_keys 的主鍵與餘額列鎖把同鍵請求
// 排成序,這裡用一把 mutex 得到同樣的可觀察行為,併發測試才有意義。
type fakeLedger struct {
	mu       sync.Mutex
	keys     map[string]*ledger.ApplyResult
	hashes   map[string]string
	balances map[int64]int64
	entries  []ledger.Op
	calls    int
	nextID   int64
	// fail 依冪等鍵注入錯誤,模擬帳本回 ErrInFlight 之類的情況。
	fail map[string]error
	// inFlight 的鍵表示「別人佔了鍵但還沒寫結果」。
	inFlight map[string]bool
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		keys:     map[string]*ledger.ApplyResult{},
		hashes:   map[string]string{},
		balances: map[int64]int64{},
		fail:     map[string]error{},
		inFlight: map[string]bool{},
	}
}

func (l *fakeLedger) Apply(_ context.Context, p ledger.ApplyParams) (*ledger.ApplyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++

	if p.IdempotencyKey == "" || p.RequestHash == "" || len(p.Ops) == 0 {
		return nil, ledger.ErrInvalidOp
	}
	if err := l.fail[p.IdempotencyKey]; err != nil {
		return nil, err
	}
	if h, ok := l.hashes[p.IdempotencyKey]; ok {
		if h != p.RequestHash {
			return nil, ledger.ErrIdempotencyConflict
		}
		if l.inFlight[p.IdempotencyKey] {
			return nil, ledger.ErrInFlight
		}
		r := *l.keys[p.IdempotencyKey]
		r.Replayed = true
		return &r, nil
	}

	res := &ledger.ApplyResult{}
	for _, op := range p.Ops {
		if op.Amount == 0 || !op.Reason.Valid() || op.UserID <= 0 || op.Currency == "" {
			return nil, ledger.ErrInvalidOp
		}
		if l.balances[op.UserID]+op.Amount < 0 {
			return nil, ledger.ErrInsufficientBalance
		}
		l.balances[op.UserID] += op.Amount
		l.nextID++
		l.entries = append(l.entries, op)
		res.Entries = append(res.Entries, ledger.EntryResult{
			EntryID: l.nextID, UserID: op.UserID, Currency: op.Currency,
			NewBalance: l.balances[op.UserID],
		})
	}
	l.hashes[p.IdempotencyKey] = p.RequestHash
	l.keys[p.IdempotencyKey] = res
	return res, nil
}

func (l *fakeLedger) balance(userID int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[userID]
}

func (l *fakeLedger) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *fakeLedger) entryCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// ── 場景組裝 ─────────────────────────────────────────────────────

// scene 是一屆四人賽事:P1 勝 P2(決賽)、P3 與 P4 首輪被淘汰。
// 需要別的形狀時就地改 fields,不另外造第二套建構器。
type scene struct {
	repo *fakeRepo
	led  *fakeLedger
	svc  *Service
}

const (
	slug        = "baiye-1"
	tPublicID   = "01TOURNAMENT000000000000AA"
	prizeCoin   = "coin"
	judgeUserID = int64(9001)
)

// player 造一位選手。userID = 0 表示尚未綁定平台帳號。
func player(id int64, name string, userID int64, status tournament.PlayerStatus) tournament.Player {
	p := tournament.Player{
		ID:           id,
		PublicID:     "01PLAYER00000000000000000" + string(rune('A'+id)),
		TournamentID: 1,
		DisplayName:  name,
		Status:       status,
	}
	if userID != 0 {
		u := userID
		p.UserID = &u
	}
	return p
}

func finalMatch(winnerID int64, p1, p2 tournament.Player) match.Match {
	return match.Match{
		ID: 100, PublicID: "01MATCHFINAL0000000000000", TournamentID: 1,
		Round: 2, Slot: 0, Status: match.StatusDone, ResultKind: match.ResultNormal,
		WinnerPlayerID: winnerID,
		P1:             match.Player{ID: p1.ID, PublicID: p1.PublicID, DisplayName: p1.DisplayName},
		P2:             match.Player{ID: p2.ID, PublicID: p2.PublicID, DisplayName: p2.DisplayName},
	}
}

func semiMatch(id int64, slot int, p1, p2 tournament.Player) match.Match {
	return match.Match{
		ID: id, PublicID: "01MATCHSEMI00000000000000", TournamentID: 1,
		Round: 1, Slot: slot, Status: match.StatusDone, ResultKind: match.ResultNormal,
		WinnerPlayerID: p1.ID,
		P1:             match.Player{ID: p1.ID, PublicID: p1.PublicID, DisplayName: p1.DisplayName},
		P2:             match.Player{ID: p2.ID, PublicID: p2.PublicID, DisplayName: p2.DisplayName},
	}
}

// newScene 組出預設場景。config 由呼叫端給,那是每個測試真正要變的東西。
func newScene(config string, players []tournament.Player, matches []match.Match) *scene {
	repo := &fakeRepo{
		tournaments: map[string]tournament.Tournament{
			slug: {
				ID: 1, PublicID: tPublicID, Slug: slug, Name: "百業試鋒 第一屆",
				Phase: tournament.PhaseFinished, ConfigRaw: []byte(config),
			},
		},
		players: map[int64][]tournament.Player{1: players},
		matches: map[int64][]match.Match{1: matches},
	}
	led := newFakeLedger()
	return &scene{repo: repo, led: led, svc: New(repo, led, prizeCoin)}
}

// fourPlayers 是最常用的名單:全部綁好帳號。
func fourPlayers() []tournament.Player {
	return []tournament.Player{
		player(1, "甲", 101, tournament.PlayerActive),
		player(2, "乙", 102, tournament.PlayerEliminated),
		player(3, "丙", 103, tournament.PlayerEliminated),
		player(4, "丁", 104, tournament.PlayerEliminated),
	}
}

func fourMatches(ps []tournament.Player) []match.Match {
	return []match.Match{
		semiMatch(10, 0, ps[0], ps[2]),
		semiMatch(11, 1, ps[1], ps[3]),
		finalMatch(ps[0].ID, ps[0], ps[1]),
	}
}

func (s *scene) award(ctx context.Context) (*AwardResult, error) {
	return s.svc.AwardPrizes(ctx, AwardParams{TournamentSlug: slug, ActorUserID: judgeUserID})
}
