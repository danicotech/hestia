package notificationpg_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/infrastructure/storage/notificationpg"
)

const vis = 30 * time.Second

func ids(as []notification.Announcement) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.EventID)
	}
	return out
}

// ── 白名單 ───────────────────────────────────────────────────────────────

// 清單外的 topic 一則都不能出現。這是閘道能看到什麼的權威測試:
// 「全部 topic 都給」會讓 Bot 收到退款、工單建立這類內部事件。
func TestPullOnlyReturnsDiscordTopics(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")

	want := map[int64]string{}
	for _, topic := range notification.DiscordTopics() {
		want[insertEvent(t, ctx, topic, payloadFor(t, ctx, topic, user))] = topic
	}
	// 保留給 in-process 消費者的 topic:認領不到,也不該被碰。
	reserved := map[int64]string{}
	for _, topic := range notification.ReservedTopics() {
		reserved[insertEvent(t, ctx, topic, map[string]any{"user_id": user})] = topic
	}

	got, err := svc.Pull(ctx, 50, vis)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("拿到 %d 則,想要 %d 則:%v", len(got), len(want), ids(got))
	}
	for _, a := range got {
		if a.ChannelKey == "" || a.Title == "" {
			t.Errorf("公告缺欄位:%+v", a)
		}
	}
	// event_id 現在是不透明值,沒辦法直接比對 —— 改用「Ack 了誰」來證明
	// 回傳的正好是白名單那幾筆(而且一筆不多)。
	if n, err := svc.Ack(ctx, ids(got)); err != nil || n != len(want) {
		t.Fatalf("Ack:n=%d err=%v,想要 %d", n, err, len(want))
	}
	for id, topic := range want {
		if status, _ := eventState(t, ctx, id); status != "done" {
			t.Errorf("%s(id=%d)沒被認領/確認:status=%s", topic, id, status)
		}
	}
	// 保留清單的事件連 attempts 都不該被動到。
	for id, topic := range reserved {
		status, attempts := eventState(t, ctx, id)
		if status != "pending" || attempts != 0 {
			t.Errorf("%s(id=%d)被通知拉取碰到了:status=%s attempts=%d",
				topic, id, status, attempts)
		}
	}
}

// payloadFor 產生各 topic 的真實 payload 形狀(欄位名與產生端一致)。
func payloadFor(t *testing.T, ctx context.Context, topic string, user int64) map[string]any {
	t.Helper()
	switch topic {
	case notification.TopicDailyClaimed:
		return map[string]any{"user_id": user, "amount": 30, "streak": 3,
			"claim_date": "2026-09-08"}
	case notification.TopicShopPurchased:
		return map[string]any{"user_id": user, "item_public_id": newItem(t, ctx, "頭銜"),
			"fulfillment": "auto_perk", "entitlement_id": 987654,
			"currency": "coin", "price": 100}
	case notification.TopicMarketSold:
		return map[string]any{"order_id": 555111, "listing_public_id": "01LIST",
			"item_instance_public_id": "01INST", "buyer_id": user, "seller_id": user,
			"currency": "coin", "price": 250, "fee": 12}
	case notification.TopicRedemptionHandled:
		return map[string]any{"redemption_public_id": "01RED", "user_id": user,
			"item_public_id": newItem(t, ctx, "手工"), "status": "fulfilled",
			"handled_by": 424242, "refund_amount": 100}
	default:
		t.Fatalf("測試沒有替 %s 準備 payload —— 新增 topic 時要一起補", topic)
		return nil
	}
}

// 內部 id / 內部欄位絕不能出現在交給閘道的內容裡(鐵則 5)。
func TestPullPayloadWhitelistEndToEnd(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	item := newItem(t, ctx, "限定頭銜")
	insertEvent(t, ctx, notification.TopicShopPurchased, map[string]any{
		"user_id": user, "item_public_id": item, "fulfillment": "auto_role",
		"entitlement_id": 987654321, "currency": "coin", "price": 100,
	})

	got, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("想要 1 則,拿到 %d 則", len(got))
	}
	flat := got[0].Title
	for _, f := range got[0].Fields {
		flat += "|" + f.K + "|" + f.V
	}
	for _, bad := range []string{
		strconv.FormatInt(user, 10), // 內部 user id
		"987654321",                 // 內部 entitlement id
		"auto_role",                 // 內部履約型態
	} {
		if strings.Contains(flat, bad) {
			t.Errorf("內部值 %q 洩漏給閘道:%s", bad, flat)
		}
	}
	if !strings.Contains(flat, "限定頭銜") || !strings.Contains(flat, "小明") {
		t.Errorf("該給的名稱沒給:%s", flat)
	}
}

