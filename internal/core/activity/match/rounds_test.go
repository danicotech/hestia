package match

import (
	"context"
	"errors"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 本檔測 2026-09-13 增補的裁判動線:設定確認、逐回合、季軍戰、違規。
// 假物件會在順序寫反(先結算再寫回合)或跳過守門時直接失敗,所以這裡的
// 「成功」不只是回傳值對,還代表每一步的順序都對。

// ── 三局兩勝的完整流程 ──────────────────────────────────────────

// 驗收條件 2:開盤 → 封盤 → 設定確認 → 第 1 回合 → p1 勝 → 第 2 回合 → p1 勝 → 整場 done。
// Bets 埠的呼叫順序:OpenMarkets 在開盤、CloseMarkets 在第 1 回合開始、
// SettleRound ×2、SettleMatch ×1。
func TestMultiRoundFlow(t *testing.T) {
	f := fourPlayers(t)
	f.config(3, false)
	f.db.openBets["M-R1S0"] = 4
	f.db.openRoundBets["M-R1S0"] = map[int]int{1: 2, 2: 3}
	f.runToLocked(t, "M-R1S0")

	r1 := f.startRound(t, "M-R1S0")
	if r1.Round.RoundNo != 1 || r1.Match.Status != StatusLive || r1.ClosedMarkets != 4 {
		t.Fatalf("第一回合開始應 = 開打 + 關盤: %+v", r1)
	}
	f1 := f.finishRound(t, "M-R1S0", 1, "P-A")
	switch {
	case f1.Decided:
		t.Fatal("1:0 還沒定案")
	case f1.Score != (Score{P1Wins: 1, P2Wins: 0}):
		t.Fatalf("比數 = %+v", f1.Score)
	case f1.RoundSettledBetCount != 2:
		t.Fatalf("第 1 回合盤口應結算 2 張,得到 %d", f1.RoundSettledBetCount)
	case f1.Round.WinnerPublicID != "P-A":
		t.Fatalf("回合勝者 = %q", f1.Round.WinnerPublicID)
	case f.db.matches[10].status != StatusLive:
		t.Fatalf("1:0 後場次應仍是 live: %s", f.db.matches[10].status)
	}

	r2 := f.startRound(t, "M-R1S0")
	if r2.Round.RoundNo != 2 || r2.ClosedMarkets != 0 {
		t.Fatalf("第二回合: %+v", r2)
	}
	f2 := f.finishRound(t, "M-R1S0", 2, "P-A")
	switch {
	case !f2.Decided || f2.Outcome == nil:
		t.Fatalf("2:0 應整場定案: %+v", f2)
	case f2.Match.Status != StatusDone || f2.Match.WinnerPlayerID != 1:
		t.Fatalf("場次未正確結束: %+v", f2.Match)
	case f2.Score.String() != "2:0":
		t.Fatalf("比數 = %s", f2.Score)
	case f2.Outcome.SettledBetCount != 4:
		t.Fatalf("整場盤口應結算 4 張,得到 %d", f2.Outcome.SettledBetCount)
	case f.db.matches[12].p1ID != 1:
		t.Fatalf("勝者應晉級決賽 P1: %+v", f.db.matches[12])
	case f.db.players[2].status != tournament.PlayerEliminated:
		t.Fatal("敗方應標 eliminated")
	}

	assertCallOrder(t, f.db.calls,
		"OpenMarkets(M-R1S0)",
		"ConfirmSetup(M-R1S0)",
		"MarkLive(M-R1S0)", "CloseMarkets(M-R1S0)", "StartRound(M-R1S0,1)",
		"FinishRound(M-R1S0,1,winner=1)", "SettleRound(M-R1S0,1)",
		"StartRound(M-R1S0,2)",
		"FinishRound(M-R1S0,2,winner=1)", "SettleRound(M-R1S0,2)",
		"MarkFinished(M-R1S0,winner=1,kind=normal)", "SettleMatch(M-R1S0)")
	// OpenMarkets 只在開盤那一次,SettleMatch 只一次。
	for _, c := range []string{"OpenMarkets(M-R1S0)", "SettleMatch(M-R1S0)", "CloseMarkets(M-R1S0)"} {
		if n := countCalls(f.db.calls, c); n != 1 {
			t.Fatalf("%s 應恰好一次,得到 %d", c, n)
		}
	}
	if n := countCalls(f.db.calls, "SettleRound(M-R1S0,1)") + countCalls(f.db.calls, "SettleRound(M-R1S0,2)"); n != 2 {
		t.Fatalf("SettleRound 應兩次,得到 %d", n)
	}

	// 賽果公告帶比數;回合事件各兩則(開始 / 結束),而且只推不進 outbox。
	fin := f.eventsByTopic(TopicMatchFinished)
	if len(fin) != 1 {
		t.Fatalf("賽果事件 = %d 則", len(fin))
	}
	p := decodeMatchEvent(t, fin[0])
	if p.Result == nil || p.Result.Score == nil || p.Result.Score.Text != "2:0" || p.Result.Score.WinsNeeded != 2 {
		t.Fatalf("賽果公告要帶回合比數: %+v", p.Result)
	}
	if got := len(f.eventsByTopic(TopicRoundStarted)); got != 2 {
		t.Fatalf("回合開始事件 = %d", got)
	}
	if got := len(f.eventsByTopic(TopicRoundFinished)); got != 2 {
		t.Fatalf("回合結束事件 = %d", got)
	}
	for _, e := range f.eventsByTopic(TopicRoundFinished) {
		if e.Announced() {
			t.Fatal("回合事件不該進 outbox(沒有人認領)")
		}
		rp := decodeMatchEvent(t, e)
		if rp.CurrentRound == nil || rp.CurrentRound.Winner == nil || rp.CurrentRound.Winner.DisplayName != "阿甲" {
			t.Fatalf("回合事件要帶當回合與勝者: %+v", rp.CurrentRound)
		}
	}
	if !f.eventsByTopic(TopicMatchFinished)[0].Announced() {
		t.Fatal("賽果公告要進 outbox")
	}

	// 稽核:確認、開打、兩次開始回合、兩次結束回合、判定各一筆。
	want := map[string]int{
		ActionConfirmSetup: 1, ActionStartMatch: 1, ActionStartRound: 2,
		ActionFinishRound: 2, ActionReportResult: 1,
	}
	for action, n := range want {
		if got := countAudits(f.db.audits, action); got != n {
			t.Fatalf("稽核 %s = %d,期望 %d", action, got, n)
		}
	}
}

// 三局打滿:1:1 之後第三回合才定案。
func TestMultiRoundGoesToThird(t *testing.T) {
	f := fourPlayers(t)
	f.config(3, false)
	f.runToLocked(t, "M-R1S1")
	f.startRound(t, "M-R1S1")
	f.finishRound(t, "M-R1S1", 1, "P-C")
	f.startRound(t, "M-R1S1")
	if rf := f.finishRound(t, "M-R1S1", 2, "P-D"); rf.Decided || rf.Score.String() != "1:1" {
		t.Fatalf("1:1 不該定案: %+v", rf)
	}
	f.startRound(t, "M-R1S1")
	rf := f.finishRound(t, "M-R1S1", 3, "P-D")
	if !rf.Decided || rf.Match.WinnerPlayerID != 4 || rf.Score.String() != "1:2" {
		t.Fatalf("2:1 應由阿丁勝出: %+v", rf)
	}
	p := decodeMatchEvent(t, f.eventsByTopic(TopicMatchFinished)[0])
	if p.Result.Score.Text != "1:2" || p.Result.Winner.DisplayName != "阿丁" {
		t.Fatalf("賽果 = %+v", p.Result)
	}
}

// ── 守門 ────────────────────────────────────────────────────────

// 驗收條件 4:2:0 之後再開回合 → ErrMatchDecided;同一回合結束兩次 → 錯;勝者不在場上 → 錯。
func TestRoundGates(t *testing.T) {
	ctx := context.Background()

	t.Run("定案後不能再開回合", func(t *testing.T) {
		f := fourPlayers(t)
		f.config(3, false)
		f.runToLocked(t, "M-R1S0")
		f.startRound(t, "M-R1S0")
		f.finishRound(t, "M-R1S0", 1, "P-A")
		f.startRound(t, "M-R1S0")
		f.finishRound(t, "M-R1S0", 2, "P-A")
		_, err := f.svc.StartRound(ctx, "M-R1S0", judgeID)
		if !errors.Is(err, ErrMatchDecided) {
			t.Fatalf("err = %v,期望 ErrMatchDecided", err)
		}
		if len(f.db.rounds[10]) != 2 {
			t.Fatal("被擋下來卻建了第三回合")
		}
	})

	t.Run("同一回合不能結束兩次", func(t *testing.T) {
		f := fourPlayers(t)
		f.config(3, false)
		f.runToLocked(t, "M-R1S0")
		f.startRound(t, "M-R1S0")
		f.finishRound(t, "M-R1S0", 1, "P-A")
		before := countCalls(f.db.calls, "SettleRound(M-R1S0,1)")
		_, err := f.svc.FinishRound(ctx, FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 1, WinnerPublicID: "P-B", ActorUserID: judgeID, Note: "", Confirm: true})
		if !errors.Is(err, ErrRoundAlreadyFinished) {
			t.Fatalf("err = %v,期望 ErrRoundAlreadyFinished", err)
		}
		if f.db.rounds[10][0].winnerID != 1 {
			t.Fatal("第二次結束改寫了第一次的勝者")
		}
		if countCalls(f.db.calls, "SettleRound(M-R1S0,1)") != before {
			t.Fatal("同一回合結算了兩次")
		}
	})

	t.Run("勝者不在場上", func(t *testing.T) {
		f := fourPlayers(t)
		f.config(3, false)
		f.runToLocked(t, "M-R1S0")
		f.startRound(t, "M-R1S0")
		_, err := f.svc.FinishRound(ctx, FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 1, WinnerPublicID: "P-C", ActorUserID: judgeID, Note: "", Confirm: true})
		if !errors.Is(err, ErrWinnerNotInMatch) {
			t.Fatalf("err = %v,期望 ErrWinnerNotInMatch", err)
		}
		if f.db.rounds[10][0].finishedAt != nil {
			t.Fatal("被擋下來卻結束了回合")
		}
	})

	t.Run("上一回合沒結束不能開下一回合", func(t *testing.T) {
		f := fourPlayers(t)
		f.config(3, false)
		f.runToLocked(t, "M-R1S0")
		f.startRound(t, "M-R1S0")
		_, err := f.svc.StartRound(ctx, "M-R1S0", judgeID)
		if !errors.Is(err, ErrRoundInProgress) {
			t.Fatalf("err = %v,期望 ErrRoundInProgress", err)
		}
		if len(f.db.rounds[10]) != 1 {
			t.Fatal("被擋下來卻建了回合")
		}
	})

	t.Run("回合不存在", func(t *testing.T) {
		f := fourPlayers(t)
		f.config(3, false)
		f.runToLocked(t, "M-R1S0")
		f.startRound(t, "M-R1S0")
		_, err := f.svc.FinishRound(ctx, FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 2, WinnerPublicID: "P-A", ActorUserID: judgeID, Note: "", Confirm: true})
		if !errors.Is(err, ErrRoundNotFound) {
			t.Fatalf("err = %v,期望 ErrRoundNotFound", err)
		}
	})

	t.Run("還沒開打不能結束回合", func(t *testing.T) {
		f := fourPlayers(t)
		f.config(3, false)
		f.runToLocked(t, "M-R1S0")
		_, err := f.svc.FinishRound(ctx, FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 1, WinnerPublicID: "P-A", ActorUserID: judgeID, Note: "", Confirm: true})
		if !errors.Is(err, ErrNotLive) {
			t.Fatalf("err = %v,期望 ErrNotLive", err)
		}
	})

	t.Run("缺裁判身分", func(t *testing.T) {
		f := fourPlayers(t)
		if _, err := f.svc.StartRound(ctx, "M-R1S0", 0); !errors.Is(err, tournament.ErrActorRequired) {
			t.Fatalf("StartRound err = %v", err)
		}
		if _, err := f.svc.FinishRound(ctx, FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 1, WinnerPublicID: "P-A", ActorUserID: 0, Note: "", Confirm: true}); !errors.Is(err, tournament.ErrActorRequired) {
			t.Fatalf("FinishRound err = %v", err)
		}
		if _, err := f.svc.ConfirmSetup(ctx, "M-R1S0", 0); !errors.Is(err, tournament.ErrActorRequired) {
			t.Fatalf("ConfirmSetup err = %v", err)
		}
	})
}

