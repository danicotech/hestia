package transport

import (
	"context"
	"sync"
)

// callState 是「一次請求的共用便條」,由最外層的 HTTP middleware 建立。
//
// 為什麼需要它:攔截器順序是 recover → OTel → event_logs → 認證,
// event_logs 在認證的**外層**(否則被擋下的請求就不會留紀錄,違反 schemas/13
// 「失敗與 denied 也要記」)。但外層看不到內層塞進 ctx 的值——ctx 是往內傳的。
// 所以由外層先放一個可寫的便條,內層把解出來的 user id 寫回去。
//
// logged 則是相反方向的需求:HTTP middleware 要知道「這個請求有沒有進到 RPC
// 層並留下紀錄」,沒有的話(解碼失敗、路徑不存在、請求體超限)由它補一列。
type callState struct {
	requestID string

	mu     sync.Mutex
	userID int64
	logged bool
}

func (s *callState) setUserID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userID = id
}

func (s *callState) currentUserID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userID
}

// markLogged 記下「RPC 層已經寫過紀錄」,回傳先前的值。
func (s *callState) markLogged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logged = true
}

func (s *callState) alreadyLogged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logged
}

type ctxKey struct{}

var stateKey ctxKey

func withState(ctx context.Context, st *callState) context.Context {
	return context.WithValue(ctx, stateKey, st)
}

func stateFrom(ctx context.Context) *callState {
	st, _ := ctx.Value(stateKey).(*callState)
	return st
}

// RequestID 回傳本次請求的 ULID(跨層追蹤用,同時是 event_logs.request_id
// 與回應的 X-Request-Id header)。不在請求情境時回空字串。
func RequestID(ctx context.Context) string {
	if st := stateFrom(ctx); st != nil {
		return st.requestID
	}
	return ""
}

// UserID 回傳認證攔截器解出的內部 user id。ok=false 表示本次請求未認證。
func UserID(ctx context.Context) (int64, bool) {
	st := stateFrom(ctx)
	if st == nil {
		return 0, false
	}
	id := st.currentUserID()
	return id, id != 0
}

// requireUser 是 handler 取主體的統一入口:沒有身分就是 Unauthenticated,
// 不讓任何 handler 自己決定「沒身分時預設是誰」。
func requireUser(ctx context.Context) (int64, error) {
	id, ok := UserID(ctx)
	if !ok {
		return 0, toConnectError(ErrUnauthenticated)
	}
	return id, nil
}
