package readpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/readpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/shoppg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 對真 Postgres 跑(hestia/CLAUDE.md):讀取側的重點正是 SQL 條件本身
// ——軟刪除過濾、上架判定、排序穩定性——mock 掉資料庫等於什麼都沒測。
// 整個 package 共用一個容器,各測試用獨立使用者/商品隔離。

var (
	pool *pgxpool.Pool
	svc  *readpg.Service
	shp  *shoppg.Service
)

func TestMain(m *testing.M) {
	flag.Parse()
	// -short 整包跳過,而不是靠每一支測試自己呼叫 setup(t)。
	//
	// 原本的寫法是逐測試 opt-in,而 summary_test.go 的九支全部漏掉了 ——
	// 於是 -short 時 pool 是 nil,第一支就 panic,`go test -short ./...`
	// 整個 repo 都停在這裡。這個套件沒有一支測試能在沒有資料庫時做事,
	// 所以判斷只該有一處。
	if testing.Short() {
		fmt.Fprintln(os.Stderr, "readpg: 需要 Docker,-short 模式整包跳過")
		os.Exit(0)
	}
	{
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = readpg.New(pool)
		shp = shoppg.New(pool, ledgerpg.New(pool))
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
}

// setup 只是取 ctx。-short 的跳過在 TestMain,不在這裡 ——
// 同一個判斷放兩處,漏掉的那一處就是 summary_test.go 當初出事的方式。
func setup(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// ── 測試資料 ────────────────────────────────────────────────────────────

// newUser 建一個有 display_name / avatar_url 的使用者。
func newUser(t *testing.T, displayName string) (id int64, publicID string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id, display_name, avatar_url)
		 VALUES (gen_random_uuid()::text, $1, $2) RETURNING id, public_id`,
		displayName, "https://cdn.example/"+displayName+".png",
	).Scan(&id, &publicID)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id, publicID
}

// newBareUser 建一個 display_name / avatar_url 皆為 NULL 的使用者。
func newBareUser(t *testing.T) (id int64, publicID string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text)
		 RETURNING id, public_id`,
	).Scan(&id, &publicID)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id, publicID
}

func softDelete(t *testing.T, userID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE platform.users SET deleted_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("軟刪除使用者: %v", err)
	}
}

// itemSpec 零值 = 上架中(listed_at 在一小時前)、auto_perk、免費、不限購。
type itemSpec struct {
	name         string
	fulfillment  string
	price        int64
	unlisted     bool // listed_at NULL(從未上架)
	futureListed bool // listed_at 在未來(上架時間未到)
	delisted     bool // delisted_at 已過(已下架)
}

func newItem(t *testing.T, sp itemSpec) (id int64, publicID string) {
	t.Helper()
	if sp.fulfillment == "" {
		sp.fulfillment = "auto_perk"
	}
	if sp.name == "" {
		sp.name = "test item"
	}
	listed := "now() - interval '1 hour'"
	switch {
	case sp.unlisted:
		listed = "NULL"
	case sp.futureListed:
		listed = "now() + interval '1 hour'"
	}
	delisted := "NULL"
	if sp.delisted {
		delisted = "now() - interval '1 minute'"
	}
	q := fmt.Sprintf(`INSERT INTO platform.shop_items
		(public_id, name, description, fulfillment, currency, price,
		 refund_window_seconds, listed_at, delisted_at)
		VALUES (gen_random_uuid()::text, $1, 'desc', $2, 'coin', $3, 600, %s, %s)
		RETURNING id, public_id`, listed, delisted)
	if err := pool.QueryRow(context.Background(), q, sp.name, sp.fulfillment, sp.price).
		Scan(&id, &publicID); err != nil {
		t.Fatalf("建測試商品: %v", err)
	}
	return id, publicID
}

// grantEntitlement 直接寫 entitlements(讀取側測試不需要走完整購買流程)。
func grantEntitlement(t *testing.T, userID, itemID int64, expiresSQL, revokedSQL string) (id int64, publicID string) {
	t.Helper()
	if expiresSQL == "" {
		expiresSQL = "NULL"
	}
	if revokedSQL == "" {
		revokedSQL = "NULL"
	}
	q := fmt.Sprintf(`INSERT INTO platform.entitlements
		(public_id, user_id, item_id, expires_at, refundable_until, revoked_at)
		VALUES (gen_random_uuid()::text, $1, $2, %s, now() + interval '10 minutes', %s)
		RETURNING id, public_id`, expiresSQL, revokedSQL)
	if err := pool.QueryRow(context.Background(), q, userID, itemID).Scan(&id, &publicID); err != nil {
		t.Fatalf("建測試權益: %v", err)
	}
	return id, publicID
}

