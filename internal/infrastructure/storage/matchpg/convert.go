package matchpg

import (
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// matchRow 是「場次 + 雙方快照」那八支 query 共同的回傳形狀。
//
// 為什麼要自己再宣告一次:sqlc 為每一支 query 生成獨立的 Row 型別
// (LockMatchForJudgeRow、MarkMatchFinishedRow……),但 activity_match.sql 刻意讓
// 它們逐欄一致。欄名、型別、順序全同的結構在 Go 裡可以直接轉換,所以八支共用
// 一份 toMatch —— 抄八份轉換程式碼的後果不是冗長,是「其中一份漏改一欄」
// 會在半年後的某個晉級路徑上炸開,而且炸在與成因無關的地方。
//
// 下面那組 var _ = matchRow(...) 讓「查詢檔某一支的欄位被改動」變成編譯錯誤,
// 而不是執行期才發現的錯位。
type matchRow struct {
	ID               int64
	PublicID         string
	TournamentID     int64
	Round            int32
	Slot             int32
	Status           string
	ResultKind       string
	HandicapOpen     bool
	HandicapLockedAt *time.Time
	StreamUrl        *string //nolint:revive // 欄名必須與 sqlc 生成的 Row 逐字相同,結構轉換才成立
	StartedAt        *time.Time
	FinishedAt       *time.Time
	WinnerPlayerID   *int64
	P1PlayerID       *int64
	P1PublicID       *string
	P1FencerID       *int64
	P1DisplayName    *string
	P1RankLevel      *int16
	P1Status         *string
	P2PlayerID       *int64
	P2PublicID       *string
	P2FencerID       *int64
	P2DisplayName    *string
	P2RankLevel      *int16
	P2Status         *string
}

// 形狀契約的編譯期守衛:任一支 query 的回傳欄位一旦與其他支分歧,這裡就編不過。
var (
	_ = func(r db.LockMatchForJudgeRow) matchRow { return matchRow(r) }
	_ = func(r db.LockMatchAtForJudgeRow) matchRow { return matchRow(r) }
	_ = func(r db.GetMatchForJudgeRow) matchRow { return matchRow(r) }
	_ = func(r db.LockUnfinishedMatchesOfPlayerRow) matchRow { return matchRow(r) }
	_ = func(r db.MarkMatchReadyRow) matchRow { return matchRow(r) }
	_ = func(r db.MarkMatchLiveRow) matchRow { return matchRow(r) }
	_ = func(r db.MarkMatchFinishedRow) matchRow { return matchRow(r) }
	_ = func(r db.SetMatchStreamURLRow) matchRow { return matchRow(r) }
	_ = func(r db.SeatMatchPlayerRow) matchRow { return matchRow(r) }
)

// playerRow 同理,對應 LockPlayerForJudge 與 SetTournamentPlayerStatus。
type playerRow struct {
	ID           int64
	PublicID     string
	TournamentID int64
	FencerID     int64
	DisplayName  string
	RankLevel    *int16
	Status       string
}

var (
	_ = func(r db.LockPlayerForJudgeRow) playerRow { return playerRow(r) }
	_ = func(r db.SetTournamentPlayerStatusRow) playerRow { return playerRow(r) }
)

// toMatch 把一列翻成領域物件。
//
// 雙方走 LEFT JOIN,所以每個欄位都可能是 NULL(對手未定)。NULL 一律落成
// match.Player 的零值 —— 埠用 Player.ID == 0 表達「這一側尚未確定」,
// 在這裡多造一個 *Player 會讓每個呼叫端各判一次 nil。
func toMatch(r matchRow) match.Match {
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

// side 組出場上的一方。
//
// TournamentID 取場次的那一個而不是再 JOIN 一次 tournament_players.tournament_id:
// 兩者恆等(matches 與其選手同屬一屆,由建樹保證),多讀一欄只是為了同一個值繞路。
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

// toPlayer 把選手列翻成領域物件。rank_level 為 NULL(未評段)時落成
// bp.RankUnspecified —— 「未評」在 bp 套件裡就是 0,不需要第二種表示法。
func toPlayer(r playerRow) match.Player {
	return match.Player{
		ID:           r.ID,
		PublicID:     r.PublicID,
		TournamentID: r.TournamentID,
		FencerID:     r.FencerID,
		DisplayName:  r.DisplayName,
		Rank:         bp.Rank(deref(r.RankLevel)),
		Status:       tournament.PlayerStatus(r.Status),
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
