package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/infrastructure/maintenance"
)

func newTestRunner() *maintenance.Runner {
	r := maintenance.New(pool)
	r.PollInterval = 20 * time.Millisecond
	return r
}

// TestRunnerWritesEventLog:成功與失敗的執行都要在 event_logs 留下 job.run 紀錄,
// response 帶 processed,失敗還要帶 error。
func TestRunnerWritesEventLog(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	okJob := fmt.Sprintf("test_ok_%d", time.Now().UnixNano())
	errJob := fmt.Sprintf("test_err_%d", time.Now().UnixNano())
	r := newTestRunner()
	r.Register(okJob, time.Hour, func(ctx context.Context) (int, error) { return 7, nil })
	r.Register(errJob, time.Hour, func(ctx context.Context) (int, error) {
		return 3, errors.New("boom-error")
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool {
		return countEventLogs(t, okJob, "") >= 1 && countEventLogs(t, errJob, "") >= 1
	}, "兩個 job 的 event_logs 紀錄")
	cancel()
	<-done

	var status string
	var latencyMs *int32
	var processed int
	err := pool.QueryRow(ctx0(),
		`SELECT status, latency_ms, (response->>'processed')::int
		 FROM platform.event_logs
		 WHERE channel = 'system' AND kind = 'job.run' AND action = $1`, okJob,
	).Scan(&status, &latencyMs, &processed)
	if err != nil {
		t.Fatalf("查 ok job 紀錄: %v", err)
	}
	if status != "ok" || processed != 7 || latencyMs == nil {
		t.Errorf("ok job 紀錄不對:status=%q processed=%d latency_ms=%v", status, processed, latencyMs)
	}

	var errMsg string
	err = pool.QueryRow(ctx0(),
		`SELECT status, (response->>'processed')::int, response->>'error'
		 FROM platform.event_logs
		 WHERE channel = 'system' AND kind = 'job.run' AND action = $1`, errJob,
	).Scan(&status, &processed, &errMsg)
	if err != nil {
		t.Fatalf("查 error job 紀錄: %v", err)
	}
	if status != "error" || errMsg != "boom-error" {
		t.Errorf("error job 紀錄不對:status=%q error=%q", status, errMsg)
	}
}

// TestRunnerConcurrentSameJob:兩個 runner(模擬多實例)同時跑同名 job,
// advisory lock 保證同一時刻恰一個執行;搶輸的一方本輪跳過、不報錯、不留紀錄。
// 搶輸方會在下一個 tick 重試(見 TestRunnerRetriesAfterLockBusy),因此這裡把
// PollInterval 拉大,讓觀察窗落在單一輪內,互斥的斷言才乾淨。
func TestRunnerConcurrentSameJob(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	name := fmt.Sprintf("test_concurrent_%d", time.Now().UnixNano())
	var calls atomic.Int32
	jobFn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		time.Sleep(700 * time.Millisecond) // 撐住鎖,讓另一個 runner 的嘗試落在執行期間
		return 1, nil
	}
	r1, r2 := newTestRunner(), newTestRunner()
	r1.PollInterval, r2.PollInterval = 30*time.Second, 30*time.Second
	r1.Register(name, time.Hour, jobFn)
	r2.Register(name, time.Hour, jobFn)

	var wg sync.WaitGroup
	for _, r := range []*maintenance.Runner{r1, r2} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Run(ctx) }()
	}
	waitFor(t, 5*time.Second, func() bool {
		return countEventLogs(t, name, "") >= 1
	}, "併發 job 的 event_logs 紀錄")
	time.Sleep(200 * time.Millisecond) // 若第二個也執行了,給它寫紀錄的時間
	cancel()
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("同名 job 併發應恰執行一次,實際 %d 次", got)
	}
	if got := countEventLogs(t, name, ""); got != 1 {
		t.Errorf("應恰有 1 列 job.run 紀錄(搶輸的跳過不記),實際 %d", got)
	}
}

// TestRunnerRetriesAfterLockBusy:搶輸 advisory lock 不推進排程——鎖被別的
// 「實例」(這裡用手動持鎖的連線模擬)佔住時 runner 每個 tick 重試,
// 鎖一釋放就在下一個 tick 內執行,不用等完整的 every 週期。
func TestRunnerRetriesAfterLockBusy(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	name := fmt.Sprintf("test_lockbusy_%d", time.Now().UnixNano())

	// 外部連線先佔住同名鎖(模擬另一個實例執行中)
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取鎖連線: %v", err)
	}
	defer lockConn.Release()
	var got bool
	if err := lockConn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext('maintenance'), hashtext($1))`, name).Scan(&got); err != nil || !got {
		t.Fatalf("外部持鎖失敗: got=%v err=%v", got, err)
	}

	var calls atomic.Int32
	r := newTestRunner()
	r.Register(name, time.Hour, func(ctx context.Context) (int, error) {
		calls.Add(1)
		return 1, nil
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// 鎖被佔住期間:job 不得執行(每個 tick 都在重試但都搶輸)
	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("鎖被佔住時 job 不得執行,實際執行了 %d 次", got)
	}

	// 釋放鎖 → runner 應在幾個 tick 內執行(every=1h,若排程被錯誤推進就等不到)
	if _, err := lockConn.Exec(ctx,
		`SELECT pg_advisory_unlock(hashtext('maintenance'), hashtext($1))`, name); err != nil {
		t.Fatalf("釋放外部鎖: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return calls.Load() >= 1 },
		"鎖釋放後 job 在下一個 tick 執行")
	cancel()
	<-done
}

// TestRunnerPanicRecovers:job panic 收斂成 error 記進 event_logs,
// runner 存活並在下次到期正常重跑(也證明 panic 那次的鎖有釋放)。
func TestRunnerPanicRecovers(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	name := fmt.Sprintf("test_panic_%d", time.Now().UnixNano())
	var calls atomic.Int32
	r := newTestRunner()
	r.Register(name, 50*time.Millisecond, func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			panic("boom-panic")
		}
		return 1, nil
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool {
		return countEventLogs(t, name, "error") >= 1 && countEventLogs(t, name, "ok") >= 1
	}, "panic 的 error 紀錄與後續的 ok 紀錄")
	cancel()
	<-done

	var errMsg string
	err := pool.QueryRow(ctx0(),
		`SELECT response->>'error' FROM platform.event_logs
		 WHERE channel = 'system' AND kind = 'job.run' AND action = $1 AND status = 'error'
		 LIMIT 1`, name).Scan(&errMsg)
	if err != nil {
		t.Fatalf("查 panic 紀錄: %v", err)
	}
	if errMsg == "" || !strings.Contains(errMsg, "boom-panic") {
		t.Errorf("panic 訊息應收斂進 error 欄位,實際 %q", errMsg)
	}
}

// TestRunnerContextCancel:ctx 取消時 Run 乾淨退出。
func TestRunnerContextCancel(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := newTestRunner()
	r.Register("test_cancel_noop", time.Hour, func(ctx context.Context) (int, error) { return 0, nil })

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run 應回傳 context.Canceled,實際 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 沒有在 ctx 取消後退出")
	}
}

func ctx0() context.Context { return context.Background() }
