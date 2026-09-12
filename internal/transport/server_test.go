package transport_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/transport"
)

// 入口層測試不需要 Postgres:要驗的是「協定 ↔ core 介面」的接線
// (錯誤映射、認證、panic 復原、稽核欄位),不是資料庫行為。
// 真資料庫的部分在 storage/eventlogpg 的整合測試。

const testToken = "s3cret-access-token"

const testUserID int64 = 42

// ── 假的 core 實作 ────────────────────────────────────────────────

type fakeDaily struct {
	res      *daily.Result
	err      error
	panicNow bool
}

func (f *fakeDaily) Claim(context.Context, int64) (*daily.Result, error) {
	if f.panicNow {
		panic("測試用 panic")
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

type fakeShop struct {
	purchase *shop.PurchaseResult
	handle   *shop.HandleResult
	err      error
}

func (f *fakeShop) Purchase(context.Context, shop.PurchaseParams) (*shop.PurchaseResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.purchase, nil
}

func (f *fakeShop) RefundPurchase(context.Context, shop.RefundParams) (*shop.RefundResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &shop.RefundResult{Amount: 100, Currency: "coin"}, nil
}

func (f *fakeShop) HandleRedemption(context.Context, shop.HandleParams) (*shop.HandleResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.handle, nil
}

func (f *fakeShop) CancelRedemption(context.Context, shop.CancelParams) (*shop.HandleResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.handle, nil
}

type fakeAdmin struct {
	res *adminecon.Result
	err error
}

func (f *fakeAdmin) Grant(context.Context, adminecon.AdjustParams) (*adminecon.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func (f *fakeAdmin) Deduct(context.Context, adminecon.AdjustParams) (*adminecon.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func (f *fakeAdmin) Refund(context.Context, adminecon.RefundParams) (*adminecon.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

type fakeLedger struct {
	balance int64
	err     error
}

func (f *fakeLedger) Apply(context.Context, ledger.ApplyParams) (*ledger.ApplyResult, error) {
	return nil, errors.New("測試不該經由 transport 直接動帳本")
}
func (f *fakeLedger) Balance(context.Context, int64, string) (int64, error) {
	return f.balance, f.err
}
func (f *fakeLedger) Reconcile(context.Context) ([]ledger.Mismatch, error) { return nil, nil }

// ── 假的入口層 port ──────────────────────────────────────────────

type fakeDirectory struct{ err error }

func (f fakeDirectory) UserID(_ context.Context, publicID string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if publicID == "missing" {
		return 0, transport.ErrNotFound
	}
	return 7, nil
}
func (f fakeDirectory) UserPublicID(context.Context, int64) (string, error) { return "U-PUBLIC", nil }
func (f fakeDirectory) EntitlementID(_ context.Context, publicID string) (int64, error) {
	if publicID == "missing" {
		return 0, transport.ErrNotFound
	}
	return 11, nil
}
func (f fakeDirectory) RedemptionID(_ context.Context, publicID string) (int64, error) {
	if publicID == "missing" {
		return 0, transport.ErrNotFound
	}
	return 13, nil
}

type fakeCatalog struct{ items []readmodel.ItemView }

func (f fakeCatalog) Items(context.Context, bool) ([]readmodel.ItemView, error) {
	return f.items, nil
}
func (f fakeCatalog) Entitlements(context.Context, int64, bool) ([]readmodel.EntitlementView, error) {
	return nil, nil
}
func (f fakeCatalog) Redemptions(context.Context, int64, shop.RedemptionStatus) ([]readmodel.RedemptionView, error) {
	return nil, nil
}

type fakeProfiles struct {
	view     *readmodel.ProfileView
	balances []readmodel.BalanceView
}

func (f fakeProfiles) Profile(context.Context, int64) (*readmodel.ProfileView, error) {
	return f.view, nil
}
func (f fakeProfiles) Balances(context.Context, int64) ([]readmodel.BalanceView, error) {
	return f.balances, nil
}
func (f fakeProfiles) SetTimezone(_ context.Context, _ int64, tz string) (*readmodel.ProfileView, error) {
	return &readmodel.ProfileView{PublicID: "U1", Timezone: tz}, nil
}
func (f fakeProfiles) Summary(context.Context, int64) (*readmodel.SummaryView, error) {
	if f.view == nil {
		return &readmodel.SummaryView{}, nil
	}
	return &readmodel.SummaryView{Profile: *f.view, Balances: f.balances}, nil
}
func (f fakeProfiles) Leaderboard(context.Context, string, int32) ([]readmodel.LeaderboardEntry, error) {
	return nil, nil
}

type fakeEntries struct {
	page    *readmodel.LedgerEntriesPage
	lastQry readmodel.LedgerEntriesQuery
	mu      sync.Mutex
}

func (f *fakeEntries) ListEntries(_ context.Context, q readmodel.LedgerEntriesQuery) (*readmodel.LedgerEntriesPage, error) {
	f.mu.Lock()
	f.lastQry = q
	f.mu.Unlock()
	return f.page, nil
}

func (f *fakeEntries) query() readmodel.LedgerEntriesQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQry
}

// fakeAuth 只認一把 token,其餘一律拒絕。
type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, token string) (int64, error) {
	if token != testToken {
		return 0, transport.ErrUnauthenticated
	}
	return testUserID, nil
}

type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, int64, string) error { return nil }

type denyAuthorizer struct{}

func (denyAuthorizer) Authorize(context.Context, int64, string) error {
	return transport.ErrPermissionDenied
}

// recordSink 把 event_logs 收在記憶體裡(DB 版本的測試在 eventlogpg)。
type recordSink struct {
	mu    sync.Mutex
	recs  []eventlog.Record
	err   error
	block chan struct{} // 非 nil 時每次寫入都卡在這裡,模擬 DB 不可達
}

func (s *recordSink) LogEvent(_ context.Context, rec eventlog.Record) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return s.err
}

func (s *recordSink) all() []eventlog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]eventlog.Record(nil), s.recs...)
}

