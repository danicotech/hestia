// migrate:首次部署與後續升級的唯一入口。
// 結構全部內嵌於 binary(schema 前置 + 14 個 migration + seed),不依賴外部 SQL 檔;
// 冪等 —— 已套用過的版本自動跳過,重複執行安全。
//
// 首次部署就兩步:
//
//	docker compose up -d                                  # 1. 起基礎設施(純容器)
//	PLATFORM_DATABASE_URL=postgres://... go run ./cmd/migrate   # 2. 結構一次到位
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/danicotech/hestia/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("PLATFORM_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("環境變數 PLATFORM_DATABASE_URL 未設定")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("連線失敗: %w", err)
	}
	defer func() { _ = db.Close() }()

	if len(os.Args) > 1 && os.Args[1] == "status" {
		return migrations.Status(context.Background(), db)
	}
	return migrations.Apply(context.Background(), db)
}
