package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/handicappg"
	"github.com/danicotech/hestia/internal/infrastructure/storage/testdb"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// catalogue-sync 的價值在於「跑完之後 DB 裡的文字是 binary 那一份、而且有人簽名」,
// 所以驗的是 DB 裡的文字、cost、稽核列;不是指令有沒有回 nil。
// 規則本身(拒絕、列數核對)在 handicappg 的測試裡,這裡只驗 CLI 接線那一層。

func newTournament(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	pub, err := ulid.New()
	if err != nil {
		t.Fatalf("ulid: %v", err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase)
		 VALUES ($1, $2, '測試賽事', 1, 'signup') RETURNING id`, pub, slug).Scan(&id); err != nil {
		t.Fatalf("建賽事: %v", err)
	}
	return id
}

func newUser(t *testing.T, pool *pgxpool.Pool) (int64, string) {
	t.Helper()
	pub, err := ulid.New()
	if err != nil {
		t.Fatalf("ulid: %v", err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO platform.users (public_id, display_name) VALUES ($1, '管理員') RETURNING id`, pub).Scan(&id); err != nil {
		t.Fatalf("建使用者: %v", err)
	}
	return id, pub
}

func refereeNoteOf(t *testing.T, pool *pgxpool.Pool, tournamentID int64, key string) string {
	t.Helper()
	var note *string
	if err := pool.QueryRow(context.Background(),
		`SELECT referee_note FROM activity.handicap_items WHERE tournament_id = $1 AND key = $2`,
		tournamentID, key).Scan(&note); err != nil {
		t.Fatalf("讀 referee_note: %v", err)
	}
	if note == nil {
		return ""
	}
	return *note
}

