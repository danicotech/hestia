package transport

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
)

// meHandler 一律以「Authorization 認出來的使用者」為主體,
// 請求裡沒有 user_public_id —— 代查別人是管理端的事,不從這裡開後門。
type meHandler struct {
	profiles ProfileReader
	writer   ProfileWriter
	privacy  PrivacyStore
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

// GetPrivacy 讀兩級退出設定。
//
// 沒有設定過的使用者回**預設值**(兩者皆 false)而不是 NotFound:
// 第一次執行 /privacy 的人必然沒有設定列,回錯的話這個指令對新使用者
// 永遠是壞的。「沒有列 = 預設值」的權威在 readpg,入口層不重複判斷。
func (h meHandler) GetPrivacy(
	ctx context.Context, _ *connect.Request[platformv1.GetPrivacyRequest],
) (*connect.Response[platformv1.GetPrivacyResponse], error) {
	if h.privacy == nil {
		return nil, unimplemented("MeService.GetPrivacy")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.privacy.Privacy(ctx, userID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.GetPrivacyResponse{Settings: privacyToProto(v)}), nil
}

// UpdatePrivacy 更新兩級退出設定。兩個布林**各自可設**:
// 沒帶的那個維持原值,不是重設為 false(proto3 的 optional 就是為此)。
//
// 兩個都沒帶 = InvalidArgument。寫入 RPC 靜靜地什麼都不做比報錯難查得多
// ——呼叫端會以為設定成功了,而使用者以為自己已經退出。
func (h meHandler) UpdatePrivacy(
	ctx context.Context, req *connect.Request[platformv1.UpdatePrivacyRequest],
) (*connect.Response[platformv1.UpdatePrivacyResponse], error) {
	if h.privacy == nil {
		return nil, unimplemented("MeService.UpdatePrivacy")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.OptOutLogging == nil && m.OptOutAiCorpus == nil {
		return nil, invalidArgument("至少要指定 opt_out_logging 或 opt_out_ai_corpus 其中一項")
	}
	v, err := h.privacy.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{
		OptOutLogging:  m.OptOutLogging,
		OptOutAICorpus: m.OptOutAiCorpus,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.UpdatePrivacyResponse{Settings: privacyToProto(v)}), nil
}

// GetSummary 是 /profile 的資料來源:一次回檔案、餘額、等級、寵物、徽章。
func (h meHandler) GetSummary(
	ctx context.Context, _ *connect.Request[platformv1.GetSummaryRequest],
) (*connect.Response[platformv1.GetSummaryResponse], error) {
	if h.profiles == nil {
		return nil, unimplemented("MeService.GetSummary")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	sum, err := h.profiles.Summary(ctx, userID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(summaryToProto(sum)), nil
}

// GetLeaderboard 回某社群的 XP 排行。
//
// 不要求呼叫者屬於該社群:排行榜本來就是公開的,而且 Discord 上看得到
// 這個指令的人本來就在那個伺服器裡。
func (h meHandler) GetLeaderboard(
	ctx context.Context, req *connect.Request[platformv1.GetLeaderboardRequest],
) (*connect.Response[platformv1.GetLeaderboardResponse], error) {
	if h.profiles == nil {
		return nil, unimplemented("MeService.GetLeaderboard")
	}
	if _, err := requireUser(ctx); err != nil {
		return nil, err
	}
	entries, err := h.profiles.Leaderboard(ctx,
		req.Msg.GetCommunityPublicId(), req.Msg.GetLimit())
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(leaderboardToProto(entries)), nil
}
