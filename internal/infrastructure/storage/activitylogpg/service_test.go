package activitylogpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/activitylogpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/infrastructure/storage/xppg"
)

// 整合測試對真 Postgres 跑:要驗的正是資料庫的約束與鎖行為
// (冪等、併發、事實與彙總的原子性),mock 掉就什麼都沒測到。

var (
	pool *pgxpool.Pool
	svc  *activitylogpg.Service
	// noXP 是沒接 XP 服務的版本,用來確認「不發 XP」的路徑也完全正常。
	noXP *activitylogpg.Service
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
		svc = activitylogpg.New(pool, xppg.New(pool), nil)
		noXP = activitylogpg.New(pool, nil, nil)
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

// ── 測試資料 ──────────────────────────────────────────────────────

// fixture 是一個獨立的社群 + 空間 + 使用者。每個測試各建一組,互不干擾。
type fixture struct {
	communityID int64
	spaceID     int64
	guildID     string
	userID      int64
	discordID   string
}

var seq struct {
	mu sync.Mutex
	n  int
}

func nextID(prefix string) string {
	seq.mu.Lock()
	defer seq.mu.Unlock()
	seq.n++
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.n)
}

const xpRuleset = `{"sources":{"discord_message":{"xp":5},"discord_voice":{"xp_per_minute":2}}}`

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	var f fixture

	var rulesetID int64
	must(t, pool.QueryRow(ctx,
		`INSERT INTO platform.xp_rulesets (name, config) VALUES ('test', $1::jsonb) RETURNING id`,
		xpRuleset).Scan(&rulesetID))
	must(t, pool.QueryRow(ctx,
		`INSERT INTO platform.communities (public_id, name, xp_ruleset_id)
		 VALUES ($1, 'test', $2) RETURNING id`, nextID("c"), rulesetID).Scan(&f.communityID))

	f.guildID = nextID("guild")
	must(t, pool.QueryRow(ctx,
		`INSERT INTO platform.community_spaces (public_id, community_id, provider, external_id)
		 VALUES ($1, $2, 'discord', $3) RETURNING id`,
		nextID("s"), f.communityID, f.guildID).Scan(&f.spaceID))

	f.userID, f.discordID = newLinkedUser(t)
	return f
}

// newLinkedUser 建一個有 Discord 綁定的使用者。
func newLinkedUser(t *testing.T) (int64, string) {
	t.Helper()
	ctx := context.Background()
	var userID int64
	must(t, pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id) VALUES ($1) RETURNING id`, nextID("u")).Scan(&userID))
	discordID := nextID("d")
	_, err := pool.Exec(ctx,
		`INSERT INTO platform.identities (user_id, provider, provider_user_id)
		 VALUES ($1, 'discord', $2)`, userID, discordID)
	must(t, err)
	return userID, discordID
}

// registerChannel 在 space_channels 註冊一個頻道(白名單的權威)。
func registerChannel(t *testing.T, spaceID int64, kind string, logMessages, grantXP bool) string {
	t.Helper()
	ext := nextID("ch")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.space_channels (space_id, external_id, kind, log_messages, grant_xp)
		 VALUES ($1, $2, $3, $4, $5)`, spaceID, ext, kind, logMessages, grantXP)
	must(t, err)
	return ext
}

