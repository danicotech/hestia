package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// 選手 session 是純運算加一次查詢,沒有真的資料庫參與,所以這組測試
// 不需要容器也不吃 -short。會打到資料庫的是 adapter,那一層自己測。

const (
	testSecret = "0123456789abcdef0123456789abcdef" // 32 bytes,測試用
	testSlug   = "2026-baiye-shifeng"
	testPlayer = "01JBXPLAYER0000000000000001"
)

var testNow = time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC)

// fakeRepo 是 tournament_players.passcode_issued_at 的記憶體版本。
type fakeRepo struct {
	issued map[string]time.Time
	err    error
	calls  int
}

func newRepo(issuedAt time.Time) *fakeRepo {
	return &fakeRepo{issued: map[string]time.Time{key(testSlug, testPlayer): issuedAt}}
}

func key(slug, player string) string { return slug + "|" + player }

func (r *fakeRepo) PasscodeIssuedAt(_ context.Context, slug, player string) (time.Time, error) {
	r.calls++
	if r.err != nil {
		return time.Time{}, r.err
	}
	t, ok := r.issued[key(slug, player)]
	if !ok {
		return time.Time{}, tournament.ErrPlayerNotFound
	}
	return t, nil
}

// rotate 模擬裁判按下「重新產生通行碼」:UPDATE 同時改 hash 與 issued_at。
func (r *fakeRepo) rotate(at time.Time) {
	r.issued[key(testSlug, testPlayer)] = at
}

// clock 是可推動的測試時鐘。
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newSigner(t *testing.T) *identity.Signer {
	t.Helper()
	s, err := identity.NewSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func newService(t *testing.T, repo Repo, c *clock) *Service {
	t.Helper()
	return New(newSigner(t), repo, WithClock(c.now))
}

func issue(t *testing.T, svc *Service) (string, time.Time) {
	t.Helper()
	tok, exp, err := svc.Issue(context.Background(), Identity{
		TournamentSlug: testSlug, PlayerPublicID: testPlayer,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok, exp
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	c := &clock{t: testNow}
	svc := newService(t, newRepo(testNow.Add(-time.Hour)), c)

	tok, exp := issue(t, svc)
	if got := exp.Sub(testNow); got != TTL {
		t.Fatalf("session 壽命 = %v,要 %v", got, TTL)
	}
	if !strings.HasPrefix(tok, "v1.") || strings.Count(tok, ".") != 2 {
		t.Fatalf("格式應為 v1.<claims>.<sig>,得到 %q", tok)
	}
	// token 會躺在每一個請求的 cookie 裡,長度要留在合理範圍。
	if len(tok) > 512 {
		t.Fatalf("token 不該這麼長:%d bytes", len(tok))
	}

	id, err := svc.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.TournamentSlug != testSlug || id.PlayerPublicID != testPlayer {
		t.Fatalf("身分不符: %+v", id)
	}
	if !id.Valid() {
		t.Fatal("回來的身分應該是完整的")
	}
}

// token 由瀏覽器持有,內部 BIGINT id 進去就等於進了對外契約(專案鐵則 5)。
// 這個測試把 claims 的欄位集合釘死:日後有人想「順手」把 player_id 塞進來,
// 會在這裡被擋下。
func TestClaimsCarryNoInternalIDs(t *testing.T) {
	c := &clock{t: testNow}
	svc := newService(t, newRepo(testNow), c)
	tok, _ := issue(t, svc)

	raw := decodeClaims(t, tok)
	got := make([]string, 0, len(raw))
	for k := range raw {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"exp", "iat", "pat", "plr", "tnt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("claims 欄位 = %v,要 %v", got, want)
	}
	if raw["plr"] != testPlayer || raw["tnt"] != testSlug {
		t.Fatalf("claims 內容不符: %v", raw)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	c := &clock{t: testNow}
	repo := newRepo(testNow)
	svc := newService(t, repo, c)
	tok, _ := issue(t, svc)

	// 到期前一秒仍然有效
	c.add(TTL - time.Second)
	if _, err := svc.Verify(context.Background(), tok); err != nil {
		t.Fatalf("未到期應通過: %v", err)
	}
	// 剛好到期的那一秒就不接受(>=,不是 >),與 VerifyAccess 同一條規則
	c.add(time.Second)
	if _, err := svc.Verify(context.Background(), tok); !errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("到期時刻要回 ErrTokenExpired,得到 %v", err)
	}
	c.add(24 * time.Hour)
	if _, err := svc.Verify(context.Background(), tok); !errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("過期要回 ErrTokenExpired,得到 %v", err)
	}
}

// 過期的 token 不該讓我們白白查一次資料庫(這支函式跑在未認證請求上)。
func TestVerifyDoesNotHitRepoOnBadToken(t *testing.T) {
	c := &clock{t: testNow}
	repo := newRepo(testNow)
	svc := newService(t, repo, c)
	tok, _ := issue(t, svc)

	before := repo.calls
	c.add(TTL)
	if _, err := svc.Verify(context.Background(), tok); !errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("要 ErrTokenExpired,得到 %v", err)
	}
	if _, err := svc.Verify(context.Background(), "v1.garbage.garbage"); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("要 ErrInvalidToken,得到 %v", err)
	}
	if repo.calls != before {
		t.Fatalf("驗不過的 token 不該查資料庫,多了 %d 次", repo.calls-before)
	}
}

