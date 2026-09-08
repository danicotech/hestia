package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
)

// 責任鏈(由外而內):
//
//	auditHTTP(HTTP middleware)  request_id、X-Request-Id、補記沒進到 RPC 的請求
//	  └ recoverInterceptor       panic → Internal,絕不讓一個壞請求帶走整個進程
//	      └ otelconnect          span / metrics(trace_id 由此而來)
//	          └ eventLogInterceptor  event_logs 一 RPC 一列(schemas/13)
//	              └ authInterceptor  Bearer → user id;放在 event_logs 之內,
//	                                 被擋下的請求才會留下 status='denied' 的紀錄
//
// request_id 從 HTTP 層就產生(不是攔截器),因為 connect 在攔截器之前解碼:
// 解碼失敗、請求體超限的流量攔截器根本看不到,而那正是最該留痕的流量。

// recoverInterceptor 把 handler / 內層攔截器的 panic 轉成 Internal 錯誤。
// 它只**轉換**;event_logs 那一列由內層的 eventLogInterceptor 在重新 panic
// 之前寫好,兩者靠 request_id 關聯(所以 request_id 必須比它更外層)。
func recoverInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (res connect.AnyResponse, err error) {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "handler panic",
						"procedure", req.Spec().Procedure,
						"request_id", RequestID(ctx),
						"panic", fmt.Sprint(r),
						"stack", string(debug.Stack()),
					)
					res, err = nil, connectError(connect.CodeInternal, errInternal)
				}
			}()
			return next(ctx, req)
		}
	}
}

// publicProcedures 是不需要 Authorization 的 procedure。
// 白名單而非黑名單:新增 RPC 忘了登記,結果是「要登入」而不是「裸奔」。
var publicProcedures = map[string]struct{}{
	platformv1connect.AuthServiceStartDiscordLoginProcedure:    {},
	platformv1connect.AuthServiceCompleteDiscordLoginProcedure: {},
	platformv1connect.AuthServiceRefreshSessionProcedure:       {},
	platformv1connect.AuthServiceLogoutProcedure:               {},
	// 商品列表是公開型錄,未登入也要看得到(才有東西吸引人登入)。
	platformv1connect.ShopServiceListItemsProcedure: {},
}

// privilegedServices 是「除了登入還要授權」的服務(schemas/03-authz.md)。
//
// 用**服務前綴**而不是逐條列 RPC 是刻意的:逐條列舉在授權方向上是 fail open
// ——管理服務新增一個 RPC 忘了登記,任何登入使用者就能呼叫。前綴讓新 RPC
// 預設落在「需要授權」那一側,而「整個服務是否需要授權」在 New() 啟動時
// 用 proto descriptor 反查驗證(verifyProcedureCoverage),漏配置直接開不起來。
var privilegedServices = []string{
	platformv1connect.AdminEconomyServiceName,
}

func requiresAuthorization(procedure string) bool {
	for _, svc := range privilegedServices {
		if strings.HasPrefix(procedure, "/"+svc+"/") {
			return true
		}
	}
	return false
}

// serviceIdentityServices 是「只收服務身分」的服務(stentor 這類後端呼叫方)。
//
// 同樣用服務前綴而不是逐條列 RPC:逐條列在這個方向上也是 fail open ——
// ActivityService 新增一個 RPC 忘了登記,任何登入使用者就能拿它偽造活動記錄
// (而活動記錄會變成 XP)。前綴讓新 RPC 預設落在「只收服務」那一側,
// 而「有沒有漏掉整個服務」在 New() 啟動時用 proto descriptor 反查驗證。
var serviceIdentityServices = []string{
	platformv1connect.ActivityServiceName,
	platformv1connect.NotificationServiceName,
}

func requiresServiceIdentity(procedure string) bool {
	for _, svc := range serviceIdentityServices {
		if strings.HasPrefix(procedure, "/"+svc+"/") {
			return true
		}
	}
	return false
}

