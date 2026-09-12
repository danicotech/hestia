package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/core/platform/play"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// 錯誤映射逐項驗證。這張表是 client 重試策略的依據,改動要有意識。
func TestToConnectError(t *testing.T) {
	cases := []struct {
		err  error
		want connect.Code
	}{
		{ledger.ErrInsufficientBalance, connect.CodeFailedPrecondition},
		{ledger.ErrIdempotencyConflict, connect.CodeAborted},
		{ledger.ErrInFlight, connect.CodeUnavailable},
		{ledger.ErrInvalidOp, connect.CodeInvalidArgument},

		{play.ErrNotFound, connect.CodeNotFound},
		{play.ErrDisabled, connect.CodeFailedPrecondition},
		{play.ErrDailyLimit, connect.CodeResourceExhausted},
		{play.ErrPoolEmpty, connect.CodeFailedPrecondition},
		{play.ErrAlreadyEntered, connect.CodeAlreadyExists},
		{play.ErrClosed, connect.CodeFailedPrecondition},
		{play.ErrNotOwner, connect.CodePermissionDenied},
		{play.ErrInvalidParams, connect.CodeInvalidArgument},

		{daily.ErrAlreadyClaimed, connect.CodeAlreadyExists},
		{daily.ErrTimezoneChangeCooldown, connect.CodeFailedPrecondition},
		{daily.ErrLedgerStateConflict, connect.CodeInternal},
		{daily.ErrInvalidConfig, connect.CodeInternal},

		{shop.ErrInvalidRequest, connect.CodeInvalidArgument},
		{shop.ErrUserNotFound, connect.CodeNotFound},
		{shop.ErrItemNotFound, connect.CodeNotFound},
		{shop.ErrEntitlementNotFound, connect.CodeNotFound},
		{shop.ErrRedemptionNotFound, connect.CodeNotFound},
		{shop.ErrItemNotListed, connect.CodeFailedPrecondition},
		{shop.ErrPerUserLimitReached, connect.CodeFailedPrecondition},
		{shop.ErrAlreadyRevoked, connect.CodeFailedPrecondition},
		{shop.ErrRefundWindowClosed, connect.CodeFailedPrecondition},
		{shop.ErrRedemptionNotPending, connect.CodeFailedPrecondition},
		{shop.ErrNotEntitlementOwner, connect.CodePermissionDenied},
		{shop.ErrNotRedemptionOwner, connect.CodePermissionDenied},
		{shop.ErrLedgerStateConflict, connect.CodeInternal},

		{adminecon.ErrReasonRequired, connect.CodeInvalidArgument},
		{adminecon.ErrInvalidInput, connect.CodeInvalidArgument},
		{adminecon.ErrEntryNotFound, connect.CodeNotFound},
		{adminecon.ErrNotRefundable, connect.CodeFailedPrecondition},
		{adminecon.ErrAlreadyRefunded, connect.CodeAlreadyExists},

		// 活動記錄:兩個「系統狀態沒準備好」用 FailedPrecondition —— 重試無用,
		// 呼叫端該做的是引導綁定 / 註冊 guild,不是退避重打。
		{activitylog.ErrActorNotLinked, connect.CodeFailedPrecondition},
		{activitylog.ErrSpaceNotRegistered, connect.CodeFailedPrecondition},
		{activitylog.ErrInvalidRequest, connect.CodeInvalidArgument},

		// identity:憑證類一律 unauthenticated(意思都是「重新登入」)。
		{identity.ErrInvalidToken, connect.CodeUnauthenticated},
		{identity.ErrTokenExpired, connect.CodeUnauthenticated},
		{identity.ErrSessionNotFound, connect.CodeUnauthenticated},
		{identity.ErrSessionRevoked, connect.CodeUnauthenticated},
		{identity.ErrSessionExpired, connect.CodeUnauthenticated},
		{identity.ErrTokenReuseDetected, connect.CodeUnauthenticated},
		{identity.ErrInvalidState, connect.CodeInvalidArgument},
		{identity.ErrStateExpired, connect.CodeInvalidArgument},
		// state 與瀏覽器 cookie 不符 = login CSRF 的訊號,不是「你沒登入」。
		// 用 denied 讓它在 event_logs 一眼可見。
		{identity.ErrStateMismatch, connect.CodePermissionDenied},
		{identity.ErrInvalidRedirect, connect.CodeInvalidArgument},
		{identity.ErrProviderExchange, connect.CodeUnavailable},
		{identity.ErrAccountDeleted, connect.CodePermissionDenied},
		{identity.ErrInvalidConfig, connect.CodeInternal},

		// 通知拉取:參數不合法(一次 Ack 太多筆)。
		{notification.ErrInvalidRequest, connect.CodeInvalidArgument},

		// 入口層自己的憑證錯誤:它們不是 core sentinel,但同樣在表上
		// (為了有穩定的 reason),所以同樣要驗 code。
		{errMixedCredentials, connect.CodePermissionDenied},
		{errServiceOnUserRPC, connect.CodePermissionDenied},
		{errDelegationNotGranted, connect.CodePermissionDenied},
		{errUserOnServiceRPC, connect.CodePermissionDenied},
		{errActingUserRequired, connect.CodeInvalidArgument},
		{errActingUserFormat, connect.CodeInvalidArgument},
		{errActingUserProvider, connect.CodeInvalidArgument},

		// 瀏覽器登入流程:錯誤碼會進導回網址,所以與 RPC 共用同一張表。
		{errOAuthDenied, connect.CodePermissionDenied},
		{errOAuthCallbackInvalid, connect.CodeInvalidArgument},
		{errLoginFailed, connect.CodeInternal},

		{ErrNotFound, connect.CodeNotFound},
		{ErrUnauthenticated, connect.CodeUnauthenticated},
		{ErrPermissionDenied, connect.CodePermissionDenied},

		{context.Canceled, connect.CodeCanceled},
		{context.DeadlineExceeded, connect.CodeDeadlineExceeded},
	}
	for _, c := range cases {
		t.Run(c.err.Error(), func(t *testing.T) {
			got := toConnectError(c.err)
			if code := connect.CodeOf(got); code != c.want {
				t.Fatalf("code = %v, 期望 %v", code, c.want)
			}
			// 包過一層的錯誤(實作常用 fmt.Errorf 加上下文)也要認得出來。
			wrapped := toConnectError(fmt.Errorf("實作層上下文: %w", c.err))
			if code := connect.CodeOf(wrapped); code != c.want {
				t.Fatalf("包裝後 code = %v, 期望 %v", code, c.want)
			}
		})
	}
	// errorCodes 是唯一映射表:漏測就是漏映射。
	if len(cases) != len(errorCodes) {
		t.Fatalf("測到 %d 條映射,errorCodes 有 %d 條——新增映射要一併加測試", len(cases), len(errorCodes))
	}
}

