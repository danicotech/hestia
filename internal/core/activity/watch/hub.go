package watch

import (
	"errors"
	"sync"
	"sync/atomic"
)

// DefaultBuffer 是每個訂閱者的緩衝長度。
//
// 為什麼是 64 而不是 1:一場比賽判定勝負會在同一個交易裡連發賽果、晉級、
// 冠軍,加上緊接著的投票變動,瞬間好幾則是常態。緩衝 1 會讓一個剛好在
// 寫 HTTP frame 的正常訂閱者被誤判成慢訂閱者。
//
// 為什麼不是 4096:緩衝的作用是吸收突波,不是掩蓋一個接不動的訂閱者。
// 緩衝越大,「這個人已經落後很久了」就越晚被發現,而落後的訂閱者看到的是
// 一串過期的變化 —— 對即時畫面來說,那比直接叫他重連更糟。
const DefaultBuffer = 64

var (
	// ErrLagged 表示這個訂閱者跟不上,已經被踢掉。
	//
	// 這不是內部錯誤,是一個要**告訴前端**的狀態:它該重連,然後重新拉一次
	// 完整狀態(watch.proto 檔頭的三步)。默默丟訊息不告訴它的話,它的畫面會
	// 停在某個中間狀態,而且看起來完全正常。
	ErrLagged = errors.New("訂閱者跟不上推播速度,已中斷;請重連並重新取得完整狀態")
	// ErrHubClosed 表示伺服器正在關閉。
	ErrHubClosed = errors.New("推播服務正在關閉")
)

// 訂閱結束的原因。用整數而不是存 error:結束原因會被發布端(持鎖)寫、
// 被 handler(不持鎖)讀,atomic 是這裡唯一不必為了讀一個常數而再拿一次鎖的做法。
const (
	reasonClient = iota // 客戶端自己結束(或 context 取消)
	reasonLagged
	reasonHubClosed
)

// Hub 依賽事 slug 把變化扇出給訂閱者。
//
// # 慢訂閱者絕不拖垮任何人
//
// 發布端跑在 watchpg 的 LISTEN 迴圈裡 —— 那是**單一一條** goroutine,
// 而且它同時服務所有賽事的所有訂閱者。在那裡阻塞一下,代價不是「這個人慢一點」,
// 而是整個推播停擺,所有觀眾的畫面一起凍住。
//
// 所以 Publish 對每個訂閱者只做一次非阻塞送出:送得進去就送,送不進去就
// 當場把那個訂閱者踢掉(關閉它的 channel,原因記為 ErrLagged)。
// 沒有任何一條路徑會讓 Publish 等待訂閱者。
//
// 為什麼是「踢掉」而不是「丟掉這一則」:這條流送的是狀態,不是事件日誌。
// 丟掉中間幾則的訂閱者會帶著一個永遠補不回來的缺口繼續跑,畫面看起來正常
// 但內容是錯的;踢掉它則會讓前端重連並重新拉一次完整狀態,回到正確。
//
// # 沒有 goroutine
//
// Hub 自己不開任何 goroutine:訂閱者的生命週期由它自己的 handler 用
// select + ctx.Done() 管。這讓「洩漏 goroutine」在結構上不可能發生 ——
// 沒有東西可以洩漏。
type Hub struct {
	mu     sync.Mutex
	subs   map[string]map[*Subscription]struct{}
	closed bool
	buffer int

	// lagged 是被踢掉的訂閱者累計數。它是這個系統唯一看得出「推播端開始
	// 跟不上」的訊號,所以要導出讓 metrics / 測試讀得到。
	lagged atomic.Int64
}

// NewHub 建立樞紐。buffer <= 0 時用 DefaultBuffer。
func NewHub(buffer int) *Hub {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Hub{
		subs:   make(map[string]map[*Subscription]struct{}),
		buffer: buffer,
	}
}

// Subscription 是一個訂閱者。由 Subscribe 建立,handler 負責 Close。
type Subscription struct {
	hub        *Hub
	tournament string
	ch         chan Update

	// once 保證 channel 只會被關一次。關閉的來源有三個(客戶端結束、
	// 跟不上被踢、Hub 關閉),而重複關閉 channel 是 panic。
	once   sync.Once
	reason atomic.Int32
}

// Updates 是變化的來源。channel 關閉即訂閱結束,原因用 Err 問。
func (s *Subscription) Updates() <-chan Update { return s.ch }

