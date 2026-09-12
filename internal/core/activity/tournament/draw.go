package tournament

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strconv"
)

// DrawSeed 是一次抽籤的亂數種子。
//
// # 為什麼要把種子存下來
//
// 抽籤是這場活動裡最容易被質疑的一步:任何一張對戰表都可以被說成「喬過的」。
// 程式自證清白的唯一辦法是**可重現** —— 把種子公開,任何人拿同一份參賽名單
// 重跑一次就會得到位元相同的結果。
//
// 這也是為什麼種子用 crypto/rand 產生而洗牌用 math/rand/v2:
// 種子必須不可預測(可預測就等於可以挑一個自己喜歡的結果),
// 但由種子推出對戰表的那一步必須是完全確定的,否則重現不了。
type DrawSeed uint64

// NewDrawSeed 產生一個不可預測的種子。
//
// crypto/rand 失敗一律回錯,**絕不降級**到時間戳或 math/rand:
// 種子可預測時,一個知道演算法的人可以反覆試到自己想要的籤表再去按抽籤,
// 而事後看起來完全合規 —— 這種失效沒有任何痕跡。
func NewDrawSeed() (DrawSeed, error) {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("產生抽籤種子: %w", err)
	}
	return DrawSeed(binary.BigEndian.Uint64(b[:])), nil
}

// String 是種子的呈現形式:固定 16 位小寫 hex。
//
// 固定寬度是刻意的 —— 變寬的話「0x5」與「0x05」會是同一個種子的兩種寫法,
// 而稽核紀錄裡的字串是拿來比對的,同一件事有兩種寫法就比不出來。
func (s DrawSeed) String() string {
	return fmt.Sprintf("%016x", uint64(s))
}

// ParseDrawSeed 把 String() 的輸出讀回來,供裁判重現某一次抽籤。
func ParseDrawSeed(s string) (DrawSeed, error) {
	if len(s) != 16 {
		return 0, fmt.Errorf("%w: 長度必須是 16,得到 %d", ErrInvalidSeed, len(s))
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q 不是 16 進位", ErrInvalidSeed, s)
	}
	return DrawSeed(v), nil
}

// Rand 把種子展開成 bracket.Build 要的亂數來源。
//
// PCG 需要兩個 uint64 的狀態,而種子只有一個。用 splitmix64 推出第二個,
// 而不是拿 0 去補:全零的 state 會讓 PCG 的前幾個輸出品質明顯變差,
// 而抽籤只用得到前幾個輸出(洗一次牌就結束了)。
func (s DrawSeed) Rand() *rand.Rand {
	//nolint:gosec // G404 在這裡是刻意的:抽籤必須**可重現**,而可重現與
	// 密碼學亂數互斥。不可預測性由種子本身負責 —— NewDrawSeed 走 crypto/rand,
	// 這一步只是把種子確定性地展開成一個洗牌序列。
	return rand.New(rand.NewPCG(uint64(s), splitmix64(uint64(s))))
}

// splitmix64 是 SplitMix64 的單步混合,用來從一個種子推出第二個不相關的值。
// 常數取自原始論文,不要改。
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
