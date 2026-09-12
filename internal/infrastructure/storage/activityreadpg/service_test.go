package activityreadpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activityreadpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(hestia/CLAUDE.md)。
//
// 讀取側沒有鎖與約束可測,但**排序與邊界**只有真的資料庫答得出來:
// NULL 籤位排在前面還是後面、一位選手同時有兩場未完賽時哪一場算「當前」、
// 查不到的 fencer id 會不會變成一個 nil 值塞進 map。用假資料寫的測試會
// 全部通過,而那三件事正是這個套件唯一在做的事。
//
// 全 package 共用一個容器,每支測試建自己那一屆賽事互相隔離。

var (
	pool *pgxpool.Pool
	svc  *activityreadpg.Service
)

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
	svc = activityreadpg.New(pool)
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

// ── 測試資料 ──────────────────────────────────────────────────

type fixture struct {
	t    *testing.T
	id   int64
	slug string
}

type player struct {
	id       int64
	publicID string
	fencerID int64
	name     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	setup(t)
	var (
		id   int64
		slug string
	)
	err := pool.QueryRow(context.Background(), `
		INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase, config)
		VALUES (gen_random_uuid()::text, 'read-' || gen_random_uuid()::text, $1, 1, 'in_progress', '{}'::jsonb)
		RETURNING id, slug`, "測試賽 "+t.Name()).Scan(&id, &slug)
	if err != nil {
		t.Fatalf("建賽事: %v", err)
	}
	return &fixture{t: t, id: id, slug: slug}
}

// playerOpts 是一位參賽者可調的幾個欄位。零值 = active、未評段、未抽籤。
type playerOpts struct {
	rank   bp.Rank
	seedNo int32
	status tournament.PlayerStatus
}

func (f *fixture) player(name string, o playerOpts) player {
	f.t.Helper()
	ctx := context.Background()
	p := player{name: name}
	gameID := fmt.Sprintf("%s-%d-%d", name, f.id, nextSeq())
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.fencers (public_id, game_id, discord_name, wins, losses, tournaments_played)
		VALUES (gen_random_uuid()::text, $1, $2, 3, 1, 2) RETURNING id`,
		gameID, name+"#0001").Scan(&p.fencerID); err != nil {
		f.t.Fatalf("建 fencer %s: %v", name, err)
	}
	var rank any
	if o.rank != bp.RankUnspecified {
		rank = int16(o.rank)
	}
	var seed any
	if o.seedNo != 0 {
		seed = o.seedNo
	}
	status := o.status
	if status == "" {
		status = tournament.PlayerActive
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.tournament_players
		  (public_id, tournament_id, fencer_id, display_name, discord_name,
		   passcode_hash, rank_level, seed_no, status)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, 'hash', $5, $6, $7)
		RETURNING id, public_id`,
		f.id, p.fencerID, name, name+"#0001", rank, seed, string(status),
	).Scan(&p.id, &p.publicID); err != nil {
		f.t.Fatalf("建參賽者 %s: %v", name, err)
	}
	return p
}

// matchAt 建一個對戰表節點。p2 傳零值 player 代表「對手未定」(等上一輪)。
func (f *fixture) matchAt(round, slot int, p1, p2 player, status match.Status) string {
	f.t.Helper()
	var publicID string
	var p1ID, p2ID any
	if p1.id != 0 {
		p1ID = p1.id
	}
	if p2.id != 0 {
		p2ID = p2.id
	}
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO activity.matches
		  (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, status)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6)
		RETURNING public_id`,
		f.id, round, slot, p1ID, p2ID, string(status),
	).Scan(&publicID); err != nil {
		f.t.Fatalf("建場次 r%ds%d: %v", round, slot, err)
	}
	return publicID
}

// finish 把一場判成 done 並寫上勝者。
func (f *fixture) finish(publicID string, winner player) {
	f.t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE activity.matches
		   SET status = 'done', winner_player_id = $2, result_kind = 'normal', finished_at = now()
		 WHERE public_id = $1`, publicID, winner.id); err != nil {
		f.t.Fatalf("設定賽果: %v", err)
	}
}

