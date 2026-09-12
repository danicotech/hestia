package authzpg_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/authz"
	"github.com/danicotech/hestia/internal/infrastructure/storage/authzpg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
)

// 整合測試對真 Postgres 跑:授權的真實內容(seed 的角色權限、部分索引、
// COALESCE 唯一索引、now() 的時間比較)都在資料庫裡,mock 掉就等於沒測。
// 期望值一律**從 seed 讀出來**,不在測試裡重抄一份角色權限表。

var (
	pool *pgxpool.Pool
	svc  *authzpg.Service
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Short() {
		p, cleanup, err := testdb.StartShared(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "testdb:", err)
			os.Exit(1)
		}
		pool = p
		svc = authzpg.New(pool)
		code := m.Run()
		cleanup()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func setup(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("需要 Docker,-short 模式跳過")
	}
}

// ── 測試資料 ────────────────────────────────────────────────────────

func newUser(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id) VALUES (gen_random_uuid()::text) RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試使用者: %v", err)
	}
	return id
}

func newCommunity(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.communities (public_id, name)
		 VALUES (gen_random_uuid()::text, 'test') RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建測試社群: %v", err)
	}
	return id
}

// grantRole 授予角色。communityID 為 nil = 全域;expiresAt 為 nil = 永久。
func grantRole(t *testing.T, userID int64, roleKey string, communityID *int64, expiresAt *time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_roles (user_id, role_id, community_id, source, expires_at)
		 SELECT $1, r.id, $2, 'manual', $3 FROM platform.roles r WHERE r.key = $4`,
		userID, communityID, expiresAt, roleKey)
	if err != nil {
		t.Fatalf("授予角色 %s: %v", roleKey, err)
	}
}

// restrict 建立一筆停權。expiresAt / liftedAt 為 nil = 永久 / 未解除。
func restrict(t *testing.T, userID int64, kind string, expiresAt, liftedAt *time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO platform.user_restrictions (user_id, kind, reason, created_by, expires_at, lifted_at)
		 VALUES ($1, $2, 'test', $1, $3, $4)`,
		userID, kind, expiresAt, liftedAt)
	if err != nil {
		t.Fatalf("建立停權 %s: %v", kind, err)
	}
}

