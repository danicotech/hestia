package transport_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/transport"
)

// QA 2026-09-08 黑箱探測的迴歸測試。四個獨立的問題:
//   - X-Acting-User 的內容被寫進 event_logs(呼叫端可控字串落地);
//   - 服務憑證打 public RPC 時稽核記成「某個真人做的」;
//   - 任一把服務 token 自動取得完整代打白名單(沒有 per-token scope);
//   - 補記那一列的 action 直接用未截斷的 URL path。

// ── 稽核不對 public RPC 說謊 ─────────────────────────────────────

// 服務憑證 + X-Acting-User 打 **public** procedure(這裡是 AuthService/Logout,
// 而代打白名單的註解正宣稱「登入本身不能被代打」):
// event_logs 的 user_id 必須是 NULL,而且不能有 delegated 標記。
//
// 修正前:user_id = 那個真實使用者、channel = discord、delegated 沒設 ——
// 表上與「他自己登出」完全無法區分。
func TestPublicRPCWithActingUserIsNotAttributedToUser(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		ActingUsers: &fakeActors{},
		ServiceTokens: []transport.ServiceToken{
			{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()},
		},
	})
	auth := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)

	// Auth 未注入 → Unimplemented,但那一列稽核照樣會寫。
	_, _ = auth.Logout(context.Background(), withService(
		connect.NewRequest(&platformv1.LogoutRequest{}), svcTokenOld, actingLinked))

	rec := waitRecords(t, sink, 1)[0]
	if rec.UserID != nil {
		t.Fatalf("public RPC 被記成 user_id=%d —— 稽核在說謊", *rec.UserID)
	}
	if strings.Contains(string(rec.Response), "delegated") {
		t.Fatalf("public RPC 不該有代打標記:%s", rec.Response)
	}
	// provider 仍然留著:「這個請求從 Discord 進來」是真的,值得記。
	// 它經過白名單比對,不是呼叫端的自由字串。
	if rec.Channel != eventlog.ChannelDiscord {
		t.Fatalf("channel = %q,期望 discord(入口是真的)", rec.Channel)
	}
	assertNoServiceToken(t, rec)
}

// 只收服務身分的 RPC(ActivityService)**仍然**要把主體記在 acting user 頭上
// —— 修法不能把這條一起關掉。
func TestServiceOnlyRPCStillAttributesActingUser(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		Activity:    &fakeActivity{},
		ActingUsers: &fakeActors{},
		ServiceTokens: []transport.ServiceToken{
			{Name: "stentor", Token: svcTokenOld},
		},
	})
	activity := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)

	if _, err := activity.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("RecordVoiceSession: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.UserID == nil || *rec.UserID != actingUserID {
		t.Fatalf("user_id = %v,期望 %d(活動記錄的主體就是 acting user)", rec.UserID, actingUserID)
	}
}

// 呼叫端可控的 X-Acting-User 內容一個字都不能落地(黑箱版)。
func TestActingUserNeverLandsInEventLog(t *testing.T) {
	const canary = "LEAK-CANARY-AAAA-BBBB"
	srv, sink := newServer(t, transport.Deps{
		Activity:    &fakeActivity{},
		ActingUsers: &fakeActors{},
		ServiceTokens: []transport.ServiceToken{
			{Name: "stentor", Token: svcTokenOld},
		},
	})
	activity := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)

	_, err := activity.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, canary+":1"))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v,期望 InvalidArgument", codeOf(err))
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("回應洩漏了呼叫端輸入:%v", err)
	}
	if r := transport.ErrorReason(err); r != "acting_user_provider_unsupported" {
		t.Fatalf("reason = %q,想要 acting_user_provider_unsupported", r)
	}

	rec := waitRecords(t, sink, 1)[0]
	blob := rec.Action + "|" + string(rec.Request) + "|" + string(rec.Response)
	if strings.Contains(blob, canary) {
		t.Fatalf("event_logs 落地了呼叫端輸入:%s", blob)
	}
}

// ── per-token 代打 scope ─────────────────────────────────────────

