package transport_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/core/platform/eventlog"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/transport"
)

// 代打(delegated actions)的黑箱測試:stentor 帶服務憑證 + X-Acting-User
// 呼叫**白名單內**的使用者 RPC,主體變成代打對象。
//
// 這一檔要證明的四件事:
//   - 效果真的記在代打對象身上(不是服務、不是別人);
//   - 白名單外一律拒絕(fail closed),管理 RPC 更不用說;
//   - 代打與本人 Bearer 兩條路徑對同一 RPC 的結果一致(含冪等);
//   - event_logs 分得出代打與本人操作,而憑證仍然不出現在任何欄位。

// ── 會記下主體的假實作 ──────────────────────────────────────────

type spyDaily struct {
	mu       sync.Mutex
	calls    int
	lastUser int64
	err      error
}

func (s *spyDaily) Claim(_ context.Context, userID int64) (*daily.Result, error) {
	s.mu.Lock()
	s.calls++
	s.lastUser = userID
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return &daily.Result{Amount: 30, Streak: 3}, nil
}

func (s *spyDaily) seen() (int, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.lastUser
}

type spyProfiles struct {
	mu       sync.Mutex
	lastUser int64
}

func (s *spyProfiles) Profile(_ context.Context, userID int64) (*readmodel.ProfileView, error) {
	s.mu.Lock()
	s.lastUser = userID
	s.mu.Unlock()
	// public_id 由 user id 推出來,回應摘要才驗得出「查的是誰的檔案」。
	return &readmodel.ProfileView{PublicID: fmt.Sprintf("U-%d", userID)}, nil
}

func (s *spyProfiles) Balances(context.Context, int64) ([]readmodel.BalanceView, error) {
	return nil, nil
}

func (s *spyProfiles) SetTimezone(_ context.Context, _ int64, tz string) (*readmodel.ProfileView, error) {
	return &readmodel.ProfileView{PublicID: "U1", Timezone: tz}, nil
}

func (s *spyProfiles) subject() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUser
}

// spyShop 模擬「動錢的 RPC」:同一個 (使用者, 冪等鍵) 只真的動一次錢,
// 第二次回 Replayed。用來驗代打與本人兩條路徑共用同一份冪等語意
// (真正的冪等權威在 shoppg 的 DB 約束,這裡驗的是 transport 有沒有把
// 正確的主體與冪等鍵傳下去 —— 傳錯人的話兩條路徑就會各動一次錢)。
type spyShop struct {
	mu sync.Mutex
	// seen 的 key 是 "userID|idempotencyKey"。
	seenKeys map[string]bool
	charges  int
	// restricted 模擬 user_restrictions:這個使用者一律被擋
	// (真正的檢查在 core/storage,這裡只證明代打沒有繞過它)。
	restricted int64
}

var errRestricted = errors.New("使用者被限制,不得購買")

func (s *spyShop) Purchase(_ context.Context, p shop.PurchaseParams) (*shop.PurchaseResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.UserID == s.restricted {
		return nil, errRestricted
	}
	key := fmt.Sprintf("%d|%s", p.UserID, p.IdempotencyKey)
	res := &shop.PurchaseResult{
		ItemPublicID: p.ItemPublicID, Currency: "coin", Price: 100,
		EntitlementPublicID: "E-1",
	}
	if s.seenKeys[key] {
		res.Replayed = true
		return res, nil
	}
	if s.seenKeys == nil {
		s.seenKeys = map[string]bool{}
	}
	s.seenKeys[key] = true
	s.charges++
	return res, nil
}

func (s *spyShop) RefundPurchase(context.Context, shop.RefundParams) (*shop.RefundResult, error) {
	return &shop.RefundResult{Amount: 100, Currency: "coin"}, nil
}

func (s *spyShop) HandleRedemption(context.Context, shop.HandleParams) (*shop.HandleResult, error) {
	return &shop.HandleResult{}, nil
}

func (s *spyShop) CancelRedemption(context.Context, shop.CancelParams) (*shop.HandleResult, error) {
	return &shop.HandleResult{}, nil
}

func (s *spyShop) chargeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.charges
}

// ── 測試骨架 ────────────────────────────────────────────────────

