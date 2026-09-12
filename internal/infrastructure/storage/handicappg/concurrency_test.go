package handicappg_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
)

// TestConcurrentSelectSameItem 是這個 package 存在的主要理由。
//
// 八個 goroutine 同時買同一項,預算只夠兩次 —— 恰好兩次成功,其餘全部 BP 不足。
// 驗的是 LockMatch 的 FOR UPDATE 真的把寫入路徑序列化了:規則層的餘額檢查
// 與寫入之間沒有縫,不會有兩個請求同時看到「還剩 24 BP」。
//
// core 的測試用一把 Go mutex 模擬這件事,那只能證明規則在序列化前提下正確;
// 前提本身成不成立,只有真的 Postgres 答得出來。
func TestConcurrentSelectSameItem(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}

	const goroutines = 8
	const affordable = 2 // budget 24 / cost 10

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 盡量讓八個請求撞在一起,而不是排成隊
			_, errs[i] = svc.Select(ctx, handicap.SelectParams{
				MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
			})
		}()
	}
	close(start)
	wg.Wait()

	ok, insufficient := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, handicap.ErrInsufficientBP):
			insufficient++
		default:
			t.Errorf("非預期的錯誤:%v", err)
		}
	}
	if ok != affordable || insufficient != goroutines-affordable {
		t.Fatalf("成功 %d / BP不足 %d,想要 %d / %d", ok, insufficient, affordable, goroutines-affordable)
	}
	assertSpentConsistent(t, f.matchID, f.holderID)
	if n := countSelections(t, f.matchID, f.holderID, true); n != affordable {
		t.Errorf("選擇列數 = %d,想要 %d", n, affordable)
	}
	b, err := repo.GetBudget(ctx, f.matchID, f.holderID)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if b.Spent != 20 {
		t.Errorf("spent = %d,想要 20", b.Spent)
	}
}

// TestConcurrentSelectAndLock 驗證「一邊買一邊封盤」沒有縫。
//
// 每一次購買只可能是成功或 ErrHandicapLocked —— 不會有「檢查時還沒封盤、
// 寫入時已經封盤」的第三種結果。而封盤當下回傳的那份清單就是最終清單:
// 封盤之後不可能再多出一列,否則對手照著公告準備的條件會被事後追加。
func TestConcurrentSelectAndLock(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget: %v", err)
	}

	const buyers = 6 // budget 24 / cost 4,全買得起,所以只有封盤會擋下它們

	var wg sync.WaitGroup
	errs := make([]error, buyers)
	start := make(chan struct{})
	for i := range buyers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = svc.Select(ctx, handicap.SelectParams{
				MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_四點"].Ref,
			})
		}()
	}

	var view *handicap.MatchHandicaps
	var lockErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		view, lockErr = svc.Lock(ctx, f.matchRef)
	}()

	close(start)
	wg.Wait()

	if lockErr != nil {
		t.Fatalf("Lock: %v", lockErr)
	}
	bought := 0
	for _, err := range errs {
		switch {
		case err == nil:
			bought++
		case errors.Is(err, handicap.ErrHandicapLocked):
		default:
			t.Errorf("非預期的錯誤:%v", err)
		}
	}
	if len(view.Selections) != bought {
		t.Errorf("封盤時看到 %d 項,實際成功 %d 次", len(view.Selections), bought)
	}
	// 封盤後清單不再變動:此刻重讀應與封盤當下逐筆相同。
	after, err := repo.ListSelections(ctx, f.matchID, f.holderID)
	if err != nil {
		t.Fatalf("ListSelections: %v", err)
	}
	if len(after) != len(view.Selections) {
		t.Fatalf("封盤後清單長度 = %d,封盤當下 = %d", len(after), len(view.Selections))
	}
	for i := range after {
		if after[i].PublicID != view.Selections[i].PublicID {
			t.Errorf("第 %d 筆 = %s,封盤當下是 %s", i, after[i].PublicID, view.Selections[i].PublicID)
		}
	}
	assertSpentConsistent(t, f.matchID, f.holderID)
}

