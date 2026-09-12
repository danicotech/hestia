package watchpg_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/bettingpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/watchpg"
)

// ══ 這一套方案的唯一理由 ════════════════════════════════════════

// TestNotifyOnlyOnCommit 是本套件最重要的一支測試。
//
// 選 LISTEN / NOTIFY 而不是行程內 hub,理由只有一個:**通知只在 commit 時送出**。
// 沒有這一條,觀眾就可能看到一個後來被 rollback 的賽果 —— 而那個 bug 在
// 行程內 hub 的版本裡只需要把 Publish 寫在 commit 之前就會發生。
//
// 三段依序驗:
//  1. 交易還開著的時候,線上一片安靜(不是「比較晚到」,是根本還沒送)。
//  2. commit 之後,通知到了。
//  3. rollback 的交易,通知永遠不會到 —— 而且連線是好的(下一次 commit 收得到)。
func TestNotifyOnlyOnCommit(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusReady)
	raw := newRawListener(t)
	ctx := context.Background()

	// ── 1. 交易未 commit:什麼都沒送 ──
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 transaction: %v", err)
	}
	if err := f.repo.AppendEvents(ctx, tx, []match.Event{{
		Topic: match.TopicMatchStarted, Payload: f.eventPayload(match.TopicMatchStarted, m),
	}}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	raw.expectSilence(700*time.Millisecond, "交易還沒 commit,通知不該送出")

	// ── 2. commit 之後才送 ──
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	e, ok := raw.next(5 * time.Second)
	if !ok {
		t.Fatal("commit 之後應該收得到通知")
	}
	if e.Tournament != f.slug || e.Kind != watch.KindMatch || e.Ref != m.publicID {
		t.Fatalf("信封 = %+v,要 {%s match %s}", e, f.slug, m.publicID)
	}

	// ── 3. rollback 的交易永遠不送 ──
	f.appendEvent(match.TopicMatchFinished, m, false)
	raw.expectSilence(700*time.Millisecond, "rollback 的交易不該送出通知")

	// 連線還是好的 —— 不然上面那句「沒收到」根本不能當證據。
	f.appendEvent(match.TopicMatchFinished, m, true)
	if _, ok := raw.next(5 * time.Second); !ok {
		t.Fatal("rollback 測試之後,正常的 commit 仍然要收得到通知")
	}
}

// TestVoteNotifyOnlyOnCommit 對投票路徑驗同一件事。
//
// 投票不寫 outbox,走的是另一支 query(NotifyWatchOdds,信封由 SQL 組),
// 所以 commit 語意要分開驗一次 —— 兩條路徑共用的只有「在同一個 tx 裡」這個約定,
// 而約定不會自己成立。
//
// 同時它也是 **SQL 與 Go 的信封鏡像**那條契約的守門:raw.next 會用
// watch.ParseEnvelope 解 SQL 組出來的 JSON,鍵名對不上就直接 fail。
func TestVoteNotifyOnlyOnCommit(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusReady)
	raw := newRawListener(t)
	ctx := context.Background()
	repo := bettingpg.NewRepository(pool)

	// rollback 的投票不送通知。
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 transaction: %v", err)
	}
	if err := repo.UpsertVote(ctx, tx, m.id, 1001, betting.SideP1); err != nil {
		t.Fatalf("UpsertVote: %v", err)
	}
	raw.expectSilence(700*time.Millisecond, "投票的交易還沒 commit,通知不該送出")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	raw.expectSilence(700*time.Millisecond, "rollback 的投票不該送出通知")

	// commit 的投票會送,而且信封的三個鍵 Go 這邊解得開。
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 transaction: %v", err)
	}
	if err := repo.UpsertVote(ctx, tx2, m.id, 1001, betting.SideP1); err != nil {
		t.Fatalf("UpsertVote: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	e, ok := raw.next(5 * time.Second)
	if !ok {
		t.Fatal("投票 commit 之後應該收得到通知")
	}
	if e.Tournament != f.slug || e.Kind != watch.KindOdds || e.Ref != m.publicID {
		t.Fatalf("投票信封 = %+v,要 {%s odds %s}", e, f.slug, m.publicID)
	}
}

// ══ Listener:信封 → 完整內容 → Hub ══════════════════════════════

// startListener 起一條 LISTEN 迴圈,回傳 Hub 與「已經在聽了」的等待函式。
func startListener(t *testing.T) (*watch.Hub, func(int)) {
	t.Helper()
	hub := watch.NewHub(16)
	listens := make(chan struct{}, 8)
	l := watchpg.New(pool, hub, watchpg.Options{
		// 測試不需要看 log,但真的壞掉時想看得到 —— 用 Discard 而不是 nil,
		// 避免一支測試的告警混進另一支的輸出。
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond,
		OnListen:   func() { listens <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run 回了錯誤:%v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("取消 context 之後 Run 沒有結束")
		}
		hub.Close()
	})

	// waitListening 等第 n 次(重新)建立 LISTEN。
	// 沒有它,測試會踩到「NOTIFY 早於 LISTEN」而偶發地綠 —— 那比紅更糟。
	return hub, func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-listens:
			case <-time.After(20 * time.Second):
				t.Fatalf("等不到第 %d 次 LISTEN", i+1)
			}
		}
	}
}

