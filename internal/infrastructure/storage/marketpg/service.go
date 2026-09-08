// Package marketpg 是 market.Service 的 Postgres 實作。
//
// 每個操作的全部效果——物品側寫入(item_definitions.minted_count / item_instances)、
// 掛單、成交紀錄、帳本分錄、餘額、outbox、冪等鍵——都在同一個 transaction,
// 要嘛都在要嘛都不在。
//
// 成交 = 三筆分錄(ledger-invariants 第五條),金額關係:
//
//	買方 market_purchase     −price
//	賣方 market_sale_income  +price
//	賣方 market_fee          −fee     ← 銷毀,不進任何人的餘額
//
// 賣方兩筆的淨額就是鐵則寫的 +(price − fee)。刻意拆成兩筆而不是「賣方 +(price−fee)
// 再另記一筆 −fee」:後者三筆加總是 −2fee,與實際流失的 fee 對不上,
// Reconcile(SUM(entries) = balance)會誤報。拆兩筆則每個人的分錄總和恰等於其餘額變化,
// 全域 SUM 恰好減少 fee = 真正銷毀的量。
// fee 記在賣方名下也符合經濟事實(賣家實收 price − fee),且三筆金額分別對得上
// market_orders 的 price / price / fee 欄位,稽核不必再換算。
//
// 冪等設計(一個概念一個權威):
//   - Buy / MintItem 沒有天然 UNIQUE 可擋(同人可重複買不同掛單、可持有多件同定義物品),
//     API 冪等鍵是權威。
//   - CreateListing 的權威是 item_instances.locked_by_trade_id(一件物品同時只能有一張單);
//     CancelListing 的權威是 open → 終態的單向轉移。兩者都不另設冪等鍵。
//   - 帳本介面必填冪等鍵,故傳派生鍵(命名空間化的 API 鍵 + ":ledger");
//     帳本若回報 Replayed 而市集側判定是首次 → 兩個權威矛盾,回 ErrLedgerStateConflict,
//     絕不回成功但錢不對。
//
// 冪等鍵命名空間:idempotency_keys 是全平台單一 PK,客戶端提供的鍵一律加服務端前綴
// (mintKeyPrefix / buyKeyPrefix)後才佔鍵/查鍵/進帳本,結構上不可能撞到別的服務
// (同一手法見 shoppg / admineconpg)。
//
// 鎖序(全系統同向,死鎖在結構上不可能發生):
//
//	users(user_id 升冪)→ market_listings → item_instances → user_balances(user_id 升冪)
//
// 買賣雙方併發互買時,兩邊都先鎖 users 且順序相同,不會出現 A 等 B、B 等 A。
package marketpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/market"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 冪等鍵命名空間與帳本派生鍵字尾(見 package doc)。
const (
	mintKeyPrefix   = "market.mint:api:"
	buyKeyPrefix    = "market.buy:api:"
	ledgerKeySuffix = ":ledger"
)

// refTypeMarketOrder 是三筆成交分錄的 ref_type:全部指向同一張 market_orders 列,
// 由分錄反查成交脈絡(買賣雙方、掛單、手續費)只要一跳。
const refTypeMarketOrder = "market_order"

// topicSold 是成交事件(通知買賣雙方、更新展示用讀模型)。
const topicSold = "market.sold"

// txLedger 是本服務對帳本的最小依賴:與市集側寫入同 tx 動錢。
// 實際注入 *ledgerpg.Service(ApplyInTx 刻意不在 ledger.Ledger interface 上)。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// Service 是 market.Service 的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	led  txLedger
}

var _ market.Service = (*Service)(nil)

// New 建立市集服務。led 傳 ledgerpg.New(pool) 的結果。
func New(pool *pgxpool.Pool, led txLedger) *Service {
	return &Service{pool: pool, q: db.New(pool), led: led}
}

