package transport_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/transport"
)

// 通知拉取的入口層測試。要證明的:
//   - **只收服務身分**:使用者 Bearer 一律 PermissionDenied;
//   - 它是唯一**不需要 X-Acting-User** 的服務身分 RPC(佇列不屬於任何人),
//     而 ActivityService 的既有行為完全沒變;
//   - 公告轉成 render 契約時 content 永遠是空的(@everyone 防線);
//   - 參數原樣往下傳,夾限交給實作。

type spyAnnouncements struct {
	mu        sync.Mutex
	items     []notification.Announcement
	lastMax   int32
	lastVis   time.Duration
	lastAcked []string
	ackErr    error
}

func (s *spyAnnouncements) Pull(_ context.Context, max int32, vis time.Duration) ([]notification.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMax, s.lastVis = max, vis
	return s.items, nil
}

func (s *spyAnnouncements) Ack(_ context.Context, eventIDs []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ackErr != nil {
		return 0, s.ackErr
	}
	s.lastAcked = append([]string(nil), eventIDs...)
	return len(eventIDs), nil
}

func (s *spyAnnouncements) seen() (int32, time.Duration, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastMax, s.lastVis, s.lastAcked
}

func newNotificationEnv(t *testing.T, spy *spyAnnouncements) (
	platformv1connect.NotificationServiceClient, platformv1connect.ActivityServiceClient,
) {
	t.Helper()
	srv, _ := newServer(t, transport.Deps{
		Announcements: spy,
		Activity:      &fakeActivity{},
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	return platformv1connect.NewNotificationServiceClient(srv.Client(), srv.URL),
		platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)
}

func pullReq(max, vis int32) *connect.Request[platformv1.PullAnnouncementsRequest] {
	return connect.NewRequest(&platformv1.PullAnnouncementsRequest{
		Max: max, VisibilitySeconds: vis,
	})
}

// 服務憑證、**不帶** X-Acting-User → 成功。這是本服務與 ActivityService
// 唯一的差別,也是它必須存在的理由:公告佇列沒有代表對象。
func TestPullAnnouncementsWithoutActingUser(t *testing.T) {
	spy := &spyAnnouncements{items: []notification.Announcement{{
		EventID: "42", ChannelKey: "daily", Title: "每日簽到",
		Fields: []notification.Field{{K: "成員", V: "小明"}, {K: "獲得", V: "30"}},
	}}}
	notif, _ := newNotificationEnv(t, spy)

	res, err := notif.PullAnnouncements(context.Background(),
		withService(pullReq(10, 30), svcTokenOld, ""))
	if err != nil {
		t.Fatalf("PullAnnouncements 不該需要 X-Acting-User:%v", err)
	}
	got := res.Msg.GetAnnouncements()
	if len(got) != 1 {
		t.Fatalf("拿到 %d 則,想要 1 則", len(got))
	}
	a := got[0]
	if a.GetEventId() != "42" || a.GetChannelKey() != "daily" {
		t.Errorf("event_id / channel_key 沒帶過來:%+v", a)
	}
	if a.GetView() == nil || a.GetView().GetTitle() != "每日簽到" {
		t.Fatalf("view 沒填好:%+v", a.GetView())
	}
	if len(a.GetView().GetFields()) != 2 {
		t.Errorf("欄位數 = %d,想要 2", len(a.GetView().GetFields()))
	}
	// content 永遠是空的:那是 Discord 唯一會解析 @everyone 的地方,
	// 而公告裡的名稱是使用者自己填的暱稱。
	if a.GetContent() != "" {
		t.Errorf("content 必須永遠是空的,拿到 %q", a.GetContent())
	}
	if a.GetView().GetEphemeral() {
		t.Error("頻道推播不該是 ephemeral")
	}

	max, vis, _ := spy.seen()
	if max != 10 || vis != 30*time.Second {
		t.Errorf("參數沒原樣傳下去:max=%d vis=%v", max, vis)
	}
}

// 使用者 token 打過來 → PermissionDenied(不是 Unauthenticated:
// 身分有效,只是不是這種身分)。
func TestPullAnnouncementsRejectsUserToken(t *testing.T) {
	notif, _ := newNotificationEnv(t, &spyAnnouncements{})

	if _, err := notif.PullAnnouncements(context.Background(),
		withToken(pullReq(10, 30))); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("Pull:code = %v,期望 PermissionDenied", codeOf(err))
	}
	_, err := notif.AckAnnouncements(context.Background(), withToken(
		connect.NewRequest(&platformv1.AckAnnouncementsRequest{EventIds: []string{"1"}})))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("Ack:code = %v,期望 PermissionDenied", codeOf(err))
	}
}

