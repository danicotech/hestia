// hestia 服務進程:目前掛載 outbox 消費者與健康檢查,之後的 HTTP/gRPC API 都長在這裡。
// schema 落地是獨立步驟(cmd/migrate),服務啟動不做 migration——部署順序要顯式。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/maintenance"
	"github.com/danicotech/hestia/internal/infrastructure/outbox"
	"github.com/danicotech/hestia/internal/infrastructure/reaper"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server 結束於錯誤", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("PLATFORM_DATABASE_URL")
	if dsn == "" {
		return errors.New("環境變數 PLATFORM_DATABASE_URL 未設定")
	}
	addr := os.Getenv("HESTIA_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}

	consumer := outbox.NewConsumer(pool)
	// topic handler 隨功能上線註冊(M2 起:daily.claimed → Discord 推播等)

	// 資料膨脹治理(schemas/14):分區維護、outbox/冪等鍵清理、權益到期回收
	runner := maintenance.New(pool)
	maintenance.RegisterDefaults(runner, pool)
	runner.Register("entitlement_reaper", time.Minute, reaper.New(pool).Reap)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 3)
	go func() {
		slog.Info("outbox 消費者啟動")
		if err := consumer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	go func() {
		slog.Info("maintenance 排程器啟動")
		if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	go func() {
		slog.Info("http 啟動", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	// 三個元件(consumer / runner / http)各送一次 errCh。
	// 任何一個結束——不論 error 或 nil(元件提前消失也是異常)——都走完整關機,
	// 並 join 全部 goroutine 再返回(QA:錯誤路徑不遺漏收尾,不留半死進程)。
	consumed := 0
	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("收到關機訊號")
	case err := <-errCh:
		consumed++
		runErr = err
		if err != nil {
			slog.Error("元件致命錯誤,進入關機", "err", err)
		} else {
			slog.Warn("元件在無關機訊號下自行結束,進入關機")
			runErr = errors.New("元件提前結束")
		}
	}
	stop()
	shutdown(srv)
	for ; consumed < 3; consumed++ {
		if err := <-errCh; err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
