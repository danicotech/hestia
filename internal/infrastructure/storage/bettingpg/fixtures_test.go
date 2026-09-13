package bettingpg_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 測試資料一律直接寫 SQL 建立,不繞道別的服務:
// 這裡要驗的是 bettingpg 的行為,前置資料越少間接層越好查。
// public_id 一律用 internal/shared/ulid(專案鐵則 5:對外只出現 ULID)。
//
// 唯一的例外是盤口:它走 svc.OpenMarketsInTx 建,因為那正是本套件要驗的路徑之一
// (依 config 展開、public_id 由 adapter 給、撞唯一索引出聲)。

// voteUserSeq 給投票用的假使用者 id。votes.user_id 是弱參照(不設 FK),
// 只用來擋灌票 —— 測試不需要為了數票去建真的帳號。
var voteUserSeq atomic.Int64

// fullConfig 是開發庫那種 config:三局兩勝、四種盤口、duration 線 90 秒(migration 00008)。
const fullConfig = `{
  "version": 2,
  "format": {"best_of": 3, "preamble_every_round": true, "third_place_match": true},
  "betting": {
    "enabled": true,
    "markets": [
      {"kind": "match_winner"},
      {"kind": "round_winner"},
      {"kind": "duration", "line_seconds": 90},
      {"kind": "score"}
    ],
    "odds": {"kind": "vote_share"},
    "parlay": {"legs_per_match": 1},
    "close_at": "first_round_start"
  }
}`

func newULID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New()
	if err != nil {
		t.Fatalf("產生 ULID: %v", err)
	}
	return id
}

func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES ($1) RETURNING id`, newULID(t),
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

// credit 經帳本入帳(不是直接改 user_balances)——
// 連測試的前置資料都走同一條路,SUM(entries) = balance 的對帳才有意義。
func credit(t *testing.T, userID, amount int64) {
	t.Helper()
	key := fmt.Sprintf("bettest-credit-%s", newULID(t))
	_, err := led.Apply(context.Background(), ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: key,
		Ops: []ledger.Op{{
			UserID: userID, Currency: "coin", Amount: amount, Reason: ledger.ReasonAdminGrant,
		}},
	})
	if err != nil {
		t.Fatalf("入帳 user=%d amount=%d: %v", userID, amount, err)
	}
}

func balanceOf(t *testing.T, userID int64) int64 {
	t.Helper()
	bal, err := led.Balance(context.Background(), userID, "coin")
	if err != nil {
		t.Fatalf("讀餘額 user=%d: %v", userID, err)
	}
	return bal
}

// newTournament 建一屆賽事。config 傳 "{}" 即 rules.Default()(v1 語意:只有勝負盤、單場定勝負)。
func newTournament(t *testing.T, config string) (id int64, slug string) {
	t.Helper()
	slug = "t-" + newULID(t)
	err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, config)
		 VALUES ($1, $2, 'betting test', 1, $3::jsonb) RETURNING id`,
		newULID(t), slug, config,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建賽事: %v", err)
	}
	return id, slug
}

// newPlayer 建一位參賽者(含跨屆選手檔案),兩處 user_id 都綁同一個帳號。
// userID 為 nil = 純活動報名(兩邊都沒綁)。
func newPlayer(t *testing.T, tournamentID int64, userID *int64) int64 {
	t.Helper()
	return newPlayerSplit(t, tournamentID, userID, userID)
}

// newPlayerSplit 分別指定 tournament_players.user_id 與 fencers.user_id。
// 「選手不得賭自己」有兩條比對路徑,只有把兩者拆開才驗得出第二條真的在擋:
// 純活動報名(前者 NULL)但劍士本人綁過平台帳號的情況。
func newPlayerSplit(t *testing.T, tournamentID int64, playerUserID, fencerUserID *int64) int64 {
	t.Helper()
	ctx := context.Background()
	var fencerID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO activity.fencers (public_id, game_id, user_id)
		 VALUES ($1, $2, $3) RETURNING id`,
		newULID(t), "g-"+newULID(t), fencerUserID,
	).Scan(&fencerID)
	if err != nil {
		t.Fatalf("建選手檔案: %v", err)
	}
	var playerID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO activity.tournament_players
		   (public_id, tournament_id, fencer_id, user_id, display_name,
		    passcode_hash, discord_name)
		 VALUES ($1, $2, $3, $4, $5, 'x', 'tester#0') RETURNING id`,
		newULID(t), tournamentID, fencerID, playerUserID, "P"+newULID(t)[:6],
	).Scan(&playerID)
	if err != nil {
		t.Fatalf("建參賽者: %v", err)
	}
	return playerID
}

// matchSlotSeq 讓同一屆的場次不撞 UNIQUE(tournament_id, round, slot)。
var matchSlotSeq atomic.Int64