func setOptOut(t *testing.T, userID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_privacy_settings (user_id, opt_out_logging) VALUES ($1, true)`, userID)
	must(t, err)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("測試資料準備失敗:%v", err)
	}
}

// ── 讀取斷言用的小工具 ──────────────────────────────────────────

type dailyRow struct {
	messages, reactions, voiceSeconds, onlineSeconds int32
	xpGained                                         int64
}

func readDaily(t *testing.T, f fixture, at time.Time) dailyRow {
	t.Helper()
	var d dailyRow
	err := pool.QueryRow(context.Background(),
		`SELECT messages, reactions, voice_seconds, online_seconds, xp_gained
		 FROM platform.activity_daily
		 WHERE user_id = $1 AND community_id = $2
		   AND date = ($3::timestamptz AT TIME ZONE 'UTC')::date`,
		f.userID, f.communityID, at).Scan(&d.messages, &d.reactions, &d.voiceSeconds, &d.onlineSeconds, &d.xpGained)
	if errors.Is(err, pgx.ErrNoRows) {
		// 還沒有任何活動 = 零值,不是失敗。
		return dailyRow{}
	}
	must(t, err)
	return d
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func excerptOf(t *testing.T, messageID string) *string {
	t.Helper()
	var e *string
	must(t, pool.QueryRow(context.Background(),
		`SELECT excerpt FROM platform.message_logs WHERE message_id = $1`, messageID).Scan(&e))
	return e
}

func msg(channelID, messageID, content string, at time.Time) activitylog.MessageParams {
	return activitylog.MessageParams{
		ClientRef: messageID, ChannelID: channelID, ChannelKind: "text",
		MessageID: messageID, Content: content, CreatedAt: at,
	}
}

// ── 身分解析 ──────────────────────────────────────────────────────

// 沒綁定的 Discord id 必須是可辨識的錯誤,而且**不會建出任何使用者**。
func TestResolveActorNeverCreatesUser(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)

	got, err := svc.ResolveActor(ctx, "discord", f.discordID)
	if err != nil || got != f.userID {
		t.Fatalf("ResolveActor = (%d, %v),期望 (%d, nil)", got, err, f.userID)
	}

	before := countRows(t, `SELECT count(*)::int FROM platform.users`)
	_, err = svc.ResolveActor(ctx, "discord", "never-linked-"+nextID("x"))
	if !errors.Is(err, activitylog.ErrActorNotLinked) {
		t.Fatalf("err = %v,期望 ErrActorNotLinked", err)
	}
	if after := countRows(t, `SELECT count(*)::int FROM platform.users`); after != before {
		t.Fatalf("使用者數從 %d 變成 %d ——這條路徑絕對不該建帳號", before, after)
	}
}

func TestSpaceNotRegistered(t *testing.T) {
	setup(t)
	f := newFixture(t)
	_, err := svc.RecordMessages(context.Background(), activitylog.MessagesParams{
		UserID: f.userID, GuildID: "unregistered-guild",
		Messages: []activitylog.MessageParams{msg("c", "m", "hi", time.Now())},
	})
	if !errors.Is(err, activitylog.ErrSpaceNotRegistered) {
		t.Fatalf("err = %v,期望 ErrSpaceNotRegistered", err)
	}
}

// ── 訊息:白名單與 opt-out ──────────────────────────────────────

// 頻道沒開白名單 → 不寫 message_logs,但計數照算(schemas/02:
// 退出記錄不等於退出計分)。
func TestMessageChannelWhitelistOff(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", false, true)
	now := time.Now().UTC()

	out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, nextID("m"), "祕密內容", now)},
	})
	must(t, err)
	if out.Results[0].Err != nil {
		t.Fatalf("寫入失敗:%v", out.Results[0].Err)
	}
	if out.Results[0].ContentStored {
		t.Fatal("白名單關著卻宣稱存了內容")
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_logs WHERE user_id = $1`, f.userID); n != 0 {
		t.Fatalf("message_logs 有 %d 列,期望 0", n)
	}
	if d := readDaily(t, f, now); d.messages != 1 {
		t.Fatalf("activity_daily.messages = %d,期望 1(不記錄 ≠ 不計分)", d.messages)
	}
	// 統計列必須在:它同時是冪等的權威。
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_stats WHERE user_id = $1`, f.userID); n != 1 {
		t.Fatalf("message_stats 有 %d 列,期望 1", n)
	}
}

// opt_out_logging → 有 message_logs 這一列(metadata)但 excerpt 為 NULL,
// 計數照算。
func TestMessageOptOutKeepsCountsDropsContent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	setOptOut(t, f.userID)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	id := nextID("m")

	out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, id, "不該被存下來的原文", now)},
	})
	must(t, err)
	if out.Results[0].Err != nil {
		t.Fatalf("寫入失敗:%v", out.Results[0].Err)
	}
	if out.Results[0].ContentStored {
		t.Fatal("opt-out 使用者卻宣稱存了內容")
	}
	if e := excerptOf(t, id); e != nil {
		t.Fatalf("excerpt = %q,期望 NULL", *e)
	}
	if d := readDaily(t, f, now); d.messages != 1 {
		t.Fatalf("activity_daily.messages = %d,期望 1", d.messages)
	}
}

// excerpt 依 message_excerpt_max_chars 截斷(按字元,不是 byte),
// full_length 記原始長度。
func TestMessageExcerptTruncation(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	id := nextID("m")
	content := strings.Repeat("字", 500) // 500 個中文 = 1500 bytes

	out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, id, content, time.Now().UTC())},
	})
	must(t, err)
	if out.Results[0].Err != nil {
		t.Fatalf("寫入失敗:%v", out.Results[0].Err)
	}
	e := excerptOf(t, id)
	if e == nil {
		t.Fatal("excerpt 竟然是 NULL")
	}
	if n := len([]rune(*e)); n != 200 {
		t.Fatalf("excerpt 長度 = %d 字元,期望 200(seed 的 message_excerpt_max_chars)", n)
	}
	var full int32
	must(t, pool.QueryRow(ctx,
		`SELECT full_length FROM platform.message_logs WHERE message_id = $1`, id).Scan(&full))
	if full != 500 {
		t.Fatalf("full_length = %d,期望 500", full)
	}
}

// ── 訊息:冪等 ────────────────────────────────────────────────────

// 同一則訊息重送 → 只記一次,而且回成功(不是錯誤)。
func TestMessageResendIsDeduplicated(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	m := msg(ch, nextID("m"), "hello", now)
	params := activitylog.MessagesParams{UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{m}}

	first, err := noXP.RecordMessages(ctx, params)
	must(t, err)
	if first.Results[0].Err != nil || first.Results[0].Deduplicated {
		t.Fatalf("第一次應該是新記錄:%+v", first.Results[0])
	}
	second, err := noXP.RecordMessages(ctx, params)
	must(t, err)
	if second.Results[0].Err != nil {
		t.Fatalf("重送應該成功,得到:%v", second.Results[0].Err)
	}
	if !second.Results[0].Deduplicated {
		t.Fatal("重送沒有被判定為重複")
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_logs WHERE message_id = $1`, m.MessageID); n != 1 {
		t.Fatalf("message_logs 有 %d 列,期望 1", n)
	}
	if d := readDaily(t, f, now); d.messages != 1 {
		t.Fatalf("activity_daily.messages = %d,期望 1(重送不該再計一次)", d.messages)
	}
}

