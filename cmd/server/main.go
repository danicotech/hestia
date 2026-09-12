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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/prize"
	"github.com/danicotech/hestia/internal/core/activity/session"
	"github.com/danicotech/hestia/internal/core/activity/signup"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/infrastructure/maintenance"
	"github.com/danicotech/hestia/internal/infrastructure/outbox"
	"github.com/danicotech/hestia/internal/infrastructure/reaper"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activitylogpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activityreadpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/admineconpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/authzpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/bettingpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/dailypg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/eventlogpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/identitypg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/matchpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/notificationpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/playpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/readpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/shoppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/signuppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/tournamentpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/watchpg"
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
	// 掛載前綴。空 = 掛在根(預設,行為與加這個功能之前完全相同);
	// 正式部署設 "/api" —— 前端在 https://arena.gengflow.com/,後端在同一個
	// host 的 /api 底下,同源所以 session cookie 不跨網域。
	//
	// 這裡就正規化並在不合法時直接回錯:前綴寫錯不會有任何徵兆,
	// 端點只是全部搬到別的位置,在測試環境看起來像 404。
	// 正規化後的形狀是唯一的("" 或 "/xxx"),下面才能無條件字串相接。
	basePath, err := transport.NormalizeBasePath(os.Getenv("HESTIA_BASE_PATH"))
	if err != nil {
		return fmt.Errorf("HESTIA_BASE_PATH: %w", err)
	}
	// 願意相信其轉發標頭的對端網段(見 transport.Deps.TrustedProxies)。
	// 空 = 誰都不信,sessions.ip 一律記連線對端。服務放在 Cloudflare Tunnel
	// 後面時對端永遠是 cloudflared,不設就等於每一列 session 都記同一個假 IP,
	// 那個欄位形同不存在。
	trustedProxies, err := transport.ParseTrustedProxies(os.Getenv("HESTIA_TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("HESTIA_TRUSTED_PROXIES: %w", err)
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
		// 小遊戲 / 開箱 / 抽獎 / 寵物(schemas/25)。動錢全部走帳本,
		// 與抽籤留痕同 tx —— 少了留痕,產出速率就算不出來。
		Play:          playpg.New(pool, led),
		Communities:   read,
		ServiceTokens: serviceTokens(),
		// 入口層據此決定路由前綴與 cookie 的 Path(兩者必須一致,
		// 不然登入看起來成功但每一支 RPC 都拿不到 cookie)。
		BasePath:       basePath,
		TrustedProxies: trustedProxies,
	}

	// ── 活動層(《百業試鋒》)
	//
	// 領域服務住在 internal/core/activity,M1 期間與平台層同一個 repo、同一顆
	// Postgres 的兩個 schema。第二個活動時整包抽到 themis(見 CLAUDE.md)。
	//
	// 組裝順序有意義:handicap 與 betting 要先存在,match 才組得起來 ——
	// 判定勝負是一個橫跨三者的 transaction(封盤、結算注單、晉級),
	// 而那個 tx 的擁有者是 match。
	hasher, err := signup.NewHasher(0)
	if err != nil {
		return fmt.Errorf("建立通行碼雜湊器: %w", err)
	}
	signer, err := buildSigner()
	if err != nil {
		return err
	}
	activityRead := activityreadpg.New(pool)
	signupRepo := signuppg.New(pool)
	tournamentRepo := tournamentpg.New(pool)
	tournaments := tournament.NewService(tournamentRepo)
	bets := bettingpg.New(pool, led)
	matches := match.NewService[pgx.Tx](
		matchpg.New(pool),
		matchpg.NewHandicaps(func(tx pgx.Tx) handicap.Repository { return handicappg.BindTx(tx) }),
		bets,
	)
	activityDeps := transport.ActivityDeps{
		Signup:     signup.NewService(signupRepo, hasher),
		Tournament: tournaments,
		// 建賽事是裁判專屬,與上面的唯讀 Tournament 分開注入。
		TournamentCreator: tournaments,
		Handicap:          handicap.New(handicappg.New(pool)),
		Betting:           bets,
		Matches:           matches,
		Reader:            activityRead,
		Prizes: prizeAdapter{svc: prize.New(
			prizeRepo{Service: activityRead, tournaments: tournamentRepo}, led, "")},
		Directory: read,
	}
	// 選手 session 只需要平台密鑰,不需要 Discord。沒設密鑰時報名照常,
	// 但選手登入不了(handler 會回 Unimplemented)—— 讓它安靜地半通不如
	// 在啟動時就說清楚。
	if signer != nil {
		activityDeps.Sessions = activitySessionAdapter{
			svc: session.New(signer, signupRepo),
		}
	} else {
		slog.Warn("PLATFORM_JWT_SECRET 未設定,選手登入停用(報名與公開檢視照常)")
	}
	deps.ActivityLayer = &activityDeps

	// ── 即時戰況推播
	//
	// Hub 是行程內的扇出樞紐,LISTEN 迴圈是唯一餵它的東西。兩者分開建立是
	// 因為生命週期不同:Hub 要跟著行程結束時關掉(關掉會讓所有訂閱者收到
	// 結束訊號,handler 才走得完),而 LISTEN 迴圈自己會重連,只有 ctx
	// 取消才停。
	//
	// 推播只影響「畫面會不會自動更新」——它掛掉不該讓賽事本身停擺,所以
	// 迴圈的錯誤是記 log 而不是讓行程退出。
	hub := watch.NewHub(0)
	defer hub.Close()
	deps.WatchHub = hub
	go func() {
		if err := watchpg.New(pool, hub, watchpg.Options{Logger: slog.Default()}).Run(ctx); err != nil {
			slog.Error("即時推播的 LISTEN 迴圈結束", "err", err)
		}
	}()

	// ── 身分:Discord 憑證齊全才啟用。缺了就讓 AuthService 保持 Unimplemented,
	// 其餘 API 照常運作——本機還沒申請 Discord 應用程式時也能把服務跑起來。
	if idsvc, err := buildIdentity(pool, led, signer); err != nil {
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
	// 里程碑獎勵(schemas/24)。與升級公告刻意是兩個 topic:
	// 公告由閘道投遞、獎勵在這裡發,兩者的失敗模式不同 ——
	// Discord 限流時公告會重試,但那不該讓獎勵跟著卡住。
	rewards := xppg.NewRewardGranter(xpSvc, led)
	consumer.Handle(notification.TopicLevelReward,
		func(ctx context.Context, _ string, payload []byte) error {
			return rewards.Handle(ctx, payload)
		})

	// Consumer.Handle 會直接拒絕閘道負責的 topic,所以「兩邊都處理」在結構上
	// 不可能;這道斷言讀的是實際註冊結果(不是手寫清單),當作第二層保險。
	if err := notification.AssertNoOverlap(consumer.RegisteredTopics()); err != nil {
		return fmt.Errorf("outbox topic 分工衝突: %w", err)
	}

	// 資料膨脹治理(schemas/14):分區維護、outbox/冪等鍵/session 清理、權益到期回收
	runner := maintenance.New(pool)
	maintenance.RegisterDefaults(runner, pool)
	runner.Register("entitlement_reaper", time.Minute, reaper.New(pool).Reap)

	// 全部端點掛在同一個前綴底下:healthz、ConnectRPC、瀏覽器登入路由。
	// basePath 為空時這兩行就是 "GET /healthz" 與 "/",與從前一字不差。
	//
	// srv 自己會把前綴剝掉再交給 connect mux(transport.mountAt),
	// 所以 procedure 路徑與存取層級斷言完全不受前綴影響。
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+basePath+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	// **掛在根而不是 basePath+"/"**:前綴之外也有 srv 要處理的路由
	// (/login 轉址),而且稽核層本來就設計成看得到前綴外的探測流量
	// (auditHTTP 的說明)。掛在前綴上的話那兩件事都到不了 srv,
	// 前者失效、後者是有文件卻從未生效的功能。
	//
	// 不會多開放任何東西:srv 內層的 mountAt 對前綴外的路徑照樣回 404。
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
		// 印出可信代理:設錯時 sessions.ip 會全部變成代理自己的位址,
		// 那是幾個月後才會有人發現的壞法,啟動時就要看得到。
		slog.Info("http 啟動", "addr", addr, "base_path", basePath,
			"trusted_proxies", trustedProxies)
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

// buildIdentity 在 Discord 憑證齊全時建立身分服務;沒有 Discord 憑證時回 (nil, nil, nil)。
//
// 「是否啟用登入」只由 **Discord 三個憑證**決定,不把平台密鑰算進去:
// PLATFORM_JWT_SECRET / _TOKEN_ENC_KEY 是平台自己的金鑰,先產生放著是正常的,
// 不該因為「密鑰有了但還沒去申請 Discord 應用程式」就讓整個服務起不來
// (那會擋住「先把服務跑起來看資料表」這種完全合理的事)。
//
// 但 Discord 三個之間仍是全有全無:半套的登入設定只會在使用者按下登入時才炸。
// buildSigner 建平台的 token 簽章器。回 (nil, nil) 表示沒設密鑰。
//
// 獨立於 Discord 憑證:選手 session(活動層)只需要這把密鑰,不需要 Discord
// 應用程式。綁在一起的話,還沒申請 Discord 的部署連選手都登入不了 ——
// 而選手本來就不需要平台帳號,那正是活動層身分存在的理由。
func buildSigner() (*identity.Signer, error) {
	secret := os.Getenv("PLATFORM_JWT_SECRET")
	if secret == "" {
		return nil, nil
	}
	signer, err := identity.NewSigner([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("PLATFORM_JWT_SECRET: %w", err)
	}
	return signer, nil
}

func buildIdentity(pool *pgxpool.Pool, led *ledgerpg.Service, signer *identity.Signer) (identity.Service, error) {
	cfg := identitypg.Config{
		ClientID:     os.Getenv("PLATFORM_DISCORD_CLIENT_ID"),
		ClientSecret: os.Getenv("PLATFORM_DISCORD_CLIENT_SECRET"),
		RedirectURI:  os.Getenv("PLATFORM_DISCORD_REDIRECT_URI"),
		TokenEncKey:  []byte(os.Getenv("PLATFORM_TOKEN_ENC_KEY")),
	}
	discordSet := 0
	for _, v := range []string{cfg.ClientID, cfg.ClientSecret, cfg.RedirectURI} {
		if v != "" {
			discordSet++
		}
	}
	switch discordSet {
	case 0:
		return nil, nil // 還沒申請 Discord 應用程式:登入停用,其餘照跑
	case 3:
	default:
		return nil, errors.New(
			"PLATFORM_DISCORD_CLIENT_ID / _SECRET / _REDIRECT_URI 必須全部設定或全部不設定")
	}
	// 到這裡代表要啟用登入,平台密鑰就成了必要條件
	if signer == nil || len(cfg.TokenEncKey) == 0 {
		return nil, errors.New(
			"啟用 Discord 登入還需要 PLATFORM_JWT_SECRET 與 PLATFORM_TOKEN_ENC_KEY")
	}
	svc, err := identitypg.New(pool, led, signer, cfg)
	if err != nil {
		return nil, fmt.Errorf("建立身分服務: %w", err)
	}
	return svc, nil
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
