package matchpg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/matchpg"
)

// ── 狀態轉移的 DB 側守門 ────────────────────────────────────────

// TestStateGates 驗證五支狀態轉移的 WHERE 條件真的在資料庫側擋住了。
//
// 這是本 adapter 最重要的一組斷言:service 已經在列鎖下檢查過一次狀態,
// SQL 裡那個條件是**防併發**的第二道。它一旦被改掉(或被「修正」成無條件 UPDATE),
// 單元測試不會有任何反應 —— 只有對真資料庫跑才看得出來。
func TestStateGates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)

	// pending → ready
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		got, err := f.repo.MarkReady(ctx, tx, match.ReadyWrite{
			MatchID: m.id, ActorUserID: f.judge, Reason: "開盤",
		})
		if err != nil {
			return err
		}
		if got.Status != match.StatusReady || !got.HandicapOpen {
			t.Fatalf("開盤後 status=%s handicap_open=%v", got.Status, got.HandicapOpen)
		}
		// 回傳的列必須帶著雙方快照(晉級級聯直接吃這個形狀)
		if got.P1.PublicID != a.publicID || got.P2.DisplayName != "乙" {
			t.Fatalf("回傳的雙方快照不對:%+v / %+v", got.P1, got.P2)
		}
		return nil
	}); err != nil {
		t.Fatalf("開盤: %v", err)
	}

	// 重複開盤 → ErrAlreadyOpen(而不是默默成功再公告一次)
	err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		_, err := f.repo.MarkReady(ctx, tx, match.ReadyWrite{MatchID: m.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrAlreadyOpen) {
		t.Fatalf("重複開盤應回 ErrAlreadyOpen,得到 %v", err)
	}

	// 沒封盤就開打 → ErrNotLocked
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		_, err := f.repo.MarkLive(ctx, tx, match.LiveWrite{MatchID: m.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrNotLocked) {
		t.Fatalf("未封盤開打應回 ErrNotLocked,得到 %v", err)
	}

	// 封盤(走 handicap 的那一支 UPDATE)後才進得了 live
	if _, err := f.svc.LockHandicap(ctx, match.LockHandicapParams{
		MatchPublicID: m.publicID, Confirm: true, ActorUserID: f.judge, Reason: "封盤",
	}); err != nil {
		t.Fatalf("封盤: %v", err)
	}
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		live, err := f.repo.MarkLive(ctx, tx, match.LiveWrite{MatchID: m.id, ActorUserID: f.judge})
		if err != nil {
			return err
		}
		if live.Status != match.StatusLive || live.StartedAt == nil {
			t.Fatalf("開打後 status=%s started_at=%v", live.Status, live.StartedAt)
		}
		return nil
	}); err != nil {
		t.Fatalf("開打: %v", err)
	}

	// 判定勝負,再判一次 → ErrMatchFinished
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		fin, err := f.repo.MarkFinished(ctx, tx, match.FinishWrite{
			MatchID: m.id, WinnerPlayerID: a.id, ResultKind: match.ResultNormal,
			ActorUserID: f.judge, Reason: "判勝",
		})
		if err != nil {
			return err
		}
		if fin.Status != match.StatusDone || fin.WinnerPlayerID != a.id || fin.FinishedAt == nil {
			t.Fatalf("判定後 %+v", fin)
		}
		return nil
	}); err != nil {
		t.Fatalf("判定勝負: %v", err)
	}
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		_, err := f.repo.MarkFinished(ctx, tx, match.FinishWrite{
			MatchID: m.id, WinnerPlayerID: b.id, ResultKind: match.ResultNormal, ActorUserID: f.judge,
		})
		return err
	})
	if !errors.Is(err, match.ErrMatchFinished) {
		t.Fatalf("重複判定應回 ErrMatchFinished,得到 %v", err)
	}

	// 直播連結是唯一沒有狀態閘門的一支:done 也能設,空字串清成 NULL
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		if _, err := f.repo.SetStreamURL(ctx, tx, match.StreamWrite{
			MatchID: m.id, StreamURL: "https://vod.example/1", ActorUserID: f.judge, Reason: "補 VOD",
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("設直播連結: %v", err)
	}
	if got := f.state(m.publicID); got.StreamURL == nil || *got.StreamURL != "https://vod.example/1" {
		t.Fatalf("直播連結沒寫進去:%v", got.StreamURL)
	}
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, m.publicID); err != nil {
			return err
		}
		_, err := f.repo.SetStreamURL(ctx, tx, match.StreamWrite{
			MatchID: m.id, StreamURL: "", ActorUserID: f.judge, Reason: "清掉",
		})
		return err
	}); err != nil {
		t.Fatalf("清直播連結: %v", err)
	}
	if got := f.state(m.publicID); got.StreamURL != nil {
		// 「沒有連結」只能有一種表示法,否則每個讀取端都要各判一次
		t.Fatalf("空字串應存成 NULL,得到 %q", *got.StreamURL)
	}
}