// ── 測試骨架 ────────────────────────────────────────────────────

func newServer(t *testing.T, deps transport.Deps) (*httptest.Server, *recordSink) {
	t.Helper()
	sink := &recordSink{}
	if deps.EventLog == nil {
		deps.EventLog = sink
	}
	if deps.Authenticator == nil {
		deps.Authenticator = fakeAuth{}
	}
	if deps.Logger == nil {
		// panic 測試會印堆疊,測試輸出不需要看到。
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h, err := transport.New(deps)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})
	return srv, sink
}

// waitRecords 等背景 worker 把稽核寫進 sink。
// 稽核是非同步的(刻意:不讓 DB 拖慢請求),所以斷言前要等,不能直接讀。
func waitRecords(t *testing.T, sink *recordSink, n int) []eventlog.Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		recs := sink.all()
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 %d 列 event_log,只收到 %d 列", n, len(recs))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// withToken 把合法的 Bearer 塞進請求。
func withToken[T any](req *connect.Request[T]) *connect.Request[T] {
	req.Header().Set("Authorization", "Bearer "+testToken)
	return req
}

func codeOf(err error) connect.Code { return connect.CodeOf(err) }

// postRaw 直接送 HTTP,用來測攔截器**看不到**的層(解碼、請求體上限)。
// 回傳狀態碼與 header;body 當場讀完關掉,呼叫端不必再收尾。
func postRaw(t *testing.T, srv *httptest.Server, path, contentType string, body []byte, headers ...[2]string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("建請求:%v", err)
	}
	req.Header.Set("Content-Type", contentType)
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("送請求:%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("讀回應:%v", err)
	}
	return resp.StatusCode, resp.Header
}

// ── 錯誤映射(走完整 HTTP 往返)────────────────────────────────

func TestDailyClaim_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"今天已簽到", daily.ErrAlreadyClaimed, connect.CodeAlreadyExists},
		{"改時區冷卻", daily.ErrTimezoneChangeCooldown, connect.CodeFailedPrecondition},
		{"帳本狀態矛盾", daily.ErrLedgerStateConflict, connect.CodeInternal},
		{"設定不合法", daily.ErrInvalidConfig, connect.CodeInternal},
		{"未映射的實作錯誤", errors.New("pq: 連線中斷"), connect.CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, sink := newServer(t, transport.Deps{Daily: &fakeDaily{err: c.err}})
			client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
			_, err := client.Claim(context.Background(), withToken(connect.NewRequest(&platformv1.ClaimRequest{})))
			if got := codeOf(err); got != c.want {
				t.Fatalf("code = %v, 期望 %v(err=%v)", got, c.want, err)
			}
			recs := waitRecords(t, sink, 1)
			if len(recs) != 1 {
				t.Fatalf("期望 1 列 event_log,got %d", len(recs))
			}
			if recs[0].Status != eventlog.StatusError {
				t.Fatalf("status = %q", recs[0].Status)
			}
			if recs[0].ErrorCode != c.want.String() {
				t.Fatalf("error_code = %q, 期望 %q", recs[0].ErrorCode, c.want.String())
			}
		})
	}
}

