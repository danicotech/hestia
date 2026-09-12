package transport

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/bracket"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 《百業試鋒》活動層的入口:五個 ConnectRPC 服務接到 core/activity 的領域服務。
//
// # 這個檔案為什麼同時放「對戰表 handler」與「共用轉換函式」
//
// common.proto 的訊息(Tournament / Player / Match / BracketRound / 讓武項目……)
// 五個服務都會回,轉換只能有一份 —— 兩份轉換遲早會有一份漏掉某個欄位,
// 而那種錯誤在回應裡看起來只是「某個欄位是空的」,沒有人會當成 bug。
// TournamentService 是這些訊息的主要產地,所以共用轉換住在這裡。
//
// # 三種身分,不是一種
//
//	選手      遊戲ID + 通行碼 → 活動層 session cookie(activity_signup.go)
//	下注/投票  平台帳號 Bearer(既有的認證攔截器)
//	裁判      平台帳號 Bearer + 裁判權限(activity_judge.go)
//
// TournamentService 是唯一**全部匿名可讀**的服務:這場活動的目的是
// 「闔家觀賞、強弱同樂」,看的人比打的人多,要求觀眾登入才看得到對戰表
// 會把最大的一群人擋在門外。

// ActivityTournament 是賽事階段與抽籤的領域服務(*tournament.Service 直接滿足)。
//
// 在使用端宣告介面而不是直接吃 *tournament.Service:transport 只需要這五個
// 方法,而測試需要一個不碰資料庫的替身。這與 ports.go 的其餘 port 同一個理由。
type ActivityTournament interface {
	Get(ctx context.Context, slug string) (*tournament.View, error)
	AdvancePhase(ctx context.Context, p tournament.AdvanceParams) (*tournament.View, error)
	SetRank(ctx context.Context, p tournament.SetRankParams) (*tournament.Player, error)
	Draw(ctx context.Context, p tournament.DrawParams) (*tournament.DrawResult, error)
	SwapSeeds(ctx context.Context, p tournament.SwapParams) (*tournament.Player, *tournament.Player, error)
}

// ActivityReader 是活動層的**讀取側**。
//
// # 為什麼它是一個 port 而不是領域服務的方法
//
// core/activity 目前只有寫入路徑:tournament.Service 能推階段、能抽籤,
// 但沒有任何方法回答「這屆有誰報名」「對戰表長什麼樣」「這場是誰對誰」。
// 那些是純查詢,SQL 一句就夠,沒有任何規則需要保護 —— 硬塞進領域服務
// 只會讓它多背一堆與階段機無關的方法。
//
// 回傳型別一律是既有的領域型別(tournament.Player / match.Match / signup.Fencer),
// 刻意**不在 transport 另造一套 view struct**:那會是同一個概念的第二個權威位置,
// 而且日後 core 補上讀取側時,adapter 得同時滿足兩套形狀。
//
// 實作在 storage(與 tournamentpg / matchpg 同一個 pool),由 cmd/server 注入;
// nil 時相關 RPC 回 Unimplemented 而不是 panic(與 Deps 的其餘欄位同一個約定)。
type ActivityReader interface {
	// TournamentByID 依內部 id 讀一屆賽事。查無回 tournament.ErrTournamentNotFound。
	TournamentByID(ctx context.Context, tournamentID int64) (tournament.Tournament, error)
	// TournamentByPlayer 依選手 public_id 反查他所屬的那一屆。
	//
	// 存在的理由是契約:AssignRank / SwapSeeds / RegeneratePasscode 的請求只帶
	// player_public_id(選手 public_id 是 ULID,全域唯一,不需要 slug 去消歧義),
	// 而領域服務的參數要 slug。這條反查就是那個落差,放在入口層解。
	TournamentByPlayer(ctx context.Context, playerPublicID string) (tournament.Tournament, error)
	// CountPlayers 數本屆報名人數(Tournament.player_count)。
	CountPlayers(ctx context.Context, tournamentID int64) (int32, error)
	// ListPlayers 列出本屆全部參賽者(含已淘汰、已棄賽),依籤位、再依報名序排序。
	ListPlayers(ctx context.Context, tournamentID int64) ([]tournament.Player, error)
	// PlayerByPublicID 在本屆內以 public_id 查一位參賽者。查無回 tournament.ErrPlayerNotFound。
	PlayerByPublicID(ctx context.Context, tournamentID int64, playerPublicID string) (tournament.Player, error)
	// ListMatches 列出本屆全部場次,依 (round, slot) 遞增。尚未抽籤時回空切片。
	ListMatches(ctx context.Context, tournamentID int64) ([]match.Match, error)
	// MatchByPublicID 讀單場。查無回 activityerr.ErrMatchNotFound。
	MatchByPublicID(ctx context.Context, matchPublicID string) (match.Match, error)
	// CurrentMatchOfPlayer 回這位選手目前輪到要打的場次;nil = 沒有待打的場次
	// (已出局、已棄賽、或還沒抽籤)。
	CurrentMatchOfPlayer(ctx context.Context, playerID int64) (*match.Match, error)
	// Fencers 依 fencer id 批次取跨屆檔案(裁判評段要看歷屆戰績)。
	// 查不到的 id 不出現在回傳的 map 裡,不是錯誤。
	Fencers(ctx context.Context, fencerIDs []int64) (map[int64]signup.Fencer, error)
}

