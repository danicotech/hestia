package transport

import (
	"context"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/daily"
)

type dailyHandler struct {
	svc daily.Service
}

// Claim 簽到。刻意沒有冪等鍵參數:防連點的權威是 daily_claims 的
// UNIQUE (user_id, claim_date)(帳本鐵則 4),重複簽到由 DB 撞鍵擋下,
// 對外表現為 AlreadyExists。
func (h dailyHandler) Claim(
	ctx context.Context, _ *connect.Request[platformv1.ClaimRequest],
) (*connect.Response[platformv1.ClaimResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("DailyService")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Claim(ctx, userID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.ClaimResponse{
		Amount: res.Amount,
		Streak: res.Streak,
	}), nil
}
