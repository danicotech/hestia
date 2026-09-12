package signuppg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/signuppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/infrastructure/storage/tournamentpg"
)

// 對真 Postgres 跑:這裡要驗的幾乎全是資料庫的約束在做的事 ——
// UNIQUE (game_id) 決定回鍋選手接到哪一份檔案、UNIQUE (tournament_id, fencer_id)
// 擋連點報名、fencers_user_id_uq 擋一個帳號綁兩個人。mock 掉它們就什麼都沒測到。

var (
	pool  *pgxpool.Pool
	svc   *signuppg.Service
	ranks *tournamentpg.Service
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
		svc = signuppg.New(pool)
		ranks = tournamentpg.New(pool)
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
// 裁判必須是真的使用者:admin_audit_logs.actor_user_id 是 NOT NULL 且 FK 到
// platform.users,假 id 會讓稽核 insert 失敗、把整筆換發一起 rollback。
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

func newTournament(t *testing.T, phase tournament.Phase) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase)
		 VALUES (gen_random_uuid()::text, gen_random_uuid()::text, '測試賽事', 1, $1)
		 RETURNING id`, string(phase),
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試賽事: %v", err)
	}
	return id
}

var seqMu struct {
	sync.Mutex
	n int
}

// gameID 產生一個不會撞的遊戲ID(全服唯一的自然鍵)。
func gameID(prefix string) string {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqMu.n++
	return fmt.Sprintf("%s-%d", prefix, seqMu.n)
}

func regParams(tournamentID int64, id string) signup.CreateRegistrationParams {
	return signup.CreateRegistrationParams{
		TournamentID:     tournamentID,
		RequirePhase:     tournament.PhaseSignup,
		GameID:           id,
		DisplayName:      id,
		DiscordName:      "fencer#0001",
		PasscodeHash:     "argon2-hash-" + id,
		SelfRatedRank:    bp.RankDuanshui,
		LadderRank:       "青雲",
		LadderScore:      1200,
		ArtsNote:         "擅長劍法",
		AvailabilityNote: "平日晚上",
	}
}

func fencerRow(t *testing.T, id int64) (gameID string, played int32, userID *int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT game_id, tournaments_played, user_id FROM activity.fencers WHERE id = $1`, id,
	).Scan(&gameID, &played, &userID)
	if err != nil {
		t.Fatalf("讀選手檔案: %v", err)
	}
	return gameID, played, userID
}

// passcodeRow 直接用 SQL 讀 passcode_hash 與 passcode_issued_at。
//
// 刻意繞過 adapter:2026-09-13 起沒有任何 query 讀得到 passcode_hash(登入只要
// 遊戲ID),但那兩欄仍然要被寫對 —— 雜湊是「這次換發真的換掉了一個秘密」的證據,
// issued_at 是已發出 session 的作廢依據。測試是它們現在唯一的讀者。
func passcodeRow(t *testing.T, playerID int64) (hash string, issuedAt time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT passcode_hash, passcode_issued_at FROM activity.tournament_players WHERE id = $1`,
		playerID,
	).Scan(&hash, &issuedAt); err != nil {
		t.Fatalf("讀通行碼欄位: %v", err)
	}
	return hash, issuedAt
}

// setStatus 模擬裁判把某位選手標成棄賽 / 淘汰。
func setStatus(t *testing.T, playerID int64, status tournament.PlayerStatus) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE activity.tournament_players SET status = $2 WHERE id = $1`,
		playerID, string(status),
	); err != nil {
		t.Fatalf("改狀態: %v", err)
	}
}

func countFencers(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity.fencers WHERE game_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("數選手檔案: %v", err)
	}
	return n
}

// ── 報名 ──────────────────────────────────────────────────────