func TestVerifyRejects(t *testing.T) {
	c := &clock{t: testNow}
	svc := newService(t, newRepo(testNow), c)
	good, _ := issue(t, svc)
	parts := strings.Split(good, ".")

	// 竄改 claims:把選手換成別人,沿用原簽章
	raw := decodeClaims(t, good)
	raw["plr"] = "01JBXATTACKER000000000000001"
	tampered := parts[0] + "." + encodeClaims(t, raw) + "." + parts[2]

	// 竄改 pat:把它改成 0(想讓「對帳」失去依據)
	raw2 := decodeClaims(t, good)
	raw2["pat"] = float64(0)
	zeroPat := parts[0] + "." + encodeClaims(t, raw2) + "." + parts[2]

	// 竄改簽章:翻掉一個 bit(直接改 base64 字元會踩到編碼多義性)
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("解簽章: %v", err)
	}
	sigBytes[0] ^= 0x01
	badSig := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sigBytes)

	// 換一把金鑰簽的 token
	other, err := identity.NewSigner([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	foreignSvc := New(other, newRepo(testNow), WithClock(c.now))
	foreign, _ := issue(t, foreignSvc)

	cases := []struct{ name, token string }{
		{"空字串", ""},
		{"只有版本", "v1."},
		{"錯誤版本前綴", "v2." + parts[1] + "." + parts[2]},
		{"無版本前綴", parts[1] + "." + parts[2]},
		{"竄改 claims", tampered},
		{"pat 被抹成 0", zeroPat},
		{"竄改簽章", badSig},
		{"別把金鑰簽的", foreign},
		{"簽章不是 base64url", parts[0] + "." + parts[1] + ".!!!!"},
		// 沒有長度上限的話,每個未認證請求都能叫我們做數 MB 的 base64 + HMAC
		{"超長 token", "v1." + strings.Repeat("A", 1<<20) + ".AAAA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Verify(context.Background(), tc.token); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("要 ErrInvalidToken,得到 %v", err)
			}
		})
	}
}

// 簽章正確但 claims 不完整,一樣不能過。
//
// 這種 token 只可能出自我們自己(簽章擋掉了所有外部來源),但「半個身分」
// 不該被任何地方當成已登入 —— 尤其 pat 缺席等於這張 session 沒有撤銷依據。
func TestVerifyRejectsIncompleteClaims(t *testing.T) {
	c := &clock{t: testNow}
	signer := newSigner(t)
	svc := New(signer, newRepo(testNow), WithClock(c.now))
	exp := testNow.Add(TTL).Unix()

	cases := []struct {
		name string
		c    claims
	}{
		{"缺賽事", claims{Plr: testPlayer, Pat: testNow.UnixMicro(), Exp: exp}},
		{"缺選手", claims{Tnt: testSlug, Pat: testNow.UnixMicro(), Exp: exp}},
		{"缺 pat", claims{Tnt: testSlug, Plr: testPlayer, Exp: exp}},
		{"全空", claims{Exp: exp}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := signer.Sign(TokenType, tc.c)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if _, err := svc.Verify(context.Background(), tok); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("要 ErrInvalidToken,得到 %v", err)
			}
		})
	}
}

