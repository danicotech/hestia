package transport

import (
	"encoding/json"
	"errors"
	"unicode/utf8"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
)

// event_logs 的 request / response 白名單摘要(schemas/13)。
//
// **預設拒絕**:這裡是 type switch,沒有列到的訊息型別回 nil,對應欄位寫 NULL。
// 所以「新增一個 RPC 忘了加摘要」的後果是少記東西,不是把 token 寫進資料庫。
//
// 三條收錄準則:
//   - 只放路由層級資訊與業務 public_id、金額、旗標;
//   - 憑證類(access/refresh token、OAuth code、state)一律不記——AuthService
//     的所有訊息在這裡刻意完全缺席;
//   - 自由文字(admin reason、工單 note)不記:它們的權威位置是
//     admin_audit_logs / redemptions,重複存一份就是第二個權威(專案第 9 條)。

func requestSummary(_ string, msg any) map[string]any {
	switch m := msg.(type) {
	// ── 簽到 ──
	case *platformv1.ClaimRequest:
		return map[string]any{}

	// ── 我的 ──
	case *platformv1.GetProfileRequest:
		return map[string]any{}
	case *platformv1.GetBalanceRequest:
		return map[string]any{"currency": m.GetCurrency()}
	case *platformv1.ListBalancesRequest:
		return map[string]any{}
	case *platformv1.ListEntitlementsRequest:
		return map[string]any{"include_revoked": m.GetIncludeRevoked()}
	case *platformv1.ListRedemptionsRequest:
		return map[string]any{"status": string(redemptionStatusFromProto(m.GetStatus()))}
	case *platformv1.UpdateTimezoneRequest:
		// timezone 是使用者自由輸入的字串(權威在 users.timezone),
		// 比照 admin reason / 工單 note 不記:入口層紀錄不該收任何一筆
		// 使用者可自由控制內容的欄位,那是灌爆稽核表最省力的入口。
		return map[string]any{}

	// ── 商店 ──
	case *platformv1.ListItemsRequest:
		return map[string]any{"include_delisted": m.GetIncludeDelisted()}
	case *platformv1.PurchaseRequest:
		return map[string]any{
			"item_public_id":  m.GetItemPublicId(),
			"idempotency_key": m.GetIdempotencyKey(),
		}
	case *platformv1.RefundPurchaseRequest:
		return map[string]any{
			"entitlement_public_id": m.GetEntitlementPublicId(),
			"idempotency_key":       m.GetIdempotencyKey(),
		}
	case *platformv1.CancelRedemptionRequest:
		return map[string]any{"redemption_public_id": m.GetRedemptionPublicId()}

	// ── 管理員經濟操作(reason / note 刻意不記,權威在 admin_audit_logs)──
	case *platformv1.GrantRequest:
		return map[string]any{
			"user_public_id":  m.GetUserPublicId(),
			"currency":        m.GetCurrency(),
			"amount":          m.GetAmount(),
			"idempotency_key": m.GetIdempotencyKey(),
		}
	case *platformv1.DeductRequest:
		return map[string]any{
			"user_public_id":  m.GetUserPublicId(),
			"currency":        m.GetCurrency(),
			"amount":          m.GetAmount(),
			"idempotency_key": m.GetIdempotencyKey(),
		}
	case *platformv1.RefundRequest:
		return map[string]any{"entry_ref": m.GetEntryRef()}
	case *platformv1.ListEntriesRequest:
		return map[string]any{
			"user_public_id": m.GetUserPublicId(),
			"page_size":      m.GetPageSize(),
		}
	case *platformv1.HandleRedemptionRequest:
		return map[string]any{
			"redemption_public_id": m.GetRedemptionPublicId(),
			"approve":              m.GetApprove(),
		}

	// AuthService 的請求(code / state / refresh_token)全是憑證,一律不記。
	default:
		return nil
	}
}

