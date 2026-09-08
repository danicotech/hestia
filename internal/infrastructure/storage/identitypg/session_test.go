package identitypg_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// 正常輪替:舊 token 失效、新 token 可用、rotated_from 串起來。
func TestRefreshRotation(t *testing.T) {
	setup(t)
	ctx := context.Background()
	sess := login(t, fake.newAccount(t, "rot"), identity.DeviceInfo{UserAgent: "UA-1", IP: "198.51.100.7"})

	next, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{UserAgent: "UA-2", IP: "198.51.100.8"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if next.RefreshToken == sess.RefreshToken {
		t.Fatal("輪替必須換一個 refresh token")
	}
	if next.UserID != sess.UserID {
		t.Fatalf("使用者變了:%d → %d", sess.UserID, next.UserID)
	}
	if next.AccessToken == "" {
		t.Fatal("輪替要一併發新的 access token")
	}
	if uid, err := svc.Authenticate(ctx, next.AccessToken); err != nil || uid != sess.UserID {
		t.Fatalf("新 access token 應可用:uid=%d err=%v", uid, err)
	}

	// 舊列標 rotated,新列的 rotated_from 指向舊列
	oldRow := loadSession(t, sess.SessionPublicID)
	newRow := loadSession(t, next.SessionPublicID)
	if !oldRow.Revoked || oldRow.RevokedReason == nil || *oldRow.RevokedReason != "rotated" {
		t.Errorf("舊 session 應標 rotated,實際 revoked=%v reason=%v", oldRow.Revoked, oldRow.RevokedReason)
	}
	if newRow.RotatedFrom == nil || *newRow.RotatedFrom != oldRow.ID {
		t.Errorf("rotated_from 應指向舊列 %d,實際 %v", oldRow.ID, newRow.RotatedFrom)
	}
	if newRow.Revoked {
		t.Error("新 session 不該是撤銷狀態")
	}

	// 有效裝置仍是一台(輪替不是新增裝置)
	devices, err := svc.ListDevices(ctx, sess.UserID)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 1 || devices[0].PublicID != next.SessionPublicID {
		t.Fatalf("輪替後應只剩新 session 一台裝置,實際 %+v", devices)
	}
	if devices[0].UserAgent != "UA-2" {
		t.Errorf("裝置足跡應更新為本次的 UA,實際 %q", devices[0].UserAgent)
	}

	// 再輪一次:鏈能繼續長
	third, err := svc.Refresh(ctx, next.RefreshToken, identity.DeviceInfo{})
	if err != nil {
		t.Fatalf("第二次 Refresh: %v", err)
	}
	thirdRow := loadSession(t, third.SessionPublicID)
	if thirdRow.RotatedFrom == nil || *thirdRow.RotatedFrom != newRow.ID {
		t.Errorf("第三列的 rotated_from 應指向第二列 %d,實際 %v", newRow.ID, thirdRow.RotatedFrom)
	}
}

// 重用偵測:已輪替的舊 token 再被使用 → 整條鏈全撤,之後新 token 也不能用。
func TestRefreshReuseDetection(t *testing.T) {
	setup(t)
	ctx := context.Background()
	sess := login(t, fake.newAccount(t, "reuse"), identity.DeviceInfo{})

	second, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	third, err := svc.Refresh(ctx, second.RefreshToken, identity.DeviceInfo{})
	if err != nil {
		t.Fatalf("第二次 Refresh: %v", err)
	}

	// 竊來的最舊那個 token 再用一次
	if _, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrTokenReuseDetected) {
		t.Fatalf("要回 ErrTokenReuseDetected,得到 %v", err)
	}

	// 整條鏈(三列)全部處於撤銷狀態
	for _, pid := range []string{sess.SessionPublicID, second.SessionPublicID, third.SessionPublicID} {
		if row := loadSession(t, pid); !row.Revoked {
			t.Errorf("鏈上 %s 應被撤銷", pid)
		}
	}
	// 撤銷不可逆(migration 00021 的 trigger):已 rotated 的列保留原 reason,
	// 唯一還活著的那列才被標成 reuse_detected —— 安全效果相同,稽核紀錄更完整。
	if row := loadSession(t, third.SessionPublicID); row.RevokedReason == nil || *row.RevokedReason != "reuse_detected" {
		t.Errorf("當時仍有效的 %s 應標 reuse_detected,實際 %v", third.SessionPublicID, row.RevokedReason)
	}
	for _, pid := range []string{sess.SessionPublicID, second.SessionPublicID} {
		if row := loadSession(t, pid); row.RevokedReason == nil || *row.RevokedReason != "rotated" {
			t.Errorf("已輪替的 %s 應保留 rotated,實際 %v", pid, row.RevokedReason)
		}
	}

	// 目前有效的那個 token 也不能再用(這才是重用偵測的意義:趕走小偷也趕走本人)
	if _, err := svc.Refresh(ctx, third.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionRevoked) {
		t.Errorf("鏈已撤銷,最新 token 也要失效,得到 %v", err)
	}
	devices, err := svc.ListDevices(ctx, sess.UserID)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("整條鏈撤銷後不該有有效裝置,實際 %d", len(devices))
	}
}

