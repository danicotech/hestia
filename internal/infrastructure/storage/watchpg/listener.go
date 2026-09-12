// Package watchpg 是即時戰況推播的資料庫側:LISTEN 迴圈 + 把信封讀成完整內容。
//
// # 為什麼是 LISTEN / NOTIFY
//
// 理由是正確性,寫在 internal/core/activity/watch 的檔頭:**NOTIFY 只在交易
// commit 時才送出**,rollback 的交易一個字都不會送。行程內 hub 給不了這個保證。
// 本套件的整合測試直接釘住這一條(TestNotifyOnlyOnCommit)——沒有那條測試,
// 選這個方案的唯一理由就沒有人在守。
//
// # 為什麼用專屬連線而不是從 pool 借
//
// LISTEN 是**連線層級**的狀態:借來的連線還回 pool 之後,pool 可能把它交給
// 別人、可能重置它、也可能因為閒置把它關掉,而 LISTEN 會跟著消失 ——
// 症狀是推播在服務閒了幾分鐘之後靜靜停止,而且重啟就好(所以永遠查不出原因)。
// 另外 WaitForNotification 會長時間占住連線,借 pool 的連線等於長期扣住一格。
//
// # 斷線會漏,而且不補
//
// 連線斷掉期間發出的通知收不到,重連後也不會補。這是可接受的:前端重連時
// 會先拉一次完整狀態(watch.proto 檔頭的三步),缺口會被那次拉取補平。
// 但**每一次斷線與重連都要留下 log** —— 「推播悄悄停了一小時」與
// 「推播斷線重連了三次」在畫面上看起來一模一樣,只有 log 分得出來。
package watchpg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 重連退避的預設值。
//
// 起點 200ms:資料庫重啟或網路抖一下,通常一兩秒內就回來,退避太保守會讓
// 推播白白多停幾十秒。上限 15s:資料庫真的掛了的時候,一條連線每 15 秒敲一次
// 既不會把它壓垮,也不會讓恢復之後的空窗長到有人發現。
const (
	defaultMinBackoff = 200 * time.Millisecond
	defaultMaxBackoff = 15 * time.Second
	// resolveTimeout 是「讀完整內容」的上限。
	//
	// 它跑在 LISTEN 迴圈裡,一次慢查詢就是所有觀眾一起卡住,所以寧可放棄
	// 這一則(前端下次重連會拉到完整狀態)也不要讓迴圈停住。
	resolveTimeout = 5 * time.Second
)

// Options 是 Listener 的可選設定。零值即預設。
type Options struct {
	// Logger 為 nil 時用 slog.Default()。
	Logger *slog.Logger
	// MinBackoff / MaxBackoff 是重連退避;<= 0 用預設值。
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// OnListen 在每次(重新)建立 LISTEN 之後被呼叫。
	//
	// 正式環境不用設。它存在是給測試用的:測試必須知道「LISTEN 已經生效」
	// 才能送 NOTIFY,否則會踩到「通知早於訂閱」的競態 —— 而那個競態會讓
	// 測試偶發地綠,那比紅更糟。
	OnListen func()
}

// Listener 跑 LISTEN 迴圈:收信封 → 讀完整內容 → 餵進 Hub。
type Listener struct {
	connConfig *pgx.ConnConfig
	q          *db.Queries
	hub        *watch.Hub
	log        *slog.Logger
	minBackoff time.Duration
	maxBackoff time.Duration
	onListen   func()
	now        func() time.Time
}

