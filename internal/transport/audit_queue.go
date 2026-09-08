package transport

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/eventlog"
)

// auditQueue 把 event_logs 的寫入移出請求路徑。
//
// 為什麼不能同步寫:稽核是旁路,但同步寫會讓「DB 不可達」從「少一列紀錄」
// 升級成「每個請求都多等一個 timeout」——正式環境 DB 一掛,全部請求 +3 秒,
// 對上游等同服務中斷。「不失敗」不夠,還要「不拖垮」。
//
// 取捨(明講):佇列滿了就**丟**,不阻塞、不無限成長。
// 稽核紀錄的價值高但不是交易,拿請求延遲去換一列 log 是壞交易;
// 丟掉的數量進 counter 與 log,異常時看得見。
//
// channel 刻意**不 close**:close 之後任何併發的 submit 都會 panic,
// 而 submit 在請求路徑上——用獨立的 stop 訊號收尾,關機路徑不可能打掛請求。
type auditQueue struct {
	ch   chan eventlog.Record
	sink eventlog.Logger
	log  *slog.Logger

	dropped  atomic.Int64
	stop     chan struct{}
	stopOnce sync.Once
	abort    atomic.Bool // 排空超時後不再繼續寫,讓 worker 立刻收工
	done     chan struct{}
	closed   atomic.Bool
}

const (
	// auditQueueSize 是有界緩衝。正常流量綽綽有餘;
	// 異常時(DB 慢)最多壓住這麼多列就開始丟,記憶體不會無限長。
	auditQueueSize = 1024
	// auditWriteTimeout 是單列寫入的預算。在背景執行不影響請求延遲,
	// 但仍要有上限,否則一個卡死的連線會讓整條佇列停擺。
	auditWriteTimeout = 3 * time.Second
	// auditDrainTimeout 是關機時盡力排空的預設上限(呼叫端可用 ctx 覆寫)。
	auditDrainTimeout = 5 * time.Second
)

func newAuditQueue(sink eventlog.Logger, log *slog.Logger) *auditQueue {
	q := &auditQueue{
		ch:   make(chan eventlog.Record, auditQueueSize),
		sink: sink,
		log:  log,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go q.run()
	return q
}

// submit 非阻塞投遞。滿了就丟並計數——請求路徑上絕不等待。
func (q *auditQueue) submit(rec eventlog.Record) {
	if q == nil || q.closed.Load() {
		return
	}
	select {
	case q.ch <- rec:
	default:
		n := q.dropped.Add(1)
		// 丟棄本身是異常訊號,但不能讓 log 也跟著洪水:第一筆與每 100 筆各記一次。
		if n == 1 || n%100 == 0 {
			q.log.Warn("event_logs 佇列已滿,丟棄稽核紀錄",
				"action", rec.Action, "dropped_total", n)
		}
	}
}

// run 是唯一的寫入者。單一 goroutine 也讓寫入天然序列化,不會打爆連線池。
func (q *auditQueue) run() {
	defer close(q.done)
	for {
		select {
		case rec := <-q.ch:
			q.write(rec)
		case <-q.stop:
			q.drain()
			return
		}
	}
}

// drain 盡力把緩衝裡剩下的寫完;abort 被設起來(關機超時)就立刻放手。
func (q *auditQueue) drain() {
	for !q.abort.Load() {
		select {
		case rec := <-q.ch:
			q.write(rec)
		default:
			return
		}
	}
}

func (q *auditQueue) write(rec eventlog.Record) {
	// 背景寫入本來就與請求 ctx 無關,直接用 Background + 自己的預算。
	ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
	defer cancel()
	if err := q.sink.LogEvent(ctx, rec); err != nil {
		q.log.Error("寫入 event_logs 失敗",
			"action", rec.Action, "request_id", rec.RequestID, "err", err)
	}
}

// shutdown 停止收件並盡力排空。ctx 到期就放棄剩下的(關機不能被稽核卡住),
// 且 worker 會立刻收工,不留 goroutine。重複呼叫安全。
func (q *auditQueue) shutdown(ctx context.Context) error {
	if q == nil {
		return nil
	}
	q.closed.Store(true)
	q.stopOnce.Do(func() { close(q.stop) })
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), auditDrainTimeout)
		defer cancel()
	}
	select {
	case <-q.done:
	case <-ctx.Done():
		q.abort.Store(true)
		<-q.done // worker 最多再寫完手上那一列就結束,不洩漏 goroutine
		q.log.Warn("關機時 event_logs 佇列未排空", "err", ctx.Err())
		return ctx.Err()
	}
	if n := q.dropped.Load(); n > 0 {
		q.log.Warn("event_logs 本次執行期間有丟棄", "dropped_total", n)
	}
	return nil
}

// droppedCount 供測試與未來的 metrics 讀取。
func (q *auditQueue) droppedCount() int64 {
	if q == nil {
		return 0
	}
	return q.dropped.Load()
}
