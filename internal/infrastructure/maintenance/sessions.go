package maintenance

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// JobSessionCleanup 是登入 session 的膨脹歸宿(schemas/02 增補 F)。
const JobSessionCleanup = "session_cleanup"

// SessionCleanup 刪除已撤銷或已過期、且逾 session_retention_days 的登入 session。
//
// 保留一段時間不是為了功能,是為了稽核——「這台裝置什麼時候登入過」在資安事件
// 之後才會有人問,而那時資料已經沒了就永遠答不出來。
//
// 起算點是 COALESCE(revoked_at, expires_at):撤銷過的從撤銷時起算,
// 自然過期的從到期時起算。查詢條件的形態必須與表達式索引 sessions_retention_idx
// 一致,拆成 OR 會退化成全表掃描(見 queries/identity.sql)。
//
// rotated_from 指標由 FK 的 ON DELETE SET NULL 自動斷開,不會因刪父列撞 FK。
func SessionCleanup(pool *pgxpool.Pool) JobFunc {
	return func(ctx context.Context) (int, error) {
		q := db.New(pool)
		days, err := configDays(ctx, q, "session_retention_days")
		if err != nil {
			return 0, err
		}
		n, err := q.CleanupSessions(ctx, days)
		if err != nil {
			return 0, fmt.Errorf("清理 sessions: %w", err)
		}
		return int(n), nil
	}
}
