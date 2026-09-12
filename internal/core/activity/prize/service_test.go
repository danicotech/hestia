package prize

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// 三種獎金都有的設定。季軍刻意留 0 —— 設了就發不出去(見 TestAwardPrizes季軍推不出來)。
const configFull = `{"prizes":{"champion":1000,"runner_up":500,"participation":100}}`

func awardOf(t *testing.T, res *AwardResult, publicID string, kind Kind) Award {
	t.Helper()
	for _, a := range res.Awards {
		if a.PlayerPublicID == publicID && a.Kind == kind {
			return a
		}
	}
	t.Fatalf("找不到 %s 的 %s 獎", publicID, kind)
	return Award{}
}

func TestAwardPrizes冠亞與參賽獎各自入帳(t *testing.T) {
	ps := fourPlayers()
	s := newScene(configFull, ps, fourMatches(ps))

	res, err := s.award(context.Background())
	if err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}
	if res.Replayed {
		t.Error("第一次發獎不該標成重放")
	}
	// 冠軍 1 + 亞軍 1 + 參賽 4 = 6 筆
	if len(res.Awards) != 6 {
		t.Fatalf("獎項筆數 = %d,想要 6:%+v", len(res.Awards), res.Awards)
	}
	for _, a := range res.Awards {
		if !a.Granted || a.EntryID == 0 || a.Replayed {
			t.Errorf("%s/%s 應已入帳且非重放:%+v", a.PlayerPublicID, a.Kind, a)
		}
	}
	// 冠軍拿冠軍獎 + 參賽獎,兩筆分錄而不是一筆淨額 ——
	// 「他為什麼拿到 1100」在帳本上要分得出來。
	if got := s.led.balance(101); got != 1100 {
		t.Errorf("冠軍餘額 = %d,想要 1100", got)
	}
	if got := s.led.balance(102); got != 600 {
		t.Errorf("亞軍餘額 = %d,想要 600", got)
	}
	if got := s.led.balance(103); got != 100 {
		t.Errorf("首輪淘汰者餘額 = %d,想要 100", got)
	}
	if n := s.led.entryCount(); n != 6 {
		t.Errorf("分錄數 = %d,想要 6", n)
	}
	// 每一筆都要指得出事由、收款對象與是誰發的。
	for _, op := range s.led.entries {
		if op.Reason != ledger.ReasonTournamentPrize {
			t.Errorf("reason = %q,想要 tournament_prize", op.Reason)
		}
		if op.Amount <= 0 {
			t.Errorf("獎金分錄必為正數,得到 %d", op.Amount)
		}
		if op.ActorID == nil || *op.ActorID != judgeUserID {
			t.Errorf("actor_id 應為發獎的裁判 %d", judgeUserID)
		}
		if op.RefType == nil || *op.RefType != refTypePlayer || op.RefID == nil {
			t.Errorf("分錄應指回 tournament_players 的那一列:%+v", op)
		}
	}
}

func TestAwardPrizes未綁定者不發且被點名(t *testing.T) {
	ps := fourPlayers()
	ps[0].UserID = nil // 冠軍沒綁平台帳號
	s := newScene(configFull, ps, fourMatches(ps))

	res, err := s.award(context.Background())
	if err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}

	champ := awardOf(t, res, ps[0].PublicID, KindChampion)
	if champ.Granted {
		t.Error("未綁定帳號者不該發成")
	}
	if champ.SkippedReason != UnboundReason {
		t.Errorf("skipped_reason = %q,想要 %q", champ.SkippedReason, UnboundReason)
	}
	if champ.DisplayName != "甲" || champ.PlayerPublicID == "" {
		t.Errorf("沒發成的那筆必須點得出是誰:%+v", champ)
	}
	if champ.Amount != 1000 {
		t.Errorf("沒發成也要顯示金額,得到 %d", champ.Amount)
	}
	// 他的參賽獎同樣發不出去,而別人的照發。
	if part := awardOf(t, res, ps[0].PublicID, KindParticipation); part.Granted {
		t.Error("未綁定者的參賽獎也不該發成")
	}
	if got := s.led.balance(102); got != 600 {
		t.Errorf("亞軍不該被別人的綁定狀態影響,餘額 = %d", got)
	}
	if n := s.led.entryCount(); n != 4 {
		t.Errorf("分錄數 = %d,想要 4(六筆扣掉冠軍的兩筆)", n)
	}
}

