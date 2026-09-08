package notificationpg_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/notificationpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 對真 Postgres 跑(hestia/CLAUDE.md):這裡要測的正是 FOR UPDATE SKIP LOCKED
// 的認領行為與可見性逾時 —— mock 掉資料庫等於什麼都沒測。
// 整個 package 共用一個容器;每個測試自己清 outbox_events(佇列是全域的,
// 不像使用者那樣能靠獨立列隔離)。

var (
	pool *pgxpool.Pool
	svc  *notificationpg.Service
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = notificationpg.New(pool)
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// setup 清空佇列後開始。測試之間不能互相看到對方的事件,而 outbox_events
// 沒有「屬於誰」的欄位可以隔離,所以只能清乾淨 —— 也因此本 package 的測試
// 不能平行跑(go test 預設同一個 package 內就是循序的)。
func setup(t *testing.T) context.Context {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM platform.outbox_events`); err != nil {
		t.Fatalf("清空 outbox_events: %v", err)
	}
	return ctx
}

// insertEvent 寫一筆 outbox 事件,回傳它的 id。
func insertEvent(t *testing.T, ctx context.Context, topic string, payload map[string]any) int64 {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 payload: %v", err)
	}
	var id int64
	err = pool.QueryRow(ctx,
		`INSERT INTO platform.outbox_events (topic, payload) VALUES ($1, $2) RETURNING id`,
		topic, raw,
	).Scan(&id)
	if err != nil {
		t.Fatalf("寫測試事件 %s: %v", topic, err)
	}
	return id
}

func eventState(t *testing.T, ctx context.Context, id int64) (status string, attempts int32) {
	t.Helper()
	err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM platform.outbox_events WHERE id = $1`, id,
	).Scan(&status, &attempts)
	if err != nil {
		t.Fatalf("讀事件 %d: %v", id, err)
	}
	return status, attempts
}

// expireVisibility 把可見性逾時往前撥,模擬「認領後沒有 Ack 就過期了」。
// 用改資料庫時鐘上的值而不是 sleep:睡覺讓測試變慢又不穩,
// 而這裡要驗的是「逾時之後會不會重新可見」,不是計時器準不準。
func expireVisibility(t *testing.T, ctx context.Context, ids ...int64) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`UPDATE platform.outbox_events SET next_retry_at = now() - interval '1 second'
		 WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		t.Fatalf("撥回可見性逾時: %v", err)
	}
}

func newUser(t *testing.T, ctx context.Context, displayName string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id, display_name)
		 VALUES (gen_random_uuid()::text, $1) RETURNING id`, displayName,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

func newItem(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var publicID string
	err := pool.QueryRow(ctx,
		`INSERT INTO platform.shop_items
		   (public_id, name, fulfillment, currency, price, listed_at)
		 VALUES (gen_random_uuid()::text, $1, 'auto_perk', 'coin', 10, now())
		 RETURNING public_id`, name,
	).Scan(&publicID)
	if err != nil {
		t.Fatalf("建測試商品: %v", err)
	}
	return publicID
}
