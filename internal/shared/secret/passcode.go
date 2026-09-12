// Package secret 是低熵秘密(通行碼)的產生、正規化、雜湊與驗證。
//
// # 為什麼住在 shared
//
// 兩個地方需要它:選手的報名通行碼(活動層 signup)與裁判的本地登入通行碼
// (平台層 identity)。平台層不能 import 活動層(專案鐵則 1),而複製一份
// 的代價不是多幾行程式碼 —— 是兩邊的迭代數、字元集、誘餌雜湊遲早會漂,
// 而漂掉的那一邊沒有人會發現:一個弱掉的雜湊在功能測試裡看起來跟正常的
// 一模一樣。同一個安全機制只能有一份實作(專案第 9 條)。
//
// 只認標準庫,不認任何領域型別 —— 它對「誰在登入」沒有意見。
package secret

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PasscodeLength 是通行碼的長度。6 碼是抄寫成本與強度的折衷:
// 選手要在報名送出的那一瞬間把它抄下來,7 碼以上實測抄錯率明顯上升。
const PasscodeLength = 6

// passcodeAlphabet 是通行碼字元集,**剛好 32 個字元**。
//
// 排除的是抄寫時會認錯的那幾個(schemas/20):
//
//	0 與 O、1 與 I 與 l
//
// 只用大寫是因為通行碼會被手抄、被念出來、被貼進 Discord —— 大小寫敏感
// 在這幾條路徑上全都是純粹的失敗來源,而它換來的那一點熵並不值得。
//
// 32 個字元不是湊巧:256 % 32 == 0,所以「取一個 byte 再取低 5 位」是
// **完全均勻**的抽樣,不需要拒絕重抽,也就沒有「偏一點點但測不出來」的風險。
const passcodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// DefaultHashIterations 是 PBKDF2 的預設迭代數。
//
// # 為什麼不是直接存 SHA-256
//
// 平台的 refresh token 用單次 SHA-256 存,因為那是 256 位的隨機字串 ——
// 暴力破解它比破解 SHA-256 本身還難。通行碼不是:32^6 ≈ 2^30,
// 一張顯示卡幾秒鐘就能把整個空間算完。低熵秘密必須配慢雜湊,沒有例外。
//
// 60 萬次是 OWASP 對 PBKDF2-HMAC-SHA256 的建議值。在這個成本下,
// 用 GPU 窮舉單一通行碼的 30 位空間要幾天 —— 而這個資料庫真的外洩時,
// 我們有比「別人能改某位選手的讓武選擇」更該擔心的事。
//
// 這不是銀行等級的安全,也不需要是(signup.proto 已言明):
// 它保護的是「別人不能冒充你改讓武」,而所有破壞性操作還有裁判這道人工關卡。
const DefaultHashIterations = 600_000

// hashAlgo 是雜湊字串的演算法標籤。
//
// 把演算法與迭代數**寫進雜湊字串本身**,是為了讓日後調高成本(或換演算法)
// 不需要把所有人的通行碼作廢:舊列照舊驗、新列用新參數,兩者共存。
const hashAlgo = "pbkdf2-sha256"

// saltLen 是每筆各自的鹽長度。16 bytes 足以讓彩虹表與跨帳號批次破解失效。
const saltLen = 16

// keyLen 是導出金鑰長度,對齊 SHA-256 的輸出。
const keyLen = 32

// ErrMalformedHash 是存下來的雜湊字串讀不懂(資料壞了)。
//
// 這是系統問題不是使用者問題:呼叫端一律把它折成自己的「憑證不正確」,
// 只在伺服器端留下痕跡 —— 「這個帳號的雜湊格式不對」等於承認這個帳號存在。
var ErrMalformedHash = errors.New("通行碼雜湊格式不正確")

// GenerateCode 產生 n 碼的明碼通行碼。
//
// 一律 crypto/rand,**絕不**用 math/rand:通行碼可預測就等於沒有通行碼,
// 而這種失效在功能測試裡看起來跟正常的一模一樣。
//
// n <= 0 回空字串 —— 呼叫端傳了無意義的長度,不該靜靜得到一個「看起來像
// 通行碼」的東西。
func GenerateCode(n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("產生通行碼: %w", err)
	}
	out := make([]byte, n)
	for i, v := range b {
		// 字元集長度是 32,所以低 5 位的分佈是均勻的(見 passcodeAlphabet 的註解)。
		out[i] = passcodeAlphabet[v&0x1f]
	}
	return string(out), nil
}