// ActivityDeps 是活動層五個服務的全部依賴,由 cmd/server 組好後交給 MountActivity。
//
// 與 Deps 一樣:每個欄位都可以是 nil,對應的 RPC 回 Unimplemented 而不是 panic。
// 契約先到、實作陸續補上因此是安全的常態。
type ActivityDeps struct {
	// Signup 是報名與選手身分(*signup.Service)。
	Signup ActivitySignup
	// Tournament 是階段機與抽籤(*tournament.Service)。
	Tournament ActivityTournament
	// Handicap 是讓武選購(*handicap.Service)。
	Handicap ActivityHandicap
	// Betting 是投票與下注(*betting.Service[pgx.Tx])。
	Betting ActivityBetting
	// Matches 是裁判帶一場比賽走完生命週期(*match.Service[pgx.Tx])。
	Matches ActivityMatches
	// Reader 是活動層讀取側(見 ActivityReader)。
	Reader ActivityReader
	// Prizes 是賽事獎金發放;nil = JudgeService.AwardPrizes 回 Unimplemented。
	Prizes ActivityPrizes
	// TournamentCreator 是「開一屆新賽事」。刻意與 Tournament 分開:
	// 公開的 TournamentService 持有 Tournament,不該在型別上也拿得到
	// 建賽事的能力(見 ActivityTournamentCreator)。
	TournamentCreator ActivityTournamentCreator

	// Sessions 簽發與驗證活動層 session(見 ActivitySessions)。
	// nil = 所有需要選手身分的 RPC 回 Unimplemented —— 不是「放行」。
	Sessions ActivitySessions

	// Directory 把內部 user id 換回 public_id(Player.user_public_id,鐵則 5)。
	Directory Directory

	// Authorizer 判定裁判權限。nil = JudgeService 一律 PermissionDenied(fail closed)。
	Authorizer Authorizer

	// BasePath 是服務的掛載前綴,決定活動層 session cookie 的 Path。
	// 值必須與 transport.New 用的同一份(已由 NormalizeBasePath 正規化)。
	BasePath string
}

// MountActivity 把五個活動層服務掛上 mux。
//
// 做成一個函式而不是讓組裝端自己 new 五個 handler:handler 型別是未匯出的
// (與平台層一致),而且「哪些服務屬於活動層」應該只有一個清單。
//
// opts 要傳與平台層**同一組**攔截器(recover / OTel / event_logs / 認證)——
// 活動層不該有自己的一套稽核與復原規則。
func MountActivity(mux *http.ServeMux, deps ActivityDeps, opts ...connect.HandlerOption) {
	cookie := newActivitySessionCookie(deps.BasePath)
	mux.Handle(activityv1connect.NewSignupServiceHandler(activitySignupHandler{
		svc: deps.Signup, tournaments: deps.Tournament, reader: deps.Reader,
		sessions: deps.Sessions, cookie: cookie, directory: deps.Directory,
	}, opts...))
	mux.Handle(activityv1connect.NewTournamentServiceHandler(activityTournamentHandler{
		svc: deps.Tournament, reader: deps.Reader, directory: deps.Directory,
	}, opts...))
	mux.Handle(activityv1connect.NewHandicapServiceHandler(activityHandicapHandler{
		svc: deps.Handicap, tournaments: deps.Tournament, reader: deps.Reader,
		sessions: deps.Sessions,
	}, opts...))
	mux.Handle(activityv1connect.NewBettingServiceHandler(activityBettingHandler{
		svc: deps.Betting,
	}, opts...))
	mux.Handle(activityv1connect.NewJudgeServiceHandler(activityJudgeHandler{
		tournaments: deps.Tournament, matches: deps.Matches, signup: deps.Signup,
		reader: deps.Reader, prizes: deps.Prizes, directory: deps.Directory,
		authz: deps.Authorizer, creator: deps.TournamentCreator,
	}, opts...))
}