func TestCreateRegistrationNewFencer(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("初次報名")

	reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}
	if reg.ReturningFencer {
		t.Fatal("初次參賽不該是回鍋選手")
	}
	if reg.PreviousRank != bp.RankUnspecified {
		t.Fatalf("初次參賽的往屆段位應為未評定,得到 %d", reg.PreviousRank)
	}
	if len(reg.Player.PublicID) != 26 {
		t.Fatalf("選手 public_id 應為 26 字元 ULID,得到 %q", reg.Player.PublicID)
	}
	if reg.Player.GameID != id || reg.Player.DisplayName != id {
		t.Fatalf("選手快照不對: game_id=%s display_name=%s", reg.Player.GameID, reg.Player.DisplayName)
	}
	if reg.Player.SelfRatedRank != bp.RankDuanshui || reg.Player.LadderScore != 1200 {
		t.Fatalf("報名表欄位沒寫進去: %+v", reg.Player)
	}
	if reg.Player.Rank != bp.RankUnspecified {
		t.Fatal("本屆段位要等裁判評定,報名時必須是未評定")
	}
	// discord_name 由第 4 步統一更新到跨屆檔案(一個概念一個位置)。
	if reg.Fencer.DiscordName != "fencer#0001" || reg.Fencer.TournamentsPlayed != 1 {
		t.Fatalf("跨屆檔案沒更新: %+v", reg.Fencer)
	}
}

// TestReturningFencerKeepsSameProfile 驗證回鍋選手接上同一份檔案,
// 而 PreviousRank 是**接上之前**的快照 —— 那個順序只有 adapter 判定得出來。
func TestReturningFencerKeepsSameProfile(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	id := gameID("回鍋")

	first := newTournament(t, tournament.PhaseSignup)
	reg1, err := svc.CreateRegistration(ctx, regParams(first, id))
	if err != nil {
		t.Fatalf("第一屆報名: %v", err)
	}
	// 第一屆評段 → 回寫 fencers.last_rank_level。
	if _, err := ranks.SetPlayerRank(ctx, tournament.SetPlayerRankParams{
		TournamentID: first, PlayerID: reg1.Player.ID, Rank: bp.RankFeihua,
		ActorUserID: actor, Reason: "評段",
	}); err != nil {
		t.Fatalf("評段: %v", err)
	}

	second := newTournament(t, tournament.PhaseSignup)
	reg2, err := svc.CreateRegistration(ctx, regParams(second, id))
	if err != nil {
		t.Fatalf("第二屆報名: %v", err)
	}
	if !reg2.ReturningFencer {
		t.Fatal("應辨識為回鍋選手")
	}
	if reg2.Fencer.ID != reg1.Fencer.ID {
		t.Fatalf("應接上同一份檔案: %d vs %d", reg2.Fencer.ID, reg1.Fencer.ID)
	}
	// PreviousRank 是往屆的段位,不是本屆的 —— 本屆要等裁判重新評定。
	if reg2.PreviousRank != bp.RankFeihua {
		t.Fatalf("往屆段位應為飛花(3),得到 %d", reg2.PreviousRank)
	}
	if reg2.Player.Rank != bp.RankUnspecified {
		t.Fatal("本屆段位不該被往屆值覆寫")
	}
	if _, played, _ := fencerRow(t, reg2.Fencer.ID); played != 2 {
		t.Fatalf("參賽屆數應為 2,得到 %d", played)
	}
	if countFencers(t, id) != 1 {
		t.Fatal("同一個遊戲ID 只能有一份跨屆檔案")
	}
}

func TestDuplicateRegistrationRejected(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("重複")

	if _, err := svc.CreateRegistration(ctx, regParams(tid, id)); err != nil {
		t.Fatalf("第一次報名: %v", err)
	}
	_, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if !errors.Is(err, signup.ErrAlreadyRegistered) {
		t.Fatalf("重複報名應回 ErrAlreadyRegistered,得到 %v", err)
	}
	// 失敗的那一筆整個 rollback:參賽屆數不該被多加一次。
	var played int32
	if err := pool.QueryRow(ctx,
		`SELECT tournaments_played FROM activity.fencers WHERE game_id = $1`, id).Scan(&played); err != nil {
		t.Fatalf("讀參賽屆數: %v", err)
	}
	if played != 1 {
		t.Fatalf("參賽屆數應為 1,得到 %d", played)
	}
}

func TestRegistrationRequiresSignupPhase(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignupClosed)
	id := gameID("遲到")

	_, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if !errors.Is(err, tournament.ErrWrongPhase) {
		t.Fatalf("非報名期應回 ErrWrongPhase,得到 %v", err)
	}
	// 階段條件不成立時整筆退回,不該留下孤兒 fencer ——
	// 留下來的話,這個人下次報名會被誤判成回鍋選手。
	if countFencers(t, id) != 0 {
		t.Fatal("被拒絕的報名不該留下選手檔案")
	}
}

