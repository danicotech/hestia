package transport

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/eventlog"
)

// blockingSink 把寫入卡住,模擬「稽核 DB 不可達 / 極慢」。
type blockingSink struct {
	release chan struct{}
	mu      sync.Mutex
	written int
}

func (s *blockingSink) LogEvent(context.Context, eventlog.Record) error {
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written++
	return nil
}

func (s *blockingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 佇列滿了要**丟**,不能阻塞:submit 在請求路徑上,
// 任何等待都會變成「DB 慢 = 全站慢」。
func TestAuditQueue_FullQueueDropsInsteadOfBlocking(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	q := newAuditQueue(sink, quietLogger())

	start := time.Now()
	total := auditQueueSize + 200
	for i := 0; i < total; i++ {
		q.submit(eventlog.Record{Action: "/x/Y"})
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("submit 阻塞了:%v", elapsed)
	}
	if q.droppedCount() == 0 {
		t.Fatal("超出容量就該有丟棄計數")
	}
	// 丟掉的必須是「超出容量」的那些,不能整批都丟。
	if q.droppedCount() >= int64(total) {
		t.Fatalf("丟太多了:%d / %d", q.droppedCount(), total)
	}

	close(sink.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// 關機要盡力排空:非同步不是「可以默默丟掉」的藉口。
func TestAuditQueue_ShutdownDrains(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	close(sink.release) // 不阻塞
	q := newAuditQueue(sink, quietLogger())
	for i := 0; i < 50; i++ {
		q.submit(eventlog.Record{Action: "/x/Y"})
	}
	if err := q.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := sink.count(); got != 50 {
		t.Fatalf("關機應排空 50 列,實際寫了 %d 列", got)
	}
	// 關機後再投遞不會 panic(channel 刻意不 close),也不會再寫入。
	q.submit(eventlog.Record{Action: "/x/Z"})
	if got := sink.count(); got != 50 {
		t.Fatalf("關機後不該再寫入,got %d", got)
	}
	// 重複關機是安全的。
	if err := q.shutdown(context.Background()); err != nil {
		t.Fatalf("重複 shutdown: %v", err)
	}
}

// 排空超時就放手,而且 worker 一定會結束(不洩漏 goroutine)。
func TestAuditQueue_ShutdownTimesOutWithoutLeaking(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	q := newAuditQueue(sink, quietLogger())
	for i := 0; i < 10; i++ {
		q.submit(eventlog.Record{Action: "/x/Y"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		close(sink.release) // 讓卡住的那一列寫完,worker 才走得掉
		done <- q.shutdown(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown 沒有在合理時間內返回")
	}
	select {
	case <-q.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker goroutine 沒有結束")
	}
}

// 併發投遞不能有 race,也不能漏(容量內的都要寫進去)。
func TestAuditQueue_ConcurrentSubmit(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	close(sink.release)
	q := newAuditQueue(sink, quietLogger())

	var wg sync.WaitGroup
	const workers, each = 8, 25
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				q.submit(eventlog.Record{Action: "/x/Y"})
			}
		}()
	}
	wg.Wait()
	if err := q.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := int64(sink.count()) + q.droppedCount(); got != workers*each {
		t.Fatalf("寫入 %d + 丟棄 %d != 投遞 %d", sink.count(), q.droppedCount(), workers*each)
	}
}

// nil 佇列(沒注入 EventLog)不該 panic。
func TestAuditQueue_NilIsSafe(t *testing.T) {
	var q *auditQueue
	q.submit(eventlog.Record{})
	if err := q.shutdown(context.Background()); err != nil {
		t.Fatalf("nil shutdown: %v", err)
	}
	if q.droppedCount() != 0 {
		t.Fatal("nil 佇列不該有計數")
	}
}