func TestAwardPrizes重複呼叫不重複發錢(t *testing.T) {
	ctx := context.Background()
	ps := fourPlayers()
	s := newScene(configFull, ps, fourMatches(ps))

	if _, err := s.award(ctx); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	before := s.led.balance(101)
	entries := s.led.entryCount()

	res, err := s.award(ctx)
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if !res.Replayed {
		t.Error("整批都已發過,Replayed 應為 true")
	}
	for _, a := range res.Awards {
		if !a.Granted || !a.Replayed {
			t.Errorf("%s/%s 第二次應是回放:%+v", a.PlayerPublicID, a.Kind, a)
		}
	}
	if got := s.led.balance(101); got != before {
		t.Errorf("餘額變了:%d → %d", before, got)
	}
	if n := s.led.entryCount(); n != entries {
		t.Errorf("分錄數變了:%d → %d", entries, n)
	}
}

func TestAwardPrizes沒設獎金時不產生分錄(t *testing.T) {
	ps := fourPlayers()
	for _, cfg := range []string{"", "{}", `{"prizes":{"champion":0,"runner_up":0,"third":0,"participation":0}}`} {
		s := newScene(cfg, ps, fourMatches(ps))
		res, err := s.award(context.Background())
		if err != nil {
			t.Fatalf("config=%q: %v", cfg, err)
		}
		if len(res.Awards) != 0 {
			t.Errorf("config=%q 不該有獎項:%+v", cfg, res.Awards)
		}
		if res.Replayed {
			t.Errorf("config=%q 沒事可做不算重放", cfg)
		}
		if n := s.led.callCount(); n != 0 {
			t.Errorf("config=%q 呼叫了帳本 %d 次,想要 0(帳本禁止 0 元分錄)", cfg, n)
		}
	}
}

func TestAwardPrizes棄賽者仍拿參賽獎(t *testing.T) {
	ps := fourPlayers()
	ps[3].Status = tournament.PlayerWithdrawn
	s := newScene(configFull, ps, fourMatches(ps))

	res, err := s.award(context.Background())
	if err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}
	a := awardOf(t, res, ps[3].PublicID, KindParticipation)
	if !a.Granted || a.Amount != 100 {
		t.Errorf("棄賽者的參賽獎:%+v", a)
	}
	if got := s.led.balance(104); got != 100 {
		t.Errorf("棄賽者餘額 = %d,想要 100", got)
	}
}

func TestAwardPrizes季軍推不出來(t *testing.T) {
	ps := fourPlayers()
	s := newScene(`{"prizes":{"champion":1000,"third":300}}`, ps, fourMatches(ps))

	_, err := s.award(context.Background())
	if !errors.Is(err, ErrThirdPlaceUndecidable) {
		t.Fatalf("err = %v,想要 ErrThirdPlaceUndecidable", err)
	}
	// 算不出來就一毛都不發:發一半再報錯,裁判得先去帳本查哪些人已經拿到了。
	if n := s.led.callCount(); n != 0 {
		t.Errorf("失敗前不該動錢,帳本被呼叫 %d 次", n)
	}
}

func TestAwardPrizes決賽未分勝負(t *testing.T) {
	ps := fourPlayers()
	ms := fourMatches(ps)
	ms[2].Status = match.StatusLive
	ms[2].WinnerPlayerID = 0
	s := newScene(configFull, ps, ms)

	_, err := s.award(context.Background())
	if !errors.Is(err, ErrFinalNotDecided) {
		t.Fatalf("err = %v,想要 ErrFinalNotDecided", err)
	}
	if n := s.led.callCount(); n != 0 {
		t.Errorf("失敗前不該動錢,帳本被呼叫 %d 次", n)
	}
}

// 只設參賽獎時根本不需要名次 —— 連對戰表都不該去讀。
func TestAwardPrizes只有參賽獎時不碰對戰表(t *testing.T) {
	ps := fourPlayers()
	s := newScene(`{"prizes":{"participation":100}}`, ps, nil)
	s.repo.failMatches = errors.New("不該讀對戰表")

	res, err := s.award(context.Background())
	if err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}
	if len(res.Awards) != 4 {
		t.Fatalf("獎項筆數 = %d,想要 4", len(res.Awards))
	}
}

func TestAwardPrizes尚未抽籤(t *testing.T) {
	ps := fourPlayers()
	s := newScene(configFull, ps, nil)

	_, err := s.award(context.Background())
	if !errors.Is(err, match.ErrBracketMissing) {
		t.Fatalf("err = %v,想要 match.ErrBracketMissing", err)
	}
}

