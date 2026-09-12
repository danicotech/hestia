package tournamentpg_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 抽籤相關的測試都走 tournament.Service:抽籤的可重現性是「同一個種子 →
// 同一張表 → 同樣寫進資料庫」這一整條路的性質,只測 adapter 那一段
// 證明不了裁判真的能拿種子重現一張表。

const fixedSeed = "0123456789abcdef"

// seatsOf 讀一屆賽事所有已抽到籤位的選手,player_id → seed_no。
func seatsOf(t *testing.T, tournamentID int64) map[int64]int32 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id, seed_no FROM activity.tournament_players
		 WHERE tournament_id = $1 AND seed_no IS NOT NULL`, tournamentID)
	if err != nil {
		t.Fatalf("讀籤位: %v", err)
	}
	defer rows.Close()
	out := map[int64]int32{}
	for rows.Next() {
		var id int64
		var seed int32
		if err := rows.Scan(&id, &seed); err != nil {
			t.Fatalf("掃描籤位: %v", err)
		}
		out[id] = seed
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀籤位: %v", err)
	}
	return out
}

type matchRow struct {
	Round, Slot int32
	P1, P2      *int64
	PublicID    string
}

func matchesOf(t *testing.T, tournamentID int64) []matchRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT round, slot, p1_player_id, p2_player_id, public_id
		 FROM activity.matches WHERE tournament_id = $1 ORDER BY round, slot`, tournamentID)
	if err != nil {
		t.Fatalf("讀對戰表: %v", err)
	}
	defer rows.Close()
	var out []matchRow
	for rows.Next() {
		var m matchRow
		if err := rows.Scan(&m.Round, &m.Slot, &m.P1, &m.P2, &m.PublicID); err != nil {
			t.Fatalf("掃描對戰表: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀對戰表: %v", err)
	}
	return out
}

// shape 把對戰表壓成一個可比對的字串(不含 public_id —— 那是每次重抽都會換的)。
func shape(ms []matchRow) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "r%d s%d p1=%v p2=%v;", m.Round, m.Slot, deref(m.P1), deref(m.P2))
	}
	return b.String()
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// drawableTournament 建一屆處於 drawing 階段、n 位已評段選手的賽事。
func drawableTournament(t *testing.T, n int) (id int64, slug string, players []int64) {
	t.Helper()
	id, slug = newTournament(t, tournament.PhaseDrawing)
	for i := 0; i < n; i++ {
		pid, _ := newPlayer(t, id, bp.RankKaishan)
		players = append(players, pid)
	}
	return id, slug, players
}

func TestDrawWritesSeatsAndMatches(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, players := drawableTournament(t, 5)

	res, err := tournament.NewService(svc).Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "抽籤",
	})
	if err != nil {
		t.Fatalf("抽籤: %v", err)
	}

	seats := seatsOf(t, tid)
	if len(seats) != len(players) {
		t.Fatalf("應有 %d 個籤位,得到 %d", len(players), len(seats))
	}
	seen := map[int32]bool{}
	for pid, seed := range seats {
		if seed < 1 || seed > int32(len(players)) {
			t.Fatalf("選手 %d 的籤位 %d 超出 1..%d", pid, seed, len(players))
		}
		if seen[seed] {
			t.Fatalf("籤位 %d 重複", seed)
		}
		seen[seed] = true
	}
	// 單淘汰的數學性質:每場淘汰一人,N 人要打 N-1 場。
	ms := matchesOf(t, tid)
	if len(ms) != len(players)-1 {
		t.Fatalf("場次數應為 %d,得到 %d", len(players)-1, len(ms))
	}
	if len(ms) != len(res.Matches) {
		t.Fatalf("寫入的場次數與回傳的不一致: db=%d result=%d", len(ms), len(res.Matches))
	}
	for _, m := range ms {
		if m.PublicID == "" || len(m.PublicID) != 26 {
			t.Fatalf("場次 public_id 應為 26 字元 ULID,得到 %q", m.PublicID)
		}
	}

	// 種子必須進稽核紀錄:那是整個抽籤流程唯一的可驗證性來源。
	action, after := latestAudit(t, tid)
	if action != "tournament.draw" {
		t.Fatalf("稽核 action 應為 tournament.draw,得到 %s", action)
	}
	if !strings.Contains(string(after), fixedSeed) {
		t.Fatalf("稽核紀錄的 after 必須含種子 %s,得到 %s", fixedSeed, after)
	}
}

