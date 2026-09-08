package identitypg

import (
	"bytes"
	"strings"
	"testing"
)

// provider 憑證的加密是純運算,不需要資料庫。

const cryptoTestKey = "test-token-enc-key-0123456789abc" // 32 bytes

// aadA / aadB 是兩個不同身分列的 AAD。
var (
	aadA = tokenAAD(1, "discord", "dc-1")
	aadB = tokenAAD(2, "discord", "dc-2")
)

func TestTokenCipherRoundTrip(t *testing.T) {
	c, err := newTokenCipher([]byte(cryptoTestKey))
	if err != nil {
		t.Fatalf("newTokenCipher: %v", err)
	}
	const plain = "dc-access-token-abcdefg"

	ct, err := c.encrypt(plain, aadA)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Contains(ct, []byte(plain)) {
		t.Fatal("密文含明文")
	}
	if !strings.HasPrefix(string(ct), "v1:") {
		t.Fatalf("格式應為 v<版本>:<nonce>:<ct>,得到 %q", string(ct))
	}
	got, err := c.decrypt(ct, aadA)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("還原 = %q,要 %q", got, plain)
	}

	// 同一段明文兩次加密要不同(nonce 隨機;GCM 重用 nonce 會直接洩漏明文)
	ct2, err := c.encrypt(plain, aadA)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(ct, ct2) {
		t.Fatal("兩次加密結果相同,nonce 沒有隨機")
	}

	// 空字串 → 存 NULL,不存空密文
	if b, err := c.encrypt("", aadA); err != nil || b != nil {
		t.Fatalf("空字串應回 nil:%v %v", b, err)
	}
	if s, err := c.decrypt(nil, aadA); err != nil || s != "" {
		t.Fatalf("nil 應回空字串:%q %v", s, err)
	}
}

func TestTokenCipherRejectsTamperedAndWrongKey(t *testing.T) {
	c, err := newTokenCipher([]byte(cryptoTestKey))
	if err != nil {
		t.Fatalf("newTokenCipher: %v", err)
	}
	ct, err := c.encrypt("secret-value", aadA)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// 竄改密文最後一個位元組:GCM 的認證標籤要擋下來
	tampered := bytes.Clone(ct)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := c.decrypt(tampered, aadA); err == nil {
		t.Error("被竄改的密文應解密失敗")
	}

	// 換一把金鑰解不開
	other, err := newTokenCipher([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("newTokenCipher: %v", err)
	}
	if _, err := other.decrypt(ct, aadA); err == nil {
		t.Error("別把金鑰不該解得開")
	}

	// 格式錯誤
	for _, bad := range [][]byte{[]byte("garbage"), []byte("v2:a:b"), []byte("v1:!!:!!")} {
		if _, err := c.decrypt(bad, aadA); err == nil {
			t.Errorf("%q 應解密失敗", bad)
		}
	}

	// 搬到別的身分列即解不開:AAD 綁 (user_id, provider, provider_user_id)。
	// 沒有這一條的話,具 DB 寫入能力者把 A 的 access_token_enc 複製到 B,
	// 我們就會拿 A 的 Discord 憑證代表 B 呼叫 Discord。
	if _, err := c.decrypt(ct, aadB); err == nil {
		t.Error("密文被搬到別的身分列時應解密失敗")
	}

	// 錯誤訊息不得含明文或密文
	_, err = c.decrypt(tampered, aadA)
	if err == nil {
		t.Fatal("預期有錯誤")
	}
	if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), string(ct)) {
		t.Fatalf("錯誤訊息外洩內容: %v", err)
	}
}

func TestTokenCipherRejectsWrongKeyLength(t *testing.T) {
	for _, k := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("a", 33)} {
		if _, err := newTokenCipher([]byte(k)); err == nil {
			t.Errorf("長度 %d 的金鑰應被拒絕(AES-256 需要 32 bytes)", len(k))
		}
	}
}
