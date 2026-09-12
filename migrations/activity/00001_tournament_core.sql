-- +goose Up
-- schemas/20 + schemas/26 落地:賽事、跨屆選手檔案、參賽者、單淘汰賽程樹。
--
-- 這是 activity schema 的第一個 migration。
--
-- 跨 schema 邊界(規劃書 §37):activity 與 platform 是同一顆 Postgres 的兩個
-- schema,但 activity 引用使用者時**存 user_id 不設 FK**。未來拆庫時不用解約束,
-- 現在的代價是「使用者被刪掉時這裡不會擋」—— 而 platform.users 是軟刪除,
-- 所以那個情境不會發生。
--
-- ── 為什麼段位只存數字 ────────────────────────────────────────
--
-- rank_level SMALLINT 1..4 是段位的唯一表示法。名稱(開山/斷水/飛花/無我)
-- 與境界描述放 tournaments.config,不另建 ranks 表。
--
-- 四個值、逐屆可能微調措辭,一張表不划算;更重要的是 BP 公式要拿 level 做減法,
-- 存字串就得每次查表換算。存代碼字串 + 另存 level 則是同一個概念兩個權威位置。

-- ── 跨屆選手檔案 ──────────────────────────────────────────────
-- 執行期的 schema 前置由 migrations.Apply() 負責(goose 版本表依賴 activity schema
-- 先存在,migration 自己來不及建)。下面這行「宣告」是給 sqlc 的解析器認識 schema 用的,
-- 執行期因 IF NOT EXISTS 而為 no-op —— 職責不同,不是重複邏輯。
-- 與 platform/00001 的同一行是同一個理由。
CREATE SCHEMA IF NOT EXISTS activity;

CREATE TABLE activity.fencers (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id   TEXT   NOT NULL UNIQUE,
  -- 遊戲ID 是自然鍵(09-12 確認:燕雲的遊戲ID 全服唯一)。
  -- 它同時是登入帳號,也是報名時用來撈上屆資料的鍵。
  game_id     TEXT   NOT NULL UNIQUE,
  -- 弱參照 platform.users。綁定 Discord 後才有值。
  -- 參賽不需要帳號,但**領獎必須先綁**(grill Q24)—— 獎金走 Ledger,要有收款對象。
  user_id     BIGINT,
  discord_name TEXT,

  -- 以下四欄全是衍生資料,必須可從事實表重算(全域慣例)。
  -- 冗餘存放的唯一理由是報名頁與生涯頁都要即時讀,每次 JOIN 統計不划算。
  -- 對應的重算 SQL 寫在 schemas/26,測試必須驗證兩者一致。
  last_rank_level    SMALLINT,
  last_ranked_at     TIMESTAMPTZ,
  tournaments_played INT NOT NULL DEFAULT 0,
  wins               INT NOT NULL DEFAULT 0,
  losses             INT NOT NULL DEFAULT 0,

  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT fencers_last_rank_level_check
    CHECK (last_rank_level IS NULL OR last_rank_level BETWEEN 1 AND 4),
  CONSTRAINT fencers_counts_check
    CHECK (tournaments_played >= 0 AND wins >= 0 AND losses >= 0)
);

-- 部分索引:多數 fencer 不會綁帳號,沒必要為 NULL 建索引項
CREATE UNIQUE INDEX fencers_user_id_uq ON activity.fencers (user_id) WHERE user_id IS NOT NULL;

COMMENT ON TABLE activity.fencers IS
  '跨屆選手檔案,以遊戲ID 為自然鍵。與 tournament_players 的分工:這裡是跨屆聚合,'
  '那裡是報名當下的快照 —— 因為「段位一經確認即為本屆計算依據」,本屆段位不能被下屆覆寫。';