// 一把只被授予 /daily 的憑證,不能拿去買東西 —— 這是憑證外洩時的爆炸半徑。
func TestDelegationScopeIsPerToken(t *testing.T) {
	spy := &spyShop{seenKeys: map[string]bool{}}
	srv, _ := newServer(t, transport.Deps{
		Daily:       &spyDaily{},
		Shop:        spy,
		Ledger:      &fakeLedger{balance: 500},
		Directory:   fakeDirectory{},
		ActingUsers: &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{
			Name: "daily-only", Token: svcTokenOld,
			Delegable: []string{platformv1connect.DailyServiceClaimProcedure},
		}},
	})
	daily := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	shop := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)

	if _, err := daily.Claim(context.Background(),
		withService(claimReq(), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("scope 內的 RPC 應該通:%v", err)
	}

	_, err := shop.Purchase(context.Background(),
		withService(purchaseReq("k1"), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("scope 外的 RPC:code = %v,期望 PermissionDenied", codeOf(err))
	}
	// 與「這支 RPC 根本不能代打」分得開:一個要改程式,一個要改部署設定。
	if r := transport.ErrorReason(err); r != "delegation_not_granted" {
		t.Fatalf("reason = %q,想要 delegation_not_granted", r)
	}
	if n := spy.chargeCount(); n != 0 {
		t.Fatalf("scope 外的請求動了 %d 次錢", n)
	}
}

// 沒有 scope 的憑證一條都不能代打(fail closed)。
// 只收服務身分的 RPC 不受 scope 約束 —— 那是服務自己的能力,不是代表某個人。
func TestEmptyScopeGrantsNoDelegation(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		Daily:       &spyDaily{},
		Activity:    &fakeActivity{},
		ActingUsers: &fakeActors{},
		ServiceTokens: []transport.ServiceToken{
			{Name: "ingest-only", Token: svcTokenOld}, // 沒給 Delegable
		},
	})
	daily := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	activity := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)

	_, err := daily.Claim(context.Background(), withService(claimReq(), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("沒有 scope 卻能代打:code = %v", codeOf(err))
	}
	if _, err := activity.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("只收服務身分的 RPC 不該被 scope 擋掉:%v", err)
	}
}

// 兩把憑證各有各的 scope:輪替與「不同用途不同權限」都要成立。
func TestScopeIsIndependentPerToken(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		Daily:       &spyDaily{},
		Profiles:    &spyProfiles{},
		ActingUsers: &fakeActors{},
		ServiceTokens: []transport.ServiceToken{
			{Name: "daily-only", Token: svcTokenOld,
				Delegable: []string{platformv1connect.DailyServiceClaimProcedure}},
			{Name: "profile-only", Token: svcTokenNew,
				Delegable: []string{platformv1connect.MeServiceGetProfileProcedure}},
		},
	})
	daily := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	me := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)

	if _, err := daily.Claim(context.Background(),
		withService(claimReq(), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("daily-only 打 /daily:%v", err)
	}
	if _, err := daily.Claim(context.Background(),
		withService(claimReq(), svcTokenNew, actingLinked)); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("profile-only 竟然打得到 /daily:code = %v", codeOf(err))
	}
	if _, err := me.GetProfile(context.Background(), withService(
		connect.NewRequest(&platformv1.GetProfileRequest{}), svcTokenNew, actingLinked)); err != nil {
		t.Fatalf("profile-only 打 GetProfile:%v", err)
	}
}

// scope 寫錯要在**啟動時**炸:打錯字的那條會靜靜不生效,
// 白名單外的那條代表有人以為 scope 可以擴權。兩者都不能等上線才發現。
func TestNewRejectsBadDelegationScope(t *testing.T) {
	cases := []struct {
		name  string
		scope []string
	}{
		{"打錯字的 procedure", []string{"/hestia.platform.v1.DailyService/Clam"}},
		{"不在全域白名單內", []string{platformv1connect.MeServiceUpdateTimezoneProcedure}},
		{"想拿管理 RPC", []string{platformv1connect.AdminEconomyServiceGrantProcedure}},
		{"想拿只收服務的 RPC", []string{platformv1connect.ActivityServiceRecordMessagesProcedure}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := transport.New(transport.Deps{
				ServiceTokens: []transport.ServiceToken{
					{Name: "stentor", Token: svcTokenOld, Delegable: c.scope},
				},
			})
			if err == nil {
				t.Fatal("設定有問題卻開得起來")
			}
			if !strings.Contains(err.Error(), "代打 scope") {
				t.Fatalf("錯誤訊息沒說清楚問題:%v", err)
			}
		})
	}
}

// 出貨的全域白名單本身可以整包當 scope 用(組裝端的常見寫法)。
func TestAllDelegableProceduresIsAcceptableScope(t *testing.T) {
	if _, err := transport.New(transport.Deps{
		ServiceTokens: []transport.ServiceToken{
			{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()},
		},
	}); err != nil {
		t.Fatalf("AllDelegableProcedures() 應該是合法 scope:%v", err)
	}
	if len(transport.AllDelegableProcedures()) == 0 {
		t.Fatal("代打白名單是空的?")
	}
}

// ── 補記那一列的 action ──────────────────────────────────────────

// 未認證的任何人都能打任意路徑,而 event_logs 保留 180 天。
// action 不能原樣收下超長字串(實測修正前 4001 字元直接落地)。
func TestFallbackAuditActionIsBounded(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{})

	long := "/" + strings.Repeat("A", 4000)
	postRaw(t, srv, long, "application/json", []byte(`{}`))

	rec := waitRecords(t, sink, 1)[0]
	if len(rec.Action) > 256 {
		t.Fatalf("action 長度 %d,超過上限", len(rec.Action))
	}
	if strings.Contains(rec.Action, "AAAA") {
		t.Fatalf("非 RPC 路徑不該原樣落地:%q", rec.Action)
	}
}

