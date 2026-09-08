package readpg_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/infrastructure/storage/admineconpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/readpg"
)

// LedgerReader(管理端翻帳本)的整合測試。
//
// 資料鋪設說明:分頁 / 分區 / 排序的測試需要**指定 created_at**,而動錢入口
// (ledger.Apply)一律用 DB 的 now(),控制不了時間。所以這些 case 直接 INSERT
// token_entries 鋪資料——它是讀取側的測試佈景,不是動錢路徑;
// 真正的動錢語意由 TestListEntriesRefundedMatchesAdminecon 用**真的**
// adminecon.Refund 驗證。任何情況下本 package 都不 UPDATE/DELETE token_entries。

// ensureMonthPartition 為指定月份補上分區(冪等,migration 的同一支函式)。
func ensureMonthPartition(t *testing.T, month time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`SELECT platform.create_month_partition('platform.token_entries', $1::date)`,
		month); err != nil {
		t.Fatalf("建月分區 %s: %v", month.Format("2006-01"), err)
	}
}

// insertEntries 在單一 INSERT 內鋪 n 筆分錄。
// 單一語句 ⇒ 這 n 筆的 created_at **完全相同**(now() 在語句/交易內是常數),
// 正是分頁 tie-break 要測的極端情況。回傳 id 升冪。
func insertEntries(t *testing.T, userID int64, at time.Time, n int, amount int64, reason string) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`INSERT INTO platform.token_entries (user_id, currency, amount, reason, created_at)
		 SELECT $1, 'coin', $2, $3, $4::timestamptz FROM generate_series(1, $5)
		 RETURNING id`,
		userID, amount, reason, at, n)
	if err != nil {
		t.Fatalf("鋪分錄: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("掃描分錄 id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("鋪分錄: %v", err)
	}
	return ids
}

func refs(page *readmodel.LedgerEntriesPage) []string {
	out := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, e.EntryRef)
	}
	return out
}

// ── 分頁 ────────────────────────────────────────────────────────────────

// TestListEntriesPaginationCoversEverything 走完全部資料:不重、不漏、順序嚴格遞減。
// 三批分錄各自共用同一個 created_at(單一 INSERT),所以跨頁邊界一定會落在
// 「同 created_at 的中間」——沒有 id tie-break 的話這個測試必紅。
func TestListEntriesPaginationCoversEverything(t *testing.T) {
	ctx := setup(t)
	userID, publicID := newUser(t, "ledger-page")

	base := time.Now().UTC().Truncate(time.Hour)
	var want []int64 // 期望的回傳順序:created_at DESC, id DESC
	batches := []struct {
		at time.Time
		n  int
	}{
		{base.Add(-3 * time.Hour), 7},
		{base.Add(-2 * time.Hour), 6},
		{base.Add(-1 * time.Hour), 7},
	}
	byBatch := make([][]int64, len(batches))
	for i, b := range batches {
		byBatch[i] = insertEntries(t, userID, b.at, b.n, -10, string(ledger.ReasonShopPurchase))
	}
	// created_at 由新到舊 = 批次反序;批次內 id 由大到小。
	for i := len(byBatch) - 1; i >= 0; i-- {
		ids := byBatch[i]
		for j := len(ids) - 1; j >= 0; j-- {
			want = append(want, ids[j])
		}
	}

	const pageSize = 4
	var got []string
	seen := make(map[string]int)
	token := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("翻頁沒有終止,游標可能沒有前進")
		}
		page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
			UserID: userID, PageSize: pageSize, PageToken: token,
		})
		if err != nil {
			t.Fatalf("第 %d 頁: %v", pages, err)
		}
		if len(page.Entries) > pageSize {
			t.Fatalf("第 %d 頁回了 %d 筆,超過 PageSize", pages, len(page.Entries))
		}
		for _, e := range page.Entries {
			seen[e.EntryRef]++
			got = append(got, e.EntryRef)
			if e.UserPublicID != publicID {
				t.Fatalf("分錄 %s 屬於別人: %s", e.EntryRef, e.UserPublicID)
			}
		}
		if page.NextPageToken == "" {
			// 最後一頁:不該是空頁(多撈一列的判斷正確的話,剛好翻完就沒有 token)
			if len(page.Entries) == 0 && pages > 0 {
				t.Fatal("最後多回了一頁空的,NextPageToken 的判斷有誤")
			}
			break
		}
		if page.NextPageToken == token {
			t.Fatal("NextPageToken 沒有前進,會無限迴圈")
		}
		token = page.NextPageToken
	}

	if len(got) != len(want) {
		t.Fatalf("共取回 %d 筆,預期 %d 筆(不重不漏)", len(got), len(want))
	}
	for ref, n := range seen {
		if n != 1 {
			t.Fatalf("分錄 %s 出現 %d 次,分頁重複", ref, n)
		}
	}
	for i, id := range want {
		if got[i] != strconv.FormatInt(id, 10) {
			t.Fatalf("第 %d 筆 = %s,預期 %d(created_at DESC, id DESC)", i, got[i], id)
		}
	}

	// 一次撈完(PageSize 大於總數)應該完全沒有 NextPageToken。
	whole, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{UserID: userID, PageSize: 100})
	if err != nil {
		t.Fatalf("一次撈完: %v", err)
	}
	if len(whole.Entries) != len(want) || whole.NextPageToken != "" {
		t.Fatalf("一次撈完應為 %d 筆且無 token,得到 %d 筆 token=%q",
			len(want), len(whole.Entries), whole.NextPageToken)
	}
	// 逐頁走完的順序必須與一次撈完完全一致。
	if !sameOrder(got, refs(whole)) {
		t.Fatal("逐頁翻與一次撈完的順序不一致")
	}

	// 邊界:PageSize 剛好等於總數 → 沒有下一頁(不該多回一頁空的)。
	exact, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
		UserID: userID, PageSize: int32(len(want)),
	})
	if err != nil {
		t.Fatalf("PageSize 等於總數: %v", err)
	}
	if exact.NextPageToken != "" {
		t.Fatal("PageSize 剛好等於總數時不該再給 NextPageToken")
	}
}

