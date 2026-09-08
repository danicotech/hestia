// Package market 定義物品(item_definitions / item_instances)與市集掛單寄售
// (market_listings / market_orders)的核心型別、錯誤與介面。
//
// 設計核心(schemas/09-items-market.md):
//   - 分類綁定:tradable 預設 false,只有 collectible / consumable 開;
//     badge / role / prize 永遠不可交易(成就不能用買的)
//   - max_supply 讓「全世界只有 1 個」成立 —— 發放必須是原子的檢查+遞增,
//     不是先查後寫(併發超發等於稀缺性失效)
//   - locked_by_trade_id 不可省:掛單當下就把物品鎖住,
//     否則同一件物品可以同時掛兩張單 / 同時賣給兩個人
//   - 成交 = 物品換手 + 三筆分錄 + 掛單 sold + 成交紀錄 + outbox,同一個 transaction
//
// 防洗點(schemas/09 明列,MMO 式交易的必要配套):手續費銷毀、交易門檻
// (等級 / 入群天數 / 帳號年齡)、單日交易額上限、user_restrictions 的 no_trade、
// 全額記帳可稽核(market_orders 保留買賣雙方與金額,任兩人的資金淨流向可查)。
//
// 錯誤透傳:餘額不足、冪等衝突等帳本層錯誤(ledger.ErrInsufficientBalance、
// ledger.ErrIdempotencyConflict、ledger.ErrInFlight)由實作原樣包裝上拋,
// 呼叫端用 errors.Is 判斷;本套件依分層規範只認標準庫,不重複定義同義錯誤。
package market

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Category 是物品分類的封閉枚舉(schemas/09)。
// 分類本身不決定可否交易——tradable 是 item_definitions 的欄位,
// 但慣例上只有 collectible / consumable 會開啟。
type Category string

const (
	// CategoryCollectible 收藏品(可交易)。
	CategoryCollectible Category = "collectible"
	// CategoryConsumable 消耗品(可交易)。
	CategoryConsumable Category = "consumable"
	// CategoryBadge 成就徽章(bind_on_acquire,不可交易)。
	CategoryBadge Category = "badge"
	// CategoryRole 身分組道具(不可交易)。
	CategoryRole Category = "role"
	// CategoryPrize 實體獎品(不可交易)。
	CategoryPrize Category = "prize"
)

// AcquiredVia 是物品取得管道的封閉枚舉(schemas/09)。
type AcquiredVia string

const (
	AcquiredViaShop       AcquiredVia = "shop"
	AcquiredViaTournament AcquiredVia = "tournament"
	AcquiredViaAdmin      AcquiredVia = "admin"
	AcquiredViaMarket     AcquiredVia = "market"
	AcquiredViaTrade      AcquiredVia = "trade"
)

var validAcquiredVia = map[AcquiredVia]struct{}{
	AcquiredViaShop: {}, AcquiredViaTournament: {}, AcquiredViaAdmin: {},
	AcquiredViaMarket: {}, AcquiredViaTrade: {},
}

// Valid 回報取得管道是否在枚舉內。
func (a AcquiredVia) Valid() bool { _, ok := validAcquiredVia[a]; return ok }

// ListingStatus 是掛單狀態的封閉枚舉,open 是唯一起點,其餘皆為終態。
type ListingStatus string

const (
	ListingOpen      ListingStatus = "open"
	ListingSold      ListingStatus = "sold"
	ListingCancelled ListingStatus = "cancelled"
	ListingExpired   ListingStatus = "expired"
)

// economy_configs 的 key(值一律整數 JSON)。
//
// 只有 ConfigFeeBps 目前在 seed(00014)裡(800 = 8%);其餘四個門檻鍵**尚未 seed**,
// 讀不到時視為 0 = 該項門檻/上限不啟用(marketpg 的 configInt 說明了為何是 fail-open)。
// 門檻鍵刻意用 trade_ 前綴而非 market_:它們是「玩家對玩家交易」的門檻,
// M3 後段的面交(trade_sessions)要沿用同一組值,不該有第二份權威。
const (
	// ConfigFeeBps 手續費基點(800 = 8%),銷毀。
	ConfigFeeBps = "market_fee_bps"
	// ConfigMinLevel 交易門檻:最低等級(取該使用者在各社群的最高 level)。
	ConfigMinLevel = "trade_min_level"
	// ConfigMinMemberDays 交易門檻:最低入群天數(space_members 的 MIN(joined_at) 起算,離群不歸零)。
	ConfigMinMemberDays = "trade_min_member_days"
	// ConfigMinAccountAgeDays 交易門檻:最低帳號年齡(users.created_at 起算)。
	ConfigMinAccountAgeDays = "trade_min_account_age_days"
	// ConfigDailyVolumeCap 單日交易額上限(同一使用者當日買+賣的成交額合計,0 = 不限)。
	ConfigDailyVolumeCap = "trade_daily_volume_cap"
)

// BpsDenominator 是基點的分母(10000 = 100%)。
const BpsDenominator = 10000

