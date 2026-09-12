package identitypg_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/shared/secret"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 本地登入(provider='local',migration 00031)。
//
// 這一組測試的重點有兩個,而且第二個比第一個難:
//  1. 對的憑證發得出 session,而且與 Discord 登入的是同一種;
//  2. **錯的憑證與不存在的帳號回一模一樣的東西** —— 這是登入端點是不是
//     一支帳號列舉器的分水嶺,而它在功能測試裡看起來永遠是「都失敗了,很好」。

var localSeq atomic.Int64

// newLocalJudge 直接寫 DB 建一個本地裁判帳號(cmd/admin create-judge 的等價物)。
//
// 刻意不透過 CLI:那是 package main,而這裡要測的是身分層。
// 雜湊用同一支 secret.Hasher,迭代數與服務一致(迭代數寫在雜湊字串裡,
// 所以其實不必一致 —— 但讓它一致才測得到正式路徑的形狀)。
func newLocalJudge(t *testing.T, passcode string) (loginName string, userID int64) {
	t.Helper()
	hasher, err := secret.NewHasher(testHashIterations)
	if err != nil {
		t.Fatalf("建立雜湊器: %v", err)
	}
	hash, err := hasher.Hash(secret.NormalizePasscode(passcode))
	if err != nil {
		t.Fatalf("雜湊通行碼: %v", err)
	}
	return newLocalIdentity(t, hash)
}

// newLocalIdentity 建一個帶指定 secret_hash 的本地身分(壞掉的雜湊也能塞)。
func newLocalIdentity(t *testing.T, hash string) (loginName string, userID int64) {
	t.Helper()
	ctx := context.Background()
	publicID, err := ulid.New()
	if err != nil {
		t.Fatalf("ulid: %v", err)
	}
	loginName = fmt.Sprintf("judge-%d", localSeq.Add(1))

	if err := pool.QueryRow(ctx,
		`INSERT INTO platform.users (public_id, display_name) VALUES ($1, $2) RETURNING id`,
		publicID, loginName).Scan(&userID); err != nil {
		t.Fatalf("建立使用者: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO platform.identities (user_id, provider, provider_user_id, username, secret_hash)
		 VALUES ($1, 'local', $2, $3, $4)`,
		userID, loginName, loginName, hash); err != nil {
		t.Fatalf("建立本地身分: %v", err)
	}
	return loginName, userID
}

// 對的登入名 + 通行碼 → 一個與 Discord 登入同型的 session。
func TestLocalLoginIssuesSession(t *testing.T) {
	setup(t)
	ctx := context.Background()

	const passcode = "ABCD2345EFGH"
	loginName, userID := newLocalJudge(t, passcode)

	sess, err := svc.LocalLogin(ctx, loginName, passcode,
		identity.DeviceInfo{UserAgent: "Firefox/1.0", IP: "203.0.113.7"})
	if err != nil {
		t.Fatalf("LocalLogin: %v", err)
	}
	if sess.UserID != userID {
		t.Errorf("UserID = %d,要 %d", sess.UserID, userID)
	}
	if sess.AccessToken == "" || sess.RefreshToken == "" {
		t.Fatal("access / refresh token 都必須回傳")
	}
	if sess.UserPublicID == "" || sess.SessionPublicID == "" {
		t.Fatal("public_id 必須回傳")
	}
	// 本地登入不建帳號,所以永遠不是新使用者(也就不會發第二次註冊禮)。
	if sess.IsNewUser {
		t.Error("本地登入不該標記為新使用者 —— 帳號是 cmd/admin 事先建好的")
	}

	// 發出來的東西與 Discord 登入完全同型:同一個 signer 驗得過、
	// 同一張 sessions 表有列、同一支 Refresh 換得動。
	gotUser, err := svc.Authenticate(ctx, sess.AccessToken)
	if err != nil || gotUser != userID {
		t.Fatalf("access token 驗不過:user=%d err=%v", gotUser, err)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.sessions WHERE public_id = $1 AND revoked_at IS NULL`,
		sess.SessionPublicID); n != 1 {
		t.Errorf("sessions 應有 1 列有效 session,實際 %d", n)
	}
	if _, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{}); err != nil {
		t.Errorf("本地登入的 refresh token 應該換得動:%v", err)
	}

	// last_seen_at 與 OAuth 登入一樣會被推進。
	if n := queryInt(t,
		`SELECT count(*) FROM platform.users WHERE id = $1 AND last_seen_at IS NOT NULL`,
		userID); n != 1 {
		t.Error("登入應更新 last_seen_at")
	}
}

