// Package readmodel 定義「讀取側檢視」的共用型別:入口層要顯示什麼,
// 儲存層就填什麼。
//
// 為什麼獨立一個 core 套件:Go 的介面是名目型別比對。讀取側的 port 宣告在
// transport(使用端),但實作在 infrastructure/storage,而 storage **不能**
// import transport(depguard,方向也錯)——欄位長得一模一樣的 struct 也滿足
// 不了介面。把檢視型別放在雙方都能依賴的 core,兩邊才接得起來。
//
// 邊界:這裡只有「對外顯示用」的欄位。
//   - 絕不出現內部 BIGINT id(鐵則 5);唯一例外是管理端的 LedgerEntryView.EntryRef
//     (token_entries 沒有 public_id,已裁決的管理端例外)。
//   - 金額一律 int64(鐵則 3)。
//   - 寫入側的參數型別不放這裡——那是 core/platform/<feature> 各自的事。
package readmodel

import (
	"time"

	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// ProfileView 是使用者檔案的對外檢視(platform.users 的可公開欄位)。
type ProfileView struct {
	PublicID          string
	DisplayName       string
	AvatarURL         string
	Timezone          string
	TimezoneChangedAt *time.Time
	CreatedAt         time.Time
}

// PrivacyView 是兩級退出設定的對外檢視(schemas/02-identity.md)。
//
// **沒有設定列 = 兩者皆 false**,不是錯誤:user_privacy_settings 只在使用者
// 真的動過設定時才有列。讀取側替沒設定過的人回預設值,是這個語意的唯一實作
// 位置 —— 每個呼叫端各自 fallback 就會有 N 份「預設值是什麼」的定義。
//
// 兩個旗標的語意差異在 proto 註解裡寫死(me.proto 的 PrivacySettings):
// OptOutLogging 是「不記錄原始內容」,**XP 與統計計數照常**。
type PrivacyView struct {
	OptOutLogging  bool
	OptOutAICorpus bool
	// UpdatedAt 是上次變更時間;nil = 從未設定過(回的是預設值)。
	UpdatedAt *time.Time
}

// PrivacyUpdate 是一次隱私設定變更。
//
// 兩個欄位都是指標:nil = **這次不動這一項**,不是「設成 false」。
// 用值型別的話,想關掉一個旗標就必須連另一個一起送,而呼叫端手上
// 那份「另一個」可能已經過期 —— 那會變成靜靜地把別的設定覆蓋回去。
type PrivacyUpdate struct {
	OptOutLogging  *bool
	OptOutAICorpus *bool
}

// BalanceView 是單一幣別的餘額。餘額的權威是 user_balances(帳本的快取),
// 這裡只是把它讀出來顯示。
type BalanceView struct {
	Currency string
	Amount   int64
}

// ItemView 是商品的對外檢視。stock 刻意不揭露(M1 不啟用全域限量)。
type ItemView struct {
	PublicID            string
	Name                string
	Description         string
	Fulfillment         shop.Fulfillment
	Currency            string
	Price               int64
	DurationDays        *int32
	PerUserLimit        *int32
	RefundWindowSeconds int32
	ListedAt            *time.Time
	DelistedAt          *time.Time
}

// EntitlementView 是權益的對外檢視。
type EntitlementView struct {
	PublicID        string
	ItemPublicID    string
	ItemName        string
	Fulfillment     shop.Fulfillment
	GrantedAt       time.Time
	ExpiresAt       *time.Time // nil = 永久
	RefundableUntil *time.Time // nil = 不可退
	RevokedAt       *time.Time // nil = 仍有效
}

// RedemptionView 是兌換工單的對外檢視。
type RedemptionView struct {
	PublicID     string
	ItemPublicID string
	ItemName     string
	Status       shop.RedemptionStatus
	Note         string
	CreatedAt    time.Time
	HandledAt    *time.Time
}

// LedgerEntryView 是帳本分錄的**管理端**檢視。
//
// EntryRef 是分錄的對外參照:token_entries 沒有 public_id,管理操作(退款)
// 只能用它定址——這是已裁決的管理端例外(鐵則 5 例外條款)。用 string 承載,
// 將來若補上 public_id 只換內容不換型別。一般使用者的 API 不會回傳它。
type LedgerEntryView struct {
	EntryRef      string
	UserPublicID  string
	Currency      string
	Amount        int64 // 正 = 入帳,負 = 扣款
	Reason        string
	CreatedAt     time.Time
	Refunded      bool   // 這筆扣款是否已被沖銷(同一分錄只能退一次)
	ActorPublicID string // 管理員操作才有
}

// LedgerEntriesQuery 是分錄查詢條件。UserID 是內部 id:
// public_id 的解析在入口層做完了,儲存層不必再查一次。
type LedgerEntriesQuery struct {
	UserID   int64
	After    *time.Time // 含,nil = 不限
	Before   *time.Time // 不含,nil = 不限
	PageSize int32      // 入口層已夾在 1..200
	// PageToken 是實作自訂的不透明游標(建議 keyset,不要 OFFSET)。
	PageToken string
}

// LedgerEntriesPage 是一頁分錄。NextPageToken 為空表示沒有下一頁。
type LedgerEntriesPage struct {
	Entries       []LedgerEntryView
	NextPageToken string
}