// TestDrawIsReproducible 是抽籤自證清白的方式:同一份名單 + 同一個種子
// 必須抽出位元相同的對戰表,否則「拿舊種子重跑驗證」這件事就不成立。
func TestDrawIsReproducible(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, _ := drawableTournament(t, 7)
	svcCore := tournament.NewService(svc)

	if _, err := svcCore.Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "第一次",
	}); err != nil {
		t.Fatalf("第一次抽籤: %v", err)
	}
	firstSeats, firstShape := seatsOf(t, tid), shape(matchesOf(t, tid))

	if _, err := svcCore.Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "重跑驗證",
	}); err != nil {
		t.Fatalf("第二次抽籤: %v", err)
	}
	secondSeats, secondShape := seatsOf(t, tid), shape(matchesOf(t, tid))

	if len(firstSeats) != len(secondSeats) {
		t.Fatalf("兩次抽籤的籤位數不同: %d vs %d", len(firstSeats), len(secondSeats))
	}
	for pid, seed := range firstSeats {
		if secondSeats[pid] != seed {
			t.Fatalf("選手 %d 的籤位在重跑後變了: %d → %d", pid, seed, secondSeats[pid])
		}
	}
	if firstShape != secondShape {
		t.Fatalf("同種子的對戰表不一致:\n%s\n%s", firstShape, secondShape)
	}
}

// TestRedrawClearsPreviousDraw 驗證重抽先清乾淨:不清的話 UNIQUE
// (tournament_id, seed_no) 與 (tournament_id, round, slot) 都會在中途撞上。
func TestRedrawClearsPreviousDraw(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, players := drawableTournament(t, 6)
	svcCore := tournament.NewService(svc)

	if _, err := svcCore.Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "第一次",
	}); err != nil {
		t.Fatalf("第一次抽籤: %v", err)
	}
	firstIDs := map[string]bool{}
	for _, m := range matchesOf(t, tid) {
		firstIDs[m.PublicID] = true
	}

	if _, err := svcCore.Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: "fedcba9876543210", ActorUserID: actor, Reason: "重抽",
	}); err != nil {
		t.Fatalf("重抽: %v", err)
	}

	ms := matchesOf(t, tid)
	if len(ms) != len(players)-1 {
		t.Fatalf("重抽後場次數應為 %d,得到 %d(舊場次沒清乾淨)", len(players)-1, len(ms))
	}
	for _, m := range ms {
		if firstIDs[m.PublicID] {
			t.Fatalf("場次 %s 是上一次抽籤留下來的", m.PublicID)
		}
	}
	if len(seatsOf(t, tid)) != len(players) {
		t.Fatalf("重抽後籤位數應為 %d", len(players))
	}
	if n := auditCount(t, "tournament.draw", tid); n != 2 {
		t.Fatalf("兩次抽籤應有 2 筆稽核,得到 %d", n)
	}
}

func TestReplaceDrawRejects(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _, players := drawableTournament(t, 2)

	seats := []tournament.SeatAssignment{{PlayerID: players[0], SeedNo: 1}}
	base := tournament.ReplaceDrawParams{
		TournamentID: tid, Seed: fixedSeed, Seats: seats, ActorUserID: actor,
	}

	noSeed := base
	noSeed.Seed = ""
	// 寫得進去但驗不回來的抽籤,比抽不成更糟。
	if err := svc.ReplaceDraw(ctx, noSeed); !errors.Is(err, tournament.ErrInvalidSeed) {
		t.Fatalf("空種子應回 ErrInvalidSeed,得到 %v", err)
	}
	noSeats := base
	noSeats.Seats = nil
	if err := svc.ReplaceDraw(ctx, noSeats); !errors.Is(err, tournament.ErrNoDrawablePlayers) {
		t.Fatalf("空籤位應回 ErrNoDrawablePlayers,得到 %v", err)
	}
	noActor := base
	noActor.ActorUserID = 0
	if err := svc.ReplaceDraw(ctx, noActor); !errors.Is(err, tournament.ErrActorRequired) {
		t.Fatalf("沒帶裁判應回 ErrActorRequired,得到 %v", err)
	}

	// 別屆的選手不可能在這屆有籤位:影響列數對不上就整筆退回。
	_, _, otherPlayers := drawableTournament(t, 1)
	alien := base
	alien.Seats = []tournament.SeatAssignment{{PlayerID: otherPlayers[0], SeedNo: 1}}
	if err := svc.ReplaceDraw(ctx, alien); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("外來選手應回 ErrPlayerNotFound,得到 %v", err)
	}

	// 階段在取鎖後重讀:離開 drawing 就代表籤表已確認,不能再被蓋掉。
	if _, err := pool.Exec(ctx,
		`UPDATE activity.tournaments SET phase='in_progress' WHERE id=$1`, tid); err != nil {
		t.Fatalf("改階段: %v", err)
	}
	if err := svc.ReplaceDraw(ctx, base); !errors.Is(err, tournament.ErrWrongPhase) {
		t.Fatalf("非 drawing 階段應回 ErrWrongPhase,得到 %v", err)
	}
	if len(seatsOf(t, tid)) != 0 {
		t.Fatal("被拒絕的抽籤不該留下任何籤位")
	}
}