// 未映射的錯誤一律 Internal,且訊息不外洩(可能含 SQL / 內部 id)。
func TestToConnectError_UnknownIsInternalAndOpaque(t *testing.T) {
	raw := errors.New("pq: relation platform.token_entries does not exist (user_id=42)")
	got := toConnectError(raw)
	if code := connect.CodeOf(got); code != connect.CodeInternal {
		t.Fatalf("code = %v, 期望 internal", code)
	}
	var ce *connect.Error
	if !errors.As(got, &ce) {
		t.Fatal("期望 *connect.Error")
	}
	if ce.Message() != errInternal.Error() {
		t.Fatalf("訊息外洩了:%q", ce.Message())
	}
}

func TestToConnectError_NilAndOwnPassthrough(t *testing.T) {
	if toConnectError(nil) != nil {
		t.Fatal("nil 應該還是 nil")
	}
	// 本套件產生的錯誤原樣放行:訊息是我們自己寫給使用者看的。
	own := invalidArgument("item_public_id 必填")
	got := toConnectError(own)
	if connect.CodeOf(got) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v", connect.CodeOf(got))
	}
	var ce *connect.Error
	if !errors.As(got, &ce) || ce.Message() != "item_public_id 必填" {
		t.Fatalf("自家錯誤的訊息應保留,got %v", got)
	}
}

