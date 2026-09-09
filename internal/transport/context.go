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

	// ── 服務對服務呼叫的欄位(serviceauth.go)──
	//
	// service 與 userID 是**互斥**的兩種身分,不是同一個欄位的兩種來源:
	// 前者是「stentor 這個服務在呼叫」,後者是「這個人登入後在呼叫」。
	// 混成一個欄位的話,handler 就沒辦法拒絕「服務 token 冒充使用者」。
	service string
	// actingProvider 是 X-Acting-User 的 provider(discord/…)。
	// 解析成功就記下來,**即使後續查不到綁定** —— event_logs 的 channel
	// 要靠它填(schemas/13:這類請求的入口是 discord,不是 api)。
	actingProvider string
	// actingUserID 是解出來的內部 user_id;0 = 沒有 / 查不到。
	actingUserID int64
	// actingIsSubject 標記「actingUserID 就是這次操作的主體」。
	//
	// 為什麼不是「actingUserID != 0 就算主體」(QA 2026-09-08 的發現):
	// public procedure 帶著 X-Acting-User 打進來時,acting user 是解得出來的,
	// 但這次操作**不是**他做的 —— 稽核若照樣記在他頭上,就是在說謊。
	// 唯一算主體的兩種情況由攔截器明確決定:只收服務身分的 RPC
	// (主體 = acting user),以及代打(那條路徑直接寫 userID)。
	actingIsSubject bool
	// delegated 標記「這次請求的使用者身分是代打來的」(服務憑證 + X-Acting-User
	// 打在代打白名單內的使用者 RPC)。
	//
	// 為什麼需要一個獨立旗標而不是「userID != 0 且 service != ''」:那個組合
	// 在未來多一種身分來源時就會誤判,而這個旗標是攔截器**明確決定**的結果。
	// 稽核靠它在 event_logs 標出代打(見 eventlog.go 的 delegationMark)。
	delegated bool
	// spaceID 是 handler 解析出的來源空間,寫進 event_logs.space_id。
	spaceID int64

	// ── 非 RPC handler 的稽核說明(瀏覽器登入路由)──
	//
	// 為什麼需要:瀏覽器路由不經過 connect 的攔截器,只會被最外層的
	// auditHTTP 補記一列,而那一列預設只看得到 URL 與 HTTP 狀態碼。
	// 兩者對登入都不誠實:
	//
	//   - action 會變成 "/auth/discord/callback",與同一件事的 RPC 版本
	//     (/hestia.platform.v1.AuthService/CompleteDiscordLogin)是兩個值。
	//     同一個概念在稽核表上有兩種寫法,查詢就得寫兩份(專案第 9 條)。
	//   - status 會是 ok —— 登入失敗導回前端用的是 302,而 302 不是錯誤碼。
	//     稽核表上「全部登入都成功」是最糟的一種說謊。
	//
	// 所以由 handler 明確說明結果,三個欄位一起給(理由同 setActing:
	// 只設一半的組合正是稽核說謊的來源)。
	auditAction string
	auditStatus string
	auditCode   string
}

// setAudit 讓非 RPC handler 明確說明這一列該記什麼。
func (s *callState) setAudit(action, status, errorCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditAction, s.auditStatus, s.auditCode = action, status, errorCode
}

// auditNote 取回 handler 的說明;沒設過就三個都是空字串,由補記那層決定。
func (s *callState) auditNote() (action, status, errorCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auditAction, s.auditStatus, s.auditCode
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

func (s *callState) setService(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.service = name
}

func (s *callState) currentService() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.service
}

// setActing 記下 X-Acting-User 的解析結果。
//
// isSubject 一定要由呼叫端明確給:它決定 event_logs.user_id 記不記這個人。
// 做成參數而不是另一個 setter,是為了讓「解出了誰」與「他是不是主體」
// 不可能只設一半 —— 只設一半的那個組合正是稽核說謊的來源。
func (s *callState) setActing(provider string, userID int64, isSubject bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actingProvider, s.actingUserID = provider, userID
	s.actingIsSubject = isSubject
}

func (s *callState) currentActing() (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.actingProvider, s.actingUserID
}

// setDelegated 記下「代打成立」:主體(userID)= 代打對象,並打上代打標記。
//
// 一次寫兩個欄位是刻意的:代打對象成為請求主體,與「這是代打」必須同時成立,
// 分兩個 setter 就有可能只設一半 —— 那會變成「看起來像本人操作」的稽核紀錄。
func (s *callState) setDelegated(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userID, s.delegated = userID, true
}

func (s *callState) isDelegated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delegated
}

func (s *callState) setSpaceID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spaceID = id
}