// 唯一索引路徑:sessions_rotated_from_uq 才是「一個 session 只能輪替一次」的權威。
// 這裡刻意造出「舊列還沒被標 rotated,但已經有子列指著它」的狀態——
// 前置讀取看不出異常,只有 INSERT 撞唯一鍵能擋下。
func TestRefreshUniqueIndexIsTheAuthority(t *testing.T) {
	setup(t)
	ctx := context.Background()
	sess := login(t, fake.newAccount(t, "uq"), identity.DeviceInfo{})
	old := loadSession(t, sess.SessionPublicID)

	// 直接插一列冒充「已經輪替出去的新 session」,但不動舊列的 revoked 欄位
	_, err := pool.Exec(ctx, `
		INSERT INTO platform.sessions
		  (public_id, user_id, refresh_token_hash, expires_at, rotated_from)
		VALUES ($1, $2, $3, now() + interval '30 days', $4)`,
		"TESTSHADOW"+sess.SessionPublicID, sess.UserID,
		sha256.New().Sum([]byte(sess.SessionPublicID)), old.ID)
	if err != nil {
		t.Fatalf("插影子 session: %v", err)
	}

	if _, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrTokenReuseDetected) {
		t.Fatalf("撞 sessions_rotated_from_uq 應判定為重用,得到 %v", err)
	}
	if row := loadSession(t, sess.SessionPublicID); row.RevokedReason == nil || *row.RevokedReason != "reuse_detected" {
		t.Errorf("舊列應被標 reuse_detected,實際 %v", row.RevokedReason)
	}
}