// seedPermissions 讀 migration 00014 seed 的實際內容:role key → 權限集合。
// 期望值的唯一來源(測試裡不重抄一份,抄了就是第二個權威)。
func seedPermissions(t *testing.T) map[string]map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT r.key, rp.permission
		   FROM platform.roles r
		   JOIN platform.role_permissions rp ON rp.role_id = r.id`)
	if err != nil {
		t.Fatalf("讀 seed 權限: %v", err)
	}
	defer rows.Close()

	out := map[string]map[string]bool{}
	for rows.Next() {
		var key, perm string
		if err := rows.Scan(&key, &perm); err != nil {
			t.Fatalf("掃描 seed 權限: %v", err)
		}
		if out[key] == nil {
			out[key] = map[string]bool{}
		}
		out[key][perm] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("讀 seed 權限: %v", err)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func allowed(t *testing.T, s *authzpg.Service, userID int64, procedure string) bool {
	t.Helper()
	err := s.Authorize(context.Background(), userID, procedure)
	if err == nil {
		return true
	}
	if !errors.Is(err, authz.ErrPermissionDenied) {
		t.Fatalf("Authorize 回了非授權錯誤: %v", err)
	}
	return false
}

// ── 程式碼常數 vs seed 的漂移 ───────────────────────────────────────

// 權限字串寫在程式碼、對應存在 DB,兩邊必須完全一致。
// 這個測試是「不要發明新權限字串」這條規則的自動化版本。
func TestPermissionConstantsMatchSeed(t *testing.T) {
	setup(t)

	inSeed := map[string]bool{}
	total := 0
	for _, perms := range seedPermissions(t) {
		for p := range perms {
			inSeed[p] = true
			total++
		}
	}
	// 24 = schemas/03-authz.md 的 21 組,加上 00030 給 owner / admin / judge
	// 各一個 tournament.judge。
	if total != 24 {
		t.Errorf("seed 應有 24 組角色權限,實際 %d(schemas/03-authz.md + 00030)", total)
	}

	inCode := map[string]bool{}
	for _, p := range []authz.Permission{
		authz.PermEconomyGrant, authz.PermEconomyRefund, authz.PermEconomyConfig,
		authz.PermShopManage, authz.PermRedemptionHandle, authz.PermUserRestrict,
		authz.PermRolesManage, authz.PermLogsRead, authz.PermLogsReadDeleted,
		authz.PermTournamentJudge,
	} {
		inCode[string(p)] = true
	}

	for p := range inSeed {
		if !inCode[p] {
			t.Errorf("seed 有權限 %q,程式碼沒有對應常數", p)
		}
	}
	for p := range inCode {
		if !inSeed[p] {
			t.Errorf("程式碼常數 %q 不在 seed 裡(不要發明新權限字串)", p)
		}
	}

	// 映射表用到的權限也必須是 seed 有的,否則那個 procedure 永遠沒人能過。
	for proc, perm := range authz.ProcedurePermissions() {
		if !inSeed[string(perm)] {
			t.Errorf("procedure %s 映射到 seed 沒有的權限 %q", proc, perm)
		}
	}
}

// 映射表必須涵蓋每一支需要權限的 RPC,而且不能多出沒人認得的條目
// (啟動時斷言的測試版)。漏一條等於那支 RPC 沒有權限把關。
func TestProcedureMappingCoversPrivilegedServices(t *testing.T) {
	want := []string{
		authz.ProcAdminEconomyGrant,
		authz.ProcAdminEconomyDeduct,
		authz.ProcAdminEconomyRefund,
		authz.ProcAdminEconomyHandleRedemption,
		authz.ProcAdminEconomyListEntries,

		authz.ProcJudgeCreateTournament,
		authz.ProcJudgeAdvancePhase,
		authz.ProcJudgeAssignRank,
		authz.ProcJudgeListUnranked,
		authz.ProcJudgeDrawBracket,
		authz.ProcJudgeSwapSeeds,
		authz.ProcJudgeConfirmBracket,
		authz.ProcJudgeOpenHandicap,
		authz.ProcJudgeLockHandicap,
		authz.ProcJudgeSetStreamUrl,
		authz.ProcJudgeStartMatch,
		authz.ProcJudgeReportResult,
		authz.ProcJudgeWithdrawPlayer,
		authz.ProcJudgeRegeneratePasscode,
		authz.ProcJudgeAwardPrizes,
	}
	got := authz.ProcedurePermissions()
	if len(got) != len(want) {
		t.Errorf("映射表有 %d 條,預期 %d 條", len(got), len(want))
	}
	for _, proc := range want {
		if _, ok := got[proc]; !ok {
			t.Errorf("映射表缺 %s", proc)
		}
	}
	// 回傳的是副本:改它不能影響真正的判定。
	got["/hestia.platform.v1.AdminEconomyService/Grant"] = "bogus"
	if p, _ := authz.PermissionFor(authz.ProcAdminEconomyGrant); p != authz.PermEconomyGrant {
		t.Errorf("ProcedurePermissions 回的不是副本,映射表被改成 %q", p)
	}
}

// ── seed 五角色 × 全部 procedure ────────────────────────────────────

func TestSeedRolesAgainstProcedures(t *testing.T) {
	setup(t)

	seed := seedPermissions(t)
	roles := make([]string, 0, len(seed))
	for k := range seed {
		roles = append(roles, k)
	}
	sort.Strings(roles)
	if len(roles) != 5 {
		t.Fatalf("seed 應有 5 個角色,實際 %v", roles)
	}

	mapping := authz.ProcedurePermissions()
	procs := make([]string, 0, len(mapping))
	for p := range mapping {
		procs = append(procs, p)
	}
	sort.Strings(procs)

	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			u := newUser(t)
			grantRole(t, u, role, nil, nil)
			for _, proc := range procs {
				want := seed[role][string(mapping[proc])]
				if got := allowed(t, svc, u, proc); got != want {
					t.Errorf("%s 對 %s:got allowed=%v, seed 期望 %v(權限 %s)",
						role, proc, got, want, mapping[proc])
				}
			}
		})
	}
}

// 沒有任何角色的一般使用者:全部拒絕。
func TestUserWithoutRoleDeniedEverywhere(t *testing.T) {
	setup(t)
	u := newUser(t)
	for proc := range authz.ProcedurePermissions() {
		if allowed(t, svc, u, proc) {
			t.Errorf("無角色使用者不該通過 %s", proc)
		}
	}
}

// 過期的 user_roles 不算。
func TestExpiredRoleDenied(t *testing.T) {
	setup(t)

	expired := newUser(t)
	grantRole(t, expired, "owner", nil, ptr(time.Now().Add(-time.Minute)))
	if allowed(t, svc, expired, authz.ProcAdminEconomyGrant) {
		t.Error("已過期的角色不該通過")
	}

	future := newUser(t)
	grantRole(t, future, "owner", nil, ptr(time.Now().Add(time.Hour)))
	if !allowed(t, svc, future, authz.ProcAdminEconomyGrant) {
		t.Error("未到期的角色應該通過")
	}
}

// ── community 範圍 ──────────────────────────────────────────────────

// 全域角色(community_id NULL)在任何範圍都成立;
// 特定 community 的角色只在該 community 的範圍內成立。
func TestGlobalAndCommunityScopedRoles(t *testing.T) {
	setup(t)

	c1 := newCommunity(t)
	c2 := newCommunity(t)
	scoped1 := authzpg.NewScoped(pool, c1)
	scoped2 := authzpg.NewScoped(pool, c2)

	global := newUser(t)
	grantRole(t, global, "owner", nil, nil)
	for name, s := range map[string]*authzpg.Service{"unscoped": svc, "c1": scoped1, "c2": scoped2} {
		if !allowed(t, s, global, authz.ProcAdminEconomyGrant) {
			t.Errorf("全域角色在 %s 範圍應通過", name)
		}
	}

	local := newUser(t)
	grantRole(t, local, "owner", &c1, nil)
	if !allowed(t, svc, local, authz.ProcAdminEconomyGrant) {
		t.Error("M1 不限範圍時,community 角色應通過")
	}
	if !allowed(t, scoped1, local, authz.ProcAdminEconomyGrant) {
		t.Error("同 community 範圍應通過")
	}
	if allowed(t, scoped2, local, authz.ProcAdminEconomyGrant) {
		t.Error("別的 community 的角色不該在本 community 生效")
	}
}

// ── 否決條件 ────────────────────────────────────────────────────────

// 軟刪除的使用者一律拒絕,即使角色還在。
func TestSoftDeletedUserDenied(t *testing.T) {
	setup(t)
	u := newUser(t)
	grantRole(t, u, "owner", nil, nil)
	if !allowed(t, svc, u, authz.ProcAdminEconomyGrant) {
		t.Fatal("前置:owner 應該先能通過")
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE platform.users SET deleted_at = now() WHERE id = $1`, u); err != nil {
		t.Fatalf("軟刪除: %v", err)
	}
	if allowed(t, svc, u, authz.ProcAdminEconomyGrant) {
		t.Error("軟刪除的使用者不該通過")
	}
}

