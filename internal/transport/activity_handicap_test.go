package transport

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
)

type fakeActivityHandicap struct {
	items    []handicap.Item
	itemsFor []int64

	budgetCalls []struct {
		matchPublicID string
		playerID      int64
	}
	myBudget *handicap.MyBudget

	selects   []handicap.SelectParams
	selectRes *handicap.SelectResult
	selectErr error

	voids   []handicap.VoidParams
	voidRes *handicap.Budget

	viewers    []int64
	matchHcaps *handicap.MatchHandicaps
}

func (f *fakeActivityHandicap) ListItems(_ context.Context, tournamentID int64) ([]handicap.Item, error) {
	f.itemsFor = append(f.itemsFor, tournamentID)
	return f.items, nil
}

func (f *fakeActivityHandicap) MyBudget(
	_ context.Context, matchPublicID string, playerID int64,
) (*handicap.MyBudget, error) {
	f.budgetCalls = append(f.budgetCalls, struct {
		matchPublicID string
		playerID      int64
	}{matchPublicID, playerID})
	return f.myBudget, nil
}

func (f *fakeActivityHandicap) Select(
	_ context.Context, p handicap.SelectParams,
) (*handicap.SelectResult, error) {
	f.selects = append(f.selects, p)
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	return f.selectRes, nil
}

func (f *fakeActivityHandicap) VoidSelection(
	_ context.Context, p handicap.VoidParams,
) (*handicap.Budget, error) {
	f.voids = append(f.voids, p)
	return f.voidRes, nil
}

func (f *fakeActivityHandicap) MatchHandicaps(
	_ context.Context, _ string, viewerPlayerID int64,
) (*handicap.MatchHandicaps, error) {
	f.viewers = append(f.viewers, viewerPlayerID)
	return f.matchHcaps, nil
}

func handicapDeps() (ActivityDeps, *fakeActivityHandicap, *fakeActivitySessions) {
	deps, _, _ := baseActivityDeps()
	budget := handicap.Budget{
		MatchID: 1, PlayerID: testPlayerAInternal,
		PlayerPublicID: testPlayerAID, Budget: 24, Spent: 10,
	}
	selection := handicap.Selection{
		PublicID: "01SELECTION", MatchPublicID: testMatchID,
		ItemRef: "01ITEM", ItemName: "禁迴避", Category: handicap.CategoryDefense,
		Cost: 10, TargetNote: "", CreatedAt: time.Now(),
	}
	svc := &fakeActivityHandicap{
		items: []handicap.Item{{
			Ref: "01ITEM", Category: handicap.CategoryDefense, Name: "禁迴避",
			Description: "本場不得使用迴避", Cost: 10, Repeatable: true, SortOrder: 3,
		}},
		myBudget: &handicap.MyBudget{
			HasBudget: true, Budget: budget,
			Selections: []handicap.Selection{selection}, Editable: true,
		},
		selectRes: &handicap.SelectResult{
			Selection: selection, Budget: budget,
			Advisory: handicap.AdvisoryVictoryConflict,
		},
		voidRes: &handicap.Budget{
			PlayerPublicID: testPlayerAID, Budget: 24, Spent: 0,
		},
		matchHcaps: &handicap.MatchHandicaps{
			MatchPublicID:             testMatchID,
			Status:                    "locked",
			HolderPlayerPublicID:      testPlayerAID,
			ConstrainedPlayerPublicID: testPlayerBID,
			Budget:                    &budget,
			Selections:                []handicap.Selection{selection},
			Revealed:                  true,
		},
	}
	sessions := &fakeActivitySessions{
		byToken: map[string]ActivityIdentity{
			testSessionTok: {TournamentSlug: testSlug, PlayerPublicID: testPlayerAID},
		},
	}
	deps.Handicap = svc
	deps.Sessions = sessions
	return deps, svc, sessions
}

