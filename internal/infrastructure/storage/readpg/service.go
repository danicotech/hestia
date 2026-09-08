// Package readpg 是 API 讀取側與身分解析的 Postgres 實作:transport/ports.go 的
// Directory、ProfileReader、ProfileWriter、PrivacyStore、Catalog、LedgerReader
// 六個 port。
//
// 定位:core 目前只有寫入側介面(shop.Service、ledger.Ledger、adminecon.Service……),
// 「列出我的權益」「public_id 換內部 id」「翻帳本」這類純讀取沒有 core 介面,
// 由入口層定 port、這裡提供實作;檢視型別共用 core/platform/readmodel。
//
// 四條規矩:
//
//  1. **只讀,不開 transaction。** 每個方法一個 SELECT,沒有跨語句的一致性需求。
//     唯一的寫入是 SetTimezone(ProfileWriter),它也是單一 statement 的
//     UPDATE ... RETURNING,同樣不需要 tx。
//     這裡絕不碰 token_entries / user_balances 的寫入路徑——動錢一律經 ledger,
//     token_entries 更是 append-only,本 package 對它只有 SELECT。
//
//  2. **錯誤用 core 既有的 sentinel。** transport 的 errmap 是 sentinel → connect code
//     的唯一映射表,而 readpg 不能 import transport(方向錯)。所以查無此物一律回
//     shop.ErrUserNotFound / ErrEntitlementNotFound / ErrRedemptionNotFound
//     (errmap 已經把它們映射成 NotFound);使用者側的參數錯誤回
//     shop.ErrInvalidRequest,管理端(LedgerReader)的參數錯誤回
//     adminecon.ErrInvalidInput——兩者都映射成 InvalidArgument。
//     新造 readpg 自己的錯誤只會被 errmap 當成 Internal。
//
//  3. **每個列表都有上限。** 沒有「回全部」的方法;預設 DefaultRowLimit,
//     部署時可用 WithRowLimit 調。排序一律帶 id tie-break,分頁不漂移。
//
//  4. **大表分頁用 keyset,不用 OFFSET。** 見 ListEntries 與 queries/read.sql。
//
// 時間欄位全部原樣回 TIMESTAMPTZ,不做任何時區轉換——那是呈現層的事。
package readpg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// DefaultRowLimit 是列表查詢的預設上限。
//
// 200 的理由:M1 的商品數是數十個量級,單一使用者的權益/工單同量級,
// 一般情況一次就拿得完;真的撞到上限表示該加分頁,而不是把上限拿掉
// ——「沒有上限的列表查詢」是遲早會把記憶體吃光的那種 bug。
const DefaultRowLimit = 200

// Service 同時實作 Directory、ProfileReader、ProfileWriter、PrivacyStore、
// Catalog、LedgerReader。
// 它們共用同一個連線池與同一套錯誤語意,拆成六個 struct 只會多六份樣板;
// 需要收窄能力的呼叫端在**自己那邊**宣告只含所需方法的介面(handler 已經這麼做)。
type Service struct {
	pool  *pgxpool.Pool
	q     *db.Queries
	limit int32
}

// Option 調整 Service 的行為。
type Option func(*Service)

// WithRowLimit 覆寫列表查詢的上限(<= 0 視為不設定,沿用預設)。
func WithRowLimit(n int32) Option {
	return func(s *Service) {
		if n > 0 {
			s.limit = n
		}
	}
}