func TestListEntriesPageSizeBounds(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ledger-size")
	at := time.Now().UTC().Add(-30 * time.Minute)
	insertEntries(t, userID, at, 5, -1, string(ledger.ReasonShopPurchase))

	// PageSize=1:一筆 + 有下一頁。
	one, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{UserID: userID, PageSize: 1})
	if err != nil {
		t.Fatalf("PageSize=1: %v", err)
	}
	if len(one.Entries) != 1 || one.NextPageToken == "" {
		t.Fatalf("PageSize=1 應回 1 筆且有 token,得到 %d 筆 token=%q",
			len(one.Entries), one.NextPageToken)
	}

	// PageSize <= 0 / 超過上限:防禦性夾到上限,不回錯。
	limited := readpg.New(pool, readpg.WithRowLimit(3))
	for _, size := range []int32{0, -5, 999} {
		page, err := limited.ListEntries(ctx, readmodel.LedgerEntriesQuery{
			UserID: userID, PageSize: size,
		})
		if err != nil {
			t.Fatalf("PageSize=%d: %v", size, err)
		}
		if len(page.Entries) != 3 {
			t.Fatalf("PageSize=%d 應夾到上限 3,得到 %d 筆", size, len(page.Entries))
		}
	}

	// user_id 不合法 → InvalidArgument 語意,不是 panic、也不是空頁。
	for _, id := range []int64{0, -1} {
		if _, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{UserID: id, PageSize: 10}); !errorIs(err, adminecon.ErrInvalidInput) {
			t.Fatalf("UserID=%d 應回 ErrInvalidInput,得到 %v", id, err)
		}
	}
}

func TestListEntriesEmpty(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ledger-empty")

	page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{UserID: userID, PageSize: 20})
	if err != nil {
		t.Fatalf("空結果: %v", err)
	}
	if page.Entries == nil {
		t.Fatal("Entries 應為空 slice 而非 nil")
	}
	if len(page.Entries) != 0 || page.NextPageToken != "" {
		t.Fatalf("沒有分錄時應回空頁無 token,得到 %d 筆 token=%q",
			len(page.Entries), page.NextPageToken)
	}
}

// ── 時間範圍 ────────────────────────────────────────────────────────────