func newRedemption(t *testing.T, userID, itemID int64, status string) (id int64, publicID string) {
	t.Helper()
	handled := "NULL"
	if status != "pending" {
		handled = "now()"
	}
	q := fmt.Sprintf(`INSERT INTO platform.redemptions
		(public_id, user_id, item_id, status, note, handled_at)
		VALUES (gen_random_uuid()::text, $1, $2, $3, 'note-'||$3, %s)
		RETURNING id, public_id`, handled)
	if err := pool.QueryRow(context.Background(), q, userID, itemID, status).Scan(&id, &publicID); err != nil {
		t.Fatalf("建測試工單: %v", err)
	}
	return id, publicID
}

func credit(t *testing.T, userID int64, currency string, amount int64) {
	t.Helper()
	key := fmt.Sprintf("readtest-credit-%d-%s-%d", userID, currency, amount)
	led := ledgerpg.New(pool)
	if _, err := led.Apply(context.Background(), ledger.ApplyParams{
		IdempotencyKey: key, RequestHash: "h-" + key,
		Ops: []ledger.Op{{UserID: userID, Currency: currency, Amount: amount, Reason: ledger.ReasonAdminGrant}},
	}); err != nil {
		t.Fatalf("credit %s: %v", currency, err)
	}
}

// ── Directory ───────────────────────────────────────────────────────────

func TestDirectoryUserID(t *testing.T) {
	ctx := setup(t)
	userID, publicID := newUser(t, "alice")

	got, err := svc.UserID(ctx, publicID)
	if err != nil {
		t.Fatalf("解析有效 public_id: %v", err)
	}
	if got != userID {
		t.Fatalf("UserID = %d,預期 %d", got, userID)
	}

	back, err := svc.UserPublicID(ctx, userID)
	if err != nil {
		t.Fatalf("反向解析: %v", err)
	}
	if back != publicID {
		t.Fatalf("UserPublicID = %q,預期 %q", back, publicID)
	}

	// 不存在 / 空字串 / 格式錯誤:一律回可辨識的 NotFound 語意,絕不 panic。
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"不存在的 ULID", "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{"空字串", ""},
		{"只有空白", "   "},
		{"格式錯誤", "not-a-ulid!!"},
		{"SQL 注入樣子的字串", "' OR 1=1 --"},
		{"超長字串", strings.Repeat("A", 4096)},
		// NUL 是 Postgres 的 text 存不下的位元組:不擋掉的話會拿到
		// SQLSTATE 22021,被映射成 Internal(客戶端可控字串打出 5xx)。
		{"含 NUL 位元組", "01ARZ3\x00NDEKTSV4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.UserID(ctx, tc.input); !errors.Is(err, shop.ErrUserNotFound) {
				t.Fatalf("預期 ErrUserNotFound,得到 %v", err)
			}
		})
	}
}

func TestDirectorySoftDeletedUserUnresolvable(t *testing.T) {
	ctx := setup(t)
	userID, publicID := newUser(t, "ghost")
	if _, err := svc.UserID(ctx, publicID); err != nil {
		t.Fatalf("刪除前應解得到: %v", err)
	}
	softDelete(t, userID)

	if _, err := svc.UserID(ctx, publicID); !errors.Is(err, shop.ErrUserNotFound) {
		t.Fatalf("軟刪除後 UserID 應回 ErrUserNotFound,得到 %v", err)
	}
	if _, err := svc.UserPublicID(ctx, userID); !errors.Is(err, shop.ErrUserNotFound) {
		t.Fatalf("軟刪除後 UserPublicID 應回 ErrUserNotFound,得到 %v", err)
	}
}