func TestDailyClaim_OK(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		Daily: &fakeDaily{res: &daily.Result{Amount: 120, Streak: 3}},
	})
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	res, err := client.Claim(context.Background(), withToken(connect.NewRequest(&platformv1.ClaimRequest{})))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if res.Msg.GetAmount() != 120 || res.Msg.GetStreak() != 3 {
		t.Fatalf("回應不對:%v", res.Msg)
	}
	if res.Header().Get("X-Request-Id") == "" {
		t.Fatal("回應應帶 X-Request-Id")
	}
	recs := waitRecords(t, sink, 1)
	rec := recs[0]
	if rec.Status != eventlog.StatusOK || rec.ErrorCode != "" {
		t.Fatalf("status/error_code = %q/%q", rec.Status, rec.ErrorCode)
	}
	if rec.Channel != eventlog.ChannelAPI || rec.Kind != eventlog.KindHTTPRequest {
		t.Fatalf("Connect 協定應記 api/http.request,got %q/%q", rec.Channel, rec.Kind)
	}
	if rec.Action != platformv1connect.DailyServiceClaimProcedure {
		t.Fatalf("action = %q", rec.Action)
	}
	if len(rec.RequestID) != 26 {
		t.Fatalf("request_id 應是 26 字元 ULID,got %q", rec.RequestID)
	}
	if rec.RequestID != res.Header().Get("X-Request-Id") {
		t.Fatal("回應 header 的 request_id 必須與稽核那列一致(否則對帳不起來)")
	}
	if rec.UserID == nil || *rec.UserID != testUserID {
		t.Fatalf("user_id = %v", rec.UserID)
	}
	if string(rec.Response) != `{"amount":120,"streak":3}` {
		t.Fatalf("response 摘要 = %s", rec.Response)
	}
}

func TestShopPurchase_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"餘額不足", ledger.ErrInsufficientBalance, connect.CodeFailedPrecondition},
		{"冪等鍵衝突", ledger.ErrIdempotencyConflict, connect.CodeAborted},
		{"同鍵處理中", ledger.ErrInFlight, connect.CodeUnavailable},
		{"商品不存在", shop.ErrItemNotFound, connect.CodeNotFound},
		{"未上架", shop.ErrItemNotListed, connect.CodeFailedPrecondition},
		{"已達限購", shop.ErrPerUserLimitReached, connect.CodeFailedPrecondition},
		{"參數不合法", shop.ErrInvalidRequest, connect.CodeInvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newServer(t, transport.Deps{Shop: &fakeShop{err: c.err}, Directory: fakeDirectory{}})
			client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
			_, err := client.Purchase(context.Background(), withToken(connect.NewRequest(&platformv1.PurchaseRequest{
				ItemPublicId: "ITEM-1", IdempotencyKey: "key-1",
			})))
			if got := codeOf(err); got != c.want {
				t.Fatalf("code = %v, 期望 %v(err=%v)", got, c.want, err)
			}
		})
	}
}

// 動錢的 RPC 少了冪等鍵一律擋在入口(鐵則),不會進到 core。
func TestShopPurchase_IdempotencyKeyRequired(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Shop: &fakeShop{}, Directory: fakeDirectory{}})
	client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	_, err := client.Purchase(context.Background(), withToken(connect.NewRequest(&platformv1.PurchaseRequest{
		ItemPublicId: "ITEM-1",
	})))
	if got := codeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, 期望 invalid_argument", got)
	}
	_, err = client.RefundPurchase(context.Background(), withToken(connect.NewRequest(&platformv1.RefundPurchaseRequest{
		EntitlementPublicId: "ENT-1",
	})))
	if got := codeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("退款 code = %v, 期望 invalid_argument", got)
	}
}

// public_id 查無此物 → NotFound,不會拿著 0 當內部 id 往下打。
func TestShopRefund_UnknownPublicID(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Shop: &fakeShop{}, Directory: fakeDirectory{}})
	client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	_, err := client.RefundPurchase(context.Background(), withToken(connect.NewRequest(&platformv1.RefundPurchaseRequest{
		EntitlementPublicId: "missing", IdempotencyKey: "k",
	})))
	if got := codeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, 期望 not_found", got)
	}
}

