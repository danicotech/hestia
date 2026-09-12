// Package bettingpg 是 betting.Repository[pgx.Tx] 的 Postgres 實作。
//
// # 這裡全部是真錢
//
// 下注、派彩、退款動的都是平台代幣,所以本套件比照 ledger-invariants:
//
//   - 一次操作的全部效果(注單、腿、帳本分錄、餘額、冪等鍵)都在**同一個 tx**。
//     InTx 開 tx、把它交給 core,core 回錯即 rollback —— 要嘛全在要嘛全不在。
//   - 本套件**沒有一行**碰 platform.token_entries / platform.user_balances。
//     動錢一律經 betting.Ledger 埠(實際是 ledgerpg.Service.ApplyInTx,同 tx)。
//     注單側只存三個分錄 id,那是稽核鏈的一半,不是餘額的第二個權威。
//   - 金額與賠率全程 int64。整個套件沒有 float。
//
// # 為什麼沒有 Ledger adapter
//
// betting.Ledger[TX] 在 TX 代入 pgx.Tx 之後,與 *ledgerpg.Service 的 ApplyInTx
// 簽名逐字元相同,所以 ledgerpg.Service 直接就滿足那個介面。
// 多寫一層包裝只會多一個會漂移的地方 —— New() 因此直接收 betting.Ledger[pgx.Tx]。
//
// # 鎖序(全系統同向,結構上不可能與購買/成交/簽到互鎖)
//
//	platform.users(LockUser)
//	  → activity.matches 的 advisory lock(LockMatchSettle)
//	  → activity.bets 列鎖(LockBets,**id 升冪**)
//	  → platform.user_balances(由 Ledger 取,user_id 升冪)
//
// 升冪取鎖的權威在 SQL(LockBetsForSettle 的 ORDER BY id),Go 這側一個排序都不做:
// 埠明文寫「呼叫端已排序;實作不得改順序」,在這裡再排一次就是第二個權威。
//
// # 冪等
//
// 冪等鍵沿用 platform.idempotency_keys(全系統唯一那張表),query 也沿用 ledger.sql
// 的三支,不為活動層另開一套。下注沒有任何 UNIQUE 約束擋得住連點(同一人對同一場
// 下兩注是合法的),所以 API 冪等鍵是唯一權威;鍵的命名空間前綴由 core 加。
//
// # 影響列數即斷言
//
// SetBetStakeEntry / UpdateBet / UpdateLegResults / InsertBetLegs 的 WHERE 條件都帶著
// 「這件事只能發生一次」的守衛(ledger_stake_entry_id IS NULL、status='open'、
// result='pending'、UNIQUE(bet_id, match_id))。影響列數與預期不符代表狀態已被別人動過,
// 一律回 betting.ErrLedgerStateConflict 失敗出聲 —— 絕不回成功但錢不對。
package bettingpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Repository 是 betting.Repository[pgx.Tx] 的實作。
//
// 沒有自己的 tx 欄位:每個方法都收 tx,q.WithTx(tx) 即用即拋。
// 這讓同一個 Repository 可以被多個併發請求共用,而每個請求的寫入都綁死在自己的 tx 上。
type Repository struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

var _ betting.Repository[pgx.Tx] = (*Repository)(nil)

// NewRepository 建立資料存取層。
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: db.New(pool)}
}

// New 組裝可直接使用的下注服務。
//
// led 傳 ledgerpg.New(pool) 的結果:*ledgerpg.Service 直接滿足 betting.Ledger[pgx.Tx],
// 中間沒有、也不需要任何 adapter(見 package doc)。
func New(pool *pgxpool.Pool, led betting.Ledger[pgx.Tx]) *betting.Service[pgx.Tx] {
	return betting.NewService[pgx.Tx](NewRepository(pool), led, betting.DefaultCurrency)
}

// ── transaction ───────────────────────────────────────────────────