// 併發重送(Discord 事件重送 + 多實例):只有一個 tx 能寫成功。
func TestMessageConcurrentResend(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	m := msg(ch, nextID("m"), "concurrent", now)

	const n = 8
	var wg sync.WaitGroup
	fresh := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
				UserID: f.userID, GuildID: f.guildID,
				Messages: []activitylog.MessageParams{m},
			})
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = out.Results[0].Err
			fresh[i] = out.Results[0].Err == nil && !out.Results[0].Deduplicated
		}()
	}
	wg.Wait()

	freshCount := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("第 %d 個併發呼叫失敗:%v", i, errs[i])
		}
		if fresh[i] {
			freshCount++
		}
	}
	if freshCount != 1 {
		t.Fatalf("%d 個併發呼叫有 %d 個認為自己是第一次,期望 1", n, freshCount)
	}
	if c := countRows(t, `SELECT count(*)::int FROM platform.message_logs WHERE message_id = $1`, m.MessageID); c != 1 {
		t.Fatalf("message_logs 有 %d 列,期望 1", c)
	}
	if d := readDaily(t, f, now); d.messages != 1 {
		t.Fatalf("activity_daily.messages = %d,期望 1", d.messages)
	}
}

// ── 訊息:批次 ────────────────────────────────────────────────────

// 批次裡壞掉一筆,其餘照樣成功,而且回報得出是哪一筆。
func TestMessageBatchPartialFailure(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()

	out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{
			msg(ch, nextID("m"), "ok-1", now),
			msg(ch, "", "壞的:沒有 message_id", now), // 第 2 筆
			msg(ch, nextID("m"), "ok-2", now),
		},
	})
	must(t, err)
	if len(out.Results) != 3 {
		t.Fatalf("results = %d,期望 3(逐筆對應)", len(out.Results))
	}
	if out.Results[0].Err != nil || out.Results[2].Err != nil {
		t.Fatalf("好的兩筆不該失敗:%v / %v", out.Results[0].Err, out.Results[2].Err)
	}
	if out.Results[1].Err == nil {
		t.Fatal("壞的那筆竟然成功了")
	}
	if !errors.Is(out.Results[1].Err, activitylog.ErrInvalidRequest) {
		t.Fatalf("壞的那筆 err = %v,期望 ErrInvalidRequest", out.Results[1].Err)
	}
	if d := readDaily(t, f, now); d.messages != 2 {
		t.Fatalf("activity_daily.messages = %d,期望 2(壞的那筆不計)", d.messages)
	}
}

