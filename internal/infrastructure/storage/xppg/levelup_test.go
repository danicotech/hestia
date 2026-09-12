package xppg_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/notification"
	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/xppg"
)

// 升級偵測與里程碑獎勵(schemas/24)。
//
// 這裡守的核心是:**升級事件與 XP 入帳同生共死**,而且重送不會發兩次獎勵。

// outboxOf 取出某個 topic 的待送事件 payload。
func outboxOf(t *testing.T, topic string) [][]byte {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT payload FROM platform.outbox_events WHERE topic = $1 ORDER BY id`, topic)
	if err != nil {
		t.Fatalf("讀 outbox: %v", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("掃 outbox: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func clearOutbox(t *testing.T) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM platform.outbox_events`); err != nil {
		t.Fatalf("清 outbox: %v", err)
	}
}

func TestAward_EmitsLevelUpOnCrossing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")

	// 預設曲線:2 級門檻 100。給 150 一定跨過。
	res, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 150,
	})
	if err != nil {
		t.Fatalf("Award: %v", err)
	}
	if !res.LeveledUp() {
		t.Fatalf("150 XP 應該從 1 級升到 2 級,得到 %d → %d", res.FromLevel, res.ToLevel)
	}

	// 公告與發獎是兩個 topic:失敗模式不同,不該互相牽連。
	if got := len(outboxOf(t, notification.TopicLevelUp)); got != 1 {
		t.Fatalf("應有一則升級公告,得到 %d", got)
	}
	if got := len(outboxOf(t, notification.TopicLevelReward)); got != 1 {
		t.Fatalf("應有一則發獎事件,得到 %d", got)
	}

	var p struct {
		FromLevel int32 `json:"from_level"`
		ToLevel   int32 `json:"to_level"`
		Subject   string
	}
	if err := json.Unmarshal(outboxOf(t, notification.TopicLevelUp)[0], &p); err != nil {
		t.Fatalf("解析 payload: %v", err)
	}
	if p.FromLevel != 1 || p.ToLevel != 2 {
		t.Fatalf("payload 等級 = %d → %d", p.FromLevel, p.ToLevel)
	}
}

// 沒跨過門檻就不該發事件 —— 每一則發言都貼一次「升級了」會讓頻道無法使用。
func TestAward_NoEventWithoutCrossing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")

	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 50, // 不到 100
	}); err != nil {
		t.Fatalf("Award: %v", err)
	}
	if got := len(outboxOf(t, notification.TopicLevelUp)); got != 0 {
		t.Fatalf("沒升級不該有事件,得到 %d", got)
	}
}

// 一次跳多級是可能的(語音一次入帳很大)。事件要帶區間,
// 否則呼叫端只知道新等級,會漏掉中間跨過的里程碑。
func TestAward_MultiLevelJumpCarriesRange(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")

	res, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 2700, // 10 級門檻
	})
	if err != nil {
		t.Fatalf("Award: %v", err)
	}
	if res.FromLevel != 1 || res.ToLevel != 10 {
		t.Fatalf("應從 1 級跳到 10 級,得到 %d → %d", res.FromLevel, res.ToLevel)
	}
}

// 被冷卻或 cap 攔下時整個 tx 回滾 —— 事件不能留下來,
// 否則會貼出一則「升級了」但 XP 根本沒加。
func TestAward_CappedLeavesNoEvent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"admin":{"daily_cap":10}}}`)

	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 10,
	}); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	clearOutbox(t)
	// 第二次整筆被 cap 擋掉 → 不寫任何東西
	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 5000,
	}); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if got := len(outboxOf(t, notification.TopicLevelUp)); got != 0 {
		t.Fatalf("被 cap 攔下不該留事件,得到 %d", got)
	}
}

// ── 里程碑獎勵的發放 ────────────────────────────────────────────

func seedTokenReward(t *testing.T, communityID int64, level int32, amount int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.level_rewards
		   (community_id, subject, level, reward_kind, reward_ref, amount)
		 VALUES ($1, 'user', $2, 'tokens', 'coin', $3) RETURNING id`,
		communityID, level, amount).Scan(&id)
	if err != nil {
		t.Fatalf("建里程碑獎勵: %v", err)
	}
	return id
}