// MintItem 發放一件物品實例。流程:
//
//	佔冪等鍵 → 讀定義 → 原子檢查+遞增 minted_count(超發保護)
//	→ INSERT item_instances(bound = bind_on_acquire)→ 存冪等 response → commit
//
// 冪等鍵刻意在最前面佔:發放沒有可先取得的串行化列鎖(定義列的鎖要到 UPDATE 才拿到,
// 而重放判定必須發生在遞增之前),所以用「先插鍵、撞鍵即重放」——與 ledgerpg.Apply 同型。
func (s *Service) MintItem(ctx context.Context, p market.MintParams) (*market.MintResult, error) {
	if p.OwnerID <= 0 || p.IdempotencyKey == "" {
		return nil, fmt.Errorf("owner_id / idempotency_key 必填: %w", market.ErrInvalidRequest)
	}
	if p.DefinitionID <= 0 && p.DefinitionPublicID == "" {
		return nil, fmt.Errorf("definition_id 與 definition_public_id 至少填一個: %w", market.ErrInvalidRequest)
	}
	if p.AcquiredVia != "" && !p.AcquiredVia.Valid() {
		return nil, fmt.Errorf("acquired_via %q 不在枚舉內: %w", p.AcquiredVia, market.ErrInvalidRequest)
	}
	hash := requestHash("market.mint",
		strconv.FormatInt(p.DefinitionID, 10), p.DefinitionPublicID,
		strconv.FormatInt(p.OwnerID, 10), string(p.AcquiredVia))
	key := mintKeyPrefix + p.IdempotencyKey

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{Key: key, RequestHash: hash}); err != nil {
		if isUniqueViolation(err, "idempotency_keys_pkey") {
			// 先 rollback 釋放本 tx 的連線再走重放路徑:不釋放就會拿著廢棄連線
			// 再向池要第二條(hold-and-wait),併發重送 >= MaxConns 時整池死鎖(ledgerpg 的實測教訓)。
			_ = tx.Rollback(ctx)
			return replayKey[market.MintResult](ctx, s.q, key, hash)
		}
		return nil, fmt.Errorf("佔冪等鍵: %w", err)
	}

	def, err := s.definition(ctx, qtx, p)
	if err != nil {
		return nil, err
	}
	if _, err := qtx.GetUserByID(ctx, p.OwnerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("user=%d: %w", p.OwnerID, market.ErrUserNotFound)
		}
		return nil, fmt.Errorf("讀使用者 %d: %w", p.OwnerID, err)
	}

	// ── 超發保護:檢查與遞增是同一個敘述,0 rows = 額度用完(見 market.sql)
	minted, err := qtx.MintDefinitionSupply(ctx, def.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("definition=%s 已發行 %d/%v: %w",
				def.PublicID, def.MintedCount, def.MaxSupply, market.ErrSupplyExhausted)
		}
		return nil, fmt.Errorf("遞增發行量: %w", err)
	}

	instPublicID, err := ulid.New()
	if err != nil {
		return nil, fmt.Errorf("產生物品 public_id: %w", err)
	}
	var via *string
	if p.AcquiredVia != "" {
		via = ptr(string(p.AcquiredVia))
	}
	inst, err := qtx.InsertItemInstance(ctx, db.InsertItemInstanceParams{
		PublicID:     instPublicID,
		DefinitionID: def.ID,
		OwnerID:      p.OwnerID,
		// bind_on_acquire:成就徽章一發放即綁定,否則成就可以用買的(schemas/09)。
		Bound:       minted.BindOnAcquire,
		AcquiredVia: via,
	})
	if err != nil {
		return nil, fmt.Errorf("寫入物品實例: %w", err)
	}

	result := &market.MintResult{
		InstanceID:         inst.ID,
		InstancePublicID:   inst.PublicID,
		DefinitionPublicID: minted.PublicID,
		OwnerID:            p.OwnerID,
		Bound:              minted.BindOnAcquire,
		MintedCount:        minted.MintedCount,
		AcquiredAt:         inst.AcquiredAt,
	}
	if err := saveResponse(ctx, qtx, key, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// definition 依 MintParams 解析物品定義(id 優先於 public_id)。
// 先確認存在,遞增才能把 0 rows 明確解讀成「額度用完」而不是「定義不存在」。
func (s *Service) definition(ctx context.Context, qtx *db.Queries, p market.MintParams) (*db.GetItemDefinitionByIDRow, error) {
	if p.DefinitionID > 0 {
		row, err := qtx.GetItemDefinitionByID(ctx, p.DefinitionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("definition=%d: %w", p.DefinitionID, market.ErrDefinitionNotFound)
			}
			return nil, fmt.Errorf("讀物品定義 %d: %w", p.DefinitionID, err)
		}
		return &row, nil
	}
	row, err := qtx.GetItemDefinitionByPublicID(ctx, p.DefinitionPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("definition=%s: %w", p.DefinitionPublicID, market.ErrDefinitionNotFound)
		}
		return nil, fmt.Errorf("讀物品定義 %s: %w", p.DefinitionPublicID, err)
	}
	return (*db.GetItemDefinitionByIDRow)(&row), nil
}

