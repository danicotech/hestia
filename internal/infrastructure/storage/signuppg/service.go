// Package signuppg 是 signup.Repo 的 Postgres 實作。
//
// # 原子性邊界
//
// 一次報名的四個步驟(查/建 fencer、快照舊段位、建 tournament_players、
// 更新 fencer 統計)在同一個 transaction 裡,要嘛都在要嘛都不在。
// 中途失敗留下一個「有選手檔案但沒報上名」的孤兒 fencer,會在下一次報名時
// 被誤判成回鍋選手,而那個人其實從來沒參加過。
//
// 換發通行碼的資料變更與稽核紀錄同樣同 tx —— 通行碼被換掉卻查不到是誰換的,
// 等於給了一條沒有痕跡的帳號接管路徑。
//
// # 鎖序
//
// 與 tournamentpg 同一套(權威在 queries/activity_tournament.sql 的檔頭):
// 先鎖賽事列(報名/綁定/換通行碼都是一般寫入,用 FOR SHARE),
// 再依 tournament_players → fencers 的順序寫。
//
// FOR SHARE 彼此不互斥,所以並發報名不會被彼此卡住;但它與階段推進的 UPDATE
// 互斥 —— 「service 讀到 signup、寫入時裁判剛好封閉報名」這個時間差
// 在 tx 期間就被消掉了。
//
// # 通行碼雜湊的流向(2026-09-13 起只寫不讀)
//
// 這個檔案裡**沒有任何一支方法讀得到 passcode_hash**:登入只要遊戲ID,
// 沒有東西要比對。寫入還在(報名建一份、UpdatePasscode 換一份),因為欄位是
// NOT NULL,而且換發時一起前進的 passcode_issued_at 是選手 session 的作廢依據。
//
// 稽核紀錄裡**絕不**出現明碼或雜湊(port 明文要求):稽核紀錄是給人看的,
// 而通行碼雜湊出現在任何一個給人看的地方都是多一個外洩面。
package signuppg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/tournamentpg"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 稽核紀錄的 action 與 target_type 值(命名空間同 tournamentpg)。
const (
	actionRegeneratePasscode = "tournament.passcode_regenerated"
	targetPlayer             = "tournament_player"
)

// Service 是 signup.Repo 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// reads 提供 TournamentBySlug 與 PlayerByPublicID。
	//
	// 兩個 port 的這兩支簽名刻意完全相同(見 signup.go 的 port 註解),
	// 對應的也是同一句 SQL。委派過去而不是各寫一份,是因為同一個概念
	// 只能有一個權威位置(鐵則 9);介面各自宣告則是介面隔離,
	// 兩件事不衝突。
	reads *tournamentpg.Service
}

var _ signup.Repo = (*Service)(nil)

// New 建立報名儲存層。
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool), reads: tournamentpg.New(pool)}
}