var seq struct {
	sync.Mutex
	n int
}

func nextSeq() int {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return seq.n
}

// ── 賽事 ──────────────────────────────────────────────────────

func TestTournamentByID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	got, err := svc.TournamentByID(ctx, f.id)
	if err != nil {
		t.Fatalf("TournamentByID: %v", err)
	}
	if got.ID != f.id || got.Slug != f.slug {
		t.Errorf("賽事 = %d/%s,要 %d/%s", got.ID, got.Slug, f.id, f.slug)
	}
	if got.Phase != tournament.PhaseInProgress {
		t.Errorf("階段 = %s,要 in_progress", got.Phase)
	}
	// config 必須原樣帶出來:GetMatch 要拿它算 BP(ParseConfig)。
	if len(got.ConfigRaw) == 0 {
		t.Error("config 沒帶出來")
	}

	if _, err := svc.TournamentByID(ctx, -1); !errors.Is(err, tournament.ErrTournamentNotFound) {
		t.Errorf("查無賽事 = %v,要 ErrTournamentNotFound", err)
	}
}

func TestTournamentByPlayer(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	ctx := context.Background()
	p := f.player("李璃", playerOpts{})
	other.player("甲", playerOpts{})

	got, err := svc.TournamentByPlayer(ctx, p.publicID)
	if err != nil {
		t.Fatalf("TournamentByPlayer: %v", err)
	}
	if got.ID != f.id || got.Slug != f.slug {
		t.Errorf("賽事 = %d/%s,要 %d/%s(反查跑到別屆去了)", got.ID, got.Slug, f.id, f.slug)
	}

	// 選手 public_id 是 ULID、全域唯一,查無時要回「選手不存在」——
	// 回「賽事不存在」會讓裁判去檢查一個根本沒問題的網址。
	if _, err := svc.TournamentByPlayer(ctx, "01JNOSUCHPLAYER"); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Errorf("查無選手 = %v,要 ErrPlayerNotFound", err)
	}
}

// ── 選手 ──────────────────────────────────────────────────────

// TestCountPlayersCountsEveryone 釘住「報名人數不因棄賽而減少」。
//
// 報名人數是一個已經發生過的事實。過濾 status 的話,賽事頁上的人數會在
// 活動進行中自己往下掉,而那個數字的讀者(觀眾)會以為是顯示壞了。
func TestCountPlayersCountsEveryone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	empty := newFixture(t)

	n, err := svc.CountPlayers(ctx, empty.id)
	if err != nil {
		t.Fatalf("CountPlayers(空賽事): %v", err)
	}
	if n != 0 {
		t.Errorf("空賽事人數 = %d,要 0", n)
	}

	f.player("甲", playerOpts{})
	f.player("乙", playerOpts{status: tournament.PlayerEliminated})
	f.player("丙", playerOpts{status: tournament.PlayerWithdrawn})

	n, err = svc.CountPlayers(ctx, f.id)
	if err != nil {
		t.Fatalf("CountPlayers: %v", err)
	}
	if n != 3 {
		t.Errorf("人數 = %d,要 3(含已淘汰與已棄賽)", n)
	}
}

