package betting

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// 結算(schemas/21「結算流程」)。每種盤口一個結算器,觸發時機不同:
//
//	round_winner / duration  該回合結束時(SettleRoundInTx)
//	match_winner / score     場次 done 時(SettleMatchInTx)
//	沒打到的回合的盤口       場次 done 時標 void,走退款路徑 —— **不是 lost**
//	棄賽                     所有盤口 void、所有 pending 腿 void(VoidMatchInTx)
//
// 結算器**不認識違規**:裁判依違規判給對方後,對這裡就是一個普通的回合/整場結果,
// 它只看 match_rounds 與 matches.winner_player_id。
//
// 三支 *InTx 共用同一條流程(settleMarkets):advisory lock → 撈每個盤口的 pending 腿
// → **依 id 升冪鎖注單列** → 標腿 won/lost/void → 盤口標 settled/void → 逐張注單判定 → 動錢。

// verdict 是一個盤口的判決:Winner 是實際發生的結果;OutcomeNone = 盤口作廢(void)。
type verdict struct {
	Market Market
	Winner Outcome
}

// SettleRoundInTx 在某回合結束時結算該回合的 round_winner 與 duration 盤口。
//
// 只動該 roundNo 的兩種盤口;其他回合、整場盤口不碰。重複呼叫時那兩個盤口已不在
// 未結算清單裡,自然是空結果 —— 「一個盤口只結算一次」的權威是 markets.status。
func (s *Service[TX]) SettleRoundInTx(ctx context.Context, tx TX, matchPublicID string, roundNo int) (*SettleResult, error) {
	if roundNo < 1 {
		return nil, fmt.Errorf("round_no=%d 必須 ≥ 1: %w", roundNo, ErrInvalidRequest)
	}
	m, err := s.oneMatch(ctx, tx, matchPublicID)
	if err != nil {
		return nil, err
	}
	if m.ResultKind == ResultWalkover {
		return nil, fmt.Errorf("match=%s: %w", matchPublicID, ErrWalkoverMatch)
	}
	if err := s.repo.LockMatchSettle(ctx, tx, m.ID); err != nil {
		return nil, fmt.Errorf("取結算鎖 match=%d: %w", m.ID, err)
	}

	unsettled, err := s.repo.UnsettledMarketsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀未結算盤口: %w", err)
	}
	var targets []Market
	for _, mk := range unsettled {
		if mk.Kind.PerRound() && mk.RoundNo == roundNo {
			targets = append(targets, mk)
		}
	}
	out := &SettleResult{MatchPublicID: m.PublicID}
	if len(targets) == 0 {
		return out, nil
	}

	rounds, err := s.repo.RoundsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀回合: %w", err)
	}
	rd, ok := roundByNo(rounds, roundNo)
	if !ok || !rd.Finished() {
		return nil, fmt.Errorf("match=%s round=%d: %w", m.PublicID, roundNo, ErrRoundNotFinished)
	}

	verdicts := make([]verdict, 0, len(targets))
	for _, mk := range targets {
		w, err := judgeRoundMarket(*m, mk, rd)
		if err != nil {
			return nil, err
		}
		verdicts = append(verdicts, verdict{Market: mk, Winner: w})
	}
	return s.settleMarkets(ctx, tx, *m, verdicts, out)
}

