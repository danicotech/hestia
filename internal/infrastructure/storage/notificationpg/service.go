// Package notificationpg 是通知拉取的 Postgres 實作:outbox_events → Discord 閘道。
//
// 定位:hestia 的 outbox 原本只有 in-process 消費者(infrastructure/outbox),
// 沒有對外通道,所以 stentor 的公告消費端做好了也接不上東西。
// 這個 package 補上那條通道,而且**不另造佇列** —— 認領走的是 outbox 既有的
// `FOR UPDATE SKIP LOCKED`,可見性逾時重用既有的 next_retry_at,
// 重試次數重用既有的 attempts,毒訊息終態重用既有的 status='failed'。
//
// ## 投遞語意:at-least-once
//
//	Pull  → 認領一批(next_retry_at 推到 now()+visibility),**不改 status**
//	Ack   → status='done'
//	沒 Ack → 逾時後自動重新可見,重來
//
// 重放是正常現象,不是錯誤。閘道用 event_id 去重(stentor 已實作)。
//
// ## 這個 package 絕不做的事
//
//   - **不碰 Discord 清單以外的 topic。** 每個查詢都帶 topic 白名單參數,
//     連 Ack 都要比對 —— 沒有它,閘道猜一串 id 就能把內部事件標成已完成。
//   - **不把 payload 原文交出去。** 出口只有 notification.Render 的產物;
//     內部 BIGINT id、手續費、處理者是誰都留在這個進程裡。
//   - **不寫任何領域資料。** 唯一的寫入是 outbox_events 自己的投遞狀態。
package notificationpg

import (
	"context"
	"crypto/cipher"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 批次與可見性的預設值與上限。
//
// 上限存在的理由與 readpg 的 DefaultRowLimit 相同:沒有上限的佇列拉取
// 遲早會有人送 max=10_000_000,然後一次把整張表讀進記憶體。
// 超過上限**夾到上限**而不是報錯:呼叫端要的是「盡量多給我一點」,
// 為此讓它整個請求失敗沒有好處。
const (
	DefaultBatchSize = 20
	MaxBatchSize     = 200

	DefaultVisibility = 30 * time.Second
	MaxVisibility     = 10 * time.Minute

	// MaxAckIDs 是一次 Ack 的 id 數上限。超過就是錯誤(不夾)——
	// 那是呼叫端的 bug,而不是「盡量多確認一點」的合理請求。
	MaxAckIDs = 1000

	// DefaultMaxAttempts 是毒訊息門檻:認領到第幾次就放棄。
	//
	// 數值與 outbox.Consumer 的預設值相同,但**刻意是各自的常數**:
	// 兩條投遞路徑處理的是互斥的 topic(見 notification.AssertNoOverlap),
	// 重試節奏也不同(那邊是指數退避,這邊是固定的可見性逾時)。
	// 共用一個常數會讓「調整閘道的重試耐性」意外改到 in-process 消費者。
	DefaultMaxAttempts = 8
)

// Service 是通知拉取服務。
type Service struct {
	// 刻意只留 *db.Queries,不留 pool:本 package 沒有任何跨語句的一致性需求
	// (認領那一句自己就是原子的),留著 pool 只會引誘後人開一個不必要的
	// transaction 把連線佔到渲染跑完。
	q           *db.Queries
	topics      []string
	maxAttempts int32
	// eventID 是對外識別子的金鑰化置換(見 eventid.go)。
	// 對外絕不出現內部 BIGINT id,而且沒有它就發不出合法的 event_id ——
	// 「Ack 一個自己從未認領過的事件」因此不可能。
	eventID cipher.Block
	log     *slog.Logger
}

// Option 調整 Service 的行為。
type Option func(*Service)

// WithMaxAttempts 覆寫毒訊息門檻(<= 0 視為不設定)。
func WithMaxAttempts(n int32) Option {
	return func(s *Service) {
		if n > 0 {
			s.maxAttempts = n
		}
	}
}

// WithLogger 覆寫記錄器。
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.log = l
		}
	}
}

// WithEventIDKey 指定對外識別子的金鑰。
//
// **正式部署請一定要給。** 沒給的話每次啟動都是新的一把,重啟後尚未 Ack 的
// 公告會拿到新的 event_id,而 stentor 的去重是記憶體裡的一份 event_id 集合
// —— 換句話說,那幾則會被重貼一次。給了固定金鑰就沒有這個窗口。
//
// 金鑰只用來算識別子,不保護任何資料;輪替它的代價就是上面那個重貼窗口。
//
// 收 EventIDKey 而不是 []byte 是刻意的:長度不對是**設定錯誤**,必須有人
// 看到 error,而 Option 沒有回傳值。驗證因此提前到 NewEventIDKey ——
// 設定錯誤在組裝端就炸,不會靜靜地降級成隨機金鑰。
func WithEventIDKey(k EventIDKey) Option {
	return func(s *Service) {
		if k.block != nil {
			s.eventID = k.block
		}
	}
}