// inTx 把 fn 包成一個 transaction。與 tournamentpg 同樣的理由:
// Service 不持有 tx,所以沒有巢狀交易這回事。
func (s *Service) inTx(ctx context.Context, fn func(qtx *db.Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// TournamentBySlug 查一屆賽事。查無回 tournament.ErrTournamentNotFound。
func (s *Service) TournamentBySlug(ctx context.Context, slug string) (tournament.Tournament, error) {
	return s.reads.TournamentBySlug(ctx, slug)
}

// PlayerByPublicID 在指定賽事內以 public_id 查選手。查無回 tournament.ErrPlayerNotFound。
func (s *Service) PlayerByPublicID(ctx context.Context, tournamentID int64, publicID string) (tournament.Player, error) {
	return s.reads.PlayerByPublicID(ctx, tournamentID, publicID)
}

// PasscodeIssuedAt 取這位選手目前的 passcode_issued_at,供選手 session 驗證用
// (session.Repo)。查無回 tournament.ErrPlayerNotFound。
//
// 「查無」把四種情況折成同一個錯誤:賽事不存在、選手不存在、public_id 不屬於
// 這一屆、**這個人已經不是參賽中**。這條路徑跑在未認證的請求上,分辨得出
// 哪一種就等於一支探測器。
//
// 最後那一種是「棄賽 = 鎖住這個人」成立的地方:登入不需要秘密,所以狀態是
// 唯一的門鎖,而只擋新登入的鎖要等 token 過期才生效。條件寫在 SQL 裡
// (見 PlayerPasscodeIssuedAt 的註解)。
//
// 回傳的是 DB 的值,一微秒都不加工 —— 它會與 token 裡的值逐微秒比對,
// 這裡若用本地時鐘補任何東西,結果是全屆選手隨機被登出。
func (s *Service) PasscodeIssuedAt(
	ctx context.Context, tournamentSlug, playerPublicID string,
) (time.Time, error) {
	at, err := s.q.PlayerPasscodeIssuedAt(ctx, db.PlayerPasscodeIssuedAtParams{
		Slug:           tournamentSlug,
		PlayerPublicID: playerPublicID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, fmt.Errorf("賽事 %s 的選手 %s: %w",
				tournamentSlug, playerPublicID, tournament.ErrPlayerNotFound)
		}
		return time.Time{}, fmt.Errorf("查通行碼簽發時間: %w", err)
	}
	return at, nil
}

// ── 報名 ──────────────────────────────────────────────────────

// CreateRegistration 寫入一次報名。
//
// 步驟順序不能動(見 query 檔的鎖序第 2 條):
//
//	鎖賽事(FOR SHARE)→ 查/建 fencer → 快照舊段位
//	→ 建 tournament_players(階段條件進 WHERE)→ 更新 fencer 統計
//
// 快照必須在建 tournament_players **之前**取:那之後 fencers.last_rank_level
// 仍是舊值沒錯,但把這件事寫成「順序無所謂」的程式碼,只要有人日後在中間
// 插入一支回寫 fencers 的語句,PreviousRank 就會悄悄變成本屆的值。
func (s *Service) CreateRegistration(ctx context.Context, p signup.CreateRegistrationParams) (signup.Registration, error) {
	var out signup.Registration
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		lock, err := qtx.LockTournamentShared(ctx, p.TournamentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("賽事 %d: %w", p.TournamentID, tournament.ErrTournamentNotFound)
			}
			return fmt.Errorf("鎖賽事 %d: %w", p.TournamentID, err)
		}

		fencer, returning, err := fencerForGameID(ctx, qtx, p.GameID)
		if err != nil {
			return err
		}
		// 接上檔案**之前**的跨屆段位。RankUnspecified = 初次參賽或從未評過。
		previousRank := bp.Rank(val(fencer.LastRankLevel))

		playerPublicID, err := ulid.New()
		if err != nil {
			return fmt.Errorf("產生選手 public_id: %w", err)
		}
		row, err := qtx.InsertTournamentPlayer(ctx, db.InsertTournamentPlayerParams{
			PublicID:         playerPublicID,
			TournamentID:     p.TournamentID,
			RequirePhase:     string(p.RequirePhase),
			FencerID:         fencer.ID,
			DisplayName:      p.DisplayName,
			DiscordName:      p.DiscordName,
			PasscodeHash:     p.PasscodeHash,
			SelfRatedLevel:   int16(p.SelfRatedRank),
			LadderRank:       p.LadderRank,
			LadderScore:      p.LadderScore,
			ArtsNote:         p.ArtsNote,
			AvailabilityNote: p.AvailabilityNote,
		})
		if err != nil {
			return registrationError(err, p, lock.Phase)
		}

		// 第 4 步必須排在第 3 步之後:插入成功才代表這一屆真的多參加了一次。
		// 反過來的話,撞 UNIQUE 被 rollback 的那一筆會在 rollback 前先卡住
		// 別人的 fencer 列。
		bumped, err := qtx.BumpFencerOnRegistration(ctx, db.BumpFencerOnRegistrationParams{
			DiscordName: p.DiscordName,
			FencerID:    fencer.ID,
		})
		if err != nil {
			return fmt.Errorf("更新選手 %d 的跨屆檔案: %w", fencer.ID, err)
		}

		out = signup.Registration{
			Player:          playerFromInsert(row, bumped.GameID),
			Fencer:          FencerFromRow(bumped),
			ReturningFencer: returning,
			PreviousRank:    previousRank,
		}
		return nil
	})
	return out, err
}

