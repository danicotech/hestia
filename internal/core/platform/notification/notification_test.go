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

// ── 賽事公告 ─────────────────────────────────────────────────────────────

// TestRenderHandicapLocked 釘住封盤公告的形狀。
//
// 這一則值得單獨測,因為它是整套系統對「參加者不混亂」的主要交付物:
// 選手、對手、觀眾、裁判就是靠它在同一則訊息裡對齊同一份資訊。
// 少了任何一項讓武、或漏掉指定內容,場上就會有人以為自己沒被限制。
func TestRenderHandicapLocked(t *testing.T) {
	t.Parallel()

	payload := `{
	  "tournament_name": "百業試鋒", "round_label": "八強", "slot": 2,
	  "p1": {"display_name": "李璃", "rank_name": "開山"},
	  "p2": {"display_name": "A冷", "rank_name": "無我"},
	  "handicap": {
	    "holder_display_name": "李璃", "constrained_display_name": "A冷",
	    "budget": 24, "spent": 21,
	    "items": [
	      {"name": "禁用奇術", "cost": 12},
	      {"name": "禁用迴避", "cost": 8},
	      {"name": "指定對手開局時講一句話", "cost": 1, "target_note": "今日試鋒,請多指教"}
	    ]
	  }
	}`

	got, err := notification.Render(notification.Event{
		ID: "ev-1", Topic: notification.TopicHandicapLocked, Payload: []byte(payload),
	}, notification.Names{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Title != "讓武封盤" {
		t.Errorf("title = %q", got.Title)
	}
	if got.ChannelKey != "tournament_results" {
		t.Errorf("channel = %q", got.ChannelKey)
	}

	joined := ""
	for _, f := range got.Fields {
		joined += f.K + "\x00" + f.V + "\n"
	}

	// slot 是 0-based,對外必須 +1 —— 場上喊的是「第三場」。
	for _, want := range []string{
		"八強 第 3 場",
		"李璃(開山) vs A冷(無我)",
		"A冷 本場須遵守",
		"禁用奇術", "禁用迴避",
		// 需要指定內容的項目,指定的內容本身必須出現在公告裡,
		// 否則對手看得到限制卻不知道要講哪八個字。
		"指定對手開局時講一句話:今日試鋒,請多指教",
		"3 項 · 共 21 BP",
		// 這句每一則封盤公告都要有:讓武最容易被誤解成放水,
		// 而誤解的代價是受限方消極應戰,整場就沒有看頭了。
		"讓武不是放水",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("公告裡缺少 %q\n實際:\n%s", want, strings.ReplaceAll(joined, "\x00", " / "))
		}
	}
}

// TestRenderTournamentMatchTopicsAreDiscordBound 確認五個賽事 topic 都真的
// 會被閘道拉取,而且沒有與 in-process 消費者重疊。
//
// DiscordTopics 是從 renderers 推導的:少寫一個 renderer 的症狀不是編譯錯誤,
// 是那則公告永遠不會被貼出來,而 outbox 的 pending 列永不刪除 —— 靜靜累積。
func TestRenderTournamentMatchTopicsAreDiscordBound(t *testing.T) {
	t.Parallel()

	want := []string{
		notification.TopicHandicapOpened,
		notification.TopicHandicapLocked,
		notification.TopicMatchStarted,
		notification.TopicMatchFinished,
		notification.TopicChampion,
	}
	for _, topic := range want {
		if !notification.IsDiscordTopic(topic) {
			t.Errorf("%s 不在 Discord 推播清單內", topic)
		}
	}
	if err := notification.AssertNoOverlap(want); err == nil {
		t.Error("AssertNoOverlap 應該要報這五個 topic 已由 Discord 處理")
	}
}

// TestRenderHandicapOpenedSameRank 確認同段對決也照樣公告。
//
// 沒有讓武不代表不用告訴大家這場開盤了 —— 觀眾要知道可以下注了,
// 而「本場無讓武」本身就是有用的資訊(否則會有人一直等公告)。
func TestRenderHandicapOpenedSameRank(t *testing.T) {
	t.Parallel()

	payload := `{"tournament_name":"百業試鋒","round_label":"四強","slot":0,
	  "p1":{"display_name":"墨無聲","rank_name":"無我"},
	  "p2":{"display_name":"雲中鶴","rank_name":"無我"}}`

	got, err := notification.Render(notification.Event{
		ID: "ev-2", Topic: notification.TopicHandicapOpened, Payload: []byte(payload),
	}, notification.Names{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	found := false
	for _, f := range got.Fields {
		if strings.Contains(f.V, "同段對決,本場無讓武") {
			found = true
		}
	}
	if !found {
		t.Errorf("同段對決應明說本場無讓武,得到 %+v", got.Fields)
	}
}
