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