func TestAwardPrizes對戰表異常(t *testing.T) {
	ps := fourPlayers()
	ms := fourMatches(ps)
	ms[2].WinnerPlayerID = 999 // 勝者不是場上兩人之一
	s := newScene(configFull, ps, ms)

	if _, err := s.award(context.Background()); !errors.Is(err, ErrBracketBroken) {
		t.Fatalf("err = %v,想要 ErrBracketBroken", err)
	}
}

// 決賽是不戰而勝時,敗者仍然是亞軍:他確實打到了決賽。
func TestAwardPrizes不戰而勝的決賽敗者仍是亞軍(t *testing.T) {
	ps := fourPlayers()
	ms := fourMatches(ps)
	ms[2].ResultKind = match.ResultWalkover
	ps[1].Status = tournament.PlayerWithdrawn
	s := newScene(configFull, ps, ms)

	res, err := s.award(context.Background())
	if err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}
	if a := awardOf(t, res, ps[1].PublicID, KindRunnerUp); !a.Granted || a.Amount != 500 {
		t.Errorf("亞軍獎:%+v", a)
	}
}

func TestAwardPrizes帳本回ErrInFlight時中斷且已發的不退(t *testing.T) {
	ctx := context.Background()
	ps := fourPlayers()
	s := newScene(configFull, ps, fourMatches(ps))
	runnerKey := AwardKey(tPublicID, ps[1].PublicID, KindRunnerUp)
	s.led.fail[runnerKey] = ledger.ErrInFlight

	_, err := s.award(ctx)
	if !errors.Is(err, ledger.ErrInFlight) {
		t.Fatalf("err = %v,想要 ledger.ErrInFlight", err)
	}
	if !strings.Contains(err.Error(), "乙") {
		t.Errorf("錯誤訊息要指得出卡在誰身上:%v", err)
	}
	// 帳本沒有 rollback 的概念:冠軍那筆已經 commit,而它本來就該留著。
	if got := s.led.balance(101); got != 1000 {
		t.Errorf("冠軍獎已入帳,餘額 = %d,想要 1000", got)
	}

	// 對方跑完之後重試:已發的回放,沒發的補上,誰都沒有拿兩次。
	delete(s.led.fail, runnerKey)
	res, err := s.award(ctx)
	if err != nil {
		t.Fatalf("重試: %v", err)
	}
	if res.Replayed {
		t.Error("這一次有人真的收到錢,不是純重放")
	}
	if got := s.led.balance(101); got != 1100 {
		t.Errorf("冠軍餘額 = %d,想要 1100(冠軍獎不該重發)", got)
	}
	if got := s.led.balance(102); got != 600 {
		t.Errorf("亞軍餘額 = %d,想要 600", got)
	}
	if a := awardOf(t, res, ps[0].PublicID, KindChampion); !a.Replayed {
		t.Error("冠軍獎這次應是回放")
	}
}

func TestAwardPrizes改了金額再發回冪等衝突(t *testing.T) {
	ctx := context.Background()
	ps := fourPlayers()
	s := newScene(configFull, ps, fourMatches(ps))
	if _, err := s.award(ctx); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	before := s.led.entryCount()

	// 裁判改了 config 的冠軍獎金再按一次。同一把自然鍵、不同的內容 ——
	// 帳本必須拒絕,而不是默默補上差額或再發一整筆。
	tt := s.repo.tournaments[slug]
	tt.ConfigRaw = []byte(`{"prizes":{"champion":1200,"runner_up":500,"participation":100}}`)
	s.repo.tournaments[slug] = tt

	_, err := s.award(ctx)
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("err = %v,想要 ledger.ErrIdempotencyConflict", err)
	}
	if n := s.led.entryCount(); n != before {
		t.Errorf("衝突不該留下新分錄:%d → %d", before, n)
	}
}

func TestAwardPrizes階段不對(t *testing.T) {
	ps := fourPlayers()
	for _, phase := range []tournament.Phase{
		tournament.PhaseSignup, tournament.PhaseRanked, tournament.PhaseDrawing,
	} {
		s := newScene(configFull, ps, fourMatches(ps))
		tt := s.repo.tournaments[slug]
		tt.Phase = phase
		s.repo.tournaments[slug] = tt

		_, err := s.award(context.Background())
		if !errors.Is(err, tournament.ErrWrongPhase) {
			t.Errorf("phase=%s 時 err = %v,想要 tournament.ErrWrongPhase", phase, err)
		}
		if n := s.led.callCount(); n != 0 {
			t.Errorf("phase=%s 不該動錢", phase)
		}
	}
}

