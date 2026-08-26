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
	return pool, func() { pool.Close(); terminate() }, nil
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