// TestListPlayersOrdering 驗「依籤位、再依報名序」。
//
// 抽籤之後,籤位是對戰表與選手列表唯一有意義的順序;報名期間全部是 NULL,
// 那時候要退回報名序。兩段都要測 —— NULLS 的位置是 SQL 裡最容易寫反的東西,
// 而寫反的症狀只是「列表順序怪怪的」,沒有人會當成 bug。
func TestListPlayersOrdering(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 報名序:甲 → 乙 → 丙 → 丁。籤位刻意與報名序相反,而丙、丁沒有籤位。
	a := f.player("甲", playerOpts{seedNo: 2, rank: bp.Rank(1)})
	b := f.player("乙", playerOpts{seedNo: 1, rank: bp.Rank(3)})
	c := f.player("丙", playerOpts{status: tournament.PlayerWithdrawn})
	d := f.player("丁", playerOpts{status: tournament.PlayerEliminated})

	players, err := svc.ListPlayers(ctx, f.id)
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	want := []player{b, a, c, d} // 籤位 1、籤位 2、未抽籤者依報名序
	if len(players) != len(want) {
		t.Fatalf("人數 = %d,要 %d(已淘汰與已棄賽都要在裡面)", len(players), len(want))
	}
	for i, w := range want {
		if players[i].ID != w.id {
			t.Fatalf("第 %d 位 = %s,要 %s(順序 = %v)",
				i, players[i].DisplayName, w.name, names(players))
		}
	}

	// 欄位真的有帶出來(JOIN fencers 的 game_id 是對戰表要顯示的自然鍵)。
	if players[0].GameID == "" {
		t.Error("game_id 沒帶出來")
	}
	if players[0].Rank != bp.Rank(3) {
		t.Errorf("段位 = %d,要 3", players[0].Rank)
	}
	if players[0].SeedNo != 1 {
		t.Errorf("籤位 = %d,要 1", players[0].SeedNo)
	}
	if players[2].Status != tournament.PlayerWithdrawn {
		t.Errorf("狀態 = %s,要 withdrawn", players[2].Status)
	}
	// 未抽籤的人 seed_no 是 NULL,port 用 0 表示「未抽」。
	if players[2].SeedNo != 0 {
		t.Errorf("未抽籤者的籤位 = %d,要 0", players[2].SeedNo)
	}
}

// TestListPlayersEmptyIsNotAnError 驗「還沒有人報名」回空切片而不是錯誤。
func TestListPlayersEmptyIsNotAnError(t *testing.T) {
	f := newFixture(t)
	players, err := svc.ListPlayers(context.Background(), f.id)
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	if players == nil {
		t.Fatal("要空切片而不是 nil")
	}
	if len(players) != 0 {
		t.Fatalf("人數 = %d,要 0", len(players))
	}
}

// TestPlayerByPublicIDIsScopedToTournament 釘住「A 屆的選手不能從 B 屆查到」。
//
// public_id 全域唯一,所以不帶 tournament_id 也查得到 —— 而那正是問題:
// 裁判權限的範圍是一屆,能跨屆定址就等於權限從一屆變成全部。
func TestPlayerByPublicIDIsScopedToTournament(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	ctx := context.Background()
	p := f.player("李璃", playerOpts{rank: bp.Rank(2)})

	got, err := svc.PlayerByPublicID(ctx, f.id, p.publicID)
	if err != nil {
		t.Fatalf("PlayerByPublicID: %v", err)
	}
	if got.ID != p.id || got.Rank != bp.Rank(2) {
		t.Errorf("選手 = %d/%d,要 %d/2", got.ID, got.Rank, p.id)
	}

	if _, err := svc.PlayerByPublicID(ctx, other.id, p.publicID); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Errorf("跨屆查詢 = %v,要 ErrPlayerNotFound", err)
	}
}

// ── 對戰表 ────────────────────────────────────────────────────