// ListUserItems 列出使用者持有的物品(含定義資訊與是否被掛單鎖住)。
func (s *Service) ListUserItems(ctx context.Context, userID int64) ([]market.Item, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", market.ErrInvalidRequest)
	}
	rows, err := s.q.ListUserItemInstances(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("讀使用者物品 %d: %w", userID, err)
	}
	items := make([]market.Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, market.Item{
			PublicID:           r.PublicID,
			DefinitionPublicID: r.DefinitionPublicID,
			Name:               r.Name,
			Category:           market.Category(r.Category),
			Rarity:             r.Rarity,
			IconURL:            r.IconUrl,
			Tradable:           r.Tradable,
			Bound:              r.Bound,
			Quantity:           r.Quantity,
			Locked:             r.Locked,
			AcquiredAt:         r.AcquiredAt,
			AcquiredVia:        r.AcquiredVia,
		})
	}
	return items, nil
}

// CreateListing 掛單寄售。流程:
//
//	鎖賣家 users 列 → 防洗點檢查(no_trade / 交易門檻)→ 鎖物品實例列
//	→ 屬於賣家 + 未綁定 + 定義 tradable + 未被鎖 → INSERT market_listings
//	→ locked_by_trade_id 指向本掛單 → commit
//
// 「掛單即上鎖」不可省:沒鎖就能把同一件物品同時掛兩張單、賣給兩個人(schemas/09 明文)。
func (s *Service) CreateListing(ctx context.Context, p market.CreateListingParams) (*market.Listing, error) {
	switch {
	case p.SellerID <= 0 || p.ItemInstancePublicID == "" || p.Currency == "":
		return nil, fmt.Errorf("seller_id / item_instance_public_id / currency 必填: %w", market.ErrInvalidRequest)
	case p.Price <= 0:
		return nil, fmt.Errorf("price 必須為正整數,得到 %d: %w", p.Price, market.ErrInvalidRequest)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := lockUser(ctx, qtx, p.SellerID); err != nil {
		return nil, err
	}
	limits, err := loadLimits(ctx, qtx)
	if err != nil {
		return nil, err
	}
	// 掛單不動錢,故只驗 no_trade 與交易門檻;單日交易額上限在成交時才算。
	if err := checkEligibility(ctx, qtx, p.SellerID, p.Currency, limits, 0); err != nil {
		return nil, err
	}

	inst, err := qtx.LockItemInstanceByPublicID(ctx, p.ItemInstancePublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("item=%s: %w", p.ItemInstancePublicID, market.ErrItemNotFound)
		}
		return nil, fmt.Errorf("鎖物品 %s: %w", p.ItemInstancePublicID, err)
	}
	switch {
	case inst.OwnerID != p.SellerID:
		return nil, fmt.Errorf("item=%s 屬於 user=%d 而非 %d: %w",
			p.ItemInstancePublicID, inst.OwnerID, p.SellerID, market.ErrNotItemOwner)
	case inst.Bound:
		return nil, fmt.Errorf("item=%s: %w", p.ItemInstancePublicID, market.ErrItemBound)
	case !inst.Tradable:
		return nil, fmt.Errorf("item=%s 分類 %s 不可交易: %w",
			p.ItemInstancePublicID, inst.Category, market.ErrItemNotTradable)
	case inst.LockedByTradeID != nil:
		return nil, fmt.Errorf("item=%s 已被掛單 %d 鎖住: %w",
			p.ItemInstancePublicID, *inst.LockedByTradeID, market.ErrItemLocked)
	}

	publicID, err := ulid.New()
	if err != nil {
		return nil, fmt.Errorf("產生掛單 public_id: %w", err)
	}
	row, err := qtx.InsertMarketListing(ctx, db.InsertMarketListingParams{
		PublicID:       publicID,
		SellerID:       p.SellerID,
		ItemInstanceID: inst.ID,
		Currency:       p.Currency,
		Price:          p.Price,
		ExpiresAt:      p.ExpiresAt,
	})
	if err != nil {
		if isForeignKeyViolation(err) { // 幣別不存在(currencies FK)
			return nil, fmt.Errorf("currency=%s 不存在: %w", p.Currency, market.ErrInvalidRequest)
		}
		return nil, fmt.Errorf("寫入掛單: %w", err)
	}
	// 上鎖:WHERE locked_by_trade_id IS NULL 是最後防線(我們已持有實例列鎖並檢查過)。
	n, err := qtx.LockItemInstanceForTrade(ctx, db.LockItemInstanceForTradeParams{ID: inst.ID, TradeID: row.ID})
	if err != nil {
		return nil, fmt.Errorf("鎖定物品: %w", err)
	}
	if n != 1 {
		return nil, fmt.Errorf("item=%s 鎖定失敗: %w", p.ItemInstancePublicID, market.ErrItemLocked)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &market.Listing{
		PublicID:             row.PublicID,
		SellerID:             row.SellerID,
		ItemInstancePublicID: inst.PublicID,
		Currency:             row.Currency,
		Price:                row.Price,
		Status:               market.ListingStatus(row.Status),
		ListedAt:             row.ListedAt,
		ExpiresAt:            row.ExpiresAt,
	}, nil
}

// CancelListing 賣家取消掛單:鎖掛單列 → 賣家本人 + 仍 open → 解鎖物品 → status='cancelled'。
// 過期但尚未被標記的掛單也走這條路(讓賣家隨時能把物品拿回來)。
func (s *Service) CancelListing(ctx context.Context, p market.CancelListingParams) (*market.Listing, error) {
	if p.ListingPublicID == "" || p.ByUserID <= 0 {
		return nil, fmt.Errorf("listing_public_id / by_user_id 必填: %w", market.ErrInvalidRequest)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	l, err := qtx.LockListingByPublicID(ctx, p.ListingPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("listing=%s: %w", p.ListingPublicID, market.ErrListingNotFound)
		}
		return nil, fmt.Errorf("鎖掛單 %s: %w", p.ListingPublicID, err)
	}
	if l.SellerID != p.ByUserID {
		return nil, fmt.Errorf("listing=%s 屬於 user=%d 而非 %d: %w",
			p.ListingPublicID, l.SellerID, p.ByUserID, market.ErrNotListingOwner)
	}
	if l.Status != string(market.ListingOpen) {
		return nil, fmt.Errorf("listing=%s 狀態 %s: %w", p.ListingPublicID, l.Status, market.ErrListingNotOpen)
	}

	n, err := qtx.UnlockItemInstance(ctx, db.UnlockItemInstanceParams{ID: l.ItemInstanceID, TradeID: l.ID})
	if err != nil {
		return nil, fmt.Errorf("解鎖物品: %w", err)
	}
	if n != 1 { // open 的掛單一定持有該物品的鎖;不成立代表資料被繞過服務層改動
		return nil, fmt.Errorf("listing=%s 的物品鎖不存在: %w", p.ListingPublicID, market.ErrItemStateConflict)
	}
	if n, err := qtx.UpdateListingStatus(ctx, db.UpdateListingStatusParams{
		ID: l.ID, Status: string(market.ListingCancelled),
	}); err != nil {
		return nil, fmt.Errorf("更新掛單狀態: %w", err)
	} else if n != 1 {
		return nil, fmt.Errorf("listing=%s: %w", p.ListingPublicID, market.ErrListingNotOpen)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &market.Listing{
		PublicID:  l.PublicID,
		SellerID:  l.SellerID,
		Currency:  l.Currency,
		Price:     l.Price,
		Status:    market.ListingCancelled,
		ListedAt:  l.ListedAt,
		ExpiresAt: l.ExpiresAt,
	}, nil
}

// Buy 成交。單一 transaction,流程(順序即鐵則):
//
//	讀掛單(不鎖)取得賣家 → 依 user_id 升冪鎖 users(買賣雙方)
//	→ 冪等重放檢查 → 佔冪等鍵 → 鎖掛單列並重新驗證(open / 未過期 / 非自買)
//	→ 防洗點(no_trade / 交易門檻 / 單日交易額上限)→ 鎖物品實例列並驗證鎖指向本掛單
//	→ 算手續費(整數捨去)→ INSERT market_orders → 經帳本三筆分錄(ApplyInTx,同 tx)
//	→ 物品換手 + 清鎖 → 掛單 sold → outbox 'market.sold' → 存冪等 response → commit
//
// market_orders 先於分錄:分錄的 ref_id 需要 order id,同 tx 內順序不影響原子性,
// 而「三筆分錄精確指向同一張成交單」讓稽核與日後沖銷都不必靠時間猜測(同 shoppg 的做法)。
func (s *Service) Buy(ctx context.Context, p market.BuyParams) (*market.Purchase, error) {
	if p.BuyerID <= 0 || p.ListingPublicID == "" || p.IdempotencyKey == "" {
		return nil, fmt.Errorf("buyer_id / listing_public_id / idempotency_key 必填: %w", market.ErrInvalidRequest)
	}
	hash := requestHash("market.buy", strconv.FormatInt(p.BuyerID, 10), p.ListingPublicID)
	key := buyKeyPrefix + p.IdempotencyKey

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// ── 先讀掛單決定要鎖哪些人:鎖序需要事先知道鎖的集合。
	// 這裡讀到的一切都在取得列鎖後重新驗證,不當判斷依據。
	pre, err := qtx.GetListingByPublicID(ctx, p.ListingPublicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("listing=%s: %w", p.ListingPublicID, market.ErrListingNotFound)
		}
		return nil, fmt.Errorf("讀掛單 %s: %w", p.ListingPublicID, err)
	}
	if pre.SellerID == p.BuyerID {
		return nil, fmt.Errorf("listing=%s: %w", p.ListingPublicID, market.ErrSelfPurchase)
	}

	// ── 鎖 users:買賣雙方,user_id 升冪(ledger-invariants 第四條)。
	// 互買併發(A 買 B 的單、B 買 A 的單)靠這個順序在結構上不可能死鎖。
	ids := []int64{p.BuyerID, pre.SellerID}
	slices.Sort(ids)
	for _, id := range ids {
		if err := lockUser(ctx, qtx, id); err != nil {
			return nil, err
		}
	}

	// ── 冪等:users 鎖之後查鍵,前一次同 key 的 tx 必已 commit 或已消失。
	if res, done, err := replayFromKey[market.Purchase](ctx, qtx, key, hash); done {
		return res, err
	}
	if err := qtx.InsertIdempotencyKey(ctx, db.InsertIdempotencyKeyParams{Key: key, RequestHash: hash}); err != nil {
		if isUniqueViolation(err, "idempotency_keys_pkey") {
			// users 鎖已擋掉同買家的競態;走到這裡是同鍵被不同買家/掛單重用(hash 必不同)。
			return nil, fmt.Errorf("key=%s: %w", key, ledger.ErrIdempotencyConflict)
		}
		return nil, fmt.Errorf("佔冪等鍵: %w", err)
	}

	// ── 鎖掛單:同一張單的併發買家在此串行化,後到者看到 sold 即拒絕。
	l, err := qtx.LockListingByID(ctx, pre.ID)
	if err != nil {
		return nil, fmt.Errorf("鎖掛單 %d: %w", pre.ID, err)
	}
	switch {
	case l.SellerID == p.BuyerID:
		return nil, fmt.Errorf("listing=%s: %w", p.ListingPublicID, market.ErrSelfPurchase)
	case l.Status != string(market.ListingOpen):
		return nil, fmt.Errorf("listing=%s 狀態 %s: %w", p.ListingPublicID, l.Status, market.ErrListingNotOpen)
	case l.ExpiresAt != nil && !l.DbNow.Before(*l.ExpiresAt):
		return nil, fmt.Errorf("listing=%s 已於 %s 過期: %w",
			p.ListingPublicID, l.ExpiresAt.Format(time.RFC3339), market.ErrListingExpired)
	case l.SellerID != pre.SellerID: // seller_id 不可變;不成立代表資料被繞過服務層改動
		return nil, fmt.Errorf("listing=%s 賣家已變動: %w", p.ListingPublicID, market.ErrItemStateConflict)
	}

	// ── 防洗點(schemas/09):買賣雙方都要過。
	limits, err := loadLimits(ctx, qtx)
	if err != nil {
		return nil, err
	}
	if err := checkEligibility(ctx, qtx, p.BuyerID, l.Currency, limits, l.Price); err != nil {
		return nil, fmt.Errorf("買方 %d: %w", p.BuyerID, err)
	}
	if err := checkEligibility(ctx, qtx, l.SellerID, l.Currency, limits, l.Price); err != nil {
		return nil, fmt.Errorf("賣方 %d: %w", l.SellerID, err)
	}

	// ── 鎖物品:鎖必須指向本掛單,否則不換手(ledger-invariants 第五條)。
	inst, err := qtx.LockItemInstanceByID(ctx, l.ItemInstanceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("listing=%s 的物品不存在: %w", p.ListingPublicID, market.ErrItemNotFound)
		}
		return nil, fmt.Errorf("鎖物品 %d: %w", l.ItemInstanceID, err)
	}
	switch {
	case inst.OwnerID != l.SellerID:
		return nil, fmt.Errorf("item=%s 已不屬於賣家 %d: %w",
			inst.PublicID, l.SellerID, market.ErrItemStateConflict)
	case inst.LockedByTradeID == nil || *inst.LockedByTradeID != l.ID:
		return nil, fmt.Errorf("item=%s 的鎖未指向掛單 %d: %w",
			inst.PublicID, l.ID, market.ErrItemStateConflict)
	case inst.Bound:
		return nil, fmt.Errorf("item=%s: %w", inst.PublicID, market.ErrItemBound)
	case !inst.Tradable: // 定義事後被改成不可交易,掛單就地失效
		return nil, fmt.Errorf("item=%s 分類 %s: %w", inst.PublicID, inst.Category, market.ErrItemNotTradable)
	}

	// ── 手續費:整數捨去(見 market.FeeFor);捨去後為 0 就不寫分錄(帳本禁止 0 金額)。
	feeBps, found, err := configInt(ctx, qtx, market.ConfigFeeBps)
	if err != nil {
		return nil, err
	}
	if !found { // seed(00014)有這個 key;讀不到是部署問題,不猜一個預設值把手續費靜靜關掉
		return nil, fmt.Errorf("設定 %s 不存在,無法計算手續費", market.ConfigFeeBps)
	}
	fee, err := market.FeeFor(l.Price, feeBps)
	if err != nil {
		return nil, err
	}

	ord, err := qtx.InsertMarketOrder(ctx, db.InsertMarketOrderParams{
		ListingID: l.ID, BuyerID: p.BuyerID, SellerID: l.SellerID, Price: l.Price, Fee: fee,
	})
	if err != nil {
		return nil, fmt.Errorf("寫入成交紀錄: %w", err)
	}

	// ── 三筆分錄,同 tx(順序:賣方先入帳再扣手續費,中間值不會為負)
	ops := []ledger.Op{
		{UserID: p.BuyerID, Currency: l.Currency, Amount: -l.Price,
			Reason: ledger.ReasonMarketPurchase, RefType: ptr(refTypeMarketOrder), RefID: ptr(ord.ID)},
		{UserID: l.SellerID, Currency: l.Currency, Amount: l.Price,
			Reason: ledger.ReasonMarketSaleIncome, RefType: ptr(refTypeMarketOrder), RefID: ptr(ord.ID)},
	}
	if fee > 0 {
		ops = append(ops, ledger.Op{UserID: l.SellerID, Currency: l.Currency, Amount: -fee,
			Reason: ledger.ReasonMarketFee, RefType: ptr(refTypeMarketOrder), RefID: ptr(ord.ID)})
	}
	applied, err := s.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
		IdempotencyKey: key + ledgerKeySuffix, RequestHash: hash, Ops: ops,
	})
	if err != nil {
		return nil, fmt.Errorf("帳本動錢: %w", err) // ErrInsufficientBalance 等原樣包裝透傳
	}
	if applied.Replayed {
		return nil, fmt.Errorf("帳本鍵 %s 已有結果但市集側為首次: %w",
			key+ledgerKeySuffix, market.ErrLedgerStateConflict)
	}

	// ── 物品換手 + 清鎖(WHERE 再比對原持有人與鎖,是最後防線)
	n, err := qtx.TransferItemInstance(ctx, db.TransferItemInstanceParams{
		ID: inst.ID, NewOwnerID: p.BuyerID, SellerID: l.SellerID, TradeID: l.ID,
		Bound: inst.BindOnAcquire,
	})
	if err != nil {
		return nil, fmt.Errorf("物品換手: %w", err)
	}
	if n != 1 {
		return nil, fmt.Errorf("item=%s 換手失敗: %w", inst.PublicID, market.ErrItemStateConflict)
	}
	if n, err := qtx.UpdateListingStatus(ctx, db.UpdateListingStatusParams{
		ID: l.ID, Status: string(market.ListingSold),
	}); err != nil {
		return nil, fmt.Errorf("更新掛單狀態: %w", err)
	} else if n != 1 {
		return nil, fmt.Errorf("listing=%s: %w", p.ListingPublicID, market.ErrListingNotOpen)
	}

	result := &market.Purchase{
		OrderID:              ord.ID,
		ListingPublicID:      l.PublicID,
		ItemInstancePublicID: inst.PublicID,
		BuyerID:              p.BuyerID,
		SellerID:             l.SellerID,
		Currency:             l.Currency,
		Price:                l.Price,
		Fee:                  fee,
		BuyerEntryID:         applied.Entries[0].EntryID,
		SellerEntryID:        applied.Entries[1].EntryID,
	}
	if fee > 0 {
		result.FeeEntryID = applied.Entries[2].EntryID
	}

	// ── outbox 與領域變更同 tx(鐵則第六條)
	if err := emit(ctx, qtx, topicSold, map[string]any{
		"order_id":                ord.ID,
		"listing_public_id":       l.PublicID,
		"item_instance_public_id": inst.PublicID,
		"buyer_id":                p.BuyerID,
		"seller_id":               l.SellerID,
		"currency":                l.Currency,
		"price":                   l.Price,
		"fee":                     fee,
	}); err != nil {
		return nil, err
	}
	if err := saveResponse(ctx, qtx, key, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// ── 防洗點 ──

// limits 是四項防洗點門檻的現行值(全部來自 economy_configs,0 = 該項不啟用)。
type limits struct {
	minLevel          int64
	minMemberDays     int64
	minAccountAgeDays int64
	dailyVolumeCap    int64
}

// loadLimits 讀四項門檻。**key 不存在時視為 0(不啟用)**:
// 這四個 key 目前不在 seed(00014)裡,而新增 seed 需要 migration(本次任務不得新增)。
// 選 fail-open 而非 fail-closed 是刻意的:fail-closed 會讓整個市集在門檻上線前完全不能用,
// 而這四項是「額外的防濫用限制」,不是正確性的一部分——正確性(餘額、鎖、冪等)不依賴它們。
// 一旦 seed/管理員寫入這些 key,不必改程式就會生效。
func loadLimits(ctx context.Context, qtx *db.Queries) (limits, error) {
	var out limits
	for _, spec := range []struct {
		key string
		dst *int64
	}{
		{market.ConfigMinLevel, &out.minLevel},
		{market.ConfigMinMemberDays, &out.minMemberDays},
		{market.ConfigMinAccountAgeDays, &out.minAccountAgeDays},
		{market.ConfigDailyVolumeCap, &out.dailyVolumeCap},
	} {
		v, found, err := configInt(ctx, qtx, spec.key)
		if err != nil {
			return out, err
		}
		if found {
			*spec.dst = v
		}
	}
	return out, nil
}

// checkEligibility 驗一個人的交易資格:no_trade → 交易門檻 → 單日交易額上限。
// addVolume > 0 時才檢查上限(掛單不動錢,傳 0)。
// 所有數字都由 DB 一次算出(GetTradeEligibility),用 DB 時鐘與該使用者的時區。
func checkEligibility(ctx context.Context, qtx *db.Queries, userID int64, currency string,
	lim limits, addVolume int64) error {

	e, err := qtx.GetTradeEligibility(ctx, db.GetTradeEligibilityParams{UserID: userID, Currency: currency})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("user=%d: %w", userID, market.ErrUserNotFound)
		}
		return fmt.Errorf("讀交易資格 %d: %w", userID, err)
	}
	if e.NoTrade {
		return fmt.Errorf("user=%d 有生效中的 no_trade: %w", userID, market.ErrTradeRestricted)
	}
	switch {
	case lim.minLevel > 0 && int64(e.Level) < lim.minLevel:
		return fmt.Errorf("等級 %d < 門檻 %d: %w", e.Level, lim.minLevel, market.ErrTradeThresholdNotMet)
	case lim.minMemberDays > 0 && int64(e.MemberDays) < lim.minMemberDays:
		return fmt.Errorf("入群 %d 天 < 門檻 %d 天: %w",
			e.MemberDays, lim.minMemberDays, market.ErrTradeThresholdNotMet)
	case lim.minAccountAgeDays > 0 && int64(e.AccountAgeDays) < lim.minAccountAgeDays:
		return fmt.Errorf("帳號 %d 天 < 門檻 %d 天: %w",
			e.AccountAgeDays, lim.minAccountAgeDays, market.ErrTradeThresholdNotMet)
	}
	if lim.dailyVolumeCap > 0 && addVolume > 0 && e.TradedToday+addVolume > lim.dailyVolumeCap {
		return fmt.Errorf("今日已交易 %d + 本次 %d > 上限 %d: %w",
			e.TradedToday, addVolume, lim.dailyVolumeCap, market.ErrDailyLimitExceeded)
	}
	return nil
}

