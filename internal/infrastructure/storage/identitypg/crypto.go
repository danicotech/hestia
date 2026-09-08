package identitypg

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// provider 憑證的應用層加密(schemas/02):AES-256-GCM,密文格式 `v<版本>:<nonce>:<ct>`。
//
// 為什麼不用 pgcrypto:金鑰會出現在 SQL 語句裡,而 SQL 會進 log / pg_stat_statements。
// 在應用層加密,資料庫從頭到尾看不到金鑰,也看不到明文。
//
// 版本前綴是為了換演算法/換金鑰時能共存(舊密文照舊解、新密文用新版),
// 現在只有 v1。
const encVersion = "v1"

// tokenCipher 加解密 provider 發給我們的 access / refresh token。
type tokenCipher struct {
	aead cipher.AEAD
}

// newTokenCipher 建立加密器。key 必須是 32 bytes(AES-256);
// 長度不對就拒絕啟動——用錯長度的金鑰會靜默降級成 AES-128/192。
func newTokenCipher(key []byte) (*tokenCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("token 加密金鑰必須是 32 bytes(AES-256),目前 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("建立 AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("建立 GCM: %w", err)
	}
	return &tokenCipher{aead: aead}, nil
}

// tokenAAD 產生把密文**綁在它所屬那一列身分上**的 additional authenticated data。
//
// 為什麼不能只綁版本:AAD 只有版本字串時,所有列的密文彼此可互換——
// 具 DB 寫入能力者把 identity A 的 access_token_enc 整欄複製到 B,解密照樣成功,
// 我們就會拿著 A 的 Discord 憑證代表 B 去呼叫 Discord。
// 綁上 (user_id, provider, provider_user_id) 之後,搬到別列即解密失敗。
func tokenAAD(userID int64, provider, providerUserID string) []byte {
	return fmt.Appendf(nil, "%s|%d|%s|%s", encVersion, userID, provider, providerUserID)
}

// encrypt 回傳可直接存進 BYTEA 的密文。空字串回 nil(欄位存 NULL,不存空密文)。
// aad 由 tokenAAD 產生,解密時必須給出完全相同的值。
func (c *tokenCipher) encrypt(plain string, aad []byte) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// 亂數失敗絕不降級成固定 nonce:GCM 重用 nonce 會直接洩漏明文。
		return nil, fmt.Errorf("讀亂數: %w", err)
	}
	ct := c.aead.Seal(nil, nonce, []byte(plain), aad)
	out := make([]byte, 0, len(encVersion)+2+
		base64.RawStdEncoding.EncodedLen(len(nonce))+base64.RawStdEncoding.EncodedLen(len(ct)))
	out = append(out, encVersion...)
	out = append(out, ':')
	out = base64.RawStdEncoding.AppendEncode(out, nonce)
	out = append(out, ':')
	out = base64.RawStdEncoding.AppendEncode(out, ct)
	return out, nil
}

// decrypt 還原明文;aad 必須與加密時完全相同,否則認證失敗。
// 錯誤訊息刻意不含任何密文/明文片段。
func (c *tokenCipher) decrypt(b, aad []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	parts := bytes.SplitN(b, []byte{':'}, 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("密文格式不符")
	}
	if !bytes.Equal(parts[0], []byte(encVersion)) {
		return "", fmt.Errorf("不支援的密文版本")
	}
	nonce, err := base64.RawStdEncoding.AppendDecode(nil, parts[1])
	if err != nil {
		return "", fmt.Errorf("nonce 解碼失敗")
	}
	ct, err := base64.RawStdEncoding.AppendDecode(nil, parts[2])
	if err != nil {
		return "", fmt.Errorf("密文解碼失敗")
	}
	plain, err := c.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		// 三種情況共用一個訊息:金鑰不符、密文被竄改、密文被搬到別的身分列。
		return "", fmt.Errorf("解密失敗(金鑰不符、密文被竄改,或不屬於這個身分)")
	}
	return string(plain), nil
}
