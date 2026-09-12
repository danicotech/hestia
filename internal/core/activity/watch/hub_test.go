package watch_test

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/watch"
)

const slug = "2026-baiye-shifeng"

func matchUpdate(publicID string) watch.Update {
	return watch.Update{
		Tournament: slug,
		Kind:       watch.KindMatch,
		EmittedAt:  time.Now(),
		Match:      &match.Match{PublicID: publicID, Status: match.StatusLive},
	}
}

func TestPublishReachesAllSubscribersOfThatTournament(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(4)
	defer h.Close()

	a := h.Subscribe(slug)
	b := h.Subscribe(slug)
	other := h.Subscribe("2027-other")
	defer a.Close()
	defer b.Close()
	defer other.Close()

	h.Publish(matchUpdate("m1"))

	for i, s := range []*watch.Subscription{a, b} {
		select {
		case u := <-s.Updates():
			if u.Match.PublicID != "m1" {
				t.Fatalf("訂閱者 %d 收到 %q,要 m1", i, u.Match.PublicID)
			}
		default:
			t.Fatalf("訂閱者 %d 沒收到", i)
		}
	}
	select {
	case u := <-other.Updates():
		t.Fatalf("別屆賽事的訂閱者收到了 %+v", u)
	default:
	}
}

// 慢訂閱者不能拖垮快訂閱者 —— 這是整個 Hub 存在的理由。
func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(2)
	defer h.Close()

	slow := h.Subscribe(slug)
	fast := h.Subscribe(slug)
	defer slow.Close()
	defer fast.Close()

	// fast 邊收邊消化(每收一則就交棒),slow 一則都不讀。
	// 逐則交棒是刻意的:要測的是「slow 塞住之後 fast 仍然一則不漏」,
	// 不是「誰的緩衝比較大」。
	received := make(chan string)
	go func() {
		defer close(received)
		for u := range fast.Updates() {
			received <- u.Match.PublicID
		}
	}()

	const n = 50
	for i := 0; i < n; i++ {
		done := make(chan struct{})
		go func() { defer close(done); h.Publish(matchUpdate("m")) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 則 Publish 被慢訂閱者卡住 —— 那會讓整個推播停擺", i)
		}
		select {
		case id, ok := <-received:
			if !ok || id != "m" {
				t.Fatalf("快訂閱者在第 %d 則被中斷(err=%v)", i, fast.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("快訂閱者沒收到第 %d 則", i)
		}
	}

	// slow 應該已經被踢掉,而且說得出原因。
	if _, ok := <-drain(slow); ok {
		t.Fatal("慢訂閱者的 channel 應該已經關閉")
	}
	if !errors.Is(slow.Err(), watch.ErrLagged) {
		t.Fatalf("慢訂閱者的結束原因 = %v,要 ErrLagged", slow.Err())
	}
	if h.Lagged() == 0 {
		t.Fatal("Lagged() 應該記到這一次踢除")
	}
}

// drain 把還在緩衝裡的訊息讀完,回傳同一個 channel 供「是否已關閉」的判斷。
func drain(s *watch.Subscription) <-chan watch.Update {
	for range s.Updates() {
		_ = 0
	}
	return s.Updates()
}

func TestLaggedSubscriberIsRemovedFromIndex(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(1)
	defer h.Close()

	s := h.Subscribe(slug)
	defer s.Close()
	if h.Subscribers(slug) != 1 {
		t.Fatalf("訂閱數 = %d,要 1", h.Subscribers(slug))
	}
	h.Publish(matchUpdate("m1")) // 填滿緩衝
	h.Publish(matchUpdate("m2")) // 溢位 → 踢除
	if h.Subscribers(slug) != 0 {
		t.Fatalf("被踢掉的訂閱者還留在索引裡(%d)", h.Subscribers(slug))
	}
}

func TestCloseSubscriptionRemovesIndexEntry(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(4)
	defer h.Close()

	s := h.Subscribe(slug)
	s.Close()
	if h.Subscribers(slug) != 0 {
		t.Fatalf("取消訂閱後仍有 %d 個訂閱者", h.Subscribers(slug))
	}
	if err := s.Err(); err != nil {
		t.Fatalf("客戶端自己結束不該有錯誤,得到 %v", err)
	}
	// 重複 Close 不能 panic(handler 的 defer 與 Hub.Close 會撞在一起)。
	s.Close()
	s.Close()
	// 已取消的訂閱不該再收到東西,而且 Publish 不能 panic。
	h.Publish(matchUpdate("m1"))
	if _, ok := <-s.Updates(); ok {
		t.Fatal("取消訂閱後還收得到訊息")
	}
}

func TestCloseHubEndsEverySubscriber(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(4)
	subs := make([]*watch.Subscription, 0, 20)
	for i := 0; i < 10; i++ {
		subs = append(subs, h.Subscribe(slug), h.Subscribe("2027-other"))
	}
	h.Close()

	for i, s := range subs {
		select {
		case _, ok := <-s.Updates():
			if ok {
				t.Fatalf("訂閱者 %d 在關閉後還收到訊息", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("訂閱者 %d 沒有收到結束", i)
		}
		if !errors.Is(s.Err(), watch.ErrHubClosed) {
			t.Fatalf("訂閱者 %d 的結束原因 = %v,要 ErrHubClosed", i, s.Err())
		}
	}
	// 關閉後的操作全部是安全的 no-op。
	h.Close()
	h.Publish(matchUpdate("m1"))
	late := h.Subscribe(slug)
	if _, ok := <-late.Updates(); ok {
		t.Fatal("關閉後的訂閱應該立刻結束")
	}
	if !errors.Is(late.Err(), watch.ErrHubClosed) {
		t.Fatalf("關閉後訂閱的結束原因 = %v,要 ErrHubClosed", late.Err())
	}
	for _, s := range subs {
		s.Close() // handler 的 defer 會在 Hub 關閉之後才跑
	}
}

func TestSubscribeWithoutSlugEndsImmediately(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(4)
	defer h.Close()
	s := h.Subscribe("")
	if _, ok := <-s.Updates(); ok {
		t.Fatal("沒有 slug 的訂閱不該是活的")
	}
	h.Publish(watch.Update{Kind: watch.KindMatch}) // 沒有 slug 的變化直接丟掉
}

// 併發訂閱 / 取消 / 發布同時進行,不能 panic、不能死鎖、不能漏掉清理。
// 用 -count=5 跑(這台沒有 gcc,-race 跑不起來)。
func TestConcurrentSubscribeUnsubscribePublish(t *testing.T) {
	t.Parallel()
	h := watch.NewHub(8)
	defer h.Close()

	const workers = 24
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.Publish(matchUpdate("m"))
			}
		}
	}()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := h.Subscribe(slug)
				// 一半的訂閱者認真讀,另一半完全不讀(製造慢訂閱者)。
				if i%2 == 0 {
					select {
					case <-s.Updates():
					default:
					}
				}
				s.Close()
			}
		}(i)
	}

	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	select {
	case <-wgDone:
	case <-time.After(30 * time.Second):
		t.Fatal("併發訂閱/取消/發布死鎖")
	}
	if n := h.Subscribers(slug); n != 0 {
		t.Fatalf("全部取消後仍有 %d 個訂閱者", n)
	}
}