// TestNotFound 驗證查無資料時回的是可辨識的 sentinel,而不是 pgx.ErrNoRows。
func TestNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.LockMatch(ctx, tx, "不存在的場次")
		return err
	})
	if !errors.Is(err, activityerr.ErrMatchNotFound) {
		t.Fatalf("LockMatch 查無應回 ErrMatchNotFound,得到 %v", err)
	}

	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.LockPlayer(ctx, tx, "不存在的選手")
		return err
	})
	if !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Fatalf("LockPlayer 查無應回 ErrPlayerNotFound,得到 %v", err)
	}

	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.TournamentByID(ctx, tx, -1)
		return err
	})
	if !errors.Is(err, tournament.ErrTournamentNotFound) {
		t.Fatalf("TournamentByID 查無應回 ErrTournamentNotFound,得到 %v", err)
	}

	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.LockMatchAt(ctx, tx, f.id, 9, 9)
		return err
	})
	if !errors.Is(err, activityerr.ErrMatchNotFound) {
		t.Fatalf("LockMatchAt 查無應回 ErrMatchNotFound,得到 %v", err)
	}
}

// TestZeroRowAttribution 驗證「狀態轉移影響 0 列」的兩個成因分得出來。
//
// 兩者對裁判的意義完全不同:「網址打錯了」vs「有人剛剛已經按過了」。歸因搞錯的
// 症狀是裁判照著錯誤訊息去查一個根本不存在的問題,而資料其實好好的。
//
// 刻意**不先 LockMatch**:歸因走 GetMatchForJudgeByID 以內部 id 重讀,
// 不依賴「本 tx 先前讀過這一列」——真有哪天呼叫端少鎖一次,錯誤訊息不該跟著變。
func TestZeroRowAttribution(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)

	// id 序列只給正數,-1 保證查無此場次。
	const missing int64 = -1

	cases := []struct {
		name string
		want error
		run  func(ctx context.Context, tx pgx.Tx) error
	}{
		{"開盤", activityerr.ErrMatchNotFound, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.MarkReady(ctx, tx, match.ReadyWrite{MatchID: missing, ActorUserID: f.judge})
			return err
		}},
		{"開打", activityerr.ErrMatchNotFound, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.MarkLive(ctx, tx, match.LiveWrite{MatchID: missing, ActorUserID: f.judge})
			return err
		}},
		{"判定勝負", activityerr.ErrMatchNotFound, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.MarkFinished(ctx, tx, match.FinishWrite{
				MatchID: missing, WinnerPlayerID: a.id,
				ResultKind: match.ResultNormal, ActorUserID: f.judge,
			})
			return err
		}},
		// 晉級目標整場不見了 = 對戰表被刪了一半,service 要的是另一個 sentinel。
		{"晉級", match.ErrAdvanceTargetMissing, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.SeatPlayer(ctx, tx, match.SeatWrite{
				MatchID: missing, PlayerID: b.id, IsP1: true,
			})
			return err
		}},
	}
	for _, c := range cases {
		err := f.repo.InTx(ctx, c.run)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s 對不存在的場次應回 %v,得到 %v", c.name, c.want, err)
		}
	}

	// 場次在、狀態不符 → 閘門錯誤(不是「查無此場次」)。
	err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.MarkLive(ctx, tx, match.LiveWrite{MatchID: m.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrNotLocked) {
		t.Fatalf("未封盤的場次開打應回 ErrNotLocked,得到 %v", err)
	}
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.MarkReady(ctx, tx, match.ReadyWrite{MatchID: m.id, ActorUserID: f.judge})
		return err
	}); err != nil {
		t.Fatalf("開盤: %v", err)
	}
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.MarkReady(ctx, tx, match.ReadyWrite{MatchID: m.id, ActorUserID: f.judge})
		return err
	})
	if !errors.Is(err, match.ErrAlreadyOpen) {
		t.Fatalf("重複開盤應回 ErrAlreadyOpen,得到 %v", err)
	}
	// 錯誤訊息要帶 public_id:對外(含給裁判看的訊息)永不出現內部 id。
	if !strings.Contains(err.Error(), m.publicID) {
		t.Fatalf("閘門錯誤應帶 public_id %s,得到 %q", m.publicID, err.Error())
	}
	// 已就座的一側被覆寫 → 這是資料完整性失效,不是「查無此場次」。
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.SeatPlayer(ctx, tx, match.SeatWrite{
			MatchID: m.id, PlayerID: b.id, IsP1: true,
		})
		return err
	})
	if !errors.Is(err, matchpg.ErrSeatUnavailable) {
		t.Fatalf("覆寫已就座的一側應回 ErrSeatUnavailable,得到 %v", err)
	}
}

