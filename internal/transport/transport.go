// Package transport 是 hestia 的入口層:把 proto 契約(ConnectRPC)接到 core 介面。
//
// 分層規則(hestia/CLAUDE.md,depguard 把關):handler 只認 core 的介面與本套件的
// port,**不能** import infrastructure、不碰 pgxpool。組裝在 cmd/server。
//
// 一套 proto 同時服務三種協定(Connect HTTP/JSON、gRPC、gRPC-Web),
// 所以同一個 handler 既是 REST 風格 API 也是 gRPC 服務,契約不會有兩份。
package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// protoPackage 是本服務全部 RPC 的 proto package,啟動時的覆蓋率檢查用。
const protoPackage protoreflect.FullName = "hestia.platform.v1"

// maxRequestBytes 是單一請求體的上限(1 MiB)。
// 沒有上限等於任何人都能用一個請求吃掉伺服器記憶體;
// 本服務最大的請求也只是幾個 id 與一段 reason,1 MiB 已經非常寬鬆。
const maxRequestBytes = 1 << 20

// Deps 是入口層的全部依賴,由 cmd/server 注入。
//
// 每個欄位都可以是 nil:對應的 RPC 會回 Unimplemented 而不是 panic。
// 這讓「契約先到、實作陸續補上」成為安全的常態——半成品服務不會把整個進程帶走。
type Deps struct {
	// ── core 介面(已實作,直接接)──
	Daily     daily.Service
	Shop      shop.Service
	AdminEcon adminecon.Service
	Ledger    ledger.Ledger
	// Activity 是 Discord 活動記錄寫入(schemas/10、11);nil = ActivityService
	// 全部回 Unimplemented。
	Activity activitylog.Service
	// Announcements 是 outbox → Discord 閘道的取貨口(見 ports.go);
	// nil = NotificationService 全部回 Unimplemented。
	//
	// 注入它的同時,組裝端必須確認 outbox 的 in-process handler 沒有註冊
	// notification.DiscordTopics 裡的 topic(notification.AssertNoOverlap)——
	// 兩邊都處理同一個 topic,結果是同一件事在頻道被講兩次。
	Announcements Announcements

	// ── 入口層 port(見 ports.go)──
	Auth          AuthService     // Discord OAuth 登入流程
	Profiles      ProfileReader   // 我的檔案 / 全幣別餘額
	ProfileWrites ProfileWriter   // 改時區(同一個實作可同時滿足讀寫,傳兩次即可)
	Privacy       PrivacyStore    // 兩級退出設定(/privacy optout)
	Catalog       Catalog         // 商品 / 權益 / 工單列表
	LedgerEntries LedgerReader    // 管理端分錄查詢(Refund 的前置)
	Directory     Directory       // public_id ↔ 內部 id
	EventLog      eventlog.Logger // event_logs 寫入;nil = 不寫(僅限測試,正式一定要給)
	Authorizer    Authorizer      // 管理端授權;nil = 管理端一律 PermissionDenied

	// Authenticator 把 Bearer token 換成 user id;nil = UnauthenticatedStub(全部拒絕)。
	Authenticator Authenticator

	// ServiceTokens 是服務對服務憑證(stentor → hestia,見 serviceauth.go)。
	//
	// **可以放多把**:輪替期間新舊並存,不然換 token 必然中斷服務。
	// 空 = 服務認證整個關閉,ActivityService 一律 Unauthenticated
	// (fail closed:沒設定就不接受服務呼叫,而不是誰都能寫)。
	// 值從設定注入(env / secret manager),本套件不讀環境變數。
	ServiceTokens []ServiceToken

	// ActingUsers 把 X-Acting-User 的 `discord:<snowflake>` 解成內部 user id。
	// 實作:storage/activitylogpg。nil 時服務呼叫帶了 X-Acting-User 會回
	// Unimplemented(不會靜默忽略——忽略等於把記錄寫到錯的人頭上)。
	ActingUsers activitylog.ActorResolver

	// TrustedProxies 是「願意相信其轉發標頭」的對端網段(env
	// HESTIA_TRUSTED_PROXIES,用 ParseTrustedProxies 解析)。落在清單內的
	// 對端送來的 CF-Connecting-IP / X-Forwarded-For 才會被當成 session 的來源 IP。
	//
	// **預設 nil = 誰都不信**,一律記連線對端位址。轉發標頭是純請求標頭,
	// 誰都能填;用網段比對而不是一個布林開關,是因為開關在「服務同時接得到
	// 代理與其他來源」時就破功了——那時來源 IP 變成攻擊者可自選的欄位,
	// 而稽核紀錄裡一個能自選的來源比沒有更糟。
	//
	// 部署形狀(2026-09-09):cloudflared 在同一台機器上,對端是回送位址,
	// 所以值是 127.0.0.1,::1。
	TrustedProxies []netip.Prefix

	// BasePath 是全部端點的掛載前綴(env HESTIA_BASE_PATH),例如 "/api"。
	//
	// **空字串 = 掛在根,行為與沒有這個欄位時完全相同**;沒設前綴的部署
	// 連一層包裝都不會多(見 mountAt)。
	//
	// 部署形狀(2026-09-09):前端 https://arena.gengflow.com/、後端在同一個
	// host 的 /api 底下。同源,所以 session cookie 不跨網域、SameSite=Lax 有效。
	//
	// 它同時決定 cookie 的 Path(見 sessioncookie.go / statecookie.go)。
	// 格式不合法會讓 New 回錯 —— 前綴寫錯的後果是整個 API 掛在錯的位置,
	// 而那在測試環境很可能只看起來像 404。
	BasePath string

	// StateCookiePath 明確指定 OAuth state cookie 的 Path,覆蓋 BasePath 的推導。
	//
	// 為什麼預設不是 "/auth":ConnectRPC 的路徑是 procedure 全名
	// (/hestia.platform.v1.AuthService/CompleteDiscordLogin),設成 /auth
	// 會讓瀏覽器不把 cookie 送到回呼那支 RPC,登入流程靜默失敗。
	// 空 = 用 BasePath;BasePath 也空 = "/"。
	StateCookiePath string

	// Logger 用於攔截器的告警(panic、稽核佇列滿、寫入失敗);nil = slog.Default()。
	Logger *slog.Logger
}

