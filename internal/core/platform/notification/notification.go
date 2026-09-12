// Package notification 定義「哪些 outbox 事件會送到 Discord 頻道,以及它們長什麼樣」。
//
// 它是這個問題的**唯一權威**。三件事都在這裡,不散落:
//
//  1. `DiscordTopics` —— 哪些 topic 給閘道。清單制而不是「全部都給」:
//     閘道暴露在外部平台上,退款、工單建立這類內部事件它不該知道。
//  2. `ReservedTopics` —— 哪些 topic **保留給 in-process 消費者**。
//     兩份清單互斥,`AssertNoOverlap` 讓違反在啟動時就炸 —— 同一個 topic
//     兩邊都處理,結果是同一件事在 Discord 被講兩次(或該收回的身分組被
//     閘道 Ack 掉而永遠收不回來)。
//  3. 每個 topic 的**渲染器** —— 把內部事件變成 Announcement。
//
// ## 為什麼渲染在這裡,不在閘道
//
// outbox 的 payload 是內部格式:裡面有內部 BIGINT id(user_id、entitlement_id、
// order_id、handled_by)。那些東西**不能**離開這個進程(專案鐵則 5),
// 而 stentor 的鐵則是「不解讀事件語意,只負責貼」。
//
// 所以白名單長在型別上:每個 topic 有一個只宣告**該給的欄位**的 struct,
// `json.Unmarshal` 會把沒宣告的欄位直接丟掉。要多洩漏一個欄位,必須有人
// 明確地在這裡加一行 —— 忘記維護的後果是「少給」,不是「多給」。
//
// ## 使用者可控字串的處理
//
// 顯示名稱與商品名稱會進到公告裡。兩件事:
//
//   - 一律經 `sanitize`(去控制字元、壓成單行、截長)。
//   - **絕不進 Announcement 的 content。** 本套件的 Announcement 根本沒有那個
//     欄位:Discord 只在純文字 content 解析 @everyone / @here / <@id>,
//     嵌入卡裡的同樣文字不會 ping 任何人。把使用者可自訂的暱稱放進 content,
//     等於把「改個暱稱就能 @全體」送給任何人。
package notification

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ── topic ────────────────────────────────────────────────────────────────

// 送往 Discord 頻道的 topic。字面值與產生端(dailypg / shoppg / marketpg)
// 一致 —— 它們是同一條契約的兩端,改一邊必須改另一邊(本套件的測試會抓)。
const (
	TopicDailyClaimed = "daily.claimed"
	// TopicLevelUp 是升級公告(schemas/24)。發獎走另一個 in-process topic:
	// 貼不出公告不該擋住獎勵,發不出獎勵也不該擋住公告。
	TopicLevelUp = "level.up"
	// TopicLevelReward 由 in-process 消費者處理(發角色/點數/物品)。
	TopicLevelReward       = "level.reward"
	TopicShopPurchased     = "shop.purchased"
	TopicMarketSold        = "market.sold"
	TopicRedemptionHandled = "redemption.handled"
)

// 保留給 in-process 消費者的 topic:它們要做的**不是**貼訊息。
//
//   - shop.refunded / entitlement.expired —— 消費端要收回 Discord 身分組。
//     那需要 payload 裡的 external_role_id,而 Announcement 契約只能描述
//     「貼什麼」,沒有「做什麼」。交給閘道拉取的話,Ack 之後事件就是 done,
//     身分組永遠收不回來 —— 比現在什麼都沒接更糟。
//   - redemption.created —— 給管理端的待辦訊號,不是公開公告
//     (誰買了什麼要人工履約,是工單系統的事)。
const (
	TopicShopRefunded       = "shop.refunded"
	TopicRedemptionCreated  = "redemption.created"
	TopicEntitlementExpired = "entitlement.expired"
)

// discordChannels 是 topic → 邏輯頻道名。
//
// **只給邏輯名,不給 channel id**:頻道搬家是部署設定的事(stentor 的
// CHANNEL_MAP 負責對到真的 snowflake),後端不該知道 channel id。
// 對不到頻道的公告 stentor 會略過並記 warn,那是預期行為,不是錯誤。
var discordChannels = map[string]string{
	TopicDailyClaimed:      "daily",
	TopicLevelUp:           "level_up",
	TopicShopPurchased:     "shop",
	TopicMarketSold:        "market",
	TopicRedemptionHandled: "redemptions",
}

// ReservedTopics 是保留給 in-process 消費者的 topic(排序後回傳,方便斷言)。
func ReservedTopics() []string {
	out := []string{TopicShopRefunded, TopicRedemptionCreated, TopicEntitlementExpired, TopicLevelReward}
	sort.Strings(out)
	return out
}

