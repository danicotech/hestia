package secret

import (
	"errors"
	"strings"
	"testing"
)

// testIterations 是測試用的迭代數。
//
// 正式環境要慢(那是安全性的來源),但每個測試案例都等 30 毫秒的話,
// 驗證邏輯就不會被測滿 —— 而測不滿的驗證邏輯比慢一點的雜湊危險得多。
const testIterations = 32

func newTestHasher(t *testing.T) *Hasher {
	t.Helper()
	h, err := NewHasher(testIterations)
	if err != nil {
		t.Fatalf("建立 Hasher: %v", err)
	}
	return h
}

// TestPasscodeAlphabetExcludesConfusables 字元集不得含易混淆字元。
//
// 這條是 schemas/20 明訂的:通行碼會被手抄、被念出來、被貼進 Discord,
// 0/O 與 1/I/l 在這幾條路徑上是純粹的失敗來源。
func TestPasscodeAlphabetExcludesConfusables(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"0", "O", "1", "I", "l"} {
		if strings.Contains(passcodeAlphabet, bad) {
			t.Errorf("字元集不該含易混淆字元 %q", bad)
		}
	}
	// 32 個字元是「取一個 byte 的低 5 位就是均勻抽樣」的前提。
	// 少一個字元就會產生偏差,而那種偏差測不出來卻真的削弱強度。
	if len(passcodeAlphabet) != 32 {
		t.Errorf("字元集長度 = %d,必須剛好 32(見 passcodeAlphabet 註解)", len(passcodeAlphabet))
	}
	seen := map[rune]bool{}
	for _, r := range passcodeAlphabet {
		if seen[r] {
			t.Errorf("字元 %q 在字元集裡出現兩次", r)
		}
		seen[r] = true
	}
}

func TestGeneratePasscodeShape(t *testing.T) {
	t.Parallel()

	for range 200 {
		code, err := GeneratePasscode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != PasscodeLength {
			t.Fatalf("長度 = %d,要 %d:%q", len(code), PasscodeLength, code)
		}
		for _, r := range code {
			if !strings.ContainsRune(passcodeAlphabet, r) {
				t.Fatalf("出現字元集外的字元 %q:%q", r, code)
			}
		}
	}
}

// TestGeneratePasscodeIsRandom 200 次不該撞到 —— 空間是 32^6 ≈ 10 億。
// 撞到就代表亂數來源出了問題(例如有人把 crypto/rand 換成固定種子)。
func TestGeneratePasscodeIsRandom(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 200)
	for range 200 {
		code, err := GeneratePasscode()
		if err != nil {
			t.Fatal(err)
		}
		if seen[code] {
			t.Fatalf("200 次之內撞到同一組通行碼 %q,亂數來源有問題", code)
		}
		seen[code] = true
	}
}

// TestGeneratePasscodeUsesWholeAlphabet 每個字元都應該被抽到過。
// 抽不到某些字元通常代表取模或遮罩寫錯(例如用了 &0x0f)。
func TestGeneratePasscodeUsesWholeAlphabet(t *testing.T) {
	t.Parallel()

	seen := map[rune]bool{}
	for range 2000 {
		code, err := GeneratePasscode()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range code {
			seen[r] = true
		}
	}
	for _, r := range passcodeAlphabet {
		if !seen[r] {
			t.Errorf("字元 %q 在 12000 次抽樣裡一次都沒出現,抽樣可能有偏差", r)
		}
	}
}

func TestNormalizePasscode(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"  abc234  ": "ABC234",
		"ABC234":     "ABC234",
		"abc234":     "ABC234",
		"\tAbC234\n": "ABC234",
	}
	for in, want := range cases {
		if got := NormalizePasscode(in); got != want {
			t.Errorf("NormalizePasscode(%q) = %q, 要 %q", in, got, want)
		}
	}
}

func TestHashVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	code, err := GeneratePasscode()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := h.Hash(code)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := h.Verify(encoded, code)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("正確的通行碼應該驗得過")
	}
}

// TestHashNeverContainsPlaintext 雜湊字串裡不得出現明碼。
//
// 這個雜湊會進資料庫、會被 dump、會出現在備份裡。裡面有明碼的話,
// 「DB 只存 hash」這句話就是假的。
func TestHashNeverContainsPlaintext(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	for range 50 {
		code, err := GeneratePasscode()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := h.Hash(code)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(encoded, code) {
			t.Fatalf("雜湊字串裡出現明碼:%q 在 %q 裡", code, encoded)
		}
	}
}

// TestHashIsSalted 同一組通行碼雜湊兩次必須得到不同結果。
//
// 沒有鹽的話,DB 外洩時「哪些人用了同一組碼」一眼就看得出來,
// 而且整批人可以用同一張表一次破完。
func TestHashIsSalted(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	a, err := h.Hash("ABC234")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash("ABC234")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("同一組通行碼雜湊兩次得到相同結果,鹽沒有生效")
	}
	// 兩個都要驗得過。
	for _, encoded := range []string{a, b} {
		ok, err := h.Verify(encoded, "ABC234")
		if err != nil || !ok {
			t.Errorf("加鹽後仍須驗得過:ok=%v err=%v", ok, err)
		}
	}
}

