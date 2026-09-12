package tournamentpg_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/infrastructure/storage/tournamentpg"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義)。
// 這裡要測的正是資料庫的鎖與約束行為:樂觀鎖只讓一個人贏、UNIQUE (tournament_id,
// seed_no) 在交換籤位時會不會撞、FK 會不會把稽核紀錄連帶 rollback。
// 共用一個容器,各測試建自己的賽事與選手互相隔離。

var (
	pool *pgxpool.Pool
	svc  *tournamentpg.Service
	q    *db.Queries
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = tournamentpg.New(pool)
		q = db.New(pool)
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// newUser 建一個真的 platform.users 列。
//
// 裁判不能是隨便一個數字:admin_audit_logs.actor_user_id 是 NOT NULL 且
// FK 到 platform.users,假 id 會讓稽核 insert 失敗、把整筆操作一起 rollback ——
// 症狀是「階段沒推進」,而真因在另一張表上,很難從錯誤訊息看出來。
func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

func newTournament(t *testing.T, phase tournament.Phase) (id int64, slug string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase)
		 VALUES (gen_random_uuid()::text, gen_random_uuid()::text, '測試賽事', 1, $1)
		 RETURNING id, slug`, string(phase),
	).Scan(&id, &slug)
	if err != nil {
		t.Fatalf("建測試賽事: %v", err)
	}
	return id, slug
}

// newPlayer 建一位報名者(含跨屆檔案)。rank 傳 RankUnspecified 代表未評段。
func newPlayer(t *testing.T, tournamentID int64, rank bp.Rank) (playerID int64, publicID string) {
	t.Helper()
	ctx := context.Background()
	gameID := fmt.Sprintf("御風羽-%d-%d", tournamentID, nextSeq())
	var fencerID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO activity.fencers (public_id, game_id)
		 VALUES (gen_random_uuid()::text, $1) RETURNING id`, gameID,
	).Scan(&fencerID); err != nil {
		t.Fatalf("建測試選手檔案: %v", err)
	}
	var level *int16
	if rank != bp.RankUnspecified {
		v := int16(rank)
		level = &v
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO activity.tournament_players
		   (public_id, tournament_id, fencer_id, display_name, discord_name, passcode_hash, rank_level, ranked_at)
		 VALUES (gen_random_uuid()::text, $1, $2, $3, 'tester#0001', 'hash', $4,
		         CASE WHEN $4::smallint IS NULL THEN NULL ELSE now() END)
		 RETURNING id, public_id`, tournamentID, fencerID, gameID, level,
	).Scan(&playerID, &publicID); err != nil {
		t.Fatalf("建測試參賽者: %v", err)
	}
	return playerID, publicID
}

var seqMu struct {
	sync.Mutex
	n int
}

// nextSeq 給測試資料一個不會撞的序號(game_id 是全服唯一的自然鍵)。
func nextSeq() int {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqMu.n++
	return seqMu.n
}

// auditCount 數某個 target 上某個 action 的稽核紀錄筆數。
func auditCount(t *testing.T, action string, targetID int64) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform.admin_audit_logs WHERE action = $1 AND target_id = $2`,
		action, targetID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("數稽核紀錄: %v", err)
	}
	return n
}