// recv 等一則推播。
func recv(t *testing.T, sub *watch.Subscription, timeout time.Duration) watch.Update {
	t.Helper()
	select {
	case u, ok := <-sub.Updates():
		if !ok {
			t.Fatalf("訂閱被中斷:%v", sub.Err())
		}
		return u
	case <-time.After(timeout):
		t.Fatal("等不到推播")
		return watch.Update{}
	}
}

// TestListenerPushesMatchUpdate 走完整條路:裁判動作 → NOTIFY → 讀完整狀態 → Hub。
func TestListenerPushesMatchUpdate(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusLive)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	f.appendEvent(match.TopicMatchStarted, m, true)

	u := recv(t, sub, 10*time.Second)
	if u.Kind != watch.KindMatch || u.Match == nil {
		t.Fatalf("推播 = %+v,要一則場次狀態", u)
	}
	if u.Match.PublicID != m.publicID {
		t.Fatalf("場次 = %s,要 %s", u.Match.PublicID, m.publicID)
	}
	// 信封只帶 id,完整狀態是收到之後讀出來的 —— 這幾個欄位證明它真的讀了。
	if u.Match.Status != match.StatusLive {
		t.Errorf("狀態 = %s,要 live", u.Match.Status)
	}
	if u.Match.P1.PublicID != a.publicID || u.Match.P2.PublicID != b.publicID {
		t.Errorf("雙方 = %s / %s,要 %s / %s",
			u.Match.P1.PublicID, u.Match.P2.PublicID, a.publicID, b.publicID)
	}
	if u.Tournament != f.slug {
		t.Errorf("賽事 = %s,要 %s", u.Tournament, f.slug)
	}
	if u.EmittedAt.IsZero() {
		t.Error("EmittedAt 沒填")
	}
}

// TestListenerPushesHandicapReveal 驗封盤那一則帶得出完整清單。
//
// 這是唯一一則「內容在這一刻才變公開」的變化,也是唯一一則會超過 NOTIFY
// 8000 bytes 上限的內容 —— 所以「信封 + 收到後重讀」的設計必須在這裡成立。
func TestListenerPushesHandicapReveal(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusReady)
	f.lockHandicaps(m, a)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	f.appendEvent(match.TopicHandicapLocked, m, true)

	u := recv(t, sub, 10*time.Second)
	if u.Kind != watch.KindHandicapLocked || u.Handicaps == nil {
		t.Fatalf("推播 = %+v,要一則封盤公示", u)
	}
	h := u.Handicaps
	if !h.Revealed {
		t.Error("封盤那則必須是已公開")
	}
	if len(h.Selections) != 2 {
		t.Fatalf("讓武項目 = %d 項,要 2 項", len(h.Selections))
	}
	if h.Selections[0].ItemName == "" || h.Selections[0].TargetNote == "" {
		t.Errorf("項目快照不完整:%+v", h.Selections[0])
	}
	if h.HolderPlayerPublicID != a.publicID {
		t.Errorf("施加者 = %s,要 %s", h.HolderPlayerPublicID, a.publicID)
	}
	if h.ConstrainedPlayerPublicID != b.publicID {
		t.Errorf("受限方 = %s,要 %s", h.ConstrainedPlayerPublicID, b.publicID)
	}
	if h.Budget == nil || h.Budget.Budget != 16 || h.Budget.Spent != 16 {
		t.Errorf("預算 = %+v,要 16/16", h.Budget)
	}
	if h.LockedAt == nil {
		t.Error("封盤時間沒帶出來")
	}
}