// 完全沒憑證 → Unauthenticated。
func TestPullAnnouncementsRequiresCredentials(t *testing.T) {
	notif, _ := newNotificationEnv(t, &spyAnnouncements{})

	if _, err := notif.PullAnnouncements(context.Background(), pullReq(10, 30)); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("code = %v,期望 Unauthenticated", codeOf(err))
	}
}

// ActivityService 的既有行為不變:它仍然一定要 X-Acting-User。
// (subjectlessServices 只放寬 NotificationService。)
func TestActivityStillRequiresActingUser(t *testing.T) {
	_, activity := newNotificationEnv(t, &spyAnnouncements{})

	_, err := activity.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, ""))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v,期望 InvalidArgument(缺 X-Acting-User)", codeOf(err))
	}
}

func TestAckAnnouncementsPassesIDs(t *testing.T) {
	spy := &spyAnnouncements{}
	notif, _ := newNotificationEnv(t, spy)

	res, err := notif.AckAnnouncements(context.Background(), withService(
		connect.NewRequest(&platformv1.AckAnnouncementsRequest{EventIds: []string{"1", "2", "3"}}),
		svcTokenOld, ""))
	if err != nil {
		t.Fatalf("AckAnnouncements: %v", err)
	}
	if res.Msg.GetAcknowledged() != 3 {
		t.Errorf("acknowledged = %d,想要 3", res.Msg.GetAcknowledged())
	}
	if _, _, acked := spy.seen(); len(acked) != 3 || acked[0] != "1" {
		t.Errorf("id 沒原樣傳下去:%v", acked)
	}
}

// 空清單是合法的 no-op:整批都沒貼成功時送空 Ack 比「有條件地不呼叫」好寫。
func TestAckAnnouncementsEmptyIsNoOp(t *testing.T) {
	notif, _ := newNotificationEnv(t, &spyAnnouncements{})

	res, err := notif.AckAnnouncements(context.Background(), withService(
		connect.NewRequest(&platformv1.AckAnnouncementsRequest{}), svcTokenOld, ""))
	if err != nil {
		t.Fatalf("空 Ack 不該是錯誤:%v", err)
	}
	if res.Msg.GetAcknowledged() != 0 {
		t.Errorf("acknowledged = %d,想要 0", res.Msg.GetAcknowledged())
	}
}

// 實作回的 sentinel 要映射成 InvalidArgument(而不是 Internal)。
func TestAckAnnouncementsMapsInvalidRequest(t *testing.T) {
	notif, _ := newNotificationEnv(t, &spyAnnouncements{
		ackErr: errors.New("wrapped: " + notification.ErrInvalidRequest.Error()),
	})
	// 上面那個是「沒有 wrap sentinel」的假錯誤 —— 應該落到 Internal。
	_, err := notif.AckAnnouncements(context.Background(), withService(
		connect.NewRequest(&platformv1.AckAnnouncementsRequest{EventIds: []string{"1"}}),
		svcTokenOld, ""))
	if codeOf(err) != connect.CodeInternal {
		t.Fatalf("沒有 wrap sentinel 的錯誤應該是 Internal,拿到 %v", codeOf(err))
	}

	notif2, _ := newNotificationEnv(t, &spyAnnouncements{
		ackErr: errors.Join(errors.New("一次太多筆"), notification.ErrInvalidRequest),
	})
	_, err = notif2.AckAnnouncements(context.Background(), withService(
		connect.NewRequest(&platformv1.AckAnnouncementsRequest{EventIds: []string{"1"}}),
		svcTokenOld, ""))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("wrap 了 sentinel 的錯誤應該是 InvalidArgument,拿到 %v", codeOf(err))
	}
	if r := transport.ErrorReason(err); r != "notification_invalid_request" {
		t.Errorf("reason = %q,想要 notification_invalid_request", r)
	}
}

// 未注入實作時回 Unimplemented。
func TestNotificationUnimplemented(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	notif := platformv1connect.NewNotificationServiceClient(srv.Client(), srv.URL)

	_, err := notif.PullAnnouncements(context.Background(),
		withService(pullReq(10, 30), svcTokenOld, ""))
	if codeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v,期望 Unimplemented", codeOf(err))
	}
}
