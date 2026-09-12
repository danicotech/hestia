package prize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// keyPrefix 是冪等鍵的命名空間。idempotency_keys.key 是全域唯一的主鍵,
// 前綴是「這把鍵屬於誰」的唯一依據。
const keyPrefix = "activity.prize:"

// Service 是發獎的全部規則。
type Service struct {
	repo     Repository
	led      Ledger
	currency string
}

// New 建立 Service。
//
// currency 留空時退回 betting.DefaultCurrency —— 平台目前只有一種幣別,
// 而那個字面值的權威已經在 betting 宣告過了,這裡不抄第二份(鐵則 9)。
func New(repo Repository, led Ledger, currency string) *Service {
	if currency == "" {
		currency = betting.DefaultCurrency
	}
	return &Service{repo: repo, led: led, currency: currency}
}

// AwardPrizes 發放一屆賽事的全部獎金。
//
// 流程刻意分成兩段:**先把整份獎金表算完,再開始動錢**。
// 任何算不出來的情況(季軍、決賽未決、對戰表破損、設定不合法)都在碰錢之前
// 就失敗,不會出現「發了一半才發現名次不對」。
//
// 可以重複呼叫:已發的原樣回放(Award.Replayed = true),只有還沒發的會真的入帳。
func (s *Service) AwardPrizes(ctx context.Context, p AwardParams) (*AwardResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	t, err := s.repo.TournamentBySlug(ctx, p.TournamentSlug)
	if err != nil {
		return nil, fmt.Errorf("讀賽事 %s: %w", p.TournamentSlug, err)
	}
	if t.PublicID == "" {
		// 冪等鍵由 public_id 推出。沒有它,兩屆賽事的同一位選手會共用一把鍵。
		return nil, fmt.Errorf("%w: 賽事 %s 缺 public_id,推不出冪等鍵", ErrInvalidRequest, t.Slug)
	}
	// 抽籤確認之後才有名次可談。報名期就按發獎不是「早了一點」,
	// 而是連參賽名單都還沒確定。
	if err := tournament.RequirePhase(t.Phase, tournament.PhaseInProgress, tournament.PhaseFinished); err != nil {
		return nil, err
	}

	// ParseConfig 的約定是「壞欄位退回預設值、錯誤只是診斷」,呼叫端通常可以
	// 忽略 err 繼續跑。**發獎是唯一不能這樣做的地方**:prizes 的預設值是 0,
	// 所以一個打錯的 prizes.champion 會安靜地變成「不發冠軍獎」——
	// 裁判只會看到「按了沒反應」,而不是一個指得出哪個欄位錯了的訊息。
	cfg, err := tournament.ParseConfig(t.ConfigRaw)
	if err != nil {
		return nil, fmt.Errorf("賽事 %s 的設定要先修正才能發獎: %w", t.Slug, err)
	}

	players, err := s.repo.ListPlayers(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("讀選手名單 tournament=%d: %w", t.ID, err)
	}

	awards, err := s.plan(ctx, t, cfg, players)
	if err != nil {
		return nil, err
	}

	res := &AwardResult{TournamentID: t.ID, Awards: awards}
	if err := s.pay(ctx, t, p.ActorUserID, players, res); err != nil {
		return nil, err
	}
	return res, nil
}