// GeneratePasscode 產生一組新的明碼通行碼(PasscodeLength 碼)。
func GeneratePasscode() (string, error) {
	return GenerateCode(PasscodeLength)
}

// NormalizePasscode 把使用者輸入整理成可比對的形式。
//
// 只做去空白與轉大寫。**刻意不做易混淆字元的自動修正**(把輸入的 0 當成 O):
// 那些字元已經從字元集裡拿掉了,任何含有它們的輸入本來就不可能是有效通行碼,
// 幫使用者猜只會讓「哪些輸入等價」變成一條沒有人說得清的規則。
func NormalizePasscode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// Hasher 負責通行碼的雜湊與驗證。
//
// 迭代數可注入是為了測試:正式環境要慢(這是重點),但測試每跑一個案例
// 都等 30 毫秒的話,沒有人會願意把驗證邏輯測滿。
type Hasher struct {
	iterations int
	// dummy 是一筆對誰都不會通過的雜湊,給 VerifyDummy 用。
	// 在建構時就算好,讓「查無此人」的那條路徑與正常驗證做完全等量的工。
	dummy string
}

// NewHasher 建立雜湊器。iterations <= 0 時用 DefaultHashIterations。
//
// 建構時先產生一組誘餌雜湊。這個動作不能延後到第一次用到時才做 ——
// 那次會比後續都慢,而「第一次登入失敗特別慢」本身就是一個可觀測的訊號。
func NewHasher(iterations int) (*Hasher, error) {
	if iterations <= 0 {
		iterations = DefaultHashIterations
	}
	h := &Hasher{iterations: iterations}
	decoy, err := GeneratePasscode()
	if err != nil {
		return nil, err
	}
	dummy, err := h.Hash(decoy)
	if err != nil {
		return nil, err
	}
	h.dummy = dummy
	return h, nil
}

// Hash 把明碼通行碼轉成可存進 passcode_hash / secret_hash 的字串。
//
// 輸出形式:pbkdf2-sha256$<迭代數>$<base64 鹽>$<base64 金鑰>
func (h *Hasher) Hash(passcode string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("產生通行碼鹽: %w", err)
	}
	dk, err := pbkdf2.Key(sha256.New, passcode, salt, h.iterations, keyLen)
	if err != nil {
		return "", fmt.Errorf("導出通行碼金鑰: %w", err)
	}
	enc := base64.RawStdEncoding
	return strings.Join([]string{
		hashAlgo,
		strconv.Itoa(h.iterations),
		enc.EncodeToString(salt),
		enc.EncodeToString(dk),
	}, "$"), nil
}

// Verify 比對明碼與存下來的雜湊。
//
// 回傳 (false, nil) = 碼不對;回傳 err = 存下來的雜湊本身讀不懂(資料壞了)。
// 兩者分開是因為後者是需要有人去看的系統問題,而呼叫端對使用者的回應
// 必須一模一樣 —— 分辨得出來但不說出去。
func (h *Hasher) Verify(encoded, passcode string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashAlgo {
		return false, fmt.Errorf("%w: 格式不是 %s", ErrMalformedHash, hashAlgo)
	}
	// 迭代數取自雜湊字串而不是 h.iterations:調高成本之後,舊列必須還驗得過。
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false, fmt.Errorf("%w: 迭代數不合法", ErrMalformedHash)
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil {
		return false, fmt.Errorf("%w: 鹽不是 base64", ErrMalformedHash)
	}
	want, err := enc.DecodeString(parts[3])
	if err != nil {
		return false, fmt.Errorf("%w: 金鑰不是 base64", ErrMalformedHash)
	}
	got, err := pbkdf2.Key(sha256.New, passcode, salt, iter, len(want))
	if err != nil {
		return false, fmt.Errorf("導出通行碼金鑰: %w", err)
	}
	// 常數時間比對:逐位元組提早返回會洩漏「前幾位對了」,
	// 而通行碼只有 6 碼,這種洩漏足以把窮舉成本從 32^6 降到 6×32。
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// VerifyDummy 對一筆誘餌雜湊做一次完整的驗證,結果丟掉。
//
// 這是「查無此人」那條路徑唯一的存在理由:不跑的話,不存在的帳號會
// 立刻回應、存在的帳號要等幾十毫秒,於是登入頁就變成了一份可枚舉的名單。
// 選手名單本來就是公開的,但「誰報了名」在報名期間並不是;裁判的登入名
// 更是從頭到尾都不該可被枚舉。而且這個洞一旦開著,下一個接手的人
// 很難想到要補。
func (h *Hasher) VerifyDummy(passcode string) {
	_, _ = h.Verify(h.dummy, passcode)
}
