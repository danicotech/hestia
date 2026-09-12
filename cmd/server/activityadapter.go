package main

import (
	"context"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/prize"
	"github.com/danicotech/hestia/internal/core/activity/session"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activityreadpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/tournamentpg"
	"github.com/danicotech/hestia/internal/transport"
)

// 發獎的兩個轉接。放在組裝層而不是任何一邊,理由與 authAdapter 相同:
// 入口層的 port 與領域服務各自獨立演化,而「把這兩個形狀對起來」是組裝的工作。
// 領域層若直接長成入口層的形狀,就等於契約反過來決定領域模型。

// prizeRepo 把發獎要的三個唯讀查詢湊齊。
//
// ListPlayers 與 ListMatches 來自 activityreadpg,TournamentBySlug 的權威
// 在 tournamentpg —— 沒有為了湊齊介面而在任一邊複製一份查詢。同一份
// 「本屆全部選手」若有第二個版本,遲早有一邊會漏掉棄賽者,而參賽獎要發給他們。
type prizeRepo struct {
	*activityreadpg.Service
	tournaments *tournamentpg.Service
}

func (r prizeRepo) TournamentBySlug(ctx context.Context, slug string) (tournament.Tournament, error) {
	return r.tournaments.TournamentBySlug(ctx, slug)
}

// prizeAdapter 把 prize.Service 接到入口層的 ActivityPrizes。
type prizeAdapter struct{ svc *prize.Service }

func (a prizeAdapter) AwardPrizes(
	ctx context.Context, p transport.ActivityPrizeParams,
) ([]transport.ActivityPrizeAward, bool, error) {
	// p.IdempotencyKey 刻意不往下傳:發獎的冪等鍵是從「賽事+選手+獎項」
	// 推出來的,不是呼叫端給的。理由是發獎不是一次就結束的動作 —— 有人
	// 沒綁帳號時那幾筆發不出去,裁判等他們綁好會再按一次,而那一次必然
	// 帶新的冪等鍵。用呼叫端的鍵當權威,第二次就會把已經領過的人再發一遍。
	res, err := a.svc.AwardPrizes(ctx, prize.AwardParams{
		TournamentSlug: p.TournamentSlug,
		ActorUserID:    p.ActorUserID,
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]transport.ActivityPrizeAward, 0, len(res.Awards))
	for _, a := range res.Awards {
		out = append(out, transport.ActivityPrizeAward{
			PlayerPublicID: a.PlayerPublicID,
			DisplayName:    a.DisplayName,
			Kind:           string(a.Kind),
			Amount:         a.Amount,
			Granted:        a.Granted,
			SkippedReason:  a.SkippedReason,
		})
	}
	return out, res.Replayed, nil
}

// activitySessionAdapter 把 session.Service 接到入口層的 ActivitySessions。
//
// 兩邊的身分型別欄位同名同義,轉接就只是搬運 —— 但它們**刻意是兩個型別**:
// 入口層的 port 不該因為領域層換了 session 實作就跟著改,而領域層也不該
// 為了滿足一個 proto 契約而長出入口層的形狀。
type activitySessionAdapter struct{ svc *session.Service }

func (a activitySessionAdapter) Issue(
	ctx context.Context, id transport.ActivityIdentity,
) (string, time.Time, error) {
	return a.svc.Issue(ctx, session.Identity{
		TournamentSlug: id.TournamentSlug,
		PlayerPublicID: id.PlayerPublicID,
	})
}

func (a activitySessionAdapter) Verify(
	ctx context.Context, token string,
) (transport.ActivityIdentity, error) {
	id, err := a.svc.Verify(ctx, token)
	if err != nil {
		return transport.ActivityIdentity{}, err
	}
	return transport.ActivityIdentity{
		TournamentSlug: id.TournamentSlug,
		PlayerPublicID: id.PlayerPublicID,
	}, nil
}