// plan 算出整份獎金表。不動錢,也不需要知道誰綁了帳號以外的事。
func (s *Service) plan(
	ctx context.Context, t tournament.Tournament, cfg tournament.Config, players []tournament.Player,
) ([]Award, error) {
	// 季軍先擋:它是「不管對戰表長什麼樣都發不出來」的那一種,
	// 排在最前面才不會讓裁判先收到一個關於決賽的錯誤、修好之後才撞上這個。
	if cfg.Prizes.Third > 0 {
		return nil, fmt.Errorf("%w(config 設了 prizes.third = %d)",
			ErrThirdPlaceUndecidable, cfg.Prizes.Third)
	}

	awards := make([]Award, 0, len(players)+2)
	byID := make(map[int64]tournament.Player, len(players))
	for _, pl := range players {
		byID[pl.ID] = pl
	}

	// 只有真的要發冠亞軍時才去推名次:一屆只設參賽獎的賽事,不該因為
	// 決賽還沒打完就發不了參賽獎。
	if cfg.Prizes.Champion > 0 || cfg.Prizes.RunnerUp > 0 {
		matches, err := s.repo.ListMatches(ctx, t.ID)
		if err != nil {
			return nil, fmt.Errorf("讀對戰表 tournament=%d: %w", t.ID, err)
		}
		championID, runnerUpID, err := standings(matches)
		if err != nil {
			return nil, err
		}
		if cfg.Prizes.Champion > 0 {
			a, err := newAward(byID, championID, KindChampion, cfg.Prizes.Champion)
			if err != nil {
				return nil, err
			}
			awards = append(awards, a)
		}
		if cfg.Prizes.RunnerUp > 0 {
			a, err := newAward(byID, runnerUpID, KindRunnerUp, cfg.Prizes.RunnerUp)
			if err != nil {
				return nil, err
			}
			awards = append(awards, a)
		}
	}

	// 參賽獎發給**所有**報名者:淘汰了也拿、棄賽了也拿。
	// 棄賽不是犯規,而報名本身就是這個獎要獎勵的事。
	if cfg.Prizes.Participation > 0 {
		for _, pl := range players {
			a, err := newAward(byID, pl.ID, KindParticipation, cfg.Prizes.Participation)
			if err != nil {
				return nil, err
			}
			awards = append(awards, a)
		}
	}
	return awards, nil
}

// pay 逐筆入帳。
//
// 一筆一個 Apply,不是一次 Apply 帶全部 Op:冪等鍵是「這位選手的這個獎項」,
// 一把鍵只能配一次 Apply。代價是 N 次呼叫,換到的是「第二次發獎只補新綁的人」
// (見套件註解),那個性質比省下幾次往返重要得多。
//
// 中途失敗一律整個中斷並上拋。繼續發下去的話,裁判會收到一份「有幾筆成功、
// 有幾筆失敗」的清單,而他能做的事跟現在一樣 —— 修好之後再按一次。
func (s *Service) pay(
	ctx context.Context, t tournament.Tournament, actorID int64, players []tournament.Player, res *AwardResult,
) error {
	userByPlayer := make(map[int64]int64, len(players))
	for _, pl := range players {
		if pl.Bound() {
			userByPlayer[pl.ID] = *pl.UserID
		}
	}

	granted, allReplayed := 0, true
	for i := range res.Awards {
		a := &res.Awards[i]
		if a.SkippedReason != "" {
			continue
		}
		userID, ok := userByPlayer[a.PlayerID]
		if !ok {
			// plan 已經把未綁定的標成 skipped,走到這裡代表兩處判斷不一致。
			return fmt.Errorf("%w: 選手 %s 沒有平台帳號卻被排進發獎", ErrBracketBroken, a.PlayerPublicID)
		}

		key := AwardKey(t.PublicID, a.PlayerPublicID, a.Kind)
		refID := a.PlayerID
		refType := refTypePlayer
		actor := actorID
		out, err := s.led.Apply(ctx, ledger.ApplyParams{
			IdempotencyKey: key,
			// 金額進雜湊:同一把鍵、同一個獎項,金額卻變了(裁判改了 config
			// 之後重發),帳本會回 ErrIdempotencyConflict 而不是默默補差額。
			RequestHash: hashOf(key, s.currency, strconv.FormatInt(a.Amount, 10)),
			Ops: []ledger.Op{{
				UserID:   userID,
				Currency: s.currency,
				Amount:   a.Amount,
				Reason:   ledger.ReasonTournamentPrize,
				RefType:  &refType,
				RefID:    &refID,
				ActorID:  &actor,
			}},
		})
		if err != nil {
			return fmt.Errorf("發 %s 給 %s(%s): %w", a.Kind, a.DisplayName, a.PlayerPublicID, err)
		}
		if len(out.Entries) != 1 {
			return fmt.Errorf("%w: 一筆 Op 換回 %d 筆分錄(%s / %s)",
				ErrLedgerStateConflict, len(out.Entries), a.PlayerPublicID, a.Kind)
		}
		a.Granted = true
		a.EntryID = out.Entries[0].EntryID
		a.Replayed = out.Replayed
		granted++
		if !out.Replayed {
			allReplayed = false
		}
	}
	res.Replayed = granted > 0 && allReplayed
	return nil
}

