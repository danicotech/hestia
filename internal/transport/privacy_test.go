package transport_test

import (
	"context"
	"sync"
	"testing"

	"connectrpc.com/connect"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/transport"
)

// 隱私設定的入口層測試。要證明的:
//   - 主體永遠是「認證解出來的那個人」——請求裡沒有 user id 可填,
//     所以「讀寫別人的設定」在契約層級就不存在;
//   - 代打路徑可用(stentor 的 /privacy optout 靠它);
//   - 兩個布林各自可設,原樣傳到 port(沒帶的是 nil,不是 false);
//   - 兩個都沒帶 = InvalidArgument,不是靜靜成功。

// spyPrivacy 記下每次呼叫的主體與參數。
type spyPrivacy struct {
	mu       sync.Mutex
	lastUser int64
	lastUp   readmodel.PrivacyUpdate
	// state 用主體 id 當 key:同一個 server 上兩個不同使用者的設定要互不相干。
	state map[int64]readmodel.PrivacyView
}

func newSpyPrivacy() *spyPrivacy {
	return &spyPrivacy{state: map[int64]readmodel.PrivacyView{}}
}

func (s *spyPrivacy) Privacy(_ context.Context, userID int64) (*readmodel.PrivacyView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUser = userID
	v := s.state[userID]
	return &v, nil
}

func (s *spyPrivacy) SetPrivacy(
	_ context.Context, userID int64, up readmodel.PrivacyUpdate,
) (*readmodel.PrivacyView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUser, s.lastUp = userID, up
	v := s.state[userID]
	if up.OptOutLogging != nil {
		v.OptOutLogging = *up.OptOutLogging
	}
	if up.OptOutAICorpus != nil {
		v.OptOutAICorpus = *up.OptOutAICorpus
	}
	s.state[userID] = v
	return &v, nil
}

func (s *spyPrivacy) seen() (int64, readmodel.PrivacyUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUser, s.lastUp
}

func newPrivacyEnv(t *testing.T) (platformv1connect.MeServiceClient, *spyPrivacy) {
	t.Helper()
	spy := newSpyPrivacy()
	srv, _ := newServer(t, transport.Deps{
		Privacy:       spy,
		Profiles:      fakeProfiles{},
		ActingUsers:   &fakeActors{},
		ServiceTokens: []transport.ServiceToken{{Name: "stentor", Token: svcTokenOld, Delegable: transport.AllDelegableProcedures()}},
	})
	return platformv1connect.NewMeServiceClient(srv.Client(), srv.URL), spy
}

func getPrivacyReq() *connect.Request[platformv1.GetPrivacyRequest] {
	return connect.NewRequest(&platformv1.GetPrivacyRequest{})
}

// 沒有設定列的使用者拿到預設值,不是錯誤。
func TestGetPrivacyDefaults(t *testing.T) {
	me, spy := newPrivacyEnv(t)

	res, err := me.GetPrivacy(context.Background(), withToken(getPrivacyReq()))
	if err != nil {
		t.Fatalf("GetPrivacy: %v", err)
	}
	s := res.Msg.GetSettings()
	if s == nil {
		t.Fatal("settings 不該是 nil —— 預設值也要是明確回的訊息")
	}
	if s.GetOptOutLogging() || s.GetOptOutAiCorpus() {
		t.Errorf("預設應該兩者皆 false,拿到 %+v", s)
	}
	if user, _ := spy.seen(); user != testUserID {
		t.Errorf("主體 = %d,期望 %d(Bearer 解出來的人)", user, testUserID)
	}
}

// 兩個布林各自可設:沒帶的那個要以 nil 傳到 port,不是 false。
func TestUpdatePrivacyPassesOptionalFields(t *testing.T) {
	me, spy := newPrivacyEnv(t)

	res, err := me.UpdatePrivacy(context.Background(), withToken(
		connect.NewRequest(&platformv1.UpdatePrivacyRequest{
			OptOutLogging: proto(true),
		})))
	if err != nil {
		t.Fatalf("UpdatePrivacy: %v", err)
	}
	if !res.Msg.GetSettings().GetOptOutLogging() {
		t.Error("回應沒有反映剛設定的值")
	}
	_, up := spy.seen()
	if up.OptOutLogging == nil || !*up.OptOutLogging {
		t.Errorf("opt_out_logging 沒傳到 port:%v", up.OptOutLogging)
	}
	if up.OptOutAICorpus != nil {
		t.Errorf("沒帶的欄位應該是 nil,拿到 %v —— 傳 false 會把另一項偷偷關掉", *up.OptOutAICorpus)
	}
}

