package bettingpg_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 測試資料一律直接寫 SQL 建立,不繞道別的服務:
// 這裡要驗的是 bettingpg 的行為,前置資料越少間接層越好查。
// public_id 一律用 internal/shared/ulid(專案鐵則 5:對外只出現 ULID)。

// voteUserSeq 給投票用的假使用者 id。votes.user_id 是弱參照(不設 FK),
// 只用來擋灌票 —— 測試不需要為了數票去建真的帳號。
var voteUserSeq atomic.Int64

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

// newTournament 建一屆賽事。config 傳 "{}" 即全部走 OddsConfig.Normalize() 的預設值。
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

// newMatch 建一場可下注的比賽(status='ready')。
func newMatch(t *testing.T, tournamentID, p1, p2 int64) (id int64, publicID string) {
	t.Helper()
	publicID = newULID(t)
	err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.matches
		   (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, status)
		 VALUES ($1, $2, 1, $3, $4, $5, 'ready') RETURNING id`,
		publicID, tournamentID, matchSlotSeq.Add(1), p1, p2,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建場次: %v", err)
	}
	return id, publicID
}

// finishMatch 把場次判成 done + 勝者(正常賽果)。賽事服務的職責,測試直接寫。
func finishMatch(t *testing.T, matchID, winnerPlayerID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE activity.matches
		    SET status = 'done', winner_player_id = $2, finished_at = now()
		  WHERE id = $1`, matchID, winnerPlayerID)
	if err != nil {
		t.Fatalf("判定場次 %d: %v", matchID, err)
	}
}

// walkoverMatch 把場次判成不戰而勝。賠率是按「真的打一場」算的,
// 所以這種場次走 VoidMatch 退款,不走 SettleMatch。
func walkoverMatch(t *testing.T, matchID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE activity.matches
		    SET status = 'done', result_kind = 'walkover', finished_at = now()
		  WHERE id = $1`, matchID)
	if err != nil {
		t.Fatalf("判定棄賽 %d: %v", matchID, err)
	}
}

// castVotes 灌票(每票一個不同的假使用者,對應 votes_match_user_uq)。
func castVotes(t *testing.T, matchID int64, p1, p2 int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < p1+p2; i++ {
		side := 1
		if i >= p1 {
			side = 2
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO activity.votes (match_id, user_id, side) VALUES ($1, $2, $3)`,
			matchID, voteUserSeq.Add(1)+900_000_000, side)
		if err != nil {
			t.Fatalf("灌票 match=%d: %v", matchID, err)
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
