package chunker_test

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

// 整合測試對真 Postgres 跑:要驗的正是視窗函式的切段、NOT DISTINCT FROM 的
// NULL thread 分組、advisory lock 的併發行為——mock 掉資料庫就什麼都沒測到。
// 共用一個容器;每個測試自己建 space,以 space_id 互相隔離,因此不用 t.Parallel 也不互擾。

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

// newSpace 建一個獨立的社群空間(測試之間的隔離單位)。
func newSpace(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	var communityID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.communities (public_id, name)
		 VALUES (gen_random_uuid()::text, 'chunker-test') RETURNING id`,
	).Scan(&communityID); err != nil {
		t.Fatalf("建測試社群: %v", err)
	}
	var spaceID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.community_spaces (public_id, community_id, provider, external_id)
		 VALUES (gen_random_uuid()::text, $1, 'discord', gen_random_uuid()::text) RETURNING id`,
		communityID,
	).Scan(&spaceID); err != nil {
		t.Fatalf("建測試空間: %v", err)
	}
	return spaceID
}

// newChannel 註冊頻道。logMessages=false 代表不在白名單內(schemas/01 增補 C)。
func newChannel(t *testing.T, spaceID int64, externalID string, logMessages bool) string {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.space_channels (space_id, external_id, kind, log_messages)
		 VALUES ($1, $2, 'text', $3)`, spaceID, externalID, logMessages)
	if err != nil {
		t.Fatalf("註冊頻道 %s: %v", externalID, err)
	}
	return externalID
}

// newUser 建一般使用者:有 identities 列 = 有綁第三方身分 = 不是 bot。
func newUser(t *testing.T, displayName string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id, display_name)
		 VALUES (gen_random_uuid()::text, $1) RETURNING id`, displayName,
	).Scan(&id); err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.identities (user_id, provider, provider_user_id, username)
		 VALUES ($1, 'discord', gen_random_uuid()::text, $2)`, id, displayName); err != nil {
		t.Fatalf("綁定身分: %v", err)
	}
	return id
}

// newBotUser 建 bot / 系統帳號:沒有 identities 列(不會走 OAuth 綁定)。
func newBotUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id, display_name)
		 VALUES (gen_random_uuid()::text, 'some-bot') RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("建 bot 帳號: %v", err)
	}
	return id
}

func setOptOutAICorpus(t *testing.T, userID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_privacy_settings (user_id, opt_out_ai_corpus)
		 VALUES ($1, true)`, userID)
	if err != nil {
		t.Fatalf("設定 opt_out_ai_corpus: %v", err)
	}
}

// msg 插一則訊息。minutesAgo 是相對現在的分鐘數(正 = 過去)。
func msg(t *testing.T, spaceID int64, channelID string, thread *string, userID int64, minutesAgo float64, excerpt string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.message_logs
		   (user_id, space_id, channel_id, channel_kind, thread_id, message_id, excerpt, created_at)
		 VALUES ($1, $2, $3, 'text', $4, gen_random_uuid()::text, $5,
		         now() - make_interval(secs => $6))`,
		userID, spaceID, channelID, thread, excerpt, minutesAgo*60)
	if err != nil {
		t.Fatalf("插訊息: %v", err)
	}
}

type chunkRow struct {
	SpaceID      int64
	ChannelID    string
	ThreadID     *string
	StartedAt    time.Time
	EndedAt      time.Time
	Participants []int64
	MessageCount int32
	Text         string
	EmbedModel   *string
	EmbeddedAt   *time.Time
}

func listChunks(t *testing.T, spaceID int64) []chunkRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT space_id, channel_id, thread_id, started_at, ended_at,
		        participant_user_ids, message_count, text, embed_model, embedded_at
		 FROM platform.conversation_chunks WHERE space_id = $1 ORDER BY started_at`, spaceID)
	if err != nil {
		t.Fatalf("查 chunks: %v", err)
	}
	defer rows.Close()
	var out []chunkRow
	for rows.Next() {
		var c chunkRow
		if err := rows.Scan(&c.SpaceID, &c.ChannelID, &c.ThreadID, &c.StartedAt, &c.EndedAt,
			&c.Participants, &c.MessageCount, &c.Text, &c.EmbedModel, &c.EmbeddedAt); err != nil {
			t.Fatalf("掃描 chunk: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("查 chunks: %v", err)
	}
	return out
}
