// Package authzpg 是管理端授權(core/platform/authz)的 Postgres 實作。
//
// 判定完全靠自家 DB:user_roles → role_permissions,加上軟刪除與停權兩道否決。
// 第三方身分(Discord 身分組)只是 user_roles 的來源之一(source='provider_sync'),
// 判定時一視同仁,不在這裡回頭問 provider。
package authzpg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/authz"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// Service 是 authz.Authorizer 的 Postgres 實作。
// 無狀態(只持有 pool 與 sqlc queries),可安全並行使用。
type Service struct {
	q *db.Queries
	// communityID 為 nil = 不限社群範圍(M1 只有一個社群):全域角色與任何
	// 社群的角色都算。有值時只認全域角色與該社群的角色。
	communityID *int64
}

var _ authz.Authorizer = (*Service)(nil)

// New 建立不限社群範圍的授權服務(M1 預設)。
func New(pool *pgxpool.Pool) *Service {
	return &Service{q: db.New(pool)}
}

// NewScoped 建立限定社群的授權服務:只認全域角色(user_roles.community_id
// IS NULL)與該社群的角色。多社群上線時改用這個構造式,判定 SQL 不用動。
func NewScoped(pool *pgxpool.Pool, communityID int64) *Service {
	return &Service{q: db.New(pool), communityID: &communityID}
}

// Authorize 判定 userID 能不能執行 procedure。
//
// 有權限回 nil;無權限回 authz.ErrPermissionDenied(包了診斷用的細節,
// 呼叫端用 errors.Is 比對)。資料庫錯誤原樣往上包——查不出來就不是「沒權限」,
// 是「這次判不了」,不該被記成一次提權嘗試,也不該讓請求成功。
//
// 拒絕的四種來源(未登記 / 無角色 / 軟刪除 / 停權)對外都是同一個 sentinel,
// 細節只在 wrap 的訊息裡供 log 使用,不進回應——授權失敗的理由本身就是情報。
func (s *Service) Authorize(ctx context.Context, userID int64, procedure string) error {
	perm, ok := authz.PermissionFor(procedure)
	if !ok {
		// 未登記 = 拒絕。漏登記必須是「打不開」而不是「全開」。
		return fmt.Errorf("%w:procedure %q 未登記於授權映射表", authz.ErrPermissionDenied, procedure)
	}
	if userID <= 0 {
		// 沒有有效身分卻走到授權,是上游的錯;這裡不查 DB 直接拒絕,
		// 免得 user_id=0 之類的值意外撞上任何資料。
		return fmt.Errorf("%w:沒有有效的使用者身分", authz.ErrPermissionDenied)
	}

	allowed, err := s.q.UserHasPermission(ctx, db.UserHasPermissionParams{
		UserID:      userID,
		Permission:  string(perm),
		CommunityID: s.communityID,
	})
	if err != nil {
		return fmt.Errorf("查詢授權(user=%d perm=%s): %w", userID, perm, err)
	}
	if allowed == nil || !*allowed {
		return fmt.Errorf("%w:user=%d 缺少 %s", authz.ErrPermissionDenied, userID, perm)
	}
	return nil
}
