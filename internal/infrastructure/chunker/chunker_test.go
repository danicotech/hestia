package chunker_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/infrastructure/chunker"
)

// 一段完整對話的三則發言(長度都過 min_message_chars,合起來也過 min_chunk_chars)。
var conversation = []string{
	"今天的賽程表出來了嗎 我想確認一下時間",
	"剛剛公佈了 下午三點第一場",
	"了解 那我先去準備一下裝備",
}

func build(t *testing.T) (int, error) {
	t.Helper()
	return chunker.New(pool).BuildChunks(context.Background())
}

// 間隔內的訊息合成一個 chunk;整段已封閉(最後一則超過一個 gap)。
func TestBuildChunks_SingleClosedChunk(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-single", true)
	a, b := newUser(t, "小明"), newUser(t, "小華")

	msg(t, space, ch, nil, a, 60, conversation[0])
	msg(t, space, ch, nil, b, 58, conversation[1])
	msg(t, space, ch, nil, a, 55, conversation[2])

	n, err := build(t)
	if err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	if n < 1 {
		t.Fatalf("應至少產生 1 個 chunk,得到 %d", n)
	}
	got := listChunks(t, space)
	if len(got) != 1 {
		t.Fatalf("應只有 1 個 chunk,得到 %d", len(got))
	}
	c := got[0]
	if c.MessageCount != 3 {
		t.Errorf("message_count 應為 3,得到 %d", c.MessageCount)
	}
	for _, line := range conversation {
		if !strings.Contains(c.Text, line) {
			t.Errorf("text 少了一句:%q\n實際:%q", line, c.Text)
		}
	}
	// embed_model / embedded_at 留空:嵌入模型尚未選型,換模型時靠它們判斷要重算哪些
	if c.EmbedModel != nil || c.EmbeddedAt != nil {
		t.Errorf("embed_model/embedded_at 應留 NULL,得到 %v / %v", c.EmbedModel, c.EmbeddedAt)
	}
}

// 超過 chunk_gap_minutes(seed 20)就斷開成兩個 chunk。
func TestBuildChunks_GapSplits(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-gap", true)
	a, b := newUser(t, "阿一"), newUser(t, "阿二")

	// 第一段:180 → 175 分鐘前
	msg(t, space, ch, nil, a, 180, conversation[0])
	msg(t, space, ch, nil, b, 178, conversation[1])
	msg(t, space, ch, nil, a, 175, conversation[2])
	// 中間空 75 分鐘(> 20)→ 必斷
	msg(t, space, ch, nil, b, 100, conversation[0])
	msg(t, space, ch, nil, a, 98, conversation[1])
	msg(t, space, ch, nil, b, 96, conversation[2])

	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	got := listChunks(t, space)
	if len(got) != 2 {
		t.Fatalf("應斷成 2 個 chunk,得到 %d", len(got))
	}
	if !got[0].EndedAt.Before(got[1].StartedAt) {
		t.Errorf("兩段順序不對:%v / %v", got[0], got[1])
	}
	for _, c := range got {
		if c.MessageCount != 3 {
			t.Errorf("每段應為 3 則,得到 %d", c.MessageCount)
		}
	}
}

// 還沒封閉的對話(最後一則在 gap 內)不產生 chunk——否則同一段會被切成兩個。
func TestBuildChunks_OpenConversationSkipped(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-open", true)
	a, b := newUser(t, "剛剛"), newUser(t, "還在講")

	msg(t, space, ch, nil, a, 8, conversation[0])
	msg(t, space, ch, nil, b, 5, conversation[1])
	msg(t, space, ch, nil, a, 2, conversation[2])

	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	if got := listChunks(t, space); len(got) != 0 {
		t.Fatalf("尚未封閉的對話不該產生 chunk,得到 %d 個", len(got))
	}

	// 對照組:同樣的三則發言,整段都已超過 gap → 收成「一個」chunk。
	space2 := newSpace(t)
	ch2 := newChannel(t, space2, "c-closed", true)
	msg(t, space2, ch2, nil, a, 40, conversation[0])
	msg(t, space2, ch2, nil, b, 38, conversation[1])
	msg(t, space2, ch2, nil, a, 35, conversation[2])
	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	if got := listChunks(t, space2); len(got) != 1 {
		t.Fatalf("已封閉的對話應產生 1 個 chunk,得到 %d", len(got))
	}
}

