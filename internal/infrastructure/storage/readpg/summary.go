package readpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 個人檔案總覽與排行榜(schemas/24、25)。
//
// 等級一律在這裡算完才送出去。曲線存在 xp_rulesets.config,由 DB 讀出來;
// 讓 Discord 閘道與網頁各自算一次,就會有兩份實作,而其中一份遲早會跟
// 資料庫裡的設定不同步 —— 那時「我在網頁上是 12 級,bot 說我 11 級」
// 這種問題沒有人查得出來。

// maxBadgesInSummary 是總覽顯示的徽章數上限。
//
// 不是全部:Discord 的 embed 有欄位數與長度上限,收集了兩百枚徽章的人
// 會讓整則訊息被截斷,而被截掉的是最舊的那些(也就是最珍貴的那些)。
// 完整清單走 ListEntitlements / 網頁。
const maxBadgesInSummary = 12

// Summary 一次讀完 /profile 要的全部內容。
//
// 四次查詢而不是四支 RPC:Discord 的互動視窗只有 3 秒,四次往返很容易超時,
// 而超時的表現是指令「沒有反應」—— 使用者只會再按一次。
func (s *Service) Summary(ctx context.Context, userID int64) (*readmodel.SummaryView, error) {
	profile, err := s.Profile(ctx, userID)
	if err != nil {
		return nil, err
	}
	balances, err := s.Balances(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("讀餘額: %w", err)
	}

	xpRows, err := s.q.ListUserXP(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("讀 XP: %w", err)
	}
	levels := make([]readmodel.CommunityXPView, 0, len(xpRows))
	for _, r := range xpRows {
		// config 為 NULL(community 沒指派 ruleset)時 ParseConfig 回安全預設,
		// 所以這裡不必分支 —— 與 GetCommunityXpConfig 同樣的處置。
		cfg, err := xp.ParseConfig(r.RulesetConfig)
		if err != nil {
			return nil, fmt.Errorf("解析社群 %s 的 XP 設定: %w", r.CommunityPublicID, err)
		}
		p := cfg.ProgressFor(r.Xp)
		levels = append(levels, readmodel.CommunityXPView{
			CommunityPublicID: r.CommunityPublicID,
			CommunityName:     r.CommunityName,
			XP:                r.Xp,
			Level:             p.Level,
			XPIntoLevel:       p.XPIntoLevel,
			XPForLevel:        p.XPForLevel,
		})
	}

	pet, err := s.deployedPet(ctx, userID)
	if err != nil {
		return nil, err
	}

	badgeRows, err := s.q.ListUserBadges(ctx, db.ListUserBadgesParams{OwnerID: userID, Limit: maxBadgesInSummary})
	if err != nil {
		return nil, fmt.Errorf("讀徽章: %w", err)
	}
	badges := make([]readmodel.BadgeView, 0, len(badgeRows))
	for _, b := range badgeRows {
		badges = append(badges, readmodel.BadgeView{
			PublicID:   b.PublicID,
			Name:       b.Name,
			Rarity:     deref(b.Rarity),
			IconURL:    deref(b.IconUrl),
			AcquiredAt: b.AcquiredAt,
		})
	}

	return &readmodel.SummaryView{
		Profile:  *profile,
		Balances: balances,
		XP:       levels,
		Pet:      pet,
		Badges:   badges,
	}, nil
}

// deployedPet 讀出戰中的寵物。沒有出戰寵物是常態,不是錯誤。
func (s *Service) deployedPet(ctx context.Context, userID int64) (*readmodel.PetView, error) {
	row, err := s.q.GetDeployedPet(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // 沒有出戰寵物是正常狀態,不是找不到
	}
	if err != nil {
		return nil, fmt.Errorf("讀出戰寵物: %w", err)
	}
	// 寵物與使用者共用同一條曲線(schemas/24)。寵物不屬於任何社群,
	// 所以用預設曲線 —— 牠的 XP 來源跨社群,綁其中一個社群的規則沒有道理。
	var cfg xp.Config
	p := cfg.ProgressFor(row.Xp)
	return &readmodel.PetView{
		PublicID: row.PublicID,
		Name:     row.Name,
		IconURL:  deref(row.IconUrl),
		Rarity:   deref(row.Rarity),
		XP:       row.Xp,
		Level:    p.Level,
	}, nil
}

// Leaderboard 回某個社群的 XP 排行。
//
// 走 user_xp 的索引,不掃 xp_events —— XP 不進帳本的理由之一就是這個:
// 排行榜若要 GROUP BY 幾百萬列分錄,每次開啟都是全表掃描。
func (s *Service) Leaderboard(
	ctx context.Context, communityPublicID string, limit int32,
) ([]readmodel.LeaderboardEntry, error) {
	if limit <= 0 || limit > 50 {
		// 夾到上限而不是報錯:呼叫端的意思是「給我一個排行榜」,
		// 為了一個顯示參數讓整個指令失敗沒有好處。
		limit = 10
	}
	if communityPublicID == "" {
		// 呼叫端沒帶社群(Discord 指令目前拿不到:事件還沒帶發生地)。
		// 只有在全庫剛好一個社群時才代為推定,兩個以上寧可失敗 ——
		// 猜錯的話 B 社群的人會看到 A 社群的排行榜,而那沒有任何徵兆。
		sole, err := s.q.GetSoleCommunityPublicID(ctx)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("沒有指定社群,而系統裡不是剛好一個社群: %w", shop.ErrUserNotFound)
			}
			return nil, fmt.Errorf("推定社群: %w", err)
		}
		communityPublicID = sole
	}
	rows, err := s.q.LeaderboardByXP(ctx, db.LeaderboardByXPParams{PublicID: communityPublicID, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("讀排行榜: %w", err)
	}
	// 曲線讀一次就好:同一個社群的所有人套用同一份設定。
	// 排行榜是空的時候沒有曲線可查也沒關係 —— 沒有人要算等級。
	var cfg xp.Config
	if len(rows) > 0 {
		raw, err := s.q.GetCommunityCurveByPublicID(ctx, communityPublicID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("讀社群 XP 設定: %w", err)
		}
		if cfg, err = xp.ParseConfig(raw); err != nil {
			return nil, fmt.Errorf("解析社群 XP 設定: %w", err)
		}
	}
	out := make([]readmodel.LeaderboardEntry, 0, len(rows))
	for i, r := range rows {
		out = append(out, readmodel.LeaderboardEntry{
			Rank:        int32(i) + 1, //nolint:gosec // limit 夾在 50 以內
			PublicID:    r.PublicID,
			DisplayName: deref(r.DisplayName),
			XP:          r.Xp,
			Level:       cfg.ProgressFor(r.Xp).Level,
		})
	}
	return out, nil
}

// SoleCommunityID 實作 transport.CommunityResolver。
//
// 只有在全庫剛好一個社群時才回答。兩個以上時回錯,理由見介面說明:
// 猜錯的後果是跨社群的資料互相汙染,而且沒有任何徵兆。
func (s *Service) SoleCommunityID(ctx context.Context) (int64, error) {
	id, err := s.q.GetSoleCommunityID(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("系統裡不是剛好一個社群,無法推定: %w", shop.ErrUserNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("推定社群: %w", err)
	}
	return id, nil
}
