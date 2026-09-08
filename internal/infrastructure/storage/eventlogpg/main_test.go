package eventlogpg_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(hestia/CLAUDE.md):event_logs 是分區表,
// 白名單欄位是 JSONB,NULL 語意也要驗——這些 mock 掉就什麼都沒測到。

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
	// 每個測試獨占整張表的內容,避免互相看到對方的列。
	if _, err := pool.Exec(context.Background(), `DELETE FROM platform.event_logs`); err != nil {
		t.Fatalf("清空 event_logs: %v", err)
	}
}