// New 建立通知拉取服務。
//
// topic 清單來自 core(notification.DiscordTopics),**不是**建構參數:
// 「哪些事件給閘道」是一個決定,只能有一個位置。讓組裝端傳清單進來,
// 就等於允許某個部署偷偷多給幾個 topic。
func New(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{
		q:           db.New(pool),
		topics:      notification.DiscordTopics(),
		maxAttempts: DefaultMaxAttempts,
		log:         slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.eventID == nil {
		// 沒設定金鑰:產一把行程內的,並且**出聲**。
		// 靜靜地用隨機金鑰,症狀會是「hestia 重啟後偶爾重貼一則公告」——
		// 那是最難查的一種問題,因為它看起來像 at-least-once 的正常抖動。
		s.eventID = randomEventIDCipher()
		s.log.Warn("通知拉取:未設定 event_id 金鑰,使用行程內隨機金鑰;" +
			"hestia 重啟後尚未確認的公告可能被重貼一次(用 WithEventIDKey 指定固定金鑰)")
	}
	return s
}

// Pull 認領一批待送的公告。
//
// 流程:毒訊息終止 → 認領 → 依 id 排序 → 批次查名稱 → 渲染。
//
// 為什麼不包在一個 transaction 裡:認領那一句自己就是原子的
// (CTE 取鎖 + UPDATE 同一句),而查名稱與渲染是純讀取/純計算。
// 開一個橫跨渲染的 transaction 只會讓連線被多佔住幾毫秒,
// 換不到任何一致性 —— 名稱本來就是「查的當下那一刻」的值。
func (s *Service) Pull(ctx context.Context, max int32, visibility time.Duration) ([]notification.Announcement, error) {
	limit := clampInt32(max, DefaultBatchSize, MaxBatchSize)
	vis := clampDuration(visibility, DefaultVisibility, MaxVisibility)

	// 先讓用完重試次數的事件離開待送佇列。放在認領**之前**是刻意的:
	// 否則它們會一直落在認領查詢的掃描範圍裡(雖然被 attempts 條件濾掉),
	// 而且永遠不會離開 pending —— 積壓量只增不減,清理 job 也刪不到。
	if _, err := s.q.FailExhaustedAnnouncements(ctx, db.FailExhaustedAnnouncementsParams{
		Topics:      s.topics,
		MaxAttempts: s.maxAttempts,
		RowLimit:    MaxBatchSize,
	}); err != nil {
		return nil, fmt.Errorf("終止逾重試上限的公告事件: %w", err)
	}

	rows, err := s.q.ClaimAnnouncements(ctx, db.ClaimAnnouncementsParams{
		Topics:      s.topics,
		MaxAttempts: s.maxAttempts,
		// 無條件進位到秒:欄位是 TIMESTAMPTZ,但 make_interval 收的是整數秒。
		// 截斷的話,半秒的可見性會變成 0 秒 —— 那等於沒有可見性,
		// 認領完立刻又被下一次 Pull 拿走,同一則公告會連貼兩次。
		VisibilitySeconds: int32((vis + time.Second - 1) / time.Second),
		RowLimit:          limit,
	})
	if err != nil {
		return nil, fmt.Errorf("認領公告事件: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// UPDATE ... RETURNING 不保證順序;舊事件要先貼。
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	events := make([]notification.Event, 0, len(rows))
	for _, r := range rows {
		events = append(events, notification.Event{
			// 對外識別子是金鑰化的不透明值,不是內部 id(鐵則 5),
			// 而且只有「認領」這個動作發得出來(見 eventid.go)。
			ID:      mintEventID(s.eventID, r.ID),
			Topic:   r.Topic,
			Payload: r.Payload,
		})
	}

	names, err := s.resolveNames(ctx, events)
	if err != nil {
		return nil, err
	}

	channels := s.resolveChannels(ctx, events)

	out := make([]notification.Announcement, 0, len(events))
	for _, ev := range events {
		a, err := notification.Render(ev, names)
		if err != nil {
			// 渲染不出來的事件**不回傳也不 Ack**:它留在佇列裡,
			// 可見性逾時後重來,最終由 attempts 判成毒訊息。
			// 這與 outbox.Consumer 對壞事件的處置一致 —— 壞掉的一筆
			// 不能決定整批的命運,但也不能靜靜地消失。
			s.log.Error("通知拉取:事件渲染失敗,留在佇列重試",
				"event_id", ev.ID, "topic", ev.Topic, "err", err)
			continue
		}
		a.ChannelID = channels[a.ChannelKey]
		out = append(out, a)
	}
	return out, nil
}

// resolveChannels 把這一批用到的邏輯用途解成實際頻道 id。
//
// 一批一次查,不是一則一次:這條路徑每幾秒就跑一次,N+1 在這裡特別貴。
//
// **多個空間設了同一個用途時刻意留空。** 目前的 outbox 事件不帶「發生在哪個
// 空間」(schemas/22 的 A 方案尚未落地),所以這裡沒有資訊可以決定貼哪一個。
// 猜一個的話,公告會出現在錯的伺服器 —— 那比不貼嚴重得多,而且很久沒有人
// 會發現。留空會讓閘道略過並記 warn,同時這裡也記一筆說明原因。
func (s *Service) resolveChannels(ctx context.Context, events []notification.Event) map[string]string {
	keys := map[string]struct{}{}
	for _, ev := range events {
		if k := notification.ChannelKeyFor(ev.Topic); k != "" {
			keys[k] = struct{}{}
		}
	}
	out := make(map[string]string, len(keys))
	for key := range keys {
		rows, err := s.q.ResolveChannelsForPurpose(ctx, key)
		if err != nil {
			s.log.Error("通知拉取:解析頻道失敗", "purpose", key, "err", err)
			continue
		}
		switch len(rows) {
		case 0:
			// 這個部署沒設這個用途。不是錯誤:不是每個社群都要每種公告。
		case 1:
			out[key] = rows[0].ChannelExternalID
		default:
			s.log.Warn("通知拉取:多個空間設了同一個用途,無法決定投遞目標,已略過",
				"purpose", key, "spaces", len(rows),
				"fix", "事件需帶 space_id(schemas/22 A 方案)")
		}
	}
	return out
}

// resolveNames 批次查出這一批事件要用到的顯示名稱。
//
// 「哪個欄位是人、哪個是商品」由 core 的 References 回答;這裡只負責查。
// 一批事件兩次查詢(使用者、商品),不是一則一次 —— N+1 在佇列這種
// 每秒都在跑的路徑上特別貴。
func (s *Service) resolveNames(ctx context.Context, events []notification.Event) (notification.Names, error) {
	userSet := map[int64]struct{}{}
	itemSet := map[string]struct{}{}
	for _, ev := range events {
		refs, err := notification.References(ev)
		if err != nil {
			// payload 壞掉:名稱查不查都一樣,Render 會在下一步報同一個錯。
			continue
		}
		for _, id := range refs.UserIDs {
			if id > 0 {
				userSet[id] = struct{}{}
			}
		}
		for _, pid := range refs.ItemPublicIDs {
			if pid != "" {
				itemSet[pid] = struct{}{}
			}
		}
	}

	names := notification.Names{
		Users: make(map[int64]string, len(userSet)),
		Items: make(map[string]string, len(itemSet)),
	}
	if len(userSet) > 0 {
		ids := make([]int64, 0, len(userSet))
		for id := range userSet {
			ids = append(ids, id)
		}
		rows, err := s.q.ListUserDisplayNames(ctx, ids)
		if err != nil {
			return notification.Names{}, fmt.Errorf("查公告的成員名稱: %w", err)
		}
		for _, r := range rows {
			names.Users[r.ID] = r.DisplayName
		}
	}
	if len(itemSet) > 0 {
		pids := make([]string, 0, len(itemSet))
		for pid := range itemSet {
			pids = append(pids, pid)
		}
		rows, err := s.q.ListItemNames(ctx, pids)
		if err != nil {
			return notification.Names{}, fmt.Errorf("查公告的商品名稱: %w", err)
		}
		for _, r := range rows {
			names.Items[r.PublicID] = r.Name
		}
	}
	return names, nil
}

// Ack 把已送達的事件標成完成,回傳真的從待送轉成完成的筆數。
//
// **只有本服務發出去的識別子算數**:token 是金鑰化的,認領以外的任何途徑
// 都造不出合法值,所以「Ack 一個從未被認領過的事件」不可能發生
// (QA 2026-09-08 的發現:先前送十進位 id 就能讓任意公告靜靜消失)。
//
// 驗不過的 token 靜靜略過(那不可能是我們發出去的);
// 重送的 Ack 算 0 筆而不是錯誤 —— 網路上重送是常態,不是異常。
//
// **識別子綁的是「事件」,不是「這一次認領」**,這是刻意的取捨:
// 綁認領批次的話,可見性逾時後重送會產生新的識別子,而 stentor 的去重鍵
// 正是它 —— 同一則公告會被貼兩次。殘留的情況是「A 認領後逾時、B 重新認領,
// 這時 A 遲到的 Ack 仍然有效」,那在 at-least-once 下本來就等價於
// 「A 其實送達了」,而重複的那一則由 stentor 的去重擋掉。
func (s *Service) Ack(ctx context.Context, eventIDs []string) (int, error) {
	if len(eventIDs) > MaxAckIDs {
		return 0, fmt.Errorf("一次最多確認 %d 筆,收到 %d 筆: %w",
			MaxAckIDs, len(eventIDs), notification.ErrInvalidRequest)
	}
	seen := make(map[int64]struct{}, len(eventIDs))
	ids := make([]int64, 0, len(eventIDs))
	for _, raw := range eventIDs {
		id, ok := parseEventID(s.eventID, raw)
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := s.q.AckAnnouncements(ctx, db.AckAnnouncementsParams{Ids: ids, Topics: s.topics})
	if err != nil {
		return 0, fmt.Errorf("確認公告事件: %w", err)
	}
	return int(n), nil
}

func clampInt32(v, def, max int32) int32 {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func clampDuration(v, def, max time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
