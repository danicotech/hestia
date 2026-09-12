package matchpg_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/matchpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(hestia/CLAUDE.md):要測的正是資料庫的鎖行為、
// 狀態轉移的 WHERE 守門與 rollback,mock 掉這些等於什麼都沒測。
// 全 package 共用一個容器,各測試用自己的賽事 + 自己的裁判使用者互相隔離。

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	p, cleanup, err := testdb.StartShared(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "testdb:", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// ── 讓武與下注的替身 ────────────────────────────────────────────
//
// handicappg / bettingpg 由別的 adapter 負責,本 package 不該連著它們一起測
// (那會讓一個 BP 計價的 bug 表現成「晉級錯了」)。但兩個替身都**真的動資料庫**:
//   - 封盤走 handicap 的那一支 UPDATE(matches 的封盤寫入只能有一個權威),
//     否則 locked → live 這條守門在測試裡就被繞過去了,等於沒測。
//   - 結算/退款真的改 activity.bets,rollback 測試才驗得出「注單也還原了」。

var errStub = errors.New("替身注入的失敗")

// 兩個替身都有 mu:併發測試會有兩位裁判同時呼叫它們,而那正是要測的場景。
type stubHandicaps struct {
	mu        sync.Mutex
	granted   []string
	locked    []string
	failGrant bool
}

func (h *stubHandicaps) GrantBudgetInTx(_ context.Context, _ pgx.Tx, matchPublicID string) (*handicap.Budget, error) {
	if h.failGrant {
		return nil, errStub
	}
	h.mu.Lock()
	h.granted = append(h.granted, matchPublicID)
	h.mu.Unlock()
	// nil = 本場無讓武(同段對決)。那不是錯誤,埠註解寫得很清楚。
	return nil, nil
}

func (h *stubHandicaps) LockInTx(ctx context.Context, tx pgx.Tx, matchPublicID string) (*handicap.MatchHandicaps, error) {
	q := db.New(tx)
	m, err := q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, fmt.Errorf("替身讀場次: %w", err)
	}
	now := time.Now()
	n, err := q.LockMatchHandicaps(ctx, db.LockMatchHandicapsParams{LockedAt: &now, MatchID: m.ID})
	if err != nil {
		return nil, fmt.Errorf("替身封盤: %w", err)
	}
	if n == 0 {
		return nil, handicap.ErrAlreadyLocked
	}
	h.mu.Lock()
	h.locked = append(h.locked, matchPublicID)
	h.mu.Unlock()
	return &handicap.MatchHandicaps{
		MatchPublicID: matchPublicID, Status: "locked", LockedAt: &now, Revealed: true,
	}, nil
}

type stubBets struct {
	mu         sync.Mutex
	settled    []string
	voided     []string
	failSettle bool
}

// SettleMatchInTx 把押到這場的注單標成 won。
//
// 先檢查場次真的已經是 done + normal:那正是「betting 讀到的必須是我們剛寫下的
// 勝者」這條順序要求,替身幫忙把它釘住 —— 順序寫反時測試會直接失敗,
// 而不是靜靜地算出一樣的結果。
func (b *stubBets) SettleMatchInTx(ctx context.Context, tx pgx.Tx, matchPublicID string) (*betting.SettleResult, error) {
	if b.failSettle {
		return nil, errStub
	}
	m, err := db.New(tx).GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, fmt.Errorf("替身讀場次: %w", err)
	}
	if m.Status != string(match.StatusDone) || m.WinnerPlayerID == nil {
		return nil, fmt.Errorf("結算時場次還不是 done:status=%s", m.Status)
	}
	if m.ResultKind == string(match.ResultWalkover) {
		return nil, betting.ErrWalkoverMatch
	}
	n, err := markBets(ctx, tx, m.ID, "won")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.settled = append(b.settled, matchPublicID)
	b.mu.Unlock()
	return &betting.SettleResult{MatchPublicID: matchPublicID, Bets: make([]betting.BetOutcome, n)}, nil
}

// VoidMatchInTx 把押到這場的注單標成 void(全額退款)。只對 walkover 成立。
func (b *stubBets) VoidMatchInTx(ctx context.Context, tx pgx.Tx, matchPublicID string) (*betting.SettleResult, error) {
	m, err := db.New(tx).GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, fmt.Errorf("替身讀場次: %w", err)
	}
	if m.ResultKind != string(match.ResultWalkover) {
		return nil, fmt.Errorf("退款路徑只對 walkover 成立:result_kind=%s", m.ResultKind)
	}
	n, err := markBets(ctx, tx, m.ID, "void")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.voided = append(b.voided, matchPublicID)
	b.mu.Unlock()
	return &betting.SettleResult{MatchPublicID: matchPublicID, Bets: make([]betting.BetOutcome, n)}, nil
}

