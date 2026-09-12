package identity_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// 登入名的正規化與形狀。
//
// 為什麼值得測:這支是**寫入端(cmd/admin)與讀取端(本地登入)共用的唯一權威**。
// 兩邊哪天各自 normalize 一次而規則不同,症狀會是「用大寫登入名建的帳號
// 永遠登不進去」,而使用者看到的訊息是「通行碼不正確」—— 沒有人查得到原因。

func TestNormalizeLoginName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"  lin  ":   "lin",
		"LIN":       "lin",
		"Lin.Judge": "lin.judge",
		"\tWANG\n":  "wang",
		"lin":       "lin",
	}
	for in, want := range cases {
		if got := identity.NormalizeLoginName(in); got != want {
			t.Errorf("NormalizeLoginName(%q) = %q,要 %q", in, got, want)
		}
	}
	// 冪等:正規化過的值再跑一次必須不變(否則寫入端與讀取端跑的次數不同
	// 就會得到不同答案)。
	for _, in := range []string{"  Lin  ", "WANG.A_b-c"} {
		once := identity.NormalizeLoginName(in)
		if twice := identity.NormalizeLoginName(once); twice != once {
			t.Errorf("正規化不冪等:%q → %q → %q", in, once, twice)
		}
	}
}

func TestValidateLoginName(t *testing.T) {
	t.Parallel()

	good := map[string]string{
		"lin":            "lin",
		"LIN":            "lin",
		"  Wang.A_b-c  ": "wang.a_b-c",
		"judge2026":      "judge2026",
		strings.Repeat("a", identity.LoginNameMaxLen): strings.Repeat("a", identity.LoginNameMaxLen),
	}
	for in, want := range good {
		got, err := identity.ValidateLoginName(in)
		if err != nil {
			t.Errorf("%q 應該被接受:%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateLoginName(%q) = %q,要 %q", in, got, want)
		}
	}

	bad := []string{
		"",        // 空
		"ab",      // 太短
		"   a   ", // 去空白後太短
		strings.Repeat("a", identity.LoginNameMaxLen+1), // 太長
		"林裁判",       // 非 ASCII:會被念出來、貼進 Discord、寫在指令列上
		"lin judge", // 空白
		"lin@example.com",
		"lin/judge",
		"lin\tjudge",
		"lin\x00", // 控制字元
	}
	for _, in := range bad {
		got, err := identity.ValidateLoginName(in)
		if err == nil {
			t.Errorf("%q 應該被拒絕,卻得到 %q", in, got)
			continue
		}
		if !errors.Is(err, identity.ErrInvalidLoginName) {
			t.Errorf("%q 的錯誤該是 ErrInvalidLoginName:%v", in, err)
		}
		if got != "" {
			t.Errorf("被拒時不該回值,得到 %q", got)
		}
	}
}

// ProviderLocal 的字面值就是 identities.provider 的值域之一
// (migration 00003 的欄位註解:discord | twitch | youtube | local)。
// 打錯字的話,查詢會靜靜地一列都比不到,而登入永遠是「通行碼不正確」。
func TestProviderLocalValue(t *testing.T) {
	t.Parallel()

	if identity.ProviderLocal != "local" {
		t.Fatalf("ProviderLocal = %q,必須是 DB 裡的那個字串", identity.ProviderLocal)
	}
}