// TestConcurrentRegistrationSameTournament 是連點報名鈕:
// UNIQUE (tournament_id, fencer_id) 是唯一權威,不加冪等鍵。
func TestConcurrentRegistrationSameTournament(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("連點")

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.CreateRegistration(ctx, regParams(tid, id))
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, dup int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, signup.ErrAlreadyRegistered):
			dup++
		default:
			t.Fatalf("非預期錯誤: %v", err)
		}
	}
	if ok != 1 || dup != n-1 {
		t.Fatalf("應恰有一筆成功,得到 成功=%d 重複=%d", ok, dup)
	}
	if countFencers(t, id) != 1 {
		t.Fatalf("並發只能建出一份選手檔案,得到 %d 份", countFencers(t, id))
	}
}

// TestConcurrentRegistrationSameFencerTwoTournaments 驗證 InsertFencerIfAbsent
// 的 DO NOTHING 路徑:兩邊都查不到、都想建,撞鍵的那一邊必須重讀既有列
// 繼續走完報名,而不是把錯誤丟回給使用者。
func TestConcurrentRegistrationSameFencerTwoTournaments(t *testing.T) {
	setup(t)
	ctx := context.Background()
	id := gameID("同時報兩屆")
	tids := []int64{
		newTournament(t, tournament.PhaseSignup),
		newTournament(t, tournament.PhaseSignup),
	}

	errs := make([]error, len(tids))
	fencerIDs := make([]int64, len(tids))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, tid := range tids {
		wg.Add(1)
		go func(i int, tid int64) {
			defer wg.Done()
			<-start
			reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
			errs[i] = err
			fencerIDs[i] = reg.Fencer.ID
		}(i, tid)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 屆報名失敗: %v", i, err)
		}
	}
	if fencerIDs[0] != fencerIDs[1] {
		t.Fatalf("兩筆報名應接到同一份檔案: %d vs %d", fencerIDs[0], fencerIDs[1])
	}
	if countFencers(t, id) != 1 {
		t.Fatalf("並發只能建出一份選手檔案,得到 %d 份", countFencers(t, id))
	}
	if _, played, _ := fencerRow(t, fencerIDs[0]); played != 2 {
		t.Fatalf("參賽屆數應為 2,得到 %d", played)
	}
}

// ── 登入查詢與換發通行碼 ──────────────────────────────────────

func TestPlayerByGameID(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("登入")
	reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}

	player, err := svc.PlayerByGameID(ctx, tid, id)
	if err != nil {
		t.Fatalf("查選手: %v", err)
	}
	if player.ID != reg.Player.ID || player.GameID != id {
		t.Fatalf("查到的不是同一位: %+v", player)
	}
	if player.Status != tournament.PlayerActive {
		t.Fatalf("狀態 = %s,要 active", player.Status)
	}

	// 查無此遊戲ID 與「非 active」在上層是同一個錯誤,這裡只負責回「查不到」。
	if _, err := svc.PlayerByGameID(ctx, tid, "不存在的ID"); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("查無應回 ErrPlayerNotFound,得到 %v", err)
	}
	// 別屆的遊戲ID 在這屆也查不到。
	other := newTournament(t, tournament.PhaseSignup)
	if _, err := svc.PlayerByGameID(ctx, other, id); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("跨屆應回 ErrPlayerNotFound,得到 %v", err)
	}
}

// TestPlayerByGameIDDoesNotFilterStatus:棄賽者這一層照樣查得到,狀態原樣帶出。
//
// 「誰登得進來」的權威只有 signup.Service.Login 一處 —— 它要把「非 active」與
// 「查無此人」折成同一個答案。這裡先偷偷過濾掉的話,那個判斷就有兩個家,
// 而日後改規則(例如淘汰者仍可登入看戰績)只會有人改到其中一個。
func TestPlayerByGameIDDoesNotFilterStatus(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("棄賽")
	reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}

	for _, status := range []tournament.PlayerStatus{tournament.PlayerWithdrawn, tournament.PlayerEliminated} {
		setStatus(t, reg.Player.ID, status)
		player, err := svc.PlayerByGameID(ctx, tid, id)
		if err != nil {
			t.Fatalf("%s 的選手仍應查得到: %v", status, err)
		}
		if player.Status != status {
			t.Fatalf("狀態沒帶出來: %s,要 %s", player.Status, status)
		}
	}
}

