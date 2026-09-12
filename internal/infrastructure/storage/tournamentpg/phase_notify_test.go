package tournamentpg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
)

// 階段變更的即時推播(watch.KindPhase)。
//
// 本檔驗的是**一條性質**:通知只在 commit 時送出,失敗的交易一個字都不送。
// 那是選擇 LISTEN / NOTIFY 而不是行程內 hub 的唯一理由 ——
// 沒有它,觀眾就可能看到一個後來被撤銷的階段(例如「已開賽」跳出來,
// 幾秒後又退回「抽籤中」,而資料庫裡從頭到尾沒有那件事)。

// ── 原始監聽連線 ────────────────────────────────────────────────

// phaseWatcher 是一條自己 LISTEN 的連線,直接檢查「線上真的飛了什麼」。
//
// 不走 watchpg.Listener + Hub:這裡要驗的是**有沒有送出通知**,
// 經過那兩層會多一個「讀不讀得出內容」的變因,測失敗時分不清是哪一層的問題。
type phaseWatcher struct {
	t    *testing.T
	conn *pgx.Conn
}

func newPhaseWatcher(t *testing.T) *phaseWatcher {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("撥監聽連線: %v", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{watch.Channel}.Sanitize()); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return &phaseWatcher{t: t, conn: conn}
}

// next 等下一則信封;逾時回 ok=false。
func (w *phaseWatcher) next(timeout time.Duration) (watch.Envelope, bool) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	n, err := w.conn.WaitForNotification(ctx)
	if err != nil {
		return watch.Envelope{}, false
	}
	e, err := watch.ParseEnvelope(n.Payload)
	if err != nil {
		w.t.Fatalf("線上飛過來的信封解不開:%q %v", n.Payload, err)
	}
	return e, true
}

// expectSilence 斷言這段時間內**沒有**任何通知。
func (w *phaseWatcher) expectSilence(d time.Duration, why string) {
	w.t.Helper()
	if e, ok := w.next(d); ok {
		w.t.Fatalf("%s,但收到了通知 %+v", why, e)
	}
}

// expectPhase 斷言下一則是指定賽事的階段變化。
func (w *phaseWatcher) expectPhase(slug string) {
	w.t.Helper()
	e, ok := w.next(5 * time.Second)
	if !ok {
		w.t.Fatal("等不到階段推播")
	}
	if e.Tournament != slug || e.Kind != watch.KindPhase {
		w.t.Fatalf("信封 = %+v,要 {%s phase}", e, slug)
	}
	// Ref 必須是空的:階段變化的對象是賽事本身,而賽事已經由 slug 指定了。
	// 塞一個 id 進去會讓收端去讀一個不存在的場次,然後把整則丟掉。
	if e.Ref != "" {
		w.t.Errorf("階段信封不該帶 Ref,得到 %q", e.Ref)
	}
}

// ── 只在 commit 時送出 ──────────────────────────────────────────

// failAtCommit 裝一個**延遲到 commit 才觸發**的約束觸發器,並回傳拆除它的函式。
//
// 為什麼要這麼麻煩:要證明的是「一個已經送出 NOTIFY 的交易,最後沒 commit 成功
// 就等於沒送」。用「稽核的 FK 失敗」來製造失敗也能讓交易 rollback,但那種失敗
// 發生在 pg_notify **之前或之後**取決於 adapter 裡兩行的順序 —— 哪天有人把
// 那兩行對調,測試會在什麼都沒驗到的情況下繼續綠。
//
// DEFERRABLE INITIALLY DEFERRED 的約束觸發器在 COMMIT 那一刻才跑,
// 也就是在這個交易做完的每一件事(含 pg_notify)之後。它與順序無關。
func failAtCommit(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`CREATE OR REPLACE FUNCTION activity.test_fail_at_commit() RETURNS trigger
		 LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '測試用:commit 前刻意失敗'; END $$`,
		`CREATE CONSTRAINT TRIGGER test_fail_at_commit
		 AFTER UPDATE ON activity.tournaments
		 DEFERRABLE INITIALLY DEFERRED
		 FOR EACH ROW EXECUTE FUNCTION activity.test_fail_at_commit()`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("裝失敗觸發器: %v", err)
		}
	}
	return func() {
		if _, err := pool.Exec(ctx,
			`DROP TRIGGER IF EXISTS test_fail_at_commit ON activity.tournaments`); err != nil {
			t.Fatalf("拆失敗觸發器: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DROP FUNCTION IF EXISTS activity.test_fail_at_commit()`); err != nil {
			t.Fatalf("拆失敗觸發器函式: %v", err)
		}
	}
}

