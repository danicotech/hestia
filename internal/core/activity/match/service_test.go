package match

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

const judgeID int64 = 777

// fixture 是一屆已經在進行中的賽事。
//
// 預設是 4 人樹(TotalRounds = 2):
//
//	R1 slot0  A(開山) vs B(飛花)   ← 讓武 16 BP 給 A
//	R1 slot1  C vs D
//	R2 slot0  決賽(雙方待定)
//
// 用固定的小樹而不是隨機產生:晉級鏈的斷言要人看得懂,
// 「slot 1 的勝者進決賽的 P2」這種事寫死在測試裡才有意義。
type fixture struct {
	db  *fakeDB
	svc *Service[fakeTx]
}

func newFixture(t *testing.T, totalRounds int) *fixture {
	t.Helper()
	db := newFakeDB()
	db.tournaments[1] = &Tournament{
		ID: 1, PublicID: "T1", Slug: "shifeng-1", Name: "百業試鋒 第一屆",
		Phase: tournament.PhaseInProgress, TotalRounds: totalRounds,
	}
	return &fixture{db: db, svc: NewService[fakeTx](db, db, db)}
}

func (f *fixture) addPlayer(id int64, publicID, name string, rank bp.Rank) {
	f.db.players[id] = &fakePlayer{
		id: id, publicID: publicID, tournamentID: 1, fencerID: 1000 + id,
		displayName: name, rank: rank, status: tournament.PlayerActive,
	}
	f.db.fencers[1000+id] = &fakeFencer{}
}

func (f *fixture) addMatch(id int64, publicID string, round, slot int, p1, p2 int64, status Status) {
	f.db.matches[id] = &fakeMatch{
		id: id, publicID: publicID, tournamentID: 1,
		round: round, slot: slot, p1ID: p1, p2ID: p2,
		status: status, resultKind: ResultNormal,
	}
}

// fourPlayers 是最常用的起手式。
func fourPlayers(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, 2)
	f.addPlayer(1, "P-A", "阿甲", bp.RankKaishan)
	f.addPlayer(2, "P-B", "阿乙", bp.RankFeihua)
	f.addPlayer(3, "P-C", "阿丙", bp.RankDuanshui)
	f.addPlayer(4, "P-D", "阿丁", bp.RankDuanshui)
	f.addMatch(10, "M-R1S0", 1, 0, 1, 2, StatusPending)
	f.addMatch(11, "M-R1S1", 1, 1, 3, 4, StatusPending)
	f.addMatch(12, "M-FINAL", 2, 0, 0, 0, StatusPending)
	// A 低 B 兩段 → 16 BP。
	f.db.budgets[10] = &handicap.Budget{MatchID: 10, PlayerID: 1, PlayerPublicID: "P-A", Budget: 16}
	return f
}

func (f *fixture) lastEvent(t *testing.T) Event {
	t.Helper()
	if len(f.db.events) == 0 {
		t.Fatal("沒有任何 outbox 事件")
	}
	return f.db.events[len(f.db.events)-1]
}

func (f *fixture) eventsByTopic(topic string) []Event {
	var out []Event
	for _, e := range f.db.events {
		if e.Topic == topic {
			out = append(out, e)
		}
	}
	return out
}

func decodeMatchEvent(t *testing.T, e Event) MatchEvent {
	t.Helper()
	var m MatchEvent
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	return m
}

// 走完一場的前半段(開盤 → 封盤 → 開打),讓 ReportResult 有東西可判。
func (f *fixture) runToLive(t *testing.T, matchPublicID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.svc.OpenHandicap(ctx, OpenHandicapParams{MatchPublicID: matchPublicID, ActorUserID: judgeID}); err != nil {
		t.Fatalf("開盤: %v", err)
	}
	if _, err := f.svc.LockHandicap(ctx, LockHandicapParams{MatchPublicID: matchPublicID, Confirm: true, ActorUserID: judgeID}); err != nil {
		t.Fatalf("封盤: %v", err)
	}
	if _, err := f.svc.StartMatch(ctx, StartMatchParams{MatchPublicID: matchPublicID, ActorUserID: judgeID}); err != nil {
		t.Fatalf("開打: %v", err)
	}
}

// ── 開盤 ────────────────────────────────────────────────────────

