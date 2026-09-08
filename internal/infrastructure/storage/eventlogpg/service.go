// Package eventlogpg 是 eventlog.Logger 的 Postgres 實作(schemas/13-event-logs.md)。
//
// 只有 INSERT:操作紀錄寫下去就不改(同帳本的 append-only 精神),
// 到期由 maintenance 整個分區 DROP,不做逐列刪除。
//
// 這裡不做任何過濾或遮蔽——白名單摘要是呼叫端(入口攔截器)的責任,
// 把「什麼可以記」的判斷放在最靠近原始資料的地方,而不是等到寫入層才補救。
package eventlogpg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Service 寫入 platform.event_logs。
type Service struct {
	q *db.Queries
}

// New 建立寫入器。
func New(pool *pgxpool.Pool) *Service {
	return &Service{q: db.New(pool)}
}

var _ eventlog.Logger = (*Service)(nil)

// LogEvent 寫一列操作紀錄。
//
// 刻意不重試、不排隊:稽核是旁路,寫不進去就讓呼叫端記 log 走人
// (OTel 那邊還有完整 trace 可查)。為了留紀錄而拖慢或卡住請求是本末倒置。
func (s *Service) LogEvent(ctx context.Context, rec eventlog.Record) error {
	latency := rec.LatencyMS
	err := s.q.InsertEventLog(ctx, db.InsertEventLogParams{
		RequestID: nullable(rec.RequestID),
		Channel:   rec.Channel,
		Kind:      rec.Kind,
		Action:    rec.Action,
		UserID:    rec.UserID,
		SpaceID:   rec.SpaceID,
		Status:    nullable(rec.Status),
		ErrorCode: nullable(rec.ErrorCode),
		LatencyMs: &latency,
		Request:   rec.Request,
		Response:  rec.Response,
		TraceID:   nullable(rec.TraceID),
	})
	if err != nil {
		return fmt.Errorf("寫入 event_logs: %w", err)
	}
	return nil
}

// nullable 把空字串轉成 NULL:欄位語意是「沒有這個值」,
// 存空字串會讓之後的查詢要同時處理空字串和 NULL 兩種空。
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