// 整場勝者是衍生值:回合列被動過、與要寫入的勝者對不上時,整筆失敗。
func TestFinishRoundRejectsInconsistentRounds(t *testing.T) {
	f := fourPlayers(t)
	f.config(3, false)
	f.runToLocked(t, "M-R1S0")
	f.startRound(t, "M-R1S0")
	f.finishRound(t, "M-R1S0", 1, "P-A")
	f.startRound(t, "M-R1S0")
	// 有人在資料庫裡把第 1 回合改成阿乙勝:第 2 回合阿甲勝之後比數 1:1,沒有人達標;
	// 但假物件的回合列此刻已經是「阿乙 2 勝」的話會怎樣?直接模擬最壞情況:
	// 第 1 回合被改成對手、再多塞一列對手勝 —— 對手已經 2 勝卻沒有定案。
	f.db.rounds[10][0].winnerID = 2
	now := f.db.now()
	f.db.rounds[10] = append(f.db.rounds[10], fakeRound{matchID: 10, roundNo: 3, startedAt: *now, finishedAt: now, winnerID: 2})

	_, err := f.svc.FinishRound(context.Background(), FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 2, WinnerPublicID: "P-A", ActorUserID: judgeID, Note: "", Confirm: true})
	if !errors.Is(err, ErrRoundsInconsistent) {
		t.Fatalf("err = %v,期望 ErrRoundsInconsistent", err)
	}
	if f.db.matches[10].status != StatusLive || f.db.matches[10].winnerID != 0 {
		t.Fatalf("不一致時不得寫入勝者: %+v", f.db.matches[10])
	}
	if f.db.rounds[10][1].finishedAt != nil {
		t.Fatal("整筆應 rollback,第 2 回合不該留下結果")
	}
}