// TestConcurrentGrantBudget 驗證同一場被重複發預算時只會建一列。
//
// 賽程推進與裁判手動觸發可能同時呼叫 GrantBudget;預期結果是其中一個建列、
// 其餘的走 ErrBudgetExists 分支比對金額後原樣回傳,而不是撞出裸的 23505。
func TestConcurrentGrantBudget(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	svc := handicap.New(repo)
	f := newFixture(t, defaultOpt())

	const goroutines = 6
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	budgets := make([]int64, goroutines)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, err := svc.GrantBudget(ctx, f.matchRef)
			errs[i] = err
			if b != nil {
				budgets[i] = b.Budget
			}
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 個 GrantBudget: %v", i, err)
		}
		if budgets[i] != 24 {
			t.Errorf("第 %d 個拿到 %d BP,想要 24", i, budgets[i])
		}
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM activity.match_budgets WHERE match_id = $1`, f.matchID).Scan(&rows); err != nil {
		t.Fatalf("數預算列: %v", err)
	}
	if rows != 1 {
		t.Errorf("預算列數 = %d,想要 1", rows)
	}
}

// ── 交易變體 ────────────────────────────────────────────────────

// TestBindTxParticipatesInOuterRollback 驗證綁定變體真的活在呼叫端的 tx 裡。
//
// 這是 match 套件要的那個保證:它在自己的 tx 裡推進場次狀態並呼叫 GrantBudget /
// Select,若之後有任何一步失敗,讓武的寫入必須跟著消失 —— 否則會留下
// 「預算發了但場次沒推進」這種只能人工修的狀態。
func TestBindTxParticipatesInOuterRollback(t *testing.T) {
	setup(t)
	ctx := context.Background()
	poolRepo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	items := installItems(t, poolRepo, f.tournamentID, testSpecs())

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開外層 tx: %v", err)
	}
	bound := handicappg.BindTx(tx)
	svc := handicap.New(bound)

	// 同一個綁定 Repository 上連續兩次 WithTx —— 對綁定變體來說這是正常路徑。
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget(綁定): %v", err)
	}
	res, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_十點"].Ref,
	})
	if err != nil {
		t.Fatalf("Select(綁定): %v", err)
	}

	// 外層 tx 內看得到;tx 外(另一條連線)還看不到,因為根本還沒 commit。
	if _, err := bound.GetSelection(ctx, res.Selection.PublicID); err != nil {
		t.Fatalf("tx 內讀不到自己剛寫的選擇: %v", err)
	}
	if n := countSelections(t, f.matchID, f.holderID, true); n != 0 {
		t.Fatalf("未 commit 前 tx 外就看得到 %d 列", n)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, err := poolRepo.GetBudget(ctx, f.matchID, f.holderID); !errors.Is(err, handicap.ErrNoBudget) {
		t.Errorf("rollback 後仍有預算,err = %v", err)
	}
	if _, err := poolRepo.GetSelection(ctx, res.Selection.PublicID); !errors.Is(err, handicap.ErrSelectionNotFound) {
		t.Errorf("rollback 後仍有選擇,err = %v", err)
	}
}

// TestBindTxCommitPersists 是上一個測試的另一半:外層 commit 時讓武的寫入留下來。
func TestBindTxCommitPersists(t *testing.T) {
	setup(t)
	ctx := context.Background()
	poolRepo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	items := installItems(t, poolRepo, f.tournamentID, testSpecs())

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開外層 tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	svc := handicap.New(handicappg.BindTx(tx))
	if _, err := svc.GrantBudget(ctx, f.matchRef); err != nil {
		t.Fatalf("GrantBudget(綁定): %v", err)
	}
	if _, err := svc.Select(ctx, handicap.SelectParams{
		MatchPublicID: f.matchRef, PlayerID: f.holderID, ItemRef: items["測試_四點"].Ref,
	}); err != nil {
		t.Fatalf("Select(綁定): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	b, err := poolRepo.GetBudget(ctx, f.matchID, f.holderID)
	if err != nil {
		t.Fatalf("commit 後讀預算: %v", err)
	}
	if b.Spent != 4 {
		t.Errorf("spent = %d,想要 4", b.Spent)
	}
	assertSpentConsistent(t, f.matchID, f.holderID)
}

// TestNestedWithTxRejected 驗證巢狀交易出聲而不是默默攤平。
//
// 兩個變體的差別正在這裡:池變體的 fn 裡再 WithTx 是 bug;綁定變體的第一層
// WithTx 是設計(match 在自己的 tx 裡呼叫讓武),第二層才是 bug。
func TestNestedWithTxRejected(t *testing.T) {
	setup(t)
	ctx := context.Background()

	err := handicappg.New(pool).WithTx(ctx, func(ctx context.Context, r handicap.Repository) error {
		return r.WithTx(ctx, func(context.Context, handicap.Repository) error { return nil })
	})
	if !errors.Is(err, handicappg.ErrNestedTx) {
		t.Errorf("池變體巢狀 err = %v,想要 ErrNestedTx", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("開 tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	bound := handicappg.BindTx(tx)
	// 第一層:就地執行,不該報錯。
	if err := bound.WithTx(ctx, func(context.Context, handicap.Repository) error { return nil }); err != nil {
		t.Fatalf("綁定變體第一層 WithTx 不該失敗:%v", err)
	}
	// 第二層:仍然是 bug。
	err = bound.WithTx(ctx, func(ctx context.Context, r handicap.Repository) error {
		return r.WithTx(ctx, func(context.Context, handicap.Repository) error { return nil })
	})
	if !errors.Is(err, handicappg.ErrNestedTx) {
		t.Errorf("綁定變體第二層 err = %v,想要 ErrNestedTx", err)
	}
}

// TestWithTxRollsBackOnError 驗證池變體的 fn 回錯時不留半套寫入。
func TestWithTxRollsBackOnError(t *testing.T) {
	setup(t)
	ctx := context.Background()
	repo := handicappg.New(pool)
	f := newFixture(t, defaultOpt())
	items := installItems(t, repo, f.tournamentID, testSpecs())

	sentinel := errors.New("刻意失敗")
	err := repo.WithTx(ctx, func(ctx context.Context, r handicap.Repository) error {
		if _, err := r.InsertBudget(ctx, f.matchID, f.holderID, 24); err != nil {
			return err
		}
		if _, err := r.InsertSelection(ctx, handicap.NewSelection{
			MatchID: f.matchID, PlayerID: f.holderID, ItemID: items["測試_四點"].ID, Cost: 4,
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v,想要原樣透傳的 sentinel", err)
	}
	if _, err := repo.GetBudget(ctx, f.matchID, f.holderID); !errors.Is(err, handicap.ErrNoBudget) {
		t.Errorf("回滾後仍有預算,err = %v", err)
	}
	if n := countSelections(t, f.matchID, f.holderID, true); n != 0 {
		t.Errorf("回滾後仍有 %d 列選擇", n)
	}
}