// ── 內部工具 ──

// lockUser 鎖住一個使用者列(呼叫端負責 user_id 升冪)。
func lockUser(ctx context.Context, qtx *db.Queries, userID int64) error {
	if _, err := qtx.LockUserForTrade(ctx, userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("user=%d: %w", userID, market.ErrUserNotFound)
		}
		return fmt.Errorf("鎖使用者 %d: %w", userID, err)
	}
	return nil
}

// configInt 讀 economy_configs 目前生效值(effective_at <= now() 的最新一筆)。
// found=false 表示這個 key 從未被設定過——由呼叫端決定那是預設值還是部署錯誤。
func configInt(ctx context.Context, qtx *db.Queries, key string) (int64, bool, error) {
	raw, err := qtx.GetCurrentConfig(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("讀設定 %s: %w", key, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false, fmt.Errorf("設定 %s 不是整數 (%s): %w", key, raw, err)
	}
	return v, true, nil
}

// replayFromKey 在已取得串行化鎖之後檢查 API 冪等鍵:
// done=true 表示不需(也不可)繼續執行——回重放結果或錯誤。
func replayFromKey[T any](ctx context.Context, qtx *db.Queries, key, hash string) (*T, bool, error) {
	rec, err := qtx.GetIdempotencyKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("讀冪等鍵: %w", err)
	}
	res, err := decodeKey[T](rec, key, hash)
	return res, true, err
}

