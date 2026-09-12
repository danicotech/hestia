package bp

import (
	"errors"
	"testing"
)

// TestBudgetAllRankPairs 窮舉四段位的全部 16 種組合。
//
// 這張表直接抄自使用者定的規則,不是從程式反推的:
// 每差一段 8 BP、差二段 16、差三段 24、同段 0,且**只有低段位者拿得到**。
func TestBudgetAllRankPairs(t *testing.T) {
	t.Parallel()

	// want[self][opponent],索引即 rank_level。
	want := map[Rank]map[Rank]int64{
		RankKaishan:  {RankKaishan: 0, RankDuanshui: 8, RankFeihua: 16, RankWuwo: 24},
		RankDuanshui: {RankKaishan: 0, RankDuanshui: 0, RankFeihua: 8, RankWuwo: 16},
		RankFeihua:   {RankKaishan: 0, RankDuanshui: 0, RankFeihua: 0, RankWuwo: 8},
		RankWuwo:     {RankKaishan: 0, RankDuanshui: 0, RankFeihua: 0, RankWuwo: 0},
	}

	for self := MinRank; self <= MaxRank; self++ {
		for opp := MinRank; opp <= MaxRank; opp++ {
			got, err := Budget(self, opp, DefaultPerRankGap)
			if err != nil {
				t.Fatalf("Budget(%s, %s) 回錯誤: %v", self, opp, err)
			}
			if got != want[self][opp] {
				t.Errorf("Budget(%s, %s) = %d, 要 %d", self, opp, got, want[self][opp])
			}
		}
	}
}

// TestBudgetNeverReverse 確認高段位者永遠拿不到反向補償。
//
// 這條單獨測是因為它是制度的核心承諾之一:「BP 只會由較低段位者取得,
// 高段位者不會獲得反向BP」。用不等式表達而不是查表,才擋得住日後改公式。
func TestBudgetNeverReverse(t *testing.T) {
	t.Parallel()

	for self := MinRank; self <= MaxRank; self++ {
		for opp := MinRank; opp <= MaxRank; opp++ {
			got, err := Budget(self, opp, DefaultPerRankGap)
			if err != nil {
				t.Fatal(err)
			}
			if self >= opp && got != 0 {
				t.Errorf("Budget(%s, %s) = %d,但自己不比對手低就不該有 BP", self, opp, got)
			}
			if self < opp && got <= 0 {
				t.Errorf("Budget(%s, %s) = %d,自己比對手低就該有 BP", self, opp, got)
			}
		}
	}
}

// TestBudgetRejectsUnranked 確認未評段不會被靜靜當成 0。
//
// 未評段就配對代表流程出錯(裁判還沒評完就抽籤)。若這裡回 0 而不是錯誤,
// 一場本該有 24 BP 的比賽會變成沒有讓武,而且沒有任何人會發現。
func TestBudgetRejectsUnranked(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		self     Rank
		opponent Rank
	}{
		{"雙方皆未評", RankUnspecified, RankUnspecified},
		{"自己未評", RankUnspecified, RankWuwo},
		{"對手未評", RankKaishan, RankUnspecified},
		{"超出上界", RankKaishan, Rank(5)},
		{"負數", Rank(-1), RankWuwo},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Budget(tc.self, tc.opponent, DefaultPerRankGap); !errors.Is(err, ErrUnranked) {
				t.Errorf("Budget(%d, %d) 該回 ErrUnranked,得到 %v", tc.self, tc.opponent, err)
			}
		})
	}
}

// TestBudgetPerGapOverride 確認逐屆可調,以及設定壞掉時的退路。
func TestBudgetPerGapOverride(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		perGap int64
		want   int64 // 開山 vs 無我,差三段
	}{
		{"預設 8", 8, 24},
		{"逐屆調成 10", 10, 30},
		{"調成 1", 1, 3},
		{"0 退回預設", 0, 24},
		{"負數退回預設", -5, 24},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Budget(RankKaishan, RankWuwo, tc.perGap)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("perGap=%d: Budget = %d, 要 %d", tc.perGap, got, tc.want)
			}
		})
	}
}

