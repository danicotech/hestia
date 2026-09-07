package dailypg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/daily"
	"github.com/danicotech/hestia/internal/infrastructure/storage/dailypg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義)。
// 共用一個容器,各測試用獨立使用者隔離。金額預期值來自 seed(00014):
// daily_base=100、daily_streak_step=10、daily_streak_cap=200、timezone_change_min_gap_hours=20。

var (
	pool *pgxpool.Pool
	svc  *dailypg.Service
)

// TestMain 讓整個 package 共用一個容器(每個測試各起一個要多花 ~30 秒)。
func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = dailypg.New(pool, ledgerpg.New(pool))
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
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&id) // timezone 用預設 Asia/Taipei
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

// backdate 把使用者的簽到紀錄與狀態整體往回撥 days 天、claimed_at 往回撥 hoursAgo 小時,
// 模擬「上次簽到是 N 天前」。真實時間無法快轉,測試靠操縱資料的日期欄位。
func backdate(t *testing.T, userID int64, days int, hoursAgo int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE platform.daily_claims
		 SET claim_date = claim_date - $2::int, claimed_at = now() - make_interval(hours => $3)
		 WHERE user_id = $1`, userID, days, hoursAgo); err != nil {
		t.Fatalf("回撥 daily_claims: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE platform.user_daily_state SET last_claim_date = last_claim_date - $2::int
		 WHERE user_id = $1`, userID, days); err != nil {
		t.Fatalf("回撥 user_daily_state: %v", err)
	}
	// 上次簽到的帳本冪等鍵帶著當時的 claim_date——真的在昨天簽,鍵就是昨天的日期,
	// 不會與今天的鍵相撞。回撥日期時把鍵一併清掉以維持這個對應
	// (正式環境同人同日在 daily_claims 的 PK 就擋下,到不了帳本,鍵不可能相撞)。
	if _, err := pool.Exec(ctx,
		`DELETE FROM platform.idempotency_keys WHERE key LIKE $1`,
		fmt.Sprintf("daily:%d:%%", userID)); err != nil {
		t.Fatalf("回撥冪等鍵: %v", err)
	}
}

func balanceAndEntrySum(t *testing.T, userID int64) (balance, entrySum int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE((SELECT balance FROM platform.user_balances WHERE user_id=$1 AND currency='coin'), 0),
		       COALESCE((SELECT SUM(amount) FROM platform.token_entries WHERE user_id=$1), 0)`,
		userID).Scan(&balance, &entrySum)
	if err != nil {
		t.Fatalf("查餘額與分錄合計: %v", err)
	}
	return balance, entrySum
}

// assertConsistent 驗證帳本不變量:SUM(entries) = balance(tx 原子性的最終體現)。
func assertConsistent(t *testing.T, userID int64) {
	t.Helper()
	bal, sum := balanceAndEntrySum(t, userID)
	if bal != sum {
		t.Fatalf("帳不平: balance=%d, SUM(entries)=%d", bal, sum)
	}
}

// 正常簽到:金額、簽到紀錄、狀態、outbox 事件全部正確且同 tx 落地
func TestClaimNormal(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	res, err := svc.Claim(ctx, u)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.Amount != 100 || res.Streak != 1 {
		t.Fatalf("首簽應為 amount=100 streak=1,得到 %+v", res)
	}

	bal, sum := balanceAndEntrySum(t, u)
	if bal != 100 || sum != 100 {
		t.Fatalf("餘額應為 100(balance=%d, sum=%d)", bal, sum)
	}

	var tz, source string
	var streak int32
	var amount int64
	err = pool.QueryRow(ctx,
		`SELECT timezone, source, streak, amount FROM platform.daily_claims WHERE user_id=$1`, u,
	).Scan(&tz, &source, &streak, &amount)
	if err != nil {
		t.Fatalf("讀 daily_claims: %v", err)
	}
	if tz != "Asia/Taipei" || source != "normal" || streak != 1 || amount != 100 {
		t.Fatalf("daily_claims 欄位錯誤: tz=%s source=%s streak=%d amount=%d", tz, source, streak, amount)
	}

	var cur, best int32
	err = pool.QueryRow(ctx,
		`SELECT current_streak, best_streak FROM platform.user_daily_state WHERE user_id=$1`, u,
	).Scan(&cur, &best)
	if err != nil {
		t.Fatalf("讀 user_daily_state: %v", err)
	}
	if cur != 1 || best != 1 {
		t.Fatalf("state 應為 current=1 best=1,得到 current=%d best=%d", cur, best)
	}

	var events int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='daily.claimed' AND (payload->>'user_id')::bigint = $1`, u).Scan(&events)
	if events != 1 {
		t.Fatalf("outbox 應有 1 筆 daily.claimed,得到 %d", events)
	}
	var entryReason string
	_ = pool.QueryRow(ctx, `SELECT reason FROM platform.token_entries WHERE user_id=$1`, u).Scan(&entryReason)
	if entryReason != "daily_claim" {
		t.Fatalf("分錄 reason 應為 daily_claim,得到 %s", entryReason)
	}
	assertConsistent(t, u)
}

