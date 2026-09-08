package transport

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

type shopHandler struct {
	svc       shop.Service
	catalog   Catalog
	directory Directory
}

// ListItems 是公開型錄,不需要登入(publicProcedures 有登記)。
func (h shopHandler) ListItems(
	ctx context.Context, req *connect.Request[platformv1.ListItemsRequest],
) (*connect.Response[platformv1.ListItemsResponse], error) {
	if h.catalog == nil {
		return nil, unimplemented("ShopService.ListItems")
	}
	views, err := h.catalog.Items(ctx, req.Msg.GetIncludeDelisted())
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*platformv1.ShopItem, 0, len(views))
	for _, v := range views {
		out = append(out, itemToProto(v))
	}
	return connect.NewResponse(&platformv1.ListItemsResponse{Items: out}), nil
}

// Purchase 購買。idempotency_key 必填——購買沒有天然 UNIQUE 可擋,
// 冪等鍵是防連點的權威(shop.PurchaseParams 的註解同此)。
func (h shopHandler) Purchase(
	ctx context.Context, req *connect.Request[platformv1.PurchaseRequest],
) (*connect.Response[platformv1.PurchaseResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("ShopService.Purchase")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	itemID := strings.TrimSpace(req.Msg.GetItemPublicId())
	if itemID == "" {
		return nil, invalidArgument("item_public_id 必填")
	}
	key := strings.TrimSpace(req.Msg.GetIdempotencyKey())
	if key == "" {
		return nil, invalidArgument("idempotency_key 必填(動錢的 RPC 一律要冪等鍵)")
	}
	res, err := h.svc.Purchase(ctx, shop.PurchaseParams{
		UserID: userID, ItemPublicID: itemID, IdempotencyKey: key,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// 刻意不回 LedgerEntryID / EntitlementID:那是內部 BIGINT id(專案鐵則 5)。
	return connect.NewResponse(&platformv1.PurchaseResponse{
		ItemPublicId:        res.ItemPublicID,
		Fulfillment:         fulfillmentToProto(res.Fulfillment),
		Currency:            res.Currency,
		Price:               res.Price,
		EntitlementPublicId: res.EntitlementPublicID,
		RedemptionPublicId:  res.RedemptionPublicID,
		ExpiresAt:           optTS(res.ExpiresAt),
		RefundableUntil:     optTS(res.RefundableUntil),
		Replayed:            res.Replayed,
	}), nil
}

// RefundPurchase 短窗口自助退款。權益以 public_id 定址,在這裡解成內部 id。
func (h shopHandler) RefundPurchase(
	ctx context.Context, req *connect.Request[platformv1.RefundPurchaseRequest],
) (*connect.Response[platformv1.RefundPurchaseResponse], error) {
	if h.svc == nil || h.directory == nil {
		return nil, unimplemented("ShopService.RefundPurchase")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	publicID := strings.TrimSpace(req.Msg.GetEntitlementPublicId())
	if publicID == "" {
		return nil, invalidArgument("entitlement_public_id 必填")
	}
	key := strings.TrimSpace(req.Msg.GetIdempotencyKey())
	if key == "" {
		return nil, invalidArgument("idempotency_key 必填(動錢的 RPC 一律要冪等鍵)")
	}
	entitlementID, err := h.directory.EntitlementID(ctx, publicID)
	if err != nil {
		return nil, toConnectError(err)
	}
	res, err := h.svc.RefundPurchase(ctx, shop.RefundParams{
		EntitlementID: entitlementID, ByUserID: userID, IdempotencyKey: key,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.RefundPurchaseResponse{
		EntitlementPublicId: publicID,
		Amount:              res.Amount,
		Currency:            res.Currency,
		Replayed:            res.Replayed,
	}), nil
}

// CancelRedemption 取消自己的 pending 工單。刻意沒有冪等鍵:
// pending → 終態的單向轉移在列鎖下本身就是防重權威。
func (h shopHandler) CancelRedemption(
	ctx context.Context, req *connect.Request[platformv1.CancelRedemptionRequest],
) (*connect.Response[platformv1.CancelRedemptionResponse], error) {
	if h.svc == nil || h.directory == nil {
		return nil, unimplemented("ShopService.CancelRedemption")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	publicID := strings.TrimSpace(req.Msg.GetRedemptionPublicId())
	if publicID == "" {
		return nil, invalidArgument("redemption_public_id 必填")
	}
	redemptionID, err := h.directory.RedemptionID(ctx, publicID)
	if err != nil {
		return nil, toConnectError(err)
	}
	res, err := h.svc.CancelRedemption(ctx, shop.CancelParams{
		RedemptionID: redemptionID, ByUserID: userID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.CancelRedemptionResponse{
		RedemptionPublicId: res.RedemptionPublicID,
		Status:             redemptionStatusToProto(res.Status),
		RefundAmount:       res.RefundAmount,
	}), nil
}