// New 建立讀取側服務。
func New(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{pool: pool, q: db.New(pool), limit: DefaultRowLimit}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ── port 形狀的編譯期檢查 ────────────────────────────────────────────────
//
// port 介面宣告在 transport(使用端),而 readpg 不能 import transport
// ——所以寫不出 `var _ transport.Catalog = (*Service)(nil)`。
//
// 選擇:在這裡用 **readmodel 型別**重新宣告同形狀的最小介面來斷言。
// 理由是它幾乎免費,又能在本 package 內把「方法簽名」釘住:誰改壞了
// Service 的簽名,readpg 自己就編不過,不必等到 cmd/server 組裝才發現。
// 真正的權威檢查仍然是 cmd/server 把 *Service 指派給 transport 的 port 欄位
// ——那一步會驗證這裡的鏡像有沒有跟 ports.go 一起演進。
type (
	directoryPort interface {
		UserID(ctx context.Context, publicID string) (int64, error)
		UserPublicID(ctx context.Context, userID int64) (string, error)
		EntitlementID(ctx context.Context, publicID string) (int64, error)
		RedemptionID(ctx context.Context, publicID string) (int64, error)
	}
	profileReaderPort interface {
		Profile(ctx context.Context, userID int64) (*readmodel.ProfileView, error)
		Balances(ctx context.Context, userID int64) ([]readmodel.BalanceView, error)
	}
	profileWriterPort interface {
		SetTimezone(ctx context.Context, userID int64, timezone string) (*readmodel.ProfileView, error)
	}
	privacyStorePort interface {
		Privacy(ctx context.Context, userID int64) (*readmodel.PrivacyView, error)
		SetPrivacy(ctx context.Context, userID int64, up readmodel.PrivacyUpdate) (*readmodel.PrivacyView, error)
	}
	catalogPort interface {
		Items(ctx context.Context, includeDelisted bool) ([]readmodel.ItemView, error)
		Entitlements(ctx context.Context, userID int64, includeRevoked bool) ([]readmodel.EntitlementView, error)
		Redemptions(ctx context.Context, userID int64, status shop.RedemptionStatus) ([]readmodel.RedemptionView, error)
	}
	ledgerReaderPort interface {
		ListEntries(ctx context.Context, q readmodel.LedgerEntriesQuery) (*readmodel.LedgerEntriesPage, error)
	}
)

var (
	_ directoryPort     = (*Service)(nil)
	_ profileReaderPort = (*Service)(nil)
	_ profileWriterPort = (*Service)(nil)
	_ privacyStorePort  = (*Service)(nil)
	_ catalogPort       = (*Service)(nil)
	_ ledgerReaderPort  = (*Service)(nil)
)

// cleanPublicID 把外部傳進來的 public_id 正規化,並判斷它是否**可能**命中任何列。
//
// 兩種必擋的輸入:
//   - 去空白後為空:不可能命中,提早回錯省一次往返。
//   - 含 NUL(\x00):Postgres 的 text 型別不能存 NUL,把它送進查詢會拿到
//     `invalid byte sequence for encoding "UTF8"`(SQLSTATE 22021)——那是個
//     連線層級的錯誤,會被映射成 Internal。但語意上它就只是「查無此物」,
//     不該讓客戶端可控的字串把 5xx 打出來。
//
// 其他任何形狀(長度、非 ULID 字元)都照常進 DB:格式白名單是第二套 ULID 語意,
// public_id 的權威是 UNIQUE 索引,查不到就是查不到。
func cleanPublicID(publicID string) (string, bool) {
	publicID = strings.TrimSpace(publicID)
	if publicID == "" || strings.ContainsRune(publicID, 0) {
		return "", false
	}
	return publicID, true
}

// ── Directory ───────────────────────────────────────────────────────────

// UserID 把使用者的 public_id 解成內部 id。軟刪除的帳號視同不存在。
//
// 查詢用 identity.sql 的 GetUserIDByPublicID(既有權威),不另寫一份。
func (s *Service) UserID(ctx context.Context, publicID string) (int64, error) {
	publicID, ok := cleanPublicID(publicID)
	if !ok {
		return 0, fmt.Errorf("public_id 為空或含不可能存在的位元組: %w", shop.ErrUserNotFound)
	}
	id, err := s.q.GetUserIDByPublicID(ctx, publicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("使用者 public_id 不存在: %w", shop.ErrUserNotFound)
		}
		return 0, fmt.Errorf("解析使用者 public_id: %w", err)
	}
	return id, nil
}

// UserPublicID 反向取回 public_id(軟刪除的帳號同樣查不到)。
func (s *Service) UserPublicID(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	publicID, err := s.q.GetUserPublicID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("使用者 %d 不存在: %w", userID, shop.ErrUserNotFound)
		}
		return "", fmt.Errorf("取使用者 public_id: %w", err)
	}
	return publicID, nil
}

// EntitlementID 把權益 public_id 解成內部 id。
//
// 注意:port 的簽名沒有 userID,所以這裡**不做**歸屬檢查——
// 「這個權益是不是你的」的權威在 shop.Service.RefundPurchase 的列鎖內
// (回 shop.ErrNotEntitlementOwner)。要在入口層提早擋,用 EntitlementOwner。
func (s *Service) EntitlementID(ctx context.Context, publicID string) (int64, error) {
	id, _, err := s.EntitlementOwner(ctx, publicID)
	return id, err
}

