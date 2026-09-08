package transport_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/transport"
)

// 服務對服務認證的黑箱測試:走真的 HTTP,驗「哪種身分能打哪個 RPC」
// 以及「憑證絕不進 event_logs」。DB 行為在 storage/activitylogpg。

const (
	svcTokenOld = "stentor-old-token-0000000000000000000000"
	svcTokenNew = "stentor-new-token-1111111111111111111111"
	// actingLinked 是有綁定的 Discord id;actingUnlinked 沒有綁定。
	actingLinked   = "discord:100000000000000001"
	actingUnlinked = "discord:999999999999999999"
)

const actingUserID int64 = 777

// fakeActors 模擬 identities 查詢。**它永遠不建帳號** ——
// created 計數器就是為了證明「查不到」不會變成「順手建一個」。
type fakeActors struct {
	mu      sync.Mutex
	created int
	calls   int
}

func (f *fakeActors) ResolveActor(_ context.Context, provider, externalID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if provider == activitylog.ProviderDiscord && externalID == "100000000000000001" {
		return actingUserID, nil
	}
	return 0, activitylog.ErrActorNotLinked
}

func (f *fakeActors) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// fakeActivity 記下 handler 到底有沒有被叫到、拿到的主體是誰。
type fakeActivity struct {
	mu         sync.Mutex
	calls      int
	lastUserID int64
}

func (f *fakeActivity) note(userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastUserID = userID
}

func (f *fakeActivity) seen() (int, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastUserID
}

func (f *fakeActivity) RecordVoiceSession(_ context.Context, p activitylog.VoiceSessionParams) (*activitylog.VoiceSessionResult, error) {
	f.note(p.UserID)
	return &activitylog.VoiceSessionResult{SpaceID: 55, VoiceSecondsCounted: 60}, nil
}

func (f *fakeActivity) RecordMessages(_ context.Context, p activitylog.MessagesParams) (*activitylog.MessagesResult, error) {
	f.note(p.UserID)
	out := &activitylog.MessagesResult{SpaceID: 55}
	for _, m := range p.Messages {
		out.Results = append(out.Results, activitylog.MessageResult{
			ClientRef: m.ClientRef, MessageID: m.MessageID, ContentStored: true,
		})
	}
	return out, nil
}

func (f *fakeActivity) RecordRevision(_ context.Context, p activitylog.RevisionParams) (*activitylog.RevisionResult, error) {
	f.note(p.UserID)
	return &activitylog.RevisionResult{SpaceID: 55}, nil
}

func (f *fakeActivity) RecordReaction(_ context.Context, p activitylog.ReactionParams) (*activitylog.ReactionResult, error) {
	f.note(p.UserID)
	return &activitylog.ReactionResult{SpaceID: 55}, nil
}

func (f *fakeActivity) RecordPresence(_ context.Context, p activitylog.PresenceParams) (*activitylog.PresenceResult, error) {
	f.note(p.UserID)
	return &activitylog.PresenceResult{SpaceID: 55}, nil
}

// newServiceServer 起一台同時設定了兩把服務 token(模擬輪替期)的伺服器。
func newServiceServer(t *testing.T) (
	platformv1connect.ActivityServiceClient, platformv1connect.DailyServiceClient,
	*fakeActivity, *fakeActors, *recordSink,
) {
	t.Helper()
	act := &fakeActivity{}
	actors := &fakeActors{}
	srv, sink := newServer(t, transport.Deps{
		Activity:      act,
		ActingUsers:   actors,
		Daily:         &fakeDaily{res: &daily.Result{Amount: 1, Streak: 1}},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}, {Name: "stentor-next", Token: svcTokenNew, Delegable: transport.AllDelegableProcedures()}},
	})
	return platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL),
		platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL),
		act, actors, sink
}

// withService 加上服務憑證與行為主體。
func withService[T any](req *connect.Request[T], token, acting string) *connect.Request[T] {
	if token != "" {
		req.Header().Set("X-Service-Token", token)
	}
	if acting != "" {
		req.Header().Set("X-Acting-User", acting)
	}
	return req
}

func voiceReq() *connect.Request[platformv1.RecordVoiceSessionRequest] {
	return connect.NewRequest(&platformv1.RecordVoiceSessionRequest{
		GuildId: "g1", ChannelId: "c1",
	})
}

// 輪替期間新舊兩把 token 都要通過,而且主體是 X-Acting-User 解出來的人。
func TestServiceTokenRotationBothAccepted(t *testing.T) {
	client, _, act, _, _ := newServiceServer(t)
	for _, tok := range []string{svcTokenOld, svcTokenNew} {
		if _, err := client.RecordVoiceSession(context.Background(),
			withService(voiceReq(), tok, actingLinked)); err != nil {
			t.Fatalf("token %q 應該通過:%v", tok, err)
		}
	}
	calls, userID := act.seen()
	if calls != 2 {
		t.Fatalf("handler 被叫 %d 次,期望 2", calls)
	}
	if userID != actingUserID {
		t.Fatalf("主體 = %d,期望 %d(X-Acting-User 解出來的人)", userID, actingUserID)
	}
}