// 單回合盤口結算失敗時,回合結果一起消失(與整場結算同一條紀律)。
func TestRoundSettleFailureRollsBackRound(t *testing.T) {
	f := fourPlayers(t)
	f.config(3, false)
	f.runToLocked(t, "M-R1S0")
	f.startRound(t, "M-R1S0")
	f.db.failSettleRound = true

	if _, err := f.svc.FinishRound(context.Background(), FinishRoundParams{MatchPublicID: "M-R1S0", RoundNo: 1, WinnerPublicID: "P-A", ActorUserID: judgeID, Note: "", Confirm: true}); err == nil {
		t.Fatal("應該失敗")
	}
	if f.db.rounds[10][0].finishedAt != nil {
		t.Fatal("回合結果應還原")
	}
	if countAudits(f.db.audits, ActionFinishRound) != 0 {
		t.Fatal("稽核應一併還原")
	}
	if len(f.eventsByTopic(TopicRoundFinished)) != 0 {
		t.Fatal("事件應一併還原")
	}
}

// ── ReportResult 與 best_of ─────────────────────────────────────

// 驗收條件 5:best_of = 1 的 ReportResult 仍可用,而且走回合路徑(回合列存在)。
func TestReportResultBestOfOneUsesRoundPath(t *testing.T) {
	f := fourPlayers(t) // 預設 config:best_of = 1
	f.db.openRoundBets["M-R1S1"] = map[int]int{1: 1}
	f.runToLive(t, "M-R1S1")

	out, err := f.svc.ReportResult(context.Background(), ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-D", Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("判定失敗: %v", err)
	}
	if out.Match.Status != StatusDone || out.Match.WinnerPlayerID != 4 {
		t.Fatalf("場次: %+v", out.Match)
	}
	rs := f.db.rounds[11]
	if len(rs) != 1 || rs[0].finishedAt == nil || rs[0].winnerID != 4 {
		t.Fatalf("回合列要存在且以該勝者結束: %+v", rs)
	}
	assertCallOrder(t, f.db.calls,
		"StartRound(M-R1S1,1)", "FinishRound(M-R1S1,1,winner=4)", "SettleRound(M-R1S1,1)",
		"MarkFinished(M-R1S1,winner=4,kind=normal)", "SettleMatch(M-R1S1)")
	p := decodeMatchEvent(t, f.eventsByTopic(TopicMatchFinished)[0])
	if p.Result.Score == nil || p.Result.Score.Text != "0:1" {
		t.Fatalf("單場也帶比數: %+v", p.Result)
	}
}

