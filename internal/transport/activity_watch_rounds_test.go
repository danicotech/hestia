package transport

import (
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/watch"
)

// 回合那一則推播要轉成帶 rounds 與勝場數的 MatchUpdate —— 這是計時器與比數
// 在第 2 回合起唯一的即時來源(QA 抓到 KindRound 兩端都沒接)。
func TestWatchRoundUpdateCarriesRoundsAndScore(t *testing.T) {
	m := testBracketMatches()[0]
	m.Status = match.StatusLive
	at := time.Unix(1_700_000_000, 0)
	later := at.Add(90 * time.Second)
	rounds := []match.Round{
		{RoundNo: 1, StartedAt: at, FinishedAt: &later, WinnerPlayerID: testPlayerBInternal},
		{RoundNo: 2, StartedAt: later},
	}
	m.ResolveRoundWinners(rounds)

	for _, kind := range []watch.Kind{watch.KindRound, watch.KindMatch} {
		got := watchUpdateToProto(watch.Update{
			Kind: kind, EmittedAt: at, Match: &m,
			Rounds: &watch.MatchRounds{MatchPublicID: m.PublicID, Rounds: rounds},
		})
		if got == nil || got.GetMatch() == nil {
			t.Fatalf("%s:沒轉成 MatchUpdate", kind)
		}
		pm := got.GetMatch().GetMatch()
		if len(pm.GetRounds()) != 2 {
			t.Fatalf("%s:rounds = %d,want 2", kind, len(pm.GetRounds()))
		}
		if pm.GetRounds()[0].GetWinnerPlayerPublicId() != testPlayerBID {
			t.Errorf("%s:第一回合勝者 = %q,want %s", kind, pm.GetRounds()[0].GetWinnerPlayerPublicId(), testPlayerBID)
		}
		if pm.GetRounds()[1].GetFinishedAt() != nil {
			t.Errorf("%s:進行中的回合不該有 finished_at", kind)
		}
		if pm.GetP1RoundWins() != 0 || pm.GetP2RoundWins() != 1 {
			t.Errorf("%s:比數 = %d:%d,want 0:1", kind, pm.GetP1RoundWins(), pm.GetP2RoundWins())
		}
	}

	// 沒帶回合(舊資料、單場定勝負)也還是一則合法的 MatchUpdate,只是 rounds 空。
	got := watchUpdateToProto(watch.Update{Kind: watch.KindMatch, EmittedAt: at, Match: &m})
	if got == nil || len(got.GetMatch().GetMatch().GetRounds()) != 0 {
		t.Fatalf("沒有回合資料時應回空 rounds,got %v", got)
	}
}
