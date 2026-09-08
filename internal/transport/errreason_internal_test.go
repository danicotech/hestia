package transport

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// reason 是**契約的一部分**:呼叫端會依它決定顯示登入引導還是餘額不足。
// 這一檔守住三件事:每個 sentinel 都有 reason、reason 值穩定且唯一、
// 未映射的內部錯誤絕不帶 reason。

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestEveryMappedErrorHasStableReason(t *testing.T) {
	seen := map[string]error{}
	for _, m := range errorCodes {
		if m.reason == "" {
			t.Errorf("%v 沒有 reason —— 同一個 code 下有多個成因時,呼叫端就只能比對訊息", m.err)
			continue
		}
		if !snakeCase.MatchString(m.reason) {
			t.Errorf("reason %q 不是 snake_case", m.reason)
		}
		if prev, dup := seen[m.reason]; dup {
			t.Errorf("reason %q 被兩個 sentinel 共用(%v / %v)——那就分辨不出成因了",
				m.reason, prev, m.err)
		}
		seen[m.reason] = m.err
		if got := reasonFor(m.err); got != m.reason {
			t.Errorf("reasonFor(%v) = %q,想要 %q", m.err, got, m.reason)
		}
	}
}

// 值鎖死:改名等同破壞性變更,必須是有意識的動作(改了這裡就要改
// proto/README.md 的清單與呼叫端)。這裡釘住最容易被誤用的那幾個。
func TestReasonValuesAreFrozen(t *testing.T) {
	want := map[string]error{
		"actor_not_linked":       activitylog.ErrActorNotLinked,
		"insufficient_balance":   ledger.ErrInsufficientBalance,
		"per_user_limit_reached": shop.ErrPerUserLimitReached,
		"refund_window_closed":   shop.ErrRefundWindowClosed,
		"user_not_found":         shop.ErrUserNotFound,
		"item_not_found":         shop.ErrItemNotFound,
		"unauthenticated":        ErrUnauthenticated,
		"permission_denied":      ErrPermissionDenied,
	}
	for reason, err := range want {
		if got := reasonFor(err); got != reason {
			t.Errorf("reasonFor(%v) = %q,想要 %q", err, got, reason)
		}
	}
}

// 同一個 code 下的多個成因必須分得開 —— 這正是這個功能存在的理由。
func TestSameCodeDifferentReasons(t *testing.T) {
	group := []error{
		activitylog.ErrActorNotLinked,
		ledger.ErrInsufficientBalance,
		shop.ErrPerUserLimitReached,
		shop.ErrRefundWindowClosed,
	}
	seen := map[string]bool{}
	for _, err := range group {
		ce := toConnectError(err)
		if connect.CodeOf(ce) != connect.CodeFailedPrecondition {
			t.Fatalf("%v 的 code = %v,這組應該都是 FailedPrecondition", err, connect.CodeOf(ce))
		}
		reason := ErrorReason(ce)
		if reason == "" || seen[reason] {
			t.Errorf("%v 的 reason = %q(空或重複)——閘道會分不出這四種情況", err, reason)
		}
		seen[reason] = true
	}
}

// 未映射的錯誤:Internal + 固定文案 + **沒有 reason**。
// 帶 reason 等於把內部錯誤的分類洩漏出去。
func TestUnmappedErrorsCarryNoReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"陌生錯誤", errors.New("pq: relation \"secret\" does not exist")},
		{"包過的陌生錯誤", errors.Join(errors.New("a"), errors.New("b"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ce := toConnectError(c.err)
			if connect.CodeOf(ce) != connect.CodeInternal {
				t.Fatalf("code = %v,想要 Internal", connect.CodeOf(ce))
			}
			if r := ErrorReason(ce); r != "" {
				t.Errorf("未映射的錯誤帶了 reason %q", r)
			}
		})
	}
}

// 臨時產生的參數/未實作錯誤沒有 reason:code 已經說完了全部。
func TestAdHocErrorsCarryNoReason(t *testing.T) {
	for _, err := range []error{
		invalidArgument("currency 必填"),
		unimplemented("MeService.GetPrivacy"),
	} {
		if r := ErrorReason(err); r != "" {
			t.Errorf("%v 不該有 reason,拿到 %q", err, r)
		}
	}
}

// 攔截器的憑證錯誤也要有 reason:呼叫端要分得出「我兩種憑證都帶了」
// 與「這支 RPC 不接受代打」。
func TestCredentialErrorsHaveReasons(t *testing.T) {
	want := map[string]error{
		"mixed_credentials":    errMixedCredentials,
		"service_on_user_rpc":  errServiceOnUserRPC,
		"user_on_service_rpc":  errUserOnServiceRPC,
		"acting_user_required": errActingUserRequired,
	}
	for reason, err := range want {
		ce := connectError(connect.CodePermissionDenied, err)
		if got := ErrorReason(ce); got != reason {
			t.Errorf("%v 的 reason = %q,想要 %q", err, got, reason)
		}
	}
}

// errorSummary 會把 reason 寫進 event_logs;內部錯誤那一列不會有。
func TestErrorSummaryIncludesReason(t *testing.T) {
	got := errorSummary(toConnectError(activitylog.ErrActorNotLinked))
	if got["reason"] != "actor_not_linked" {
		t.Errorf("event_logs 摘要少了 reason:%v", got)
	}
	internal := errorSummary(toConnectError(errors.New("內部細節")))
	if _, ok := internal["reason"]; ok {
		t.Errorf("內部錯誤不該在稽核裡留下分類:%v", internal)
	}
}

// 對外的 reason 清單住在 proto/README.md(呼叫端的依據),權威值住在
// errorCodes。兩份不同步 = 文件說謊,而 client 是照文件寫的。
//
// 這個測試就是那條「一個概念一個權威」的保險絲:表格漏一列、多一列、
// 或 code 寫錯,這裡都會失敗。它讀的是真的檔案,不是複製一份常數。
func TestReasonTableMatchesDoc(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "proto", "README.md"))
	if err != nil {
		t.Fatalf("讀 proto/README.md: %v", err)
	}
	row := regexp.MustCompile(`(?m)^\| ` + "`" + `([a-z0-9_]+)` + "`" + ` \| ([a-z_]+) \|`)
	documented := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		if prev, dup := documented[m[1]]; dup {
			t.Errorf("文件裡 %q 出現兩次(%s / %s)", m[1], prev, m[2])
		}
		documented[m[1]] = m[2]
	}
	if len(documented) == 0 {
		t.Fatal("在 proto/README.md 找不到 reason 表格 —— 對外契約不能只活在程式碼裡")
	}

	for _, m := range errorCodes {
		code, ok := documented[m.reason]
		if !ok {
			t.Errorf("reason %q 沒有寫進 proto/README.md", m.reason)
			continue
		}
		if want := m.code.String(); code != want {
			t.Errorf("reason %q 的 code:文件寫 %q,實際是 %q", m.reason, code, want)
		}
		delete(documented, m.reason)
	}
	for reason := range documented {
		t.Errorf("proto/README.md 有 %q,但 errorCodes 裡沒有這個 reason", reason)
	}
}
