package notification_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/notification"
)

// 這一檔要證明四件事:
//   - Discord 清單與 in-process 保留清單**互斥**(兩邊都處理 = 重複發訊息);
//   - payload 白名單真的擋得住:內部 BIGINT id / 手續費 / 處理者一個都不出現;
//   - 名稱是使用者可控字串,不會把控制字元或超長內容原樣送出去;
//   - 認不得的 topic 與壞掉的 payload 一律報錯,不會「盡力貼一則殘缺的」。

func TestDiscordTopicsAndReservedAreDisjoint(t *testing.T) {
	discord := notification.DiscordTopics()
	if len(discord) == 0 {
		t.Fatal("Discord topic 清單不該是空的")
	}
	for _, topic := range notification.ReservedTopics() {
		if notification.IsDiscordTopic(topic) {
			t.Errorf("%s 同時在 Discord 清單與 in-process 保留清單裡", topic)
		}
	}
	if err := notification.AssertNoOverlap(notification.ReservedTopics()); err != nil {
		t.Errorf("保留清單不該與 Discord 清單重疊:%v", err)
	}
}

// AssertNoOverlap 是組裝端註冊 in-process handler 時的防線。
func TestAssertNoOverlapCatchesClash(t *testing.T) {
	err := notification.AssertNoOverlap([]string{
		notification.TopicEntitlementExpired, // 合法:保留給 in-process
		notification.TopicDailyClaimed,       // 非法:閘道要拉這個
	})
	if !errors.Is(err, notification.ErrTopicOverlap) {
		t.Fatalf("想要 ErrTopicOverlap,拿到 %v", err)
	}
	if !strings.Contains(err.Error(), notification.TopicDailyClaimed) {
		t.Errorf("錯誤訊息要指出是哪個 topic:%v", err)
	}
	if strings.Contains(err.Error(), notification.TopicEntitlementExpired) {
		t.Errorf("保留清單內的 topic 不該被當成衝突:%v", err)
	}
}

// 清單外的 topic 一律不渲染 —— 認領查詢已經濾過一次,這是第二道。
func TestRenderRejectsUnknownTopic(t *testing.T) {
	for _, topic := range append(notification.ReservedTopics(), "totally.unknown") {
		ev := notification.Event{ID: "1", Topic: topic, Payload: []byte(`{}`)}
		if _, err := notification.Render(ev, notification.Names{}); !errors.Is(err, notification.ErrUnknownTopic) {
			t.Errorf("%s:想要 ErrUnknownTopic,拿到 %v", topic, err)
		}
		if _, err := notification.References(ev); !errors.Is(err, notification.ErrUnknownTopic) {
			t.Errorf("%s References:想要 ErrUnknownTopic,拿到 %v", topic, err)
		}
	}
}

func TestRenderRejectsBrokenPayload(t *testing.T) {
	ev := notification.Event{
		ID: "1", Topic: notification.TopicDailyClaimed, Payload: []byte(`{"amount":`),
	}
	if _, err := notification.Render(ev, notification.Names{}); err == nil {
		t.Fatal("壞掉的 payload 應該報錯,而不是貼一則殘缺的公告")
	}
}

// ── payload 白名單 ──────────────────────────────────────────────────────