// TestDirectoryResolveDoesNotCrossUsers 是「A 拿 B 的 public_id」的核心防線:
// 解析必須連同歸屬一起交出來,而且 A 的 public_id 不可能解成 B 的資源。
func TestDirectoryResolveDoesNotCrossUsers(t *testing.T) {
	ctx := setup(t)
	userA, _ := newUser(t, "a")
	userB, _ := newUser(t, "b")
	itemID, _ := newItem(t, itemSpec{name: "cross"})

	entA, entAPublic := grantEntitlement(t, userA, itemID, "", "")
	entB, entBPublic := grantEntitlement(t, userB, itemID, "", "")
	redA, redAPublic := newRedemption(t, userA, itemID, "pending")
	redB, redBPublic := newRedemption(t, userB, itemID, "pending")

	gotEntA, ownerA, err := svc.EntitlementOwner(ctx, entAPublic)
	if err != nil {
		t.Fatalf("解析 A 的權益: %v", err)
	}
	if gotEntA != entA || ownerA != userA {
		t.Fatalf("A 的權益解成 (id=%d, user=%d),預期 (%d, %d)", gotEntA, ownerA, entA, userA)
	}
	gotEntB, ownerB, err := svc.EntitlementOwner(ctx, entBPublic)
	if err != nil {
		t.Fatalf("解析 B 的權益: %v", err)
	}
	if gotEntB != entB || ownerB != userB {
		t.Fatalf("B 的權益解成 (id=%d, user=%d),預期 (%d, %d)", gotEntB, ownerB, entB, userB)
	}
	if gotEntA == gotEntB {
		t.Fatal("A 與 B 的權益 public_id 解到同一個內部 id")
	}

	gotRedA, redOwnerA, err := svc.RedemptionOwner(ctx, redAPublic)
	if err != nil {
		t.Fatalf("解析 A 的工單: %v", err)
	}
	if gotRedA != redA || redOwnerA != userA {
		t.Fatalf("A 的工單解成 (id=%d, user=%d),預期 (%d, %d)", gotRedA, redOwnerA, redA, userA)
	}
	gotRedB, redOwnerB, err := svc.RedemptionOwner(ctx, redBPublic)
	if err != nil {
		t.Fatalf("解析 B 的工單: %v", err)
	}
	if gotRedB != redB || redOwnerB != userB {
		t.Fatalf("B 的工單解成 (id=%d, user=%d),預期 (%d, %d)", gotRedB, redOwnerB, redB, userB)
	}

	// 命名空間不可互串:權益的 public_id 不能當工單解,反之亦然。
	if _, err := svc.RedemptionID(ctx, entAPublic); !errors.Is(err, shop.ErrRedemptionNotFound) {
		t.Fatalf("拿權益 public_id 解工單應回 ErrRedemptionNotFound,得到 %v", err)
	}
	if _, err := svc.EntitlementID(ctx, redAPublic); !errors.Is(err, shop.ErrEntitlementNotFound) {
		t.Fatalf("拿工單 public_id 解權益應回 ErrEntitlementNotFound,得到 %v", err)
	}
}

func TestDirectoryEntitlementAndRedemptionErrors(t *testing.T) {
	ctx := setup(t)
	for _, in := range []string{"", "   ", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "%%%"} {
		if _, err := svc.EntitlementID(ctx, in); !errors.Is(err, shop.ErrEntitlementNotFound) {
			t.Fatalf("EntitlementID(%q) 預期 ErrEntitlementNotFound,得到 %v", in, err)
		}
		if _, err := svc.RedemptionID(ctx, in); !errors.Is(err, shop.ErrRedemptionNotFound) {
			t.Fatalf("RedemptionID(%q) 預期 ErrRedemptionNotFound,得到 %v", in, err)
		}
	}

	// 持有人被軟刪除後,資源本身也不再可定址。
	userID, _ := newUser(t, "deleted-owner")
	itemID, _ := newItem(t, itemSpec{name: "orphan"})
	_, entPublic := grantEntitlement(t, userID, itemID, "", "")
	_, redPublic := newRedemption(t, userID, itemID, "pending")
	if _, err := svc.EntitlementID(ctx, entPublic); err != nil {
		t.Fatalf("刪除前應解得到權益: %v", err)
	}
	softDelete(t, userID)
	if _, err := svc.EntitlementID(ctx, entPublic); !errors.Is(err, shop.ErrEntitlementNotFound) {
		t.Fatalf("持有人軟刪除後應回 ErrEntitlementNotFound,得到 %v", err)
	}
	if _, err := svc.RedemptionID(ctx, redPublic); !errors.Is(err, shop.ErrRedemptionNotFound) {
		t.Fatalf("持有人軟刪除後應回 ErrRedemptionNotFound,得到 %v", err)
	}
}

// ── ProfileStore ────────────────────────────────────────────────────────

func TestProfile(t *testing.T) {
	ctx := setup(t)
	userID, publicID := newUser(t, "profiled")

	p, err := svc.Profile(ctx, userID)
	if err != nil {
		t.Fatalf("讀檔案: %v", err)
	}
	if p.PublicID != publicID {
		t.Fatalf("PublicID = %q,預期 %q", p.PublicID, publicID)
	}
	if p.DisplayName != "profiled" {
		t.Fatalf("DisplayName = %q", p.DisplayName)
	}
	if p.AvatarURL == "" {
		t.Fatal("AvatarURL 不該是空的")
	}
	if p.Timezone != "Asia/Taipei" {
		t.Fatalf("Timezone = %q,預期 schema 預設 Asia/Taipei", p.Timezone)
	}
	if p.TimezoneChangedAt != nil {
		t.Fatal("沒改過時區時 TimezoneChangedAt 應為 nil")
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("CreatedAt 不該是零值")
	}

	// NULL 的 display_name / avatar_url 攤平成空字串,不外露 nil。
	bareID, _ := newBareUser(t)
	bare, err := svc.Profile(ctx, bareID)
	if err != nil {
		t.Fatalf("讀無名使用者的檔案: %v", err)
	}
	if bare.DisplayName != "" || bare.AvatarURL != "" {
		t.Fatalf("NULL 欄位應為空字串,得到 %q / %q", bare.DisplayName, bare.AvatarURL)
	}
}

