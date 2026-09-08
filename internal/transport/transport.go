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
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
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

	// ── 入口層 port(見 ports.go)──
	Auth          AuthService     // Discord OAuth 登入流程
	Profiles      ProfileReader   // 我的檔案 / 全幣別餘額
	ProfileWrites ProfileWriter   // 改時區(同一個實作可同時滿足讀寫,傳兩次即可)
	Catalog       Catalog         // 商品 / 權益 / 工單列表
	LedgerEntries LedgerReader    // 管理端分錄查詢(Refund 的前置)
	Directory     Directory       // public_id ↔ 內部 id
	EventLog      eventlog.Logger // event_logs 寫入;nil = 不寫(僅限測試,正式一定要給)
	Authorizer    Authorizer      // 管理端授權;nil = 管理端一律 PermissionDenied

	// Authenticator 把 Bearer token 換成 user id;nil = UnauthenticatedStub(全部拒絕)。
	Authenticator Authenticator

	// TrustProxyHeaders 決定要不要用 X-Forwarded-For 當 session 的來源 IP。
	//
	// **預設 false**:XFF 是純請求標頭,誰都能填。只有在服務確定只接受
	// 來自可信反向代理的連線時才開;開著卻直接暴露在外網,等於讓稽核紀錄裡的
	// 來源 IP 變成攻擊者可自選的欄位。
	TrustProxyHeaders bool

	// StateCookiePath 是 OAuth state cookie 的 Path;空 = "/"。
	//
	// 為什麼預設不是 "/auth":ConnectRPC 的路徑是 procedure 全名
	// (/hestia.platform.v1.AuthService/CompleteDiscordLogin),設成 /auth
	// 會讓瀏覽器不把 cookie 送到回呼那支 RPC,登入流程靜默失敗。
	// 要收窄請填服務實際掛載的前綴。
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
			authInterceptor(auth, deps.Authorizer),
		),
		connect.WithReadMaxBytes(maxRequestBytes),
	}

	mux := http.NewServeMux()
	mux.Handle(platformv1connect.NewAuthServiceHandler(authHandler{
		svc:        deps.Auth,
		cookie:     newStateCookieConfig(deps.StateCookiePath),
		trustProxy: deps.TrustProxyHeaders,
	}, opts...))
	mux.Handle(platformv1connect.NewMeServiceHandler(meHandler{
		profiles: deps.Profiles, writer: deps.ProfileWrites,
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

	return &Server{handler: auditHTTP(mux, queue, log), audit: queue}, nil
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
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				procedure := "/" + name + "/" + string(methods.Get(j).Name())
				known[procedure] = struct{}{}
				if adminish && !requiresAuthorization(procedure) {
					problems = append(problems,
						procedure+" 屬於管理服務卻不需要授權(privilegedServices 漏登記)")
				}
				if _, public := publicProcedures[procedure]; public && requiresAuthorization(procedure) {
					problems = append(problems,
						procedure+" 同時被列為公開與需要授權,兩者矛盾")
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
	if len(problems) > 0 {
		return fmt.Errorf("RPC 存取層級設定有問題:%s", strings.Join(problems, ";"))
	}
	return nil
}
