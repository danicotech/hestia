package transport

import (
	"context"
	"fmt"
	"math"
	"strings"

	"connectrpc.com/connect"

	activityv1 "github.com/danicotech/hestia/gen/hestia/activity/v1"
	"github.com/danicotech/hestia/internal/core/activity/betting"
)

// 觀眾投票與下注。
//
// # 這裡全部是真錢,所以身分是平台帳號而不是通行碼
//
// 下注動的是平台代幣,收款與扣款都要有帳戶,所以本服務認 Bearer 而不是
// 活動層 session —— 與 HandicapService 完全相反。選手想下注就得先
// BindPlatformAccount,那本來就是領獎的前提。
//
// # 兩件入口層必須做對的事
//
//  1. **PlaceBet 一律要 idempotency_key。** 下注沒有任何 UNIQUE 約束擋得住
//     連點(同一人對同一場下兩注是合法的),冪等鍵是唯一的權威。
//     少了它,一次網路重送就是一次重複扣款。
//  2. **GetOdds 的 viewer 只影響 my_vote。** 賠率對所有人都一樣,否則就不是
//     賠率了;而「誰投給誰」永遠只回自己那一筆(投票直接動賠率,
//     公開投票人等於公開可操縱的標的)。
//
// 兩條操守規則(選手不得賭自己、票數不揭露投票人)都在領域層擋,
// 入口層不重複實作 —— 重複的那一份遲早會比另一份寬鬆。

// ActivityBetting 是投票與下注的領域服務。
//
// betting.Service 的 TX 是型別參數(core 不能 import pgx),所以入口層吃介面
// 而不是具體型別:*betting.Service[pgx.Tx] 直接滿足它,而 transport 一行
// pgx 都不必寫。
type ActivityBetting interface {
	Vote(ctx context.Context, p betting.VoteParams) (*betting.MatchOdds, error)
	GetOdds(ctx context.Context, p betting.GetOddsParams) ([]betting.MatchOdds, error)
	PlaceBet(ctx context.Context, p betting.PlaceBetParams) (*betting.PlaceBetResult, error)
	ListMyBets(ctx context.Context, p betting.ListMyBetsParams) ([]betting.Bet, error)
}

// maxOddsQueryIDs 是一次 GetOdds 能問的場次數上限。
//
// 對戰表頁確實要一次問很多場,但「很多」的上限是一屆賽事的場次數(人數 − 1)。
// 沒有上限等於任何人都能用一個請求叫我們做任意多次查詢 ——
// 與 maxRequestBytes 同一類的防線,只是換一個維度。
const maxOddsQueryIDs = 128

type activityBettingHandler struct {
	svc ActivityBetting
}