// TestRegistrationWithOnlyGameID:選填欄位全部留空也要寫得進去。
//
// 報名只有遊戲ID 必填(2026-09-13),而空字串在 SQL 那層要變成 NULL ——
// 「沒填」如果同時有 ” 與 NULL 兩種表示法,之後每個讀取端都得各判一次。
func TestRegistrationWithOnlyGameID(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("只有ID")

	reg, err := svc.CreateRegistration(ctx, signup.CreateRegistrationParams{
		TournamentID: tid,
		RequirePhase: tournament.PhaseSignup,
		GameID:       id,
		DisplayName:  id, // service 已把留空的顯示名補成 game_id
		PasscodeHash: "pbkdf2-sha256$1$x$y",
	})
	if err != nil {
		t.Fatalf("只填遊戲ID 的報名應該寫得進去: %v", err)
	}
	if reg.Player.DiscordName != "" || reg.Player.LadderRank != "" || reg.Player.ArtsNote != "" {
		t.Fatalf("選填欄位不該被填上東西: %+v", reg.Player)
	}
	if reg.Player.SelfRatedRank != bp.RankUnspecified || reg.Player.LadderScore != 0 {
		t.Fatalf("沒填的數值欄位應為零值: %+v", reg.Player)
	}

	var ladderRank, artsNote *string
	if err := pool.QueryRow(ctx,
		`SELECT ladder_rank, arts_note FROM activity.tournament_players WHERE id = $1`,
		reg.Player.ID,
	).Scan(&ladderRank, &artsNote); err != nil {
		t.Fatalf("讀選填欄位: %v", err)
	}
	if ladderRank != nil || artsNote != nil {
		t.Fatalf("空字串要被 NULLIF 成 NULL,得到 %v / %v", ladderRank, artsNote)
	}
	// 沒填 Discord 名稱時,跨屆檔案那一欄維持原樣(COALESCE(NULLIF(...)))。
	if reg.Fencer.DiscordName != "" {
		t.Fatalf("初次報名沒填 Discord 名稱,跨屆檔案不該有值: %q", reg.Fencer.DiscordName)
	}

	// 登入這條路走得通:只填遊戲ID 的人照樣是一位正常的參賽者。
	if _, err := svc.PlayerByGameID(ctx, tid, id); err != nil {
		t.Fatalf("查得到才登得進去: %v", err)
	}
}

// TestUpdatePasscodeInvalidatesOldHash:換發把 passcode_hash 與
// passcode_issued_at 一起換掉 —— 後者是已發出 session 的作廢依據,
// 也是這個動作現在全部的效果(登入不再比對雜湊)。
func TestUpdatePasscodeInvalidatesOldHash(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("換碼")
	reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}
	beforeHash, beforeIssued := passcodeRow(t, reg.Player.ID)

	time.Sleep(2 * time.Millisecond) // issued_at 要看得出前後
	const newHash = "argon2-hash-新的"
	if err := svc.UpdatePasscode(ctx, signup.UpdatePasscodeParams{
		TournamentID: tid, PlayerID: reg.Player.ID, PasscodeHash: newHash,
		ActorUserID: actor, Reason: "選手抄丟了",
	}); err != nil {
		t.Fatalf("換發通行碼: %v", err)
	}

	afterHash, afterIssued := passcodeRow(t, reg.Player.ID)
	if afterHash != newHash {
		t.Fatalf("雜湊應被覆寫,得到 %s", afterHash)
	}
	if afterHash == beforeHash {
		t.Fatal("舊雜湊還在,這次換發什麼也沒換掉")
	}
	if !afterIssued.After(beforeIssued) {
		t.Fatalf("issued_at 應往前走: %v → %v", beforeIssued, afterIssued)
	}

	// 換發不是封鎖:這位選手照樣登得進來(要擋人請用棄賽)。
	if _, err := svc.PlayerByGameID(ctx, tid, id); err != nil {
		t.Fatalf("換發之後仍應查得到: %v", err)
	}

	// 稽核紀錄要寫,而且**絕不可**含明碼或雜湊。
	var action, reason string
	var afterJSON []byte
	if err := pool.QueryRow(ctx,
		`SELECT action, reason, after FROM platform.admin_audit_logs
		 WHERE target_id = $1 ORDER BY id DESC LIMIT 1`, reg.Player.ID,
	).Scan(&action, &reason, &afterJSON); err != nil {
		t.Fatalf("讀稽核紀錄: %v", err)
	}
	if action != "tournament.passcode_regenerated" || reason != "選手抄丟了" {
		t.Fatalf("稽核內容不對: action=%s reason=%s", action, reason)
	}
	if strings.Contains(string(afterJSON), newHash) || strings.Contains(string(afterJSON), "hash") {
		t.Fatalf("稽核紀錄不可含通行碼雜湊: %s", afterJSON)
	}
}