// SettleMatchInTx 在場次 done 時結算整場盤口(match_winner、score),並處理剩下的
// 逐回合盤口:回合有結果的照結算(裁判沒逐回合呼叫也不會漏),沒打到的回合標 void 退款。
//
// 開放給「判定勝負」與「結算注單」要同生共死的呼叫端(賽事服務在自己的 tx 裡
// 寫完 winner_player_id 後直接接著呼叫)。
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
	win := m.WinningOutcome()
	if win == OutcomeNone {
		return nil, fmt.Errorf("match=%s winner=%d: %w", matchPublicID, m.WinnerPlayerID, ErrMatchNotDecided)
	}

	// 這裡讀到的 match 是鎖之前讀的,但那只用來解析 id 與勝方,
	// 而 matches 一旦 done 就不再變。所有會影響動錢的判斷(盤口、腿、注單的狀態)
	// 都在鎖之後才讀。
	if err := s.repo.LockMatchSettle(ctx, tx, m.ID); err != nil {
		return nil, fmt.Errorf("取結算鎖 match=%d: %w", m.ID, err)
	}

	unsettled, err := s.repo.UnsettledMarketsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀未結算盤口: %w", err)
	}
	out := &SettleResult{MatchPublicID: m.PublicID}
	if len(unsettled) == 0 {
		return out, nil
	}
	rounds, err := s.repo.RoundsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀回合: %w", err)
	}

	verdicts := make([]verdict, 0, len(unsettled))
	for _, mk := range unsettled {
		v := verdict{Market: mk}
		switch mk.Kind {
		case MarketMatchWinner:
			v.Winner = win
		case MarketScore:
			// 比分由回合勝者計數組出。組不出來(沒有回合紀錄、或平手)就作廢退款:
			// 那是資料異常,但它不該擋住裁判判定整場,而作廢是唯一不會讓任何人多拿錢的處理。
			p1, p2 := scoreOf(*m, rounds)
			v.Winner = ScoreOutcome(p1, p2)
		case MarketRoundWinner, MarketDuration:
			// 回合有結果就照結算;沒打到(2:0 結束就沒有第三回合)或沒打完就作廢。
			if rd, ok := roundByNo(rounds, mk.RoundNo); ok && rd.Finished() {
				w, err := judgeRoundMarket(*m, mk, rd)
				if err != nil {
					return nil, err
				}
				v.Winner = w
			}
		default:
			// 不認得的 kind 沒有結算器:作廢退款,不猜。
		}
		verdicts = append(verdicts, v)
	}
	return s.settleMarkets(ctx, tx, *m, verdicts, out)
}

// VoidMatchInTx 處理棄賽(result_kind = walkover):該場所有盤口 void、所有 pending 腿 void。
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
	unsettled, err := s.repo.UnsettledMarketsByMatch(ctx, tx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("讀未結算盤口: %w", err)
	}
	out := &SettleResult{MatchPublicID: m.PublicID}
	if len(unsettled) == 0 {
		return out, nil
	}
	verdicts := make([]verdict, 0, len(unsettled))
	for _, mk := range unsettled {
		verdicts = append(verdicts, verdict{Market: mk}) // Winner = OutcomeNone → void
	}
	return s.settleMarkets(ctx, tx, *m, verdicts, out)
}

// SettleMatch / VoidMatch 是自開 tx 的版本,給沒有現成 tx 的呼叫端(測試、補救工具)。

// SettleMatch 在某場分出勝負時結算所有押到它的注單。
func (s *Service[TX]) SettleMatch(ctx context.Context, matchPublicID string) (*SettleResult, error) {
	return s.inTx(ctx, func(ctx context.Context, tx TX) (*SettleResult, error) {
		return s.SettleMatchInTx(ctx, tx, matchPublicID)
	})
}

// SettleRound 結算某回合的盤口。
func (s *Service[TX]) SettleRound(ctx context.Context, matchPublicID string, roundNo int) (*SettleResult, error) {
	return s.inTx(ctx, func(ctx context.Context, tx TX) (*SettleResult, error) {
		return s.SettleRoundInTx(ctx, tx, matchPublicID, roundNo)
	})
}

// VoidMatch 處理棄賽:該場所有盤口與腿作廢。
func (s *Service[TX]) VoidMatch(ctx context.Context, matchPublicID string) (*SettleResult, error) {
	return s.inTx(ctx, func(ctx context.Context, tx TX) (*SettleResult, error) {
		return s.VoidMatchInTx(ctx, tx, matchPublicID)
	})
}

