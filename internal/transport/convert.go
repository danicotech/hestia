package transport

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// 領域型別 ↔ proto 的轉換全部集中在這裡。handler 只做「取參數、呼叫 core、包回應」,
// 不散落轉換邏輯——欄位語意改動時只有一個地方要改。

func tsPB(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// optTS 對應 proto 的 optional Timestamp:nil = 該事實不存在(永久 / 未到期 / 不可退)。
func optTS(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return tsPB(*t)
}

func fulfillmentToProto(f shop.Fulfillment) platformv1.Fulfillment {
	switch f {
	case shop.FulfillAutoRole:
		return platformv1.Fulfillment_FULFILLMENT_AUTO_ROLE
	case shop.FulfillAutoPerk:
		return platformv1.Fulfillment_FULFILLMENT_AUTO_PERK
	case shop.FulfillManual:
		return platformv1.Fulfillment_FULFILLMENT_MANUAL
	default:
		return platformv1.Fulfillment_FULFILLMENT_UNSPECIFIED
	}
}

func redemptionStatusToProto(s shop.RedemptionStatus) platformv1.RedemptionStatus {
	switch s {
	case shop.RedemptionPending:
		return platformv1.RedemptionStatus_REDEMPTION_STATUS_PENDING
	case shop.RedemptionFulfilled:
		return platformv1.RedemptionStatus_REDEMPTION_STATUS_FULFILLED
	case shop.RedemptionRejected:
		return platformv1.RedemptionStatus_REDEMPTION_STATUS_REJECTED
	case shop.RedemptionCancelled:
		return platformv1.RedemptionStatus_REDEMPTION_STATUS_CANCELLED
	default:
		return platformv1.RedemptionStatus_REDEMPTION_STATUS_UNSPECIFIED
	}
}

// redemptionStatusFromProto 用於列表過濾;UNSPECIFIED = 不過濾(空字串)。
func redemptionStatusFromProto(s platformv1.RedemptionStatus) shop.RedemptionStatus {
	switch s {
	case platformv1.RedemptionStatus_REDEMPTION_STATUS_PENDING:
		return shop.RedemptionPending
	case platformv1.RedemptionStatus_REDEMPTION_STATUS_FULFILLED:
		return shop.RedemptionFulfilled
	case platformv1.RedemptionStatus_REDEMPTION_STATUS_REJECTED:
		return shop.RedemptionRejected
	case platformv1.RedemptionStatus_REDEMPTION_STATUS_CANCELLED:
		return shop.RedemptionCancelled
	default:
		return ""
	}
}

func profileToProto(p *readmodel.ProfileView) *platformv1.UserProfile {
	if p == nil {
		return nil
	}
	return &platformv1.UserProfile{
		PublicId:          p.PublicID,
		DisplayName:       p.DisplayName,
		AvatarUrl:         p.AvatarURL,
		Timezone:          p.Timezone,
		TimezoneChangedAt: optTS(p.TimezoneChangedAt),
		CreatedAt:         tsPB(p.CreatedAt),
	}
}

func itemToProto(i readmodel.ItemView) *platformv1.ShopItem {
	return &platformv1.ShopItem{
		PublicId:            i.PublicID,
		Name:                i.Name,
		Description:         i.Description,
		Fulfillment:         fulfillmentToProto(i.Fulfillment),
		Currency:            i.Currency,
		Price:               i.Price,
		DurationDays:        i.DurationDays,
		PerUserLimit:        i.PerUserLimit,
		RefundWindowSeconds: i.RefundWindowSeconds,
		ListedAt:            optTS(i.ListedAt),
		DelistedAt:          optTS(i.DelistedAt),
	}
}

func entitlementToProto(e readmodel.EntitlementView) *platformv1.Entitlement {
	return &platformv1.Entitlement{
		PublicId:        e.PublicID,
		ItemPublicId:    e.ItemPublicID,
		ItemName:        e.ItemName,
		Fulfillment:     fulfillmentToProto(e.Fulfillment),
		GrantedAt:       tsPB(e.GrantedAt),
		ExpiresAt:       optTS(e.ExpiresAt),
		RefundableUntil: optTS(e.RefundableUntil),
		RevokedAt:       optTS(e.RevokedAt),
	}
}

func redemptionToProto(r readmodel.RedemptionView) *platformv1.Redemption {
	return &platformv1.Redemption{
		PublicId:     r.PublicID,
		ItemPublicId: r.ItemPublicID,
		ItemName:     r.ItemName,
		Status:       redemptionStatusToProto(r.Status),
		Note:         r.Note,
		CreatedAt:    tsPB(r.CreatedAt),
		HandledAt:    optTS(r.HandledAt),
	}
}

func sessionToProto(s *SessionView) *platformv1.Session {
	if s == nil {
		return nil
	}
	return &platformv1.Session{
		AccessToken:           s.AccessToken,
		AccessTokenExpiresAt:  tsPB(s.AccessTokenExpiresAt),
		RefreshToken:          s.RefreshToken,
		RefreshTokenExpiresAt: tsPB(s.RefreshTokenExpiresAt),
		UserPublicId:          s.UserPublicID,
	}
}

func balanceToProto(b readmodel.BalanceView) *platformv1.Balance {
	return &platformv1.Balance{Currency: b.Currency, Amount: b.Amount}
}

func ledgerEntryToProto(e readmodel.LedgerEntryView) *platformv1.LedgerEntry {
	return &platformv1.LedgerEntry{
		EntryRef:      e.EntryRef,
		UserPublicId:  e.UserPublicID,
		Currency:      e.Currency,
		Amount:        e.Amount,
		Reason:        e.Reason,
		CreatedAt:     tsPB(e.CreatedAt),
		Refunded:      e.Refunded,
		ActorPublicId: e.ActorPublicID,
	}
}

// optTime 是 optTS 的反向:proto 的 optional Timestamp → *time.Time。
func optTime(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
