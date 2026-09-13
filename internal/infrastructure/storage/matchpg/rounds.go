package matchpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 本檔是 2026-09-13 增補的那組寫入(migration 00006):設定確認、回合、季軍戰、違規。
// 紀律與 service.go 相同:每一支寫入在同一個 tx 裡緊接著寫稽核;0 列一律重讀歸因,
// 絕不當成成功。

// ── 設定確認 ────────────────────────────────────────────────────

// setupAudit 是設定確認寫進 admin_audit_logs.after 的內容:場次快照 + 確認的那份清單。
type setupAudit struct {
	matchAudit
	SetupConfirmedAt *time.Time                `json:"setup_confirmed_at,omitempty"`
	SetupConfirmedBy int64                     `json:"setup_confirmed_by,omitempty"`
	Checklist        []handicap.ChecklistEntry `json:"checklist"`
}

// ConfirmSetup 開賽前設定確認,並寫稽核(after 帶清單)。
func (s *Service) ConfirmSetup(ctx context.Context, tx pgx.Tx, w match.SetupConfirmWrite) (*match.Match, error) {
	q := s.q.WithTx(tx)
	row, err := q.ConfirmMatchSetup(ctx, db.ConfirmMatchSetupParams{
		ActorUserID: w.ActorUserID, MatchID: w.MatchID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, s.attributeSetup(ctx, q, w.MatchID)
		}
		return nil, fmt.Errorf("設定確認 match=%d: %w", w.MatchID, err)
	}
	m := toMatch(matchRow(row))
	checklist := w.Checklist
	if checklist == nil {
		checklist = []handicap.ChecklistEntry{}
	}
	after, err := json.Marshal(setupAudit{
		matchAudit:       snapshot(m),
		SetupConfirmedAt: m.SetupConfirmedAt,
		SetupConfirmedBy: m.SetupConfirmedBy,
		Checklist:        checklist,
	})
	if err != nil {
		return nil, fmt.Errorf("組稽核內容 match=%s: %w", m.PublicID, err)
	}
	if err := s.insertAudit(ctx, q, w.ActorUserID, match.ActionConfirmSetup, m.ID, after, w.Reason); err != nil {
		return nil, err
	}
	return &m, nil
}

// attributeSetup 把 ConfirmMatchSetup 的 0 列翻成正確的錯誤:
// 查無 / 還沒封盤(status <> locked)/ 已經確認過。
func (s *Service) attributeSetup(ctx context.Context, q *db.Queries, matchID int64) error {
	row, err := q.GetMatchForJudgeByID(ctx, matchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("match id=%d: %w", matchID, activityerr.ErrMatchNotFound)
		}
		return fmt.Errorf("重讀場次 id=%d: %w", matchID, err)
	}
	switch {
	case row.SetupConfirmedAt != nil:
		return fmt.Errorf("match=%s: %w", row.PublicID, match.ErrSetupAlreadyConfirmed)
	case row.Status != string(match.StatusLocked):
		return fmt.Errorf("match=%s status=%s: %w", row.PublicID, row.Status, match.ErrNotLocked)
	default:
		// 條件都看起來成立卻寫不進去:只剩併發改掉了狀態的瞬間。回最保守的那個。
		return fmt.Errorf("match=%s: %w", row.PublicID, match.ErrNotLocked)
	}
}

// ── 回合 ────────────────────────────────────────────────────────