// DiscordTopics 是會交給閘道拉取的 topic(排序後回傳:認領查詢要一份穩定的清單)。
func DiscordTopics() []string {
	out := make([]string, 0, len(renderers))
	for topic := range renderers {
		out = append(out, topic)
	}
	sort.Strings(out)
	return out
}

// IsDiscordTopic 回報這個 topic 是否屬於 Discord 推播。
func IsDiscordTopic(topic string) bool {
	_, ok := renderers[topic]
	return ok
}

// ErrTopicOverlap 是「同一個 topic 兩邊都處理」的訊號。
var ErrTopicOverlap = errors.New("topic 同時由 in-process 消費者與 Discord 拉取處理")

// AssertNoOverlap 檢查一組 in-process handler 的 topic 有沒有踩到 Discord 清單。
//
// 給組裝端(cmd/server 註冊 outbox handler 時)呼叫:重複投遞不是會報錯的
// bug,是「同一件事在頻道被講兩次」這種只有使用者看得到的 bug,
// 一定要在啟動時就擋下來。
func AssertNoOverlap(inProcessTopics []string) error {
	var clash []string
	for _, topic := range inProcessTopics {
		if IsDiscordTopic(topic) {
			clash = append(clash, topic)
		}
	}
	if len(clash) == 0 {
		return nil
	}
	sort.Strings(clash)
	return fmt.Errorf("%w: %s", ErrTopicOverlap, strings.Join(clash, ", "))
}

// ── 對外型別 ─────────────────────────────────────────────────────────────

// Event 是一則認領到的 outbox 事件。
//
// Payload 是**內部**格式,只有本套件的渲染器讀得到它;
// 呼叫端拿到的永遠是 Render 的產出,不是這個。
type Event struct {
	// ID 是認領/確認用的憑據(對閘道不透明,原封不動送回來即可)。
	ID      string
	Topic   string
	Payload []byte
}

// Field 是公告上的一組「標籤 / 值」。
type Field struct {
	K string
	V string
}

// Announcement 是渲染後的公告。刻意**沒有 content 欄位**(理由見套件註解)。
type Announcement struct {
	EventID    string
	ChannelKey string
	// ChannelID 是 ChannelKey 解析後的實際頻道(Discord snowflake)。
	//
	// 解析在 hestia 做,不在閘道做:對應關係存在 space_channel_purposes,
	// 而只有 hestia 碰得到資料庫。閘道拿到的是「貼到這裡」,不必自己查表 ——
	// 舊的 CHANNEL_MAP 環境變數是一份全域對應,bot 進第二個伺服器就沒有
	// 正確答案可給,那正是這個欄位要取代的東西。
	//
	// 空字串 = 這個部署沒有為該用途設頻道,或無法決定是哪一個(見儲存層)。
	// 呼叫端應略過並記 warn,那是預期行為不是錯誤。
	ChannelID string
	Title     string
	Fields    []Field
}

// Refs 是渲染前要先查好的外部名稱。
//
// 為什麼要分兩步:payload 裡只有內部 id,而公告要顯示的是人名與商品名。
// 「哪個欄位是人、哪個欄位是商品」的知識只能有一個位置 —— 就是這裡,
// 與渲染器同一個檔案。儲存層照著查,不必自己解讀 payload。
type Refs struct {
	UserIDs       []int64
	ItemPublicIDs []string
}

// Names 是查好的名稱。查不到的鍵不必補,渲染器有預設值。
type Names struct {
	Users map[int64]string
	Items map[string]string
}

// ErrInvalidRequest 是拉取/確認的參數不合法(errmap 映射成 InvalidArgument)。
//
// 只用在「重送同樣的內容一定還是失敗」的情況:一次 Ack 的 id 數超過上限。
// max / visibility_seconds 超出範圍**不算**這一類 —— 那些會被夾到上限,
// 因為呼叫端的意思是「盡量多給我一點」,為此讓整個請求失敗沒有好處。
var ErrInvalidRequest = errors.New("通知拉取的請求參數不合法")

// ErrUnknownTopic 是「這個 topic 不屬於 Discord 推播」。
// 它是防禦性的:認領查詢已經只挑清單內的 topic,走到這裡代表兩者不同步。
var ErrUnknownTopic = errors.New("topic 不在 Discord 推播清單內")