// EntitlementOwner 解析權益 public_id,同時回傳持有人的內部 user id。
//
// 為什麼多這個方法:只回 id 的話,入口層拿到 A 的 public_id 卻不知道它屬於 B,
// 只能把請求丟給 service 才發現不是自己的。一併回持有人,入口層就能在
// 「解析」這一步直接擋掉跨使用者定址,而不用把別人的內部 id 傳下去。
func (s *Service) EntitlementOwner(ctx context.Context, publicID string) (id, userID int64, err error) {
	publicID, ok := cleanPublicID(publicID)
	if !ok {
		return 0, 0, fmt.Errorf("entitlement public_id 為空或含不可能存在的位元組: %w", shop.ErrEntitlementNotFound)
	}
	row, err := s.q.ResolveEntitlement(ctx, publicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, fmt.Errorf("權益 public_id 不存在: %w", shop.ErrEntitlementNotFound)
		}
		return 0, 0, fmt.Errorf("解析權益 public_id: %w", err)
	}
	return row.ID, row.UserID, nil
}

// RedemptionID 把工單 public_id 解成內部 id(歸屬檢查同 EntitlementID 的說明)。
func (s *Service) RedemptionID(ctx context.Context, publicID string) (int64, error) {
	id, _, err := s.RedemptionOwner(ctx, publicID)
	return id, err
}

// RedemptionOwner 解析工單 public_id,同時回傳建立者的內部 user id。
func (s *Service) RedemptionOwner(ctx context.Context, publicID string) (id, userID int64, err error) {
	publicID, ok := cleanPublicID(publicID)
	if !ok {
		return 0, 0, fmt.Errorf("redemption public_id 為空或含不可能存在的位元組: %w", shop.ErrRedemptionNotFound)
	}
	row, err := s.q.ResolveRedemption(ctx, publicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, fmt.Errorf("工單 public_id 不存在: %w", shop.ErrRedemptionNotFound)
		}
		return 0, 0, fmt.Errorf("解析工單 public_id: %w", err)
	}
	return row.ID, row.UserID, nil
}

// ── ProfileStore ────────────────────────────────────────────────────────

// Profile 讀使用者的可公開欄位。軟刪除的使用者回 shop.ErrUserNotFound。
//
// 用 users.sql 既有的 GetUserByID(已含 deleted_at IS NULL),不另寫一份查詢;
// email / last_seen_at 讀出來但不放進 ProfileView——對外檢視只給該給的。
func (s *Service) Profile(ctx context.Context, userID int64) (*readmodel.ProfileView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	u, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("使用者 %d 不存在或已刪除: %w", userID, shop.ErrUserNotFound)
		}
		return nil, fmt.Errorf("讀使用者 %d: %w", userID, err)
	}
	return &readmodel.ProfileView{
		PublicID:          u.PublicID,
		DisplayName:       deref(u.DisplayName),
		AvatarURL:         deref(u.AvatarUrl),
		Timezone:          u.Timezone,
		TimezoneChangedAt: u.TimezoneChangedAt,
		CreatedAt:         u.CreatedAt,
	}, nil
}

// knownTimezoneSQL 是本 package 唯一沒走 sqlc 的 SQL。
//
// 原因:sqlc 的內建 catalog 沒有 pg_timezone_names 這個系統檢視,
// 放進 queries/read.sql 會讓 `sqlc generate` 直接失敗
// (relation "pg_timezone_names" does not exist)。
//
// 為什麼非它不可:時區字串的合法性只有一個權威——**資料庫自己認得的那份清單**。
// 用 Go 的 time.LoadLocation 驗會出現「應用層說合法、簽到的 AT TIME ZONE 算出別的
// 日界線」的第二套語意;應用層寫死 IANA 白名單更糟(每年要跟著 tzdata 更新)。
const knownTimezoneSQL = `SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)`

// SetTimezone 更新時區並回傳更新後的檔案。
//
// 刻意不在這裡檢查「改時區冷卻」:那條規則的權威在 dailypg 的簽到路徑
// (daily.ErrTimezoneChangeCooldown ——「改完時區距上次簽到未滿最小間隔就不給簽」),
// 改時區本身永遠允許。在這裡再擋一次就是同一概念兩個權威。
func (s *Service) SetTimezone(ctx context.Context, userID int64, timezone string) (*readmodel.ProfileView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	tz := strings.TrimSpace(timezone)
	if tz == "" {
		return nil, fmt.Errorf("timezone 必填: %w", shop.ErrInvalidRequest)
	}
	var known bool
	if err := s.pool.QueryRow(ctx, knownTimezoneSQL, tz).Scan(&known); err != nil {
		return nil, fmt.Errorf("驗證時區 %q: %w", tz, err)
	}
	if !known {
		return nil, fmt.Errorf("時區 %q 不是資料庫認得的 IANA 名稱: %w", tz, shop.ErrInvalidRequest)
	}

	row, err := s.q.SetUserTimezone(ctx, db.SetUserTimezoneParams{ID: userID, Timezone: tz})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("使用者 %d 不存在或已刪除: %w", userID, shop.ErrUserNotFound)
		}
		return nil, fmt.Errorf("更新使用者 %d 的時區: %w", userID, err)
	}
	return &readmodel.ProfileView{
		PublicID:          row.PublicID,
		DisplayName:       row.DisplayName,
		AvatarURL:         row.AvatarUrl,
		Timezone:          row.Timezone,
		TimezoneChangedAt: row.TimezoneChangedAt,
		CreatedAt:         row.CreatedAt,
	}, nil
}