// Server 是掛好全部服務與攔截器的 HTTP handler,外加稽核佇列的生命週期。
//
// 為什麼不是單純的 http.Handler:event_logs 由背景 worker 寫入(不拖累請求),
// 那個 goroutine 需要有人負責收尾。關機時呼叫 Shutdown 盡力排空。
type Server struct {
	handler http.Handler
	audit   *auditQueue
}

// ServeHTTP 讓 Server 直接當 http.Handler 用。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Shutdown 停止收新的稽核紀錄並盡力排空佇列。
// ctx 到期就放棄剩下的——關機不能被稽核卡住,但 goroutine 一定會結束。
func (s *Server) Shutdown(ctx context.Context) error { return s.audit.shutdown(ctx) }

// New 組出入口層。
//
// 回傳 error 而不是 panic:失敗來源是 OTel 攔截器初始化與 procedure 覆蓋率檢查,
// 兩者都屬於「啟動時設定錯誤」,應該讓 main 決定怎麼處理(而且要在上線前就炸)。
//
// 注意:gRPC 協定需要 HTTP/2。純文字部署(容器內、反向代理後面)請在 main
// 用 h2c 包一層;Connect(HTTP/JSON)與 gRPC-Web 在 HTTP/1.1 就能跑。
func New(deps Deps) (*Server, error) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	auth := deps.Authenticator
	if auth == nil {
		auth = UnauthenticatedStub{}
	}
	if err := verifyProcedureCoverage(); err != nil {
		return nil, err
	}
	// 掛載前綴的設定錯誤在這裡就炸。前綴寫錯不會有任何徵兆 ——
	// 端點只是全部搬到別的位置,在測試環境看起來像 404。
	//
	// 注意前綴**不影響**上面那道存取層級斷言,也不影響攔截器的清單比對:
	// 兩者的判準是 proto descriptor 與 connect 的 Spec().Procedure(生成碼
	// 裡的常數),與 URL 無關;前綴在進 connect mux 之前就被剝掉了。
	// 那是刻意的性質,不是巧合 —— 若哪天 procedure 改成從 URL 解析,
	// 所有清單比對都會落空,而那個方向是 fail open。
	basePath, err := NormalizeBasePath(deps.BasePath)
	if err != nil {
		return nil, fmt.Errorf("掛載前綴(HESTIA_BASE_PATH)不合法:%w", err)
	}
	// 服務憑證的設定錯誤在這裡就炸(太短、重複、沒名字)——
	// 一把不合格的 token 悄悄生效,比服務啟動失敗嚴重得多。
	verifier, err := newServiceVerifier(deps.ServiceTokens)
	if err != nil {
		return nil, fmt.Errorf("服務 token 設定有問題:%w", err)
	}
	otel, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, err
	}

	var queue *auditQueue
	if deps.EventLog != nil {
		queue = newAuditQueue(deps.EventLog, log)
	}

	// 由外而內:panic 復原 → OTel → event_logs → 認證。
	// request_id 由更外層的 auditHTTP middleware 產生(理由見 interceptors.go)。
	opts := []connect.HandlerOption{
		connect.WithInterceptors(
			recoverInterceptor(log),
			otel,
			eventLogInterceptor(queue),
			authInterceptor(auth, deps.Authorizer, verifier, deps.ActingUsers),
		),
		connect.WithReadMaxBytes(maxRequestBytes),
	}

	stateCookie := newStateCookieConfig(deps.StateCookiePath, basePath)

	mux := http.NewServeMux()
	mux.Handle(platformv1connect.NewAuthServiceHandler(authHandler{
		svc:     deps.Auth,
		cookie:  stateCookie,
		trusted: deps.TrustedProxies,
	}, opts...))
	mux.Handle(platformv1connect.NewMeServiceHandler(meHandler{
		profiles: deps.Profiles, writer: deps.ProfileWrites, privacy: deps.Privacy,
		catalog: deps.Catalog, ledger: deps.Ledger,
	}, opts...))
	mux.Handle(platformv1connect.NewDailyServiceHandler(dailyHandler{svc: deps.Daily}, opts...))
	mux.Handle(platformv1connect.NewShopServiceHandler(shopHandler{
		svc: deps.Shop, catalog: deps.Catalog, directory: deps.Directory,
	}, opts...))
	mux.Handle(platformv1connect.NewAdminEconomyServiceHandler(adminEconomyHandler{
		svc: deps.AdminEcon, shop: deps.Shop,
		directory: deps.Directory, entries: deps.LedgerEntries,
	}, opts...))
	mux.Handle(platformv1connect.NewActivityServiceHandler(activityHandler{svc: deps.Activity}, opts...))
	mux.Handle(platformv1connect.NewNotificationServiceHandler(
		notificationHandler{src: deps.Announcements}, opts...))

	// 瀏覽器用的登入路由(browserauth.go)。它們**不是** RPC:Discord 完成
	// 授權後是把使用者的瀏覽器重導到 redirect_uri,那是一個普通的 GET,
	// 沒有 Connect 的 header 也沒有 JSON body。少了這條路徑,登入走不完。
	browserAuth{
		svc:     deps.Auth,
		state:   stateCookie,
		session: newSessionCookieConfig(basePath),
		trusted: deps.TrustedProxies,
	}.register(mux)

	// 稽核在最外層才看得到前綴外的探測流量;順序不能反,
	// 理由見 auditHTTP 與 mountAt 的說明。
	// 由外而內:稽核 → /login 暫時轉址(在前綴之外)→ 剝前綴 → connect mux。
	return &Server{
		handler: auditHTTP(withLoginShim(basePath, mountAt(basePath, mux)), queue, log, basePath),
		audit:   queue,
	}, nil
}

