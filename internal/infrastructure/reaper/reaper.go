// Package reaper 回收到期的限時權益(schemas/14 entitlement_reaper)。
//
// 職責:掃 platform.entitlements 裡 expires_at 已到且未撤銷的列,標 revoked_at
// 並發 outbox `entitlement.expired`(消費端收回 Discord 身分組——Bot 離線/退群/
// 身分組已刪由消費端的重試與終態處理,不在這裡)。
//
// 併發與冪等(一個概念一個權威):防重權威是 FOR UPDATE SKIP LOCKED + 「revoked_at
// IS NULL 才匹配」——撤銷過的列不再進批次,多實例同跑或與退款(shoppg 持
// entitlement 列鎖)並發都不互擋、不重複,不需要冪等鍵。
// 每批一個 transaction:撤銷與 outbox 同 tx(帳本鐵則六),要嘛都在要嘛都不在。
// 時間全用 DB 時鐘(now()),不用 app 時鐘。
//
// 排程與 advisory lock 是 maintenance runner 的事;本套件只提供 Reap 一次收完。
package reaper

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// TopicEntitlementExpired 是到期回收的 outbox topic。
// payload:entitlement_id、user_id、item_id,商品有 external_role_id 時附上
// (auto_role 消費端憑此收回 Discord 身分組)。
const TopicEntitlementExpired = "entitlement.expired"

const defaultBatchSize = 100

// Reaper 回收到期權益。零值不可用,經 New 建立。
type Reaper struct {
	pool *pgxpool.Pool
	q    *db.Queries

	// BatchSize 每批(= 每個 tx)最多處理幾筆,預設 100;測試可覆寫。
	// 批次小讓鎖持有時間短,與退款的互斥窗口也小。
	BatchSize int32
}

// New 建立回收器。
func New(pool *pgxpool.Pool) *Reaper {
	return &Reaper{pool: pool, q: db.New(pool), BatchSize: defaultBatchSize}
}

// Reap 迴圈回收到期權益直到一批也撈不到,回傳總處理數。
// 部分成功是正常結果:回傳的 processed 是「已 commit 的筆數」,錯誤發生時
// 之前的批次不回滾,下一輪排程從中斷處繼續(結構上冪等)。
// 被其他 tx 鎖住的列(退款中)本輪被 SKIP LOCKED 跳過,由下一輪排程接手。
func (r *Reaper) Reap(ctx context.Context) (processed int, err error) {
	for {
		n, err := r.reapBatch(ctx)
		if err != nil {
			return processed, fmt.Errorf("到期回收(已處理 %d 筆): %w", processed, err)
		}
		if n == 0 {
			return processed, nil
		}
		processed += n
	}
}

// reapBatch 在單一 transaction 內處理一批:鎖列 → 逐筆寫 outbox → 批次撤銷 → commit。
func (r *Reaper) reapBatch(ctx context.Context) (int, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := r.q.WithTx(tx)

	rows, err := qtx.LockExpiredEntitlements(ctx, r.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("鎖到期權益: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
		payload := map[string]any{
			"entitlement_id": row.ID,
			"user_id":        row.UserID,
			"item_id":        row.ItemID,
		}
		if row.ExternalRoleID != nil {
			payload["external_role_id"] = *row.ExternalRoleID
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("序列化事件 entitlement=%d: %w", row.ID, err)
		}
		if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
			Topic: TopicEntitlementExpired, Payload: raw,
		}); err != nil {
			return 0, fmt.Errorf("寫 outbox entitlement=%d: %w", row.ID, err)
		}
	}

	affected, err := qtx.RevokeExpiredEntitlements(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("撤銷到期權益: %w", err)
	}
	// 持鎖之下 revoked_at 不可能被別人改動,數字對不上就是狀態矛盾:失敗出聲,
	// 整批回滾(outbox 同滅),絕不 commit 一個「撤銷數 ≠ 事件數」的批次。
	if affected != int64(len(ids)) {
		return 0, fmt.Errorf("撤銷筆數 %d ≠ 鎖定筆數 %d,狀態矛盾整批回滾", affected, len(ids))
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(rows), nil
}
