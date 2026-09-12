package tournament

import (
	"context"
	"errors"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
)

func validCreate() CreateParams {
	return CreateParams{
		CommunityPublicID: "01J0COMMUNITY",
		Slug:              "2026-baiye-shifeng",
		Name:              "百業試鋒 2026 春季賽",
		ActorUserID:       7,
	}
}

// 建出來的賽事必須停在報名期,而且讓武目錄要跟著進去 ——
// 這兩件事是 Create 存在的全部理由。
func TestCreate_DefaultsAndCatalogue(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo)

	res, err := svc.Create(context.Background(), validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Tournament.Phase != PhaseSignup {
		t.Errorf("phase = %s,新賽事必須停在報名期", res.Tournament.Phase)
	}
	if len(repo.createCalls) != 1 {
		t.Fatalf("repo.Create 被呼叫 %d 次,想要 1 次", len(repo.createCalls))
	}
	got := repo.createCalls[0]

	want, err := handicap.SeedItems()
	if err != nil {
		t.Fatalf("SeedItems: %v", err)
	}
	if len(got.HandicapItems) != len(want) {
		t.Fatalf("交給 repo 的讓武項目 %d 項,目錄有 %d 項", len(got.HandicapItems), len(want))
	}
	if res.HandicapItemCount != len(want) {
		t.Errorf("回報的項目數 = %d,實際交出去 %d 項", res.HandicapItemCount, len(want))
	}
	// 逐項比對:少一項或價格錯一個,場上就會出現一個沒有人講得清楚的限制。
	for i := range want {
		if got.HandicapItems[i] != want[i] {
			t.Fatalf("第 %d 項與目錄不符:%+v / %+v", i, got.HandicapItems[i], want[i])
		}
	}

	// 沒填任何旋鈕 = 全部預設,而且寫進 config 的位元組要能解回同一份。
	if res.Config.BPPerRankGap != DefaultBPPerRankGap {
		t.Errorf("bp_per_rank_gap = %d,想要預設 %d", res.Config.BPPerRankGap, DefaultBPPerRankGap)
	}
	if res.Config.Odds.VigBPS != DefaultVigBPS {
		t.Errorf("vig_bps = %d,想要預設 %d", res.Config.Odds.VigBPS, DefaultVigBPS)
	}
	// 獎金未定案,預設全 0(季軍獎金 > 0 會被 prize 套件拒發,那是刻意的)。
	if res.Config.Prizes != (Prizes{}) {
		t.Errorf("預設獎金 = %+v,想要全 0", res.Config.Prizes)
	}
	parsed, err := ParseConfig(got.ConfigRaw)
	if err != nil {
		t.Fatalf("寫出去的 config 解不回來: %v", err)
	}
	assertSameConfig(t, parsed, res.Config)
}

// 旋鈕有填就要照填的走,而 vig_bps = 0 必須是「不抽水」而不是「沒填」。
func TestCreate_Overrides(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo)

	zero := int64(0)
	gap := int64(12)
	qty := int32(3)
	p := validCreate()
	p.SignupBonus = 500
	p.Config = ConfigOverrides{
		BPPerRankGap:       &gap,
		VigBPS:             &zero,
		Prizes:             &Prizes{Champion: 5000, RunnerUp: 2000},
		HandicapItemMaxQty: &qty,
	}

	res, err := svc.Create(context.Background(), p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Config.BPPerRankGap != 12 {
		t.Errorf("bp_per_rank_gap = %d,want 12", res.Config.BPPerRankGap)
	}
	if res.Config.Odds.VigBPS != 0 {
		t.Errorf("vig_bps = %d,want 0(填 0 是不抽水,不是沒填)", res.Config.Odds.VigBPS)
	}
	// 沒填的那些仍然是預設。
	if res.Config.Odds.MinOddsMilli != DefaultMinOddsMilli {
		t.Errorf("min_odds_milli = %d,want %d", res.Config.Odds.MinOddsMilli, DefaultMinOddsMilli)
	}
	if res.Config.Prizes.Champion != 5000 || res.Config.Prizes.Third != 0 {
		t.Errorf("獎金 = %+v", res.Config.Prizes)
	}
	if res.Config.HandicapItemMaxQty == nil || *res.Config.HandicapItemMaxQty != 3 {
		t.Errorf("handicap_item_max_qty = %v,want 3", res.Config.HandicapItemMaxQty)
	}
	if repo.createCalls[0].SignupBonus != 500 {
		t.Errorf("signup_bonus = %d,want 500", repo.createCalls[0].SignupBonus)
	}
}

