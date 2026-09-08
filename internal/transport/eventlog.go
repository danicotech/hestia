package transport

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"

	"github.com/danicotech/hestia/internal/core/platform/eventlog"
)

// eventLogInterceptor 為每個 RPC 寫一列 event_logs——成功、失敗、被擋、panic 都要。
//
// schemas/13 的兌現重點:
//   - 統一在入口攔截器寫,業務碼裡一行都不用手刻;
//   - request / response 是白名單摘要(見 summary.go):預設什麼都不記,
//     只有明確列出的欄位會進 DB,所以漏加摘要的後果是少記,不是外洩;
//   - 投遞到背景佇列,不在請求路徑上等 DB(auditQueue 的註解說明取捨);
//     寫入失敗只記 log,絕不影響請求結果——稽核不能反過來弄壞業務。
func eventLogInterceptor(queue *auditQueue) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if queue == nil {
				return next(ctx, req)
			}
			start := time.Now()
			procedure := req.Spec().Procedure
			channel, kind := channelKind(req.Peer().Protocol)
			rec := eventlog.Record{
				RequestID: RequestID(ctx),
				Channel:   channel,
				Kind:      kind,
				Action:    procedure,
				TraceID:   traceID(ctx),
				Request:   marshalSummary(requestSummary(procedure, req.Any())),
			}

			// panic 也要留紀錄:先寫一列,再把 panic 往外丟給 recoverInterceptor。
			completed := false
			defer func() {
				if completed {
					return
				}
				r := recover()
				if r == nil {
					return
				}
				rec.LatencyMS = elapsedMS(start)
				rec.Status = eventlog.StatusError
				rec.ErrorCode = connect.CodeInternal.String()
				rec.Response = marshalSummary(map[string]any{"panic": true})
				rec.UserID = userIDPtr(ctx)
				submitEventLog(ctx, queue, rec)
				panic(r)
			}()

			res, err := next(ctx, req)
			completed = true

			rec.LatencyMS = elapsedMS(start)
			rec.UserID = userIDPtr(ctx)
			rec.Status, rec.ErrorCode = statusOf(err)
			switch {
			case err != nil:
				rec.Response = marshalSummary(errorSummary(err))
			case res != nil:
				rec.Response = marshalSummary(responseSummary(procedure, res.Any()))
			}
			submitEventLog(ctx, queue, rec)
			return res, err
		}
	}
}

// channelKind 把 connect 的協定轉成 schemas/13 的 channel / kind。
// gRPC 與 gRPC-Web 都算 grpc:對稽核而言「二進位 RPC」是同一件事。
func channelKind(protocol string) (channel, kind string) {
	switch protocol {
	case connect.ProtocolGRPC, connect.ProtocolGRPCWeb:
		return eventlog.ChannelGRPC, eventlog.KindGRPCCall
	default:
		return eventlog.ChannelAPI, eventlog.KindHTTPRequest
	}
}

// statusOf 把錯誤轉成 event_logs 的 status 與 error_code。
func statusOf(err error) (status, code string) {
	if err == nil {
		return eventlog.StatusOK, ""
	}
	c := connect.CodeOf(err)
	switch c {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return eventlog.StatusDenied, c.String()
	case connect.CodeResourceExhausted:
		return eventlog.StatusRateLimited, c.String()
	default:
		return eventlog.StatusError, c.String()
	}
}

const maxInt32 = int32(^uint32(0) >> 1)

func elapsedMS(start time.Time) int32 {
	ms := time.Since(start).Milliseconds()
	if ms > int64(maxInt32) {
		return maxInt32
	}
	return int32(ms)
}

func traceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

func userIDPtr(ctx context.Context) *int64 {
	if id, ok := UserID(ctx); ok {
		return &id
	}
	return nil
}

// submitEventLog 投遞到背景佇列,並在便條上標記「RPC 層已記」,
// 讓最外層的 HTTP middleware 知道不用補記(一個 RPC 一列)。
func submitEventLog(ctx context.Context, queue *auditQueue, rec eventlog.Record) {
	if st := stateFrom(ctx); st != nil {
		st.markLogged()
	}
	queue.submit(rec)
}
