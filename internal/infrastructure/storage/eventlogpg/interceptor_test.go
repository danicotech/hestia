package eventlogpg_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/infrastructure/storage/eventlogpg"
	"github.com/danicotech/hestia/internal/transport"
)

// 攔截器 + 真資料庫的整合測試。放在這裡而不是 internal/transport,是因為
// depguard 禁止 transport(含其測試)import storage —— 入口層不該知道 DB 存在。
// 這一層才是「兩者接起來真的有寫進 Postgres」的驗收點。

const secretToken = "super-secret-access-token"

// testUser 是 var 而非 const:斷言時要取位址比對 *int64 欄位。
var testUser = int64(4242)

type stubDaily struct {
	err      error
	panicNow bool
}

func (s stubDaily) Claim(context.Context, int64) (*daily.Result, error) {
	if s.panicNow {
		panic("測試用 panic")
	}
	if s.err != nil {
		return nil, s.err
	}
	return &daily.Result{Amount: 150, Streak: 5}, nil
}

type stubAuth struct{}

func (stubAuth) Authenticate(_ context.Context, token string) (int64, error) {
	if token != secretToken {
		return 0, transport.ErrUnauthenticated
	}
	return testUser, nil
}

func newHandler(t *testing.T, svc daily.Service) *httptest.Server {
	t.Helper()
	h, err := transport.New(transport.Deps{
		Daily:         svc,
		Authenticator: stubAuth{},
		EventLog:      eventlogpg.New(pool),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 關機要把佇列排空:漏掉的紀錄不能只是「反正是非同步」帶過。
		if err := h.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return srv
}

func claim(t *testing.T, srv *httptest.Server, withAuth bool) error {
	t.Helper()
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&platformv1.ClaimRequest{})
	if withAuth {
		req.Header().Set("Authorization", "Bearer "+secretToken)
	}
	_, err := client.Claim(context.Background(), req)
	return err
}

// 成功 / 失敗 / 被擋 / panic 各留一列,欄位齊全。
func TestInterceptorWritesOneRowPerCall(t *testing.T) {
	setup(t)

	okSrv := newHandler(t, stubDaily{})
	if err := claim(t, okSrv, true); err != nil {
		t.Fatalf("成功路徑不該出錯:%v", err)
	}

	errSrv := newHandler(t, stubDaily{err: daily.ErrAlreadyClaimed})
	if err := claim(t, errSrv, true); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("期望 already_exists,got %v", err)
	}

	if err := claim(t, okSrv, false); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("期望 unauthenticated,got %v", err)
	}

	panicSrv := newHandler(t, stubDaily{panicNow: true})
	if err := claim(t, panicSrv, true); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("panic 應被復原成 internal,got %v", err)
	}

	rows := waitRows(t, 4)
	if len(rows) != 4 {
		t.Fatalf("四次呼叫應留 4 列,got %d", len(rows))
	}
	for i, r := range rows {
		if r.Action != platformv1connect.DailyServiceClaimProcedure {
			t.Fatalf("第 %d 列 action = %q", i, r.Action)
		}
		if r.Channel != "api" || r.Kind != "http.request" {
			t.Fatalf("第 %d 列 channel/kind = %q/%q", i, r.Channel, r.Kind)
		}
		if r.RequestID == nil || len(*r.RequestID) != 26 {
			t.Fatalf("第 %d 列 request_id 應是 26 字元 ULID,got %v", i, r.RequestID)
		}
		if r.LatencyMs == nil || *r.LatencyMs < 0 {
			t.Fatalf("第 %d 列 latency_ms = %v", i, r.LatencyMs)
		}
	}

	// 逐列語意。寫入是非同步且各 server 有自己的 worker,落地順序不等於呼叫順序,
	// 所以用 (status, error_code) 指名比對,不靠索引。
	ok := pick(t, rows, "ok", nil)
	assertUser(t, ok, &testUser)
	// JSONB 會重排欄位順序,所以比對語意而不是字串。
	assertJSON(t, ok.Response, map[string]any{"amount": float64(150), "streak": float64(5)})

	claimed := pick(t, rows, "error", strPtr("already_exists"))
	assertUser(t, claimed, &testUser)

	denied := pick(t, rows, "denied", strPtr("unauthenticated"))
	assertUser(t, denied, nil)

	panicked := pick(t, rows, "error", strPtr("internal"))
	assertUser(t, panicked, &testUser)
	assertJSON(t, panicked.Response, map[string]any{"panic": true})

	// request/response 是白名單摘要:token 絕不能出現在任何一欄。
	for i, r := range rows {
		blob := string(r.Request) + string(r.Response)
		if strings.Contains(blob, secretToken) {
			t.Fatalf("第 %d 列的摘要含 token:%s", i, blob)
		}
		if strings.Contains(strings.ToLower(blob), "token") ||
			strings.Contains(strings.ToLower(blob), "authorization") {
			t.Fatalf("第 %d 列的摘要疑似含憑證欄位:%s", i, blob)
		}
	}
}

