package playpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/play"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/playpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 對真 Postgres 跑(hestia/CLAUDE.md)。這裡測的全是動錢路徑,
// 而動錢的併發行為正是資料庫的鎖行為 —— mock 掉等於什麼都沒測。

var (
	pool *pgxpool.Pool
	svc  *playpg.Service
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = playpg.New(pool, ledgerpg.New(pool))
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id, display_name)
		 VALUES (gen_random_uuid()::text, 'player') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("建使用者: %v", err)
	}
	return id
}

func newCommunity(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.communities (public_id, name)
		 VALUES (gen_random_uuid()::text, 'play-test') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("建社群: %v", err)
	}
	return id
}

// fund 直接塞餘額。繞過帳本是測試特權:這裡要測的是遊戲,不是發錢。
func fund(t *testing.T, userID, amount int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.user_balances (user_id, currency, scope, balance)
		 SELECT $1, 'coin', c.scope, $2 FROM platform.currencies c WHERE c.code = 'coin'
		 ON CONFLICT (user_id, currency, COALESCE(community_id, 0))
		 DO UPDATE SET balance = EXCLUDED.balance`, userID, amount); err != nil {
		t.Fatalf("塞餘額: %v", err)
	}
	// 餘額是帳本的快取,對帳 job 會抓不一致 —— 補一筆分錄讓兩邊相等。
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.token_entries (user_id, currency, scope, amount, reason)
		 SELECT $1, 'coin', c.scope, $2, 'admin_grant'
		 FROM platform.currencies c WHERE c.code = 'coin'`, userID, amount); err != nil {
		t.Fatalf("補分錄: %v", err)
	}
}

func balanceOf(t *testing.T, userID int64) int64 {
	t.Helper()
	var bal int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(balance), 0) FROM platform.user_balances
		 WHERE user_id = $1 AND currency = 'coin'`, userID).Scan(&bal); err != nil {
		t.Fatalf("讀餘額: %v", err)
	}
	return bal
}

// ── 小遊戲 ────────────────────────────────────────────────────

func TestPlayGame_MovesMoneyAndRecordsDraw(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)

	res, err := svc.PlayGame(ctx, play.GameParams{
		UserID: u, CommunityID: c, Game: "rps", Choice: "rock", Stake: 100,
		IdempotencyKey: fmt.Sprintf("t-%d", u),
	})
	if err != nil {
		t.Fatalf("PlayGame: %v", err)
	}

	// 淨變動 = 回收 − 投注。餘額必須剛好等於它。
	want := 1000 + res.Net()
	if got := balanceOf(t, u); got != want {
		t.Fatalf("餘額 = %d,應為 %d(淨變動 %d)", got, want, res.Net())
	}

	// 每一局都要留痕:少了它,「這個月遊戲吐了多少」就永遠算不出來。
	var draws int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.chance_draws WHERE user_id = $1 AND kind = 'minigame'`, u,
	).Scan(&draws); err != nil {
		t.Fatalf("讀抽籤紀錄: %v", err)
	}
	if draws != 1 {
		t.Fatalf("應留下一筆抽籤紀錄,得到 %d", draws)
	}
	if res.Seed == "" {
		t.Fatal("沒有種子,事後無法驗證")
	}
}

// 連點是常態不是例外。同一個 interaction 重送只能結算一次 ——
// 這是帳本鐵則三,而遊戲是最容易被連點的地方。
func TestPlayGame_IsIdempotent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)
	key := fmt.Sprintf("dup-%d", u)

	first, err := svc.PlayGame(ctx, play.GameParams{
		UserID: u, CommunityID: c, Game: "dice", Choice: "big", Stake: 100,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	after := balanceOf(t, u)

	for i := 0; i < 3; i++ {
		if _, err := svc.PlayGame(ctx, play.GameParams{
			UserID: u, CommunityID: c, Game: "dice", Choice: "big", Stake: 100,
			IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("重送第 %d 次: %v", i+1, err)
		}
	}
	if got := balanceOf(t, u); got != after {
		t.Fatalf("重送不該再動錢:%d → %d(第一局淨變動 %d)", after, got, first.Net())
	}
}

func TestPlayGame_InsufficientBalance(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 50)

	_, err := svc.PlayGame(ctx, play.GameParams{
		UserID: u, CommunityID: c, Game: "rps", Choice: "rock", Stake: 100,
		IdempotencyKey: fmt.Sprintf("poor-%d", u),
	})
	if err == nil {
		t.Fatal("餘額不足應該失敗")
	}
	if got := balanceOf(t, u); got != 50 {
		t.Fatalf("失敗不該動到餘額,得到 %d", got)
	}
}

// 每日上限是三個賭博管道共用的計數。分開算的話上限就變成三倍,
// 而設它的理由(經濟失控與觀感)不會因為換一個玩法就消失。
func TestPlayGame_DailyLimitShared(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1_000_000)

	for i := 0; i < play.MaxDrawsPerDay; i++ {
		if _, err := svc.PlayGame(ctx, play.GameParams{
			UserID: u, CommunityID: c, Game: "rps", Choice: "rock", Stake: 1,
			IdempotencyKey: fmt.Sprintf("lim-%d-%d", u, i),
		}); err != nil {
			t.Fatalf("第 %d 局: %v", i+1, err)
		}
	}
	_, err := svc.PlayGame(ctx, play.GameParams{
		UserID: u, CommunityID: c, Game: "rps", Choice: "rock", Stake: 1,
		IdempotencyKey: fmt.Sprintf("lim-%d-over", u),
	})
	if !errors.Is(err, play.ErrDailyLimit) {
		t.Fatalf("超過上限應回 ErrDailyLimit,得到 %v", err)
	}
}

func TestPlayGame_RejectsBadChoice(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u, c := newUser(t), newCommunity(t)
	fund(t, u, 1000)

	_, err := svc.PlayGame(ctx, play.GameParams{
		UserID: u, CommunityID: c, Game: "rps", Choice: "lizard", Stake: 100,
		IdempotencyKey: fmt.Sprintf("bad-%d", u),
	})
	if !errors.Is(err, play.ErrInvalidParams) {
		t.Fatalf("不合法的選擇應回 ErrInvalidParams,得到 %v", err)
	}
	// 參數錯誤在扣款之前就該回絕
	if got := balanceOf(t, u); got != 1000 {
		t.Fatalf("參數錯誤不該動錢,得到 %d", got)
	}
}
