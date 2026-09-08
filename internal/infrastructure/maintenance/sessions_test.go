package maintenance_test

import (
	"context"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/infrastructure/maintenance"
)

// insertSession 直接造一列 session(繞過服務,精確控制時間戳)。
// revoked_at / revoked_reason 必須成對(CHECK sessions_revocation_paired)。
func insertSession(t *testing.T, userID int64, publicID, expiresAt, revokedAt, reason string, rotatedFrom *int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO platform.sessions
		  (public_id, user_id, refresh_token_hash, expires_at, revoked_at, revoked_reason, rotated_from)
		VALUES ($1, $2, sha256(convert_to($1, 'UTF8')), now() + $3::interval,
		        CASE WHEN $4 = '' THEN NULL ELSE now() + $4::interval END,
		        CASE WHEN $5 = '' THEN NULL ELSE $5 END,
		        $6)
		RETURNING id`,
		publicID, userID, expiresAt, revokedAt, reason, rotatedFrom,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插 session %s: %v", publicID, err)
	}
	return id
}

func sessionExists(t *testing.T, publicID string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform.sessions WHERE public_id = $1`, publicID).Scan(&n); err != nil {
		t.Fatalf("查 session: %v", err)
	}
	return n > 0
}

// TestSessionCleanup:逾保留期的已撤銷/已過期 session 被刪,
// 窗口內的與仍有效的不動;輪替鏈的子列指標由 FK 的 ON DELETE SET NULL 自動斷開。
func TestSessionCleanup(t *testing.T) {
	setup(t)
	ctx := context.Background()
	userID := newUser(t)

	// 保留期 90 天(seed);以「距今」表達,不隨執行日期漂移
	insertSession(t, userID, "mt-sess-revoked-old", "30 days", "-100 days", "logout", nil)
	insertSession(t, userID, "mt-sess-expired-old", "-100 days", "", "", nil)
	insertSession(t, userID, "mt-sess-revoked-new", "30 days", "-10 days", "logout", nil)
	insertSession(t, userID, "mt-sess-expired-new", "-10 days", "", "", nil)
	insertSession(t, userID, "mt-sess-active", "30 days", "", "", nil)
	// 鏈:老父列該刪、子列仍有效
	parent := insertSession(t, userID, "mt-sess-chain-parent", "30 days", "-100 days", "rotated", nil)
	insertSession(t, userID, "mt-sess-chain-child", "30 days", "", "", &parent)

	processed, err := maintenance.SessionCleanup(pool)(ctx)
	if err != nil {
		t.Fatalf("session_cleanup: %v", err)
	}
	if processed < 3 {
		t.Errorf("processed = %d,至少應刪掉本測試的 3 列(撤銷逾期、過期逾期、鏈上父列)", processed)
	}

	gone := []string{"mt-sess-revoked-old", "mt-sess-expired-old", "mt-sess-chain-parent"}
	for _, pid := range gone {
		if sessionExists(t, pid) {
			t.Errorf("%s 逾保留期,應被刪除", pid)
		}
	}
	kept := []string{"mt-sess-revoked-new", "mt-sess-expired-new", "mt-sess-active", "mt-sess-chain-child"}
	for _, pid := range kept {
		if !sessionExists(t, pid) {
			t.Errorf("%s 不該被刪除", pid)
		}
	}

	// 父列被刪後,子列的 rotated_from 應被 FK 斷成 NULL(而不是整批清理撞 FK 失敗)
	var rotatedFrom *int64
	if err := pool.QueryRow(ctx,
		`SELECT rotated_from FROM platform.sessions WHERE public_id = 'mt-sess-chain-child'`,
	).Scan(&rotatedFrom); err != nil {
		t.Fatalf("讀子列: %v", err)
	}
	if rotatedFrom != nil {
		t.Errorf("父列已刪,子列的 rotated_from 應為 NULL,實際 %d", *rotatedFrom)
	}

	// 冪等:再跑一次沒有東西可刪
	if processed, err := maintenance.SessionCleanup(pool)(ctx); err != nil {
		t.Fatalf("第二次 session_cleanup: %v", err)
	} else if processed != 0 {
		t.Errorf("第二次執行不該再刪,processed = %d", processed)
	}
}

// 設定缺失即失敗出聲:保留期歸零等於「全刪」,不能用預設值掩蓋設定錯誤。
func TestSessionCleanupConfigMissing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	userID := newUser(t)
	const key = "session_retention_days"

	// 哨兵:設定壞掉時它必須毫髮無傷
	insertSession(t, userID, "mt-sess-sentinel", "30 days", "-400 days", "logout", nil)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM platform.sessions WHERE public_id = 'mt-sess-sentinel'`)
	})

	if _, err := pool.Exec(ctx,
		`DELETE FROM platform.economy_configs WHERE key = $1`, key); err != nil {
		t.Fatalf("移除設定: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `
			INSERT INTO platform.economy_configs (key, value, note)
			VALUES ($1, '90', 'maintenance 測試還原')`, key)
	})

	processed, err := maintenance.SessionCleanup(pool)(ctx)
	if err == nil {
		t.Fatalf("設定缺失應回報錯誤,卻成功了(processed=%d)", processed)
	}
	if !strings.Contains(err.Error(), "讀設定") {
		t.Errorf("錯誤訊息應說明是讀設定失敗,實際:%v", err)
	}
	if processed != 0 {
		t.Errorf("失敗時 processed 應為 0,實際 %d", processed)
	}
	if !sessionExists(t, "mt-sess-sentinel") {
		t.Error("設定壞掉時不得執行清理,哨兵不該被刪")
	}

	// 非整數設定也要失敗
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO platform.economy_configs (key, value, note)
		VALUES ($1, '"abc"'::jsonb, 'maintenance 測試覆寫') RETURNING id`, key).Scan(&id); err != nil {
		t.Fatalf("覆寫設定: %v", err)
	}
	if _, err := maintenance.SessionCleanup(pool)(ctx); err == nil {
		t.Error("非整數設定應回報錯誤")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM platform.economy_configs WHERE id = $1`, id); err != nil {
		t.Fatalf("還原設定: %v", err)
	}
}