// opt_out_ai_corpus 的人只踢掉他自己的發言,同段對話其他人的照留。
func TestBuildChunks_OptOutUserExcludedOthersKept(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-optout", true)
	quiet := newUser(t, "不入語料的人")
	setOptOutAICorpus(t, quiet)
	a, b := newUser(t, "甲"), newUser(t, "乙")

	msg(t, space, ch, nil, a, 60, conversation[0])
	msg(t, space, ch, nil, quiet, 59, "這句話絕對不可以進語料庫喔喔喔")
	msg(t, space, ch, nil, b, 58, conversation[1])
	msg(t, space, ch, nil, a, 55, conversation[2])

	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	got := listChunks(t, space)
	if len(got) != 1 {
		t.Fatalf("應產生 1 個 chunk,得到 %d", len(got))
	}
	c := got[0]
	if strings.Contains(c.Text, "絕對不可以進語料庫") {
		t.Errorf("opt-out 使用者的內容進了 text:%q", c.Text)
	}
	if c.MessageCount != 3 {
		t.Errorf("message_count 應只算 3 則合格訊息,得到 %d", c.MessageCount)
	}
	for _, id := range c.Participants {
		if id == quiet {
			t.Errorf("participant_user_ids 不該含 opt-out 使用者 %d:%v", quiet, c.Participants)
		}
	}
	if !containsAll(c.Participants, a, b) {
		t.Errorf("participant_user_ids 應含其餘發言者 %d/%d,得到 %v", a, b, c.Participants)
	}
}

// 純表情 / 極短 / Bot / 未列入白名單的頻道:一律不算發言。
func TestBuildChunks_NoiseExcluded(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-noise", true)
	newChannel(t, space, "c-unlisted", false) // 白名單外
	a, b := newUser(t, "正常人"), newUser(t, "另一個正常人")
	bot := newBotUser(t)

	msg(t, space, ch, nil, a, 60, conversation[0])
	msg(t, space, ch, nil, b, 59, "😂😂😂")                     // 純 unicode 表情
	msg(t, space, ch, nil, b, 59, "<:pepe:12345> <@!99>")    // 純自訂表情 + mention
	msg(t, space, ch, nil, b, 58, "w")                       // 極短
	msg(t, space, ch, nil, bot, 58, "【系統公告】伺服器將於今晚維護,請提早離線") // bot
	msg(t, space, ch, nil, b, 57, conversation[1])
	msg(t, space, ch, nil, a, 55, conversation[2])
	// 白名單外的頻道整段都不該進語料
	msg(t, space, "c-unlisted", nil, a, 60, conversation[0])
	msg(t, space, "c-unlisted", nil, b, 59, conversation[1])
	msg(t, space, "c-unlisted", nil, a, 58, conversation[2])
	// 根本沒註冊的頻道
	msg(t, space, "c-ghost", nil, a, 60, conversation[0])
	msg(t, space, "c-ghost", nil, b, 59, conversation[1])
	msg(t, space, "c-ghost", nil, a, 58, conversation[2])

	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	got := listChunks(t, space)
	if len(got) != 1 {
		t.Fatalf("只有白名單頻道那段該成 chunk,得到 %d 個", len(got))
	}
	c := got[0]
	if c.ChannelID != ch {
		t.Errorf("chunk 頻道應為 %s,得到 %s", ch, c.ChannelID)
	}
	if c.MessageCount != 3 {
		t.Errorf("雜訊應全部排除,message_count 應為 3,得到 %d", c.MessageCount)
	}
	for _, bad := range []string{"😂", "pepe", "系統公告"} {
		if strings.Contains(c.Text, bad) {
			t.Errorf("text 含被排除的內容 %q:%q", bad, c.Text)
		}
	}
	for _, id := range c.Participants {
		if id == bot {
			t.Errorf("participant_user_ids 不該含 bot %d:%v", bot, c.Participants)
		}
	}
	// 去識別化:只有內部 id,暱稱一個字都不該出現
	for _, name := range []string{"正常人", "另一個正常人", "some-bot"} {
		if strings.Contains(c.Text, name) {
			t.Errorf("text 出現暱稱 %q(必須去識別化):%q", name, c.Text)
		}
	}
}