// New 建立 Listener。
//
// pool 只用來**讀完整內容**;LISTEN 走另外一條自己撥的連線(見 package doc),
// 連線參數沿用 pool 的設定 —— 兩者連的必須是同一個資料庫,而讓組裝端
// 再傳一次連線字串就是給了它傳錯的機會。
func New(pool *pgxpool.Pool, hub *watch.Hub, opt Options) *Listener {
	l := &Listener{
		connConfig: pool.Config().ConnConfig.Copy(),
		q:          db.New(pool),
		hub:        hub,
		log:        opt.Logger,
		minBackoff: opt.MinBackoff,
		maxBackoff: opt.MaxBackoff,
		onListen:   opt.OnListen,
		now:        time.Now,
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if l.minBackoff <= 0 {
		l.minBackoff = defaultMinBackoff
	}
	if l.maxBackoff < l.minBackoff {
		l.maxBackoff = defaultMaxBackoff
	}
	if l.maxBackoff < l.minBackoff {
		l.maxBackoff = l.minBackoff
	}
	return l
}

// Run 阻塞執行 LISTEN 迴圈直到 ctx 取消。
//
// **永遠不會因為一次失敗就結束**:連不上、LISTEN 被拒、連線中途斷掉,
// 一律退避後重來。回傳 nil 表示 ctx 取消(正常關機)——
// 呼叫端不需要分辨「該不該重啟它」,因為它自己會重啟自己。
//
// Hub 的關閉不在這裡:Hub 的生命週期比這條迴圈長(它在斷線重連期間仍然
// 持有訂閱者),誰建立誰負責關。
func (l *Listener) Run(ctx context.Context) error {
	backoff := l.minBackoff
	for {
		if stopping(ctx) {
			return nil
		}
		established, err := l.listen(ctx)
		// 關機優先於錯誤處理:ctx 一取消,listen 必定帶著一個「連線被取消」
		// 的錯誤回來,而那不是故障,不該記成告警也不該退避重來。
		if stopping(ctx) {
			l.log.Info("戰況推播的 LISTEN 迴圈結束(服務關閉中)")
			return nil
		}
		if established {
			// 這次是「連上之後才斷的」:下一次退避從頭算起。
			// 不重設的話,一次長時間離線會讓之後每一次短暫抖動都要等滿 15 秒。
			backoff = l.minBackoff
			l.log.Warn("戰況推播的 LISTEN 連線中斷,準備重連",
				"error", err, "retry_in", backoff, "channel", watch.Channel)
		} else {
			l.log.Error("戰況推播連不上資料庫,將重試",
				"error", err, "retry_in", backoff, "channel", watch.Channel)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < l.maxBackoff {
			backoff = min(backoff*2, l.maxBackoff)
		}
	}
}

// stopping 回報服務是不是正在關機。
//
// 用 select 而不是 ctx.Err() != nil,是為了讓「關機」與「錯誤」在程式碼上
// 就分得開:ctx 取消時 listen 一定會帶著一個錯誤回來,但那個錯誤不是故障。
func stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// listen 撥一條專屬連線、LISTEN、然後收到天荒地老。
//
// 回傳的 established 表示「這一輪有沒有真的建立起訂閱」,呼叫端用它決定
// 要不要重設退避 —— 連都連不上的時候不該重設,不然就是一個忙迴圈。
func (l *Listener) listen(ctx context.Context) (established bool, err error) {
	conn, err := pgx.ConnectConfig(ctx, l.connConfig.Copy())
	if err != nil {
		return false, fmt.Errorf("撥推播專用連線: %w", err)
	}
	defer func() {
		// 關閉不能用 ctx:正常關機時 ctx 已經取消,那會讓 Close 立刻放棄,
		// 連線只能等 TCP 逾時才在伺服器端消失。
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	// 頻道名是識別字不是值,不能用參數;Sanitize 是這裡唯一正確的引法。
	// (名字本身是 Go 常數不是使用者輸入,這一句是防「日後有人把它變成設定」。)
	channel := pgx.Identifier{watch.Channel}.Sanitize()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return false, fmt.Errorf("LISTEN %s: %w", watch.Channel, err)
	}
	l.log.Info("戰況推播已開始接收", "channel", watch.Channel)
	if l.onListen != nil {
		l.onListen()
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("等待通知: %w", err)
		}
		l.dispatch(ctx, n.Payload)
	}
}

// dispatch 把一則通知變成一則推播。
//
// **任何失敗都只記 log 然後繼續**:這條迴圈服務所有觀眾,為了一則讀不出來的
// 變化而中斷它,等於讓一筆壞資料關掉整個推播。漏掉的那一則會在前端下次
// 重連拉完整狀態時補回來。
func (l *Listener) dispatch(ctx context.Context, payload string) {
	env, err := watch.ParseEnvelope(payload)
	if err != nil {
		l.log.Warn("收到無法解析的推播信封", "error", err, "payload", payload)
		return
	}
	// 沒有人在看的賽事就不必查資料庫了。裁判在後台操作、還沒有人打開頁面
	// 是常態,而這個判斷讓那段期間的成本降到一次 map 查詢。
	if l.hub.Subscribers(env.Tournament) == 0 {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	u, err := l.resolve(readCtx, env)
	if err != nil {
		l.log.Warn("讀不出推播內容,略過這一則",
			"error", err, "tournament", env.Tournament, "kind", env.Kind, "ref", env.Ref)
		return
	}
	l.hub.Publish(*u)
}

// resolve 依信封讀出完整內容。
func (l *Listener) resolve(ctx context.Context, env watch.Envelope) (*watch.Update, error) {
	u := &watch.Update{Tournament: env.Tournament, Kind: env.Kind, EmittedAt: l.now().UTC()}
	switch env.Kind {
	case watch.KindMatch:
		m, err := l.readMatch(ctx, env.Ref)
		if err != nil {
			return nil, err
		}
		u.Match = m
	case watch.KindHandicapLocked:
		h, err := l.readHandicaps(ctx, env.Ref)
		if err != nil {
			return nil, err
		}
		u.Handicaps = h
	case watch.KindOdds:
		o, err := l.readOdds(ctx, env.Ref)
		if err != nil {
			return nil, err
		}
		u.Odds = o
	case watch.KindPhase:
		p, err := l.readPhase(ctx, env.Tournament)
		if err != nil {
			return nil, err
		}
		u.Phase = p
	case watch.KindChampion:
		c, err := l.readChampion(ctx, env.Ref)
		if err != nil {
			return nil, err
		}
		u.Champion = c
	default:
		// 新版本的發送端可能送出舊版本不認得的種類(滾動更新)。
		// 那不是錯誤,略過就好 —— 但要看得見,不然「前端說沒收到」會查很久。
		return nil, fmt.Errorf("不認得的推播種類 %q", env.Kind)
	}
	return u, nil
}

// notFound 把 pgx.ErrNoRows 包成看得懂的訊息。
//
// 查無此列在這條路徑上是**正常**可達的:通知在飛的時候那一列被刪了
// (重抽籤會刪掉整屆場次)。所以它是 Warn 等級的一則 log,不是錯誤上拋。
func notFound(what, ref string, err error) error {
	if isNoRows(err) {
		return fmt.Errorf("%s %s 已不存在", what, ref)
	}
	return fmt.Errorf("讀%s %s: %w", what, ref, err)
}

// isNoRows 判斷「查無此列」。
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
