package maintenance_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(mock 掉資料庫的 advisory lock / 分區 DDL 測試沒有意義)。
// 共用一個容器;測試間以獨立的 job 名 / topic / key 隔離,不用 t.Parallel(DDL 與鎖會互擾)。

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

func newSpace(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	var communityID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO platform.communities (public_id, name)
		 VALUES (gen_random_uuid()::text, 'maintenance-test') RETURNING id`,
	).Scan(&communityID)
	if err != nil {
		t.Fatalf("建測試社群: %v", err)
	}
	var spaceID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO platform.community_spaces (public_id, community_id, provider, external_id)
		 VALUES (gen_random_uuid()::text, $1, 'discord', gen_random_uuid()::text) RETURNING id`,
		communityID,
	).Scan(&spaceID)
	if err != nil {
		t.Fatalf("建測試空間: %v", err)
	}
	return spaceID
}

// regclassExists 回報表(或分區)目前是否存在。
func regclassExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("查 to_regclass(%s): %v", name, err)
	}
	return exists
}

// createMonthPartition 用 migration 00001 的函式建指定月分區(測試造舊分區用)。
func createMonthPartition(t *testing.T, table string, month time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`SELECT platform.create_month_partition($1::regclass, $2::date)`,
		table, month)
	if err != nil {
		t.Fatalf("建 %s 的 %s 分區: %v", table, month.Format("2006-01"), err)
	}
}

// countEventLogs 數某 action 的 job.run 紀錄(可再依 status 過濾,status 空字串 = 不過濾)。
func countEventLogs(t *testing.T, action, status string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform.event_logs
		 WHERE channel = 'system' AND kind = 'job.run' AND action = $1
		   AND ($2 = '' OR status = $2)`, action, status).Scan(&n)
	if err != nil {
		t.Fatalf("數 event_logs: %v", err)
	}
	return n
}

// waitFor 輪詢直到條件成立或逾時。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到:%s", what)
}
