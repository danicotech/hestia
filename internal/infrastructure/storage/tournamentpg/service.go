// Package tournamentpg 是 tournament.Repo 的 Postgres 實作。
//
// # 原子性邊界
//
// 每個會寫入的方法自成一個 transaction,而且**稽核紀錄寫在裡面**。
// 這是 port 註解明文要求的:另開一個 Auditor port 讓 service 事後補寫的話,
// 「資料改了但稽核沒記上」就是一個能真實發生的狀態 —— 而抽籤種子一旦漏記,
// 那張對戰表就再也無法向任何人證明它是怎麼抽出來的。綁在同一個 tx 裡,
// 這個狀態在結構上不存在。
//
// # 鎖序
//
// 權威是 queries/activity_tournament.sql 的檔頭,這裡只說明落點:
//
//  1. 先鎖賽事列。破壞性操作(抽籤、退回階段、交換籤位)用 FOR UPDATE,
//     讓這三者對同一屆賽事完全串行化;一般寫入(評段)用 FOR SHARE,
//     彼此不互斥但會擋住階段推進 —— 「檢查通過之後階段不會在腳下被換掉」
//     就是這麼來的。
//  2. 之後一律 tournament_players → fencers → matches。
//
// UpdatePhase 是唯一不先取鎖的寫入:它的權威是 WHERE phase = @from 這道樂觀鎖,
// 而 UPDATE 本身就會鎖住賽事列。並發的另一方在 READ COMMITTED 下會等鎖、
// 重新比對條件、影響 0 列,於是拿到 ErrPhaseConflict —— 那正是要的結果,
// 多取一次鎖只會讓同一件事有兩個做法。
//
// # 階段變更會在同一個 tx 裡發推播
//
// UpdatePhase 與 RollbackToRanked 都在自己那個 tx 裡呼叫 notifyPhase。
// 掛在 tx 內是這個設計的全部意義:NOTIFY 只在 commit 時送出,所以
// 「兩個裁判同時推階段、輸的那個被 rollback」不可能讓觀眾看到一個
// 沒有發生過的階段。
//
// # ULID 由這一層產生
//
// matches 的 public_id 走 internal/shared/ulid,SQL 不生 id(鐵則 5)。
package tournamentpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 稽核紀錄的 action 與 target_type 值。
//
// 全部加 "tournament." 前綴:admin_audit_logs 是全平台共用的一張表,
// 沒有命名空間的話 "draw" 這種字串遲早會與別的子系統撞在一起,
// 而稽核紀錄的用途正是事後撈出「某某動作做過幾次、誰做的」。
const (
	actionCreated      = "tournament.created"
	actionPhaseChanged = "tournament.phase_changed"
	actionRollback     = "tournament.rollback_to_ranked"
	actionDraw         = "tournament.draw"
	actionSwapSeeds    = "tournament.swap_seeds"
	actionSetRank      = "tournament.set_player_rank"

	targetTournament = "tournament"
	targetPlayer     = "tournament_player"
)

// Service 是 tournament.Repo 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

var _ tournament.Repo = (*Service)(nil)

// New 建立賽事儲存層。
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

// inTx 把 fn 包成一個 transaction,fn 收到的 *db.Queries 綁在該 tx 上。
//
// Service 刻意**不持有** pgx.Tx 欄位:本套件的每個方法都是一個完整的原子操作,
// 沒有「把兩個方法拼成更大的 tx」這種用法,於是巢狀呼叫這件事根本無從發生
// —— 沒有可以被巢狀的狀態,就不需要一道擋巢狀的檢查。
// 真要跨 schema 同 tx 動錢(報名獎金走 Ledger.ApplyInTx)時,那是組合的問題:
// 由持有 tx 的那一側把 tx 傳進來,而不是讓 Service 偷偷記住一個 tx。
func (s *Service) inTx(ctx context.Context, fn func(qtx *db.Queries) error) error {
	return s.inTxRaw(ctx, func(_ pgx.Tx, qtx *db.Queries) error { return fn(qtx) })
}

