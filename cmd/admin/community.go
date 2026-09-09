package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// providerDiscord 是目前唯一支援的 provider。
//
// 沒有做成旗標:community_spaces.provider 支援 twitch / youtube,但
// hestia 的身分層現在只認得 Discord,註冊一個 twitch space 會建出一列
// 誰也用不到的資料。等身分層真的支援時再開這個旗標,而不是先給一個
// 看起來能用的假選項。
const providerDiscord = "discord"

// snowflakePattern 是 Discord id 的形狀。長度上限給到 20:
// snowflake 是 64-bit,十進位最多 20 位。
//
// 為什麼要驗:打錯一個字元不會有任何錯誤——只會建出一個永遠對不到的 space,
// 然後 bot 的每一則記錄都被拒,而你會以為是別的地方壞了。
var snowflakePattern = regexp.MustCompile(`^[0-9]{17,20}$`)

// channelKinds 對應 schemas/01 的 space_channels.kind。
var channelKinds = map[string]bool{
	"text": true, "voice": true, "forum": true, "announcement": true,
}

func requireSnowflake(label, value string) error {
	if value == "" {
		return fmt.Errorf("--%s 必填", label)
	}
	if !snowflakePattern.MatchString(value) {
		return fmt.Errorf("--%s %q 不像 Discord id(應該是 17~20 位數字)"+
			";要複製 id 請先在 Discord 設定 → 進階 開啟開發者模式", label, value)
	}
	return nil
}

// ── register-space ────────────────────────────────────────────