// ── PrivacyStore ────────────────────────────────────────────────────────

// Privacy 讀兩級退出設定。
//
// **沒有列不是錯誤**:user_privacy_settings 只在使用者動過設定時才有列,
// 沒有列就是「兩者皆 false」的預設狀態。這是 `/privacy` 指令能不能用的關鍵
// ——第一次執行的人必然沒有列,回 NotFound 的話這個功能對新使用者永遠是壞的。
//
// UpdatedAt 為 nil 就是「從未設定過」的標記,呼叫端據此分得出
// 「預設值」與「設過又改回預設」。
func (s *Service) Privacy(ctx context.Context, userID int64) (*readmodel.PrivacyView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	row, err := s.q.GetUserPrivacy(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 使用者不存在 / 軟刪除也會走到這裡。刻意**不**為此多打一次
			// users 查詢分辨兩者:隱私設定是「我的」資源,而呼叫端的
			// user id 來自認證攔截器,不可能是別人的 —— 分辨出來也沒有
			// 第二種處置,只是多一次往返。
			return &readmodel.PrivacyView{}, nil
		}
		return nil, fmt.Errorf("讀使用者 %d 的隱私設定: %w", userID, err)
	}
	updated := row.UpdatedAt
	return &readmodel.PrivacyView{
		OptOutLogging:  row.OptOutLogging,
		OptOutAICorpus: row.OptOutAiCorpus,
		UpdatedAt:      &updated,
	}, nil
}

// SetPrivacy 更新兩級退出設定並回傳更新後的**完整**設定。
//
// up 的兩個欄位各自可為 nil(= 這次不動這一項),語意由 SQL 的 COALESCE
// 實作(見 queries/read.sql):沒有列就建列,有列就只改指定的那幾項。
// 全 nil 的呼叫在入口層就被擋掉(寫入 RPC 不該靜靜地什麼都不做),
// 這裡再擋一次是防禦性的:沒有它,全 nil 會變成一次「只更新 updated_at」的寫入。
func (s *Service) SetPrivacy(
	ctx context.Context, userID int64, up readmodel.PrivacyUpdate,
) (*readmodel.PrivacyView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	if up.OptOutLogging == nil && up.OptOutAICorpus == nil {
		return nil, fmt.Errorf("至少要指定一項隱私設定: %w", shop.ErrInvalidRequest)
	}
	row, err := s.q.UpsertUserPrivacy(ctx, db.UpsertUserPrivacyParams{
		UserID:         userID,
		OptOutLogging:  up.OptOutLogging,
		OptOutAiCorpus: up.OptOutAICorpus,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// INSERT ... SELECT 選不到來源列 = 使用者不存在或已軟刪除。
			return nil, fmt.Errorf("使用者 %d 不存在或已刪除: %w", userID, shop.ErrUserNotFound)
		}
		return nil, fmt.Errorf("更新使用者 %d 的隱私設定: %w", userID, err)
	}
	updated := row.UpdatedAt
	return &readmodel.PrivacyView{
		OptOutLogging:  row.OptOutLogging,
		OptOutAICorpus: row.OptOutAiCorpus,
		UpdatedAt:      &updated,
	}, nil
}