func TestOpenHandicap(t *testing.T) {
	f := fourPlayers(t)
	res, err := f.svc.OpenHandicap(context.Background(), OpenHandicapParams{
		MatchPublicID: "M-R1S0", ActorUserID: judgeID, Reason: "雙方確認時段",
	})
	if err != nil {
		t.Fatalf("開盤失敗: %v", err)
	}
	if res.Match.Status != StatusReady || !res.Match.HandicapOpen {
		t.Fatalf("狀態應為 ready + handicap_open,得到 %s / %v", res.Match.Status, res.Match.HandicapOpen)
	}
	if res.Budget == nil || res.Budget.Budget != 16 {
		t.Fatalf("應發 16 BP,得到 %+v", res.Budget)
	}

	ev := f.lastEvent(t)
	if ev.Topic != TopicHandicapOpened {
		t.Fatalf("topic = %s", ev.Topic)
	}
	p := decodeMatchEvent(t, ev)
	// 公告要顯示的東西都必須在 payload 裡 —— renderer 不該再查 DB。
	switch {
	case p.TournamentSlug != "shifeng-1" || p.TournamentName == "":
		t.Fatalf("缺賽事資訊: %+v", p)
	case p.RoundLabel != "四強":
		t.Fatalf("輪次名稱 = %q", p.RoundLabel)
	case p.P1 == nil || p.P1.DisplayName != "阿甲" || p.P1.RankName != "開山":
		t.Fatalf("P1 顯示資料不全: %+v", p.P1)
	case p.P2 == nil || p.P2.DisplayName != "阿乙" || p.P2.RankName != "飛花":
		t.Fatalf("P2 顯示資料不全: %+v", p.P2)
	case p.Handicap == nil:
		t.Fatal("開盤提醒必須帶 BP 數,否則提醒不出「你有 N BP 還沒花」")
	case p.Handicap.Remaining != 16 || p.Handicap.HolderDisplayName != "阿甲":
		t.Fatalf("讓武區塊不正確: %+v", p.Handicap)
	case p.Handicap.ConstrainedDisplayName != "阿乙":
		t.Fatalf("受限方應為阿乙: %+v", p.Handicap)
	}
}

func TestOpenHandicapSameRankHasNoBudget(t *testing.T) {
	f := fourPlayers(t) // R1S1 是斷水 vs 斷水,沒有預算列
	res, err := f.svc.OpenHandicap(context.Background(), OpenHandicapParams{
		MatchPublicID: "M-R1S1", ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("開盤失敗: %v", err)
	}
	if res.Budget != nil {
		t.Fatalf("同段對決不該有預算: %+v", res.Budget)
	}
	if p := decodeMatchEvent(t, f.lastEvent(t)); p.Handicap != nil {
		t.Fatalf("同段對決的公告不該出現讓武區塊: %+v", p.Handicap)
	}
}

func TestOpenHandicapGates(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		setup   func(f *fixture)
		params  OpenHandicapParams
		wantErr error
	}{
		{
			name:    "缺裁判身分",
			params:  OpenHandicapParams{MatchPublicID: "M-R1S0"},
			wantErr: tournament.ErrActorRequired,
		},
		{
			name:    "缺場次",
			params:  OpenHandicapParams{ActorUserID: judgeID},
			wantErr: ErrInvalidRequest,
		},
		{
			name:    "場次不存在",
			params:  OpenHandicapParams{MatchPublicID: "M-NOPE", ActorUserID: judgeID},
			wantErr: activityerr.ErrMatchNotFound,
		},
		{
			name:    "雙方未確定",
			params:  OpenHandicapParams{MatchPublicID: "M-FINAL", ActorUserID: judgeID},
			wantErr: activityerr.ErrPlayersNotSet,
		},
		{
			name:    "重複開盤",
			setup:   func(f *fixture) { f.db.matches[10].status = StatusReady },
			params:  OpenHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID},
			wantErr: ErrAlreadyOpen,
		},
		{
			name:    "已完賽",
			setup:   func(f *fixture) { f.db.matches[10].status = StatusDone },
			params:  OpenHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID},
			wantErr: ErrMatchFinished,
		},
		{
			name:    "一方已棄賽",
			setup:   func(f *fixture) { f.db.players[2].status = tournament.PlayerWithdrawn },
			params:  OpenHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID},
			wantErr: ErrPlayerWithdrawn,
		},
		{
			name:    "賽事還在報名期",
			setup:   func(f *fixture) { f.db.tournaments[1].Phase = tournament.PhaseSignup },
			params:  OpenHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID},
			wantErr: tournament.ErrWrongPhase,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fourPlayers(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			_, err := f.svc.OpenHandicap(ctx, tc.params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v,期望 %v", err, tc.wantErr)
			}
			if len(f.db.events) != 0 {
				t.Fatalf("被擋下的動作不該發事件: %+v", f.db.events)
			}
		})
	}
}

