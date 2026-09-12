package transport

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/gen/hestia/activity/v1/activityv1connect"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// 裁判後台 —— 御風羽幹部操作賽事的唯一入口。
//
// # 三道門,缺一不可
//
//	平台帳號    requireUser(活動層 session 在這裡**沒有**意義:
//	            裁判是平台使用者,而稽核紀錄要記在平台身分上)
//	裁判權限    Authorizer.Authorize(每一支都查,不是挑幾支)
//	稽核紀錄    由領域層與 adapter 在同一個 transaction 內寫
//
// 權限檢查在 handler,而攔截器的 privilegedServices 也會查一次 —— 這是
// 刻意保留的重複,不是忘了收斂。攔截器只在 transport.New() 建的那組
// handler option 裡,而 MountActivity 收的是 `opts ...connect.HandlerOption`:
// 少傳那組選項一樣掛得起來,而且不會有任何東西出聲。那條路徑上,handler
// 這道是唯一的把關,所以它留著。
//
// 兩邊不會漂移:兩者查的都是 authz.PermissionFor(procedure),同一張映射表。
// 映射表與 privilegedServices 對不對得起來,由 verifyProcedureCoverage 在
// New() 啟動時反推斷言(宣告了權限卻不需要授權 → 開不起來)。
//
// # confirm 原樣往下傳,不在這裡判斷
//
// 封盤、判定勝者、棄賽三個不可逆動作的 confirm 由 match.Service 檢查
// (它回 ErrConfirmationRequired)。入口層自己判斷會變成第二個權威位置,
// 而那一份遲早會在某支 RPC 上被忘記。
//
// 例外是 ConfirmBracket 與 AwardPrizes:前者的領域動作是 AdvancePhase
// (階段機沒有、也不該有 confirm 參數 —— 推進階段本身不是不可逆的),
// 後者還沒有領域服務。這兩支的 confirm 只能在這裡擋,而且擋成 InvalidArgument:
// 請求少了一個必要欄位,重送同樣的內容一定還是失敗。

// ActivityMatches 是「裁判帶一場比賽走完生命週期」的編排(match.Service)。
//
// 與 ActivityBetting 同一個理由吃介面:match.Service 的 TX 是型別參數,
// 由 infrastructure 用 pgx.Tx 實例化。
type ActivityMatches interface {
	OpenHandicap(ctx context.Context, p match.OpenHandicapParams) (*match.OpenHandicapResult, error)
	LockHandicap(ctx context.Context, p match.LockHandicapParams) (*match.LockHandicapResult, error)
	StartMatch(ctx context.Context, p match.StartMatchParams) (*match.Match, error)
	ReportResult(ctx context.Context, p match.ReportResultParams) (*match.ReportResultOutcome, error)
	WithdrawPlayer(ctx context.Context, p match.WithdrawPlayerParams) (*match.WithdrawResult, error)
	SetStreamURL(ctx context.Context, p match.SetStreamURLParams) (*match.Match, error)
}

// ActivityPrizeParams 是一次發獎請求。
type ActivityPrizeParams struct {
	TournamentSlug string
	// ActorUserID 是執行的裁判。發獎要進 admin_audit_logs。
	ActorUserID int64
	// IdempotencyKey 必填 —— 這一步真的動平台代幣。
	IdempotencyKey string
}

// ActivityPrizeAward 是一筆發獎結果。
type ActivityPrizeAward struct {
	PlayerPublicID string
	DisplayName    string
	// Kind 是 champion / runner_up / third / participation。
	Kind   string
	Amount int64
	// Granted 為 false 時 SkippedReason 說明原因
	// (目前唯一的原因是得獎者尚未綁定平台帳號)。
	Granted       bool
	SkippedReason string
}

// ActivityPrizes 是賽事獎金發放。
//
// 它是一個 port 而不是領域服務,因為**領域服務還不存在**:發獎要讀名次、
// 查綁定、走 Ledger、寫稽核,那是一個完整的用例,不是入口層能拼出來的東西。
// 在它落地之前,這個介面把契約的形狀定下來,nil 時 RPC 回 Unimplemented。
//
// Replayed 表示冪等重放(本次未實際動錢)。
type ActivityPrizes interface {
	AwardPrizes(ctx context.Context, p ActivityPrizeParams) (awards []ActivityPrizeAward, replayed bool, err error)
}