// InTx 開一個 transaction 執行 fn。
//
// fn 回錯就不 commit,defer 的 Rollback 把這次的**全部**效果抹掉 ——
// 包含 Ledger 在同一個 tx 裡寫下的分錄與餘額變動(ApplyInTx 的原子性由呼叫端決定)。
// 「餘額不足時注單、腿、分錄、冪等鍵全部不留痕跡」就是靠這一句。
func (r *Repository) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	// Commit 之後的 Rollback 是 no-op(pgx 回 ErrTxClosed),所以無條件 defer 是安全的。
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		return err // 原樣上拋:core 的錯誤語意(4xx vs 重試)不該被這層改寫
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ── 鎖 ────────────────────────────────────────────────────────────

// LockMatchSettle 取同一場比賽的結算互斥鎖(pg_advisory_xact_lock)。
// 隨 tx 結束自動釋放 —— 這裡不提供、也不該提供手動解鎖。
func (r *Repository) LockMatchSettle(ctx context.Context, tx pgx.Tx, matchID int64) error {
	if err := r.q.WithTx(tx).LockMatchForSettle(ctx, matchID); err != nil {
		return fmt.Errorf("取結算鎖 match=%d: %w", matchID, err)
	}
	return nil
}

// LockUser 鎖住 platform.users 的那一列,把同一人的下注串行化。
//
// 查無列 = 使用者不存在或已軟刪除。回 ErrInvalidRequest 而不是自創一個錯誤:
// 動錢的對象不存在時,「請求本身不合法」就是這件事的全部語意,而 core 的錯誤清單
// 刻意沒有 ErrUserNotFound(下注者必有平台帳號,這是呼叫端先驗過的前提)。
func (r *Repository) LockUser(ctx context.Context, tx pgx.Tx, userID int64) error {
	if _, err := r.q.WithTx(tx).LockUserForBet(ctx, userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("user=%d 不存在或已停用: %w", userID, betting.ErrInvalidRequest)
		}
		return fmt.Errorf("鎖使用者 %d: %w", userID, err)
	}
	return nil
}

// LockBets 依 betIDs 升冪取注單列鎖。
//
// **刻意不排序、不去重**:埠的合約是「呼叫端已排序;實作不得改順序」,而真正的
// 保證在 SQL 的 ORDER BY id(LockRows 節點在 Sort 之上,鎖照輸出順序取)。
// 在 Go 這側再排一次,等於讓「取鎖順序」有兩個權威,哪天兩邊不一致就是死鎖。
//
// 空清單直接回:WHERE id = ANY('{}') 本來就不匹配任何列,少一次往返。
func (r *Repository) LockBets(ctx context.Context, tx pgx.Tx, betIDs []int64) error {
	if len(betIDs) == 0 {
		return nil
	}
	if err := r.q.WithTx(tx).LockBetsForSettle(ctx, betIDs); err != nil {
		return fmt.Errorf("鎖注單 %v: %w", betIDs, err)
	}
	return nil
}

// ── 賽事與場次 ────────────────────────────────────────────────────

// TournamentBySlug 找不到回 (nil, nil)。錯誤語意由 core 決定,這層不翻譯成
// ErrTournamentNotFound —— 實作端不必知道「查無列」對業務代表什麼。
func (r *Repository) TournamentBySlug(ctx context.Context, tx pgx.Tx, slug string) (*betting.Tournament, error) {
	row, err := r.q.WithTx(tx).BettingTournamentBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("讀賽事 %s: %w", slug, err)
	}
	return &betting.Tournament{ID: row.ID, Slug: row.Slug}, nil
}