func markBets(ctx context.Context, tx pgx.Tx, matchID int64, status string) (int, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE activity.bets b
		   SET status = $2, settled_at = now()
		  FROM activity.bet_legs l
		 WHERE l.bet_id = b.id AND l.match_id = $1 AND b.status = 'open'`, matchID, status)
	if err != nil {
		return 0, fmt.Errorf("替身結算注單: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ── 測試資料 ────────────────────────────────────────────────────

type player struct {
	id       int64
	publicID string
	fencerID int64
	name     string
}

type mrec struct {
	id       int64
	publicID string
	round    int
	slot     int
}

type fixture struct {
	t     *testing.T
	repo  *matchpg.Service
	hcap  *stubHandicaps
	bets  *stubBets
	svc   *match.Service[pgx.Tx]
	id    int64
	slug  string
	judge int64
}

// newFixture 建一屆 in_progress 的賽事與一位裁判(真的 platform.users 列 ——
// admin_audit_logs.actor_user_id 是 NOT NULL 且有 FK,假 id 會寫不進去)。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	setup(t)
	ctx := context.Background()

	var judge int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&judge); err != nil {
		t.Fatalf("建裁判使用者: %v", err)
	}

	slug := "t-" + randomSuffix(t)
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase, config)
		VALUES (gen_random_uuid()::text, $1, $2, 1, 'in_progress', '{}'::jsonb)
		RETURNING id`, slug, "測試賽 "+slug).Scan(&id); err != nil {
		t.Fatalf("建賽事: %v", err)
	}

	f := &fixture{
		t: t, repo: matchpg.New(pool),
		hcap: &stubHandicaps{}, bets: &stubBets{},
		id: id, slug: slug, judge: judge,
	}
	f.svc = match.NewService[pgx.Tx](f.repo, f.hcap, f.bets)
	return f
}

// useRealHandicaps 把替身換成真的 handicappg,用來驗接線本身。
//
// 其餘測試不用它:讓武的計價規則有自己的 package 在測,混進來只會讓一個 BP
// 計價的 bug 表現成「晉級錯了」。
func (f *fixture) useRealHandicaps() {
	f.t.Helper()
	f.svc = match.NewService[pgx.Tx](f.repo, matchpg.NewHandicaps(
		func(tx pgx.Tx) handicap.Repository { return handicappg.BindTx(tx) },
	), f.bets)
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&s); err != nil {
		t.Fatalf("取亂數: %v", err)
	}
	return s[:8]
}

// player 建一位參賽者(含跨屆檔案)。rank 0 = 未評段。
func (f *fixture) player(name string, rank int16) player {
	f.t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(f.t)

	var fencerID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.fencers (public_id, game_id, discord_name)
		VALUES (gen_random_uuid()::text, $1, $2) RETURNING id`,
		name+"-"+suffix, name).Scan(&fencerID); err != nil {
		f.t.Fatalf("建 fencer %s: %v", name, err)
	}

	var rankArg any
	if rank > 0 {
		rankArg = rank
	}
	p := player{fencerID: fencerID, name: name}
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.tournament_players
		  (public_id, tournament_id, fencer_id, display_name, rank_level,
		   passcode_hash, discord_name)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, 'x', $3)
		RETURNING id, public_id`,
		f.id, fencerID, name, rankArg).Scan(&p.id, &p.publicID); err != nil {
		f.t.Fatalf("建參賽者 %s: %v", name, err)
	}
	return p
}

// match 建一場比賽。playerID 傳 0 表示該側未定(等上一輪)。
func (f *fixture) match(round, slot int, p1, p2 int64, status match.Status) mrec {
	f.t.Helper()
	m := mrec{round: round, slot: slot}
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO activity.matches
		  (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, status)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6)
		RETURNING id, public_id`,
		f.id, round, slot, nullID(p1), nullID(p2), string(status),
	).Scan(&m.id, &m.publicID); err != nil {
		f.t.Fatalf("建場次 r%ds%d: %v", round, slot, err)
	}
	return m
}

// doneMatch 建一場已完賽的比賽,給「棄賽發生在賽程中段」這類情境鋪路。
func (f *fixture) doneMatch(round, slot int, p1, p2, winner int64) mrec {
	f.t.Helper()
	m := f.match(round, slot, p1, p2, match.StatusPending)
	if _, err := pool.Exec(context.Background(), `
		UPDATE activity.matches
		   SET status = 'done', winner_player_id = $2, finished_at = now(),
		       handicap_locked_at = now()
		 WHERE id = $1`, m.id, winner); err != nil {
		f.t.Fatalf("設定已完賽場次 r%ds%d: %v", round, slot, err)
	}
	return m
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ── 斷言用的讀取 ────────────────────────────────────────────────

type matchState struct {
	Status     match.Status
	ResultKind match.ResultKind
	Winner     int64
	P1, P2     int64
	Open       bool
	LockedAt   *time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	StreamURL  *string
}

func (f *fixture) state(publicID string) matchState {
	f.t.Helper()
	var st matchState
	var winner, p1, p2 *int64
	if err := pool.QueryRow(context.Background(), `
		SELECT status, result_kind, winner_player_id, p1_player_id, p2_player_id,
		       handicap_open, handicap_locked_at, started_at, finished_at, stream_url
		  FROM activity.matches WHERE public_id = $1`, publicID,
	).Scan(&st.Status, &st.ResultKind, &winner, &p1, &p2,
		&st.Open, &st.LockedAt, &st.StartedAt, &st.FinishedAt, &st.StreamURL); err != nil {
		f.t.Fatalf("讀場次狀態 %s: %v", publicID, err)
	}
	st.Winner, st.P1, st.P2 = derefID(winner), derefID(p1), derefID(p2)
	return st
}

func derefID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (f *fixture) playerStatus(publicID string) string {
	f.t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM activity.tournament_players WHERE public_id = $1`, publicID).Scan(&s); err != nil {
		f.t.Fatalf("讀選手狀態 %s: %v", publicID, err)
	}
	return s
}