// latestAudit 取某個 target 最新一筆稽核紀錄的 action / after。
func latestAudit(t *testing.T, targetID int64) (action string, after []byte) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT action, after FROM platform.admin_audit_logs
		 WHERE target_id = $1 ORDER BY id DESC LIMIT 1`, targetID,
	).Scan(&action, &after)
	if err != nil {
		t.Fatalf("讀稽核紀錄: %v", err)
	}
	return action, after
}

// rankAudit 是一筆評段稽核的 before / after。只解出 rank_level ——
// 這筆紀錄回答的問題就是「段位從幾變成幾」。
type rankAudit struct {
	Before struct {
		RankLevel int `json:"rank_level"`
	}
	After struct {
		RankLevel int `json:"rank_level"`
	}
}

// rankAudits 依寫入順序讀出某位選手的全部評段稽核。
func rankAudits(t *testing.T, playerID int64) []rankAudit {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT before, after FROM platform.admin_audit_logs
		 WHERE action = $1 AND target_id = $2 ORDER BY id`,
		"tournament.set_player_rank", playerID)
	if err != nil {
		t.Fatalf("讀評段稽核: %v", err)
	}
	defer rows.Close()

	var out []rankAudit
	for rows.Next() {
		var before, after []byte
		if err := rows.Scan(&before, &after); err != nil {
			t.Fatalf("掃描評段稽核: %v", err)
		}
		var e rankAudit
		if err := json.Unmarshal(before, &e.Before); err != nil {
			t.Fatalf("解析 before %s: %v", before, err)
		}
		if err := json.Unmarshal(after, &e.After); err != nil {
			t.Fatalf("解析 after %s: %v", after, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀評段稽核: %v", err)
	}
	return out
}

func phaseOf(t *testing.T, tournamentID int64) string {
	t.Helper()
	var phase string
	if err := pool.QueryRow(context.Background(),
		`SELECT phase FROM activity.tournaments WHERE id = $1`, tournamentID).Scan(&phase); err != nil {
		t.Fatalf("讀賽事階段: %v", err)
	}
	return phase
}

// ── 讀取 ──────────────────────────────────────────────────────

func TestTournamentBySlug(t *testing.T) {
	setup(t)
	ctx := context.Background()
	id, slug := newTournament(t, tournament.PhaseSignup)

	got, err := svc.TournamentBySlug(ctx, slug)
	if err != nil {
		t.Fatalf("查賽事: %v", err)
	}
	if got.ID != id || got.Phase != tournament.PhaseSignup {
		t.Fatalf("讀回的賽事不對: id=%d phase=%s", got.ID, got.Phase)
	}
	if len(got.ConfigRaw) == 0 {
		t.Fatal("config 應為 JSONB 的 {} 而不是空位元組")
	}

	if _, err := svc.TournamentBySlug(ctx, "不存在的-slug"); !errors.Is(err, tournament.ErrTournamentNotFound) {
		t.Fatalf("查無賽事應回 ErrTournamentNotFound,得到 %v", err)
	}
}

func TestPlayerLookupIsScopedToTournament(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tidA, _ := newTournament(t, tournament.PhaseSignup)
	tidB, _ := newTournament(t, tournament.PhaseSignup)
	_, publicID := newPlayer(t, tidA, bp.RankKaishan)

	if _, err := svc.PlayerByPublicID(ctx, tidA, publicID); err != nil {
		t.Fatalf("同屆應查得到: %v", err)
	}
	// A 屆的選手 public_id 拿到 B 屆必須查不到 —— 否則裁判權限的範圍
	// 就從一屆變成全部。
	if _, err := svc.PlayerByPublicID(ctx, tidB, publicID); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("跨屆應回 ErrPlayerNotFound,得到 %v", err)
	}
}

func TestCountUnrankedActivePlayers(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid, _ := newTournament(t, tournament.PhaseRanking)
	newPlayer(t, tid, bp.RankKaishan)
	unranked, _ := newPlayer(t, tid, bp.RankUnspecified)
	withdrawn, _ := newPlayer(t, tid, bp.RankUnspecified)
	if _, err := pool.Exec(ctx,
		`UPDATE activity.tournament_players SET status='withdrawn' WHERE id=$1`, withdrawn); err != nil {
		t.Fatalf("設定棄賽: %v", err)
	}

	n, err := svc.CountUnrankedActivePlayers(ctx, tid)
	if err != nil {
		t.Fatalf("數未評段: %v", err)
	}
	// 棄賽者不算:把他們算進來會讓裁判被一個永遠評不完的數字擋住。
	if n != 1 {
		t.Fatalf("未評段人數應為 1(只有 %d),得到 %d", unranked, n)
	}

	players, err := svc.ListDrawablePlayers(ctx, tid)
	if err != nil {
		t.Fatalf("列可抽籤選手: %v", err)
	}
	if len(players) != 1 {
		t.Fatalf("可抽籤選手應為 1 位,得到 %d", len(players))
	}
	if players[0].GameID == "" {
		t.Fatal("可抽籤選手應帶出 game_id(對戰表要顯示)")
	}
}

// ── 階段 ──────────────────────────────────────────────────────