// OddsConfig 讀賠率參數(tournaments.config)與單注上限(platform 的 economy_configs)。
//
// 兩個來源刻意不同:前五項是逐屆玩法旋鈕,max_stake 是平台層的風險控制 ——
// 兩邊都放會漂移,而漂移的症狀是「同一顆按鈕在不同賽事有不同上限」。
//
// 查無列回零值而不是錯誤:零值一律由 betting.OddsConfig.Normalize() 補成預設值,
// 那是「預設值長什麼樣」的唯一權威。這層多補一次就是第二份會漂移的預設值。
// PayoutToleranceBps 不讀:它不是 schemas/21 訂的設定鍵(見 activity_betting.sql)。
func (r *Repository) OddsConfig(ctx context.Context, tx pgx.Tx, tournamentID int64) (betting.OddsConfig, error) {
	row, err := r.q.WithTx(tx).BettingOddsConfig(ctx, tournamentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return betting.OddsConfig{}, nil
		}
		return betting.OddsConfig{}, fmt.Errorf("讀賠率參數 tournament=%d: %w", tournamentID, err)
	}
	return betting.OddsConfig{
		SmoothingVotes: row.SmoothingVotes,
		VigBps:         row.VigBps,
		MinOddsMilli:   row.MinOddsMilli,
		MaxOddsMilli:   row.MaxOddsMilli,
		MaxParlayMilli: row.MaxParlayMilli,
		MaxStake:       row.MaxStake,
	}, nil
}

// MatchesByPublicIDs 批次讀場次。回傳順序不拘(呼叫端自行用 public_id 索引)。
func (r *Repository) MatchesByPublicIDs(ctx context.Context, tx pgx.Tx, publicIDs []string) ([]betting.Match, error) {
	if len(publicIDs) == 0 {
		return nil, nil
	}
	rows, err := r.q.WithTx(tx).BettingMatchesByPublicIDs(ctx, publicIDs)
	if err != nil {
		return nil, fmt.Errorf("讀場次 %v: %w", publicIDs, err)
	}
	out := make([]betting.Match, 0, len(rows))
	for _, m := range rows {
		out = append(out, betting.Match{
			ID:             m.ID,
			PublicID:       m.PublicID,
			TournamentID:   m.TournamentID,
			Round:          int(m.Round),
			Slot:           int(m.Slot),
			Status:         betting.MatchStatus(m.Status),
			ResultKind:     betting.ResultKind(m.ResultKind),
			WinnerPlayerID: m.WinnerPlayerID,
			// 兩組 user_id 都帶上,「選手不得賭自己」的兩條比對路徑缺一不可
			// (只比 tournament_players 的話,沒綁帳號報名的選手就能賭自己輸)。
			P1: betting.Participant{
				PlayerID:     m.P1PlayerID,
				DisplayName:  m.P1DisplayName,
				PlayerUserID: m.P1PlayerUserID,
				FencerUserID: m.P1FencerUserID,
			},
			P2: betting.Participant{
				PlayerID:     m.P2PlayerID,
				DisplayName:  m.P2DisplayName,
				PlayerUserID: m.P2PlayerUserID,
				FencerUserID: m.P2FencerUserID,
			},
		})
	}
	return out, nil
}

// ── 投票 ──────────────────────────────────────────────────────────

// VoteTallies 數票。沒人投票的場次不會有列 —— map 查不到即零票,
// 不在這裡補零值進去:betting.Tally 的零值本來就是零票,補一次只是多一份假資料。
func (r *Repository) VoteTallies(ctx context.Context, tx pgx.Tx, matchIDs []int64) (map[int64]betting.Tally, error) {
	out := make(map[int64]betting.Tally, len(matchIDs))
	if len(matchIDs) == 0 {
		return out, nil
	}
	rows, err := r.q.WithTx(tx).VoteTalliesByMatch(ctx, matchIDs)
	if err != nil {
		return nil, fmt.Errorf("數票 %v: %w", matchIDs, err)
	}
	for _, t := range rows {
		out[t.MatchID] = betting.Tally{P1: t.P1Votes, P2: t.P2Votes}
	}
	return out, nil
}