func TestUpdatePasscodeRejects(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("換碼失敗")
	reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}

	// 沒有「選手自助重取」這條路:沒帶裁判就不做。
	if err := svc.UpdatePasscode(ctx, signup.UpdatePasscodeParams{
		TournamentID: tid, PlayerID: reg.Player.ID, PasscodeHash: "x",
	}); !errors.Is(err, tournament.ErrActorRequired) {
		t.Fatalf("沒帶裁判應回 ErrActorRequired,得到 %v", err)
	}
	if err := svc.UpdatePasscode(ctx, signup.UpdatePasscodeParams{
		TournamentID: tid, PlayerID: reg.Player.ID + 100000, PasscodeHash: "x", ActorUserID: actor,
	}); !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("查無選手應回 ErrPlayerNotFound,得到 %v", err)
	}
	// 兩次失敗都不該改到任何東西。
	if hash, _ := passcodeRow(t, reg.Player.ID); hash != "argon2-hash-"+id {
		t.Fatalf("雜湊不該被改動,得到 %s", hash)
	}
}

// ── 綁定平台帳號 ──────────────────────────────────────────────

// TestBindWritesBothTables:只寫一處的話,領獎時查哪一張表會得到不同答案。
func TestBindWritesBothTables(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	id := gameID("綁定")
	reg, err := svc.CreateRegistration(ctx, regParams(tid, id))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}
	user := newUser(t)

	got, err := svc.BindPlatformAccount(ctx, signup.BindParams{
		TournamentID: tid, PlayerID: reg.Player.ID, FencerID: reg.Fencer.ID, UserID: user,
	})
	if err != nil {
		t.Fatalf("綁定: %v", err)
	}
	if !got.Bound() || *got.UserID != user {
		t.Fatalf("回傳的選手應已綁定,得到 %v", got.UserID)
	}
	if _, _, fencerUser := fencerRow(t, reg.Fencer.ID); fencerUser == nil || *fencerUser != user {
		t.Fatalf("跨屆檔案也要綁上,得到 %v", fencerUser)
	}

	// 重複綁同一個帳號是冪等的成功:使用者連點兩次不該看到錯誤。
	if _, err := svc.BindPlatformAccount(ctx, signup.BindParams{
		TournamentID: tid, PlayerID: reg.Player.ID, FencerID: reg.Fencer.ID, UserID: user,
	}); err != nil {
		t.Fatalf("重複綁定同一帳號應成功: %v", err)
	}

	// 改綁收款人要裁判介入並留稽核,不能靠這支 API 悄悄換掉。
	other := newUser(t)
	if _, err := svc.BindPlatformAccount(ctx, signup.BindParams{
		TournamentID: tid, PlayerID: reg.Player.ID, FencerID: reg.Fencer.ID, UserID: other,
	}); !errors.Is(err, signup.ErrAlreadyBound) {
		t.Fatalf("改綁應回 ErrAlreadyBound,得到 %v", err)
	}
	if _, _, fencerUser := fencerRow(t, reg.Fencer.ID); fencerUser == nil || *fencerUser != user {
		t.Fatalf("被拒絕的改綁不該動到跨屆檔案,得到 %v", fencerUser)
	}
}