// 場次 live 卻沒有回合列(舊資料)時,ReportResult 自己建第 1 回合再結束它。
func TestReportResultCreatesRoundWhenMissing(t *testing.T) {
	f := fourPlayers(t)
	f.db.matches[11].status = StatusLive
	out, err := f.svc.ReportResult(context.Background(), ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-C", Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("判定失敗: %v", err)
	}
	if out.Match.WinnerPlayerID != 3 || len(f.db.rounds[11]) != 1 {
		t.Fatalf("應補建第 1 回合並定案: %+v / %+v", out.Match, f.db.rounds[11])
	}
}

// 驗收條件 5:best_of = 3 的 ReportResult → ErrMultiRoundMatch,什麼都不寫。
func TestReportResultRejectsMultiRound(t *testing.T) {
	f := fourPlayers(t)
	f.config(3, false)
	f.runToLive(t, "M-R1S1")
	f.db.calls = nil

	_, err := f.svc.ReportResult(context.Background(), ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-C", Confirm: true, ActorUserID: judgeID,
	})
	if !errors.Is(err, ErrMultiRoundMatch) {
		t.Fatalf("err = %v,期望 ErrMultiRoundMatch", err)
	}
	if f.db.matches[11].status != StatusLive || f.db.rounds[11][0].finishedAt != nil {
		t.Fatal("被擋下來卻動了場次或回合")
	}
}