// 事實與彙總同 tx:中途失敗兩者都不留。
//
// 失敗注入用 NUL 字元 —— Postgres 的 text 不接受 0x00,所以寫 message_logs
// 那一步必定炸,而它排在「認領 message_stats」與「更新 activity_daily」**之後**。
// 兩者都必須跟著 rollback。
func TestMessageFactAndAggregateAreAtomic(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	bad := nextID("m")

	out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, bad, "壞內容\x00含 NUL", now)},
	})
	must(t, err)
	if out.Results[0].Err == nil {
		t.Skip("這個 Postgres 接受了 NUL 字元,換一種失敗注入才測得到")
	}

	if n := countRows(t, `SELECT count(*)::int FROM platform.message_stats WHERE message_id = $1`, bad); n != 0 {
		t.Fatalf("失敗的 tx 留下了 %d 列 message_stats", n)
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_logs WHERE message_id = $1`, bad); n != 0 {
		t.Fatalf("失敗的 tx 留下了 %d 列 message_logs", n)
	}
	if d := readDaily(t, f, now); d.messages != 0 {
		t.Fatalf("activity_daily.messages = %d,期望 0(彙總必須跟著 rollback)", d.messages)
	}

	// 而且失敗不會擋住之後的合法寫入(冪等鍵沒有被搶走)。
	ok, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, bad, "乾淨的內容", now)},
	})
	must(t, err)
	if ok.Results[0].Err != nil || ok.Results[0].Deduplicated {
		t.Fatalf("失敗後的重試應該成功寫入:%+v", ok.Results[0])
	}
}

// ── 訊息:XP ──────────────────────────────────────────────────────

// 發言發 XP:xp_events 有事實,activity_daily.xp_gained 有回填。
func TestMessageAwardsXP(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()

	out, err := svc.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, nextID("m"), "hi", now)},
	})
	must(t, err)
	if got := out.Results[0].XPAwarded; got != 5 {
		t.Fatalf("XPAwarded = %d,期望 5(ruleset 的 discord_message.xp)", got)
	}
	if n := countRows(t,
		`SELECT count(*)::int FROM platform.xp_events WHERE user_id = $1 AND source = 'discord_message'`,
		f.userID); n != 1 {
		t.Fatalf("xp_events 有 %d 列,期望 1", n)
	}
	if d := readDaily(t, f, now); d.xpGained != 5 {
		t.Fatalf("activity_daily.xp_gained = %d,期望 5", d.xpGained)
	}
}

// grant_xp=false 的頻道(指令區/刷頻區)只記錄不發 XP。
func TestMessageNoXPWhenChannelDisabled(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, false)
	now := time.Now().UTC()

	out, err := svc.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, nextID("m"), "hi", now)},
	})
	must(t, err)
	if got := out.Results[0].XPAwarded; got != 0 {
		t.Fatalf("XPAwarded = %d,期望 0", got)
	}
	if d := readDaily(t, f, now); d.messages != 1 || d.xpGained != 0 {
		t.Fatalf("messages/xp = %d/%d,期望 1/0(照記錄但不發)", d.messages, d.xpGained)
	}
}

// ── 語音 ──────────────────────────────────────────────────────────

// 先記進場,再補收尾:秒數只計一次,重送不再計。
func TestVoiceSessionOpenThenClose(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "voice", false, true)
	joined := time.Now().UTC().Truncate(time.Second)
	left := joined.Add(10 * time.Minute)

	open := activitylog.VoiceSessionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
		JoinedAt: joined, PeerCountAvg: 3,
	}
	r1, err := noXP.RecordVoiceSession(ctx, open)
	must(t, err)
	if r1.Deduplicated || r1.VoiceSecondsCounted != 0 {
		t.Fatalf("剛進場不該有秒數:%+v", r1)
	}

	closeIt := open
	closeIt.LeftAt = &left
	r2, err := noXP.RecordVoiceSession(ctx, closeIt)
	must(t, err)
	if !r2.Closed || r2.VoiceSecondsCounted != 600 {
		t.Fatalf("收尾結果 = %+v,期望 closed=true seconds=600", r2)
	}

	r3, err := noXP.RecordVoiceSession(ctx, closeIt)
	must(t, err)
	if !r3.Deduplicated || r3.VoiceSecondsCounted != 0 {
		t.Fatalf("重送應該被判重複:%+v", r3)
	}

	if n := countRows(t, `SELECT count(*)::int FROM platform.voice_sessions WHERE user_id = $1`, f.userID); n != 1 {
		t.Fatalf("voice_sessions 有 %d 列,期望 1", n)
	}
	if d := readDaily(t, f, joined); d.voiceSeconds != 600 {
		t.Fatalf("activity_daily.voice_seconds = %d,期望 600", d.voiceSeconds)
	}
}

// 併發送同一段語音:只留一列。
func TestVoiceSessionConcurrentDedup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "voice", false, true)
	joined := time.Now().UTC().Truncate(time.Second)
	left := joined.Add(time.Minute)
	p := activitylog.VoiceSessionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
		JoinedAt: joined, LeftAt: &left, PeerCountAvg: 3,
	}

	const n = 8
	var wg sync.WaitGroup
	counted := make([]int32, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := noXP.RecordVoiceSession(ctx, p)
			if err != nil {
				t.Errorf("第 %d 個併發呼叫失敗:%v", i, err)
				return
			}
			counted[i] = r.VoiceSecondsCounted
		}()
	}
	wg.Wait()

	total := int32(0)
	for _, c := range counted {
		total += c
	}
	if total != 60 {
		t.Fatalf("併發計入了 %d 秒,期望 60(只能算一次)", total)
	}
	if c := countRows(t, `SELECT count(*)::int FROM platform.voice_sessions WHERE user_id = $1`, f.userID); c != 1 {
		t.Fatalf("voice_sessions 有 %d 列,期望 1", c)
	}
	if d := readDaily(t, f, joined); d.voiceSeconds != 60 {
		t.Fatalf("activity_daily.voice_seconds = %d,期望 60", d.voiceSeconds)
	}
}

// 防掛機門檻:人數不足 voice_xp_min_peers(seed=2)就不發 XP,但事實照記。
func TestVoiceXPRequiresMinPeers(t *testing.T) {
	setup(t)
	ctx := context.Background()
	ch := ""
	joined := time.Now().UTC().Truncate(time.Second)
	left := joined.Add(10 * time.Minute)

	cases := []struct {
		name  string
		peers float32
		deaf  bool
		want  int64
	}{
		{"一個人掛機", 1, false, 0},
		{"兩個人", 2, false, 20}, // 10 分鐘 × xp_per_minute=2
		{"人夠但自己拒聽", 3, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ch = registerChannel(t, f.spaceID, "voice", false, true)
			deaf := c.deaf
			r, err := svc.RecordVoiceSession(ctx, activitylog.VoiceSessionParams{
				UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
				JoinedAt: joined, LeftAt: &left, PeerCountAvg: c.peers, SelfDeafened: &deaf,
			})
			must(t, err)
			if r.XPAwarded != c.want {
				t.Fatalf("XPAwarded = %d,期望 %d", r.XPAwarded, c.want)
			}
			// 不論發不發 XP,事實與秒數都要記。
			if r.VoiceSecondsCounted != 600 {
				t.Fatalf("VoiceSecondsCounted = %d,期望 600(門檻只影響 XP)", r.VoiceSecondsCounted)
			}
			if d := readDaily(t, f, joined); d.voiceSeconds != 600 || d.xpGained != c.want {
				t.Fatalf("activity_daily voice/xp = %d/%d,期望 600/%d", d.voiceSeconds, d.xpGained, c.want)
			}
		})
	}
}

// ── Reaction ──────────────────────────────────────────────────────

// 按 → 取消 → 再按:reaction_count 跟著動,但當日參與只算一次(防刷)。
func TestReactionAddRemoveReadd(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	_, authorDiscord := newLinkedUser(t)
	now := time.Now().UTC()
	messageID := nextID("m")

	add := activitylog.ReactionParams{
		UserID: f.userID, GuildID: f.guildID, MessageID: messageID,
		Emoji: "👍", OccurredAt: now, MessageAuthorID: authorDiscord,
	}

	r, err := noXP.RecordReaction(ctx, add)
	must(t, err)
	if r.Deduplicated || !r.StatsUpdated {
		t.Fatalf("第一次按的結果 = %+v", r)
	}
	assertReactionCount(t, messageID, 1)
	if d := readDaily(t, f, now); d.reactions != 1 {
		t.Fatalf("activity_daily.reactions = %d,期望 1", d.reactions)
	}

	// 重送同一次「按」→ 判重複。
	again, err := noXP.RecordReaction(ctx, add)
	must(t, err)
	if !again.Deduplicated {
		t.Fatal("重送沒有被判重複")
	}
	assertReactionCount(t, messageID, 1)

	// 取消。
	remove := add
	remove.Removed = true
	remove.OccurredAt = now.Add(time.Minute)
	if _, err := noXP.RecordReaction(ctx, remove); err != nil {
		t.Fatalf("取消失敗:%v", err)
	}
	assertReactionCount(t, messageID, 0)

	// 再按回來:計數回來,但當日參與**不再加一次**。
	readd := add
	readd.OccurredAt = now.Add(2 * time.Minute)
	if _, err := noXP.RecordReaction(ctx, readd); err != nil {
		t.Fatalf("再按失敗:%v", err)
	}
	assertReactionCount(t, messageID, 1)
	if d := readDaily(t, f, now); d.reactions != 1 {
		t.Fatalf("activity_daily.reactions = %d,期望 1(按了取消再按不能刷)", d.reactions)
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.reaction_events WHERE message_id = $1`, messageID); n != 1 {
		t.Fatalf("reaction_events 有 %d 列,期望 1(天然鍵只有一列)", n)
	}
}