// full_ban 生效中一律拒絕(封鎖的人不該還能用管理權限);
// 已解除 / 已過期的限制、以及其他等級的限制都不影響授權。
func TestFullBanOverridesRole(t *testing.T) {
	setup(t)

	cases := []struct {
		name      string
		kind      string
		expiresAt *time.Time
		liftedAt  *time.Time
		want      bool
	}{
		{"生效中的 full_ban", "full_ban", nil, nil, false},
		{"未到期的 full_ban", "full_ban", ptr(time.Now().Add(time.Hour)), nil, false},
		{"已解除的 full_ban", "full_ban", nil, ptr(time.Now().Add(-time.Minute)), true},
		{"已過期的 full_ban", "full_ban", ptr(time.Now().Add(-time.Minute)), nil, true},
		{"no_trade 不影響授權", "no_trade", nil, nil, true},
		{"no_earn 不影響授權", "no_earn", nil, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUser(t)
			grantRole(t, u, "owner", nil, nil)
			restrict(t, u, c.kind, c.expiresAt, c.liftedAt)
			if got := allowed(t, svc, u, authz.ProcAdminEconomyGrant); got != c.want {
				t.Errorf("got allowed=%v, want %v", got, c.want)
			}
		})
	}
}

// 多筆限制:只要有一筆生效中的 full_ban 就拒絕。
func TestFullBanAmongLiftedOnesStillDenies(t *testing.T) {
	setup(t)
	u := newUser(t)
	grantRole(t, u, "owner", nil, nil)
	restrict(t, u, "full_ban", nil, ptr(time.Now().Add(-time.Hour)))
	restrict(t, u, "full_ban", nil, nil)
	if allowed(t, svc, u, authz.ProcAdminEconomyGrant) {
		t.Error("有一筆生效中的 full_ban 就該拒絕")
	}
}