// ── 設定確認 ────────────────────────────────────────────────────

func TestConfirmSetup(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.selections[10] = []handicap.Selection{
		{PublicID: "S-1", ItemKey: "skill.ban_two", ItemName: "禁用兩招", Category: handicap.CategorySkill,
			ItemRefereeNote: "對手不得使用指定兩招", TargetNote: "松濤劍法、踏雪無痕", Cost: 6},
		{PublicID: "S-2", ItemKey: "defense.no_jump", ItemName: "禁跳躍", Category: handicap.CategoryDefense,
			ItemRefereeNote: "對手全場不得跳躍", Cost: 2, Voided: true},
	}

	// 封盤前:檢視只有場次,沒有清單;確認被擋。
	rv, err := f.svc.ReviewSetup(ctx, "M-R1S0")
	if err != nil {
		t.Fatalf("ReviewSetup: %v", err)
	}
	if rv.Checklist != nil || rv.Handicaps != nil {
		t.Fatalf("封盤前不該有清單: %+v", rv)
	}
	if _, err := f.svc.ConfirmSetup(ctx, "M-R1S0", judgeID); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("封盤前確認應回 ErrNotLocked,得到 %v", err)
	}

	if _, err := f.svc.OpenHandicap(ctx, OpenHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID}); err != nil {
		t.Fatalf("開盤: %v", err)
	}
	if _, err := f.svc.LockHandicap(ctx, LockHandicapParams{MatchPublicID: "M-R1S0", Confirm: true, ActorUserID: judgeID}); err != nil {
		t.Fatalf("封盤: %v", err)
	}

	rv, err = f.svc.ReviewSetup(ctx, "M-R1S0")
	if err != nil {
		t.Fatalf("ReviewSetup: %v", err)
	}
	// 清單是 handicap 的推導值:未作廢的一筆,帶 referee_note 與指定內容。
	if len(rv.Checklist) != 1 || rv.Checklist[0].RefereeNote != "對手不得使用指定兩招" ||
		rv.Checklist[0].TargetNote != "松濤劍法、踏雪無痕" {
		t.Fatalf("清單 = %+v", rv.Checklist)
	}
	if rv.WinsNeeded != 1 || rv.Match.SetupConfirmedAt != nil {
		t.Fatalf("檢視 = %+v", rv)
	}

	m, err := f.svc.ConfirmSetup(ctx, "M-R1S0", judgeID)
	if err != nil {
		t.Fatalf("確認: %v", err)
	}
	if m.SetupConfirmedAt == nil || m.SetupConfirmedBy != judgeID || m.Status != StatusLocked {
		t.Fatalf("確認後: %+v", m)
	}
	if countAudits(f.db.audits, ActionConfirmSetup) != 1 {
		t.Fatal("確認要寫一筆稽核")
	}
	first := *m.SetupConfirmedAt

	// 重複確認要出聲,而且不能蓋掉第一次的時間。
	if _, err := f.svc.ConfirmSetup(ctx, "M-R1S0", 999); !errors.Is(err, ErrSetupAlreadyConfirmed) {
		t.Fatalf("重複確認應回 ErrSetupAlreadyConfirmed,得到 %v", err)
	}
	if got := f.db.matches[10]; !got.confirmedAt.Equal(first) || got.confirmedBy != judgeID {
		t.Fatalf("第二次確認蓋掉了第一次: %+v", got)
	}
	// 確認不發公告。
	if len(f.eventsByTopic(TopicHandicapLocked)) != 1 || len(f.db.events) != 2 {
		t.Fatalf("確認不該發事件: %+v", f.db.events)
	}
}

// ── 季軍戰 ──────────────────────────────────────────────────────

// eightPlayers 是 8 人樹(TotalRounds = 3):準決賽是 R2 的兩場、決賽 R3。
// 準決賽直接以雙方已就座的狀態建好,首輪不建(與本檔要驗的事無關)。
func eightPlayers(t *testing.T, thirdPlace bool) *fixture {
	t.Helper()
	f := newFixture(t, 3)
	f.config(1, thirdPlace)
	f.addPlayer(1, "P-A", "阿甲", bp.RankDuanshui)
	f.addPlayer(2, "P-B", "阿乙", bp.RankDuanshui)
	f.addPlayer(3, "P-C", "阿丙", bp.RankDuanshui)
	f.addPlayer(4, "P-D", "阿丁", bp.RankDuanshui)
	f.addMatch(20, "M-SEMI0", 2, 0, 1, 2, StatusPending)
	f.addMatch(21, "M-SEMI1", 2, 1, 3, 4, StatusPending)
	f.addMatch(30, "M-FINAL", 3, 0, 0, 0, StatusPending)
	return f
}