// 選購側的每一支都要選手身分 —— 沒有 session 就不該碰得到任何人的 BP。
func TestHandicapPurchasePathRequiresPlayerSession(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	_, err := client.GetMyBudget(ctx, connect.NewRequest(&activityv1.GetMyBudgetRequest{
		MatchPublicId: testMatchID,
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	_, err = client.Select(ctx, connect.NewRequest(&activityv1.SelectRequest{
		MatchPublicId: testMatchID, ItemPublicId: "01ITEM",
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	_, err = client.VoidSelection(ctx, connect.NewRequest(&activityv1.VoidSelectionRequest{
		SelectionPublicId: "01SELECTION",
	}))
	requireCode(t, err, connect.CodeUnauthenticated)

	// 平台帳號不是選手身分,不能互相頂替。
	_, err = client.Select(ctx, withUser(connect.NewRequest(&activityv1.SelectRequest{
		MatchPublicId: testMatchID, ItemPublicId: "01ITEM",
	}), testUserID))
	requireCode(t, err, connect.CodeUnauthenticated)

	if len(svc.selects) != 0 {
		t.Fatalf("沒有身分時不該碰領域層,卻呼叫了 %d 次", len(svc.selects))
	}
}

// 施加者永遠來自 session:請求裡沒有、也不該有任何指定選手的欄位。
func TestSelectTakesPlayerFromSessionOnly(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	got, err := client.Select(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.SelectRequest{
			MatchPublicId: testMatchID, ItemPublicId: "01ITEM", TargetNote: "凌霄劍法",
		}), testSessionTok))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(svc.selects) != 1 {
		t.Fatalf("領域層收到 %d 次購買", len(svc.selects))
	}
	p := svc.selects[0]
	if p.PlayerID != testPlayerAInternal {
		t.Fatalf("PlayerID = %d,want %d(來自 session)", p.PlayerID, testPlayerAInternal)
	}
	if p.ItemRef != "01ITEM" || p.MatchPublicID != testMatchID || p.TargetNote != "凌霄劍法" {
		t.Fatalf("參數轉換錯誤:%+v", p)
	}
	// advisory 非空不代表失敗,購買已經成立。
	if got.Msg.GetAdvisory() != handicap.AdvisoryVictoryConflict {
		t.Fatalf("advisory = %q", got.Msg.GetAdvisory())
	}
	if got.Msg.GetSelection().GetPublicId() != "01SELECTION" {
		t.Fatalf("selection = %q", got.Msg.GetSelection().GetPublicId())
	}
	if got.Msg.GetBudget().GetRemaining() != 14 {
		t.Fatalf("remaining = %d,want 14(24 − 10,由伺服器算)", got.Msg.GetBudget().GetRemaining())
	}
}

func TestSelectRequiresIDs(t *testing.T) {
	deps, _, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	_, err := client.Select(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.SelectRequest{ItemPublicId: "01ITEM"}), testSessionTok))
	requireCode(t, err, connect.CodeInvalidArgument)

	_, err = client.Select(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.SelectRequest{MatchPublicId: testMatchID}), testSessionTok))
	requireCode(t, err, connect.CodeInvalidArgument)
}

func TestGetMyBudgetPassesSessionPlayer(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	got, err := client.GetMyBudget(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.GetMyBudgetRequest{MatchPublicId: testMatchID}), testSessionTok))
	if err != nil {
		t.Fatalf("GetMyBudget: %v", err)
	}
	if len(svc.budgetCalls) != 1 || svc.budgetCalls[0].playerID != testPlayerAInternal {
		t.Fatalf("領域層收到的選手 = %+v", svc.budgetCalls)
	}
	if got.Msg.GetBudget().GetMatchPublicId() != testMatchID {
		t.Fatalf("budget.match_public_id = %q", got.Msg.GetBudget().GetMatchPublicId())
	}
	if !got.Msg.GetEditable() {
		t.Fatal("editable 應為 true")
	}
	if len(got.Msg.GetSelections()) != 1 {
		t.Fatalf("selections 數 = %d", len(got.Msg.GetSelections()))
	}
}