// Vote 投票給某一方。一場一票,再投即改票。
func (h activityBettingHandler) Vote(
	ctx context.Context, req *connect.Request[activityv1.VoteRequest],
) (*connect.Response[activityv1.VoteResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("BettingService.Vote")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	matchID := strings.TrimSpace(req.Msg.GetMatchPublicId())
	if matchID == "" {
		return nil, invalidArgument("match_public_id 必填")
	}
	side, err := sideFromProto(req.Msg.GetSide())
	if err != nil {
		return nil, err
	}
	odds, err := h.svc.Vote(ctx, betting.VoteParams{
		UserID: userID, MatchPublicID: matchID, Side: side,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	// 一併回賠率:票數變了賠率就會變,讓前端再打一次 GetOdds 只會讓它
	// 顯示一個「投票前」與「投票後」中間的狀態。
	return connect.NewResponse(&activityv1.VoteResponse{Odds: matchOddsToProto(*odds)}), nil
}

// GetOdds 取目前賠率與票數。**匿名可讀** —— 觀眾不必登入就看得到盤口。
//
// 未登入時 viewer 是 0,領域層據此把 my_vote 填成 0(沒投或未登入)。
// 這裡刻意不用 requireUser:賠率是公開資訊,要求登入才看得到,
// 等於把最大的一群觀眾擋在門外。
func (h activityBettingHandler) GetOdds(
	ctx context.Context, req *connect.Request[activityv1.GetOddsRequest],
) (*connect.Response[activityv1.GetOddsResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("BettingService.GetOdds")
	}
	ids := make([]string, 0, len(req.Msg.GetMatchPublicIds()))
	for _, raw := range req.Msg.GetMatchPublicIds() {
		if id := strings.TrimSpace(raw); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, invalidArgument("match_public_ids 至少要有一個")
	}
	if len(ids) > maxOddsQueryIDs {
		return nil, invalidArgument(fmt.Sprintf("match_public_ids 最多 %d 個", maxOddsQueryIDs))
	}
	// 未登入就是 0。UserID 的第二個回傳值刻意不當成錯誤處理 ——
	// 這支 RPC 的匿名路徑是正常路徑,不是降級。
	viewerID, _ := UserID(ctx)
	odds, err := h.svc.GetOdds(ctx, betting.GetOddsParams{
		MatchPublicIDs: ids, ViewerUserID: viewerID,
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*activityv1.MatchOdds, 0, len(odds))
	for _, o := range odds {
		out = append(out, matchOddsToProto(o))
	}
	return connect.NewResponse(&activityv1.GetOddsResponse{Odds: out}), nil
}

// PlaceBet 下注。單場或串關都走這支。
func (h activityBettingHandler) PlaceBet(
	ctx context.Context, req *connect.Request[activityv1.PlaceBetRequest],
) (*connect.Response[activityv1.PlaceBetResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("BettingService.PlaceBet")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	key := strings.TrimSpace(req.Msg.GetIdempotencyKey())
	if key == "" {
		return nil, invalidArgument("idempotency_key 必填(動錢的 RPC 一律要冪等鍵)")
	}
	legs := req.Msg.GetLegs()
	if len(legs) == 0 {
		return nil, invalidArgument("legs 至少要有一腿")
	}
	in := make([]betting.LegInput, 0, len(legs))
	for i, leg := range legs {
		matchID := strings.TrimSpace(leg.GetMatchPublicId())
		if matchID == "" {
			return nil, invalidArgument(fmt.Sprintf("legs[%d].match_public_id 必填", i))
		}
		side, err := sideFromProto(leg.GetSide())
		if err != nil {
			return nil, err
		}
		in = append(in, betting.LegInput{MatchPublicID: matchID, Side: side})
	}
	// 腿數上限、重複押同一場、stake 範圍、選手賭自己 —— 全部在領域層擋。
	// 入口層只擋「請求本身組不起來」的情況。
	res, err := h.svc.PlaceBet(ctx, betting.PlaceBetParams{
		UserID:         userID,
		TournamentSlug: slug,
		Stake:          req.Msg.GetStake(),
		Legs:           in,
		IdempotencyKey: key,
		ExpectedPayout: req.Msg.GetExpectedPayout(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&activityv1.PlaceBetResponse{
		Bet: betToProto(res.Bet), Replayed: res.Replayed,
	}), nil
}

// ListMyBets 列出我的注單。永遠只列自己的 —— user id 來自認證,不從請求讀。
func (h activityBettingHandler) ListMyBets(
	ctx context.Context, req *connect.Request[activityv1.ListMyBetsRequest],
) (*connect.Response[activityv1.ListMyBetsResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("BettingService.ListMyBets")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(req.Msg.GetTournamentSlug())
	if slug == "" {
		return nil, invalidArgument("tournament_slug 必填")
	}
	bets, err := h.svc.ListMyBets(ctx, betting.ListMyBetsParams{
		UserID: userID, TournamentSlug: slug, OpenOnly: req.Msg.GetOpenOnly(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	out := make([]*activityv1.Bet, 0, len(bets))
	for _, b := range bets {
		out = append(out, betToProto(b))
	}
	return connect.NewResponse(&activityv1.ListMyBetsResponse{Bets: out}), nil
}

// ── betting.proto 的轉換 ──────────────────────────────────────

// sideFromProto 把 1/2 轉成領域的 Side。
//
// 在入口層擋掉 0 與其他值是刻意的:proto3 沒有 required,side 沒填會是 0,
// 而 0 在領域層是「未投票」的合法值。不擋的話,一個漏填 side 的請求會變成
// 一個語意不明的呼叫,而不是一個清楚的 400。
func sideFromProto(v int32) (betting.Side, error) {
	s := betting.Side(v)
	if !s.Valid() {
		return 0, invalidArgument("side 必須是 1(p1)或 2(p2)")
	}
	return s, nil
}

func betStatusToProto(s betting.BetStatus) activityv1.BetStatus {
	switch s {
	case betting.BetOpen:
		return activityv1.BetStatus_BET_STATUS_OPEN
	case betting.BetWon:
		return activityv1.BetStatus_BET_STATUS_WON
	case betting.BetLost:
		return activityv1.BetStatus_BET_STATUS_LOST
	case betting.BetVoid:
		return activityv1.BetStatus_BET_STATUS_VOID
	default:
		return activityv1.BetStatus_BET_STATUS_UNSPECIFIED
	}
}

func legResultToProto(r betting.LegResult) activityv1.LegResult {
	switch r {
	case betting.LegPending:
		return activityv1.LegResult_LEG_RESULT_PENDING
	case betting.LegWon:
		return activityv1.LegResult_LEG_RESULT_WON
	case betting.LegLost:
		return activityv1.LegResult_LEG_RESULT_LOST
	case betting.LegVoid:
		return activityv1.LegResult_LEG_RESULT_VOID
	default:
		return activityv1.LegResult_LEG_RESULT_UNSPECIFIED
	}
}

func matchOddsToProto(o betting.MatchOdds) *activityv1.MatchOdds {
	return &activityv1.MatchOdds{
		MatchPublicId: o.MatchPublicID,
		P1Votes:       clampInt32(o.P1Votes),
		P2Votes:       clampInt32(o.P2Votes),
		P1OddsMilli:   o.P1OddsMilli,
		P2OddsMilli:   o.P2OddsMilli,
		OpenForBets:   o.OpenForBets,
		MyVote:        int32(o.MyVote),
	}
}

func betToProto(b betting.Bet) *activityv1.Bet {
	legs := make([]*activityv1.BetLeg, 0, len(b.Legs))
	for _, l := range b.Legs {
		legs = append(legs, betLegToProto(l))
	}
	return &activityv1.Bet{
		PublicId:           b.PublicID,
		Stake:              b.Stake,
		PotentialPayout:    b.PotentialPayout,
		Status:             betStatusToProto(b.Status),
		Legs:               legs,
		PayoutRecalculated: b.PayoutRecalculated,
		CreatedAt:          tsPB(b.CreatedAt),
		SettledAt:          optTS(b.SettledAt),
	}
	// LedgerStakeEntryID / LedgerPayoutEntryID / LedgerRefundEntryID 刻意不出現:
	// 那是內部 BIGINT id(鐵則 5)。稽核鏈是給管理端用的,不是注單頁的內容。
}

// betLegToProto 轉一腿。
//
// match_label 目前只組得出輪次與場次序號:bet_legs 存的是 round/slot 與
// **押的那一方**的名字快照,沒有對手的名字,所以做不出 proto 註解舉例的
// 「首輪 · 李璃 vs A冷」。這是領域層的缺口,不是這裡偷懶 —— 硬要組的話
// 得在列注單時回查每一場,而那正是快照欄位要避免的事。
func betLegToProto(l betting.Leg) *activityv1.BetLeg {
	return &activityv1.BetLeg{
		MatchPublicId:   l.MatchPublicID,
		MatchLabel:      betLegLabel(l),
		Side:            int32(l.Side),
		SideDisplayName: l.SideDisplayName,
		OddsMilli:       l.OddsMilli,
		Result:          legResultToProto(l.Result),
	}
}

func betLegLabel(l betting.Leg) string {
	if l.Round <= 0 {
		return ""
	}
	return fmt.Sprintf("第 %d 輪 · 第 %d 場", l.Round, l.Slot+1)
}

// clampInt32 把票數收進 proto 的 int32。
//
// 票數在領域層是 int64(來自 COUNT(*)),契約是 int32。溢位在實務上不會發生
// (一屆賽事的觀眾不會有二十億人),但**靜默截斷**會讓票數變成負的,
// 而負票數會讓賠率算出荒謬的值。夾住而不是讓它繞回去。
func clampInt32(v int64) int32 {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < 0:
		return 0
	default:
		return int32(v)
	}
}