func (s *Service[TX]) inTx(ctx context.Context, fn func(context.Context, TX) (*SettleResult, error)) (*SettleResult, error) {
	var out *SettleResult
	err := s.repo.InTx(ctx, func(ctx context.Context, tx TX) error {
		r, err := fn(ctx, tx)
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

// ── 判決 ──────────────────────────────────────────────────────────

// judgeRoundMarket 給一個逐回合盤口判出實際結果。回合必須已結束。
//
//	round_winner  該回合勝者 == p1 / p2
//	duration      finished_at − started_at vs line_seconds;**等於 line 算 over**
//	              (schemas/21 待確認 ①,已同意)。直接比 time.Duration,不先截成整數秒 ——
//	              截了就會出現「89.9 秒算 89 秒」這種沒人說得清的邊界。
func judgeRoundMarket(m Match, mk Market, rd RoundResult) (Outcome, error) {
	switch mk.Kind {
	case MarketRoundWinner:
		o := m.SideOf(rd.WinnerPlayerID)
		if o == OutcomeNone {
			return OutcomeNone, fmt.Errorf("match=%s round=%d 勝者 %d 不是場上兩人之一: %w",
				m.PublicID, rd.RoundNo, rd.WinnerPlayerID, ErrMatchNotDecided)
		}
		return o, nil
	case MarketDuration:
		if rd.FinishedAt == nil {
			return OutcomeNone, fmt.Errorf("match=%s round=%d: %w", m.PublicID, rd.RoundNo, ErrRoundNotFinished)
		}
		if rd.FinishedAt.Sub(rd.StartedAt) >= time.Duration(mk.LineSeconds)*time.Second {
			return OutcomeOver, nil
		}
		return OutcomeUnder, nil
	default:
		return OutcomeNone, fmt.Errorf("market=%s kind=%s 不是逐回合盤口: %w", mk.PublicID, mk.Kind, ErrInvalidRequest)
	}
}

// scoreOf 由回合勝者計出兩邊勝場。勝者不是場上兩人之一的回合不計(資料異常)。
func scoreOf(m Match, rounds []RoundResult) (p1Wins, p2Wins int) {
	for _, rd := range rounds {
		if !rd.Finished() {
			continue
		}
		switch m.SideOf(rd.WinnerPlayerID) {
		case OutcomeP1:
			p1Wins++
		case OutcomeP2:
			p2Wins++
		}
	}
	return p1Wins, p2Wins
}

func roundByNo(rounds []RoundResult, no int) (RoundResult, bool) {
	for _, rd := range rounds {
		if rd.RoundNo == no {
			return rd, true
		}
	}
	return RoundResult{}, false
}

// ── 共用流程 ──────────────────────────────────────────────────────

// settleMarkets 依判決標腿、標盤口、判注單、動錢。呼叫前必須已持有該場的 advisory lock。
func (s *Service[TX]) settleMarkets(ctx context.Context, tx TX, m Match, verdicts []verdict, out *SettleResult) (*SettleResult, error) {
	var wonIDs, lostIDs, voidIDs []int64
	var all []Leg
	for _, v := range verdicts {
		pending, err := s.repo.PendingLegsByMarket(ctx, tx, v.Market.ID)
		if err != nil {
			return nil, fmt.Errorf("讀盤口 %s 待判定的腿: %w", v.Market.PublicID, err)
		}
		for _, l := range pending {
			switch {
			case v.Winner == OutcomeNone:
				voidIDs = append(voidIDs, l.ID)
			case l.Outcome == v.Winner:
				wonIDs = append(wonIDs, l.ID)
			default:
				lostIDs = append(lostIDs, l.ID)
			}
		}
		all = append(all, pending...)
	}
	out.LegsWon, out.LegsLost, out.LegsVoid = len(wonIDs), len(lostIDs), len(voidIDs)

	// 注單列鎖:在寫任何腿之前,依 id 升冪。
	betIDs := betIDsOf(all)
	if err := s.repo.LockBets(ctx, tx, betIDs); err != nil {
		return nil, fmt.Errorf("鎖注單: %w", err)
	}
	if err := s.repo.UpdateLegResults(ctx, tx, wonIDs, LegWon); err != nil {
		return nil, fmt.Errorf("標記贏的腿: %w", err)
	}
	if err := s.repo.UpdateLegResults(ctx, tx, lostIDs, LegLost); err != nil {
		return nil, fmt.Errorf("標記輸的腿: %w", err)
	}
	if err := s.repo.UpdateLegResults(ctx, tx, voidIDs, LegVoid); err != nil {
		return nil, fmt.Errorf("作廢的腿: %w", err)
	}

	// 盤口狀態是「一個盤口只結算一次」的權威;已結算過(0 列)是錯誤,不是巧合。
	for _, v := range verdicts {
		status := MarketSettled
		if v.Winner == OutcomeNone {
			status = MarketVoid
		}
		if err := s.repo.SettleMarket(ctx, tx, v.Market.ID, status); err != nil {
			return nil, fmt.Errorf("盤口 %s 標 %s: %w", v.Market.PublicID, status, err)
		}
		if status == MarketVoid {
			out.MarketsVoided++
		} else {
			out.MarketsSettled++
		}
	}

	if len(betIDs) == 0 {
		return out, nil
	}
	outcomes, err := s.applyOutcomes(ctx, tx, m, betIDs)
	if err != nil {
		return nil, err
	}
	out.Bets = outcomes
	return out, nil
}

// applyOutcomes 逐張注單判定狀態並動錢。有腿作廢的注單依剩餘腿重算 potential_payout。
//
// 只處理 status 仍為 open 的注單 —— 已經結算過的一律略過。這是「不重複派彩」
// 的真正權威:advisory lock 防的是併發,狀態檢查防的是重跑。
func (s *Service[TX]) applyOutcomes(ctx context.Context, tx TX, m Match, betIDs []int64) ([]BetOutcome, error) {
	bets, err := s.repo.BetsByIDs(ctx, tx, betIDs)
	if err != nil {
		return nil, fmt.Errorf("讀注單: %w", err)
	}
	legsByBet, err := s.repo.LegsByBetIDs(ctx, tx, betIDs)
	if err != nil {
		return nil, fmt.Errorf("讀注單的腿: %w", err)
	}

	// 重算要用這屆的串關封頂;只在真的有腿作廢時才讀。
	// 一張注單的腿不會跨屆(bets.tournament_id 是單一權威),所以一份參數夠用。
	var cfg *OddsConfig
	oddsCfg := func() (OddsConfig, error) {
		if cfg == nil {
			r, err := s.rules(ctx, tx, m.TournamentID)
			if err != nil {
				return OddsConfig{}, err
			}
			c := r.Odds()
			cfg = &c
		}
		return *cfg, nil
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
		if hasVoidLeg(legs) {
			// 一腿不成立了,把它從乘積裡拿掉;剩餘每一腿仍用自己下注當下鎖定的賠率。
			if remaining := RemainingOdds(legs); len(remaining) > 0 {
				c, err := oddsCfg()
				if err != nil {
					return nil, err
				}
				p := ParlayPayout(b.Stake, remaining, c.MaxParlayMilli)
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
			// 派彩是新的正數分錄,金額是注單上寫死的承諾(或移腿後的重算值)。
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
				// 沒判定出結果也沒重算金額,這張注單這次完全沒變 —— 不寫。
				continue
			}
		}

		if err := s.repo.UpdateBet(ctx, tx, upd); err != nil {
			return nil, fmt.Errorf("更新注單 %d: %w", b.ID, err)
		}
		outcomes = append(outcomes, oc)
	}
	return outcomes, nil
}

func hasVoidLeg(legs []Leg) bool {
	for _, l := range legs {
		if l.Result == LegVoid {
			return true
		}
	}
	return false
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