// TestListenerPushesOdds 驗票數變動推出來的賠率是算出來的,不是讀出來的。
func TestListenerPushesOdds(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusReady)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	ctx := context.Background()
	repo := bettingpg.NewRepository(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 transaction: %v", err)
	}
	for _, uid := range []int64{2001, 2002, 2003} {
		if err := repo.UpsertVote(ctx, tx, m.id, uid, betting.SideP1); err != nil {
			t.Fatalf("UpsertVote: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	u := recv(t, sub, 10*time.Second)
	if u.Kind != watch.KindOdds || u.Odds == nil {
		t.Fatalf("推播 = %+v,要一則賠率變動", u)
	}
	o := u.Odds
	if o.P1Votes != 3 || o.P2Votes != 0 {
		t.Errorf("票數 = %d / %d,要 3 / 0", o.P1Votes, o.P2Votes)
	}
	// 三票全押 p1:p1 的賠率必須比 p2 低(賠率是機率的倒數)。
	if o.P1OddsMilli >= o.P2OddsMilli {
		t.Errorf("賠率 = %d / %d,一面倒那邊應該比較低", o.P1OddsMilli, o.P2OddsMilli)
	}
	if o.P1OddsMilli <= betting.OddsScale {
		t.Errorf("賠率 %d 不合法(必須 > 1.0)", o.P1OddsMilli)
	}
	// 廣播不可能因人而異 —— 這個欄位在推播裡永遠是空的。
	if o.MyVote != betting.SideNone {
		t.Errorf("推播不該帶 MyVote,得到 %v", o.MyVote)
	}
	if !o.OpenForBets {
		t.Error("ready 的場次應該還能下注")
	}
}

// TestListenerPushesChampion 驗冠軍那則讀得出顯示名與段位(橫幅要用)。
func TestListenerPushesChampion(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 4), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusLive)
	f.finish(m, a)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	f.appendEvent(match.TopicChampion, m, true)

	u := recv(t, sub, 10*time.Second)
	if u.Kind != watch.KindChampion || u.Champion == nil {
		t.Fatalf("推播 = %+v,要一則冠軍", u)
	}
	c := u.Champion
	if c.PlayerPublicID != a.publicID || c.DisplayName != "李璃" {
		t.Errorf("冠軍 = %s/%s,要 %s/李璃", c.PlayerPublicID, c.DisplayName, a.publicID)
	}
	if int(c.Rank) != 4 {
		t.Errorf("冠軍段位 = %d,要 4", c.Rank)
	}
	if c.RunnerUpPublicID != b.publicID {
		t.Errorf("亞軍 = %s,要 %s", c.RunnerUpPublicID, b.publicID)
	}
	if c.FinalMatchPublicID != m.publicID {
		t.Errorf("決賽 = %s,要 %s", c.FinalMatchPublicID, m.publicID)
	}
}

// TestListenerPushesPhase 驗階段那一則。
//
// 目前**沒有任何程式會送出這個信封**:階段推進寫在 tournamentpg,那不在本次
// 的檔案範圍內(見回報)。這支測試證明的是「信封一旦送出,收端讀得出正確階段」
// —— 發送端補上時只要加一行,不必動本套件。
func TestListenerPushesPhase(t *testing.T) {
	f := newFixture(t)
	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	f.notifyEnvelope(watch.Envelope{Tournament: f.slug, Kind: watch.KindPhase})

	u := recv(t, sub, 10*time.Second)
	if u.Kind != watch.KindPhase {
		t.Fatalf("推播 = %+v,要一則階段變化", u)
	}
	if u.Phase != tournament.PhaseInProgress {
		t.Errorf("階段 = %s,要 in_progress", u.Phase)
	}
}

// TestOnlySubscribedTournamentsAreResolved 驗沒有人在看的賽事不會白讀資料庫,
// 而且別屆的變化不會漏進來。
func TestOnlySubscribedTournamentsAreResolved(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusLive)
	oa, ob := other.player("甲", 1), other.player("乙", 2)
	om := other.match(1, 0, oa, ob, match.StatusLive)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	// 先送別屆的(沒有人訂閱),再送自己的。
	other.appendEvent(match.TopicMatchStarted, om, true)
	f.appendEvent(match.TopicMatchStarted, m, true)

	u := recv(t, sub, 10*time.Second)
	if u.Match == nil || u.Match.PublicID != m.publicID {
		t.Fatalf("收到的是別屆的變化:%+v", u)
	}
	select {
	case extra := <-sub.Updates():
		t.Fatalf("不該再有第二則:%+v", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestBadNotificationDoesNotStopTheLoop 驗一則壞掉的通知不會關掉整個推播。
//
// 這條迴圈服務所有觀眾:為了一筆讀不出來的資料而中斷它,等於讓一個已刪除的
// 場次把全場的即時畫面一起關掉。
func TestBadNotificationDoesNotStopTheLoop(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusLive)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	ctx := context.Background()
	// 1. 完全不是 JSON。
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, watch.Channel, "not json"); err != nil {
		t.Fatalf("送壞掉的通知: %v", err)
	}
	// 2. 合法信封但指向不存在的場次。
	f.notifyEnvelope(watch.Envelope{Tournament: f.slug, Kind: watch.KindMatch, Ref: "01JDOESNOTEXIST"})
	// 3. 認不得的種類。
	f.notifyEnvelope(watch.Envelope{Tournament: f.slug, Kind: "from_the_future", Ref: m.publicID})
	// 4. 正常的一則 —— 迴圈還活著的話,只有這一則會到。
	f.appendEvent(match.TopicMatchStarted, m, true)

	u := recv(t, sub, 10*time.Second)
	if u.Match == nil || u.Match.PublicID != m.publicID {
		t.Fatalf("推播 = %+v,要那一場正常的", u)
	}
}