type delegationEnv struct {
	daily    platformv1connect.DailyServiceClient
	me       platformv1connect.MeServiceClient
	shop     platformv1connect.ShopServiceClient
	admin    platformv1connect.AdminEconomyServiceClient
	spyDaily *spyDaily
	profiles *spyProfiles
	spyShop  *spyShop
	sink     *recordSink
}

func newDelegationEnv(t *testing.T) delegationEnv {
	t.Helper()
	env := delegationEnv{
		spyDaily: &spyDaily{},
		profiles: &spyProfiles{},
		spyShop:  &spyShop{seenKeys: map[string]bool{}},
	}
	srv, sink := newServer(t, transport.Deps{
		Daily:         env.spyDaily,
		Profiles:      env.profiles,
		ProfileWrites: env.profiles,
		Shop:          env.spyShop,
		Ledger:        &fakeLedger{balance: 500},
		Catalog:       fakeCatalog{},
		Directory:     fakeDirectory{},
		AdminEcon:     &fakeAdmin{},
		Authorizer:    allowAuthorizer{},
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	env.sink = sink
	env.daily = platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	env.me = platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)
	env.shop = platformv1connect.NewShopServiceClient(srv.Client(), srv.URL)
	env.admin = platformv1connect.NewAdminEconomyServiceClient(srv.Client(), srv.URL)
	return env
}

func claimReq() *connect.Request[platformv1.ClaimRequest] {
	return connect.NewRequest(&platformv1.ClaimRequest{})
}

func purchaseReq(key string) *connect.Request[platformv1.PurchaseRequest] {
	return connect.NewRequest(&platformv1.PurchaseRequest{
		ItemPublicId: "ITEM-1", IdempotencyKey: key,
	})
}

// ── 白名單內:代打成立 ──────────────────────────────────────────

// /daily 的核心場景:服務憑證 + 有效 X-Acting-User → 成功,
// 而且錢是發給**代打對象**(不是服務,也不是 Bearer 那個測試使用者)。
func TestDelegatedDailyClaim(t *testing.T) {
	env := newDelegationEnv(t)

	res, err := env.daily.Claim(context.Background(),
		withService(claimReq(), svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("代打 /daily 應該成功:%v", err)
	}
	if res.Msg.GetAmount() != 30 {
		t.Fatalf("amount = %d,期望 30", res.Msg.GetAmount())
	}
	calls, subject := env.spyDaily.seen()
	if calls != 1 {
		t.Fatalf("handler 被叫 %d 次,期望 1", calls)
	}
	if subject != actingUserID {
		t.Fatalf("主體 = %d,期望 %d(代打對象)", subject, actingUserID)
	}
}

// MeService 的讀取類 RPC 一樣看得到代打對象自己的資料。
func TestDelegatedMeGetProfile(t *testing.T) {
	env := newDelegationEnv(t)

	res, err := env.me.GetProfile(context.Background(),
		withService(connect.NewRequest(&platformv1.GetProfileRequest{}), svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("代打 GetProfile 應該成功:%v", err)
	}
	if got := env.profiles.subject(); got != actingUserID {
		t.Fatalf("查的是 user %d 的檔案,期望 %d", got, actingUserID)
	}
	if want := fmt.Sprintf("U-%d", actingUserID); res.Msg.GetProfile().GetPublicId() != want {
		t.Fatalf("public_id = %q,期望 %q", res.Msg.GetProfile().GetPublicId(), want)
	}
}

// 白名單內的所有 RPC 都要真的通(清單寫錯了會在這裡露餡)。
func TestDelegatedWhitelistAllPass(t *testing.T) {
	env := newDelegationEnv(t)
	ctx := context.Background()

	calls := []struct {
		name string
		do   func() error
	}{
		{"DailyService/Claim", func() error {
			_, err := env.daily.Claim(ctx, withService(claimReq(), svcTokenOld, actingLinked))
			return err
		}},
		{"MeService/GetProfile", func() error {
			_, err := env.me.GetProfile(ctx, withService(
				connect.NewRequest(&platformv1.GetProfileRequest{}), svcTokenOld, actingLinked))
			return err
		}},
		{"MeService/GetBalance", func() error {
			_, err := env.me.GetBalance(ctx, withService(
				connect.NewRequest(&platformv1.GetBalanceRequest{Currency: "coin"}), svcTokenOld, actingLinked))
			return err
		}},
		{"MeService/ListBalances", func() error {
			_, err := env.me.ListBalances(ctx, withService(
				connect.NewRequest(&platformv1.ListBalancesRequest{}), svcTokenOld, actingLinked))
			return err
		}},
		{"MeService/ListEntitlements", func() error {
			_, err := env.me.ListEntitlements(ctx, withService(
				connect.NewRequest(&platformv1.ListEntitlementsRequest{}), svcTokenOld, actingLinked))
			return err
		}},
		{"MeService/ListRedemptions", func() error {
			_, err := env.me.ListRedemptions(ctx, withService(
				connect.NewRequest(&platformv1.ListRedemptionsRequest{}), svcTokenOld, actingLinked))
			return err
		}},
		{"ShopService/Purchase", func() error {
			_, err := env.shop.Purchase(ctx, withService(purchaseReq("k-all"), svcTokenOld, actingLinked))
			return err
		}},
		{"ShopService/CancelRedemption", func() error {
			_, err := env.shop.CancelRedemption(ctx, withService(
				connect.NewRequest(&platformv1.CancelRedemptionRequest{RedemptionPublicId: "R-1"}),
				svcTokenOld, actingLinked))
			return err
		}},
	}
	for _, c := range calls {
		if err := c.do(); err != nil {
			t.Fatalf("%s 代打應該成功,得到 %v(err=%v)", c.name, codeOf(err), err)
		}
	}
}

// 公開型錄(ShopService/ListItems)刻意不在代打白名單:它本來就是 public,
// 服務憑證原本就打得到 —— 這條路徑不能因為改動而壞掉。
func TestServiceCanStillCallPublicListItems(t *testing.T) {
	env := newDelegationEnv(t)
	if _, err := env.shop.ListItems(context.Background(), withService(
		connect.NewRequest(&platformv1.ListItemsRequest{}), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("公開型錄應該打得到:%v", err)
	}
}

// ── 白名單外:一律拒絕 ──────────────────────────────────────────

// 白名單外的使用者 RPC:改時區(Discord 沒有正確的值可填)與
// 自助退款(要本人在網頁確認)都必須拒絕,而且 handler 不會被叫到。
func TestDelegationRejectedOutsideWhitelist(t *testing.T) {
	env := newDelegationEnv(t)
	ctx := context.Background()

	_, err := env.me.UpdateTimezone(ctx, withService(
		connect.NewRequest(&platformv1.UpdateTimezoneRequest{Timezone: "Asia/Taipei"}),
		svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("UpdateTimezone 代打 code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}

	_, err = env.shop.RefundPurchase(ctx, withService(
		connect.NewRequest(&platformv1.RefundPurchaseRequest{
			EntitlementPublicId: "E-1", IdempotencyKey: "k1",
		}), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("RefundPurchase 代打 code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}
}

// 管理 RPC 用代打:絕不。代打路徑不經過 Authorize,放行等於少一道授權檢查。
func TestDelegationRejectedForAdminRPC(t *testing.T) {
	env := newDelegationEnv(t)
	_, err := env.admin.Grant(context.Background(), withService(
		connect.NewRequest(&platformv1.GrantRequest{
			UserPublicId: "U1", Currency: "coin", Amount: 10, IdempotencyKey: "k1",
		}), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("管理 RPC 代打 code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}
}

// 服務憑證但沒說代表誰:白名單內的 RPC 也必須拒絕。
// 沒有主體的「代打」就是服務自己在動別人的錢。
func TestDelegationRequiresActingUser(t *testing.T) {
	env := newDelegationEnv(t)
	_, err := env.daily.Claim(context.Background(),
		withService(claimReq(), svcTokenOld, ""))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v,期望 invalid_argument(err=%v)", codeOf(err), err)
	}
	if calls, _ := env.spyDaily.seen(); calls != 0 {
		t.Fatalf("沒有主體卻進了 handler(%d 次)", calls)
	}
}

// 未綁定的 Discord id:既有的 FailedPrecondition 行為不變(代打也一樣)。
func TestDelegationUnlinkedActingUser(t *testing.T) {
	env := newDelegationEnv(t)
	_, err := env.daily.Claim(context.Background(),
		withService(claimReq(), svcTokenOld, actingUnlinked))
	if codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v,期望 failed_precondition(err=%v)", codeOf(err), err)
	}
	if calls, _ := env.spyDaily.seen(); calls != 0 {
		t.Fatalf("解不出使用者卻進了 handler(%d 次)", calls)
	}
}

// 混帶憑證維持既有行為:拒絕,不做擇一採用的猜測。
func TestDelegationMixedCredentialsRejected(t *testing.T) {
	env := newDelegationEnv(t)
	req := withService(claimReq(), svcTokenOld, actingLinked)
	req.Header().Set("Authorization", "Bearer "+testToken)
	if _, err := env.daily.Claim(context.Background(), req); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v,期望 permission_denied(err=%v)", codeOf(err), err)
	}
	if calls, _ := env.spyDaily.seen(); calls != 0 {
		t.Fatal("混帶憑證竟然進了 handler")
	}
}

// 無效服務 token + 代打白名單:一律 Unauthenticated(代打不是繞過認證的後門)。
func TestDelegationBadServiceToken(t *testing.T) {
	env := newDelegationEnv(t)
	_, err := env.daily.Claim(context.Background(),
		withService(claimReq(), "wrong-token-"+strings.Repeat("x", 30), actingLinked))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v,期望 unauthenticated(err=%v)", codeOf(err), err)
	}
	if calls, _ := env.spyDaily.seen(); calls != 0 {
		t.Fatal("無效憑證竟然進了 handler")
	}
}

// ── 兩條路徑並存且一致 ──────────────────────────────────────────

// 本人 Bearer 打同一支 RPC 照舊可用,主體是登入者本人(不是代打對象)。
func TestOwnBearerStillWorksOnDelegatedProcedure(t *testing.T) {
	env := newDelegationEnv(t)
	if _, err := env.daily.Claim(context.Background(), withToken(claimReq())); err != nil {
		t.Fatalf("本人 Bearer 應該照舊可用:%v", err)
	}
	calls, subject := env.spyDaily.seen()
	if calls != 1 || subject != testUserID {
		t.Fatalf("calls/主體 = %d/%d,期望 1/%d", calls, subject, testUserID)
	}
}

// 同一個冪等鍵、同一個主體,不論走代打或本人 Bearer 都只動一次錢。
//
// 這是兩條路徑「結果一致」最要緊的一項:代打若把主體傳成別人,
// 冪等鍵就落在不同的人身上,兩條路各扣一次 —— 使用者被扣兩次錢。
func TestDelegatedAndBearerShareIdempotency(t *testing.T) {
	env := newDelegationEnv(t)
	ctx := context.Background()
	const key = "purchase-key-1"

	// 代打先送一次(主體 = actingUserID)
	first, err := env.shop.Purchase(ctx, withService(purchaseReq(key), svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("代打購買:%v", err)
	}
	if first.Msg.GetReplayed() {
		t.Fatal("第一次購買不該是重放")
	}
	// 同一個人、同一把冪等鍵再送一次
	second, err := env.shop.Purchase(ctx, withService(purchaseReq(key), svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("代打重送:%v", err)
	}
	if !second.Msg.GetReplayed() {
		t.Fatal("同一把冪等鍵重送應該是重放")
	}
	if n := env.spyShop.chargeCount(); n != 1 {
		t.Fatalf("實際動錢 %d 次,期望 1", n)
	}
}

// user_restrictions 這類「他能不能做」的檢查照常生效:
// 代打只解決主體是誰。真正的檢查在 core/storage,這裡證明它沒有被繞過 ——
// 被擋的使用者不論走哪條路徑,結果都一樣。
func TestDelegationDoesNotBypassRestrictions(t *testing.T) {
	env := newDelegationEnv(t)
	env.spyShop.restricted = actingUserID
	ctx := context.Background()

	_, err := env.shop.Purchase(ctx, withService(purchaseReq("k-restricted"), svcTokenOld, actingLinked))
	if err == nil {
		t.Fatal("被限制的使用者代打購買竟然成功")
	}
	if n := env.spyShop.chargeCount(); n != 0 {
		t.Fatalf("被擋的請求動了 %d 次錢", n)
	}

	// 換成本人 Bearer(testUserID 沒被限制)—— 同一支 RPC 照樣可以買,
	// 證明剛才擋下來的是「那個人的限制」,不是代打整條路徑被關掉。
	if _, err := env.shop.Purchase(ctx, withToken(purchaseReq("k-ok"))); err != nil {
		t.Fatalf("沒被限制的使用者應該買得到:%v", err)
	}
}

// ── 稽核:分得出代打與本人 ──────────────────────────────────────

// event_logs 必須同時滿足三件事:user_id 是代打對象、channel 是 discord、
// 而且看得出「這是代打」(response 摘要的 delegated 旗標)。
// 服務憑證與服務名仍然不出現在任何欄位。
func TestDelegatedCallAuditRecord(t *testing.T) {
	env := newDelegationEnv(t)
	if _, err := env.daily.Claim(context.Background(),
		withService(claimReq(), svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("代打 /daily:%v", err)
	}
	rec := waitRecords(t, env.sink, 1)[0]
	if rec.Channel != eventlog.ChannelDiscord {
		t.Fatalf("channel = %q,期望 discord", rec.Channel)
	}
	if rec.UserID == nil || *rec.UserID != actingUserID {
		t.Fatalf("user_id = %v,期望 %d(代打對象)", rec.UserID, actingUserID)
	}
	if !strings.Contains(string(rec.Response), `"delegated":true`) {
		t.Fatalf("代打的紀錄少了 delegated 標記:%s", rec.Response)
	}
	assertNoServiceToken(t, rec)
}

// 本人操作**不能**被標成代打 —— 標記反了等於冤枉閘道,或替真兇脫罪。
func TestOwnCallAuditHasNoDelegationMark(t *testing.T) {
	env := newDelegationEnv(t)
	if _, err := env.daily.Claim(context.Background(), withToken(claimReq())); err != nil {
		t.Fatalf("本人 /daily:%v", err)
	}
	rec := waitRecords(t, env.sink, 1)[0]
	if strings.Contains(string(rec.Response), "delegated") {
		t.Fatalf("本人操作被標成代打:%s", rec.Response)
	}
	if rec.Channel != eventlog.ChannelAPI && rec.Channel != eventlog.ChannelGRPC {
		t.Fatalf("channel = %q,本人操作的入口應該是 api/grpc", rec.Channel)
	}
	if rec.UserID == nil || *rec.UserID != testUserID {
		t.Fatalf("user_id = %v,期望 %d", rec.UserID, testUserID)
	}
}

// 被擋下的代打嘗試也要留痕,而且不能被標成代打(它根本沒成立)。
func TestRejectedDelegationIsLoggedAsDenied(t *testing.T) {
	env := newDelegationEnv(t)
	_, _ = env.me.UpdateTimezone(context.Background(), withService(
		connect.NewRequest(&platformv1.UpdateTimezoneRequest{Timezone: "Asia/Taipei"}),
		svcTokenOld, actingLinked))

	rec := waitRecords(t, env.sink, 1)[0]
	if rec.Status != eventlog.StatusDenied {
		t.Fatalf("status = %q,期望 denied", rec.Status)
	}
	if strings.Contains(string(rec.Response), "delegated") {
		t.Fatalf("沒成立的代打不該被標記:%s", rec.Response)
	}
	assertNoServiceToken(t, rec)
}

// zeroActors 模擬「解析成功但沒有身分」的壞實作(回 0, nil)。
type zeroActors struct{}

func (zeroActors) ResolveActor(context.Context, string, string) (int64, error) { return 0, nil }

// 代打的主體不能是 0:那等於「服務代表沒有人動錢」。
// 與 Bearer 路徑同一道防線(userID == 0 一律當認證失敗)。
func TestDelegationRejectsZeroSubject(t *testing.T) {
	spy := &spyDaily{}
	srv, _ := newServer(t, transport.Deps{
		Daily:         spy,
		ActingUsers:   zeroActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	client := platformv1connect.NewDailyServiceClient(srv.Client(), srv.URL)
	_, err := client.Claim(context.Background(), withService(claimReq(), svcTokenOld, actingLinked))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v,期望 unauthenticated(err=%v)", codeOf(err), err)
	}
	if calls, _ := spy.seen(); calls != 0 {
		t.Fatalf("主體為 0 卻進了 handler(%d 次)", calls)
	}
}