// subjectlessServices 是「只收服務身分,而且**沒有使用者主體**」的服務。
//
// 為什麼需要這個區分:serviceIdentityServices 原本隱含「主體 = X-Acting-User」
// ——ActivityService 的每一支 RPC 都在記某個人的活動,少了代表對象就是把
// 記錄寫到錯的人頭上,所以缺 header 一律拒絕。
//
// 通知拉取不是那種東西。**公告佇列不屬於任何人**:stentor 拉一批待送事件,
// 沒有「代表誰」可言。硬要它填一個 X-Acting-User,結果是 event_logs 把每一次
// 拉取都記在一個與這次操作毫無關係的使用者身上 —— 稽核紀錄說謊比沒有紀錄更糟。
//
// 三個守住的性質:
//
//   - **用服務前綴,不是逐條列 RPC。** 與 delegatedProcedures 相反:漏登記
//     一條代打是 fail closed(那支不能代打),但漏登記一條 subjectless 會讓
//     NotificationService 新增的 RPC 突然要求 X-Acting-User,呼叫端全部壞掉。
//     整個服務有沒有使用者主體是服務層級的性質,不是逐支決定的。
//   - **必須是 serviceIdentityServices 的子集。** 「沒有使用者主體」只在
//     「只收服務身分」的前提下有意義;啟動時斷言(verifyProcedureCoverage)。
//   - **它只放寬 X-Acting-User 這一項。** 服務憑證照樣要驗,使用者憑證照樣
//     被拒絕 —— 這裡沒有任何一道認證被繞過。
var subjectlessServices = []string{
	platformv1connect.NotificationServiceName,
}

func isSubjectless(procedure string) bool {
	for _, svc := range subjectlessServices {
		if strings.HasPrefix(procedure, "/"+svc+"/") {
			return true
		}
	}
	return false
}

// requiresActingUserHeader 回報這個 procedure 是否一定要帶 X-Acting-User。
func requiresActingUserHeader(procedure string) bool {
	return requiresServiceIdentity(procedure) && !isSubjectless(procedure)
}

// delegatedProcedures 是「服務可以代打」的使用者 RPC(第五類存取層級)。
//
// 為什麼需要這一類:Discord 使用者打 /daily、/balance、/shop 時,
// stentor 帶的是服務憑證(它沒有那個人的 session,鐵則 2 也不給它 DB),
// 但要做的事是**那個人的**事。沒有這條路,M2 的斜線指令全部不通。
//
// 三個必須守住的性質:
//
//   - **逐條列舉,不是服務前綴。** 與 privilegedServices / serviceIdentityServices
//     相反的方向:那兩份用前綴是因為漏登記等於 fail open;這份漏登記等於
//     「這個 RPC 不能代打」,是 fail closed,所以可以逐條列 —— 而且必須逐條列:
//     代打的風險逐個 RPC 都不同,「某服務的全部 RPC 都能代打」是不能給的授權。
//   - **代打只解決「主體是誰」,不解決「他能不能做」。** user_restrictions、
//     餘額、冪等、限購全部照常在 core 那層跑 —— 這裡一個檢查都沒有繞過。
//   - **清單與其他三份互斥**,啟動時斷言(verifyProcedureCoverage)。
//
// 刻意不在裡面的四類(不是漏,是決定):
//
//   - MeService/UpdateTimezone —— schemas/02:Discord 不提供時區,只有網頁端
//     拿得到。從 Discord 代打改時區沒有正確的值可填,只能填錯的。
//   - ShopService/RefundPurchase —— 退款要本人在網頁上確認。
//   - AuthService/* —— 登入本身不能被代打(那等於服務可以幫任何人登入)。
//   - AdminEconomyService/* —— 管理操作絕不代打(而且它需要使用者授權,
//     代打路徑根本不會跑到 Authorize)。
//   - ShopService/ListItems —— 它已經在 publicProcedures 裡(公開型錄),
//     服務憑證本來就打得到。再列一次就是同一個決定有兩個權威位置(專案第 9 條),
//     啟動斷言會直接擋下來。
var delegatedProcedures = map[string]struct{}{
	platformv1connect.DailyServiceClaimProcedure:         {},
	platformv1connect.MeServiceGetProfileProcedure:       {},
	platformv1connect.MeServiceGetBalanceProcedure:       {},
	platformv1connect.MeServiceListBalancesProcedure:     {},
	platformv1connect.MeServiceListEntitlementsProcedure: {},
	platformv1connect.MeServiceListRedemptionsProcedure:  {},
	// 隱私設定:使用者的 Discord 身分由 Discord 自己證明,從 /privacy 調整
	// **自己的**設定是合理的 —— 而且這是唯一做得出 `/privacy optout` 的路徑
	// (網頁端要先登入,而最需要退出的人正是只在 Discord 出沒的人)。
	//
	// 與刻意排除的 UpdateTimezone 的差別:那支代打不了是因為 Discord
	// **拿不到正確的值**可填(schemas/02:Discord 不提供時區),不是因為風險。
	// 隱私設定的值就是使用者當場在指令裡選的,沒有這個問題。
	platformv1connect.MeServiceGetPrivacyProcedure:         {},
	platformv1connect.MeServiceUpdatePrivacyProcedure:      {},
	platformv1connect.ShopServicePurchaseProcedure:         {},
	platformv1connect.ShopServiceCancelRedemptionProcedure: {},
}