func TestProfileSoftDeletedAndMissing(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "soon-gone")
	softDelete(t, userID)

	if _, err := svc.Profile(ctx, userID); !errors.Is(err, shop.ErrUserNotFound) {
		t.Fatalf("軟刪除的使用者應查不到,得到 %v", err)
	}
	for _, id := range []int64{0, -1, 1 << 40} {
		if _, err := svc.Profile(ctx, id); !errors.Is(err, shop.ErrUserNotFound) {
			t.Fatalf("Profile(%d) 預期 ErrUserNotFound,得到 %v", id, err)
		}
	}
}

func TestSetTimezone(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "tz")

	p, err := svc.SetTimezone(ctx, userID, "Asia/Tokyo")
	if err != nil {
		t.Fatalf("設定合法時區: %v", err)
	}
	if p.Timezone != "Asia/Tokyo" {
		t.Fatalf("Timezone = %q", p.Timezone)
	}
	if p.TimezoneChangedAt == nil {
		t.Fatal("改過時區後 timezone_changed_at 應有值(簽到冷卻要用)")
	}
	// 回傳的檔案要與 DB 實際狀態一致。
	after, err := svc.Profile(ctx, userID)
	if err != nil || after.Timezone != "Asia/Tokyo" {
		t.Fatalf("重讀檔案 = %+v, err=%v", after, err)
	}

	// 前後空白容許,但語意不合法的一律 InvalidArgument。
	if p, err := svc.SetTimezone(ctx, userID, "  Europe/Berlin  "); err != nil || p.Timezone != "Europe/Berlin" {
		t.Fatalf("含空白的合法時區應被接受,得到 %+v / %v", p, err)
	}
	for _, tz := range []string{"", "   ", "Mars/Olympus", "Asia/Taipei; DROP TABLE users", "UTC+8"} {
		if _, err := svc.SetTimezone(ctx, userID, tz); !errors.Is(err, shop.ErrInvalidRequest) {
			t.Fatalf("SetTimezone(%q) 預期 ErrInvalidRequest,得到 %v", tz, err)
		}
	}

	// 軟刪除的使用者改不動時區。
	goneID, _ := newUser(t, "tz-gone")
	softDelete(t, goneID)
	if _, err := svc.SetTimezone(ctx, goneID, "Asia/Tokyo"); !errors.Is(err, shop.ErrUserNotFound) {
		t.Fatalf("軟刪除使用者改時區應回 ErrUserNotFound,得到 %v", err)
	}
}

// TestSetTimezoneConcurrent 鎖住「UPDATE ... RETURNING 是同一個 statement」這件事:
// 併發改時區時,每個呼叫拿回的必須是**自己寫進去的值**。
// 若拆成 UPDATE 再 SELECT,這個測試會抓到「拿回別人的時區」。
func TestSetTimezoneConcurrent(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "tz-race")
	zones := []string{
		"Asia/Tokyo", "Europe/Berlin", "America/New_York", "Australia/Sydney",
		"Asia/Taipei", "Europe/London", "America/Sao_Paulo", "Africa/Cairo",
	}

	var wg sync.WaitGroup
	errs := make([]error, len(zones))
	got := make([]string, len(zones))
	for i, tz := range zones {
		wg.Add(1)
		go func(i int, tz string) {
			defer wg.Done()
			p, err := svc.SetTimezone(ctx, userID, tz)
			if err != nil {
				errs[i] = err
				return
			}
			got[i] = p.Timezone
		}(i, tz)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("併發改時區 %q 失敗: %v", zones[i], err)
		}
		if got[i] != zones[i] {
			t.Fatalf("設定 %q 卻拿回 %q(UPDATE 與 SELECT 被拆開了)", zones[i], got[i])
		}
	}

	final, err := svc.Profile(ctx, userID)
	if err != nil {
		t.Fatalf("重讀檔案: %v", err)
	}
	if !contains(zones, final.Timezone) {
		t.Fatalf("最終時區 %q 不在候選集合內", final.Timezone)
	}
}

