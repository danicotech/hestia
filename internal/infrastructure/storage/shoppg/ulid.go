package shoppg

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"
)

// crockford 是 ULID 規格的 Crockford base32 字母表(排除 I L O U)。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID 產生 26 字元的 ULID(48-bit 毫秒時間戳 + 80-bit 亂數,Crockford base32)。
// 對外 public_id 規則是 ULID(專案鐵則 5);repo 尚無 ULID 依賴,
// 為單一 id 生成不值得加第三方套件,這裡用標準庫實作規格。
func newULID() (string, error) {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[5-i] = byte(ms >> (8 * i))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("讀亂數: %w", err)
	}
	n := new(big.Int).SetBytes(b[:])
	base := big.NewInt(32)
	mod := new(big.Int)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = crockford[mod.Int64()]
	}
	return string(out), nil
}