// roundAudit 是回合動作寫進 admin_audit_logs.after 的內容。
type roundAudit struct {
	MatchPublicID        string     `json:"match_public_id"`
	RoundNo              int        `json:"round_no"`
	StartedAt            time.Time  `json:"started_at"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	WinnerPlayerPublicID string     `json:"winner_player_public_id,omitempty"`
}

// ListRounds 讀一場的全部回合,依 round_no。空場次回空切片。
func (s *Service) ListRounds(ctx context.Context, tx pgx.Tx, matchID int64) ([]match.Round, error) {
	rows, err := s.q.WithTx(tx).ListMatchRounds(ctx, matchID)
	if err != nil {
		return nil, fmt.Errorf("讀回合 match=%d: %w", matchID, err)
	}
	out := make([]match.Round, 0, len(rows))
	for _, r := range rows {
		out = append(out, toRound(r))
	}
	return out, nil
}

// StartRound 建一列回合(started_at = now()),並寫稽核。
//
// UNIQUE (match_id, round_no) 撞上 = 同一回合已經開始過 → ErrRoundInProgress;
// FK 撞上 = 場次不存在。service 在列鎖下已經算過 round_no,這兩條是防併發的第二道。
func (s *Service) StartRound(ctx context.Context, tx pgx.Tx, w match.RoundStartWrite) (*match.Round, error) {
	q := s.q.WithTx(tx)
	row, err := q.InsertMatchRound(ctx, db.InsertMatchRoundParams{
		MatchID: w.MatchID, RoundNo: int32(w.RoundNo),
	})
	if err != nil {
		switch pgCode(err) {
		case "23505":
			return nil, fmt.Errorf("match id=%d round=%d: %w", w.MatchID, w.RoundNo, match.ErrRoundInProgress)
		case "23503":
			return nil, fmt.Errorf("match id=%d: %w", w.MatchID, activityerr.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("建回合 match=%d round=%d: %w", w.MatchID, w.RoundNo, err)
	}
	r := toRound(row)
	m, err := s.byID(ctx, q, w.MatchID)
	if err != nil {
		return nil, err
	}
	after, err := json.Marshal(roundAudit{
		MatchPublicID: m.PublicID, RoundNo: r.RoundNo, StartedAt: r.StartedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("組稽核內容 match=%s: %w", m.PublicID, err)
	}
	if err := s.insertAudit(ctx, q, w.ActorUserID, match.ActionStartRound, m.ID, after, w.Reason); err != nil {
		return nil, err
	}
	return &r, nil
}

// FinishRound 填該回合勝者(finished_at = now()),並寫稽核。
//
// 0 列 = 查無此回合或已結束,重讀分辨。勝者是否為場上兩人之一由 service 擋
// (DB 無法跨表 CHECK);finished_at >= started_at 由 match_rounds_order_check 守。
func (s *Service) FinishRound(ctx context.Context, tx pgx.Tx, w match.RoundFinishWrite) (*match.Round, error) {
	q := s.q.WithTx(tx)
	row, err := q.FinishMatchRound(ctx, db.FinishMatchRoundParams{
		WinnerPlayerID: w.WinnerPlayerID, MatchID: w.MatchID, RoundNo: int32(w.RoundNo),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, s.attributeRound(ctx, q, w.MatchID, w.RoundNo)
		}
		return nil, fmt.Errorf("結束回合 match=%d round=%d: %w", w.MatchID, w.RoundNo, err)
	}
	r := toRound(row)
	m, err := s.byID(ctx, q, w.MatchID)
	if err != nil {
		return nil, err
	}
	r.WinnerPublicID = m.PlayerByID(r.WinnerPlayerID).PublicID
	after, err := json.Marshal(roundAudit{
		MatchPublicID: m.PublicID, RoundNo: r.RoundNo, StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, WinnerPlayerPublicID: r.WinnerPublicID,
	})
	if err != nil {
		return nil, fmt.Errorf("組稽核內容 match=%s: %w", m.PublicID, err)
	}
	if err := s.insertAudit(ctx, q, w.ActorUserID, match.ActionFinishRound, m.ID, after, w.Reason); err != nil {
		return nil, err
	}
	return &r, nil
}

// attributeRound 把 FinishMatchRound 的 0 列翻成正確的錯誤:回合不存在,或已經結束。
func (s *Service) attributeRound(ctx context.Context, q *db.Queries, matchID int64, roundNo int) error {
	rows, err := q.ListMatchRounds(ctx, matchID)
	if err != nil {
		return fmt.Errorf("重讀回合 match=%d: %w", matchID, err)
	}
	for _, r := range rows {
		if int(r.RoundNo) == roundNo {
			return fmt.Errorf("match id=%d round=%d: %w", matchID, roundNo, match.ErrRoundAlreadyFinished)
		}
	}
	return fmt.Errorf("match id=%d round=%d: %w", matchID, roundNo, match.ErrRoundNotFound)
}

// ── 季軍戰 ──────────────────────────────────────────────────────

// InsertThirdPlaceMatch 建季軍戰。**不寫稽核**(準決賽判定的後果,理由同 SeatPlayer)。
//
// public_id 在這裡產生 —— core 不認識 ULID 套件,SQL 也生不出來。
// UNIQUE (tournament_id, round, slot) 撞上代表已經建過:service 先查過 FindThirdPlaceMatch,
// 走到這裡只可能是併發;整筆失敗比默默多一場安全。
func (s *Service) InsertThirdPlaceMatch(ctx context.Context, tx pgx.Tx, w match.ThirdPlaceWrite) (*match.Match, error) {
	pubID, err := ulid.New()
	if err != nil {
		return nil, fmt.Errorf("產生季軍戰 public_id: %w", err)
	}
	row, err := s.q.WithTx(tx).InsertThirdPlaceMatch(ctx, db.InsertThirdPlaceMatchParams{
		TournamentID: w.TournamentID, PublicID: pubID,
		Round: int32(w.Round), Slot: int32(w.Slot),
		P1PlayerID: w.P1PlayerID, P2PlayerID: w.P2PlayerID,
	})
	if err != nil {
		return nil, fmt.Errorf("建季軍戰 tournament=%d round=%d slot=%d: %w",
			w.TournamentID, w.Round, w.Slot, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// FindThirdPlaceMatch 讀本屆的季軍戰;沒有回 (nil, nil)。
func (s *Service) FindThirdPlaceMatch(ctx context.Context, tx pgx.Tx, tournamentID int64) (*match.Match, error) {
	row, err := s.q.WithTx(tx).FindThirdPlaceMatch(ctx, tournamentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("讀季軍戰 tournament=%d: %w", tournamentID, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// ── 違規 ────────────────────────────────────────────────────────

// violationAudit 是「裁判記了一筆違規」寫進 admin_audit_logs.after 的內容。
//
// 記的是違規紀錄本身的內容(誰、哪一項、怎麼判、為什麼),而不是只記
// 「記了一筆」:紀錄之後若被質疑,要查的就是裁判當時寫下了什麼。
type violationAudit struct {
	MatchPublicID     string       `json:"match_public_id"`
	ViolationPublicID string       `json:"violation_public_id"`
	RoundNo           *int         `json:"round_no,omitempty"`
	PlayerPublicID    string       `json:"player_public_id"`
	ItemPublicID      string       `json:"item_public_id,omitempty"`
	ItemKey           string       `json:"item_key,omitempty"`
	Ruling            match.Ruling `json:"ruling"`
	Note              string       `json:"note"`
}

// RecordViolation 記一筆違規並寫稽核。
//
// 項目以 public_id 在**本屆**內找(GetHandicapItemByPublicID 帶 tournament_id):
// 拿上一屆的項目 id 記這一屆的違規必須是「找不到」。查無回 handicap.ErrItemNotFound。
// note 非空由 service 擋,match_violations_note_check 是最後一道。
func (s *Service) RecordViolation(ctx context.Context, tx pgx.Tx, w match.ViolationWrite) (*match.Violation, error) {
	q := s.q.WithTx(tx)
	m, err := s.byID(ctx, q, w.MatchID)
	if err != nil {
		return nil, err
	}
	player := m.PlayerByID(w.PlayerID)
	if !player.Seated() {
		return nil, fmt.Errorf("match=%s player id=%d: %w", m.PublicID, w.PlayerID, match.ErrPlayerNotInMatch)
	}

	var itemID *int64
	var itemKey, itemName string
	if w.ItemPublicID != "" {
		item, err := q.GetHandicapItemByPublicID(ctx, db.GetHandicapItemByPublicIDParams{
			TournamentID: m.TournamentID, PublicID: w.ItemPublicID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("item=%s: %w", w.ItemPublicID, handicap.ErrItemNotFound)
			}
			return nil, fmt.Errorf("讀讓武項目 %s: %w", w.ItemPublicID, err)
		}
		itemID, itemKey, itemName = ptr(item.ID), item.Key, item.Name
	}
	var roundNo *int32
	if w.RoundNo != nil {
		roundNo = ptr(int32(*w.RoundNo))
	}

	pubID, err := ulid.New()
	if err != nil {
		return nil, fmt.Errorf("產生違規紀錄 public_id: %w", err)
	}
	row, err := q.InsertMatchViolation(ctx, db.InsertMatchViolationParams{
		PublicID: pubID, MatchID: m.ID, RoundNo: roundNo, PlayerID: player.ID,
		ItemID: itemID, Ruling: string(w.Ruling), Note: w.Note, RecordedBy: w.ActorUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("寫違規紀錄 match=%s: %w", m.PublicID, err)
	}

	v := match.Violation{
		PublicID:          row.PublicID,
		MatchPublicID:     m.PublicID,
		RoundNo:           w.RoundNo,
		PlayerPublicID:    player.PublicID,
		PlayerDisplayName: player.DisplayName,
		ItemPublicID:      w.ItemPublicID,
		ItemKey:           itemKey,
		ItemName:          itemName,
		Ruling:            match.Ruling(row.Ruling),
		Note:              row.Note,
		RecordedBy:        row.RecordedBy,
		CreatedAt:         row.CreatedAt,
	}
	after, err := json.Marshal(violationAudit{
		MatchPublicID: m.PublicID, ViolationPublicID: v.PublicID, RoundNo: v.RoundNo,
		PlayerPublicID: v.PlayerPublicID, ItemPublicID: v.ItemPublicID, ItemKey: v.ItemKey,
		Ruling: v.Ruling, Note: v.Note,
	})
	if err != nil {
		return nil, fmt.Errorf("組稽核內容 match=%s: %w", m.PublicID, err)
	}
	if err := s.insertAudit(ctx, q, w.ActorUserID, match.ActionRecordViolation, m.ID, after, v.Note); err != nil {
		return nil, err
	}
	return &v, nil
}

// ListViolations 讀一場的全部違規,依發生順序。空場次回空切片。
func (s *Service) ListViolations(ctx context.Context, tx pgx.Tx, matchID int64) ([]match.Violation, error) {
	q := s.q.WithTx(tx)
	rows, err := q.ListMatchViolations(ctx, matchID)
	if err != nil {
		return nil, fmt.Errorf("讀違規紀錄 match=%d: %w", matchID, err)
	}
	out := make([]match.Violation, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	m, err := s.byID(ctx, q, matchID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, toViolation(violationRow(r), m.PublicID))
	}
	return out, nil
}

// ── 內部 ────────────────────────────────────────────────────────

// byID 以內部 id 無鎖重讀場次(本 tx 已持有列鎖)。查無回 ErrMatchNotFound。
func (s *Service) byID(ctx context.Context, q *db.Queries, matchID int64) (*match.Match, error) {
	row, err := q.GetMatchForJudgeByID(ctx, matchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match id=%d: %w", matchID, activityerr.ErrMatchNotFound)
		}
		return nil, fmt.Errorf("重讀場次 id=%d: %w", matchID, err)
	}
	m := toMatch(matchRow(row))
	return &m, nil
}

// insertAudit 寫一筆以場次為 target 的稽核。
func (s *Service) insertAudit(ctx context.Context, q *db.Queries,
	actorUserID int64, action string, matchID int64, after []byte, reason string,
) error {
	if _, err := q.InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: actorUserID,
		Action:      action,
		TargetType:  ptr(match.AuditTargetMatch),
		TargetID:    ptr(matchID),
		After:       after,
		Reason:      reason,
	}); err != nil {
		return fmt.Errorf("寫稽核紀錄 match id=%d action=%s: %w", matchID, action, err)
	}
	return nil
}

// pgCode 取 Postgres 的 SQLSTATE;不是 pg 錯誤時回空字串。
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