// replayKey 是「先佔鍵、撞鍵才回頭讀」路徑的重放(MintItem 用)。
// 呼叫前必須已 rollback 自己的 tx,否則會拿著廢棄連線再要一條(hold-and-wait)。
func replayKey[T any](ctx context.Context, q *db.Queries, key, hash string) (*T, error) {
	rec, err := q.GetIdempotencyKey(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("讀冪等鍵: %w", err)
	}
	return decodeKey[T](rec, key, hash)
}

// decodeKey 判讀冪等鍵紀錄:內容不同 → 衝突;結果未寫入 → 對方仍在處理中。
func decodeKey[T any](rec db.PlatformIdempotencyKey, key, hash string) (*T, error) {
	if rec.RequestHash != hash {
		return nil, fmt.Errorf("key=%s: %w", key, ledger.ErrIdempotencyConflict)
	}
	if rec.Response == nil {
		// 鍵在但結果還沒寫:對方 tx 仍在進行(理論上會先被鎖擋住,這裡是保險分支)。
		// 回可重試錯誤,不回成功。
		return nil, ledger.ErrInFlight
	}
	var result T
	if err := json.Unmarshal(rec.Response, &result); err != nil {
		return nil, fmt.Errorf("反序列化冪等結果: %w", err)
	}
	markReplayed(&result)
	return &result, nil
}

// markReplayed 設定結果的 Replayed 旗標(兩種結果型別都有)。
func markReplayed(v any) {
	switch r := v.(type) {
	case *market.MintResult:
		r.Replayed = true
	case *market.Purchase:
		r.Replayed = true
	}
}

// saveResponse 在 commit 前存入完整結果:commit 成功 ⇔ 鍵帶著結果存在。
func saveResponse(ctx context.Context, qtx *db.Queries, key string, result any) error {
	respJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("序列化結果: %w", err)
	}
	if err := qtx.SetIdempotencyResponse(ctx, db.SetIdempotencyResponseParams{Key: key, Response: respJSON}); err != nil {
		return fmt.Errorf("存冪等結果: %w", err)
	}
	return nil
}

// emit 與領域變更同 tx 寫 outbox(ledger-invariants 第六條)。
func emit(ctx context.Context, qtx *db.Queries, topic string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化事件 %s: %w", topic, err)
	}
	if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{Topic: topic, Payload: raw}); err != nil {
		return fmt.Errorf("寫 outbox %s: %w", topic, err)
	}
	return nil
}

// requestHash 對請求內容取決定性雜湊(sha256 hex),偵測同 key 不同內容的重用。
func requestHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func ptr[T any](v T) *T { return &v }
