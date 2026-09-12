package watchpg_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/watch"
	"github.com/danicotech/hestia/internal/infrastructure/storage/matchpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑,而且**非這樣不可**:本套件唯一的存在理由是
// 「NOTIFY 只在 commit 時送出」,那是資料庫的性質。把資料庫 mock 掉之後,
// 剩下的測試會全部通過,而那個保證一個字都沒被驗到。
//
// 全 package 共用一個容器,各測試用自己的賽事隔離。

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
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

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// ── 測試資料 ────────────────────────────────────────────────────

type fixture struct {
	t    *testing.T
	id   int64
	slug string
	repo *matchpg.Service
}

type player struct {
	id       int64
	publicID string
	name     string
}

type mrec struct {
	id       int64
	publicID string
}

// newFixture 建一屆獨立的賽事。slug 帶測試名,失敗時看得出是哪一支。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	setup(t)
	slug := fmt.Sprintf("watch-%s-%d", t.Name(), time.Now().UnixNano())
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase, config)
		VALUES (gen_random_uuid()::text, $1, $2, 1, 'in_progress', '{}'::jsonb)
		RETURNING id`, slug, "測試賽 "+slug).Scan(&id); err != nil {
		t.Fatalf("建賽事: %v", err)
	}
	return &fixture{t: t, id: id, slug: slug, repo: matchpg.New(pool)}
}

func (f *fixture) player(name string, rank int) player {
	f.t.Helper()
	ctx := context.Background()
	var fencerID int64
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.fencers (public_id, game_id, discord_name)
		VALUES (gen_random_uuid()::text, $1, $2) RETURNING id`,
		name+"-"+suffix, name).Scan(&fencerID); err != nil {
		f.t.Fatalf("建 fencer %s: %v", name, err)
	}
	var rankArg any
	if rank > 0 {
		rankArg = rank
	}
	p := player{name: name}
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.tournament_players
		  (public_id, tournament_id, fencer_id, display_name, rank_level, passcode_hash, discord_name)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, 'x', $3)
		RETURNING id, public_id`,
		f.id, fencerID, name, rankArg).Scan(&p.id, &p.publicID); err != nil {
		f.t.Fatalf("建參賽者 %s: %v", name, err)
	}
	return p
}

func (f *fixture) match(round, slot int, p1, p2 player, status match.Status) mrec {
	f.t.Helper()
	var m mrec
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO activity.matches
		  (public_id, tournament_id, round, slot, p1_player_id, p2_player_id, status)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6)
		RETURNING id, public_id`,
		f.id, round, slot, p1.id, p2.id, string(status),
	).Scan(&m.id, &m.publicID); err != nil {
		f.t.Fatalf("建場次 r%ds%d: %v", round, slot, err)
	}
	return m
}

