package xppg_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/xp"
)

// AwardInTx 的整合測試:同 tx 綁定的變體(給 activitylogpg 那類
// 「事實 + 彙總 + XP 必須同生共死」的呼叫端用)。
//
// 要證明四件事:
//   - 原子性由呼叫端決定:rollback → xp_events 與 user_xp 都不留;
//   - 冷卻 / daily_cap / source 檢查與 Award 完全一致;
//   - 失敗被 savepoint 隔離,呼叫端的 tx 還能用;
//   - 併發下投影一致性(user_xp.xp == SUM(xp_events.amount))不變。

// begin 開一個呼叫端 tx,測試結束保證收尾。
func begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("開 transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func awardInTx(t *testing.T, tx pgx.Tx, userID, communityID, amount int64, source string) *xp.AwardResult {
	t.Helper()
	res, err := svc.AwardInTx(context.Background(), tx, xp.AwardParams{
		UserID: userID, CommunityID: communityID, Source: source, Amount: amount,
	})
	if err != nil {
		t.Fatalf("AwardInTx: %v", err)
	}
	return res
}

// hasProjection 回報 user_xp 有沒有這一列(rollback 之後連空列都不該留)。
func hasProjection(t *testing.T, userID, communityID int64) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM platform.user_xp WHERE user_id=$1 AND community_id=$2`,
		userID, communityID).Scan(&n); err != nil {
		t.Fatalf("count(user_xp): %v", err)
	}
	return n > 0
}

// 呼叫端 rollback → 事實與投影都不留。這正是 AwardInTx 存在的理由:
// 沒有它,XP 只能在呼叫端 commit 之後另開 tx 發,中間掛掉就對不起來。
func TestAwardInTxRollbackLeavesNothing(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")
	ctx := context.Background()

	tx := begin(t)
	if res := awardInTx(t, tx, u, c, 15, "discord_message"); res.Awarded != 15 || res.XP != 15 {
		t.Fatalf("入帳結果不符: %+v", res)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if n := countEvents(t, u, c); n != 0 {
		t.Fatalf("rollback 後 xp_events 還有 %d 列", n)
	}
	if hasProjection(t, u, c) {
		t.Fatal("rollback 後 user_xp 還留著投影列")
	}
}

// 呼叫端 commit → 事實與投影一起落地,結果與 Award 相同。
// 另外驗「呼叫端自己的寫入」與 XP 真的在同一個 tx 裡:
// 一起 commit 才會兩者都在。
func TestAwardInTxCommitLandsWithCallerWork(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")
	ctx := context.Background()

	tx := begin(t)
	// 呼叫端自己的「事實」:借 activity_daily 當代表(activitylogpg 的真實用法)。
	if _, err := tx.Exec(ctx,
		`INSERT INTO platform.activity_daily (user_id, community_id, date, messages)
		 VALUES ($1, $2, current_date, 1)`, u, c); err != nil {
		t.Fatalf("寫呼叫端的事實: %v", err)
	}
	if res := awardInTx(t, tx, u, c, 20, "discord_message"); res.Awarded != 20 {
		t.Fatalf("入帳結果不符: %+v", res)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if sum := sumEvents(t, u, c); sum != 20 {
		t.Fatalf("SUM(xp_events) = %d,期望 20", sum)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 20 {
		t.Fatalf("user_xp.xp = %d,期望 20", xpVal)
	}
	var messages int32
	if err := pool.QueryRow(ctx,
		`SELECT messages FROM platform.activity_daily
		 WHERE user_id=$1 AND community_id=$2 AND date=current_date`, u, c).Scan(&messages); err != nil {
		t.Fatalf("讀呼叫端的事實: %v", err)
	}
	if messages != 1 {
		t.Fatalf("呼叫端的事實沒跟著落地(messages=%d)", messages)
	}
}

// 冷卻語意與 Award 一致:期內被攔、不入帳、不留任何東西(連空投影列都不留),
// 而且被攔之後呼叫端的 tx 仍然可用(攔下是正常結果,不是錯誤)。
func TestAwardInTxCooldownMatchesAward(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"cooldown_seconds":60}}}`)
	ctx := context.Background()

	// 第一筆走 Award(另一條路徑)落地,證明兩條路徑共用同一個計時器。
	if res := award(t, u, c, 10, "discord_message"); res.Awarded != 10 {
		t.Fatalf("第一筆應入帳: %+v", res)
	}

	tx := begin(t)
	res := awardInTx(t, tx, u, c, 10, "discord_message")
	if !res.OnCooldown || res.Awarded != 0 {
		t.Fatalf("冷卻期內應被攔: %+v", res)
	}
	if res.XP != 10 {
		t.Fatalf("被攔時應回當時的 xp,得到 %d", res.XP)
	}
	// 被攔之後 tx 還活著:呼叫端可以照樣 commit 自己的事實。
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("冷卻攔下之後呼叫端的 tx 壞了: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n := countEvents(t, u, c); n != 1 {
		t.Fatalf("被攔的嘗試不該留下事件,xp_events 有 %d 列", n)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 10 {
		t.Fatalf("user_xp.xp = %d,期望 10", xpVal)
	}
}