func TestAdminEconomy_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"沒填理由", adminecon.ErrReasonRequired, connect.CodeInvalidArgument},
		{"參數不合法", adminecon.ErrInvalidInput, connect.CodeInvalidArgument},
		{"找不到分錄", adminecon.ErrEntryNotFound, connect.CodeNotFound},
		{"不可退款", adminecon.ErrNotRefundable, connect.CodeFailedPrecondition},
		{"已退過款", adminecon.ErrAlreadyRefunded, connect.CodeAlreadyExists},
		{"餘額不足", ledger.ErrInsufficientBalance, connect.CodeFailedPrecondition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newServer(t, transport.Deps{
				AdminEcon: &fakeAdmin{err: c.err}, Directory: fakeDirectory{},
				Authorizer: allowAuthorizer{},
			})
			client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
			_, err := client.Grant(context.Background(), withToken(connect.NewRequest(&platformv1.GrantRequest{
				UserPublicId: "U1", Currency: "coin", Amount: 10,
				Reason: "測試", IdempotencyKey: "k1",
			})))
			if got := codeOf(err); got != c.want {
				t.Fatalf("code = %v, 期望 %v(err=%v)", got, c.want, err)
			}
		})
	}
}

// port 實作回的 connect.Error 不可信:code 保留,訊息換成安全文案,
// 內部細節不進回應、也不進稽核。
func TestUntrustedConnectErrorIsSanitized(t *testing.T) {
	const leak = "pq: SELECT * FROM platform.users WHERE id=7"
	srv, sink := newServer(t, transport.Deps{
		AdminEcon:  &fakeAdmin{res: &adminecon.Result{}},
		Directory:  fakeDirectory{err: connect.NewError(connect.CodeNotFound, errors.New(leak))},
		Authorizer: allowAuthorizer{},
	})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	_, err := client.Grant(context.Background(), withToken(connect.NewRequest(&platformv1.GrantRequest{
		UserPublicId: "U1", Currency: "coin", Amount: 1, Reason: "r", IdempotencyKey: "k",
	})))
	if got := codeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code 應保留 not_found,got %v", got)
	}
	if strings.Contains(err.Error(), leak) {
		t.Fatalf("外部 connect.Error 的訊息外洩了:%v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if strings.Contains(string(rec.Response), "platform.users") {
		t.Fatalf("稽核摘要外洩內部細節:%s", rec.Response)
	}
}

// ── 認證 / 授權 ────────────────────────────────────────────────

func TestProtectedProcedures_RequireBearer(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		Daily: &fakeDaily{res: &daily.Result{}}, Shop: &fakeShop{},
		Ledger: &fakeLedger{}, Catalog: fakeCatalog{}, Directory: fakeDirectory{},
	})
	ctx := context.Background()
	dailyClient := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	if _, err := dailyClient.Claim(ctx, connect.NewRequest(&platformv1.ClaimRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Claim 沒帶 token 應回 unauthenticated,got %v", err)
	}
	me := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	if _, err := me.GetBalance(ctx, connect.NewRequest(&platformv1.GetBalanceRequest{Currency: "coin"})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("GetBalance 沒帶 token 應回 unauthenticated,got %v", err)
	}
	shopClient := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	if _, err := shopClient.Purchase(ctx, connect.NewRequest(&platformv1.PurchaseRequest{
		ItemPublicId: "ITEM-1", IdempotencyKey: "k",
	})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Purchase 沒帶 token 應回 unauthenticated,got %v", err)
	}
	// 無效 token 一樣是 unauthenticated。
	req := connect.NewRequest(&platformv1.ClaimRequest{})
	req.Header().Set("Authorization", "Bearer 偽造的")
	if _, err := dailyClient.Claim(ctx, req); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("偽造 token 應回 unauthenticated,got %v", err)
	}

	// 被擋下的請求也要留紀錄(schemas/13:denied 要記),且不帶 user_id。
	recs := waitRecords(t, sink, 4)
	if len(recs) != 4 {
		t.Fatalf("四次被擋的請求應留 4 列,got %d", len(recs))
	}
	for _, rec := range recs {
		if rec.Status != eventlog.StatusDenied {
			t.Fatalf("status = %q, 期望 denied", rec.Status)
		}
		if rec.UserID != nil {
			t.Fatalf("未認證不該有 user_id,got %v", *rec.UserID)
		}
	}
}

