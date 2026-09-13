package matchpg_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activityreadpg"
)

// 本檔對真 Postgres 驗 2026-09-13 增補的裁判動線(migration 00006):
// 設定確認、逐回合、季軍戰、違規。core 的假物件已經驗過編排順序;
// 這裡要驗的是**只有資料庫答得出來的事**:CHECK 與 UNIQUE 真的擋、0 列歸因對、
// 併發連點只成功一次、rollback 連稽核與回合一起消失。

// ── 完整流程(驗收條件 2)────────────────────────────────────────

func TestMultiRoundFlow(t *testing.T) {
	f := newFixture(t)
	f.setConfig(3, false)
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	final := f.match(2, 0, 0, 0, match.StatusPending)
	f.runToLocked(m)
	betID := f.bet(m.id)

	st := f.state(m.publicID)
	if st.ConfirmedAt == nil || st.ConfirmedBy != f.judge || st.StartedAt != nil {
		t.Fatalf("設定確認後、開打前:%+v", st)
	}
	if len(f.bets.opened) != 1 || f.bets.opened[0] != m.publicID {
		t.Fatalf("盤口應在開盤時建:%v", f.bets.opened)
	}

	r1 := f.startRound(m)
	st = f.state(m.publicID)
	switch {
	case st.Status != match.StatusLive || st.StartedAt == nil:
		t.Fatalf("第一回合開始 = 開打:%+v", st)
	case r1.Round.RoundNo != 1 || r1.ClosedMarkets != 1:
		t.Fatalf("第一回合應關掉那個盤口:%+v", r1)
	case len(f.bets.closed) != 1:
		t.Fatalf("關盤應恰好一次:%v", f.bets.closed)
	}
	if rs := f.rounds(m.id); len(rs) != 1 || rs[0].FinishedAt != nil {
		t.Fatalf("回合列:%+v", rs)
	}

	f1 := f.finishRound(m, 1, a)
	if f1.Decided || f1.Score.String() != "1:0" {
		t.Fatalf("1:0 不該定案:%+v", f1)
	}
	if f.state(m.publicID).Status != match.StatusLive {
		t.Fatal("1:0 後場次應仍是 live")
	}

	f.startRound(m)
	f2 := f.finishRound(m, 2, a)
	st = f.state(m.publicID)
	switch {
	case !f2.Decided || f2.Outcome == nil:
		t.Fatalf("2:0 應整場定案:%+v", f2)
	case st.Status != match.StatusDone || st.Winner != a.id || st.FinishedAt == nil:
		t.Fatalf("場次未正確結束:%+v", st)
	case f.state(final.publicID).P1 != a.id:
		t.Fatal("勝者應晉級決賽 P1")
	case f.playerStatus(b.publicID) != string(tournament.PlayerEliminated):
		t.Fatal("敗方應標 eliminated")
	case f.betStatus(betID) != "won":
		t.Fatalf("整場盤口應結算,注單狀態 %q", f.betStatus(betID))
	}
	rs := f.rounds(m.id)
	if len(rs) != 2 || rs[0].Winner != a.id || rs[1].Winner != a.id || rs[1].FinishedAt == nil {
		t.Fatalf("回合列:%+v", rs)
	}

	// Bets 埠的順序:OpenMarkets(開盤)→ CloseMarkets(第 1 回合)→ SettleRound ×2 → SettleMatch ×1。
	if got := f.bets.settledRounds; len(got) != 2 || got[0] != m.publicID+"#1" || got[1] != m.publicID+"#2" {
		t.Fatalf("單回合結算:%v", got)
	}
	if len(f.bets.settled) != 1 {
		t.Fatalf("整場結算應恰好一次:%v", f.bets.settled)
	}

	// 稽核:每一步都在。
	acts := f.auditActions()
	for action, n := range map[string]int{
		match.ActionConfirmSetup: 1, match.ActionStartMatch: 1, match.ActionStartRound: 2,
		match.ActionFinishRound: 2, match.ActionReportResult: 1,
	} {
		if got := count(acts, action); got != n {
			t.Fatalf("稽核 %s = %d,期望 %d(全部:%v)", action, got, n, acts)
		}
	}
	// 設定確認的稽核 after 要帶清單(這次替身沒給清單,至少要有這個鍵)。
	for _, a := range f.audits() {
		if a.Action == match.ActionConfirmSetup {
			if _, ok := a.After["checklist"]; !ok {
				t.Fatalf("設定確認的稽核缺 checklist:%+v", a.After)
			}
		}
	}

	// outbox:回合事件不進 outbox;賽果公告帶比數。
	topics := f.topics()
	if count(topics, match.TopicRoundStarted)+count(topics, match.TopicRoundFinished) != 0 {
		t.Fatalf("回合事件不該進 outbox:%v", topics)
	}
	for _, e := range f.events() {
		if e.Topic != match.TopicMatchFinished {
			continue
		}
		res := e.Payload["result"].(map[string]any)
		score, ok := res["score"].(map[string]any)
		if !ok || score["text"] != "2:0" {
			t.Fatalf("賽果公告要帶回合比數:%+v", res)
		}
	}
}