// port 實作回的 connect.Error 不可信:code 保留(client 重試策略需要),
// 訊息換掉(可能含 SQL、內部 id、他人資料)。
func TestToConnectError_ForeignConnectErrorIsSanitized(t *testing.T) {
	foreign := connect.NewError(connect.CodeNotFound,
		errors.New("pq: user 7 not found in platform.users"))
	got := toConnectError(foreign)
	if code := connect.CodeOf(got); code != connect.CodeNotFound {
		t.Fatalf("code 應保留,got %v", code)
	}
	var ce *connect.Error
	if !errors.As(got, &ce) {
		t.Fatal("期望 *connect.Error")
	}
	if ce.Message() != errUntrusted.Error() {
		t.Fatalf("外部訊息外洩:%q", ce.Message())
	}
	if trusted(foreign) {
		t.Fatal("外部錯誤不該被當成自家的")
	}
	// 稽核也不能記外部訊息。
	if _, ok := errorSummary(foreign)["message"]; ok {
		t.Fatal("不可信的錯誤訊息不該進 event_logs")
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header  string
		want    string
		wantOK  bool
		comment string
	}{
		{"Bearer abc123", "abc123", true, "標準"},
		{"bearer abc123", "abc123", true, "scheme 不分大小寫"},
		{"BEARER  abc123 ", "abc123", true, "多餘空白"},
		{"", "", false, "沒帶"},
		{"Bearer ", "", false, "空 token"},
		{"Basic abc123", "", false, "非 bearer"},
		{"abc123", "", false, "沒有 scheme"},
	}
	for _, c := range cases {
		t.Run(c.comment, func(t *testing.T) {
			got, ok := bearerToken(c.header)
			if got != c.want || ok != c.wantOK {
				t.Fatalf("bearerToken(%q) = %q,%v;期望 %q,%v", c.header, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// 白名單摘要:憑證類訊息完全缺席,自由文字不記。
func TestSummary_WhitelistExcludesSecrets(t *testing.T) {
	secretRequests := []any{
		&platformv1.CompleteDiscordLoginRequest{Code: "oauth-code", State: "state"},
		&platformv1.RefreshSessionRequest{RefreshToken: "refresh-token"},
		&platformv1.LogoutRequest{RefreshToken: "refresh-token"},
		&platformv1.StartDiscordLoginRequest{RedirectUri: "https://example.test"},
	}
	for _, msg := range secretRequests {
		if got := requestSummary("", msg); got != nil {
			t.Fatalf("%T 不該有摘要,got %v", msg, got)
		}
	}
	secretResponses := []any{
		&platformv1.CompleteDiscordLoginResponse{Session: &platformv1.Session{AccessToken: "at"}},
		&platformv1.RefreshSessionResponse{Session: &platformv1.Session{RefreshToken: "rt"}},
	}
	for _, msg := range secretResponses {
		if got := responseSummary("", msg); got != nil {
			t.Fatalf("%T 不該有摘要,got %v", msg, got)
		}
	}

	// 管理動作的 reason / note 權威在 admin_audit_logs / redemptions,這裡不重複存。
	grant := requestSummary("", &platformv1.GrantRequest{
		UserPublicId: "U1", Currency: "coin", Amount: 100,
		Reason: "補償上週停機", IdempotencyKey: "k1",
	})
	if _, ok := grant["reason"]; ok {
		t.Fatal("reason 不該進 event_logs")
	}
	if grant["user_public_id"] != "U1" || grant["amount"].(int64) != 100 {
		t.Fatalf("摘要內容不對:%v", grant)
	}

	handle := requestSummary("", &platformv1.HandleRedemptionRequest{
		RedemptionPublicId: "R1", Approve: true, Note: strPtr("私人備註"),
	})
	if _, ok := handle["note"]; ok {
		t.Fatal("note 不該進 event_logs")
	}
}

// 未登記的訊息型別回 nil:漏加摘要的後果是少記,不是外洩。
func TestSummary_UnknownTypeIsNil(t *testing.T) {
	type unknown struct{}
	if got := requestSummary("", &unknown{}); got != nil {
		t.Fatalf("未知型別應回 nil,got %v", got)
	}
	if got := responseSummary("", &unknown{}); got != nil {
		t.Fatalf("未知型別應回 nil,got %v", got)
	}
}

// Internal 錯誤的訊息不進 DB(可能含內部細節);其餘 code 的 sentinel 文案可保存。
func TestErrorSummary(t *testing.T) {
	internal := errorSummary(connect.NewError(connect.CodeInternal, errors.New("pq: ...")))
	if internal["code"] != "internal" {
		t.Fatalf("code = %v", internal["code"])
	}
	if _, ok := internal["message"]; ok {
		t.Fatal("internal 的訊息不該入庫")
	}
	domain := errorSummary(toConnectError(shop.ErrRefundWindowClosed))
	if domain["message"] != shop.ErrRefundWindowClosed.Error() {
		t.Fatalf("領域錯誤訊息應保留,got %v", domain["message"])
	}
}

func strPtr(s string) *string { return &s }

// 摘要裡的字串一律有上限:沒有的話,任一有效 session 都能灌爆 event_logs。
func TestMarshalSummaryTruncates(t *testing.T) {
	long := strings.Repeat("x", summaryMaxStringBytes*4)
	raw := marshalSummary(map[string]any{"idempotency_key": long, "amount": int64(1)})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("摘要不是合法 JSON:%v", err)
	}
	if n := len(got["idempotency_key"].(string)); n > summaryMaxStringBytes {
		t.Fatalf("截斷失效:%d bytes", n)
	}
	if got["truncated"] != true {
		t.Fatal("截斷了就要標記")
	}
	// 沒超過就不該多出標記(正常請求的摘要不要噪音)。
	raw = marshalSummary(map[string]any{"item_public_id": "ITEM-1"})
	if strings.Contains(string(raw), "truncated") {
		t.Fatalf("沒截斷不該有標記:%s", raw)
	}
}

// 多位元組字元不能被切成半個 rune(切壞會讓 JSONB 寫入失敗)。
func TestTruncateUTF8KeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("中", 200) // 每字 3 bytes
	cut, did := truncateUTF8(s, summaryMaxStringBytes)
	if !did {
		t.Fatal("應該有截斷")
	}
	if !utf8.ValidString(cut) {
		t.Fatalf("切壞了 rune:%q", cut)
	}
	if len(cut) > summaryMaxStringBytes {
		t.Fatalf("超過上限:%d", len(cut))
	}
}

// 啟動時的存取層級檢查:公開白名單留了不存在的 procedure(改 proto 後忘了同步)
// 必須在 New() 就炸,而不是讓「本來想公開的 RPC」默默變成要登入。
func TestVerifyProcedureCoverage_StalePublicEntry(t *testing.T) {
	if err := verifyProcedureCoverage(); err != nil {
		t.Fatalf("目前的設定應該是乾淨的:%v", err)
	}
	const stale = "/hestia.platform.v1.ShopService/NoSuchMethod"
	publicProcedures[stale] = struct{}{}
	defer delete(publicProcedures, stale)
	if err := verifyProcedureCoverage(); err == nil {
		t.Fatal("白名單有不存在的 procedure 時應該回錯")
	}
}

// 管理服務的每一條 RPC 都必須落在「需要授權」那一側:
// 逐條列舉在授權方向上是 fail open,新增 RPC 忘了登記就會裸奔。
func TestAdminServiceIsFullyPrivileged(t *testing.T) {
	for _, proc := range []string{
		platformv1connect.AdminEconomyServiceGrantProcedure,
		platformv1connect.AdminEconomyServiceDeductProcedure,
		platformv1connect.AdminEconomyServiceRefundProcedure,
		platformv1connect.AdminEconomyServiceHandleRedemptionProcedure,
		platformv1connect.AdminEconomyServiceListEntriesProcedure,
		// 還不存在的方法也要被前綴涵蓋(這正是用前綴而不是列舉的理由)。
		"/hestia.platform.v1.AdminEconomyService/FutureMethod",
	} {
		if !requiresAuthorization(proc) {
			t.Fatalf("%s 應該需要授權", proc)
		}
	}
	if requiresAuthorization(platformv1connect.DailyServiceClaimProcedure) {
		t.Fatal("一般 RPC 不該需要管理授權")
	}
}
