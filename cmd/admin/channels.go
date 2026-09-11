package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 頻道用途的設定(schemas/22)。
//
// 這取代了 stentor 的 CHANNEL_MAP 環境變數:那是一份全域對應,
// bot 進到第二個伺服器時就沒有正確答案可給。

func setChannel(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("set-channel", flag.ContinueOnError)
	guild := fs.String("guild", "", "Discord 伺服器 id")
	purpose := fs.String("purpose", "", "用途;admin list-purposes 看有哪些")
	channel := fs.String("channel", "", "Discord 頻道 id")
	name := fs.String("name", "", "頻道顯示名稱(選填,只是快照)")
	kind := fs.String("kind", "text", "頻道型別:text / voice / forum / announcement")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSnowflake("guild", *guild); err != nil {
		return err
	}
	if err := requireSnowflake("channel", *channel); err != nil {
		return err
	}
	if *purpose == "" {
		return errors.New("--purpose 必填(用 admin list-purposes 看有哪些)")
	}
	if !channelKinds[*kind] {
		return fmt.Errorf("--kind %q 不合法,只能是 text / voice / forum / announcement", *kind)
	}

	q := db.New(pool)
	space, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: *guild,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("guild %s 還沒註冊,先跑 admin register-space --guild %s", *guild, *guild)
	}
	if err != nil {
		return fmt.Errorf("查詢 space: %w", err)
	}

	// 補建 + 設用途要同一個 transaction:只補建了頻道卻沒設成用途,
	// 會留下一列看起來像「有人註冊過」的頻道,而使用者以為指令失敗了。
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("開啟交易: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := db.New(tx)

	// space_channel_purposes 對 space_channels 有外鍵(打錯 channel id 當場擋下)。
	// 缺列就補建,免得為了設一個公告頻道還要先跑 register-channel。
	if err := qtx.EnsureSpaceChannel(ctx, db.EnsureSpaceChannelParams{
		SpaceID: space.ID, ExternalID: *channel, Name: optional(*name), Kind: *kind,
	}); err != nil {
		return fmt.Errorf("註冊頻道: %w", err)
	}

	row, err := qtx.SetChannelPurpose(ctx, db.SetChannelPurposeParams{
		SpaceID: space.ID, Purpose: *purpose, ChannelExternalID: *channel,
	})
	if err != nil {
		// 外鍵違反 = purpose 不在註冊表裡。這是最容易打錯的參數,
		// 所以直接把可用清單印出來,不要讓人自己去猜。
		if isForeignKeyViolation(err) {
			return fmt.Errorf("用途 %q 不存在。可用的:%s", *purpose, purposeList(ctx, q))
		}
		return fmt.Errorf("設定用途: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交: %w", err)
	}

	fmt.Printf("已設定:\n  用途   %s\n  頻道   %s\n  guild  %s\n",
		row.Purpose, row.ChannelExternalID, *guild)
	return nil
}

func clearChannel(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("clear-channel", flag.ContinueOnError)
	guild := fs.String("guild", "", "Discord 伺服器 id")
	purpose := fs.String("purpose", "", "要取消的用途")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSnowflake("guild", *guild); err != nil {
		return err
	}
	if *purpose == "" {
		return errors.New("--purpose 必填")
	}

	q := db.New(pool)
	space, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: *guild,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("guild %s 還沒註冊", *guild)
	}
	if err != nil {
		return fmt.Errorf("查詢 space: %w", err)
	}

	n, err := q.ClearChannelPurpose(ctx, db.ClearChannelPurposeParams{
		SpaceID: space.ID, Purpose: *purpose,
	})
	if err != nil {
		return fmt.Errorf("取消用途: %w", err)
	}
	if n == 0 {
		fmt.Printf("用途 %s 本來就沒有設定,沒有變更。\n", *purpose)
		return nil
	}
	fmt.Printf("已取消用途 %s。該用途的公告從現在起不會投遞到這個伺服器。\n", *purpose)
	return nil
}

func listPurposes(ctx context.Context, pool *pgxpool.Pool) error {
	q := db.New(pool)
	rows, err := q.ListChannelPurposes(ctx)
	if err != nil {
		return fmt.Errorf("列出用途: %w", err)
	}
	fmt.Println("可用的頻道用途:")
	for _, r := range rows {
		fmt.Printf("  %-20s %-14s %s\n", r.Key, r.Name, derefOr(r.Description, ""))
	}
	fmt.Println("\n設定方式:admin set-channel --guild <伺服器id> --purpose <用途> --channel <頻道id>")
	return nil
}

// purposeList 供錯誤訊息使用。查不到就回空字串——顯示問題不該蓋掉原本的錯誤。
func purposeList(ctx context.Context, q *db.Queries) string {
	rows, err := q.ListChannelPurposes(ctx)
	if err != nil {
		return ""
	}
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += ", "
		}
		out += r.Key
	}
	return out
}
