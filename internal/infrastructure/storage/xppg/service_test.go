package xppg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/infrastructure/storage/xppg"
)

// 整合測試對真 Postgres 跑(hestia/CLAUDE.md:mock 掉資料庫的併發測試沒有意義)。
// 共用一個容器,各測試用獨立 user + community 隔離。

var (
	pool *pgxpool.Pool
	svc  *xppg.Service
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
		svc = xppg.New(pool)
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

// newCommunity 建測試社群。config 非空時一併建 xp_rulesets 列並指派;
// 空字串 = 不指派 ruleset(M1 現實情境:communities.xp_ruleset_id 為 NULL)。
func newCommunity(t *testing.T, config string) int64 {
	t.Helper()
	ctx := context.Background()
	var rulesetID *int64
	if config != "" {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO platform.xp_rulesets (name, config) VALUES ('test', $1::jsonb) RETURNING id`,
			config,
		).Scan(&id); err != nil {
			t.Fatalf("建 ruleset: %v", err)
		}
		rulesetID = &id
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.communities (public_id, name, xp_ruleset_id)
		 VALUES (gen_random_uuid()::text, 'test', $1) RETURNING id`,
		rulesetID,
	).Scan(&id); err != nil {
		t.Fatalf("建 community: %v", err)
	}
	return id
}

func award(t *testing.T, userID, communityID, amount int64, source string) *xp.AwardResult {
	t.Helper()
	res, err := svc.Award(context.Background(), xp.AwardParams{
		UserID: userID, CommunityID: communityID, Source: source, Amount: amount,
	})
	if err != nil {
		t.Fatalf("award: %v", err)
	}
	return res
}

func sumEvents(t *testing.T, userID, communityID int64) int64 {
	t.Helper()
	var sum int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0) FROM platform.xp_events WHERE user_id=$1 AND community_id=$2`,
		userID, communityID,
	).Scan(&sum); err != nil {
		t.Fatalf("SUM(xp_events): %v", err)
	}
	return sum
}

func countEvents(t *testing.T, userID, communityID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform.xp_events WHERE user_id=$1 AND community_id=$2`,
		userID, communityID,
	).Scan(&n); err != nil {
		t.Fatalf("count(xp_events): %v", err)
	}
	return n
}