// ── 未確認不得開打:應用層 + DB 層各一道(驗收條件 3)─────────────

func TestStartRoundRequiresSetupConfirmed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	if _, err := f.svc.OpenHandicap(ctx, match.OpenHandicapParams{MatchPublicID: m.publicID, ActorUserID: f.judge}); err != nil {
		t.Fatalf("開盤: %v", err)
	}
	if _, err := f.svc.LockHandicap(ctx, match.LockHandicapParams{MatchPublicID: m.publicID, Confirm: true, ActorUserID: f.judge}); err != nil {
		t.Fatalf("封盤: %v", err)
	}

	// 應用層:給得出人話,而且什麼都沒寫。
	_, err := f.svc.StartRound(ctx, m.publicID, f.judge)
	if !errors.Is(err, match.ErrSetupNotConfirmed) {
		t.Fatalf("err = %v,期望 ErrSetupNotConfirmed", err)
	}
	if st := f.state(m.publicID); st.Status != match.StatusLocked || st.StartedAt != nil {
		t.Fatalf("被擋下來卻開打了:%+v", st)
	}
	if len(f.rounds(m.id)) != 0 || len(f.bets.closed) != 0 {
		t.Fatal("被擋下來卻建了回合或關了盤")
	}

	// DB 層:直接跳過應用層寫 started_at,被 matches_started_requires_setup_check 擋(23514)。
	_, err = pool.Exec(ctx, `UPDATE activity.matches SET started_at = now(), status = 'live' WHERE id = $1`, m.id)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "matches_started_requires_setup_check" {
		t.Fatalf("繞過應用層直接寫 started_at 應被 CHECK 擋,得到 %v", err)
	}
	// 而 MarkMatchLive 的 WHERE 也擋(0 列 → 歸因成未確認),不是靠 CHECK 才失敗。
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.MarkLive(ctx, tx, match.LiveWrite{MatchID: m.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrSetupNotConfirmed) {
		t.Fatalf("MarkLive 應歸因成 ErrSetupNotConfirmed,得到 %v", err)
	}
	// 封盤前也不能確認(DB CHECK:setup_confirmed_at 只能在 locked/live/done)。
	other := f.match(1, 1, a.id, b.id, match.StatusReady)
	_, err = pool.Exec(ctx, `UPDATE activity.matches SET setup_confirmed_at = now() WHERE id = $1`, other.id)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("封盤前寫 setup_confirmed_at 應被 CHECK 擋,得到 %v", err)
	}
}

// ── 回合的 DB 側守門(驗收條件 4)─────────────────────────────────

