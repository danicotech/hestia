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
			protoChannel, protoKind := channelKind(req.Peer().Protocol)
			rec := eventlog.Record{
				RequestID: RequestID(ctx),
				Channel:   protoChannel,
				Kind:      protoKind,
				Action:    procedure,
				TraceID:   traceID(ctx),
				Request:   marshalSummary(requestSummary(procedure, req.Any())),
			}
			// channel / kind 要等認證跑完才知道(認證在本攔截器**內層**):
			// stentor 代打的請求走的是 HTTP,但它的入口其實是 Discord。
			// 兩者都在送出前重算,panic 路徑也一樣。
			stamp := func(r *eventlog.Record) {
				r.Channel, r.Kind = originOf(ctx, protoChannel, protoKind)
				r.UserID = userIDPtr(ctx)
				r.SpaceID = spaceIDPtr(ctx)
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
				rec.Response = marshalSummary(markDelegation(ctx, map[string]any{"panic": true}))
				stamp(&rec)
				submitEventLog(ctx, queue, rec)
				panic(r)
			}()

			res, err := next(ctx, req)
			completed = true

			rec.LatencyMS = elapsedMS(start)
			stamp(&rec)
			rec.Status, rec.ErrorCode = statusOf(err)
			switch {
			case err != nil:
				rec.Response = marshalSummary(markDelegation(ctx, errorSummary(err)))
			case res != nil:
				rec.Response = marshalSummary(markDelegation(ctx, responseSummary(procedure, res.Any())))
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

// originOf 決定這一列的 channel / kind。
//
// 為什麼不能只看協定:stentor 代打的請求在網路上是 HTTP/gRPC,但它的**入口**
// 是 Discord —— schemas/13 的 channel 問的是「操作從哪個入口進來」。
// 判斷依據是 X-Acting-User 的 provider(服務認證解析出來的),不是服務名:
// 服務名是憑證的一部分,絕不能出現在 event_logs 的任何欄位。
//
// kind 用 `<provider>.event`,跟既有的 twitch.event / youtube.event 同一套
// 命名。**這是 kind 值域的新增值**(schemas/13 的清單原本只有 discord.command
// / discord.component,兩者對「閘道事件」都不誠實),已在交付說明中標記待確認。
func originOf(ctx context.Context, protoChannel, protoKind string) (channel, kind string) {
	st := stateFrom(ctx)
	if st == nil {
		return protoChannel, protoKind
	}
	provider, _ := st.currentActing()
	switch provider {
	case eventlog.ChannelDiscord:
		return eventlog.ChannelDiscord, "discord.event"
	case eventlog.ChannelTwitch:
		return eventlog.ChannelTwitch, eventlog.KindTwitchEvent
	case eventlog.ChannelYouTube:
		return eventlog.ChannelYouTube, eventlog.KindYouTubeEvent
	default:
		return protoChannel, protoKind
	}
}

// delegationKey 是 event_logs.response 摘要裡的代打標記。
//
// 為什麼用既有欄位而不是新增一欄:schema 已定案(schemas/13),而 response
// 摘要正是「這次呼叫的結果脈絡」該待的地方。查稽核時
// `response->>'delegated' = 'true'` 就能把閘道代打的操作全部撈出來。
//
// 為什麼一定要標:代打之後 user_id 記的是**代打對象**(這是對的:錢確實
// 進了他的口袋),但這樣一來「他自己在網頁上按的」與「stentor 代他按的」
// 在表上長得一模一樣。出事時分不出責任歸屬,就等於沒有稽核。
//
// 服務名/憑證仍然絕不入庫(serviceauth.go 的鐵則):這裡只標「是代打」,
// 不標「哪個服務代的」—— 有效 token 有幾把、叫什麼名字,都不該從稽核表看得出來。
// 要追是哪一台呼叫的,走 trace_id 去 Grafana(schemas/13 的反查閉環)。
const delegationKey = "delegated"

// markDelegation 在代打時把標記併進摘要。
//
// summary 為 nil(白名單沒收錄那個訊息型別)時仍會建一個 map:代打這件事
// 比「這支 RPC 的摘要沒登記」更重要,不能因為前者缺席就跟著漏掉。
func markDelegation(ctx context.Context, summary map[string]any) map[string]any {
	st := stateFrom(ctx)
	if st == nil || !st.isDelegated() {
		return summary
	}
	if summary == nil {
		summary = map[string]any{}
	}
	summary[delegationKey] = true
	return summary
}

// userIDPtr 取「這次操作記在誰頭上」:使用者呼叫是登入者,服務呼叫是
// X-Acting-User 解出來的人(subjectUserID)。服務憑證本身永不入庫。
func userIDPtr(ctx context.Context) *int64 {
	st := stateFrom(ctx)
	if st == nil {
		return nil
	}
	if id := st.subjectUserID(); id != 0 {
		return &id
	}
	return nil
}

func spaceIDPtr(ctx context.Context) *int64 {
	st := stateFrom(ctx)
	if st == nil {
		return nil
	}
	if id := st.currentSpaceID(); id != 0 {
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