// TestListEntriesTimeRange:After 含、Before 不含(port 契約)。
func TestListEntriesTimeRange(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ledger-range")

	base := time.Now().UTC().Truncate(time.Second)
	t0 := base.Add(-3 * time.Hour)
	t1 := base.Add(-2 * time.Hour)
	t2 := base.Add(-1 * time.Hour)
	id0 := insertEntries(t, userID, t0, 1, -1, string(ledger.ReasonShopPurchase))[0]
	id1 := insertEntries(t, userID, t1, 1, -2, string(ledger.ReasonShopPurchase))[0]
	id2 := insertEntries(t, userID, t2, 1, -3, string(ledger.ReasonShopPurchase))[0]

	ref := func(id int64) string { return strconv.FormatInt(id, 10) }

	cases := []struct {
		name   string
		after  *time.Time
		before *time.Time
		want   []string
	}{
		{"不限", nil, nil, []string{ref(id2), ref(id1), ref(id0)}},
		{"after 含邊界", &t1, nil, []string{ref(id2), ref(id1)}},
		{"before 不含邊界", nil, &t2, []string{ref(id1), ref(id0)}},
		{"兩邊都給", &t1, &t2, []string{ref(id1)}},
		{"區間顛倒 → 空", &t2, &t1, nil},
		{"區間內無資料 → 空", ptrTime(base.Add(time.Hour)), ptrTime(base.Add(2 * time.Hour)), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
				UserID: userID, After: tc.after, Before: tc.before, PageSize: 50,
			})
			if err != nil {
				t.Fatalf("查詢: %v", err)
			}
			if !sameOrder(refs(page), tc.want) {
				t.Fatalf("得到 %v,預期 %v", refs(page), tc.want)
			}
		})
	}

	// 時間範圍 + 分頁一起用:範圍條件必須跟著游標一路帶下去。
	page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
		UserID: userID, After: &t0, Before: ptrTime(t2.Add(time.Second)), PageSize: 1,
	})
	if err != nil {
		t.Fatalf("範圍 + 分頁第一頁: %v", err)
	}
	var walked []string
	for {
		walked = append(walked, refs(page)...)
		if page.NextPageToken == "" {
			break
		}
		page, err = svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
			UserID: userID, After: &t0, Before: ptrTime(t2.Add(time.Second)),
			PageSize: 1, PageToken: page.NextPageToken,
		})
		if err != nil {
			t.Fatalf("範圍 + 分頁續頁: %v", err)
		}
	}
	if !sameOrder(walked, []string{ref(id2), ref(id1), ref(id0)}) {
		t.Fatalf("範圍 + 分頁走完 = %v", walked)
	}
}

// ── 游標的來源驗證 ──────────────────────────────────────────────────────

// TestListEntriesPageTokenRejected:畸形 / 竄改的游標一律回可辨識錯誤,
// 而且無論如何都不可能讀到別人的分錄。
func TestListEntriesPageTokenRejected(t *testing.T) {
	ctx := setup(t)
	userA, _ := newUser(t, "cursor-a")
	userB, publicB := newUser(t, "cursor-b")
	at := time.Now().UTC().Add(-10 * time.Minute)
	insertEntries(t, userA, at, 3, -1, string(ledger.ReasonShopPurchase))
	insertEntries(t, userB, at, 3, -1, string(ledger.ReasonShopPurchase))

	// A 的合法游標。
	pageA, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{UserID: userA, PageSize: 1})
	if err != nil || pageA.NextPageToken == "" {
		t.Fatalf("取 A 的游標: %v / token=%q", err, pageA.NextPageToken)
	}
	tokenA := pageA.NextPageToken

	bad := []struct {
		name  string
		token string
	}{
		{"不是 base64", "!!!not-base64!!!"},
		{"base64 但不是游標", base64.RawURLEncoding.EncodeToString([]byte("random-garbage"))},
		{"欄位數不足", base64.RawURLEncoding.EncodeToString([]byte("v1|1|2"))},
		{"版本不符", base64.RawURLEncoding.EncodeToString([]byte("v0|1|2|3"))},
		{"user 欄位非整數", base64.RawURLEncoding.EncodeToString([]byte("v1|abc|2|3"))},
		{"時間欄位非整數", base64.RawURLEncoding.EncodeToString(
			[]byte(fmt.Sprintf("v1|%d|xx|3", userB)))},
		{"id 欄位非整數", base64.RawURLEncoding.EncodeToString(
			[]byte(fmt.Sprintf("v1|%d|2|yy", userB)))},
		// 最關鍵的一條:把 A 的游標貼到 B 的查詢。
		{"別人的游標", tokenA},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
				UserID: userB, PageSize: 10, PageToken: tc.token,
			})
			if !errorIs(err, adminecon.ErrInvalidInput) {
				t.Fatalf("預期 ErrInvalidInput,得到 %v", err)
			}
			if page != nil {
				t.Fatal("被拒絕的查詢不該回任何資料")
			}
		})
	}

	// 偽造一個「格式完全合法、但位置被亂改」的 B 自己的游標:
	// 游標只決定位置,user_id 永遠來自參數 —— 結果只可能是 B 自己的分錄。
	forged := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf("v1|%d|%d|%d", userB, time.Now().AddDate(10, 0, 0).UnixMicro(), 1<<40)))
	page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
		UserID: userB, PageSize: 50, PageToken: forged,
	})
	if err != nil {
		t.Fatalf("格式合法的偽造游標不該報錯: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("竄改位置後仍應只看到 B 自己的 3 筆,得到 %d 筆", len(page.Entries))
	}
	for _, e := range page.Entries {
		if e.UserPublicID != publicB {
			t.Fatalf("竄改游標讀到別人的分錄: %s", e.UserPublicID)
		}
	}
}