func TestUpdatePhaseWritesAudit(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseSignup)

	err := svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
		TournamentID: tid, From: tournament.PhaseSignup, To: tournament.PhaseSignupClosed,
		ActorUserID: actor, Reason: "報名截止",
	})
	if err != nil {
		t.Fatalf("推進階段: %v", err)
	}
	if got := phaseOf(t, tid); got != string(tournament.PhaseSignupClosed) {
		t.Fatalf("階段應為 signup_closed,得到 %s", got)
	}
	if n := auditCount(t, "tournament.phase_changed", tid); n != 1 {
		t.Fatalf("稽核紀錄應有 1 筆,得到 %d", n)
	}
}

func TestUpdatePhaseRequiresActor(t *testing.T) {
	setup(t)
	tid, _ := newTournament(t, tournament.PhaseSignup)
	err := svc.UpdatePhase(context.Background(), tournament.UpdatePhaseParams{
		TournamentID: tid, From: tournament.PhaseSignup, To: tournament.PhaseSignupClosed,
	})
	// actor=0 會讓稽核的 FK 失敗、整筆 rollback。與其讓它變成一個資料庫錯誤,
	// 不如在入口就講清楚是誰沒帶。
	if !errors.Is(err, tournament.ErrActorRequired) {
		t.Fatalf("應回 ErrActorRequired,得到 %v", err)
	}
	if got := phaseOf(t, tid); got != string(tournament.PhaseSignup) {
		t.Fatalf("階段不該被改動,得到 %s", got)
	}
}

// TestUpdatePhaseConcurrent 是樂觀鎖的核心驗證:兩個裁判同時按「進入下一階段」,
// 只有一個能成功,另一個必須知道自己什麼都沒做到。
func TestUpdatePhaseConcurrent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseSignup)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
				TournamentID: tid, From: tournament.PhaseSignup, To: tournament.PhaseSignupClosed,
				ActorUserID: actor, Reason: "併發測試",
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
	// 稽核紀錄跟著資料變更一起 rollback:輸的那一方不該留下「我推進了階段」的痕跡。
	if n := auditCount(t, "tournament.phase_changed", tid); n != 1 {
		t.Fatalf("稽核紀錄應只有 1 筆,得到 %d", n)
	}
}

// ── 評段 ──────────────────────────────────────────────────────

func TestSetPlayerRank(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseRanking)
	playerID, _ := newPlayer(t, tid, bp.RankUnspecified)

	got, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
		TournamentID: tid, PlayerID: playerID, Rank: bp.RankFeihua,
		ActorUserID: actor, Reason: "評段",
	})
	if err != nil {
		t.Fatalf("評段: %v", err)
	}
	if got.Rank != bp.RankFeihua || got.RankedBy == nil || *got.RankedBy != actor {
		t.Fatalf("回傳的選手不對: rank=%d ranked_by=%v", got.Rank, got.RankedBy)
	}
	if got.GameID == "" {
		t.Fatal("回傳的選手應帶 game_id")
	}

	// 跨屆快照同 tx 一起更新。
	var last *int16
	if err := pool.QueryRow(ctx,
		`SELECT f.last_rank_level FROM activity.fencers f
		 JOIN activity.tournament_players tp ON tp.fencer_id = f.id WHERE tp.id = $1`,
		playerID).Scan(&last); err != nil {
		t.Fatalf("讀跨屆段位: %v", err)
	}
	if last == nil || *last != int16(bp.RankFeihua) {
		t.Fatalf("fencers.last_rank_level 應為 3,得到 %v", last)
	}
	if n := auditCount(t, "tournament.set_player_rank", playerID); n != 1 {
		t.Fatalf("評段稽核應有 1 筆,得到 %d", n)
	}
}

