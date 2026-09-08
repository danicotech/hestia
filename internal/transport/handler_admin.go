package transport

import (
	"context"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// adminEconomyHandler 是管理端。四個 procedure 都列在 privilegedProcedures,
// 沒有注入 Authorizer 時一律 PermissionDenied(fail closed)。
type adminEconomyHandler struct {
	svc       adminecon.Service
	shop      shop.Service
	directory Directory
	entries   LedgerReader
}

// 分頁參數。上限存在的理由是有人一定會送 page_size=1000000:
// 一次撈爆分區表的查詢會拖垮整個 DB,不是只有那個請求變慢。
const (
	defaultEntriesPageSize int32 = 50
	maxEntriesPageSize     int32 = 200
)

// ListEntries 列出某位使用者的帳本分錄。
//
// 為什麼這條 RPC 必須存在:Refund 用 entry_ref 定址,但在它之前沒有任何 RPC
// 產出 entry_ref——管理員只能直接讀資料庫才叫得動退款,契約自相矛盾。
func (h adminEconomyHandler) ListEntries(
	ctx context.Context, req *connect.Request[platformv1.ListEntriesRequest],
) (*connect.Response[platformv1.ListEntriesResponse], error) {
	if h.entries == nil || h.directory == nil {
		return nil, unimplemented("AdminEconomyService.ListEntries")
	}
	if _, err := requireUser(ctx); err != nil {
		return nil, err
	}
	publicID := strings.TrimSpace(req.Msg.GetUserPublicId())
	if publicID == "" {
		return nil, invalidArgument("user_public_id 必填")
	}
	userID, err := h.directory.UserID(ctx, publicID)
	if err != nil {
		return nil, toConnectError(err)
	}
	size := req.Msg.GetPageSize()
	switch {
	case size <= 0:
		size = defaultEntriesPageSize
	case size > maxEntriesPageSize:
		size = maxEntriesPageSize
	}
	page, err := h.entries.ListEntries(ctx, readmodel.LedgerEntriesQuery{
		UserID:    userID,
		After:     optTime(req.Msg.GetAfter()),
		Before:    optTime(req.Msg.GetBefore()),
		PageSize:  size,
		PageToken: req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*platformv1.LedgerEntry, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, ledgerEntryToProto(e))
	}
	return connect.NewResponse(&platformv1.ListEntriesResponse{
		Entries: out, NextPageToken: page.NextPageToken,
	}), nil
}

func (h adminEconomyHandler) Grant(
	ctx context.Context, req *connect.Request[platformv1.GrantRequest],
) (*connect.Response[platformv1.GrantResponse], error) {
	res, publicID, err := h.adjust(ctx, adjustInput{
		userPublicID: req.Msg.GetUserPublicId(),
		currency:     req.Msg.GetCurrency(),
		amount:       req.Msg.GetAmount(),
		reason:       req.Msg.GetReason(),
		key:          req.Msg.GetIdempotencyKey(),
	}, func(s adminecon.Service, p adminecon.AdjustParams) (*adminecon.Result, error) {
		return s.Grant(ctx, p)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&platformv1.GrantResponse{
		Adjustment: adjustmentToProto(res, publicID),
	}), nil
}

func (h adminEconomyHandler) Deduct(
	ctx context.Context, req *connect.Request[platformv1.DeductRequest],
) (*connect.Response[platformv1.DeductResponse], error) {
	res, publicID, err := h.adjust(ctx, adjustInput{
		userPublicID: req.Msg.GetUserPublicId(),
		currency:     req.Msg.GetCurrency(),
		amount:       req.Msg.GetAmount(),
		reason:       req.Msg.GetReason(),
		key:          req.Msg.GetIdempotencyKey(),
	}, func(s adminecon.Service, p adminecon.AdjustParams) (*adminecon.Result, error) {
		return s.Deduct(ctx, p)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&platformv1.DeductResponse{
		Adjustment: adjustmentToProto(res, publicID),
	}), nil
}

// Refund 萬能退款。刻意沒有 idempotency_key:權威是由分錄導出的決定性鍵
// refund:entry:{id},呼叫端再給一把就是第二套權威。
func (h adminEconomyHandler) Refund(
	ctx context.Context, req *connect.Request[platformv1.RefundRequest],
) (*connect.Response[platformv1.RefundResponse], error) {
	if h.svc == nil || h.directory == nil {
		return nil, unimplemented("AdminEconomyService.Refund")
	}
	actorID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.Msg.GetReason())
	if reason == "" {
		return nil, invalidArgument("reason 必填(管理動作一律要留理由)")
	}
	entryID, err := parseEntryRef(req.Msg.GetEntryRef())
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Refund(ctx, adminecon.RefundParams{
		ActorUserID: actorID, EntryID: entryID, Reason: reason,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	userPublicID, err := h.directory.UserPublicID(ctx, res.UserID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.RefundResponse{
		Adjustment: adjustmentToProto(res, userPublicID),
	}), nil
}

// HandleRedemption 處理 pending 工單。approve=true → fulfilled(不動錢,
// 購買時的押款即最終扣款);false → rejected(全額退)。
// 沒有冪等鍵:單向狀態機就是防重權威,重複處理回 FailedPrecondition。
func (h adminEconomyHandler) HandleRedemption(
	ctx context.Context, req *connect.Request[platformv1.HandleRedemptionRequest],
) (*connect.Response[platformv1.HandleRedemptionResponse], error) {
	if h.shop == nil || h.directory == nil {
		return nil, unimplemented("AdminEconomyService.HandleRedemption")
	}
	actorID, err := requireUser(ctx)
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
	var note *string
	if req.Msg.Note != nil {
		n := strings.TrimSpace(req.Msg.GetNote())
		note = &n
	}
	res, err := h.shop.HandleRedemption(ctx, shop.HandleParams{
		RedemptionID: redemptionID, HandlerUserID: actorID,
		Approve: req.Msg.GetApprove(), Note: note,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.HandleRedemptionResponse{
		RedemptionPublicId: res.RedemptionPublicID,
		Status:             redemptionStatusToProto(res.Status),
		RefundAmount:       res.RefundAmount,
	}), nil
}

type adjustInput struct {
	userPublicID string
	currency     string
	amount       int64
	reason       string
	key          string
}

// adjust 是 Grant / Deduct 的共同前置:驗參數、解 public_id、呼叫 core。
// 金額方向由呼叫的方法決定(core 收的 Amount 一律為正),這裡不做正負號魔術。
func (h adminEconomyHandler) adjust(
	ctx context.Context, in adjustInput,
	call func(adminecon.Service, adminecon.AdjustParams) (*adminecon.Result, error),
) (*adminecon.Result, string, error) {
	if h.svc == nil || h.directory == nil {
		return nil, "", unimplemented("AdminEconomyService")
	}
	actorID, err := requireUser(ctx)
	if err != nil {
		return nil, "", err
	}
	publicID := strings.TrimSpace(in.userPublicID)
	if publicID == "" {
		return nil, "", invalidArgument("user_public_id 必填")
	}
	if strings.TrimSpace(in.currency) == "" {
		return nil, "", invalidArgument("currency 必填")
	}
	if in.amount <= 0 {
		return nil, "", invalidArgument("amount 必須大於 0")
	}
	if strings.TrimSpace(in.reason) == "" {
		return nil, "", invalidArgument("reason 必填(管理動作一律要留理由)")
	}
	key := strings.TrimSpace(in.key)
	if key == "" {
		return nil, "", invalidArgument("idempotency_key 必填(動錢的 RPC 一律要冪等鍵)")
	}
	targetID, err := h.directory.UserID(ctx, publicID)
	if err != nil {
		return nil, "", toConnectError(err)
	}
	res, err := call(h.svc, adminecon.AdjustParams{
		IdempotencyKey: key,
		ActorUserID:    actorID,
		UserID:         targetID,
		Currency:       strings.TrimSpace(in.currency),
		Amount:         in.amount,
		Reason:         strings.TrimSpace(in.reason),
	})
	if err != nil {
		return nil, "", toConnectError(err)
	}
	return res, publicID, nil
}

func adjustmentToProto(res *adminecon.Result, userPublicID string) *platformv1.LedgerAdjustment {
	return &platformv1.LedgerAdjustment{
		UserPublicId: userPublicID,
		Currency:     res.Currency,
		Amount:       res.Amount,
		NewBalance:   res.NewBalance,
		Replayed:     res.Replayed,
	}
}

// parseEntryRef 解析要沖銷的分錄參照。
//
// ⚠ 已知 schema 缺口:platform.token_entries 沒有 public_id 欄位,
// 而專案鐵則 5 要求對外只出現 public_id。契約用 string 承載是為了等
// token_entries 補上 public_id 之後,只換這個函式的內容、不動 proto 欄位型別,
// 既有 client 不會壞。在那之前值是分錄 id 的十進位字串。
func parseEntryRef(ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, invalidArgument("entry_ref 必填")
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return 0, invalidArgument("entry_ref 格式不正確")
	}
	return id, nil
}