// 登入名大小寫不敏感、通行碼去空白轉大寫 —— 正規化必須與寫入端同一套。
//
// 這條不是便利性:寫入端 normalize 而讀取端不 normalize,症狀會是
// 「用大寫登入名建的帳號永遠登不進去」,而錯誤訊息說的是「通行碼不正確」。
func TestLocalLoginNormalizesInput(t *testing.T) {
	setup(t)

	const passcode = "MNPQ4567RSTU"
	loginName, _ := newLocalJudge(t, passcode)

	cases := map[string][2]string{
		"登入名大寫":   {strings.ToUpper(loginName), passcode},
		"登入名前後空白": {"  " + loginName + " ", passcode},
		"通行碼小寫":   {loginName, strings.ToLower(passcode)},
		"通行碼含空白":  {loginName, " " + passcode + "\t"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.LocalLogin(context.Background(), c[0], c[1], identity.DeviceInfo{}); err != nil {
				t.Fatalf("應該登得進去:%v", err)
			}
		})
	}
}

// **本組最重要的一條**:所有失敗路徑回一模一樣的錯誤。
//
// 不只是 errors.Is 對得上 —— 訊息字串也必須逐字相同。分得出來的話,
// 這支 RPC 就成了一份「誰是裁判」的名單,而裁判帳號握有判勝負與發獎的能力。
func TestLocalLoginFailuresAreIndistinguishable(t *testing.T) {
	setup(t)
	ctx := context.Background()

	const passcode = "VWXY2345ZABC"
	loginName, _ := newLocalJudge(t, passcode)

	// 一個 OAuth 身分:它的 secret_hash 必須是 NULL,而且它的
	// provider_user_id(Discord snowflake)不能拿來當本地登入名。
	discordID := fake.newAccount(t, "oauth-user")
	oauthSess := login(t, discordID, identity.DeviceInfo{})
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities
		 WHERE provider = 'discord' AND provider_user_id = $1 AND secret_hash IS NULL`,
		discordID); n != 1 {
		t.Fatal("OAuth 身分的 secret_hash 必須是 NULL —— 那一欄只屬於 local")
	}

	// 一個 secret_hash 壞掉的本地身分(資料損毀)。
	brokenName, _ := newLocalIdentity(t, "這不是一個雜湊")

	// 一個已軟刪除的裁判帳號。
	deletedName, deletedID := newLocalJudge(t, passcode)
	if _, err := pool.Exec(ctx,
		`UPDATE platform.users SET deleted_at = now() WHERE id = $1`, deletedID); err != nil {
		t.Fatalf("軟刪除: %v", err)
	}

	cases := map[string][2]string{
		"通行碼錯":            {loginName, "AAAA2222BBBB"},
		"通行碼空白":           {loginName, ""},
		"查無此登入名":          {"nobody-at-all", passcode},
		"登入名是 Discord id": {discordID, passcode},
		"登入名是 ULID":       {oauthSess.UserPublicID, passcode},
		"雜湊壞掉":            {brokenName, passcode},
		"帳號已註銷":           {deletedName, passcode},
		"兩個都空":            {"", ""},
	}
	want := identity.ErrInvalidCredentials.Error()
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			sess, err := svc.LocalLogin(ctx, c[0], c[1], identity.DeviceInfo{})
			if sess != nil {
				t.Fatal("不該發出 session")
			}
			if !errors.Is(err, identity.ErrInvalidCredentials) {
				t.Fatalf("要回 ErrInvalidCredentials,得到 %v", err)
			}
			// 逐字相同:包一層上下文(fmt.Errorf)就足以讓呼叫端分辨成因。
			if got := err.Error(); got != want {
				t.Fatalf("訊息 = %q,必須與其他失敗路徑逐字相同(%q)", got, want)
			}
			// 已註銷的帳號**不能**回 ErrAccountDeleted:那個錯誤本身就是
			// 「這個登入名存在過」的證明。
			if errors.Is(err, identity.ErrAccountDeleted) {
				t.Fatal("洩漏了帳號已註銷")
			}
		})
	}

	// 正確憑證仍然登得進去 —— 確認上面全滅不是因為整條路徑壞了。
	if _, err := svc.LocalLogin(ctx, loginName, passcode, identity.DeviceInfo{}); err != nil {
		t.Fatalf("對的憑證應該登得進去:%v", err)
	}
}

// 查無此人那條路徑必須跑完一次完整的雜湊(誘餌)。
//
// 只統一回應而不補平時間,登入端點就是一支慢一點的列舉器:
// 不存在的登入名會立刻回,存在的要等雜湊算完。
//
// 用「比例」而不是絕對值來斷言:CI 的時鐘與負載都不可靠,但
// 「查無此人比錯誤通行碼快一個數量級」這件事一定是誘餌沒跑。
func TestLocalLoginUnknownNameStillHashes(t *testing.T) {
	setup(t)
	ctx := context.Background()

	const passcode = "CDEF2345GHJK"
	loginName, _ := newLocalJudge(t, passcode)

	measure := func(name, code string) time.Duration {
		// 取多次的總和:單次量測在有 GC 的環境下噪音太大。
		start := time.Now()
		for range 5 {
			_, _ = svc.LocalLogin(ctx, name, code, identity.DeviceInfo{})
		}
		return time.Since(start)
	}
	// 先各跑一次熱身(連線池、查詢計畫)。
	measure(loginName, "AAAA2222BBBB")
	measure("nobody-at-all", passcode)

	known := measure(loginName, "AAAA2222BBBB")
	unknown := measure("nobody-at-all", passcode)

	// 查無此人不該比「存在但碼錯」快一個數量級。testHashIterations 很低,
	// 所以這裡抓的是「誘餌完全沒跑」這種量級的差距,不是微秒級的洩漏。
	if unknown*10 < known {
		t.Fatalf("查無此登入名快太多(%v vs %v)—— 誘餌雜湊沒跑,這是一支帳號列舉器",
			unknown, known)
	}
}

// 併發登入:同一個裁判在多個裝置上同時登入(或連點登入鈕)。
//
// 每一次都要拿到自己的 session,而不是互相覆蓋或撞鎖失敗 ——
// issueSession 會先拿 user_sessions 的 advisory lock,所以這裡實際在測的是
// 「串行化之後每個人都還是拿得到結果」。
func TestLocalLoginConcurrent(t *testing.T) {
	setup(t)
	ctx := context.Background()

	const passcode = "JKLM2345NPQR"
	loginName, userID := newLocalJudge(t, passcode)

	const n = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		ids   = map[string]bool{}
		fails []error
	)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			// 一半用對的碼、一半用錯的碼:錯的那些必須全部失敗,
			// 而且不能因為併發就變成別種錯誤。
			code := passcode
			if i%2 == 1 {
				code = "WRONG2345XYZQ"
			}
			sess, err := svc.LocalLogin(ctx, loginName, code, identity.DeviceInfo{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case i%2 == 1:
				if !errors.Is(err, identity.ErrInvalidCredentials) {
					fails = append(fails, fmt.Errorf("錯誤通行碼得到 %w", err))
				}
			case err != nil:
				fails = append(fails, err)
			default:
				ids[sess.SessionPublicID] = true
			}
		}()
	}
	wg.Wait()

	for _, err := range fails {
		t.Errorf("併發登入失敗:%v", err)
	}
	if len(ids) != n/2 {
		t.Fatalf("拿到 %d 個相異 session,要 %d 個", len(ids), n/2)
	}
	if got := queryInt(t,
		`SELECT count(*) FROM platform.sessions WHERE user_id = $1 AND revoked_at IS NULL`,
		userID); got != int64(n/2) {
		t.Errorf("DB 裡有 %d 個有效 session,要 %d 個", got, n/2)
	}
}

// OAuth 身分不會因為這次改動而多出 secret_hash,也不能用本地登入進來。
//
// 查詢把 provider = 'local' 寫死在 WHERE 裡就是為了這件事:少了它,
// 一個 Discord 帳號的 snowflake 就成了可猜測的登入名。
func TestOAuthIdentityHasNoLocalSecret(t *testing.T) {
	setup(t)

	discordID := fake.newAccount(t, "no-secret")
	sess := login(t, discordID, identity.DeviceInfo{})

	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE user_id = $1 AND secret_hash IS NOT NULL`,
		sess.UserID); n != 0 {
		t.Fatal("OAuth 登入不該寫入 secret_hash")
	}
	// 就算硬把 Discord 身分塞一個雜湊進去,本地登入也不該認它。
	if _, err := pool.Exec(context.Background(),
		`UPDATE platform.identities SET secret_hash = $1
		 WHERE provider = 'discord' AND provider_user_id = $2`,
		mustHash(t, "ZZZZ2345YYYY"), discordID); err != nil {
		t.Fatalf("塞雜湊: %v", err)
	}
	_, err := svc.LocalLogin(context.Background(), discordID, "ZZZZ2345YYYY", identity.DeviceInfo{})
	if !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("Discord 身分不該能走本地登入,得到 %v", err)
	}
}

func mustHash(t *testing.T, passcode string) string {
	t.Helper()
	h, err := secret.NewHasher(testHashIterations)
	if err != nil {
		t.Fatalf("建立雜湊器: %v", err)
	}
	encoded, err := h.Hash(passcode)
	if err != nil {
		t.Fatalf("雜湊: %v", err)
	}
	return encoded
}