// 訂閱者結束之後不留 goroutine —— Hub 自己不開 goroutine,這條測的是
// 「這個性質沒有在某次改動中被破壞」。
func TestHubLeaksNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	h := watch.NewHub(4)
	for i := 0; i < 100; i++ {
		s := h.Subscribe(slug)
		h.Publish(matchUpdate("m"))
		s.Close()
	}
	h.Close()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutine 從 %d 長到 %d", before, after)
	}
}

// ── 信封 ────────────────────────────────────────────────────────

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()
	in := watch.Envelope{Tournament: slug, Kind: watch.KindHandicapLocked, Ref: "01J8MATCH"}
	payload, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// 鍵名是契約(SQL 側有鏡像),所以直接釘住字面形狀。
	if !strings.Contains(payload, `"t":`) || !strings.Contains(payload, `"k":`) || !strings.Contains(payload, `"r":`) {
		t.Fatalf("信封鍵名變了:%s", payload)
	}
	out, err := watch.ParseEnvelope(payload)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if out != in {
		t.Fatalf("round trip 不一致:%+v ≠ %+v", out, in)
	}
}

func TestEnvelopeRejectsIncomplete(t *testing.T) {
	t.Parallel()
	cases := map[string]watch.Envelope{
		"缺 slug":         {Kind: watch.KindMatch, Ref: "m1"},
		"缺 kind":         {Tournament: slug, Ref: "m1"},
		"缺 ref(非 phase)": {Tournament: slug, Kind: watch.KindMatch},
	}
	for name, e := range cases {
		if _, err := e.Marshal(); err == nil {
			t.Errorf("%s:應該被拒絕", name)
		}
	}
	// phase 不需要 ref:賽事本身已經由 slug 指定。
	if _, err := (watch.Envelope{Tournament: slug, Kind: watch.KindPhase}).Marshal(); err != nil {
		t.Errorf("phase 信封不該要求 ref:%v", err)
	}
	if _, err := watch.ParseEnvelope("not json"); err == nil {
		t.Error("壞掉的 payload 應該回錯")
	}
	if _, err := watch.ParseEnvelope(`{"t":"x"}`); err == nil {
		t.Error("缺 kind 的 payload 應該回錯")
	}
}

