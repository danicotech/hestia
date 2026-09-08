package notificationpg

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// 公告的對外識別子(event_id)。
//
// ## 它要同時滿足三件互相拉扯的事
//
//  1. **對外不出現內部 BIGINT id**(專案鐵則 5)。outbox_events 沒有 public_id
//     欄位,而本任務不得新增 migration —— 所以識別子只能算出來,不能存。
//  2. **不可偽造。** QA 2026-09-08:先前直接送 id 的十進位字串,任何持有服務
//     憑證的人都能 Ack 一個**從未被自己認領過**的事件(甚至是別人正在處理的),
//     那則公告就靜靜消失了。識別子必須只有「認領」這個動作發得出來。
//  3. **對同一個事件永遠一樣。** stentor 用 event_id 去重(at-least-once 下
//     重放是常態)。若識別子綁定「這一次認領」,重送就會拿到新的值,
//     去重直接失效 —— 同一則公告會被貼兩次。這比問題 2 更容易發生。
//
// ## 作法:單一區塊的 AES 決定性加密
//
//	明文 16 bytes = <8 bytes 固定標記> || <8 bytes big-endian id>
//	token         = "n1." + base64url(AES(key, 明文))
//
// AES 對單一區塊就是一個帶金鑰的置換(PRP):同一個 id 永遠得到同一個 token
// (滿足 3),而沒有金鑰算不出任何 token(滿足 2),密文裡看不出 id 的任何
// 結構、連號的事件也不會得到連號的 token(滿足 1)。
//
// **這不是「用 ECB 加密資料」**。ECB 的惡名來自「多個區塊、明文有重複樣式」,
// 這裡明文永遠只有一個區塊,而且相同明文本來就該得到相同密文(去重需要它)。
//
// 驗證靠固定標記:偽造一個 token 等於要湊出一段密文,解密後前 8 bytes
// 剛好等於標記 —— 亂猜的機率是 2^-64。標記比對用常數時間。
//
// ## 金鑰
//
// 沒有設定時每次啟動隨機產生(見 New)。後果是 hestia 重啟後,**尚未 Ack 的**
// 那幾則公告會拿到新的 token:它們會被重送(at-least-once 本來就允許),
// 而 stentor 的去重記憶體裡沒有新 token,所以最多重貼一次。
// 這與 stentor 自己重啟時的行為一致(它的去重本來就在記憶體裡)。
// 要連重啟都不重貼,用 WithEventIDKey 給一把固定金鑰。
const (
	eventTokenVersion = "n1"
	// eventTokenMarker 是明文的前 8 bytes。內容本身不重要,重要的是它固定。
	eventTokenMarker = "hestiaN1"
	// eventIDKeySize 是金鑰長度(AES-256)。
	eventIDKeySize = 32
)

// EventIDKey 是**驗證過**的識別子金鑰。
//
// 為什麼是一個型別而不是 []byte:長度不對是設定錯誤,得有人看到 error,
// 而 functional option 沒有回傳值。把驗證提前到這裡,組裝端就非處理 error 不可
// —— 沒有「設定寫錯卻靜靜降級成隨機金鑰」這條路。
type EventIDKey struct{ block cipher.Block }

// NewEventIDKey 驗證並包裝金鑰(必須是 32 bytes,AES-256)。
func NewEventIDKey(key []byte) (EventIDKey, error) {
	if len(key) != eventIDKeySize {
		return EventIDKey{}, fmt.Errorf(
			"event_id 金鑰必須是 %d bytes,拿到 %d", eventIDKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return EventIDKey{}, fmt.Errorf("建立 event_id 金鑰: %w", err)
	}
	return EventIDKey{block: block}, nil
}

// randomEventIDCipher 產生一把行程內金鑰。
//
// 兩個都不回 error 的理由:crypto/rand.Read 在 Go 1.24 之後不會回錯
// (系統 RNG 壞掉時它自己 panic —— 那確實是不該繼續執行的狀態),
// 而長度是這裡自己給的常數,aes.NewCipher 不可能失敗。
func randomEventIDCipher() cipher.Block {
	key := make([]byte, eventIDKeySize)
	if _, err := rand.Read(key); err != nil {
		panic("notificationpg: 系統亂數不可用: " + err.Error())
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("notificationpg: 建立 event_id 金鑰失敗: " + err.Error())
	}
	return block
}

// mintEventID 把內部 id 轉成對外的不透明識別子。
func mintEventID(block cipher.Block, id int64) string {
	var plain [aes.BlockSize]byte
	copy(plain[:8], eventTokenMarker)
	binary.BigEndian.PutUint64(plain[8:], uint64(id))

	var out [aes.BlockSize]byte
	block.Encrypt(out[:], plain[:])
	return eventTokenVersion + "." + base64.RawURLEncoding.EncodeToString(out[:])
}

// parseEventID 把識別子換回內部 id。
//
// 任何一步不對都回 (0, false),而且**不區分失敗原因**:格式錯、版本錯、
// 標記錯都是同一個答案。呼叫端(Ack)對這些一律靜靜略過 ——
// 告訴對方「你這個 token 格式對但簽章錯」等於給偽造者一個神諭。
func parseEventID(block cipher.Block, token string) (int64, bool) {
	version, encoded, found := strings.Cut(token, ".")
	if !found || version != eventTokenVersion {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != aes.BlockSize {
		return 0, false
	}
	var plain [aes.BlockSize]byte
	block.Decrypt(plain[:], raw)
	if subtle.ConstantTimeCompare(plain[:8], []byte(eventTokenMarker)) != 1 {
		return 0, false
	}
	id := int64(binary.BigEndian.Uint64(plain[8:]))
	if id <= 0 {
		return 0, false
	}
	return id, true
}
