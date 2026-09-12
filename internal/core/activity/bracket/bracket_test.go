package bracket

import (
	"errors"
	"math/rand/v2"
	"testing"
)

// newRand 給固定種子,讓失敗可以重現。
func newRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed*2+1))
}

func players(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}

// TestBuildInvariants 對 1..64 人逐一驗證單淘汰的結構性質。
//
// 這是本 package 最重要的測試。人數不是 2 的冪次時的輪空處理,
// 是那種「人工看一張 32 人的表看不出錯」的地方 —— 只能靠不變式。
func TestBuildInvariants(t *testing.T) {
	t.Parallel()

	for n := 1; n <= 64; n++ {
		t.Run(name(n), func(t *testing.T) {
			t.Parallel()

			b, err := Build(players(n), newRand(uint64(n)))
			if err != nil {
				t.Fatalf("%d 人建樹失敗: %v", n, err)
			}

			// ① 場次總數恆為 N−1。每場淘汰一人,從 N 人選出 1 人就要淘汰 N−1 人。
			if got := b.MatchCount(); got != n-1 {
				t.Errorf("%d 人應有 %d 場,得到 %d", n, n-1, got)
			}

			// ② 每個人都要有籤位,而且籤位不重複。
			if len(b.Seeds) != n {
				t.Errorf("籤位數 %d,應為 %d", len(b.Seeds), n)
			}
			usedSeed := make(map[int]int64, n)
			for id, seed := range b.Seeds {
				if seed < 1 || seed > n {
					t.Errorf("選手 %d 的籤位 %d 超出 1..%d", id, seed, n)
				}
				if prev, dup := usedSeed[seed]; dup {
					t.Errorf("籤位 %d 同時給了 %d 與 %d", seed, prev, id)
				}
				usedSeed[seed] = id
			}

			// ③ 每個人在首輪恰好出現一次(打一場或輪空,不會兩者皆是)。
			//
			// n = 1 是退化情況:樹寬 1、零輪、零場,那個人直接是冠軍,
			// 既不打首輪也不算輪空。所以這條不變式從 2 人才開始成立。
			appearances := make(map[int64]int, n)
			for _, m := range b.Matches {
				if m.Round != 1 {
					continue
				}
				if m.P1 == 0 || m.P2 == 0 {
					t.Errorf("首輪 slot %d 有空位 —— 輪空不該產生比賽", m.Slot)
				}
				appearances[m.P1]++
				appearances[m.P2]++
			}
			for _, id := range b.Byes {
				appearances[id]++
			}
			if n >= 2 {
				for _, id := range players(n) {
					if appearances[id] != 1 {
						t.Errorf("選手 %d 在首輪出現 %d 次,應為 1 次", id, appearances[id])
					}
				}
			}

			// ④ 輪空人數 = 補齊到 2 的冪次的差額。
			wantByes := b.Size - n
			if len(b.Byes) != wantByes {
				t.Errorf("輪空 %d 人,應為 %d 人", len(b.Byes), wantByes)
			}

			// ⑤ Size 是 >= n 的最小 2 的冪次。
			if b.Size < n || b.Size&(b.Size-1) != 0 {
				t.Errorf("Size %d 不是 >= %d 的 2 的冪次", b.Size, n)
			}
			if n > 1 && b.Size/2 >= n {
				t.Errorf("Size %d 對 %d 人來說過大", b.Size, n)
			}

			// ⑥ (Round, Slot) 不重複 —— 對應 DB 的 UNIQUE 約束。
			seen := make(map[[2]int]bool, len(b.Matches))
			for _, m := range b.Matches {
				k := [2]int{m.Round, m.Slot}
				if seen[k] {
					t.Errorf("(round=%d, slot=%d) 重複", m.Round, m.Slot)
				}
				seen[k] = true
			}

			// ⑦ 沒有人自己打自己。
			for _, m := range b.Matches {
				if m.P1 != 0 && m.P1 == m.P2 {
					t.Errorf("(round=%d, slot=%d) 選手 %d 自己打自己", m.Round, m.Slot, m.P1)
				}
			}
		})
	}
}

// TestBuildOnePlayer 確認只有一人報名時不會炸,也不會產生任何比賽。
func TestBuildOnePlayer(t *testing.T) {
	t.Parallel()

	b, err := Build(players(1), newRand(1))
	if err != nil {
		t.Fatalf("一人建樹失敗: %v", err)
	}
	if b.MatchCount() != 0 {
		t.Errorf("一個人不該有比賽,得到 %d 場", b.MatchCount())
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	t.Parallel()

	if _, err := Build(nil, newRand(1)); !errors.Is(err, ErrNoPlayers) {
		t.Errorf("空名單該回 ErrNoPlayers,得到 %v", err)
	}
	if _, err := Build([]int64{1, 2, 1}, newRand(1)); err == nil {
		t.Error("重複的參賽者該被擋下")
	}
	if _, err := Build([]int64{1, 0}, newRand(1)); err == nil {
		t.Error("id 為 0 該被擋下")
	}
}

// TestBuildDeterministic 確認同一個種子排出同一張表。
//
// 裁判需要向人證明「這張表確實是這個種子抽出來的」時,靠的就是這個性質。
func TestBuildDeterministic(t *testing.T) {
	t.Parallel()

	a, err := Build(players(13), newRand(42))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Build(players(13), newRand(42))
	if err != nil {
		t.Fatal(err)
	}

	if len(a.Matches) != len(c.Matches) {
		t.Fatalf("場次數不同: %d vs %d", len(a.Matches), len(c.Matches))
	}
	for i := range a.Matches {
		if a.Matches[i] != c.Matches[i] {
			t.Errorf("第 %d 場不同: %+v vs %+v", i, a.Matches[i], c.Matches[i])
		}
	}
	for id, seed := range a.Seeds {
		if c.Seeds[id] != seed {
			t.Errorf("選手 %d 的籤位不同: %d vs %d", id, seed, c.Seeds[id])
		}
	}
}

// TestBuildDoesNotMutateInput 確認抽籤不會洗亂呼叫端的切片。
func TestBuildDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	in := players(16)
	before := make([]int64, len(in))
	copy(before, in)

	if _, err := Build(in, newRand(7)); err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("輸入切片被改動:位置 %d 從 %d 變成 %d", i, before[i], in[i])
		}
	}
}

