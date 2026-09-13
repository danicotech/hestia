package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
)

// catalogue-sync:把 binary 內嵌的讓武目錄(catalogue.json)同步進既有的一屆。
//
// # 為什麼需要它
//
// InstallSeedItems 是 ON CONFLICT DO NOTHING:審定後的文字進不了已開的賽事
// (2026-09-13 實際發生:2026-baiye-shifeng 的 34 條 referee_note 全是舊草稿,
// 當時用一次性 SQL 就地更新)。這支指令是那次 SQL 的長期版本。
//
// 規則全在 handicap.Service.SyncCatalogue(以 key 匹配、不動 cost、該屆有選擇即拒、
// 影響列數核對);這裡只負責解 slug 與 actor、開 tx、寫稽核、印報告。
//
// # 稽核與同步同一個 tx
//
// admin_audit_logs 的那一列和 UPDATE 同生共死:「文字改了但沒有紀錄」與
// 「有紀錄但文字沒改」都是事後查不出來的狀態。所以用 handicappg.BindTx 把
// 讓武服務綁在這裡開的 tx 上,稽核寫完才 commit。

const auditActionCatalogueSync = "handicap.catalogue_sync"

func catalogueSync(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("catalogue-sync", flag.ContinueOnError)
	slug := fs.String("tournament", "", "賽事 slug(網址列上那個)")
	actor := fs.String("actor", "", "執行者的使用者 public_id(admin_audit_logs.actor_user_id 要記在誰名下)")
	reason := fs.String("reason", "同步讓武目錄(catalogue.json)", "寫進稽核的理由")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" {
		return errors.New("--tournament 必填")
	}
	if *actor == "" {
		return errors.New("--actor 必填(使用者 public_id;create-judge 印出的那一串)")
	}

	q := db.New(pool)
	t, err := q.GetTournamentBySlug(ctx, *slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("賽事 %q 不存在", *slug)
	}
	if err != nil {
		return fmt.Errorf("查賽事: %w", err)
	}
	// actor 收 public_id 而不是內部 id:對外(含終端機前的人)只出現 public_id(鐵則 5)。
	// 查無此人要在動手前擋:actor_user_id 是 NOT NULL 外鍵,塞錯會讓整個 tx 連同同步一起回滾,
	// 但那時錯誤訊息是一句看不懂的 23503。
	actorID, err := q.GetUserIDByPublicID(ctx, *actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("使用者 %q 不存在(或已刪除)", *actor)
	}
	if err != nil {
		return fmt.Errorf("查使用者: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("開啟交易: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rep, err := handicap.New(handicappg.BindTx(tx)).SyncCatalogue(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("同步 %s: %w", *slug, err)
	}
	after, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("序列化報告: %w", err)
	}
	if _, err := db.New(tx).InsertAdminAudit(ctx, db.InsertAdminAuditParams{
		ActorUserID: actorID,
		Action:      auditActionCatalogueSync,
		TargetType:  optional("tournament"),
		TargetID:    &t.ID,
		After:       after,
		Reason:      *reason,
	}); err != nil {
		return fmt.Errorf("寫稽核紀錄: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交: %w", err)
	}

	fmt.Printf("已同步讓武目錄:\n  賽事    %s(%s)\n  更新    %d 項\n", t.Slug, t.PublicID, rep.Updated)
	if len(rep.ExtraKeys) > 0 {
		fmt.Printf("  該屆有、目錄沒有(未動):%v\n", rep.ExtraKeys)
	}
	fmt.Println("  cost 一律不動;稽核已記在", *actor, "名下。")
	return nil
}