type activityJudgeHandler struct {
	tournaments ActivityTournament
	matches     ActivityMatches
	signup      ActivitySignup
	reader      ActivityReader
	prizes      ActivityPrizes
	directory   Directory
	authz       Authorizer
}

// requireJudge 是本服務取裁判身分的統一入口。
//
// authz 未注入時一律拒絕(fail closed):授權沒接上就把裁判後台開著,
// 是比功能缺席嚴重得多的問題 —— 這裡的每一支都能改變賽果或發錢。
func (h activityJudgeHandler) requireJudge(ctx context.Context, procedure string) (int64, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return 0, err
	}
	if h.authz == nil {
		return 0, toConnectError(ErrPermissionDenied)
	}
	if err := h.authz.Authorize(ctx, userID, procedure); err != nil {
		return 0, toConnectError(err)
	}
	return userID, nil
}

// AdvancePhase 推進(或退回)賽事階段。
func (h activityJudgeHandler) AdvancePhase(
	ctx context.Context, req *connect.Request[activityv1.AdvancePhaseRequest],
) (*connect.Response[activityv1.AdvancePhaseResponse], error) {
	if h.tournaments == nil || h.reader == nil {
		return nil, unimplemented("JudgeService.AdvancePhase")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	to, ok := phaseFromProto(req.Msg.GetToPhase())
	if !ok {
		return nil, invalidArgument("to_phase 必須是七個階段之一")
	}
	view, err := h.tournaments.AdvancePhase(ctx, tournament.AdvanceParams{
		Slug: slug, To: to, ActorUserID: actorID, Reason: req.Msg.GetNote(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	pb, err := h.tournamentToProto(ctx, view)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.AdvancePhaseResponse{Tournament: pb}), nil
}

// AssignRank 評定單一選手的段位。
//
// 抽籤後改段位會回 tournament.ErrRanksLocked(而不是一般的階段錯誤)——
// 那個區別對裁判很重要:它有明確的補救動作(退回 ranked 重抽),
// 而一般的「現在不能改」沒有。
func (h activityJudgeHandler) AssignRank(
	ctx context.Context, req *connect.Request[activityv1.AssignRankRequest],
) (*connect.Response[activityv1.AssignRankResponse], error) {
	if h.tournaments == nil || h.reader == nil || h.directory == nil {
		return nil, unimplemented("JudgeService.AssignRank")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	playerID := strings.TrimSpace(req.Msg.GetPlayerPublicId())
	if playerID == "" {
		return nil, invalidArgument("player_public_id 必填")
	}
	rank := rankFromProto(req.Msg.GetRank())
	if !rank.Valid() {
		return nil, invalidArgument("rank 必須是開山 / 斷水 / 飛花 / 無我 之一")
	}
	t, err := h.reader.TournamentByPlayer(ctx, playerID)
	if err != nil {
		return nil, toConnectError(err)
	}
	player, err := h.tournaments.SetRank(ctx, tournament.SetRankParams{
		Slug:           t.Slug,
		PlayerPublicID: playerID,
		Rank:           rank,
		ActorUserID:    actorID,
		Reason:         req.Msg.GetNote(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// 裁判永遠看得到段位(他就是評的人),所以 showRank 恆為 true ——
	// ranksPublished 管的是**對外**公布,不是後台。
	pb, err := playerWithAccountToProto(ctx, h.directory, *player, true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.AssignRankResponse{Player: pb}), nil
}

// ListUnranked 列出待評段的選手與他們的報名資料。
//
// PlayerDossier 是**只有裁判看得到**的檢視:報名表的內容(自評段位、論劍積分、
// 實戰經驗、可出賽時段)不在任何公開 RPC 裡出現。
func (h activityJudgeHandler) ListUnranked(
	ctx context.Context, req *connect.Request[activityv1.ListUnrankedRequest],
) (*connect.Response[activityv1.ListUnrankedResponse], error) {
	if h.tournaments == nil || h.reader == nil || h.directory == nil {
		return nil, unimplemented("JudgeService.ListUnranked")
	}
	if _, err := h.requireJudge(ctx, req.Spec().Procedure); err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	view, err := h.tournaments.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	players, err := h.reader.ListPlayers(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	includeRanked := req.Msg.GetIncludeRanked()
	wanted := make([]tournament.Player, 0, len(players))
	fencerIDs := make([]int64, 0, len(players))
	for _, p := range players {
		if p.Ranked() && !includeRanked {
			continue
		}
		wanted = append(wanted, p)
		fencerIDs = append(fencerIDs, p.FencerID)
	}
	fencers, err := h.reader.Fencers(ctx, fencerIDs)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*activityv1.PlayerDossier, 0, len(wanted))
	for _, p := range wanted {
		pb, err := playerWithAccountToProto(ctx, h.directory, p, true)
		if err != nil {
			return nil, err
		}
		out = append(out, dossierToProto(pb, p, fencers[p.FencerID]))
	}
	return connect.NewResponse(&activityv1.ListUnrankedResponse{Players: out}), nil
}

// DrawBracket 隨機抽籤。確認前可重複呼叫以重抽。
//
// 回傳的 seed 要給裁判存證:被質疑「這籤是不是喬過的」時,拿同一個種子
// 重跑一次會得到位元相同的對戰表。那是程式唯一能自證清白的辦法。
func (h activityJudgeHandler) DrawBracket(
	ctx context.Context, req *connect.Request[activityv1.DrawBracketRequest],
) (*connect.Response[activityv1.DrawBracketResponse], error) {
	if h.tournaments == nil || h.reader == nil {
		return nil, unimplemented("JudgeService.DrawBracket")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	res, err := h.tournaments.Draw(ctx, tournament.DrawParams{
		Slug:        slug,
		Seed:        drawSeedFromProto(req.Msg.GetSeed()),
		ActorUserID: actorID,
		Reason:      "",
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	rounds, err := h.bracketRounds(ctx, slug)
	if err != nil {
		return nil, err
	}
	// DrawResult 只帶內部 id,對外一律換成 public_id(鐵則 5)。
	publicByID := make(map[int64]string, len(res.Players))
	for _, p := range res.Players {
		publicByID[p.ID] = p.PublicID
	}
	byes := make([]string, 0, len(res.ByePlayerIDs))
	for _, id := range res.ByePlayerIDs {
		if pub, ok := publicByID[id]; ok {
			byes = append(byes, pub)
		}
	}
	return connect.NewResponse(&activityv1.DrawBracketResponse{
		Rounds:             rounds,
		Seed:               drawSeedToProto(res.Seed),
		ByePlayerPublicIds: byes,
	}), nil
}

// SwapSeeds 交換兩人的籤位。
//
// 請求只帶兩個選手 public_id(ULID 全域唯一,不需要 slug 消歧義),
// 而領域服務要 slug,所以先反查。兩人不同屆時領域層會查不到第二個人而失敗 ——
// 那正是想要的結果,不必在這裡再擋一次。
func (h activityJudgeHandler) SwapSeeds(
	ctx context.Context, req *connect.Request[activityv1.SwapSeedsRequest],
) (*connect.Response[activityv1.SwapSeedsResponse], error) {
	if h.tournaments == nil || h.reader == nil {
		return nil, unimplemented("JudgeService.SwapSeeds")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	a := strings.TrimSpace(req.Msg.GetPlayerAPublicId())
	b := strings.TrimSpace(req.Msg.GetPlayerBPublicId())
	if a == "" || b == "" {
		return nil, invalidArgument("player_a_public_id 與 player_b_public_id 必填")
	}
	t, err := h.reader.TournamentByPlayer(ctx, a)
	if err != nil {
		return nil, toConnectError(err)
	}
	if _, _, err := h.tournaments.SwapSeeds(ctx, tournament.SwapParams{
		Slug:            t.Slug,
		PlayerAPublicID: a,
		PlayerBPublicID: b,
		ActorUserID:     actorID,
		Reason:          req.Msg.GetNote(),
	}); err != nil {
		return nil, toConnectError(err)
	}
	rounds, err := h.bracketRounds(ctx, t.Slug)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.SwapSeedsResponse{Rounds: rounds}), nil
}

// ConfirmBracket 確認對戰表並公布。
//
// 「確認」不是一個獨立的旗標,而是階段推進 drawing → in_progress ——
// 離開 drawing 就等於鎖定籤位。少一個 confirmed 欄位,就少一個
// 「旗標說確認了但階段還在 drawing」的矛盾狀態(tournament.Draw 的註解同此)。
func (h activityJudgeHandler) ConfirmBracket(
	ctx context.Context, req *connect.Request[activityv1.ConfirmBracketRequest],
) (*connect.Response[activityv1.ConfirmBracketResponse], error) {
	if h.tournaments == nil || h.reader == nil {
		return nil, unimplemented("JudgeService.ConfirmBracket")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	if !req.Msg.GetConfirm() {
		return nil, invalidArgument("confirm 必須為 true(確認後籤位鎖定並公布)")
	}
	view, err := h.tournaments.AdvancePhase(ctx, tournament.AdvanceParams{
		Slug:        slug,
		To:          tournament.PhaseInProgress,
		ActorUserID: actorID,
		Reason:      "確認對戰表",
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	pb, err := h.tournamentToProto(ctx, view)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.ConfirmBracketResponse{Tournament: pb}), nil
}

// OpenHandicap 開盤:讓該場的低段位方可以開始選讓武。
func (h activityJudgeHandler) OpenHandicap(
	ctx context.Context, req *connect.Request[activityv1.OpenHandicapRequest],
) (*connect.Response[activityv1.OpenHandicapResponse], error) {
	if h.matches == nil {
		return nil, unimplemented("JudgeService.OpenHandicap")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	res, err := h.matches.OpenHandicap(ctx, match.OpenHandicapParams{
		MatchPublicID: matchID, ActorUserID: actorID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// res.Budget 為 nil = 同段對決,本場無讓武。契約上沒有這個欄位,
	// 前端要知道的話去問 GetMatch 的 handicap_budget(那是公開資訊)。
	return connect.NewResponse(&activityv1.OpenHandicapResponse{
		Match: matchToProto(res.Match),
	}), nil
}

// LockHandicap 封盤。**不可逆**:選手不能再改,內容立刻公開並發 Discord 公告。
//
// confirm 原樣往下傳 —— 二次確認的權威在 match.Service(它回
// ErrConfirmationRequired),入口層不自己判斷。
func (h activityJudgeHandler) LockHandicap(
	ctx context.Context, req *connect.Request[activityv1.LockHandicapRequest],
) (*connect.Response[activityv1.LockHandicapResponse], error) {
	if h.matches == nil {
		return nil, unimplemented("JudgeService.LockHandicap")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	res, err := h.matches.LockHandicap(ctx, match.LockHandicapParams{
		MatchPublicID: matchID,
		Confirm:       req.Msg.GetConfirm(),
		ActorUserID:   actorID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&activityv1.LockHandicapResponse{
		Match:     matchToProto(res.Match),
		Handicaps: matchHandicapsToProto(res.Handicaps),
	}), nil
}

// SetStreamUrl 設定直播連結。空字串 = 清掉。
func (h activityJudgeHandler) SetStreamUrl(
	ctx context.Context, req *connect.Request[activityv1.SetStreamUrlRequest],
) (*connect.Response[activityv1.SetStreamUrlResponse], error) {
	if h.matches == nil {
		return nil, unimplemented("JudgeService.SetStreamUrl")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	m, err := h.matches.SetStreamURL(ctx, match.SetStreamURLParams{
		MatchPublicID: matchID,
		StreamURL:     strings.TrimSpace(req.Msg.GetStreamUrl()),
		ActorUserID:   actorID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&activityv1.SetStreamUrlResponse{Match: matchToProto(*m)}), nil
}

// StartMatch 標記開打。這一步同時關閉下注。
func (h activityJudgeHandler) StartMatch(
	ctx context.Context, req *connect.Request[activityv1.StartMatchRequest],
) (*connect.Response[activityv1.StartMatchResponse], error) {
	if h.matches == nil {
		return nil, unimplemented("JudgeService.StartMatch")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	m, err := h.matches.StartMatch(ctx, match.StartMatchParams{
		MatchPublicID: matchID, ActorUserID: actorID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&activityv1.StartMatchResponse{Match: matchToProto(*m)}), nil
}

// ReportResult 判定勝負。**不可逆**:觸發晉級與下注結算。
func (h activityJudgeHandler) ReportResult(
	ctx context.Context, req *connect.Request[activityv1.ReportResultRequest],
) (*connect.Response[activityv1.ReportResultResponse], error) {
	if h.matches == nil {
		return nil, unimplemented("JudgeService.ReportResult")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	winnerID := strings.TrimSpace(req.Msg.GetWinnerPlayerPublicId())
	if winnerID == "" {
		return nil, invalidArgument("winner_player_public_id 必填")
	}
	out, err := h.matches.ReportResult(ctx, match.ReportResultParams{
		MatchPublicID:        matchID,
		WinnerPlayerPublicID: winnerID,
		Confirm:              req.Msg.GetConfirm(),
		ActorUserID:          actorID,
		Reason:               req.Msg.GetNote(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&activityv1.ReportResultResponse{
		Match:           matchToProto(out.Match),
		AdvancedMatches: matchesToProto(out.AdvancedMatches),
		SettledBetCount: int32(out.SettledBetCount),
	}), nil
}

// WithdrawPlayer 選手棄賽。**不可逆**:對手不戰而勝,該場所有注單作廢退款。
func (h activityJudgeHandler) WithdrawPlayer(
	ctx context.Context, req *connect.Request[activityv1.WithdrawPlayerRequest],
) (*connect.Response[activityv1.WithdrawPlayerResponse], error) {
	if h.matches == nil || h.reader == nil || h.directory == nil {
		return nil, unimplemented("JudgeService.WithdrawPlayer")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	playerID := strings.TrimSpace(req.Msg.GetPlayerPublicId())
	if playerID == "" {
		return nil, invalidArgument("player_public_id 必填")
	}
	res, err := h.matches.WithdrawPlayer(ctx, match.WithdrawPlayerParams{
		PlayerPublicID: playerID,
		Confirm:        req.Msg.GetConfirm(),
		ActorUserID:    actorID,
		Reason:         req.Msg.GetNote(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// 重讀完整的選手列:match.Player 只帶場上要用的欄位,而回應的是
	// 公開的 Player 訊息(要有遊戲ID、籤位、綁定的平台帳號)。
	full, err := h.reader.PlayerByPublicID(ctx, res.Player.TournamentID, res.Player.PublicID)
	if err != nil {
		return nil, toConnectError(err)
	}
	pb, err := playerWithAccountToProto(ctx, h.directory, full, true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&activityv1.WithdrawPlayerResponse{
		Player:          pb,
		WalkoverMatches: matchesToProto(res.WalkoverMatches),
		VoidedBetCount:  int32(res.VoidedBetCount),
	}), nil
}

// RegeneratePasscode 重新產生通行碼。舊碼立即失效。
//
// 「立即失效」不需要撤銷清單:passcode_hash 被覆寫的那一刻,舊碼就再也
// 比對不過。回傳的明碼要由裁判私訊給本人 —— 它**只在這裡出現一次**,
// 不進稽核紀錄也不進任何日誌。
func (h activityJudgeHandler) RegeneratePasscode(
	ctx context.Context, req *connect.Request[activityv1.RegeneratePasscodeRequest],
) (*connect.Response[activityv1.RegeneratePasscodeResponse], error) {
	if h.signup == nil || h.reader == nil {
		return nil, unimplemented("JudgeService.RegeneratePasscode")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	playerID := strings.TrimSpace(req.Msg.GetPlayerPublicId())
	if playerID == "" {
		return nil, invalidArgument("player_public_id 必填")
	}
	t, err := h.reader.TournamentByPlayer(ctx, playerID)
	if err != nil {
		return nil, toConnectError(err)
	}
	res, err := h.signup.RegeneratePasscode(ctx, signup.RegenerateParams{
		TournamentSlug: t.Slug,
		PlayerPublicID: playerID,
		ActorUserID:    actorID,
		Reason:         "裁判重新產生通行碼",
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// 回應裡刻意只有通行碼:多回一個 Player 訊息不會讓裁判多知道什麼,
	// 卻讓這個「唯一一次吐明碼」的回應變成一個會被順手記進 log 的大物件。
	return connect.NewResponse(&activityv1.RegeneratePasscodeResponse{
		Passcode: res.Passcode,
	}), nil
}

// AwardPrizes 發放賽事獎金。
//
// 這一步真的動平台代幣,所以 idempotency_key 必填(帳本鐵則 4),
// 而且需要 confirm。得獎者尚未綁定平台帳號時該筆 granted = false ——
// 獎金走 Ledger,沒有收款對象就發不出去,但那不該讓整批發獎失敗。
func (h activityJudgeHandler) AwardPrizes(
	ctx context.Context, req *connect.Request[activityv1.AwardPrizesRequest],
) (*connect.Response[activityv1.AwardPrizesResponse], error) {
	if h.prizes == nil {
		return nil, unimplemented("JudgeService.AwardPrizes")
	}
	actorID, err := h.requireJudge(ctx, req.Spec().Procedure)
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	if !req.Msg.GetConfirm() {
		return nil, invalidArgument("confirm 必須為 true(這一步會實際動平台代幣)")
	}
	key := strings.TrimSpace(req.Msg.GetIdempotencyKey())
	if key == "" {
		return nil, invalidArgument("idempotency_key 必填(動錢的 RPC 一律要冪等鍵)")
	}
	awards, replayed, err := h.prizes.AwardPrizes(ctx, ActivityPrizeParams{
		TournamentSlug: slug, ActorUserID: actorID, IdempotencyKey: key,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*activityv1.PrizeAward, 0, len(awards))
	for _, a := range awards {
		out = append(out, &activityv1.PrizeAward{
			PlayerPublicId: a.PlayerPublicID,
			DisplayName:    a.DisplayName,
			PrizeKind:      a.Kind,
			Amount:         a.Amount,
			Granted:        a.Granted,
			SkippedReason:  a.SkippedReason,
		})
	}
	return connect.NewResponse(&activityv1.AwardPrizesResponse{
		Awards: out, Replayed: replayed,
	}), nil
}

// ── 內部輔助 ────────────────────────────────────────────────────

// bracketRounds 重讀對戰表組成分輪檢視。
//
// 為什麼抽完籤還要再讀一次:DrawResult 帶的是 MatchSeat(round/slot/選手內部 id),
// 場次的 public_id 由 adapter 在寫入時產生,不在結果裡。對外只能出現 public_id
// (鐵則 5),所以這一次回讀不是多餘的 —— 它是唯一拿得到 public_id 的路徑。
func (h activityJudgeHandler) bracketRounds(
	ctx context.Context, slug string,
) ([]*activityv1.BracketRound, error) {
	view, err := h.tournaments.Get(ctx, slug)
	if err != nil {
		return nil, toConnectError(err)
	}
	matches, err := h.reader.ListMatches(ctx, view.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return bracketRoundsToProto(matches), nil
}

func (h activityJudgeHandler) tournamentToProto(
	ctx context.Context, v *tournament.View,
) (*activityv1.Tournament, error) {
	count, err := h.reader.CountPlayers(ctx, v.Tournament.ID)
	if err != nil {
		return nil, toConnectError(err)
	}
	return tournamentToProto(v, count), nil
}

func matchesToProto(ms []match.Match) []*activityv1.Match {
	out := make([]*activityv1.Match, 0, len(ms))
	for _, m := range ms {
		out = append(out, matchToProto(m))
	}
	return out
}

// dossierToProto 組出評段用的完整檢視。
//
// fencer 是跨屆檔案;查不到時給零值而不是失敗 —— 一個 fencers 列暫時讀不到
// 不該讓整份待評名單打不開,那會直接卡住評段流程。
func dossierToProto(
	player *activityv1.Player, p tournament.Player, f signup.Fencer,
) *activityv1.PlayerDossier {
	return &activityv1.PlayerDossier{
		Player:            player,
		DiscordName:       p.DiscordName,
		SelfRatedRank:     rankToProto(p.SelfRatedRank),
		LadderRank:        p.LadderRank,
		LadderScore:       p.LadderScore,
		ArtsNote:          p.ArtsNote,
		AvailabilityNote:  p.AvailabilityNote,
		PreviousRank:      rankToProto(f.LastRankLevel),
		TournamentsPlayed: f.TournamentsPlayed,
		Wins:              f.Wins,
		Losses:            f.Losses,
	}
}

// drawSeedFromProto 把契約上的 int64 種子轉成領域的 16 位 hex 字串。
//
// 0 代表「伺服器自己產一個」,對應 DrawParams.Seed 的空字串。
// 用 uint64 當中介是因為 DrawSeed 本來就是 uint64:contract 上寫 int64
// 只是因為 protobuf 沒有 uint64 的 JSON 友善表示,兩者的位元完全相同。
func drawSeedFromProto(seed int64) string {
	if seed == 0 {
		return ""
	}
	return tournament.DrawSeed(uint64(seed)).String()
}

func drawSeedToProto(seed tournament.DrawSeed) int64 {
	return int64(uint64(seed))
}

// 編譯期斷言:五個 handler 必須完整實作 proto 契約。
//
// 集中在同一處而不是散在五個檔案的最後一行:少一支方法或簽名打錯時,
// 錯誤會指在這裡,而不是在 MountActivity 那一行變成一大段型別推導訊息。
var (
	_ activityv1connect.SignupServiceHandler     = activitySignupHandler{}
	_ activityv1connect.TournamentServiceHandler = activityTournamentHandler{}
	_ activityv1connect.HandicapServiceHandler   = activityHandicapHandler{}
	_ activityv1connect.BettingServiceHandler    = activityBettingHandler{}
	_ activityv1connect.JudgeServiceHandler      = activityJudgeHandler{}
)