// ── 封盤 ────────────────────────────────────────────────────────

func TestLockHandicapPublishesFullList(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.selections[10] = []handicap.Selection{
		{ItemRef: "I-1", ItemName: "指定對手武學", Category: handicap.CategoryWeapon, Cost: 20, TargetNote: "松濤劍法"},
		{ItemRef: "I-2", ItemName: "禁跳躍", Category: handicap.CategoryDefense, Cost: 2},
	}
	f.db.budgets[10].Spent = 22

	if _, err := f.svc.OpenHandicap(ctx, OpenHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID}); err != nil {
		t.Fatalf("開盤: %v", err)
	}
	res, err := f.svc.LockHandicap(ctx, LockHandicapParams{
		MatchPublicID: "M-R1S0", Confirm: true, ActorUserID: judgeID, Reason: "時間到",
	})
	if err != nil {
		t.Fatalf("封盤失敗: %v", err)
	}
	if res.Match.Status != StatusLocked || res.Match.HandicapLockedAt == nil {
		t.Fatalf("封盤後應為 locked + 有時間戳: %+v", res.Match)
	}

	ev := f.lastEvent(t)
	if ev.Topic != TopicHandicapLocked {
		t.Fatalf("topic = %s", ev.Topic)
	}
	p := decodeMatchEvent(t, ev)
	if p.Handicap == nil || len(p.Handicap.Items) != 2 {
		t.Fatalf("封盤公示必須帶完整讓武清單: %+v", p.Handicap)
	}
	first := p.Handicap.Items[0]
	switch {
	case first.Name != "指定對手武學" || first.Cost != 20 || first.TargetNote != "松濤劍法":
		t.Fatalf("項目資料不全(公告靠它排版): %+v", first)
	case first.Category != handicap.CategoryWeapon:
		t.Fatalf("缺分類: %+v", first)
	case p.Handicap.Budget != 16 || p.Handicap.Spent != 22:
		t.Fatalf("預算數字不對: %+v", p.Handicap)
	case p.Handicap.HolderDisplayName != "阿甲" || p.Handicap.ConstrainedDisplayName != "阿乙":
		t.Fatalf("雙方顯示名不全: %+v", p.Handicap)
	}

	// 稽核:封盤的資料變更在 handicap 套件,所以這一筆必須由本套件補上,
	// 而且 after 要記下公示出去的那份清單。
	var locked *JudgeAction
	for i := range f.db.audits {
		if f.db.audits[i].Action == ActionLockHandicap {
			locked = &f.db.audits[i]
		}
	}
	if locked == nil {
		t.Fatal("封盤沒有寫稽核紀錄")
	}
	var after lockAudit
	if err := json.Unmarshal(locked.After, &after); err != nil {
		t.Fatalf("解析稽核 after: %v", err)
	}
	if after.Handicap == nil || len(after.Handicap.Items) != 2 {
		t.Fatalf("稽核紀錄必須含封盤清單: %s", locked.After)
	}
}