// Err 回報訂閱為什麼結束。**在 channel 關閉之後才有意義。**
//
//	nil           客戶端自己結束的(正常)
//	ErrLagged     跟不上,被踢掉 —— 前端該重連並重新拉完整狀態
//	ErrHubClosed  伺服器關閉中
func (s *Subscription) Err() error {
	switch s.reason.Load() {
	case reasonLagged:
		return ErrLagged
	case reasonHubClosed:
		return ErrHubClosed
	default:
		return nil
	}
}

// Close 取消訂閱。可重複呼叫(handler 的 defer 與 Hub 的關閉會撞在一起)。
func (s *Subscription) Close() {
	s.hub.remove(s, reasonClient)
}

// finish 關閉 channel 並記下原因。**必須在持有 hub.mu 時呼叫** ——
// Publish 也在鎖內送訊息,兩者由同一把鎖排開,「送到已關閉的 channel」
// 因此在結構上不可能發生(那會 panic,而且是在推播迴圈裡 panic)。
func (s *Subscription) finish(reason int32) {
	s.once.Do(func() {
		s.reason.Store(reason)
		close(s.ch)
	})
}

// Subscribe 訂閱一屆賽事。
//
// Hub 已關閉時回傳一個**已經結束**的訂閱(channel 已關、Err 是 ErrHubClosed),
// 而不是 nil 或 error:呼叫端的迴圈長得一模一樣,關機期間進來的請求會立刻
// 拿到一個乾淨的結束,不必為這個情境寫第二條路徑。
func (h *Hub) Subscribe(tournamentSlug string) *Subscription {
	s := &Subscription{
		hub:        h,
		tournament: tournamentSlug,
		ch:         make(chan Update, h.buffer),
	}
	if tournamentSlug == "" {
		// 沒有 slug 的訂閱永遠收不到東西(Publish 依 slug 分組),留著它只會是
		// 一條假裝活著的連線。handler 本來就會先擋掉空 slug,這是第二道。
		s.finish(reasonClient)
		return s
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		s.finish(reasonHubClosed)
		return s
	}
	if h.subs[tournamentSlug] == nil {
		h.subs[tournamentSlug] = make(map[*Subscription]struct{})
	}
	h.subs[tournamentSlug][s] = struct{}{}
	return s
}

// remove 把訂閱從索引裡拿掉並結束它。
func (h *Hub) remove(s *Subscription, reason int32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.detachLocked(s)
	s.finish(reason)
}

// detachLocked 只從索引移除,不關 channel。呼叫端必須持有 h.mu。
func (h *Hub) detachLocked(s *Subscription) {
	set := h.subs[s.tournament]
	if set == nil {
		return
	}
	delete(set, s)
	if len(set) == 0 {
		// 空的 map 留著就是一個依賽事 slug 無限成長的洩漏
		// (slug 由請求帶進來,誰都能訂閱一個不存在的賽事)。
		delete(h.subs, s.tournament)
	}
}

// Publish 把一則變化送給該屆的所有訂閱者。
//
// **這個方法永遠不阻塞**,也永遠不回錯:它跑在 LISTEN 迴圈裡,而那條迴圈
// 停一秒就是所有觀眾的畫面停一秒。送不進去的訂閱者當場被踢掉(見 Hub 的說明)。
//
// Hub 已關閉或沒有人訂閱這一屆時直接返回 —— 沒有訂閱者的賽事是常態
// (裁判在後台操作、還沒有人打開頁面)。
func (h *Hub) Publish(u Update) {
	if u.Tournament == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	var lagged []*Subscription
	for s := range h.subs[u.Tournament] {
		select {
		case s.ch <- u:
		default:
			// 緩衝滿了。在這裡等 = 整個推播停擺,所以踢掉它。
			lagged = append(lagged, s)
		}
	}
	for _, s := range lagged {
		h.detachLocked(s)
		s.finish(reasonLagged)
	}
	h.lagged.Add(int64(len(lagged)))
}

// Close 結束所有訂閱。可重複呼叫;之後的 Publish 是 no-op,
// 之後的 Subscribe 會拿到一個已結束的訂閱。
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, set := range h.subs {
		for s := range set {
			s.finish(reasonHubClosed)
		}
	}
	// 整張表換掉而不是逐一 delete:已經結束的訂閱不需要再被任何東西參照。
	h.subs = make(map[string]map[*Subscription]struct{})
}

// Subscribers 回報某一屆目前的訂閱數。給 metrics 與測試用。
func (h *Hub) Subscribers(tournamentSlug string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[tournamentSlug])
}

// Lagged 是累計被踢掉的訂閱者數。持續上升代表推播端送得比訂閱者收得快,
// 那是「該調緩衝」或「該查為什麼 handler 寫得這麼慢」的訊號。
func (h *Hub) Lagged() int64 { return h.lagged.Load() }