// inTxRaw 與 inTx 相同,但額外把 pgx.Tx 本身交出去。
//
// 只有 Create 需要它:複製讓武目錄要把 handicappg 綁在**這個** tx 上
// (handicappg.BindTx),而那個函式收的是 tx 不是 *db.Queries。
// 大多數方法不該拿到 tx —— 拿得到就有人會在裡面自己 Commit,
// 所以預設入口仍然是只給 *db.Queries 的 inTx。
func (s *Service) inTxRaw(ctx context.Context, fn func(tx pgx.Tx, qtx *db.Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx, s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ── 讀取 ──────────────────────────────────────────────────────

// TournamentBySlug 查一屆賽事。查無回 ErrTournamentNotFound。
func (s *Service) TournamentBySlug(ctx context.Context, slug string) (tournament.Tournament, error) {
	row, err := s.q.GetTournamentBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tournament.Tournament{}, fmt.Errorf("slug=%s: %w", slug, tournament.ErrTournamentNotFound)
		}
		return tournament.Tournament{}, fmt.Errorf("讀賽事 %s: %w", slug, err)
	}
	return TournamentFromRow(row)
}

// CountUnrankedActivePlayers 數還有幾位 active 選手沒評段。
func (s *Service) CountUnrankedActivePlayers(ctx context.Context, tournamentID int64) (int64, error) {
	n, err := s.q.CountUnrankedActivePlayers(ctx, tournamentID)
	if err != nil {
		return 0, fmt.Errorf("數賽事 %d 未評段選手: %w", tournamentID, err)
	}
	return n, nil
}

// ListDrawablePlayers 列出可以進抽籤的選手,依 id 遞增。
func (s *Service) ListDrawablePlayers(ctx context.Context, tournamentID int64) ([]tournament.Player, error) {
	rows, err := s.q.ListDrawablePlayers(ctx, tournamentID)
	if err != nil {
		return nil, fmt.Errorf("列賽事 %d 的可抽籤選手: %w", tournamentID, err)
	}
	out := make([]tournament.Player, len(rows))
	for i, r := range rows {
		out[i] = PlayerFromRow(db.GetPlayerByPublicIDRow(r))
	}
	return out, nil
}

// PlayerByPublicID 在指定賽事內以 public_id 查選手。查無回 ErrPlayerNotFound。
func (s *Service) PlayerByPublicID(ctx context.Context, tournamentID int64, publicID string) (tournament.Player, error) {
	row, err := s.q.GetPlayerByPublicID(ctx, db.GetPlayerByPublicIDParams{
		TournamentID: tournamentID, PublicID: publicID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tournament.Player{}, fmt.Errorf("賽事 %d 的選手 %s: %w",
				tournamentID, publicID, tournament.ErrPlayerNotFound)
		}
		return tournament.Player{}, fmt.Errorf("讀選手 %s: %w", publicID, err)
	}
	return PlayerFromRow(row), nil
}

// ── 階段 ──────────────────────────────────────────────────────

// UpdatePhase 推進階段。動不到列回 ErrPhaseConflict。
//
// 0 列也涵蓋「賽事不存在」,但這裡一律回 ErrPhaseConflict:呼叫端在這之前
// 必然已經讀過這屆賽事(轉換合法性要由讀到的 from 決定),所以走到這裡
// 而動不到列,現實中的成因只有一個 —— 有人搶先改了階段。
func (s *Service) UpdatePhase(ctx context.Context, p tournament.UpdatePhaseParams) error {
	if p.ActorUserID == 0 {
		return fmt.Errorf("推進階段: %w", tournament.ErrActorRequired)
	}
	return s.inTx(ctx, func(qtx *db.Queries) error {
		slug, err := s.advancePhase(ctx, qtx, p.TournamentID, p.From, p.To)
		if err != nil {
			return err
		}
		if err := writeAudit(ctx, qtx, auditEntry{
			ActorUserID: p.ActorUserID,
			Action:      actionPhaseChanged,
			TargetType:  targetTournament,
			TargetID:    p.TournamentID,
			Before:      map[string]any{"phase": string(p.From)},
			After:       map[string]any{"phase": string(p.To)},
			Reason:      p.Reason,
		}); err != nil {
			return err
		}
		return notifyPhase(ctx, qtx, slug)
	})
}