// TestSetPlayerRankAuditKeepsPreviousRank 驗證改段位的稽核記得住**舊值**。
//
// 改段位多半是異議流程的結果,而 UPDATE 一下去,「原本評幾段」就只剩稽核這一份。
// 記不出來的話,日後有人不服氣時沒有任何東西拿得出來對。
func TestSetPlayerRankAuditKeepsPreviousRank(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseRanking)
	playerID, _ := newPlayer(t, tid, bp.RankUnspecified)

	set := func(rank bp.Rank) {
		t.Helper()
		if _, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
			TournamentID: tid, PlayerID: playerID, Rank: rank,
			ActorUserID: actor, Reason: "評段",
		}); err != nil {
			t.Fatalf("評段 %d: %v", rank, err)
		}
	}
	set(bp.RankKaishan)
	set(bp.RankWuwo)

	entries := rankAudits(t, playerID)
	if len(entries) != 2 {
		t.Fatalf("評段稽核應有 2 筆,得到 %d", len(entries))
	}
	// 第一筆:未評段 → 開山。舊值 0 也是事實的一部分,不能省略成「沒有 before」,
	// 否則「本來就沒評過」與「有評過但沒記到」在事後看起來一模一樣。
	if entries[0].Before.RankLevel != int(bp.RankUnspecified) ||
		entries[0].After.RankLevel != int(bp.RankKaishan) {
		t.Fatalf("第一筆稽核應是 0→1,得到 %d→%d",
			entries[0].Before.RankLevel, entries[0].After.RankLevel)
	}
	// 第二筆:開山 → 無我。before 必須是**第一次寫下的值**,不是原始的 0。
	if entries[1].Before.RankLevel != int(bp.RankKaishan) ||
		entries[1].After.RankLevel != int(bp.RankWuwo) {
		t.Fatalf("第二筆稽核應是 1→4,得到 %d→%d",
			entries[1].Before.RankLevel, entries[1].After.RankLevel)
	}
}

func TestSetPlayerRankRejects(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseRanking)
	playerID, _ := newPlayer(t, tid, bp.RankUnspecified)

	if _, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
		TournamentID: tid, PlayerID: playerID, Rank: bp.Rank(9), ActorUserID: actor,
	}); !errors.Is(err, tournament.ErrInvalidRank) {
		t.Fatalf("段位 9 應回 ErrInvalidRank,得到 %v", err)
	}
	if _, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
		TournamentID: tid, PlayerID: playerID + 100000, Rank: bp.RankWuwo, ActorUserID: actor,
	}); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("查無選手應回 ErrPlayerNotFound,得到 %v", err)
	}
	if _, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
		TournamentID: tid, PlayerID: playerID, Rank: bp.RankWuwo,
	}); !errors.Is(err, tournament.ErrActorRequired) {
		t.Fatalf("沒帶裁判應回 ErrActorRequired,得到 %v", err)
	}
}

// ── 衍生資料對帳 ──────────────────────────────────────────────

// TestRecalcFencerStatsMatchesIncremental 驗證增量更新的值與從事實表重算的值一致。
//
// fencers 的四個統計欄位全是衍生資料(schemas/26),冗餘存放只是為了讀得快。
// 增量路徑(報名 +1、評段回寫 last_rank_level)與重算口徑一旦分岔,
// 生涯頁會長期顯示一個沒有人看得出哪裡錯的數字。
func TestRecalcFencerStatsMatchesIncremental(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseDrawing)

	p1, _ := newPlayer(t, tid, bp.RankDuanshui)
	p2, _ := newPlayer(t, tid, bp.RankKaishan)
	if _, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
		TournamentID: tid, PlayerID: p1, Rank: bp.RankWuwo, ActorUserID: actor, Reason: "評段",
	}); err != nil {
		t.Fatalf("評段: %v", err)
	}

	// 一場已分勝負的比賽:p1 勝、p2 敗。
	if _, err := pool.Exec(ctx,
		`INSERT INTO activity.matches
		   (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, winner_player_id, status)
		 VALUES (gen_random_uuid()::text, $1, 1, 0, $2, $3, $2, 'done')`, tid, p1, p2); err != nil {
		t.Fatalf("建測試場次: %v", err)
	}

	var fencer1 int64
	if err := pool.QueryRow(ctx,
		`SELECT fencer_id FROM activity.tournament_players WHERE id = $1`, p1).Scan(&fencer1); err != nil {
		t.Fatalf("讀 fencer_id: %v", err)
	}

	row, err := q.RecalcFencerStats(ctx, fencer1)
	if err != nil {
		t.Fatalf("重算統計: %v", err)
	}
	// tournaments_played 的增量路徑在 signuppg(報名 +1),這裡的測試資料是直接插的,
	// 所以存的值是 0 而算出來是 1 —— 差異正是對帳要抓的東西。
	if row.CalcTournamentsPlayed != 1 || row.CalcWins != 1 || row.CalcLosses != 0 {
		t.Fatalf("重算口徑不符: played=%d wins=%d losses=%d",
			row.CalcTournamentsPlayed, row.CalcWins, row.CalcLosses)
	}
	// 評段的增量回寫與重算必須一致。
	if row.StoredLastRankLevel == nil || row.CalcLastRankLevel == nil ||
		*row.StoredLastRankLevel != *row.CalcLastRankLevel {
		t.Fatalf("last_rank_level 增量值與重算值不一致: stored=%v calc=%v",
			row.StoredLastRankLevel, row.CalcLastRankLevel)
	}

	// 對帳修復之後,存的值與算的值必須完全相同。
	if _, err := q.ApplyFencerStats(ctx, db.ApplyFencerStatsParams{
		TournamentsPlayed: row.CalcTournamentsPlayed,
		Wins:              row.CalcWins,
		Losses:            row.CalcLosses,
		LastRankLevel:     row.CalcLastRankLevel,
		LastRankedAt:      row.CalcLastRankedAt,
		FencerID:          fencer1,
	}); err != nil {
		t.Fatalf("套用重算結果: %v", err)
	}
	after, err := q.RecalcFencerStats(ctx, fencer1)
	if err != nil {
		t.Fatalf("再次重算: %v", err)
	}
	if after.StoredTournamentsPlayed != after.CalcTournamentsPlayed ||
		after.StoredWins != after.CalcWins || after.StoredLosses != after.CalcLosses {
		t.Fatalf("對帳後仍不一致: %+v", after)
	}
}