// 公開型錄不需要登入(否則沒登入的人看不到任何東西)。
func TestListItems_IsPublic(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		Catalog: fakeCatalog{items: []readmodel.ItemView{{
			PublicID: "ITEM-1", Name: "測試商品",
			Fulfillment: shop.FulfillAutoRole, Currency: "coin", Price: 500,
		}}},
	})
	client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	res, err := client.ListItems(context.Background(), connect.NewRequest(&platformv1.ListItemsRequest{}))
	if err != nil {
		t.Fatalf("ListItems 應該公開,got %v", err)
	}
	if len(res.Msg.GetItems()) != 1 || res.Msg.GetItems()[0].GetPublicId() != "ITEM-1" {
		t.Fatalf("回應不對:%v", res.Msg)
	}
	if got := res.Msg.GetItems()[0].GetFulfillment(); got != platformv1.Fulfillment_FULFILLMENT_AUTO_ROLE {
		t.Fatalf("fulfillment = %v", got)
	}
}

// 沒注入 Authorizer 時管理端一律拒絕:授權沒接上就把管理端開著,
// 比「功能還沒上線」嚴重得多。
func TestAdminProcedures_FailClosedWithoutAuthorizer(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		AdminEcon: &fakeAdmin{res: &adminecon.Result{}}, Directory: fakeDirectory{},
	})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	_, err := client.Grant(context.Background(), withToken(connect.NewRequest(&platformv1.GrantRequest{
		UserPublicId: "U1", Currency: "coin", Amount: 10, Reason: "r", IdempotencyKey: "k",
	})))
	if got := codeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v, 期望 permission_denied", got)
	}
	recs := waitRecords(t, sink, 1)
	if recs[0].Status != eventlog.StatusDenied {
		t.Fatalf("被擋的管理操作應是 denied,got %+v", recs[0])
	}
	// 提權嘗試正是最需要知道行為人的那一列:身分已認出來就必須記下去。
	if recs[0].UserID == nil || *recs[0].UserID != testUserID {
		t.Fatalf("被授權擋下的那列必須有 user_id,got %v", recs[0].UserID)
	}
}

// Authorizer 明確拒絕時同樣要記得是誰。
func TestAdminProcedures_DeniedByAuthorizerKeepsUserID(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		AdminEcon: &fakeAdmin{res: &adminecon.Result{}}, Directory: fakeDirectory{},
		Authorizer: denyAuthorizer{},
	})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	_, err := client.Deduct(context.Background(), withToken(connect.NewRequest(&platformv1.DeductRequest{
		UserPublicId: "U1", Currency: "coin", Amount: 10, Reason: "r", IdempotencyKey: "k",
	})))
	if got := codeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v, 期望 permission_denied", got)
	}
	recs := waitRecords(t, sink, 1)
	if recs[0].UserID == nil || *recs[0].UserID != testUserID {
		t.Fatalf("被授權擋下的那列必須有 user_id,got %v", recs[0].UserID)
	}
}

func TestAdminGrant_OK(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{
		AdminEcon: &fakeAdmin{res: &adminecon.Result{
			EntryID: 99, UserID: 7, Currency: "coin", Amount: 500, NewBalance: 1500,
		}},
		Directory: fakeDirectory{}, Authorizer: allowAuthorizer{},
	})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	res, err := client.Grant(context.Background(), withToken(connect.NewRequest(&platformv1.GrantRequest{
		UserPublicId: "U1", Currency: "coin", Amount: 500,
		Reason: "補償停機", IdempotencyKey: "k1",
	})))
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	adj := res.Msg.GetAdjustment()
	if adj.GetUserPublicId() != "U1" || adj.GetAmount() != 500 || adj.GetNewBalance() != 1500 {
		t.Fatalf("回應不對:%v", adj)
	}
	// 稽核摘要不含 reason(權威在 admin_audit_logs),也不含任何內部 id。
	req := string(waitRecords(t, sink, 1)[0].Request)
	if want := `{"amount":500,"currency":"coin","idempotency_key":"k1","user_public_id":"U1"}`; req != want {
		t.Fatalf("request 摘要 = %s", req)
	}
}

// ── ListEntries(Refund 的前置)────────────────────────────────

