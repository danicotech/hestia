// Package maintenance 是資料膨脹治理的排程器(schemas/14):
// 註冊制 job + 單一 ticker 迴圈,每個 job 依自己的間隔到期才跑。
//
//   - 多實例防重:每次執行先拿 pg_try_advisory_lock(hashtext('maintenance'), hashtext(job名)),
//     拿不到就跳過本輪(別的實例正在跑),下一個 tick 再試——nextRun 只在取鎖成功後才推進,
//     搶輸不會被罰等一整個 every。session-level 鎖釘在專用連線上,
//     跑完在同一條連線 unlock;連線壞掉時鎖也隨斷線自動釋放。
//   - 可觀測:每次「實際執行」寫一列 event_logs(channel=system, kind=job.run),
//     成功失敗都記——schemas/13 的 job.run 在此兌現。跳過(搶輸鎖)不算執行,不記。
//   - 隔離:job panic 收斂成 error,只影響該次執行,不炸 runner。
package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// JobFunc 是一次 job 執行。processed 是本次處理的量(刪了幾列、卸了幾個分區),
// 進 event_logs 的 response;回傳 error 即記 status=error,下次到期照常重跑。
type JobFunc func(ctx context.Context) (processed int, err error)

type job struct {
	name    string
	every   time.Duration
	fn      JobFunc
	nextRun time.Time // 零值 = 啟動後第一輪就到期(維護 job 在重啟時補跑是刻意的)
}

type Runner struct {
	pool *pgxpool.Pool
	jobs []*job

	// 以下有預設值,測試可覆寫。
	PollInterval time.Duration // ticker 輪詢間隔;job 的 every 以此為解析度
	Logger       *slog.Logger
}

func New(pool *pgxpool.Pool) *Runner {
	return &Runner{
		pool:         pool,
		PollInterval: time.Minute,
		Logger:       slog.Default(),
	}
}

// Register 註冊 job。只能在 Run 之前呼叫(之後不做併發保護)。
func (r *Runner) Register(name string, every time.Duration, fn JobFunc) {
	r.jobs = append(r.jobs, &job{name: name, every: every, fn: fn})
}

// Run 輪詢直到 ctx 取消;到期的 job 依註冊順序逐一同步執行。
// 啟動時先跑一輪(維護 job 錯過排程比多跑一次糟),之後每個 tick 檢查到期。
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		r.runDue(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Runner) runDue(ctx context.Context) {
	for _, j := range r.jobs {
		if ctx.Err() != nil {
			return // 關機中:不開始新 job
		}
		now := time.Now()
		if now.Before(j.nextRun) {
			continue
		}
		// 只有實際執行(取到鎖並跑完 fn)才推進 nextRun:搶輸鎖或取鎖失敗都留在
		// 「到期」狀態,下一個 tick 再試。不會變緊迴圈——runDue 每個 tick 只進來一次,
		// 且 job 在本迴圈內同步執行。
		if r.runJob(ctx, j) {
			j.nextRun = now.Add(j.every)
		}
	}
}

// runJob 執行單一 job:釘連線拿 advisory lock → 跑 fn(panic 收斂)→ 寫 event_logs → unlock。
// 回傳是否實際執行了 fn(搶輸鎖或基礎設施錯誤 = false,呼叫端不推進排程)。
func (r *Runner) runJob(ctx context.Context, j *job) bool {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		r.Logger.Error("maintenance: 取鎖連線失敗", "job", j.name, "err", err)
		return false
	}
	defer conn.Release()

	q := db.New(conn)
	locked, err := q.TryMaintenanceLock(ctx, j.name)
	if err != nil {
		r.Logger.Error("maintenance: 取 advisory lock 失敗", "job", j.name, "err", err)
		return false
	}
	if !locked {
		r.Logger.Debug("maintenance: 別的實例持鎖,下一個 tick 再試", "job", j.name)
		return false
	}
	defer func() {
		// 即使 ctx 已取消也要 unlock(WithoutCancel):session 鎖不隨 tx 結束釋放
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := q.MaintenanceUnlock(uctx, j.name); err != nil {
			// unlock 失敗多半是連線已壞:關掉讓 pool 丟棄,鎖隨斷線自動釋放,不會卡死其他實例
			r.Logger.Error("maintenance: unlock 失敗,關閉連線釋放鎖", "job", j.name, "err", err)
			_ = conn.Conn().Close(context.Background())
		}
	}()

	start := time.Now()
	processed, jerr := runSafely(ctx, j.fn)
	r.logRun(ctx, j.name, processed, time.Since(start), jerr)
	return true
}

// runSafely 把 job 的 panic 收斂成 error:一個壞 job 只影響自己這次執行,不炸 runner。
func runSafely(ctx context.Context, fn JobFunc) (processed int, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("job panic: %v", rec)
		}
	}()
	return fn(ctx)
}

// logRun 把一次執行寫進 event_logs(失敗也要記)。寫入本身失敗只能記 slog——
// 不讓可觀測性問題反過來影響 job 執行結果。
func (r *Runner) logRun(ctx context.Context, name string, processed int, latency time.Duration, jerr error) {
	status := "ok"
	resp := map[string]any{"processed": processed}
	if jerr != nil {
		status = "error"
		resp["error"] = jerr.Error()
		r.Logger.Error("maintenance: job 失敗", "job", name, "processed", processed, "err", jerr)
	} else {
		r.Logger.Info("maintenance: job 完成", "job", name, "processed", processed,
			"latency_ms", latency.Milliseconds())
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		payload = []byte(`{}`)
	}
	latencyMs := int32(min(latency.Milliseconds(), int64(1<<31-1)))

	// 關機中(ctx 已取消)也要把已完成的執行記下來:WithoutCancel + 短逾時
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := db.New(r.pool).InsertJobRunLog(lctx, db.InsertJobRunLogParams{
		Action:    name,
		Status:    &status,
		LatencyMs: &latencyMs,
		Response:  payload,
	}); err != nil {
		r.Logger.Error("maintenance: 寫 job.run event_log 失敗", "job", name, "err", err)
	}
}