// ── Refunded 與實際退款一致 ────────────────────────────────────────────

// TestListEntriesRefundedMatchesAdminecon 走**真的**動錢路徑:
// Grant → Deduct → adminecon.Refund,驗證 Refunded 的判定
// (ref_type='token_entry' 指向原分錄)與實際退款一致。
func TestListEntriesRefundedMatchesAdminecon(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ledger-refund")
	adminID, adminPublic := newUser(t, "ledger-admin")
	admin := admineconpg.New(pool, ledgerpg.New(pool))

	if _, err := admin.Grant(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("readtest-grant-%d", userID),
		ActorUserID:    adminID, UserID: userID, Currency: "coin", Amount: 1000,
		Reason: "測試發點",
	}); err != nil {
		t.Fatalf("發點: %v", err)
	}
	deduct, err := admin.Deduct(ctx, adminecon.AdjustParams{
		IdempotencyKey: fmt.Sprintf("readtest-deduct-%d", userID),
		ActorUserID:    adminID, UserID: userID, Currency: "coin", Amount: 300,
		Reason: "測試扣點",
	})
	if err != nil {
		t.Fatalf("扣點: %v", err)
	}
	deductRef := strconv.FormatInt(deduct.EntryID, 10)

	find := func(ref string) *readmodel.LedgerEntryView {
		t.Helper()
		page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{UserID: userID, PageSize: 50})
		if err != nil {
			t.Fatalf("列分錄: %v", err)
		}
		for i := range page.Entries {
			if page.Entries[i].EntryRef == ref {
				return &page.Entries[i]
			}
		}
		t.Fatalf("分錄 %s 不在清單裡: %v", ref, refs(page))
		return nil
	}

	// 退款前:扣款分錄未被沖銷,且管理員操作要帶 actor。
	before := find(deductRef)
	if before.Refunded {
		t.Fatal("還沒退款,Refunded 不該是 true")
	}
	if before.Amount != -300 || before.Currency != "coin" ||
		before.Reason != string(ledger.ReasonAdminDeduct) {
		t.Fatalf("扣款分錄內容不對: %+v", before)
	}
	if before.ActorPublicID != adminPublic {
		t.Fatalf("ActorPublicID = %q,預期管理員 %q", before.ActorPublicID, adminPublic)
	}

	// 真的退一次。
	refund, err := admin.Refund(ctx, adminecon.RefundParams{
		ActorUserID: adminID, EntryID: deduct.EntryID, Reason: "測試退款",
	})
	if err != nil {
		t.Fatalf("退款: %v", err)
	}

	// 退款後:原扣款分錄 Refunded=true;退款分錄本身沒有被沖銷。
	after := find(deductRef)
	if !after.Refunded {
		t.Fatal("退款後原扣款分錄的 Refunded 應為 true")
	}
	refundEntry := find(strconv.FormatInt(refund.EntryID, 10))
	if refundEntry.Refunded {
		t.Fatal("退款分錄本身不該被標成已沖銷")
	}
	if refundEntry.Amount != 300 || refundEntry.Reason != string(ledger.ReasonRefund) {
		t.Fatalf("退款分錄內容不對: %+v", refundEntry)
	}

	// 非管理員操作(簽到之類)沒有 actor → ActorPublicID 空字串。
	plainID := insertEntries(t, userID, time.Now().UTC(), 1, 50, string(ledger.ReasonDailyClaim))[0]
	plain := find(strconv.FormatInt(plainID, 10))
	if plain.ActorPublicID != "" {
		t.Fatalf("沒有 actor 的分錄 ActorPublicID 應為空,得到 %q", plain.ActorPublicID)
	}
	if plain.Refunded {
		t.Fatal("入帳分錄沒有沖銷,Refunded 應為 false")
	}
}