-- ── 賽事 ──────────────────────────────────────────────────────
CREATE TABLE activity.tournaments (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id    TEXT   NOT NULL UNIQUE,
  slug         TEXT   NOT NULL UNIQUE,
  name         TEXT   NOT NULL,
  -- 弱參照 platform.communities
  community_id BIGINT NOT NULL,
  phase        TEXT   NOT NULL DEFAULT 'signup',
  -- BP 規則、段位名稱、賠率參數、獎金表全在這裡。
  -- 逐屆可調正是活動層的性質;放獨立欄位會讓每次調規則都變成一次 ALTER。
  config       JSONB  NOT NULL DEFAULT '{}',
  -- 報名獎勵,經 Ledger 發放(reason=event_reward)。0 = 不發。
  signup_bonus BIGINT NOT NULL DEFAULT 0,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- 七階段。比舊草稿多了 ranking/ranked(段位要在配對前公布,留異議空間),
  -- 少了 preparation(準備期自我配裝的玩法整個移除)。
  CONSTRAINT tournaments_phase_check CHECK (phase IN (
    'signup', 'signup_closed', 'ranking', 'ranked', 'drawing', 'in_progress', 'finished'
  )),
  CONSTRAINT tournaments_signup_bonus_check CHECK (signup_bonus >= 0)
);

COMMENT ON COLUMN activity.tournaments.phase IS
  '階段限制一律在伺服器端驗證(requirePhase),前端變灰只是視覺。';

-- ── 參賽者(逐屆快照)──────────────────────────────────────────
CREATE TABLE activity.tournament_players (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id     TEXT   NOT NULL UNIQUE,
  tournament_id BIGINT NOT NULL REFERENCES activity.tournaments(id),
  fencer_id     BIGINT NOT NULL REFERENCES activity.fencers(id),
  -- 允許 NULL:純活動報名不需平台帳號(grill Q12)。
  -- 這造成雙軌身分 —— 選手用遊戲ID+通行碼,下注者必須有平台帳號。
  user_id       BIGINT,
  -- 報名當下的快照。之後改名不影響已公布的對戰表。
  display_name  TEXT   NOT NULL,

  -- 裁判評定。NULL = 未評;未評者不得進入抽籤(應用層擋)。
  rank_level    SMALLINT,
  ranked_at     TIMESTAMPTZ,
  ranked_by     BIGINT,  -- 弱參照 platform.users

  -- 通行碼只存 hash,明碼僅在報名送出當下顯示一次。裁判後台也看不到明碼,
  -- 只能「重新產生」(舊碼即失效,動作寫入 admin_audit_logs)。
  passcode_hash       TEXT NOT NULL,
  passcode_issued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- 報名表欄位(裁判靠這些評段)
  discord_name      TEXT NOT NULL,
  self_rated_level  SMALLINT,
  ladder_rank       TEXT,
  ladder_score      INT,
  arts_note         TEXT,
  availability_note TEXT,

  seed_no    INT,
  status     TEXT NOT NULL DEFAULT 'active',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT tournament_players_rank_level_check
    CHECK (rank_level IS NULL OR rank_level BETWEEN 1 AND 4),
  CONSTRAINT tournament_players_self_rated_level_check
    CHECK (self_rated_level IS NULL OR self_rated_level BETWEEN 1 AND 4),
  CONSTRAINT tournament_players_status_check
    CHECK (status IN ('active', 'eliminated', 'withdrawn')),
  -- 一人一屆只能報一次
  CONSTRAINT tournament_players_tournament_fencer_uq UNIQUE (tournament_id, fencer_id),
  -- 籤位不可重複。NULL(未抽)不受 UNIQUE 限制,正是我們要的
  CONSTRAINT tournament_players_tournament_seed_uq UNIQUE (tournament_id, seed_no)
);

CREATE INDEX tournament_players_tournament_idx ON activity.tournament_players (tournament_id);
CREATE INDEX tournament_players_fencer_idx ON activity.tournament_players (fencer_id);
-- 下注時要比對「下注者是不是本場選手」,這條走 user_id
CREATE INDEX tournament_players_user_idx
  ON activity.tournament_players (user_id) WHERE user_id IS NOT NULL;

