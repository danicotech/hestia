package storage_test

import (
	"context"
	"testing"

	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// TestMigrationsAndConstraints 是骨架煙霧測試:
// 容器起得來、全部 migration 套得上、且資料庫層的鐵則約束真的在擋。
func TestMigrationsAndConstraints(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
	pool := testdb.Start(t)
	ctx := context.Background()

	// seed 到位
	var currencies, roles, perms, configs int
	row := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.currencies),
		(SELECT count(*) FROM platform.roles),
		(SELECT count(*) FROM platform.role_permissions),
		(SELECT count(*) FROM platform.economy_configs)`)
	if err := row.Scan(&currencies, &roles, &perms, &configs); err != nil {
		t.Fatalf("查 seed 失敗: %v", err)
	}
	// roles 5 / role_permissions 24:00030 加了 judge 角色與 tournament.judge,
	// 後者給 owner / admin / judge 各一組。
	if currencies != 1 || roles != 5 || perms != 24 || configs != 22 {
		t.Fatalf("seed 數量不符: currencies=%d roles=%d perms=%d configs=%d", currencies, roles, perms, configs)
	}

	// 建測試使用者
	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id) VALUES ('01TEST0000000000000000TEST') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("建使用者失敗: %v", err)
	}

	// 鐵則:負餘額被 CHECK 擋
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.user_balances (user_id, currency, balance) VALUES ($1,'coin',-1)`, userID,
	); err == nil {
		t.Fatal("負餘額應被 CHECK 約束拒絕,卻成功了")
	}

	// 鐵則:簽到連點撞 PK
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.daily_claims (user_id, claim_date, timezone, streak, amount)
		 VALUES ($1,'2026-08-26','Asia/Taipei',1,100)`, userID); err != nil {
		t.Fatalf("第一次簽到應成功: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.daily_claims (user_id, claim_date, timezone, streak, amount)
		 VALUES ($1,'2026-08-26','Asia/Taipei',1,100)`, userID); err == nil {
		t.Fatal("同日重複簽到應撞唯一鍵,卻成功了")
	}

	// 分區:分錄落進月分區而非 default
	var part string
	if err := pool.QueryRow(ctx, `
		INSERT INTO platform.token_entries (user_id, currency, amount, reason)
		VALUES ($1,'coin',100,'signup_bonus')
		RETURNING tableoid::regclass::text`, userID,
	).Scan(&part); err != nil {
		t.Fatalf("寫分錄失敗: %v", err)
	}
	if part == "platform.token_entries_default" {
		t.Fatalf("分錄落進 default 分區,月分區未生效: %s", part)
	}
}
