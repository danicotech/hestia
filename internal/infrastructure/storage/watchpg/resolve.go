package watchpg

import (
	"context"
	"fmt"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 這個檔案把信封讀成完整內容。
//
// # 一支新查詢都沒有(除了那兩支 pg_notify)
//
// 推播要讀的東西,查詢頁本來就在讀:場次走 GetMatchForJudge、讓武走
// FindMatchBudgetByMatch + ListHandicapSelections、票數走 VoteTalliesByMatch、
// 賠率參數走 BettingOddsConfig、階段走 GetTournamentBySlug。
//
// 為推播另寫一套「差不多但欄位少一點」的查詢,會讓同一個畫面上的同一個數字
// 有兩條產生路徑 —— 而它們會在某次改動之後給出不同答案(專案規則 9)。
// 賠率尤其不能有第二條路:它的公式在 betting 套件,這裡只負責把票數餵進去。

// readMatch 讀一場比賽的完整狀態。
func (l *Listener) readMatch(ctx context.Context, matchPublicID string) (*match.Match, error) {
	row, err := l.q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, notFound("場次", matchPublicID, err)
	}
	m := toMatch(row)
	return &m, nil
}

// readHandicaps 讀封盤後的完整讓武清單。
//
// 這是整個推播管線裡唯一「內容在這一刻才變公開」的一則,所以它一定要讀出
// 全部選擇 —— 只送一個場次 id 讓前端自己查的話,「公開的是封盤當下那一份」
// 就沒有人保證得了。
func (l *Listener) readHandicaps(ctx context.Context, matchPublicID string) (*handicap.MatchHandicaps, error) {
	row, err := l.q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, notFound("場次", matchPublicID, err)
	}
	m := toMatch(row)
	out := &handicap.MatchHandicaps{
		MatchPublicID: m.PublicID,
		Status:        string(m.Status),
		LockedAt:      m.HandicapLockedAt,
		// 封盤之後才會走到這裡,所以恆為 true:這一則的全部意義就是「公開了」。
		Revealed:   true,
		Selections: []handicap.Selection{},
	}

	b, err := l.q.FindMatchBudgetByMatch(ctx, m.ID)
	if err != nil {
		// 沒有預算列 = 本場無讓武(同段對決)。那不是錯誤,而且仍然要推 ——
		// 封盤本身是觀眾要看到的狀態變化,清單是空的也一樣。
		if isNoRows(err) {
			return out, nil
		}
		return nil, notFound("讓武預算", matchPublicID, err)
	}
	out.HolderPlayerPublicID = b.PlayerPublicID
	out.ConstrainedPlayerPublicID = opponentPublicID(m, b.PlayerID)
	out.Budget = &handicap.Budget{
		MatchID:        b.MatchID,
		PlayerID:       b.PlayerID,
		PlayerPublicID: b.PlayerPublicID,
		Budget:         b.Budget,
		Spent:          b.Spent,
	}

	rows, err := l.q.ListHandicapSelections(ctx, db.ListHandicapSelectionsParams{
		MatchID: m.ID, PlayerID: b.PlayerID,
	})
	if err != nil {
		return nil, fmt.Errorf("讀讓武清單 %s: %w", matchPublicID, err)
	}
	for _, r := range rows {
		out.Selections = append(out.Selections, handicap.Selection{
			ID:            r.ID,
			PublicID:      r.PublicID,
			MatchID:       r.MatchID,
			MatchPublicID: r.MatchPublicID,
			PlayerID:      r.PlayerID,
			ItemID:        r.ItemID,
			ItemRef:       r.ItemRef,
			ItemName:      r.ItemName,
			Category:      handicap.Category(r.Category),
			Cost:          r.Cost,
			TargetNote:    deref(r.TargetNote),
			Voided:        r.Voided,
			CreatedAt:     r.CreatedAt,
		})
	}
	return out, nil
}

// readOdds 讀票數並算出當下的賠率。
//
// **賠率不從資料庫讀,是算出來的** —— 它沒有被存在任何地方(存了就會與票數
// 不同步)。公式的權威在 betting.MatchOddsOf,這裡只負責把票數與設定餵進去。
//
// MyVote 刻意留空:推播是廣播,一則訊息要送給這一屆的所有訂閱者,
// 不可能因人而異(watch.proto 的「匿名可讀」)。
func (l *Listener) readOdds(ctx context.Context, matchPublicID string) (*betting.MatchOdds, error) {
	row, err := l.q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, notFound("場次", matchPublicID, err)
	}
	m := toMatch(row)

	tallies, err := l.q.VoteTalliesByMatch(ctx, []int64{m.ID})
	if err != nil {
		return nil, fmt.Errorf("數票 %s: %w", matchPublicID, err)
	}
	var tally betting.Tally
	for _, t := range tallies {
		if t.MatchID == m.ID {
			tally = betting.Tally{P1: t.P1Votes, P2: t.P2Votes}
		}
	}
	// 沒有任何人投票的場次不會有列 —— 零票是合法輸入,平滑參數會給出兩邊
	// 相同的賠率,不需要在這裡補特例。

	cfgRow, err := l.q.BettingOddsConfig(ctx, m.TournamentID)
	if err != nil {
		return nil, fmt.Errorf("讀賠率設定 %s: %w", matchPublicID, err)
	}
	cfg := betting.OddsConfig{
		SmoothingVotes: cfgRow.SmoothingVotes,
		VigBps:         cfgRow.VigBps,
		MinOddsMilli:   cfgRow.MinOddsMilli,
		MaxOddsMilli:   cfgRow.MaxOddsMilli,
		MaxParlayMilli: cfgRow.MaxParlayMilli,
		MaxStake:       cfgRow.MaxStake,
	}.Normalize()

	p1, p2 := betting.MatchOddsOf(tally, cfg)
	return &betting.MatchOdds{
		MatchPublicID: m.PublicID,
		P1Votes:       tally.P1,
		P2Votes:       tally.P2,
		P1OddsMilli:   p1,
		P2OddsMilli:   p2,
		OpenForBets:   betting.MatchStatus(m.Status).OpenForBets(),
		MyVote:        betting.SideNone,
	}, nil
}