// 併發按同一個表情:只留一列,計數只加一次。
func TestReactionConcurrentDedup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	_, authorDiscord := newLinkedUser(t)
	now := time.Now().UTC()
	messageID := nextID("m")
	p := activitylog.ReactionParams{
		UserID: f.userID, GuildID: f.guildID, MessageID: messageID,
		Emoji: "🔥", OccurredAt: now, MessageAuthorID: authorDiscord,
	}

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := noXP.RecordReaction(ctx, p); err != nil {
				t.Errorf("併發 RecordReaction 失敗:%v", err)
			}
		}()
	}
	wg.Wait()

	if c := countRows(t, `SELECT count(*)::int FROM platform.reaction_events WHERE message_id = $1`, messageID); c != 1 {
		t.Fatalf("reaction_events 有 %d 列,期望 1", c)
	}
	assertReactionCount(t, messageID, 1)
	if d := readDaily(t, f, now); d.reactions != 1 {
		t.Fatalf("activity_daily.reactions = %d,期望 1", d.reactions)
	}
}

// 沒見過的訊息又沒給作者 → 只記事實,不硬猜一個 message_stats 作者。
func TestReactionWithoutKnownAuthorSkipsStats(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	messageID := nextID("m")
	r, err := noXP.RecordReaction(ctx, activitylog.ReactionParams{
		UserID: f.userID, GuildID: f.guildID, MessageID: messageID,
		Emoji: "❤", OccurredAt: time.Now().UTC(),
	})
	must(t, err)
	if r.StatsUpdated {
		t.Fatal("沒有作者資訊卻宣稱更新了 message_stats")
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.reaction_events WHERE message_id = $1`, messageID); n != 1 {
		t.Fatalf("reaction_events 有 %d 列,期望 1(事實還是要記)", n)
	}
}

func assertReactionCount(t *testing.T, messageID string, want int32) {
	t.Helper()
	var got int32
	must(t, pool.QueryRow(context.Background(),
		`SELECT reaction_count FROM platform.message_stats WHERE message_id = $1`, messageID).Scan(&got))
	if got != want {
		t.Fatalf("message_stats.reaction_count = %d,期望 %d", got, want)
	}
}

// ── 訊息舊版本 ────────────────────────────────────────────────────

func TestRevisionEditedAndDedup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	at := time.Now().UTC().Truncate(time.Microsecond)
	p := activitylog.RevisionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
		MessageID: nextID("m"), PreviousContent: "舊的內容",
		Kind: activitylog.RevisionEdited, CapturedAt: at,
	}

	r1, err := noXP.RecordRevision(ctx, p)
	must(t, err)
	if r1.Deduplicated || !r1.ExcerptStored {
		t.Fatalf("第一次編輯紀錄 = %+v", r1)
	}
	r2, err := noXP.RecordRevision(ctx, p)
	must(t, err)
	if !r2.Deduplicated {
		t.Fatal("同一次編輯重送沒有被判重複")
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_revisions WHERE message_id = $1`, p.MessageID); n != 1 {
		t.Fatalf("message_revisions 有 %d 列,期望 1", n)
	}

	// 第二次編輯(不同 captured_at)是另一個版本,要記下來。
	p2 := p
	p2.CapturedAt = at.Add(time.Minute)
	p2.PreviousContent = "第二版"
	if _, err := noXP.RecordRevision(ctx, p2); err != nil {
		t.Fatalf("第二次編輯失敗:%v", err)
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_revisions WHERE message_id = $1`, p.MessageID); n != 2 {
		t.Fatalf("message_revisions 有 %d 列,期望 2", n)
	}
}

// 刪除:只會記一次,而且會在 message_logs 打上 deleted_at。
func TestRevisionDeletedMarksMessageLog(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	id := nextID("m")

	out, err := noXP.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, id, "會被刪掉的訊息", now)},
	})
	must(t, err)
	if out.Results[0].Err != nil {
		t.Fatalf("前置寫入失敗:%v", out.Results[0].Err)
	}

	del := activitylog.RevisionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch, MessageID: id,
		PreviousContent: "會被刪掉的訊息", Kind: activitylog.RevisionDeleted,
		CapturedAt: now.Add(time.Minute),
	}
	if _, err := noXP.RecordRevision(ctx, del); err != nil {
		t.Fatalf("記刪除失敗:%v", err)
	}
	// 刪除事件重送時間不同也只記一次(Discord 不給刪除時間)。
	del2 := del
	del2.CapturedAt = now.Add(2 * time.Minute)
	r, err := noXP.RecordRevision(ctx, del2)
	must(t, err)
	if !r.Deduplicated {
		t.Fatal("重送的刪除事件沒有被判重複")
	}
	if n := countRows(t,
		`SELECT count(*)::int FROM platform.message_revisions WHERE message_id = $1 AND kind = 'deleted'`,
		id); n != 1 {
		t.Fatalf("刪除版本有 %d 列,期望 1", n)
	}
	var deletedAt *time.Time
	must(t, pool.QueryRow(ctx,
		`SELECT deleted_at FROM platform.message_logs WHERE message_id = $1`, id).Scan(&deletedAt))
	if deletedAt == nil {
		t.Fatal("message_logs.deleted_at 沒有被標記")
	}
}

// 白名單關著時,舊版本只留事件不留內容。
func TestRevisionRespectsWhitelist(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", false, true)
	id := nextID("m")
	r, err := noXP.RecordRevision(ctx, activitylog.RevisionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
		MessageID: id, PreviousContent: "祕密舊內容",
		Kind: activitylog.RevisionEdited, CapturedAt: time.Now().UTC(),
	})
	must(t, err)
	if r.ExcerptStored {
		t.Fatal("白名單關著卻存了舊內容")
	}
	var stored *string
	must(t, pool.QueryRow(ctx,
		`SELECT excerpt FROM platform.message_revisions WHERE message_id = $1`, id).Scan(&stored))
	if stored != nil {
		t.Fatalf("excerpt = %q,期望 NULL", *stored)
	}
}

// ── Presence ──────────────────────────────────────────────────────

func TestPresenceOpenCloseAndDedup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	started := time.Now().UTC().Truncate(time.Second)
	ended := started.Add(30 * time.Minute)

	open := activitylog.PresenceParams{
		UserID: f.userID, GuildID: f.guildID, Status: "online", StartedAt: started,
	}
	r1, err := noXP.RecordPresence(ctx, open)
	must(t, err)
	if r1.OnlineSecondsCounted != 0 {
		t.Fatalf("區間還開著就計了 %d 秒", r1.OnlineSecondsCounted)
	}

	closed := open
	closed.EndedAt = &ended
	r2, err := noXP.RecordPresence(ctx, closed)
	must(t, err)
	if !r2.Closed || r2.OnlineSecondsCounted != 1800 {
		t.Fatalf("收尾結果 = %+v,期望 closed=true seconds=1800", r2)
	}

	r3, err := noXP.RecordPresence(ctx, closed)
	must(t, err)
	if !r3.Deduplicated {
		t.Fatal("重送沒有被判重複")
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.presence_spans WHERE user_id = $1`, f.userID); n != 1 {
		t.Fatalf("presence_spans 有 %d 列,期望 1", n)
	}
	if d := readDaily(t, f, started); d.onlineSeconds != 1800 {
		t.Fatalf("activity_daily.online_seconds = %d,期望 1800", d.onlineSeconds)
	}
	// presence 永遠不發 XP(schemas/10:不可靠且掛機可刷)。
	if d := readDaily(t, f, started); d.xpGained != 0 {
		t.Fatalf("presence 竟然發了 %d XP", d.xpGained)
	}
}

