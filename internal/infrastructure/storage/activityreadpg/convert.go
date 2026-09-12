package activityreadpg

import (
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// matchRow 是「場次 + 雙方快照」那組 query 共同的回傳形狀。
//
// # 為什麼這裡有一份自己的映射
//
// matchpg 與 watchpg 各有一份同樣的 toMatch。那不是疏忽:matchpg 的那一份
// 不導出,因為它還背著「八支裁判 query 的欄位必須逐欄一致」這個內部約定,
// 導出等於把那個約定變成公開 API(watchpg 的同名函式已經記下這個理由)。
// 三份共用的是**欄位對應**,不是那個約定 —— 而欄位對應本身有下面那組
// 編譯期守衛釘住:任何一支 query 的欄位被改動,這裡當場編不過。
//
// 收斂的方向不是把映射抄得更少,而是有一天讓 db 那層直接生出領域型別;
// 在那之前,一個「會在 build 時爆掉」的複本比一個跨套件的導出約定安全。
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

// 形狀契約的編譯期守衛。
//
// 讀取側的兩支新 query 與既有的 GetMatchForJudge 必須逐欄一致 —— 一致才能
// 共用同一份 toMatch,而 Go 的結構轉換只在欄位名稱、型別、順序三者都相同時
// 才編得過。哪天有人在 activity_read.sql 少列一欄,build 就會當場失敗,
// 而不是在對戰表上靜靜地少一個名字。
var (
	_ = func(r db.GetMatchForJudgeRow) matchRow { return matchRow(r) }
	_ = func(r db.ListTournamentMatchesRow) matchRow { return matchRow(r) }
	_ = func(r db.CurrentMatchOfPlayerRow) matchRow { return matchRow(r) }
)

// toMatch 把一列翻成領域物件。
//
// 雙方走 LEFT JOIN,所以每個欄位都可能是 NULL(對手未定 = 等上一輪)。
// NULL 一律落成 match.Player 的零值 —— 埠用 Player.ID == 0 表達「這一側尚未
// 確定」(match.Player.Seated),多造一個 *Player 只會讓每個呼叫端各判一次 nil。
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

// side 組出場上的一方;LEFT JOIN 的 NULL(對手未定)落成零值。
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

// deref 取指標值,nil 即型別零值。
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
