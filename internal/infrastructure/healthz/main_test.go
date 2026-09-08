package healthz_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/healthz"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 監控指標查的是分區目錄、advisory 之外的真實表狀態與 Postgres 的分區路由行為,
// 對真 Postgres 跑是唯一有意義的驗法。
//
// 隔離約定:這個 package 的檢查大多是**全域**的(整張 outbox、整個 schema 的分區),
// 所以每個測試都必須把自己造的異常在 t.Cleanup 清乾淨——最後那個
// 「全部正常 → processed=0」的測試靠這個前提成立。

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

func setup(t *testing.T) *healthz.Checker {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
	// 對帳走真的 ledger 實作:healthz 不自己寫第二套比對 SQL
	return healthz.New(pool, ledgerpg.New(pool))
}

func mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("執行 SQL 失敗(%s): %v", sql, err)
	}
}

func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

// insertJobRun 造一筆 job.run 紀錄(schemas/13),ageMinutes 是相對現在的分鐘數。
func insertJobRun(t *testing.T, action, status string, ageMinutes float64) {
	t.Helper()
	mustExec(t, `INSERT INTO platform.event_logs (channel, kind, action, status, created_at)
	             VALUES ('system', 'job.run', $1, $2, now() - make_interval(secs => $3))`,
		action, status, ageMinutes*60)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM platform.event_logs WHERE channel='system' AND kind='job.run' AND action=$1`, action)
	})
}

func jobHealth(t *testing.T, list []healthz.JobHealth, name string) healthz.JobHealth {
	t.Helper()
	for _, j := range list {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("結果裡找不到 job %s:%+v", name, list)
	return healthz.JobHealth{}
}

func hasAnomaly(list []healthz.Anomaly, kind string) bool {
	for _, a := range list {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// nextMonthPartitionName 回傳下月分區的名字(與 migration 00001 的命名規則一致)。
// 先歸零到本月一號再加一個月:直接對「今天」加一個月在月底會跳過整個月。
func nextMonthPartitionName(table string) string {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0)
	return fmt.Sprintf("%s_%s", table, next.Format("2006_01"))
}
