package betting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/danicotech/hestia/internal/core/activity/activityerr"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// DefaultCurrency 是下注用的幣別(platform.currencies 的 seed 只有這一種)。
// 留成可設定是因為未來多幣別時這裡不該再改一次。
const DefaultCurrency = "coin"

// refTypeBet 是分錄的 ref_type:下注/派彩/退款的分錄都指回注單。
// 稽核鏈的另一半是 bets.ledger_stake_entry_id / ledger_refund_entry_id,兩邊互指。
const refTypeBet = "bet"

// 冪等鍵命名空間。
//
// 客戶端送來的鍵加前綴後才進 idempotency_keys —— 不加前綴的話,
// 一個惡意客戶端只要送 "bet.payout:01ABC" 當自己的鍵,就能佔走某張注單的派彩鍵,
// 讓那次派彩變成「重放」而不動錢。前綴讓兩個命名空間結構上不可能相撞。
const (
	placeKeyPrefix = "bet.place:api:"
	// ledgerKeySuffix 是帳本派生鍵的字尾。派生鍵不構成第二套權威:
	// 注單這邊的鍵才是權威,帳本那把只是同一次請求在帳本側的身分。
	ledgerKeySuffix = ":ledger"
	// 結算/退款的內部決定性鍵。一張注單只會被派彩一次、退款一次,
	// 鍵就直接用注單的 public_id —— 不需要也不該讓呼叫端傳鍵進來。
	payoutKeyPrefix = "bet.payout:"
	refundKeyPrefix = "bet.refund:"
)

// LegInput 是要押的一腿:哪個盤口的哪個結果。
//
// 場次由盤口反推(proto 的 BetLegInput 只帶 market_public_id)。MatchPublicID 可選:
// 有帶就驗「盤口屬於那一場」(schemas/21「程式另驗 market.match_id == leg.match_id」),
// 對不上回 ErrMarketNotFound;沒帶就不驗。「同一場只能一腿」以反推出的場次為單位擋。
type LegInput struct {
	MarketPublicID string
	Outcome        Outcome
	// MatchPublicID 可選,見上。
	MatchPublicID string
}

// VoteParams 是一次投票:對某個盤口投一個結果。MatchPublicID 可選,語意同 LegInput。
type VoteParams struct {
	UserID         int64
	MarketPublicID string
	Outcome        Outcome
	MatchPublicID  string
}

// GetOddsParams 是一次賠率查詢。
type GetOddsParams struct {
	MatchPublicIDs []string
	// ViewerUserID 0 = 匿名。只影響回傳的 MyVote,不影響賠率 ——
	// 賠率對所有人都一樣,否則就不是賠率了。
	ViewerUserID int64
}

// PlaceBetParams 是一次下注。
type PlaceBetParams struct {
	UserID         int64
	TournamentSlug string
	// Stake 是本金。串關的所有腿共用這一筆,不是各押各的。
	Stake int64
	Legs  []LegInput
	// IdempotencyKey 必填。下注沒有任何 UNIQUE 約束擋得住連點
	// (同一人對同一場下兩注是合法的),所以冪等鍵是唯一的權威。
	IdempotencyKey string
	// ExpectedPayout 是客戶端算出的預期賠付,0 = 不檢查。
	ExpectedPayout int64
}

// PlaceBetResult 是下注的結果;冪等重放時原樣回傳(JSON 存在冪等鍵裡)。
type PlaceBetResult struct {
	Bet Bet `json:"bet"`
	// Replayed = true 表示冪等重放,本次未實際扣款。不序列化 ——
	// 它描述的是「這次呼叫」,不是「那張注單」。
	Replayed bool `json:"-"`
}

// ListMyBetsParams 是列自己的注單。永遠只列自己的。
type ListMyBetsParams struct {
	UserID         int64
	TournamentSlug string
	OpenOnly       bool
}

// BetOutcome 是一張注單在結算/退款後的結果。
type BetOutcome struct {
	BetID       int64
	BetPublicID string
	Status      BetStatus
	// Amount 是本次實際動的錢(派彩或退款)。0 = 這張沒動錢(輸掉、或仍 open)。
	Amount int64
	// LedgerEntryID 是對應的分錄 id;0 = 沒動錢。
	LedgerEntryID int64
	// PayoutRecalculated = 這張的賠付因有腿作廢而重算過。
	PayoutRecalculated bool
}