// daily_cap 語意與 Award 一致:裁到剩餘額度部分入帳,滿了整筆攔下。
func TestAwardInTxDailyCapMatchesAward(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"daily_cap":100}}}`)
	ctx := context.Background()

	tx := begin(t)
	if res := awardInTx(t, tx, u, c, 60, "discord_message"); res.Awarded != 60 || res.Capped {
		t.Fatalf("cap 內應全額入帳: %+v", res)
	}
	// 同一個 tx 內再送:看得到自己剛寫的事件,所以裁到 40。
	if res := awardInTx(t, tx, u, c, 60, "discord_message"); res.Awarded != 40 || !res.Capped {
		t.Fatalf("應裁到 40 並標記 Capped: %+v", res)
	}
	if res := awardInTx(t, tx, u, c, 10, "discord_message"); res.Awarded != 0 || !res.Capped {
		t.Fatalf("cap 滿後應整筆攔下: %+v", res)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if sum := sumEvents(t, u, c); sum != 100 {
		t.Fatalf("入帳總量應恰為 cap=100,得到 %d", sum)
	}
	if n := countEvents(t, u, c); n != 2 {
		t.Fatalf("xp_events 應有 2 列(攔下的不落地),得到 %d", n)
	}
}

// source 檢查與參數驗證和 Award 一致,而且**不毒化呼叫端的 tx**:
// 這是 savepoint 隔離的重點 —— 呼叫端有權決定「XP 發不成但事實照記」。
func TestAwardInTxErrorsDoNotPoisonCallerTx(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")
	ctx := context.Background()

	tx := begin(t)
	_, err := svc.AwardInTx(ctx, tx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "no_such_source", Amount: 10,
	})
	if !errors.Is(err, xp.ErrUnknownSource) {
		t.Fatalf("不存在的 source 應回 ErrUnknownSource,得到 %v", err)
	}

	// 真正會產生 SQL 錯誤的一種:user 不存在 → user_xp 的 FK 違反。
	// 沒有 savepoint 的話這裡就會把呼叫端的 tx 弄成 aborted。
	_, err = svc.AwardInTx(ctx, tx, xp.AwardParams{
		UserID: 999999999, CommunityID: c, Source: "discord_message", Amount: 10,
	})
	if err == nil {
		t.Fatal("不存在的 user 竟然入帳成功")
	}

	// tx 仍然可用:呼叫端照樣寫得進自己的事實並 commit。
	if _, err := tx.Exec(ctx,
		`INSERT INTO platform.activity_daily (user_id, community_id, date, messages)
		 VALUES ($1, $2, current_date, 1)`, u, c); err != nil {
		t.Fatalf("XP 失敗後呼叫端的 tx 壞了: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := countEvents(t, u, c); n != 0 {
		t.Fatalf("失敗的入帳留下了 %d 列 xp_events", n)
	}
}

// 併發下投影一致性:每個 goroutine 各自持有一個 tx 呼叫 AwardInTx,
// user_xp 的列鎖照樣串行化 —— 這是同 tx 變體最容易被寫壞的性質。
func TestAwardInTxConcurrentProjectionConsistency(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")
	ctx := context.Background()

	// 每個 goroutine 佔一條連線(自己的 tx),不能超過連線池上限,
	// 否則測的會是「等連線」而不是「等列鎖」。
	n := int(pool.Config().MaxConns) - 1
	if n > 10 {
		n = 10
	}
	if n < 2 {
		t.Skipf("連線池只有 %d 條連線,測不出併發", pool.Config().MaxConns)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := svc.AwardInTx(ctx, tx, xp.AwardParams{
				UserID: u, CommunityID: c, Source: "discord_message", Amount: 7,
			}); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	sum := sumEvents(t, u, c)
	xpVal, _ := projectedXP(t, u, c)
	if sum != int64(7*n) {
		t.Fatalf("SUM(xp_events) 應為 %d,得到 %d", 7*n, sum)
	}
	if xpVal != sum {
		t.Fatalf("投影不一致:user_xp.xp=%d,SUM(events)=%d", xpVal, sum)
	}
}

// 併發 + 冷卻:各自 tx 也只有一筆能入帳(DB 單一時鐘 + 列鎖,與 Award 同性質)。
func TestAwardInTxConcurrentCooldown(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"cooldown_seconds":60}}}`)
	ctx := context.Background()

	n := int(pool.Config().MaxConns) - 1
	if n > 8 {
		n = 8
	}
	if n < 2 {
		t.Skipf("連線池只有 %d 條連線,測不出併發", pool.Config().MaxConns)
	}

	var wg sync.WaitGroup
	results := make([]*xp.AwardResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			results[i], errs[i] = svc.AwardInTx(ctx, tx, xp.AwardParams{
				UserID: u, CommunityID: c, Source: "discord_message", Amount: 10,
			})
			if errs[i] == nil {
				errs[i] = tx.Commit(ctx)
			}
		}(i)
	}
	wg.Wait()

	landed := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i].Awarded > 0 {
			landed++
		} else if !results[i].OnCooldown {
			t.Fatalf("goroutine %d 未入帳也未標記冷卻: %+v", i, results[i])
		}
	}
	if landed != 1 {
		t.Fatalf("併發下應恰好 1 筆入帳,得到 %d", landed)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 10 {
		t.Fatalf("user_xp.xp = %d,期望 10", xpVal)
	}
}