// TestSeatGuards 驗證晉級寫入的兩道守門:目標必須還在 pending、該側必須是空的。
//
// 撞上任一道都是資料完整性失效(對戰表被動過),必須整筆失敗 ——
// 默默跳過的後果是勝者從對戰表上消失,而且沒有人會發現。
func TestSeatGuards(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 1)
	next := f.match(2, 0, a.id, 0, match.StatusPending)

	err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, next.publicID); err != nil {
			return err
		}
		_, err := f.repo.SeatPlayer(ctx, tx, match.SeatWrite{
			MatchID: next.id, PlayerID: b.id, IsP1: true, // P1 已經坐了甲
		})
		return err
	})
	if !errors.Is(err, matchpg.ErrSeatUnavailable) {
		t.Fatalf("覆寫已就座的一側應失敗,得到 %v", err)
	}

	// 空的那一側寫得進去
	if err := f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, next.publicID); err != nil {
			return err
		}
		seated, err := f.repo.SeatPlayer(ctx, tx, match.SeatWrite{
			MatchID: next.id, PlayerID: b.id, IsP1: false,
		})
		if err != nil {
			return err
		}
		if seated.P2.ID != b.id {
			t.Fatalf("晉級寫錯側:%+v", seated)
		}
		return nil
	}); err != nil {
		t.Fatalf("晉級寫入: %v", err)
	}

	// 已經不是 pending 的場次不能再塞人
	f.runToLive(next)
	err = f.repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.repo.LockMatch(ctx, tx, next.publicID); err != nil {
			return err
		}
		_, err := f.repo.SeatPlayer(ctx, tx, match.SeatWrite{MatchID: next.id, PlayerID: b.id, IsP1: true})
		return err
	})
	if !errors.Is(err, matchpg.ErrSeatUnavailable) {
		t.Fatalf("非 pending 場次應拒絕晉級寫入,得到 %v", err)
	}
}

// ── 晉級鏈 ──────────────────────────────────────────────────────

// TestAdvanceChain 驗證判完一場之後勝者真的出現在下一場的**正確位置**。
//
// 位置要驗對:slot 偶數進 P1、奇數進 P2。晉級到錯的一側是人工看對戰表
// 看不出來的 bug —— 對戰表照樣畫得出來,只是下一輪的對手配錯了。
func TestAdvanceChain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	c, d := f.player("丙", 3), f.player("丁", 4)

	m0 := f.match(1, 0, a.id, b.id, match.StatusPending)
	m1 := f.match(1, 1, c.id, d.id, match.StatusPending)
	final := f.match(2, 0, 0, 0, match.StatusPending)

	f.runToLive(m0)
	out, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: m0.publicID, WinnerPlayerPublicID: a.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "3:1",
	})
	if err != nil {
		t.Fatalf("判定 r1s0: %v", err)
	}
	if out.ChampionPlayerPublicID != "" {
		t.Fatalf("第一輪不該產生冠軍:%s", out.ChampionPlayerPublicID)
	}
	if st := f.state(final.publicID); st.P1 != a.id || st.P2 != 0 {
		t.Fatalf("slot 0 的勝者應坐 P1:%+v", st)
	}
	if f.playerStatus(b.publicID) != string(tournament.PlayerEliminated) {
		t.Fatalf("敗方應標 eliminated")
	}

	f.runToLive(m1)
	if _, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: m1.publicID, WinnerPlayerPublicID: c.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "3:0",
	}); err != nil {
		t.Fatalf("判定 r1s1: %v", err)
	}
	if st := f.state(final.publicID); st.P1 != a.id || st.P2 != c.id {
		t.Fatalf("slot 1 的勝者應坐 P2:%+v", st)
	}

	// 決賽:沒有下一場,勝者即冠軍
	f.runToLive(final)
	out, err = f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: final.publicID, WinnerPlayerPublicID: c.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "決賽",
	})
	if err != nil {
		t.Fatalf("判定決賽: %v", err)
	}
	if out.ChampionPlayerPublicID != c.publicID {
		t.Fatalf("冠軍應為丙,得到 %q", out.ChampionPlayerPublicID)
	}

	// 戰績:勝者每勝一場 +1,敗者 +1(口徑與 RecalcFencerStats 一致)
	if w, l := f.record(c.fencerID); w != 2 || l != 0 {
		t.Fatalf("丙的戰績應為 2 勝 0 敗,得到 %d/%d", w, l)
	}
	if w, l := f.record(a.fencerID); w != 1 || l != 1 {
		t.Fatalf("甲的戰績應為 1 勝 1 敗,得到 %d/%d", w, l)
	}
	if w, l := f.record(d.fencerID); w != 0 || l != 1 {
		t.Fatalf("丁的戰績應為 0 勝 1 敗,得到 %d/%d", w, l)
	}

	if !contains(f.topics(), match.TopicChampion) {
		t.Fatalf("缺冠軍公告:%v", f.topics())
	}
}

