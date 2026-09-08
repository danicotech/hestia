package transport

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// meHandler 一律以「Authorization 認出來的使用者」為主體,
// 請求裡沒有 user_public_id —— 代查別人是管理端的事,不從這裡開後門。
type meHandler struct {
	profiles ProfileReader
	writer   ProfileWriter
	catalog  Catalog
	ledger   ledger.Ledger
}

func (h meHandler) GetProfile(
	ctx context.Context, _ *connect.Request[platformv1.GetProfileRequest],
) (*connect.Response[platformv1.GetProfileResponse], error) {
	if h.profiles == nil {
		return nil, unimplemented("MeService.GetProfile")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	p, err := h.profiles.Profile(ctx, userID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.GetProfileResponse{Profile: profileToProto(p)}), nil
}

func (h meHandler) GetBalance(
	ctx context.Context, req *connect.Request[platformv1.GetBalanceRequest],
) (*connect.Response[platformv1.GetBalanceResponse], error) {
	if h.ledger == nil {
		return nil, unimplemented("MeService.GetBalance")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	currency := strings.TrimSpace(req.Msg.GetCurrency())
	if currency == "" {
		return nil, invalidArgument("currency 必填")
	}
	amount, err := h.ledger.Balance(ctx, userID, currency)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.GetBalanceResponse{
		Balance: &platformv1.Balance{Currency: currency, Amount: amount},
	}), nil
}

// ListBalances 一次回全部幣別。與 GetBalance 的分工:GetBalance 走帳本介面
// (單一幣別的權威查詢),ListBalances 走讀取側 port(一次撈完 user_balances)。
func (h meHandler) ListBalances(
	ctx context.Context, _ *connect.Request[platformv1.ListBalancesRequest],
) (*connect.Response[platformv1.ListBalancesResponse], error) {
	if h.profiles == nil {
		return nil, unimplemented("MeService.ListBalances")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	views, err := h.profiles.Balances(ctx, userID)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*platformv1.Balance, 0, len(views))
	for _, v := range views {
		out = append(out, balanceToProto(v))
	}
	return connect.NewResponse(&platformv1.ListBalancesResponse{Balances: out}), nil
}

func (h meHandler) ListEntitlements(
	ctx context.Context, req *connect.Request[platformv1.ListEntitlementsRequest],
) (*connect.Response[platformv1.ListEntitlementsResponse], error) {
	if h.catalog == nil {
		return nil, unimplemented("MeService.ListEntitlements")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	views, err := h.catalog.Entitlements(ctx, userID, req.Msg.GetIncludeRevoked())
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*platformv1.Entitlement, 0, len(views))
	for _, v := range views {
		out = append(out, entitlementToProto(v))
	}
	return connect.NewResponse(&platformv1.ListEntitlementsResponse{Entitlements: out}), nil
}

func (h meHandler) ListRedemptions(
	ctx context.Context, req *connect.Request[platformv1.ListRedemptionsRequest],
) (*connect.Response[platformv1.ListRedemptionsResponse], error) {
	if h.catalog == nil {
		return nil, unimplemented("MeService.ListRedemptions")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	views, err := h.catalog.Redemptions(ctx, userID, redemptionStatusFromProto(req.Msg.GetStatus()))
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*platformv1.Redemption, 0, len(views))
	for _, v := range views {
		out = append(out, redemptionToProto(v))
	}
	return connect.NewResponse(&platformv1.ListRedemptionsResponse{Redemptions: out}), nil
}

// UpdateTimezone 只擋空字串;時區字串的語意驗證留給實作(DB 是唯一真實來源),
// 這裡不維護第二份 IANA 清單。
func (h meHandler) UpdateTimezone(
	ctx context.Context, req *connect.Request[platformv1.UpdateTimezoneRequest],
) (*connect.Response[platformv1.UpdateTimezoneResponse], error) {
	if h.writer == nil {
		return nil, unimplemented("MeService.UpdateTimezone")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	tz := strings.TrimSpace(req.Msg.GetTimezone())
	if tz == "" {
		return nil, invalidArgument("timezone 必填")
	}
	p, err := h.writer.SetTimezone(ctx, userID, tz)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.UpdateTimezoneResponse{Profile: profileToProto(p)}), nil
}