// 錯的 / 空的服務 token 一律 Unauthenticated,而且 handler 不會被叫到。
func TestServiceTokenRejected(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  connect.Code
	}{
		{"錯誤 token", "not-the-right-token-aaaaaaaaaaaaaaaaaaaa", connect.CodeUnauthenticated},
		{"差一個字元", svcTokenOld + "x", connect.CodeUnauthenticated},
		// 完全不帶憑證時走使用者那條路:沒有 Bearer = 未認證。
		{"完全沒有憑證", "", connect.CodeUnauthenticated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, _, act, _, _ := newServiceServer(t)
			_, err := client.RecordVoiceSession(context.Background(),
				withService(voiceReq(), c.token, actingLinked))
			if codeOf(err) != c.want {
				t.Fatalf("code = %v,期望 %v(err=%v)", codeOf(err), c.want, err)
			}
			if calls, _ := act.seen(); calls != 0 {
				t.Fatalf("被擋下的請求竟然進到 handler(%d 次)", calls)
			}
		})
	}
}

// 使用者 token 不能呼叫活動記錄 RPC:身分有效,但不是這種身分。
func TestUserTokenCannotCallActivityRPC(t *testing.T) {
	client, _, act, _, sink := newServiceServer(t)
	_, err := client.RecordVoiceSession(context.Background(), withToken(voiceReq()))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}
	if calls, _ := act.seen(); calls != 0 {
		t.Fatalf("handler 竟然被叫到")
	}
	// 被擋下的那一列必須看得出是誰在試(認證在授權之前寫進便條)。
	recs := waitRecords(t, sink, 1)
	if recs[0].UserID == nil || *recs[0].UserID != testUserID {
		t.Fatalf("event_logs.user_id = %v,期望 %d", recs[0].UserID, testUserID)
	}
	if recs[0].Status != eventlog.StatusDenied {
		t.Fatalf("status = %q,期望 denied", recs[0].Status)
	}
}

// 服務 token 不能呼叫**白名單外**的使用者 RPC:「代表某人」不等於「就是某人」,
// 能代打哪幾支是逐條決定的(delegatedProcedures,測試在 delegation_test.go)。
func TestServiceTokenCannotCallNonDelegatedUserRPC(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		ProfileWrites: fakeProfiles{},
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	me := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	_, err := me.UpdateTimezone(context.Background(), withService(
		connect.NewRequest(&platformv1.UpdateTimezoneRequest{Timezone: "Asia/Taipei"}),
		svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}
}

// 同時帶兩種憑證 = 拒絕。不做「擇一採用」的猜測。
func TestMixedCredentialsRejected(t *testing.T) {
	client, _, act, _, _ := newServiceServer(t)
	req := withService(voiceReq(), svcTokenOld, actingLinked)
	req.Header().Set("Authorization", "Bearer "+testToken)
	if _, err := client.RecordVoiceSession(context.Background(), req); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}
	if calls, _ := act.seen(); calls != 0 {
		t.Fatal("handler 竟然被叫到")
	}
}

// 沒綁定的 Discord id → 可辨識的 FailedPrecondition,而且沒有建出任何使用者。
func TestActingUserNotLinked(t *testing.T) {
	client, _, act, actors, sink := newServiceServer(t)
	_, err := client.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingUnlinked))
	if codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v,期望 failed_precondition(err=%v)", codeOf(err), err)
	}
	if calls, _ := act.seen(); calls != 0 {
		t.Fatal("解析失敗卻還是進了 handler")
	}
	if n := actors.createdCount(); n != 0 {
		t.Fatalf("竟然建了 %d 個使用者——這條路徑永遠不該建帳號", n)
	}
	// 解不出使用者的那一列,channel 仍然是 discord(入口來源看得出來)。
	recs := waitRecords(t, sink, 1)
	if recs[0].Channel != eventlog.ChannelDiscord {
		t.Fatalf("channel = %q,期望 discord", recs[0].Channel)
	}
	if recs[0].UserID != nil {
		t.Fatalf("解不出使用者時 user_id 應該是 NULL,得到 %v", *recs[0].UserID)
	}
}

// 只收服務的 RPC 一定要說明代表誰:少了 X-Acting-User 是參數錯誤。
func TestActingUserRequired(t *testing.T) {
	client, _, _, _, _ := newServiceServer(t)
	_, err := client.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, ""))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v,期望 invalid_argument(err=%v)", codeOf(err), err)
	}
}

func TestActingUserBadFormat(t *testing.T) {
	client, _, _, _, _ := newServiceServer(t)
	for _, bad := range []string{"123456", "twitch:123", "discord:"} {
		_, err := client.RecordVoiceSession(context.Background(),
			withService(voiceReq(), svcTokenOld, bad))
		if codeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("X-Acting-User=%q code = %v,期望 invalid_argument", bad, codeOf(err))
		}
	}
}