// 用途分離:三種 token 互相拿去驗,一律失敗。
//
// 這是「拿 access token 當選手 session 用」的防線,而它不靠任何欄位比對 ——
// typ 混進被簽的訊息,跨用途就是簽章不符。
func TestTokenTypeSeparation(t *testing.T) {
	c := &clock{t: testNow}
	signer := newSigner(t)
	svc := New(signer, newRepo(testNow), WithClock(c.now))

	sessionTok, _ := issue(t, svc)
	accessTok, _, err := signer.SignAccess("01JBXUSER000000000000000001", "01JBXSESS000000000000000001", testNow)
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	stateTok, err := signer.SignState("/tournaments/"+testSlug, testNow)
	if err != nil {
		t.Fatalf("SignState: %v", err)
	}

	// access / state 拿來當選手 session
	for _, tc := range []struct{ name, token string }{
		{"access 當選手 session", accessTok},
		{"state 當選手 session", stateTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Verify(context.Background(), tc.token); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("要 ErrInvalidToken,得到 %v", err)
			}
		})
	}

	// 選手 session 拿去當 access / state
	if _, err := signer.VerifyAccess(sessionTok, testNow); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("選手 session 當 access 用要被拒,得到 %v", err)
	}
	if _, err := signer.VerifyState(sessionTok, sessionTok, testNow); !errors.Is(err, identity.ErrInvalidState) {
		t.Fatalf("選手 session 當 state 用要被拒,得到 %v", err)
	}

	// 另一個活動層用途也不能互換(日後多一種 token 時這條線仍在)
	var dst claims
	if err := signer.Verify("some_other_purpose", sessionTok, &dst); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("換一個用途驗要被拒,得到 %v", err)
	}
}

// 裁判按「重新產生通行碼」之後,用舊碼登入拿到的 session 必須當場失效。
// 補發通行碼的情境就是「原本那組可能落到別人手上」,這不是 nice-to-have。
func TestPasscodeRotationInvalidatesSession(t *testing.T) {
	c := &clock{t: testNow}
	repo := newRepo(testNow.Add(-24 * time.Hour))
	svc := newService(t, repo, c)

	tok, _ := issue(t, svc)
	if _, err := svc.Verify(context.Background(), tok); err != nil {
		t.Fatalf("換發前應通過: %v", err)
	}

	repo.rotate(testNow)
	_, err := svc.Verify(context.Background(), tok)
	if !errors.Is(err, ErrPasscodeRotated) {
		t.Fatalf("換發後要回 ErrPasscodeRotated,得到 %v", err)
	}
	// 對呼叫端而言這與「token 無效」是同一件事(errmap 據此回 Unauthenticated)
	if !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("ErrPasscodeRotated 必須包 identity.ErrInvalidToken,得到 %v", err)
	}

	// 換發之後重新登入拿到的新 session 要能用
	fresh, _ := issue(t, svc)
	if _, err := svc.Verify(context.Background(), fresh); err != nil {
		t.Fatalf("換發後的新 session 應通過: %v", err)
	}
	// 而舊的仍然死著 —— 新舊並存不是「最後一次換發後才失效」
	if _, err := svc.Verify(context.Background(), tok); !errors.Is(err, ErrPasscodeRotated) {
		t.Fatalf("舊 session 要持續失效,得到 %v", err)
	}
}

// 同一秒內的兩次換發也必須讓舊 session 失效。
// pat 若截到秒,這一格的舊 session 會活下來 —— 用微秒就是為了這個。
func TestRotationWithinSameSecondInvalidates(t *testing.T) {
	c := &clock{t: testNow}
	base := testNow.Add(-time.Hour)
	repo := newRepo(base)
	svc := newService(t, repo, c)

	tok, _ := issue(t, svc)
	repo.rotate(base.Add(time.Millisecond))
	if _, err := svc.Verify(context.Background(), tok); !errors.Is(err, ErrPasscodeRotated) {
		t.Fatalf("同秒內換發要讓舊 session 失效,得到 %v", err)
	}
}