// TestPhaseNotifyOnlyOnCommit 是本檔最重要的一支。
//
// 兩段:
//  1. 一個走完全程、連 pg_notify 都下了,但 commit 失敗的交易 —— 線上一片安靜,
//     而且階段沒有變。觀眾因此不可能看到一個沒有發生的階段。
//  2. 對照組:同一屆、同一個轉換再跑一次(這次會成功)—— 通知到了。
//     沒有這一段,上面那句「沒收到」根本不能當證據:監聽連線可能從頭到尾就是壞的。
func TestPhaseNotifyOnlyOnCommit(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	id, slug := newTournament(t, tournament.PhaseSignup)
	w := newPhaseWatcher(t)

	// ── 1. commit 失敗的交易不送 ──
	undo := failAtCommit(t)
	err := svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
		TournamentID: id,
		From:         tournament.PhaseSignup,
		To:           tournament.PhaseSignupClosed,
		ActorUserID:  actor,
		Reason:       "封閉報名",
	})
	undo()
	if err == nil {
		t.Fatal("commit 應該失敗,測試前提不成立")
	}
	// 錯誤必須來自 **commit** 那一步。這一句是整段的前提:它證明交易主體
	// (含 pg_notify)每一句都跑完了、失敗發生在最後 —— 否則「沒收到通知」
	// 的原因可能只是「根本還沒送」,那就什麼都沒驗到。
	if !strings.HasPrefix(err.Error(), "commit:") {
		t.Fatalf("錯誤 = %v,要在 commit 階段失敗", err)
	}
	w.expectSilence(700*time.Millisecond, "交易沒 commit 成功,通知不該送出")
	if got := phaseOf(t, id); got != string(tournament.PhaseSignup) {
		t.Fatalf("階段 = %s,要 signup(整筆應該被 rollback)", got)
	}

	// ── 2. 對照組:同樣的轉換,這次會 commit ──
	if err := svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
		TournamentID: id,
		From:         tournament.PhaseSignup,
		To:           tournament.PhaseSignupClosed,
		ActorUserID:  actor,
		Reason:       "封閉報名",
	}); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	w.expectPhase(slug)
	if got := phaseOf(t, id); got != string(tournament.PhaseSignupClosed) {
		t.Fatalf("階段 = %s,要 signup_closed", got)
	}
}

// TestPhaseConflictSendsNothing 驗樂觀鎖輸的那一方不發通知。
//
// 兩位裁判同時按「進入下一階段」時只有一個真的改到東西。輸的那個如果也發了
// 一則通知,觀眾的畫面就會為了一件沒有發生的事重新整理一次 ——
// 而在決賽當下,那是所有人同時對伺服器再打一輪查詢。
func TestPhaseConflictSendsNothing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	id, slug := newTournament(t, tournament.PhaseSignup)
	w := newPhaseWatcher(t)

	// 先推一次(這一則會送)。
	if err := svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
		TournamentID: id, From: tournament.PhaseSignup, To: tournament.PhaseSignupClosed,
		ActorUserID: actor, Reason: "第一次",
	}); err != nil {
		t.Fatalf("第一次推進: %v", err)
	}
	w.expectPhase(slug)

	// 再推一次同樣的轉換:from 已經對不上了。
	err := svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
		TournamentID: id, From: tournament.PhaseSignup, To: tournament.PhaseSignupClosed,
		ActorUserID: actor, Reason: "遲到的第二次",
	})
	if !errors.Is(err, tournament.ErrPhaseConflict) {
		t.Fatalf("第二次 = %v,要 ErrPhaseConflict", err)
	}
	w.expectSilence(700*time.Millisecond, "什麼都沒改到的推進不該送通知")
}

// TestConcurrentPhaseAdvanceSendsExactlyOne 驗兩位裁判同時按下去,線上只飛一則。
//
// 併發必須對真的資料庫測:贏家由 WHERE phase = @from 這道樂觀鎖決定,
// 而「輸的那一方有沒有順手發一則通知」正是 mock 掉資料庫之後看不到的東西。
func TestConcurrentPhaseAdvanceSendsExactlyOne(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	id, slug := newTournament(t, tournament.PhaseSignup)
	w := newPhaseWatcher(t)

	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- svc.UpdatePhase(ctx, tournament.UpdatePhaseParams{
				TournamentID: id, From: tournament.PhaseSignup, To: tournament.PhaseSignupClosed,
				ActorUserID: actor, Reason: "同時按下去",
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, tournament.ErrPhaseConflict):
		default:
			t.Errorf("非預期的錯誤:%v", err)
		}
	}
	if won != 1 {
		t.Fatalf("成功的有 %d 個,要恰好 1 個", won)
	}
	w.expectPhase(slug)
	w.expectSilence(700*time.Millisecond, "只有一次真的改到階段,線上就只該有一則")
}

// TestRollbackToRankedNotifies 驗退回 ranked 也會推播。
//
// 它同樣是一次階段變更,而且是後果最大的那一種:對戰表在這一刻整張被刪掉。
// 正在看對戰表的觀眾如果沒收到通知,畫面就會停在一張已經不存在的表上,
// 直到他自己重新整理為止。
func TestRollbackToRankedNotifies(t *testing.T) {
	setup(t)
	ctx := context.Background()
	actor := newUser(t)
	id, slug, _ := drawableTournament(t, 4) // 已在 drawing 階段
	w := newPhaseWatcher(t)
	if err := svc.RollbackToRanked(ctx, tournament.RollbackParams{
		TournamentID: id, ActorUserID: actor, Reason: "重新評段",
	}); err != nil {
		t.Fatalf("RollbackToRanked: %v", err)
	}
	w.expectPhase(slug)

	// 階段對不上的退回什麼都沒改到,同樣不該送。
	err := svc.RollbackToRanked(ctx, tournament.RollbackParams{
		TournamentID: id, ActorUserID: actor, Reason: "再退一次",
	})
	if !errors.Is(err, tournament.ErrPhaseConflict) {
		t.Fatalf("第二次退回 = %v,要 ErrPhaseConflict", err)
	}
	w.expectSilence(700*time.Millisecond, "什麼都沒改到的退回不該送通知")
}