// UpsertVote 一場一票,改票即覆蓋。權威是 votes_match_user_uq 這條 UNIQUE
// (SQL 的 ON CONFLICT DO UPDATE),不是應用層的「先查再決定 insert 還是 update」。
func (r *Repository) UpsertVote(ctx context.Context, tx pgx.Tx, matchID, userID int64, side betting.Side) error {
	err := r.q.WithTx(tx).UpsertVote(ctx, db.UpsertVoteParams{
		MatchID: matchID, UserID: userID, Side: int16(side),
	})
	if err != nil {
		return fmt.Errorf("寫入投票 match=%d user=%d: %w", matchID, userID, err)
	}
	return nil
}

// MyVotes 只查 userID 自己的票。這裡沒有、也不會有查別人投給誰的版本 ——
// 票數直接推導賠率,公開投票人等於公開可操縱的標的。
func (r *Repository) MyVotes(ctx context.Context, tx pgx.Tx, matchIDs []int64, userID int64) (map[int64]betting.Side, error) {
	out := make(map[int64]betting.Side, len(matchIDs))
	if len(matchIDs) == 0 || userID <= 0 {
		return out, nil
	}
	rows, err := r.q.WithTx(tx).MyVotesByMatch(ctx, db.MyVotesByMatchParams{
		UserID: userID, MatchIds: matchIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("讀自己的投票 user=%d: %w", userID, err)
	}
	for _, v := range rows {
		out[v.MatchID] = betting.Side(v.Side)
	}
	return out, nil
}

// ── 冪等鍵(沿用 platform.idempotency_keys 與 ledger.sql 的三支 query)──

// GetIdempotency 查不到回 (nil, nil)。
func (r *Repository) GetIdempotency(ctx context.Context, tx pgx.Tx, key string) (*betting.IdempotencyRecord, error) {
	row, err := r.q.WithTx(tx).GetIdempotencyKey(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("讀冪等鍵: %w", err)
	}
	return &betting.IdempotencyRecord{RequestHash: row.RequestHash, Response: row.Response}, nil
}

// InsertIdempotency 佔鍵(response 留 NULL,commit 前才由 SaveIdempotencyResponse 補上)。
//
// 撞 PK 轉成 ledger.ErrIdempotencyConflict:同一人的競態已經被 LockUser 擋掉,
// 走到這裡代表同一把鍵被**別人或別的內容**重用,那是 409 不是重試。
func (r *Repository) InsertIdempotency(ctx context.Context, tx pgx.Tx, key, requestHash string) error {
	err := r.q.WithTx(tx).InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{
		Key: key, RequestHash: requestHash,
	})
	if err != nil {
		if isUniqueViolation(err, "idempotency_keys_pkey") {
			return fmt.Errorf("key=%s: %w", key, ledger.ErrIdempotencyConflict)
		}
		return fmt.Errorf("佔冪等鍵: %w", err)
	}
	return nil
}

// SaveIdempotencyResponse 在 commit 前存入完整結果:commit 成功 ⇔ 鍵帶著結果存在。
func (r *Repository) SaveIdempotencyResponse(ctx context.Context, tx pgx.Tx, key string, response []byte) error {
	err := r.q.WithTx(tx).SetIdempotencyResponse(ctx, db.SetIdempotencyResponseParams{
		Key: key, Response: response,
	})
	if err != nil {
		return fmt.Errorf("存冪等結果: %w", err)
	}
	return nil
}

// ── 下注 ──────────────────────────────────────────────────────────

// InsertBet 建注單,回傳內部 id 與**資料庫寫下的** created_at。
// 時間由 DB 給:注單與帳本分錄的時間必須出自同一個時鐘,否則對帳時會看到
// 「分錄比注單早」這種查不出原因的排序。
func (r *Repository) InsertBet(ctx context.Context, tx pgx.Tx, b betting.NewBet) (int64, time.Time, error) {
	row, err := r.q.WithTx(tx).InsertBet(ctx, db.InsertBetParams{
		PublicID:        b.PublicID,
		TournamentID:    b.TournamentID,
		UserID:          b.UserID,
		Stake:           b.Stake,
		PotentialPayout: b.PotentialPayout,
	})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("建注單 %s: %w", b.PublicID, err)
	}
	return row.ID, row.CreatedAt, nil
}