// activityTournamentHandler 是賽事的公開檢視。五支 RPC 全部匿名可讀 ——
// 這裡沒有任何一行 requireUser,那是刻意的,不是漏掉。
type activityTournamentHandler struct {
	svc       ActivityTournament
	reader    ActivityReader
	directory Directory
}

// GetTournament 取一屆賽事的基本資訊與當前階段。
func (h activityTournamentHandler) GetTournament(
	ctx context.Context, req *connect.Request[activityv1.GetTournamentRequest],
) (*connect.Response[activityv1.GetTournamentResponse], error) {
	if h.svc == nil || h.reader == nil {
		return nil, unimplemented("TournamentService.GetTournament")
	}
	slug := strings.TrimSpace(req.Msg.GetSlug())
	if slug == "" {
		return nil, invalidArgument("slug 必填")
	}
	view, err := h.svc.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	pb, err := h.tournamentToProto(ctx, view)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.GetTournamentResponse{Tournament: pb}), nil
}

// ListRanks 取段位定義。名稱與境界描述存在 tournaments.config(逐屆可調措辭),
// 所以由伺服器給 —— 前端寫死中文的話,改一次措辭就要改兩個 repo。
func (h activityTournamentHandler) ListRanks(
	ctx context.Context, req *connect.Request[activityv1.ListRanksRequest],
) (*connect.Response[activityv1.ListRanksResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("TournamentService.ListRanks")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	view, err := h.svc.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	// view.ConfigErr 非 nil 表示 config 有壞欄位、已逐項退回預設值,
	// Config 仍然可用(ParseConfig 的契約)。這裡刻意不因此失敗 ——
	// 一份打錯的 config 不該讓規則頁整個打不開。
	out := make([]*activityv1.RankInfo, 0, len(view.Config.Ranks))
	for _, ri := range view.Config.Ranks {
		out = append(out, rankInfoToProto(ri))
	}
	return connect.NewResponse(&activityv1.ListRanksResponse{
		Ranks:        out,
		BpPerRankGap: view.Config.BPPerRankGap,
	}), nil
}

// ListPlayers 列出參賽者。
//
// ranks_published 由階段決定而不是「看有沒有人有段位」:評段中就已經有一半的人
// 有段位了,那時候公布等於提前洩漏。前端靠這個旗標決定要不要顯示段位欄。
func (h activityTournamentHandler) ListPlayers(
	ctx context.Context, req *connect.Request[activityv1.ListPlayersRequest],
) (*connect.Response[activityv1.ListPlayersResponse], error) {
	if h.svc == nil || h.reader == nil || h.directory == nil {
		return nil, unimplemented("TournamentService.ListPlayers")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	view, err := h.svc.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	players, err := h.reader.ListPlayers(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	published := ranksPublished(view.Tournament.Phase)
	out, err := playersWithAccountsToProto(ctx, h.directory, players, published)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.ListPlayersResponse{
		Players: out, RanksPublished: published,
	}), nil
}

// GetBracket 取完整對戰表,按輪分組。
//
// 分組在伺服器做而不是讓前端 group by:手機版是**分輪檢視**而不是縮小的完整樹
// (視覺規範硬規則 3),而輪次名稱(「八強」還是「四強」)取決於總輪數,
// 前端手上沒有那個資訊。
func (h activityTournamentHandler) GetBracket(
	ctx context.Context, req *connect.Request[activityv1.GetBracketRequest],
) (*connect.Response[activityv1.GetBracketResponse], error) {
	if h.svc == nil || h.reader == nil || h.directory == nil {
		return nil, unimplemented("TournamentService.GetBracket")
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	view, err := h.svc.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	matches, err := h.reader.ListMatches(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	if len(matches) == 0 {
		// 尚未抽籤。回空的 rounds 加上一個明確的旗標,而不是 404 ——
		// 賽事存在、只是還沒抽,前端要顯示的是「尚未抽籤」不是「找不到」。
		return connect.NewResponse(&activityv1.GetBracketResponse{BracketPublished: false}), nil
	}
	players, err := h.reader.ListPlayers(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	inBracket := playersInMatches(players, matches)
	pbPlayers, err := playersWithAccountsToProto(ctx, h.directory, inBracket, ranksPublished(view.Tournament.Phase))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.GetBracketResponse{
		Rounds:           bracketRoundsToProto(matches),
		Players:          pbPlayers,
		BracketPublished: true,
	}), nil
}

// GetMatch 取單場詳情。
//
// handicap_budget 是公開資訊:段位在 RANKED 就公布了,BP 由段位差直接推得出來,
// 藏起來只會讓觀眾看不懂為什麼某方能施加三項限制。真正要保護的是
// 「買了什麼」,那在 HandicapService(封盤前只回自己的)。
func (h activityTournamentHandler) GetMatch(
	ctx context.Context, req *connect.Request[activityv1.GetMatchRequest],
) (*connect.Response[activityv1.GetMatchResponse], error) {
	if h.reader == nil || h.directory == nil {
		return nil, unimplemented("TournamentService.GetMatch")
	}
	publicID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if publicID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	m, err := h.reader.MatchByPublicID(ctx, publicID)
	if err != nil {
		return nil, toConnectError(err)
	}
	t, err := h.reader.TournamentByID(ctx, m.TournamentID)
	if err != nil {
		return nil, toConnectError(err)
	}
	cfg, _ := tournament.ParseConfig(t.ConfigRaw)

	res := &activityv1.GetMatchResponse{Match: matchToProto(m)}
	if p1, err := h.fullPlayer(ctx, m.TournamentID, m.P1); err != nil {
		return nil, err
	} else if p1 != nil {
		res.P1 = p1
	}
	if p2, err := h.fullPlayer(ctx, m.TournamentID, m.P2); err != nil {
		return nil, err
	} else if p2 != nil {
		res.P2 = p2
	}
	// bp.Holder 在任一方未評段時回錯。那不是使用者的問題(對戰表在評段前
	// 根本抽不出來),所以這裡只當成「本場還算不出 BP」,不讓整支 RPC 失敗。
	if budget, holderIsP1, err := bp.Holder(m.P1.Rank, m.P2.Rank, cfg.BPPerRankGap); err == nil && budget > 0 {
		res.HandicapBudget = budget
		if holderIsP1 {
			res.HandicapHolderPublicId = m.P1.PublicID
		} else {
			res.HandicapHolderPublicId = m.P2.PublicID
		}
	}
	return connect.NewResponse(res), nil
}

// fullPlayer 把場上的一方補成完整的 Player 訊息(含遊戲ID、籤位、綁定的平台帳號)。
//
// match.Player 刻意只帶場上需要的欄位,而觀眾頁要顯示的比那多。
// 未就位(等上一輪)時回 nil,對應 proto 的 optional。
func (h activityTournamentHandler) fullPlayer(
	ctx context.Context, tournamentID int64, p match.Player,
) (*activityv1.Player, error) {
	if !p.Seated() {
		return nil, nil
	}
	full, err := h.reader.PlayerByPublicID(ctx, tournamentID, p.PublicID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return playerWithAccountToProto(ctx, h.directory, full, true)
}

// playersWithAccountsToProto 把一批選手轉成 proto,並把綁定的內部 user id
// 換成 public_id(鐵則 5)。
//
// 做成套件層函式而不是 handler 的方法:五個服務都要組 Player 訊息,
// 綁在某一個 handler 上就會有第二份拷貝出現在別的檔案裡。
//
// showRank = false 時段位一律回 UNSPECIFIED:段位未公布時,連「這個人已經
// 被評過了」都不該從回應裡看得出來 —— 那是評段進度,不是公開資訊。
func playersWithAccountsToProto(
	ctx context.Context, dir Directory, players []tournament.Player, showRank bool,
) ([]*activityv1.Player, error) {
	out := make([]*activityv1.Player, 0, len(players))
	for _, p := range players {
		pb, err := playerWithAccountToProto(ctx, dir, p, showRank)
		if err != nil {
			return nil, err
		}
		out = append(out, pb)
	}
	return out, nil
}

func playerWithAccountToProto(
	ctx context.Context, dir Directory, p tournament.Player, showRank bool,
) (*activityv1.Player, error) {
	userPublicID := ""
	if p.Bound() {
		id, err := dir.UserPublicID(ctx, *p.UserID)
		if err != nil {
			return nil, toConnectError(err)
		}
		userPublicID = id
	}
	return playerToProto(p, userPublicID, showRank), nil
}

// tournamentToProto 組出賽事訊息。報名人數要多查一次,所以不是純轉換函式。
func (h activityTournamentHandler) tournamentToProto(
	ctx context.Context, v *tournament.View,
) (*activityv1.Tournament, error) {
	count, err := h.reader.CountPlayers(ctx, v.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return tournamentToProto(v, count), nil
}

// ── common.proto 的共用轉換(五個服務共用,只此一份)────────────────

// ranksPublished 回報段位是否已對外公布。
//
// 刻意以階段為準:ranked 之後才算公布(那個階段的存在理由就是「公布後、
// 抽籤前,讓選手有異議可提」)。用「有沒有人已經有段位」來判斷的話,
// 評段進行到一半就會開始外洩。
func ranksPublished(p tournament.Phase) bool {
	switch p {
	case tournament.PhaseRanked, tournament.PhaseDrawing,
		tournament.PhaseInProgress, tournament.PhaseFinished:
		return true
	default:
		return false
	}
}

func tournamentToProto(v *tournament.View, playerCount int32) *activityv1.Tournament {
	return &activityv1.Tournament{
		PublicId:     v.Tournament.PublicID,
		Slug:         v.Tournament.Slug,
		Name:         v.Tournament.Name,
		Phase:        phaseToProto(v.Tournament.Phase),
		BpPerRankGap: v.Config.BPPerRankGap,
		PlayerCount:  playerCount,
		CreatedAt:    tsPB(v.Tournament.CreatedAt),
	}
}

func rankInfoToProto(ri tournament.RankInfo) *activityv1.RankInfo {
	return &activityv1.RankInfo{
		Rank:        rankToProto(ri.Rank),
		Name:        ri.Name,
		Title:       ri.Title,
		Description: ri.Description,
	}
}

func playerToProto(p tournament.Player, userPublicID string, showRank bool) *activityv1.Player {
	rank := activityv1.Rank_RANK_UNSPECIFIED
	if showRank {
		rank = rankToProto(p.Rank)
	}
	return &activityv1.Player{
		PublicId:     p.PublicID,
		DisplayName:  p.DisplayName,
		GameId:       p.GameID,
		Rank:         rank,
		Status:       playerStatusToProto(p.Status),
		SeedNo:       p.SeedNo,
		UserPublicId: userPublicID,
	}
}

// 這裡刻意**沒有** matchPlayerToProto(從 match.Player 直接組 Player 訊息)。
//
// match.Player 只帶場上要用的欄位,少了遊戲ID、籤位與綁定的平台帳號。
// 用它組回應的話,那三個欄位會永遠是空的,而呼叫端分不出「這個人沒綁帳號」
// 與「這條路徑查不到」。所以場上兩側一律用 public_id 回查完整的那一列
// (activityTournamentHandler.fullPlayer),多一次查詢換一個誠實的回應。

func matchToProto(m match.Match) *activityv1.Match {
	out := &activityv1.Match{
		PublicId:     m.PublicID,
		Round:        int32(m.Round),
		Slot:         int32(m.Slot),
		Status:       matchStatusToProto(m.Status),
		ResultKind:   resultKindToProto(m.ResultKind),
		HandicapOpen: m.HandicapOpen,
		StreamUrl:    m.StreamURL,
		StartedAt:    optTS(m.StartedAt),
		FinishedAt:   optTS(m.FinishedAt),
	}
	if m.P1.Seated() {
		out.P1PlayerPublicId = m.P1.PublicID
	}
	if m.P2.Seated() {
		out.P2PlayerPublicId = m.P2.PublicID
	}
	// 勝者以 public_id 呈現(鐵則 5)。winner_player_id 對不上場上兩人時
	// 留空 —— 那是資料異常,寧可少一個欄位也不要吐一個內部 id 出去。
	switch m.WinnerPlayerID {
	case 0:
	case m.P1.ID:
		out.WinnerPlayerPublicId = m.P1.PublicID
	case m.P2.ID:
		out.WinnerPlayerPublicId = m.P2.PublicID
	}
	return out
}

// bracketRoundsToProto 把依 (round, slot) 排好的場次切成分輪檢視。
//
// 總輪數取自場次的最大 round,再交給 bracket.Shape 推出輪次名稱 ——
// 「八強」還是「四強」取決於樹的大小,那個規則的權威在 bracket 套件,
// 不在這裡重寫一份。
func bracketRoundsToProto(matches []match.Match) []*activityv1.BracketRound {
	if len(matches) == 0 {
		return nil
	}
	total := 0
	for _, m := range matches {
		if m.Round > total {
			total = m.Round
		}
	}
	shape := bracket.Shape(total)

	byRound := map[int][]*activityv1.Match{}
	rounds := make([]int, 0, total)
	for _, m := range matches {
		if _, ok := byRound[m.Round]; !ok {
			rounds = append(rounds, m.Round)
		}
		byRound[m.Round] = append(byRound[m.Round], matchToProto(m))
	}
	sort.Ints(rounds)

	out := make([]*activityv1.BracketRound, 0, len(rounds))
	for _, r := range rounds {
		label := ""
		if shape != nil {
			label = shape.RoundLabel(r)
		}
		out = append(out, &activityv1.BracketRound{
			Round:   int32(r),
			Label:   label,
			Matches: byRound[r],
		})
	}
	return out
}

// playersInMatches 篩出真的出現在對戰表上的選手。
//
// 為什麼不直接回全部報名者:報名截止後棄賽、或抽籤時已經退出的人不在樹上,
// 把他們一起回去會讓前端的「對戰表裡的選手」清單多出幾個找不到節點的名字。
func playersInMatches(players []tournament.Player, matches []match.Match) []tournament.Player {
	seen := make(map[int64]struct{}, len(matches)*2)
	for _, m := range matches {
		if m.P1.Seated() {
			seen[m.P1.ID] = struct{}{}
		}
		if m.P2.Seated() {
			seen[m.P2.ID] = struct{}{}
		}
	}
	out := make([]tournament.Player, 0, len(seen))
	for _, p := range players {
		if _, ok := seen[p.ID]; ok {
			out = append(out, p)
		}
	}
	return out
}

// rankToProto:proto 的列舉值**刻意等於** bp.Rank 與資料庫的 rank_level,
// 所以這裡是一次範圍檢查加型別轉換,不是查表。三處一致才不會在轉換層出錯。
func rankToProto(r bp.Rank) activityv1.Rank {
	if !r.Valid() {
		return activityv1.Rank_RANK_UNSPECIFIED
	}
	return activityv1.Rank(r)
}

// rankFromProto 的反向同理。UNSPECIFIED 對應 bp.RankUnspecified(尚未評定),
// 是合法輸入 —— 報名表的自評段位可以不填。
func rankFromProto(r activityv1.Rank) bp.Rank {
	switch r {
	case activityv1.Rank_RANK_KAISHAN, activityv1.Rank_RANK_DUANSHUI,
		activityv1.Rank_RANK_FEIHUA, activityv1.Rank_RANK_WUWO:
		return bp.Rank(r)
	default:
		return bp.RankUnspecified
	}
}

// phaseToProto / phaseFromProto 是七階段的對照。
//
// 用 switch 而不是「字面值順序剛好一致」的算術:Phase 的權威表示法是字串
// (等於 DB 的 CHECK 值域),proto 是數字,兩者沒有任何機制保證順序會一直對齊。
func phaseToProto(p tournament.Phase) activityv1.TournamentPhase {
	switch p {
	case tournament.PhaseSignup:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_SIGNUP
	case tournament.PhaseSignupClosed:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_SIGNUP_CLOSED
	case tournament.PhaseRanking:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_RANKING
	case tournament.PhaseRanked:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_RANKED
	case tournament.PhaseDrawing:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_DRAWING
	case tournament.PhaseInProgress:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_IN_PROGRESS
	case tournament.PhaseFinished:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_FINISHED
	default:
		return activityv1.TournamentPhase_TOURNAMENT_PHASE_UNSPECIFIED
	}
}

func phaseFromProto(p activityv1.TournamentPhase) (tournament.Phase, bool) {
	switch p {
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_SIGNUP:
		return tournament.PhaseSignup, true
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_SIGNUP_CLOSED:
		return tournament.PhaseSignupClosed, true
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_RANKING:
		return tournament.PhaseRanking, true
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_RANKED:
		return tournament.PhaseRanked, true
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_DRAWING:
		return tournament.PhaseDrawing, true
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_IN_PROGRESS:
		return tournament.PhaseInProgress, true
	case activityv1.TournamentPhase_TOURNAMENT_PHASE_FINISHED:
		return tournament.PhaseFinished, true
	default:
		return "", false
	}
}

func playerStatusToProto(s tournament.PlayerStatus) activityv1.PlayerStatus {
	switch s {
	case tournament.PlayerActive:
		return activityv1.PlayerStatus_PLAYER_STATUS_ACTIVE
	case tournament.PlayerEliminated:
		return activityv1.PlayerStatus_PLAYER_STATUS_ELIMINATED
	case tournament.PlayerWithdrawn:
		return activityv1.PlayerStatus_PLAYER_STATUS_WITHDRAWN
	default:
		return activityv1.PlayerStatus_PLAYER_STATUS_UNSPECIFIED
	}
}

// matchStatusToProto 收字串而不是 match.Status:讓武那邊的 Match.Status 是
// 純字串(handicap 刻意不另造一個枚舉,見它的套件註解),兩邊共用這一個函式。
func matchStatusToProto(s match.Status) activityv1.MatchStatus {
	return matchStatusStringToProto(string(s))
}

func matchStatusStringToProto(s string) activityv1.MatchStatus {
	switch match.Status(s) {
	case match.StatusPending:
		return activityv1.MatchStatus_MATCH_STATUS_PENDING
	case match.StatusReady:
		return activityv1.MatchStatus_MATCH_STATUS_READY
	case match.StatusLocked:
		return activityv1.MatchStatus_MATCH_STATUS_LOCKED
	case match.StatusLive:
		return activityv1.MatchStatus_MATCH_STATUS_LIVE
	case match.StatusDone:
		return activityv1.MatchStatus_MATCH_STATUS_DONE
	default:
		return activityv1.MatchStatus_MATCH_STATUS_UNSPECIFIED
	}
}

func resultKindToProto(k match.ResultKind) activityv1.MatchResultKind {
	switch k {
	case match.ResultNormal:
		return activityv1.MatchResultKind_MATCH_RESULT_KIND_NORMAL
	case match.ResultWalkover:
		return activityv1.MatchResultKind_MATCH_RESULT_KIND_WALKOVER
	default:
		return activityv1.MatchResultKind_MATCH_RESULT_KIND_UNSPECIFIED
	}
}

func handicapCategoryToProto(c handicap.Category) activityv1.HandicapCategory {
	switch c {
	case handicap.CategoryWeapon:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_WEAPON
	case handicap.CategorySkill:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_SKILL
	case handicap.CategoryDefense:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_DEFENSE
	case handicap.CategoryPoison:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_POISON
	case handicap.CategoryVictory:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_VICTORY
	case handicap.CategoryRule:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_RULE
	default:
		return activityv1.HandicapCategory_HANDICAP_CATEGORY_UNSPECIFIED
	}
}