// TestListMatchesOrdering 驗依 (round, slot) 遞增,而且對手未定的那一側是零值。
func TestListMatchesOrdering(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", playerOpts{seedNo: 1, rank: bp.Rank(4)})
	b := f.player("乙", playerOpts{seedNo: 2, rank: bp.Rank(1)})
	c := f.player("丙", playerOpts{seedNo: 3, rank: bp.Rank(2)})
	d := f.player("丁", playerOpts{seedNo: 4, rank: bp.Rank(3)})

	// 刻意亂序建立:回傳順序只能來自 ORDER BY,不能來自插入順序。
	final := f.matchAt(2, 0, player{}, player{}, match.StatusPending)
	s1 := f.matchAt(1, 1, c, d, match.StatusReady)
	s0 := f.matchAt(1, 0, a, b, match.StatusLive)

	matches, err := svc.ListMatches(ctx, f.id)
	if err != nil {
		t.Fatalf("ListMatches: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("場次數 = %d,要 3", len(matches))
	}
	wantOrder := []string{s0, s1, final}
	for i, w := range wantOrder {
		if matches[i].PublicID != w {
			t.Fatalf("第 %d 場 = r%ds%d,順序不對",
				i, matches[i].Round, matches[i].Slot)
		}
	}

	// 雙方快照要真的帶出來(對戰表節點上要顯示名字與段位)。
	first := matches[0]
	if first.P1.PublicID != a.publicID || first.P2.PublicID != b.publicID {
		t.Errorf("首場雙方 = %s / %s,要 %s / %s",
			first.P1.PublicID, first.P2.PublicID, a.publicID, b.publicID)
	}
	if first.P1.Rank != bp.Rank(4) || first.P1.DisplayName != "甲" {
		t.Errorf("P1 快照不完整:%+v", first.P1)
	}
	if first.Status != match.StatusLive {
		t.Errorf("狀態 = %s,要 live", first.Status)
	}

	// 決賽兩側都還沒人,Seated() 必須是 false ——
	// 用零值表達「未就位」是 port 的約定,LEFT JOIN 的 NULL 必須落到那裡。
	last := matches[2]
	if last.P1.Seated() || last.P2.Seated() {
		t.Errorf("決賽不該有人就位:%+v / %+v", last.P1, last.P2)
	}
	if last.P1.PublicID != "" || last.P1.ID != 0 {
		t.Errorf("未就位的一側要是零值,得到 %+v", last.P1)
	}
}

// TestListMatchesBeforeDraw 驗「尚未抽籤」回空切片、nil 錯誤。
//
// transport 靠這個分辨「還沒抽」與「找不到賽事」:回錯誤的話,報名期間
// 打開對戰表頁會看到 404,而賽事明明存在。
func TestListMatchesBeforeDraw(t *testing.T) {
	f := newFixture(t)
	f.player("甲", playerOpts{})

	matches, err := svc.ListMatches(context.Background(), f.id)
	if err != nil {
		t.Fatalf("尚未抽籤時不該回錯誤,得到 %v", err)
	}
	if matches == nil {
		t.Fatal("要空切片而不是 nil")
	}
	if len(matches) != 0 {
		t.Fatalf("場次數 = %d,要 0", len(matches))
	}
}

func TestMatchByPublicID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", playerOpts{rank: bp.Rank(4)})
	b := f.player("乙", playerOpts{rank: bp.Rank(1)})
	id := f.matchAt(1, 0, a, b, match.StatusLive)

	got, err := svc.MatchByPublicID(ctx, id)
	if err != nil {
		t.Fatalf("MatchByPublicID: %v", err)
	}
	if got.PublicID != id || got.TournamentID != f.id {
		t.Errorf("場次 = %s/%d,要 %s/%d", got.PublicID, got.TournamentID, id, f.id)
	}
	if got.P1.ID != a.id || got.P2.ID != b.id {
		t.Errorf("雙方 = %d / %d,要 %d / %d", got.P1.ID, got.P2.ID, a.id, b.id)
	}

	if _, err := svc.MatchByPublicID(ctx, "01JNOSUCHMATCH"); !errors.Is(err, activityerr.ErrMatchNotFound) {
		t.Errorf("查無場次 = %v,要 ErrMatchNotFound", err)
	}
}

// ── 當前場次 ──────────────────────────────────────────────────

// TestCurrentMatchOfPlayerPicksEarliest 驗一位選手同時有兩場未完賽時,
// 回的是 (round, slot) 最小的那一場。
//
// 這是真實可達的狀態:剛贏下第一輪,晉級寫進第二輪的節點,那一刻他在
// 兩場上都出現(第一輪那場已 done,但第二輪之後還可能再有)。沒有排序的話
// 「你的下一場」會在兩者之間跳動,而選手會以為自己看錯了。
func TestCurrentMatchOfPlayerPicksEarliest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", playerOpts{seedNo: 1})
	b := f.player("乙", playerOpts{seedNo: 2})

	later := f.matchAt(2, 0, a, player{}, match.StatusPending)
	now := f.matchAt(1, 0, a, b, match.StatusReady)
	_ = later

	got, err := svc.CurrentMatchOfPlayer(ctx, a.id)
	if err != nil {
		t.Fatalf("CurrentMatchOfPlayer: %v", err)
	}
	if got == nil {
		t.Fatal("要回那場 ready 的比賽")
	}
	if got.PublicID != now {
		t.Errorf("當前場次 = r%ds%d,要第一輪那場", got.Round, got.Slot)
	}
}