// ── fail closed ────────────────────────────────────────────────────

// 未登記的 procedure 一律拒絕,連 owner 也是。
func TestUnregisteredProcedureDenied(t *testing.T) {
	setup(t)
	u := newUser(t)
	grantRole(t, u, "owner", nil, nil)
	for _, proc := range []string{
		"",
		"/hestia.platform.v1.AdminEconomyService/BrandNewRPC",
		"/hestia.platform.v1.AdminSomethingService/Delete",
		"/hestia.platform.v1.adminEconomyService/Grant", // 大小寫不同 = 不同 procedure
		"hestia.platform.v1.AdminEconomyService/Grant",  // 少了前導斜線
	} {
		err := svc.Authorize(context.Background(), u, proc)
		if !errors.Is(err, authz.ErrPermissionDenied) {
			t.Errorf("未登記的 procedure %q 應回 ErrPermissionDenied,got %v", proc, err)
		}
	}
}

// user id 為 0(上游沒解出身分)不該撞上任何資料。
func TestZeroUserIDDenied(t *testing.T) {
	setup(t)
	for _, id := range []int64{0, -1} {
		if err := svc.Authorize(context.Background(), id, authz.ProcAdminEconomyGrant); !errors.Is(err, authz.ErrPermissionDenied) {
			t.Errorf("user_id=%d 應拒絕,got %v", id, err)
		}
	}
}

// ── 併發 ────────────────────────────────────────────────────────────

// 授權是唯讀的,但它在每個管理請求的路徑上被高頻並行呼叫:
// 驗證沒有共享狀態(不同使用者/procedure 的結果不會互相污染)。
func TestConcurrentAuthorize(t *testing.T) {
	setup(t)

	c := newCommunity(t)
	scoped := authzpg.NewScoped(pool, c)

	owner := newUser(t)
	grantRole(t, owner, "owner", nil, nil)
	auditor := newUser(t)
	grantRole(t, auditor, "auditor", nil, nil)
	banned := newUser(t)
	grantRole(t, banned, "owner", nil, nil)
	restrict(t, banned, "full_ban", nil, nil)
	plain := newUser(t)

	cases := []struct {
		svc  *authzpg.Service
		user int64
		proc string
		want bool
	}{
		{svc, owner, authz.ProcAdminEconomyGrant, true},
		{svc, owner, authz.ProcAdminEconomyListEntries, true},
		{svc, auditor, authz.ProcAdminEconomyListEntries, true},
		{svc, auditor, authz.ProcAdminEconomyGrant, false},
		{svc, banned, authz.ProcAdminEconomyGrant, false},
		{svc, plain, authz.ProcAdminEconomyRefund, false},
		{scoped, owner, authz.ProcAdminEconomyRefund, true},
		{svc, owner, "/hestia.platform.v1.AdminEconomyService/Unknown", false},
	}

	const rounds = 25
	var wg sync.WaitGroup
	errCh := make(chan string, len(cases)*rounds)
	for range rounds {
		for i, c := range cases {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := c.svc.Authorize(context.Background(), c.user, c.proc)
				got := err == nil
				if !got && !errors.Is(err, authz.ErrPermissionDenied) {
					errCh <- fmt.Sprintf("case %d 非授權錯誤: %v", i, err)
					return
				}
				if got != c.want {
					errCh <- fmt.Sprintf("case %d(user=%d proc=%s):got %v want %v", i, c.user, c.proc, got, c.want)
				}
			}()
		}
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}