func TestListEntries(t *testing.T) {
	entries := &fakeEntries{page: &readmodel.LedgerEntriesPage{
		Entries: []readmodel.LedgerEntryView{{
			EntryRef: "1024", UserPublicID: "U1", Currency: "coin",
			Amount: -500, Reason: "shop_purchase", CreatedAt: time.Now(), Refunded: false,
		}},
		NextPageToken: "cursor-2",
	}}
	srv, _ := newServer(t, transport.Deps{
		LedgerEntries: entries, Directory: fakeDirectory{}, Authorizer: allowAuthorizer{},
	})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	res, err := client.ListEntries(context.Background(), withToken(connect.NewRequest(&platformv1.ListEntriesRequest{
		UserPublicId: "U1", PageSize: 5000,
	})))
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(res.Msg.GetEntries()) != 1 || res.Msg.GetEntries()[0].GetEntryRef() != "1024" {
		t.Fatalf("回應不對:%v", res.Msg)
	}
	if res.Msg.GetNextPageToken() != "cursor-2" {
		t.Fatalf("next_page_token = %q", res.Msg.GetNextPageToken())
	}
	// page_size 必須夾在上限:一次撈爆分區表會拖垮整個 DB,不只那個請求。
	if got := entries.query().PageSize; got != 200 {
		t.Fatalf("page_size 應夾到 200,got %d", got)
	}
	if got := entries.query().UserID; got != 7 {
		t.Fatalf("public_id 應已解成內部 id,got %d", got)
	}
}

func TestListEntries_RequiresAuthorization(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{
		LedgerEntries: &fakeEntries{page: &readmodel.LedgerEntriesPage{}},
		Directory:     fakeDirectory{},
	})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	_, err := client.ListEntries(context.Background(), withToken(connect.NewRequest(&platformv1.ListEntriesRequest{
		UserPublicId: "U1",
	})))
	if got := codeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("沒有 Authorizer 時管理端應拒絕,got %v", got)
	}
}

// ── panic 復原 ────────────────────────────────────────────────

func TestPanicRecoveredAsInternal(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{Daily: &fakeDaily{panicNow: true}})
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	_, err := client.Claim(context.Background(), withToken(connect.NewRequest(&platformv1.ClaimRequest{})))
	if got := codeOf(err); got != connect.CodeInternal {
		t.Fatalf("code = %v, 期望 internal", got)
	}
	recs := waitRecords(t, sink, 1)
	if len(recs) != 1 {
		t.Fatalf("panic 也要留一列(且只有一列),got %d", len(recs))
	}
	if recs[0].Status != eventlog.StatusError || recs[0].ErrorCode != connect.CodeInternal.String() {
		t.Fatalf("panic 那列 = %+v", recs[0])
	}
	if string(recs[0].Response) != `{"panic":true}` {
		t.Fatalf("panic 摘要 = %s", recs[0].Response)
	}
	if recs[0].UserID == nil || *recs[0].UserID != testUserID {
		t.Fatal("panic 那列也要記得是誰打的")
	}
	if recs[0].RequestID == "" {
		t.Fatal("panic 那列必須有 request_id,否則 log 裡的堆疊關聯不到")
	}
}

// ── 稽核不得拖垮請求 ──────────────────────────────────────────