// 每個 topic 都灌一份「比真實 payload 更多欄位」的 JSON:真實產生端有的
// 內部欄位全部放進去,再斷言它們一個字都沒出現在渲染結果裡。
func TestRenderPayloadWhitelist(t *testing.T) {
	names := notification.Names{
		Users: map[int64]string{7: "小明", 8: "小美"},
		Items: map[string]string{"01ITEM": "限定頭銜"},
	}
	cases := []struct {
		topic   string
		payload map[string]any
		// forbidden 是**絕不能**出現在輸出裡的字串(內部 id、金額細節、原文)。
		forbidden []string
		wantChan  string
	}{
		{
			topic: notification.TopicDailyClaimed,
			payload: map[string]any{
				"user_id": 7, "amount": 30, "streak": 3, "claim_date": "2026-09-08",
				"internal_note": "不該出現",
			},
			forbidden: []string{"7", "2026-09-08", "不該出現"},
			wantChan:  "daily",
		},
		{
			topic: notification.TopicShopPurchased,
			payload: map[string]any{
				"user_id": 7, "item_public_id": "01ITEM", "fulfillment": "auto_role",
				"entitlement_id": 987654, "currency": "coin", "price": 100,
			},
			// entitlement_id 是內部 BIGINT(鐵則 5),fulfillment 是內部履約型態。
			forbidden: []string{"987654", "auto_role"},
			wantChan:  "shop",
		},
		{
			topic: notification.TopicMarketSold,
			payload: map[string]any{
				"order_id": 555111, "listing_public_id": "01LIST",
				"item_instance_public_id": "01INST",
				"buyer_id":                8, "seller_id": 7, "currency": "coin",
				"price": 250, "fee": 12,
			},
			// order_id 是內部 BIGINT;fee 是平台抽成,不是公告內容。
			forbidden: []string{"555111", "01LIST", "01INST", "12"},
			wantChan:  "market",
		},
		{
			topic: notification.TopicRedemptionHandled,
			payload: map[string]any{
				"redemption_public_id": "01RED", "user_id": 7,
				"item_public_id": "01ITEM", "status": "fulfilled",
				"handled_by": 424242, "refund_amount": 100,
			},
			// handled_by 同時是內部 id 與「誰批的」;refund_amount 不上頻道。
			forbidden: []string{"424242", "100"},
			wantChan:  "redemptions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.topic, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			ev := notification.Event{ID: "42", Topic: tc.topic, Payload: raw}
			a, err := notification.Render(ev, names)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if a.EventID != "42" {
				t.Errorf("event_id 要原樣帶回:%q", a.EventID)
			}
			if a.ChannelKey != tc.wantChan {
				t.Errorf("channel_key = %q,想要 %q", a.ChannelKey, tc.wantChan)
			}
			if a.Title == "" || len(a.Fields) == 0 {
				t.Fatalf("公告不該是空的:%+v", a)
			}
			flat := a.Title
			for _, f := range a.Fields {
				flat += "|" + f.K + "|" + f.V
			}
			for _, bad := range tc.forbidden {
				if strings.Contains(flat, bad) {
					t.Errorf("內部欄位 %q 洩漏到公告裡:%s", bad, flat)
				}
			}
		})
	}
}

// 查不到名稱時要有可讀的預設值,不能露出內部 id。
func TestRenderFallsBackToPlaceholderNames(t *testing.T) {
	raw := []byte(`{"user_id":7,"item_public_id":"01ITEM","currency":"coin","price":5}`)
	a, err := notification.Render(
		notification.Event{ID: "1", Topic: notification.TopicShopPurchased, Payload: raw},
		notification.Names{}, // 什麼都沒查到
	)
	if err != nil {
		t.Fatal(err)
	}
	var member, item string
	for _, f := range a.Fields {
		switch f.K {
		case "成員":
			member = f.V
		case "商品":
			item = f.V
		}
	}
	if member != "未知成員" || item != "未知商品" {
		t.Fatalf("想要預設名稱,拿到 成員=%q 商品=%q", member, item)
	}
	if strings.Contains(member+item, "7") || strings.Contains(member+item, "01ITEM") {
		t.Errorf("找不到名稱時不該退回內部 id / public_id:%q %q", member, item)
	}
}

// 顯示名稱是使用者自己填的:換行、控制字元、超長內容都要被壓掉。
func TestRenderSanitizesUserControlledNames(t *testing.T) {
	evil := "@everyone\n\t看我" + strings.Repeat("長", 200)
	raw := []byte(`{"user_id":7,"amount":1,"streak":1}`)
	a, err := notification.Render(
		notification.Event{ID: "1", Topic: notification.TopicDailyClaimed, Payload: raw},
		notification.Names{Users: map[int64]string{7: evil}},
	)
	if err != nil {
		t.Fatal(err)
	}
	name := a.Fields[0].V
	if strings.ContainsAny(name, "\n\t") {
		t.Errorf("名稱裡還有控制字元:%q", name)
	}
	if n := len([]rune(name)); n > 64 {
		t.Errorf("名稱沒有截斷,長度 %d:%q", n, name)
	}
	// @everyone 本身不必移除(嵌入卡不會 ping),但它絕不能離開 Fields ——
	// Announcement 型別根本沒有 content 欄位,這是型別層級的保證。
	if !strings.Contains(name, "@everyone") {
		t.Logf("名稱被截斷或改寫:%q", name)
	}
}