// offline 區間不是線上時長。
func TestPresenceOfflineNotCounted(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	started := time.Now().UTC().Truncate(time.Second)
	ended := started.Add(time.Hour)

	r, err := noXP.RecordPresence(ctx, activitylog.PresenceParams{
		UserID: f.userID, GuildID: f.guildID, Status: "offline",
		StartedAt: started, EndedAt: &ended,
	})
	must(t, err)
	if r.OnlineSecondsCounted != 0 {
		t.Fatalf("offline 竟然計了 %d 秒", r.OnlineSecondsCounted)
	}
	if d := readDaily(t, f, started); d.onlineSeconds != 0 {
		t.Fatalf("activity_daily.online_seconds = %d,期望 0", d.onlineSeconds)
	}
}

func TestPresenceRejectsUnknownStatus(t *testing.T) {
	setup(t)
	f := newFixture(t)
	_, err := noXP.RecordPresence(context.Background(), activitylog.PresenceParams{
		UserID: f.userID, GuildID: f.guildID, Status: "invisible",
		StartedAt: time.Now().UTC(),
	})
	if !errors.Is(err, activitylog.ErrInvalidRequest) {
		t.Fatalf("err = %v,期望 ErrInvalidRequest", err)
	}
}

// ── XP 與事實同一個 tx ────────────────────────────────────────────

