package healthz

import (
	"context"
	"fmt"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// Report 是一次完整健康檢查的結果。欄位是「事實」,Anomalies() 才做判斷——
// 這樣同一份結果既能餵給 HTTP handler 原樣輸出,也能給 job 拿去決定要不要告警。
type Report struct {
	LedgerMismatches []ledger.Mismatch
	Outbox           OutboxBacklog
	Jobs             []JobHealth
	Partitions       PartitionHealth
	SessionReuse     int64
	// SessionReuseWindow:上面那個數字的統計窗口,帶著走讓報告自我解釋。
	SessionReuseWindow time.Duration
}

// Anomaly 是一項異常。Kind 給機器分類,Detail 給人看。
type Anomaly struct {
	Kind   string
	Detail string
}

// Anomalies 把 Report 攤成異常清單(空 = 一切正常)。
// 一項異常一列:告警接法之後才定,先確保「有幾件事不對」是可數的。
func (r Report) Anomalies() []Anomaly {
	var out []Anomaly
	if n := len(r.LedgerMismatches); n > 0 {
		m := r.LedgerMismatches[0]
		out = append(out, Anomaly{
			Kind: "ledger_mismatch",
			Detail: fmt.Sprintf("%d 組 SUM(entries) != balance(例:user=%d %s balance=%d entries=%d)",
				n, m.UserID, m.Currency, m.Balance, m.EntryTotal),
		})
	}
	if r.Outbox.Stale() {
		out = append(out, Anomaly{
			Kind: "outbox_backlog",
			Detail: fmt.Sprintf("最舊的 pending 事件已積壓 %s(門檻 %s),pending=%d",
				r.Outbox.OldestPendingAge.Round(time.Second), r.Outbox.StaleAfter, r.Outbox.Pending),
		})
	}
	if r.Outbox.Failed > 0 {
		out = append(out, Anomaly{
			Kind:   "outbox_failed",
			Detail: fmt.Sprintf("%d 筆事件已進 failed(重試耗盡,需人工處理)", r.Outbox.Failed),
		})
	}
	for _, j := range r.Jobs {
		if !j.Stale {
			continue
		}
		if j.LastSuccessAt.IsZero() {
			out = append(out, Anomaly{
				Kind:   "job_never_ran",
				Detail: fmt.Sprintf("job %s 從未成功執行過(預期每 %s 一次)", j.Name, j.Every),
			})
			continue
		}
		out = append(out, Anomaly{
			Kind: "job_stale",
			Detail: fmt.Sprintf("job %s 已 %s 沒有成功執行(預期每 %s 一次)",
				j.Name, j.Age.Round(time.Second), j.Every),
		})
	}
	for _, t := range r.Partitions.NextMonthMissing {
		out = append(out, Anomaly{
			Kind:   "partition_next_month_missing",
			Detail: fmt.Sprintf("%s 沒有下個月的分區,月初起寫入會全部落進 DEFAULT", t),
		})
	}
	for _, d := range r.Partitions.DefaultWithRows {
		out = append(out, Anomaly{
			Kind: "partition_default_has_rows",
			Detail: fmt.Sprintf("%s 的 DEFAULT 分區有資料(取樣到 %d 列):有列沒被正確路由,將躲過保留期清理",
				d.Parent, d.RowCount),
		})
	}
	if r.SessionReuse > 0 {
		out = append(out, Anomaly{
			Kind: "session_reuse_detected",
			Detail: fmt.Sprintf("%s 內有 %d 個 session 因 reuse_detected 被撤銷(token 可能外洩)",
				r.SessionReuseWindow, r.SessionReuse),
		})
	}
	return out
}

// Check 跑全部檢查。任何一項查不到都直接回錯——監控自己壞掉時不能假裝系統健康。
func (c *Checker) Check(ctx context.Context, expect []JobExpectation) (Report, error) {
	var r Report
	var err error

	if r.LedgerMismatches, err = c.LedgerMismatches(ctx); err != nil {
		return r, err
	}
	if r.Outbox, err = c.Outbox(ctx); err != nil {
		return r, err
	}
	if r.Jobs, err = c.Jobs(ctx, expect); err != nil {
		return r, err
	}
	if r.Partitions, err = c.Partitions(ctx); err != nil {
		return r, err
	}
	if r.SessionReuse, err = c.SessionReuse(ctx); err != nil {
		return r, err
	}
	r.SessionReuseWindow = c.SessionReuseWindow
	return r, nil
}

// HealthCheck 是 maintenance job(每小時):跑全部檢查,異常用 slog.Error 記,
// 正常記 Info。processed = 發現的異常項數。
//
// 回傳 error 只保留給「檢查本身跑不動」(SQL 失敗);發現異常不是 job 失敗——
// job 成功地完成了它的工作,是系統有問題。這個分野讓 event_logs 裡的
// status=error 仍然只代表排程器自己壞了。
//
// 注意:expect 不該包含 health_check 自己。它沒跑的話也不會有人來報告它沒跑,
// 這層要靠外部存活監控(容器健康檢查)。
func (c *Checker) HealthCheck(expect []JobExpectation) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		rep, err := c.Check(ctx, expect)
		if err != nil {
			return 0, err
		}
		anomalies := rep.Anomalies()
		for _, a := range anomalies {
			c.Logger.Error("healthz: 異常", "kind", a.Kind, "detail", a.Detail)
		}
		if len(anomalies) == 0 {
			c.Logger.Info("healthz: 全部正常",
				"outbox_pending", rep.Outbox.Pending,
				"jobs_checked", len(rep.Jobs))
		}
		return len(anomalies), nil
	}
}