// 兩個都沒帶 = InvalidArgument。寫入 RPC 不能靜靜地什麼都不做。
func TestUpdatePrivacyRejectsEmptyRequest(t *testing.T) {
	me, _ := newPrivacyEnv(t)

	_, err := me.UpdatePrivacy(context.Background(), withToken(
		connect.NewRequest(&platformv1.UpdatePrivacyRequest{})))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v,期望 InvalidArgument(err=%v)", codeOf(err), err)
	}
}

// 代打:stentor 帶服務憑證 + X-Acting-User,主體是代打對象。
// 這是 `/privacy optout` 唯一走得通的路徑。
func TestDelegatedPrivacy(t *testing.T) {
	me, spy := newPrivacyEnv(t)

	if _, err := me.UpdatePrivacy(context.Background(), withService(
		connect.NewRequest(&platformv1.UpdatePrivacyRequest{OptOutAiCorpus: proto(true)}),
		svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("代打 UpdatePrivacy 應該成功:%v", err)
	}
	if user, _ := spy.seen(); user != actingUserID {
		t.Fatalf("主體 = %d,期望 %d(代打對象)", user, actingUserID)
	}

	res, err := me.GetPrivacy(context.Background(),
		withService(getPrivacyReq(), svcTokenOld, actingLinked))
	if err != nil {
		t.Fatalf("代打 GetPrivacy 應該成功:%v", err)
	}
	if !res.Msg.GetSettings().GetOptOutAiCorpus() {
		t.Error("代打讀到的不是代打對象剛剛設定的值")
	}
}

// 代打的效果只落在代打對象身上:Bearer 那個使用者的設定不受影響。
// 「讀寫別人的設定」在契約層級不存在(請求裡沒有 user id),
// 這個測試證明兩條路徑的主體確實是分開的。
func TestPrivacyIsolatedPerSubject(t *testing.T) {
	me, _ := newPrivacyEnv(t)

	if _, err := me.UpdatePrivacy(context.Background(), withService(
		connect.NewRequest(&platformv1.UpdatePrivacyRequest{OptOutLogging: proto(true)}),
		svcTokenOld, actingLinked)); err != nil {
		t.Fatalf("代打設定: %v", err)
	}
	res, err := me.GetPrivacy(context.Background(), withToken(getPrivacyReq()))
	if err != nil {
		t.Fatalf("本人 GetPrivacy: %v", err)
	}
	if res.Msg.GetSettings().GetOptOutLogging() {
		t.Error("代打對象的設定漏到了另一個使用者身上")
	}
}

// 沒有任何憑證 = Unauthenticated(隱私設定不是公開資料)。
func TestPrivacyRequiresIdentity(t *testing.T) {
	me, _ := newPrivacyEnv(t)

	if _, err := me.GetPrivacy(context.Background(), getPrivacyReq()); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetPrivacy 無憑證:code = %v", codeOf(err))
	}
	_, err := me.UpdatePrivacy(context.Background(),
		connect.NewRequest(&platformv1.UpdatePrivacyRequest{OptOutLogging: proto(true)}))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("UpdatePrivacy 無憑證:code = %v", codeOf(err))
	}
}

// 服務憑證但沒說明代表誰 = InvalidArgument(代打一定要有主體)。
func TestPrivacyDelegationRequiresActingUser(t *testing.T) {
	me, _ := newPrivacyEnv(t)

	_, err := me.GetPrivacy(context.Background(),
		withService(getPrivacyReq(), svcTokenOld, ""))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v,期望 InvalidArgument(err=%v)", codeOf(err), err)
	}
}

// 未注入實作時回 Unimplemented,而不是 panic。
func TestPrivacyUnimplemented(t *testing.T) {
	srv, _ := newServer(t, transport.Deps{})
	me := platformv1connect.NewMeServiceClient(srv.Client(), srv.URL)

	if _, err := me.GetPrivacy(context.Background(), withToken(getPrivacyReq())); codeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v,期望 Unimplemented", codeOf(err))
	}
}

func proto(b bool) *bool { return &b }