func registerSpace(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("register-space", flag.ContinueOnError)
	guild := fs.String("guild", "", "Discord 伺服器 id(snowflake)")
	name := fs.String("name", "", "顯示名稱(選填)")
	community := fs.String("community", "", "掛到既有社群的 public_id;省略則新建一個")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSnowflake("guild", *guild); err != nil {
		return err
	}

	q := db.New(pool)

	// 先查:重跑不該噴 UNIQUE violation,也不該無聲改掉既有設定。
	existing, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: *guild,
	})
	if err == nil {
		fmt.Printf("已經註冊過了,沒有變更:\n  space   %s(id=%d)\n  guild   %s\n",
			existing.PublicID, existing.ID, *guild)
		if existing.Name != nil {
			fmt.Printf("  名稱    %s\n", *existing.Name)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("查詢既有 space: %w", err)
	}

	// 建立 community 與 space 要嘛都成:只建了 community 會留下一個空社群,
	// 而下一次重跑會再建一個,越跑越多。
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("開啟交易: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := db.New(tx)

	comm, err := resolveCommunity(ctx, qtx, *community, *name, *guild)
	if err != nil {
		return err
	}

	spacePublicID, err := ulid.New()
	if err != nil {
		return fmt.Errorf("產生 public_id: %w", err)
	}
	space, err := qtx.CreateSpace(ctx, db.CreateSpaceParams{
		PublicID:    spacePublicID,
		CommunityID: comm.ID,
		Provider:    providerDiscord,
		ExternalID:  *guild,
		Name:        optional(*name),
	})
	if err != nil {
		return fmt.Errorf("建立 space: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交: %w", err)
	}

	fmt.Printf("註冊完成:\n  社群    %s(%s)\n  space   %s(id=%d)\n  guild   %s\n",
		comm.Name, comm.PublicID, space.PublicID, space.ID, *guild)
	fmt.Println("\n這個伺服器的訊息、語音與 XP 從現在起會被記錄。")
	fmt.Println("要保存訊息**內容**還需要逐頻道註冊(admin register-channel --log-messages)。")
	return nil
}

// community 是 resolveCommunity 的結果。用具名結構而不是三個回傳值:
// 兩個都是 string 的回傳值很容易在呼叫端被對調,而編譯器攔不住。
type community struct {
	ID       int64
	PublicID string
	Name     string
}

// resolveCommunity 決定 space 掛在哪個社群下:給了 --community 就用既有的,
// 否則新建一個。
func resolveCommunity(
	ctx context.Context, q *db.Queries, publicID, spaceName, guild string,
) (community, error) {
	if publicID != "" {
		c, err := q.GetCommunityByPublicID(ctx, publicID)
		if errors.Is(err, pgx.ErrNoRows) {
			return community{}, fmt.Errorf(
				"找不到 public_id 為 %q 的社群(用 admin list 看有哪些)", publicID)
		}
		if err != nil {
			return community{}, fmt.Errorf("查詢社群: %w", err)
		}
		return community{ID: c.ID, PublicID: c.PublicID, Name: c.Name}, nil
	}

	// 沒指定就用 space 的名字當社群名;連名字都沒給就用 guild id,
	// 至少看得出是哪一個,之後改名不影響任何關聯。
	name := strings.TrimSpace(spaceName)
	if name == "" {
		name = "Discord " + guild
	}
	id, err := ulid.New()
	if err != nil {
		return community{}, fmt.Errorf("產生 public_id: %w", err)
	}
	c, err := q.CreateCommunity(ctx, db.CreateCommunityParams{PublicID: id, Name: name})
	if err != nil {
		return community{}, fmt.Errorf("建立社群: %w", err)
	}
	return community{ID: c.ID, PublicID: c.PublicID, Name: c.Name}, nil
}

// ── register-channel ──────────────────────────────────────────

func registerChannel(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("register-channel", flag.ContinueOnError)
	guild := fs.String("guild", "", "Discord 伺服器 id")
	channel := fs.String("channel", "", "Discord 頻道 id")
	kind := fs.String("kind", "text", "text | voice | forum | announcement")
	name := fs.String("name", "", "顯示名稱(選填)")
	logMessages := fs.Bool("log-messages", false, "保存訊息內容(預設不保存)")
	noXP := fs.Bool("no-xp", false, "這個頻道不計 XP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSnowflake("guild", *guild); err != nil {
		return err
	}
	if err := requireSnowflake("channel", *channel); err != nil {
		return err
	}
	if !channelKinds[*kind] {
		return fmt.Errorf("--kind %q 不合法,只能是 text / voice / forum / announcement", *kind)
	}

	q := db.New(pool)
	space, err := q.GetSpaceByProviderExternalID(ctx, db.GetSpaceByProviderExternalIDParams{
		Provider: providerDiscord, ExternalID: *guild,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("guild %s 還沒註冊,先跑 admin register-space --guild %s", *guild, *guild)
	}
	if err != nil {
		return fmt.Errorf("查詢 space: %w", err)
	}

	row, err := q.UpsertSpaceChannel(ctx, db.UpsertSpaceChannelParams{
		SpaceID:     space.ID,
		ExternalID:  *channel,
		Name:        optional(*name),
		Kind:        *kind,
		LogMessages: *logMessages,
		GrantXp:     !*noXP,
	})
	if err != nil {
		return fmt.Errorf("註冊頻道: %w", err)
	}

	fmt.Printf("頻道已註冊:\n  channel %s(id=%d,kind=%s)\n  保存內容 %s\n  計 XP    %s\n",
		row.ExternalID, row.ID, row.Kind, yesNo(row.LogMessages), yesNo(row.GrantXp))
	return nil
}

// ── list ──────────────────────────────────────────────────────

func listAll(ctx context.Context, pool *pgxpool.Pool) error {
	q := db.New(pool)

	communities, err := q.ListCommunities(ctx)
	if err != nil {
		return fmt.Errorf("列出社群: %w", err)
	}
	if len(communities) == 0 {
		fmt.Println("還沒有任何社群。")
		fmt.Println("先跑:admin register-space --guild <你的伺服器 id> --name \"名稱\"")
		return nil
	}

	fmt.Println("社群:")
	for _, c := range communities {
		ruleset := "(無,採安全預設)"
		if c.XpRulesetID != nil {
			ruleset = fmt.Sprintf("ruleset=%d", *c.XpRulesetID)
		}
		fmt.Printf("  %s  %s  %s\n", c.PublicID, c.Name, ruleset)
	}

	spaces, err := q.ListSpaces(ctx)
	if err != nil {
		return fmt.Errorf("列出空間: %w", err)
	}
	fmt.Println("\n空間:")
	if len(spaces) == 0 {
		fmt.Println("  (無)")
		return nil
	}
	for _, s := range spaces {
		fmt.Printf("  %s  %s:%s  %s  ← %s\n",
			s.PublicID, s.Provider, s.ExternalID, derefOr(s.Name, "(未命名)"), s.CommunityName)

		channels, err := q.ListSpaceChannels(ctx, s.ID)
		if err != nil {
			return fmt.Errorf("列出頻道: %w", err)
		}
		if len(channels) == 0 {
			fmt.Println("      頻道:(未註冊任何頻道 —— 照樣計 XP,但不保存訊息內容)")
			continue
		}
		for _, ch := range channels {
			state := ""
			if ch.ArchivedAt != nil {
				state = "  [已封存]"
			}
			fmt.Printf("      %s  %-13s  保存內容=%s  計XP=%s%s\n",
				ch.ExternalID, derefOr(ch.Name, "("+ch.Kind+")"),
				yesNo(ch.LogMessages), yesNo(ch.GrantXp), state)
		}
	}
	return nil
}

// ── 小工具 ────────────────────────────────────────────────────

// optional 把空字串轉成 NULL:欄位可為 NULL,存空字串等於製造第二種「沒有值」。
func optional(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
