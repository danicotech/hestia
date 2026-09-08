package transport

import (
	"context"
	"time"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	renderv1 "github.com/danicotech/hestia/gen/hestia/render/v1"
)

// notificationHandler 是 outbox → Discord 閘道的取貨口。
//
// **只接受服務身分,而且不帶 X-Acting-User。** 它是本服務目前唯一的
// 「無使用者主體」RPC:拉一批公告不是代表任何人做的事
// (見 interceptors.go 的 subjectlessServices)。
//
// 入口層在這裡刻意極薄:白名單 topic、可見性夾限、渲染、payload 過濾
// 全部在 core/storage 那側。handler 碰不到 outbox 的 payload,
// 所以「不小心把內部欄位傳出去」在這一層不可能發生。
type notificationHandler struct {
	src Announcements
}

// PullAnnouncements 認領一批待送的公告。
func (h notificationHandler) PullAnnouncements(
	ctx context.Context, req *connect.Request[platformv1.PullAnnouncementsRequest],
) (*connect.Response[platformv1.PullAnnouncementsResponse], error) {
	if h.src == nil {
		return nil, unimplemented("NotificationService")
	}
	if err := requireService(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	// max / visibility 不在這裡驗範圍:夾限的權威在實作(它才知道自己的
	// 批次上限與逾時上限)。入口層再夾一次就是同一個決定兩個位置。
	items, err := h.src.Pull(ctx, m.GetMax(), time.Duration(m.GetVisibilitySeconds())*time.Second)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*renderv1.Announcement, 0, len(items))
	for _, a := range items {
		out = append(out, announcementToProto(a))
	}
	return connect.NewResponse(&platformv1.PullAnnouncementsResponse{Announcements: out}), nil
}

// AckAnnouncements 確認送達。
//
// 空清單是合法的 no-op(回 0),不是錯誤:呼叫端整批都沒貼成功時
// 送一個空的 Ack 比「有條件地不呼叫」好寫,也不該因此拿到錯誤。
func (h notificationHandler) AckAnnouncements(
	ctx context.Context, req *connect.Request[platformv1.AckAnnouncementsRequest],
) (*connect.Response[platformv1.AckAnnouncementsResponse], error) {
	if h.src == nil {
		return nil, unimplemented("NotificationService")
	}
	if err := requireService(ctx); err != nil {
		return nil, err
	}
	n, err := h.src.Ack(ctx, req.Msg.GetEventIds())
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.AckAnnouncementsResponse{
		Acknowledged: int32(n),
	}), nil
}