func (f *fixture) report(t *testing.T, matchPublicID, winner string) *ReportResultOutcome {
	t.Helper()
	f.runToLive(t, matchPublicID)
	out, err := f.svc.ReportResult(context.Background(), ReportResultParams{
		MatchPublicID: matchPublicID, WinnerPlayerPublicID: winner, Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("判定 %s: %v", matchPublicID, err)
	}
	return out
}

func (f *fixture) thirdPlaceMatch() *fakeMatch {
	for _, m := range f.db.matches {
		if m.kind == KindThirdPlace {
			return m
		}
	}
	return nil
}

// 驗收條件 6:兩場準決賽都 done 後出現一場 kind=third_place,雙方是兩位敗者;只建一次。
func TestThirdPlaceMatchCreatedAfterBothSemis(t *testing.T) {
	f := eightPlayers(t, true)

	out := f.report(t, "M-SEMI0", "P-A") // 阿乙敗
	if out.ThirdPlaceMatch != nil || f.thirdPlaceMatch() != nil {
		t.Fatal("只判完一場準決賽不該建季軍戰")
	}

	out = f.report(t, "M-SEMI1", "P-D") // 阿丙敗
	third := f.thirdPlaceMatch()
	switch {
	case out.ThirdPlaceMatch == nil || third == nil:
		t.Fatal("兩場準決賽都判完應建季軍戰")
	case third.round != 3 || third.slot != 1:
		t.Fatalf("季軍戰位置應是決賽那一輪 slot 1,得到 r%ds%d", third.round, third.slot)
	case third.p1ID != 2 || third.p2ID != 3:
		t.Fatalf("季軍戰雙方應是兩位敗者(slot 0 的敗者在 P1):p1=%d p2=%d", third.p1ID, third.p2ID)
	case third.status != StatusPending:
		t.Fatalf("季軍戰應等裁判開盤:%s", third.status)
	case out.ThirdPlaceMatch.PublicID != third.publicID:
		t.Fatalf("回傳的季軍戰不是建出來的那一場")
	}
	p := decodeMatchEvent(t, f.eventsByTopic(TopicMatchFinished)[1])
	if p.Result.ThirdPlaceMatchPublicID != third.publicID {
		t.Fatalf("賽果公告要帶季軍戰: %+v", p.Result)
	}

	// 決賽判完不會再建一場。
	f.report(t, "M-FINAL", "P-A")
	n := 0
	for _, m := range f.db.matches {
		if m.kind == KindThirdPlace {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("季軍戰應只建一次,得到 %d 場", n)
	}

	// 季軍戰走完整流程:勝者不晉級、沒有冠軍事件、敗者不會再被標一次。
	champions := len(f.eventsByTopic(TopicChampion))
	out = f.report(t, third.publicID, "P-C")
	switch {
	case out.ChampionPlayerPublicID != "":
		t.Fatalf("季軍戰不該產生冠軍: %s", out.ChampionPlayerPublicID)
	case len(out.AdvancedMatches) != 0:
		t.Fatalf("季軍戰勝者不晉級: %+v", out.AdvancedMatches)
	case f.db.matches[third.id].status != StatusDone || f.db.matches[third.id].winnerID != 3:
		t.Fatalf("季軍戰結果: %+v", f.db.matches[third.id])
	case len(f.eventsByTopic(TopicChampion)) != champions:
		t.Fatal("季軍戰不該發冠軍公告")
	case f.db.fencers[1003].wins != 1 || f.db.fencers[1002].losses != 2:
		t.Fatalf("戰績照算: 丙=%+v 乙=%+v", f.db.fencers[1003], f.db.fencers[1002])
	}
	fin := f.eventsByTopic(TopicMatchFinished)
	last := decodeMatchEvent(t, fin[len(fin)-1])
	if last.Kind != KindThirdPlace || last.IsFinal {
		t.Fatalf("季軍戰的賽果公告要標 kind、不是決賽: kind=%s is_final=%v", last.Kind, last.IsFinal)
	}
}

// 季軍戰已經存在(例如上一次判定的 tx 已建、或人工建過)時,第二場準決賽判完不再建:
// FindThirdPlaceMatch 是第一道,UNIQUE (tournament_id, round, slot) 是第二道 ——
// 少了第一道,這裡會撞到假物件模擬的 UNIQUE 而整筆失敗。
func TestThirdPlaceMatchNotDuplicated(t *testing.T) {
	f := eightPlayers(t, true)
	f.db.matches[40] = &fakeMatch{
		id: 40, publicID: "M-3RD-PRE", tournamentID: 1, round: 3, slot: 1, kind: KindThirdPlace,
		status: StatusPending, resultKind: ResultNormal, p1ID: 2, p2ID: 3,
	}
	f.report(t, "M-SEMI0", "P-A")
	out := f.report(t, "M-SEMI1", "P-D")
	if out.ThirdPlaceMatch != nil {
		t.Fatalf("已存在的季軍戰不該再建:%+v", out.ThirdPlaceMatch)
	}
	if countCalls(f.db.calls, "InsertThirdPlaceMatch(3,1,p1=2,p2=3)") != 0 {
		t.Fatal("已存在仍嘗試 INSERT")
	}
}

// 驗收條件 6:third_place_match = false 時不建。
func TestThirdPlaceMatchDisabled(t *testing.T) {
	f := eightPlayers(t, false)
	f.report(t, "M-SEMI0", "P-A")
	out := f.report(t, "M-SEMI1", "P-D")
	if out.ThirdPlaceMatch != nil || f.thirdPlaceMatch() != nil {
		t.Fatal("本屆不打季軍戰卻建了")
	}
}

// 準決賽是棄賽判的不戰而勝時,棄賽者仍是「敗者」;季軍戰建好後他那一側當場判成不戰而勝。
func TestThirdPlaceWithWithdrawnLoserIsWalkover(t *testing.T) {
	f := eightPlayers(t, true)
	f.report(t, "M-SEMI0", "P-A")
	if _, err := f.svc.WithdrawPlayer(context.Background(), WithdrawPlayerParams{
		PlayerPublicID: "P-D", Confirm: true, ActorUserID: judgeID,
	}); err != nil {
		t.Fatalf("棄賽: %v", err)
	}
	third := f.thirdPlaceMatch()
	if third == nil {
		t.Fatal("第二場準決賽以不戰而勝判完,季軍戰應成形")
	}
	if third.status != StatusDone || third.resultKind != ResultWalkover || third.winnerID != 2 {
		t.Fatalf("棄賽者那一側應判成不戰而勝、阿乙得季軍: %+v", third)
	}
}

// ── 違規 ────────────────────────────────────────────────────────

// 驗收條件 7:note 空被拒;player 不在場上被拒;紀錄寫入且稽核有一筆;不改變任何回合或賽果。
func TestRecordViolation(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.config(3, false)
	f.runToLocked(t, "M-R1S0")
	f.startRound(t, "M-R1S0")
	f.finishRound(t, "M-R1S0", 1, "P-A")
	f.startRound(t, "M-R1S0")
	matchesBefore := f.db.snapshot().matches
	roundsBefore := f.db.snapshot().rounds
	eventsBefore := len(f.db.events)
	round := 2

	t.Run("note 空", func(t *testing.T) {
		_, err := f.svc.RecordViolation(ctx, RecordViolationParams{
			MatchPublicID: "M-R1S0", RoundNo: &round, PlayerPublicID: "P-B",
			Ruling: RulingWarning, Note: "  ", ActorUserID: judgeID,
		})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("player 不在場上", func(t *testing.T) {
		before := len(f.db.calls)
		_, err := f.svc.RecordViolation(ctx, RecordViolationParams{
			MatchPublicID: "M-R1S0", PlayerPublicID: "P-C",
			Ruling: RulingWarning, Note: "用了禁招", ActorUserID: judgeID,
		})
		if !errors.Is(err, ErrPlayerNotInMatch) {
			t.Fatalf("err = %v", err)
		}
		// service 要在寫入之前就擋住(adapter 的同一道檢查是第二層,不是第一層)。
		for _, c := range f.db.calls[before:] {
			if c != "LockMatch(M-R1S0)" {
				t.Fatalf("被擋下來卻有寫入呼叫: %v", f.db.calls[before:])
			}
		}
	})
	t.Run("ruling 不合法", func(t *testing.T) {
		_, err := f.svc.RecordViolation(ctx, RecordViolationParams{
			MatchPublicID: "M-R1S0", PlayerPublicID: "P-B",
			Ruling: Ruling("disqualify"), Note: "x", ActorUserID: judgeID,
		})
		if !errors.Is(err, ErrInvalidRuling) {
			t.Fatalf("err = %v", err)
		}
	})
	if len(f.db.violations[10]) != 0 || countAudits(f.db.audits, ActionRecordViolation) != 0 {
		t.Fatal("被擋下的紀錄不該留下任何東西")
	}

	v, err := f.svc.RecordViolation(ctx, RecordViolationParams{
		MatchPublicID: "M-R1S0", RoundNo: &round, PlayerPublicID: "P-B", ItemPublicID: "I-9",
		Ruling: RulingRoundLoss, Note: "第二回合用了被禁的松濤劍法,判該回合負", ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("記錄違規: %v", err)
	}
	switch {
	case v.PublicID == "" || v.MatchPublicID != "M-R1S0" || v.PlayerPublicID != "P-B":
		t.Fatalf("紀錄 = %+v", v)
	case v.RoundNo == nil || *v.RoundNo != 2 || v.Ruling != RulingRoundLoss:
		t.Fatalf("紀錄 = %+v", v)
	case v.ItemPublicID != "I-9":
		t.Fatalf("紀錄 = %+v", v)
	case len(f.db.violations[10]) != 1:
		t.Fatal("紀錄沒寫進去")
	case countAudits(f.db.audits, ActionRecordViolation) != 1:
		t.Fatal("稽核應恰好一筆")
	}

	// 不觸發任何後果:場次、回合、事件全部不變。
	after := f.db.snapshot()
	for id, m := range matchesBefore {
		if after.matches[id] != m {
			t.Fatalf("違規紀錄改了場次 %d: %+v → %+v", id, m, after.matches[id])
		}
	}
	for id, rs := range roundsBefore {
		if len(after.rounds[id]) != len(rs) {
			t.Fatalf("違規紀錄改了回合數")
		}
		for i := range rs {
			if after.rounds[id][i] != rs[i] {
				t.Fatalf("違規紀錄改了回合 %d#%d", id, i)
			}
		}
	}
	if len(f.db.events) != eventsBefore {
		t.Fatalf("違規不單發公告: %+v", f.db.events[eventsBefore:])
	}

	list, err := f.svc.ListMatchViolations(ctx, "M-R1S0")
	if err != nil || len(list) != 1 || list[0].PublicID != v.PublicID {
		t.Fatalf("列表 = %+v, err = %v", list, err)
	}
}

// 判負的違規會進賽果公告(grill Q14);警告不會。
func TestFinishedEventCarriesLossViolations(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.config(3, false)
	f.runToLocked(t, "M-R1S0")
	f.startRound(t, "M-R1S0")
	f.finishRound(t, "M-R1S0", 1, "P-A")
	f.startRound(t, "M-R1S0")
	round := 2
	for _, v := range []RecordViolationParams{
		{MatchPublicID: "M-R1S0", RoundNo: &round, PlayerPublicID: "P-B", Ruling: RulingRoundLoss, Note: "用了禁招,判該回合負", ActorUserID: judgeID},
		{MatchPublicID: "M-R1S0", PlayerPublicID: "P-A", Ruling: RulingWarning, Note: "狠話沒說完整,口頭警告", ActorUserID: judgeID},
	} {
		if _, err := f.svc.RecordViolation(ctx, v); err != nil {
			t.Fatalf("記錄違規: %v", err)
		}
	}
	// 裁判依判決把第 2 回合填給對方 → 2:0 定案。
	f.finishRound(t, "M-R1S0", 2, "P-A")

	p := decodeMatchEvent(t, f.eventsByTopic(TopicMatchFinished)[0])
	if len(p.Result.Violations) != 1 {
		t.Fatalf("賽果公告應只帶判負的那一筆: %+v", p.Result.Violations)
	}
	got := p.Result.Violations[0]
	if got.DisplayName != "阿乙" || got.RoundNo != 2 || got.Ruling != RulingRoundLoss || got.Note == "" {
		t.Fatalf("違規原因不完整: %+v", got)
	}
}

// ── helpers ─────────────────────────────────────────────────────

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func countAudits(audits []JudgeAction, action string) int {
	n := 0
	for _, a := range audits {
		if a.Action == action {
			n++
		}
	}
	return n
}

// 二次確認的權威在 core:沒帶 confirm 連場次都不讀,更不會動到回合。
func TestFinishRoundRequiresConfirm(t *testing.T) {
	_, err := (&Service[fakeTx]{}).FinishRound(context.Background(), FinishRoundParams{
		MatchPublicID: "M-R1S0", RoundNo: 1, WinnerPublicID: "P-A", ActorUserID: 7,
	})
	if !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("err = %v,want ErrConfirmationRequired", err)
	}
}
