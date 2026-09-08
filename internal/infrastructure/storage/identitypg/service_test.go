package identitypg_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

func TestStartLoginBuildsAuthorizeURL(t *testing.T) {
	setup(t)

	authURL, state, err := svc.StartLogin("/tournaments/7")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("解析 authorize URL: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != testClientID {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != testRedirect {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("scope") != "identify email" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	if q.Get("state") != state {
		t.Errorf("state 沒放進 URL")
	}

	// open redirect:絕對 URL 在 StartLogin 就被擋下,不會走到 provider
	if _, _, err := svc.StartLogin("https://evil.com"); !errors.Is(err, identity.ErrInvalidRedirect) {
		t.Errorf("絕對 URL 應被拒,得到 %v", err)
	}
	if _, _, err := svc.StartLogin("//evil.com"); !errors.Is(err, identity.ErrInvalidRedirect) {
		t.Errorf("//evil.com 應被拒,得到 %v", err)
	}
}

// 首次登入:建 users + identities + privacy 三列、發註冊禮、回可用的 session。
func TestFirstLoginCreatesUserGrantsBonus(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "alice")

	sess := login(t, discordID, identity.DeviceInfo{UserAgent: "Firefox/1.0", IP: "203.0.113.9"})
	if !sess.IsNewUser {
		t.Error("首次登入應標記為新使用者")
	}
	if sess.Redirect != "/lobby" {
		t.Errorf("Redirect = %q,應原樣帶回 state 裡的相對路徑", sess.Redirect)
	}
	if sess.AccessToken == "" || sess.RefreshToken == "" {
		t.Fatal("access / refresh token 都必須回傳")
	}
	if sess.UserPublicID == "" || sess.SessionPublicID == "" {
		t.Fatal("public_id 必須回傳")
	}

	// 三張表各一列
	if n := queryInt(t, `SELECT count(*) FROM platform.users WHERE id = $1`, sess.UserID); n != 1 {
		t.Errorf("users 應有 1 列,實際 %d", n)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider = 'discord' AND provider_user_id = $1`,
		discordID); n != 1 {
		t.Errorf("identities 應有 1 列,實際 %d", n)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.user_privacy_settings WHERE user_id = $1`, sess.UserID); n != 1 {
		t.Errorf("user_privacy_settings 應有 1 列,實際 %d", n)
	}

	// Discord 憑證有加密(不得出現明文,也不得是空的)
	var enc []byte
	if err := pool.QueryRow(ctx,
		`SELECT access_token_enc FROM platform.identities WHERE provider_user_id = $1`,
		discordID).Scan(&enc); err != nil {
		t.Fatalf("讀 access_token_enc: %v", err)
	}
	if len(enc) == 0 {
		t.Error("access_token_enc 不該是空的")
	}
	if !strings.HasPrefix(string(enc), "v1:") {
		t.Errorf("密文格式應為 v<版本>:<nonce>:<ct>,得到 %q", string(enc))
	}
	if strings.Contains(string(enc), "dc-access-") {
		t.Error("access_token_enc 出現明文 token")
	}

	// 註冊禮:餘額 = signup_bonus,分錄 reason 正確
	bonus := configInt(t, "signup_bonus")
	if got := queryInt(t,
		`SELECT balance FROM platform.user_balances WHERE user_id = $1 AND currency = 'coin'`,
		sess.UserID); got != bonus {
		t.Errorf("餘額 = %d,應為 signup_bonus = %d", got, bonus)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.token_entries
		 WHERE user_id = $1 AND reason = 'signup_bonus' AND amount = $2`,
		sess.UserID, bonus); n != 1 {
		t.Errorf("signup_bonus 分錄應為 1 筆,實際 %d", n)
	}
	// 帳本鐵則:SUM(entries) = balance
	if got := queryInt(t,
		`SELECT COALESCE(SUM(amount), 0) FROM platform.token_entries WHERE user_id = $1`,
		sess.UserID); got != bonus {
		t.Errorf("SUM(entries) = %d,應等於餘額 %d", got, bonus)
	}

	// session 立即可用
	uid, err := svc.Authenticate(ctx, sess.AccessToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if uid != sess.UserID {
		t.Errorf("Authenticate 回 %d,應為 %d", uid, sess.UserID)
	}
	devices, err := svc.ListDevices(ctx, sess.UserID)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("應有 1 台裝置,實際 %d", len(devices))
	}
	if devices[0].PublicID != sess.SessionPublicID {
		t.Errorf("裝置 public_id 不符")
	}
	if devices[0].UserAgent != "Firefox/1.0" || devices[0].IP != "203.0.113.9" {
		t.Errorf("裝置足跡沒存好: %+v", devices[0])
	}
}

// 二次登入同一個 Discord 帳號:不重複建 user、不重複發註冊禮、username 有更新。
func TestSecondLoginNoDuplicateUserOrBonus(t *testing.T) {
	setup(t)
	discordID := fake.newAccount(t, "bob")

	first := login(t, discordID, identity.DeviceInfo{UserAgent: "Chrome/1.0"})
	bonus := configInt(t, "signup_bonus")

	// Discord 側改名,再登一次
	fake.upsert(fakeProfile{ID: discordID, Username: "bob", GlobalName: "Bobby", Email: "bob@example.test", Avatar: "z9"})
	second := login(t, discordID, identity.DeviceInfo{UserAgent: "Safari/1.0"})

	if second.IsNewUser {
		t.Error("第二次登入不該標記為新使用者")
	}
	if second.UserID != first.UserID {
		t.Fatalf("同一 Discord 帳號應對應同一個 user:%d vs %d", first.UserID, second.UserID)
	}
	if second.SessionPublicID == first.SessionPublicID {
		t.Error("第二次登入應是另一個 session(另一台裝置)")
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider_user_id = $1`, discordID); n != 1 {
		t.Errorf("identities 應仍只有 1 列,實際 %d", n)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.token_entries WHERE user_id = $1 AND reason = 'signup_bonus'`,
		first.UserID); n != 1 {
		t.Errorf("註冊禮只能發一次,實際 %d 筆分錄", n)
	}
	if got := queryInt(t,
		`SELECT balance FROM platform.user_balances WHERE user_id = $1 AND currency = 'coin'`,
		first.UserID); got != bonus {
		t.Errorf("餘額 = %d,不該因第二次登入變動(應為 %d)", got, bonus)
	}

	var username string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM platform.identities WHERE provider_user_id = $1`, discordID,
	).Scan(&username); err != nil {
		t.Fatalf("讀 username: %v", err)
	}
	if username != "Bobby" {
		t.Errorf("username = %q,應更新成 provider 側的 global_name", username)
	}

	// 兩台裝置都在
	devices, err := svc.ListDevices(context.Background(), first.UserID)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("應有 2 台裝置,實際 %d", len(devices))
	}
}