// InsertLegs 一次寫入整張注單的腿(unnest 批次,一次往返、同一個敘述)。
//
// 三個平行陣列刻意在**同一個迴圈**組出來:拆成三個迴圈就讓「其中一個少一格」
// 這種寫法存在,而 unnest 會把短的補 NULL 撞上 NOT NULL —— 那還算好的,
// 更糟的是三者錯位,插進去的是一張賠率對到別場的注單。
//
// 撞 bet_legs_bet_match_uq(同一注單押同一場兩次 = 押完 p1 再押 p2 穩賺)一律失敗,
// **不做 ON CONFLICT**:少插一腿的注單會用多一腿算出來的賠付派彩。
// 轉成 ErrDuplicateLeg 只是給呼叫端可讀的語意,錯誤照樣中止整筆(tx 一起 rollback)。
func (r *Repository) InsertLegs(ctx context.Context, tx pgx.Tx, betID int64, legs []betting.NewLeg) error {
	if len(legs) == 0 {
		return fmt.Errorf("bet=%d 沒有任何一腿: %w", betID, betting.ErrInvalidRequest)
	}
	matchIDs := make([]int64, 0, len(legs))
	sides := make([]int16, 0, len(legs))
	odds := make([]int64, 0, len(legs))
	for _, l := range legs {
		matchIDs = append(matchIDs, l.MatchID)
		sides = append(sides, int16(l.Side))
		odds = append(odds, l.OddsMilli)
	}

	n, err := r.q.WithTx(tx).InsertBetLegs(ctx, db.InsertBetLegsParams{
		BetID: betID, MatchIds: matchIDs, Sides: sides, OddsMilli: odds,
	})
	if err != nil {
		if isUniqueViolation(err, "bet_legs_bet_match_uq") {
			return fmt.Errorf("bet=%d 重複押同一場: %w", betID, betting.ErrDuplicateLeg)
		}
		return fmt.Errorf("寫入注單 %d 的腿: %w", betID, err)
	}
	if n != int64(len(legs)) {
		// 走不到(unnest 不會少插);真的發生代表賠付與實際押注不一致,寧可整筆失敗。
		return fmt.Errorf("bet=%d 應插 %d 腿實插 %d: %w",
			betID, len(legs), n, betting.ErrLedgerStateConflict)
	}
	return nil
}

// SetBetStakeEntry 補上扣款分錄 id,完成稽核鏈(注單 ↔ 帳本雙向可查)。
//
// SQL 帶 ledger_stake_entry_id IS NULL:分錄 id 一旦寫下就是歷史,改掉它等於讓一筆
// 扣款換了一張注單。同 tx 內剛建的注單必定是 NULL,所以影響 0 列 = 狀態矛盾。
func (r *Repository) SetBetStakeEntry(ctx context.Context, tx pgx.Tx, betID, entryID int64) error {
	n, err := r.q.WithTx(tx).SetBetStakeEntry(ctx, db.SetBetStakeEntryParams{
		BetID: betID, EntryID: entryID,
	})
	if err != nil {
		return fmt.Errorf("回寫扣款分錄 id bet=%d: %w", betID, err)
	}
	if n == 0 {
		return fmt.Errorf("bet=%d 已有扣款分錄 id 或不存在: %w", betID, betting.ErrLedgerStateConflict)
	}
	return nil
}

// ── 結算 ──────────────────────────────────────────────────────────