func responseSummary(_ string, msg any) map[string]any {
	switch m := msg.(type) {
	// ── 簽到 ──
	case *platformv1.ClaimResponse:
		return map[string]any{"amount": m.GetAmount(), "streak": m.GetStreak()}

	// ── 我的(列表只記筆數,內容本身不是入口層的事)──
	case *platformv1.GetProfileResponse:
		return map[string]any{"user_public_id": m.GetProfile().GetPublicId()}
	case *platformv1.GetBalanceResponse:
		return map[string]any{
			"currency": m.GetBalance().GetCurrency(),
			"amount":   m.GetBalance().GetAmount(),
		}
	case *platformv1.ListBalancesResponse:
		return map[string]any{"count": len(m.GetBalances())}
	case *platformv1.ListEntitlementsResponse:
		return map[string]any{"count": len(m.GetEntitlements())}
	case *platformv1.ListRedemptionsResponse:
		return map[string]any{"count": len(m.GetRedemptions())}
	case *platformv1.UpdateTimezoneResponse:
		return map[string]any{
			"user_public_id": m.GetProfile().GetPublicId(),
			"timezone":       m.GetProfile().GetTimezone(),
		}

	// ── 商店 ──
	case *platformv1.ListItemsResponse:
		return map[string]any{"count": len(m.GetItems())}
	case *platformv1.PurchaseResponse:
		return map[string]any{
			"item_public_id":        m.GetItemPublicId(),
			"entitlement_public_id": m.GetEntitlementPublicId(),
			"redemption_public_id":  m.GetRedemptionPublicId(),
			"currency":              m.GetCurrency(),
			"price":                 m.GetPrice(),
			"replayed":              m.GetReplayed(),
		}
	case *platformv1.RefundPurchaseResponse:
		return map[string]any{
			"entitlement_public_id": m.GetEntitlementPublicId(),
			"currency":              m.GetCurrency(),
			"amount":                m.GetAmount(),
			"replayed":              m.GetReplayed(),
		}
	case *platformv1.CancelRedemptionResponse:
		return map[string]any{
			"redemption_public_id": m.GetRedemptionPublicId(),
			"status":               string(redemptionStatusFromProto(m.GetStatus())),
			"refund_amount":        m.GetRefundAmount(),
		}

	// ── 管理員經濟操作 ──
	case *platformv1.GrantResponse:
		return adjustmentSummary(m.GetAdjustment())
	case *platformv1.DeductResponse:
		return adjustmentSummary(m.GetAdjustment())
	case *platformv1.RefundResponse:
		return adjustmentSummary(m.GetAdjustment())
	case *platformv1.ListEntriesResponse:
		return map[string]any{"count": len(m.GetEntries())}
	case *platformv1.HandleRedemptionResponse:
		return map[string]any{
			"redemption_public_id": m.GetRedemptionPublicId(),
			"status":               string(redemptionStatusFromProto(m.GetStatus())),
			"refund_amount":        m.GetRefundAmount(),
		}

	// AuthService 的回應含 access/refresh token,一律不記。
	default:
		return nil
	}
}

func adjustmentSummary(a *platformv1.LedgerAdjustment) map[string]any {
	return map[string]any{
		"user_public_id": a.GetUserPublicId(),
		"currency":       a.GetCurrency(),
		"amount":         a.GetAmount(),
		"new_balance":    a.GetNewBalance(),
		"replayed":       a.GetReplayed(),
	}
}

// errorSummary 記錯誤細節。兩道門檻:
//   - 只有**本套件產生**的錯誤(ownError 標記)訊息才入庫。port 實作回的
//     connect.Error 訊息不可信,可能含 SQL、內部 id、他人資料。
//   - Internal 的訊息一律不入庫(那是我們自己的固定文案,也沒有價值)。
func errorSummary(err error) map[string]any {
	code := connect.CodeOf(err)
	out := map[string]any{"code": code.String()}
	var ce *connect.Error
	if trusted(err) && code != connect.CodeInternal && errors.As(err, &ce) {
		out["message"] = ce.Message()
	}
	return out
}

// summaryMaxStringBytes 是摘要裡單一字串值的上限。
//
// 為什麼一定要有:摘要在 handler 之前就算好,所以**即使 RPC 最後回
// unimplemented / invalid_argument,那一列還是會落地**。任何 client 可控的
// 字串(idempotency_key、public_id、page_token)沒有上限,就等於任一有效
// session 都能拿 event_logs 灌爆平台唯一的資料庫——而且保留 180 天。
// 256 bytes 足以裝下 ULID(26)與正常的冪等鍵,異常長度本來就只有惡意用途。
const summaryMaxStringBytes = 256

func marshalSummary(m map[string]any) json.RawMessage {
	if m == nil {
		return nil
	}
	truncated := false
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if cut, did := truncateUTF8(s, summaryMaxStringBytes); did {
			m[k] = cut
			truncated = true
		}
	}
	if truncated {
		// 標記出來,查稽核的人才知道看到的不是完整值。
		m["truncated"] = true
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// truncateUTF8 在不切壞 rune 的前提下把字串截到 max bytes。
func truncateUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