// ── 可見性與 at-least-once ───────────────────────────────────────────────

// 認領後在逾時之前不會被第二次拿到;逾時之後會。
func TestClaimHidesUntilVisibilityExpires(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	id := insertEvent(t, ctx, notification.TopicDailyClaimed,
		map[string]any{"user_id": user, "amount": 30, "streak": 1})

	first, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("第一次 Pull: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("第一次沒拿到那則事件:%v", ids(first))
	}
	// 對外識別子不是內部 id(鐵則 5)。
	if first[0].EventID == strconv.FormatInt(id, 10) {
		t.Fatalf("event_id 直接是內部 BIGINT id:%q", first[0].EventID)
	}
	// 認領不改 status —— 沒 Ack 的事件仍然是待送的。
	if status, attempts := eventState(t, ctx, id); status != "pending" || attempts != 1 {
		t.Errorf("認領後 status=%s attempts=%d,想要 pending/1", status, attempts)
	}

	second, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("第二次 Pull: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("可見性逾時之前不該再拿到:%v", ids(second))
	}

	expireVisibility(t, ctx, id)
	third, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("第三次 Pull: %v", err)
	}
	if len(third) != 1 {
		t.Fatalf("逾時後應該重新可見(at-least-once):%v", ids(third))
	}
	// **重送必須拿到同一個 event_id**:那是 stentor 的去重鍵,
	// 每次認領換一個值等於去重失效,同一則公告會被貼兩次。
	if third[0].EventID != first[0].EventID {
		t.Fatalf("重送的 event_id 變了(%q → %q)——閘道的去重會失效",
			first[0].EventID, third[0].EventID)
	}
	if _, attempts := eventState(t, ctx, id); attempts != 2 {
		t.Errorf("重送要算一次重試,attempts=%d", attempts)
	}
}

func TestAckRemovesFromQueue(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	id := insertEvent(t, ctx, notification.TopicDailyClaimed,
		map[string]any{"user_id": user, "amount": 30, "streak": 1})

	got, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	n, err := svc.Ack(ctx, ids(got))
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n != 1 {
		t.Fatalf("Ack 回 %d,想要 1", n)
	}
	if status, _ := eventState(t, ctx, id); status != "done" {
		t.Errorf("Ack 後 status=%s,想要 done", status)
	}

	// 重送 Ack 是常態,不是錯誤:算 0 筆。
	if n, err := svc.Ack(ctx, ids(got)); err != nil || n != 0 {
		t.Errorf("重送 Ack:n=%d err=%v,想要 0/nil", n, err)
	}
	// 逾時也不會再回來 —— done 就是終態。
	expireVisibility(t, ctx, id)
	if again, err := svc.Pull(ctx, 10, vis); err != nil || len(again) != 0 {
		t.Errorf("Ack 過的事件又出現了:%v(err=%v)", ids(again), err)
	}
}

// Ack 只能碰 Discord 清單內的 topic。
//
// 現在有兩道防線:識別子造不出來(它從沒被認領過),而且就算造得出來,
// SQL 的 topic 條件也擋著。這裡刻意用**本服務自己鑄出來的**合法識別子,
// 才測得到第二道 —— 否則第一道就先擋掉了。
func TestAckIgnoresReservedTopics(t *testing.T) {
	ctx := setup(t)
	id := insertEvent(t, ctx, notification.TopicEntitlementExpired,
		map[string]any{"entitlement_id": 1, "user_id": 1, "item_id": 1})

	n, err := svc.Ack(ctx, []string{notificationpg.MintEventIDForTest(svc, id)})
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n != 0 {
		t.Fatalf("Ack 動到了清單外的事件:n=%d", n)
	}
	if status, _ := eventState(t, ctx, id); status != "pending" {
		t.Errorf("清單外的事件被改成 %s", status)
	}
}

// ── 識別子不可偽造 ───────────────────────────────────────────────────────