func TestLockHandicapGates(t *testing.T) {
	ctx := context.Background()
	t.Run("沒有二次確認", func(t *testing.T) {
		f := fourPlayers(t)
		f.db.matches[10].status = StatusReady
		_, err := f.svc.LockHandicap(ctx, LockHandicapParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID})
		if !errors.Is(err, ErrConfirmationRequired) {
			t.Fatalf("err = %v", err)
		}
		if f.db.matches[10].status != StatusReady {
			t.Fatal("被擋下來卻改了狀態")
		}
	})
	t.Run("還沒開盤", func(t *testing.T) {
		f := fourPlayers(t)
		_, err := f.svc.LockHandicap(ctx, LockHandicapParams{MatchPublicID: "M-R1S0", Confirm: true, ActorUserID: judgeID})
		if !errors.Is(err, ErrNotReady) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("重複封盤", func(t *testing.T) {
		f := fourPlayers(t)
		f.db.matches[10].status = StatusLocked
		_, err := f.svc.LockHandicap(ctx, LockHandicapParams{MatchPublicID: "M-R1S0", Confirm: true, ActorUserID: judgeID})
		if !errors.Is(err, handicap.ErrAlreadyLocked) {
			t.Fatalf("err = %v", err)
		}
		if len(f.db.events) != 0 {
			t.Fatal("重複封盤不該再公告一次")
		}
	})
}

// ── 開打 ────────────────────────────────────────────────────────

func TestStartMatch(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.matches[10].status = StatusLocked

	m, err := f.svc.StartMatch(ctx, StartMatchParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID})
	if err != nil {
		t.Fatalf("開打失敗: %v", err)
	}
	if m.Status != StatusLive || m.StartedAt == nil {
		t.Fatalf("應為 live + started_at: %+v", m)
	}
	if ev := f.lastEvent(t); ev.Topic != TopicMatchStarted {
		t.Fatalf("topic = %s", ev.Topic)
	}
}

func TestStartMatchRequiresLocked(t *testing.T) {
	ctx := context.Background()
	for _, st := range []Status{StatusPending, StatusReady, StatusLive} {
		f := fourPlayers(t)
		f.db.matches[10].status = st
		_, err := f.svc.StartMatch(ctx, StartMatchParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID})
		if !errors.Is(err, ErrNotLocked) {
			t.Fatalf("status=%s 時 err = %v,期望 ErrNotLocked", st, err)
		}
	}
}

// ── 判定勝負 ────────────────────────────────────────────────────

func TestReportResult(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.openBets["M-R1S1"] = 5
	f.runToLive(t, "M-R1S1") // 阿丙(3) vs 阿丁(4),slot 1

	out, err := f.svc.ReportResult(ctx, ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-C",
		Confirm: true, ActorUserID: judgeID, Reason: "3:1",
	})
	if err != nil {
		t.Fatalf("判定失敗: %v", err)
	}

	switch {
	case out.Match.Status != StatusDone || out.Match.WinnerPlayerID != 3:
		t.Fatalf("場次未正確結束: %+v", out.Match)
	case out.Match.ResultKind != ResultNormal || out.Match.FinishedAt == nil:
		t.Fatalf("result_kind / finished_at 不正確: %+v", out.Match)
	case out.SettledBetCount != 5:
		t.Fatalf("結算注單數 = %d,期望 5", out.SettledBetCount)
	case f.db.players[4].status != tournament.PlayerEliminated:
		t.Fatalf("敗方應標 eliminated,得到 %s", f.db.players[4].status)
	case f.db.players[3].status != tournament.PlayerActive:
		t.Fatalf("勝方狀態不該變:%s", f.db.players[3].status)
	case f.db.fencers[1003].wins != 1 || f.db.fencers[1003].losses != 0:
		t.Fatalf("勝方戰績 = %+v", f.db.fencers[1003])
	case f.db.fencers[1004].wins != 0 || f.db.fencers[1004].losses != 1:
		t.Fatalf("敗方戰績 = %+v", f.db.fencers[1004])
	}

	// 晉級鏈:R1 slot1 的勝者進決賽的 **P2**(bracket.Advance 的映射)。
	if len(out.AdvancedMatches) != 1 || out.AdvancedMatches[0].PublicID != "M-FINAL" {
		t.Fatalf("晉級場次 = %+v", out.AdvancedMatches)
	}
	if f.db.matches[12].p2ID != 3 || f.db.matches[12].p1ID != 0 {
		t.Fatalf("應晉級到決賽 P2:p1=%d p2=%d", f.db.matches[12].p1ID, f.db.matches[12].p2ID)
	}
	// 開盤是裁判的另一個動作,不自動開。
	if f.db.matches[12].status != StatusPending || f.db.matches[12].handicapOpen {
		t.Fatalf("下一場不該被自動開盤: %+v", f.db.matches[12])
	}
	if out.ChampionPlayerPublicID != "" {
		t.Fatalf("這不是決賽,不該有冠軍: %s", out.ChampionPlayerPublicID)
	}

	// 結算必須發生在寫完 matches 之後(fake 會在順序錯時直接失敗,這裡再確認一次順序)。
	assertCallOrder(t, f.db.calls, "MarkFinished(M-R1S1,winner=3,kind=normal)", "SettleMatch(M-R1S1)")

	ev := f.eventsByTopic(TopicMatchFinished)
	if len(ev) != 1 {
		t.Fatalf("賽果事件 = %d 則", len(ev))
	}
	p := decodeMatchEvent(t, ev[0])
	switch {
	case p.Result == nil || p.Result.Winner == nil || p.Result.Winner.DisplayName != "阿丙":
		t.Fatalf("賽果缺勝者顯示名: %+v", p.Result)
	case p.Result.Loser == nil || p.Result.Loser.DisplayName != "阿丁":
		t.Fatalf("賽果缺敗者: %+v", p.Result)
	case p.Result.Kind != ResultNormal:
		t.Fatalf("kind = %s", p.Result.Kind)
	case p.Result.NextMatchPublicID != "M-FINAL" || p.Result.NextRoundLabel != "決賽":
		t.Fatalf("賽果要帶晉級去向: %+v", p.Result)
	case p.Result.SettledBetCount != 5:
		t.Fatalf("賽果要帶結算注單數: %+v", p.Result)
	}
}