// References 回報這則事件需要先查哪些名稱。
// payload 壞掉時回空 Refs 與 error —— 呼叫端可以選擇照樣渲染(會拿到預設名稱)。
func References(ev Event) (Refs, error) {
	r, ok := renderers[ev.Topic]
	if !ok {
		return Refs{}, fmt.Errorf("%s: %w", ev.Topic, ErrUnknownTopic)
	}
	p, err := r.decode(ev.Payload)
	if err != nil {
		return Refs{}, fmt.Errorf("解析 %s 的 payload: %w", ev.Topic, err)
	}
	return p.refs(), nil
}

// Render 把事件渲染成公告。
//
// payload 解析失敗一律回 error 而不是「盡力貼一則殘缺的」:
// 貼錯的公告收不回來,寧可讓事件留在佇列裡重試並最終進毒訊息終態。
func Render(ev Event, names Names) (Announcement, error) {
	r, ok := renderers[ev.Topic]
	if !ok {
		return Announcement{}, fmt.Errorf("%s: %w", ev.Topic, ErrUnknownTopic)
	}
	p, err := r.decode(ev.Payload)
	if err != nil {
		return Announcement{}, fmt.Errorf("解析 %s 的 payload: %w", ev.Topic, err)
	}
	title, fields := p.render(names)
	return Announcement{
		EventID:    ev.ID,
		ChannelKey: discordChannels[ev.Topic],
		Title:      title,
		Fields:     fields,
	}, nil
}

// ── 渲染器 ───────────────────────────────────────────────────────────────

// payload 是「解析後的某個 topic」。兩個方法對應渲染的兩步。
type payload interface {
	refs() Refs
	render(names Names) (title string, fields []Field)
}

type renderer struct {
	decode func([]byte) (payload, error)
}