// QA 2026-09-08:先前 event_id 就是十進位的內部 id,任何持服務憑證者都能
// Ack 一個**從未被自己認領過**的事件 —— 那則公告會靜默消失。
func TestAckRejectsNeverClaimedEvent(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	id := insertEvent(t, ctx, notification.TopicDailyClaimed,
		map[string]any{"user_id": user, "amount": 30, "streak": 1})

	// 內部 id 的各種可能寫法,一個都不能生效。
	guesses := []string{
		strconv.FormatInt(id, 10),
		"n1." + strconv.FormatInt(id, 10),
		strconv.FormatInt(id, 10) + ".",
		"n1.AAAAAAAAAAAAAAAAAAAAAA",
		"n1." + strings.Repeat("A", 22),
	}
	n, err := svc.Ack(ctx, guesses)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n != 0 {
		t.Fatalf("猜出來的識別子竟然生效了:n=%d", n)
	}
	if status, _ := eventState(t, ctx, id); status != "pending" {
		t.Fatalf("從未被認領的事件被 Ack 掉了:status=%s", status)
	}

	// 認領過之後,同一個事件的合法識別子才有效。
	got, err := svc.Pull(ctx, 10, vis)
	if err != nil || len(got) != 1 {
		t.Fatalf("Pull:n=%d err=%v", len(got), err)
	}
	if n, err := svc.Ack(ctx, ids(got)); err != nil || n != 1 {
		t.Fatalf("認領後的 Ack:n=%d err=%v", n, err)
	}
}

// 另一把金鑰鑄出來的識別子一律無效。
func TestAckRejectsForeignKeyToken(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	id := insertEvent(t, ctx, notification.TopicDailyClaimed,
		map[string]any{"user_id": user, "amount": 30, "streak": 1})

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	k, err := notificationpg.NewEventIDKey(key)
	if err != nil {
		t.Fatalf("NewEventIDKey: %v", err)
	}
	other := notificationpg.New(pool, notificationpg.WithEventIDKey(k))

	forged := notificationpg.MintEventIDForTest(other, id)
	if n, err := svc.Ack(ctx, []string{forged}); err != nil || n != 0 {
		t.Fatalf("別把金鑰的識別子竟然生效:n=%d err=%v", n, err)
	}
	if status, _ := eventState(t, ctx, id); status != "pending" {
		t.Fatalf("事件被別把金鑰的識別子 Ack 掉了:status=%s", status)
	}
}

// 固定金鑰 → 重啟後識別子不變(這就是 WithEventIDKey 存在的理由),
// 而且連號的事件不會得到看得出關聯的識別子。
func TestFixedKeyGivesStableOpaqueEventIDs(t *testing.T) {
	k, err := notificationpg.NewEventIDKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewEventIDKey: %v", err)
	}
	a := notificationpg.New(pool, notificationpg.WithEventIDKey(k))
	b := notificationpg.New(pool, notificationpg.WithEventIDKey(k))

	const id int64 = 4242
	if notificationpg.MintEventIDForTest(a, id) != notificationpg.MintEventIDForTest(b, id) {
		t.Fatal("同一把金鑰的兩個 instance 鑄出不同識別子 —— 重啟就會重貼")
	}
	first := notificationpg.MintEventIDForTest(a, id)
	second := notificationpg.MintEventIDForTest(a, id+1)
	if first == second {
		t.Fatal("不同事件鑄出相同識別子")
	}
	if strings.Contains(first, strconv.FormatInt(id, 10)) {
		t.Fatalf("識別子沒有藏住內部 id:%q", first)
	}
}

func TestNewEventIDKeyRejectsBadLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31, 33} {
		if _, err := notificationpg.NewEventIDKey(make([]byte, n)); err == nil {
			t.Errorf("長度 %d 的金鑰竟然通過", n)
		}
	}
}

// 解析不出來的 id 靜靜略過,不是錯誤(重送/亂送都不該讓整批失敗)。
func TestAckIgnoresGarbageIDs(t *testing.T) {
	ctx := setup(t)
	n, err := svc.Ack(ctx, []string{"", "abc", "-1", "0", "999999999"})
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n != 0 {
		t.Fatalf("想要 0,拿到 %d", n)
	}
}

func TestAckRejectsOversizedBatch(t *testing.T) {
	ctx := setup(t)
	tooMany := make([]string, notificationpg.MaxAckIDs+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(i + 1)
	}
	if _, err := svc.Ack(ctx, tooMany); !errors.Is(err, notification.ErrInvalidRequest) {
		t.Fatalf("想要 ErrInvalidRequest,拿到 %v", err)
	}
}

// ── 併發 ─────────────────────────────────────────────────────────────────