// 防連點:同一使用者併發簽 10 次,恰好成功 1 次,錢只加一次(權威 = daily_claims 的 PK)
func TestConcurrentDuplicateClaims(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc.Claim(ctx, u) }(i)
	}
	wg.Wait()

	success, alreadyClaimed := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, daily.ErrAlreadyClaimed):
			alreadyClaimed++
		default:
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if success != 1 || alreadyClaimed != n-1 {
		t.Fatalf("應恰好 1 成功 %d 已簽到,得到 success=%d already=%d", n-1, success, alreadyClaimed)
	}

	bal, _ := balanceAndEntrySum(t, u)
	if bal != 100 {
		t.Fatalf("餘額應只加一次(100),得到 %d", bal)
	}
	var claims, entries, events int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.daily_claims WHERE user_id=$1`, u).Scan(&claims)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u).Scan(&entries)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='daily.claimed' AND (payload->>'user_id')::bigint = $1`, u).Scan(&events)
	if claims != 1 || entries != 1 || events != 1 {
		t.Fatalf("落地筆數應各為 1,得到 claims=%d entries=%d events=%d", claims, entries, events)
	}
	assertConsistent(t, u)
}

// 連續兩天:streak 遞增、金額按 step 上升
func TestStreakIncrementsNextDay(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	if _, err := svc.Claim(ctx, u); err != nil {
		t.Fatalf("第一天: %v", err)
	}
	backdate(t, u, 1, 24) // 把第一天的紀錄挪成「昨天」

	res, err := svc.Claim(ctx, u)
	if err != nil {
		t.Fatalf("第二天: %v", err)
	}
	if res.Streak != 2 || res.Amount != 110 {
		t.Fatalf("第二天應為 streak=2 amount=110,得到 %+v", res)
	}
	bal, _ := balanceAndEntrySum(t, u)
	if bal != 210 {
		t.Fatalf("兩天累計餘額應為 210,得到 %d", bal)
	}
	assertConsistent(t, u)
}

// 斷簽歸 1:上次簽到是 3 天前 → streak 重算為 1,best_streak 由 GREATEST 保住
func TestStreakResetsAfterGap(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	// 直接鋪狀態:曾連簽 5 天(最佳 9),最後一次是當地 3 天前
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.user_daily_state (user_id, current_streak, best_streak, last_claim_date)
		VALUES ($1, 5, 9, (now() AT TIME ZONE 'Asia/Taipei')::date - 3)`, u); err != nil {
		t.Fatalf("鋪狀態: %v", err)
	}

	res, err := svc.Claim(ctx, u)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.Streak != 1 || res.Amount != 100 {
		t.Fatalf("斷簽應歸 1(amount=100),得到 %+v", res)
	}
	var cur, best int32
	_ = pool.QueryRow(ctx,
		`SELECT current_streak, best_streak FROM platform.user_daily_state WHERE user_id=$1`, u,
	).Scan(&cur, &best)
	if cur != 1 || best != 9 {
		t.Fatalf("state 應為 current=1 best=9(GREATEST 保護),得到 current=%d best=%d", cur, best)
	}
	assertConsistent(t, u)
}

// 金額封頂:高 streak 時金額停在 daily_streak_cap
func TestStreakAmountCapped(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	// 曾連簽 20 天,最後一次是當地昨天 → 本次 streak=21,金額 min(100+10×20, 200)=200
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.user_daily_state (user_id, current_streak, best_streak, last_claim_date)
		VALUES ($1, 20, 20, (now() AT TIME ZONE 'Asia/Taipei')::date - 1)`, u); err != nil {
		t.Fatalf("鋪狀態: %v", err)
	}

	res, err := svc.Claim(ctx, u)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.Streak != 21 || res.Amount != 200 {
		t.Fatalf("應為 streak=21 amount=200(封頂),得到 %+v", res)
	}
	var best int32
	_ = pool.QueryRow(ctx, `SELECT best_streak FROM platform.user_daily_state WHERE user_id=$1`, u).Scan(&best)
	if best != 21 {
		t.Fatalf("best_streak 應更新為 21,得到 %d", best)
	}
	assertConsistent(t, u)
}

