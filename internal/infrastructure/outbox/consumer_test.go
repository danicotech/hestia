package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/infrastructure/outbox"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 對真 Postgres 跑:要驗的正是 SKIP LOCKED 的鎖行為,mock 無意義。
// 各測試用獨立 topic 隔離;結束時自己的事件都收斂到 done/failed/未到期,
// 不留「立即可搶」的列去干擾下一個測試。

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
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

func insertEvent(t *testing.T, topic string, payload string) int64 {
	t.Helper()
	id, err := db.New(pool).InsertOutboxEvent(context.Background(), db.InsertOutboxEventParams{
		Topic: topic, Payload: []byte(payload),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

func eventState(t *testing.T, id int64) (status string, attempts int32, nextRetry *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT status, attempts, next_retry_at FROM platform.outbox_events WHERE id = $1`, id).
		Scan(&status, &attempts, &nextRetry)
	if err != nil {
		t.Fatalf("讀事件狀態: %v", err)
	}
	return
}

// 成功路徑:handler 收到 payload,事件標 done
func TestDispatchDone(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := outbox.NewConsumer(pool)
	var got []string
	c.Handle("t1.ok", func(_ context.Context, _ string, payload []byte) error {
		got = append(got, string(payload))
		return nil
	})
	a := insertEvent(t, "t1.ok", `{"n":1}`)
	b := insertEvent(t, "t1.ok", `{"n":2}`)

	n, err := c.ProcessOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("ProcessOnce = (%d, %v),要 (2, nil)", n, err)
	}
	for _, id := range []int64{a, b} {
		if s, _, _ := eventState(t, id); s != "done" {
			t.Fatalf("事件 %d 狀態 %q,要 done", id, s)
		}
	}
	// JSONB 會正規化空白,解析後比對
	var first struct{ N int }
	if len(got) != 2 || json.Unmarshal([]byte(got[0]), &first) != nil || first.N != 1 {
		t.Fatalf("handler 收到 %v", got)
	}
}

// 失敗 → attempts+1、next_retry 在未來;未到期不會被再搶
func TestRetryBackoff(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := outbox.NewConsumer(pool)
	c.Handle("t2.retry", func(context.Context, string, []byte) error {
		return errors.New("下游掛了")
	})
	id := insertEvent(t, "t2.retry", `{}`)

	if n, err := c.ProcessOnce(ctx); err != nil || n != 1 {
		t.Fatalf("第一輪 = (%d, %v)", n, err)
	}
	s, attempts, retry := eventState(t, id)
	if s != "pending" || attempts != 1 || retry == nil || !retry.After(time.Now()) {
		t.Fatalf("要 pending/1/未來重試,得到 %s/%d/%v", s, attempts, retry)
	}
	// 未到期:立即再跑不該搶到
	if n, err := c.ProcessOnce(ctx); err != nil || n != 0 {
		t.Fatalf("未到期不應再處理,得到 (%d, %v)", n, err)
	}
}

// 毒訊息:達 MaxAttempts 標 failed,不再佔佇列
func TestPoisonMarkedFailed(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := outbox.NewConsumer(pool)
	c.MaxAttempts = 2
	calls := 0
	c.Handle("t3.poison", func(context.Context, string, []byte) error {
		calls++
		return errors.New("永遠失敗")
	})
	id := insertEvent(t, "t3.poison", `{}`)

	if _, err := c.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 把重試時間撥回過去,模擬到期
	if _, err := pool.Exec(ctx,
		`UPDATE platform.outbox_events SET next_retry_at = now() - interval '1s' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s, attempts, _ := eventState(t, id); s != "failed" || attempts != 2 {
		t.Fatalf("要 failed/2,得到 %s/%d", s, attempts)
	}
	if calls != 2 {
		t.Fatalf("handler 應被呼叫 2 次,實際 %d", calls)
	}
	// 終態不再被搶
	if n, err := c.ProcessOnce(ctx); err != nil || n != 0 {
		t.Fatalf("failed 不應再處理,得到 (%d, %v)", n, err)
	}
}

// handler panic 收斂成一般失敗,不炸掉消費者、不拖累同批其他事件
func TestPanicRecovered(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c := outbox.NewConsumer(pool)
	c.Handle("t4.panic", func(context.Context, string, []byte) error { panic("boom") })
	var okHit bool
	c.Handle("t4.ok", func(context.Context, string, []byte) error { okHit = true; return nil })
	bad := insertEvent(t, "t4.panic", `{}`)
	good := insertEvent(t, "t4.ok", `{}`)

	if n, err := c.ProcessOnce(ctx); err != nil || n != 2 {
		t.Fatalf("ProcessOnce = (%d, %v)", n, err)
	}
	if s, _, _ := eventState(t, bad); s != "pending" {
		t.Fatalf("panic 事件要進重試(pending),得到 %s", s)
	}
	if s, _, _ := eventState(t, good); s != "done" || !okHit {
		t.Fatalf("同批正常事件要 done,得到 %s(handler hit=%v)", s, okHit)
	}
}

// 未註冊 topic:走一般失敗路徑(handler 可能晚於事件部署,不能直接丟)
func TestUnknownTopicRetries(t *testing.T) {
	setup(t)
	c := outbox.NewConsumer(pool)
	id := insertEvent(t, "t5.nobody", `{}`)
	if _, err := c.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, attempts, retry := eventState(t, id); s != "pending" || attempts != 1 || retry == nil {
		t.Fatalf("要 pending/1/有重試時間,得到 %s/%d/%v", s, attempts, retry)
	}
}

// SKIP LOCKED:兩個並發消費者,每筆事件恰好處理一次
func TestConcurrentConsumersNoDoubleProcessing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	const total = 30

	var mu sync.Mutex
	seen := map[string]int{}
	handler := func(_ context.Context, _ string, payload []byte) error {
		mu.Lock()
		seen[string(payload)]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // 拉長持鎖窗,逼出重疊
		return nil
	}
	for i := 0; i < total; i++ {
		insertEvent(t, "t6.race", fmt.Sprintf(`{"i":%d}`, i))
	}

	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		c := outbox.NewConsumer(pool)
		c.BatchSize = 5
		c.Handle("t6.race", handler)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := c.ProcessOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("處理到 %d 種事件,要 %d", len(seen), total)
	}
	for p, c := range seen {
		if c != 1 {
			t.Fatalf("事件 %s 被處理 %d 次(要恰好 1 次)", p, c)
		}
	}
}

// Run:ctx 取消要乾淨退出(QA 補的覆蓋——條件 6)
func TestRunExitsOnCancel(t *testing.T) {
	setup(t)
	c := outbox.NewConsumer(pool)
	c.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run 回傳 %v,要 context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 沒有在時限內退出")
	}
}