// FeeFor 依基點算手續費,**無條件捨去**(整數除法,truncate;price/bps 皆非負,
// 等同 floor)。捨去的方向是「對賣家有利」:賣家實收 price − fee,fee 少算一單位
// 就是賣家多拿一單位,平台少銷毀一單位——寧可少銷毀,也不要向賣家多收沒明說的錢。
// 於是 price 很小時 fee 可能為 0(例:price=1、bps=800 → 0),此時不寫手續費分錄。
//
// bps 必須在 [0, BpsDenominator];超出或會整數溢位時回錯,絕不悄悄夾住——
// 設定寫錯是部署問題,失敗出聲比默默算出奇怪的金額安全。
func FeeFor(price, bps int64) (int64, error) {
	if price < 0 {
		return 0, fmt.Errorf("price=%d 不可為負: %w", price, ErrInvalidRequest)
	}
	if bps < 0 || bps > BpsDenominator {
		return 0, fmt.Errorf("手續費基點 %d 不在 [0,%d]: %w", bps, BpsDenominator, ErrInvalidRequest)
	}
	if bps > 0 && price > math.MaxInt64/bps {
		return 0, fmt.Errorf("price=%d × bps=%d 溢位: %w", price, bps, ErrInvalidRequest)
	}
	return price * bps / BpsDenominator, nil
}

// MintParams 是一次物品發放請求。DefinitionID 與 DefinitionPublicID 擇一填:
// 站內呼叫端(賽事發獎、管理員發放)手上多半是內部 id,對外 API 只有 public_id。
type MintParams struct {
	DefinitionID       int64  // 內部 id;> 0 時優先
	DefinitionPublicID string // 對外 ULID;DefinitionID 為 0 時必填
	OwnerID            int64
	AcquiredVia        AcquiredVia
	// IdempotencyKey 必填:發放會消耗 max_supply 額度,重送兩次就多鑄一件。
	// 沒有天然 UNIQUE 可擋(同人可持有多件同定義物品),API 冪等鍵是唯一權威。
	IdempotencyKey string
}

// MintResult 是一次發放的結果;冪等重放時原樣回傳(JSON 存於冪等鍵)。
type MintResult struct {
	InstanceID         int64     `json:"instance_id"`
	InstancePublicID   string    `json:"instance_public_id"`
	DefinitionPublicID string    `json:"definition_public_id"`
	OwnerID            int64     `json:"owner_id"`
	Bound              bool      `json:"bound"`
	MintedCount        int32     `json:"minted_count"` // 本次發放後的累計發行量
	AcquiredAt         time.Time `json:"acquired_at"`
	Replayed           bool      `json:"-"`
}

// Item 是使用者持有的一件物品(含定義資訊)。
type Item struct {
	PublicID           string
	DefinitionPublicID string
	Name               string
	Category           Category
	Rarity             *string
	IconURL            *string
	Tradable           bool
	Bound              bool
	Quantity           int32
	// Locked = true 表示正被掛單/交易鎖住,不能再掛單。
	Locked      bool
	AcquiredAt  time.Time
	AcquiredVia *string
}

// CreateListingParams 是一次掛單請求。
//
// 刻意沒有冪等鍵:同一件物品最多只能有一張進行中的掛單,
// 這件事由 item_instances.locked_by_trade_id 在列鎖下保證(重送第二次回 ErrItemLocked)。
// 再加一套 API 冪等鍵就是同一個概念兩個權威(專案第 9 條)。
type CreateListingParams struct {
	SellerID             int64
	ItemInstancePublicID string
	Currency             string
	Price                int64      // > 0
	ExpiresAt            *time.Time // nil = 不過期
}

// CancelListingParams 是賣家取消掛單的請求。同樣不需冪等鍵:
// open → 終態的單向轉移在列鎖下本身就是防重權威,重複取消回 ErrListingNotOpen。
type CancelListingParams struct {
	ListingPublicID string
	ByUserID        int64 // 必須是掛單的賣家
}

// Listing 是一張掛單。
type Listing struct {
	PublicID             string
	SellerID             int64
	ItemInstancePublicID string
	Currency             string
	Price                int64
	Status               ListingStatus
	ListedAt             time.Time
	ExpiresAt            *time.Time
}

// BuyParams 是一次成交請求。
type BuyParams struct {
	BuyerID         int64
	ListingPublicID string
	// IdempotencyKey 必填:成交會動錢,連點/重送不得成交兩次。
	IdempotencyKey string
}

// Purchase 是一次成交的完整結果;冪等重放時原樣回傳(JSON 存於冪等鍵)。
//
// 金額關係(整數,見 FeeFor):買方付 Price、賣方實收 Price − Fee、Fee 銷毀。
type Purchase struct {
	OrderID              int64  `json:"order_id"`
	ListingPublicID      string `json:"listing_public_id"`
	ItemInstancePublicID string `json:"item_instance_public_id"`
	BuyerID              int64  `json:"buyer_id"`
	SellerID             int64  `json:"seller_id"`
	Currency             string `json:"currency"`
	Price                int64  `json:"price"`
	Fee                  int64  `json:"fee"`
	// BuyerEntryID / SellerEntryID / FeeEntryID 是三筆分錄的 id;
	// FeeEntryID = 0 表示 fee 捨去後為 0,未寫手續費分錄。
	BuyerEntryID  int64 `json:"buyer_entry_id"`
	SellerEntryID int64 `json:"seller_entry_id"`
	FeeEntryID    int64 `json:"fee_entry_id"`
	Replayed      bool  `json:"-"`
}