// TestCurrentMatchOfPlayerIncludesUndecidedOpponent 驗「等上一輪」也算當前場次。
//
// 選手頁要顯示的是「下一場,對手未定」,不是「你沒有比賽」——
// 後者會讓一個還在賽中的人以為自己已經出局了。
func TestCurrentMatchOfPlayerIncludesUndecidedOpponent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", playerOpts{seedNo: 1})
	f.matchAt(2, 0, a, player{}, match.StatusPending)

	got, err := svc.CurrentMatchOfPlayer(ctx, a.id)
	if err != nil {
		t.Fatalf("CurrentMatchOfPlayer: %v", err)
	}
	if got == nil {
		t.Fatal("對手未定的場次仍然是「當前場次」")
	}
	if got.P2.Seated() {
		t.Errorf("對手應該未定,得到 %+v", got.P2)
	}
}

// TestCurrentMatchOfPlayerNilCases 把三種「沒有待打的場次」一次釘住。
//
// 三者的成因完全不同,但對呼叫端是同一個答案(nil, nil):
//
//	未抽籤   整屆一場都還沒有
//	已淘汰   輸的那場是 done,之後的節點坐的是對手
//	已棄賽   **場次可能還在**(對手未定的那些留在 pending 等晉級鏈接手),
//	         所以必須靠選手自己的 status 擋 —— 沒有那道條件的話,
//	         一位已經棄賽的人登入後會看到「你的下一場」。
func TestCurrentMatchOfPlayerNilCases(t *testing.T) {
	ctx := context.Background()

	t.Run("未抽籤", func(t *testing.T) {
		f := newFixture(t)
		a := f.player("甲", playerOpts{})
		got, err := svc.CurrentMatchOfPlayer(ctx, a.id)
		if err != nil {
			t.Fatalf("CurrentMatchOfPlayer: %v", err)
		}
		if got != nil {
			t.Errorf("尚未抽籤時要回 nil,得到 %+v", got)
		}
	})

	t.Run("已淘汰", func(t *testing.T) {
		f := newFixture(t)
		a := f.player("甲", playerOpts{seedNo: 1})
		b := f.player("乙", playerOpts{seedNo: 2})
		id := f.matchAt(1, 0, a, b, match.StatusLive)
		f.finish(id, b)
		if _, err := pool.Exec(ctx,
			`UPDATE activity.tournament_players SET status = 'eliminated' WHERE id = $1`, a.id); err != nil {
			t.Fatalf("標記淘汰: %v", err)
		}

		got, err := svc.CurrentMatchOfPlayer(ctx, a.id)
		if err != nil {
			t.Fatalf("CurrentMatchOfPlayer: %v", err)
		}
		if got != nil {
			t.Errorf("已淘汰要回 nil,得到 r%ds%d", got.Round, got.Slot)
		}
		// 對照組:贏的那一位也沒有待打的場次(第二輪還沒建),同樣是 nil。
		winner, err := svc.CurrentMatchOfPlayer(ctx, b.id)
		if err != nil {
			t.Fatalf("CurrentMatchOfPlayer(勝者): %v", err)
		}
		if winner != nil {
			t.Errorf("勝者沒有下一場時也要回 nil,得到 r%ds%d", winner.Round, winner.Slot)
		}
	})

	t.Run("已棄賽但場次還在", func(t *testing.T) {
		f := newFixture(t)
		a := f.player("甲", playerOpts{seedNo: 1, status: tournament.PlayerWithdrawn})
		// 對手未定的節點不會被棄賽級聯判成不戰而勝,所以它留在 pending。
		f.matchAt(2, 0, a, player{}, match.StatusPending)

		got, err := svc.CurrentMatchOfPlayer(ctx, a.id)
		if err != nil {
			t.Fatalf("CurrentMatchOfPlayer: %v", err)
		}
		if got != nil {
			t.Errorf("已棄賽要回 nil,得到 r%ds%d(status 那道條件漏了)", got.Round, got.Slot)
		}
	})
}