// decodeAs 是共用的解碼樣板。
//
// 關鍵性質:T 只宣告**該給的欄位**,json 把 payload 裡其他欄位全部丟掉。
// 白名單因此不是一份要人維護的清單,而是型別本身 —— 想多洩漏一個欄位,
// 得先在下面某個 struct 上加一行。
func decodeAs[T payload](b []byte) (payload, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

var renderers = map[string]renderer{
	TopicDailyClaimed:      {decode: decodeAs[dailyClaimed]},
	TopicLevelUp:           {decode: decodeAs[levelUp]},
	TopicShopPurchased:     {decode: decodeAs[shopPurchased]},
	TopicMarketSold:        {decode: decodeAs[marketSold]},
	TopicRedemptionHandled: {decode: decodeAs[redemptionHandled]},
}

// dailyClaimed ← dailypg。claim_date 刻意不宣告:公告貼的是「剛剛發生的事」,
// 日期只會讓卡片變長。
type dailyClaimed struct {
	UserID int64 `json:"user_id"`
	Amount int64 `json:"amount"`
	Streak int32 `json:"streak"`
}

func (p dailyClaimed) refs() Refs { return Refs{UserIDs: []int64{p.UserID}} }

func (p dailyClaimed) render(n Names) (string, []Field) {
	return "每日簽到", []Field{
		{K: "成員", V: userName(n, p.UserID)},
		{K: "獲得", V: strconv.FormatInt(p.Amount, 10)},
		{K: "連續", V: strconv.FormatInt(int64(p.Streak), 10) + " 天"},
	}
}

// shopPurchased ← shoppg。entitlement_id(內部 BIGINT)與 fulfillment
// (內部履約型態)刻意不宣告:前者違反鐵則 5,後者不是使用者要看的東西。
type shopPurchased struct {
	UserID       int64  `json:"user_id"`
	ItemPublicID string `json:"item_public_id"`
	Currency     string `json:"currency"`
	Price        int64  `json:"price"`
}

func (p shopPurchased) refs() Refs {
	return Refs{UserIDs: []int64{p.UserID}, ItemPublicIDs: []string{p.ItemPublicID}}
}

func (p shopPurchased) render(n Names) (string, []Field) {
	return "商店購買", []Field{
		{K: "成員", V: userName(n, p.UserID)},
		{K: "商品", V: itemName(n, p.ItemPublicID)},
		{K: "花費", V: money(p.Price, p.Currency)},
	}
}

// marketSold ← marketpg。order_id(內部 BIGINT)不宣告;
// fee 也不宣告 —— 平台抽成是經濟參數,不是公告的內容。
type marketSold struct {
	BuyerID  int64  `json:"buyer_id"`
	SellerID int64  `json:"seller_id"`
	Currency string `json:"currency"`
	Price    int64  `json:"price"`
}

func (p marketSold) refs() Refs { return Refs{UserIDs: []int64{p.SellerID, p.BuyerID}} }

func (p marketSold) render(n Names) (string, []Field) {
	return "市集成交", []Field{
		{K: "賣家", V: userName(n, p.SellerID)},
		{K: "買家", V: userName(n, p.BuyerID)},
		{K: "成交價", V: money(p.Price, p.Currency)},
	}
}

// redemptionHandled ← shoppg。handled_by(處理者的內部 BIGINT id)刻意不宣告:
// 它同時是內部 id 與「誰批的」——兩個理由各自都足以排除。
// refund_amount 也不宣告:那是當事人與帳本之間的事,不上頻道。
type redemptionHandled struct {
	PublicID     string `json:"redemption_public_id"`
	UserID       int64  `json:"user_id"`
	ItemPublicID string `json:"item_public_id"`
	Status       string `json:"status"`
}

func (p redemptionHandled) refs() Refs {
	return Refs{UserIDs: []int64{p.UserID}, ItemPublicIDs: []string{p.ItemPublicID}}
}

func (p redemptionHandled) render(n Names) (string, []Field) {
	return "兌換工單已處理", []Field{
		{K: "成員", V: userName(n, p.UserID)},
		{K: "商品", V: itemName(n, p.ItemPublicID)},
		{K: "結果", V: statusLabel(p.Status)},
		{K: "工單", V: sanitize(p.PublicID)},
	}
}

// statusLabel 把工單終態換成中文。
//
// 用封閉對照表而不是直接印 payload 的字串:那個欄位雖然來自我們自己的程式碼,
// 但它是**從資料庫讀出來的文字**。公告不轉印任何未經比對的字串,
// 認不得就說認不得。
func statusLabel(status string) string {
	switch status {
	case "fulfilled":
		return "已完成"
	case "rejected":
		return "已婉拒"
	case "cancelled":
		return "已取消"
	default:
		return "狀態未知"
	}
}

// ── 名稱與格式 ───────────────────────────────────────────────────────────

const (
	unknownUser = "未知成員"
	unknownItem = "未知商品"
	// maxNameRunes 是名稱在公告裡的長度上限。Discord 的欄位值上限是 1024,
	// 這裡收得更緊:公告卡片要能讀,而暱稱是使用者自己填的。
	maxNameRunes = 48
)

func userName(n Names, id int64) string {
	if s := sanitize(n.Users[id]); s != "" {
		return s
	}
	return unknownUser
}

func itemName(n Names, publicID string) string {
	if s := sanitize(n.Items[publicID]); s != "" {
		return s
	}
	return unknownItem
}

// money 把金額與幣別組成一格。金額是 int64(鐵則 3),幣別是 currencies.code。
func money(amount int64, currency string) string {
	c := sanitize(currency)
	if c == "" {
		return strconv.FormatInt(amount, 10)
	}
	return strconv.FormatInt(amount, 10) + " " + c
}

// sanitize 把任何要進公告的字串壓成安全的單行短字串。
//
// 三件事:控制字元(含換行)換成空白、壓掉多餘空白、超長截斷。
// 不做 Markdown 跳脫 —— 公告走嵌入卡,最壞情況是排版變醜;
// 真正危險的 @everyone 是靠「不放進 content」擋掉的,不是靠字串處理。
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true // 開頭的空白直接吃掉
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !space {
				b.WriteRune(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = false
	}
	out := strings.TrimSpace(b.String())
	runes := []rune(out)
	if len(runes) > maxNameRunes {
		return strings.TrimSpace(string(runes[:maxNameRunes])) + "…"
	}
	return out
}

// ChannelKeyFor 回傳 topic 對應的邏輯頻道用途;不在清單內回空字串。
//
// 匯出它是為了讓儲存層能先把一批事件要用到的用途收集起來、一次查完,
// 而不必為此把 discordChannels 這張表複製一份出去(專案第 9 條)。
func ChannelKeyFor(topic string) string { return discordChannels[topic] }

// levelUp 是升級公告的 payload(schemas/24)。
//
// 只帶等級與主體,不帶 XP 數字:公告是給整個頻道看的,
// 而「他有多少 XP」是個人檔案的事,貼在公開頻道沒有必要。
type levelUp struct {
	UserID    int64  `json:"user_id"`
	Subject   string `json:"subject"`
	FromLevel int32  `json:"from_level"`
	ToLevel   int32  `json:"to_level"`
}

func (p levelUp) refs() Refs { return Refs{UserIDs: []int64{p.UserID}} }

func (p levelUp) render(n Names) (string, []Field) {
	title := "升級了"
	if p.Subject == "pet" {
		title = "寵物升級了"
	}
	return title, []Field{
		{K: "成員", V: userName(n, p.UserID)},
		{K: "等級", V: fmt.Sprintf("Lv.%d → **Lv.%d**", p.FromLevel, p.ToLevel)},
	}
}
