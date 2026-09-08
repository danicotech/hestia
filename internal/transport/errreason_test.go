package transport_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/transport"
)

// reason 必須在**三種協定**上都讀得到:Connect 走 HTTP header,
// gRPC 與 gRPC-Web 走 trailer。少了任何一種,呼叫端就得依部署方式寫兩套邏輯。
//
// 這一檔走完整 HTTP 往返(httptest),不是直接呼叫 handler ——
// 要驗的正是「metadata 有沒有真的過得去線路」。
func TestErrorReasonAcrossProtocols(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		Daily: &fakeDaily{err: daily.ErrAlreadyClaimed},
	})

	protocols := []struct {
		name string
		opt  connect.ClientOption
	}{
		// 預設就是 Connect 協定(HTTP/JSON),不必特別指定。
		{"connect", connect.WithClientOptions()},
		{"grpc", connect.WithGRPC()},
		{"grpcweb", connect.WithGRPCWeb()},
	}
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL, p.opt)
			_, err := client.Claim(context.Background(), withToken(claimReq()))
			if err == nil {
				t.Fatal("想要錯誤")
			}
			if connect.CodeOf(err) != connect.CodeAlreadyExists {
				t.Fatalf("code = %v,想要 AlreadyExists", connect.CodeOf(err))
			}
			if got := transport.ErrorReason(err); got != "daily_already_claimed" {
				t.Fatalf("%s 上讀不到 reason:%q", p.name, got)
			}
		})
	}
}

// 閘道真正的問題場景:兩個都是 FailedPrecondition,處置完全不同
// (引導綁定 vs 顯示餘額不足)。沒有 reason 就只能比對中文訊息。
func TestGatewayCanDistinguishFailedPreconditions(t *testing.T) {
	// 未綁定的 Discord 使用者:錯誤來自認證攔截器的身分解析,
	// 這正是 stentor 最需要分辨的那一個(要顯示登入引導)。
	notLinked, _ := newServer(t, transport.Deps{
		Activity:      &fakeActivity{},
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	activity := platformv1connect.NewActivityServiceClient(notLinked.Client(), notLinked.URL)
	_, err := activity.RecordVoiceSession(context.Background(),
		withService(voiceReq(), svcTokenOld, actingUnlinked))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v", connect.CodeOf(err))
	}
	if got := transport.ErrorReason(err); got != "actor_not_linked" {
		t.Fatalf("reason = %q,想要 actor_not_linked", got)
	}

	broke, _ := newServer(t, transport.Deps{
		Daily: &fakeDaily{err: daily.ErrTimezoneChangeCooldown},
	})
	dailyClient := platformv1connect.NewDailyServiceClient(broke.Client(), broke.URL)
	_, err = dailyClient.Claim(context.Background(), withToken(claimReq()))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v", connect.CodeOf(err))
	}
	if got := transport.ErrorReason(err); got != "timezone_change_cooldown" {
		t.Fatalf("reason = %q,想要 timezone_change_cooldown", got)
	}
}

// 內部錯誤在線路上也不能帶 reason(不只是在進程內)。
func TestInternalErrorHasNoReasonOnTheWire(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		// 未映射的錯誤:對外必須是 Internal + 固定文案,原文只進 log。
		Daily: &fakeDaily{err: errors.New(`pq: relation "secret" does not exist`)},
	})
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)

	_, err := client.Claim(context.Background(), withToken(claimReq()))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v,想要 Internal", connect.CodeOf(err))
	}
	if got := transport.ErrorReason(err); got != "" {
		t.Fatalf("內部錯誤帶了 reason %q —— 那是在洩漏內部分類", got)
	}
}