func TestReportResultFinalMakesChampion(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.matches[12].p1ID, f.db.matches[12].p2ID = 1, 3
	f.runToLive(t, "M-FINAL")

	out, err := f.svc.ReportResult(ctx, ReportResultParams{
		MatchPublicID: "M-FINAL", WinnerPlayerPublicID: "P-A", Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("判定失敗: %v", err)
	}
	if out.ChampionPlayerPublicID != "P-A" {
		t.Fatalf("冠軍 = %q", out.ChampionPlayerPublicID)
	}
	if len(out.AdvancedMatches) != 0 {
		t.Fatalf("決賽沒有下一場: %+v", out.AdvancedMatches)
	}

	evs := f.eventsByTopic(TopicChampion)
	if len(evs) != 1 {
		t.Fatalf("冠軍事件 = %d 則", len(evs))
	}
	var c ChampionEvent
	if err := json.Unmarshal(evs[0].Payload, &c); err != nil {
		t.Fatalf("解析冠軍 payload: %v", err)
	}
	switch {
	case c.Champion.DisplayName != "阿甲" || c.Champion.RankName != "開山":
		t.Fatalf("冠軍資料不全: %+v", c.Champion)
	case c.RunnerUp == nil || c.RunnerUp.DisplayName != "阿丙":
		t.Fatalf("缺亞軍: %+v", c.RunnerUp)
	case c.TournamentSlug != "shifeng-1" || c.FinalMatchID != "M-FINAL":
		t.Fatalf("缺賽事資訊: %+v", c)
	}
}

func TestReportResultGates(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		setup   func(f *fixture)
		params  ReportResultParams
		wantErr error
	}{
		{
			name:    "沒有二次確認",
			params:  ReportResultParams{MatchPublicID: "M-R1S0", WinnerPlayerPublicID: "P-A", ActorUserID: judgeID},
			wantErr: ErrConfirmationRequired,
		},
		{
			name:    "缺裁判身分",
			params:  ReportResultParams{MatchPublicID: "M-R1S0", WinnerPlayerPublicID: "P-A", Confirm: true},
			wantErr: tournament.ErrActorRequired,
		},
		{
			name:    "還沒開打",
			setup:   func(f *fixture) { f.db.matches[10].status = StatusLocked },
			params:  ReportResultParams{MatchPublicID: "M-R1S0", WinnerPlayerPublicID: "P-A", Confirm: true, ActorUserID: judgeID},
			wantErr: ErrNotLive,
		},
		{
			name:    "已經判過",
			setup:   func(f *fixture) { f.db.matches[10].status = StatusDone },
			params:  ReportResultParams{MatchPublicID: "M-R1S0", WinnerPlayerPublicID: "P-A", Confirm: true, ActorUserID: judgeID},
			wantErr: ErrMatchFinished,
		},
		{
			name:    "勝者不是本場選手",
			setup:   func(f *fixture) { f.db.matches[10].status = StatusLive },
			params:  ReportResultParams{MatchPublicID: "M-R1S0", WinnerPlayerPublicID: "P-C", Confirm: true, ActorUserID: judgeID},
			wantErr: ErrWinnerNotInMatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fourPlayers(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			f.db.events = nil
			_, err := f.svc.ReportResult(ctx, tc.params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v,期望 %v", err, tc.wantErr)
			}
			if len(f.db.events) != 0 {
				t.Fatalf("被擋下的動作不該發事件: %+v", f.db.events)
			}
			if f.db.fencers[1001].wins != 0 {
				t.Fatal("被擋下的動作不該動戰績")
			}
		})
	}
}