// advancePhase 下那一句樂觀鎖 UPDATE,回傳賽事 slug(推播的路由鍵)。
//
// 抽出來是因為 RollbackToRanked 走的是同一句:退回 ranked 也是一次階段變更,
// 它同樣要寫稽核、同樣要推播。兩處各寫一遍的話,漏掉的那一邊不會有任何
// 錯誤訊息 —— 只是那個階段變化在觀眾的畫面上不會出現。
func (s *Service) advancePhase(ctx context.Context, qtx *db.Queries,
	tournamentID int64, from, to tournament.Phase,
) (string, error) {
	slug, err := qtx.UpdateTournamentPhase(ctx, db.UpdateTournamentPhaseParams{
		TournamentID: tournamentID,
		FromPhase:    string(from),
		ToPhase:      string(to),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("賽事 %d %s→%s: %w",
				tournamentID, from, to, tournament.ErrPhaseConflict)
		}
		return "", fmt.Errorf("推進賽事 %d 階段: %w", tournamentID, err)
	}
	return slug, nil
}

// notifyPhase 在**呼叫端的 tx 內**送出一則階段變化的推播信封。
//
// 必須在這個 tx 裡,不能挪到 commit 之後:NOTIFY 只在 commit 時送出,
// rollback 的交易一個字都不送(internal/core/activity/watch 的檔頭)——
// 觀眾因此不可能看到一個後來被撤銷的階段。挪出去就失去這個唯一的理由。
//
// 信封的 Ref 留空:階段變化的對象是賽事本身,而賽事已經由 Tournament(slug)
// 指定了。watch.Envelope.Valid 對 KindPhase 特別允許空 Ref,就是為了這一則。
//
// 失敗一律讓整筆階段推進失敗(而不是吞掉):pg_notify 出錯代表這個 transaction
// 已經被 Postgres 標成 aborted,後面每一句都只會回「current transaction is
// aborted」—— 吞掉只會把成因換成一個看不懂的錯誤。
// 形狀照 matchpg 的 notifyWatch,不另造一套信封。
func notifyPhase(ctx context.Context, qtx *db.Queries, slug string) error {
	payload, err := watch.Envelope{Tournament: slug, Kind: watch.KindPhase}.Marshal()
	if err != nil {
		return fmt.Errorf("組推播信封 slug=%s: %w", slug, err)
	}
	if err := qtx.NotifyActivityWatch(ctx, db.NotifyActivityWatchParams{
		Channel: watch.Channel,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("送階段推播 slug=%s: %w", slug, err)
	}
	return nil
}

// ── 評段 ──────────────────────────────────────────────────────

// SetPlayerRank 寫入本屆段位、回寫跨屆快照,並回傳更新後的選手。
func (s *Service) SetPlayerRank(ctx context.Context, p tournament.SetPlayerRankParams) (tournament.Player, error) {
	if p.ActorUserID == 0 {
		return tournament.Player{}, fmt.Errorf("評定段位: %w", tournament.ErrActorRequired)
	}
	// 段位不合法時擋在這裡而不是丟給 CHECK:兩者都擋得住,但 CHECK 失敗會讓
	// 整筆 tx 進入 aborted 狀態,而且錯誤訊息是資料庫的用語,裁判看不懂。
	if !p.Rank.Valid() {
		return tournament.Player{}, fmt.Errorf("%w: %d", tournament.ErrInvalidRank, p.Rank)
	}

	var out tournament.Player
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		// 鎖序第 1 步(一般寫入版):FOR SHARE 讓評段與報名可以並發,
		// 但擋住階段推進 —— service 判定「現在還能改段位」之後,
		// 不會有人在這個 tx 進行中把賽事推進 drawing。
		if _, err := qtx.LockTournamentShared(ctx, p.TournamentID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("賽事 %d: %w", p.TournamentID, tournament.ErrTournamentNotFound)
			}
			return fmt.Errorf("鎖賽事 %d: %w", p.TournamentID, err)
		}

		// 先讀舊段位:改段位多半是異議流程的結果,而「原本評幾段」是日後
		// 有人不服氣時唯一查得到的東西 —— UPDATE 一下去它就不存在了。
		// 順便也把「查無此選手」提前到這裡分辨,下面那支 UPDATE 的 0 列因此
		// 只剩理論上的可能(同 tx 內剛讀到的列不會憑空消失)。
		prevRow, err := qtx.GetPlayerByID(ctx, db.GetPlayerByIDParams{
			TournamentID: p.TournamentID, ID: p.PlayerID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("賽事 %d 的選手 %d: %w",
					p.TournamentID, p.PlayerID, tournament.ErrPlayerNotFound)
			}
			return fmt.Errorf("讀選手 %d 的原段位: %w", p.PlayerID, err)
		}
		// GetPlayerByID 與 GetPlayerByPublicID 的列逐欄相同,所以共用同一份映射;
		// 哪天兩支分歧,這個結構轉換會在編譯期爆掉(見 PlayerFromRow)。
		prev := PlayerFromRow(db.GetPlayerByPublicIDRow(prevRow))

		row, err := qtx.SetPlayerRank(ctx, db.SetPlayerRankParams{
			RankLevel:    int16(p.Rank),
			RankedBy:     p.ActorUserID,
			PlayerID:     p.PlayerID,
			TournamentID: p.TournamentID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("賽事 %d 的選手 %d: %w",
					p.TournamentID, p.PlayerID, tournament.ErrPlayerNotFound)
			}
			if isCheckViolation(err, "tournament_players_rank_level_check") {
				return fmt.Errorf("%w: %d", tournament.ErrInvalidRank, p.Rank)
			}
			return fmt.Errorf("寫入選手 %d 段位: %w", p.PlayerID, err)
		}

		// 鎖序第 2 步:tournament_players 之後才動 fencers。
		// 這是跨屆快照的增量更新路徑,真值隨時可用 RecalcFencerStats 對帳。
		n, err := qtx.TouchFencerLastRank(ctx, db.TouchFencerLastRankParams{
			RankLevel: int16(p.Rank),
			PlayerID:  p.PlayerID,
		})
		if err != nil {
			return fmt.Errorf("回寫選手 %d 的跨屆段位: %w", p.PlayerID, err)
		}
		if n != 1 {
			// 上一句才剛更新到這一列,對應的 fencer 不可能不存在(FK 保證)。
			// 真的發生代表資料出了更大的問題,寧可整筆退回也不要留下半套。
			return fmt.Errorf("回寫選手 %d 的跨屆段位影響 %d 列,預期 1 列", p.PlayerID, n)
		}

		if err := writeAudit(ctx, qtx, auditEntry{
			ActorUserID: p.ActorUserID,
			Action:      actionSetRank,
			TargetType:  targetPlayer,
			TargetID:    p.PlayerID,
			// before 只記 rank_level:這筆稽核回答的問題就是「段位從幾變成幾」,
			// 把整列快照塞進去只會讓真正有爭議的那一欄淹在二十個沒動過的欄位裡。
			// 未評段的人舊值是 0(bp.RankUnspecified),那也是事實的一部分。
			Before: map[string]any{
				"rank_level": int(prev.Rank),
			},
			After: map[string]any{
				"tournament_id": p.TournamentID,
				"player_id":     p.PlayerID,
				"rank_level":    int(p.Rank),
			},
			Reason: p.Reason,
		}); err != nil {
			return err
		}
		out = PlayerFromRow(db.GetPlayerByPublicIDRow(row))
		return nil
	})
	return out, err
}