func TestVerifyRejectsWrongPasscode(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	encoded, err := h.Hash("ABC234")
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{"ABC235", "abc234", "", "ABC2345", "XYZ789"} {
		ok, err := h.Verify(encoded, wrong)
		if err != nil {
			t.Fatalf("驗證 %q 不該回錯誤(碼不對不是系統問題):%v", wrong, err)
		}
		if ok {
			t.Errorf("%q 不該驗得過", wrong)
		}
	}
}

// TestVerifyRejectsMalformedHash 資料庫裡的雜湊讀不懂時要**回錯誤**而不是 false。
//
// 兩者必須分得出來:false 是使用者打錯碼,錯誤是資料壞了需要有人去看。
// 都回 false 的話,一整批壞掉的雜湊會表現成「這些人都忘記自己的密碼」。
func TestVerifyRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	bad := []string{
		"",
		"plaintext",
		"bcrypt$12$abc$def",             // 演算法不對
		"pbkdf2-sha256$abc$c2FsdA$a2V5", // 迭代數不是數字
		"pbkdf2-sha256$0$c2FsdA$a2V5",   // 迭代數 0
		"pbkdf2-sha256$1000$!!!$a2V5",   // 鹽不是 base64
		"pbkdf2-sha256$1000$c2FsdA$!!!", // 金鑰不是 base64
		"pbkdf2-sha256$1000$c2FsdA",     // 欄位數不對
	}
	for _, encoded := range bad {
		ok, err := h.Verify(encoded, "ABC234")
		if !errors.Is(err, ErrMalformedHash) {
			t.Errorf("%q 要回 ErrMalformedHash,得到 ok=%v err=%v", encoded, ok, err)
		}
		if ok {
			t.Errorf("%q 不該驗得過", encoded)
		}
	}
}

// TestVerifyHonoursEmbeddedIterations 迭代數存在雜湊字串裡,所以調高成本之後
// 舊列還要驗得過 —— 否則調參數等於把全場選手鎖在門外。
func TestVerifyHonoursEmbeddedIterations(t *testing.T) {
	t.Parallel()

	old, err := NewHasher(16)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := old.Hash("ABC234")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, "$16$") {
		t.Fatalf("迭代數應寫進雜湊字串:%q", encoded)
	}

	upgraded, err := NewHasher(64)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := upgraded.Verify(encoded, "ABC234")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("調高迭代數之後,舊雜湊必須仍然驗得過")
	}
}

// TestVerifyDummyNeverMatches 誘餌雜湊對任何輸入都不會通過,
// 而且不會噴錯 —— 它會被放在登入的失敗路徑上,噴錯就等於暴露了那條路徑。
func TestVerifyDummyNeverMatches(t *testing.T) {
	t.Parallel()

	h := newTestHasher(t)
	if h.dummy == "" {
		t.Fatal("誘餌雜湊應在建構時就算好,不能延後(第一次登入失敗會特別慢)")
	}
	for _, code := range []string{"", "ABC234", "XYZ789"} {
		ok, err := h.Verify(h.dummy, code)
		if err != nil {
			t.Errorf("誘餌雜湊本身必須是合法格式:%v", err)
		}
		if ok {
			t.Errorf("誘餌雜湊不該對 %q 通過", code)
		}
	}
	h.VerifyDummy("ABC234") // 不該 panic
}

func TestNewHasherDefaultsIterations(t *testing.T) {
	t.Parallel()

	// iterations <= 0 要退回預設,而不是變成「雜湊一次就好」。
	// 迭代數 0 的 PBKDF2 等於沒有拉伸,而 6 碼通行碼在那個成本下幾秒就破了。
	for _, iter := range []int{0, -1} {
		h, err := NewHasher(iter)
		if err != nil {
			t.Fatal(err)
		}
		if h.iterations != DefaultHashIterations {
			t.Errorf("NewHasher(%d).iterations = %d,要 %d", iter, h.iterations, DefaultHashIterations)
		}
	}
}

// TestGenerateCodeLength 長度可指定 —— 裁判的本地登入通行碼用比選手更長的碼
// (同一支產生器、同一個字元集,只有長度不同)。
func TestGenerateCodeLength(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 6, 12, 32} {
		code, err := GenerateCode(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != n {
			t.Errorf("GenerateCode(%d) 長度 = %d", n, len(code))
		}
		for _, r := range code {
			if !strings.ContainsRune(passcodeAlphabet, r) {
				t.Errorf("GenerateCode(%d) 出現字元集外的字元 %q", n, r)
			}
		}
	}
	// n <= 0 回空字串:呼叫端傳了無意義的長度,不該靜靜拿到一個
	// 「看起來像通行碼」的東西。
	for _, n := range []int{0, -1} {
		if code, err := GenerateCode(n); err != nil || code != "" {
			t.Errorf("GenerateCode(%d) = %q, %v,要空字串", n, code, err)
		}
	}
}
