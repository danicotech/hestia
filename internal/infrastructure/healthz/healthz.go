// Package healthz 是維運監控指標(schemas/14「監控與告警」)。
//
// 定位:每個檢查都是一句便宜的 SQL,可以被 HTTP handler(/healthz 之類)或
// 排程 job 直接呼叫。它只負責「看見」,不負責修——修是 maintenance 各 job 的事。
//
// 一個概念一個權威:對帳不在這裡重寫,直接呼叫 ledger.Reconcile()(全量比對已經在
// ledgerpg 實作過);分區的預建與卸載也不在這裡,這裡只回報「下個月建了沒 /
// DEFAULT 裡有沒有東西」。
package healthz

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Checker 持有所有檢查需要的相依。門檻是欄位,測試與呼叫端可調。
type Checker struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	ledger ledger.Ledger

	// OutboxStaleAfter:pending 事件超過這個年齡就算積壓。
	// 「有幾筆 pending」本身不是異常(正常吞吐時隨時都有),「最舊的躺了多久」才是。
	OutboxStaleAfter time.Duration
	// SessionReuseWindow:重用偵測的統計窗口。
	SessionReuseWindow time.Duration
	// DefaultPartitionSampleLimit:DEFAULT 分區只數到這麼多列就停。
	// 精確數字沒有額外價值——DEFAULT 裡有 1 列和有 100 萬列都是同一個結論(路由錯了),
	// 而每小時對一張意外變大的表做全表 count 反而會變成新的問題。
	DefaultPartitionSampleLimit int32
	// Logger:job 模式下寫告警用。
	Logger *slog.Logger
}

func New(pool *pgxpool.Pool, led ledger.Ledger) *Checker {
	return &Checker{
		pool:                        pool,
		q:                           db.New(pool),
		ledger:                      led,
		OutboxStaleAfter:            15 * time.Minute,
		SessionReuseWindow:          24 * time.Hour,
		DefaultPartitionSampleLimit: 1000,
		Logger:                      slog.Default(),
	}
}

// ── 1. 對帳 ────────────────────────────────────────────────────────────────

// LedgerMismatches 回傳 SUM(token_entries) != user_balances 的每一組。
// 空清單 = 帳是平的。刻意不自己寫 SQL:ledger.Reconcile() 已經是這件事的權威。
func (c *Checker) LedgerMismatches(ctx context.Context) ([]ledger.Mismatch, error) {
	mis, err := c.ledger.Reconcile(ctx)
	if err != nil {
		return nil, fmt.Errorf("對帳: %w", err)
	}
	return mis, nil
}

// ── 2. outbox 積壓 ─────────────────────────────────────────────────────────

type OutboxBacklog struct {
	Pending          int64
	Failed           int64
	OldestPendingAge time.Duration // 沒有 pending 時為 0
	StaleAfter       time.Duration // 判定用的門檻(帶著走,報告才自我解釋)
}

// Stale 回報最舊的 pending 是否已經躺太久。
func (b OutboxBacklog) Stale() bool {
	return b.Pending > 0 && b.OldestPendingAge > b.StaleAfter
}

func (c *Checker) Outbox(ctx context.Context) (OutboxBacklog, error) {
	row, err := c.q.OutboxBacklog(ctx)
	if err != nil {
		return OutboxBacklog{}, fmt.Errorf("查 outbox 積壓: %w", err)
	}
	return OutboxBacklog{
		Pending:          row.Pending,
		Failed:           row.Failed,
		OldestPendingAge: time.Duration(row.OldestPendingAgeSeconds) * time.Second,
		StaleAfter:       c.OutboxStaleAfter,
	}, nil
}

// ── 3. 排程器健康 ──────────────────────────────────────────────────────────

// JobExpectation:一個 job 與它的預期執行間隔。間隔的權威在註冊端(maintenance),
// 這裡只收參數——healthz 自己另存一份間隔就會變成第二個權威。
type JobExpectation struct {
	Name  string
	Every time.Duration
}

type JobHealth struct {
	Name          string
	Every         time.Duration
	LastSuccessAt time.Time // 零值 = 從來沒成功跑過
	Age           time.Duration
	Stale         bool
}

// Jobs 從 event_logs(channel=system, kind=job.run, status=ok)查各 job 最近一次成功時間。
//
// 判定「太久沒跑」的門檻是 2 × 間隔:runner 是在 job 跑完才寫紀錄的,所以剛好一個
// 間隔時年齡本來就會接近 Every,用 1× 會一直誤報。錯過兩輪才叫真的有事。
// 從來沒跑過(event_logs 裡沒有紀錄)一律算異常——缺席是最需要被看見的那種故障。
func (c *Checker) Jobs(ctx context.Context, expect []JobExpectation) ([]JobHealth, error) {
	if len(expect) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(expect))
	for _, e := range expect {
		names = append(names, e.Name)
	}
	rows, err := c.q.JobLastSuccess(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("查 job 最近成功時間: %w", err)
	}
	last := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		last[r.JobName] = r.LastSuccessAt
	}

	now := time.Now()
	out := make([]JobHealth, 0, len(expect))
	for _, e := range expect {
		h := JobHealth{Name: e.Name, Every: e.Every}
		ts, ok := last[e.Name]
		if !ok {
			h.Stale = true // 從未成功執行
			out = append(out, h)
			continue
		}
		h.LastSuccessAt = ts
		h.Age = now.Sub(ts)
		h.Stale = h.Age > 2*e.Every
		out = append(out, h)
	}
	return out, nil
}

// ── 5. session 異常 ────────────────────────────────────────────────────────

// SessionReuse 回傳窗口內因 reuse_detected 而撤銷的 session 數。
// 這個數字從 0 變正 = 有人拿著已輪替的 refresh token 來換(schemas/02 增補 F),
// 是安全事件不是效能指標,所以門檻就是 0。
func (c *Checker) SessionReuse(ctx context.Context) (int64, error) {
	hours := int32(c.SessionReuseWindow / time.Hour)
	if hours < 1 {
		hours = 1
	}
	n, err := c.q.RecentSessionReuse(ctx, hours)
	if err != nil {
		return 0, fmt.Errorf("查 session 重用偵測: %w", err)
	}
	return n, nil
}
