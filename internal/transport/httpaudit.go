package transport

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// headerRequestID 是回應一律帶上的追蹤鍵。**成功與失敗都要帶**——
// 失敗的請求才是最需要對帳的那些。
const headerRequestID = "X-Request-Id"

// auditHTTP 是最外層的 HTTP middleware,做三件攔截器做不到的事:
//
//  1. 產生 request_id 並**在呼叫 handler 之前**寫進回應 header。
//     攔截器只能在有回應物件時設 header,錯誤路徑就沒有了。
//  2. 讓 request_id 從最外層就存在,所以連 panic 的那行 log 都關聯得上
//     event_logs 的那一列。
//  3. 補記「進來過但沒進到 RPC 層」的請求。connect 在攔截器**之前**就解碼
//     並檢查請求體大小,所以壞 JSON、超大請求體、不存在的路徑原本完全無稽核
//     ——那正是被人試探時最該留痕的流量。
//
// 注意:這一層拿不到 OTel span(span 由內層的 otelconnect 建立),
// 所以補記的列 trace_id 為空;要看細節去 Grafana 用 request_id 撈。
//
// basePath 是掛載前綴(可為空)。這一層刻意包在**剝掉前綴之前**:
// 前綴外的探測流量(/wp-login.php、/.env)才留得下紀錄 —— 那正是最該留痕的
// 流量,而剝前綴的那層對它們只會回 404 就沒了。代價是 r.URL.Path 帶著前綴,
// 所以 auditAction 自己要把它拿掉(action 必須是 procedure 全名)。
func auditHTTP(next http.Handler, queue *auditQueue, log *slog.Logger, basePath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := ulid.New()
		if err != nil {
			// crypto/rand 掛了是系統級異常,不降級用可預測的 id。
			log.ErrorContext(r.Context(), "產生 request_id 失敗", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		st := &callState{requestID: id}
		w.Header().Set(headerRequestID, id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			// RPC 層已經寫過就不重複記(一個 RPC 一列)。
			if queue == nil || st.alreadyLogged() {
				return
			}
			queue.submit(fallbackRecord(r, st, rec.status, elapsedMS(start), basePath))
		}()
		next.ServeHTTP(rec, r.WithContext(withState(r.Context(), st)))
	})
}

// fallbackRecord 組出「沒進到 RPC 層」的那一列。
// request 摘要一律 nil:請求體沒解出來(或根本不該解),沒有可信的白名單內容。
//
// handler 留了說明(瀏覽器登入路由,見 callState.setAudit)就以它為準:
// 那三個欄位是 handler **知道**的事實,HTTP 狀態碼只是它的投影 ——
// 登入失敗導回前端用的是 302,光看狀態碼會把它記成成功。
func fallbackRecord(r *http.Request, st *callState, status int, latency int32, basePath string) eventlog.Record {
	channel, kind := channelKindFromContentType(r.Header.Get("Content-Type"))
	action, noteStatus, noteCode := st.auditNote()
	if action == "" {
		action = auditAction(r.URL.Path, basePath)
	}
	code := noteCode
	if noteStatus == "" {
		noteStatus, code = statusFromHTTP(status), codeFromHTTPStatus(status)
	}
	out := eventlog.Record{
		RequestID: st.requestID,
		Channel:   channel,
		Kind:      kind,
		Action:    action,
		Status:    noteStatus,
		LatencyMS: latency,
		Response:  marshalSummary(map[string]any{"http_status": status}),
	}
	if id := st.subjectUserID(); id != 0 {
		out.UserID = &id
	}
	if code != "" {
		out.ErrorCode = code
	}
	return out
}

// unknownAction 是「這條路徑不是我們的 RPC」的固定值。
const unknownAction = "(non-rpc path)"