// ── 跨屆檔案 ──────────────────────────────────────────────────

// TestFencersSkipsMissingIDs 釘住「查不到的 id 不出現在 map 裡,而且不是錯誤」。
//
// 呼叫端(裁判評段頁)本來就要處理「這個人沒有跨屆資料」。做成錯誤的話,
// 一個查不到的 id 會讓整頁打不開;塞一個零值進去更糟 —— 那會顯示成
// 「0 勝 0 敗」,看起來像事實。
func TestFencersSkipsMissingIDs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", playerOpts{})
	b := f.player("乙", playerOpts{})

	// 重複的 id 與一個不存在的 id 一起送進去。
	got, err := svc.Fencers(ctx, []int64{a.fencerID, b.fencerID, a.fencerID, -1})
	if err != nil {
		t.Fatalf("Fencers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("map 大小 = %d,要 2(不存在的 id 不該有鍵)", len(got))
	}
	if _, ok := got[-1]; ok {
		t.Error("不存在的 id 出現在 map 裡")
	}
	fa, ok := got[a.fencerID]
	if !ok {
		t.Fatalf("map 裡少了 %d", a.fencerID)
	}
	if fa.Wins != 3 || fa.Losses != 1 || fa.TournamentsPlayed != 2 {
		t.Errorf("戰績 = %d/%d/%d,要 3/1/2", fa.Wins, fa.Losses, fa.TournamentsPlayed)
	}
	if fa.GameID == "" {
		t.Error("game_id 沒帶出來")
	}
}

// TestFencersEmptyInput 驗空輸入回空 map(不是 nil,也不打資料庫)。
func TestFencersEmptyInput(t *testing.T) {
	setup(t)
	got, err := svc.Fencers(context.Background(), nil)
	if err != nil {
		t.Fatalf("Fencers(nil): %v", err)
	}
	if got == nil {
		t.Fatal("要空 map 而不是 nil —— 呼叫端會直接索引")
	}
	if len(got) != 0 {
		t.Fatalf("map 大小 = %d,要 0", len(got))
	}
}

// ── 併發 ──────────────────────────────────────────────────────

// TestConcurrentReadsAreConsistent 驗一個 Service 被多個請求同時用不會出事。
//
// 決賽當下會有上百人同時打開對戰表頁,而 Service 是單例(cmd/server 只 new 一次)。
// 它沒有任何跨呼叫的狀態,這支測試釘住的就是那件事 —— 哪天有人替它加一個
// 快取欄位,這裡會先炸(-race 下),而不是在活動當天炸。
func TestConcurrentReadsAreConsistent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", playerOpts{seedNo: 1, rank: bp.Rank(4)})
	b := f.player("乙", playerOpts{seedNo: 2, rank: bp.Rank(1)})
	id := f.matchAt(1, 0, a, b, match.StatusLive)

	const n = 24
	errs := make(chan error, n*4)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.TournamentByID(ctx, f.id); err != nil {
				errs <- err
			}
			ps, err := svc.ListPlayers(ctx, f.id)
			if err != nil {
				errs <- err
			} else if len(ps) != 2 {
				errs <- fmt.Errorf("ListPlayers 回了 %d 位", len(ps))
			}
			ms, err := svc.ListMatches(ctx, f.id)
			if err != nil {
				errs <- err
			} else if len(ms) != 1 || ms[0].PublicID != id {
				errs <- fmt.Errorf("ListMatches 回了 %d 場", len(ms))
			}
			if _, err := svc.MatchByPublicID(ctx, id); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("併發讀取失敗:%v", err)
	}
}

func names(ps []tournament.Player) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.DisplayName
	}
	return out
}
