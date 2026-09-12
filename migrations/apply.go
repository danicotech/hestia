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
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS activity`); err != nil {
		return fmt.Errorf("建 activity schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("建 pgvector extension: %w", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	// platform 先跑。activity 對 platform 是弱參照(存 user_id 不設 FK),
	// 所以順序其實不影響 DDL 能不能過 —— 但「平台先於活動」是這個專案的
	// 依賴方向,順序寫死在這裡比寫在註解裡可靠。
	goose.SetBaseFS(Platform)
	goose.SetTableName("platform.goose_db_version")
	if err := goose.UpContext(ctx, db, "platform"); err != nil {
		return fmt.Errorf("套用 platform migration: %w", err)
	}

	// 各自一張版本表:抽成 themis 之後兩邊會是不同的 binary 在跑,
	// 共用版本表會讓其中一邊看到另一邊的版本號而誤判。
	goose.SetBaseFS(Activity)
	goose.SetTableName("activity.goose_db_version")
	if err := goose.UpContext(ctx, db, "activity"); err != nil {
		return fmt.Errorf("套用 activity migration: %w", err)
	}
	return nil
}

// Status 印出目前套用狀態(cmd/migrate status 用)。
// 乾淨 DB 上也可執行:「部署前先看狀態」是最自然的使用情境,
// 所以這裡同樣做 schema 前置(冪等),讓 goose 能建立版本表後回報「全部 Pending」。
func Status(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS platform`); err != nil {
		return fmt.Errorf("建 platform schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS activity`); err != nil {
		return fmt.Errorf("建 activity schema: %w", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	goose.SetBaseFS(Platform)
	goose.SetTableName("platform.goose_db_version")
	if err := goose.StatusContext(ctx, db, "platform"); err != nil {
		return fmt.Errorf("platform migration 狀態: %w", err)
	}

	goose.SetBaseFS(Activity)
	goose.SetTableName("activity.goose_db_version")
	if err := goose.StatusContext(ctx, db, "activity"); err != nil {
		return fmt.Errorf("activity migration 狀態: %w", err)
	}
	return nil
}