// delegableGlobally 回報這個 procedure **在契約上**能不能被代打
// (與是哪一把憑證無關)。
//
// 後兩個條件是執行期的兜底:代打路徑**不經過** Authorize,也不是服務身分
// 該用的路徑,所以就算有人把管理 RPC 或活動記錄 RPC 加進白名單,這裡也拒絕。
// 正常情況下它們永遠是 false —— 啟動斷言會讓那種設定根本開不起來。
func delegableGlobally(procedure string) bool {
	if _, ok := delegatedProcedures[procedure]; !ok {
		return false
	}
	return !requiresAuthorization(procedure) && !requiresServiceIdentity(procedure)
}

// allowsDelegation 回報**這一把憑證**能不能代打這支 procedure。
//
// 兩道條件缺一不可,而且順序有意義:
//
//	delegableGlobally  這支 RPC 在契約上可不可以被代打(全域決定)
//	entry.canDelegate  這把憑證有沒有被授予這一支(per-token scope)
//
// 全域白名單管「這件事可不可以透過代打做」,scope 管「這個呼叫方可不可以做」。
// 少了 scope 那一半(QA 2026-09-08 之前的狀態),任何一把有效服務 token 都
// 自動取得完整白名單 —— 包含花錢的 ShopService/Purchase。一把憑證外洩的
// 爆炸半徑等於全部已綁定使用者的餘額,而輪替一把 token 的成本遠低於此。
func allowsDelegation(procedure string, entry *serviceEntry) bool {
	return delegableGlobally(procedure) && entry.canDelegate(procedure)
}

// authInterceptor 決定「這次呼叫是誰」。兩條互斥的路徑:
//
//	X-Service-Token   → 服務身分 + X-Acting-User 解出的行為主體
//	Authorization     → 使用者身分(Bearer → 內部 user id)
//
// 兩者同時出現 = 拒絕。不做「擇一採用」的猜測:猜測邏輯正是提權漏洞的來源。
func authInterceptor(
	auth Authenticator, authz Authorizer, verifier *serviceVerifier, actors activitylog.ActorResolver,
) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			procedure := req.Spec().Procedure
			serviceToken := req.Header().Get(headerServiceToken)
			bearer, hasBearer := bearerToken(req.Header().Get("Authorization"))

			if serviceToken != "" {
				if err := authenticateService(ctx, req, procedure, verifier, actors, hasBearer); err != nil {
					return nil, err
				}
				return next(ctx, req)
			}

			if _, ok := publicProcedures[procedure]; ok {
				return next(ctx, req)
			}
			if !hasBearer {
				return nil, toConnectError(ErrUnauthenticated)
			}
			userID, err := auth.Authenticate(ctx, bearer)
			if err != nil {
				return nil, toConnectError(err)
			}
			if userID == 0 {
				// 實作回了「成功但沒有身分」——當作認證失敗,不讓 0 冒充使用者。
				return nil, toConnectError(ErrUnauthenticated)
			}
			// 身分一確定就寫進便條,**在授權判斷之前**:
			// 提權嘗試被擋下的那一列,正是最需要知道行為人是誰的紀錄。
			if st := stateFrom(ctx); st != nil {
				st.setUserID(userID)
			}
			// 使用者 token 打到只收服務的 RPC:身分有效但不是這種身分。
			// 刻意放在寫進便條之後 —— 這一列必須看得出是誰在試。
			if requiresServiceIdentity(procedure) {
				return nil, connectError(connect.CodePermissionDenied, errUserOnServiceRPC)
			}
			if requiresAuthorization(procedure) {
				if authz == nil {
					return nil, toConnectError(ErrPermissionDenied)
				}
				if err := authz.Authorize(ctx, userID, procedure); err != nil {
					return nil, toConnectError(err)
				}
			}
			return next(ctx, req)
		}
	}
}