// 改時區 + 距上次簽到 < 20h → 拒絕;冷卻已滿(21h)→ 放行
func TestTimezoneChangeCooldown(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	if _, err := svc.Claim(ctx, u); err != nil {
		t.Fatalf("第一次簽到: %v", err)
	}
	backdate(t, u, 1, 5) // 上次簽到:當地昨天、5 小時前(隔日但未滿 20h)

	// 改時區(晚於上次 claimed_at)
	if _, err := pool.Exec(ctx,
		`UPDATE platform.users SET timezone='Asia/Tokyo', timezone_changed_at=now() WHERE id=$1`, u); err != nil {
		t.Fatalf("改時區: %v", err)
	}

	if _, err := svc.Claim(ctx, u); !errors.Is(err, daily.ErrTimezoneChangeCooldown) {
		t.Fatalf("改時區後 5h 內再簽應回 ErrTimezoneChangeCooldown,得到 %v", err)
	}
	// 被拒的嘗試不得留下任何痕跡
	bal, _ := balanceAndEntrySum(t, u)
	if bal != 100 {
		t.Fatalf("被拒後餘額應仍為 100,得到 %d", bal)
	}

	// 把上次 claimed_at 撥到 21 小時前 → 冷卻已滿,放行
	if _, err := pool.Exec(ctx,
		`UPDATE platform.daily_claims SET claimed_at = now() - interval '21 hours' WHERE user_id=$1`, u); err != nil {
		t.Fatalf("撥 claimed_at: %v", err)
	}
	res, err := svc.Claim(ctx, u)
	if err != nil {
		t.Fatalf("冷卻已滿仍被拒: %v", err)
	}
	if res.Streak != 2 {
		t.Fatalf("連續日簽到 streak 應為 2,得到 %d", res.Streak)
	}
	if bal, _ := balanceAndEntrySum(t, u); bal != 100+res.Amount {
		t.Fatalf("放行的簽到必須真的動錢,餘額應為 %d,得到 %d", 100+res.Amount, bal)
	}
	// 時區快照:新的一筆記的是改後的時區
	var tz string
	_ = pool.QueryRow(ctx, `SELECT timezone FROM platform.daily_claims
		WHERE user_id=$1 ORDER BY claimed_at DESC LIMIT 1`, u).Scan(&tz)
	if tz != "Asia/Tokyo" {
		t.Fatalf("新簽到的時區快照應為 Asia/Tokyo,得到 %s", tz)
	}
	assertConsistent(t, u)
}

// 沒改時區:同時區隔日簽到即使只隔幾分鐘也放行(20h 檢查只在改時區後生效)
func TestNextDayClaimWithoutTimezoneChangeNotBlocked(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	if _, err := svc.Claim(ctx, u); err != nil {
		t.Fatalf("第一天: %v", err)
	}
	// 上次簽到:當地昨天、僅 1 小時前(模擬 23:30 簽完、00:30 再簽)
	backdate(t, u, 1, 1)

	res, err := svc.Claim(ctx, u)
	if err != nil {
		t.Fatalf("沒改時區的隔日簽到不該被 20h 擋下: %v", err)
	}
	if res.Streak != 2 || res.Amount != 110 {
		t.Fatalf("應為 streak=2 amount=110,得到 %+v", res)
	}
	assertConsistent(t, u)
}

// 改時區時間早於上次簽到 → 冷卻不適用(只防「改了時區去多簽」,不罰改過時區的人一輩子)
func TestTimezoneChangedBeforeLastClaimNotBlocked(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	// 很久以前改過時區
	if _, err := pool.Exec(ctx,
		`UPDATE platform.users SET timezone_changed_at = now() - interval '48 hours' WHERE id=$1`, u); err != nil {
		t.Fatalf("鋪 timezone_changed_at: %v", err)
	}
	if _, err := svc.Claim(ctx, u); err != nil {
		t.Fatalf("第一天: %v", err)
	}
	backdate(t, u, 1, 2) // 上次簽到 2 小時前(< 20h),但改時區在它之前

	if _, err := svc.Claim(ctx, u); err != nil {
		t.Fatalf("改時區早於上次簽到,不該觸發冷卻: %v", err)
	}
	assertConsistent(t, u)
}

// tx 原子性:重複簽到被拒後,claims/entries/balance/outbox/state 全部不變
func TestAtomicityOnRejectedClaim(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	if _, err := svc.Claim(ctx, u); err != nil {
		t.Fatalf("首簽: %v", err)
	}
	if _, err := svc.Claim(ctx, u); !errors.Is(err, daily.ErrAlreadyClaimed) {
		t.Fatalf("同日再簽應回 ErrAlreadyClaimed,得到 %v", err)
	}

	var claims, entries, events int
	var cur int32
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.daily_claims WHERE user_id=$1`, u).Scan(&claims)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u).Scan(&entries)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='daily.claimed' AND (payload->>'user_id')::bigint = $1`, u).Scan(&events)
	_ = pool.QueryRow(ctx, `SELECT current_streak FROM platform.user_daily_state WHERE user_id=$1`, u).Scan(&cur)
	if claims != 1 || entries != 1 || events != 1 || cur != 1 {
		t.Fatalf("被拒的 tx 不得留下任何寫入: claims=%d entries=%d events=%d streak=%d",
			claims, entries, events, cur)
	}
	bal, sum := balanceAndEntrySum(t, u)
	if bal != 100 || sum != 100 {
		t.Fatalf("餘額應維持 100(balance=%d, SUM=%d)", bal, sum)
	}
}