// 太短的對話(則數不足 / 文字太少)不值得嵌入。
func TestBuildChunks_TooShortConversationSkipped(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-short", true)
	a, b := newUser(t, "問"), newUser(t, "答")

	// 兩則:呼叫與回應,不算對話
	msg(t, space, ch, nil, a, 60, "有人在嗎 現在還有人在線上嗎")
	msg(t, space, ch, nil, b, 59, "我在 怎麼了嗎有什麼需要幫忙的")
	// 另一段:則數夠但字數太少
	msg(t, space, ch, nil, a, 200, "在嗎")
	msg(t, space, ch, nil, b, 199, "在啊")
	msg(t, space, ch, nil, a, 198, "好喔")

	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	if got := listChunks(t, space); len(got) != 0 {
		t.Fatalf("太短的對話不該產生 chunk,得到 %d 個:%+v", len(got), got)
	}
}

// thread 是分組鍵的一部分:同頻道不同 thread 不能混成一段。
func TestBuildChunks_ThreadIsPartOfGrouping(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-thread", true)
	a, b := newUser(t, "T1"), newUser(t, "T2")
	th := "thread-1"

	msg(t, space, ch, nil, a, 60, conversation[0])
	msg(t, space, ch, nil, b, 59, conversation[1])
	msg(t, space, ch, nil, a, 58, conversation[2])
	msg(t, space, ch, &th, b, 60, conversation[0])
	msg(t, space, ch, &th, a, 59, conversation[1])
	msg(t, space, ch, &th, b, 58, conversation[2])

	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	got := listChunks(t, space)
	if len(got) != 2 {
		t.Fatalf("主頻道與 thread 應各成一段,得到 %d", len(got))
	}
	var withThread, withoutThread int
	for _, c := range got {
		if c.ThreadID == nil {
			withoutThread++
		} else if *c.ThreadID == th {
			withThread++
		}
	}
	if withThread != 1 || withoutThread != 1 {
		t.Errorf("thread 分組不對:%+v", got)
	}
}

// 重跑不重複產生;新對話進來只補新的那一段。
func TestBuildChunks_Idempotent(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-idem", true)
	a, b := newUser(t, "重跑甲"), newUser(t, "重跑乙")

	msg(t, space, ch, nil, a, 120, conversation[0])
	msg(t, space, ch, nil, b, 118, conversation[1])
	msg(t, space, ch, nil, a, 116, conversation[2])

	if _, err := build(t); err != nil {
		t.Fatalf("第一次 BuildChunks: %v", err)
	}
	first := listChunks(t, space)
	if len(first) != 1 {
		t.Fatalf("第一次應產生 1 個 chunk,得到 %d", len(first))
	}
	for i := 0; i < 3; i++ {
		if _, err := build(t); err != nil {
			t.Fatalf("重跑 BuildChunks: %v", err)
		}
	}
	if again := listChunks(t, space); len(again) != 1 {
		t.Fatalf("重跑不該重複產生,得到 %d 個", len(again))
	}

	// 補一段新的已封閉對話,只該多出 1 個 chunk
	msg(t, space, ch, nil, b, 60, conversation[0])
	msg(t, space, ch, nil, a, 58, conversation[1])
	msg(t, space, ch, nil, b, 56, conversation[2])
	if _, err := build(t); err != nil {
		t.Fatalf("BuildChunks: %v", err)
	}
	if got := listChunks(t, space); len(got) != 2 {
		t.Fatalf("新對話應只多出 1 個 chunk,總數得到 %d", len(got))
	}
}

// 併發:多個實例同時跑(runner 的 advisory lock 擋的是同名 job 重入,
// 直接呼叫的路徑沒有那層保護),不得產生重複 chunk。
func TestBuildChunks_ConcurrentRuns(t *testing.T) {
	setup(t)
	space := newSpace(t)
	ch := newChannel(t, space, "c-concurrent", true)
	a, b := newUser(t, "併發甲"), newUser(t, "併發乙")

	msg(t, space, ch, nil, a, 120, conversation[0])
	msg(t, space, ch, nil, b, 118, conversation[1])
	msg(t, space, ch, nil, a, 116, conversation[2])

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	inserted := make(chan int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := chunker.New(pool).BuildChunks(context.Background())
			if err != nil {
				errs <- err
				return
			}
			inserted <- got
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(inserted)
	for err := range errs {
		t.Fatalf("併發 BuildChunks: %v", err)
	}
	sum := 0
	for v := range inserted {
		sum += v
	}
	if sum != 1 {
		t.Errorf("八個併發執行加起來只該插 1 個 chunk,得到 %d", sum)
	}
	if got := listChunks(t, space); len(got) != 1 {
		t.Fatalf("併發不得產生重複 chunk,得到 %d 個", len(got))
	}
}

func containsAll(haystack []int64, want ...int64) bool {
	for _, w := range want {
		found := false
		for _, h := range haystack {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
