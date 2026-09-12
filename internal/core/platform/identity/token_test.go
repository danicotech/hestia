package identity_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// 簽章 token 是純運算,沒有資料庫參與,所以這組測試不需要容器也不吃 -short。

const testSecret = "0123456789abcdef0123456789abcdef" // 32 bytes,測試用

func newSigner(t *testing.T) *identity.Signer {
	t.Helper()
	s, err := identity.NewSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestSignerRejectsShortSecret(t *testing.T) {
	if _, err := identity.NewSigner([]byte("too-short")); err == nil {
		t.Fatal("短金鑰應該被拒絕")
	}
	if _, err := identity.NewSigner(nil); err == nil {
		t.Fatal("空金鑰應該被拒絕")
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	s := newSigner(t)
	now := time.Now()

	tok, exp, err := s.SignAccess("USER123", "SESS456", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if got := exp.Sub(now); got != identity.AccessTokenTTL {
		t.Fatalf("access token 壽命 = %v,要 %v", got, identity.AccessTokenTTL)
	}
	if !strings.HasPrefix(tok, "v1.") || strings.Count(tok, ".") != 2 {
		t.Fatalf("格式應為 v1.<claims>.<sig>,得到 %q", tok)
	}

	c, err := s.VerifyAccess(tok, now)
	if err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	if c.Sub != "USER123" || c.Sid != "SESS456" {
		t.Fatalf("claims 不符: %+v", c)
	}
	// sub 必須是 public_id 而非內部 BIGINT id(專案鐵則 5):
	// token 會被客戶端解讀,內部 id 進 token 就等於進了對外契約。
	if c.Sub == "" {
		t.Fatal("sub 不可為空")
	}
}

func TestAccessTokenRejects(t *testing.T) {
	s := newSigner(t)
	now := time.Now()
	good, _, err := s.SignAccess("USER123", "SESS456", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	parts := strings.Split(good, ".")

	// 竄改 claims:改掉 sub 但沿用原簽章
	var c identity.AccessClaims
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解 claims: %v", err)
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("反序列化 claims: %v", err)
	}
	c.Sub = "ATTACKER"
	tampered, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("序列化 claims: %v", err)
	}
	tamperedToken := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[2]

	// 竄改簽章:解出簽章位元組、翻掉一個 bit、重新編碼
	// (直接改 base64 字元會踩到「同一組位元組有多種編碼」,測到的不是竄改)
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("解簽章: %v", err)
	}
	sigBytes[0] ^= 0x01
	badSigToken := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sigBytes)

	// 非嚴格 base64:尾端填充位元被動過的「同義」編碼也必須拒絕(token 形態唯一)
	malleable := []byte(parts[2])
	malleable[len(malleable)-1] = map[byte]byte{'A': 'B', 'Q': 'R', 'g': 'h', 'w': 'x'}[malleable[len(malleable)-1]]
	malleableToken := parts[0] + "." + parts[1] + "." + string(malleable)

	// 換一把金鑰簽的 token
	other, err := identity.NewSigner([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	foreign, _, err := other.SignAccess("USER123", "SESS456", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}

	// 用途混用:state token 拿來當 access token
	stateTok, err := s.SignState("/lobby", now)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"空字串", "", identity.ErrInvalidToken},
		{"只有版本", "v1.", identity.ErrInvalidToken},
		{"錯誤版本前綴", "v2." + parts[1] + "." + parts[2], identity.ErrInvalidToken},
		{"無版本前綴", parts[1] + "." + parts[2], identity.ErrInvalidToken},
		{"竄改 claims", tamperedToken, identity.ErrInvalidToken},
		{"竄改簽章", badSigToken, identity.ErrInvalidToken},
		{"填充位元被動過的同義編碼", malleableToken, identity.ErrInvalidToken},
		{"別把金鑰簽的", foreign, identity.ErrInvalidToken},
		{"state 當 access 用", stateTok, identity.ErrInvalidToken},
		{"簽章不是 base64url", parts[0] + "." + parts[1] + ".!!!!", identity.ErrInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.VerifyAccess(tc.token, now); !errors.Is(err, tc.want) {
				t.Fatalf("要 %v,得到 %v", tc.want, err)
			}
		})
	}

	// 過期:簽章正確但 exp 已過
	if _, err := s.VerifyAccess(good, now.Add(identity.AccessTokenTTL+time.Second)); !errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("過期要回 ErrTokenExpired,得到 %v", err)
	}
	// 剛好到期的那一秒就不接受(>=,不是 >)
	if _, err := s.VerifyAccess(good, now.Add(identity.AccessTokenTTL)); !errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("到期時刻要回 ErrTokenExpired,得到 %v", err)
	}
}

