package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/shared/secret"
)

// create-judge 的價值在於「跑完之後那個人真的登得進去、而且按得下判勝負」,
// 所以驗的是資料庫裡的四樣東西 + 印出來的通行碼真的對得上存下來的雜湊,
// 不是指令有沒有回 nil。
//
// 全部子測試共用一個容器:建帳號用的是正式迭代數(600k),
// 容器起停比雜湊還貴,沒有理由為每個案例各起一個。

// captureStdout 收走 create-judge 印出來的東西。
//
// 非測不可:明碼通行碼**只存在於那一行輸出裡**(DB 只有雜湊),
// 印錯、印成雜湊、或印出一組跟存進去的不一樣的碼,都不會有任何其他症狀 ——
// 直到賽事當天裁判登不進去。
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = orig
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("讀 stdout: %v", err)
	}
	_ = r.Close()
	return string(out), runErr
}

// passcodeFrom 從輸出裡撈出「通行碼  XXXX」那一行的值。
func passcodeFrom(t *testing.T, out string) string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "通行碼"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("輸出裡找不到通行碼:\n%s", out)
	return ""
}

// storedHash 讀回 identities.secret_hash(測試絕不比對明碼,DB 裡也沒有明碼)。
func storedHash(t *testing.T, pool *pgxpool.Pool, loginName string) string {
	t.Helper()
	var hash *string
	err := pool.QueryRow(context.Background(),
		`SELECT secret_hash FROM platform.identities
		 WHERE provider = 'local' AND provider_user_id = $1`, loginName).Scan(&hash)
	if err != nil {
		t.Fatalf("讀 secret_hash: %v", err)
	}
	if hash == nil {
		t.Fatal("local 身分的 secret_hash 不該是 NULL")
	}
	return *hash
}

func verifyPasscode(t *testing.T, hash, passcode string) bool {
	t.Helper()
	h, err := secret.NewHasher(0)
	if err != nil {
		t.Fatalf("建立雜湊器: %v", err)
	}
	ok, err := h.Verify(hash, secret.NormalizePasscode(passcode))
	if err != nil {
		t.Fatalf("驗證通行碼: %v", err)
	}
	return ok
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("查詢失敗: %v\nSQL: %s", err, sql)
	}
	return n
}