// AuthService 的請求與回應全是憑證,摘要必須是 NULL(不是「空物件」)。
func TestAuthServiceLeavesNoPayload(t *testing.T) {
	setup(t)
	srv := newHandler(t, stubDaily{})
	client := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	_, err := client.CompleteDiscordLogin(context.Background(),
		connect.NewRequest(&platformv1.CompleteDiscordLoginRequest{
			Code: "oauth-code-不該入庫", State: "state-不該入庫",
		}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("AuthService 未注入應回 unimplemented,got %v", err)
	}
	rows := waitRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("期望 1 列,got %d", len(rows))
	}
	if rows[0].Request != nil {
		t.Fatalf("AuthService 的請求摘要必須是 NULL,got %s", rows[0].Request)
	}
	if s := string(rows[0].Response); strings.Contains(s, "oauth-code") || strings.Contains(s, "state") {
		t.Fatalf("回應摘要洩漏憑證:%s", s)
	}
	if rows[0].UserID != nil {
		t.Fatalf("未登入的請求不該有 user_id:%v", *rows[0].UserID)
	}
}

// 稽核寫入失敗不能影響請求(關掉 DB 連線模擬)。
func TestBrokenSinkDoesNotBreakRequest(t *testing.T) {
	setup(t)
	broken, err := brokenPool()
	if err != nil {
		t.Fatalf("建立壞連線池:%v", err)
	}
	defer broken.Close()

	h, err := transport.New(transport.Deps{
		Daily:         stubDaily{},
		Authenticator: stubAuth{},
		EventLog:      eventlogpg.New(broken),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	start := time.Now()
	if err := claim(t, srv, true); err != nil {
		t.Fatalf("稽核寫不進去也不該讓請求失敗:%v", err)
	}
	// 而且不能拖慢:寫入在背景,DB 不可達時請求延遲不受影響。
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("稽核 DB 不可達時請求跟著慢了:%v", elapsed)
	}
	if rows := selectRows(t); len(rows) != 0 {
		t.Fatalf("壞的 sink 不該寫進真表,got %d 列", len(rows))
	}
}

// assertJSON 比對 JSONB 欄位的語意內容。
func assertJSON(t *testing.T, raw []byte, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("不是合法 JSON(%s): %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("摘要 = %v, 期望 %v", got, want)
	}
}

// pick 找出唯一一列符合 (status, errorCode) 的紀錄;找不到或多於一列都是錯。
func pick(t *testing.T, rows []row, status string, code *string) row {
	t.Helper()
	var found []row
	for _, r := range rows {
		if r.Status == nil || *r.Status != status {
			continue
		}
		switch {
		case code == nil && r.ErrorCode != nil:
			continue
		case code != nil && (r.ErrorCode == nil || *r.ErrorCode != *code):
			continue
		}
		found = append(found, r)
	}
	if len(found) != 1 {
		t.Fatalf("期望剛好一列 status=%q code=%v,got %d 列", status, code, len(found))
	}
	return found[0]
}

func assertUser(t *testing.T, r row, userID *int64) {
	t.Helper()
	switch {
	case userID == nil && r.UserID != nil:
		t.Fatalf("不該有 user_id:%v", *r.UserID)
	case userID != nil && (r.UserID == nil || *r.UserID != *userID):
		t.Fatalf("user_id = %v, 期望 %v", r.UserID, *userID)
	}
}

func strPtr(s string) *string { return &s }

// brokenPool 指向一個不存在的位址:pgxpool 是延遲連線,所以建立時不會失敗,
// 每次寫入才會失敗——正好模擬「稽核 DB 掛掉」。
func brokenPool() (*pgxpool.Pool, error) {
	return pgxpool.New(context.Background(),
		"postgres://nobody:nobody@127.0.0.1:1/nonexistent?connect_timeout=1")
}

// connect 在攔截器之前解碼,壞 JSON 原本完全無稽核。
// 這裡驗最外層 middleware 的補記真的有進到 Postgres。
func TestMalformedBodyIsAuditedToDB(t *testing.T) {
	setup(t)
	srv := newHandler(t, stubDaily{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+platformv1connect.DailyServiceClaimProcedure,
		strings.NewReader("{壞掉的 JSON"))
	if err != nil {
		t.Fatalf("建請求:%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("送請求:%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 400 {
		t.Fatalf("壞 JSON 應該失敗,got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("失敗的回應也要帶 X-Request-Id")
	}
	r := waitRows(t, 1)[0]
	if r.Action != platformv1connect.DailyServiceClaimProcedure {
		t.Fatalf("action = %q", r.Action)
	}
	if r.Status == nil || *r.Status != "error" {
		t.Fatalf("status = %v", r.Status)
	}
	if r.Request != nil {
		t.Fatalf("解不出來的請求不該有摘要:%s", r.Request)
	}
	if r.RequestID == nil || *r.RequestID != resp.Header.Get("X-Request-Id") {
		t.Fatalf("request_id 要與回應 header 一致才對得起帳:%v", r.RequestID)
	}
}