// 錯誤訊息會進 log、進 APM、可能被回給客戶端 —— 任何一處含 token 都等於憑證外洩。
func TestTokenErrorsNeverLeakToken(t *testing.T) {
	s := newSigner(t)
	now := time.Now()
	good, _, err := s.SignAccess("USER123", "SESS456", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}

	_, err = s.VerifyAccess(good, now.Add(time.Hour))
	assertNoSecret(t, err, good, testSecret)

	_, err = s.VerifyAccess(good+"x", now)
	assertNoSecret(t, err, good, testSecret)

	_, err = s.VerifyState(good, good, now)
	assertNoSecret(t, err, good, testSecret)
}

func assertNoSecret(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("預期有錯誤")
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			t.Fatalf("錯誤訊息外洩機密: %q", msg)
		}
	}
}

func TestStateRoundTripAndExpiry(t *testing.T) {
	s := newSigner(t)
	now := time.Now()

	tok, err := s.SignState("/tournaments/42?tab=bracket", now)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}
	c, err := s.VerifyState(tok, tok, now)
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if c.Redirect != "/tournaments/42?tab=bracket" {
		t.Fatalf("redirect = %q", c.Redirect)
	}
	if c.Nonce == "" {
		t.Fatal("nonce 不可為空")
	}

	// 每次簽發的 state 必須不同(nonce 是隨機的)
	tok2, err := s.SignState("/tournaments/42?tab=bracket", now)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}
	if tok == tok2 {
		t.Fatal("兩次簽發的 state 相同,nonce 沒有生效")
	}

	if _, err := s.VerifyState(tok, tok, now.Add(identity.StateTTL+time.Second)); !errors.Is(err, identity.ErrStateExpired) {
		t.Fatalf("過期要回 ErrStateExpired,得到 %v", err)
	}
	if _, err := s.VerifyState("v1.abc.def", "v1.abc.def", now); !errors.Is(err, identity.ErrInvalidState) {
		t.Fatalf("亂寫的 state 要回 ErrInvalidState,得到 %v", err)
	}
	// access token 拿來當 state 用:用途分離擋下
	access, _, err := s.SignAccess("U", "S", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if _, err := s.VerifyState(access, access, now); !errors.Is(err, identity.ErrInvalidState) {
		t.Fatalf("access 當 state 用要被拒,得到 %v", err)
	}
}

// open redirect:攻擊者能控制 redirect 就能把剛登入的使用者送去釣魚站。
func TestCleanRedirect(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/lobby", "/lobby"},
		{"/a/b?c=d#e", "/a/b?c=d#e"},
	}
	for _, tc := range ok {
		got, err := identity.CleanRedirect(tc.in)
		if err != nil {
			t.Fatalf("CleanRedirect(%q) 意外失敗: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("CleanRedirect(%q) = %q,要 %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"https://evil.com",
		"http://evil.com/x",
		"//evil.com",
		"//evil.com/path",
		"///evil.com",
		`/\evil.com`,
		`\\evil.com`,
		"javascript:alert(1)",
		"mailto:a@b.c",
		"lobby",               // 相對但非以 / 開頭,瀏覽器解析依賴當前路徑
		"/lobby\nSet-Cookie:", // 控制字元(header 注入)
		strings.Repeat("/a", 400),
	}
	for _, in := range bad {
		if got, err := identity.CleanRedirect(in); !errors.Is(err, identity.ErrInvalidRedirect) {
			t.Fatalf("CleanRedirect(%q) 應被拒,得到 (%q, %v)", in, got, err)
		}
	}

	// SignState 也要在簽發端就擋下
	s := newSigner(t)
	if _, err := s.SignState("https://evil.com", time.Now()); !errors.Is(err, identity.ErrInvalidRedirect) {
		t.Fatalf("SignState 應拒絕絕對 URL,得到 %v", err)
	}
	if _, err := s.SignState("//evil.com", time.Now()); !errors.Is(err, identity.ErrInvalidRedirect) {
		t.Fatalf("SignState 應拒絕 protocol-relative URL,得到 %v", err)
	}
}

// login CSRF / session fixation:簽章只證明 state 是我們簽的,
// 對任何瀏覽器都有效;唯一能證明「它屬於發起請求的那個瀏覽器」的是 cookie 比對。
func TestStateRequiresMatchingCookie(t *testing.T) {
	s := newSigner(t)
	now := time.Now()

	mine, err := s.SignState("/lobby", now)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}
	// 攻擊者自己走一次登入拿到的合法 state(簽章完全正確)
	attacker, err := s.SignState("/lobby", now)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}

	cases := []struct {
		name         string
		state, cooki string
	}{
		{"cookie 缺失", mine, ""},
		{"cookie 為空字串", mine, ""},
		{"state 為空", "", mine},
		{"兩者皆空", "", ""},
		{"攻擊者的 state 配受害者的 cookie", attacker, mine},
		{"受害者的 state 配攻擊者的 cookie", mine, attacker},
		{"cookie 被截斷", mine, mine[:len(mine)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.VerifyState(tc.state, tc.cooki, now); !errors.Is(err, identity.ErrStateMismatch) {
				t.Fatalf("要 ErrStateMismatch,得到 %v", err)
			}
		})
	}

	// 相符才通過
	c, err := s.VerifyState(mine, mine, now)
	if err != nil {
		t.Fatalf("state 與 cookie 相符應通過: %v", err)
	}
	if c.Redirect != "/lobby" {
		t.Errorf("redirect = %q", c.Redirect)
	}
}