// Balances 回傳使用者所有幣別的餘額。
//
// 語意鎖死:**沒有列 = 沒有那個幣別的餘額 = 0**。全新使用者回空 slice(非 nil),
// 不是「coin: 0」——user_balances 是餘額的唯一權威,讀取側替它補零列
// 就是在憑空造資料。呼叫端要顯示 0,自己把缺的幣別補成 0 即可。
func (s *Service) Balances(ctx context.Context, userID int64) ([]readmodel.BalanceView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	rows, err := s.q.ListUserBalances(ctx, db.ListUserBalancesParams{
		UserID: userID, RowLimit: s.limit,
	})
	if err != nil {
		return nil, fmt.Errorf("讀使用者 %d 的餘額: %w", userID, err)
	}
	out := make([]readmodel.BalanceView, 0, len(rows))
	for _, r := range rows {
		out = append(out, readmodel.BalanceView{Currency: r.Currency, Amount: r.Balance})
	}
	return out, nil
}

// ── Catalog ─────────────────────────────────────────────────────────────

// Items 列出上架中的商品。
//
// 上架判定與 shoppg 的購買判定是同一套條件(DB 時鐘),見 queries/read.sql 的註解:
// 型錄看得到的一定買得到,看不到的一定買不到。includeDelisted=true 只多露出
// 已下架的(絕版收藏用),從未上架與上架時間未到的商品任何情況都不露出。
func (s *Service) Items(ctx context.Context, includeDelisted bool) ([]readmodel.ItemView, error) {
	rows, err := s.q.ListListedItems(ctx, db.ListListedItemsParams{
		IncludeDelisted: includeDelisted, RowLimit: s.limit,
	})
	if err != nil {
		return nil, fmt.Errorf("列出商品: %w", err)
	}
	out := make([]readmodel.ItemView, 0, len(rows))
	for _, r := range rows {
		out = append(out, readmodel.ItemView{
			PublicID:            r.PublicID,
			Name:                r.Name,
			Description:         r.Description,
			Fulfillment:         shop.Fulfillment(r.Fulfillment),
			Currency:            r.Currency,
			Price:               r.Price,
			DurationDays:        r.DurationDays,
			PerUserLimit:        r.PerUserLimit,
			RefundWindowSeconds: r.RefundWindowSeconds,
			ListedAt:            r.ListedAt,
			DelistedAt:          r.DelistedAt,
		})
	}
	return out, nil
}

// Entitlements 列出使用者的權益,新的在前。
//
// includeRevoked=false(預設)濾掉已撤銷(含已退款)的;
// **已過期的仍然回傳**,由 ExpiresAt 讓呈現層自己判斷要顯示成「已到期」還是收起來。
// 軟刪除的使用者一律回空清單。
func (s *Service) Entitlements(ctx context.Context, userID int64, includeRevoked bool) ([]readmodel.EntitlementView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	rows, err := s.q.ListUserEntitlements(ctx, db.ListUserEntitlementsParams{
		UserID: userID, IncludeRevoked: includeRevoked, RowLimit: s.limit,
	})
	if err != nil {
		return nil, fmt.Errorf("列出使用者 %d 的權益: %w", userID, err)
	}
	out := make([]readmodel.EntitlementView, 0, len(rows))
	for _, r := range rows {
		out = append(out, readmodel.EntitlementView{
			PublicID:        r.PublicID,
			ItemPublicID:    r.ItemPublicID,
			ItemName:        r.ItemName,
			Fulfillment:     shop.Fulfillment(r.Fulfillment),
			GrantedAt:       r.GrantedAt,
			ExpiresAt:       r.ExpiresAt,
			RefundableUntil: r.RefundableUntil,
			RevokedAt:       r.RevokedAt,
		})
	}
	return out, nil
}

// Redemptions 列出使用者的兌換工單,新的在前。status 為空字串 = 不過濾。
func (s *Service) Redemptions(ctx context.Context, userID int64, status shop.RedemptionStatus) ([]readmodel.RedemptionView, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", shop.ErrUserNotFound)
	}
	var filter *string
	if status != "" {
		v := string(status)
		filter = &v
	}
	rows, err := s.q.ListUserRedemptions(ctx, db.ListUserRedemptionsParams{
		UserID: userID, Status: filter, RowLimit: s.limit,
	})
	if err != nil {
		return nil, fmt.Errorf("列出使用者 %d 的工單: %w", userID, err)
	}
	out := make([]readmodel.RedemptionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, readmodel.RedemptionView{
			PublicID:     r.PublicID,
			ItemPublicID: r.ItemPublicID,
			ItemName:     r.ItemName,
			Status:       shop.RedemptionStatus(r.Status),
			Note:         r.Note,
			CreatedAt:    r.CreatedAt,
			HandledAt:    r.HandledAt,
		})
	}
	return out, nil
}

// deref 把 nullable TEXT 攤平成字串:NULL → ""(「沒有」的唯一表示法,
// 對外檢視不出現 nil 指標)。
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