// ── 稽核與事件 ──────────────────────────────────────────────────

// TestAuditAndOutbox 驗證「該寫的寫了、不該寫的沒寫」。
//
// 稽核漏一筆不會有任何症狀,直到有人來問「這場是誰判的」。
func TestAuditAndOutbox(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	f.match(2, 0, 0, 0, match.StatusPending)

	f.runToLive(m)
	if _, err := f.svc.SetStreamURL(ctx, match.SetStreamURLParams{
		MatchPublicID: m.publicID, StreamURL: "https://live.example/1",
		ActorUserID: f.judge, Reason: "直播",
	}); err != nil {
		t.Fatalf("設直播: %v", err)
	}
	if _, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: m.publicID, WinnerPlayerPublicID: a.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "3:2",
	}); err != nil {
		t.Fatalf("判定: %v", err)
	}

	want := []string{
		match.ActionOpenHandicap, match.ActionLockHandicap, match.ActionStartMatch,
		match.ActionSetStreamURL, match.ActionReportResult,
	}
	got := f.auditActions()
	if len(got) != len(want) {
		t.Fatalf("稽核筆數不對:want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("稽核順序不對:want %v, got %v", want, got)
		}
	}
	// 晉級(SeatPlayer)與戰績(BumpFencerRecord)是判定勝負的後果,不另記一筆
	if n := count(got, match.ActionReportResult); n != 1 {
		t.Fatalf("判定勝負應只留一筆稽核,得到 %d", n)
	}

	audits := f.audits()
	last := audits[len(audits)-1]
	if last.TargetType != "activity.match" || last.TargetID != m.id {
		t.Fatalf("稽核的 target 不對:%+v", last)
	}
	if last.After["winner_player_public_id"] != a.publicID {
		t.Fatalf("稽核 after 應記下勝者的 public_id:%+v", last.After)
	}
	if last.Reason != "3:2" {
		t.Fatalf("稽核理由沒寫進去:%q", last.Reason)
	}

	// 四則公告 + payload 自己就夠渲染
	topics := f.topics()
	for _, want := range []string{
		match.TopicHandicapOpened, match.TopicHandicapLocked,
		match.TopicMatchStarted, match.TopicMatchFinished,
	} {
		if !contains(topics, want) {
			t.Fatalf("缺公告 %s:%v", want, topics)
		}
	}
	// 設直播連結刻意不發公告(連結會改、會補,每改一次就廣播等於洗頻)
	if len(topics) != 4 {
		t.Fatalf("公告數不對(設直播不該發):%v", topics)
	}

	for _, e := range f.events() {
		if e.Payload["match_public_id"] != m.publicID {
			t.Fatalf("%s 的 payload 缺 match_public_id:%+v", e.Topic, e.Payload)
		}
		if e.Payload["round_label"] != "決賽" && e.Payload["round_label"] != "四強" {
			t.Fatalf("%s 的 round_label 不對:%v", e.Topic, e.Payload["round_label"])
		}
		p1, ok := e.Payload["p1"].(map[string]any)
		if !ok || p1["display_name"] != "甲" {
			t.Fatalf("%s 的 payload 缺雙方快照:%+v", e.Topic, e.Payload)
		}
		if e.Topic != match.TopicMatchFinished {
			continue
		}
		res, ok := e.Payload["result"].(map[string]any)
		if !ok {
			t.Fatalf("賽果公告缺 result:%+v", e.Payload)
		}
		winner, ok := res["winner"].(map[string]any)
		if !ok || winner["player_public_id"] != a.publicID {
			t.Fatalf("賽果公告的勝者不對:%+v", res)
		}
		if res["next_match_public_id"] == nil {
			t.Fatalf("賽果公告應帶晉級目標:%+v", res)
		}
	}
}

