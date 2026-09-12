package signup

import "github.com/danicotech/hestia/internal/shared/secret"

// 通行碼的實作住在 internal/shared/secret,本檔只是活動層的門面。
//
// # 為什麼要搬出去
//
// 裁判的本地登入(平台層 identity)需要同一套雜湊:同樣的 PBKDF2 參數、
// 同樣的誘餌雜湊、同樣的常數時間比對。平台層不能 import 活動層(專案鐵則 1),
// 而複製一份的代價不是多幾行程式碼 —— 是兩邊的參數遲早會漂,
// 且漂掉的那一邊沒有人會發現(弱掉的雜湊在功能測試裡看起來完全正常)。
//
// # 為什麼這個門面還留著
//
// 活動層的呼叫端談的是「選手通行碼」,不是「某種低熵秘密」。保留
// signup.GeneratePasscode / signup.NewHasher 讓 service.go 讀起來仍是
// 領域語言,也讓日後選手通行碼真的要與裁判通行碼分開時,只有這一個檔案要改。
// 它不是第二份實作:每一個符號都直接指向 secret,沒有任何行為住在這裡。

// PasscodeLength 是選手通行碼的長度(6 碼,見 secret.PasscodeLength)。
const PasscodeLength = secret.PasscodeLength

// DefaultHashIterations 是 PBKDF2 的預設迭代數(見 secret.DefaultHashIterations)。
const DefaultHashIterations = secret.DefaultHashIterations

// Hasher 是通行碼的雜湊與驗證器。
//
// 型別別名而不是包一層:包一層就會有「activity 的 Hasher」與
// 「platform 的 Hasher」兩個型別,而它們是同一個東西。
type Hasher = secret.Hasher

// NewHasher 建立雜湊器。iterations <= 0 時用 DefaultHashIterations。
func NewHasher(iterations int) (*Hasher, error) { return secret.NewHasher(iterations) }

// GeneratePasscode 產生一組新的明碼通行碼。
func GeneratePasscode() (string, error) { return secret.GeneratePasscode() }

// NormalizePasscode 把使用者輸入整理成可比對的形式(去空白、轉大寫)。
func NormalizePasscode(s string) string { return secret.NormalizePasscode(s) }
