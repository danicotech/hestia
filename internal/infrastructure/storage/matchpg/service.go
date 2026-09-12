// Package matchpg 是 match.Repository[pgx.Tx] 的 Postgres 實作。
//
// # 一個裁判動作 = 一個 transaction
//
// 六個裁判動作(開盤、封盤、開打、判勝負、棄賽、設直播)每一個都牽動三到五個
// 套件。InTx 開的那一個 transaction 就是「要嘛全發生、要嘛全不發生」的範圍:
// 場次狀態、淘汰、跨屆戰績、讓武、下注派彩、稽核、outbox 全掛在它身上。
//
// # 為什麼每個入口都先取 advisory lock
//
// 兩個入口的列鎖方向天生相反(activity_match.sql 檔頭):
//
//	ReportResult    matches → tournament_players → fencers
//	WithdrawPlayer  tournament_players → matches
//
// 兩位裁判同時對同一場的兩位選手做這兩件事,就是一個真實可達的 40P01。
// 所以 LockMatch / LockPlayer 的第一句一律是該屆的 pg_advisory_xact_lock,
// 讓同屆的裁判動作彼此完全串行化。**漏掉不會有任何可見症狀** ——
// 只會在裁判併發操作時偶發死鎖,而那時候現場沒有人有辦法重現。
//
// # 稽核與領域變更寫在同一個方法裡
//
// 埠刻意不另開 Auditor:MarkReady / MarkLive / MarkFinished / SetStreamURL 與
// 「只有 withdrawn 的」SetPlayerStatus 各自在同一個 tx 裡緊接著寫
// platform.admin_audit_logs。分開的話,「資料改了但稽核沒記上」就是一個能真實
// 發生的狀態,而賽事的公正性完全建立在「誰改了什麼查得到」這件事上。
//
// SeatPlayer 與 BumpFencerRecord 不寫稽核:它們是判定勝負的**後果**,理由已經
// 在 MarkFinished 那筆裡。為後果各記一筆只會讓稽核表一半的列沒有資訊量。
package matchpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// ErrSeatUnavailable 表示晉級目標的那一側寫不進去:目標場次已經不是 pending,
// 或該側已經坐了人。
//
// 這是資料完整性失效而不是使用者錯誤(對戰表被動過,或算出的側別與建樹時不一致),
// 所以不對應 match 套件的任何 sentinel —— 它要做的事只有一件:讓整筆動作失敗。
// 默默跳過的後果是勝者從對戰表上消失,而且沒有人會發現。
var ErrSeatUnavailable = errors.New("晉級目標場次的該側無法寫入")

// Service 是 match.Repository[pgx.Tx] 的實作。
//
// 沒有任何跨呼叫的狀態:每個方法都收 tx,q.WithTx(tx) 即用即拋,
// 所以同一個 Service 可以被多個併發請求共用。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

var _ match.Repository[pgx.Tx] = (*Service)(nil)

// New 建立服務。
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

// InTx 開一個 transaction 執行 fn。fn 回錯即 rollback —— 連同讓武封盤、
// 下注派彩、稽核與 outbox 事件一併消失。
func (s *Service) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ── 讀取 ────────────────────────────────────────────────────────