func projectedXP(t *testing.T, userID, communityID int64) (xpVal int64, level int) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT xp, level FROM platform.user_xp WHERE user_id=$1 AND community_id=$2`,
		userID, communityID,
	).Scan(&xpVal, &level)
	if err != nil {
		t.Fatalf("讀 user_xp: %v", err)
	}
	return xpVal, level
}

// 基本流:無 ruleset 的社群(M1 現實)入帳走安全預設(無冷卻、無 cap),
// 事件與投影同 tx 落地,level 恆 0。
func TestAwardBasic(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")

	res := award(t, u, c, 10, "discord_message")
	if res.Awarded != 10 || res.OnCooldown || res.Capped || res.XP != 10 {
		t.Fatalf("第一筆結果不符: %+v", res)
	}
	res = award(t, u, c, 25, "discord_voice")
	if res.Awarded != 25 || res.XP != 35 {
		t.Fatalf("第二筆結果不符: %+v", res)
	}

	if n := countEvents(t, u, c); n != 2 {
		t.Fatalf("xp_events 應有 2 列,得到 %d", n)
	}
	xpVal, level := projectedXP(t, u, c)
	if xpVal != 35 {
		t.Fatalf("user_xp.xp 應為 35,得到 %d", xpVal)
	}
	if level != 0 {
		t.Fatalf("M1 level 應恆 0,得到 %d", level)
	}
	var lastXpAt *string
	_ = pool.QueryRow(context.Background(),
		`SELECT last_xp_at::text FROM platform.user_xp WHERE user_id=$1 AND community_id=$2`, u, c,
	).Scan(&lastXpAt)
	if lastXpAt == nil {
		t.Fatal("入帳後 last_xp_at 不應為 NULL")
	}
}

// 核心不變量:併發 20 筆同 user 入帳後,投影 == SUM(事實)。
func TestConcurrentAwardsProjectionConsistency(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Award(context.Background(), xp.AwardParams{
				UserID: u, CommunityID: c, Source: "discord_message", Amount: 7,
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	sum := sumEvents(t, u, c)
	xpVal, _ := projectedXP(t, u, c)
	if sum != 7*n {
		t.Fatalf("SUM(xp_events) 應為 %d,得到 %d", 7*n, sum)
	}
	if xpVal != sum {
		t.Fatalf("投影不一致:user_xp.xp=%d,SUM(events)=%d", xpVal, sum)
	}
}

// 冷卻:期內第二筆被攔(不入帳、不重置計時),冷卻過後可再入帳。
func TestCooldown(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"cooldown_seconds":60}}}`)

	if res := award(t, u, c, 10, "discord_message"); res.Awarded != 10 {
		t.Fatalf("第一筆應入帳: %+v", res)
	}
	res := award(t, u, c, 10, "discord_message")
	if !res.OnCooldown || res.Awarded != 0 {
		t.Fatalf("冷卻期內第二筆應被攔: %+v", res)
	}
	if n := countEvents(t, u, c); n != 1 {
		t.Fatalf("被攔的嘗試不應留下事件,得到 %d 列", n)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 10 {
		t.Fatalf("被攔後 xp 應維持 10,得到 %d", xpVal)
	}

	// 冷卻過後(把該 source 的事件倒回 61 秒前,不用真的等)可入帳。
	// 冷卻計時器 = 該 source 最近一筆 xp_events,不是 user_xp.last_xp_at
	backdateEvents(t, u, c, "discord_message", 61)
	if res := award(t, u, c, 10, "discord_message"); res.OnCooldown || res.Awarded != 10 {
		t.Fatalf("冷卻過後應可入帳: %+v", res)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 20 {
		t.Fatalf("冷卻過後 xp 應為 20,得到 %d", xpVal)
	}
}

// 冷卻 + 併發:同時打 10 筆,row lock 串行化後恰好 1 筆入帳,其餘全數冷卻攔下。
func TestCooldownConcurrent(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"cooldown_seconds":60}}}`)

	const n = 10
	var wg sync.WaitGroup
	results := make([]*xp.AwardResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.Award(context.Background(), xp.AwardParams{
				UserID: u, CommunityID: c, Source: "discord_message", Amount: 10,
			})
		}(i)
	}
	wg.Wait()

	landed := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i].Awarded > 0 {
			landed++
		} else if !results[i].OnCooldown {
			t.Fatalf("goroutine %d 未入帳也未標記冷卻: %+v", i, results[i])
		}
	}
	if landed != 1 {
		t.Fatalf("併發下應恰好 1 筆入帳,得到 %d", landed)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 10 {
		t.Fatalf("xp 應為 10,得到 %d", xpVal)
	}
}

// daily_cap 邊界:裁到 cap 部分入帳(對玩家友善,見 service.go 註解),
// 滿了之後整筆攔下且不留事件;入帳總量恰好收斂在 cap。
func TestDailyCapBoundary(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"daily_cap":100}}}`)

	if res := award(t, u, c, 60, "discord_message"); res.Awarded != 60 || res.Capped {
		t.Fatalf("cap 內第一筆應全額入帳: %+v", res)
	}
	// 60 + 60 > 100:裁到剩餘 40 部分入帳
	res := award(t, u, c, 60, "discord_message")
	if res.Awarded != 40 || !res.Capped {
		t.Fatalf("第二筆應裁到 40 且標記 Capped: %+v", res)
	}
	// 已滿:整筆攔下,不留事件
	res = award(t, u, c, 10, "discord_message")
	if res.Awarded != 0 || !res.Capped {
		t.Fatalf("cap 滿後應攔下: %+v", res)
	}
	if n := countEvents(t, u, c); n != 2 {
		t.Fatalf("xp_events 應只有 2 列(攔下的不落地),得到 %d", n)
	}
	if sum := sumEvents(t, u, c); sum != 100 {
		t.Fatalf("入帳總量應恰為 cap=100,得到 %d", sum)
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 100 {
		t.Fatalf("user_xp.xp 應為 100,得到 %d", xpVal)
	}

	// cap 只管這個 source:其他 source 不受影響
	if res := award(t, u, c, 5, "discord_voice"); res.Awarded != 5 || res.Capped {
		t.Fatalf("其他 source 不應被 discord_message 的 cap 波及: %+v", res)
	}
}

