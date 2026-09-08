// hestia 服務進程:HTTP/gRPC API、outbox 消費者、維運排程器。
//
// schema 落地是獨立步驟(cmd/migrate),服務啟動不做 migration——部署順序要顯式。
//
// 設定全部從環境變數讀,而且**只在這裡讀**:各套件經建構子注入,
// 這樣單元測試不必碰環境,也不會有「某個套件偷偷讀了某個變數」。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/infrastructure/maintenance"
	"github.com/danicotech/hestia/internal/infrastructure/outbox"
	"github.com/danicotech/hestia/internal/infrastructure/reaper"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activitylogpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/admineconpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/authzpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/dailypg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/eventlogpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/identitypg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/notificationpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/readpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/shoppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/xppg"
	"github.com/danicotech/hestia/internal/transport"
)

// nowFunc 讓 adapter 與測試共用同一個時間來源。
var nowFunc = time.Now

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

	// ── 領域服務(帳本是其他動錢服務的共同依賴)
	led := ledgerpg.New(pool)
	read := readpg.New(pool)
	xpSvc := xppg.New(pool)
	activity := activitylogpg.New(pool, xpSvc, slog.Default())
	deps := transport.Deps{
		Daily:         dailypg.New(pool, led),
		Shop:          shoppg.New(pool, led),
		AdminEcon:     admineconpg.New(pool, led),
		Ledger:        led,
		Profiles:      read,
		ProfileWrites: read,
		Catalog:       read,
		LedgerEntries: read,
		Directory:     read,
		EventLog:      eventlogpg.New(pool),
		Authorizer:    authzpg.New(pool),
		Privacy:       read,
		Activity:      activity,
		ActingUsers:   activity,
		Announcements: notificationpg.New(pool, notifyOpts()...),
		ServiceTokens: serviceTokens(),
	}

	// ── 身分:Discord 憑證齊全才啟用。缺了就讓 AuthService 保持 Unimplemented,
	// 其餘 API 照常運作——本機還沒申請 Discord 應用程式時也能把服務跑起來。
	if idsvc, signer, err := buildIdentity(pool, led); err != nil {
		return err
	} else if idsvc != nil {
		deps.Auth = &authAdapter{svc: idsvc, signer: signer, profiles: read, dir: read}
		deps.Authenticator = idsvc
		slog.Info("身分服務已啟用(Discord OAuth2)")
	} else {
		slog.Warn("Discord 憑證未設定,登入相關 API 停用",
			"需要", "PLATFORM_DISCORD_CLIENT_ID / _SECRET / _REDIRECT_URI、PLATFORM_JWT_SECRET、PLATFORM_TOKEN_ENC_KEY")
	}

	srv, err := transport.New(deps)
	if err != nil {
		return fmt.Errorf("建立 API server: %w", err)
	}

	// 閘道經 NotificationService 拉取的 topic 由它負責投遞,in-process 消費者必須跳過:
	// 否則會認領 → 找不到 handler → 退避重試到上限 → 標 failed,公告被燒掉。
	consumer := outbox.NewConsumer(pool)
	consumer.ExcludedTopics = notification.DiscordTopics()
	// 之後在這裡註冊 in-process handler(如身分組回收)。Consumer.Handle 會直接
	// 拒絕閘道負責的 topic,所以「兩邊都處理」在結構上不可能;這道斷言讀的是
	// 實際註冊結果(不是手寫清單),當作第二層保險。
	if err := notification.AssertNoOverlap(consumer.RegisteredTopics()); err != nil {
		return fmt.Errorf("outbox topic 分工衝突: %w", err)
	}

	// 資料膨脹治理(schemas/14):分區維護、outbox/冪等鍵/session 清理、權益到期回收
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
	mux.Handle("/", srv)
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// 三個背景元件各送一次 errCh。任何一個結束——不論 error 或 nil
	// (元件提前消失也是異常)——都走完整關機,並 join 全部 goroutine 再返回。
	errCh := make(chan error, 3)
	go func() {
		slog.Info("outbox 消費者啟動")
		errCh <- ignoreCanceled(consumer.Run(ctx))
	}()
	go func() {
		slog.Info("maintenance 排程器啟動")
		errCh <- ignoreCanceled(runner.Run(ctx))
	}()
	go func() {
		slog.Info("http 啟動", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	consumed := 0
	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("收到關機訊號")
	case err := <-errCh:
		consumed++
		if err != nil {
			runErr = err
			slog.Error("元件致命錯誤,進入關機", "err", err)
		} else {
			runErr = errors.New("元件提前結束")
			slog.Warn("元件在無關機訊號下自行結束,進入關機")
		}
	}
	stop()
	shutdown(httpSrv, srv)
	for ; consumed < 3; consumed++ {
		if err := <-errCh; err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

// buildIdentity 在 Discord 憑證齊全時建立身分服務;完全沒設定時回 (nil, nil, nil)。
// 設定「一半」視為錯誤——半套的登入設定只會在使用者按下登入時才炸,不如啟動就講。
func buildIdentity(pool *pgxpool.Pool, led *ledgerpg.Service) (identity.Service, *identity.Signer, error) {
	cfg := identitypg.Config{
		ClientID:     os.Getenv("PLATFORM_DISCORD_CLIENT_ID"),
		ClientSecret: os.Getenv("PLATFORM_DISCORD_CLIENT_SECRET"),
		RedirectURI:  os.Getenv("PLATFORM_DISCORD_REDIRECT_URI"),
		TokenEncKey:  []byte(os.Getenv("PLATFORM_TOKEN_ENC_KEY")),
	}
	secret := os.Getenv("PLATFORM_JWT_SECRET")

	set := 0
	for _, v := range []string{cfg.ClientID, cfg.ClientSecret, cfg.RedirectURI, string(cfg.TokenEncKey), secret} {
		if v != "" {
			set++
		}
	}
	switch set {
	case 0:
		return nil, nil, nil
	case 5:
	default:
		return nil, nil, errors.New("身分服務的 5 個環境變數必須全部設定或全部不設定(見 .env.example)")
	}

	signer, err := identity.NewSigner([]byte(secret))
	if err != nil {
		return nil, nil, fmt.Errorf("PLATFORM_JWT_SECRET: %w", err)
	}
	svc, err := identitypg.New(pool, led, signer, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("建立身分服務: %w", err)
	}
	return svc, signer, nil
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// shutdown 先停 HTTP(不再收新請求),再排空 API server 的稽核佇列。
// 順序不能反:佇列還在收東西時排空是白做工。
func shutdown(httpSrv *http.Server, srv *transport.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("稽核佇列未完全排空", "err", err)
	}
}

// serviceTokens 從 PLATFORM_SERVICE_TOKENS 讀服務憑證,格式 `name:token,name:token`。
// 支援多把是刻意的:輪替期間新舊並存,否則換 token 必然中斷服務。
// 格式不合法就整把跳過並警告——寧可少一把讓呼叫方明確被拒,
// 也不要把畸形字串當成有效憑證。完全沒設定時服務照常起,只是服務端點全部拒絕。
func serviceTokens() []transport.ServiceToken {
	raw := os.Getenv("PLATFORM_SERVICE_TOKENS")
	if raw == "" {
		return nil
	}
	var out []transport.ServiceToken
	for _, pair := range strings.Split(raw, ",") {
		name, token, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok || name == "" || token == "" {
			slog.Warn("PLATFORM_SERVICE_TOKENS 有一項格式不符 name:token,已跳過")
			continue
		}
		// Delegable 決定這把憑證能代表使用者做什麼。空 = 不能代打(fail closed)。
		// 目前只有 stentor 一個呼叫方,給全部;發第二把憑證時應逐條收窄,
		// 因為「能代打」等於「能代表任何已綁定使用者花錢」。
		out = append(out, transport.ServiceToken{
			Name: name, Token: token,
			Delegable: transport.AllDelegableProcedures(),
		})
	}
	return out
}

// notifyOpts 提供公告事件 id 的簽章金鑰。
//
// 沒設定時 notificationpg 會用行程內隨機金鑰並警告:功能照常,
// 但 hestia 重啟後未 Ack 的公告會拿到新的 id,閘道的去重鍵因此失效,
// 那幾則可能被重貼一次。設一把固定金鑰就沒有這個窗口。
func notifyOpts() []notificationpg.Option {
	raw := os.Getenv("PLATFORM_NOTIFY_EVENT_KEY")
	if raw == "" {
		return nil
	}
	key, err := notificationpg.NewEventIDKey([]byte(raw))
	if err != nil {
		// 設錯不能靜靜降級成隨機金鑰——那會讓人以為窗口已經關上
		slog.Error("PLATFORM_NOTIFY_EVENT_KEY 無效,改用行程內隨機金鑰", "err", err)
		return nil
	}
	return []notificationpg.Option{notificationpg.WithEventIDKey(key)}
}