// 併發用同一個 refresh token:恰一個成功,另一個被判定重用,整條鏈隨即全撤。
func TestConcurrentRefreshSameToken(t *testing.T) {
	setup(t)
	ctx := context.Background()
	sess := login(t, fake.newAccount(t, "race"), identity.DeviceInfo{})

	const n = 2
	var wg sync.WaitGroup
	results := make([]*identity.Session, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{})
		}()
	}
	close(start)
	wg.Wait()

	var ok, reuse int
	for i := range n {
		switch {
		case errs[i] == nil:
			ok++
		case errors.Is(errs[i], identity.ErrTokenReuseDetected):
			reuse++
		default:
			t.Fatalf("#%d 非預期錯誤: %v", i, errs[i])
		}
	}
	if ok != 1 || reuse != 1 {
		t.Fatalf("應恰 1 成功 1 重用,實際 成功=%d 重用=%d", ok, reuse)
	}

	// 同一個 token 被用了兩次 = 已外洩:連成功那條也要被撤掉
	for i := range n {
		if results[i] == nil {
			continue
		}
		if _, err := svc.Refresh(ctx, results[i].RefreshToken, identity.DeviceInfo{}); err == nil {
			t.Error("重用偵測後,勝出的那個 token 也必須失效")
		}
	}
	devices, err := svc.ListDevices(ctx, sess.UserID)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("重用偵測後不該有有效裝置,實際 %d", len(devices))
	}
	// 鏈上不得留下任何未撤銷的列
	if n := queryInt(t,
		`SELECT count(*) FROM platform.sessions WHERE user_id = $1 AND revoked_at IS NULL`,
		sess.UserID); n != 0 {
		t.Errorf("鏈上仍有 %d 列未撤銷", n)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.sessions WHERE user_id = $1 AND revoked_reason = 'reuse_detected'`,
		sess.UserID); n != 1 {
		t.Errorf("勝出那列應被標 reuse_detected,實際 %d 列", n)
	}
}

func TestRefreshRejectsUnknownAndExpired(t *testing.T) {
	setup(t)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, "", identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionNotFound) {
		t.Errorf("空 token 要回 ErrSessionNotFound,得到 %v", err)
	}
	if _, err := svc.Refresh(ctx, "no-such-token", identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionNotFound) {
		t.Errorf("不存在的 token 要回 ErrSessionNotFound,得到 %v", err)
	}

	sess := login(t, fake.newAccount(t, "exp"), identity.DeviceInfo{})
	if _, err := pool.Exec(ctx,
		`UPDATE platform.sessions SET expires_at = now() - interval '1 day' WHERE public_id = $1`,
		sess.SessionPublicID); err != nil {
		t.Fatalf("造過期 session: %v", err)
	}
	if _, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionExpired) {
		t.Errorf("過期 session 要回 ErrSessionExpired,得到 %v", err)
	}
	if devices, err := svc.ListDevices(ctx, sess.UserID); err != nil || len(devices) != 0 {
		t.Errorf("過期 session 不該出現在裝置清單:%+v err=%v", devices, err)
	}
}

// 登出這台:該 session 的 refresh 立刻失效,別台不受影響;也不能登出別人的裝置。
func TestLogout(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "logout")
	a := login(t, discordID, identity.DeviceInfo{UserAgent: "device-A"})
	b := login(t, discordID, identity.DeviceInfo{UserAgent: "device-B"})

	if err := svc.Logout(ctx, a.SessionPublicID, a.UserID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Refresh(ctx, a.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionRevoked) {
		t.Errorf("登出後 refresh 要回 ErrSessionRevoked,得到 %v", err)
	}
	if row := loadSession(t, a.SessionPublicID); row.RevokedReason == nil || *row.RevokedReason != "logout" {
		t.Errorf("reason 應為 logout,實際 %v", row.RevokedReason)
	}
	// 另一台不受影響(這裡只看 DB 狀態:對 b 做 Refresh 會輪替掉它,
	// 後面的越權測試就沒有「仍然有效的 token」可用了)
	if row := loadSession(t, b.SessionPublicID); row.Revoked {
		t.Error("另一台裝置不該被登出")
	}
	// 重複登出同一台
	if err := svc.Logout(ctx, a.SessionPublicID, a.UserID); !errors.Is(err, identity.ErrSessionNotFound) {
		t.Errorf("重複登出要回 ErrSessionNotFound,得到 %v", err)
	}

	// 越權:別人的 user id 帶著我的 session public_id 動不了
	other := login(t, fake.newAccount(t, "logout-other"), identity.DeviceInfo{})
	if err := svc.Logout(ctx, b.SessionPublicID, other.UserID); !errors.Is(err, identity.ErrSessionNotFound) {
		t.Errorf("不得登出別人的裝置,得到 %v", err)
	}
	if _, err := svc.Refresh(ctx, b.RefreshToken, identity.DeviceInfo{}); err != nil {
		t.Errorf("越權登出不該生效,b 的 token 仍應可用: %v", err)
	}
}

// 封鎖:一次撤掉所有裝置。
func TestRevokeAllForUser(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "ban")
	a := login(t, discordID, identity.DeviceInfo{UserAgent: "A"})
	b := login(t, discordID, identity.DeviceInfo{UserAgent: "B"})
	c := login(t, discordID, identity.DeviceInfo{UserAgent: "C"})

	n, err := svc.RevokeAllForUser(ctx, a.UserID, identity.RevokeAdminBan)
	if err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	if n != 3 {
		t.Errorf("應撤銷 3 個 session,實際 %d", n)
	}
	for _, s := range []*identity.Session{a, b, c} {
		if _, err := svc.Refresh(ctx, s.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionRevoked) {
			t.Errorf("裝置 %s 應失效,得到 %v", s.SessionPublicID, err)
		}
		if row := loadSession(t, s.SessionPublicID); row.RevokedReason == nil || *row.RevokedReason != "admin_ban" {
			t.Errorf("reason 應為 admin_ban,實際 %v", row.RevokedReason)
		}
	}
	if devices, err := svc.ListDevices(ctx, a.UserID); err != nil || len(devices) != 0 {
		t.Errorf("全撤後不該有裝置:%+v err=%v", devices, err)
	}

	// 冪等:再撤一次是 0 列,不報錯
	if n, err := svc.RevokeAllForUser(ctx, a.UserID, identity.RevokeAdminBan); err != nil || n != 0 {
		t.Errorf("重複全撤應為 0 列無錯誤,得到 n=%d err=%v", n, err)
	}
	// 不合法的理由要被擋(DB 的 CHECK 值域是最後防線,不是第一道)
	if _, err := svc.RevokeAllForUser(ctx, a.UserID, identity.RevokeReason("whatever")); err == nil {
		t.Error("不在枚舉內的撤銷理由應被拒絕")
	}
}

// 軟刪除的帳號:session 還在表裡且未過期,但一律視為無效——
// 否則「刪了帳號還能無限續期」。schema 表達不了跨表條件,只能靠查詢契約。
func TestSoftDeletedUserCannotUseSessions(t *testing.T) {
	setup(t)
	ctx := context.Background()
	sess := login(t, fake.newAccount(t, "gone"), identity.DeviceInfo{})

	if _, err := pool.Exec(ctx,
		`UPDATE platform.users SET deleted_at = now() WHERE id = $1`, sess.UserID); err != nil {
		t.Fatalf("軟刪除使用者: %v", err)
	}

	if _, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrAccountDeleted) {
		t.Errorf("軟刪除後 refresh 要回 ErrAccountDeleted,得到 %v", err)
	}
	if _, err := svc.Authenticate(ctx, sess.AccessToken); !errors.Is(err, identity.ErrAccountDeleted) {
		t.Errorf("軟刪除後 access token 要失效,得到 %v", err)
	}
	if devices, err := svc.ListDevices(ctx, sess.UserID); err != nil || len(devices) != 0 {
		t.Errorf("軟刪除後不該列出裝置:%+v err=%v", devices, err)
	}
	// account_deleted 是合法的撤銷理由(供刪帳號流程用)
	if _, err := svc.RevokeAllForUser(ctx, sess.UserID, identity.RevokeAccountDeleted); err != nil {
		t.Errorf("RevokeAllForUser(account_deleted): %v", err)
	}
	if row := loadSession(t, sess.SessionPublicID); row.RevokedReason == nil || *row.RevokedReason != "account_deleted" {
		t.Errorf("reason 應為 account_deleted,實際 %v", row.RevokedReason)
	}
}

// 已軟刪除的 Discord 綁定不得重新登入(唯一鍵還被佔著,不能再建一個 user)。
func TestSoftDeletedUserCannotLogIn(t *testing.T) {
	setup(t)
	ctx := context.Background()
	discordID := fake.newAccount(t, "gone2")
	sess := login(t, discordID, identity.DeviceInfo{})
	if _, err := pool.Exec(ctx,
		`UPDATE platform.users SET deleted_at = now() WHERE id = $1`, sess.UserID); err != nil {
		t.Fatalf("軟刪除使用者: %v", err)
	}

	if _, err := tryLogin(discordID, identity.DeviceInfo{}); !errors.Is(err, identity.ErrAccountDeleted) {
		t.Fatalf("已刪除帳號重新登入要回 ErrAccountDeleted,得到 %v", err)
	}
	if n := queryInt(t,
		`SELECT count(*) FROM platform.identities WHERE provider_user_id = $1`, discordID); n != 1 {
		t.Errorf("不得因此多出一個綁定,實際 %d 列", n)
	}
}

// Authenticate 是純簽章驗證:偽造/過期/別把金鑰簽的一律拒絕。
func TestAuthenticateRejectsBadTokens(t *testing.T) {
	setup(t)
	ctx := context.Background()

	for _, tok := range []string{"", "garbage", "v1.abc.def", "v2.abc.def"} {
		if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, identity.ErrInvalidToken) {
			t.Errorf("token %q 要回 ErrInvalidToken,得到 %v", tok, err)
		}
	}
	// 簽章正確但使用者不存在(public_id 是我們自己簽的假 ULID)
	fakeTok, _, err := signer.SignAccess("NOSUCHUSERPUBLICID00000000", "SESSX", time.Now())
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	if _, err := svc.Authenticate(ctx, fakeTok); !errors.Is(err, identity.ErrAccountDeleted) {
		t.Errorf("查無使用者要回 ErrAccountDeleted,得到 %v", err)
	}
}

// 裝置清單上的 public_id 只要經過一次背景 refresh 就會指向已 rotated 的舊列。
// 「登出這台」必須撤掉整條鏈,否則該裝置靠輪替出來的新 token 繼續有效——
// 而使用者看到的是「已登出」。
func TestLogoutAfterRotationKillsWholeChain(t *testing.T) {
	setup(t)
	ctx := context.Background()
	sess := login(t, fake.newAccount(t, "logout-rotated"), identity.DeviceInfo{UserAgent: "phone"})

	// 使用者手上的裝置清單記著這個 public_id
	stalePublicID := sess.SessionPublicID

	// 背景 refresh:舊列變 rotated、換了新 public_id 與新 token
	next, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{UserAgent: "phone"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	third, err := svc.Refresh(ctx, next.RefreshToken, identity.DeviceInfo{UserAgent: "phone"})
	if err != nil {
		t.Fatalf("第二次 Refresh: %v", err)
	}
	if third.SessionPublicID == stalePublicID {
		t.Fatal("前置條件不成立:輪替後 public_id 應該換了")
	}

	// 使用者按下「登出這台」,帶的是清單上那個已經過時的 public_id
	if err := svc.Logout(ctx, stalePublicID, sess.UserID); err != nil {
		t.Fatalf("用已輪替的 public_id 登出應成功: %v", err)
	}

	// 該裝置目前真正在用的 token 必須立刻失效
	if _, err := svc.Refresh(ctx, third.RefreshToken, identity.DeviceInfo{}); !errors.Is(err, identity.ErrSessionRevoked) {
		t.Fatalf("登出後該裝置的新 token 必須失效,得到 %v", err)
	}
	if devices, err := svc.ListDevices(ctx, sess.UserID); err != nil || len(devices) != 0 {
		t.Errorf("登出後不該有有效裝置:%+v err=%v", devices, err)
	}
	if row := loadSession(t, third.SessionPublicID); row.RevokedReason == nil || *row.RevokedReason != "logout" {
		t.Errorf("當時仍有效那列的 reason 應為 logout,實際 %v", row.RevokedReason)
	}
	// 已輪替的舊列保留 rotated(撤銷不可逆),但整條鏈都已撤銷
	if n := queryInt(t,
		`SELECT count(*) FROM platform.sessions WHERE user_id = $1 AND revoked_at IS NULL`,
		sess.UserID); n != 0 {
		t.Errorf("鏈上仍有 %d 列未撤銷", n)
	}
	// 重複登出:鏈上已無未撤銷列
	if err := svc.Logout(ctx, stalePublicID, sess.UserID); !errors.Is(err, identity.ErrSessionNotFound) {
		t.Errorf("重複登出要回 ErrSessionNotFound,得到 %v", err)
	}
}

// 撤鏈不跟隨呼叫端 ctx:客戶端送出重用的 token 後立刻斷線,鏈仍必須被撤掉。
//
// 手法:另一條連線先霸住該使用者的 advisory lock,讓撤鏈卡住;確認它真的在等之後
// 取消呼叫端 ctx,再放開鎖。若撤鏈跟著呼叫端 ctx 走,這裡就會中途放棄
// ——「偵測到重用但鏈還活著」正是最不能發生的失敗模式。
func TestReuseDetectionSurvivesCallerCancel(t *testing.T) {
	setup(t)
	bg := context.Background()
	sess := login(t, fake.newAccount(t, "cancel"), identity.DeviceInfo{})
	next, err := svc.Refresh(bg, sess.RefreshToken, identity.DeviceInfo{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// 霸住 advisory lock(與服務用的是同一把:classid=hashtext('user_sessions'))
	blocker, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("開阻擋用 tx: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = blocker.Rollback(bg)
		}
	}()
	if _, err := blocker.Exec(bg,
		`SELECT pg_advisory_xact_lock(hashtext('user_sessions'), ($1::bigint & 2147483647)::int)`,
		sess.UserID); err != nil {
		t.Fatalf("取阻擋鎖: %v", err)
	}

	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() {
		_, err := svc.Refresh(ctx, sess.RefreshToken, identity.DeviceInfo{})
		done <- err
	}()

	// 等到撤鏈真的卡在 advisory lock 上,才取消呼叫端 ctx
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(bg,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`,
		).Scan(&waiting); err != nil {
			t.Fatalf("查 pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("等不到撤鏈卡在 advisory lock 上")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel() // 客戶端斷線
	time.Sleep(100 * time.Millisecond)
	released = true
	if err := blocker.Rollback(bg); err != nil { // 放開鎖
		t.Fatalf("釋放阻擋鎖: %v", err)
	}

	if err := <-done; !errors.Is(err, identity.ErrTokenReuseDetected) {
		t.Fatalf("要回 ErrTokenReuseDetected(安全事件不得被降級成 DB 錯誤),得到 %v", err)
	}

	// 呼叫端早就斷線了,鏈仍必須已經撤乾淨
	if n := queryInt(t,
		`SELECT count(*) FROM platform.sessions WHERE user_id = $1 AND revoked_at IS NULL`,
		sess.UserID); n != 0 {
		t.Errorf("呼叫端取消後鏈仍有 %d 列未撤銷", n)
	}
	if _, err := svc.Refresh(bg, next.RefreshToken, identity.DeviceInfo{}); err == nil {
		t.Error("重用偵測後鏈上的 token 都必須失效")
	}
}