func TestHolder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		p1, p2     Rank
		wantBudget int64
		wantIsP1   bool
	}{
		{"p1 較低,拿 24", RankKaishan, RankWuwo, 24, true},
		{"p2 較低,拿 24", RankWuwo, RankKaishan, 24, false},
		{"p1 較低,拿 8", RankFeihua, RankWuwo, 8, true},
		{"p2 較低,拿 16", RankWuwo, RankDuanshui, 16, false},
		{"同段,無人取得", RankDuanshui, RankDuanshui, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			budget, isP1, err := Holder(tc.p1, tc.p2, DefaultPerRankGap)
			if err != nil {
				t.Fatal(err)
			}
			if budget != tc.wantBudget {
				t.Errorf("budget = %d, 要 %d", budget, tc.wantBudget)
			}
			if budget > 0 && isP1 != tc.wantIsP1 {
				t.Errorf("holderIsP1 = %v, 要 %v", isP1, tc.wantIsP1)
			}
		})
	}
}

// TestHolderSymmetry 確認交換 p1/p2 只會換持有者,不會換金額。
//
// 對戰表上誰被排在上面純屬抽籤結果,不該影響任何人拿到多少 BP。
func TestHolderSymmetry(t *testing.T) {
	t.Parallel()

	for a := MinRank; a <= MaxRank; a++ {
		for b := MinRank; b <= MaxRank; b++ {
			ab, abIsP1, err := Holder(a, b, DefaultPerRankGap)
			if err != nil {
				t.Fatal(err)
			}
			ba, baIsP1, err := Holder(b, a, DefaultPerRankGap)
			if err != nil {
				t.Fatal(err)
			}
			if ab != ba {
				t.Errorf("Holder(%s,%s)=%d 但 Holder(%s,%s)=%d,金額不該因排序而不同", a, b, ab, b, a, ba)
			}
			if ab > 0 && abIsP1 == baIsP1 {
				t.Errorf("Holder(%s,%s) 與 Holder(%s,%s) 的持有者該相反", a, b, b, a)
			}
		}
	}
}

// TestCanAfford 特別盯著「剛好花光」這個邊界 —— off-by-one 只會在這裡顯現。
func TestCanAfford(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                string
		budget, spent, cost int64
		want                bool
	}{
		{"全新預算買得起", 24, 0, 20, true},
		{"剛好花光", 24, 4, 20, true},
		{"差一點", 24, 5, 20, false},
		{"已經花光再買", 24, 24, 1, false},
		{"零成本項目不合法", 24, 0, 0, false},
		{"負成本不合法", 24, 0, -5, false},
		{"負已花費是壞資料", 24, -1, 5, false},
		{"沒有預算", 0, 0, 1, false},
		{"重複買同一項到花光", 24, 21, 3, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CanAfford(tc.budget, tc.spent, tc.cost); got != tc.want {
				t.Errorf("CanAfford(%d, %d, %d) = %v, 要 %v", tc.budget, tc.spent, tc.cost, got, tc.want)
			}
		})
	}
}

func TestRemainingNeverNegative(t *testing.T) {
	t.Parallel()

	if got := Remaining(24, 24); got != 0 {
		t.Errorf("花光時剩餘該是 0,得到 %d", got)
	}
	// 資料壞掉時(spent > budget)也不該讓前端看到負數餘額。
	if got := Remaining(24, 30); got != 0 {
		t.Errorf("超支時剩餘該夾到 0,得到 %d", got)
	}
	if got := Remaining(24, 4); got != 20 {
		t.Errorf("Remaining(24, 4) = %d, 要 20", got)
	}
}

// TestRankValid 確認段位邊界。rank_level 的 DB CHECK 是 1..4,這裡必須一致。
func TestRankValid(t *testing.T) {
	t.Parallel()

	for r := Rank(-2); r <= Rank(6); r++ {
		want := r >= 1 && r <= 4
		if got := r.Valid(); got != want {
			t.Errorf("Rank(%d).Valid() = %v, 要 %v", r, got, want)
		}
	}
}

// TestMaxPossibleBudget 釘住「最高 24 BP」這個對外承諾。
//
// 規則文案寫死了「無我對上開山,開山取得最高 24 BP」。
// 若日後有人加了第五個段位而忘了改文案,這條會先炸。
func TestMaxPossibleBudget(t *testing.T) {
	t.Parallel()

	max, err := Budget(MinRank, MaxRank, DefaultPerRankGap)
	if err != nil {
		t.Fatal(err)
	}
	if max != 24 {
		t.Errorf("最大 BP = %d,但規則文案對外承諾的是 24", max)
	}
}
