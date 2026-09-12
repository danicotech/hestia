package tournamentpg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// UNIQUE 約束的名稱。Postgres 從欄位定義自動生成(`slug TEXT NOT NULL UNIQUE`
// → tournaments_slug_key),沒有寫在 migration 裡,所以在這裡留下對照。
// 改名了測試會紅:TestCreate_DuplicateSlug 驗的正是「撞的是這一個」。
const constraintSlugUnique = "tournaments_slug_key"

// Create 開一屆新賽事並複製讓武目錄,見 tournament.CreateTournamentParams。
//
// # 三件事同一個 transaction
//
// 建賽事列、複製 34 項讓武目錄、寫稽核紀錄。中途任何一步失敗,整屆賽事
// 都不會存在 —— 這不是「比較乾淨」,是必要的:一屆有賽事列卻沒有讓武項目的
// 賽事,不會有任何東西出聲,它要等到選手走到選讓武那一步才顯現,
// 而那時候已經報名、評段、抽籤完了。
//
// 讓武項目走 handicappg.BindTx 綁在這個 tx 上,不自己再寫一份 insert:
// 「怎麼把 ItemSpec 攤成八個陣列餵給 unnest」只該有一個權威位置(鐵則 9),
// 而那份在 handicappg.InsertItems ——那八個陣列少 append 一次就會安靜地錯位。
func (s *Service) Create(
	ctx context.Context, p tournament.CreateTournamentParams,
) (tournament.Tournament, error) {
	if p.ActorUserID == 0 {
		return tournament.Tournament{}, fmt.Errorf("建立賽事: %w", tournament.ErrActorRequired)
	}
	// 目錄是空的代表呼叫端漏了一步。讓它成功的話,結果正是上面那個
	// 「沒有讓武項目的賽事」—— 唯一的差別是這次是我們自己造出來的。
	if len(p.HandicapItems) == 0 {
		return tournament.Tournament{}, fmt.Errorf("%w: 讓武項目目錄是空的", tournament.ErrInvalidRequest)
	}

	publicID, err := ulid.New()
	if err != nil {
		return tournament.Tournament{}, fmt.Errorf("產生賽事 public_id: %w", err)
	}

	var out tournament.Tournament
	err = s.inTxRaw(ctx, func(tx pgx.Tx, qtx *db.Queries) error {
		row, err := qtx.InsertTournament(ctx, db.InsertTournamentParams{
			PublicID:          publicID,
			Slug:              p.Slug,
			Name:              p.Name,
			CommunityPublicID: p.CommunityPublicID,
			Config:            p.ConfigRaw,
			SignupBonus:       p.SignupBonus,
		})
		if err != nil {
			return createError(p, err)
		}
		t, err := TournamentFromRow(row)
		if err != nil {
			return err
		}

		n, err := handicappg.BindTx(tx).InsertItems(ctx, t.ID, p.HandicapItems)
		if err != nil {
			return fmt.Errorf("複製賽事 %s 的讓武目錄: %w", p.Slug, err)
		}
		// InsertItems 走 ON CONFLICT DO NOTHING(那是為了「重跑安裝不改價格」)。
		// 對一屆**剛建出來**的賽事,唯一能被吞掉的成因是目錄自己有重複的
		// (category, name) —— 那是目錄壞了,不是可以接受的少裝幾項。
		if n != len(p.HandicapItems) {
			return fmt.Errorf("賽事 %s 的讓武目錄裝了 %d 項,預期 %d 項",
				p.Slug, n, len(p.HandicapItems))
		}

		// 不送 watch 推播:這一屆此刻還沒有任何觀眾,而信封的路由鍵(slug)
		// 也是這句 INSERT 才剛讓它存在的。
		if err := writeAudit(ctx, qtx, auditEntry{
			ActorUserID: p.ActorUserID,
			Action:      actionCreated,
			TargetType:  targetTournament,
			TargetID:    t.ID,
			// 不記 config 全文:那是一份幾百位元組、絕大部分是預設值的 JSON,
			// 塞進稽核只會把真正有人會查的四個欄位淹掉。config 的真值就在
			// tournaments.config 上,而這一列是它唯一的建立紀錄。
			After: map[string]any{
				"public_id":           t.PublicID,
				"slug":                t.Slug,
				"name":                t.Name,
				"community_id":        t.CommunityID,
				"phase":               string(t.Phase),
				"signup_bonus":        t.SignupBonus,
				"handicap_item_count": n,
			},
			Reason: p.Reason,
		}); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// createError 把 INSERT 的資料庫錯誤翻成領域 sentinel。
//
// 兩種都是**使用者輸入造成的**,不該變成 500:代號撞了要他換一個,
// 社群不存在要他確認貼上的 public_id。其餘原樣往上,那才是真的內部錯誤。
func createError(p tournament.CreateTournamentParams, err error) error {
	switch {
	case isUniqueViolation(err, constraintSlugUnique):
		return fmt.Errorf("%w: %s", tournament.ErrSlugTaken, p.Slug)
	case isNotNullViolation(err, "community_id"):
		return fmt.Errorf("%w: %s", tournament.ErrCommunityNotFound, p.CommunityPublicID)
	default:
		return fmt.Errorf("建立賽事 %s: %w", p.Slug, err)
	}
}