// finish 判一場的勝負(直接寫,不走裁判流程 —— 本套件測的是推播不是判決)。
func (f *fixture) finish(m mrec, winner player) {
	f.t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE activity.matches
		   SET status = 'done', winner_player_id = $2, finished_at = now()
		 WHERE id = $1`, m.id, winner.id); err != nil {
		f.t.Fatalf("設定賽果: %v", err)
	}
}

// lockHandicaps 建一份讓武預算與兩項選擇,並把場次設成已封盤。
func (f *fixture) lockHandicaps(m mrec, holder player) {
	f.t.Helper()
	ctx := context.Background()
	var itemID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO activity.handicap_items (public_id, tournament_id, category, name, cost)
		VALUES (gen_random_uuid()::text, $1, 'weapon', '禁切武器', 8)
		RETURNING id`, f.id).Scan(&itemID); err != nil {
		f.t.Fatalf("建讓武項目: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO activity.match_budgets (match_id, player_id, budget, spent)
		VALUES ($1, $2, 16, 16)`, m.id, holder.id); err != nil {
		f.t.Fatalf("建讓武預算: %v", err)
	}
	for i, note := range []string{"指定武學:落霞", "禁止跳躍"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO activity.handicap_selections
			  (public_id, match_id, player_id, item_id, cost, target_note)
			VALUES (gen_random_uuid()::text, $1, $2, $3, 8, $4)`,
			m.id, holder.id, itemID, note); err != nil {
			f.t.Fatalf("建讓武選擇 %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE activity.matches
		   SET status = 'locked', handicap_open = false, handicap_locked_at = now()
		 WHERE id = $1`, m.id); err != nil {
		f.t.Fatalf("設定封盤: %v", err)
	}
}

// appendEvent 走**正式路徑**發一則場次事件:matchpg.AppendEvents 在一個
// transaction 裡寫 outbox + NOTIFY。commit 參數決定這個交易的下場 ——
// 「rollback 的交易不會送出 NOTIFY」就是靠它驗的。
func (f *fixture) appendEvent(topic string, m mrec, commit bool) {
	f.t.Helper()
	ctx := context.Background()
	payload := f.eventPayload(topic, m)

	tx, err := pool.Begin(ctx)
	if err != nil {
		f.t.Fatalf("開 transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := f.repo.AppendEvents(ctx, tx, []match.Event{{Topic: topic, Payload: payload}}); err != nil {
		f.t.Fatalf("AppendEvents: %v", err)
	}
	if !commit {
		if err := tx.Rollback(ctx); err != nil {
			f.t.Fatalf("rollback: %v", err)
		}
		return
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
}

// eventPayload 組出與正式流程同形狀的 outbox payload。
func (f *fixture) eventPayload(topic string, m mrec) []byte {
	f.t.Helper()
	var (
		raw []byte
		err error
	)
	if topic == match.TopicChampion {
		raw, err = json.Marshal(match.ChampionEvent{
			TournamentSlug: f.slug, TournamentName: "測試賽",
			FinalMatchID: m.publicID, TotalRounds: 1,
		})
	} else {
		raw, err = json.Marshal(match.MatchEvent{
			TournamentSlug: f.slug, TournamentName: "測試賽",
			MatchPublicID: m.publicID, Round: 1, Slot: 0,
		})
	}
	if err != nil {
		f.t.Fatalf("組 payload: %v", err)
	}
	return raw
}

// notifyEnvelope 直接送一個信封(不經 outbox)。
//
// 給「讓武封盤 / 階段 / 冠軍」這類目前沒有發送點、或發送點不在本套件檔案範圍內
// 的種類用:要測的是**收到信封之後讀得出正確內容**,信封怎麼來的是另一回事。
func (f *fixture) notifyEnvelope(e watch.Envelope) {
	f.t.Helper()
	payload, err := e.Marshal()
	if err != nil {
		f.t.Fatalf("組信封: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`SELECT pg_notify($1, $2)`, watch.Channel, payload); err != nil {
		f.t.Fatalf("送通知: %v", err)
	}
}

// ── 原始監聽連線 ────────────────────────────────────────────────

// rawListener 是一條自己 LISTEN 的連線,用來直接檢查「線上真的飛了什麼」。
//
// 為什麼不一律走 Listener + Hub:commit / rollback 那條性質要驗的是
// **有沒有送出通知**,經過 Hub 會多一層「有沒有訂閱者、讀不讀得出內容」的
// 干擾;測不出來的時候會分不清是哪一層的問題。
type rawListener struct {
	t    *testing.T
	conn *pgx.Conn
}

func newRawListener(t *testing.T) *rawListener {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("撥監聽連線: %v", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{watch.Channel}.Sanitize()); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return &rawListener{t: t, conn: conn}
}

// next 等下一則通知;逾時回 ok=false。
func (r *rawListener) next(timeout time.Duration) (watch.Envelope, bool) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	n, err := r.conn.WaitForNotification(ctx)
	if err != nil {
		return watch.Envelope{}, false
	}
	e, err := watch.ParseEnvelope(n.Payload)
	if err != nil {
		r.t.Fatalf("線上飛過來的信封解不開(SQL 與 Go 的鏡像對不上?):%q %v", n.Payload, err)
	}
	return e, true
}

// expectSilence 斷言在這段時間內**沒有**任何通知。
func (r *rawListener) expectSilence(d time.Duration, why string) {
	r.t.Helper()
	if e, ok := r.next(d); ok {
		r.t.Fatalf("%s,但收到了通知 %+v", why, e)
	}
}