// 同段對決 / 高段位方:沒有預算不是錯誤,而是一個帶說明的正常回應。
func TestGetMyBudgetWithoutBudgetIsNotAnError(t *testing.T) {
	deps, svc, _ := handicapDeps()
	svc.myBudget = &handicap.MyBudget{
		HasBudget: false, Editable: false,
		LockedReason: "本場沒有讓武 —— 同段對決,或你是段位較高的一方",
	}
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	got, err := client.GetMyBudget(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.GetMyBudgetRequest{MatchPublicId: testMatchID}), testSessionTok))
	if err != nil {
		t.Fatalf("沒有預算不該是錯誤:%v", err)
	}
	if got.Msg.Budget != nil {
		t.Fatal("沒有預算時 budget 應該缺席,而不是一個全 0 的訊息")
	}
	if got.Msg.GetLockedReason() == "" {
		t.Fatal("locked_reason 應該有人話說明")
	}
}

// session 綁一屆:拿上一屆的 session 問這一屆要被擋下,不是靜靜換一屆回答。
func TestListItemsRejectsSessionFromAnotherTournament(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	_, err := client.ListItems(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.ListItemsRequest{TournamentSlug: "2025-old-event"}),
		testSessionTok))
	requireCode(t, err, connect.CodePermissionDenied)
	if len(svc.itemsFor) != 0 {
		t.Fatal("被擋下的請求不該碰領域層")
	}

	got, err := client.ListItems(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.ListItemsRequest{TournamentSlug: testSlug}), testSessionTok))
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(got.Msg.GetItems()) != 1 {
		t.Fatalf("項目數 = %d", len(got.Msg.GetItems()))
	}
	if got.Msg.GetItems()[0].GetCategory() != activityv1.HandicapCategory_HANDICAP_CATEGORY_DEFENSE {
		t.Fatalf("分類轉換錯誤:%v", got.Msg.GetItems()[0].GetCategory())
	}
}

// 可見性的分流在領域層,入口層只負責把「請求者是誰」傳下去。
// 這個測試驗的就是那個值:匿名是 0,帶 session 是本人的內部 id。
func TestGetMatchHandicapsPassesViewerIdentity(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()
	req := func() *connect.Request[activityv1.GetMatchHandicapsRequest] {
		return connect.NewRequest(&activityv1.GetMatchHandicapsRequest{MatchPublicId: testMatchID})
	}

	// 匿名:看得到(封盤後公開),viewer = 0。
	got, err := client.GetMatchHandicaps(ctx, req())
	if err != nil {
		t.Fatalf("GetMatchHandicaps(匿名): %v", err)
	}
	if got.Msg.GetHandicaps().GetMatchPublicId() != testMatchID {
		t.Fatalf("match = %q", got.Msg.GetHandicaps().GetMatchPublicId())
	}

	// 帶 session:viewer = 本人的內部 id。
	if _, err := client.GetMatchHandicaps(ctx, withPlayerSession(req(), testSessionTok)); err != nil {
		t.Fatalf("GetMatchHandicaps(選手): %v", err)
	}

	// 壞掉的 session:退化成匿名而不是回錯 —— 過期的 cookie 不該讓觀眾
	// 連公開的封盤內容都看不到。
	if _, err := client.GetMatchHandicaps(ctx, withPlayerSession(req(), "壞掉的")); err != nil {
		t.Fatalf("GetMatchHandicaps(壞 session): %v", err)
	}

	want := []int64{0, testPlayerAInternal, 0}
	if len(svc.viewers) != len(want) {
		t.Fatalf("viewer 記錄 = %v", svc.viewers)
	}
	for i, w := range want {
		if svc.viewers[i] != w {
			t.Fatalf("第 %d 次的 viewer = %d,want %d", i+1, svc.viewers[i], w)
		}
	}
}

