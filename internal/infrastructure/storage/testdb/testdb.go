// Package testdb 提供整合測試用的真 Postgres:testcontainers 起容器 + goose 套 migration。
// 對真資料庫測試是刻意的(hestia/CLAUDE.md):mock 掉資料庫的併發測試沒有意義,
// 要測的正是資料庫的鎖行為與約束。
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // goose 走 database/sql,用 pgx stdlib driver
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danicotech/hestia/migrations"
)

// Start 起一個乾淨的 Postgres、套完整 migration,生命週期綁在呼叫測試的 t.Cleanup。
// 適合單一測試獨占容器;多個測試共用請改用 StartShared(TestMain 管生命週期)。
func Start(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, cleanup, err := StartShared(context.Background())
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(cleanup)
	return pool
}

// StartShared 起容器 + 套 migration,不依賴 *testing.T。
// 給 TestMain 用:整個 package 共用一個容器,結束時呼叫 cleanup。
func StartShared(ctx context.Context) (pool *pgxpool.Pool, cleanup func(), err error) {
	ctr, err := tcpostgres.Run(ctx,
		"pgvector/pgvector:pg17", // 與 compose 同映像,避免測試/正式版本差
		tcpostgres.WithDatabase("hestia_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("起 Postgres 容器: %w", err)
	}
	terminate := func() { _ = ctr.Terminate(context.Background()) }

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		terminate()
		return nil, nil, fmt.Errorf("取連線字串: %w", err)
	}
	if err := applyMigrations(dsn); err != nil {
		terminate()
		return nil, nil, err
	}
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		terminate()
		return nil, nil, fmt.Errorf("建連線池: %w", err)
	}
	if err := stabilizeSeedClock(ctx, pool); err != nil {
		pool.Close()
		terminate()
		return nil, nil, err
	}
	if err := waitForSeedVisible(ctx, pool); err != nil {
		pool.Close()
		terminate()
		return nil, nil, err
	}
	return pool, func() { pool.Close(); terminate() }, nil
}

// stabilizeSeedClock 把 seed 設定的生效時間往前挪一小時。
//
// 為什麼:設定讀取一律是 effective_at <= now()。Docker Desktop(Windows)的 VM 時鐘
// 在容器起來後可能被往回校正幾秒——只在啟動時等待擋不住「跑到一半才回跳」。
// 那時 seed 會突然「還沒生效」,而讀不到設定的正確行為是**靜默降級**
// (例如語音就這次不發 XP,事實照記),於是測試會以難以理解的方式偶發失敗:
// 表面症狀是「XP 失敗時應該回錯」,真因卻是「XP 根本沒被嘗試」。
//
// 往前挪一小時讓任何合理的時鐘回跳都吃不到這個邊界。只動測試容器裡的資料,
// 而 effective_at 的語意本來就是「何時生效」——在全新的測試庫裡挪早它無害。
// 需要驗 effective_at 行為的測試自己插新列,不受影響。
func stabilizeSeedClock(ctx context.Context, pool *pgxpool.Pool) error {
	const q = `UPDATE platform.economy_configs
	              SET effective_at = effective_at - interval '1 hour'
	            WHERE created_by IS NULL`
	if _, err := pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("調整 seed 設定的生效時間: %w", err)
	}
	return nil
}

// waitForSeedVisible 等到 seed 的 economy_configs 真的「生效」才把 pool 交出去。
//
// 為什麼需要:設定讀取一律用 effective_at <= now() 取最新一筆,而 seed 的
// effective_at 是 migration 當下的 DB 時鐘。Docker Desktop(Windows)的 VM 時鐘
// 在容器起來後可能被往回校正幾秒——那幾秒內整份 seed「還沒生效」,任何讀設定的
// 程式碼都會拿不到值而正確地報錯,測試就變成偶發失敗。
//
// 這是測試環境的時鐘問題,不是服務的 bug:服務讀不到必要設定就明確失敗
// (而不是猜一個預設值把手續費之類的東西靜靜關掉)是刻意的設計。
//
// 放在這裡而不是各 package 各寫一份:三個 package 已經各自踩過同一個坑,
// 而「容器剛起來時 seed 可能還看不見」是 testdb 這個容器擁有者的責任。
func waitForSeedVisible(ctx context.Context, pool *pgxpool.Pool) error {
	const q = `SELECT count(*) FROM platform.economy_configs
	            WHERE created_by IS NULL AND effective_at <= now()`
	deadline := time.Now().Add(30 * time.Second)
	var n, want int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.economy_configs WHERE created_by IS NULL`).Scan(&want); err != nil {
		return fmt.Errorf("讀 seed 設定數: %w", err)
	}
	for {
		if err := pool.QueryRow(ctx, q).Scan(&n); err == nil && n == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("seed 的 economy_configs 一直沒生效(容器時鐘?):可見 %d/%d", n, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func applyMigrations(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("開 database/sql 連線: %w", err)
	}
	defer func() { _ = db.Close() }()

	// 與正式部署走同一個入口(migrations.Apply),測試環境不另外複製初始化邏輯
	goose.SetLogger(goose.NopLogger())
	if err := migrations.Apply(context.Background(), db); err != nil {
		return fmt.Errorf("套 migration: %w", err)
	}
	return nil
}