// verifyProcedureCoverage 在啟動時用 proto descriptor 反查,確認每個 RPC 的
// 存取層級都是**刻意決定**的,而不是「忘了登記」的預設值。
//
// 為什麼要這道檢查:認證用白名單(漏登記 = 要登入,fail closed),
// 但授權是「哪些服務需要」(漏登記 = 只要登入就能用,fail open)。
// 方向相反的兩套規則放著不管,遲早會有人新增一個管理 RPC 而忘了配置。
// 這裡讓那種疏漏在**啟動時**就炸,而不是上線後被人發現。
func verifyProcedureCoverage() error {
	known := map[string]struct{}{}
	var problems []string

	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() != protoPackage {
			return true
		}
		services := fd.Services()
		for i := 0; i < services.Len(); i++ {
			sd := services.Get(i)
			name := string(sd.FullName())
			// 服務名開頭是 Admin 的,一律必須落在需要授權的那一側。
			adminish := strings.HasPrefix(string(sd.Name()), "Admin")
			// 服務名開頭是 Activity 或 Notification 的,一律必須落在
			// 「只收服務身分」那一側。與 Admin 那條同樣的理由:漏登記在這個方向
			// 是 fail open —— 活動記錄會變成 XP(任何登入使用者都能自己刷),
			// 而通知拉取能把待送公告認領走(任何登入使用者都能讓整個伺服器
			// 在可見性逾時之前收不到公告)。
			serviceOnly := strings.HasPrefix(string(sd.Name()), "Activity") ||
				strings.HasPrefix(string(sd.Name()), "Notification")
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				procedure := "/" + name + "/" + string(methods.Get(j).Name())
				known[procedure] = struct{}{}
				if adminish && !requiresAuthorization(procedure) {
					problems = append(problems,
						procedure+" 屬於管理服務卻不需要授權(privilegedServices 漏登記)")
				}
				if serviceOnly && !requiresServiceIdentity(procedure) {
					problems = append(problems,
						procedure+" 屬於只收服務身分的服務卻沒登記(serviceIdentityServices 漏登記)")
				}
				_, public := publicProcedures[procedure]
				if public && requiresAuthorization(procedure) {
					problems = append(problems,
						procedure+" 同時被列為公開與需要授權,兩者矛盾")
				}
				if public && requiresServiceIdentity(procedure) {
					problems = append(problems,
						procedure+" 同時被列為公開與只收服務身分,兩者矛盾")
				}
				if requiresServiceIdentity(procedure) && requiresAuthorization(procedure) {
					problems = append(problems,
						procedure+" 同時要求服務身分與使用者授權,沒有任何呼叫端滿足得了")
				}
			}
		}
		return true
	})

	if len(known) == 0 {
		return fmt.Errorf("找不到 %s 的 proto descriptor,生成碼可能沒被連進來", protoPackage)
	}
	// 公開白名單裡的每一條都必須真的存在:改了 proto 卻留下舊字串,
	// 結果是「本來想開放的 RPC 其實需要登入」——沉默的錯誤。
	for procedure := range publicProcedures {
		if _, ok := known[procedure]; !ok {
			problems = append(problems, procedure+" 在公開白名單裡但 proto 沒有這個 procedure")
		}
	}
	// 同理:serviceIdentityServices 裡列的服務名必須真的存在。改了 proto 卻
	// 留下舊字串,結果是「本來只給服務用的 RPC 變成任何登入者都能呼叫」。
	for _, svc := range serviceIdentityServices {
		if !knownService(known, svc) {
			problems = append(problems, svc+" 在服務身分清單裡但 proto 沒有這個服務")
		}
	}
	// subjectlessServices 同理,而且多一條:它必須是 serviceIdentityServices
	// 的子集。「沒有使用者主體」只在「只收服務身分」的前提下有意義 ——
	// 把一個使用者 RPC 列進去,結果會是「服務憑證打得到、卻不必說明代表誰」,
	// 那正是代打路徑最不能出現的形狀。
	for _, svc := range subjectlessServices {
		if !knownService(known, svc) {
			problems = append(problems, svc+" 在無主體服務清單裡但 proto 沒有這個服務")
			continue
		}
		if !knownService(serviceIdentitySet(known), svc) {
			problems = append(problems,
				svc+" 被列為無使用者主體,卻不在 serviceIdentityServices 裡")
		}
	}
	problems = append(problems, delegationProblems(known)...)
	if len(problems) > 0 {
		return fmt.Errorf("RPC 存取層級設定有問題:%s", strings.Join(problems, ";"))
	}
	return nil
}