func (s *callState) currentSpaceID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spaceID
}

// subjectUserID 是「這次操作記在誰頭上」:使用者呼叫是登入者,
// 服務呼叫是 X-Acting-User 解出來的人。event_logs.user_id 用這個。
//
// 刻意與 currentUserID 分開:稽核要的是「誰的行為」,授權要的是
// 「誰通過了認證」。只收服務的 RPC(ActivityService)兩者不同 ——
// 那裡 userID 是 0,主體只在 actingUserID 上,handler 拿不到「登入使用者」。
// 代打(delegatedProcedures)是唯一兩者相等的情況,而那是攔截器明確決定的,
// 不是靠這個 fallback 湊出來的。
//
// fallback **只在 actingIsSubject 成立時**發生(QA 2026-09-08):
// 原本是「userID 為 0 就無條件退回 actingUserID」,結果服務憑證打任何
// public RPC(含整個 AuthService)帶上 header,就會被記成「這個真人做的」,
// 而且 delegated 旗標還不會設 —— 稽核表上與本人操作長得一模一樣。
// 現在「他是不是主體」是攔截器明確標記的結果,不是這裡猜出來的。
func (s *callState) subjectUserID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userID != 0 {
		return s.userID
	}
	if s.actingIsSubject {
		return s.actingUserID
	}
	return 0
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
//
// 注意它讀的是 UserID 而不是 subjectUserID:X-Acting-User 只有在**代打白名單
// 通過**時才會成為這裡的答案(authInterceptor 明確 setDelegated),
// 白名單外的服務呼叫連 handler 都進不來。handler 因此不必知道請求走哪條路 ——
// 兩條路徑的主體語意一模一樣,差別只在 event_logs 標不標代打。
func requireUser(ctx context.Context) (int64, error) {
	id, ok := UserID(ctx)
	if !ok {
		return 0, toConnectError(ErrUnauthenticated)
	}
	return id, nil
}

// ServiceName 回傳通過服務認證的呼叫方名稱。ok=false 表示這不是服務呼叫。
func ServiceName(ctx context.Context) (string, bool) {
	st := stateFrom(ctx)
	if st == nil {
		return "", false
	}
	name := st.currentService()
	return name, name != ""
}

// ActingUserID 回傳 X-Acting-User 解出的內部 user id(服務呼叫專用)。
func ActingUserID(ctx context.Context) (int64, bool) {
	st := stateFrom(ctx)
	if st == nil {
		return 0, false
	}
	_, id := st.currentActing()
	return id, id != 0
}

// requireService 是「只收服務身分、且沒有使用者主體」的 handler 的統一入口
// (目前只有 NotificationService)。
//
// 與 requireActingUser 的分工:那個問「服務代表誰」,這個只問
// 「這是不是服務在呼叫」。公告佇列不屬於任何人 —— 硬要一個代表對象,
// 只會讓稽核紀錄把拉取記在一個與這次操作無關的人頭上。
//
// 正常情況下攔截器早就擋掉使用者憑證了(requiresServiceIdentity)。
// 這裡是防禦性的兜底:handler 不假設攔截器的清單一定配置正確。
func requireService(ctx context.Context) error {
	if _, ok := ServiceName(ctx); !ok {
		return toConnectError(ErrPermissionDenied)
	}
	return nil
}

// requireActingUser 是活動記錄 handler 取行為主體的統一入口。
//
// 兩道檢查都必要:沒有服務身分(有人拿使用者 token 打過來)是 PermissionDenied;
// 有服務身分但沒解出使用者,代表攔截器那關已經回過錯了,這裡是防禦性的兜底。
func requireActingUser(ctx context.Context) (int64, error) {
	if _, ok := ServiceName(ctx); !ok {
		return 0, toConnectError(ErrPermissionDenied)
	}
	id, ok := ActingUserID(ctx)
	if !ok {
		return 0, toConnectError(ErrUnauthenticated)
	}
	return id, nil
}

// noteAudit 是瀏覽器路由留下稽核說明的統一入口(見 callState.setAudit)。
// 不在請求情境時靜靜略過 —— 稽核不能反過來弄壞業務。
func noteAudit(ctx context.Context, action, status, errorCode string) {
	if st := stateFrom(ctx); st != nil {
		st.setAudit(action, status, errorCode)
	}
}

// noteSpace 把 handler 解出的來源空間記進便條,供 event_logs.space_id 使用。
func noteSpace(ctx context.Context, spaceID int64) {
	if spaceID == 0 {
		return
	}
	if st := stateFrom(ctx); st != nil {
		st.setSpaceID(spaceID)
	}
}