// ── 棄賽 ────────────────────────────────────────────────────────

// TestWithdrawWalkover 驗證棄賽的直接後果:對手不戰而勝、照常晉級、注單全額退款。
func TestWithdrawWalkover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	f.player("丙", 3)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	final := f.match(2, 0, 0, 0, match.StatusPending)
	betID := f.bet(m.id)

	res, err := f.svc.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
		PlayerPublicID: a.publicID, Confirm: true, ActorUserID: f.judge, Reason: "臨時有事",
	})
	if err != nil {
		t.Fatalf("棄賽: %v", err)
	}
	if len(res.WalkoverMatches) != 1 || res.VoidedBetCount != 1 {
		t.Fatalf("應判一場不戰而勝、退一張注單:%+v", res)
	}
	st := f.state(m.publicID)
	if st.Status != match.StatusDone || st.ResultKind != match.ResultWalkover || st.Winner != b.id {
		t.Fatalf("不戰而勝的場次狀態不對:%+v", st)
	}
	if got := f.state(final.publicID); got.P1 != b.id {
		t.Fatalf("對手應照常晉級:%+v", got)
	}
	if f.playerStatus(a.publicID) != string(tournament.PlayerWithdrawn) {
		t.Fatalf("棄賽者的狀態應維持 withdrawn(不能被 eliminated 蓋掉)")
	}
	if got := f.betStatus(betID); got != "void" {
		// 賠率是按「真的打一場」算的,不戰而勝不該讓任何人贏錢
		t.Fatalf("注單應全額退款,得到 %q", got)
	}
	if w, l := f.record(a.fencerID); w != 0 || l != 1 {
		t.Fatalf("棄賽者也照算一敗(戰績要誠實),得到 %d/%d", w, l)
	}

	acts := f.auditActions()
	if !contains(acts, match.ActionWithdraw) || !contains(acts, match.ActionReportResult) {
		t.Fatalf("棄賽應留下 player.withdraw 與 match.report_result:%v", acts)
	}
	// 敗方標 eliminated 不另記稽核(那是判勝負的後果),所以只有這兩筆
	if len(acts) != 2 {
		t.Fatalf("稽核筆數不對:%v", acts)
	}

	// 重複棄賽要出聲:第一次已經判了不戰而勝也退了款,第二次沒有事情可做
	if _, err := f.svc.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
		PlayerPublicID: a.publicID, Confirm: true, ActorUserID: f.judge, Reason: "再來一次",
	}); !errors.Is(err, match.ErrAlreadyWithdrawn) {
		t.Fatalf("重複棄賽應回 ErrAlreadyWithdrawn,得到 %v", err)
	}
}

// TestWithdrawPendingOpponentCascade 是本套件補的那個洞:
// 棄賽者坐在一個**對手尚未確定**的位置上(輪空直接進第二輪)。
//
// 那一場在棄賽當下判不了(沒有勝者可寫),如果晉級鏈不接手,它就會永遠停在
// pending —— 裁判沒有任何動作推得動它,因為棄賽者已經棄賽過了。
func TestWithdrawPendingOpponentCascade(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", 1) // 輪空,直接坐進決賽的 P2
	b, c := f.player("乙", 2), f.player("丙", 3)

	semi := f.match(1, 0, b.id, c.id, match.StatusPending)
	final := f.match(2, 0, 0, a.id, match.StatusPending)

	res, err := f.svc.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
		PlayerPublicID: a.publicID, Confirm: true, ActorUserID: f.judge, Reason: "退賽",
	})
	if err != nil {
		t.Fatalf("棄賽: %v", err)
	}
	if len(res.WalkoverMatches) != 0 {
		t.Fatalf("對手未定的場次現在判不了:%+v", res.WalkoverMatches)
	}
	if st := f.state(final.publicID); st.Status != match.StatusPending {
		t.Fatalf("決賽此刻應仍是 pending:%+v", st)
	}

	// 另一半產生時,晉級鏈要立刻把它判掉
	f.runToLive(semi)
	out, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: semi.publicID, WinnerPlayerPublicID: b.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "3:0",
	})
	if err != nil {
		t.Fatalf("判定準決賽: %v", err)
	}
	st := f.state(final.publicID)
	if st.Status != match.StatusDone || st.ResultKind != match.ResultWalkover || st.Winner != b.id {
		t.Fatalf("決賽應被級聯判成不戰而勝:%+v", st)
	}
	if out.ChampionPlayerPublicID != b.publicID {
		t.Fatalf("乙應直接奪冠,得到 %q", out.ChampionPlayerPublicID)
	}
	if n := count(f.topics(), match.TopicMatchFinished); n != 2 {
		t.Fatalf("應有兩則賽果公告(準決賽 + 級聯的決賽):%v", f.topics())
	}
	if w, l := f.record(b.fencerID); w != 2 || l != 0 {
		t.Fatalf("乙應記 2 勝(含不戰而勝),得到 %d/%d", w, l)
	}
}