// 對手在上一輪之前就棄賽、又坐在下一場的位子上時,晉級鏈要把那一場一併判掉,
// 否則它永遠停在 pending —— 裁判沒有任何動作推得動它(棄賽已經發生過了)。
func TestReportResultCascadesIntoWithdrawnOpponent(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.matches[12].p1ID = 1 // 阿甲輪空直接進決賽
	f.db.players[1].status = tournament.PlayerWithdrawn
	f.db.openBets["M-R1S1"] = 2
	f.db.openBets["M-FINAL"] = 3
	f.runToLive(t, "M-R1S1")

	out, err := f.svc.ReportResult(ctx, ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-C", Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("判定失敗: %v", err)
	}
	if len(out.AdvancedMatches) != 1 || out.AdvancedMatches[0].PublicID != "M-FINAL" {
		t.Fatalf("晉級場次 = %+v", out.AdvancedMatches)
	}
	if out.AdvancedMatches[0].Status != StatusDone || out.AdvancedMatches[0].ResultKind != ResultWalkover {
		t.Fatalf("決賽應被判不戰而勝: %+v", out.AdvancedMatches[0])
	}
	switch {
	case out.SettledBetCount != 2:
		t.Fatalf("結算注單數 = %d", out.SettledBetCount)
	case out.VoidedBetCount != 3:
		t.Fatalf("退款注單數 = %d(不戰而勝要全額退)", out.VoidedBetCount)
	case out.ChampionPlayerPublicID != "P-C":
		t.Fatalf("冠軍 = %q", out.ChampionPlayerPublicID)
	case f.db.players[1].status != tournament.PlayerWithdrawn:
		t.Fatalf("棄賽者不該被覆寫成 eliminated:%s", f.db.players[1].status)
	case f.db.fencers[1001].losses != 1:
		t.Fatalf("棄賽者的 losses 應 +1,得到 %+v", f.db.fencers[1001])
	case f.db.fencers[1003].wins != 2:
		t.Fatalf("勝者兩勝: %+v", f.db.fencers[1003])
	}
	if got := len(f.eventsByTopic(TopicMatchFinished)); got != 2 {
		t.Fatalf("賽果事件 = %d 則,期望 2", got)
	}
}

// ── 棄賽 ────────────────────────────────────────────────────────

func TestWithdrawPlayer(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.matches[10].status = StatusLocked
	f.db.openBets["M-R1S0"] = 4

	out, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{
		PlayerPublicID: "P-A", Confirm: true, ActorUserID: judgeID, Reason: "臨時有事",
	})
	if err != nil {
		t.Fatalf("棄賽失敗: %v", err)
	}
	switch {
	case out.Player.Status != tournament.PlayerWithdrawn:
		t.Fatalf("選手狀態 = %s", out.Player.Status)
	case len(out.WalkoverMatches) != 1 || out.WalkoverMatches[0].PublicID != "M-R1S0":
		t.Fatalf("不戰而勝場次 = %+v", out.WalkoverMatches)
	case out.WalkoverMatches[0].ResultKind != ResultWalkover:
		t.Fatalf("result_kind = %s", out.WalkoverMatches[0].ResultKind)
	case out.WalkoverMatches[0].WinnerPlayerID != 2:
		t.Fatalf("勝者應為對手,得到 %d", out.WalkoverMatches[0].WinnerPlayerID)
	case out.VoidedBetCount != 4:
		t.Fatalf("退款注單數 = %d", out.VoidedBetCount)
	// 棄賽者的 losses 要 +1(schemas/26 待確認 ④:戰績誠實反映結果)。
	case f.db.fencers[1001].losses != 1:
		t.Fatalf("棄賽者戰績 = %+v", f.db.fencers[1001])
	case f.db.fencers[1002].wins != 1:
		t.Fatalf("對手應記一勝: %+v", f.db.fencers[1002])
	}
	// 對手照常晉級:R1 slot0 的勝者進決賽 P1。
	if f.db.matches[12].p1ID != 2 {
		t.Fatalf("對手未晉級:決賽 p1 = %d", f.db.matches[12].p1ID)
	}
	if len(out.AdvancedMatches) != 1 || out.AdvancedMatches[0].PublicID != "M-FINAL" {
		t.Fatalf("晉級場次 = %+v", out.AdvancedMatches)
	}

	p := decodeMatchEvent(t, f.lastEvent(t))
	if p.Result == nil || p.Result.Kind != ResultWalkover || p.Result.VoidedBetCount != 4 {
		t.Fatalf("棄賽公告內容不正確: %+v", p.Result)
	}
	assertCallOrder(t, f.db.calls,
		"SetPlayerStatus(P-A,withdrawn)",
		"LockUnfinishedMatchesOfPlayer(1)",
		"MarkFinished(M-R1S0,winner=2,kind=walkover)",
		"VoidMatch(M-R1S0)")
}

