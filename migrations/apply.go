package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
)

// Apply 是資料庫結構的唯一初始化入口:前置(schema、extension)+ 全部 migration。
// 冪等,重複執行安全。cmd/migrate 與測試(testdb)都走這裡 —— 不要在別處複製這段邏輯。
func Apply(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS platform`); err != nil {
		return fmt.Errorf("建 platform schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("建 pgvector extension: %w", err)
	}

	goose.SetBaseFS(Platform)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	goose.SetTableName("platform.goose_db_version")
	return goose.UpContext(ctx, db, "platform")
}

// Status 印出目前套用狀態(cmd/migrate status 用)。
// 乾淨 DB 上也可執行:「部署前先看狀態」是最自然的使用情境,
// 所以這裡同樣做 schema 前置(冪等),讓 goose 能建立版本表後回報「全部 Pending」。
func Status(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS platform`); err != nil {
		return fmt.Errorf("建 platform schema: %w", err)
	}
	goose.SetBaseFS(Platform)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	goose.SetTableName("platform.goose_db_version")
	return goose.StatusContext(ctx, db, "platform")
}