// TestConcurrentSetPlayerRankAuditChain 確認併發評段時稽核鏈不會斷。
//
// 這條測試存在的理由,是它抓到過一個真的 bug:GetPlayerByID 原本沒有列鎖,
// 而 SetPlayerRank 只握著賽事列的 FOR SHARE —— 兩位裁判可以並發評同一個人。
// READ COMMITTED 下兩邊都讀到同一個舊值,兩筆稽核的 before 就都記成那個值。
//
// 這個錯的可怕之處是它**只在併發時發生**:序列化跑完全正常,所有其他測試照樣綠。
// 而它壞掉的時機,恰好是最需要稽核的時機 —— 有人對段位吵起來、兩位裁判同時動手。
//
// 判準不是「誰先誰後」(併發本來就不保證順序),而是**鏈要接得起來**:
// 把稽核依 id 排好之後,每一筆的 before 必須等於前一筆的 after。
func TestConcurrentSetPlayerRankAuditChain(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid, _ := newTournament(t, tournament.PhaseRanking)
	playerID, _ := newPlayer(t, tid, bp.RankUnspecified)

	ranks := []bp.Rank{bp.RankKaishan, bp.RankDuanshui, bp.RankFeihua, bp.RankWuwo}
	var wg sync.WaitGroup
	for _, r := range ranks {
		wg.Add(1)
		go func(rank bp.Rank) {
			defer wg.Done()
			if _, err := svc.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
				TournamentID: tid, PlayerID: playerID, Rank: rank,
				ActorUserID: actor, Reason: "併發評段",
			}); err != nil {
				t.Errorf("評段 %d: %v", rank, err)
			}
		}(r)
	}
	wg.Wait()

	got := rankAudits(t, playerID)
	if len(got) != len(ranks) {
		t.Fatalf("稽核 %d 筆,要 %d 筆", len(got), len(ranks))
	}

	// 第一筆的 before 必為未評段(0),之後每一筆接上前一筆。
	prev := int(bp.RankUnspecified)
	for i, a := range got {
		if a.Before.RankLevel != prev {
			t.Errorf("稽核[%d] before=%d,要 %d(鏈斷了:前一筆 after 是 %d)",
				i, a.Before.RankLevel, prev, prev)
		}
		prev = a.After.RankLevel
	}

	// 最後一筆的 after 必須等於選手當下的實際段位,否則稽核與事實不符。
	var actual int16
	if err := pool.QueryRow(ctx,
		`SELECT rank_level FROM activity.tournament_players WHERE id = $1`, playerID,
	).Scan(&actual); err != nil {
		t.Fatalf("讀實際段位: %v", err)
	}
	if int(actual) != prev {
		t.Errorf("稽核最後記的是 %d,但選手實際段位是 %d", prev, actual)
	}
}
