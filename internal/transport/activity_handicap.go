package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
)

// 讓武 BP 的選購 ——《百業試鋒》的核心玩法。
//
// # 這個服務為什麼不用平台帳號
//
// 買讓武的是選手,而選手未必有平台帳號(報名不需要)。所以除了
// GetMatchHandicaps,每一支都認活動層 session cookie,不認 Bearer。
//
// # PlayerID 永遠來自 session,永遠不從請求裡讀
//
// handicap.SelectParams.PlayerID 的註解已經寫了理由:讓請求指定施加者
// 等於讓任何人花別人的 BP。入口層是唯一能犯這個錯的地方,所以這裡
// 沒有任何一行從 req.Msg 取選手身分 —— 那是刻意的,不是漏掉欄位。
//
// # 封盤前後的可見性由領域層守
//
// GetMatchHandicaps 的分流(封盤前只回請求者自己的、封盤後全部公開)
// 完全在 handicap.Service.MatchHandicaps 裡。入口層只做一件事:
// 把請求者解成 viewerPlayerID(匿名就是 0)交下去。
// 在這裡多寫一次「如果還沒封盤就過濾掉別人的」會是第二個權威位置,
// 而兩份可見性規則裡遲早有一份會比另一份寬鬆。

// ActivityHandicap 是讓武選購的領域服務(*handicap.Service 直接滿足)。
type ActivityHandicap interface {
	ListItems(ctx context.Context, tournamentID int64) ([]handicap.Item, error)
	MyBudget(ctx context.Context, matchPublicID string, playerID int64) (*handicap.MyBudget, error)
	Select(ctx context.Context, p handicap.SelectParams) (*handicap.SelectResult, error)
	VoidSelection(ctx context.Context, p handicap.VoidParams) (*handicap.Budget, error)
	MatchHandicaps(ctx context.Context, matchPublicID string, viewerPlayerID int64) (*handicap.MatchHandicaps, error)
}

// errActivitySessionWrongTournament 是「這個 session 不屬於你請求的那一屆」。
//
// 通行碼是逐屆發的,session 也綁一屆。回 PermissionDenied 而不是靜靜改用
// session 的那一屆:前端送錯 slug 時,默默給它另一屆的資料是最難查的那種 bug。
var errActivitySessionWrongTournament = errors.New("目前的選手身分不屬於這屆賽事")

type activityHandicapHandler struct {
	svc         ActivityHandicap
	tournaments ActivityTournament
	reader      ActivityReader
	sessions    ActivitySessions
}