// mrec 是一場可下注的比賽:場次 + 依該屆規則開好的盤口。
type mrec struct {
	id      int64
	pub     string
	p1, p2  int64
	markets []betting.Market
}

// market 找某種盤口(逐回合盤口指定 roundNo,其餘傳 0)。
func (m mrec) market(t *testing.T, kind betting.MarketKind, roundNo int) betting.Market {
	t.Helper()
	for _, mk := range m.markets {
		if mk.Kind == kind && mk.RoundNo == roundNo {
			return mk
		}
	}
	t.Fatalf("場次 %s 沒有 %s round=%d 的盤口", m.pub, kind, roundNo)
	return betting.Market{}
}

// leg 組一腿。
func (m mrec) leg(t *testing.T, kind betting.MarketKind, roundNo int, o betting.Outcome) betting.LegInput {
	t.Helper()
	return betting.LegInput{MatchPublicID: m.pub, MarketPublicID: m.market(t, kind, roundNo).PublicID, Outcome: o}
}

// winner 是最常用的一腿:整場勝負。
func (m mrec) winner(t *testing.T, o betting.Outcome) betting.LegInput {
	t.Helper()
	return m.leg(t, betting.MarketMatchWinner, 0, o)
}

// newMatch 建一場 ready 的比賽並開盤(走 OpenMarketsInTx)。
func newMatch(t *testing.T, tournamentID, p1, p2 int64) mrec {
	t.Helper()
	m := mrec{pub: newULID(t), p1: p1, p2: p2}
	err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.matches
		   (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, status)
		 VALUES ($1, $2, 1, $3, $4, $5, 'ready') RETURNING id`,
		m.pub, tournamentID, matchSlotSeq.Add(1), p1, p2,
	).Scan(&m.id)
	if err != nil {
		t.Fatalf("建場次: %v", err)
	}
	m.markets = openMarkets(t, m.pub)
	return m
}

// openMarkets 走正式路徑開盤。
func openMarkets(t *testing.T, matchPub string) []betting.Market {
	t.Helper()
	var out []betting.Market
	err := repo.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = svc.OpenMarketsInTx(ctx, tx, matchPub)
		return err
	})
	if err != nil {
		t.Fatalf("開盤 %s: %v", matchPub, err)
	}
	return out
}

// startMatch 場次開打:status live、started_at、關盤(走 CloseMarketsInTx)。回關掉的盤口數。
func startMatch(t *testing.T, m mrec) int {
	t.Helper()
	ctx := context.Background()
	// setup_confirmed_at 一併寫:matches_started_requires_setup_check 要求未確認設定不得開打
	// (裁判流程的事,這裡只是讓資料合法)。
	if _, err := pool.Exec(ctx,
		`UPDATE activity.matches
		    SET status = 'live', started_at = now(), setup_confirmed_at = now()
		  WHERE id = $1`, m.id); err != nil {
		t.Fatalf("開打 %d: %v", m.id, err)
	}
	var n int
	err := repo.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = svc.CloseMarketsInTx(ctx, tx, m.pub)
		return err
	})
	if err != nil {
		t.Fatalf("關盤 %s: %v", m.pub, err)
	}
	return n
}

// playRound 記一回合:seconds 是時長(started_at 往回推),winner 是選手 id。
func playRound(t *testing.T, m mrec, roundNo int, winnerPlayerID int64, seconds int64) {
	t.Helper()
	// 時間全由 SQL 算(now() − seconds),不讓 Go 的時鐘與 DB 的時鐘混用。
	_, err := pool.Exec(context.Background(),
		`INSERT INTO activity.match_rounds (match_id, round_no, started_at, finished_at, winner_player_id)
		 VALUES ($1, $2, now() - make_interval(secs => $3), now(), $4)`,
		m.id, roundNo, seconds, winnerPlayerID)
	if err != nil {
		t.Fatalf("記回合 match=%d round=%d: %v", m.id, roundNo, err)
	}
}

// playRoundExact 記一回合,時長精確到奈秒(邊界測試用)。
func playRoundExact(t *testing.T, m mrec, roundNo int, winnerPlayerID int64, d time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO activity.match_rounds (match_id, round_no, started_at, finished_at, winner_player_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		m.id, roundNo, time.Unix(1_800_000_000, 0).UTC(), time.Unix(1_800_000_000, 0).UTC().Add(d), winnerPlayerID)
	if err != nil {
		t.Fatalf("記回合 match=%d round=%d: %v", m.id, roundNo, err)
	}
}

// finishMatch 把場次判成 done + 勝者(正常賽果)。賽事服務的職責,測試直接寫。
func finishMatch(t *testing.T, m mrec, winnerPlayerID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE activity.matches
		    SET status = 'done', winner_player_id = $2, finished_at = now()
		  WHERE id = $1`, m.id, winnerPlayerID)
	if err != nil {
		t.Fatalf("判定場次 %d: %v", m.id, err)
	}
}