var (
	// errMixedCredentials:同時帶服務與使用者憑證。拒絕而不是擇一。
	errMixedCredentials = errors.New("不可同時提供 " + headerServiceToken + " 與 Authorization")
	// errServiceOnUserRPC:服務身分打到**不在代打白名單**的使用者 RPC。
	// 「代表某人」不等於「就是某人」:能代打哪幾支是逐條決定的
	// (delegatedProcedures),沒列到的一律拒絕。
	errServiceOnUserRPC = errors.New("這個使用者 RPC 不接受代打")
	// errDelegationNotGranted:這支 RPC **可以**被代打,但這把憑證沒有被授權。
	// 與上面那個分開是刻意的:一個是契約問題(誰來都不行),一個是設定問題
	// (換一把有 scope 的憑證就行)。混成一個訊號,呼叫端就分不出該改程式
	// 還是該改部署設定 —— 而這正是 reason 要解決的那類問題。
	errDelegationNotGranted = errors.New("這把服務憑證沒有代打這個 RPC 的授權")
	// errUserOnServiceRPC:使用者身分打到只收服務的 RPC。
	errUserOnServiceRPC = errors.New("這個 RPC 只接受服務身分")
	// errActingUserRequired:只收服務的 RPC 一定要說明代表誰。
	errActingUserRequired = errors.New("缺少 " + headerActingUser)
)