// readPhase 讀賽事當前階段。
func (l *Listener) readPhase(ctx context.Context, slug string) (tournament.Phase, error) {
	row, err := l.q.GetTournamentBySlug(ctx, slug)
	if err != nil {
		return "", notFound("賽事", slug, err)
	}
	return tournament.Phase(row.Phase), nil
}

// readChampion 從決賽那一場讀出冠軍與亞軍。
//
// 為什麼不另存一張「冠軍」表:冠軍就是決賽的勝者,存第二份只會多一個
// 可能與對戰表矛盾的地方(專案規則 9)。
func (l *Listener) readChampion(ctx context.Context, finalMatchPublicID string) (*watch.Champion, error) {
	row, err := l.q.GetMatchForJudge(ctx, finalMatchPublicID)
	if err != nil {
		return nil, notFound("決賽場次", finalMatchPublicID, err)
	}
	m := toMatch(row)
	winner, loser := m.P1, m.P2
	switch m.WinnerPlayerID {
	case 0:
		// 通知在飛的時候勝者被清掉了(或信封指到一場還沒打完的比賽)。
		return nil, fmt.Errorf("決賽 %s 尚未分出勝負", finalMatchPublicID)
	case m.P2.ID:
		winner, loser = m.P2, m.P1
	}
	c := &watch.Champion{
		FinalMatchPublicID: m.PublicID,
		PlayerPublicID:     winner.PublicID,
		DisplayName:        winner.DisplayName,
		Rank:               winner.Rank,
	}
	if loser.Seated() {
		c.RunnerUpPublicID = loser.PublicID
		c.RunnerUpDisplayName = loser.DisplayName
	}
	return c, nil
}

// opponentPublicID 回傳場上另一方的 public_id;對手未定時回空字串。
func opponentPublicID(m match.Match, playerID int64) string {
	return m.Opponent(playerID).PublicID
}

// toMatch 把 GetMatchForJudge 的一列翻成領域物件。
//
// 與 matchpg 的同名函式是同一套映射,但那一份不導出(它還背著八支 query
// 共用的形狀契約,導出等於把那個內部約定變成公開 API)。這裡只需要
// 單一一支查詢的映射,所以複製的是欄位對應,不是那個約定。
func toMatch(r db.GetMatchForJudgeRow) match.Match {
	return match.Match{
		ID:               r.ID,
		PublicID:         r.PublicID,
		TournamentID:     r.TournamentID,
		Round:            int(r.Round),
		Slot:             int(r.Slot),
		Status:           match.Status(r.Status),
		ResultKind:       match.ResultKind(r.ResultKind),
		HandicapOpen:     r.HandicapOpen,
		HandicapLockedAt: r.HandicapLockedAt,
		StreamURL:        deref(r.StreamUrl),
		StartedAt:        r.StartedAt,
		FinishedAt:       r.FinishedAt,
		WinnerPlayerID:   deref(r.WinnerPlayerID),
		P1: side(r.TournamentID, r.P1PlayerID, r.P1PublicID,
			r.P1FencerID, r.P1DisplayName, r.P1RankLevel, r.P1Status),
		P2: side(r.TournamentID, r.P2PlayerID, r.P2PublicID,
			r.P2FencerID, r.P2DisplayName, r.P2RankLevel, r.P2Status),
	}
}

// side 組出場上的一方;LEFT JOIN 的 NULL(對手未定)落成零值。
func side(tournamentID int64, id *int64, publicID *string,
	fencerID *int64, displayName *string, rank *int16, status *string,
) match.Player {
	if id == nil {
		return match.Player{}
	}
	return match.Player{
		ID:           *id,
		PublicID:     deref(publicID),
		TournamentID: tournamentID,
		FencerID:     deref(fencerID),
		DisplayName:  deref(displayName),
		Rank:         bp.Rank(deref(rank)),
		Status:       tournament.PlayerStatus(deref(status)),
	}
}

// deref 取指標值,nil 即型別零值。
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
