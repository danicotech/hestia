package eventlogpg_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/infrastructure/storage/eventlogpg"
)

// row 是從 event_logs 讀回來的一列(欄位順序與 selectRows 對齊)。
type row struct {
	RequestID *string
	Channel   string
	Kind      string
	Action    string
	UserID    *int64
	SpaceID   *int64
	Status    *string
	ErrorCode *string
	LatencyMs *int32
	Request   []byte
	Response  []byte
	TraceID   *string
}

func selectRows(t *testing.T) []row {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT request_id, channel, kind, action, user_id, space_id,
		       status, error_code, latency_ms, request, response, trace_id
		  FROM platform.event_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("查 event_logs: %v", err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.RequestID, &r.Channel, &r.Kind, &r.Action, &r.UserID, &r.SpaceID,
			&r.Status, &r.ErrorCode, &r.LatencyMs, &r.Request, &r.Response, &r.TraceID); err != nil {
			t.Fatalf("掃描: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀取: %v", err)
	}
	return out
}

func TestLogEvent_WritesAllFields(t *testing.T) {
	setup(t)
	svc := eventlogpg.New(pool)
	userID := int64(77)
	err := svc.LogEvent(context.Background(), eventlog.Record{
		RequestID: "01J8ZZZZZZZZZZZZZZZZZZZZZZ",
		Channel:   eventlog.ChannelAPI,
		Kind:      eventlog.KindHTTPRequest,
		Action:    "/hestia.platform.v1.ShopService/Purchase",
		UserID:    &userID,
		Status:    eventlog.StatusOK,
		LatencyMS: 42,
		Request:   json.RawMessage(`{"item_public_id":"ITEM-1"}`),
		Response:  json.RawMessage(`{"entitlement_public_id":"ENT-1","price":500}`),
		TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
	})
	if err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	rows := selectRows(t)
	if len(rows) != 1 {
		t.Fatalf("期望 1 列,got %d", len(rows))
	}
	r := rows[0]
	if r.Channel != "api" || r.Kind != "http.request" {
		t.Fatalf("channel/kind = %q/%q", r.Channel, r.Kind)
	}
	if r.UserID == nil || *r.UserID != 77 {
		t.Fatalf("user_id = %v", r.UserID)
	}
	if r.Status == nil || *r.Status != "ok" {
		t.Fatalf("status = %v", r.Status)
	}
	if r.LatencyMs == nil || *r.LatencyMs != 42 {
		t.Fatalf("latency_ms = %v", r.LatencyMs)
	}
	if r.TraceID == nil || *r.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %v", r.TraceID)
	}
	var req map[string]any
	if err := json.Unmarshal(r.Request, &req); err != nil {
		t.Fatalf("request 不是合法 JSONB: %v", err)
	}
	if req["item_public_id"] != "ITEM-1" {
		t.Fatalf("request = %s", r.Request)
	}
}

// 空字串一律寫 NULL:欄位語意是「沒有這個值」,存空字串會讓查詢要處理兩種空。
func TestLogEvent_EmptyOptionalsBecomeNull(t *testing.T) {
	setup(t)
	svc := eventlogpg.New(pool)
	err := svc.LogEvent(context.Background(), eventlog.Record{
		Channel: eventlog.ChannelSystem,
		Kind:    eventlog.KindJobRun,
		Action:  "entitlement_reaper",
		Status:  eventlog.StatusOK,
	})
	if err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	r := selectRows(t)[0]
	if r.RequestID != nil || r.ErrorCode != nil || r.TraceID != nil {
		t.Fatalf("空字串應該是 NULL:%+v", r)
	}
	if r.UserID != nil || r.SpaceID != nil {
		t.Fatalf("系統操作不該有 user_id/space_id:%+v", r)
	}
	if r.Request != nil || r.Response != nil {
		t.Fatalf("沒有摘要就該是 NULL:%s / %s", r.Request, r.Response)
	}
}

// waitRows 等背景 worker 把稽核寫進 DB。
// 寫入是非同步的(刻意:不讓 DB 拖慢請求),斷言前必須等。
func waitRows(t *testing.T, n int) []row {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := selectRows(t)
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 %d 列 event_logs,只有 %d 列", n, len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