// TestConcurrentDrawsSerialize 驗證 LockTournamentExclusive 真的讓重抽串行化:
// 兩次抽籤交錯的話會撞 UNIQUE (tournament_id, seed_no)。
func TestConcurrentDrawsSerialize(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, players := drawableTournament(t, 8)
	svcCore := tournament.NewService(svc)

	seeds := []string{fixedSeed, "fedcba9876543210"}
	errs := make([]error, len(seeds))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, seed := range seeds {
		wg.Add(1)
		go func(i int, seed string) {
			defer wg.Done()
			<-start
			_, errs[i] = svcCore.Draw(ctx, tournament.DrawParams{
				Slug: slug, Seed: seed, ActorUserID: actor, Reason: "併發抽籤",
			})
		}(i, seed)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 次抽籤失敗(串行化沒生效?): %v", i, err)
		}
	}
	if got := len(matchesOf(t, tid)); got != len(players)-1 {
		t.Fatalf("最終場次數應為 %d,得到 %d", len(players)-1, got)
	}
	if got := len(seatsOf(t, tid)); got != len(players) {
		t.Fatalf("最終籤位數應為 %d,得到 %d", len(players), got)
	}
}

// ── 交換籤位 ──────────────────────────────────────────────────

// TestSwapSeedsUpdatesBothTables 是最容易寫錯的一支:只改籤號不改對戰表時,
// 沒有任何約束會報錯 —— 所以這裡兩邊都要驗。
func TestSwapSeedsUpdatesBothTables(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, _ := drawableTournament(t, 4)

	if _, err := tournament.NewService(svc).Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "抽籤",
	}); err != nil {
		t.Fatalf("抽籤: %v", err)
	}

	before := seatsOf(t, tid)
	bySeed := map[int32]int64{}
	for pid, seed := range before {
		bySeed[seed] = pid
	}
	a, b := bySeed[1], bySeed[3]
	beforeMatches := matchesOf(t, tid)

	if err := svc.SwapSeeds(ctx, tournament.SwapSeedsParams{
		TournamentID: tid, PlayerAID: a, PlayerBID: b, ActorUserID: actor, Reason: "拆開同公會",
	}); err != nil {
		t.Fatalf("交換籤位: %v", err)
	}

	after := seatsOf(t, tid)
	if after[a] != before[b] || after[b] != before[a] {
		t.Fatalf("籤位沒換到: a %d→%d, b %d→%d", before[a], after[a], before[b], after[b])
	}
	// 對戰表上的引用必須同步互換,否則籤位表與對戰表會互相矛盾。
	want := make([]matchRow, len(beforeMatches))
	copy(want, beforeMatches)
	for i := range want {
		want[i].P1 = swapRef(want[i].P1, a, b)
		want[i].P2 = swapRef(want[i].P2, a, b)
	}
	// 這兩位必須真的出現在對戰表上,否則「對戰表有沒有跟著換」這件事
	// 根本沒被測到 —— 一個永遠會通過的斷言比沒有斷言更糟。
	if shape(want) == shape(beforeMatches) {
		t.Fatal("測試前提不成立:選中的兩位選手沒有出現在對戰表上")
	}
	if got := shape(matchesOf(t, tid)); got != shape(want) {
		t.Fatalf("對戰表沒有跟著換:\n得到 %s\n預期 %s", got, shape(want))
	}

	action, _ := latestAudit(t, tid)
	if action != "tournament.swap_seeds" {
		t.Fatalf("稽核 action 應為 tournament.swap_seeds,得到 %s", action)
	}
}

func swapRef(p *int64, a, b int64) *int64 {
	if p == nil {
		return nil
	}
	switch *p {
	case a:
		v := b
		return &v
	case b:
		v := a
		return &v
	default:
		return p
	}
}

