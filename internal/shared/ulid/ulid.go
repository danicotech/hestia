// Package ulid 產生對外識別子用的 ULID。
//
// 專案鐵則 5:對外 API 只出現 public_id(ULID),永不出現內部 BIGINT id。
// 產生 ULID 的地方不只一處(商店權益 / 工單 public_id、入口層 request_id),
// 所以收斂成一個套件——一個概念一個權威位置(專案第 9 條)。
//
// 為單一 id 生成不值得加第三方套件,這裡用標準庫實作規格。
package ulid

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"
)

// crockford 是 ULID 規格的 Crockford base32 字母表(排除 I L O U)。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New 產生 26 字元的 ULID(48-bit 毫秒時間戳 + 80-bit 亂數,Crockford base32)。
//
// crypto/rand 失敗一律回錯,**絕不降級**到 math/rand:
// public_id 可預測等於可枚舉,悄悄變弱比明著失敗危險得多。
func New() (string, error) {
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
