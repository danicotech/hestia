package tournamentpg_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/rules"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// config 的讀寫都走 rules(經 tournament.MarshalConfig / ParseConfig 委派):
// 這裡測的是「經過真的 JSONB 欄位之後」往返仍然成立 —— Postgres 會重排鍵、
// 正規化空白,位元組不會相同,但語意必須相同。

// insertTournamentWithConfig 直接塞一列賽事,config 給什麼存什麼。
// 模擬的是「升版前就存在的列」與「migration 00008 產生的列」。
func insertTournamentWithConfig(t *testing.T, config string) string {
	t.Helper()
	slug := fmt.Sprintf("cfg-%d", nextSeq())
	var communityID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM platform.communities WHERE public_id = $1`, newCommunity(t),
	).Scan(&communityID); err != nil {
		t.Fatalf("查社群: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO activity.tournaments (public_id, slug, name, community_id, phase, config)
		 VALUES (gen_random_uuid()::text, $1, '設定測試', $2, 'signup', $3::jsonb)`,
		slug, communityID, config,
	); err != nil {
		t.Fatalf("建賽事: %v", err)
	}
	return slug
}

// 建賽事寫進去的 config 讀回來要是同一份,而且欄位裡存的是 v2。
func TestConfig_CreateRoundTripsThroughJSONB(t *testing.T) {
	setup(t)
	ctx := context.Background()

	bestOf, third, gap := 3, true, int64(10)
	cfg, err := tournament.NewConfig(tournament.ConfigOverrides{
		BestOf: &bestOf, ThirdPlaceMatch: &third, BPPerRankGap: &gap,
		Prizes: &tournament.Prizes{Champion: 5000},
	})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	raw, err := tournament.MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig: %v", err)
	}
	p := createParams(t, newCommunity(t))
	p.ActorUserID = newUser(t)
	p.ConfigRaw = raw

	created, err := svc.Create(ctx, p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := svc.TournamentBySlug(ctx, created.Slug)
	if err != nil {
		t.Fatalf("TournamentBySlug: %v", err)
	}
	parsed, err := tournament.ParseConfig(got.ConfigRaw)
	if err != nil {
		t.Fatalf("自己寫的 config 讀回來不該有診斷:%v", err)
	}
	if !reflect.DeepEqual(parsed, cfg) {
		t.Errorf("往返不等:\n got %+v\nwant %+v", parsed, cfg)
	}

	var version int
	if err := pool.QueryRow(ctx,
		`SELECT (config->>'version')::int FROM activity.tournaments WHERE id = $1`, created.ID,
	).Scan(&version); err != nil {
		t.Fatalf("讀 version: %v", err)
	}
	if version != rules.CurrentVersion {
		t.Errorf("欄位裡的 version = %d,want %d", version, rules.CurrentVersion)
	}
}

// migration 00008 對開發庫那屆產生的形狀,經過 JSONB 存取之後 Parse 必須零診斷,
// 且是第一屆定案的值(三局兩勝、季軍戰、四種盤口、duration 線 90 秒)。
func TestConfig_Migration00008ShapeReadsClean(t *testing.T) {
	setup(t)
	slug := insertTournamentWithConfig(t, `{
	  "version": 2,
	  "ranks": [
	    {"level": 1, "name": "開山", "title": "初試之境", "description": "初入 PVP、實戰場次較少。"},
	    {"level": 2, "name": "斷水", "title": "初成之境", "description": "已具操作能力,對局觀念尚未成熟。"},
	    {"level": 3, "name": "飛花", "title": "純熟之境", "description": "成熟的 PVP 實力與足夠實戰經驗。"},
	    {"level": 4, "name": "無我", "title": "歷戰之境", "description": "頂尖實戰能力與大量對局經驗。"}
	  ],
	  "bp": {"kind": "linear_gap", "per_rank_gap": 8},
	  "format": {"best_of": 3, "preamble_every_round": true, "third_place_match": true},
	  "handicap": {"draw_pools": {"wuxue": []}},
	  "timer": {"start": "round_start"},
	  "betting": {
	    "enabled": true,
	    "markets": [
	      {"kind": "match_winner"}, {"kind": "round_winner"},
	      {"kind": "duration", "line_seconds": 90}, {"kind": "score"}
	    ],
	    "odds": {"smoothing": 5, "vig_bps": 800, "min_odds_milli": 1050, "max_odds_milli": 12000, "max_parlay_milli": 300000, "kind": "vote_share"},
	    "parlay": {"legs_per_match": 1},
	    "close_at": "first_round_start"
	  },
	  "prizes": {"champion": 0, "runner_up": 0, "third": 0, "participation": 0}
	}`)

	got, err := svc.TournamentBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("TournamentBySlug: %v", err)
	}
	cfg, err := tournament.ParseConfig(got.ConfigRaw)
	if err != nil {
		t.Fatalf("00008 的形狀不該有診斷:%v", err)
	}
	if cfg.Format != (rules.FormatRule{BestOf: 3, PreambleEveryRound: true, ThirdPlaceMatch: true}) {
		t.Errorf("format = %+v", cfg.Format)
	}
	if len(cfg.Betting.Markets) != 4 || cfg.Betting.Markets[2] != (rules.MarketRule{Kind: rules.MarketDuration, LineSeconds: 90}) {
		t.Errorf("markets = %+v", cfg.Betting.Markets)
	}
	if cfg.BP.PerRankGap != 8 || cfg.Handicap.ItemMaxQty != nil {
		t.Errorf("bp = %+v, item_max_qty = %v", cfg.BP, cfg.Handicap.ItemMaxQty)
	}
}

// 升版前的 v1 列(讀取端的相容保險):零診斷、v1 語意。
func TestConfig_V1RowStillReads(t *testing.T) {
	setup(t)
	slug := insertTournamentWithConfig(t, `{"bp_per_rank_gap": 12, "odds": {"vig_bps": 500}, "prizes": {"champion": 100}}`)

	got, err := svc.TournamentBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("TournamentBySlug: %v", err)
	}
	cfg, err := tournament.ParseConfig(got.ConfigRaw)
	if err != nil {
		t.Fatalf("合法的 v1 不該有診斷:%v", err)
	}
	if cfg.Version != rules.CurrentVersion || cfg.BP.PerRankGap != 12 || cfg.Betting.Odds.VigBPS != 500 || cfg.Prizes.Champion != 100 {
		t.Errorf("v1 的鍵沒搬到 v2 的位置:%+v", cfg)
	}
	if cfg.Format.BestOf != 1 || cfg.Format.ThirdPlaceMatch || len(cfg.Betting.Markets) != 1 {
		t.Errorf("v1 升版要是 v1 語意(單場、無季軍戰、只有勝負盤):%+v", cfg.Format)
	}
}