func TestRoundGuardsOnDB(t *testing.T) {
	f := newFixture(t)
	f.setConfig(3, false)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	c := f.player("丙", 3)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	f.match(2, 0, 0, 0, match.StatusPending)
	f.runToLocked(m)
	f.startRound(m)

	// 勝者不在場上 → 應用層擋,回合不動。
	if _, err := f.svc.FinishRound(ctx, match.FinishRoundParams{MatchPublicID: m.publicID, RoundNo: 1, WinnerPublicID: c.publicID, ActorUserID: f.judge, Note: "", Confirm: true}); !errors.Is(err, match.ErrWinnerNotInMatch) {
		t.Fatalf("err = %v,期望 ErrWinnerNotInMatch", err)
	}
	// 上一回合沒結束不能開下一回合;直接對 DB 建同一回合撞 UNIQUE → ErrRoundInProgress。
	if _, err := f.svc.StartRound(ctx, m.publicID, f.judge); !errors.Is(err, match.ErrRoundInProgress) {
		t.Fatalf("err = %v,期望 ErrRoundInProgress", err)
	}
	err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.StartRound(ctx, tx, match.RoundStartWrite{MatchID: m.id, RoundNo: 1, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrRoundInProgress) {
		t.Fatalf("同一回合建第二列應撞 UNIQUE 並歸因,得到 %v", err)
	}

	f.finishRound(m, 1, a)
	// 同一回合結束兩次:service 擋;直接對 DB 再 UPDATE 也是 0 列 → ErrRoundAlreadyFinished。
	if _, err := f.svc.FinishRound(ctx, match.FinishRoundParams{MatchPublicID: m.publicID, RoundNo: 1, WinnerPublicID: b.publicID, ActorUserID: f.judge, Note: "", Confirm: true}); !errors.Is(err, match.ErrRoundAlreadyFinished) {
		t.Fatalf("err = %v,期望 ErrRoundAlreadyFinished", err)
	}
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.FinishRound(ctx, tx, match.RoundFinishWrite{MatchID: m.id, RoundNo: 1, WinnerPlayerID: b.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrRoundAlreadyFinished) {
		t.Fatalf("finished_at IS NULL 守門應歸因成 ErrRoundAlreadyFinished,得到 %v", err)
	}
	if rs := f.rounds(m.id); rs[0].Winner != a.id {
		t.Fatalf("第二次結束改寫了勝者:%+v", rs)
	}
	// 不存在的回合。
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.FinishRound(ctx, tx, match.RoundFinishWrite{MatchID: m.id, RoundNo: 5, WinnerPlayerID: a.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrRoundNotFound) {
		t.Fatalf("err = %v,期望 ErrRoundNotFound", err)
	}

	f.startRound(m)
	f.finishRound(m, 2, a)
	// 2:0 之後再開回合 → ErrMatchDecided。
	if _, err := f.svc.StartRound(ctx, m.publicID, f.judge); !errors.Is(err, match.ErrMatchDecided) {
		t.Fatalf("err = %v,期望 ErrMatchDecided", err)
	}
	if len(f.rounds(m.id)) != 2 {
		t.Fatal("被擋下來卻建了第三回合")
	}
	// best_of = 3 的 ReportResult → ErrMultiRoundMatch(驗收條件 5)。
	m2 := f.match(1, 1, a.id, c.id, match.StatusPending)
	f.runToLive(m2)
	if _, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: m2.publicID, WinnerPlayerPublicID: a.publicID, Confirm: true, ActorUserID: f.judge,
	}); !errors.Is(err, match.ErrMultiRoundMatch) {
		t.Fatalf("多回合制的 ReportResult 應回 ErrMultiRoundMatch,得到 %v", err)
	}
	if st := f.state(m2.publicID); st.Status != match.StatusLive {
		t.Fatalf("被擋下來卻動了場次:%+v", st)
	}
}

// 整場勝者是衍生值:回合列被動過、與要寫入的勝者對不上時,整筆失敗。
func TestFinishRoundRejectsInconsistentRounds(t *testing.T) {
	f := newFixture(t)
	f.setConfig(3, false)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	f.match(2, 0, 0, 0, match.StatusPending)
	f.runToLocked(m)
	f.startRound(m)
	f.finishRound(m, 1, a)
	f.startRound(m)
	// 有人直接在資料庫裡塞了兩列乙勝:乙已達標卻沒定案。
	if _, err := pool.Exec(ctx,
		`UPDATE activity.match_rounds SET winner_player_id = $2 WHERE match_id = $1 AND round_no = 1`, m.id, b.id); err != nil {
		t.Fatalf("動回合列: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO activity.match_rounds (match_id, round_no, started_at, finished_at, winner_player_id)
		VALUES ($1, 3, now(), now(), $2)`, m.id, b.id); err != nil {
		t.Fatalf("塞回合列: %v", err)
	}
	auditsBefore := len(f.audits())

	_, err := f.svc.FinishRound(ctx, match.FinishRoundParams{MatchPublicID: m.publicID, RoundNo: 2, WinnerPublicID: a.publicID, ActorUserID: f.judge, Note: "", Confirm: true})
	if !errors.Is(err, match.ErrRoundsInconsistent) {
		t.Fatalf("err = %v,期望 ErrRoundsInconsistent", err)
	}
	if st := f.state(m.publicID); st.Status != match.StatusLive || st.Winner != 0 {
		t.Fatalf("不一致時不得寫入勝者:%+v", st)
	}
	if rs := f.rounds(m.id); rs[1].FinishedAt != nil {
		t.Fatalf("整筆應 rollback,第 2 回合不該留下結果:%+v", rs)
	}
	if len(f.audits()) != auditsBefore {
		t.Fatal("稽核應一併還原")
	}
}

// 單回合盤口結算失敗 → 回合結果、稽核一起消失。
func TestRollbackOnRoundSettleFailure(t *testing.T) {
	f := newFixture(t)
	f.setConfig(3, false)
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	f.runToLocked(m)
	f.startRound(m)
	auditsBefore := len(f.audits())
	f.bets.failSettleRound = true

	if _, err := f.svc.FinishRound(context.Background(), match.FinishRoundParams{MatchPublicID: m.publicID, RoundNo: 1, WinnerPublicID: a.publicID, ActorUserID: f.judge, Note: "", Confirm: true}); !errors.Is(err, errStub) {
		t.Fatalf("結算失敗應整筆失敗,得到 %v", err)
	}
	if rs := f.rounds(m.id); rs[0].FinishedAt != nil || rs[0].Winner != 0 {
		t.Fatalf("回合結果應還原:%+v", rs)
	}
	if len(f.audits()) != auditsBefore {
		t.Fatal("稽核應一併還原")
	}
	f.bets.failSettleRound = false
	f.finishRound(m, 1, a)
}

// ── 併發 ────────────────────────────────────────────────────────

// 兩位裁判同時按「結束第 1 回合」(甚至填不同勝者):列鎖讓第二位看到已結束的回合,
// 回合只結算一次、勝者只有一個。
func TestConcurrentFinishRound(t *testing.T) {
	setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		f := newFixture(t)
		f.setConfig(3, false)
		a, b := f.player("甲", 1), f.player("乙", 2)
		m := f.match(1, 0, a.id, b.id, match.StatusPending)
		f.runToLocked(m)
		f.startRound(m)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		winners := []player{a, b}
		wg.Add(2)
		for k := range errs {
			go func() {
				defer wg.Done()
				_, errs[k] = f.svc.FinishRound(ctx, match.FinishRoundParams{MatchPublicID: m.publicID, RoundNo: 1, WinnerPublicID: winners[k].publicID, ActorUserID: f.judge, Note: "", Confirm: true})
			}()
		}
		wg.Wait()

		ok, failed := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case isDeadlock(err):
				t.Fatalf("第 %d 輪出現死鎖:%v", i, err)
			case errors.Is(err, match.ErrRoundAlreadyFinished):
				failed++
			default:
				t.Fatalf("第 %d 輪出現非預期錯誤:%v", i, err)
			}
		}
		if ok != 1 || failed != 1 {
			t.Fatalf("第 %d 輪:連點應只成功一次(成功 %d、已結束 %d)", i, ok, failed)
		}
		if n := len(f.bets.settledRounds); n != 1 {
			t.Fatalf("第 %d 輪:單回合結算應恰好一次,得到 %d", i, n)
		}
		if n := count(f.auditActions(), match.ActionFinishRound); n != 1 {
			t.Fatalf("第 %d 輪:稽核應只有一筆結束回合,得到 %d", i, n)
		}
	}
}

// 兩位裁判同時按「正式決鬥開始」:回合只建一列、盤口只關一次。
func TestConcurrentStartRound(t *testing.T) {
	setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		f := newFixture(t)
		f.setConfig(3, false)
		a, b := f.player("甲", 1), f.player("乙", 2)
		m := f.match(1, 0, a.id, b.id, match.StatusPending)
		f.runToLocked(m)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		for k := range errs {
			go func() {
				defer wg.Done()
				_, errs[k] = f.svc.StartRound(ctx, m.publicID, f.judge)
			}()
		}
		wg.Wait()

		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case isDeadlock(err):
				t.Fatalf("第 %d 輪出現死鎖:%v", i, err)
			case errors.Is(err, match.ErrRoundInProgress):
			default:
				t.Fatalf("第 %d 輪出現非預期錯誤:%v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("第 %d 輪:連點應只成功一次,得到 %d", i, ok)
		}
		if rs := f.rounds(m.id); len(rs) != 1 {
			t.Fatalf("第 %d 輪:回合列應恰好一列,得到 %d", i, len(rs))
		}
		if n := count(f.auditActions(), match.ActionStartMatch); n != 1 {
			t.Fatalf("第 %d 輪:開打稽核應恰好一筆,得到 %d", i, n)
		}
	}
}

// ── 季軍戰(驗收條件 6)──────────────────────────────────────────

// eightBracket 建 8 人樹的後半:兩場準決賽(R2)與決賽(R3),首輪不建。
func eightBracket(t *testing.T, thirdPlace bool) (*fixture, [4]player, [3]mrec) {
	t.Helper()
	f := newFixture(t)
	f.setConfig(1, thirdPlace)
	ps := [4]player{f.player("甲", 1), f.player("乙", 1), f.player("丙", 1), f.player("丁", 1)}
	// 先建決賽讓 total_rounds = 3。
	final := f.match(3, 0, 0, 0, match.StatusPending)
	s0 := f.match(2, 0, ps[0].id, ps[1].id, match.StatusPending)
	s1 := f.match(2, 1, ps[2].id, ps[3].id, match.StatusPending)
	return f, ps, [3]mrec{s0, s1, final}
}

func (f *fixture) report(m mrec, winner player) *match.ReportResultOutcome {
	f.t.Helper()
	f.runToLive(m)
	out, err := f.svc.ReportResult(context.Background(), match.ReportResultParams{
		MatchPublicID: m.publicID, WinnerPlayerPublicID: winner.publicID, Confirm: true, ActorUserID: f.judge,
	})
	if err != nil {
		f.t.Fatalf("判定 %s: %v", m.publicID, err)
	}
	return out
}

func TestThirdPlaceMatch(t *testing.T) {
	f, ps, ms := eightBracket(t, true)
	ctx := context.Background()

	out := f.report(ms[0], ps[0]) // 乙敗
	if out.ThirdPlaceMatch != nil || f.thirdPlace() != nil {
		t.Fatal("只判完一場準決賽不該建季軍戰")
	}
	out = f.report(ms[1], ps[3]) // 丙敗
	third := f.thirdPlace()
	switch {
	case out.ThirdPlaceMatch == nil || third == nil:
		t.Fatal("兩場準決賽都判完應建季軍戰")
	case third.Kind != match.KindThirdPlace || third.Status != match.StatusPending:
		t.Fatalf("季軍戰:%+v", third)
	case third.P1 != ps[1].id || third.P2 != ps[2].id:
		t.Fatalf("季軍戰雙方應是兩位敗者(slot 0 的敗者在 P1):p1=%d p2=%d", third.P1, third.P2)
	}
	var round, slot int
	if err := pool.QueryRow(ctx,
		`SELECT round, slot FROM activity.matches WHERE public_id = $1`, out.ThirdPlaceMatch.PublicID).Scan(&round, &slot); err != nil {
		t.Fatalf("讀季軍戰位置: %v", err)
	}
	if round != 3 || slot != 1 {
		t.Fatalf("季軍戰應在決賽那一輪 slot 1,得到 r%ds%d", round, slot)
	}

	// 決賽判完不會再建一場;直接再 INSERT 同位置撞 UNIQUE。
	f.report(ms[2], ps[0])
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM activity.matches WHERE tournament_id = $1 AND kind = 'third_place'`, f.id).Scan(&n); err != nil {
		t.Fatalf("數季軍戰: %v", err)
	}
	if n != 1 {
		t.Fatalf("季軍戰應只建一次,得到 %d", n)
	}
	err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.InsertThirdPlaceMatch(ctx, tx, match.ThirdPlaceWrite{
			TournamentID: f.id, Round: 3, Slot: 1, P1PlayerID: ps[1].id, P2PlayerID: ps[2].id,
		})
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("同位置再建一場應撞 UNIQUE,得到 %v", err)
	}

	// 季軍戰走與其他場次完全相同的流程:開盤、封盤、確認、開打、判定;勝者不晉級、沒有冠軍。
	tm := mrec{publicID: out.ThirdPlaceMatch.PublicID, id: out.ThirdPlaceMatch.ID}
	champions := count(f.topics(), match.TopicChampion)
	out = f.report(tm, ps[2])
	st := f.state(tm.publicID)
	switch {
	case out.ChampionPlayerPublicID != "" || len(out.AdvancedMatches) != 0:
		t.Fatalf("季軍戰勝者不晉級、不是冠軍:%+v", out)
	case st.Status != match.StatusDone || st.Winner != ps[2].id:
		t.Fatalf("季軍戰結果:%+v", st)
	case count(f.topics(), match.TopicChampion) != champions:
		t.Fatal("季軍戰不該發冠軍公告")
	}
	// AwardPrizes 日後從這裡取季軍。
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		got, err := f.repo.FindThirdPlaceMatch(ctx, tx, f.id)
		if err != nil {
			return err
		}
		if got == nil || got.WinnerPlayerID != ps[2].id || got.Kind != match.KindThirdPlace {
			t.Fatalf("FindThirdPlaceMatch = %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("讀季軍戰: %v", err)
	}
}

func TestThirdPlaceMatchDisabled(t *testing.T) {
	f, ps, ms := eightBracket(t, false)
	f.report(ms[0], ps[0])
	if out := f.report(ms[1], ps[3]); out.ThirdPlaceMatch != nil || f.thirdPlace() != nil {
		t.Fatal("本屆不打季軍戰卻建了")
	}
	if err := f.repo.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := f.repo.FindThirdPlaceMatch(ctx, tx, f.id)
		if err != nil || got != nil {
			t.Fatalf("FindThirdPlaceMatch = %+v, %v(應 nil, nil)", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ── 違規(驗收條件 7)────────────────────────────────────────────

func TestRecordViolation(t *testing.T) {
	f := newFixture(t)
	f.setConfig(3, false)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	c := f.player("丙", 3)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	itemID := f.item("skill.ban_two", "禁用兩招")
	f.runToLocked(m)
	f.startRound(m)
	f.finishRound(m, 1, a)
	f.startRound(m)
	stBefore := f.state(m.publicID)
	roundsBefore := f.rounds(m.id)
	auditsBefore := len(f.audits())
	topicsBefore := len(f.topics())
	round := 2

	// note 空:service 擋;直接對 DB 寫也被 CHECK 擋。
	if _, err := f.svc.RecordViolation(ctx, match.RecordViolationParams{
		MatchPublicID: m.publicID, PlayerPublicID: b.publicID, Ruling: match.RulingWarning, Note: " ", ActorUserID: f.judge,
	}); !errors.Is(err, match.ErrInvalidRequest) {
		t.Fatalf("note 空應被拒,得到 %v", err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO activity.match_violations (public_id, match_id, player_id, ruling, note, recorded_by)
		VALUES (gen_random_uuid()::text, $1, $2, 'warning', '  ', $3)`, m.id, b.id, f.judge)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("note 空直接寫 DB 應被 CHECK 擋,得到 %v", err)
	}
	// player 不在場上。
	if _, err := f.svc.RecordViolation(ctx, match.RecordViolationParams{
		MatchPublicID: m.publicID, PlayerPublicID: c.publicID, Ruling: match.RulingWarning, Note: "x", ActorUserID: f.judge,
	}); !errors.Is(err, match.ErrPlayerNotInMatch) {
		t.Fatalf("不在場上應被拒,得到 %v", err)
	}
	// 項目要在本屆內找得到。
	if _, err := f.svc.RecordViolation(ctx, match.RecordViolationParams{
		MatchPublicID: m.publicID, PlayerPublicID: b.publicID, ItemPublicID: "01NOSUCHITEM",
		Ruling: match.RulingWarning, Note: "x", ActorUserID: f.judge,
	}); !errors.Is(err, handicap.ErrItemNotFound) {
		t.Fatalf("查無項目應回 handicap.ErrItemNotFound,得到 %v", err)
	}
	if f.violationCount(m.id) != 0 || len(f.audits()) != auditsBefore {
		t.Fatal("被擋下的紀錄不該留下任何東西")
	}

	v, err := f.svc.RecordViolation(ctx, match.RecordViolationParams{
		MatchPublicID: m.publicID, RoundNo: &round, PlayerPublicID: b.publicID, ItemPublicID: itemID,
		Ruling: match.RulingRoundLoss, Note: "第二回合用了被禁的松濤劍法,判該回合負", ActorUserID: f.judge,
	})
	if err != nil {
		t.Fatalf("記錄違規: %v", err)
	}
	switch {
	case v.PublicID == "" || v.MatchPublicID != m.publicID:
		t.Fatalf("紀錄 = %+v", v)
	case v.PlayerPublicID != b.publicID || v.PlayerDisplayName != "乙":
		t.Fatalf("紀錄 = %+v", v)
	case v.ItemPublicID != itemID || v.ItemKey != "skill.ban_two" || v.ItemName != "禁用兩招":
		t.Fatalf("紀錄 = %+v", v)
	case v.RoundNo == nil || *v.RoundNo != 2 || v.Ruling != match.RulingRoundLoss || v.RecordedBy != f.judge:
		t.Fatalf("紀錄 = %+v", v)
	case f.violationCount(m.id) != 1:
		t.Fatal("紀錄沒寫進去")
	}
	audits := f.audits()
	if len(audits) != auditsBefore+1 || audits[len(audits)-1].Action != match.ActionRecordViolation {
		t.Fatalf("稽核應恰好多一筆 record_violation:%v", f.auditActions())
	}
	if last := audits[len(audits)-1]; last.After["violation_public_id"] != v.PublicID || last.After["item_key"] != "skill.ban_two" {
		t.Fatalf("稽核 after 不足以還原這筆紀錄:%+v", last.After)
	}

	// 不觸發任何後果:場次、回合、outbox 全部不變。
	if st := f.state(m.publicID); !reflect.DeepEqual(st, stBefore) {
		t.Fatalf("違規紀錄改了場次:%+v → %+v", stBefore, st)
	}
	if rs := f.rounds(m.id); len(rs) != len(roundsBefore) || rs[1].FinishedAt != nil || rs[0].Winner != roundsBefore[0].Winner {
		t.Fatalf("違規紀錄改了回合:%+v → %+v", roundsBefore, rs)
	}
	if len(f.topics()) != topicsBefore {
		t.Fatalf("違規不單發公告:%v", f.topics())
	}

	list, err := f.svc.ListMatchViolations(ctx, m.publicID)
	if err != nil || len(list) != 1 || list[0].PublicID != v.PublicID || list[0].ItemName != "禁用兩招" {
		t.Fatalf("列表 = %+v, err = %v", list, err)
	}

	// 裁判依判決把第 2 回合填給對方 → 2:0 定案,賽果公告帶上原因。
	f.finishRound(m, 2, a)
	for _, e := range f.events() {
		if e.Topic != match.TopicMatchFinished {
			continue
		}
		res := e.Payload["result"].(map[string]any)
		vs, ok := res["violations"].([]any)
		if !ok || len(vs) != 1 {
			t.Fatalf("賽果公告應帶判負原因:%+v", res)
		}
		if first := vs[0].(map[string]any); first["ruling"] != "round_loss" || first["display_name"] != "乙" {
			t.Fatalf("違規原因不完整:%+v", first)
		}
	}
}

// ── 讀取側:對戰表的比分 ─────────────────────────────────────────

func TestRoundsByMatches(t *testing.T) {
	f := newFixture(t)
	f.setConfig(3, false)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	empty := f.match(1, 1, a.id, b.id, match.StatusPending)
	f.runToLocked(m)
	f.startRound(m)
	f.finishRound(m, 1, b)
	f.startRound(m)

	reader := activityreadpg.New(pool)
	got, err := reader.RoundsByMatches(ctx, []int64{m.id, empty.id, -1})
	if err != nil {
		t.Fatalf("RoundsByMatches: %v", err)
	}
	if _, ok := got[empty.id]; ok {
		t.Fatal("沒有回合的場次不該出現在 map 裡")
	}
	rs := got[m.id]
	if len(rs) != 2 || rs[0].RoundNo != 1 || rs[1].RoundNo != 2 || rs[0].WinnerPlayerID != b.id || rs[1].Finished() {
		t.Fatalf("回合 = %+v", rs)
	}
	// WinnerPublicID 由呼叫端用場次補;比數只看 id,不需要先補。
	mm, err := reader.MatchByPublicID(ctx, m.publicID)
	if err != nil {
		t.Fatalf("MatchByPublicID: %v", err)
	}
	if mm.Kind != match.KindBracket || mm.SetupConfirmedAt == nil || mm.SetupConfirmedBy != f.judge {
		t.Fatalf("讀取側要帶新欄位:%+v", mm)
	}
	mm.ResolveRoundWinners(rs)
	if rs[0].WinnerPublicID != b.publicID || rs[1].WinnerPublicID != "" {
		t.Fatalf("補 public_id 後:%+v", rs)
	}
	if sc := match.ScoreOf(mm, rs); sc.String() != "0:1" {
		t.Fatalf("比數 = %s", sc)
	}
	if empty, err := reader.RoundsByMatches(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("空輸入應回空 map:%v %v", empty, err)
	}
}