// failingXP 模擬「XP 入帳失敗」(source 停用、DB 出事都會長這樣)。
type failingXP struct{}

var errXPDown = errors.New("XP 服務故障")

func (failingXP) AwardInTx(context.Context, pgx.Tx, xp.AwardParams) (*xp.AwardResult, error) {
	return nil, errXPDown
}

// poisonXP 先用真的 xppg 入帳,**再**回錯:用來證明 XP 寫在呼叫端的 tx 裡。
// 如果 XP 另開 tx(舊設計),它寫的東西就不會跟著事實一起 rollback。
type poisonXP struct{ real *xppg.Service }

func (p poisonXP) AwardInTx(ctx context.Context, tx pgx.Tx, params xp.AwardParams) (*xp.AwardResult, error) {
	if _, err := p.real.AwardInTx(ctx, tx, params); err != nil {
		return nil, err
	}
	return nil, errors.New("入帳之後才失敗")
}

// 訊息:XP 發放失敗 → 事實與彙總都不留(三者同 tx)。
// 而且冪等鍵沒被佔走 —— 閘道重送時是真的重試,不是撞冪等後永遠拿不到 XP。
func TestMessageXPFailureRollsBackFact(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	id := nextID("m")

	broken := activitylogpg.New(pool, failingXP{}, nil)
	out, err := broken.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, id, "hi", now)},
	})
	must(t, err)
	if out.Results[0].Err == nil {
		t.Fatal("XP 失敗時這一筆應該回錯(事實不該留下)")
	}

	if n := countRows(t, `SELECT count(*)::int FROM platform.message_stats WHERE message_id = $1`, id); n != 0 {
		t.Fatalf("XP 失敗卻留下了 %d 列 message_stats", n)
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_logs WHERE message_id = $1`, id); n != 0 {
		t.Fatalf("XP 失敗卻留下了 %d 列 message_logs", n)
	}
	if d := readDaily(t, f, now); d.messages != 0 || d.xpGained != 0 {
		t.Fatalf("activity_daily messages/xp = %d/%d,期望 0/0", d.messages, d.xpGained)
	}

	// XP 修好之後重送同一則:冪等鍵沒被佔走,這次要真的寫進去並發 XP。
	retry, err := svc.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, id, "hi", now)},
	})
	must(t, err)
	if r := retry.Results[0]; r.Err != nil || r.Deduplicated || r.XPAwarded != 5 {
		t.Fatalf("修好後重送應該成功並發 XP:%+v", r)
	}
}

// 訊息:XP 已經寫進去、之後才失敗 → 連 xp_events 都要跟著 rollback。
// 這是「同一個 tx」與「另開 tx」唯一分得出來的地方。
func TestMessageXPWriteIsInCallerTx(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "text", true, true)
	now := time.Now().UTC()
	id := nextID("m")

	poisoned := activitylogpg.New(pool, poisonXP{real: xppg.New(pool)}, nil)
	out, err := poisoned.RecordMessages(ctx, activitylog.MessagesParams{
		UserID: f.userID, GuildID: f.guildID,
		Messages: []activitylog.MessageParams{msg(ch, id, "hi", now)},
	})
	must(t, err)
	if out.Results[0].Err == nil {
		t.Fatal("這一筆應該回錯")
	}

	if n := countRows(t,
		`SELECT count(*)::int FROM platform.xp_events WHERE user_id = $1 AND community_id = $2`,
		f.userID, f.communityID); n != 0 {
		t.Fatalf("XP 沒跟著事實 rollback:xp_events 留下 %d 列(XP 另開了 tx?)", n)
	}
	if n := countRows(t,
		`SELECT count(*)::int FROM platform.user_xp WHERE user_id = $1 AND community_id = $2`,
		f.userID, f.communityID); n != 0 {
		t.Fatalf("user_xp 投影列沒跟著 rollback(%d 列)", n)
	}
	if n := countRows(t, `SELECT count(*)::int FROM platform.message_stats WHERE message_id = $1`, id); n != 0 {
		t.Fatalf("事實留下了 %d 列 message_stats", n)
	}
}

// 語音:XP 失敗 → 整段語音的事實與彙總都不留。
func TestVoiceXPFailureRollsBackFact(t *testing.T) {
	setup(t)
	ctx := context.Background()
	f := newFixture(t)
	ch := registerChannel(t, f.spaceID, "voice", false, true)
	joined := time.Now().UTC().Truncate(time.Second)
	left := joined.Add(10 * time.Minute)

	broken := activitylogpg.New(pool, failingXP{}, nil)
	_, err := broken.RecordVoiceSession(ctx, activitylog.VoiceSessionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
		JoinedAt: joined, LeftAt: &left, PeerCountAvg: 3,
	})
	if err == nil {
		t.Fatal("XP 失敗時應該回錯")
	}
	if !errors.Is(err, errXPDown) {
		t.Fatalf("錯誤應該包住原因,得到 %v", err)
	}

	if n := countRows(t,
		`SELECT count(*)::int FROM platform.voice_sessions WHERE user_id = $1 AND channel_id = $2`,
		f.userID, ch); n != 0 {
		t.Fatalf("XP 失敗卻留下了 %d 列 voice_sessions", n)
	}
	if d := readDaily(t, f, joined); d.voiceSeconds != 0 || d.xpGained != 0 {
		t.Fatalf("activity_daily voice/xp = %d/%d,期望 0/0", d.voiceSeconds, d.xpGained)
	}

	// 修好後重送:advisory lock 那條路徑沒有留下殘骸,這次要完整落地。
	r, err := svc.RecordVoiceSession(ctx, activitylog.VoiceSessionParams{
		UserID: f.userID, GuildID: f.guildID, ChannelID: ch,
		JoinedAt: joined, LeftAt: &left, PeerCountAvg: 3,
	})
	must(t, err)
	if r.Deduplicated {
		t.Fatal("重送不該被當成重複(前一次根本沒寫進去)")
	}
	if r.XPAwarded != 20 {
		t.Fatalf("XPAwarded = %d,期望 20", r.XPAwarded)
	}
	if d := readDaily(t, f, joined); d.voiceSeconds != 600 || d.xpGained != 20 {
		t.Fatalf("activity_daily voice/xp = %d/%d,期望 600/20", d.voiceSeconds, d.xpGained)
	}
}