// ListItems 列出本屆所有可購買的讓武項目。
//
// # 為什麼帶了 slug 就不需要身分
//
// 這份清單**就是規則**:名稱、說明、BP 價格。規則頁要讓還沒報名的人、
// 以及觀眾都查得到 —— 這場活動的目的本來就是「闔家觀賞」,而選手在選讓武
// 的時候必須手邊就查得到價目,那是這個系統最初要解決的混亂。
//
// 它不含 referee_note(裁判怎麼確認對手遵守),那一欄從不離開裁判端。
//
// 沒帶 slug 時仍然需要身分:那是選手端的便利寫法(「我這一屆的項目」),
// 而「我」只能從 session 來。
func (h activityHandicapHandler) ListItems(
	ctx context.Context, req *connect.Request[activityv1.ListItemsRequest],
) (*connect.Response[activityv1.ListItemsResponse], error) {
	if h.svc == nil || h.tournaments == nil {
		return nil, unimplemented("HandicapService.ListItems")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	// 有 session 就仍然比對它屬於哪一屆。這一道不是為了擋人 —— 沒有 session
	// 的人本來就讀得到 —— 是為了擋**前端送錯 slug**:對一個已登入的選手
	// 默默顯示另一屆的價格,是最難查的那種 bug。
	id, idErr := requireActivityIdentity(ctx, h.sessions, req.Header())
	switch {
	case slug == "":
		// 選手端的便利寫法(「我這一屆的項目」),而「我」只能從 session 來。
		if idErr != nil {
			return nil, idErr
		}
		slug = id.TournamentSlug
	case idErr == nil && slug != id.TournamentSlug:
		return nil, connectError(connect.CodePermissionDenied, errActivitySessionWrongTournament)
	}
	view, err := h.tournaments.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	items, err := h.svc.ListItems(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*activityv1.HandicapItem, 0, len(items))
	for _, it := range items {
		out = append(out, handicapItemToProto(it))
	}
	return connect.NewResponse(&activityv1.ListItemsResponse{Items: out}), nil
}

// GetMyBudget 取我在某場的 BP 預算與已選項目。
//
// 沒有預算不是錯誤(同段對決、或我是高段位方):領域層回 HasBudget = false
// 加一句人話說明,前端顯示「本場無讓武」。入口層不把它變成 404 ——
// 「查不到」與「本來就沒有」對使用者是兩件完全不同的事。
func (h activityHandicapHandler) GetMyBudget(
	ctx context.Context, req *connect.Request[activityv1.GetMyBudgetRequest],
) (*connect.Response[activityv1.GetMyBudgetResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("HandicapService.GetMyBudget")
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	playerID, err := h.currentPlayerID(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	mine, err := h.svc.MyBudget(ctx, matchID, playerID)
	if err != nil {
		return nil, toConnectError(err)
	}
	res := &activityv1.GetMyBudgetResponse{
		Editable:     mine.Editable,
		LockedReason: mine.LockedReason,
		Selections:   selectionsToProto(mine.Selections),
	}
	if mine.HasBudget {
		res.Budget = bpBudgetToProto(matchID, mine.Budget)
	}
	return connect.NewResponse(res), nil
}

// Select 買一項讓武。
//
// 刻意沒有 idempotency_key:BP 不是代幣(每輪重發、賽後作廢、不可交易),
// 重複扣一次的後果是使用者看到數字不對,裁判封盤前退掉即可 ——
// 不是對不上帳。動真錢的 PlaceBet 與 AwardPrizes 才有冪等鍵。
func (h activityHandicapHandler) Select(
	ctx context.Context, req *connect.Request[activityv1.SelectRequest],
) (*connect.Response[activityv1.SelectResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("HandicapService.Select")
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	itemID := strings.TrimSpace(req.Msg.GetItemPublicId())
	if itemID == "" {
		return nil, invalidArgument("item_public_id 必填")
	}
	playerID, err := h.currentPlayerID(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: matchID,
		PlayerID:      playerID,
		ItemRef:       itemID,
		TargetNote:    req.Msg.GetTargetNote(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// advisory 非空**不代表失敗**:victory 類互相矛盾由裁判臨場判,
	// 系統只提醒不阻擋(擋一半會讓選手誤以為沒被擋的都相容)。
	return connect.NewResponse(&activityv1.SelectResponse{
		Selection: handicapSelectionToProto(res.Selection),
		Budget:    bpBudgetToProto(matchID, res.Budget),
		Advisory:  res.Advisory,
	}), nil
}

// VoidSelection 退掉一項。只在封盤前可行。
func (h activityHandicapHandler) VoidSelection(
	ctx context.Context, req *connect.Request[activityv1.VoidSelectionRequest],
) (*connect.Response[activityv1.VoidSelectionResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("HandicapService.VoidSelection")
	}
	selectionID := strings.TrimSpace(req.Msg.GetSelectionPublicId())
	if selectionID == "" {
		return nil, invalidArgument("selection_public_id 必填")
	}
	playerID, err := h.currentPlayerID(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	budget, err := h.svc.VoidSelection(ctx, handicap.VoidParams{
		SelectionPublicID: selectionID,
		PlayerID:          playerID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// match_public_id 留空:handicap.Budget 沒有這個欄位,而請求只帶
	// selection_public_id,入口層無從得知是哪一場(見回報的領域層缺口)。
	// 寧可留空也不要猜一個 —— 猜錯的 id 會讓前端去查一場不存在的比賽。
	return connect.NewResponse(&activityv1.VoidSelectionResponse{
		Budget: bpBudgetToProto("", *budget),
	}), nil
}

// GetMatchHandicaps 取某場的讓武全貌。
//
// 這是本服務唯一**匿名可讀**的 RPC:封盤後的讓武內容就是 Discord 公告的
// 內容,觀眾本來就看得到。封盤前的過濾在領域層,入口層只負責把
// 「請求者是誰」傳下去 —— 匿名時那個答案是 0。
func (h activityHandicapHandler) GetMatchHandicaps(
	ctx context.Context, req *connect.Request[activityv1.GetMatchHandicapsRequest],
) (*connect.Response[activityv1.GetMatchHandicapsResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("HandicapService.GetMatchHandicaps")
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	// 沒有 session、或 session 壞掉,一律當匿名 —— 過期的 cookie 不該讓
	// 觀眾連公開的封盤內容都看不到。
	var viewerPlayerID int64
	if id, ok := optionalActivityIdentity(ctx, h.sessions, req.Header()); ok {
		if pid, err := h.playerID(ctx, id); err == nil {
			viewerPlayerID = pid
		}
	}
	mh, err := h.svc.MatchHandicaps(ctx, matchID, viewerPlayerID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&activityv1.GetMatchHandicapsResponse{
		Handicaps: matchHandicapsToProto(mh),
	}), nil
}

// currentPlayerID 把活動層 session 解成內部選手 id。
//
// 內部 id 不在 token 裡(鐵則 5),所以每次都要用 public_id 反查。
// 那不只是為了守鐵則:選手被刪、棄賽、或換了一屆之後,舊 token 會在
// 這一步自然失效,不需要另一張撤銷清單。
func (h activityHandicapHandler) currentPlayerID(ctx context.Context, header http.Header) (int64, error) {
	id, err := requireActivityIdentity(ctx, h.sessions, header)
	if err != nil {
		return 0, err
	}
	return h.playerID(ctx, id)
}

func (h activityHandicapHandler) playerID(ctx context.Context, id ActivityIdentity) (int64, error) {
	if h.tournaments == nil || h.reader == nil {
		return 0, unimplemented("活動層選手查詢")
	}
	_, player, err := resolveActivityPlayer(ctx, h.tournaments, h.reader, id)
	if err != nil {
		return 0, err
	}
	return player.ID, nil
}

// ── handicap.proto 的轉換 ──────────────────────────────────────

func handicapItemToProto(i handicap.Item) *activityv1.HandicapItem {
	return &activityv1.HandicapItem{
		PublicId:    i.Ref,
		Category:    handicapCategoryToProto(i.Category),
		Name:        i.Name,
		Description: i.Description,
		Cost:        i.Cost,
		Repeatable:  i.Repeatable,
		// RequiresTargetNote 是推導值不是欄位(規則在 handicap/items.go),
		// 所以問領域型別而不是讀資料庫的某一欄。
		RequiresTargetNote: i.RequiresTargetNote(),
		SortOrder:          i.SortOrder,
	}
	// RefereeNote 刻意不出現:那是給裁判看的執行說明,不是選手端的內容。
}

func handicapSelectionToProto(s handicap.Selection) *activityv1.HandicapSelection {
	return &activityv1.HandicapSelection{
		PublicId:     s.PublicID,
		ItemPublicId: s.ItemRef,
		ItemName:     s.ItemName,
		Category:     handicapCategoryToProto(s.Category),
		Cost:         s.Cost,
		TargetNote:   s.TargetNote,
		CreatedAt:    tsPB(s.CreatedAt),
	}
}

func selectionsToProto(sels []handicap.Selection) []*activityv1.HandicapSelection {
	out := make([]*activityv1.HandicapSelection, 0, len(sels))
	for _, s := range sels {
		out = append(out, handicapSelectionToProto(s))
	}
	return out
}

// bpBudgetToProto 組出預算訊息。
//
// remaining 由伺服器算好給(domain 的 Budget.Remaining(),永不為負),
// 不讓前端自己減:兩邊各算一次,遲早會有一邊在「花超過」的邊界上算出負數。
//
// matchPublicID 由呼叫端給:handicap.Budget 存的是內部 match id,
// 而對外只能出現 public_id。
func bpBudgetToProto(matchPublicID string, b handicap.Budget) *activityv1.BpBudget {
	return &activityv1.BpBudget{
		MatchPublicId:  matchPublicID,
		PlayerPublicId: b.PlayerPublicID,
		Budget:         b.Budget,
		Spent:          b.Spent,
		Remaining:      b.Remaining(),
	}
}

func matchHandicapsToProto(mh *handicap.MatchHandicaps) *activityv1.MatchHandicaps {
	if mh == nil {
		return nil
	}
	out := &activityv1.MatchHandicaps{
		MatchPublicId:             mh.MatchPublicID,
		Status:                    matchStatusStringToProto(mh.Status),
		Selections:                selectionsToProto(mh.Selections),
		ConstrainedPlayerPublicId: mh.ConstrainedPlayerPublicID,
		LockedAt:                  optTS(mh.LockedAt),
	}
	// Budget 為 nil 有兩種來源,而且都不該被補成 0:本場無讓武(同段對決),
	// 或封盤前由非施加者查詢(對手連「已經花了多少」都不該知道)。
	if mh.Budget != nil {
		out.Budget = bpBudgetToProto(mh.MatchPublicID, *mh.Budget)
	}
	return out
}