// delegationProblems 檢查代打白名單(delegatedProcedures)。
//
// 為什麼是啟動時的硬檢查而不是註解上的約定:代打是唯一「服務憑證能讓請求
// 變成某個使用者」的路徑,清單寫錯的後果直接是提權或功能靜默失效,
// 兩者都要在上線前炸,不能等人在正式環境發現。
//
// 四道檢查:
//
//  1. procedure 必須真的存在於 proto —— 改了 proto 卻留下舊字串,結果是
//     「本來想開放代打的 RPC 其實不通」,而且完全沒有徵兆(沉默的錯誤)。
//  2. 不能與「只收服務」重疊 —— 那類 RPC 的主體刻意不是登入使用者
//     (handler 用 requireActingUser),兩種語意混在一起沒有人說得清主體是誰。
//  3. 不能與「需要授權」重疊 —— 代打路徑不經過 Authorize,列進去等於
//     讓管理 RPC 少一道授權檢查。這是最危險的一種設定錯誤。
//  4. 不能與 public 重疊 —— public 的 RPC 服務憑證本來就打得到,再列一次
//     就是同一個決定有兩個權威位置(專案第 9 條),日後兩邊改一半必然打架。
func delegationProblems(known map[string]struct{}) []string {
	var problems []string
	for procedure := range delegatedProcedures {
		if _, ok := known[procedure]; !ok {
			problems = append(problems, procedure+" 在代打白名單裡但 proto 沒有這個 procedure")
		}
		if requiresServiceIdentity(procedure) {
			problems = append(problems, procedure+" 同時被列為代打與只收服務身分,主體語意矛盾")
		}
		if requiresAuthorization(procedure) {
			problems = append(problems, procedure+" 同時被列為代打與需要授權,代打路徑不經過授權檢查")
		}
		if _, public := publicProcedures[procedure]; public {
			problems = append(problems, procedure+" 同時被列為代打與公開,公開的 RPC 不需要代打")
		}
	}
	// map 走訪順序不定,排序讓錯誤訊息可重現(測試與 log 才對得起來)。
	sort.Strings(problems)
	return problems
}

// serviceIdentitySet 把 known 收窄成「只收服務身分」的那些 procedure,
// 供 subjectlessServices 的子集檢查用。
func serviceIdentitySet(known map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(known))
	for procedure := range known {
		if requiresServiceIdentity(procedure) {
			out[procedure] = struct{}{}
		}
	}
	return out
}

// knownService 回報 descriptor 裡有沒有這個服務的任何一個 procedure。
func knownService(known map[string]struct{}, service string) bool {
	prefix := "/" + service + "/"
	for procedure := range known {
		if strings.HasPrefix(procedure, prefix) {
			return true
		}
	}
	return false
}