// 錯誤語意:呼叫端據此決定回應文案(都是 4xx 級,不該盲目重試)。
var (
	// ErrInvalidRequest 表示請求參數不合法。
	ErrInvalidRequest = errors.New("請求參數不合法")
	// ErrUserNotFound 表示使用者不存在或已刪除。
	ErrUserNotFound = errors.New("使用者不存在")
	// ErrDefinitionNotFound 表示物品定義不存在。
	ErrDefinitionNotFound = errors.New("物品定義不存在")
	// ErrSupplyExhausted 表示已達 max_supply,不能再發放(稀缺性的硬邊界)。
	ErrSupplyExhausted = errors.New("物品已達發行上限")
	// ErrItemNotFound 表示物品實例不存在。
	ErrItemNotFound = errors.New("物品不存在")
	// ErrNotItemOwner 表示物品不屬於請求的使用者。
	ErrNotItemOwner = errors.New("物品不屬於該使用者")
	// ErrItemBound 表示物品已綁定持有人,不可交易。
	ErrItemBound = errors.New("物品已綁定,不可交易")
	// ErrItemNotTradable 表示物品定義 tradable=false(身分組/徽章/賽事道具/實體獎品)。
	ErrItemNotTradable = errors.New("物品不可交易")
	// ErrItemLocked 表示物品已被其他掛單/交易鎖住。
	ErrItemLocked = errors.New("物品已被掛單或交易鎖住")
	// ErrListingNotFound 表示掛單不存在。
	ErrListingNotFound = errors.New("掛單不存在")
	// ErrListingNotOpen 表示掛單已成交/已取消/已過期(狀態機單向)。
	ErrListingNotOpen = errors.New("掛單已結束")
	// ErrListingExpired 表示掛單已過 expires_at。
	ErrListingExpired = errors.New("掛單已過期")
	// ErrNotListingOwner 表示掛單不屬於請求的使用者。
	ErrNotListingOwner = errors.New("掛單不屬於該使用者")
	// ErrSelfPurchase 表示賣家買自己的掛單(左手換右手,洗點的最短路徑)。
	ErrSelfPurchase = errors.New("不能購買自己的掛單")
	// ErrTradeRestricted 表示買賣任一方有生效中的 no_trade 限制。
	ErrTradeRestricted = errors.New("交易權限已被限制")
	// ErrTradeThresholdNotMet 表示未達交易門檻(等級 / 入群天數 / 帳號年齡)。
	ErrTradeThresholdNotMet = errors.New("未達交易門檻")
	// ErrDailyLimitExceeded 表示超過單日交易額上限。
	ErrDailyLimitExceeded = errors.New("超過單日交易額上限")
	// ErrItemStateConflict 表示物品狀態與掛單矛盾(擁有者已變、鎖不指向本掛單)。
	// 正常流程到不了,出現代表資料被繞過服務層改動——失敗出聲,絕不硬換手。
	ErrItemStateConflict = errors.New("物品狀態與掛單矛盾")
	// ErrLedgerStateConflict 表示市集紀錄與帳本狀態矛盾(如:掛單仍 open 但成交冪等鍵已有結果)。
	ErrLedgerStateConflict = errors.New("市集紀錄與帳本狀態矛盾")
)

// Service 是市集與物品的對外介面。M1 為 in-process 實作(marketpg)。
// 每個方法的全部效果——物品側寫入、掛單、成交紀錄、帳本分錄、餘額、outbox——
// 都在單一 transaction,要嘛全成立要嘛全不成立。
type Service interface {
	// MintItem 發放一件物品實例。max_supply 非 NULL 時,
	// 檢查與 minted_count 遞增是同一個原子敘述(併發不得超發);
	// bind_on_acquire=true 的定義,實例出生即 bound。
	MintItem(ctx context.Context, p MintParams) (*MintResult, error)

	// ListUserItems 列出使用者持有的物品(含定義資訊與是否被鎖)。
	ListUserItems(ctx context.Context, userID int64) ([]Item, error)

	// CreateListing 掛單寄售:物品須屬於賣家、未綁定、定義 tradable、未被鎖;
	// 成立時同 tx 把 locked_by_trade_id 指向本掛單。
	CreateListing(ctx context.Context, p CreateListingParams) (*Listing, error)

	// CancelListing 賣家取消掛單:解鎖物品 + 狀態 cancelled。
	CancelListing(ctx context.Context, p CancelListingParams) (*Listing, error)

	// Buy 成交:三筆分錄(買方 market_purchase / 賣方 market_sale_income /
	// 手續費 market_fee 銷毀)+ 物品換手 + 掛單 sold + market_orders + outbox
	// 'market.sold',全部同一個 transaction,鎖序依 user_id 升冪。
	Buy(ctx context.Context, p BuyParams) (*Purchase, error)
}