func TestCreateJudge(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()

	t.Run("一次做完四件事", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return createJudge(ctx, pool, []string{"--login", "lin", "--name", "林裁判"})
		})
		if err != nil {
			t.Fatalf("create-judge: %v", err)
		}

		// 1) users
		var userID int64
		var publicID string
		if err := pool.QueryRow(ctx,
			`SELECT u.id, u.public_id FROM platform.users u
			 JOIN platform.identities i ON i.user_id = u.id
			 WHERE i.provider = 'local' AND i.provider_user_id = 'lin'`).Scan(&userID, &publicID); err != nil {
			t.Fatalf("查不到裁判的 users 列: %v", err)
		}
		if len(publicID) != 26 {
			t.Errorf("public_id 應該是 26 字元 ULID,got %q", publicID)
		}
		// 對外只給 public_id,內部 BIGINT 不該出現在輸出裡(專案鐵則 5)。
		if strings.Contains(out, "\n  使用者  "+publicID) == false {
			t.Errorf("輸出應該印出 public_id:\n%s", out)
		}

		// 2) identities(provider='local' 且有雜湊)
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.identities
			 WHERE user_id = $1 AND provider = 'local' AND secret_hash IS NOT NULL`, userID); n != 1 {
			t.Errorf("identities 應有 1 列本地身分,實際 %d", n)
		}

		// 3) judge 角色(全域、manual)
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.user_roles ur
			 JOIN platform.roles r ON r.id = ur.role_id
			 WHERE ur.user_id = $1 AND r.key = 'judge'
			   AND ur.community_id IS NULL AND ur.source = 'manual'`, userID); n != 1 {
			t.Errorf("應授予 judge 角色,實際 %d 列", n)
		}
		// 順帶確認這個角色真的帶得動 tournament.judge 權限(migration 00030)。
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.user_roles ur
			 JOIN platform.role_permissions rp ON rp.role_id = ur.role_id
			 WHERE ur.user_id = $1 AND rp.permission = 'tournament.judge'`, userID); n == 0 {
			t.Error("judge 角色沒有帶到 tournament.judge 權限")
		}

		// 4) 隱私設定(與 OAuth 首登同一套預設)
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.user_privacy_settings WHERE user_id = $1`, userID); n != 1 {
			t.Errorf("user_privacy_settings 應有 1 列,實際 %d", n)
		}

		// 印出來的通行碼必須真的是存進去的那一組。
		passcode := passcodeFrom(t, out)
		if len(passcode) != judgePasscodeLength {
			t.Errorf("通行碼長度 = %d,要 %d:%q", len(passcode), judgePasscodeLength, passcode)
		}
		for _, r := range passcode {
			if strings.ContainsRune("01OIl", r) {
				t.Errorf("通行碼含易混淆字元 %q:%q", r, passcode)
			}
		}
		if !verifyPasscode(t, storedHash(t, pool, "lin"), passcode) {
			t.Fatal("印出來的通行碼驗不過存下來的雜湊 —— 裁判永遠登不進去")
		}
		// 明碼絕不進 DB。
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.identities WHERE secret_hash LIKE '%' || $1 || '%'`,
			passcode); n != 0 {
			t.Fatal("雜湊字串裡出現明碼")
		}
	})

	t.Run("登入名轉小寫", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return createJudge(ctx, pool, []string{"--login", "  WANG  "})
		})
		if err != nil {
			t.Fatalf("create-judge: %v", err)
		}
		if !strings.Contains(out, "登入名  wang") {
			t.Errorf("登入名應正規化成小寫:\n%s", out)
		}
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.identities WHERE provider = 'local' AND provider_user_id = 'wang'`); n != 1 {
			t.Errorf("DB 裡應該是正規化後的登入名,實際 %d 列", n)
		}
	})

	t.Run("登入名不合法就擋在 DB 之前", func(t *testing.T) {
		before := countRows(t, pool, `SELECT count(*) FROM platform.users`)
		for _, bad := range []string{"", "ab", "有中文", "with space", "UPPER@dom", strings.Repeat("a", 65)} {
			if err := createJudge(ctx, pool, []string{"--login", bad}); err == nil {
				t.Errorf("%q 應該被拒絕", bad)
			}
		}
		if after := countRows(t, pool, `SELECT count(*) FROM platform.users`); after != before {
			t.Fatalf("被拒的輸入不該留下 users 列(%d → %d)", before, after)
		}
	})

	t.Run("登入名重複要報錯且不留殘骸", func(t *testing.T) {
		before := countRows(t, pool, `SELECT count(*) FROM platform.users`)
		err := createJudge(ctx, pool, []string{"--login", "lin"})
		if err == nil {
			t.Fatal("重複的登入名應該報錯")
		}
		if !strings.Contains(err.Error(), "--reset-passcode") {
			t.Errorf("錯誤訊息該指出怎麼修:%v", err)
		}
		// 建 users 與建 identities 同一個 transaction:撞唯一鍵時
		// 那一列 users 必須跟著回滾,否則會留下一個登不進去的幽靈帳號。
		if after := countRows(t, pool, `SELECT count(*) FROM platform.users`); after != before {
			t.Fatalf("撞鍵時留下了殘骸(%d → %d)", before, after)
		}
	})

	t.Run("reset-passcode 換碼但不換帳號", func(t *testing.T) {
		var userIDBefore int64
		if err := pool.QueryRow(ctx,
			`SELECT user_id FROM platform.identities WHERE provider = 'local' AND provider_user_id = 'lin'`,
		).Scan(&userIDBefore); err != nil {
			t.Fatalf("讀 user_id: %v", err)
		}
		oldHash := storedHash(t, pool, "lin")

		out, err := captureStdout(t, func() error {
			return createJudge(ctx, pool, []string{"--login", "lin", "--reset-passcode"})
		})
		if err != nil {
			t.Fatalf("reset-passcode: %v", err)
		}
		newPasscode := passcodeFrom(t, out)
		newHash := storedHash(t, pool, "lin")
		if newHash == oldHash {
			t.Fatal("雜湊沒有換")
		}
		if !verifyPasscode(t, newHash, newPasscode) {
			t.Fatal("印出來的新通行碼驗不過新的雜湊")
		}

		var userIDAfter int64
		if err := pool.QueryRow(ctx,
			`SELECT user_id FROM platform.identities WHERE provider = 'local' AND provider_user_id = 'lin'`,
		).Scan(&userIDAfter); err != nil {
			t.Fatalf("讀 user_id: %v", err)
		}
		if userIDAfter != userIDBefore {
			t.Fatal("重設通行碼不該換掉平台帳號 —— 稽核紀錄都掛在那個 id 上")
		}
		// 角色不該被授予第二次(user_roles_uniq + ON CONFLICT DO NOTHING)。
		if n := countRows(t, pool,
			`SELECT count(*) FROM platform.user_roles WHERE user_id = $1`, userIDBefore); n != 1 {
			t.Errorf("user_roles 應該仍是 1 列,實際 %d", n)
		}
	})

	t.Run("reset-passcode 對不存在的帳號等於建立", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return createJudge(ctx, pool, []string{"--login", "chen", "--reset-passcode"})
		})
		if err != nil {
			t.Fatalf("create-judge: %v", err)
		}
		if !verifyPasscode(t, storedHash(t, pool, "chen"), passcodeFrom(t, out)) {
			t.Fatal("新建帳號的通行碼驗不過")
		}
	})
}