func TestEnvelopeRejectsOversized(t *testing.T) {
	t.Parallel()
	e := watch.Envelope{Tournament: strings.Repeat("x", watch.MaxEnvelopeBytes), Kind: watch.KindMatch, Ref: "m1"}
	if _, err := e.Marshal(); err == nil {
		t.Fatal("過長的信封必須被擋下 —— NOTIFY 失敗會讓整個交易失敗")
	}
}

// 每一個 match 的 outbox topic 都必須對得到一種推播,否則那個變化不會即時出現,
// 而且不會有任何錯誤訊息。
func TestEveryMatchTopicMapsToAKind(t *testing.T) {
	t.Parallel()
	payloads := map[string][]byte{
		match.TopicHandicapOpened: []byte(`{"tournament_slug":"` + slug + `","match_public_id":"m1"}`),
		match.TopicHandicapLocked: []byte(`{"tournament_slug":"` + slug + `","match_public_id":"m1"}`),
		match.TopicMatchStarted:   []byte(`{"tournament_slug":"` + slug + `","match_public_id":"m1"}`),
		match.TopicMatchFinished:  []byte(`{"tournament_slug":"` + slug + `","match_public_id":"m1"}`),
		match.TopicChampion:       []byte(`{"tournament_slug":"` + slug + `","final_match_public_id":"m9"}`),
	}
	for topic, payload := range payloads {
		e, ok := watch.EnvelopeForEvent(topic, payload)
		if !ok {
			t.Errorf("topic %s 沒有對應的推播種類", topic)
			continue
		}
		if e.Tournament != slug || e.Ref == "" {
			t.Errorf("topic %s 的信封不完整:%+v", topic, e)
		}
	}
	if e, ok := watch.EnvelopeForEvent(match.TopicChampion, payloads[match.TopicChampion]); !ok || e.Ref != "m9" {
		t.Errorf("冠軍那則要取 final_match_public_id,得到 %+v", e)
	}
	if e, ok := watch.EnvelopeForEvent(match.TopicHandicapLocked, payloads[match.TopicHandicapLocked]); !ok ||
		e.Kind != watch.KindHandicapLocked {
		t.Errorf("封盤要是 handicap_locked,得到 %+v", e)
	}
}

func TestEnvelopeForEventIgnoresUnrelated(t *testing.T) {
	t.Parallel()
	if _, ok := watch.EnvelopeForEvent("ledger.entry_created", []byte(`{"amount":1}`)); ok {
		t.Error("與戰況無關的 topic 不該產生信封")
	}
	if _, ok := watch.EnvelopeForEvent(match.TopicMatchStarted, []byte(`{`)); ok {
		t.Error("解不開的 payload 不該產生信封")
	}
	if _, ok := watch.EnvelopeForEvent(match.TopicMatchStarted, []byte(`{"match_public_id":"m1"}`)); ok {
		t.Error("沒有賽事 slug 的事件路由不出去,不該產生信封")
	}
}

// 冠軍那則的 payload 形狀由 match.ChampionEvent 決定,這裡確認真的餵得進去
// (欄位名改掉的話這條會紅,而不是等到活動當天冠軍橫幅不跳出來)。
func TestEnvelopeForRealChampionEvent(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(match.ChampionEvent{
		TournamentSlug: slug,
		Champion:       match.SideInfo{PlayerPublicID: "p1", DisplayName: "李璃", RankLevel: bp.Rank(4)},
		FinalMatchID:   "m9",
		TotalRounds:    3,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e, ok := watch.EnvelopeForEvent(match.TopicChampion, raw)
	if !ok || e.Kind != watch.KindChampion || e.Ref != "m9" || e.Tournament != slug {
		t.Fatalf("冠軍事件的信封 = %+v ok=%v", e, ok)
	}
}