// sink 卡死(DB 不可達)時,請求延遲**不受影響**。
// 舊版同步寫入在這個情境下每個請求要多等一個 timeout,等同服務中斷。
func TestBlockedSinkDoesNotSlowRequests(t *testing.T) {
	block := make(chan struct{})
	sink := &recordSink{block: block}
	srv, _ := newServer(t, transport.Deps{
		Daily:    &fakeDaily{res: &daily.Result{Amount: 10, Streak: 1}},
		EventLog: sink,
	})
	defer close(block) // 收工時放行,worker 才能結束

	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := client.Claim(context.Background(),
			withToken(connect.NewRequest(&platformv1.ClaimRequest{}))); err != nil {
			t.Fatalf("第 %d 次 Claim:%v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("稽核卡住時請求也跟著慢了:5 次共 %v", elapsed)
	}
}

// 稽核寫入失敗不能影響請求結果——稽核是旁路。
func TestEventLogFailureDoesNotBreakRequest(t *testing.T) {
	sink := &recordSink{err: errors.New("DB 掛了")}
	srv, _ := newServer(t, transport.Deps{
		Daily:    &fakeDaily{res: &daily.Result{Amount: 10, Streak: 1}},
		EventLog: sink,
	})
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	res, err := client.Claim(context.Background(), withToken(connect.NewRequest(&platformv1.ClaimRequest{})))
	if err != nil {
		t.Fatalf("稽核失敗不該讓請求失敗:%v", err)
	}
	if res.Msg.GetAmount() != 10 {
		t.Fatalf("回應不對:%v", res.Msg)
	}
}

// ── 摘要的體積上限 ────────────────────────────────────────────

// client 可控字串必須有上限。沒有的話,任一有效 session 都能用
// event_logs 灌爆平台唯一的 DB(而且保留 180 天)。
func TestOversizedFieldIsTruncatedInAudit(t *testing.T) {
	huge := strings.Repeat("A", 100_000)
	srv, sink := newServer(t, transport.Deps{Shop: &fakeShop{
		purchase: &shop.PurchaseResult{ItemPublicID: "ITEM-1", Currency: "coin"},
	}, Directory: fakeDirectory{}})
	client := platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	if _, err := client.Purchase(context.Background(), withToken(connect.NewRequest(&platformv1.PurchaseRequest{
		ItemPublicId: "ITEM-1", IdempotencyKey: huge,
	}))); err != nil {
		t.Fatalf("Purchase: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if len(rec.Request) > 1024 {
		t.Fatalf("request 摘要沒有上限:%d bytes", len(rec.Request))
	}
	if !strings.Contains(string(rec.Request), `"truncated":true`) {
		t.Fatalf("截斷了就要標記出來:%s", rec.Request)
	}
}

// 即使 RPC 最後回 unimplemented,摘要仍會落地——所以上限必須在摘要層,
// 不能靠「handler 會拒絕」來保護。
func TestOversizedFieldTruncatedEvenWhenUnimplemented(t *testing.T) {
	huge := strings.Repeat("B", 100_000)
	srv, sink := newServer(t, transport.Deps{Directory: fakeDirectory{}, Authorizer: allowAuthorizer{}})
	client := platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	_, err := client.ListEntries(context.Background(), withToken(connect.NewRequest(&platformv1.ListEntriesRequest{
		UserPublicId: huge,
	})))
	if got := codeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("code = %v", got)
	}
	rec := waitRecords(t, sink, 1)[0]
	if len(rec.Request) > 1024 {
		t.Fatalf("unimplemented 的請求摘要也必須有上限:%d bytes", len(rec.Request))
	}
}

// 使用者自由文字(時區)不進稽核:權威在 users 表,這裡不重複也不給灌水入口。
func TestFreeTextNotAudited(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{ProfileWrites: fakeProfiles{}})
	client := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	if _, err := client.UpdateTimezone(context.Background(), withToken(connect.NewRequest(&platformv1.UpdateTimezoneRequest{
		Timezone: "Asia/Taipei-不該入庫",
	}))); err != nil {
		t.Fatalf("UpdateTimezone: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if strings.Contains(string(rec.Request), "Taipei") {
		t.Fatalf("時區字串不該進稽核:%s", rec.Request)
	}
}

// ── 沒進到 RPC 層的請求 ──────────────────────────────────────

// connect 在攔截器**之前**解碼,所以壞 JSON 原本完全無稽核。
// 最外層的 HTTP middleware 補記——被人試探時最該留痕的就是這種流量。
func TestMalformedBodyIsAudited(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{Daily: &fakeDaily{res: &daily.Result{}}})
	status, header := postRaw(t, srv, platformv1connect.DailyServiceClaimProcedure,
		"application/json", []byte("{這不是 JSON"))
	if status < 400 {
		t.Fatalf("壞 JSON 應該失敗,got %d", status)
	}
	if header.Get("X-Request-Id") == "" {
		t.Fatal("失敗的回應也要帶 X-Request-Id(最需要對帳的就是它們)")
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Action != platformv1connect.DailyServiceClaimProcedure {
		t.Fatalf("action = %q", rec.Action)
	}
	if rec.Status != eventlog.StatusError || rec.RequestID == "" {
		t.Fatalf("補記那列不完整:%+v", rec)
	}
	if rec.Request != nil {
		t.Fatalf("解不出來的請求不該有摘要:%s", rec.Request)
	}
}

// 請求體上限:沒有上限等於任何人都能用一個請求吃掉記憶體。
//
// body 刻意是**合法且語意正確**的 ClaimRequest(`{}` 後面接一堆空白):
// 這樣「被拒」只可能來自大小上限,不會被解碼錯誤矇混過去。
func TestOversizedRequestBodyRejectedAndAudited(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{Daily: &fakeDaily{res: &daily.Result{}}})
	body := append([]byte(`{}`), bytes.Repeat([]byte(" "), 2<<20)...)
	status, _ := postRaw(t, srv, platformv1connect.DailyServiceClaimProcedure, "application/json", body,
		[2]string{"Authorization", "Bearer " + testToken})
	if status < 400 {
		t.Fatalf("超大請求體應該被拒,got %d", status)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Action != platformv1connect.DailyServiceClaimProcedure {
		t.Fatalf("action = %q", rec.Action)
	}
	if rec.Status == eventlog.StatusOK {
		t.Fatalf("被拒的請求不該記成 ok:%+v", rec)
	}
}

// 不存在的路徑也要留痕(掃描行為看得見)。
func TestUnknownPathIsAudited(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{})
	status, _ := postRaw(t, srv, "/hestia.platform.v1.NoSuchService/Whatever", "application/json", []byte("{}"))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Action != "/hestia.platform.v1.NoSuchService/Whatever" {
		t.Fatalf("action = %q", rec.Action)
	}
}

