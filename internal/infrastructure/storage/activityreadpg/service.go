// Package activityreadpg 是《百業試鋒》活動層**讀取側**的 Postgres 實作
// (transport.ActivityReader)。
//
// # 為什麼它是一個獨立的套件,而不是 tournamentpg / matchpg 的幾個方法
//
// 那兩個套件各自實作一個領域埠(tournament.Repo / match.Repository),而領域埠
// 的每一支方法都背著鎖序、階段守門與稽核的約定。讀取側要的是完全相反的東西:
// 不取任何鎖、不寫任何東西、看得到已淘汰與已棄賽的人、對匿名觀眾開放。
// 混進去的話,那兩個套件「所有方法都在鎖序約定之內」的性質就會出現例外 ——
// 而例外正是日後有人照著旁邊那支抄出死鎖的來源。
//
// # 這裡沒有新的「怎麼把一列變成領域物件」
//
// 選手走 tournamentpg.PlayerFromRow、賽事走 tournamentpg.TournamentFromRow、
// 跨屆檔案走 signuppg.FencerFromRow,PlayerByPublicID 整支直接委派給
// tournamentpg(簽名與那邊逐字相同)。同一個概念只有一個權威位置(鐵則 9);
// 唯一自己寫的是場次的列映射,理由見 convert.go。
//
// # 全部是 SELECT
//
// 本套件一句 INSERT / UPDATE / DELETE 都沒有,也不開 transaction:
// 每個方法都是單一一句查詢,沒有「兩句之間要保持一致」的需求。
package activityreadpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/signuppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/tournamentpg"
)

// Service 是 transport.ActivityReader 的實作。
//
// 沒有任何跨呼叫的狀態,可以被所有併發請求共用。
type Service struct {
	q *db.Queries
	// players 提供 PlayerByPublicID。委派而不是再寫一份:兩邊的簽名與那一句
	// SQL 完全相同,寫第二份就是同一個概念的第二個權威位置(鐵則 9)。
	players *tournamentpg.Service
}

// New 建立讀取側服務。與 tournamentpg / matchpg 用同一個 pool。
func New(pool *pgxpool.Pool) *Service {
	return &Service{q: db.New(pool), players: tournamentpg.New(pool)}
}

// ── port 形狀的編譯期檢查 ──────────────────────────────────────
//
// port 介面宣告在使用端(transport.ActivityReader),而 infrastructure 不能
// import transport —— 所以寫不出 `var _ transport.ActivityReader = (*Service)(nil)`。
// 這裡用同形狀的鏡像介面斷言,理由與 readpg 的同一段註解相同:它幾乎免費,
// 又能讓「誰改壞了 Service 的簽名」在本套件就編不過,不必等到 cmd/server。
// 真正的權威檢查仍是 cmd/server 把 *Service 指派給 ActivityDeps.Reader 那一行。
type activityReaderPort interface {
	TournamentByID(ctx context.Context, tournamentID int64) (tournament.Tournament, error)
	TournamentByPlayer(ctx context.Context, playerPublicID string) (tournament.Tournament, error)
	CountPlayers(ctx context.Context, tournamentID int64) (int32, error)
	ListPlayers(ctx context.Context, tournamentID int64) ([]tournament.Player, error)
	PlayerByPublicID(ctx context.Context, tournamentID int64, playerPublicID string) (tournament.Player, error)
	ListMatches(ctx context.Context, tournamentID int64) ([]match.Match, error)
	MatchByPublicID(ctx context.Context, matchPublicID string) (match.Match, error)
	CurrentMatchOfPlayer(ctx context.Context, playerID int64) (*match.Match, error)
	Fencers(ctx context.Context, fencerIDs []int64) (map[int64]signup.Fencer, error)
}

var _ activityReaderPort = (*Service)(nil)

// ── 賽事 ──────────────────────────────────────────────────────

// TournamentByID 依內部 id 讀一屆賽事。查無回 tournament.ErrTournamentNotFound。
func (s *Service) TournamentByID(ctx context.Context, tournamentID int64) (tournament.Tournament, error) {
	row, err := s.q.GetTournamentByID(ctx, tournamentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tournament.Tournament{}, fmt.Errorf("tournament=%d: %w",
				tournamentID, tournament.ErrTournamentNotFound)
		}
		return tournament.Tournament{}, fmt.Errorf("讀賽事 %d: %w", tournamentID, err)
	}
	return tournamentpg.TournamentFromRow(row)
}

// TournamentByPlayer 依選手 public_id 反查他所屬的那一屆。
//
// 查無回 tournament.ErrPlayerNotFound 而不是 ErrTournamentNotFound:
// 呼叫端手上只有一個選手 id,「這位選手不存在」才是他能據以行動的答案 ——
// 回「賽事不存在」會讓裁判去檢查賽事的網址,而那裡根本沒有問題。
func (s *Service) TournamentByPlayer(ctx context.Context, playerPublicID string) (tournament.Tournament, error) {
	row, err := s.q.GetTournamentByPlayerPublicID(ctx, playerPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tournament.Tournament{}, fmt.Errorf("player=%s: %w",
				playerPublicID, tournament.ErrPlayerNotFound)
		}
		return tournament.Tournament{}, fmt.Errorf("依選手 %s 反查賽事: %w", playerPublicID, err)
	}
	return tournamentpg.TournamentFromRow(row)
}