func TestBalances(t *testing.T) {
	ctx := setup(t)

	// 全新使用者:沒有 user_balances 列 → 空清單(非 nil),不是「coin: 0」。
	freshID, _ := newUser(t, "fresh")
	got, err := svc.Balances(ctx, freshID)
	if err != nil {
		t.Fatalf("讀新使用者餘額: %v", err)
	}
	if got == nil {
		t.Fatal("應回空 slice 而非 nil")
	}
	if len(got) != 0 {
		t.Fatalf("新使用者應無餘額列,得到 %+v", got)
	}

	// 多幣別:一列一幣別,依 currency 升冪(穩定排序)。
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.currencies (code, name) VALUES ('gem','Gem') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("補測試幣別: %v", err)
	}
	richID, _ := newUser(t, "rich")
	credit(t, richID, "gem", 7)
	credit(t, richID, "coin", 500)

	got, err = svc.Balances(ctx, richID)
	if err != nil {
		t.Fatalf("讀多幣別餘額: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("預期 2 個幣別,得到 %+v", got)
	}
	if got[0].Currency != "coin" || got[0].Amount != 500 {
		t.Fatalf("第一列應是 coin=500,得到 %+v", got[0])
	}
	if got[1].Currency != "gem" || got[1].Amount != 7 {
		t.Fatalf("第二列應是 gem=7,得到 %+v", got[1])
	}

	// 軟刪除的使用者查不到自己的餘額。
	softDelete(t, richID)
	got, err = svc.Balances(ctx, richID)
	if err != nil {
		t.Fatalf("軟刪除後讀餘額: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("軟刪除使用者應回空清單,得到 %+v", got)
	}
}

// ── Catalog:權益與工單 ─────────────────────────────────────────────────

func TestEntitlementsList(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "holder")
	otherID, _ := newUser(t, "other")
	liveItem, liveItemPublic := newItem(t, itemSpec{name: "live perk", fulfillment: "auto_role"})
	expItem, _ := newItem(t, itemSpec{name: "expired perk"})
	revItem, _ := newItem(t, itemSpec{name: "revoked perk"})

	_, livePublic := grantEntitlement(t, userID, liveItem, "now() + interval '30 days'", "")
	_, expiredPublic := grantEntitlement(t, userID, expItem, "now() - interval '1 day'", "")
	_, revokedPublic := grantEntitlement(t, userID, revItem, "", "now()")
	// 別人的權益絕不能出現在我的清單裡。
	_, foreignPublic := grantEntitlement(t, otherID, liveItem, "", "")

	list, err := svc.Entitlements(ctx, userID, false)
	if err != nil {
		t.Fatalf("列權益: %v", err)
	}
	ids := publicIDs(list)
	if !contains(ids, livePublic) {
		t.Fatalf("有效權益不見了: %v", ids)
	}
	// 過期的仍然回傳(由 ExpiresAt 讓呈現層判斷),撤銷的不回傳。
	if !contains(ids, expiredPublic) {
		t.Fatalf("過期權益應仍回傳(有 ExpiresAt 可判斷): %v", ids)
	}
	if contains(ids, revokedPublic) {
		t.Fatalf("已撤銷的權益不該出現: %v", ids)
	}
	if contains(ids, foreignPublic) {
		t.Fatalf("別人的權益出現在我的清單: %v", ids)
	}

	// includeRevoked=true 才看得到撤銷的,且 revoked_at 有值。
	all, err := svc.Entitlements(ctx, userID, true)
	if err != nil {
		t.Fatalf("列全部權益: %v", err)
	}
	found := false
	for _, e := range all {
		if e.PublicID == revokedPublic {
			found = true
			if e.RevokedAt == nil {
				t.Fatal("已撤銷的權益 RevokedAt 不該是 nil")
			}
		}
	}
	if !found {
		t.Fatalf("includeRevoked=true 應看得到已撤銷的: %v", publicIDs(all))
	}

	// 顯示欄位齊全:商品 public_id、名稱、履約型態都要帶出來。
	for _, e := range all {
		if e.PublicID == livePublic {
			if e.ItemPublicID != liveItemPublic || e.ItemName != "live perk" {
				t.Fatalf("商品顯示欄位不對: %+v", e)
			}
			if e.Fulfillment != shop.FulfillAutoRole {
				t.Fatalf("Fulfillment = %q,預期 auto_role", e.Fulfillment)
			}
			if e.GrantedAt.IsZero() || e.RefundableUntil == nil {
				t.Fatalf("時間欄位不對: %+v", e)
			}
		}
	}

	// 排序穩定:新的在前,連續兩次呼叫順序一致。
	if !sortedDescByGrant(all) {
		t.Fatalf("權益清單未依 granted_at 由新到舊: %v", publicIDs(all))
	}
	again, err := svc.Entitlements(ctx, userID, true)
	if err != nil {
		t.Fatalf("重列權益: %v", err)
	}
	if !sameOrder(publicIDs(all), publicIDs(again)) {
		t.Fatal("兩次呼叫的順序不一致,排序不穩定")
	}

	// 軟刪除的使用者看不到任何權益。
	softDelete(t, userID)
	gone, err := svc.Entitlements(ctx, userID, true)
	if err != nil {
		t.Fatalf("軟刪除後列權益: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("軟刪除使用者應回空清單,得到 %v", publicIDs(gone))
	}
}

func TestRedemptionsList(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ticket-holder")
	otherID, _ := newUser(t, "ticket-other")
	itemID, itemPublic := newItem(t, itemSpec{name: "manual thing", fulfillment: "manual"})

	_, pendingPublic := newRedemption(t, userID, itemID, "pending")
	_, fulfilledPublic := newRedemption(t, userID, itemID, "fulfilled")
	_, rejectedPublic := newRedemption(t, userID, itemID, "rejected")
	_, cancelledPublic := newRedemption(t, userID, itemID, "cancelled")
	_, foreignPublic := newRedemption(t, otherID, itemID, "pending")

	all, err := svc.Redemptions(ctx, userID, "")
	if err != nil {
		t.Fatalf("列全部工單: %v", err)
	}
	ids := redemptionIDs(all)
	for _, want := range []string{pendingPublic, fulfilledPublic, rejectedPublic, cancelledPublic} {
		if !contains(ids, want) {
			t.Fatalf("status 為空應回全部,少了 %s: %v", want, ids)
		}
	}
	if contains(ids, foreignPublic) {
		t.Fatalf("別人的工單出現在我的清單: %v", ids)
	}
	if len(all) != 4 {
		t.Fatalf("預期 4 張工單,得到 %d", len(all))
	}

	// 狀態正確地帶出來,顯示欄位齊全。
	for _, r := range all {
		switch r.PublicID {
		case pendingPublic:
			if r.Status != shop.RedemptionPending {
				t.Fatalf("狀態 = %q,預期 pending", r.Status)
			}
			if r.HandledAt != nil {
				t.Fatal("pending 工單不該有 handled_at")
			}
		case fulfilledPublic:
			if r.Status != shop.RedemptionFulfilled || r.HandledAt == nil {
				t.Fatalf("fulfilled 工單不對: %+v", r)
			}
		}
		if r.ItemPublicID != itemPublic || r.ItemName != "manual thing" {
			t.Fatalf("商品顯示欄位不對: %+v", r)
		}
		if r.Note == "" || r.CreatedAt.IsZero() {
			t.Fatalf("note / created_at 不對: %+v", r)
		}
	}

	// 依狀態過濾。
	for _, tc := range []struct {
		status shop.RedemptionStatus
		want   string
	}{
		{shop.RedemptionPending, pendingPublic},
		{shop.RedemptionFulfilled, fulfilledPublic},
		{shop.RedemptionRejected, rejectedPublic},
		{shop.RedemptionCancelled, cancelledPublic},
	} {
		got, err := svc.Redemptions(ctx, userID, tc.status)
		if err != nil {
			t.Fatalf("依 %s 過濾: %v", tc.status, err)
		}
		if len(got) != 1 || got[0].PublicID != tc.want || got[0].Status != tc.status {
			t.Fatalf("依 %s 過濾應只回 1 張(%s),得到 %+v", tc.status, tc.want, redemptionIDs(got))
		}
	}

	// 排序穩定:新的在前,兩次呼叫一致。
	again, err := svc.Redemptions(ctx, userID, "")
	if err != nil {
		t.Fatalf("重列工單: %v", err)
	}
	if !sameOrder(redemptionIDs(all), redemptionIDs(again)) {
		t.Fatal("兩次呼叫的順序不一致,排序不穩定")
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt.Before(all[i].CreatedAt) {
			t.Fatalf("工單清單未依 created_at 由新到舊: %v", redemptionIDs(all))
		}
	}

	softDelete(t, userID)
	gone, err := svc.Redemptions(ctx, userID, "")
	if err != nil {
		t.Fatalf("軟刪除後列工單: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("軟刪除使用者應回空清單,得到 %v", redemptionIDs(gone))
	}
}

// ── Catalog:上架條件與購買判定必須是同一套 ─────────────────────────────

// TestItemsListingMatchesPurchase 是本 package 最重要的一個測試:
// 型錄的 is_listed 判定(read.sql)與購買的 is_listed 判定(shop.sql)是兩段
// 各自獨立的 SQL,只靠註解要求一致。這裡用同一批資料同時跑兩邊,
// 「看得到的就買得到、看不到的就買不到」被打破時立刻紅燈。
func TestItemsListingMatchesPurchase(t *testing.T) {
	ctx := setup(t)

	cases := []struct {
		name        string
		spec        itemSpec
		wantVisible bool
	}{
		{"上架中", itemSpec{name: "case-listed"}, true},
		{"從未上架", itemSpec{name: "case-unlisted", unlisted: true}, false},
		{"上架時間未到", itemSpec{name: "case-future", futureListed: true}, false},
		{"已下架", itemSpec{name: "case-delisted", delisted: true}, false},
	}

	publicIDByCase := make([]string, len(cases))
	for i, tc := range cases {
		_, pub := newItem(t, tc.spec)
		publicIDByCase[i] = pub
	}

	listed, err := svc.Items(ctx, false)
	if err != nil {
		t.Fatalf("列型錄: %v", err)
	}
	visible := itemIDs(listed)

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := publicIDByCase[i]
			inCatalog := contains(visible, pub)
			if inCatalog != tc.wantVisible {
				t.Fatalf("型錄可見性 = %v,預期 %v", inCatalog, tc.wantVisible)
			}

			// 每個 case 用獨立買家,免得 per_user_limit / 既有權益互相干擾。
			buyer, _ := newUser(t, "buyer-"+tc.name)
			_, err := shp.Purchase(ctx, shop.PurchaseParams{
				UserID: buyer, ItemPublicID: pub,
				IdempotencyKey: fmt.Sprintf("readtest-buy-%s-%d", pub, buyer),
			})
			purchasable := err == nil
			if !purchasable && !errors.Is(err, shop.ErrItemNotListed) {
				t.Fatalf("購買失敗但原因不是上架判定: %v", err)
			}
			if purchasable != inCatalog {
				t.Fatalf("型錄可見=%v 但可購買=%v ——兩套上架判定已經分岔", inCatalog, purchasable)
			}
		})
	}

	// includeDelisted=true 只多露出「已下架」的絕版品(它們仍然不可購買),
	// 從未上架與上架時間未到的草稿任何情況都不露出。
	withDelisted, err := svc.Items(ctx, true)
	if err != nil {
		t.Fatalf("列含下架的型錄: %v", err)
	}
	all := itemIDs(withDelisted)
	if !contains(all, publicIDByCase[3]) {
		t.Fatal("includeDelisted=true 應看得到已下架的商品")
	}
	if contains(all, publicIDByCase[1]) || contains(all, publicIDByCase[2]) {
		t.Fatal("從未上架 / 上架時間未到的商品不該出現在任何型錄")
	}
}

