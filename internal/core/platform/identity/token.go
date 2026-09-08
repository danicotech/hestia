package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 為什麼不用 JWT(schemas/02 增補 F):唯一的消費者是我們自己的 stentor / theatron,
// 不需要跨組織互通——而 JWT 的複雜度(演算法協商、header 解析)幾乎全部源自互通需求,
// 且是 alg=none / alg 混淆這類經典漏洞的溫床。
//
// 格式固定: v1.<base64url(claims_json)>.<base64url(HMAC-SHA256)>
//
//   - 演算法寫死在格式版本裡,**沒有** alg 欄位可以協商 ⇒ 結構上不可能有 alg 混淆
//   - 驗證順序:比版本前綴 → hmac.Equal 常數時間比簽章 → 才解析 claims → 才檢查 exp
//     (先驗簽再解析:未經驗證的位元組永遠不進 json.Unmarshal)
const tokenVersion = "v1"

// 用途分離(domain separation):typ 不放進 token,而是**混進被簽的訊息**。
// 於是 state token 拿去當 access token 用會直接簽章不符——
// 既不需要新增可被竄改的欄位,也堵掉 token 混用。
const (
	typAccess = "access"
	typState  = "state"
)

// minSecretLen 是 HMAC-SHA256 金鑰的下限。短金鑰能通過所有測試卻毫無強度,
// 必須在建構時就失敗出聲,不能等到被暴力破解才知道。
const minSecretLen = 32

// maxTokenLen 是驗證時接受的 token 長度上限。
// access claims 實際不到 200 bytes,4 KB 綽綽有餘;沒有上限的話,
// 每一個**未認證**的請求都能叫我們對數 MB 的字串做 base64 decode + HMAC。
const maxTokenLen = 4096

// AccessClaims 是 access token 的內容。
//
// Sub 刻意是 **public_id(ULID)** 而非內部 BIGINT id:token 會被客戶端解讀,
// 內部 id 進 token 就等於進了對外契約(專案鐵則 5)。
type AccessClaims struct {
	Sub string `json:"sub"` // user public_id
	Sid string `json:"sid"` // session public_id
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// StateClaims 是 OAuth2 state 的內容:無狀態 CSRF,不落任何儲存
// (不進 DB 也不進 Redis——沒有狀態就沒有清理、沒有重啟遺失、少一個活動零件)。
type StateClaims struct {
	Nonce    string `json:"nonce"`
	Redirect string `json:"redirect"`
	Exp      int64  `json:"exp"`
}

// Signer 簽發與驗證平台自己的簽章 token。金鑰由建構子注入
// (PLATFORM_JWT_SECRET 的讀取是組裝層的事,契約層不碰環境變數)。
type Signer struct {
	secret []byte
}

// NewSigner 建立簽章器。secret 長度不足即拒絕啟動。
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("簽章金鑰至少需要 %d bytes,目前 %d", minSecretLen, len(secret))
	}
	// 複製一份:呼叫端之後改動自己那個 slice 不該影響已建立的簽章器。
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &Signer{secret: cp}, nil
}

