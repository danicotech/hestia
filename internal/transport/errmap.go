package transport

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// errorCodes 是 core sentinel error → connect code 的**唯一**映射表。
//
// 集中一處的理由:同一個領域錯誤在不同 handler 回不同 code,對 client 是災難
// (重試策略全錯)。要加新錯誤就加在這裡,不要在 handler 裡臨時判斷。
//
// code 選擇原則:
//   - InvalidArgument:請求本身不合法,重送同樣的內容一定還是失敗。
//   - FailedPrecondition:請求合法,但系統當前狀態不允許(餘額、窗口、狀態機)。
//     刻意不用 ResourceExhausted——它在 connect 對應 HTTP 429,client 會當成
//     限流而重試,但「餘額不足 / 已達限購」重試永遠不會成功。
//   - AlreadyExists:這件事已經發生過了(今天簽過、這筆已退過)。
//   - Aborted:併發衝突,client 應該重新讀狀態後再決定。
//   - PermissionDenied:身分有效但這東西不是你的。
//   - Internal:兩個權威互相矛盾等異常狀態——必須失敗出聲,不能靜靜回成功。
var errorCodes = []struct {
	err  error
	code connect.Code
}{
	// ── 帳本(ledger)──────────────────────────────────────────────
	{ledger.ErrInsufficientBalance, connect.CodeFailedPrecondition},
	{ledger.ErrIdempotencyConflict, connect.CodeAborted},
	// 同一把冪等鍵正在處理中:稍後重試會成功(拿到原本的結果),
	// 用 Unavailable 讓 client 的標準重試策略自然接手。
	{ledger.ErrInFlight, connect.CodeUnavailable},
	{ledger.ErrInvalidOp, connect.CodeInvalidArgument},

	// ── 簽到(daily)──────────────────────────────────────────────
	{daily.ErrAlreadyClaimed, connect.CodeAlreadyExists},
	{daily.ErrTimezoneChangeCooldown, connect.CodeFailedPrecondition},
	{daily.ErrLedgerStateConflict, connect.CodeInternal},
	{daily.ErrInvalidConfig, connect.CodeInternal},

	// ── 商店(shop)──────────────────────────────────────────────
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

	// ── 管理員經濟操作(adminecon)──────────────────────────────
	{adminecon.ErrReasonRequired, connect.CodeInvalidArgument},
	{adminecon.ErrInvalidInput, connect.CodeInvalidArgument},
	{adminecon.ErrEntryNotFound, connect.CodeNotFound},
	{adminecon.ErrNotRefundable, connect.CodeFailedPrecondition},
	{adminecon.ErrAlreadyRefunded, connect.CodeAlreadyExists},

	// ── 身分 / session(identity)────────────────────────────────
	// 憑證類錯誤一律 Unauthenticated:對 client 的意思都是「重新登入」。
	{identity.ErrInvalidToken, connect.CodeUnauthenticated},
	{identity.ErrTokenExpired, connect.CodeUnauthenticated},
	{identity.ErrSessionNotFound, connect.CodeUnauthenticated},
	{identity.ErrSessionRevoked, connect.CodeUnauthenticated},
	{identity.ErrSessionExpired, connect.CodeUnauthenticated},
	{identity.ErrTokenReuseDetected, connect.CodeUnauthenticated},
	// state 相關:是「這次回呼的參數有問題」,不是「你沒登入」。
	{identity.ErrInvalidState, connect.CodeInvalidArgument},
	{identity.ErrStateExpired, connect.CodeInvalidArgument},
	// ErrStateMismatch 刻意用 PermissionDenied 而不是 Unauthenticated:
	// 它的意思是「這次回呼不屬於這個瀏覽器」——那是 login CSRF 的訊號,
	// 不是「請你登入」。用 denied 也讓它在 event_logs 裡一眼可見。
	{identity.ErrStateMismatch, connect.CodePermissionDenied},
	{identity.ErrInvalidRedirect, connect.CodeInvalidArgument},
	// 外部 provider 掛了/超時:稍後重試有意義。
	{identity.ErrProviderExchange, connect.CodeUnavailable},
	{identity.ErrAccountDeleted, connect.CodePermissionDenied},
	{identity.ErrInvalidConfig, connect.CodeInternal},

	// ── 入口層自己的 port 錯誤 ──────────────────────────────────
	{ErrNotFound, connect.CodeNotFound},
	{ErrUnauthenticated, connect.CodeUnauthenticated},
	{ErrPermissionDenied, connect.CodePermissionDenied},

	// ── context ────────────────────────────────────────────────────
	{context.Canceled, connect.CodeCanceled},
	{context.DeadlineExceeded, connect.CodeDeadlineExceeded},
}

var (
	// errInternal 是未知錯誤對外的固定訊息。
	// 未映射的錯誤可能包含 SQL、連線字串、內部 id——一律不外洩,原文只進 log。
	errInternal = errors.New("內部錯誤")
	// errUntrusted 給「別人做的 connect.Error」用:code 保留(client 的重試
	// 策略需要),訊息換掉。
	errUntrusted = errors.New("請求無法完成")
)

// ownError 標記「這個 connect.Error 是本套件產生的,訊息可信」。
//
// 為什麼需要標記:port 的實作不在本套件控制之下,它若回一個
// *connect.Error,原本會被原樣放行,訊息直接進回應與 event_logs——
// 那是一條沒有防線的洩漏通道(SQL、內部 id、他人資料都可能在裡面)。
// 有了標記,只有我們自己寫的文案能出去。
type ownError struct{ err *connect.Error }

func (e ownError) Error() string { return e.err.Error() }
func (e ownError) Unwrap() error { return e.err }

// connectError 產生「可信」的 connect 錯誤。本套件一律用它,不要直接
// connect.NewError——直接建的沒有標記,訊息會在稽核時被當成外部來源丟掉。
func connectError(code connect.Code, err error) error {
	return ownError{connect.NewError(code, err)}
}

// trusted 回報錯誤是否由本套件產生。
func trusted(err error) bool {
	var own ownError
	return errors.As(err, &own)
}

// toConnectError 把任何錯誤轉成帶正確 code 的連線層錯誤。
//
//	本套件產生的          → 原樣放行(訊息是寫給使用者看的)
//	認得的 core sentinel  → 表上的 code + sentinel 自己的訊息
//	別人做的 connect.Error → 保留 code,訊息換成安全文案
//	其他                  → Internal + 固定訊息,原文只進 log
func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	if trusted(err) {
		return err
	}
	for _, m := range errorCodes {
		if errors.Is(err, m.err) {
			return connectError(m.code, m.err)
		}
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return connectError(ce.Code(), errUntrusted)
	}
	return connectError(connect.CodeInternal, errInternal)
}

// invalidArgument 是 handler 做參數檢查時的統一產生器
// (proto 沒有 required,必填欄位只能在這裡擋)。
func invalidArgument(msg string) error {
	return connectError(connect.CodeInvalidArgument, errors.New(msg))
}

// unimplemented 給尚未注入實作的服務用。回 Unimplemented 而不是 Internal,
// client 才知道「這個功能還沒上線」而不是「伺服器壞了」。
func unimplemented(name string) error {
	return connectError(connect.CodeUnimplemented, errors.New(name+" 尚未提供"))
}
