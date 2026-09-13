package handicap

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// 封盤時的系統抽選(grill Q5 / Q13)。
//
// 抽選發生在**封盤那一刻、同一個 tx**:封盤不可逆 ⇒ 抽選結果不可逆,
// 封盤公示因此是完整的 —— 不會出現「買了隨機武學但是哪一個還不知道」。
// 抽不出來(池空)整個封盤失敗,而不是封了盤留一個空白給裁判現場補抽。
//
// 兩種抽法的差別在候選清單從哪來:
//
//	direction_keys  固定四鍵,清單在程式裡(排除恆等排列後 23 種)
//	wuxue           清單在賽事 config(handicap.draw_pools.wuxue),權威在 rules 套件

// randomSource 是抽選要的全部亂數能力。*rand.Rand 逐字元滿足它。
//
// 抽出來做介面而不是直接放 *rand.Rand,是為了預設值:*rand.Rand 不是
// goroutine-safe,而 matchpg 每個 tx 現做一個 Service,沒注入時得有一個
// 可以被同時用的來源 —— math/rand/v2 的頂層函式正是。
type randomSource interface {
	IntN(n int) int
}

// globalRand 走 math/rand/v2 的頂層函式:自動播種、goroutine-safe。
type globalRand struct{}

// 抽讓武不是密碼學用途:可預測性沒有價值可偷,需要的是可重放(WithRand 固定種子)。
func (globalRand) IntN(n int) int { return rand.IntN(n) } //nolint:gosec // 非安全用途

// WithRand 換掉抽選的亂數來源(與 bracket.Build 同一個做法:固定種子得到固定結果,
// 測試據此釘住輸出)。**傳入的 *rand.Rand 不是 goroutine-safe**,只該在
// 單一 Service 實例不會被並行呼叫時使用;正式路徑不注入,用預設的全域來源。
func WithRand(r *rand.Rand) Option {
	return func(s *Service) {
		if r != nil {
			s.rng = r
		}
	}
}

// directionKeys 是「打亂方向鍵」的四個鍵,順序即結果字串的順序。
var directionKeys = [4]string{"上", "下", "左", "右"}

// directionPermutations 是四鍵的全部排列**去掉恆等**(24 → 23),順序固定,
// 抽選只是在裡面取一個索引 —— 「永不恆等」由這份表保證,不靠抽到再重抽。
var directionPermutations = nonIdentityPermutations(len(directionKeys))

// nonIdentityPermutations 列出 0..n-1 的全部排列,去掉 [0,1,...,n-1] 那一個。
func nonIdentityPermutations(n int) [][]int {
	var out [][]int
	var rec func(prefix []int, rest []int)
	rec = func(prefix []int, rest []int) {
		if len(rest) == 0 {
			p := make([]int, len(prefix))
			copy(p, prefix)
			out = append(out, p)
			return
		}
		for i, v := range rest {
			next := make([]int, 0, len(rest)-1)
			next = append(next, rest[:i]...)
			next = append(next, rest[i+1:]...)
			rec(append(prefix, v), next)
		}
	}
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i
	}
	rec(nil, ids)

	kept := out[:0]
	for _, p := range out {
		if !isIdentity(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

func isIdentity(p []int) bool {
	for i, v := range p {
		if v != i {
			return false
		}
	}
	return true
}

// drawDirectionKeys 抽一個非恆等排列,回可讀形式:「上→下 下→左 左→右 右→上」
// (每段 = 原鍵→新鍵,空白分隔,順序固定為上下左右)。
func drawDirectionKeys(r randomSource) string {
	p := directionPermutations[r.IntN(len(directionPermutations))]
	parts := make([]string, len(p))
	for from, to := range p {
		parts[from] = directionKeys[from] + "→" + directionKeys[to]
	}
	return strings.Join(parts, " ")
}

// drawFromPool 從命名的池裡抽一個。池不存在或為空都是 ErrDrawPoolEmpty ——
// 對封盤來說兩者是同一件事:抽不出來。
func drawFromPool(r randomSource, pools DrawPools, name string) (string, error) {
	if pools == nil {
		return "", fmt.Errorf("%w: 沒有抽選池 %q", ErrDrawPoolEmpty, name)
	}
	pool, ok := pools.DrawPool(name)
	if !ok || len(pool) == 0 {
		return "", fmt.Errorf("%w: %q", ErrDrawPoolEmpty, name)
	}
	return pool[r.IntN(len(pool))], nil
}

// draw 依項目參數抽一次。呼叫端已確認 Draw 非空。
func (s *Service) draw(sel Selection, pools DrawPools) (string, error) {
	switch sel.ItemParams.Draw {
	case DrawDirectionKeys:
		return drawDirectionKeys(s.rng), nil
	case DrawWuxue:
		return drawFromPool(s.rng, pools, DrawWuxue) // draw 的值就是池名
	default:
		// ItemParams.Validate 已擋掉未知值;走到這裡是 adapter 沒經過驗證就塞進來。
		return "", fmt.Errorf("%w: 未知的 draw %q(selection=%s)",
			ErrInvalidItemParams, sel.ItemParams.Draw, sel.PublicID)
	}
}