// localToday 用 DB 算使用者時區的當地今天(與服務同一種算法的獨立來源)。
func localToday(t *testing.T, tz string) string {
	t.Helper()
	var d string
	if err := pool.QueryRow(context.Background(),
		`SELECT to_char((now() AT TIME ZONE $1)::date, 'YYYY-MM-DD')`, tz).Scan(&d); err != nil {
		t.Fatalf("算當地今天: %v", err)
	}
	return d
}

// 兩個防重權威矛盾:帳本已有今天的冪等鍵+結果、daily_claims 卻無對應列(異常狀態)
// → Claim 必須失敗出聲且零寫入,不得回成功但錢沒動。
func TestLedgerStateConflictFailsLoud(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	key := fmt.Sprintf("daily:%d:%s", u, localToday(t, "Asia/Taipei"))
	// request_hash 與服務端一致(= key 本身),讓 ApplyInTx 走到 replay 而非 hash 衝突
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.idempotency_keys (key, request_hash, response)
		VALUES ($1, $1, '{"entries":[]}')`, key); err != nil {
		t.Fatalf("鋪異常冪等鍵: %v", err)
	}

	if _, err := svc.Claim(ctx, u); !errors.Is(err, daily.ErrLedgerStateConflict) {
		t.Fatalf("應回 ErrLedgerStateConflict,得到 %v", err)
	}

	var claims, entries, events, states int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.daily_claims WHERE user_id=$1`, u).Scan(&claims)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u).Scan(&entries)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
		WHERE topic='daily.claimed' AND (payload->>'user_id')::bigint = $1`, u).Scan(&events)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.user_daily_state WHERE user_id=$1`, u).Scan(&states)
	if claims != 0 || entries != 0 || events != 0 || states != 0 {
		t.Fatalf("矛盾狀態的 tx 必須零寫入: claims=%d entries=%d events=%d states=%d",
			claims, entries, events, states)
	}
	if bal, _ := balanceAndEntrySum(t, u); bal != 0 {
		t.Fatalf("餘額應為 0,得到 %d", bal)
	}
}

// users 列鎖串行化同一使用者的併發 Claim:兩併發一成功一 ErrAlreadyClaimed,
// 且在時限內完成(證明鎖存在也不死鎖)。
func TestConcurrentSameUserSerializedNoDeadlock(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u := newUser(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = svc.Claim(ctx, u) }(i)
	}
	wg.Wait()

	success, already := 0, 0
	for i, err := range errs {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("goroutine %d 逾時——使用者鎖死鎖", i)
		case err == nil:
			success++
		case errors.Is(err, daily.ErrAlreadyClaimed):
			already++
		default:
			t.Fatalf("goroutine %d 非預期錯誤: %v", i, err)
		}
	}
	if success != 1 || already != 1 {
		t.Fatalf("應恰好 1 成功 1 已簽到,得到 success=%d already=%d", success, already)
	}
	if bal, _ := balanceAndEntrySum(t, u); bal != 100 {
		t.Fatalf("錢應只動一次(100),得到 %d", bal)
	}
	assertConsistent(t, u)
}

// 壞 config(負值)必須拒絕出聲、零寫入,不能默默反向扣錢
func TestNegativeConfigRejected(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)

	// 插一筆較新的負值 daily_base;測試結束還原,不影響其他測試
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.economy_configs (key, value, note)
		VALUES ('daily_base', '-100', 'test: negative value')`); err != nil {
		t.Fatalf("鋪負值設定: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM platform.economy_configs WHERE key='daily_base' AND note='test: negative value'`)
	})

	if _, err := svc.Claim(ctx, u); !errors.Is(err, daily.ErrInvalidConfig) {
		t.Fatalf("負值設定應回 ErrInvalidConfig,得到 %v", err)
	}

	var claims, entries int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.daily_claims WHERE user_id=$1`, u).Scan(&claims)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform.token_entries WHERE user_id=$1`, u).Scan(&entries)
	if claims != 0 || entries != 0 {
		t.Fatalf("被拒的 tx 必須零寫入: claims=%d entries=%d", claims, entries)
	}
	if bal, _ := balanceAndEntrySum(t, u); bal != 0 {
		t.Fatalf("餘額應為 0,得到 %d", bal)
	}
}