func TestSwapSeedsRejects(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, players := drawableTournament(t, 4)

	// 還沒抽籤時沒有人有籤位。
	if err := svc.SwapSeeds(ctx, tournament.SwapSeedsParams{
		TournamentID: tid, PlayerAID: players[0], PlayerBID: players[1], ActorUserID: actor,
	}); !errors.Is(err, tournament.ErrPlayerNotSeeded) {
		t.Fatalf("未抽籤應回 ErrPlayerNotSeeded,得到 %v", err)
	}
	if err := svc.SwapSeeds(ctx, tournament.SwapSeedsParams{
		TournamentID: tid, PlayerAID: players[0], PlayerBID: players[0], ActorUserID: actor,
	}); !errors.Is(err, tournament.ErrSamePlayer) {
		t.Fatalf("同一人應回 ErrSamePlayer,得到 %v", err)
	}

	if _, err := tournament.NewService(svc).Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "抽籤",
	}); err != nil {
		t.Fatalf("抽籤: %v", err)
	}
	if err := svc.SwapSeeds(ctx, tournament.SwapSeedsParams{
		TournamentID: tid, PlayerAID: players[0], PlayerBID: players[0] + 100000, ActorUserID: actor,
	}); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("查無選手應回 ErrPlayerNotFound,得到 %v", err)
	}
	// 被拒絕的交換不該留下中間狀態(騰空位那一步只存在於 tx 內)。
	seats := seatsOf(t, tid)
	if len(seats) != len(players) {
		t.Fatalf("失敗的交換不該弄丟籤位,剩 %d 個", len(seats))
	}
}

// ── 退回階段 ──────────────────────────────────────────────────

func TestRollbackToRankedClearsDraw(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, _ := drawableTournament(t, 5)

	if _, err := tournament.NewService(svc).Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "抽籤",
	}); err != nil {
		t.Fatalf("抽籤: %v", err)
	}

	if err := svc.RollbackToRanked(ctx, tournament.RollbackParams{
		TournamentID: tid, ActorUserID: actor, Reason: "段位有異議",
	}); err != nil {
		t.Fatalf("退回階段: %v", err)
	}
	if got := phaseOf(t, tid); got != string(tournament.PhaseRanked) {
		t.Fatalf("階段應為 ranked,得到 %s", got)
	}
	if got := len(matchesOf(t, tid)); got != 0 {
		t.Fatalf("對戰表應被刪光,剩 %d 場", got)
	}
	if got := len(seatsOf(t, tid)); got != 0 {
		t.Fatalf("籤位應全部清成 NULL,剩 %d 個", got)
	}
	if n := auditCount(t, "tournament.rollback_to_ranked", tid); n != 1 {
		t.Fatalf("退回階段稽核應有 1 筆,得到 %d", n)
	}
}

// TestRollbackChecksPhaseBeforeDeleting 驗證「先擋階段再刪資料」:
// 階段不對的時候,對戰表必須原封不動。
func TestRollbackChecksPhaseBeforeDeleting(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, players := drawableTournament(t, 4)

	if _, err := tournament.NewService(svc).Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "抽籤",
	}); err != nil {
		t.Fatalf("抽籤: %v", err)
	}
	// 裁判已經確認籤表並開賽。
	if _, err := pool.Exec(ctx,
		`UPDATE activity.tournaments SET phase='in_progress' WHERE id=$1`, tid); err != nil {
		t.Fatalf("改階段: %v", err)
	}

	err := svc.RollbackToRanked(ctx, tournament.RollbackParams{
		TournamentID: tid, ActorUserID: actor, Reason: "手滑",
	})
	if !errors.Is(err, tournament.ErrPhaseConflict) {
		t.Fatalf("非 drawing 階段應回 ErrPhaseConflict,得到 %v", err)
	}
	if got := len(matchesOf(t, tid)); got != len(players)-1 {
		t.Fatalf("對戰表不該被刪,剩 %d 場", got)
	}
	if got := len(seatsOf(t, tid)); got != len(players) {
		t.Fatalf("籤位不該被清,剩 %d 個", got)
	}
	if n := auditCount(t, "tournament.rollback_to_ranked", tid); n != 0 {
		t.Fatalf("失敗的退回不該留稽核紀錄,得到 %d 筆", n)
	}
}

// TestConcurrentRollback 兩個裁判同時按退回,只有一個能動到東西。
func TestConcurrentRollback(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, slug, _ := drawableTournament(t, 4)
	if _, err := tournament.NewService(svc).Draw(ctx, tournament.DrawParams{
		Slug: slug, Seed: fixedSeed, ActorUserID: actor, Reason: "抽籤",
	}); err != nil {
		t.Fatalf("抽籤: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = svc.RollbackToRanked(ctx, tournament.RollbackParams{
				TournamentID: tid, ActorUserID: actor, Reason: "併發退回",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, conflict int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, tournament.ErrPhaseConflict):
			conflict++
		default:
			t.Fatalf("非預期錯誤: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("應恰有一個成功一個衝突,得到 成功=%d 衝突=%d", ok, conflict)
	}
	if n := auditCount(t, "tournament.rollback_to_ranked", tid); n != 1 {
		t.Fatalf("稽核應只有 1 筆,得到 %d", n)
	}
}
