package transport

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// fakeTournamentCreator 是「兩個介面都滿足」的替身,對應真正注入的
// tournament.Service。基礎替身(fakeActivityTournament)刻意只實作
// ActivityTournament —— 於是「能力偵測拿不到 Create」那條路也測得到。
type fakeTournamentCreator struct {
	*fakeActivityTournament
	calls []tournament.CreateParams
	res   *tournament.CreateResult
	err   error
}

func (f *fakeTournamentCreator) Create(
	_ context.Context, p tournament.CreateParams,
) (*tournament.CreateResult, error) {
	f.calls = append(f.calls, p)
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func createDeps(t *testing.T) (ActivityDeps, *fakeTournamentCreator, *fakeActivityAuthorizer) {
	t.Helper()
	deps, tsvc, _ := baseActivityDeps()
	cfg := tournament.DefaultConfig()
	creator := &fakeTournamentCreator{
		fakeActivityTournament: tsvc,
		res: &tournament.CreateResult{
			View: tournament.View{
				Tournament: tournament.Tournament{
					PublicID: "01J0TOURNAMENT", Slug: testSlug, Name: "百業試鋒",
					Phase: tournament.PhaseSignup,
				},
				Config: cfg,
			},
			HandicapItemCount: 34,
		},
	}
	authz := &fakeActivityAuthorizer{allow: true}
	deps.TournamentCreator = creator
	deps.Authorizer = authz
	return deps, creator, authz
}

func createRequest() *connect.Request[activityv1.CreateTournamentRequest] {
	return connect.NewRequest(&activityv1.CreateTournamentRequest{
		CommunityPublicId: "01J0COMMUNITY",
		Slug:              testSlug,
		Name:              "百業試鋒",
	})
}

// 未登入、無裁判權限、未注入 Authorizer 這三道,與其餘裁判 RPC 共用
// TestJudgeServiceRequiresPlatformAccountAndPermission 與
// TestJudgeServiceWithoutAuthorizerDeniesEverything —— CreateTournament 已在
// judgeCalls 裡,不在這裡再寫一份(同一條規則只該有一個測試說它)。

func TestCreateTournamentPassesActorAndDefaults(t *testing.T) {
	deps, creator, _ := createDeps(t)
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	resp, err := client.CreateTournament(context.Background(), withUser(createRequest(), testUserID))
	if err != nil {
		t.Fatalf("CreateTournament: %v", err)
	}
	if resp.Msg.GetHandicapItemCount() != 34 {
		t.Errorf("handicap_item_count = %d,want 34", resp.Msg.GetHandicapItemCount())
	}
	if resp.Msg.GetTournament().GetPhase() != activityv1.TournamentPhase_TOURNAMENT_PHASE_SIGNUP {
		t.Errorf("phase = %v,新賽事必須停在報名期", resp.Msg.GetTournament().GetPhase())
	}
	if len(creator.calls) != 1 {
		t.Fatalf("Create 被呼叫 %d 次", len(creator.calls))
	}
	got := creator.calls[0]
	if got.ActorUserID != testUserID {
		t.Errorf("actor = %d,want %d", got.ActorUserID, testUserID)
	}
	if got.Config != (tournament.ConfigOverrides{}) {
		t.Errorf("沒填的旋鈕應該全是 nil,拿到 %+v", got.Config)
	}
}

// vig_bps 是唯一「0 有意義」的旋鈕:proto 那側開成 optional,
// 這裡要真的分得出「沒填」與「填了 0」。其餘欄位的 0 一律是沒填。
func TestCreateTournamentZeroVigIsNotAbsent(t *testing.T) {
	deps, creator, _ := createDeps(t)
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	req := createRequest()
	zero := int64(0)
	req.Msg.Odds = &activityv1.TournamentOddsSettings{VigBps: &zero, Smoothing: 0}
	req.Msg.BpPerRankGap = 12
	req.Msg.Prizes = &activityv1.TournamentPrizeSettings{Champion: 5000}
	if _, err := client.CreateTournament(context.Background(), withUser(req, testUserID)); err != nil {
		t.Fatalf("CreateTournament: %v", err)
	}

	got := creator.calls[0].Config
	if got.VigBPS == nil || *got.VigBPS != 0 {
		t.Errorf("vig_bps = %v,填 0 必須傳成 0 而不是 nil", got.VigBPS)
	}
	if got.Smoothing != nil {
		t.Errorf("smoothing = %v,沒填的 0 必須是 nil", *got.Smoothing)
	}
	if got.BPPerRankGap == nil || *got.BPPerRankGap != 12 {
		t.Errorf("bp_per_rank_gap = %v,want 12", got.BPPerRankGap)
	}
	if got.Prizes == nil || got.Prizes.Champion != 5000 || got.Prizes.Third != 0 {
		t.Errorf("prizes = %+v", got.Prizes)
	}
}

// 領域錯誤要映射成呼叫端分得出來的 code + reason。
// 代號撞了與代號格式錯是兩件事,client 的下一步完全不同。
func TestCreateTournamentErrorMapping(t *testing.T) {
	cases := map[string]struct {
		err    error
		code   connect.Code
		reason string
	}{
		"代號已被使用": {tournament.ErrSlugTaken, connect.CodeAlreadyExists, "tournament_slug_taken"},
		"代號不合法":  {tournament.ErrInvalidSlug, connect.CodeInvalidArgument, "tournament_invalid_slug"},
		"社群不存在":  {tournament.ErrCommunityNotFound, connect.CodeNotFound, "community_not_found"},
		"設定不合法":  {tournament.ErrConfigMalformed, connect.CodeInvalidArgument, "tournament_invalid_config"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			deps, creator, _ := createDeps(t)
			creator.err = c.err
			srv := newActivityServer(t, deps)
			client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

			_, err := client.CreateTournament(context.Background(), withUser(createRequest(), testUserID))
			requireCode(t, err, c.code)
			if got := ErrorReason(err); got != c.reason {
				t.Errorf("reason = %q,want %q", got, c.reason)
			}
		})
	}
}