// 兩個 Pull 同時打:同一則事件絕不能同時給兩邊(SKIP LOCKED 的重點)。
func TestConcurrentPullsDoNotOverlap(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	const total = 40
	for i := 0; i < total; i++ {
		insertEvent(t, ctx, notification.TopicDailyClaimed,
			map[string]any{"user_id": user, "amount": 10, "streak": i})
	}

	var wg sync.WaitGroup
	results := make([][]string, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := svc.Pull(context.Background(), total, vis)
			results[i], errs[i] = ids(got), err
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Pull %d: %v", i, err)
		}
		for _, id := range results[i] {
			seen[id]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("事件 %s 被兩個 Pull 同時拿到(%d 次)", id, n)
		}
	}
	if len(seen) != total {
		t.Errorf("兩邊合計拿到 %d 則,想要 %d 則(SKIP LOCKED 不該漏)", len(seen), total)
	}
}

// ── 毒訊息 ───────────────────────────────────────────────────────────────

// 達重試上限的事件不再回傳,而且會被標成 failed 離開待送佇列
// (維持 outbox 既有的毒訊息語意:一則壞掉的不能永遠卡在佇列裡)。
func TestExhaustedEventsAreNotReturnedAndFail(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	id := insertEvent(t, ctx, notification.TopicDailyClaimed,
		map[string]any{"user_id": user, "amount": 30, "streak": 1})
	if _, err := pool.Exec(ctx,
		`UPDATE platform.outbox_events SET attempts = $1 WHERE id = $2`,
		notificationpg.DefaultMaxAttempts, id); err != nil {
		t.Fatalf("撥高 attempts: %v", err)
	}

	got, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("達重試上限的事件不該回傳:%v", ids(got))
	}
	if status, _ := eventState(t, ctx, id); status != "failed" {
		t.Errorf("想要 failed,拿到 %s", status)
	}
}

// 渲染不出來的事件不回傳、也不 Ack:它留在佇列裡,最後由 attempts 判成毒訊息。
func TestBrokenPayloadStaysInQueue(t *testing.T) {
	ctx := setup(t)
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO platform.outbox_events (topic, payload)
		 VALUES ($1, '"不是物件"'::jsonb) RETURNING id`,
		notification.TopicDailyClaimed,
	).Scan(&id)
	if err != nil {
		t.Fatalf("寫壞掉的事件: %v", err)
	}

	got, err := svc.Pull(ctx, 10, vis)
	if err != nil {
		t.Fatalf("Pull 不該因為一則壞事件而整批失敗: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("壞掉的事件不該被渲染出來:%v", ids(got))
	}
	status, attempts := eventState(t, ctx, id)
	if status != "pending" || attempts != 1 {
		t.Errorf("壞事件應該留在佇列並算一次重試:status=%s attempts=%d", status, attempts)
	}
}

// ── 參數夾限 ─────────────────────────────────────────────────────────────

func TestPullClampsBatchSize(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	inserted := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		inserted = append(inserted, insertEvent(t, ctx, notification.TopicDailyClaimed,
			map[string]any{"user_id": user, "amount": 1, "streak": i}))
	}
	got, err := svc.Pull(ctx, 2, vis)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("max=2 卻拿到 %d 則", len(got))
	}
	// 舊的先給:公告順序就是事件順序(UPDATE ... RETURNING 不保證順序,
	// 排序是實作補上的)。識別子不透明,所以拿最舊的兩筆自己鑄一次來比對。
	want := []string{
		notificationpg.MintEventIDForTest(svc, inserted[0]),
		notificationpg.MintEventIDForTest(svc, inserted[1]),
	}
	if got[0].EventID != want[0] || got[1].EventID != want[1] {
		t.Errorf("回傳沒有依 id 由小到大給最舊的兩筆:%v,想要 %v", ids(got), want)
	}
}

// visibility 不足一秒時無條件進位:截斷成 0 秒等於沒有可見性,
// 同一則公告會被下一次 Pull 立刻再拿走一次。
func TestSubSecondVisibilityStillHides(t *testing.T) {
	ctx := setup(t)
	user := newUser(t, ctx, "小明")
	insertEvent(t, ctx, notification.TopicDailyClaimed,
		map[string]any{"user_id": user, "amount": 1, "streak": 1})

	if got, err := svc.Pull(ctx, 10, 100*time.Millisecond); err != nil || len(got) != 1 {
		t.Fatalf("第一次 Pull:n=%d err=%v", len(got), err)
	}
	got, err := svc.Pull(ctx, 10, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("第二次 Pull: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不足一秒的可見性被截成 0,事件立刻又被拿走:%v", ids(got))
	}
}