// 一次棄賽可能牽動不只一場:本輪判給對手之後,對手晉級的那一場如果
// 另一邊早就棄賽了,同一個 tx 裡要一起判掉。
func TestWithdrawPlayerCascadesMultipleMatches(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 3) // 8 人樹
	f.addPlayer(1, "P-W", "退賽者", bp.RankDuanshui)
	f.addPlayer(2, "P-Y", "對手", bp.RankDuanshui)
	f.addPlayer(3, "P-Z", "早退者", bp.RankDuanshui)
	f.db.players[3].status = tournament.PlayerWithdrawn
	f.addMatch(20, "M-R2S0", 2, 0, 1, 2, StatusLocked) // W vs Y
	f.addMatch(21, "M-R3S0", 3, 0, 0, 3, StatusPending)
	f.db.openBets["M-R2S0"] = 2
	f.db.openBets["M-R3S0"] = 1

	out, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{
		PlayerPublicID: "P-W", Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("棄賽失敗: %v", err)
	}
	if len(out.WalkoverMatches) != 2 {
		t.Fatalf("應一起判掉兩場: %+v", out.WalkoverMatches)
	}
	if out.VoidedBetCount != 3 {
		t.Fatalf("兩場的注單都要退: %d", out.VoidedBetCount)
	}
	if out.ChampionPlayerPublicID != "P-Y" {
		t.Fatalf("冠軍 = %q", out.ChampionPlayerPublicID)
	}
	if f.db.fencers[1002].wins != 2 {
		t.Fatalf("對手應記兩勝: %+v", f.db.fencers[1002])
	}
	if len(f.eventsByTopic(TopicChampion)) != 1 {
		t.Fatal("缺冠軍事件")
	}
}

// 對手還沒產生的場次判不了(沒有勝者可寫,DB 的 CHECK 也不允許),
// 留給晉級鏈處理。
func TestWithdrawPlayerSkipsMatchWithoutOpponent(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.matches[12].p1ID = 1 // 阿甲輪空進決賽,對手待定
	f.db.matches[10].p1ID = 0 // 他沒有首輪

	out, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{
		PlayerPublicID: "P-A", Confirm: true, ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("棄賽失敗: %v", err)
	}
	if len(out.WalkoverMatches) != 0 {
		t.Fatalf("不該判任何場次: %+v", out.WalkoverMatches)
	}
	if f.db.matches[12].status != StatusPending {
		t.Fatalf("決賽狀態 = %s", f.db.matches[12].status)
	}
	if f.db.fencers[1001].losses != 0 {
		t.Fatal("沒有場次被判,不該記敗場")
	}
}

