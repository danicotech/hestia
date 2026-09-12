package xppg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/ledger"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 里程碑獎勵的發放(schemas/24)。
//
// 由 in-process outbox 消費者呼叫,topic = notification.TopicLevelReward。
// 與升級公告分開的理由見 emitLevelUp:兩者失敗模式不同,不該互相牽連。

// txLedger 是本套件對帳本的最小依賴:與發放同 tx 動錢。
// 實際注入 *ledgerpg.Service(ApplyInTx 刻意不在 ledger.Ledger interface 上,
// 因為未來的 HTTP 版沒有 tx 可傳)。與 dailypg 同樣的宣告方式。
type txLedger interface {
	ApplyInTx(ctx context.Context, tx pgx.Tx, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}

// RewardGranter 發放一次升級所跨過的全部里程碑獎勵。
//
// led 用來發點數類獎勵;nil 時點數獎勵會被跳過並回報 —— 不是靜默忽略,
// 因為「設定了獎勵但沒發」正是最難被發現的那種壞法。
type RewardGranter struct {
	svc *Service
	led txLedger
}

// NewRewardGranter 建立發放器。led 傳 ledgerpg.New(pool) 的結果。
func NewRewardGranter(svc *Service, led txLedger) *RewardGranter {
	return &RewardGranter{svc: svc, led: led}
}

// levelUpPayload 與 xppg.emitLevelUp 寫出的形狀一致。
type levelUpPayload struct {
	UserID      int64  `json:"user_id"`
	CommunityID int64  `json:"community_id"`
	Subject     string `json:"subject"`
	FromLevel   int32  `json:"from_level"`
	ToLevel     int32  `json:"to_level"`
}

// Handle 是 outbox.Handler 的形狀:處理一則 level.reward 事件。
//
// 整批在同一個 transaction:跨了三級、發到第二級時失敗,不該留下
// 「第一級發了、第二、三級沒發」的狀態 —— 重試時第一級會被冪等鍵擋掉,
// 那三個獎勵就永遠只有一個到手。
func (g *RewardGranter) Handle(ctx context.Context, payload []byte) error {
	var p levelUpPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("解析升級事件: %w", err)
	}
	if p.UserID <= 0 || p.CommunityID <= 0 || p.ToLevel <= p.FromLevel {
		return fmt.Errorf("升級事件內容不合法: %+v", p)
	}
	subject := p.Subject
	if subject == "" {
		subject = "user"
	}

	tx, err := g.svc.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := g.svc.q.WithTx(tx)

	rewards, err := qtx.ListLevelRewardsBetween(ctx, db.ListLevelRewardsBetweenParams{
		CommunityID: p.CommunityID,
		Subject:     subject,
		Level:       p.FromLevel,
		Level_2:     p.ToLevel,
	})
	if err != nil {
		return fmt.Errorf("查里程碑獎勵: %w", err)
	}
	if len(rewards) == 0 {
		return nil // 這幾級沒有設獎勵,是常態
	}

	for _, r := range rewards {
		// 冪等:撞鍵 = 已經發過(至少一次投遞下重送是常態,不是錯誤)。
		n, err := qtx.ClaimLevelReward(ctx, db.ClaimLevelRewardParams{
			RewardID: r.ID, UserID: p.UserID,
		})
		if err != nil {
			return fmt.Errorf("登記獎勵 %d: %w", r.ID, err)
		}
		if n == 0 {
			continue
		}
		if err := g.grant(ctx, tx, qtx, p, r); err != nil {
			return fmt.Errorf("發放獎勵 %d(%s): %w", r.ID, r.RewardKind, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (g *RewardGranter) grant(
	ctx context.Context, tx pgx.Tx, qtx *db.Queries,
	p levelUpPayload, r db.ListLevelRewardsBetweenRow,
) error {
	switch r.RewardKind {
	case "tokens":
		if g.led == nil {
			return errors.New("點數獎勵需要帳本,但沒有注入")
		}
		if r.Amount == nil || *r.Amount <= 0 {
			return fmt.Errorf("點數獎勵金額不合法: %v", r.Amount)
		}
		// 冪等鍵含 reward_id 與 user:同一個里程碑對同一個人只會發一次,
		// 即使 outbox 重送到這裡(level_reward_grants 已經擋過一層,
		// 這是帳本自己的那一層 —— 動錢一律要冪等,鐵則三)。
		key := fmt.Sprintf("level_reward:%d:%d", r.ID, p.UserID)
		_, err := g.led.ApplyInTx(ctx, tx, ledger.ApplyParams{
			IdempotencyKey: key,
			RequestHash:    key,
			Ops: []ledger.Op{{
				UserID:   p.UserID,
				Currency: r.RewardRef,
				Amount:   *r.Amount,
				Reason:   ledger.ReasonAdminGrant,
			}},
		})
		return err

	case "role":
		n, err := qtx.GrantRoleByPublicID(ctx, db.GrantRoleByPublicIDParams{
			UserID: p.UserID, CommunityID: &p.CommunityID, PublicID: r.RewardRef,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			// 0 列有兩種可能:角色 public_id 打錯,或這個人已經有了。
			// 後者無害,前者是設定錯誤 —— 但在這裡分不出來,而讓整個
			// 升級發放失敗會連帶擋住其他獎勵。回 nil 並由呼叫端的 log 留痕。
			return nil
		}
		return nil

	case "item":
		id, err := ulid.New()
		if err != nil {
			return fmt.Errorf("產生 public_id: %w", err)
		}
		_, err = qtx.GrantItemByDefinitionPublicID(ctx, db.GrantItemByDefinitionPublicIDParams{
			PublicID: id, OwnerID: p.UserID, PublicID_2: r.RewardRef,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("物品定義 %q 不存在", r.RewardRef)
		}
		return err

	case "entitlement":
		// 尚未接上(schemas/24 列了四種,這一種的發放路徑要走 shop 的
		// 履約邏輯,不是單純 INSERT)。**明確回錯而不是靜默跳過**:
		// 設定了獎勵卻什麼都沒發生,是最難被發現的壞法。
		return errors.New("entitlement 類獎勵尚未接上發放路徑")

	default:
		return fmt.Errorf("不認得的獎勵類型 %q", r.RewardKind)
	}
}