// ── 選手 ──────────────────────────────────────────────────────

// CountPlayers 數本屆報名人數(含已淘汰、已棄賽 —— 報名人數是已經發生的事實)。
func (s *Service) CountPlayers(ctx context.Context, tournamentID int64) (int32, error) {
	n, err := s.q.CountTournamentPlayers(ctx, tournamentID)
	if err != nil {
		return 0, fmt.Errorf("數賽事 %d 報名人數: %w", tournamentID, err)
	}
	return n, nil
}

// ListPlayers 列出本屆全部參賽者,依籤位、再依報名序排序(排序的權威在 SQL)。
//
// 一位都沒有時回**空切片**而不是 nil:呼叫端會直接 range,而「還沒有人報名」
// 不是錯誤。
func (s *Service) ListPlayers(ctx context.Context, tournamentID int64) ([]tournament.Player, error) {
	rows, err := s.q.ListTournamentPlayers(ctx, tournamentID)
	if err != nil {
		return nil, fmt.Errorf("列賽事 %d 的參賽者: %w", tournamentID, err)
	}
	out := make([]tournament.Player, len(rows))
	for i, r := range rows {
		out[i] = tournamentpg.PlayerFromRow(db.GetPlayerByPublicIDRow(r))
	}
	return out, nil
}

// PlayerByPublicID 在本屆內以 public_id 查一位參賽者。查無回 tournament.ErrPlayerNotFound。
func (s *Service) PlayerByPublicID(
	ctx context.Context, tournamentID int64, playerPublicID string,
) (tournament.Player, error) {
	return s.players.PlayerByPublicID(ctx, tournamentID, playerPublicID)
}

// ── 對戰表 ────────────────────────────────────────────────────

// ListMatches 列出本屆全部場次,依 (round, slot) 遞增。
//
// 尚未抽籤時回**空切片、nil 錯誤**:賽事存在、只是還沒抽,而 transport 正是
// 靠「空清單」決定顯示「尚未抽籤」而不是 404。回 nil 切片也能 range,
// 但回空切片讓「有沒有查到」與「切片是不是 nil」不再有任何關聯。
func (s *Service) ListMatches(ctx context.Context, tournamentID int64) ([]match.Match, error) {
	rows, err := s.q.ListTournamentMatches(ctx, tournamentID)
	if err != nil {
		return nil, fmt.Errorf("列賽事 %d 的場次: %w", tournamentID, err)
	}
	out := make([]match.Match, len(rows))
	for i, r := range rows {
		out[i] = toMatch(matchRow(r))
	}
	return out, nil
}

// MatchByPublicID 讀單場。查無回 activityerr.ErrMatchNotFound。
//
// 走既有的 GetMatchForJudge(無鎖重讀),不另寫一支:欄位就是對戰表要的全部,
// 而推播路徑(watchpg)讀的也是它 —— 同一個畫面上的同一個數字不該有兩條
// 產生路徑(鐵則 9)。名字裡的 ForJudge 是它第一個呼叫端留下的,不是權限語意。
func (s *Service) MatchByPublicID(ctx context.Context, matchPublicID string) (match.Match, error) {
	row, err := s.q.GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return match.Match{}, fmt.Errorf("match=%s: %w", matchPublicID, activityerr.ErrMatchNotFound)
		}
		return match.Match{}, fmt.Errorf("讀場次 %s: %w", matchPublicID, err)
	}
	return toMatch(matchRow(row)), nil
}

// CurrentMatchOfPlayer 回這位選手目前輪到要打的場次。
//
// nil, nil = 沒有待打的場次(已出局、已棄賽、或還沒抽籤)。**那不是錯誤** ——
// 「你現在沒有比賽」是選手頁一個完全正常的狀態,把它做成錯誤會逼呼叫端
// 用 errors.Is 去判斷一件本來就會發生的事。三種情況怎麼落到 0 列,見 SQL。
func (s *Service) CurrentMatchOfPlayer(ctx context.Context, playerID int64) (*match.Match, error) {
	row, err := s.q.CurrentMatchOfPlayer(ctx, playerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("讀選手 %d 的當前場次: %w", playerID, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// ── 跨屆檔案 ──────────────────────────────────────────────────

// Fencers 依 fencer id 批次取跨屆檔案。
//
// 查不到的 id 不出現在回傳的 map 裡,而且**不是錯誤**:呼叫端(裁判評段頁)
// 本來就要處理「這個人沒有跨屆資料」,把它做成錯誤會讓一個查不到的 id
// 害整頁打不開。
//
// 空輸入直接回空 map,不往資料庫走一趟 —— ListUnranked 在全員已評段時
// 會傳一個空切片進來,那是常態。
func (s *Service) Fencers(ctx context.Context, fencerIDs []int64) (map[int64]signup.Fencer, error) {
	out := make(map[int64]signup.Fencer, len(fencerIDs))
	if len(fencerIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.ListFencersByIDs(ctx, fencerIDs)
	if err != nil {
		return nil, fmt.Errorf("讀 %d 位選手的跨屆檔案: %w", len(fencerIDs), err)
	}
	for _, r := range rows {
		out[r.ID] = signuppg.FencerFromRow(r)
	}
	return out, nil
}