func (f *fixture) record(fencerID int64) (wins, losses int) {
	f.t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT wins, losses FROM activity.fencers WHERE id = $1`, fencerID).Scan(&wins, &losses); err != nil {
		f.t.Fatalf("讀戰績 fencer=%d: %v", fencerID, err)
	}
	return wins, losses
}

type auditRow struct {
	Action     string
	TargetType string
	TargetID   int64
	After      map[string]any
	Reason     string
}

// audits 只看本 fixture 那位裁判寫的,所以各測試之間不會互相污染。
func (f *fixture) audits() []auditRow {
	f.t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT action, coalesce(target_type, ''), coalesce(target_id, 0), coalesce(after, '{}'::jsonb), reason
		  FROM platform.admin_audit_logs
		 WHERE actor_user_id = $1
		 ORDER BY id`, f.judge)
	if err != nil {
		f.t.Fatalf("讀稽核: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var raw []byte
		if err := rows.Scan(&a.Action, &a.TargetType, &a.TargetID, &raw, &a.Reason); err != nil {
			f.t.Fatalf("掃稽核: %v", err)
		}
		if err := json.Unmarshal(raw, &a.After); err != nil {
			f.t.Fatalf("解稽核 after: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("讀稽核: %v", err)
	}
	return out
}

func (f *fixture) auditActions() []string {
	f.t.Helper()
	var out []string
	for _, a := range f.audits() {
		out = append(out, a.Action)
	}
	return out
}

type eventRow struct {
	Topic   string
	Payload map[string]any
}

// events 依 payload 裡的 tournament_slug 過濾 —— 五則公告都帶這一欄,
// 而每個 fixture 的 slug 是唯一的。
func (f *fixture) events() []eventRow {
	f.t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT topic, payload FROM platform.outbox_events
		 WHERE payload->>'tournament_slug' = $1 ORDER BY id`, f.slug)
	if err != nil {
		f.t.Fatalf("讀 outbox: %v", err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var e eventRow
		var raw []byte
		if err := rows.Scan(&e.Topic, &raw); err != nil {
			f.t.Fatalf("掃 outbox: %v", err)
		}
		if err := json.Unmarshal(raw, &e.Payload); err != nil {
			f.t.Fatalf("解 payload: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("讀 outbox: %v", err)
	}
	return out
}

func (f *fixture) topics() []string {
	f.t.Helper()
	var out []string
	for _, e := range f.events() {
		out = append(out, e.Topic)
	}
	return out
}

// bet 建一張押某場的注單,讓 rollback 與結算計數驗得出來。
func (f *fixture) bet(matchID int64) int64 {
	f.t.Helper()
	ctx := context.Background()
	var betID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.bets (public_id, tournament_id, user_id, stake, potential_payout)
		VALUES (gen_random_uuid()::text, $1, $2, 100, 180)
		RETURNING id`, f.id, f.judge).Scan(&betID); err != nil {
		f.t.Fatalf("建注單: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO activity.bet_legs (bet_id, match_id, side, odds_milli)
		VALUES ($1, $2, 1, 1800)`, betID, matchID); err != nil {
		f.t.Fatalf("建注單腿: %v", err)
	}
	return betID
}

func (f *fixture) betStatus(betID int64) string {
	f.t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM activity.bets WHERE id = $1`, betID).Scan(&s); err != nil {
		f.t.Fatalf("讀注單狀態: %v", err)
	}
	return s
}

// ── 常用動作 ────────────────────────────────────────────────────

// runToLive 把一場比賽帶到 live(開盤 → 封盤 → 開打),回傳最後的狀態。
func (f *fixture) runToLive(m mrec) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.svc.OpenHandicap(ctx, match.OpenHandicapParams{
		MatchPublicID: m.publicID, ActorUserID: f.judge, Reason: "開盤",
	}); err != nil {
		f.t.Fatalf("開盤 %s: %v", m.publicID, err)
	}
	if _, err := f.svc.LockHandicap(ctx, match.LockHandicapParams{
		MatchPublicID: m.publicID, Confirm: true, ActorUserID: f.judge, Reason: "封盤",
	}); err != nil {
		f.t.Fatalf("封盤 %s: %v", m.publicID, err)
	}
	if _, err := f.svc.StartMatch(ctx, match.StartMatchParams{
		MatchPublicID: m.publicID, ActorUserID: f.judge, Reason: "開打",
	}); err != nil {
		f.t.Fatalf("開打 %s: %v", m.publicID, err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