// 壞掉的旋鈕在建立時是**致命的**:替裁判猜一個值,等於開了一屆
// 規則與他以為的不一樣的賽事,而那要到第一場算 BP 時才有人看得出來。
func TestCreate_BadConfigIsFatal(t *testing.T) {
	neg := int64(-1)
	bigMin := int64(99999)
	cases := map[string]ConfigOverrides{
		"bp_per_rank_gap 為負":       {BPPerRankGap: &neg},
		"vig_bps 為負":               {VigBPS: &neg},
		"下限高於上限":                   {MinOddsMilli: &bigMin},
		"獎金為負":                     {Prizes: &Prizes{Champion: -1}},
		"handicap_item_max_qty 為負": {HandicapItemMaxQty: ptrOf(int32(-1))},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{}
			p := validCreate()
			p.Config = overrides
			_, err := NewService(repo).Create(context.Background(), p)
			if !errors.Is(err, ErrConfigMalformed) {
				t.Fatalf("err = %v,想要 ErrConfigMalformed", err)
			}
			if len(repo.createCalls) != 0 {
				t.Fatal("設定不合法時不該碰資料庫")
			}
		})
	}
}

// 必填欄位與稽核身分:缺了就不該走到 repo。
func TestCreate_RejectsIncompleteRequests(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CreateParams)
		want   error
	}{
		"沒有裁判身分": {func(p *CreateParams) { p.ActorUserID = 0 }, ErrActorRequired},
		"沒有名稱":   {func(p *CreateParams) { p.Name = "   " }, ErrInvalidRequest},
		"沒有社群":   {func(p *CreateParams) { p.CommunityPublicID = "" }, ErrInvalidRequest},
		"報名獎勵為負": {func(p *CreateParams) { p.SignupBonus = -1 }, ErrInvalidRequest},
		"代號不合法":  {func(p *CreateParams) { p.Slug = "2026--baiye" }, ErrInvalidSlug},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{}
			p := validCreate()
			c.mutate(&p)
			_, err := NewService(repo).Create(context.Background(), p)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v,想要 %v", err, c.want)
			}
			if len(repo.createCalls) != 0 {
				t.Fatal("請求不合法時不該碰資料庫")
			}
		})
	}
}

// repo 的錯誤要原樣透傳:ErrSlugTaken 被包成別的東西,呼叫端就分不出
// 「換一個代號」與「格式錯了」。
func TestCreate_RepoErrorPassesThrough(t *testing.T) {
	repo := &fakeRepo{createErr: ErrSlugTaken}
	_, err := NewService(repo).Create(context.Background(), validCreate())
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("err = %v,想要 ErrSlugTaken", err)
	}
}

func TestValidateSlug(t *testing.T) {
	ok := map[string]string{
		"2026-baiye-shifeng": "2026-baiye-shifeng",
		"  2026-Baiye  ":     "2026-baiye", // 去空白 + 轉小寫是打字的意外,不必拒絕
		"abc":                "abc",
		"2026":               "2026",
	}
	for in, want := range ok {
		got, err := ValidateSlug(in)
		if err != nil {
			t.Errorf("ValidateSlug(%q) 回錯誤 %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateSlug(%q) = %q,want %q", in, got, want)
		}
	}

	// 拒絕的一律拒絕,不「幫他修掉」—— 靜靜改寫的話,他存下來的網址會是
	// 打不開的那一個。
	bad := []string{
		"", "ab", "-2026", "2026-", "2026--baiye", "2026_baiye",
		"2026 baiye", "賽事", "a/b",
	}
	for _, in := range bad {
		if got, err := ValidateSlug(in); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("ValidateSlug(%q) = %q, %v,想要 ErrInvalidSlug", in, got, err)
		}
	}
}

func ptrOf[T any](v T) *T { return &v }

// assertSameConfig 比對兩份設定的每一個欄位(Ranks 是切片,不能直接 ==)。
func assertSameConfig(t *testing.T, got, want Config) {
	t.Helper()
	if got.BPPerRankGap != want.BPPerRankGap {
		t.Errorf("bp_per_rank_gap = %d,want %d", got.BPPerRankGap, want.BPPerRankGap)
	}
	if got.Odds != want.Odds {
		t.Errorf("odds = %+v,want %+v", got.Odds, want.Odds)
	}
	if got.Prizes != want.Prizes {
		t.Errorf("prizes = %+v,want %+v", got.Prizes, want.Prizes)
	}
	switch {
	case got.HandicapItemMaxQty == nil && want.HandicapItemMaxQty == nil:
	case got.HandicapItemMaxQty == nil || want.HandicapItemMaxQty == nil:
		t.Errorf("handicap_item_max_qty = %v,want %v", got.HandicapItemMaxQty, want.HandicapItemMaxQty)
	case *got.HandicapItemMaxQty != *want.HandicapItemMaxQty:
		t.Errorf("handicap_item_max_qty = %d,want %d", *got.HandicapItemMaxQty, *want.HandicapItemMaxQty)
	}
	if len(got.Ranks) != len(want.Ranks) {
		t.Fatalf("段位 %d 段,want %d 段", len(got.Ranks), len(want.Ranks))
	}
	for i := range want.Ranks {
		if got.Ranks[i] != want.Ranks[i] {
			t.Errorf("第 %d 段 = %+v,want %+v", i, got.Ranks[i], want.Ranks[i])
		}
	}
}