func TestItemsFields(t *testing.T) {
	ctx := setup(t)
	_, pub := newItem(t, itemSpec{name: "field check", fulfillment: "auto_role", price: 250})

	list, err := svc.Items(ctx, false)
	if err != nil {
		t.Fatalf("列型錄: %v", err)
	}
	for _, it := range list {
		if it.PublicID != pub {
			continue
		}
		if it.Name != "field check" || it.Description != "desc" {
			t.Fatalf("名稱/描述不對: %+v", it)
		}
		if it.Fulfillment != shop.FulfillAutoRole {
			t.Fatalf("Fulfillment = %q", it.Fulfillment)
		}
		if it.Currency != "coin" || it.Price != 250 || it.RefundWindowSeconds != 600 {
			t.Fatalf("幣別/價格/退款窗口不對: %+v", it)
		}
		if it.ListedAt == nil || it.DelistedAt != nil {
			t.Fatalf("上架時間欄位不對: %+v", it)
		}
		if it.DurationDays != nil || it.PerUserLimit != nil {
			t.Fatalf("NULL 的可選欄位應保持 nil: %+v", it)
		}
		return
	}
	t.Fatalf("剛建的商品不在型錄裡: %v", itemIDs(list))
}

// TestItemsRowLimitAndStableOrder 驗證列表有上限、且排序在同分時仍然穩定。
// 五個商品用同一個 INSERT 建立 → listed_at 完全相同,只剩 id tie-break 能定序;
// 沒有 tie-break 的話這個測試會間歇性失敗(分頁漂移的縮影)。
func TestItemsRowLimitAndStableOrder(t *testing.T) {
	ctx := setup(t)

	// listed_at = now():保證比其他測試建立的商品(now() - 1 hour)都新,
	// 因此穩定佔據「新到舊」排序的最前面。
	rows, err := pool.Query(ctx,
		`INSERT INTO platform.shop_items
		   (public_id, name, fulfillment, currency, price, refund_window_seconds, listed_at)
		 SELECT gen_random_uuid()::text, 'limit-' || g, 'auto_perk', 'coin', 0, 600, now()
		 FROM generate_series(1, 5) g
		 RETURNING public_id`)
	if err != nil {
		t.Fatalf("批次建商品: %v", err)
	}
	defer rows.Close()
	var mine []string
	for rows.Next() {
		var pub string
		if err := rows.Scan(&pub); err != nil {
			t.Fatalf("掃描 public_id: %v", err)
		}
		mine = append(mine, pub)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("批次建商品: %v", err)
	}
	if len(mine) != 5 {
		t.Fatalf("預期建 5 個商品,得到 %d", len(mine))
	}

	limited := readpg.New(pool, readpg.WithRowLimit(3))
	first, err := limited.Items(ctx, false)
	if err != nil {
		t.Fatalf("列型錄(上限 3): %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("上限 3 應只回 3 筆,得到 %d", len(first))
	}
	// 這 3 筆必須都是剛才那批(listed_at 最新),否則排序沒生效。
	for _, it := range first {
		if !contains(mine, it.PublicID) {
			t.Fatalf("上限內出現非最新批次的商品 %s", it.PublicID)
		}
	}
	// 同 listed_at 下靠 id DESC 定序 → 最後插入的三個。
	if !sameOrder(itemIDs(first), []string{mine[4], mine[3], mine[2]}) {
		t.Fatalf("同 listed_at 的排序不是 id DESC: %v", itemIDs(first))
	}
	for range 5 {
		again, err := limited.Items(ctx, false)
		if err != nil {
			t.Fatalf("重列型錄: %v", err)
		}
		if !sameOrder(itemIDs(first), itemIDs(again)) {
			t.Fatalf("重複呼叫順序不一致:%v vs %v", itemIDs(first), itemIDs(again))
		}
	}

	// WithRowLimit(<=0) 視為不設定,沿用預設。
	def := readpg.New(pool, readpg.WithRowLimit(0))
	if got, err := def.Items(ctx, false); err != nil || len(got) > readpg.DefaultRowLimit {
		t.Fatalf("非法上限應沿用預設,得到 len=%d err=%v", len(got), err)
	}
}

// TestListsRespectLimitPerUser 確認使用者維度的列表同樣吃上限。
func TestListsRespectLimitPerUser(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "many")
	itemID, _ := newItem(t, itemSpec{name: "bulk", fulfillment: "manual"})
	for range 4 {
		grantEntitlement(t, userID, itemID, "", "")
		newRedemption(t, userID, itemID, "pending")
	}

	limited := readpg.New(pool, readpg.WithRowLimit(2))
	ents, err := limited.Entitlements(ctx, userID, true)
	if err != nil {
		t.Fatalf("列權益: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("權益清單應受上限 2 限制,得到 %d", len(ents))
	}
	reds, err := limited.Redemptions(ctx, userID, "")
	if err != nil {
		t.Fatalf("列工單: %v", err)
	}
	if len(reds) != 2 {
		t.Fatalf("工單清單應受上限 2 限制,得到 %d", len(reds))
	}
	bals, err := limited.Balances(ctx, userID)
	if err != nil {
		t.Fatalf("列餘額: %v", err)
	}
	if len(bals) > 2 {
		t.Fatalf("餘額清單應受上限 2 限制,得到 %d", len(bals))
	}
}

// TestConcurrentReads 讀取側沒有共用可變狀態,併發呼叫必須全部成功且結果一致
// (連點「我的頁面」是最常見的實際流量形狀)。
func TestConcurrentReads(t *testing.T) {
	ctx := setup(t)
	userID, publicID := newUser(t, "concurrent")
	itemID, _ := newItem(t, itemSpec{name: "conc"})
	_, entPublic := grantEntitlement(t, userID, itemID, "", "")
	credit(t, userID, "coin", 42)

	const n = 24
	var wg sync.WaitGroup
	errCh := make(chan error, n*5)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := svc.UserID(ctx, publicID); err != nil || id != userID {
				errCh <- fmt.Errorf("UserID = %d, err=%w", id, err)
			}
			if p, err := svc.Profile(ctx, userID); err != nil || p.PublicID != publicID {
				errCh <- fmt.Errorf("Profile err=%w", err)
			}
			if b, err := svc.Balances(ctx, userID); err != nil || len(b) != 1 || b[0].Amount != 42 {
				errCh <- fmt.Errorf("Balances = %+v, err=%w", b, err)
			}
			if e, err := svc.Entitlements(ctx, userID, false); err != nil ||
				len(e) != 1 || e[0].PublicID != entPublic {
				errCh <- fmt.Errorf("Entitlements = %v, err=%w", publicIDs(e), err)
			}
			if _, err := svc.Items(ctx, false); err != nil {
				errCh <- fmt.Errorf("Items err=%w", err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("併發讀取失敗: %v", err)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func publicIDs(xs []readmodel.EntitlementView) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.PublicID)
	}
	return out
}

func redemptionIDs(xs []readmodel.RedemptionView) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.PublicID)
	}
	return out
}

func itemIDs(xs []readmodel.ItemView) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.PublicID)
	}
	return out
}

func sortedDescByGrant(xs []readmodel.EntitlementView) bool {
	var prev time.Time
	for i, x := range xs {
		if i > 0 && prev.Before(x.GrantedAt) {
			return false
		}
		prev = x.GrantedAt
	}
	return true
}