// PendingLegsByMatch 撈出押到這場、還沒判定的腿(ORDER BY bet_id, id)。
// 順序穩定是必要的:呼叫端據此推出取鎖順序,而取鎖順序是防死鎖的全部。
func (r *Repository) PendingLegsByMatch(ctx context.Context, tx pgx.Tx, matchID int64) ([]betting.Leg, error) {
	rows, err := r.q.WithTx(tx).PendingLegsByMatch(ctx, matchID)
	if err != nil {
		return nil, fmt.Errorf("讀待判定的腿 match=%d: %w", matchID, err)
	}
	out := make([]betting.Leg, 0, len(rows))
	for _, l := range rows {
		// 展示欄位(match_public_id / round / slot / 顯示名)刻意留空:
		// 結算路徑用不到,為它多 join 四欄是白工。要展示的路徑走 LegsByBetIDs。
		out = append(out, betting.Leg{
			ID:        l.ID,
			BetID:     l.BetID,
			MatchID:   l.MatchID,
			Side:      betting.Side(l.Side),
			OddsMilli: l.OddsMilli,
			Result:    betting.LegResult(l.Result),
		})
	}
	return out, nil
}

// UpdateLegResults 把指定的腿標成同一個結果。
//
// SQL 帶 result = 'pending' 的重跑保護,所以影響列數必定等於 legIDs 的長度 ——
// 這些 id 全是同一個 tx 內、同一把 advisory lock 保護下剛讀出來的 pending 腿。
// 不相等代表有人在鎖外面改了腿,那是帳本對不上的起點,失敗出聲。
func (r *Repository) UpdateLegResults(ctx context.Context, tx pgx.Tx, legIDs []int64, result betting.LegResult) error {
	if len(legIDs) == 0 {
		return nil
	}
	n, err := r.q.WithTx(tx).UpdateLegResults(ctx, db.UpdateLegResultsParams{
		Result: string(result), LegIds: legIDs,
	})
	if err != nil {
		return fmt.Errorf("標記腿 %v = %s: %w", legIDs, result, err)
	}
	if n != int64(len(legIDs)) {
		return fmt.Errorf("應標記 %d 腿實際 %d(有腿已被判定): %w",
			len(legIDs), n, betting.ErrLedgerStateConflict)
	}
	return nil
}

// BetsByIDs 讀注單本體(不含腿)。呼叫前必須已 LockBets ——
// 派彩依賴的是**鎖之後**的 status。
func (r *Repository) BetsByIDs(ctx context.Context, tx pgx.Tx, betIDs []int64) ([]betting.Bet, error) {
	if len(betIDs) == 0 {
		return nil, nil
	}
	rows, err := r.q.WithTx(tx).BetsByIDs(ctx, betIDs)
	if err != nil {
		return nil, fmt.Errorf("讀注單 %v: %w", betIDs, err)
	}
	return toBets(rows), nil
}

// LegsByBetIDs 讀這些注單的**全部**腿(含已判定的),依 bet_id 分組。
//
// 不能只讀 pending:注單的狀態是所有腿的合成,只看這一場的腿會把串關的另一腿
// 當成不存在,一張還在等的注單會被判成全贏。
func (r *Repository) LegsByBetIDs(ctx context.Context, tx pgx.Tx, betIDs []int64) (map[int64][]betting.Leg, error) {
	out := make(map[int64][]betting.Leg, len(betIDs))
	if len(betIDs) == 0 {
		return out, nil
	}
	rows, err := r.q.WithTx(tx).LegsByBetIDs(ctx, betIDs)
	if err != nil {
		return nil, fmt.Errorf("讀注單的腿 %v: %w", betIDs, err)
	}
	for _, l := range rows {
		out[l.BetID] = append(out[l.BetID], betting.Leg{
			ID:              l.ID,
			BetID:           l.BetID,
			MatchID:         l.MatchID,
			MatchPublicID:   l.MatchPublicID,
			Round:           int(l.Round),
			Slot:            int(l.Slot),
			Side:            betting.Side(l.Side),
			SideDisplayName: l.SideDisplayName,
			OddsMilli:       l.OddsMilli,
			Result:          betting.LegResult(l.Result),
		})
	}
	return out, nil
}