// Run:積壓時不等 tick 連續清空(QA 補的覆蓋——條件 7)
func TestRunDrainsBacklogBeforeTick(t *testing.T) {
	setup(t)
	c := outbox.NewConsumer(pool)
	c.BatchSize = 5
	c.PollInterval = time.Minute // 故意設很長:清空必須不靠 tick
	var mu sync.Mutex
	handled := 0
	c.Handle("t7.drain", func(context.Context, string, []byte) error {
		mu.Lock()
		handled++
		mu.Unlock()
		return nil
	})
	const total = 15
	for i := 0; i < total; i++ {
		insertEvent(t, "t7.drain", fmt.Sprintf(`{"i":%d}`, i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := handled
		mu.Unlock()
		if n == total {
			return // 遠短於一個 tick(1 分鐘)即清空
		}
		if time.Now().After(deadline) {
			t.Fatalf("5 秒內只處理 %d/%d(tick 是 1 分鐘,代表沒有連續抓)", n, total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 掛住的 handler:逾時視同失敗走退避,不佔死持鎖 tx(QA 發現的風險 2)
func TestHandlerTimeout(t *testing.T) {
	setup(t)
	c := outbox.NewConsumer(pool)
	c.HandlerTimeout = 50 * time.Millisecond
	c.Handle("t8.stuck", func(ctx context.Context, _ string, _ []byte) error {
		<-ctx.Done() // 模擬尊重 ctx 的慢 handler
		return ctx.Err()
	})
	id := insertEvent(t, "t8.stuck", `{}`)
	start := time.Now()
	if n, err := c.ProcessOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("ProcessOnce = (%d, %v)", n, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("逾時防護沒生效,耗時 %v", elapsed)
	}
	if s, attempts, _ := eventState(t, id); s != "pending" || attempts != 1 {
		t.Fatalf("要 pending/1(走退避),得到 %s/%d", s, attempts)
	}
}

// 排除清單:由別人負責投遞的 topic 不得被這個消費者認領
// (否則找不到 handler → 退避重試 → 標 failed,公告會被燒掉)
func TestExcludedTopicsAreNotClaimed(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mine := insertEvent(t, "t9.mine", `{}`)
	theirs := insertEvent(t, "t9.theirs", `{}`)

	c := outbox.NewConsumer(pool)
	c.ExcludedTopics = []string{"t9.theirs"}
	c.Handle("t9.mine", func(context.Context, string, []byte) error { return nil })

	if n, err := c.ProcessOnce(ctx); err != nil || n != 1 {
		t.Fatalf("只該處理自己的那筆,得到 (%d, %v)", n, err)
	}
	if s, _, _ := eventState(t, mine); s != "done" {
		t.Fatalf("自己的事件應 done,得到 %s", s)
	}
	// 被排除的事件必須「原封不動」——不是 pending 但 attempts 被加過
	s, attempts, retry := eventState(t, theirs)
	if s != "pending" || attempts != 0 || retry != nil {
		t.Fatalf("被排除的事件應原封不動,得到 %s/%d/%v", s, attempts, retry)
	}
	// 空清單(nil)必須回到「全部都歸我」——SQL 的 NULL 陷阱迴歸測試
	c2 := outbox.NewConsumer(pool)
	c2.Handle("t9.theirs", func(context.Context, string, []byte) error { return nil })
	if n, err := c2.ProcessOnce(ctx); err != nil || n != 1 {
		t.Fatalf("nil 排除清單應照常認領,得到 (%d, %v)", n, err)
	}
}

// 閘道負責的 topic 不得被註冊 in-process handler——重複投遞不會報錯,
// 只會讓使用者看到同一則公告兩次,所以必須在啟動組裝期就擋下來。
func TestHandleRejectsGatewayTopic(t *testing.T) {
	c := outbox.NewConsumer(nil)
	defer func() {
		if recover() == nil {
			t.Fatal("註冊閘道 topic 應 panic,結果沒有")
		}
	}()
	c.Handle(notification.DiscordTopics()[0], func(context.Context, string, []byte) error { return nil })
}

// 一般 topic 照常註冊,且 RegisteredTopics 如實回報(供啟動斷言用)
func TestRegisteredTopics(t *testing.T) {
	c := outbox.NewConsumer(nil)
	c.Handle("t10.mine", func(context.Context, string, []byte) error { return nil })
	got := c.RegisteredTopics()
	if len(got) != 1 || got[0] != "t10.mine" {
		t.Fatalf("RegisteredTopics = %v", got)
	}
	if err := notification.AssertNoOverlap(got); err != nil {
		t.Fatalf("一般 topic 不該衝突: %v", err)
	}
}