// 必填欄位在入口擋:proto 沒有 required,少一個欄位就是 InvalidArgument。
func TestCreateTournamentRequiredFields(t *testing.T) {
	blanks := map[string]func(*activityv1.CreateTournamentRequest){
		"community_public_id": func(m *activityv1.CreateTournamentRequest) { m.CommunityPublicId = " " },
		"slug":                func(m *activityv1.CreateTournamentRequest) { m.Slug = "" },
		"name":                func(m *activityv1.CreateTournamentRequest) { m.Name = "\t" },
	}
	for field, blank := range blanks {
		t.Run(field, func(t *testing.T) {
			deps, creator, _ := createDeps(t)
			srv := newActivityServer(t, deps)
			client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

			req := createRequest()
			blank(req.Msg)
			_, err := client.CreateTournament(context.Background(), withUser(req, testUserID))
			requireCode(t, err, connect.CodeInvalidArgument)
			if len(creator.calls) != 0 {
				t.Fatal("欄位沒填卻碰到了領域層")
			}
		})
	}
}

// 注入的實作沒有建賽事的能力時回 Unimplemented,不是 Internal ——
// 呼叫端才知道「這個功能還沒接上」而不是「伺服器壞了」。
func TestCreateTournamentUnimplementedWithoutCreator(t *testing.T) {
	deps, _, _ := baseActivityDeps()
	deps.Authorizer = &fakeActivityAuthorizer{allow: true}
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewJudgeServiceClient(srv.Client(), srv.URL)

	requireCode(t, mustFail(client.CreateTournament(
		context.Background(), withUser(createRequest(), testUserID))),
		connect.CodeUnimplemented)
}

// mustFail 丟掉成功時的回應,只留錯誤 —— 讓拒絕類的斷言能寫成一行。
func mustFail[T any](_ *connect.Response[T], err error) error { return err }
