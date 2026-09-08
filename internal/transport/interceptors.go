package transport

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
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

// authInterceptor 取 Authorization: Bearer,換成內部 user id 寫進便條。
func authInterceptor(auth Authenticator, authz Authorizer) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			procedure := req.Spec().Procedure
			if _, ok := publicProcedures[procedure]; ok {
				return next(ctx, req)
			}
			token, ok := bearerToken(req.Header().Get("Authorization"))
			if !ok {
				return nil, toConnectError(ErrUnauthenticated)
			}
			userID, err := auth.Authenticate(ctx, token)
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

// bearerToken 解析 "Bearer <token>";大小寫不敏感(RFC 7235 的 scheme 不分大小寫)。
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