func TestWithdrawPlayerGates(t *testing.T) {
	ctx := context.Background()
	t.Run("沒有二次確認", func(t *testing.T) {
		f := fourPlayers(t)
		_, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{PlayerPublicID: "P-A", ActorUserID: judgeID})
		if !errors.Is(err, ErrConfirmationRequired) {
			t.Fatalf("err = %v", err)
		}
		if f.db.players[1].status != tournament.PlayerActive {
			t.Fatal("被擋下來卻改了狀態")
		}
	})
	t.Run("重複棄賽", func(t *testing.T) {
		f := fourPlayers(t)
		f.db.players[1].status = tournament.PlayerWithdrawn
		_, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{PlayerPublicID: "P-A", Confirm: true, ActorUserID: judgeID})
		if !errors.Is(err, ErrAlreadyWithdrawn) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("查無此人", func(t *testing.T) {
		f := fourPlayers(t)
		_, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{PlayerPublicID: "P-X", Confirm: true, ActorUserID: judgeID})
		if !errors.Is(err, tournament.ErrPlayerNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("缺裁判身分", func(t *testing.T) {
		f := fourPlayers(t)
		_, err := f.svc.WithdrawPlayer(ctx, WithdrawPlayerParams{PlayerPublicID: "P-A", Confirm: true})
		if !errors.Is(err, tournament.ErrActorRequired) {
			t.Fatalf("err = %v", err)
		}
	})
}

// ── 直播連結 ────────────────────────────────────────────────────

func TestSetStreamURL(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)

	m, err := f.svc.SetStreamURL(ctx, SetStreamURLParams{
		MatchPublicID: "M-R1S0", StreamURL: "https://twitch.tv/x", ActorUserID: judgeID,
	})
	if err != nil {
		t.Fatalf("設定失敗: %v", err)
	}
	if m.StreamURL != "https://twitch.tv/x" {
		t.Fatalf("連結 = %q", m.StreamURL)
	}
	// 空字串 = 清掉。
	m, err = f.svc.SetStreamURL(ctx, SetStreamURLParams{MatchPublicID: "M-R1S0", ActorUserID: judgeID})
	if err != nil {
		t.Fatalf("清除失敗: %v", err)
	}
	if m.StreamURL != "" {
		t.Fatalf("應清空,得到 %q", m.StreamURL)
	}
	// 直播連結會改來改去,不該每改一次就廣播一次。
	if len(f.db.events) != 0 {
		t.Fatalf("設直播連結不該發公告: %+v", f.db.events)
	}
	if len(f.db.audits) != 2 {
		t.Fatalf("兩次設定都要進稽核,得到 %d 筆", len(f.db.audits))
	}
}

// ── 原子性 ──────────────────────────────────────────────────────

// 事件寫入失敗 = 整筆 rollback。領域變更與 outbox 事件同一個 transaction
// 是平台鐵則 6,而它只有在「其中一半失敗」時才驗得到。
func TestEventFailureRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.db.openBets["M-R1S1"] = 3
	f.runToLive(t, "M-R1S1")
	f.db.failAppendEvents = true

	_, err := f.svc.ReportResult(ctx, ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-C", Confirm: true, ActorUserID: judgeID,
	})
	if err == nil {
		t.Fatal("應該失敗")
	}
	switch {
	case f.db.matches[11].status != StatusLive:
		t.Fatalf("場次狀態應還原: %s", f.db.matches[11].status)
	case f.db.matches[12].p2ID != 0:
		t.Fatal("晉級應一併還原")
	case f.db.players[4].status != tournament.PlayerActive:
		t.Fatal("淘汰標記應還原")
	case f.db.fencers[1003].wins != 0:
		t.Fatal("戰績應還原")
	case f.db.openBets["M-R1S1"] != 3:
		t.Fatal("結算應一併還原")
	}
}

// 結算失敗(例如注單與帳本狀態矛盾)時,勝負判定也不能留下來。
func TestSettlementFailureRollsBackResult(t *testing.T) {
	ctx := context.Background()
	f := fourPlayers(t)
	f.runToLive(t, "M-R1S1")
	// 注單與帳本狀態矛盾:betting 必須失敗出聲,不能回成功但錢不對。
	f.db.failSettle = true

	_, err := f.svc.ReportResult(ctx, ReportResultParams{
		MatchPublicID: "M-R1S1", WinnerPlayerPublicID: "P-C", Confirm: true, ActorUserID: judgeID,
	})
	if err == nil {
		t.Fatal("應該失敗")
	}
	if f.db.matches[11].status != StatusLive || f.db.matches[11].winnerID != 0 {
		t.Fatalf("判定應被還原: %+v", f.db.matches[11])
	}
}

// assertCallOrder 斷言 want 裡的呼叫依序出現在 calls 中(可以不連續)。
func assertCallOrder(t *testing.T, calls []string, want ...string) {
	t.Helper()
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("呼叫順序不符,卡在 %q\n實際: %v", want[i], calls)
	}
}