func TestAwardPrizes設定不合法時拒發(t *testing.T) {
	ps := fourPlayers()
	// prizes.champion 是負數:ParseConfig 會退回 0 並回診斷錯誤。
	// 照那份設定發下去 = 安靜地不發冠軍獎,裁判只會看到「按了沒反應」。
	s := newScene(`{"prizes":{"champion":-100,"participation":100}}`, ps, fourMatches(ps))

	_, err := s.award(context.Background())
	if !errors.Is(err, tournament.ErrConfigMalformed) {
		t.Fatalf("err = %v,想要 tournament.ErrConfigMalformed", err)
	}
	if n := s.led.callCount(); n != 0 {
		t.Errorf("設定沒修好之前不該動錢,帳本被呼叫 %d 次", n)
	}
}

func TestAwardPrizes賽事不存在(t *testing.T) {
	s := newScene(configFull, fourPlayers(), nil)
	_, err := s.svc.AwardPrizes(context.Background(), AwardParams{
		TournamentSlug: "沒這屆", ActorUserID: judgeUserID,
	})
	if !errors.Is(err, tournament.ErrTournamentNotFound) {
		t.Fatalf("err = %v,想要 tournament.ErrTournamentNotFound", err)
	}
}

func TestAwardPrizes參數檢查(t *testing.T) {
	s := newScene(configFull, fourPlayers(), nil)
	cases := []struct {
		name string
		p    AwardParams
		want error
	}{
		{"缺 slug", AwardParams{ActorUserID: judgeUserID}, ErrInvalidRequest},
		{"只有空白的 slug", AwardParams{TournamentSlug: "  ", ActorUserID: judgeUserID}, ErrInvalidRequest},
		{"沒帶裁判", AwardParams{TournamentSlug: slug}, tournament.ErrActorRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.svc.AwardPrizes(context.Background(), c.p); !errors.Is(err, c.want) {
				t.Fatalf("err = %v,想要 %v", err, c.want)
			}
		})
	}
}

// 裁判連點:兩個請求同時進來,錢只能發一次。
//
// 這是本套件唯一真正要靠冪等擋住的競態,所以不 mock 掉時序 ——
// fakeLedger 的鍵表與真的 idempotency_keys 主鍵是同一個語意。
func TestAwardPrizes併發連點只發一次(t *testing.T) {
	ctx := context.Background()
	ps := fourPlayers()
	s := newScene(configFull, ps, fourMatches(ps))

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.award(ctx)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		// ErrInFlight 是合法的併發結果(對方佔了鍵還沒寫完),其餘都不該出現。
		if err != nil && !errors.Is(err, ledger.ErrInFlight) {
			t.Fatalf("第 %d 次: %v", i, err)
		}
	}
	if got := s.led.balance(101); got != 1100 {
		t.Errorf("冠軍餘額 = %d,想要 1100", got)
	}
	if got := s.led.balance(102); got != 600 {
		t.Errorf("亞軍餘額 = %d,想要 600", got)
	}
	if got := s.led.entryCount(); got != 6 {
		t.Errorf("分錄數 = %d,想要 6(連點不該產生多餘分錄)", got)
	}
}

func TestAwardKey含賽事與選手與獎項(t *testing.T) {
	a := AwardKey(tPublicID, "01PLAYERX", KindChampion)
	b := AwardKey(tPublicID, "01PLAYERX", KindParticipation)
	c := AwardKey("01OTHERTOURNAMENT", "01PLAYERX", KindChampion)
	d := AwardKey(tPublicID, "01PLAYERY", KindChampion)
	for _, other := range []string{b, c, d} {
		if a == other {
			t.Errorf("不同的獎應該有不同的鍵:%s == %s", a, other)
		}
	}
	if !strings.HasPrefix(a, keyPrefix) {
		t.Errorf("鍵少了命名空間前綴:%s", a)
	}
}

// 幣別留空時退回 betting 的那一份權威值,不是空字串(空字串會被帳本擋成 ErrInvalidOp)。
func TestNew幣別預設(t *testing.T) {
	ps := fourPlayers()
	repo := &fakeRepo{
		tournaments: map[string]tournament.Tournament{
			slug: {ID: 1, PublicID: tPublicID, Slug: slug, Phase: tournament.PhaseFinished,
				ConfigRaw: []byte(`{"prizes":{"participation":50}}`)},
		},
		players: map[int64][]tournament.Player{1: ps},
	}
	led := newFakeLedger()
	svc := New(repo, led, "")
	if _, err := svc.AwardPrizes(context.Background(), AwardParams{
		TournamentSlug: slug, ActorUserID: judgeUserID,
	}); err != nil {
		t.Fatalf("AwardPrizes: %v", err)
	}
	if led.entries[0].Currency == "" {
		t.Error("幣別不該是空字串")
	}
}