// 封盤前由非施加者查詢時,領域層把 Budget 留成 nil。
// 入口層**不能**把它補成一個全 0 的訊息 —— 那會讓對手以為預算是 0。
func TestMatchHandicapsKeepsHiddenBudgetAbsent(t *testing.T) {
	mh := &handicap.MatchHandicaps{
		MatchPublicID: testMatchID,
		Status:        "ready",
		Revealed:      false,
		Budget:        nil,
	}
	pb := matchHandicapsToProto(mh)
	if pb.Budget != nil {
		t.Fatal("封盤前非施加者不該看到任何預算數字")
	}
	if pb.LockedAt != nil {
		t.Fatal("尚未封盤時 locked_at 應該缺席")
	}
	if pb.GetStatus() != activityv1.MatchStatus_MATCH_STATUS_READY {
		t.Fatalf("status = %v", pb.GetStatus())
	}
}

func TestVoidSelectionUsesSessionPlayer(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	got, err := client.VoidSelection(context.Background(), withPlayerSession(
		connect.NewRequest(&activityv1.VoidSelectionRequest{SelectionPublicId: "01SELECTION"}),
		testSessionTok))
	if err != nil {
		t.Fatalf("VoidSelection: %v", err)
	}
	if len(svc.voids) != 1 || svc.voids[0].PlayerID != testPlayerAInternal {
		t.Fatalf("退選的選手身分錯誤:%+v", svc.voids)
	}
	if svc.voids[0].SelectionPublicID != "01SELECTION" {
		t.Fatalf("selection = %q", svc.voids[0].SelectionPublicID)
	}
	if got.Msg.GetBudget().GetRemaining() != 24 {
		t.Fatalf("退完後 remaining = %d,want 24", got.Msg.GetBudget().GetRemaining())
	}
}

// BP 預算的 remaining 由伺服器算,而且永不為負。
func TestBpBudgetRemainingNeverNegative(t *testing.T) {
	pb := bpBudgetToProto(testMatchID, handicap.Budget{
		PlayerPublicID: testPlayerAID, Budget: 8, Spent: 24,
	})
	if pb.GetRemaining() != 0 {
		t.Fatalf("remaining = %d,want 0(永不為負)", pb.GetRemaining())
	}
	_ = bp.DefaultPerRankGap
}

// 讓武項目總表是規則本身,沒報名的人與觀眾都要查得到。
//
// 這一頁是「選購時手邊查得到價目」的地方,而那正是這個系統最初要解決的混亂。
// 清單裡沒有 referee_note(裁判怎麼確認對手遵守),那一欄從不離開裁判端。
func TestListItemsIsPublicWithExplicitSlug(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	// 完全沒有 session —— 一個還在考慮要不要報名的人。
	got, err := client.ListItems(context.Background(),
		connect.NewRequest(&activityv1.ListItemsRequest{TournamentSlug: testSlug}))
	if err != nil {
		t.Fatalf("匿名應該讀得到規則:%v", err)
	}
	if len(got.Msg.GetItems()) == 0 {
		t.Fatal("回了空清單,規則頁會是一片空白")
	}
	if len(svc.itemsFor) != 1 {
		t.Fatalf("應該真的去查了本屆:%v", svc.itemsFor)
	}
}

// 沒帶 slug 又沒有身分,就真的無從得知要哪一屆。
func TestListItemsWithoutSlugStillNeedsIdentity(t *testing.T) {
	deps, svc, _ := handicapDeps()
	srv := newActivityServer(t, deps)
	client := activityv1connect.NewHandicapServiceClient(srv.Client(), srv.URL)

	_, err := client.ListItems(context.Background(),
		connect.NewRequest(&activityv1.ListItemsRequest{}))
	requireCode(t, err, connect.CodeUnauthenticated)
	if len(svc.itemsFor) != 0 {
		t.Fatal("問不出是哪一屆就不該碰領域層")
	}
}