// 併發首登(同一 Discord 帳號、兩個不同 code):恰建一個 user、註冊禮恰發一次。
// 權威是 UNIQUE(provider, provider_user_id) —— 輸的 tx 連同它剛建的 user 與分錄一起回滾。
func TestConcurrentFirstLogin(t *testing.T) {
	setup(t)
	discordID := fake.newAccount(t, "carol")

	const n = 2
	var wg sync.WaitGroup
	sessions := make([]*identity.Session, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sessions[i], errs[i] = tryLogin(discordID, identity.DeviceInfo{UserAgent: "UA"})
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("併發登入 #%d 失敗: %v", i, err)
		}
	}
	if sessions[0].UserID != sessions[1].UserID {
		t.Fatalf("併發登入建了兩個 user:%d 與 %d", sessions[0].UserID, sessions[1].UserID)
	}
	userID := sessions[0].UserID

	if got := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider_user_id = $1`, discordID); got != 1 {
		t.Errorf("identities 應恰 1 列,實際 %d", got)
	}
	if got := queryInt(t,
		`SELECT count(*) FROM platform.token_entries WHERE user_id = $1 AND reason = 'signup_bonus'`,
		userID); got != 1 {
		t.Errorf("註冊禮應恰發一次,實際 %d 筆", got)
	}
	bonus := configInt(t, "signup_bonus")
	if got := queryInt(t,
		`SELECT balance FROM platform.user_balances WHERE user_id = $1 AND currency = 'coin'`,
		userID); got != bonus {
		t.Errorf("餘額 = %d,應為 %d", got, bonus)
	}
	// 恰好一個 user:落敗那個 tx 建的 user 必須隨 rollback 消失
	if got := queryInt(t,
		`SELECT count(*) FROM platform.users u
		 JOIN platform.identities i ON i.user_id = u.id
		 WHERE i.provider_user_id = $1`, discordID); got != 1 {
		t.Errorf("應恰 1 個 user,實際 %d", got)
	}
	// 兩次登入 = 兩個有效 session(兩台裝置),不是兩個帳號
	devices, err := svc.ListDevices(context.Background(), userID)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != n {
		t.Errorf("應有 %d 台裝置,實際 %d", n, len(devices))
	}
}

// state 是無狀態 CSRF 防線:過期、竄改、亂寫都不能換到 session。
func TestCompleteLoginRejectsBadState(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "dave")

	// 過期的 state(用簽章器直接造一個 11 分鐘前簽的)
	expired, err := signer.SignState("/lobby", time.Now().Add(-identity.StateTTL-time.Minute))
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}
	if _, err := svc.CompleteLogin(ctx, fake.issueCode(discordID), expired, expired, identity.DeviceInfo{}); !errors.Is(err, identity.ErrStateExpired) {
		t.Errorf("過期 state 要回 ErrStateExpired,得到 %v", err)
	}

	// 竄改簽章
	_, good, err := svc.StartLogin("/lobby")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if _, err := svc.CompleteLogin(ctx, fake.issueCode(discordID), good+"x", good+"x", identity.DeviceInfo{}); !errors.Is(err, identity.ErrInvalidState) {
		t.Errorf("竄改的 state 要回 ErrInvalidState,得到 %v", err)
	}
	if _, err := svc.CompleteLogin(ctx, fake.issueCode(discordID), "", "", identity.DeviceInfo{}); !errors.Is(err, identity.ErrStateMismatch) {
		t.Errorf("空 state 要回 ErrStateMismatch,得到 %v", err)
	}

	// state 沒過但 code 無效:不得建立任何帳號
	if _, err := svc.CompleteLogin(ctx, "bogus-code", good, good, identity.DeviceInfo{}); !errors.Is(err, identity.ErrProviderExchange) {
		t.Errorf("無效 code 要回 ErrProviderExchange,得到 %v", err)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider_user_id = $1`, discordID); n != 0 {
		t.Errorf("state/code 失敗不該建立任何綁定,實際 %d 列", n)
	}
}