// 選手棄賽、被刪、或 token 是上一屆的:查不到那一列就沒有身分可言。
func TestPlayerGone(t *testing.T) {
	c := &clock{t: testNow}
	repo := newRepo(testNow)
	svc := newService(t, repo, c)
	tok, _ := issue(t, svc)

	delete(repo.issued, key(testSlug, testPlayer))
	err := errFromVerify(t, svc, tok)
	if !errors.Is(err, ErrPlayerGone) || !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("要 ErrPlayerGone(且包 ErrInvalidToken),得到 %v", err)
	}

	// 簽發端同樣不該無中生有
	if _, _, err := svc.Issue(context.Background(), Identity{
		TournamentSlug: testSlug, PlayerPublicID: testPlayer,
	}); !errors.Is(err, ErrPlayerGone) {
		t.Fatalf("Issue 查無選手要回 ErrPlayerGone,得到 %v", err)
	}
}

// 資料庫掛掉不是憑證問題。折成 ErrInvalidToken 的話,一次故障會表現成
// 「全站選手同時被登出」,而所有人重新登入只會讓故障更嚴重。
func TestRepoFailureIsNotAuthFailure(t *testing.T) {
	c := &clock{t: testNow}
	repo := newRepo(testNow)
	svc := newService(t, repo, c)
	tok, _ := issue(t, svc)

	boom := errors.New("connection refused")
	repo.err = boom
	err := errFromVerify(t, svc, tok)
	if !errors.Is(err, boom) {
		t.Fatalf("要原樣回資料庫錯誤,得到 %v", err)
	}
	if errors.Is(err, identity.ErrInvalidToken) || errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("資料庫錯誤不可折成憑證錯誤: %v", err)
	}
}

func TestIssueRejectsIncompleteIdentity(t *testing.T) {
	c := &clock{t: testNow}
	repo := newRepo(testNow)
	svc := newService(t, repo, c)

	cases := []struct {
		name string
		id   Identity
	}{
		{"兩者皆空", Identity{}},
		{"缺賽事", Identity{PlayerPublicID: testPlayer}},
		{"缺選手", Identity{TournamentSlug: testSlug}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := svc.Issue(context.Background(), tc.id); !errors.Is(err, ErrInvalidIdentity) {
				t.Fatalf("要 ErrInvalidIdentity,得到 %v", err)
			}
		})
	}
	if repo.calls != 0 {
		t.Fatalf("身分不完整時不該查資料庫,查了 %d 次", repo.calls)
	}
}

// passcode_issued_at 是 NOT NULL,零值只可能是實作有問題。
// 寧可簽不出來,也不要簽一張 pat 對不上任何東西(= 永遠不會失效)的 token。
func TestIssueRejectsZeroPasscodeTime(t *testing.T) {
	c := &clock{t: testNow}
	svc := newService(t, newRepo(time.Time{}), c)

	if _, _, err := svc.Issue(context.Background(), Identity{
		TournamentSlug: testSlug, PlayerPublicID: testPlayer,
	}); !errors.Is(err, ErrPasscodeTimeUnavailable) {
		t.Fatalf("要 ErrPasscodeTimeUnavailable,得到 %v", err)
	}
}

// 錯誤訊息會進 log、進 APM、可能被回給客戶端 —— 任何一處含 token 都等於憑證外洩。
func TestErrorsNeverLeakToken(t *testing.T) {
	c := &clock{t: testNow}
	svc := newService(t, newRepo(testNow), c)
	tok, _ := issue(t, svc)

	c.add(TTL + time.Hour)
	assertNoSecret(t, errFromVerify(t, svc, tok), tok, testSecret)

	c.t = testNow
	assertNoSecret(t, errFromVerify(t, svc, tok+"x"), tok, testSecret)
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

func errFromVerify(t *testing.T, svc *Service, token string) error {
	t.Helper()
	id, err := svc.Verify(context.Background(), token)
	if err == nil {
		t.Fatalf("預期驗證失敗,卻回了 %+v", id)
	}
	if id.Valid() {
		t.Fatalf("失敗時不該回身分: %+v", id)
	}
	return err
}

func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token 格式不符: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解 claims: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("反序列化 claims: %v", err)
	}
	return m
}

func encodeClaims(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化 claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