// TestWithdrawChainedWalkovers 驗證一次棄賽沿著晉級鏈連判兩場。
//
// 情境:甲在準決賽棄賽 → 乙不戰而勝 → 乙晉級決賽,而決賽的另一邊(丁)
// 更早就已經棄賽 → 決賽當場再判一次不戰而勝,乙奪冠。
func TestWithdrawChainedWalkovers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.player("甲", 1)
	b, c := f.player("乙", 2), f.player("丙", 3)
	d := f.player("丁", 4)

	f.doneMatch(1, 0, b.id, c.id, b.id)                    // 乙已經打進準決賽
	semi := f.match(2, 0, b.id, a.id, match.StatusPending) // 乙 vs 甲
	final := f.match(3, 0, 0, d.id, match.StatusPending)   // 丁兩輪輪空坐在 P2
	betID := f.bet(semi.id)

	// 丁先棄賽(另一位裁判、更早的時間點)
	if _, err := f.svc.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
		PlayerPublicID: d.publicID, Confirm: true, ActorUserID: f.judge, Reason: "丁退賽",
	}); err != nil {
		t.Fatalf("丁棄賽: %v", err)
	}

	res, err := f.svc.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
		PlayerPublicID: a.publicID, Confirm: true, ActorUserID: f.judge, Reason: "甲退賽",
	})
	if err != nil {
		t.Fatalf("甲棄賽: %v", err)
	}
	if len(res.WalkoverMatches) != 2 {
		t.Fatalf("應連判兩場不戰而勝,得到 %d 場", len(res.WalkoverMatches))
	}
	if res.ChampionPlayerPublicID != b.publicID {
		t.Fatalf("乙應不戰而勝奪冠,得到 %q", res.ChampionPlayerPublicID)
	}
	for _, m := range []mrec{semi, final} {
		st := f.state(m.publicID)
		if st.Status != match.StatusDone || st.ResultKind != match.ResultWalkover || st.Winner != b.id {
			t.Fatalf("r%ds%d 應是乙的不戰而勝:%+v", m.round, m.slot, st)
		}
	}
	if got := f.betStatus(betID); got != "void" {
		t.Fatalf("準決賽的注單應退款,得到 %q", got)
	}
	if w, _ := f.record(b.fencerID); w != 2 {
		t.Fatalf("乙應記 2 勝,得到 %d", w)
	}
	if !contains(f.topics(), match.TopicChampion) {
		t.Fatalf("缺冠軍公告:%v", f.topics())
	}
}

// ── 併發 ────────────────────────────────────────────────────────

// TestConcurrentJudging 是加那把 per-tournament advisory lock 的唯一理由。
//
// 兩位裁判同時對同一場的兩位選手做 ReportResult 與 WithdrawPlayer,兩條路徑的
// 列鎖方向天生相反(matches → tournament_players vs tournament_players → matches),
// 沒有那把 advisory lock 就是一個真實可達的 40P01。
//
// 斷言兩件事:**沒有死鎖**,而且結果只有一種 —— 場次恰好被判一次,
// 勝者是沒有棄賽的那一位。誰先誰後不重要,重要的是不會兩個都成立。
func TestConcurrentJudging(t *testing.T) {
	setup(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f := newFixture(t)
		a, b := f.player("甲", 1), f.player("乙", 2)
		m := f.match(1, 0, a.id, b.id, match.StatusPending)
		final := f.match(2, 0, 0, 0, match.StatusPending)
		f.runToLive(m)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = f.svc.ReportResult(ctx, match.ReportResultParams{
				MatchPublicID: m.publicID, WinnerPlayerPublicID: a.publicID,
				Confirm: true, ActorUserID: f.judge, Reason: "3:1",
			})
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = f.svc.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
				PlayerPublicID: b.publicID, Confirm: true, ActorUserID: f.judge, Reason: "乙退賽",
			})
		}()
		wg.Wait()

		for _, err := range errs {
			if isDeadlock(err) {
				t.Fatalf("第 %d 輪出現死鎖(advisory lock 沒生效?):%v", i, err)
			}
		}
		st := f.state(m.publicID)
		if st.Status != match.StatusDone || st.Winner != a.id {
			t.Fatalf("第 %d 輪:場次應恰好被判一次且勝者為甲:%+v(errs=%v)", i, st, errs)
		}
		if n := count(f.topics(), match.TopicMatchFinished); n != 1 {
			t.Fatalf("第 %d 輪:賽果公告應恰好一則,得到 %d(errs=%v)", i, n, errs)
		}
		if got := f.state(final.publicID); got.P1 != a.id {
			t.Fatalf("第 %d 輪:勝者應恰好晉級一次:%+v", i, got)
		}
	}
}