func balanceOf(t *testing.T, userID int64) int64 {
	t.Helper()
	var bal int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(balance), 0) FROM platform.user_balances
		 WHERE user_id = $1 AND currency = 'coin'`, userID).Scan(&bal)
	if err != nil {
		t.Fatalf("讀餘額: %v", err)
	}
	return bal
}

func TestRewardGranter_GrantsTokens(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")
	seedTokenReward(t, c, 2, 500)

	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 150,
	}); err != nil {
		t.Fatalf("Award: %v", err)
	}
	payloads := outboxOf(t, notification.TopicLevelReward)
	if len(payloads) != 1 {
		t.Fatalf("應有一則發獎事件,得到 %d", len(payloads))
	}

	g := xppg.NewRewardGranter(svc, ledgerpg.New(pool))
	if err := g.Handle(ctx, payloads[0]); err != nil {
		t.Fatalf("發獎: %v", err)
	}
	if got := balanceOf(t, u); got != 500 {
		t.Fatalf("應入帳 500,得到 %d", got)
	}
}

// outbox 是至少一次投遞,重送是常態。發兩次獎勵是這裡最貴的 bug ——
// 錢發出去就收不回來,只能再記一筆沖銷。
func TestRewardGranter_IsIdempotent(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")
	seedTokenReward(t, c, 2, 500)

	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 150,
	}); err != nil {
		t.Fatalf("Award: %v", err)
	}
	payload := outboxOf(t, notification.TopicLevelReward)[0]

	g := xppg.NewRewardGranter(svc, ledgerpg.New(pool))
	for i := 0; i < 3; i++ {
		if err := g.Handle(ctx, payload); err != nil {
			t.Fatalf("第 %d 次發獎: %v", i+1, err)
		}
	}
	if got := balanceOf(t, u); got != 500 {
		t.Fatalf("重送三次只該發一次,餘額應為 500,得到 %d", got)
	}
}

// 跨三級時,中間每一級的獎勵都要發到 —— 只看新等級會漏掉中間那些。
func TestRewardGranter_GrantsEveryCrossedMilestone(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")
	seedTokenReward(t, c, 2, 100)
	seedTokenReward(t, c, 3, 200)
	seedTokenReward(t, c, 5, 400)
	seedTokenReward(t, c, 50, 9999) // 沒跨到,不該發

	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 800, // 5 級門檻
	}); err != nil {
		t.Fatalf("Award: %v", err)
	}
	g := xppg.NewRewardGranter(svc, ledgerpg.New(pool))
	if err := g.Handle(ctx, outboxOf(t, notification.TopicLevelReward)[0]); err != nil {
		t.Fatalf("發獎: %v", err)
	}
	if got := balanceOf(t, u); got != 700 {
		t.Fatalf("應發 100+200+400=700,得到 %d", got)
	}
}

// 設錯的獎勵不能讓整批消失:壞掉的那一筆要讓事件重試,
// 而重試時已經發過的那些被冪等鍵擋著,不會重複。
func TestRewardGranter_UnknownKindFails(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clearOutbox(t)
	u := newUser(t)
	c := newCommunity(t, "")
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.level_rewards
		   (community_id, subject, level, reward_kind, reward_ref)
		 VALUES ($1, 'user', 2, 'entitlement', 'whatever')`, c); err != nil {
		t.Fatalf("建獎勵: %v", err)
	}
	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 150,
	}); err != nil {
		t.Fatalf("Award: %v", err)
	}
	g := xppg.NewRewardGranter(svc, ledgerpg.New(pool))
	err := g.Handle(ctx, outboxOf(t, notification.TopicLevelReward)[0])
	if err == nil {
		t.Fatal("尚未接上的獎勵類型應該明確失敗,而不是靜默跳過")
	}
	// 失敗要整批回滾,否則登記了卻沒發,重試時會被冪等鍵擋掉而永遠不發
	var grants int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.level_reward_grants WHERE user_id = $1`, u,
	).Scan(&grants); err != nil {
		t.Fatalf("讀發放紀錄: %v", err)
	}
	if grants != 0 {
		t.Fatalf("失敗後不該留下發放登記,得到 %d 筆", grants)
	}
}

// 寵物與使用者「各自獨立但同時漲」(schemas/25 Q11)。
// 共用一條的話,換一隻寵物還是同等級,養不養都一樣,沒有人會在意。
func TestAward_DeployedPetGainsSameXP(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	c := newCommunity(t, "")

	// 直接發一隻出戰中的寵物
	var defID, instID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.item_definitions (public_id, name, category)
		 VALUES (gen_random_uuid()::text, '橘貓', 'pet') RETURNING id`).Scan(&defID); err != nil {
		t.Fatalf("建寵物定義: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.item_instances (public_id, definition_id, owner_id, acquired_at)
		 VALUES (gen_random_uuid()::text, $1, $2, now()) RETURNING id`,
		defID, u).Scan(&instID); err != nil {
		t.Fatalf("發寵物: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.pet_states (item_instance_id, owner_id, deployed)
		 VALUES ($1, $2, true)`, instID, u); err != nil {
		t.Fatalf("建寵物狀態: %v", err)
	}

	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 150,
	}); err != nil {
		t.Fatalf("Award: %v", err)
	}

	var petXP int64
	if err := pool.QueryRow(ctx,
		`SELECT xp FROM platform.pet_states WHERE item_instance_id = $1`, instID).Scan(&petXP); err != nil {
		t.Fatalf("讀寵物 XP: %v", err)
	}
	if petXP != 150 {
		t.Fatalf("出戰寵物應拿到同樣的 150 XP,得到 %d", petXP)
	}
}

// 沒有出戰寵物是常態(多數人一開始都沒有),不該讓入帳失敗。
func TestAward_NoDeployedPetIsFine(t *testing.T) {
	setup(t)
	ctx := context.Background()
	u := newUser(t)
	c := newCommunity(t, "")
	if _, err := svc.Award(ctx, xp.AwardParams{
		UserID: u, CommunityID: c, Source: "admin", Amount: 50,
	}); err != nil {
		t.Fatalf("沒有寵物不該失敗: %v", err)
	}
}
