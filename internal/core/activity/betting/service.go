package betting

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// LegInput 是要押的一腿。
type LegInput struct {
	MatchPublicID string
	Side          Side
}

// VoteParams 是一次投票。
type VoteParams struct {
	UserID        int64
	MatchPublicID string
	Side          Side
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

// SettleResult 是一場比賽結算(或棄賽退款)的完整結果。
type SettleResult struct {
	MatchPublicID string
	LegsWon       int
	LegsLost      int
	LegsVoid      int
	Bets          []BetOutcome
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

// Vote 投票給某一方。一場一票,再投即改票(UPSERT)。
//
// 回傳只有票數與賠率。**誰投給誰永遠不會出現在任何回應裡** ——
// votes.user_id 的唯一用途是擋灌票。
func (s *Service[TX]) Vote(ctx context.Context, p VoteParams) (*MatchOdds, error) {
	if p.UserID <= 0 || p.MatchPublicID == "" || !p.Side.Valid() {
		return nil, fmt.Errorf("user_id / match_public_id / side 必填且合法: %w", ErrInvalidRequest)
	}

	var out *MatchOdds
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		m, err := s.oneMatch(ctx, tx, p.MatchPublicID)
		if err != nil {
			return err
		}
		if !m.Status.OpenForVotes() {
			return fmt.Errorf("match=%s status=%s: %w", m.PublicID, m.Status, ErrVotingClosed)
		}
		if err := s.repo.UpsertVote(ctx, tx, m.ID, p.UserID, p.Side); err != nil {
			return fmt.Errorf("寫入投票: %w", err)
		}

		cfg, err := s.oddsConfig(ctx, tx, m.TournamentID)
		if err != nil {
			return err
		}
		tallies, err := s.repo.VoteTallies(ctx, tx, []int64{m.ID})
		if err != nil {
			return fmt.Errorf("數票: %w", err)
		}
		odds := buildOdds(*m, tallies[m.ID], cfg, p.Side)
		out = &odds
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetOdds 取得多場的即時賠率與票數。匿名可讀。
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
		tallies, err := s.repo.VoteTallies(ctx, tx, matchIDs)
		if err != nil {
			return fmt.Errorf("數票: %w", err)
		}

		// 只查自己的票。這裡沒有、也不該有「查別人投給誰」的分支。
		mine := map[int64]Side{}
		if p.ViewerUserID > 0 {
			mine, err = s.repo.MyVotes(ctx, tx, matchIDs, p.ViewerUserID)
			if err != nil {
				return fmt.Errorf("讀自己的投票: %w", err)
			}
		}

		// 賠率參數逐屆不同,一場一場問會放大成 N 次查詢,所以按 tournament 去重。
		cfgs := map[int64]OddsConfig{}
		out = make([]MatchOdds, 0, len(matches))
		for _, m := range matches {
			cfg, ok := cfgs[m.TournamentID]
			if !ok {
				if cfg, err = s.oddsConfig(ctx, tx, m.TournamentID); err != nil {
					return err
				}
				cfgs[m.TournamentID] = cfg
			}
			out = append(out, buildOdds(m, tallies[m.ID], cfg, mine[m.ID]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
//	鎖使用者 → 冪等重放檢查 → 佔冪等鍵 → 讀賽事/場次 → 操守與封盤檢查
//	→ 算賠率與賠付 → 建注單與腿 → **最後才動錢** → 補分錄 id → outbox → 存冪等結果
//
// 動錢排在最後有兩個理由:一是所有會拒絕這張單的檢查都該在碰錢之前做完;
// 二是 ApplyInTx 的 savepoint rollback **不釋放已取得的餘額列鎖**,
// 餘額不足回來之後 tx 還握著那列,這時候只該立刻收尾,不該再做別的事。
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
		return nil, fmt.Errorf("slug=%s: %w", p.TournamentSlug, ErrTournamentNotFound)
	}
	cfg, err := s.oddsConfig(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	if p.Stake > cfg.MaxStake {
		return nil, fmt.Errorf("stake=%d 上限=%d: %w", p.Stake, cfg.MaxStake, ErrStakeTooLarge)
	}

	pubIDs := make([]string, 0, len(p.Legs))
	for _, l := range p.Legs {
		pubIDs = append(pubIDs, l.MatchPublicID)
	}
	found, err := s.repo.MatchesByPublicIDs(ctx, tx, pubIDs)
	if err != nil {
		return nil, fmt.Errorf("讀場次: %w", err)
	}
	byPub := make(map[string]Match, len(found))
	for _, m := range found {
		byPub[m.PublicID] = m
	}

	ordered := make([]Match, 0, len(p.Legs))
	matchIDs := make([]int64, 0, len(p.Legs))
	for _, l := range p.Legs {
		m, ok := byPub[l.MatchPublicID]
		if !ok {
			return nil, fmt.Errorf("match=%s: %w", l.MatchPublicID, ErrMatchNotFound)
		}
		if m.TournamentID != t.ID {
			return nil, fmt.Errorf("match=%s: %w", l.MatchPublicID, ErrMatchNotInTournament)
		}
		if !m.Status.OpenForBets() {
			return nil, fmt.Errorf("match=%s status=%s: %w", l.MatchPublicID, m.Status, ErrBettingClosed)
		}
		// 選手不得對自己參與的場次下注。兩條比對路徑都要走:
		// tournament_players.user_id 擋有綁帳號報名的,fencers.user_id 擋
		// 「純活動報名(tournament_players.user_id IS NULL)但劍士本人綁過帳號」的。
		// 兩者都沒綁時擋不住 —— 那是雙軌身分的固有代價,只能靠裁判事後稽核。
		if m.P1.IsUser(p.UserID) || m.P2.IsUser(p.UserID) {
			return nil, fmt.Errorf("match=%s user=%d: %w", l.MatchPublicID, p.UserID, ErrSelfBet)
		}
		ordered = append(ordered, m)
		matchIDs = append(matchIDs, m.ID)
	}

	tallies, err := s.repo.VoteTallies(ctx, tx, matchIDs)
	if err != nil {
		return nil, fmt.Errorf("數票: %w", err)
	}

	newLegs := make([]NewLeg, 0, len(p.Legs))
	oddsList := make([]int64, 0, len(p.Legs))
	for i, l := range p.Legs {
		m := ordered[i]
		tally := tallies[m.ID]
		o := OddsMilli(tally.Votes(l.Side), tally.Total(), cfg)
		newLegs = append(newLegs, NewLeg{MatchID: m.ID, Side: l.Side, OddsMilli: o})
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
		m := ordered[i]
		bet.Legs = append(bet.Legs, Leg{
			BetID:           betID,
			MatchID:         nl.MatchID,
			MatchPublicID:   m.PublicID,
			Round:           m.Round,
			Slot:            m.Slot,
			Side:            nl.Side,
			SideDisplayName: m.Participant(nl.Side).DisplayName,
			OddsMilli:       nl.OddsMilli,
			Result:          LegPending,
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
			return fmt.Errorf("slug=%s: %w", p.TournamentSlug, ErrTournamentNotFound)
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

// ── 結算 ──────────────────────────────────────────────────────────

// SettleMatch 在某場分出勝負時結算所有押到它的注單。
func (s *Service[TX]) SettleMatch(ctx context.Context, matchPublicID string) (*SettleResult, error) {
	var out *SettleResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		r, err := s.SettleMatchInTx(ctx, tx, matchPublicID)
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

// SettleMatchInTx 是結算的本體,開放給「判定勝負」與「結算注單」要同生共死的
// 呼叫端(賽事服務在自己的 tx 裡寫完 winner_player_id 後直接接著呼叫)。
//
// 流程:
//
//	advisory lock 防同場重複結算 → 撈 pending 腿 → **依 id 升冪鎖注單列**
//	→ 標腿 won/lost → 重讀全部腿 → 逐張注單判定 → 派彩 → outbox
//
// 為什麼要鎖注單列:advisory lock 只擋得住「同一場被結算兩次」。
// 一張串關的兩腿分屬 A、B 兩場時,A 與 B 同時結算會各自持有不同的 advisory lock,
// 兩邊都看不到對方剛標好的腿,於是兩邊都判定「還有 pending」——
// 結果是一張全贏的注單永遠停在 open。注單列鎖讓第二個 tx 等到第一個 commit 之後
// 才重讀,那時它看得到另一腿已經 won。升冪取鎖則是全域的防死鎖慣例。
func (s *Service[TX]) SettleMatchInTx(ctx context.Context, tx TX, matchPublicID string) (*SettleResult, error) {
	m, err := s.oneMatch(ctx, tx, matchPublicID)
	if err != nil {
		return nil, err
	}
	if m.ResultKind == ResultWalkover {
		// 賠率是按「真的打一場」算的,不戰而勝不該讓任何人贏錢。
		return nil, fmt.Errorf("match=%s: %w", matchPublicID, ErrWalkoverMatch)
	}
	if m.Status != MatchDone {
		return nil, fmt.Errorf("match=%s status=%s: %w", matchPublicID, m.Status, ErrMatchNotDecided)
	}
	win := m.WinningSide()
	if win == SideNone {
		return nil, fmt.Errorf("match=%s winner=%d: %w", matchPublicID, m.WinnerPlayerID, ErrMatchNotDecided)
	}

	// 這裡讀到的 match 是鎖之前讀的,但那只用來解析 id 與勝方,
	// 而 matches 一旦 done 就不再變。所有會影響動錢的判斷(腿的狀態、注單的狀態)
	// 都在鎖之後才讀。
	if err := s.repo.LockMatchSettle(ctx, tx, m.ID); err != nil {
		return nil, fmt.Errorf("取結算鎖 match=%d: %w", m.ID, err)
	}

	pending, err := s.repo.PendingLegsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀待判定的腿: %w", err)
	}
	out := &SettleResult{MatchPublicID: m.PublicID}
	if len(pending) == 0 {
		return out, nil
	}

	var wonIDs, lostIDs []int64
	for _, l := range pending {
		if l.Side == win {
			wonIDs = append(wonIDs, l.ID)
		} else {
			lostIDs = append(lostIDs, l.ID)
		}
	}
	out.LegsWon, out.LegsLost = len(wonIDs), len(lostIDs)

	betIDs := betIDsOf(pending)
	if err := s.repo.LockBets(ctx, tx, betIDs); err != nil {
		return nil, fmt.Errorf("鎖注單: %w", err)
	}
	if err := s.repo.UpdateLegResults(ctx, tx, wonIDs, LegWon); err != nil {
		return nil, fmt.Errorf("標記勝方的腿: %w", err)
	}
	if err := s.repo.UpdateLegResults(ctx, tx, lostIDs, LegLost); err != nil {
		return nil, fmt.Errorf("標記敗方的腿: %w", err)
	}

	outcomes, err := s.applyOutcomes(ctx, tx, *m, betIDs, false)
	if err != nil {
		return nil, err
	}
	out.Bets = outcomes
	return out, nil
}

// VoidMatch 處理棄賽(result_kind = walkover):該場所有腿作廢。
func (s *Service[TX]) VoidMatch(ctx context.Context, matchPublicID string) (*SettleResult, error) {
	var out *SettleResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		r, err := s.VoidMatchInTx(ctx, tx, matchPublicID)
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

// VoidMatchInTx 是棄賽退款的本體。與正常結算共用同一把 advisory lock。
//
//	單場注單(只有這一腿)→ 全額退 stake,status = void
//	串關注單             → 移除該腿,依剩餘腿各自鎖定的 odds_milli 重算賠付
//	                        剩餘全 won → won 依重算金額派彩
//	                        剩餘有 pending → 維持 open(等其他場次)
//	                        剩餘有 lost → lost(棄賽不救已經輸掉的注單)
//	                        全部都 void → void 全額退
//
// 重算不違反「派彩用下注當下鎖定的金額」:那條講的是不因**票數變動**而重算,
// 這裡是一腿**不成立**了,把它從乘積裡拿掉,剩餘每一腿的賠率一個都沒被改動。
// payout_recalculated 就是為了讓對帳看得出這個差別。
func (s *Service[TX]) VoidMatchInTx(ctx context.Context, tx TX, matchPublicID string) (*SettleResult, error) {
	m, err := s.oneMatch(ctx, tx, matchPublicID)
	if err != nil {
		return nil, err
	}
	if m.ResultKind != ResultWalkover {
		return nil, fmt.Errorf("match=%s kind=%s: %w", matchPublicID, m.ResultKind, ErrNotWalkover)
	}
	if m.Status != MatchDone {
		return nil, fmt.Errorf("match=%s status=%s: %w", matchPublicID, m.Status, ErrMatchNotDecided)
	}

	if err := s.repo.LockMatchSettle(ctx, tx, m.ID); err != nil {
		return nil, fmt.Errorf("取結算鎖 match=%d: %w", m.ID, err)
	}

	pending, err := s.repo.PendingLegsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀待判定的腿: %w", err)
	}
	out := &SettleResult{MatchPublicID: m.PublicID, LegsVoid: len(pending)}
	if len(pending) == 0 {
		return out, nil
	}

	legIDs := make([]int64, 0, len(pending))
	for _, l := range pending {
		legIDs = append(legIDs, l.ID)
	}
	betIDs := betIDsOf(pending)
	if err := s.repo.LockBets(ctx, tx, betIDs); err != nil {
		return nil, fmt.Errorf("鎖注單: %w", err)
	}
	if err := s.repo.UpdateLegResults(ctx, tx, legIDs, LegVoid); err != nil {
		return nil, fmt.Errorf("作廢這場的腿: %w", err)
	}

	outcomes, err := s.applyOutcomes(ctx, tx, *m, betIDs, true)
	if err != nil {
		return nil, err
	}
	out.Bets = outcomes
	return out, nil
}

// applyOutcomes 逐張注單判定狀態並動錢。recalc = true 時(棄賽路徑)
// 依剩餘腿重算 potential_payout。
//
// 只處理 status 仍為 open 的注單 —— 已經結算過的一律略過。這是「不重複派彩」
// 的真正權威:advisory lock 防的是併發,狀態檢查防的是重跑。
func (s *Service[TX]) applyOutcomes(ctx context.Context, tx TX, m Match, betIDs []int64, recalc bool) ([]BetOutcome, error) {
	bets, err := s.repo.BetsByIDs(ctx, tx, betIDs)
	if err != nil {
		return nil, fmt.Errorf("讀注單: %w", err)
	}
	legsByBet, err := s.repo.LegsByBetIDs(ctx, tx, betIDs)
	if err != nil {
		return nil, fmt.Errorf("讀注單的腿: %w", err)
	}

	var cfg OddsConfig
	if recalc {
		// 一張注單的腿不會跨屆(bets.tournament_id 是單一權威),所以一份參數夠用。
		if cfg, err = s.oddsConfig(ctx, tx, m.TournamentID); err != nil {
			return nil, err
		}
	}

	// 依 id 升冪處理,與 LockBets 的取鎖順序一致,結果也才可重現。
	slices.SortFunc(bets, func(a, b Bet) int { return cmp.Compare(a.ID, b.ID) })
	outcomes := make([]BetOutcome, 0, len(bets))

	for _, b := range bets {
		if b.Status != BetOpen {
			continue
		}
		legs := legsByBet[b.ID]
		status := EvaluateBet(legs)

		payout := b.PotentialPayout
		var newPayout *int64
		if recalc {
			if remaining := RemainingOdds(legs); len(remaining) > 0 {
				p := ParlayPayout(b.Stake, remaining, cfg.MaxParlayMilli)
				newPayout, payout = &p, p
			}
		}

		upd := BetUpdate{BetID: b.ID, Status: status, PotentialPayout: newPayout}
		oc := BetOutcome{
			BetID:              b.ID,
			BetPublicID:        b.PublicID,
			Status:             status,
			PayoutRecalculated: b.PayoutRecalculated || newPayout != nil,
		}

		switch status {
		case BetWon:
			// 派彩是新的正數分錄,金額是注單上寫死的承諾(或棄賽移腿後的重算值)。
			entryID, err := s.moveMoney(ctx, tx, b, payoutKeyPrefix+b.PublicID,
				ledger.ReasonBetPayout, payout)
			if err != nil {
				return nil, err
			}
			oc.Amount, oc.LedgerEntryID = payout, entryID
			upd.LedgerPayoutEntryID = &entryID
		case BetVoid:
			// 全額退本金。退款同樣是新的正數分錄,絕不改舊紀錄。
			entryID, err := s.moveMoney(ctx, tx, b, refundKeyPrefix+b.PublicID,
				ledger.ReasonBetRefund, b.Stake)
			if err != nil {
				return nil, err
			}
			oc.Amount, oc.LedgerEntryID = b.Stake, entryID
			upd.LedgerRefundEntryID = &entryID
		case BetLost:
			// 不動錢:本金在下注當下就扣掉了。
		case BetOpen:
			if newPayout == nil {
				// 沒判定出結果也沒重算金額,這張注單這次完全沒變 —— 不寫、不發事件。
				continue
			}
		}

		if err := s.repo.UpdateBet(ctx, tx, upd); err != nil {
			return nil, fmt.Errorf("更新注單 %d: %w", b.ID, err)
		}
		b.Status, b.PotentialPayout, b.PayoutRecalculated = status, payout, oc.PayoutRecalculated
		outcomes = append(outcomes, oc)
	}
	return outcomes, nil
}

// moveMoney 發一筆正數分錄(派彩或退款)並回傳分錄 id。
//
// 冪等鍵是由注單 public_id 決定性推出來的,不是呼叫端傳進來的:
// 一張注單只會被派彩一次、退款一次,不需要外部提供身分。
func (s *Service[TX]) moveMoney(ctx context.Context, tx TX, b Bet, key string, reason ledger.Reason, amount int64) (int64, error) {
	if amount <= 0 {
		// 帳本禁止 0 金額分錄。走到這裡代表賠付算成 0,那是 bug 不是狀態。
		return 0, fmt.Errorf("bet=%s amount=%d: %w", b.PublicID, amount, ErrLedgerStateConflict)
	}
	refID := b.ID
	refType := refTypeBet
	res, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: key,
		RequestHash:    hashOf(key, strconv.FormatInt(amount, 10)),
		Ops: []ledger.Op{{
			UserID:   b.UserID,
			Currency: s.currency,
			Amount:   amount,
			Reason:   reason,
			RefType:  &refType,
			RefID:    &refID,
		}},
	})
	if err != nil {
		return 0, fmt.Errorf("bet=%s reason=%s: %w", b.PublicID, reason, err)
	}
	if len(res.Entries) != 1 {
		return 0, fmt.Errorf("bet=%s reason=%s 分錄數 %d: %w", b.PublicID, reason, len(res.Entries), ErrLedgerStateConflict)
	}
	return res.Entries[0].EntryID, nil
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
	return nil, fmt.Errorf("match=%s: %w", publicID, ErrMatchNotFound)
}

func (s *Service[TX]) oddsConfig(ctx context.Context, tx TX, tournamentID int64) (OddsConfig, error) {
	cfg, err := s.repo.OddsConfig(ctx, tx, tournamentID)
	if err != nil {
		return OddsConfig{}, fmt.Errorf("讀賠率參數 tournament=%d: %w", tournamentID, err)
	}
	return cfg.Normalize(), nil
}

// buildOdds 組出對外的賠率回應。除了 mine(查詢者自己那一票)之外,
// 這裡不接受任何與投票歸屬有關的輸入 —— 型別上就沒有洩漏的地方。
func buildOdds(m Match, t Tally, cfg OddsConfig, mine Side) MatchOdds {
	p1, p2 := MatchOddsOf(t, cfg)
	return MatchOdds{
		MatchPublicID: m.PublicID,
		P1Votes:       t.P1,
		P2Votes:       t.P2,
		P1OddsMilli:   p1,
		P2OddsMilli:   p2,
		OpenForBets:   m.Status.OpenForBets(),
		MyVote:        mine,
	}
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
	seen := make(map[string]struct{}, len(p.Legs))
	for _, l := range p.Legs {
		if l.MatchPublicID == "" {
			return fmt.Errorf("match_public_id 必填: %w", ErrInvalidRequest)
		}
		if !l.Side.Valid() {
			return fmt.Errorf("side=%d 必須是 1 或 2: %w", l.Side, ErrInvalidRequest)
		}
		if _, dup := seen[l.MatchPublicID]; dup {
			// 押完 p1 再押 p2 等於穩賺。bet_legs 的 UNIQUE 兜底,
			// 但在這裡擋才給得出可讀的錯誤,而不是資料庫約束錯誤。
			return fmt.Errorf("match=%s: %w", l.MatchPublicID, ErrDuplicateLeg)
		}
		seen[l.MatchPublicID] = struct{}{}
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
		parts = append(parts, l.MatchPublicID+":"+strconv.Itoa(int(l.Side)))
	}
	return hashOf(parts...)
}

func hashOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}