// provider 端失敗:不得留下半套資料。
func TestCompleteLoginProviderFailure(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "erin")

	fake.mu.Lock()
	fake.tokenStatus = 500
	fake.mu.Unlock()
	defer func() {
		fake.mu.Lock()
		fake.tokenStatus = 0
		fake.mu.Unlock()
	}()

	_, state, err := svc.StartLogin("/lobby")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	_, err = svc.CompleteLogin(ctx, fake.issueCode(discordID), state, state, identity.DeviceInfo{})
	if !errors.Is(err, identity.ErrProviderExchange) {
		t.Fatalf("要回 ErrProviderExchange,得到 %v", err)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider_user_id = $1`, discordID); n != 0 {
		t.Errorf("provider 失敗不該建立綁定,實際 %d 列", n)
	}
}

// 錯誤訊息會進 log / APM,含 token 或 secret 等於憑證外洩。
func TestErrorsNeverLeakSecrets(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "frank")
	sess := login(t, discordID, identity.DeviceInfo{})

	secrets := []string{sess.RefreshToken, sess.AccessToken, testSecret, testJWTSecret, testEncKey}

	// 1. 竄改 refresh token
	_, err := svc.Refresh(ctx, sess.RefreshToken+"x", identity.DeviceInfo{})
	assertNoSecrets(t, err, secrets...)

	// 2. 竄改 access token
	_, err = svc.Authenticate(ctx, sess.AccessToken+"x")
	assertNoSecrets(t, err, secrets...)

	// 3. 換 provider 憑證失敗
	fake.mu.Lock()
	fake.tokenStatus = 401
	fake.mu.Unlock()
	_, state, err := svc.StartLogin("/lobby")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	code := fake.issueCode(discordID)
	_, err = svc.CompleteLogin(ctx, code, state, state, identity.DeviceInfo{})
	fake.mu.Lock()
	fake.tokenStatus = 0
	fake.mu.Unlock()
	assertNoSecrets(t, err, append(secrets, code)...)

	// 4. 重用偵測(錯誤路徑最容易順手把 token 印出來)
	rotated, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	_, err = svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{})
	assertNoSecrets(t, err, append(secrets, rotated.RefreshToken)...)
}

func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("預期有錯誤")
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			t.Fatalf("錯誤訊息外洩機密(%q):%s", s, msg)
		}
	}
}

// login CSRF / session fixation:攻擊者拿自己的合法 state + 自己的 code 組回呼 URL
// 誘導受害者瀏覽器造訪。受害者瀏覽器沒有對應的 state cookie,必須整個擋下——
// 否則受害者會被種上攻擊者帳號的 session,之後的活動與點數全記在攻擊者名下。
func TestCompleteLoginRequiresStateCookie(t *testing.T) {
	setup(t)
	ctx := context.Background()
	attacker := fake.newAccount(t, "csrf-attacker")

	_, attackerState, err := svc.StartLogin("/lobby")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	_, victimState, err := svc.StartLogin("/lobby")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	cases := []struct {
		name          string
		state, cookie string
	}{
		{"受害者瀏覽器沒有 state cookie", attackerState, ""},
		{"受害者的 cookie 對不上攻擊者的 state", attackerState, victimState},
		{"cookie 有但 state 缺", "", attackerState},
		{"cookie 被截斷", attackerState, attackerState[:len(attackerState)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := fake.issueCode(attacker)
			_, err := svc.CompleteLogin(ctx, code, tc.state, tc.cookie, identity.DeviceInfo{})
			if !errors.Is(err, identity.ErrStateMismatch) {
				t.Fatalf("要回 ErrStateMismatch,得到 %v", err)
			}
		})
	}
	// 一列都不該被建出來
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider_user_id = $1`, attacker); n != 0 {
		t.Errorf("CSRF 嘗試不該建立任何綁定,實際 %d 列", n)
	}

	// 同一個瀏覽器(state 與 cookie 相符)才通過
	sess, err := tryLogin(attacker, identity.DeviceInfo{})
	if err != nil {
		t.Fatalf("state 與 cookie 相符應成功: %v", err)
	}
	if sess.UserID == 0 {
		t.Error("應回傳可用的 session")
	}
}