// UpdateBet 套用結算/退款結果。
//
// 這一層**不算任何東西**:settled_at 由 SQL 的 CASE 依 status 決定(core 不該有
// 第二個時鐘),payout_recalculated 由 SQL 在 potential_payout 非 NULL 時一併設 true
// (兩者綁在同一句裡,就沒有「只更新金額忘了設旗標」的寫法存在)。
//
// SQL 帶 status = 'open':影響 0 列 = 這張注單已經被別人結算過。派彩是真錢,
// 這時候回成功就是重複派彩的起點 —— 一律失敗出聲。
func (r *Repository) UpdateBet(ctx context.Context, tx pgx.Tx, u betting.BetUpdate) error {
	n, err := r.q.WithTx(tx).UpdateBet(ctx, db.UpdateBetParams{
		BetID:           u.BetID,
		Status:          string(u.Status),
		PotentialPayout: u.PotentialPayout,
		RefundEntryID:   u.LedgerRefundEntryID,
		PayoutEntryID:   u.LedgerPayoutEntryID,
	})
	if err != nil {
		return fmt.Errorf("更新注單 %d: %w", u.BetID, err)
	}
	if n == 0 {
		return fmt.Errorf("bet=%d 已非 open(可能已被結算): %w", u.BetID, betting.ErrLedgerStateConflict)
	}
	return nil
}

// ── 列出自己的注單 ────────────────────────────────────────────────

// BetsByUser 列某人在某屆的注單(含腿),新到舊。
//
// openOnly 分派到**兩支不同的 query**,不是傳一個布林進 SQL:bets_open_idx 是
// WHERE status = 'open' 的部分索引,只有把述詞寫成字面值 planner 才用得上它。
// 寫成 (NOT $3 OR status='open') 的話,prepared statement 的通用計畫永遠退回全表過濾,
// 而已結算的注單會愈積愈多。
func (r *Repository) BetsByUser(ctx context.Context, tx pgx.Tx, tournamentID, userID int64, openOnly bool) ([]betting.Bet, error) {
	qtx := r.q.WithTx(tx)

	var rows []db.ActivityBet
	var err error
	if openOnly {
		rows, err = qtx.OpenBetsByUser(ctx, db.OpenBetsByUserParams{
			TournamentID: tournamentID, UserID: userID,
		})
	} else {
		rows, err = qtx.BetsByUser(ctx, db.BetsByUserParams{
			TournamentID: tournamentID, UserID: userID,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("讀注單 tournament=%d user=%d: %w", tournamentID, userID, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	bets := toBets(rows)
	ids := make([]int64, 0, len(bets))
	for _, b := range bets {
		ids = append(ids, b.ID)
	}
	// 一次把所有腿撈回來再分派,不是逐張回查(N+1 在列表頁就是一個人 30 次往返)。
	legs, err := r.LegsByBetIDs(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range bets {
		bets[i].Legs = legs[bets[i].ID]
	}
	return bets, nil
}

// ── 轉換與工具 ────────────────────────────────────────────────────

// toBets 把資料列轉成領域物件。三個分錄 id 在資料庫可為 NULL(那筆金流還沒發生),
// 領域型別用 0 表示「沒有」—— 0 不是合法的分錄 id,不需要指標。
func toBets(rows []db.ActivityBet) []betting.Bet {
	out := make([]betting.Bet, 0, len(rows))
	for _, b := range rows {
		out = append(out, betting.Bet{
			ID:                  b.ID,
			PublicID:            b.PublicID,
			UserID:              b.UserID,
			TournamentID:        b.TournamentID,
			Stake:               b.Stake,
			PotentialPayout:     b.PotentialPayout,
			Status:              betting.BetStatus(b.Status),
			PayoutRecalculated:  b.PayoutRecalculated,
			LedgerStakeEntryID:  deref(b.LedgerStakeEntryID),
			LedgerRefundEntryID: deref(b.LedgerRefundEntryID),
			LedgerPayoutEntryID: deref(b.LedgerPayoutEntryID),
			CreatedAt:           b.CreatedAt,
			SettledAt:           b.SettledAt,
		})
	}
	return out
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
