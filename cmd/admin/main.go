// admin:管理端的一次性操作。目前只有社群/空間/頻道的註冊。
//
// 為什麼是 CLI 而不是管理 RPC:這些操作在有前端之前沒人叫得動,
// 而「新增一個 Discord 伺服器」是 bot 能開始記錄任何東西的前提——
// 沒有它,整個活躍度與 XP 是死的。先用最短的路徑把缺口補上,
// 查詢層(queries/community.sql)之後補管理 RPC 時直接複用。
//
// 用法:
//
//	go run ./cmd/admin list
//	go run ./cmd/admin register-space   --guild <snowflake> --name "測試伺服器"
//	go run ./cmd/admin register-channel --guild <snowflake> --channel <snowflake> --kind text --log-messages
//
// 連線字串從 PLATFORM_DATABASE_URL 讀(與 migrate 同一個變數,
// 不另外發明一個——一個概念一個位置)。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

const usage = `用法:
  admin list
        列出已註冊的社群、空間與頻道

  admin register-space --guild <id> [--name <名稱>] [--community <public_id>]
        把一個 Discord 伺服器註冊成 community_space。
        沒給 --community 就順便建一個同名的社群。
        已經註冊過的話不動它,只印出現況。

  admin list-purposes
        列出可設定的頻道用途

  admin set-channel   --guild <id> --purpose <用途> --channel <id> [--name <名稱>] [--kind <型別>]
  admin clear-channel --guild <id> --purpose <用途>
        設定/取消「這個伺服器的公告貼哪裡」。取代了 stentor 舊的 CHANNEL_MAP
        環境變數——那是一份全域對應,bot 進第二個伺服器時就沒有正確答案可給。
        頻道若還沒在 space_channels 裡,會一併補建。

  admin register-channel --guild <id> --channel <id> --kind <text|voice|forum|announcement>
                        [--name <名稱>] [--log-messages] [--no-xp]
        註冊單一頻道。**只有想保存訊息內容時才需要**——
        沒註冊的頻道照樣計 XP,只是內容不落地(白名單制)。
        重跑會以這次的旗標覆蓋。

環境變數:
  PLATFORM_DATABASE_URL   必填,與 cmd/migrate 相同`

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		return errors.New("缺少子命令")
	}

	dsn := os.Getenv("PLATFORM_DATABASE_URL")
	if dsn == "" {
		return errors.New("環境變數 PLATFORM_DATABASE_URL 未設定")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("連線失敗: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("資料庫沒有回應: %w", err)
	}

	switch os.Args[1] {
	case "list":
		return listAll(ctx, pool)
	case "register-space":
		return registerSpace(ctx, pool, os.Args[2:])
	case "register-channel":
		return registerChannel(ctx, pool, os.Args[2:])
	case "list-purposes":
		return listPurposes(ctx, pool)
	case "set-channel":
		return setChannel(ctx, pool, os.Args[2:])
	case "clear-channel":
		return clearChannel(ctx, pool, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		fmt.Fprintln(os.Stderr, usage)
		return fmt.Errorf("不認得的子命令 %q", os.Args[1])
	}
}