// authenticateService 走服務憑證那條路:驗 token → 判定可呼叫的 RPC 範圍
// → 解析 X-Acting-User。全部成功才回 nil。
//
// 三種可呼叫的範圍,語意各不相同:
//
//	只收服務的 RPC(ActivityService)  主體 = X-Acting-User,但**不是登入使用者**
//	                                 (handler 用 requireActingUser 取)
//	代打白名單內的使用者 RPC          主體 = X-Acting-User,而且**就是這次請求的
//	                                 使用者**(handler 照常用 requireUser 取)
//	public RPC                       不需要主體;帶了也只影響稽核的 channel
//
// 中間那類是唯一會把 acting user 寫進 st.userID 的路徑,而且只在
// allowsDelegation 為真時發生 —— 白名單外的使用者 RPC 在更前面就被擋掉了。
func authenticateService(
	ctx context.Context, req connect.AnyRequest, procedure string,
	verifier *serviceVerifier, actors activitylog.ActorResolver, hasBearer bool,
) error {
	if hasBearer {
		return connectError(connect.CodePermissionDenied, errMixedCredentials)
	}
	entry, ok := verifier.verify(req.Header().Get(headerServiceToken))
	if !ok {
		// 無效 token 一律 Unauthenticated,且訊息不透露任何比對細節。
		return toConnectError(ErrUnauthenticated)
	}
	st := stateFrom(ctx)
	if st != nil {
		st.setService(entry.name)
	}

	_, isPublic := publicProcedures[procedure]
	needsService := requiresServiceIdentity(procedure)
	delegated := allowsDelegation(procedure, entry)
	if !needsService && !isPublic && !delegated {
		// 分辨兩種拒絕:契約上就不能代打,還是這把憑證沒被授權。
		if delegableGlobally(procedure) {
			return connectError(connect.CodePermissionDenied, errDelegationNotGranted)
		}
		return connectError(connect.CodePermissionDenied, errServiceOnUserRPC)
	}

	// public RPC:**X-Acting-User 一律不解析成主體**(QA 2026-09-08)。
	//
	// 在此之前,服務憑證打任何 public procedure(包含整個 AuthService)只要
	// 帶上 header,event_logs 就會記成「某個真實使用者從 discord 做了這件事」,
	// 而且 delegated 旗標不會設 —— 看起來與本人操作一模一樣。
	// 實測 AuthService/Logout 被記成 user_id=<真人>、channel=discord,
	// 而代打白名單的註解正宣稱「登入本身不能被代打」。稽核說謊比沒有稽核更糟。
	//
	// 保留的只有 provider(而且它經過白名單比對,不是呼叫端的自由字串):
	// 「這個請求從 Discord 進來」是真的,值得記進 channel;
	// 「它代表某個人」不是真的,所以不解析、不查 identities、不寫主體。
	raw := req.Header().Get(headerActingUser)
	if isPublic && !needsService && !delegated {
		if raw == "" {
			return nil
		}
		provider, _, err := parseActingUser(raw)
		if err != nil {
			return connectError(connect.CodeInvalidArgument, err)
		}
		if st != nil {
			st.setActing(provider, 0, false)
		}
		return nil
	}

	if raw == "" {
		// 有使用者主體的服務 RPC(ActivityService)與代打都必須說明代表誰。
		// 代打尤其不能省:沒有主體的「代打」就是服務自己在動別人的錢,絕不允許。
		//
		// 例外是 subjectlessServices(通知拉取):那裡根本沒有代表對象,
		// 要求一個只會逼呼叫端填一個假的進來。
		if requiresActingUserHeader(procedure) || delegated {
			return connectError(connect.CodeInvalidArgument, errActingUserRequired)
		}
		return nil
	}
	provider, externalID, err := parseActingUser(raw)
	if err != nil {
		return connectError(connect.CodeInvalidArgument, err)
	}
	// provider 先記下來:即使綁定查不到,event_logs 的 channel 也要是 discord。
	// 第三個參數是「這個 acting user 算不算本次操作的主體」——
	// 這一步還沒解出人,當然不算。
	if st != nil {
		st.setActing(provider, 0, false)
	}
	if actors == nil {
		return connectError(connect.CodeUnimplemented, errors.New("身分解析尚未提供"))
	}
	userID, err := actors.ResolveActor(ctx, provider, externalID)
	if err != nil {
		// ErrActorNotLinked 會被 errmap 映射成 FailedPrecondition:
		// 呼叫端據此知道「這個 Discord 使用者還沒綁定」,而不是盲目重試。
		return toConnectError(err)
	}
	if userID == 0 {
		// 實作回了「成功但沒有身分」——當作解析失敗,不讓 0 冒充使用者
		// (與 Bearer 那條路徑同一道防線)。代打尤其不能放行:
		// 主體是 0 的代打等於「服務代表沒有人動錢」。
		return toConnectError(ErrUnauthenticated)
	}
	if st != nil {
		// needsService 那一類(ActivityService)的主體就是 acting user,
		// handler 用 requireActingUser 取,event_logs 也記在他頭上 ——
		// 所以這裡明確標記「acting user 是主體」,而不是讓稽核層自己猜。
		st.setActing(provider, userID, needsService)
		if delegated {
			// 代打成立:這次請求的主體**就是**代打對象,handler 用 requireUser
			// 拿到的是他。同時記下 delegated 旗標 —— 稽核必須分得出
			// 「他自己做的」與「閘道代他做的」,否則出事查不出責任歸屬。
			st.setDelegated(userID)
		}
	}
	return nil
}

// bearerToken 解析 "Bearer <token>";大小寫不敏感(RFC 7235 的 scheme 不分大小寫)。
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