// TestByesLandInRoundTwo 確認輪空者確實出現在第二輪的位置上,而不是憑空消失。
func TestByesLandInRoundTwo(t *testing.T) {
	t.Parallel()

	// 5 人 → 樹寬 8 → 3 個輪空。
	b, err := Build(players(5), newRand(99))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Byes) != 3 {
		t.Fatalf("應有 3 個輪空,得到 %d", len(b.Byes))
	}

	inRoundTwo := make(map[int64]bool)
	for _, m := range b.Matches {
		if m.Round != 2 {
			continue
		}
		if m.P1 != 0 {
			inRoundTwo[m.P1] = true
		}
		if m.P2 != 0 {
			inRoundTwo[m.P2] = true
		}
	}
	for _, id := range b.Byes {
		if !inRoundTwo[id] {
			t.Errorf("輪空者 %d 沒有出現在第二輪", id)
		}
	}
}

// TestAdvanceChain 模擬一整場賽事打到底,確認晉級鏈不會斷。
//
// 這是最接近真實使用的測試:從首輪一路推到決賽,每一步都讓 P1 獲勝,
// 最後必須剛好剩一個冠軍。
func TestAdvanceChain(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 3, 5, 7, 8, 11, 16, 23, 32} {
		t.Run(name(n), func(t *testing.T) {
			t.Parallel()

			b, err := Build(players(n), newRand(uint64(n)*17))
			if err != nil {
				t.Fatal(err)
			}

			// 以 (round,slot) 索引,方便邊推邊填。
			idx := make(map[[2]int]*Match, len(b.Matches))
			for i := range b.Matches {
				idx[[2]int{b.Matches[i].Round, b.Matches[i].Slot}] = &b.Matches[i]
			}

			var champion int64
			if n == 1 {
				return
			}

			played := 0
			for round := 1; round <= b.TotalRounds; round++ {
				for slot := 0; slot < b.Size>>round; slot++ {
					m := idx[[2]int{round, slot}]
					if m == nil {
						continue // 首輪的輪空位置沒有比賽
					}
					if m.P1 == 0 || m.P2 == 0 {
						t.Fatalf("(round=%d slot=%d) 開打時仍有空位: P1=%d P2=%d",
							round, slot, m.P1, m.P2)
					}
					winner := m.P1
					played++

					nr, ns, isP1, ok := b.Advance(round, slot)
					if !ok {
						champion = winner
						continue
					}
					next := idx[[2]int{nr, ns}]
					if next == nil {
						t.Fatalf("(round=%d slot=%d) 的晉級目標 (round=%d slot=%d) 不存在",
							round, slot, nr, ns)
					}
					if isP1 {
						next.P1 = winner
					} else {
						next.P2 = winner
					}
				}
			}

			if played != n-1 {
				t.Errorf("實際打了 %d 場,應為 %d 場", played, n-1)
			}
			if champion == 0 {
				t.Error("打完沒有產生冠軍")
			}
		})
	}
}

// TestAdvanceFinalHasNoNext 確認決賽之後沒有下一場。
func TestAdvanceFinalHasNoNext(t *testing.T) {
	t.Parallel()

	b, err := Build(players(8), newRand(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := b.Advance(b.TotalRounds, 0); ok {
		t.Error("決賽不該有下一場")
	}
	if _, _, _, ok := b.Advance(0, 0); ok {
		t.Error("round 0 不合法,不該回 ok")
	}
}

func TestRoundLabel(t *testing.T) {
	t.Parallel()

	b, err := Build(players(16), newRand(5))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "十六強", 2: "八強", 3: "四強", 4: "決賽"}
	for round, label := range want {
		if got := b.RoundLabel(round); got != label {
			t.Errorf("16 人賽第 %d 輪 = %q, 要 %q", round, got, label)
		}
	}
	if got := b.RoundLabel(99); got != "" {
		t.Errorf("超出範圍的輪次該回空字串,得到 %q", got)
	}
}

// TestRoundLabelDependsOnSize 釘住「同一個 round 編號在不同規模意義不同」。
//
// 這正是 label 由伺服器算而不是前端寫死的理由。
func TestRoundLabelDependsOnSize(t *testing.T) {
	t.Parallel()

	small, err := Build(players(8), newRand(1))
	if err != nil {
		t.Fatal(err)
	}
	large, err := Build(players(32), newRand(1))
	if err != nil {
		t.Fatal(err)
	}
	if small.RoundLabel(1) != "八強" {
		t.Errorf("8 人賽第 1 輪 = %q, 要「八強」", small.RoundLabel(1))
	}
	if large.RoundLabel(1) != "三十二強" {
		t.Errorf("32 人賽第 1 輪 = %q, 要「三十二強」", large.RoundLabel(1))
	}
}

func name(n int) string {
	return string(rune('0'+n/10)) + string(rune('0'+n%10)) + "人"
}