// auditAction 決定補記那一列的 action(QA 2026-09-08)。
//
// r.URL.Path 在這條路徑上是**未認證的任何人都能自由控制的字串**,而
// event_logs 保留 180 天。summary.go 早就為 client 可控字串定了 256 bytes
// 的上限,但補記走的是另一條路,不受那條規則管 —— 實測 4001 字元直接落地,
// 同一個威脅模型只擋了一半。
//
// 三段處理,順序有意義:
//
//   - **先剝掉掛載前綴。** action 記的必須是 procedure 全名
//     (/hestia.platform.v1.X/Y),而不是 /api/hestia.platform.v1.X/Y ——
//     前綴是部署細節,讓它進資料表會使同一支 RPC 在不同部署下長成兩個值
//     (專案第 9 條)。剝不掉(路徑根本不在前綴底下)就是探測流量。
//   - **不是 /hestia. 開頭的一律換成固定字串。** 這一層包住的是 connect mux,
//     真正的 RPC 路徑一定是 procedure 全名;其餘全是探測流量,把它們的路徑
//     原樣存下來對查稽核毫無幫助,卻是最省力的寫入管道。要看實際打了什麼,
//     用 request_id 去 log 撈 —— log 不是要保留 180 天的資料表。
//   - **是我們的 RPC 才留,而且照樣截斷。** 合法的 procedure 遠短於上限,
//     會撞到截斷的只有「前綴對但後面接一長串」的變形攻擊。
func auditAction(path, basePath string) string {
	rest, ok := stripBasePath(path, basePath)
	if !ok {
		return unknownAction
	}
	// 兩個 package 都要認。只認平台層的話,裁判的每一個動作
	// (判勝負、棄賽、封盤)在 HTTP 稽核裡都會變成 unknown ——
	// 而那些正是最需要查得出「誰在什麼時候做了什麼」的動作。
	var ours bool
	for _, pkg := range scannedPackages() {
		if strings.HasPrefix(rest, "/"+string(pkg)+".") {
			ours = true
			break
		}
	}
	if !ours {
		return unknownAction
	}
	action, _ := truncateUTF8(rest, summaryMaxStringBytes)
	return action
}

func statusFromHTTP(status int) string {
	switch {
	case status < 400:
		return eventlog.StatusOK
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return eventlog.StatusDenied
	case status == http.StatusTooManyRequests:
		return eventlog.StatusRateLimited
	default:
		return eventlog.StatusError
	}
}

// codeFromHTTPStatus 把 HTTP 狀態碼折回 connect code 的字串,
// 讓補記的列與 RPC 層的列用同一套 error_code 詞彙(查詢時不必分兩種寫法)。
func codeFromHTTPStatus(status int) string {
	switch {
	case status < 400:
		return ""
	case status == http.StatusBadRequest:
		return connect.CodeInvalidArgument.String()
	case status == http.StatusUnauthorized:
		return connect.CodeUnauthenticated.String()
	case status == http.StatusForbidden:
		return connect.CodePermissionDenied.String()
	case status == http.StatusNotFound:
		return connect.CodeUnimplemented.String()
	case status == http.StatusRequestTimeout:
		return connect.CodeDeadlineExceeded.String()
	case status == http.StatusRequestEntityTooLarge:
		return connect.CodeResourceExhausted.String()
	case status == http.StatusTooManyRequests:
		return connect.CodeResourceExhausted.String()
	case status == http.StatusNotImplemented, status == http.StatusMethodNotAllowed,
		status == http.StatusUnsupportedMediaType:
		return connect.CodeUnimplemented.String()
	case status == http.StatusServiceUnavailable:
		return connect.CodeUnavailable.String()
	default:
		return connect.CodeInternal.String()
	}
}

// channelKindFromContentType 在拿不到 connect peer 時判斷協定
// (補記的列也要能和 RPC 層的列一起查)。
func channelKindFromContentType(ct string) (channel, kind string) {
	ct = strings.ToLower(ct)
	if strings.HasPrefix(ct, "application/grpc") {
		return eventlog.ChannelGRPC, eventlog.KindGRPCCall
	}
	return eventlog.ChannelAPI, eventlog.KindHTTPRequest
}

// statusRecorder 只為了知道最後回了幾號。Unwrap 讓 http.ResponseController
// (Flush / SetWriteDeadline 等)照樣穿透到真正的 writer——connect 的串流需要。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
