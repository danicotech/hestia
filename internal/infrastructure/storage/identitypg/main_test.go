package identitypg_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/infrastructure/storage/identitypg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/ledgerpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑(ledger-invariants:mock 掉資料庫的併發測試沒有意義——
// 要測的正是唯一索引與鎖的行為)。Discord 用 httptest 假冒:
// 打真的 Discord 既不可重現、會被 rate limit,更不該把真憑證放進測試。

const (
	testJWTSecret = "test-jwt-secret-0123456789abcdef" // 32 bytes
	testEncKey    = "test-token-enc-key-0123456789abc" // 32 bytes = AES-256
	testClientID  = "test-client-id"
	testSecret    = "test-client-secret"
	testRedirect  = "https://example.test/auth/callback"
)

var (
	pool   *pgxpool.Pool
	svc    *identitypg.Service
	signer *identity.Signer
	fake   *fakeDiscord
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	p, cleanup, err := testdb.StartShared(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "testdb:", err)
		os.Exit(1)
	}
	pool = p

	fake = newFakeDiscord()
	signer, err = identity.NewSigner([]byte(testJWTSecret))
	if err != nil {
		fmt.Fprintln(os.Stderr, "signer:", err)
		os.Exit(1)
	}
	svc, err = identitypg.New(pool, ledgerpg.New(pool), signer, identitypg.Config{
		ClientID:     testClientID,
		ClientSecret: testSecret,
		RedirectURI:  testRedirect,
		Scopes:       []string{"identify", "email"},
		Endpoints:    fake.endpoints(),
		TokenEncKey:  []byte(testEncKey),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "identitypg.New:", err)
		os.Exit(1)
	}

	code := m.Run()
	fake.Close()
	cleanup()
	os.Exit(code)
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// ── 假 Discord ────────────────────────────────────────────────────────────

type fakeProfile struct {
	ID         string
	Username   string
	GlobalName string
	Email      string
	Avatar     string
}

type fakeDiscord struct {
	*httptest.Server
	mu       sync.Mutex
	profiles map[string]*fakeProfile // discord id → 資料
	codes    map[string]string       // authorization code → discord id
	tokens   map[string]string       // access token → discord id
	seq      atomic.Int64
	// tokenStatus 非 0 時 /oauth2/token 一律回這個狀態碼(模擬 provider 失敗)
	tokenStatus int
}

func newFakeDiscord() *fakeDiscord {
	f := &fakeDiscord{
		profiles: map[string]*fakeProfile{},
		codes:    map[string]string{},
		tokens:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", f.handleToken)
	mux.HandleFunc("/api/users/@me", f.handleMe)
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakeDiscord) endpoints() identitypg.Endpoints {
	return identitypg.Endpoints{
		Authorize: f.URL + "/oauth2/authorize",
		Token:     f.URL + "/oauth2/token",
		API:       f.URL + "/api",
		CDN:       f.URL + "/cdn",
	}
}

// upsert 設定(或更新)一個 Discord 帳號的資料。
func (f *fakeDiscord) upsert(p fakeProfile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.profiles[p.ID] = &cp
}

// newAccount 建一個全新的 Discord 帳號,回傳 id。
func (f *fakeDiscord) newAccount(t *testing.T, username string) string {
	t.Helper()
	id := fmt.Sprintf("dc-%d", f.seq.Add(1))
	f.upsert(fakeProfile{ID: id, Username: username, Email: id + "@example.test", Avatar: "abc123"})
	return id
}

// issueCode 為某個帳號發一個一次性的 authorization code。
func (f *fakeDiscord) issueCode(discordID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := fmt.Sprintf("code-%d", f.seq.Add(1))
	f.codes[code] = discordID
	return code
}

func (f *fakeDiscord) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := f.tokenStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("client_id") != testClientID || r.PostForm.Get("client_secret") != testSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" ||
		r.PostForm.Get("redirect_uri") != testRedirect {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	discordID, ok := f.codes[r.PostForm.Get("code")]
	if ok {
		delete(f.codes, r.PostForm.Get("code")) // code 只能用一次,跟真的一樣
	}
	var access string
	if ok {
		access = fmt.Sprintf("dc-access-%d", f.seq.Add(1))
		f.tokens[access] = discordID
	}
	f.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"refresh_token": "dc-refresh-" + access,
		"token_type":    "Bearer",
		"scope":         "identify email",
		"expires_in":    604800,
	})
}

func (f *fakeDiscord) handleMe(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	discordID, ok := f.tokens[token]
	var p *fakeProfile
	if ok {
		p = f.profiles[discordID]
	}
	f.mu.Unlock()
	if p == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":          p.ID,
		"username":    p.Username,
		"global_name": p.GlobalName,
		"avatar":      p.Avatar,
		"email":       p.Email,
	})
}

// ── 共用輔助 ──────────────────────────────────────────────────────────────

// login 走完整個 StartLogin → CompleteLogin 流程。
func login(t *testing.T, discordID string, dev identity.DeviceInfo) *identity.Session {
	t.Helper()
	sess, err := tryLogin(discordID, dev)
	if err != nil {
		t.Fatalf("登入 %s: %v", discordID, err)
	}
	return sess
}

// tryLogin 模擬完整的瀏覽器流程:StartLogin 回的 state 同時進 URL 與 cookie,
// 回呼時兩者都帶回來(入口層負責寫 HttpOnly + SameSite=Lax cookie)。
func tryLogin(discordID string, dev identity.DeviceInfo) (*identity.Session, error) {
	_, state, err := svc.StartLogin("/lobby")
	if err != nil {
		return nil, err
	}
	return svc.CompleteLogin(context.Background(), fake.issueCode(discordID), state, state, dev)
}

func queryInt(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("查詢失敗: %v\nSQL: %s", err, sql)
	}
	return n
}

func configInt(t *testing.T, key string) int64 {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT value FROM platform.economy_configs
		 WHERE key = $1 AND effective_at <= now() ORDER BY effective_at DESC LIMIT 1`, key).Scan(&raw)
	if err != nil {
		t.Fatalf("讀設定 %s: %v", key, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("設定 %s 不是整數: %v", key, err)
	}
	return v
}

// sessionRow 是測試直接查 DB 的 session 快照(繞過服務,驗證真實落地狀態)。
type sessionRow struct {
	ID            int64
	PublicID      string
	RevokedReason *string
	RotatedFrom   *int64
	Revoked       bool
}

func loadSession(t *testing.T, publicID string) sessionRow {
	t.Helper()
	var r sessionRow
	err := pool.QueryRow(context.Background(),
		`SELECT id, public_id, revoked_reason, rotated_from, revoked_at IS NOT NULL
		 FROM platform.sessions WHERE public_id = $1`, publicID,
	).Scan(&r.ID, &r.PublicID, &r.RevokedReason, &r.RotatedFrom, &r.Revoked)
	if err != nil {
		t.Fatalf("讀 session %s: %v", publicID, err)
	}
	return r
}