// fencerForGameID 取得(必要時建立)遊戲ID 對應的跨屆選手檔案。
//
// returning 表示這個遊戲ID 在本次之前就已經有檔案 —— 報名頁要靠它顯示
// 「歡迎回來」與往屆段位。
//
// 並發時兩個請求都會查不到而都想建,UNIQUE (game_id) 是唯一權威:
// InsertFencerIfAbsent 是 ON CONFLICT DO NOTHING RETURNING,撞鍵的那一邊
// 拿不到列,必須回頭重讀既有列繼續走完報名 —— 使用者只是按了報名鈕,
// 他沒有做錯任何事,不該收到一個撞鍵錯誤。
func fencerForGameID(ctx context.Context, qtx *db.Queries, gameID string) (db.ActivityFencer, bool, error) {
	fencer, err := qtx.GetFencerByGameID(ctx, gameID)
	if err == nil {
		return fencer, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.ActivityFencer{}, false, fmt.Errorf("查選手檔案 %s: %w", gameID, err)
	}

	publicID, err := ulid.New()
	if err != nil {
		return db.ActivityFencer{}, false, fmt.Errorf("產生選手檔案 public_id: %w", err)
	}
	fencer, err = qtx.InsertFencerIfAbsent(ctx, db.InsertFencerIfAbsentParams{
		PublicID: publicID, GameID: gameID,
	})
	if err == nil {
		return fencer, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.ActivityFencer{}, false, fmt.Errorf("建立選手檔案 %s: %w", gameID, err)
	}

	// DO NOTHING = 有人搶先建了(那一筆此時已 commit,否則我們還會卡在
	// 投機插入的鎖上)。重讀之後照樣走完報名。
	fencer, err = qtx.GetFencerByGameID(ctx, gameID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 撞鍵了卻讀不到,代表對手的 tx 回滾了而我們的插入早已放棄。
			// 這是可重試的短暫狀態,但不能假裝報名成功。
			return db.ActivityFencer{}, false, fmt.Errorf("選手檔案 %s 撞鍵後重讀不到,請重試一次", gameID)
		}
		return db.ActivityFencer{}, false, fmt.Errorf("重讀選手檔案 %s: %w", gameID, err)
	}
	return fencer, true, nil
}

// registrationError 把報名寫入的失敗翻成 port 的語意。
func registrationError(err error, p signup.CreateRegistrationParams, lockedPhase string) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// INSERT ... SELECT 的 WHERE phase = @require_phase 不成立。
		// lockedPhase 是取鎖後讀到的真值,拿它組訊息才說得出「現在是哪一階段」。
		return fmt.Errorf("賽事 %d 目前在 %s 階段,報名需要 %s: %w",
			p.TournamentID, lockedPhase, p.RequirePhase, tournament.ErrWrongPhase)
	case isUniqueViolation(err, "tournament_players_tournament_fencer_uq"):
		// 連點報名鈕由這條約束擋,不加冪等鍵(鐵則 9)。
		return fmt.Errorf("%s: %w", p.GameID, signup.ErrAlreadyRegistered)
	case isCheckViolation(err, "tournament_players_self_rated_level_check"):
		return fmt.Errorf("%w: 自評段位 %d", tournament.ErrInvalidRank, p.SelfRatedRank)
	default:
		return fmt.Errorf("寫入賽事 %d 的報名: %w", p.TournamentID, err)
	}
}

// ── 登入 ──────────────────────────────────────────────────────

// PlayerByGameID 依遊戲ID 取本屆的選手(登入用)。
//
// **不過濾 status**:只有 active 登得進來,但那條規則的權威在
// signup.Service.Login —— 它同時要把「非 active」與「查無此人」折成同一個答案,
// 而那件事只能有一個地方做得到(port 註解寫了同一句)。
//
// 查無回 ErrPlayerNotFound —— 呼叫端**不得**把這個錯誤原樣回給使用者
// (Login 會一律轉成 ErrInvalidCredentials)。錯誤訊息裡不放 game_id:
// 它最終會進伺服器日誌,而「誰報了名」在報名期間不是公開資訊。
func (s *Service) PlayerByGameID(ctx context.Context, tournamentID int64, gameID string) (tournament.Player, error) {
	row, err := s.q.GetPlayerByGameID(ctx, db.GetPlayerByGameIDParams{
		TournamentID: tournamentID, GameID: gameID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tournament.Player{}, fmt.Errorf("賽事 %d 查無此遊戲ID: %w",
				tournamentID, tournament.ErrPlayerNotFound)
		}
		return tournament.Player{}, fmt.Errorf("讀賽事 %d 的登入選手: %w", tournamentID, err)
	}
	// 欄位與 GetPlayerByPublicID 逐字相同,所以直接轉型交給唯一那份映射(鐵則 9)。
	return tournamentpg.PlayerFromRow(db.GetPlayerByPublicIDRow(row)), nil
}

