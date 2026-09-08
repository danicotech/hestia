// Package eventlog 定義入口層操作紀錄的契約(schemas/13-event-logs.md)。
//
// 為什麼是 core 而不是某個入口自己的型別:寫這張表的入口不只一個——
// HTTP/gRPC 攔截器、Discord 指令分派器、webhook 接收器、排程 runner
// 各寫一次,但**格式只有一份**。契約放這裡,四個入口都依賴同一個介面,
// 實作(eventlogpg)只有一份。
//
// 邊界:記「發生過什麼操作、誰、結果、多慢」,不記內容本身——
// 訊息在 message_logs、錢在 token_entries、管理理由在 admin_audit_logs。
package eventlog

import (
	"context"
	"encoding/json"
)

// channel:操作從哪個入口進來。
const (
	ChannelAPI     = "api"  // Connect / HTTP+JSON
	ChannelGRPC    = "grpc" // gRPC 與 gRPC-Web
	ChannelDiscord = "discord"
	ChannelTwitch  = "twitch"
	ChannelYouTube = "youtube"
	ChannelSystem  = "system" // 排程任務
)

// kind:操作的形態。
const (
	KindHTTPRequest      = "http.request"
	KindGRPCCall         = "grpc.call"
	KindDiscordCommand   = "discord.command"
	KindDiscordComponent = "discord.component"
	KindTwitchEvent      = "twitch.event"
	KindYouTubeEvent     = "youtube.event"
	KindJobRun           = "job.run"
)

// status:結果。失敗與被擋下的請求也要記(schemas/13 寫入約定)。
const (
	StatusOK          = "ok"
	StatusError       = "error"
	StatusDenied      = "denied"
	StatusRateLimited = "rate_limited"
)

// Record 是 platform.event_logs 的一列。欄位對齊資料表,實作不用再翻譯一次。
type Record struct {
	// RequestID 是 ULID,跨層追蹤用(冪等鍵 / outbox 可引用)。
	RequestID string
	Channel   string
	Kind      string
	// Action 是具體動作:gRPC 的 procedure 全名、/daily claim、entitlement_reaper…
	Action string
	// UserID / SpaceID 是弱參照(不設 FK),未登入或系統操作為 nil。
	UserID    *int64
	SpaceID   *int64
	Status    string
	ErrorCode string
	LatencyMS int32
	// Request / Response 是**白名單**摘要 JSON:只放路由層級資訊與業務 public_id。
	// 絕不放 token、密碼、訊息內容或完整 payload。nil = 寫 NULL。
	Request  json.RawMessage
	Response json.RawMessage
	// TraceID 是 W3C trace id,反查 Grafana(Tempo)用;沒有 span 時為空。
	TraceID string
}

// Logger 是操作紀錄的寫入介面。實作:infrastructure/storage/eventlogpg。
//
// 實作契約:寫入失敗要回 error,但**呼叫端不得因此讓業務失敗**——
// 稽核是旁路,不能反過來弄壞請求。
type Logger interface {
	LogEvent(ctx context.Context, rec Record) error
}
