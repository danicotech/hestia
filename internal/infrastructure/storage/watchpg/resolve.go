package watchpg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/rules"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 這個檔案把信封讀成完整內容。
//
// # 一支新查詢都沒有(除了那兩支 pg_notify)
//
// 推播要讀的東西,查詢頁本來就在讀:場次走 GetMatchForJudge、讓武走
// FindMatchBudgetByMatch + ListHandicapSelections、盤口走 ListMarketsByMatches、
// 票數走 VoteTalliesByMarkets、規則走 GetTournamentByID + rules.Parse、階段走 GetTournamentBySlug。
//
// 為推播另寫一套「差不多但欄位少一點」的查詢,會讓同一個畫面上的同一個數字
// 有兩條產生路徑 —— 而它們會在某次改動之後給出不同答案(專案規則 9)。
// 賠率尤其不能有第二條路:它的公式在 betting 套件,連「把票數與盤口組成一場的賠率」
// 這一步也走 betting.BuildMatchOdds,這裡只負責把資料列翻成它要的形狀。

// readMatch 讀一場比賽的完整狀態。
func (l *Listener) readMatch(ctx context.Context, matchPublicID string) (*match.Match, error) {
	row, err := l.q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, notFound("場次", matchPublicID, err)
	}
	m := toMatch(row)
	return &m, nil
}

// readRounds 讀一場比賽與它的全部回合(回合開始 / 結束、場次定案的推播內容)。
//
// 回合勝者在列上是內部 id,這裡用場上兩人對回 public_id(Match.ResolveRoundWinners),
// 訂閱者收到的就是對戰表頁查詢會回的同一個形狀。
func (l *Listener) readRounds(ctx context.Context, matchPublicID string) (*match.Match, *watch.MatchRounds, error) {
	m, err := l.readMatch(ctx, matchPublicID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := l.q.ListMatchRounds(ctx, m.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("讀回合 %s: %w", matchPublicID, err)
	}
	rounds := make([]match.Round, 0, len(rows))
	for _, r := range rows {
		rounds = append(rounds, toRound(r))
	}
	m.ResolveRoundWinners(rounds)
	return m, &watch.MatchRounds{MatchPublicID: m.PublicID, Rounds: rounds}, nil
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

// readOdds 讀一場全部盤口的票數並算出當下的賠率。
//
// **賠率不從資料庫讀,是算出來的** —— 它沒有被存在任何地方(存了就會與票數
// 不同步)。信封的 Ref 是場次 public_id(一票只動一個盤口,但畫面上顯示的是整場
// 的盤口清單),所以這裡一次解出該場所有盤口,與 GetOdds 回的是同一個東西。
//
// MyVote 刻意留空(mine 傳 nil):推播是廣播,一則訊息要送給這一屆的所有訂閱者,
// 不可能因人而異(watch.proto 的「匿名可讀」)。
func (l *Listener) readOdds(ctx context.Context, matchPublicID string) (*betting.MatchOdds, error) {
	row, err := l.q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		return nil, notFound("場次", matchPublicID, err)
	}
	m := toMatch(row)

	marketRows, err := l.q.ListMarketsByMatches(ctx, []int64{m.ID})
	if err != nil {
		return nil, fmt.Errorf("讀盤口 %s: %w", matchPublicID, err)
	}
	markets := make([]betting.Market, 0, len(marketRows))
	marketIDs := make([]int64, 0, len(marketRows))
	for _, mk := range marketRows {
		line, err := lineSecondsOf(mk.Params)
		if err != nil {
			return nil, fmt.Errorf("盤口 %s 的參數: %w", mk.PublicID, err)
		}
		markets = append(markets, betting.Market{
			ID:          mk.ID,
			PublicID:    mk.PublicID,
			MatchID:     mk.MatchID,
			Kind:        betting.MarketKind(mk.Kind),
			RoundNo:     int(deref(mk.RoundNo)),
			LineSeconds: line,
			Status:      betting.MarketStatus(mk.Status),
		})
		marketIDs = append(marketIDs, mk.ID)
	}

	// 沒有任何人投票的盤口不會有列 —— 零票是合法輸入,平滑參數會給出各結果
	// 相同的賠率,不需要在這裡補特例。
	tallies := map[int64]betting.Tally{}
	if len(marketIDs) > 0 {
		rows, err := l.q.VoteTalliesByMarkets(ctx, marketIDs)
		if err != nil {
			return nil, fmt.Errorf("數票 %s: %w", matchPublicID, err)
		}
		for _, t := range rows {
			tally := tallies[t.MarketID]
			if tally == nil {
				tally = betting.Tally{}
				tallies[t.MarketID] = tally
			}
			tally[betting.Outcome(t.Outcome)] = t.Votes
		}
	}

	// 規則(賠率參數、best_of → 比分盤的結果數)= tournaments.config 經 rules.Parse。
	// 壞設定退回預設、錯誤只是診斷,推播不是記它的地方(rules.Validate 在建賽事時擋)。
	// MaxStake 對賠率無關,推播不讀 economy_configs。
	t, err := l.q.GetTournamentByID(ctx, m.TournamentID)
	if err != nil {
		return nil, notFound("賽事", matchPublicID, err)
	}
	cfg, _ := rules.Parse(t.Config)

	// 選手名要帶:p1 / p2 類結果的顯示名由 betting.OutcomeLabel 用它組,
	// 少了名字,推播回的 label 會是空的而查詢回的不是(同一欄位兩種答案)。
	bm := betting.Match{
		PublicID: m.PublicID, Status: betting.MatchStatus(m.Status),
		P1: betting.Participant{PlayerID: m.P1.ID, DisplayName: m.P1.DisplayName},
		P2: betting.Participant{PlayerID: m.P2.ID, DisplayName: m.P2.DisplayName},
	}
	out := betting.BuildMatchOdds(bm, markets, tallies, betting.Rules{Config: cfg}, nil)
	return &out, nil
}

// marketParams 是 markets.params 的形狀(與 bettingpg 同一份 JSON 鍵,權威是 migration 00007)。
type marketParams struct {
	LineSeconds int64 `json:"line_seconds,omitempty"`
}

// lineSecondsOf 從 markets.params 取 line_seconds(缺鍵 = 0)。讀不懂的 JSON 失敗出聲:
// 這條線決定 over/under 的顯示,壞掉的參數不該靜靜變成 0 秒。
func lineSecondsOf(raw []byte) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var p marketParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, fmt.Errorf("解析 params %q: %w", string(raw), err)
	}
	return p.LineSeconds, nil
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

// toRound 把回合列翻成 core 型別。WinnerPublicID 留給 ResolveRoundWinners 填。
func toRound(r db.ActivityMatchRound) match.Round {
	out := match.Round{RoundNo: int(r.RoundNo), StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	if r.WinnerPlayerID != nil {
		out.WinnerPlayerID = *r.WinnerPlayerID
	}
	return out
}