COMMENT ON COLUMN activity.tournament_players.rank_level IS
  '本屆評定段位,1=開山 2=斷水 3=飛花 4=無我。一經確認即為本屆計算 BP 的依據。'
  '進入 drawing 階段後不可再改 —— BP 依段位差算出,抽籤後改段位會讓已發的 match_budgets 對不上。';

-- ── 賽程樹 ────────────────────────────────────────────────────
CREATE TABLE activity.matches (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id     TEXT   NOT NULL UNIQUE,
  tournament_id BIGINT NOT NULL REFERENCES activity.tournaments(id),
  round         INT    NOT NULL,
  slot          INT    NOT NULL,

  -- NULL = 尚未確定(等上一輪)。人數不設上限、不假設 2 的冪次,
  -- 輪空(bye)在建樹時就讓該方直接晉級,不建一場空比賽。
  p1_player_id     BIGINT REFERENCES activity.tournament_players(id),
  p2_player_id     BIGINT REFERENCES activity.tournament_players(id),
  winner_player_id BIGINT REFERENCES activity.tournament_players(id),

  status TEXT NOT NULL DEFAULT 'pending',

  -- 讓武開盤/封盤,裁判手動(不綁時鐘 —— 活動實際上常常延後)。
  -- 舊草稿這欄叫 shop_open,語意是「賽中商店」;現在是賽前的讓武選購。
  handicap_open      BOOLEAN NOT NULL DEFAULT false,
  handicap_locked_at TIMESTAMPTZ,

  stream_url  TEXT,
  -- walkover = 不戰而勝。該場所有注單作廢退款(schemas/21)。
  result_kind TEXT NOT NULL DEFAULT 'normal',

  started_at  TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- 讓武與下注是兩條不同的時間線:
  --   pending 對手未齊        讓武 ✗  下注 ✗
  --   ready   雙方確定已開盤  讓武 ✓  下注 ✓
  --   locked  讓武封盤已公開  讓武 ✗  下注 ✓
  --   live    比賽中          讓武 ✗  下注 ✗
  --   done    已分勝負        讓武 ✗  下注 ✗
  -- 讓武要早收(對手需要時間適應),下注可以開到開賽前一刻。
  CONSTRAINT matches_status_check
    CHECK (status IN ('pending', 'ready', 'locked', 'live', 'done')),
  CONSTRAINT matches_result_kind_check
    CHECK (result_kind IN ('normal', 'walkover')),
  CONSTRAINT matches_round_check CHECK (round >= 1),
  CONSTRAINT matches_slot_check CHECK (slot >= 0),
  -- 不能自己打自己。建樹有 bug 時這條會當場擋下來
  CONSTRAINT matches_distinct_players_check
    CHECK (p1_player_id IS NULL OR p2_player_id IS NULL OR p1_player_id <> p2_player_id),
  -- 勝者必須是場上兩人之一
  CONSTRAINT matches_winner_is_participant_check
    CHECK (winner_player_id IS NULL
           OR winner_player_id = p1_player_id
           OR winner_player_id = p2_player_id),
  -- 封盤時間只在封盤後才有意義
  CONSTRAINT matches_locked_at_check
    CHECK (handicap_locked_at IS NULL OR status IN ('locked', 'live', 'done')),
  CONSTRAINT matches_tournament_round_slot_uq UNIQUE (tournament_id, round, slot)
);

CREATE INDEX matches_tournament_round_idx ON activity.matches (tournament_id, round);
-- 對戰表要標 LIVE、下注頁要列可下注場次,兩者都走 status
CREATE INDEX matches_tournament_status_idx ON activity.matches (tournament_id, status);

COMMENT ON TABLE activity.matches IS
  '單淘汰賽程樹。round 1 = 首輪,slot 為該輪內 0-based 位置。'
  '同輪比賽彼此不共用選手 —— 這是串關天然無相關的結構性保證,不需要特判。';

-- +goose Down
DROP TABLE activity.matches;
DROP TABLE activity.tournament_players;
DROP TABLE activity.tournaments;
DROP TABLE activity.fencers;