// 匯出的 Sign / Verify 是給平台以外的用途用的(目前是活動層的選手 session)。
// 它必須與 access / state 一樣有用途分離,而且**不能成為簽發平台身分的後門**。
func TestCustomPurposeTokens(t *testing.T) {
	s := newSigner(t)
	now := time.Now()

	type custom struct {
		Who string `json:"who"`
		Exp int64  `json:"exp"`
	}
	want := custom{Who: "01JBXPLAYER0000000000000001", Exp: now.Add(time.Hour).Unix()}

	tok, err := s.Sign("player_session", want)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	var got custom
	if err := s.Verify("player_session", tok, &got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != want {
		t.Fatalf("claims 不符: %+v,要 %+v", got, want)
	}

	// 用途分離:自訂用途與平台的兩種用途,以及自訂用途彼此之間,全部不可互換
	if err := s.Verify("other_purpose", tok, &got); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("換用途驗要被拒,得到 %v", err)
	}
	if _, err := s.VerifyAccess(tok, now); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("自訂用途當 access 用要被拒,得到 %v", err)
	}
	if _, err := s.VerifyState(tok, tok, now); !errors.Is(err, identity.ErrInvalidState) {
		t.Fatalf("自訂用途當 state 用要被拒,得到 %v", err)
	}
	access, _, err := s.SignAccess("USER123", "SESS456", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if err := s.Verify("player_session", access, &got); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("access 當自訂用途用要被拒,得到 %v", err)
	}
	state, err := s.SignState("/lobby", now)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}
	if err := s.Verify("player_session", state, &got); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("state 當自訂用途用要被拒,得到 %v", err)
	}

	// 長度上限同樣適用(未認證請求會走這條路)
	huge := "v1." + strings.Repeat("A", 1<<20) + ".AAAA"
	if err := s.Verify("player_session", huge, &got); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("超長 token 要被拒,得到 %v", err)
	}
	// 空 token
	if err := s.Verify("player_session", "", &got); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("空 token 要被拒,得到 %v", err)
	}
}

// 匯出簽章原語**不等於**匯出「簽發任意平台身分」的能力:
// 少了這道檢查,任何拿得到 Signer 的程式碼都能 Sign("access", 自己捏的 claims)。
func TestSignRejectsReservedAndMalformedPurpose(t *testing.T) {
	s := newSigner(t)

	bad := []string{
		"access", // 平台保留
		"state",  // 平台保留
		"",
		"Player_Session",        // 大寫
		"player.session",        // 分隔符
		"player-session",        // 連字號
		"player session",        // 空白
		"player\x00session",     // 控制字元
		strings.Repeat("a", 33), // 過長
	}
	for _, typ := range bad {
		t.Run(typ, func(t *testing.T) {
			if _, err := s.Sign(typ, map[string]string{"a": "b"}); !errors.Is(err, identity.ErrInvalidTokenType) {
				t.Fatalf("Sign(%q) 要回 ErrInvalidTokenType,得到 %v", typ, err)
			}
			var dst map[string]string
			if err := s.Verify(typ, "v1.aa.bb", &dst); !errors.Is(err, identity.ErrInvalidTokenType) {
				t.Fatalf("Verify(%q) 要回 ErrInvalidTokenType,得到 %v", typ, err)
			}
		})
	}

	// 用途給錯是程式錯誤,不是憑證問題 —— 不可被當成「請重新登入」。
	_, err := s.Sign("access", map[string]string{"a": "b"})
	if errors.Is(err, identity.ErrInvalidToken) || errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("ErrInvalidTokenType 不該屬於憑證錯誤族: %v", err)
	}
}

// 沒有長度上限的話,每個未認證請求都能叫我們對數 MB 的字串做 base64 + HMAC。
func TestVerifyRejectsOversizedToken(t *testing.T) {
	s := newSigner(t)
	now := time.Now()
	huge := "v1." + strings.Repeat("A", 1<<20) + ".AAAA"

	if _, err := s.VerifyAccess(huge, now); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("超長 access token 要回 ErrInvalidToken,得到 %v", err)
	}
	if _, err := s.VerifyState(huge, huge, now); !errors.Is(err, identity.ErrInvalidState) {
		t.Fatalf("超長 state 要回 ErrInvalidState,得到 %v", err)
	}
	// 正常長度不受影響
	good, _, err := s.SignAccess("USER123", "SESS456", now)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if len(good) > 512 {
		t.Fatalf("正常 access token 不該這麼長:%d bytes", len(good))
	}
	if _, err := s.VerifyAccess(good, now); err != nil {
		t.Fatalf("正常 token 應通過: %v", err)
	}
}