// TestConcurrentDoubleReport 是裁判連點兩次「判定勝負」:matches 的列鎖要讓
// 第二次看到的是已經 done 的狀態,而不是兩次都派彩。
func TestConcurrentDoubleReport(t *testing.T) {
	setup(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f := newFixture(t)
		a, b := f.player("甲", 1), f.player("乙", 2)
		m := f.match(1, 0, a.id, b.id, match.StatusPending)
		f.match(2, 0, 0, 0, match.StatusPending)
		f.runToLive(m)
		betID := f.bet(m.id)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		for k := range errs {
			go func() {
				defer wg.Done()
				_, errs[k] = f.svc.ReportResult(ctx, match.ReportResultParams{
					MatchPublicID: m.publicID, WinnerPlayerPublicID: a.publicID,
					Confirm: true, ActorUserID: f.judge, Reason: "3:1",
				})
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
			case errors.Is(err, match.ErrMatchFinished):
				failed++
			default:
				t.Fatalf("第 %d 輪出現非預期錯誤:%v", i, err)
			}
		}
		if ok != 1 || failed != 1 {
			t.Fatalf("第 %d 輪:連點應只成功一次(成功 %d、已完賽 %d)", i, ok, failed)
		}
		if n := count(f.auditActions(), match.ActionReportResult); n != 1 {
			t.Fatalf("第 %d 輪:稽核應只有一筆判定,得到 %d", i, n)
		}
		if got := f.betStatus(betID); got != "won" {
			t.Fatalf("第 %d 輪:注單應只結算一次,狀態 %q", i, got)
		}
		if w, _ := f.record(a.fencerID); w != 1 {
			t.Fatalf("第 %d 輪:戰績只該 +1,得到 %d", i, w)
		}
	}
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// ── 接線 ────────────────────────────────────────────────────────

// TestHandicapWiring 驗證 matchpg.NewHandicaps + handicappg.BindTx 這條接線真的
// 把讓武的寫入放進了**我們的** tx。
//
// 用真的 handicappg 而不是替身:這個測試要驗的就是接線本身 ——
// 型別對得上只代表編得過,「發預算與改場次狀態同生共死」得對真資料庫才看得出來。
func TestHandicapWiring(t *testing.T) {
	f := newFixture(t)
	f.useRealHandicaps()
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 3) // 兩段差 → 預設 8 BP/段 → 16 BP
	m := f.match(1, 0, a.id, b.id, match.StatusPending)

	opened, err := f.svc.OpenHandicap(ctx, match.OpenHandicapParams{
		MatchPublicID: m.publicID, ActorUserID: f.judge, Reason: "開盤",
	})
	if err != nil {
		t.Fatalf("開盤: %v", err)
	}
	if opened.Budget == nil || opened.Budget.Budget != 16 || opened.Budget.PlayerID != a.id {
		t.Fatalf("低段位方應拿到 16 BP:%+v", opened.Budget)
	}
	var budget int64
	if err := pool.QueryRow(ctx,
		`SELECT budget FROM activity.match_budgets WHERE match_id = $1`, m.id).Scan(&budget); err != nil {
		t.Fatalf("預算應與場次狀態同一個 tx 落地: %v", err)
	}
	if budget != 16 {
		t.Fatalf("落地的預算不對:%d", budget)
	}

	locked, err := f.svc.LockHandicap(ctx, match.LockHandicapParams{
		MatchPublicID: m.publicID, Confirm: true, ActorUserID: f.judge, Reason: "封盤",
	})
	if err != nil {
		t.Fatalf("封盤: %v", err)
	}
	if !locked.Handicaps.Revealed || locked.Match.Status != match.StatusLocked {
		t.Fatalf("封盤後應公開且 status=locked:%+v", locked.Match)
	}
	// 封盤的那句 matches UPDATE 權威在 handicap 套件,match 重讀才看得到它
	if st := f.state(m.publicID); st.LockedAt == nil || st.Open {
		t.Fatalf("封盤欄位沒寫進去:%+v", st)
	}
	// 重複封盤用讓武套件的錯誤,不在 match 這裡造同義詞
	if _, err := f.svc.LockHandicap(ctx, match.LockHandicapParams{
		MatchPublicID: m.publicID, Confirm: true, ActorUserID: f.judge, Reason: "再封一次",
	}); !errors.Is(err, handicap.ErrAlreadyLocked) {
		t.Fatalf("重複封盤應回 handicap.ErrAlreadyLocked,得到 %v", err)
	}
}