// UpdatePasscode 換發通行碼 —— 現在的效果是把這位選手所有已發出的 session 踢掉
// (passcode_issued_at 一起前進,見 queries/activity_tournament.sql 的檔頭)。
func (s *Service) UpdatePasscode(ctx context.Context, p signup.UpdatePasscodeParams) error {
	// ActorUserID 必為非 0:admin_audit_logs.actor_user_id 是 NOT NULL 且
	// FK 到 platform.users,塞 0 會讓稽核 insert 失敗、把整筆換發一起 rollback。
	// core 已經擋過一次,這裡再擋是因為「沒有選手自助重取這條路」是這支方法的前提,
	// 而前提不該只寫在別人的檔案裡。
	if p.ActorUserID == 0 {
		return fmt.Errorf("換發通行碼: %w", tournament.ErrActorRequired)
	}
	return s.inTx(ctx, func(qtx *db.Queries) error {
		if _, err := qtx.LockTournamentShared(ctx, p.TournamentID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("賽事 %d: %w", p.TournamentID, tournament.ErrTournamentNotFound)
			}
			return fmt.Errorf("鎖賽事 %d: %w", p.TournamentID, err)
		}

		n, err := qtx.UpdatePlayerPasscode(ctx, db.UpdatePlayerPasscodeParams{
			PasscodeHash: p.PasscodeHash,
			PlayerID:     p.PlayerID,
			TournamentID: p.TournamentID,
		})
		if err != nil {
			return fmt.Errorf("換發選手 %d 的通行碼: %w", p.PlayerID, err)
		}
		if n == 0 {
			return fmt.Errorf("賽事 %d 的選手 %d: %w",
				p.TournamentID, p.PlayerID, tournament.ErrPlayerNotFound)
		}

		// after 只記「哪一屆的哪一位被換了碼」。明碼與雜湊都不進稽核紀錄 ——
		// 稽核是要能被人翻閱的,而通行碼雜湊不該出現在任何給人翻閱的地方。
		after, err := json.Marshal(map[string]any{
			"tournament_id": p.TournamentID,
			"player_id":     p.PlayerID,
		})
		if err != nil {
			return fmt.Errorf("序列化稽核內容: %w", err)
		}
		// 直接用 queries/audit.sql 的 InsertAdminAudit(與 admineconpg 同一種寫法):
		// 稽核紀錄怎麼寫只有那一個權威位置,這裡不再包一層自己的 helper。
		if _, err := qtx.InsertAdminAudit(ctx, db.InsertAdminAuditParams{
			ActorUserID: p.ActorUserID,
			Action:      actionRegeneratePasscode,
			TargetType:  ptr(targetPlayer),
			TargetID:    ptr(p.PlayerID),
			After:       after,
			Reason:      p.Reason,
		}); err != nil {
			return fmt.Errorf("寫稽核紀錄 %s: %w", actionRegeneratePasscode, err)
		}
		return nil
	})
}

// ── 綁定平台帳號 ──────────────────────────────────────────────