// SettleResult 是一次結算(一回合、一整場、或棄賽退款)的完整結果。
type SettleResult struct {
	MatchPublicID string
	// MarketsSettled / MarketsVoided 是這次標成 settled / void 的盤口數。
	MarketsSettled int
	MarketsVoided  int
	LegsWon        int
	LegsLost       int
	LegsVoid       int
	Bets           []BetOutcome
}

// Service 是投票與下注的核心邏輯。TX 由 infrastructure 實例化成 pgx.Tx。
type Service[TX any] struct {
	repo     Repository[TX]
	led      Ledger[TX]
	currency string
	// newPublicID 可注入,測試才有辦法斷言確定的 public_id。
	newPublicID func() (string, error)
}

// NewService 建立服務。currency 留空時用 DefaultCurrency。
func NewService[TX any](repo Repository[TX], led Ledger[TX], currency string) *Service[TX] {
	if currency == "" {
		currency = DefaultCurrency
	}
	return &Service[TX]{repo: repo, led: led, currency: currency, newPublicID: ulid.New}
}

// ── 投票 ──────────────────────────────────────────────────────────

// Vote 對一個盤口投一個結果。一盤口一票,再投即改票(UPSERT)。
//
// 回傳只有該盤口的票數與賠率。**誰投給誰永遠不會出現在任何回應裡** ——
// votes.user_id 的唯一用途是擋灌票。
func (s *Service[TX]) Vote(ctx context.Context, p VoteParams) (*MarketOdds, error) {
	if p.UserID <= 0 || p.MarketPublicID == "" || p.Outcome == OutcomeNone {
		return nil, fmt.Errorf("user_id / market_public_id / outcome 必填: %w", ErrInvalidRequest)
	}

	var out *MarketOdds
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		refs, err := s.marketRefs(ctx, tx, []string{p.MarketPublicID})
		if err != nil {
			return err
		}
		ref := refs[p.MarketPublicID]
		if p.MatchPublicID != "" && p.MatchPublicID != ref.MatchPublicID {
			return fmt.Errorf("match=%s market=%s 不屬於該場: %w", p.MatchPublicID, p.MarketPublicID, ErrMarketNotFound)
		}
		mk := ref.Market
		m, err := s.oneMatch(ctx, tx, ref.MatchPublicID)
		if err != nil {
			return err
		}
		if !m.Status.OpenForVotes() {
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrVotingClosed)
		}
		r, err := s.rules(ctx, tx, m.TournamentID)
		if err != nil {
			return err
		}
		if mk.Status != MarketOpen {
			return fmt.Errorf("market=%s status=%s: %w", mk.PublicID, mk.Status, ErrVotingClosed)
		}
		if !r.ValidOutcome(mk.Kind, p.Outcome) {
			return fmt.Errorf("market=%s kind=%s outcome=%s: %w", mk.PublicID, mk.Kind, p.Outcome, ErrOutcomeInvalid)
		}
		if err := s.repo.UpsertVote(ctx, tx, mk.ID, p.UserID, p.Outcome); err != nil {
			return fmt.Errorf("寫入投票: %w", err)
		}

		tallies, err := s.repo.VoteTallies(ctx, tx, []int64{mk.ID})
		if err != nil {
			return fmt.Errorf("數票: %w", err)
		}
		odds := buildMarketOdds(*m, mk, tallies[mk.ID], r, p.Outcome)
		out = &odds
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetOdds 取得多場的即時賠率與票數(每場全部盤口)。匿名可讀。
func (s *Service[TX]) GetOdds(ctx context.Context, p GetOddsParams) ([]MatchOdds, error) {
	ids := dedupStrings(p.MatchPublicIDs)
	if len(ids) == 0 {
		return nil, nil
	}

	var out []MatchOdds
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		matches, err := s.repo.MatchesByPublicIDs(ctx, tx, ids)
		if err != nil {
			return fmt.Errorf("讀場次: %w", err)
		}
		if len(matches) == 0 {
			return nil
		}

		matchIDs := make([]int64, 0, len(matches))
		for _, m := range matches {
			matchIDs = append(matchIDs, m.ID)
		}
		markets, err := s.repo.MarketsByMatches(ctx, tx, matchIDs)
		if err != nil {
			return fmt.Errorf("讀盤口: %w", err)
		}
		marketIDs := make([]int64, 0, len(markets))
		byMatch := make(map[int64][]Market, len(matches))
		for _, mk := range markets {
			marketIDs = append(marketIDs, mk.ID)
			byMatch[mk.MatchID] = append(byMatch[mk.MatchID], mk)
		}
		tallies, err := s.repo.VoteTallies(ctx, tx, marketIDs)
		if err != nil {
			return fmt.Errorf("數票: %w", err)
		}

		// 只查自己的票。這裡沒有、也不該有「查別人投給誰」的分支。
		mine := map[int64]Outcome{}
		if p.ViewerUserID > 0 && len(marketIDs) > 0 {
			mine, err = s.repo.MyVotes(ctx, tx, marketIDs, p.ViewerUserID)
			if err != nil {
				return fmt.Errorf("讀自己的投票: %w", err)
			}
		}

		// 規則逐屆不同,一場一場問會放大成 N 次查詢,所以按 tournament 去重。
		rulesByT := map[int64]Rules{}
		out = make([]MatchOdds, 0, len(matches))
		for _, m := range matches {
			r, ok := rulesByT[m.TournamentID]
			if !ok {
				if r, err = s.rules(ctx, tx, m.TournamentID); err != nil {
					return err
				}
				rulesByT[m.TournamentID] = r
			}
			out = append(out, BuildMatchOdds(m, byMatch[m.ID], tallies, r, mine))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BuildMatchOdds 由票數組出一場全部盤口的賠率。純函數,沒有 I/O。
//
// 導出是為了即時戰況推播(watchpg):它讀到同一批票數後要組出**同一個**東西,
// 而賠率的公式只能有一條產生路徑(專案規則 9)。mine 是查詢者自己的票
// (推播是廣播,傳 nil 即 MyVote 全空);除此之外這裡不接受任何與投票歸屬有關的輸入。
func BuildMatchOdds(m Match, markets []Market, tallies map[int64]Tally, r Rules, mine map[int64]Outcome) MatchOdds {
	out := MatchOdds{
		MatchPublicID: m.PublicID,
		OpenForBets:   m.Status.OpenForBets(),
		Markets:       make([]MarketOdds, 0, len(markets)),
	}
	for _, mk := range markets {
		out.Markets = append(out.Markets, buildMarketOdds(m, mk, tallies[mk.ID], r, mine[mk.ID]))
	}
	return out
}

func buildMarketOdds(m Match, mk Market, t Tally, r Rules, mine Outcome) MarketOdds {
	outcomes := r.Outcomes(mk.Kind)
	votes := make(map[Outcome]int64, len(outcomes))
	labels := make(map[Outcome]string, len(outcomes))
	for _, o := range outcomes {
		votes[o] = t[o]
		labels[o] = OutcomeLabel(mk.Kind, o, m.P1.DisplayName, m.P2.DisplayName, mk.LineSeconds)
	}
	return MarketOdds{
		Market:        mk,
		MatchPublicID: m.PublicID,
		Outcomes:      outcomes,
		Odds:          MarketOddsOf(t, outcomes, r.Odds()),
		Votes:         votes,
		Labels:        labels,
		MyVote:        mine,
	}
}

// ── 下注 ──────────────────────────────────────────────────────────

// PlaceBet 下注。單場或串關都走這支,扣款與建注單在同一個 transaction。
func (s *Service[TX]) PlaceBet(ctx context.Context, p PlaceBetParams) (*PlaceBetResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	var out *PlaceBetResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		r, err := s.placeBetInTx(ctx, tx, p)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// placeBetInTx 是下注的全部流程。順序是刻意的:
//
//	鎖使用者 → 冪等重放檢查 → 佔冪等鍵 → 讀賽事/規則/場次/盤口 → 操守與封盤檢查
//	→ 算賠率與賠付 → 建注單與腿 → **最後才動錢** → 補分錄 id → 存冪等結果
//
// 動錢排在最後有兩個理由:
//
//  1. 所有會拒絕這張單的檢查都該在碰錢之前做完。
//  2. 失敗之後 tx 仍握著它在 savepoint **外面**取得的鎖(platform.users 那列),
//     這時候只該立刻收尾,不該再做別的事。
//
// 這裡原本寫的第二個理由是「savepoint rollback 不釋放餘額列鎖」,
// 2026-09-12 實測推翻(bettingpg 的 TestApplyInTxContract 把行為釘住了)——
// 餘額列的鎖確實會放。結論不變,但理由換成上面那個,因為它是真的。
func (s *Service[TX]) placeBetInTx(ctx context.Context, tx TX, p PlaceBetParams) (*PlaceBetResult, error) {
	key := placeKeyPrefix + p.IdempotencyKey
	hash := placeRequestHash(p)

	// 同一使用者的下注串行化:冪等重放的判定,要在前一筆同鍵請求 commit 之後
	// 才有意義。沒有這把鎖,兩個連點的請求會同時查不到鍵、同時扣兩次款。
	if err := s.repo.LockUser(ctx, tx, p.UserID); err != nil {
		return nil, fmt.Errorf("鎖使用者 %d: %w", p.UserID, err)
	}

	rec, err := s.repo.GetIdempotency(ctx, tx, key)
	if err != nil {
		return nil, fmt.Errorf("讀冪等鍵: %w", err)
	}
	if rec != nil {
		if rec.RequestHash != hash {
			return nil, fmt.Errorf("key=%s: %w", p.IdempotencyKey, ledger.ErrIdempotencyConflict)
		}
		if rec.Response == nil {
			// 理論上到不了:同鍵的前一個 tx 還在跑的話,我們會被 LockUser 擋在外面。
			return nil, ledger.ErrInFlight
		}
		var res PlaceBetResult
		if err := json.Unmarshal(rec.Response, &res); err != nil {
			return nil, fmt.Errorf("反序列化冪等結果: %w", err)
		}
		res.Replayed = true
		return &res, nil
	}
	if err := s.repo.InsertIdempotency(ctx, tx, key, hash); err != nil {
		return nil, fmt.Errorf("佔冪等鍵: %w", err)
	}

	t, err := s.repo.TournamentBySlug(ctx, tx, p.TournamentSlug)
	if err != nil {
		return nil, fmt.Errorf("讀賽事 %s: %w", p.TournamentSlug, err)
	}
	if t == nil {
		return nil, fmt.Errorf("slug=%s: %w", p.TournamentSlug, tournament.ErrTournamentNotFound)
	}
	r, err := s.rules(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	if !r.Config.Betting.Enabled {
		return nil, fmt.Errorf("tournament=%s 本屆不開盤: %w", p.TournamentSlug, ErrBettingClosed)
	}
	cfg := r.Odds()
	if p.Stake > cfg.MaxStake {
		return nil, fmt.Errorf("stake=%d 上限=%d: %w", p.Stake, cfg.MaxStake, ErrStakeTooLarge)
	}

	// 場次由盤口反推。同一場只能一腿:兩腿反推出同一場就擋,不管是不是同一個盤口
	// (整場勝負 + 比分是同一件事押兩次;bet_legs 的 UNIQUE (bet_id, match_id) 兜底)。
	marketPubs := make([]string, 0, len(p.Legs))
	for _, l := range p.Legs {
		marketPubs = append(marketPubs, l.MarketPublicID)
	}
	refs, err := s.marketRefs(ctx, tx, marketPubs)
	if err != nil {
		return nil, err
	}
	matchPubs := make([]string, 0, len(p.Legs))
	seenMatch := make(map[string]struct{}, len(p.Legs))
	for _, l := range p.Legs {
		ref := refs[l.MarketPublicID]
		if l.MatchPublicID != "" && l.MatchPublicID != ref.MatchPublicID {
			return nil, fmt.Errorf("match=%s market=%s 不屬於該場: %w", l.MatchPublicID, l.MarketPublicID, ErrMarketNotFound)
		}
		if _, dup := seenMatch[ref.MatchPublicID]; dup {
			return nil, fmt.Errorf("match=%s: %w", ref.MatchPublicID, ErrDuplicateMatchInParlay)
		}
		seenMatch[ref.MatchPublicID] = struct{}{}
		matchPubs = append(matchPubs, ref.MatchPublicID)
	}
	found, err := s.repo.MatchesByPublicIDs(ctx, tx, matchPubs)
	if err != nil {
		return nil, fmt.Errorf("讀場次: %w", err)
	}
	byPub := make(map[string]Match, len(found))
	for _, m := range found {
		byPub[m.PublicID] = m
	}

	ordered := make([]Match, 0, len(p.Legs))
	chosen := make([]Market, 0, len(p.Legs))
	marketIDs := make([]int64, 0, len(p.Legs))
	for _, l := range p.Legs {
		ref := refs[l.MarketPublicID]
		m, ok := byPub[ref.MatchPublicID]
		if !ok {
			return nil, fmt.Errorf("match=%s: %w", ref.MatchPublicID, activityerr.ErrMatchNotFound)
		}
		if m.TournamentID != t.ID {
			return nil, fmt.Errorf("match=%s: %w", m.PublicID, ErrMatchNotInTournament)
		}
		if !m.Status.OpenForBets() {
			return nil, fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrBettingClosed)
		}
		// 選手不得對自己參與的場次下注。兩條比對路徑都要走:
		// tournament_players.user_id 擋有綁帳號報名的,fencers.user_id 擋
		// 「純活動報名(tournament_players.user_id IS NULL)但劍士本人綁過帳號」的。
		// 兩者都沒綁時擋不住 —— 那是雙軌身分的固有代價,只能靠裁判事後稽核。
		if m.P1.IsUser(p.UserID) || m.P2.IsUser(p.UserID) {
			return nil, fmt.Errorf("match=%s user=%d: %w", m.PublicID, p.UserID, ErrSelfBet)
		}
		mk := ref.Market
		if mk.MatchID != m.ID {
			// 反推與讀回來的場次對不上:資料異常,不猜。
			return nil, fmt.Errorf("market=%s match=%d≠%d: %w", mk.PublicID, mk.MatchID, m.ID, ErrLedgerStateConflict)
		}
		ordered = append(ordered, m)
		if mk.Status != MarketOpen {
			return nil, fmt.Errorf("market=%s status=%s: %w", mk.PublicID, mk.Status, ErrMarketClosed)
		}
		if !r.ValidOutcome(mk.Kind, l.Outcome) {
			return nil, fmt.Errorf("market=%s kind=%s outcome=%s: %w", mk.PublicID, mk.Kind, l.Outcome, ErrOutcomeInvalid)
		}
		chosen = append(chosen, mk)
		marketIDs = append(marketIDs, mk.ID)
	}

	tallies, err := s.repo.VoteTallies(ctx, tx, marketIDs)
	if err != nil {
		return nil, fmt.Errorf("數票: %w", err)
	}

	newLegs := make([]NewLeg, 0, len(p.Legs))
	oddsList := make([]int64, 0, len(p.Legs))
	for i, l := range p.Legs {
		mk := chosen[i]
		tally := tallies[mk.ID]
		outcomes := r.Outcomes(mk.Kind)
		o := OddsMilli(tally[l.Outcome], tally.Total(), len(outcomes), cfg)
		newLegs = append(newLegs, NewLeg{MatchID: mk.MatchID, MarketID: mk.ID, Outcome: l.Outcome, OddsMilli: o})
		oddsList = append(oddsList, o)
	}

	payout := ParlayPayout(p.Stake, oddsList, cfg.MaxParlayMilli)
	if !WithinTolerance(p.ExpectedPayout, payout, cfg.PayoutToleranceBps) {
		return nil, fmt.Errorf("預期 %d 實際 %d: %w", p.ExpectedPayout, payout, ErrOddsMoved)
	}

	publicID, err := s.newPublicID()
	if err != nil {
		return nil, fmt.Errorf("產生 public_id: %w", err)
	}
	betID, createdAt, err := s.repo.InsertBet(ctx, tx, NewBet{
		PublicID:        publicID,
		TournamentID:    t.ID,
		UserID:          p.UserID,
		Stake:           p.Stake,
		PotentialPayout: payout,
	})
	if err != nil {
		return nil, fmt.Errorf("建注單: %w", err)
	}
	if err := s.repo.InsertLegs(ctx, tx, betID, newLegs); err != nil {
		return nil, fmt.Errorf("建注單的腿: %w", err)
	}

	// ── 動錢。注單先建是因為分錄的 ref_id 要指向注單,而注單 id 要插入後才有;
	// 分錄 id 也要寫回注單,所以兩者必有一方要補寫一次(同 tx 內,原子性不受影響)。
	refID := betID
	refType := refTypeBet
	res, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: key + ledgerKeySuffix,
		RequestHash:    hash,
		Ops: []ledger.Op{{
			UserID:   p.UserID,
			Currency: s.currency,
			Amount:   -p.Stake,
			Reason:   ledger.ReasonBetStake,
			RefType:  &refType,
			RefID:    &refID,
		}},
	})
	if err != nil {
		return nil, err
	}
	if res.Replayed || len(res.Entries) != 1 {
		// 我們的冪等鍵剛剛才插進去,帳本那把派生鍵不可能已經有結果。
		// 真的發生代表兩套鍵對不上,寧可整筆失敗也不要回成功但錢不對。
		return nil, fmt.Errorf("bet=%s: %w", publicID, ErrLedgerStateConflict)
	}
	if err := s.repo.SetBetStakeEntry(ctx, tx, betID, res.Entries[0].EntryID); err != nil {
		return nil, fmt.Errorf("回寫扣款分錄 id: %w", err)
	}

	bet := Bet{
		ID:              betID,
		PublicID:        publicID,
		UserID:          p.UserID,
		TournamentID:    t.ID,
		Stake:           p.Stake,
		PotentialPayout: payout,
		Status:          BetOpen,
		CreatedAt:       createdAt,
		Legs:            make([]Leg, 0, len(newLegs)),
	}
	for i, nl := range newLegs {
		m, mk := ordered[i], chosen[i]
		bet.Legs = append(bet.Legs, Leg{
			BetID:          betID,
			MatchID:        nl.MatchID,
			MatchPublicID:  m.PublicID,
			Round:          m.Round,
			Slot:           m.Slot,
			MarketID:       mk.ID,
			MarketPublicID: mk.PublicID,
			MarketKind:     mk.Kind,
			MarketRoundNo:  mk.RoundNo,
			LineSeconds:    mk.LineSeconds,
			Outcome:        nl.Outcome,
			OutcomeLabel:   OutcomeLabel(mk.Kind, nl.Outcome, m.P1.DisplayName, m.P2.DisplayName, mk.LineSeconds),
			OddsMilli:      nl.OddsMilli,
			Result:         LegPending,
		})
	}
	result := &PlaceBetResult{Bet: bet}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("序列化結果: %w", err)
	}
	if err := s.repo.SaveIdempotencyResponse(ctx, tx, key, payload); err != nil {
		return nil, fmt.Errorf("存冪等結果: %w", err)
	}
	return result, nil
}