// ── 未注入實作 ────────────────────────────────────────────────

func TestUnimplementedServices(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{})
	ctx := context.Background()
	auth := platformv1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	if _, err := auth.StartDiscordLogin(ctx, connect.NewRequest(&platformv1.StartDiscordLoginRequest{})); codeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("AuthService 未注入應回 unimplemented,got %v", err)
	}
	if _, err := auth.RefreshSession(ctx, connect.NewRequest(&platformv1.RefreshSessionRequest{RefreshToken: "x"})); codeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("RefreshSession 未注入應回 unimplemented,got %v", err)
	}
	me := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	if _, err := me.GetProfile(ctx, withToken(connect.NewRequest(&platformv1.GetProfileRequest{}))); codeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("MeService.GetProfile 未注入應回 unimplemented,got %v", err)
	}
	if _, err := me.ListBalances(ctx, withToken(connect.NewRequest(&platformv1.ListBalancesRequest{}))); codeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("MeService.ListBalances 未注入應回 unimplemented,got %v", err)
	}
}

func TestListBalances(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{Profiles: fakeProfiles{
		balances: []readmodel.BalanceView{{Currency: "coin", Amount: 1200}, {Currency: "gem", Amount: 3}},
	}})
	client := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	res, err := client.ListBalances(context.Background(), withToken(connect.NewRequest(&platformv1.ListBalancesRequest{})))
	if err != nil {
		t.Fatalf("ListBalances: %v", err)
	}
	if len(res.Msg.GetBalances()) != 2 || res.Msg.GetBalances()[0].GetAmount() != 1200 {
		t.Fatalf("回應不對:%v", res.Msg)
	}
}

// gRPC 協定的 event_logs 走 grpc / grpc.call(schemas/13 的 channel 分類)。
func TestGRPCProtocolChannel(t *testing.T) {
	srv, sink := newServer(t, transport.Deps{Daily: &fakeDaily{res: &daily.Result{Amount: 1}}})
	// httptest 是 HTTP/1.1,用 gRPC-Web(同樣歸類為 grpc)驗協定分流。
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL, connect.WithGRPCWeb())
	if _, err := client.Claim(context.Background(), withToken(connect.NewRequest(&platformv1.ClaimRequest{}))); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	rec := waitRecords(t, sink, 1)[0]
	if rec.Channel != eventlog.ChannelGRPC || rec.Kind != eventlog.KindGRPCCall {
		t.Fatalf("channel/kind = %q/%q,期望 grpc/grpc.call", rec.Channel, rec.Kind)
	}
}

// 確認 handler 真的拿得到攔截器解出的身分(不是每次都用同一個假 id)。
func TestUserIDReachesHandler(t *testing.T) {
	var seen int64
	srv, _ := newServer(t, transport.Deps{Ledger: &fakeLedger{balance: 888},
		Catalog: fakeCatalog{}, Directory: fakeDirectory{},
		Daily: dailyFunc(func(_ context.Context, userID int64) (*daily.Result, error) {
			seen = userID
			return &daily.Result{Amount: 1, Streak: 1}, nil
		})})
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	if _, err := client.Claim(context.Background(), withToken(connect.NewRequest(&platformv1.ClaimRequest{}))); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if seen != testUserID {
		t.Fatalf("handler 拿到的 user id = %d, 期望 %d", seen, testUserID)
	}
}

type dailyFunc func(context.Context, int64) (*daily.Result, error)

func (f dailyFunc) Claim(ctx context.Context, userID int64) (*daily.Result, error) {
	return f(ctx, userID)
}
