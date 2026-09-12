package playpg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/play"
	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 寵物(schemas/25)。擁有權在 item_instances,狀態在 pet_states ——
// 這裡只碰狀態那一半。
//
// 等級用與使用者相同的曲線(schemas/24)。寵物不屬於任何社群,
// 所以套預設曲線:牠的 XP 來源跨社群,綁其中一個社群的規則沒有道理。

// maxNicknameLen 是暱稱長度上限。
//
// 不限長度的話,一個人可以用暱稱把 /profile 的 embed 撐爆 ——
// 與 event_logs 的摘要截斷是同一類問題:外界可控的字串要有上界。
const maxNicknameLen = 24

// ListPets 列出「我的寵物」。出戰中的排最前面。
func (s *Service) ListPets(ctx context.Context, userID int64) ([]play.PetListItem, error) {
	rows, err := s.q.ListPets(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("列出寵物: %w", err)
	}
	var cfg xp.Config
	out := make([]play.PetListItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, play.PetListItem{
			PublicID: r.PublicID,
			Name:     r.Name,
			Rarity:   derefStr(r.Rarity),
			IconURL:  derefStr(r.IconUrl),
			XP:       r.Xp,
			Level:    cfg.ProgressFor(r.Xp).Level,
			Deployed: r.Deployed,
		})
	}
	return out, nil
}

// DeployPet 換出戰寵物。
//
// 先把全部收起來再設新的,同一個 transaction:
// 部分唯一索引(同時只能出戰一隻)是最後防線,但直接撞它會得到
// 約束錯誤而不是可讀訊息,而使用者看到的會是「發生未知錯誤」。
func (s *Service) DeployPet(ctx context.Context, userID int64, petPublicID string) error {
	pet, err := s.q.GetPetByPublicID(ctx, petPublicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("寵物 %q: %w", petPublicID, play.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("查寵物: %w", err)
	}
	if pet.OwnerID != userID {
		// 不揭露「這隻存在但不是你的」與「這隻不存在」的差別會比較安全,
		// 但寵物的 public_id 本來就會出現在公開的排行榜上,沒有保密價值;
		// 給精確的訊息對使用者比較有用。
		return fmt.Errorf("寵物 %q: %w", pet.Name, play.ErrNotOwner)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.UndeployAllPets(ctx, userID); err != nil {
		return fmt.Errorf("收起原本的寵物: %w", err)
	}
	n, err := qtx.DeployPet(ctx, db.DeployPetParams{
		ItemInstanceID: pet.ItemInstanceID, OwnerID: userID,
	})
	if err != nil {
		return fmt.Errorf("出戰: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("寵物 %q: %w", petPublicID, play.ErrNotFound)
	}
	return tx.Commit(ctx)
}

// RenamePet 改暱稱。空字串 = 還原成物品定義的名稱。
func (s *Service) RenamePet(ctx context.Context, userID int64, petPublicID, nickname string) error {
	name := strings.TrimSpace(nickname)
	if len([]rune(name)) > maxNicknameLen {
		return fmt.Errorf("暱稱最多 %d 個字: %w", maxNicknameLen, play.ErrInvalidParams)
	}
	pet, err := s.q.GetPetByPublicID(ctx, petPublicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("寵物 %q: %w", petPublicID, play.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("查寵物: %w", err)
	}
	if pet.OwnerID != userID {
		return fmt.Errorf("寵物 %q: %w", pet.Name, play.ErrNotOwner)
	}

	var arg *string
	if name != "" {
		arg = &name
	}
	n, err := s.q.RenamePet(ctx, db.RenamePetParams{
		ItemInstanceID: pet.ItemInstanceID, OwnerID: userID, Nickname: arg,
	})
	if err != nil {
		return fmt.Errorf("改名: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("寵物 %q: %w", petPublicID, play.ErrNotFound)
	}
	return nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