// TestBindRejectsAccountAlreadyUsed:一個帳號只能是一個人,
// 否則獎金會匯給「兩個人共用的那個錢包」。
func TestBindRejectsAccountAlreadyUsed(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	user := newUser(t)

	first, err := svc.CreateRegistration(ctx, regParams(tid, gameID("甲")))
	if err != nil {
		t.Fatalf("甲報名: %v", err)
	}
	second, err := svc.CreateRegistration(ctx, regParams(tid, gameID("乙")))
	if err != nil {
		t.Fatalf("乙報名: %v", err)
	}
	if _, err := svc.BindPlatformAccount(ctx, signup.BindParams{
		TournamentID: tid, PlayerID: first.Player.ID, FencerID: first.Fencer.ID, UserID: user,
	}); err != nil {
		t.Fatalf("甲綁定: %v", err)
	}

	_, err = svc.BindPlatformAccount(ctx, signup.BindParams{
		TournamentID: tid, PlayerID: second.Player.ID, FencerID: second.Fencer.ID, UserID: user,
	})
	if !errors.Is(err, signup.ErrUserAlreadyBound) {
		t.Fatalf("帳號已綁別人應回 ErrUserAlreadyBound,得到 %v", err)
	}
	// 兩張表要一起改,所以失敗的那一筆連 tournament_players 都不該留下痕跡。
	var playerUser *int64
	if err := pool.QueryRow(ctx,
		`SELECT user_id FROM activity.tournament_players WHERE id = $1`, second.Player.ID,
	).Scan(&playerUser); err != nil {
		t.Fatalf("讀選手綁定: %v", err)
	}
	if playerUser != nil {
		t.Fatalf("失敗的綁定不該寫進 tournament_players,得到 %v", *playerUser)
	}
}

func TestBindRequiresUser(t *testing.T) {
	setup(t)
	_, err := svc.BindPlatformAccount(context.Background(), signup.BindParams{
		TournamentID: 1, PlayerID: 1, FencerID: 1,
	})
	if !errors.Is(err, signup.ErrUserRequired) {
		t.Fatalf("沒帶帳號應回 ErrUserRequired,得到 %v", err)
	}
}

// TestConcurrentBindSameUser 兩個裝置同時按綁定:兩邊綁的是同一個帳號,
// 所以兩邊都該成功(冪等),而且只寫出一份綁定。
func TestConcurrentBindSameUser(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)
	reg, err := svc.CreateRegistration(ctx, regParams(tid, gameID("併發綁定")))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}
	user := newUser(t)

	errs := make([]error, 3)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.BindPlatformAccount(ctx, signup.BindParams{
				TournamentID: tid, PlayerID: reg.Player.ID, FencerID: reg.Fencer.ID, UserID: user,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 次綁定失敗: %v", i, err)
		}
	}
	if _, _, fencerUser := fencerRow(t, reg.Fencer.ID); fencerUser == nil || *fencerUser != user {
		t.Fatalf("跨屆檔案應綁到 %d,得到 %v", user, fencerUser)
	}
}

// TestPasscodeIssuedAtRequiresActivePlayer:非參賽中的選手,session 驗證當場失效。
//
// 這條是「棄賽 = 鎖住這個人」成立的前提。登入不再需要任何秘密(只要遊戲ID),
// 所以狀態是唯一能把人擋在外面的東西 —— 如果它只擋新登入、不影響已發出的
// token,那道鎖要等 12 小時才生效,而裁判按下棄賽的時候要的是現在。
//
// 已淘汰者一起被擋是刻意的:session 代表「以參賽者身分動作」的權利,
// 而對戰表與戰績本來就公開。
func TestPasscodeIssuedAtRequiresActivePlayer(t *testing.T) {
	setup(t)
	ctx := context.Background()
	tid := newTournament(t, tournament.PhaseSignup)

	var slug string
	if err := pool.QueryRow(ctx,
		`SELECT slug FROM activity.tournaments WHERE id = $1`, tid).Scan(&slug); err != nil {
		t.Fatalf("讀 slug: %v", err)
	}

	reg, err := svc.CreateRegistration(ctx, regParams(tid, gameID("狀態")))
	if err != nil {
		t.Fatalf("報名: %v", err)
	}
	if _, err := svc.PasscodeIssuedAt(ctx, slug, reg.Player.PublicID); err != nil {
		t.Fatalf("參賽中的人應該通得過: %v", err)
	}

	for _, status := range []string{"withdrawn", "eliminated"} {
		t.Run(status, func(t *testing.T) {
			if _, err := pool.Exec(ctx,
				`UPDATE activity.tournament_players SET status = $1 WHERE id = $2`,
				status, reg.Player.ID); err != nil {
				t.Fatalf("改狀態: %v", err)
			}
			_, err := svc.PasscodeIssuedAt(ctx, slug, reg.Player.PublicID)
			if !errors.Is(err, tournament.ErrPlayerNotFound) {
				t.Fatalf("狀態 %s 應該與查無此人同一個錯誤,得到 %v", status, err)
			}
		})
	}
}