// 前綴對但後面接一長串的變形:仍然要截斷。
func TestFallbackAuditActionTruncatesRPCLookalike(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{})

	long := "/hestia.platform.v1.DailyService/" + strings.Repeat("B", 4000)
	postRaw(t, srv, long, "application/json", []byte(`{}`))

	rec := waitRecords(t, sink, 1)[0]
	if len(rec.Action) > 256 {
		t.Fatalf("action 長度 %d,超過上限", len(rec.Action))
	}
	if !strings.HasPrefix(rec.Action, "/hestia.platform.v1.DailyService/") {
		t.Fatalf("合法前綴應該留著,方便查:%q", rec.Action)
	}
}

// 真的 RPC 路徑完整保留(截斷不能把正常流量弄糊)。
func TestFallbackAuditKeepsRealProcedure(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{})

	// 壞掉的 JSON:連解碼都過不了,所以走補記那條路。
	postRaw(t, srv, platformv1connect.DailyServiceClaimProcedure,
		"application/json", []byte(`{`), [2]string{"Authorization", "Bearer " + testToken})

	rec := waitRecords(t, sink, 1)[0]
	if rec.Action != platformv1connect.DailyServiceClaimProcedure {
		t.Fatalf("action = %q,期望完整的 procedure", rec.Action)
	}
	if rec.Status == "" || rec.ErrorCode == "" {
		t.Fatalf("補記的列缺欄位:%+v", rec)
	}
}

// ── 批次結果的成因 ──────────────────────────────────────────────

// perMessageFailure 讓每一筆訊息各自帶一個不同的錯誤。
type perMessageFailure struct {
	fakeActivity
	errs []error
}

func (f *perMessageFailure) RecordMessages(
	_ context.Context, p activitylog.MessagesParams,
) (*activitylog.MessagesResult, error) {
	out := &activitylog.MessagesResult{SpaceID: 55}
	for i, m := range p.Messages {
		r := activitylog.MessageResult{ClientRef: m.ClientRef, MessageID: m.MessageID}
		if i < len(f.errs) {
			r.Err = f.errs[i]
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// 批次裡**單筆**失敗時,呼叫端一樣要拿得到機器可讀的成因。
//
// 整批失敗走 connect 錯誤,metadata 帶得了 reason;單筆失敗是 body 裡的一列,
// 沒有 metadata 可搭 —— 少了 error_reason,呼叫端只能回頭比對中文訊息,
// 正是這一輪要消滅的做法。
func TestBatchMessageResultsCarryReason(t *testing.T) {
	svc := &perMessageFailure{errs: []error{
		activitylog.ErrSpaceNotRegistered,
		shop.ErrUserNotFound,
		errors.New("pq: 內部細節"), // 未映射 → 沒有 reason
		nil,                    // 成功 → 沒有 reason
	}}
	srv, _ := newServer(t, transport.Deps{
		Activity:      svc,
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld}},
	})
	client := platformv1connect.NewActivityServiceClient(srv.Client(), srv.URL)

	msgs := make([]*platformv1.MessageRecord, 0, 4)
	for i := range svc.errs {
		msgs = append(msgs, &platformv1.MessageRecord{
			ClientRef: strconv.Itoa(i), ChannelId: "c1",
			MessageId: "m" + strconv.Itoa(i),
		})
	}
	res, err := client.RecordMessages(context.Background(), withService(
		connect.NewRequest(&platformv1.RecordMessagesRequest{GuildId: "g1", Messages: msgs}),
		svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("RecordMessages: %v", err)
	}

	want := []string{"space_not_registered", "user_not_found", "", ""}
	got := res.Msg.GetResults()
	if len(got) != len(want) {
		t.Fatalf("拿到 %d 筆結果,想要 %d 筆", len(got), len(want))
	}
	for i, r := range got {
		if r.GetErrorReason() != want[i] {
			t.Errorf("第 %d 筆 reason = %q,想要 %q", i, r.GetErrorReason(), want[i])
		}
	}
	// 未映射的那一筆:有 code,但**沒有** reason(不洩漏內部分類),
	// 而且訊息也不是原文。
	third := got[2]
	if third.GetErrorCode() != connect.CodeInternal.String() {
		t.Errorf("未映射錯誤的 code = %q", third.GetErrorCode())
	}
	if strings.Contains(third.GetErrorMessage(), "內部細節") {
		t.Errorf("未映射錯誤的原文外洩:%q", third.GetErrorMessage())
	}
}