// LockMatch 讀一場比賽並鎖住該列。第一句是該屆的 advisory lock(見 package doc)。
func (s *Service) LockMatch(ctx context.Context, tx pgx.Tx, matchPublicID string) (*match.Match, error) {
	q := s.q.WithTx(tx)
	if err := q.LockTournamentForJudgingByMatch(ctx, matchPublicID); err != nil {
		return nil, fmt.Errorf("取賽事裁判鎖 match=%s: %w", matchPublicID, err)
	}
	row, err := q.LockMatchForJudge(ctx, matchPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match=%s: %w", matchPublicID, match.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("鎖場次 %s: %w", matchPublicID, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// LockMatchAt 依位置讀晉級目標並鎖住該列。
//
// 不再取 advisory lock:呼叫端必定已經在同一個 tx 裡經 LockMatch / LockPlayer
// 取過同一屆的鎖(晉級目標與來源同屆),重複取只是多一次無謂的往返。
func (s *Service) LockMatchAt(ctx context.Context, tx pgx.Tx, tournamentID int64, round, slot int) (*match.Match, error) {
	row, err := s.q.WithTx(tx).LockMatchAtForJudge(ctx, db.LockMatchAtForJudgeParams{
		TournamentID: tournamentID,
		Round:        int32(round),
		Slot:         int32(slot),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("tournament=%d round=%d slot=%d: %w",
				tournamentID, round, slot, match.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("鎖場次 tournament=%d round=%d slot=%d: %w",
			tournamentID, round, slot, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// GetMatch 無鎖重讀。封盤那句 UPDATE 由 handicap 套件下,回來要看得到它寫的樣子。
func (s *Service) GetMatch(ctx context.Context, tx pgx.Tx, matchPublicID string) (*match.Match, error) {
	row, err := s.q.WithTx(tx).GetMatchForJudge(ctx, matchPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match=%s: %w", matchPublicID, match.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("讀場次 %s: %w", matchPublicID, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// TournamentByID 讀賽事(含 TotalRounds),並以 FOR SHARE 把階段釘住。
func (s *Service) TournamentByID(ctx context.Context, tx pgx.Tx, tournamentID int64) (*match.Tournament, error) {
	row, err := s.q.WithTx(tx).JudgeTournamentByID(ctx, tournamentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("tournament=%d: %w", tournamentID, tournament.ErrTournamentNotFound)
		}
		return nil, fmt.Errorf("讀賽事 %d: %w", tournamentID, err)
	}
	return &match.Tournament{
		ID:          row.ID,
		PublicID:    row.PublicID,
		Slug:        row.Slug,
		Name:        row.Name,
		Phase:       tournament.Phase(row.Phase),
		ConfigRaw:   row.Config,
		TotalRounds: int(row.TotalRounds),
	}, nil
}

// LockPlayer 讀參賽者並鎖住該列。棄賽路徑的入口,所以先取該屆的 advisory lock。
func (s *Service) LockPlayer(ctx context.Context, tx pgx.Tx, playerPublicID string) (*match.Player, error) {
	q := s.q.WithTx(tx)
	if err := q.LockTournamentForJudgingByPlayer(ctx, playerPublicID); err != nil {
		return nil, fmt.Errorf("取賽事裁判鎖 player=%s: %w", playerPublicID, err)
	}
	row, err := q.LockPlayerForJudge(ctx, playerPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("player=%s: %w", playerPublicID, tournament.ErrPlayerNotFound)
		}
		return nil, fmt.Errorf("鎖選手 %s: %w", playerPublicID, err)
	}
	p := toPlayer(playerRow(row))
	return &p, nil
}

// LockUnfinishedMatchesOfPlayer 列出並鎖住這位選手所有未完賽的場次,依 (round, slot) 遞增。
func (s *Service) LockUnfinishedMatchesOfPlayer(ctx context.Context, tx pgx.Tx, playerID int64) ([]match.Match, error) {
	rows, err := s.q.WithTx(tx).LockUnfinishedMatchesOfPlayer(ctx, playerID)
	if err != nil {
		return nil, fmt.Errorf("讀未完賽場次 player=%d: %w", playerID, err)
	}
	out := make([]match.Match, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMatch(matchRow(r)))
	}
	return out, nil
}

// ── 狀態轉移 ────────────────────────────────────────────────────

// MarkReady 開盤:pending → ready、handicap_open = true,並寫稽核。
func (s *Service) MarkReady(ctx context.Context, tx pgx.Tx, w match.ReadyWrite) (*match.Match, error) {
	q := s.q.WithTx(tx)
	row, err := q.MarkMatchReady(ctx, w.MatchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 0 列 = 場次不存在、或已經開過盤。後者必須失敗:
			// 默默成功等於再對低段位者吵一次開盤提醒。
			return nil, s.attribute(ctx, q, w.MatchID, match.ErrAlreadyOpen)
		}
		return nil, fmt.Errorf("開盤 match=%d: %w", w.MatchID, err)
	}
	m := toMatch(matchRow(row))
	if err := s.auditMatch(ctx, q, w.ActorUserID, match.ActionOpenHandicap, m, w.Reason); err != nil {
		return nil, err
	}
	return &m, nil
}

// MarkLive 開打:locked → live、started_at = now(),並寫稽核。
func (s *Service) MarkLive(ctx context.Context, tx pgx.Tx, w match.LiveWrite) (*match.Match, error) {
	q := s.q.WithTx(tx)
	row, err := q.MarkMatchLive(ctx, w.MatchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, s.attribute(ctx, q, w.MatchID, match.ErrNotLocked)
		}
		return nil, fmt.Errorf("開打 match=%d: %w", w.MatchID, err)
	}
	m := toMatch(matchRow(row))
	if err := s.auditMatch(ctx, q, w.ActorUserID, match.ActionStartMatch, m, w.Reason); err != nil {
		return nil, err
	}
	return &m, nil
}

// MarkFinished 判定勝負(勝者 / done / finished_at / result_kind 同一句),並寫稽核。
func (s *Service) MarkFinished(ctx context.Context, tx pgx.Tx, w match.FinishWrite) (*match.Match, error) {
	q := s.q.WithTx(tx)
	row, err := q.MarkMatchFinished(ctx, db.MarkMatchFinishedParams{
		WinnerPlayerID: w.WinnerPlayerID,
		ResultKind:     string(w.ResultKind),
		MatchID:        w.MatchID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 守門是 status <> done,所以 0 列只可能是重複判定(或場次不存在)。
			return nil, s.attribute(ctx, q, w.MatchID, match.ErrMatchFinished)
		}
		return nil, fmt.Errorf("判定勝負 match=%d: %w", w.MatchID, err)
	}
	m := toMatch(matchRow(row))
	if err := s.auditMatch(ctx, q, w.ActorUserID, match.ActionReportResult, m, w.Reason); err != nil {
		return nil, err
	}
	return &m, nil
}

// SetStreamURL 設定或清除直播連結,並寫稽核。空字串在 SQL 側 NULLIF 成 NULL。
func (s *Service) SetStreamURL(ctx context.Context, tx pgx.Tx, w match.StreamWrite) (*match.Match, error) {
	q := s.q.WithTx(tx)
	row, err := q.SetMatchStreamURL(ctx, db.SetMatchStreamURLParams{
		StreamUrl: w.StreamURL,
		MatchID:   w.MatchID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 這一支沒有狀態閘門(賽後補 VOD 是常態),0 列只能是場次不存在。
			return nil, fmt.Errorf("match id=%d: %w", w.MatchID, match.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("設定直播連結 match=%d: %w", w.MatchID, err)
	}
	m := toMatch(matchRow(row))
	if err := s.auditMatch(ctx, q, w.ActorUserID, match.ActionSetStreamURL, m, w.Reason); err != nil {
		return nil, err
	}
	return &m, nil
}

// SeatPlayer 把晉級者寫進目標場次的一側。**不寫稽核**(晉級是判定勝負的後果)。
func (s *Service) SeatPlayer(ctx context.Context, tx pgx.Tx, w match.SeatWrite) (*match.Match, error) {
	q := s.q.WithTx(tx)
	row, err := q.SeatMatchPlayer(ctx, db.SeatMatchPlayerParams{
		IsP1:     w.IsP1,
		PlayerID: w.PlayerID,
		MatchID:  w.MatchID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, s.attribute(ctx, q, w.MatchID, ErrSeatUnavailable)
		}
		return nil, fmt.Errorf("寫入晉級 match=%d player=%d: %w", w.MatchID, w.PlayerID, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// SetPlayerStatus 改參賽者狀態。**只有 withdrawn 寫稽核**:棄賽是裁判的動作,
// 而 eliminated 是判定勝負的後果(已被 MarkFinished 那筆涵蓋)。
func (s *Service) SetPlayerStatus(ctx context.Context, tx pgx.Tx, w match.PlayerStatusWrite) (*match.Player, error) {
	q := s.q.WithTx(tx)
	row, err := q.SetTournamentPlayerStatus(ctx, db.SetTournamentPlayerStatusParams{
		Status:   string(w.Status),
		PlayerID: w.PlayerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 這一支刻意沒有 status 的守門,所以 0 列只能是查無此選手。
			return nil, fmt.Errorf("player id=%d: %w", w.PlayerID, tournament.ErrPlayerNotFound)
		}
		return nil, fmt.Errorf("改選手狀態 player=%d: %w", w.PlayerID, err)
	}
	p := toPlayer(playerRow(row))
	if w.Status != tournament.PlayerWithdrawn {
		return &p, nil
	}
	after, err := json.Marshal(playerAudit{
		PlayerPublicID: p.PublicID,
		DisplayName:    p.DisplayName,
		Status:         p.Status,
	})
	if err != nil {
		return nil, fmt.Errorf("組稽核內容 player=%s: %w", p.PublicID, err)
	}
	if _, err := q.InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: w.ActorUserID,
		Action:      match.ActionWithdraw,
		TargetType:  ptr(match.AuditTargetPlayer),
		TargetID:    ptr(p.ID),
		After:       after,
		Reason:      w.Reason,
	}); err != nil {
		return nil, fmt.Errorf("寫稽核紀錄 player=%s: %w", p.PublicID, err)
	}
	return &p, nil
}

// BumpFencerRecord 增量更新跨屆戰績。**不寫稽核**,理由同 SeatPlayer。
func (s *Service) BumpFencerRecord(ctx context.Context, tx pgx.Tx, w match.FencerRecordWrite) error {
	n, err := s.q.WithTx(tx).BumpFencerRecord(ctx, db.BumpFencerRecordParams{
		WinsDelta:   w.WinsDelta,
		LossesDelta: w.LossesDelta,
		FencerID:    w.FencerID,
	})
	if err != nil {
		return fmt.Errorf("更新戰績 fencer=%d: %w", w.FencerID, err)
	}
	if n == 0 {
		// tournament_players.fencer_id 有 FK,所以查無此人只可能是資料被動過。
		// 靜靜跳過的後果是生涯頁永遠少一場勝負,而且沒有留下任何線索。
		return fmt.Errorf("更新戰績 fencer=%d: 查無此選手檔案", w.FencerID)
	}
	return nil
}

// RecordJudgeAction 寫一筆 admin_audit_logs。**只有封盤走這裡**:封盤對 matches
// 的 UPDATE 屬於 handicap 套件,本套件在那個動作上沒有自己的寫入可以順手掛稽核。
func (s *Service) RecordJudgeAction(ctx context.Context, tx pgx.Tx, a match.JudgeAction) error {
	var targetType *string
	if a.TargetType != "" {
		targetType = ptr(a.TargetType)
	}
	var targetID *int64
	if a.TargetID != 0 {
		targetID = ptr(a.TargetID)
	}
	if _, err := s.q.WithTx(tx).InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: a.ActorUserID,
		Action:      a.Action,
		TargetType:  targetType,
		TargetID:    targetID,
		After:       a.After,
		Reason:      a.Reason,
	}); err != nil {
		return fmt.Errorf("寫稽核紀錄 action=%s: %w", a.Action, err)
	}
	return nil
}

// AppendEvents 與領域變更同 tx 寫 outbox,並順手送出即時戰況的推播信封。
//
// 推播掛在這裡而不是另開一條路徑,是因為這五個 topic 就是「一場比賽的生命週期
// 會對外講的全部的話」,而即時戰況要推的正是同一組時刻。更重要的是它必須
// **在這個 tx 裡**:NOTIFY 只在 commit 時送出,rollback 的交易不會送 ——
// 觀眾因此不可能看到一個後來被撤銷的賽果(internal/core/activity/watch 的檔頭)。
func (s *Service) AppendEvents(ctx context.Context, tx pgx.Tx, events []match.Event) error {
	if len(events) == 0 {
		return nil
	}
	q := s.q.WithTx(tx)
	for _, e := range events {
		if _, err := q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
			Topic:   e.Topic,
			Payload: e.Payload,
		}); err != nil {
			return fmt.Errorf("寫 outbox topic=%s: %w", e.Topic, err)
		}
		if err := s.notifyWatch(ctx, q, e); err != nil {
			return err
		}
	}
	return nil
}

// notifyWatch 送出一則即時戰況的推播信封。
//
// 失敗一律讓整個裁判動作失敗(而不是吞掉繼續):pg_notify 出錯代表這個
// transaction 已經被 Postgres 標成 aborted,後面每一句都只會回
// 「current transaction is aborted」—— 吞掉只會把成因換成一個看不懂的錯誤。
//
// 與戰況無關的 topic(EnvelopeForEvent 回 false)直接跳過,那不是錯誤。
func (s *Service) notifyWatch(ctx context.Context, q *db.Queries, e match.Event) error {
	env, ok := watch.EnvelopeForEvent(e.Topic, e.Payload)
	if !ok {
		return nil
	}
	payload, err := env.Marshal()
	if err != nil {
		return fmt.Errorf("組推播信封 topic=%s: %w", e.Topic, err)
	}
	if err := q.NotifyActivityWatch(ctx, db.NotifyActivityWatchParams{
		Channel: watch.Channel,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("送推播通知 topic=%s: %w", e.Topic, err)
	}
	return nil
}

// ── 內部 ────────────────────────────────────────────────────────

// matchAudit 是場次動作寫進 admin_audit_logs.after 的快照。
//
// 記的是**動作之後的狀態**而不是「開盤了」三個字:日後有人問「這場什麼時候
// 開打的」「誰判的、判給誰」,查的就是這裡 —— matches 那一列早已被後續動作蓋過去。
type matchAudit struct {
	MatchPublicID        string           `json:"match_public_id"`
	Round                int              `json:"round"`
	Slot                 int              `json:"slot"`
	Status               match.Status     `json:"status"`
	HandicapOpen         bool             `json:"handicap_open"`
	ResultKind           match.ResultKind `json:"result_kind,omitempty"`
	WinnerPlayerPublicID string           `json:"winner_player_public_id,omitempty"`
	StartedAt            *time.Time       `json:"started_at,omitempty"`
	FinishedAt           *time.Time       `json:"finished_at,omitempty"`
	StreamURL            string           `json:"stream_url,omitempty"`
}

// playerAudit 是棄賽寫進 admin_audit_logs.after 的快照。
type playerAudit struct {
	PlayerPublicID string                  `json:"player_public_id"`
	DisplayName    string                  `json:"display_name"`
	Status         tournament.PlayerStatus `json:"status"`
}

// auditMatch 在同一個 tx 裡替一個場次動作寫稽核。
func (s *Service) auditMatch(ctx context.Context, q *db.Queries,
	actorUserID int64, action string, m match.Match, reason string,
) error {
	a := matchAudit{
		MatchPublicID: m.PublicID,
		Round:         m.Round,
		Slot:          m.Slot,
		Status:        m.Status,
		HandicapOpen:  m.HandicapOpen,
		StartedAt:     m.StartedAt,
		FinishedAt:    m.FinishedAt,
		StreamURL:     m.StreamURL,
	}
	if m.WinnerPlayerID != 0 {
		a.ResultKind = m.ResultKind
		// 記 public_id 而不是內部 id:稽核是給人查的,對外也只認 public_id。
		switch m.WinnerPlayerID {
		case m.P1.ID:
			a.WinnerPlayerPublicID = m.P1.PublicID
		case m.P2.ID:
			a.WinnerPlayerPublicID = m.P2.PublicID
		}
	}
	after, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("組稽核內容 match=%s: %w", m.PublicID, err)
	}
	if _, err := q.InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: actorUserID,
		Action:      action,
		TargetType:  ptr(match.AuditTargetMatch),
		TargetID:    ptr(m.ID),
		After:       after,
		Reason:      reason,
	}); err != nil {
		return fmt.Errorf("寫稽核紀錄 match=%s action=%s: %w", m.PublicID, action, err)
	}
	return nil
}

// attribute 把「狀態轉移影響 0 列」翻成正確的錯誤。
//
// 0 列有兩個成因:場次不存在,或狀態已經不是閘門要的那個。兩者對裁判的意義完全
// 不同(「網址打錯了」vs「有人剛剛已經按過了」),所以要重讀一次才分得出來。
// 重讀走 GetMatchForJudgeByID(無鎖)—— 本 tx 已經持有該列鎖,再鎖一次沒有效果,
// 只會多一次寫 xmax 的代價;而錯誤訊息裡的 public_id 就取自重讀回來的那一列,
// 那是給人看的唯一識別(對外永不出現內部 id)。
func (s *Service) attribute(ctx context.Context, q *db.Queries, matchID int64, stale error) error {
	row, err := q.GetMatchForJudgeByID(ctx, matchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if errors.Is(stale, ErrSeatUnavailable) {
				// 晉級目標整場不見了 = 對戰表被刪了一半,service 要的是這個錯誤。
				return fmt.Errorf("match id=%d: %w", matchID, match.ErrAdvanceTargetMissing)
			}
			return fmt.Errorf("match id=%d: %w", matchID, match.ErrMatchNotFound)
		}
		return fmt.Errorf("重讀場次 id=%d: %w", matchID, err)
	}
	return fmt.Errorf("match=%s: %w", row.PublicID, stale)
}

// ptr 取位址。sqlc 的可空欄位一律是指標,而 Go 沒有字面值取址的寫法。
func ptr[T any](v T) *T { return &v }