// BindPlatformAccount 把 user_id 同時寫進 tournament_players 與 fencers。
//
// 兩處都寫是必要的:只寫一處的話,領獎時查哪一張表會得到不同答案。
// 順序是 tournament_players → fencers(鎖序);反過來會與 SetPlayerRank
// 形成環 —— 裁判正在評某人的段,而那個人同時在綁帳號。
func (s *Service) BindPlatformAccount(ctx context.Context, p signup.BindParams) (tournament.Player, error) {
	if p.UserID == 0 {
		return tournament.Player{}, signup.ErrUserRequired
	}

	var out tournament.Player
	err := s.inTx(ctx, func(qtx *db.Queries) error {
		if _, err := qtx.LockTournamentShared(ctx, p.TournamentID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("賽事 %d: %w", p.TournamentID, tournament.ErrTournamentNotFound)
			}
			return fmt.Errorf("鎖賽事 %d: %w", p.TournamentID, err)
		}

		row, err := qtx.BindPlayerUser(ctx, db.BindPlayerUserParams{
			UserID: p.UserID, PlayerID: p.PlayerID, TournamentID: p.TournamentID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// WHERE 帶 (user_id IS NULL OR user_id = @user_id):綁同一個帳號
				// 是冪等的成功,所以 0 列有兩種可能 —— 已綁在別的帳號上,或查無此選手。
				//
				// 不靠「呼叫端應該先查過」來推論:那是對呼叫端行為的假設,
				// 而假設會過期。兩者對使用者的意思完全不同(前者要先解綁,
				// 後者是拿錯 id),猜錯就是把人導向錯的補救動作。
				if _, qerr := qtx.GetPlayerByID(ctx, db.GetPlayerByIDParams{
					TournamentID: p.TournamentID, ID: p.PlayerID,
				}); errors.Is(qerr, pgx.ErrNoRows) {
					return fmt.Errorf("選手 %d: %w", p.PlayerID, tournament.ErrPlayerNotFound)
				} else if qerr != nil {
					return fmt.Errorf("重讀選手 %d: %w", p.PlayerID, qerr)
				}
				return fmt.Errorf("選手 %d: %w", p.PlayerID, signup.ErrAlreadyBound)
			}
			if isUniqueViolation(err, "fencers_user_id_uq") {
				return fmt.Errorf("帳號 %d: %w", p.UserID, signup.ErrUserAlreadyBound)
			}
			return fmt.Errorf("綁定選手 %d: %w", p.PlayerID, err)
		}

		n, err := qtx.BindFencerUser(ctx, db.BindFencerUserParams{
			UserID: p.UserID, FencerID: p.FencerID,
		})
		if err != nil {
			// 這個帳號已經綁在別位 fencer 身上。一個帳號只能是一個人,
			// 否則獎金會匯給「兩個人共用的那個錢包」。
			if isUniqueViolation(err, "fencers_user_id_uq") {
				return fmt.Errorf("帳號 %d: %w", p.UserID, signup.ErrUserAlreadyBound)
			}
			return fmt.Errorf("綁定選手檔案 %d: %w", p.FencerID, err)
		}
		if n == 0 {
			// 這位 fencer 已綁在別的帳號上(跨屆的綁定與本屆不一致)。
			// 兩張表要一起改,所以整筆退回。
			return fmt.Errorf("選手檔案 %d: %w", p.FencerID, signup.ErrAlreadyBound)
		}

		out = tournamentpg.PlayerFromRow(db.GetPlayerByPublicIDRow(row))
		return nil
	})
	return out, err
}

// ── 列轉換 ────────────────────────────────────────────────────

// FencerFromRow 把 sqlc 的選手檔案列轉成 port 型別。
func FencerFromRow(r db.ActivityFencer) signup.Fencer {
	return signup.Fencer{
		ID:                r.ID,
		PublicID:          r.PublicID,
		GameID:            r.GameID,
		UserID:            r.UserID,
		DiscordName:       val(r.DiscordName),
		LastRankLevel:     bp.Rank(val(r.LastRankLevel)),
		LastRankedAt:      r.LastRankedAt,
		TournamentsPlayed: r.TournamentsPlayed,
		Wins:              r.Wins,
		Losses:            r.Losses,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

// playerFromInsert 把報名寫入的回傳列轉成 Player。
//
// InsertTournamentPlayer 不 JOIN fencers:插入的當下 game_id 已經在手上,
// 為了一個已知的值多 JOIN 一次是白工。所以這裡把它補回去,
// 再交給唯一那份轉換函式(鐵則 9)。
func playerFromInsert(r db.InsertTournamentPlayerRow, gameID string) tournament.Player {
	return tournamentpg.PlayerFromRow(db.GetPlayerByPublicIDRow{
		ID:               r.ID,
		PublicID:         r.PublicID,
		TournamentID:     r.TournamentID,
		FencerID:         r.FencerID,
		UserID:           r.UserID,
		DisplayName:      r.DisplayName,
		DiscordName:      r.DiscordName,
		RankLevel:        r.RankLevel,
		RankedAt:         r.RankedAt,
		RankedBy:         r.RankedBy,
		SelfRatedLevel:   r.SelfRatedLevel,
		LadderRank:       r.LadderRank,
		LadderScore:      r.LadderScore,
		ArtsNote:         r.ArtsNote,
		AvailabilityNote: r.AvailabilityNote,
		SeedNo:           r.SeedNo,
		Status:           r.Status,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		GameID:           gameID,
	})
}

// ── 小工具 ────────────────────────────────────────────────────

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == constraint
}

func ptr[T any](v T) *T { return &v }

func val[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
