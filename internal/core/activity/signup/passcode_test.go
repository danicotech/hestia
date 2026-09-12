package signup

import (
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/shared/secret"
)

// 通行碼本身的行為測試住在 internal/shared/secret(實作在那裡)。
// 本檔只留兩件事:service_test.go 共用的雜湊器工廠,以及一條
// 「門面真的指向 shared 的那份實作」的接線測試 —— 沒有它的話,
// 有人把 signup 這層改回自己實作也不會有任何測試變紅。

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

// 門面與 shared 實作是同一個東西:signup 產生的碼,secret 的雜湊器驗得過,
// 反之亦然。任何一邊被改成自己的實作,這條就會斷。
func TestPasscodeFacadeDelegatesToShared(t *testing.T) {
	t.Parallel()

	if PasscodeLength != secret.PasscodeLength || DefaultHashIterations != secret.DefaultHashIterations {
		t.Fatal("門面的常數與 shared 不一致 —— 參數已經漂了")
	}

	code, err := GeneratePasscode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != secret.PasscodeLength {
		t.Fatalf("長度 = %d,要 %d:%q", len(code), secret.PasscodeLength, code)
	}
	if NormalizePasscode("  "+strings.ToLower(code)+"\n") != code {
		t.Fatal("正規化沒有走 shared 的那份")
	}

	// signup 雜湊 → secret 驗證,以及反向。
	mine := newTestHasher(t)
	theirs, err := secret.NewHasher(testIterations)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := mine.Hash(code)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := theirs.Verify(encoded, code); err != nil || !ok {
		t.Fatalf("shared 驗不過 signup 產生的雜湊:ok=%v err=%v", ok, err)
	}
	encoded, err = theirs.Hash(code)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := mine.Verify(encoded, code); err != nil || !ok {
		t.Fatalf("signup 驗不過 shared 產生的雜湊:ok=%v err=%v", ok, err)
	}
}