// 服務呼叫成功時,event_logs 的 channel 是 discord、user_id 是代表的那個人、
// space_id 由 handler 補上,而**服務憑證不出現在任何欄位**。
func TestServiceCallAuditRecord(t *testing.T) {
	client, _, _, _, sink := newServiceServer(t)
	if _, err := client.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("RecordVoiceSession: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Channel != eventlog.ChannelDiscord {
		t.Fatalf("channel = %q,期望 discord", rec.Channel)
	}
	if rec.UserID == nil || *rec.UserID != actingUserID {
		t.Fatalf("user_id = %v,期望 %d", rec.UserID, actingUserID)
	}
	if rec.SpaceID == nil || *rec.SpaceID != 55 {
		t.Fatalf("space_id = %v,期望 55", rec.SpaceID)
	}
	assertNoServiceToken(t, rec)
}

// 憑證絕不進稽核表 —— 成功、被擋、格式錯,每一種路徑都要驗。
func TestServiceTokenNeverInEventLogs(t *testing.T) {
	client, daily, _, _, sink := newServiceServer(t)
	ctx := context.Background()

	_, _ = client.RecordVoiceSession(ctx, withService(voiceReq(), svcTokenOld, actingLinked))
	_, _ = client.RecordVoiceSession(ctx, withService(voiceReq(), svcTokenOld, actingUnlinked))
	_, _ = client.RecordVoiceSession(ctx, withService(voiceReq(), "bad-token-"+strings.Repeat("z", 30), actingLinked))
	_, _ = daily.Claim(ctx, withService(connect.NewRequest(&platformv1.ClaimRequest{}), svcTokenNew, actingLinked))

	for _, rec := range waitRecords(t, sink, 4) {
		assertNoServiceToken(t, rec)
	}
}

func assertNoServiceToken(t *testing.T, rec eventlog.Record) {
	t.Helper()
	haystack := strings.Join([]string{
		rec.RequestID, rec.Channel, rec.Kind, rec.Action, rec.Status,
		rec.ErrorCode, rec.TraceID, string(rec.Request), string(rec.Response),
	}, "|")
	for _, secret := range []string{svcTokenOld, svcTokenNew, "stentor"} {
		if strings.Contains(haystack, secret) {
			t.Fatalf("event_logs 出現了不該有的字串 %q:%s", secret, haystack)
		}
	}
}

// 批次的每一筆都回結果,而且 request 摘要只記筆數不記內容。
func TestRecordMessagesSummaryHasNoContent(t *testing.T) {
	client, _, _, _, sink := newServiceServer(t)
	const secret = "這是不該進稽核表的訊息原文"
	res, err := client.RecordMessages(context.Background(), withService(
		connect.NewRequest(&platformv1.RecordMessagesRequest{
			GuildId: "g1",
			Messages: []*platformv1.MessageRecord{
				{ClientRef: "a", MessageId: "m1", ChannelId: "c1", ChannelKind: "text", Content: secret},
				{ClientRef: "b", MessageId: "m2", ChannelId: "c1", ChannelKind: "text", Content: secret},
			},
		}), svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("RecordMessages: %v", err)
	}
	if got := len(res.Msg.GetResults()); got != 2 {
		t.Fatalf("results = %d,期望 2", got)
	}
	if res.Msg.GetSucceeded() != 2 || res.Msg.GetFailed() != 0 {
		t.Fatalf("succeeded/failed = %d/%d", res.Msg.GetSucceeded(), res.Msg.GetFailed())
	}
	rec := waitRecords(t, sink, 1)[0]
	if strings.Contains(string(rec.Request), secret) {
		t.Fatalf("訊息原文進了 event_logs:%s", rec.Request)
	}
	if !strings.Contains(string(rec.Request), `"count":2`) {
		t.Fatalf("摘要少了筆數:%s", rec.Request)
	}
}

// ActivityService 沒注入實作時回 Unimplemented(而不是 panic 或靜默成功)。
func TestActivityUnimplemented(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	client := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)
	_, err := client.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v,期望 unimplemented(err=%v)", codeOf(err), err)
	}
}

// 沒設定任何服務 token = 服務認證關閉,ActivityService 一律拒絕(fail closed)。
func TestNoServiceTokensConfiguredRejects(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Activity: &fakeActivity{}, ActingUsers: &fakeActors{}})
	client := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)
	_, err := client.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v,期望 unauthenticated(err=%v)", codeOf(err), err)
	}
}

// 設定不合法時服務開不起來 —— 不合格的 token 悄悄生效是更糟的失敗。
func TestNewRejectsBadServiceTokenConfig(t *testing.T) {
	_, err := transport.New(transport.Deps{
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: "too-short"}},
	})
	if err == nil {
		t.Fatal("期望啟動失敗")
	}
}
