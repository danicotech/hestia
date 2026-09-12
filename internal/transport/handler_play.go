package transport

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/play"
)

// 小遊戲、開箱、抽獎、寵物的入口(schemas/25)。
//
// svc 未注入時全部回 Unimplemented —— 與其他 handler 一致:
// 功能缺席比「看起來能用但什麼都不會發生」好得多。
type playHandler struct {
	svc play.Service
	// dir 把內部 user id 解成對外 public_id(鐵則 5)。
	dir Directory
	// communities 決定這次操作屬於哪個社群。
	//
	// 目前由「全庫唯一的社群」推定 —— 事件還沒帶發生地(schemas/22 A 方案),
	// 所以這裡沒有更好的資訊。兩個以上社群時實作會明確失敗而不是猜。
	communities CommunityResolver
}

func (h playHandler) PlayGame(
	ctx context.Context, req *connect.Request[platformv1.PlayGameRequest],
) (*connect.Response[platformv1.PlayGameResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.PlayGame")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	communityID, err := h.communityFor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.PlayGame(ctx, play.GameParams{
		UserID:         userID,
		CommunityID:    communityID,
		Game:           req.Msg.GetGame(),
		Choice:         req.Msg.GetChoice(),
		Stake:          req.Msg.GetStake(),
		IdempotencyKey: req.Msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.PlayGameResponse{
		Game: res.Game, Choice: res.Choice, Result: res.Result,
		Stake: res.Stake, Returned: res.Payout, Balance: res.Balance, Seed: res.Seed,
	}), nil
}

func (h playHandler) ListLootBoxes(
	ctx context.Context, _ *connect.Request[platformv1.ListLootBoxesRequest],
) (*connect.Response[platformv1.ListLootBoxesResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.ListLootBoxes")
	}
	if _, err := requireUser(ctx); err != nil {
		return nil, err
	}
	communityID, err := h.communityFor(ctx)
	if err != nil {
		return nil, err
	}
	boxes, err := h.svc.ListBoxes(ctx, communityID)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := &platformv1.ListLootBoxesResponse{
		Boxes: make([]*platformv1.LootBox, 0, len(boxes)),
	}
	for _, b := range boxes {
		out.Boxes = append(out.Boxes, &platformv1.LootBox{
			PublicId: b.PublicID, Name: b.Name,
			Currency: b.Currency, Cost: b.Cost, DailyLimit: b.DailyLimit,
		})
	}
	return connect.NewResponse(out), nil
}

func (h playHandler) OpenLootBox(
	ctx context.Context, req *connect.Request[platformv1.OpenLootBoxRequest],
) (*connect.Response[platformv1.OpenLootBoxResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.OpenLootBox")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.OpenBox(ctx, play.OpenParams{
		UserID:         userID,
		BoxPublicID:    req.Msg.GetBoxPublicId(),
		IdempotencyKey: req.Msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.OpenLootBoxResponse{
		BoxName: res.BoxName, RewardKind: res.RewardKind, RewardName: res.RewardName,
		Amount: res.Amount, Balance: res.Balance, Seed: res.Seed,
	}), nil
}

func (h playHandler) ListGiveaways(
	ctx context.Context, _ *connect.Request[platformv1.ListGiveawaysRequest],
) (*connect.Response[platformv1.ListGiveawaysResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.ListGiveaways")
	}
	if _, err := requireUser(ctx); err != nil {
		return nil, err
	}
	communityID, err := h.communityFor(ctx)
	if err != nil {
		return nil, err
	}
	list, err := h.svc.ListGiveaways(ctx, communityID)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := &platformv1.ListGiveawaysResponse{
		Giveaways: make([]*platformv1.Giveaway, 0, len(list)),
	}
	for _, g := range list {
		out.Giveaways = append(out.Giveaways, &platformv1.Giveaway{
			PublicId: g.PublicID, Title: g.Title, EntryCost: g.EntryCost,
			WinnerCount: g.WinnerCount, Entries: g.Entries,
			ClosesAt: timestamppb.New(g.ClosesAt),
		})
	}
	return connect.NewResponse(out), nil
}

func (h playHandler) EnterGiveaway(
	ctx context.Context, req *connect.Request[platformv1.EnterGiveawayRequest],
) (*connect.Response[platformv1.EnterGiveawayResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.EnterGiveaway")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.EnterGiveaway(ctx, userID, req.Msg.GetGiveawayPublicId()); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.EnterGiveawayResponse{}), nil
}

// DrawGiveaway 需要授權(它把獎品發出去)。授權由攔截器依
// privilegedProcedures 判定,這裡不重複檢查 —— 兩處檢查會有一處先過期。
func (h playHandler) DrawGiveaway(
	ctx context.Context, req *connect.Request[platformv1.DrawGiveawayRequest],
) (*connect.Response[platformv1.DrawGiveawayResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.DrawGiveaway")
	}
	if _, err := requireUser(ctx); err != nil {
		return nil, err
	}
	res, err := h.svc.DrawGiveaway(ctx, req.Msg.GetGiveawayPublicId())
	if err != nil {
		return nil, toConnectError(err)
	}
	out := &platformv1.DrawGiveawayResponse{
		Title:   res.Title,
		Winners: make([]*platformv1.GiveawayWinner, 0, len(res.Winners)),
	}
	for _, w := range res.Winners {
		out.Winners = append(out.Winners, &platformv1.GiveawayWinner{
			UserPublicId: w.UserPublicID, DisplayName: w.DisplayName,
		})
	}
	return connect.NewResponse(out), nil
}

func (h playHandler) ListPets(
	ctx context.Context, _ *connect.Request[platformv1.ListPetsRequest],
) (*connect.Response[platformv1.ListPetsResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.ListPets")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	pets, err := h.svc.ListPets(ctx, userID)
	if err != nil {
		return nil, toConnectError(err)
	}
	out := &platformv1.ListPetsResponse{Pets: make([]*platformv1.Pet, 0, len(pets))}
	for _, p := range pets {
		out.Pets = append(out.Pets, &platformv1.Pet{
			PublicId: p.PublicID, Name: p.Name, Rarity: p.Rarity, IconUrl: p.IconURL,
			Xp: p.XP, Level: p.Level, Deployed: p.Deployed,
		})
	}
	return connect.NewResponse(out), nil
}

func (h playHandler) DeployPet(
	ctx context.Context, req *connect.Request[platformv1.DeployPetRequest],
) (*connect.Response[platformv1.DeployPetResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.DeployPet")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.DeployPet(ctx, userID, req.Msg.GetPetPublicId()); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.DeployPetResponse{}), nil
}

func (h playHandler) RenamePet(
	ctx context.Context, req *connect.Request[platformv1.RenamePetRequest],
) (*connect.Response[platformv1.RenamePetResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("PlayService.RenamePet")
	}
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.RenamePet(ctx, userID, req.Msg.GetPetPublicId(), req.Msg.GetNickname()); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&platformv1.RenamePetResponse{}), nil
}

// communityFor 決定這次操作屬於哪個社群。
func (h playHandler) communityFor(ctx context.Context) (int64, error) {
	if h.communities == nil {
		return 0, unimplemented("社群解析未注入")
	}
	id, err := h.communities.SoleCommunityID(ctx)
	if err != nil {
		return 0, toConnectError(err)
	}
	return id, nil
}