// ListMyBets 列出自己的注單。
func (s *Service[TX]) ListMyBets(ctx context.Context, p ListMyBetsParams) ([]Bet, error) {
	if p.UserID <= 0 || p.TournamentSlug == "" {
		return nil, fmt.Errorf("user_id / tournament_slug 必填: %w", ErrInvalidRequest)
	}
	var out []Bet
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		t, err := s.repo.TournamentBySlug(ctx, tx, p.TournamentSlug)
		if err != nil {
			return fmt.Errorf("讀賽事 %s: %w", p.TournamentSlug, err)
		}
		if t == nil {
			return fmt.Errorf("slug=%s: %w", p.TournamentSlug, tournament.ErrTournamentNotFound)
		}
		out, err = s.repo.BetsByUser(ctx, tx, t.ID, p.UserID, p.OpenOnly)
		if err != nil {
			return fmt.Errorf("讀注單: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── 內部工具 ──────────────────────────────────────────────────────

func (s *Service[TX]) oneMatch(ctx context.Context, tx TX, publicID string) (*Match, error) {
	if publicID == "" {
		return nil, fmt.Errorf("match_public_id 必填: %w", ErrInvalidRequest)
	}
	ms, err := s.repo.MatchesByPublicIDs(ctx, tx, []string{publicID})
	if err != nil {
		return nil, fmt.Errorf("讀場次 %s: %w", publicID, err)
	}
	for i := range ms {
		if ms[i].PublicID == publicID {
			return &ms[i], nil
		}
	}
	return nil, fmt.Errorf("match=%s: %w", publicID, activityerr.ErrMatchNotFound)
}

func (s *Service[TX]) rules(ctx context.Context, tx TX, tournamentID int64) (Rules, error) {
	r, err := s.repo.Rules(ctx, tx, tournamentID)
	if err != nil {
		return Rules{}, fmt.Errorf("讀賽事規則 tournament=%d: %w", tournamentID, err)
	}
	return r, nil
}

// marketRefs 以 public_id 讀盤口,任何一個查無就回 ErrMarketNotFound。
func (s *Service[TX]) marketRefs(ctx context.Context, tx TX, publicIDs []string) (map[string]MarketRef, error) {
	refs, err := s.repo.MarketsByPublicIDs(ctx, tx, dedupStrings(publicIDs))
	if err != nil {
		return nil, fmt.Errorf("讀盤口: %w", err)
	}
	byPub := make(map[string]MarketRef, len(refs))
	for _, r := range refs {
		byPub[r.PublicID] = r
	}
	for _, id := range publicIDs {
		if _, ok := byPub[id]; !ok {
			return nil, fmt.Errorf("market=%s: %w", id, ErrMarketNotFound)
		}
	}
	return byPub, nil
}

// betIDsOf 取出這些腿所屬注單的 id,去重後**升冪**排序。
// 順序就是取鎖順序,不要在別處重新排。
func betIDsOf(legs []Leg) []int64 {
	ids := make([]int64, 0, len(legs))
	for _, l := range legs {
		ids = append(ids, l.BetID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// validate 做所有不需要碰資料庫就能判斷的檢查。
func (p PlaceBetParams) validate() error {
	switch {
	case p.UserID <= 0:
		return fmt.Errorf("user_id 必填(下注需要平台帳號): %w", ErrInvalidRequest)
	case p.TournamentSlug == "":
		return fmt.Errorf("tournament_slug 必填: %w", ErrInvalidRequest)
	case p.IdempotencyKey == "":
		return fmt.Errorf("idempotency_key 必填(動錢一律冪等): %w", ErrInvalidRequest)
	case p.Stake <= 0:
		return fmt.Errorf("stake=%d 必須為正: %w", p.Stake, ErrInvalidRequest)
	case len(p.Legs) == 0:
		return fmt.Errorf("至少要押一場: %w", ErrInvalidRequest)
	case len(p.Legs) > MaxLegsPerBet:
		return fmt.Errorf("腿數 %d 上限 %d: %w", len(p.Legs), MaxLegsPerBet, ErrTooManyLegs)
	}
	seenMarket := make(map[string]struct{}, len(p.Legs))
	seenMatch := make(map[string]struct{}, len(p.Legs))
	for _, l := range p.Legs {
		if l.MarketPublicID == "" {
			return fmt.Errorf("market_public_id 必填: %w", ErrInvalidRequest)
		}
		if l.Outcome == OutcomeNone {
			return fmt.Errorf("market=%s outcome 必填: %w", l.MarketPublicID, ErrInvalidRequest)
		}
		// 不碰資料庫就判得出的重複先擋在這裡(同一盤口兩次、或客戶端有帶場次且同場兩次);
		// 反推場次後的完整判定在 placeBetInTx。bet_legs 的 UNIQUE (bet_id, match_id) 兜底。
		if _, dup := seenMarket[l.MarketPublicID]; dup {
			return fmt.Errorf("market=%s: %w", l.MarketPublicID, ErrDuplicateMatchInParlay)
		}
		seenMarket[l.MarketPublicID] = struct{}{}
		if l.MatchPublicID != "" {
			if _, dup := seenMatch[l.MatchPublicID]; dup {
				return fmt.Errorf("match=%s: %w", l.MatchPublicID, ErrDuplicateMatchInParlay)
			}
			seenMatch[l.MatchPublicID] = struct{}{}
		}
	}
	return nil
}

// placeRequestHash 是「同一把鍵是不是同一個請求」的判準。
//
// 刻意不含賠率:賠率隨票數浮動,把它放進雜湊會讓每次重試都變成內容不同,
// 冪等就失效了。腿的順序算在內 —— 客戶端重試送的是同一份請求。
func placeRequestHash(p PlaceBetParams) string {
	parts := make([]string, 0, len(p.Legs)+3)
	parts = append(parts,
		strconv.FormatInt(p.UserID, 10),
		p.TournamentSlug,
		strconv.FormatInt(p.Stake, 10))
	for _, l := range p.Legs {
		parts = append(parts, l.MarketPublicID+":"+string(l.Outcome))
	}
	return hashOf(parts...)
}

func hashOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}