// daily_cap + 併發:20 筆 ×10 同時打,cap=50,總入帳恰為 50。
func TestDailyCapConcurrent(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"daily_cap":50}}}`)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Award(context.Background(), xp.AwardParams{
				UserID: u, CommunityID: c, Source: "discord_message", Amount: 10,
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if sum := sumEvents(t, u, c); sum != 50 {
		t.Fatalf("併發下入帳總量應恰為 cap=50,得到 %d", sum)
	}
	xpVal, _ := projectedXP(t, u, c)
	if xpVal != 50 {
		t.Fatalf("投影應為 50,得到 %d", xpVal)
	}
}

// source 防護:不存在與 disabled 都被拒絕(先於 FK 給出可讀錯誤)。
func TestSourceValidation(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")

	_, err := svc.Award(context.Background(), xp.AwardParams{
		UserID: u, CommunityID: c, Source: "no_such_source", Amount: 10,
	})
	if !errors.Is(err, xp.ErrUnknownSource) {
		t.Fatalf("不存在的 source 應回 ErrUnknownSource,得到 %v", err)
	}

	// 停用的 source:插測試專用 key(不動 seed 的三筆,避免影響並行測試)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO platform.xp_event_types (key, name, enabled)
		 VALUES ('test_disabled', '測試停用', false) ON CONFLICT (key) DO NOTHING`); err != nil {
		t.Fatalf("插停用 source: %v", err)
	}
	_, err = svc.Award(context.Background(), xp.AwardParams{
		UserID: u, CommunityID: c, Source: "test_disabled", Amount: 10,
	})
	if !errors.Is(err, xp.ErrSourceDisabled) {
		t.Fatalf("停用的 source 應回 ErrSourceDisabled,得到 %v", err)
	}
	if n := countEvents(t, u, c); n != 0 {
		t.Fatalf("被拒的請求不應留下事件,得到 %d 列", n)
	}
}

// 參數與社群檢查。
func TestInvalidParams(t *testing.T) {
	setup(t)
	u := newUser(t)

	cases := []xp.AwardParams{
		{UserID: 0, CommunityID: 1, Source: "discord_message", Amount: 10},
		{UserID: u, CommunityID: 0, Source: "discord_message", Amount: 10},
		{UserID: u, CommunityID: 1, Source: "", Amount: 10},
		{UserID: u, CommunityID: 1, Source: "discord_message", Amount: 0},
		{UserID: u, CommunityID: 999999999, Source: "discord_message", Amount: 10}, // 社群不存在
	}
	for i, p := range cases {
		if _, err := svc.Award(context.Background(), p); !errors.Is(err, xp.ErrInvalidParams) {
			t.Fatalf("case %d 應回 ErrInvalidParams,得到 %v", i, err)
		}
	}
}

// admin 來源可為負(手動增減,schemas/06 增補 E 的 seed 說明);投影跟著減。
func TestAdminNegativeAdjustment(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")

	award(t, u, c, 100, "discord_message")
	res := award(t, u, c, -30, "admin")
	if res.Awarded != -30 || res.XP != 70 {
		t.Fatalf("負數調整結果不符: %+v", res)
	}
	if sum := sumEvents(t, u, c); sum != 70 {
		t.Fatalf("SUM 應為 70,得到 %d", sum)
	}
}

// Rebuild:投影可重算是全域慣例 —— 弄壞投影後重算 == SUM(事實)。
func TestRebuild(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")
	ctx := context.Background()

	award(t, u, c, 10, "discord_message")
	award(t, u, c, 20, "discord_voice")
	award(t, u, c, 5, "admin")

	// 人為弄壞投影(繞過服務直接改 —— 僅測試允許,user_xp 不是帳本)
	if _, err := pool.Exec(ctx,
		`UPDATE platform.user_xp SET xp = xp + 999, last_xp_at = NULL
		 WHERE user_id=$1 AND community_id=$2`, u, c); err != nil {
		t.Fatalf("弄壞投影: %v", err)
	}

	total, err := svc.Rebuild(ctx, u, c)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if total != 35 {
		t.Fatalf("Rebuild 應回 35,得到 %d", total)
	}
	xpVal, level := projectedXP(t, u, c)
	if xpVal != 35 {
		t.Fatalf("重算後 user_xp.xp 應為 35,得到 %d", xpVal)
	}
	if level != 0 {
		t.Fatalf("Rebuild 不應動 level,得到 %d", level)
	}
	var lastXpAt *string
	_ = pool.QueryRow(ctx,
		`SELECT last_xp_at::text FROM platform.user_xp WHERE user_id=$1 AND community_id=$2`, u, c,
	).Scan(&lastXpAt)
	if lastXpAt == nil {
		t.Fatal("Rebuild 應把 last_xp_at 重建為 MAX(created_at)")
	}

	// 沒有任何事件的組合:Rebuild 建列並歸零
	u2 := newUser(t)
	total, err = svc.Rebuild(ctx, u2, c)
	if err != nil {
		t.Fatalf("空 rebuild: %v", err)
	}
	if total != 0 {
		t.Fatalf("無事件的 Rebuild 應回 0,得到 %d", total)
	}
}