func TestCatalogueSync(t *testing.T) {
	pool := testdb.Start(t)
	ctx := context.Background()
	repo := handicappg.New(pool)

	tournamentID := newTournament(t, pool, "sync-me")
	if _, err := handicap.New(repo).InstallSeedItems(ctx, tournamentID); err != nil {
		t.Fatalf("InstallSeedItems: %v", err)
	}
	actorID, actorPub := newUser(t, pool)

	const key = "victory.survive_90s"
	// 舊草稿:文字與 cost 都改掉,同步後只有文字該回來。
	if _, err := pool.Exec(ctx,
		`UPDATE activity.handicap_items SET referee_note = '舊草稿', cost = 999
		  WHERE tournament_id = $1 AND key = $2`, tournamentID, key); err != nil {
		t.Fatalf("改成舊草稿: %v", err)
	}

	t.Run("同步文字、不動 cost、寫稽核", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return catalogueSync(ctx, pool, []string{"--tournament", "sync-me", "--actor", actorPub})
		})
		if err != nil {
			t.Fatalf("catalogue-sync: %v\n%s", err, out)
		}
		if !strings.Contains(out, "更新    34 項") {
			t.Errorf("輸出應報 34 項:\n%s", out)
		}
		if got := refereeNoteOf(t, pool, tournamentID, key); got == "舊草稿" || got == "" {
			t.Errorf("referee_note 沒有同步:%q", got)
		}
		var cost int64
		if err := pool.QueryRow(ctx,
			`SELECT cost FROM activity.handicap_items WHERE tournament_id = $1 AND key = $2`,
			tournamentID, key).Scan(&cost); err != nil {
			t.Fatalf("讀 cost: %v", err)
		}
		if cost != 999 {
			t.Errorf("cost 被動了:%d", cost)
		}

		// 稽核:actor 是 --actor 那個人、target 是這一屆、after 是報告。
		var gotActor, gotTarget int64
		var targetType string
		var after []byte
		if err := pool.QueryRow(ctx,
			`SELECT actor_user_id, target_type, target_id, after FROM platform.admin_audit_logs
			  WHERE action = $1 ORDER BY id DESC LIMIT 1`, auditActionCatalogueSync,
		).Scan(&gotActor, &targetType, &gotTarget, &after); err != nil {
			t.Fatalf("讀稽核: %v", err)
		}
		if gotActor != actorID || targetType != "tournament" || gotTarget != tournamentID {
			t.Errorf("稽核 actor=%d target=%s/%d,想要 %d / tournament/%d", gotActor, targetType, gotTarget, actorID, tournamentID)
		}
		var rep handicap.SyncReport
		if err := json.Unmarshal(after, &rep); err != nil || rep.Updated != 34 {
			t.Errorf("稽核 after 應是同步報告:%s (%v)", after, err)
		}
	})

	t.Run("有選擇時拒絕且不寫稽核", func(t *testing.T) {
		// 直接塞一場比賽與一筆選擇(規則在 handicappg 測;這裡只要「有一筆」)。
		var fencerID, playerID, matchID, itemID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO activity.fencers (public_id, game_id) VALUES ($1, $2) RETURNING id`,
			mustULID(t), "g-"+mustULID(t)).Scan(&fencerID); err != nil {
			t.Fatalf("建選手檔案: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO activity.tournament_players
			   (public_id, tournament_id, fencer_id, display_name, passcode_hash, discord_name, rank_level)
			 VALUES ($1, $2, $3, '選手', 'x', 'd', 1) RETURNING id`,
			mustULID(t), tournamentID, fencerID).Scan(&playerID); err != nil {
			t.Fatalf("建參賽者: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO activity.matches (public_id, tournament_id, round, slot, p1_player_id, status)
			 VALUES ($1, $2, 1, 0, $3, 'ready') RETURNING id`,
			mustULID(t), tournamentID, playerID).Scan(&matchID); err != nil {
			t.Fatalf("建場次: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO activity.match_budgets (match_id, player_id, budget) VALUES ($1, $2, 24)`,
			matchID, playerID); err != nil {
			t.Fatalf("建預算: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT id FROM activity.handicap_items WHERE tournament_id = $1 AND key = $2`,
			tournamentID, key).Scan(&itemID); err != nil {
			t.Fatalf("讀項目: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO activity.handicap_selections (public_id, match_id, player_id, item_id, cost, voided)
			 VALUES ($1, $2, $3, $4, 1, true)`, mustULID(t), matchID, playerID, itemID); err != nil {
			t.Fatalf("建選擇: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE activity.handicap_items SET referee_note = '舊草稿' WHERE tournament_id = $1 AND key = $2`,
			tournamentID, key); err != nil {
			t.Fatalf("改成舊草稿: %v", err)
		}

		before := countRows(t, pool, `SELECT count(*) FROM platform.admin_audit_logs WHERE action = $1`, auditActionCatalogueSync)
		err := catalogueSync(ctx, pool, []string{"--tournament", "sync-me", "--actor", actorPub})
		if !errors.Is(err, handicap.ErrCatalogueInUse) {
			t.Fatalf("err = %v,想要 ErrCatalogueInUse", err)
		}
		if got := refereeNoteOf(t, pool, tournamentID, key); got != "舊草稿" {
			t.Errorf("被拒的同步不該動到文字:%q", got)
		}
		if after := countRows(t, pool, `SELECT count(*) FROM platform.admin_audit_logs WHERE action = $1`, auditActionCatalogueSync); after != before {
			t.Errorf("被拒的同步不該留下稽核(%d → %d)", before, after)
		}
	})

	t.Run("參數與查無資料", func(t *testing.T) {
		for name, args := range map[string][]string{
			"缺 tournament": {"--actor", actorPub},
			"缺 actor":      {"--tournament", "sync-me"},
			"賽事不存在":        {"--tournament", "no-such", "--actor", actorPub},
			"使用者不存在":       {"--tournament", "sync-me", "--actor", "01ZZZZZZZZZZZZZZZZZZZZZZZZ"},
		} {
			if err := catalogueSync(ctx, pool, args); err == nil {
				t.Errorf("%s 應該報錯", name)
			}
		}
	})
}

func mustULID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New()
	if err != nil {
		t.Fatalf("ulid: %v", err)
	}
	return id
}