// TestListenerReconnects 驗連線被砍掉之後會自己回來。
//
// 這是斷線處理唯一有意義的測法:真的把後端連線 terminate 掉,然後要求
// 「重新 LISTEN 之後,新的通知還是收得到」。斷線期間的通知會遺失,那是
// 可接受的(前端重連會先拉完整狀態),但**恢復**不是可選的。
func TestListenerReconnects(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusLive)

	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	// 砍掉那條正在 LISTEN 的後端連線。
	ctx := context.Background()
	var killed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int FROM (
			SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND query LIKE 'LISTEN %'
		) t`).Scan(&killed); err != nil {
		t.Fatalf("砍連線: %v", err)
	}
	if killed == 0 {
		t.Fatal("找不到正在 LISTEN 的連線,測試前提不成立")
	}

	// 第二次 LISTEN = 重連成功。
	waitListening(1)

	f.appendEvent(match.TopicMatchStarted, m, true)
	u := recv(t, sub, 10*time.Second)
	if u.Match == nil || u.Match.PublicID != m.publicID {
		t.Fatalf("重連之後收到 %+v,要那一場", u)
	}
}

// TestRunReturnsOnContextCancel 驗優雅關閉:ctx 一取消就結束,而且不回錯誤。
func TestRunReturnsOnContextCancel(t *testing.T) {
	setup(t)
	hub := watch.NewHub(4)
	defer hub.Close()
	l := watchpg.New(pool, hub, watchpg.Options{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	time.Sleep(300 * time.Millisecond) // 讓它真的連上
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("正常關機不該回錯誤,得到 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消 context 之後 Run 沒有結束")
	}
}

// TestRunKeepsRetryingWhenDatabaseIsUnreachable 驗連不上時不會直接放棄。
//
// 資料庫重啟是會發生的事。Run 在那段期間必須持續重試,而不是回一個錯誤
// 然後把推播永遠關掉 —— 沒有人會注意到一個「已經結束的背景 goroutine」。
func TestRunKeepsRetryingWhenDatabaseIsUnreachable(t *testing.T) {
	setup(t)
	hub := watch.NewHub(4)
	defer hub.Close()

	badPool := newBadPool(t)
	defer badPool.Close()

	l := watchpg.New(badPool, hub, watchpg.Options{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("連不上資料庫時 Run 不該結束,得到 %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("取消之後不該回錯誤,得到 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消 context 之後 Run 沒有結束")
	}
}

// TestSlowSubscriberDoesNotStallTheListener 驗一個不讀訊息的訂閱者不會讓
// 其他觀眾停下來 —— Hub 的性質在真的 LISTEN 迴圈底下仍然成立。
func TestSlowSubscriberDoesNotStallTheListener(t *testing.T) {
	f := newFixture(t)
	a, b := f.player("李璃", 1), f.player("A冷", 3)
	m := f.match(1, 0, a, b, match.StatusLive)

	hub, waitListening := startListener(t)
	waitListening(1)
	slow := hub.Subscribe(f.slug) // 完全不讀
	fast := hub.Subscribe(f.slug)
	defer slow.Close()
	defer fast.Close()

	f.appendEvent(match.TopicMatchStarted, m, true)
	u := recv(t, fast, 10*time.Second)
	if u.Match == nil {
		t.Fatalf("快訂閱者沒收到場次變化:%+v", u)
	}
	_ = slow // 慢訂閱者的命運由 Hub 的單元測試釘住,這裡只要求它不影響 fast
}

// 確認錯誤沒有被吞:notFound 的訊息會經過 log,但 resolve 的失敗不該讓
// Publish 發出一則空的 Update(那會讓前端把畫面清空)。
func TestMissingMatchPublishesNothing(t *testing.T) {
	f := newFixture(t)
	hub, waitListening := startListener(t)
	waitListening(1)
	sub := hub.Subscribe(f.slug)
	defer sub.Close()

	f.notifyEnvelope(watch.Envelope{Tournament: f.slug, Kind: watch.KindMatch, Ref: "01JGONE"})
	select {
	case u := <-sub.Updates():
		t.Fatalf("讀不出內容時不該推播,得到 %+v", u)
	case <-time.After(2 * time.Second):
	}
}

// newBadPool 建一個指向「沒有人在聽的埠」的 pool。
//
// pgxpool 是懶連線的:建立時不會撥號,所以這裡拿得到一個看起來正常、
// 但每次撥號都會失敗的 pool —— 那正是「資料庫掛了」對 Listener 的樣子。
func newBadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Port = 1
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("建壞掉的 pool: %v", err)
	}
	return p
}