// backdateEvents 把該 source 的事件時間倒轉 n 秒,模擬冷卻經過(不用真的 sleep)。
func backdateEvents(t *testing.T, userID, communityID int64, source string, seconds int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE platform.xp_events
		 SET created_at = created_at - make_interval(secs => $4)
		 WHERE user_id=$1 AND community_id=$2 AND source=$3`,
		userID, communityID, source, seconds); err != nil {
		t.Fatalf("倒轉 xp_events.created_at: %v", err)
	}
}

// 冷卻按 source 各自計時:voice(無冷卻)高頻入帳不影響 message 的冷卻節奏(QA 中1)。
func TestCooldownPerSource(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"cooldown_seconds":60}}}`)

	if res := award(t, u, c, 10, "discord_message"); res.Awarded != 10 {
		t.Fatalf("message 第一筆應入帳: %+v", res)
	}
	// voice 高頻入帳(無冷卻,全數落地)——舊設計會不斷重置共用計時器
	for i := 0; i < 3; i++ {
		if res := award(t, u, c, 2, "discord_voice"); res.Awarded != 2 {
			t.Fatalf("voice 第 %d 筆應入帳: %+v", i, res)
		}
	}
	// message 仍在自己的冷卻窗內
	if res := award(t, u, c, 10, "discord_message"); !res.OnCooldown {
		t.Fatalf("message 冷卻窗內應被攔: %+v", res)
	}
	// message 自己的冷卻到期;到期後又有新的 voice 入帳(共用計時器的話會再被餓死)
	backdateEvents(t, u, c, "discord_message", 61)
	for i := 0; i < 2; i++ {
		if res := award(t, u, c, 2, "discord_voice"); res.Awarded != 2 {
			t.Fatalf("voice 補刀第 %d 筆應入帳: %+v", i, res)
		}
	}
	res := award(t, u, c, 10, "discord_message")
	if res.OnCooldown || res.Awarded != 10 {
		t.Fatalf("message 冷卻照自己的節奏到期,應可入帳: %+v", res)
	}
}

// admin 修正不重置其他 source 的冷卻計時(QA 中1 的另一半)。
func TestCooldownUnaffectedByAdmin(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"cooldown_seconds":60}}}`)

	award(t, u, c, 10, "discord_message")
	backdateEvents(t, u, c, "discord_message", 61) // message 冷卻已到期
	// 剛發生的 admin 負數修正 —— 舊設計會重置 last_xp_at 把 message 再攔 60 秒
	if res := award(t, u, c, -3, "admin"); res.Awarded != -3 {
		t.Fatalf("admin 修正應入帳: %+v", res)
	}
	res := award(t, u, c, 10, "discord_message")
	if res.OnCooldown || res.Awarded != 10 {
		t.Fatalf("admin 修正不應重置 message 冷卻: %+v", res)
	}
}

// 負數 Amount 僅限 admin(QA 中2):其他 source 收負數回 ErrInvalidParams、零落地。
func TestNegativeAmountOnlyAdmin(t *testing.T) {
	setup(t)
	u, c := newUser(t), newCommunity(t, "")

	for _, src := range []string{"discord_message", "discord_voice"} {
		_, err := svc.Award(context.Background(), xp.AwardParams{
			UserID: u, CommunityID: c, Source: src, Amount: -10,
		})
		if !errors.Is(err, xp.ErrInvalidParams) {
			t.Fatalf("source %q 的負數應回 ErrInvalidParams,得到 %v", src, err)
		}
	}
	if n := countEvents(t, u, c); n != 0 {
		t.Fatalf("被拒的負數不應留下事件,得到 %d 列", n)
	}
}

// daily_cap = 0 → 不設限(「> 0 才啟用」語意,schemas/01 A:缺省 = 不設限)。
func TestDailyCapZeroMeansUnlimited(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":{"discord_message":{"xp":10,"daily_cap":0}}}`)

	for i := 0; i < 3; i++ {
		res := award(t, u, c, 1000, "discord_message")
		if res.Awarded != 1000 || res.Capped {
			t.Fatalf("cap=0 不應設限,第 %d 筆: %+v", i, res)
		}
	}
	if xpVal, _ := projectedXP(t, u, c); xpVal != 3000 {
		t.Fatalf("xp 應為 3000,得到 %d", xpVal)
	}
}

// config JSONB 形狀不符:Award 回 error(不 panic)、不落任何事件。
func TestMalformedConfig(t *testing.T) {
	setup(t)
	u := newUser(t)
	c := newCommunity(t, `{"sources":[1,2,3]}`) // 合法 JSON、非法形狀

	_, err := svc.Award(context.Background(), xp.AwardParams{
		UserID: u, CommunityID: c, Source: "discord_message", Amount: 10,
	})
	if err == nil {
		t.Fatal("形狀不符的 config 應回 error")
	}
	if n := countEvents(t, u, c); n != 0 {
		t.Fatalf("config 壞掉時不應落任何事件,得到 %d 列", n)
	}
}