// ── Rollback ────────────────────────────────────────────────────

// TestRollbackOnSettleFailure 驗證動錢那一步失敗時,前面五件事全部還原。
//
// 這是整個套件的存在理由:判了勝負但沒派彩、晉級了但沒公告,每一種都要人工
// 去資料庫修。所以要嘛六件事都發生,要嘛一件都沒發生。
func TestRollbackOnSettleFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 2)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)
	final := f.match(2, 0, 0, 0, match.StatusPending)
	f.runToLive(m)
	betID := f.bet(m.id)

	auditsBefore := len(f.audits())
	topicsBefore := len(f.topics())

	f.bets.failSettle = true
	if _, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: m.publicID, WinnerPlayerPublicID: a.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "3:1",
	}); !errors.Is(err, errStub) {
		t.Fatalf("結算失敗應整筆失敗,得到 %v", err)
	}

	if st := f.state(m.publicID); st.Status != match.StatusLive || st.Winner != 0 {
		t.Fatalf("場次應還原成 live:%+v", st)
	}
	if st := f.state(final.publicID); st.P1 != 0 {
		t.Fatalf("晉級應一併還原:%+v", st)
	}
	if f.playerStatus(b.publicID) != string(tournament.PlayerActive) {
		t.Fatalf("淘汰應一併還原")
	}
	if w, l := f.record(a.fencerID); w != 0 || l != 0 {
		t.Fatalf("戰績應一併還原,得到 %d/%d", w, l)
	}
	if got := f.betStatus(betID); got != "open" {
		t.Fatalf("注單應維持 open,得到 %q", got)
	}
	if n := len(f.audits()); n != auditsBefore {
		t.Fatalf("稽核應一併還原:%d → %d", auditsBefore, n)
	}
	if n := len(f.topics()); n != topicsBefore {
		t.Fatalf("outbox 應一併還原:%d → %d", topicsBefore, n)
	}

	// 修好之後同一個動作應該照樣做得完 —— rollback 沒有留下任何殘跡
	f.bets.failSettle = false
	if _, err := f.svc.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID: m.publicID, WinnerPlayerPublicID: a.publicID,
		Confirm: true, ActorUserID: f.judge, Reason: "3:1",
	}); err != nil {
		t.Fatalf("重試判定: %v", err)
	}
	if st := f.state(m.publicID); st.Status != match.StatusDone || st.Winner != a.id {
		t.Fatalf("重試後應正常完成:%+v", st)
	}
	if got := f.betStatus(betID); got != "won" {
		t.Fatalf("重試後注單應結算,得到 %q", got)
	}
}

// TestRollbackOnGrantFailure 驗證開盤時發 BP 失敗,狀態不會停在「ready 但沒預算」。
func TestRollbackOnGrantFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.player("甲", 1), f.player("乙", 3)
	m := f.match(1, 0, a.id, b.id, match.StatusPending)

	f.hcap.failGrant = true
	if _, err := f.svc.OpenHandicap(ctx, match.OpenHandicapParams{
		MatchPublicID: m.publicID, ActorUserID: f.judge, Reason: "開盤",
	}); !errors.Is(err, errStub) {
		t.Fatalf("發 BP 失敗應整筆失敗,得到 %v", err)
	}
	if st := f.state(m.publicID); st.Status != match.StatusPending || st.Open {
		t.Fatalf("開盤應整筆還原:%+v", st)
	}
	if n := len(f.audits()); n != 0 {
		t.Fatalf("稽核應一併還原,得到 %d 筆", n)
	}
	if n := len(f.topics()); n != 0 {
		t.Fatalf("outbox 應一併還原,得到 %d 則", n)
	}
}
