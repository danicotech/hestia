package handicappg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/rules"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// LockMatch 讀一場比賽並鎖住該列。整個讓武的序列化點。
func (r *Repo) LockMatch(ctx context.Context, matchPublicID string) (*handicap.Match, error) {
	row, err := r.q.LockMatchForHandicap(ctx, matchPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match=%s: %w", matchPublicID, activityerr.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("鎖場次 %s: %w", matchPublicID, err)
	}
	return fromLockRow(row), nil
}

// GetMatch 是唯讀路徑用的無鎖版本。
func (r *Repo) GetMatch(ctx context.Context, matchPublicID string) (*handicap.Match, error) {
	row, err := r.q.GetMatchForHandicap(ctx, matchPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match=%s: %w", matchPublicID, activityerr.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("讀場次 %s: %w", matchPublicID, err)
	}
	return fromGetRow(row), nil
}

// LockHandicaps 封盤。0 列 = 已經封過(WHERE handicap_locked_at IS NULL 擋掉)。
//
// 封盤不可逆這件事由 DB 的述詞保證,不是由這裡先讀再寫 ——
// 先讀再寫會留下一道縫,而這一句自己就是原子的。
func (r *Repo) LockHandicaps(ctx context.Context, matchID int64, at time.Time) error {
	n, err := r.q.LockMatchHandicaps(ctx, db.LockMatchHandicapsParams{
		LockedAt: &at,
		MatchID:  matchID,
	})
	if err != nil {
		return fmt.Errorf("封盤 match=%d: %w", matchID, err)
	}
	if n == 0 {
		// 重複封盤要出聲:封盤會觸發 Discord 公告,默默成功等於再公告一次。
		return fmt.Errorf("match=%d: %w", matchID, handicap.ErrAlreadyLocked)
	}
	return nil
}

// SetSelectionDraw 寫入抽選結果。0 列 = 已抽過(WHERE draw_result IS NULL 擋掉)。
//
// at 由呼叫端傳入(= 封盤時間),不是 now():「抽選時間等於封盤時間」是語意,
// 不該靠同一個 tx 內兩個 now() 相等這種巧合。
func (r *Repo) SetSelectionDraw(ctx context.Context, selectionID int64, result string, at time.Time) error {
	n, err := r.q.SetHandicapSelectionDraw(ctx, db.SetHandicapSelectionDrawParams{
		Result: result,
		At:     at,
		ID:     selectionID,
	})
	if err != nil {
		return fmt.Errorf("寫入抽選結果 selection=%d: %w", selectionID, err)
	}
	if n == 0 {
		return fmt.Errorf("selection=%d: %w", selectionID, handicap.ErrAlreadyDrawn)
	}
	return nil
}

// DrawPools 讀該屆的 config 並交給 rules.Parse。回的 rules.Config 逐字元滿足 handicap.DrawPools。
//
// 走 GetTournamentByID 拿整份 config 而不是 HandicapDrawPools 那支只撈池的查詢:
// 後者回的是 pgx 已經解成 map 的 jsonb,而解析的權威是 rules.Parse(它吃 []byte)——
// 用它就得自己再序列化一次,或自己解一次 JSON,兩者都是第二個權威。
//
// rules.Parse 的錯誤只是診斷(壞欄位逐一退回預設值,Config 仍可用),這裡不讓它擋封盤:
// 一份 betting 區塊打錯字的 config 不該讓一場買了「隨機武學」的比賽封不了盤。
// 池真的抽不出來時,core 會以 ErrDrawPoolEmpty 擋下 —— 那才是這條路徑該出的聲。
func (r *Repo) DrawPools(ctx context.Context, tournamentID int64) (handicap.DrawPools, error) {
	t, err := r.q.GetTournamentByID(ctx, tournamentID)
	if err != nil {
		// 場次的 tournament_id 有 FK,走到這裡查無此屆只可能是資料被動過;
		// 不翻成 sentinel(呼叫端沒有可以據此做的決定),原樣冒上來。
		return nil, fmt.Errorf("讀賽事 config tournament=%d: %w", tournamentID, err)
	}
	cfg, _ := rules.Parse(t.Config)
	return cfg, nil
}

// matchFields 是兩支場次查詢共同的欄位形狀。
//
// sqlc 為每支查詢各生一個 row 型別(LockMatchForHandicapRow / GetMatchForHandicapRow),
// 欄位逐字相同但在 Go 眼中是兩個不相干的型別。轉成 core 型別的邏輯只寫一份,
// 免得日後改欄位時只改了一邊 —— 那種 bug 會表現成「唯讀頁正確、選購頁錯」。
type matchFields struct {
	ID               int64
	PublicID         string
	TournamentID     int64
	Status           string
	HandicapOpen     bool
	HandicapLockedAt *time.Time
	P1PlayerID       *int64
	P2PlayerID       *int64
	P1PlayerPublicID *string
	P2PlayerPublicID *string
	P1RankLevel      *int16
	P2RankLevel      *int16
	PerRankGap       int64
}

func (f matchFields) toMatch() *handicap.Match {
	return &handicap.Match{
		ID:               f.ID,
		PublicID:         f.PublicID,
		TournamentID:     f.TournamentID,
		Status:           f.Status,
		HandicapOpen:     f.HandicapOpen,
		LockedAt:         f.HandicapLockedAt,
		P1PlayerID:       deref(f.P1PlayerID),
		P2PlayerID:       deref(f.P2PlayerID),
		P1PlayerPublicID: deref(f.P1PlayerPublicID),
		P2PlayerPublicID: deref(f.P2PlayerPublicID),
		// rank_level 為 NULL(尚未評段)攤成 bp.RankUnspecified —— bp.Holder 會
		// 把它擋成 ErrUnranked,而不是當成某個有效段位去算 BP。
		P1Rank:     bp.Rank(deref(f.P1RankLevel)),
		P2Rank:     bp.Rank(deref(f.P2RankLevel)),
		PerRankGap: f.PerRankGap,
	}
}

func fromLockRow(r db.LockMatchForHandicapRow) *handicap.Match {
	return matchFields{
		ID:               r.ID,
		PublicID:         r.PublicID,
		TournamentID:     r.TournamentID,
		Status:           r.Status,
		HandicapOpen:     r.HandicapOpen,
		HandicapLockedAt: r.HandicapLockedAt,
		P1PlayerID:       r.P1PlayerID,
		P2PlayerID:       r.P2PlayerID,
		P1PlayerPublicID: r.P1PlayerPublicID,
		P2PlayerPublicID: r.P2PlayerPublicID,
		P1RankLevel:      r.P1RankLevel,
		P2RankLevel:      r.P2RankLevel,
		PerRankGap:       r.PerRankGap,
	}.toMatch()
}

func fromGetRow(r db.GetMatchForHandicapRow) *handicap.Match {
	return matchFields{
		ID:               r.ID,
		PublicID:         r.PublicID,
		TournamentID:     r.TournamentID,
		Status:           r.Status,
		HandicapOpen:     r.HandicapOpen,
		HandicapLockedAt: r.HandicapLockedAt,
		P1PlayerID:       r.P1PlayerID,
		P2PlayerID:       r.P2PlayerID,
		P1PlayerPublicID: r.P1PlayerPublicID,
		P2PlayerPublicID: r.P2PlayerPublicID,
		P1RankLevel:      r.P1RankLevel,
		P2RankLevel:      r.P2RankLevel,
		PerRankGap:       r.PerRankGap,
	}.toMatch()
}