// finishBestOf3 記完一場三局兩勝並判定整場。winners 是逐回合勝者,seconds 是逐回合時長。
func finishBestOf3(t *testing.T, m mrec, winners []int64, seconds []int64) {
	t.Helper()
	for i, w := range winners {
		playRound(t, m, i+1, w, seconds[i])
	}
	finishMatch(t, m, winners[len(winners)-1])
}

// walkoverMatch 把場次判成不戰而勝。賠率是按「真的打一場」算的,
// 所以這種場次走 VoidMatch 退款,不走 SettleMatch。
func walkoverMatch(t *testing.T, m mrec) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE activity.matches
		    SET status = 'done', result_kind = 'walkover', finished_at = now()
		  WHERE id = $1`, m.id)
	if err != nil {
		t.Fatalf("判定棄賽 %d: %v", m.id, err)
	}
}

// castVotes 對一個盤口灌票(每票一個不同的假使用者,對應 votes_market_user_uq)。
func castVotes(t *testing.T, mk betting.Market, votes map[betting.Outcome]int) {
	t.Helper()
	ctx := context.Background()
	for o, n := range votes {
		for i := 0; i < n; i++ {
			_, err := pool.Exec(ctx,
				`INSERT INTO activity.votes (market_id, user_id, outcome) VALUES ($1, $2, $3)`,
				mk.ID, voteUserSeq.Add(1)+900_000_000, string(o))
			if err != nil {
				t.Fatalf("灌票 market=%d: %v", mk.ID, err)
			}
		}
	}
}

// betRow 是注單在資料庫裡的樣子(含三個稽核鏈欄位)。
type betRow struct {
	ID                 int64
	Status             string
	Stake              int64
	PotentialPayout    int64
	PayoutRecalculated bool
	StakeEntryID       *int64
	RefundEntryID      *int64
	PayoutEntryID      *int64
	SettledAt          *string
}

func readBet(t *testing.T, publicID string) betRow {
	t.Helper()
	var b betRow
	err := pool.QueryRow(context.Background(),
		`SELECT id, status, stake, potential_payout, payout_recalculated,
		        ledger_stake_entry_id, ledger_refund_entry_id, ledger_payout_entry_id,
		        settled_at::text
		   FROM activity.bets WHERE public_id = $1`, publicID,
	).Scan(&b.ID, &b.Status, &b.Stake, &b.PotentialPayout, &b.PayoutRecalculated,
		&b.StakeEntryID, &b.RefundEntryID, &b.PayoutEntryID, &b.SettledAt)
	if err != nil {
		t.Fatalf("讀注單 %s: %v", publicID, err)
	}
	return b
}

// marketStatus 讀盤口目前的狀態。
func marketStatus(t *testing.T, marketID int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM activity.markets WHERE id = $1`, marketID).Scan(&s); err != nil {
		t.Fatalf("讀盤口 %d: %v", marketID, err)
	}
	return s
}

// legResult 讀某張注單在某盤口那一腿的 result。
func legResult(t *testing.T, betID, marketID int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT result FROM activity.bet_legs WHERE bet_id = $1 AND market_id = $2`, betID, marketID).Scan(&s); err != nil {
		t.Fatalf("讀腿 bet=%d market=%d: %v", betID, marketID, err)
	}
	return s
}

func countRows(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("計數失敗 (%s): %v", query, err)
	}
	return n
}

// assertReconciled 驗證 ledger-invariants 的核心性質:
// user_balances 只是快取,任何時刻都必須等於 SUM(token_entries)。
func assertReconciled(t *testing.T, userIDs ...int64) {
	t.Helper()
	for _, id := range userIDs {
		var cached, summed int64
		err := pool.QueryRow(context.Background(),
			`SELECT
			   COALESCE((SELECT balance FROM platform.user_balances
			              WHERE user_id = $1 AND currency = 'coin'), 0),
			   COALESCE((SELECT sum(amount) FROM platform.token_entries
			              WHERE user_id = $1 AND currency = 'coin'), 0)`, id,
		).Scan(&cached, &summed)
		if err != nil {
			t.Fatalf("對帳查詢 user=%d: %v", id, err)
		}
		if cached != summed {
			t.Fatalf("對帳失敗 user=%d: user_balances=%d SUM(token_entries)=%d", id, cached, summed)
		}
	}
}

// entryOf 讀一筆分錄,用來驗「派彩/退款是新的正數分錄」與稽核鏈指向。
func entryOf(t *testing.T, id int64) (amount int64, reason string, refType *string, refID *int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT amount, reason, ref_type, ref_id FROM platform.token_entries WHERE id = $1`, id,
	).Scan(&amount, &reason, &refType, &refID)
	if err != nil {
		t.Fatalf("讀分錄 %d: %v", id, err)
	}
	return amount, reason, refType, refID
}