// TestListEntriesConcurrentPaging:讀取側沒有共用可變狀態,
// 多個呼叫端同時翻同一個使用者的帳本,必須各自拿到完全一樣的結果。
// (游標的編解碼若不小心用了共享 buffer,這個測試會抓到。)
func TestListEntriesConcurrentPaging(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ledger-conc")
	at := time.Now().UTC().Add(-20 * time.Minute)
	insertEntries(t, userID, at, 9, -1, string(ledger.ReasonShopPurchase))
	insertEntries(t, userID, at.Add(time.Minute), 8, -2, string(ledger.ReasonShopPurchase))

	walk := func() ([]string, error) {
		var out []string
		token := ""
		for i := 0; i < 50; i++ {
			page, err := svc.ListEntries(ctx, readmodel.LedgerEntriesQuery{
				UserID: userID, PageSize: 3, PageToken: token,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, refs(page)...)
			if page.NextPageToken == "" {
				return out, nil
			}
			token = page.NextPageToken
		}
		return nil, fmt.Errorf("翻頁沒有終止")
	}

	want, err := walk()
	if err != nil {
		t.Fatalf("基準翻頁: %v", err)
	}
	if len(want) != 17 {
		t.Fatalf("基準應為 17 筆,得到 %d", len(want))
	}

	const n = 16
	var wg sync.WaitGroup
	results := make([][]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = walk()
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("併發翻頁 %d: %v", i, errs[i])
		}
		if !sameOrder(results[i], want) {
			t.Fatalf("併發翻頁 %d 的結果與基準不同:%v vs %v", i, results[i], want)
		}
	}
}

// ── 查詢計畫:keyset 走索引 + 分區裁剪 ──────────────────────────────────