// standings 從對戰表推出冠亞軍。
//
// 決賽 = 最後一輪的 slot 0。單淘汰的最後一輪只有這一場
// (bracket 的樹寬每輪減半),所以不需要別的判準。
func standings(matches []match.Match) (championID, runnerUpID int64, err error) {
	totalRounds := 0
	for _, m := range matches {
		if m.Round > totalRounds {
			totalRounds = m.Round
		}
	}
	if totalRounds == 0 {
		// 沒有任何場次:還沒抽籤。不自己造一個「唯一的報名者就是冠軍」的規則 ——
		// 一人成賽是退化情況,誰是冠軍該由裁判決定,不是由空的對戰表推出來。
		return 0, 0, match.ErrBracketMissing
	}

	var final *match.Match
	for i := range matches {
		if matches[i].Round == totalRounds && matches[i].Slot == 0 {
			final = &matches[i]
			break
		}
	}
	if final == nil {
		return 0, 0, fmt.Errorf("%w: 找不到決賽(round=%d slot=0)", ErrBracketBroken, totalRounds)
	}
	if final.Status != match.StatusDone || final.WinnerPlayerID == 0 {
		return 0, 0, fmt.Errorf("%w: 決賽 %s 目前是 %s", ErrFinalNotDecided, final.PublicID, final.Status)
	}

	championID = final.WinnerPlayerID
	loser := final.Opponent(championID)
	if !loser.Seated() {
		// 勝者不是場上兩人之一,或決賽根本只有一方。兩者都是資料異常。
		return 0, 0, fmt.Errorf("%w: 決賽 %s 的勝者 %d 找不到對手",
			ErrBracketBroken, final.PublicID, championID)
	}
	return championID, loser.ID, nil
}

// newAward 組出一筆待發的獎。未綁定平台帳號者在這裡就標成 skipped。
func newAward(byID map[int64]tournament.Player, playerID int64, kind Kind, amount int64) (Award, error) {
	pl, ok := byID[playerID]
	if !ok {
		return Award{}, fmt.Errorf("%w: 選手 %d 不在本屆名單裡", ErrBracketBroken, playerID)
	}
	if pl.PublicID == "" {
		return Award{}, fmt.Errorf("%w: 選手 %d 缺 public_id,推不出冪等鍵", ErrInvalidRequest, playerID)
	}
	a := Award{
		PlayerID:       pl.ID,
		PlayerPublicID: pl.PublicID,
		DisplayName:    pl.DisplayName,
		Kind:           kind,
		Amount:         amount,
	}
	if !pl.Bound() {
		a.SkippedReason = UnboundReason
	}
	return a, nil
}

// AwardKey 是一筆獎金的冪等鍵。
//
// 導出是為了讓 adapter 與稽核查詢能反查同一把鍵(「這筆獎到底發了沒」
// 在 idempotency_keys 裡答得出來),而不是各自再拼一次字串。
//
// 用 public_id 而不是內部 id:這把鍵是全域主鍵,而 ULID 保證跨屆不會撞。
func AwardKey(tournamentPublicID, playerPublicID string, kind Kind) string {
	return keyPrefix + tournamentPublicID + ":" + playerPublicID + ":" + string(kind)
}

func hashOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// validate 做所有不必碰資料庫就能判斷的檢查。
func (p AwardParams) validate() error {
	if strings.TrimSpace(p.TournamentSlug) == "" {
		return fmt.Errorf("tournament_slug 必填: %w", ErrInvalidRequest)
	}
	if p.ActorUserID <= 0 {
		// 每一筆分錄的 actor_id 都要指得出是誰發的,否則獎金在帳本上沒有來歷。
		return fmt.Errorf("發獎必須指明執行的裁判: %w", tournament.ErrActorRequired)
	}
	return nil
}