// ── 列轉換 ────────────────────────────────────────────────────

// PlayerFromRow 把 sqlc 的選手列轉成 port 型別。
//
// 匯出是給 signuppg 用的:兩個 port 的 PlayerByPublicID 簽名刻意相同,
// 而「怎麼把一列變成 Player」只該有一份(鐵則 9)。
//
// 其餘回傳選手的 query(GetPlayerByID / ListDrawablePlayers / SetPlayerRank / BindPlayerUser)
// 的列型別欄位完全一致,直接用 Go 的結構轉換餵進來 —— 那個轉換只在欄位名稱、
// 型別、順序三者都相同時才編得過,所以它不可能把兩欄接錯;哪天 sqlc 換了欄位
// 順序,build 會當場失敗而不是靜靜地錯。
func PlayerFromRow(r db.GetPlayerByPublicIDRow) tournament.Player {
	return tournament.Player{
		ID:               r.ID,
		PublicID:         r.PublicID,
		TournamentID:     r.TournamentID,
		FencerID:         r.FencerID,
		UserID:           r.UserID,
		GameID:           r.GameID,
		DisplayName:      r.DisplayName,
		DiscordName:      r.DiscordName,
		Rank:             bp.Rank(val(r.RankLevel)),
		RankedAt:         r.RankedAt,
		RankedBy:         r.RankedBy,
		SelfRatedRank:    bp.Rank(val(r.SelfRatedLevel)),
		LadderRank:       val(r.LadderRank),
		LadderScore:      val(r.LadderScore),
		ArtsNote:         val(r.ArtsNote),
		AvailabilityNote: val(r.AvailabilityNote),
		SeedNo:           val(r.SeedNo),
		Status:           tournament.PlayerStatus(r.Status),
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

// TournamentFromRow 把賽事列轉成 port 型別。
//
// 匯出的理由與 PlayerFromRow 相同:讀取側(activityreadpg)也要把
// activity.tournaments 的一列變成 tournament.Tournament,而「怎麼變」
// ——尤其是 phase 解不開時要整筆失敗而不是落成零值——只該有一份(鐵則 9)。
// GetTournamentByID / GetTournamentByPlayerPublicID 的欄位與 GetTournamentBySlug
// 逐字相同,sqlc 因此把三支都生成同一個 db.ActivityTournament。
func TournamentFromRow(r db.ActivityTournament) (tournament.Tournament, error) {
	phase, err := tournament.ParsePhase(r.Phase)
	if err != nil {
		// DB 的 CHECK 擋得住這件事,走到這裡代表 migration 與 Phase 枚舉分岔了。
		// 回一個零值階段的話,後面每一道 RequirePhase 都會失敗得莫名其妙。
		return tournament.Tournament{}, fmt.Errorf("賽事 %d 的階段 %q: %w", r.ID, r.Phase, err)
	}
	return tournament.Tournament{
		ID:          r.ID,
		PublicID:    r.PublicID,
		Slug:        r.Slug,
		Name:        r.Name,
		CommunityID: r.CommunityID,
		Phase:       phase,
		ConfigRaw:   r.Config,
		SignupBonus: r.SignupBonus,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}, nil
}

// ── 稽核 ──────────────────────────────────────────────────────

// auditEntry 是一筆稽核紀錄的內容。Before / After 傳 nil 表示那一欄不寫。
type auditEntry struct {
	ActorUserID int64
	Action      string
	TargetType  string
	TargetID    int64
	Before      any
	After       any
	Reason      string
}

// writeAudit 在**呼叫端的 tx 內**寫 platform.admin_audit_logs。
//
// 用 queries/audit.sql 既有的 InsertAdminAudit,不為活動層另寫一支 insert:
// 稽核紀錄怎麼寫只能有一個權威位置(鐵則 9)。
//
// actor_user_id 是 NOT NULL 且 FK 到 platform.users,塞 0 會讓整筆 transaction
// 連同資料變更一起 rollback —— 所以每個入口都先擋掉 0(ErrActorRequired),
// 走到這裡必為非 0。
func writeAudit(ctx context.Context, qtx *db.Queries, e auditEntry) error {
	before, err := marshalAudit(e.Before)
	if err != nil {
		return err
	}
	after, err := marshalAudit(e.After)
	if err != nil {
		return err
	}
	if _, err := qtx.InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: e.ActorUserID,
		Action:      e.Action,
		TargetType:  ptr(e.TargetType),
		TargetID:    ptr(e.TargetID),
		Before:      before,
		After:       after,
		Reason:      e.Reason,
	}); err != nil {
		return fmt.Errorf("寫稽核紀錄 %s: %w", e.Action, err)
	}
	return nil
}

func marshalAudit(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化稽核內容: %w", err)
	}
	return raw, nil
}

// ── 小工具 ────────────────────────────────────────────────────

// isCheckViolation 判斷是不是指定的 CHECK 約束把寫入擋下來。
func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == constraint
}

// isUniqueViolation 判斷是不是指定的 UNIQUE 約束把寫入擋下來。
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// isNotNullViolation 判斷是不是指定欄位的 NOT NULL 把寫入擋下來。
//
// 用在 InsertTournament:社群的 public_id 查不到時,子查詢回 NULL 而不是 0 列,
// 於是失敗發生在 community_id 的 NOT NULL 上。分辨得出來才能回
// ErrCommunityNotFound —— 否則裁判只會看到一句「內部錯誤」。
func isNotNullViolation(err error, column string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23502" && pgErr.ColumnName == column
}

func ptr[T any](v T) *T { return &v }

// val 取指標的值,nil 取零值。
//
// 幾個可空欄位在 port 裡刻意是值型別(0 / "" 就是「沒有」的語意),
// 轉換時每一欄各寫一次 if 只會把真正的邏輯淹掉。
func val[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