// SignAccess 簽發 access token,回傳 token 與到期時間。
func (s *Signer) SignAccess(userPublicID, sessionPublicID string, now time.Time) (string, time.Time, error) {
	if userPublicID == "" || sessionPublicID == "" {
		return "", time.Time{}, fmt.Errorf("sub/sid 不可為空: %w", ErrInvalidToken)
	}
	exp := now.Add(AccessTokenTTL)
	tok, err := s.sign(typAccess, AccessClaims{
		Sub: userPublicID,
		Sid: sessionPublicID,
		Iat: now.Unix(),
		Exp: exp.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// VerifyAccess 驗證 access token 並回傳 claims。
func (s *Signer) VerifyAccess(token string, now time.Time) (*AccessClaims, error) {
	var c AccessClaims
	if err := s.verify(typAccess, token, &c); err != nil {
		return nil, err
	}
	if now.Unix() >= c.Exp {
		return nil, ErrTokenExpired
	}
	if c.Sub == "" || c.Sid == "" {
		return nil, fmt.Errorf("claims 缺 sub/sid: %w", ErrInvalidToken)
	}
	return &c, nil
}

// SignState 簽發 OAuth state。redirectPath 必須是站內相對路徑。
func (s *Signer) SignState(redirectPath string, now time.Time) (string, error) {
	clean, err := CleanRedirect(redirectPath)
	if err != nil {
		return "", err
	}
	nonce, err := randomNonce()
	if err != nil {
		return "", err
	}
	return s.sign(typState, StateClaims{
		Nonce:    nonce,
		Redirect: clean,
		Exp:      now.Add(StateTTL).Unix(),
	})
}

// VerifyState 驗證 OAuth state。
//
// fromCookie 是入口層從 HttpOnly + SameSite=Lax cookie 讀回來的同一個 state。
// **這個比對才是 CSRF 防護本身**:簽章只證明「這是我們簽的」,
// 而我們簽的 state 對任何瀏覽器都有效——攻擊者自己走一次登入拿到合法 state、
// 配上自己的 authorization code 組成回呼 URL 誘導受害者造訪,就能把
// 攻擊者的帳號種進受害者的瀏覽器(login CSRF / session fixation)。
// 只有「這個 state 也出現在**發起請求的那個瀏覽器**的 cookie 裡」能排除它。
//
// 比對放在這裡而不是留給入口層自律:安全檢查一旦可以被忘記,遲早會被忘記。
//
// redirect 在簽發時已驗過,這裡**再驗一次**:簽發端的規則之後若放寬,
// 舊 token 不會因此變成 open redirect;而且「消費點永遠自己驗一次」
// 是唯一不依賴其他程式碼正確性的寫法。
func (s *Signer) VerifyState(token, fromCookie string, now time.Time) (*StateClaims, error) {
	// 常數時間比對;兩者都空(cookie 遺失、被清掉)也一律拒絕,
	// 絕不「沒有 cookie 就跳過檢查」——那是所有 CSRF 防護最常見的破口。
	if token == "" || fromCookie == "" || !hmac.Equal([]byte(token), []byte(fromCookie)) {
		return nil, ErrStateMismatch
	}
	var c StateClaims
	if err := s.verify(typState, token, &c); err != nil {
		if errors.Is(err, ErrInvalidToken) {
			return nil, fmt.Errorf("%w: %w", ErrInvalidState, err)
		}
		return nil, err
	}
	if now.Unix() >= c.Exp {
		return nil, ErrStateExpired
	}
	if c.Nonce == "" {
		return nil, fmt.Errorf("%w: 缺 nonce", ErrInvalidState)
	}
	clean, err := CleanRedirect(c.Redirect)
	if err != nil {
		return nil, err
	}
	c.Redirect = clean
	return &c, nil
}

// sign 產生 v1.<claims>.<sig>。
func (s *Signer) sign(typ string, claims any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("序列化 claims: %w", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return tokenVersion + "." + enc + "." + base64.RawURLEncoding.EncodeToString(s.mac(typ, enc)), nil
}

// verify 依序做:版本前綴 → 常數時間比簽章 → 解析 claims。
// 任何一步失敗都回同一個 ErrInvalidToken(不告訴攻擊者是哪一步錯的),
// 且**訊息絕不含 token 內容**——錯誤字串會進 log,那等於把憑證寫進 log。
func (s *Signer) verify(typ, token string, dst any) error {
	if len(token) > maxTokenLen {
		return fmt.Errorf("token 過長: %w", ErrInvalidToken)
	}
	prefix := tokenVersion + "."
	if !strings.HasPrefix(token, prefix) {
		return fmt.Errorf("版本前綴不符: %w", ErrInvalidToken)
	}
	rest := token[len(prefix):]
	dot := strings.LastIndexByte(rest, '.')
	if dot <= 0 || dot == len(rest)-1 {
		return fmt.Errorf("格式不符: %w", ErrInvalidToken)
	}
	enc, sigPart := rest[:dot], rest[dot+1:]

	// Strict():拒絕尾端非零填充位元。非嚴格模式下同一組簽章位元組有數種合法編碼,
	// 等於同一個 token 有數個字串形態——會讓「token 唯一」這件事悄悄不成立。
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigPart)
	if err != nil {
		return fmt.Errorf("簽章不是 base64url: %w", ErrInvalidToken)
	}
	if !hmac.Equal(sig, s.mac(typ, enc)) {
		return fmt.Errorf("簽章不符: %w", ErrInvalidToken)
	}

	// 到這裡才碰 payload:未經驗證的位元組不進解析器。
	payload, err := base64.RawURLEncoding.Strict().DecodeString(enc)
	if err != nil {
		return fmt.Errorf("claims 不是 base64url: %w", ErrInvalidToken)
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("claims 不是合法 JSON: %w", ErrInvalidToken)
	}
	return nil
}

// mac 對 "<版本>.<用途>.<claims>" 取 HMAC-SHA256。
// 用途混進被簽訊息 = 跨用途的 token 一律簽章不符。
func (s *Signer) mac(typ, enc string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(tokenVersion))
	m.Write([]byte{'.'})
	m.Write([]byte(typ))
	m.Write([]byte{'.'})
	m.Write([]byte(enc))
	return m.Sum(nil)
}

// CleanRedirect 驗證登入後的導回路徑必須是**站內相對路徑**,並回傳正規化結果。
//
// 這是 open redirect 的唯一防線:攻擊者能控制 redirect 就能把使用者
// (帶著剛登入的狀態)送去釣魚站。拒絕的形態:
//
//	https://evil.com   絕對 URL
//	//evil.com         protocol-relative,瀏覽器會當成絕對 URL
//	/\evil.com         反斜線,部分瀏覽器等同 //
//	javascript:...     有 scheme
func CleanRedirect(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if len(p) > 512 {
		return "", fmt.Errorf("%w: 過長", ErrInvalidRedirect)
	}
	// 反斜線與控制字元先擋掉:它們是各家瀏覽器解析差異的來源。
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' || p[i] < 0x20 || p[i] == 0x7f {
			return "", fmt.Errorf("%w: 含不合法字元", ErrInvalidRedirect)
		}
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("%w: %s", ErrInvalidRedirect, "必須以單一 / 開頭")
	}
	u, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("%w: 無法解析", ErrInvalidRedirect)
	}
	if u.IsAbs() || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "", fmt.Errorf("%w: 不可帶 scheme 或 host", ErrInvalidRedirect)
	}
	return p, nil
}

// randomNonce 產生 state 的一次性亂數。crypto/rand 失敗一律回錯,絕不降級。
func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("讀亂數: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