// TestListEntriesPlanPrunesPartitions 用**真的那一段 SQL**(從 queries/read.sql
// 讀出來,把 sqlc 佔位符換成 $n)跑 EXPLAIN ANALYZE,確認:
//   - 時間範圍條件讓 planner 裁掉不相關的月分區(created_at 是分區鍵);
//   - 掃描走索引,不是整表掃。
//
// 從檔案讀 SQL 而不是在測試裡複製一份,是為了避免「測試驗的是另一段 SQL」。
func TestListEntriesPlanPrunesPartitions(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "ledger-plan")

	// 三個過去的月份各鋪一批,確保有多個分區可以被裁掉。
	monthStart := func(offset int) time.Time {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, offset, 0)
	}
	target := monthStart(-2)
	for _, off := range []int{-3, -2, -1} {
		ensureMonthPartition(t, monthStart(off))
	}
	// 目標分區鋪多一點資料,planner 才有理由選索引而不是整表掃。
	insertEntries(t, userID, monthStart(-3).Add(72*time.Hour), 50, -1, string(ledger.ReasonShopPurchase))
	insertEntries(t, userID, target.Add(72*time.Hour), 1500, -1, string(ledger.ReasonShopPurchase))
	insertEntries(t, userID, monthStart(-1).Add(72*time.Hour), 50, -1, string(ledger.ReasonShopPurchase))
	if _, err := pool.Exec(ctx, `ANALYZE platform.token_entries`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	sql := queryFromReadSQL(t, "ListLedgerEntries")
	after := target
	before := target.AddDate(0, 1, 0)
	plan := explain(t, sql, userID, &after, &before, nil, int64(0), int32(20))

	// 1) 分區裁剪:驅動掃描(別名 e,CTE 內那個)只該碰目標月份的分區。
	//    用別名區分是必要的——退款探測(別名 r)本來就會碰每個分區,
	//    它不是分區裁剪要管的事(ref_id 不是分區鍵)。
	partitions := tokenEntryPartitions(t)
	targetPart := fmt.Sprintf("token_entries_%s", target.Format("2006_01"))
	if !strings.Contains(plan, "on "+targetPart+" e") {
		t.Fatalf("計畫裡沒有掃描目標分區 %s:\n%s", targetPart, plan)
	}
	pruned := 0
	for _, part := range partitions {
		if part == targetPart {
			continue
		}
		if strings.Contains(plan, "on "+part+" e") {
			t.Fatalf("分區 %s 沒有被裁掉(created_at 條件應該讓 planner 排除它):\n%s", part, plan)
		}
		pruned++
	}
	if pruned == 0 {
		t.Fatal("測試環境只有一個分區,裁剪沒被真正驗證")
	}
	t.Logf("分區裁剪:驅動掃描排除了 %d 個分區,只掃 %s", pruned, targetPart)

	// 2) keyset 走 (user_id, created_at) 索引,不是整表掃。
	if strings.Contains(plan, "Seq Scan on "+targetPart+" e") {
		t.Fatalf("目標分區被整表掃,keyset 沒有用到索引:\n%s", plan)
	}
	if !strings.Contains(plan, "user_id_created_at_idx") {
		t.Fatalf("計畫沒有用到 (user_id, created_at) 索引:\n%s", plan)
	}

	// 3) 退款探測必須被 CTE 的 LIMIT 綁住:探測次數 = 一頁的筆數,
	//    不是「範圍內全部分錄」,更不是把全表的沖銷分錄讀進雜湊表。
	if strings.Contains(plan, "SubPlan") {
		t.Fatalf("退款判定又變回 SubPlan(會掃全部沖銷分錄):\n%s", plan)
	}
	if !strings.Contains(plan, "ref_type_ref_id_idx") {
		t.Logf("提醒:退款探測沒走 ref 索引(測試資料量小,planner 可能選整表掃):\n%s", plan)
	}

	// 4) 帶游標的第二頁同樣要裁剪(冗餘的 created_at <= cursor 條件的用途:
	//    ROW(a,b) < ROW(c,d) 這種形式 planner 推不出分區邊界)。
	cursorAt := target.Add(72 * time.Hour)
	plan2 := explain(t, sql, userID, nil, nil, &cursorAt, int64(1<<40), int32(20))
	newerPart := fmt.Sprintf("token_entries_%s", monthStart(-1).Format("2006_01"))
	if strings.Contains(plan2, "on "+newerPart+" e") {
		t.Fatalf("游標之後(更新)的分區 %s 沒有被裁掉:\n%s", newerPart, plan2)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func ptrTime(t time.Time) *time.Time { return &t }

// errorIs 是 errors.Is 的薄包裝:nil 一律算不符,避免「根本沒出錯」也通過。
func errorIs(err, target error) bool {
	return err != nil && errors.Is(err, target)
}

func explain(t *testing.T, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("讀計畫: %v", err)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	return sb.String()
}

func tokenEntryPartitions(t *testing.T) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.relname FROM pg_inherits i
		 JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'platform.token_entries'::regclass
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("查分區清單: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("讀分區名: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("查分區清單: %v", err)
	}
	return out
}

var sqlcArgRe = regexp.MustCompile(`sqlc\.n?arg\(([a-z_]+)\)`)

// queryFromReadSQL 從 queries/read.sql 取出指定查詢,並把 sqlc 佔位符換成 $n
// (依首次出現順序編號,與 sqlc 的規則相同)。
func queryFromReadSQL(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../queries/read.sql")
	if err != nil {
		t.Fatalf("讀 read.sql: %v", err)
	}
	marker := "-- name: " + name + " :"
	idx := strings.Index(string(raw), marker)
	if idx < 0 {
		t.Fatalf("read.sql 裡找不到查詢 %s", name)
	}
	body := string(raw)[idx:]
	if next := strings.Index(body[len(marker):], "-- name: "); next >= 0 {
		body = body[:len(marker)+next]
	}
	body = strings.TrimSpace(body)
	body = strings.TrimSuffix(body, ";")

	order := map[string]int{}
	next := 0
	body = sqlcArgRe.ReplaceAllStringFunc(body, func(m string) string {
		arg := sqlcArgRe.FindStringSubmatch(m)[1]
		if n, ok := order[arg]; ok {
			return "$" + strconv.Itoa(n)
		}
		next++
		order[arg] = next
		return "$" + strconv.Itoa(next)
	})
	return body
}
