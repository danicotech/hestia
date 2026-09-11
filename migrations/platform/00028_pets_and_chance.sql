-- +goose Up
-- schemas/25 落地:寵物、裝備、抽獎活動、開箱、小遊戲、共用抽籤留痕。
--
-- 核心決定:寵物與裝備**不另建一套所有權系統**。item_definitions /
-- item_instances 已經處理好稀缺(max_supply)、綁定(bind_on_acquire)與
-- 交易鎖(locked_by_trade_id),而那三件正是寵物需要的。重寫等於把最容易
-- 出錯的交易鎖再寫一遍。
--
-- category 沒有 CHECK 約束,所以 'pet' / 'equipment' 兩個新分類不需要 DDL。
-- 既有分類:collectible / consumable / badge / role / prize。
-- badge 已經存在 —— **獎章不是新東西**,它就是物品系統的一個分類。

-- ── 物品鎖的原因 ──────────────────────────────────────────────
--
-- locked_by_trade_id 原本的語意是「被某筆交易鎖住」。裝備不是交易,
-- 但同樣必須鎖:貓戴著項圈 → 同一個項圈掛市集賣掉 → 貓還戴著 → 項圈變兩個。
--
-- 加 locked_reason 而不是沿用同一個欄位:排查「為什麼這件賣不掉」時
-- 要看得出是掛在市集還是穿在身上,否則只能一張表一張表去猜。
ALTER TABLE platform.item_instances
  ADD COLUMN locked_reason TEXT,
  ADD CONSTRAINT item_instances_locked_reason_check
    CHECK (locked_reason IS NULL OR locked_reason IN ('trade', 'equipped'));

-- 複合外鍵的被參照側:讓 pet_states 能把 owner_id 帶過去而不可能不一致
ALTER TABLE platform.item_instances
  ADD CONSTRAINT item_instances_id_owner_uq UNIQUE (id, owner_id);

-- ── 寵物狀態 ──────────────────────────────────────────────────
CREATE TABLE platform.pet_states (
  -- 一隻寵物一列,PK 直接用實例 id,不另設代理鍵
  item_instance_id BIGINT PRIMARY KEY REFERENCES platform.item_instances(id),
  -- owner_id 是 item_instances.owner_id 的受控副本(複合外鍵保證一致)。
  -- 存它的唯一理由是下面那條部分唯一索引:「同一個人同時只能出戰一隻」
  -- 是玩法核心規則,值得由 DB 保證而不是靠每個呼叫端自律。
  owner_id         BIGINT NOT NULL,
  nickname         TEXT,
  xp               BIGINT NOT NULL DEFAULT 0,
  deployed         BOOLEAN NOT NULL DEFAULT false,
  -- 本輪不使用,為「照顧」玩法預留。加欄位比日後 ALTER 一張大表便宜。
  last_fed_at      TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT pet_states_xp_check CHECK (xp >= 0),
  CONSTRAINT pet_states_owner_fkey
    FOREIGN KEY (item_instance_id, owner_id)
    REFERENCES platform.item_instances (id, owner_id)
);

-- 換手時外鍵會擋住「狀態列還記著舊主人」的不一致
CREATE UNIQUE INDEX pet_states_deployed_uq
  ON platform.pet_states (owner_id) WHERE deployed;

CREATE INDEX pet_states_owner_idx ON platform.pet_states (owner_id);
-- 排行榜:本社群最高等的寵物(等級是 xp 的純函數,排 xp 即等於排等級)
CREATE INDEX pet_states_xp_idx ON platform.pet_states (xp DESC);

COMMENT ON TABLE platform.pet_states IS
  '寵物的可變狀態。等級不存欄位,由 xp 用 xp_rulesets.config 的曲線反推(schemas/24)。'
  '擁有權、稀缺、交易鎖全在 item_instances —— 寵物可以連同等級在市集換手。';

-- ── 寵物裝備 ──────────────────────────────────────────────────
CREATE TABLE platform.pet_equipment (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pet_instance_id  BIGINT NOT NULL REFERENCES platform.pet_states(item_instance_id),
  slot             TEXT   NOT NULL,
  -- UNIQUE:一件裝備同時只能戴在一隻身上。沒有這條就能一件當多件用。
  item_instance_id BIGINT NOT NULL UNIQUE REFERENCES platform.item_instances(id),
  equipped_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT pet_equipment_slot_check
    CHECK (slot IN ('collar', 'hat', 'accessory')),
  -- 一個部位一件
  CONSTRAINT pet_equipment_slot_uq UNIQUE (pet_instance_id, slot)
);

-- ── 寵物 XP ───────────────────────────────────────────────────
--
-- 一個活動事件同時給使用者與當前出戰寵物各自的 XP(各自獨立但同時漲)。
-- 多寫一列而不是另開一張表:寵物 XP 因此可從 xp_events 重算,
-- 符合「衍生資料必須可重建」。
ALTER TABLE platform.xp_events
  ADD COLUMN pet_instance_id BIGINT REFERENCES platform.item_instances(id);

COMMENT ON COLUMN platform.xp_events.pet_instance_id IS
  'NULL = 這筆 XP 是給人的;有值 = 給那隻寵物的。'
  '同一個活動事件會產生兩列(人一列、出戰寵物一列)。';

-- ── 抽獎活動(raffle)───────────────────────────────────────────
--
-- 與開箱的關鍵差別:獎品是**管理員先掏出來的**,所以總量不變,只換口袋。
-- 開箱才是水龍頭。
CREATE TABLE platform.giveaways (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id    TEXT NOT NULL UNIQUE,
  community_id BIGINT NOT NULL REFERENCES platform.communities(id),
  space_id     BIGINT NOT NULL REFERENCES platform.community_spaces(id),
  title        TEXT NOT NULL,
  prize_kind   TEXT NOT NULL,
  prize_ref    TEXT NOT NULL,
  prize_amount BIGINT,
  winner_count INT  NOT NULL DEFAULT 1,
  entry_cost   BIGINT NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'open',
  opens_at     TIMESTAMPTZ NOT NULL,
  closes_at    TIMESTAMPTZ NOT NULL,
  drawn_at     TIMESTAMPTZ,
  created_by   BIGINT NOT NULL REFERENCES platform.users(id),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT giveaways_prize_kind_check
    CHECK (prize_kind IN ('tokens', 'item', 'entitlement')),
  CONSTRAINT giveaways_status_check
    CHECK (status IN ('open', 'drawn', 'cancelled')),
  CONSTRAINT giveaways_winner_count_check CHECK (winner_count > 0),
  CONSTRAINT giveaways_entry_cost_check CHECK (entry_cost >= 0),
  CONSTRAINT giveaways_amount_check
    CHECK ((prize_kind = 'tokens') = (prize_amount IS NOT NULL)
           AND (prize_amount IS NULL OR prize_amount > 0)),
  -- 關得比開還早的抽獎沒有人參加得到,而那個錯誤在建立當下完全看不出來
  CONSTRAINT giveaways_window_check CHECK (closes_at > opens_at)
);

CREATE INDEX giveaways_open_idx
  ON platform.giveaways (community_id, closes_at) WHERE status = 'open';

COMMENT ON TABLE platform.giveaways IS
  '獎品在**建立時**就從管理員帳上扣除並凍結,不是開獎時才扣 —— '
  '否則管理員在開獎前把點數花光,抽獎就變成空頭支票。';

CREATE TABLE platform.giveaway_entries (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  giveaway_id BIGINT NOT NULL REFERENCES platform.giveaways(id),
  user_id     BIGINT NOT NULL REFERENCES platform.users(id),
  won         BOOLEAN NOT NULL DEFAULT false,
  entered_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- 一人一次。靠約束擋,不靠應用層檢查(與每日簽到同理)
  CONSTRAINT giveaway_entries_uq UNIQUE (giveaway_id, user_id)
);

CREATE INDEX giveaway_entries_winners_idx
  ON platform.giveaway_entries (giveaway_id) WHERE won;

-- ── 開箱 ──────────────────────────────────────────────────────
CREATE TABLE platform.loot_boxes (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id     TEXT NOT NULL UNIQUE,
  community_id  BIGINT NOT NULL REFERENCES platform.communities(id),
  name          TEXT NOT NULL,
  cost_currency TEXT NOT NULL REFERENCES platform.currencies(code),
  cost_amount   BIGINT NOT NULL,
  -- 每人每日抽取上限;NULL = 不限
  daily_limit   INT,
  enabled       BOOLEAN NOT NULL DEFAULT true,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT loot_boxes_cost_check CHECK (cost_amount > 0),
  CONSTRAINT loot_boxes_daily_limit_check CHECK (daily_limit IS NULL OR daily_limit > 0)
);

CREATE TABLE platform.loot_box_items (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  box_id      BIGINT NOT NULL REFERENCES platform.loot_boxes(id),
  reward_kind TEXT   NOT NULL,
  reward_ref  TEXT   NOT NULL,
  amount      BIGINT,
  -- 權重而非百分比:加一個新獎品時不必重算其他所有項目的百分比,
  -- 而那正是每次都會算錯的地方。實際機率 = weight / SUM(weight),
  -- 只計入 remaining IS NULL OR remaining > 0 的列。
  weight      INT    NOT NULL,
  -- NULL = 無限(機率表);有值 = 有限獎池,抽完即止。
  -- 有限獎池是 max_supply 的搭配:純機率表會讓「全世界只有 1 個」形同虛設。
  remaining   INT,

  CONSTRAINT loot_box_items_kind_check
    CHECK (reward_kind IN ('tokens', 'item', 'entitlement')),
  CONSTRAINT loot_box_items_weight_check CHECK (weight > 0),
  CONSTRAINT loot_box_items_remaining_check CHECK (remaining IS NULL OR remaining >= 0),
  CONSTRAINT loot_box_items_amount_check
    CHECK ((reward_kind = 'tokens') = (amount IS NOT NULL)
           AND (amount IS NULL OR amount > 0)),
  -- 點數沒有稀缺性,用「剩 30 個 100 點」管理它只會很奇怪
  CONSTRAINT loot_box_items_tokens_unlimited_check
    CHECK (reward_kind <> 'tokens' OR remaining IS NULL)
);

CREATE INDEX loot_box_items_box_idx ON platform.loot_box_items (box_id);

-- ── 共用抽籤留痕 ──────────────────────────────────────────────
--
-- 開箱、小遊戲、賽事下注三處都會產生隨機結果。各寫各的就無法回答
-- 「這個月系統總共吐出多少點」—— 而那是唯一能在通膨之前發現通膨的方法。
CREATE TABLE platform.chance_draws (
  id            BIGINT GENERATED ALWAYS AS IDENTITY,
  community_id  BIGINT NOT NULL REFERENCES platform.communities(id),
  user_id       BIGINT NOT NULL REFERENCES platform.users(id),
  kind          TEXT   NOT NULL,
  ref           TEXT   NOT NULL,
  -- **抽籤當下的賠率表,不是指向設定的參照。**
  -- 賠率會改;存參照的話,三個月後查這筆紀錄看到的是今天的賠率,
  -- 那份紀錄就失去了證明力。與帳本 append-only 是同一個原則。
  odds_snapshot JSONB  NOT NULL,
  seed          TEXT   NOT NULL,
  outcome       JSONB  NOT NULL,
  stake         BIGINT NOT NULL DEFAULT 0,
  payout        BIGINT NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (id, created_at),
  CONSTRAINT chance_draws_kind_check
    CHECK (kind IN ('loot_box', 'minigame', 'betting')),
  CONSTRAINT chance_draws_stake_check CHECK (stake >= 0),
  CONSTRAINT chance_draws_payout_check CHECK (payout >= 0)
) PARTITION BY RANGE (created_at);

-- DEFAULT 安全網:排程沒跑到時寫入仍然成功,不會因為缺分區而整批失敗
CREATE TABLE platform.chance_draws_default PARTITION OF platform.chance_draws DEFAULT;

-- 每月分區由 platform.ensure_month_partitions() 自動建立 ——
-- 它掃 pg_partitioned_table,新表不必登記
SELECT platform.create_month_partition('platform.chance_draws'::regclass, now()::date);
SELECT platform.create_month_partition('platform.chance_draws'::regclass, (now() + interval '1 month')::date);

-- 產出速率:SUM(payout) - SUM(stake),依社群與時間
CREATE INDEX chance_draws_faucet_idx
  ON platform.chance_draws (community_id, kind, created_at DESC);
-- 每日次數上限的判定(schemas/25:20 次/人/日)
CREATE INDEX chance_draws_user_daily_idx
  ON platform.chance_draws (user_id, created_at DESC);

COMMENT ON TABLE platform.chance_draws IS
  '三處隨機(開箱 / 小遊戲 / 賽事下注)共用的留痕。三件事:'
  '(1) 隨時算得出實際產出速率,不必等到通膨才發現;'
  '(2) 有人質疑黑箱時拿得出種子與賠率;'
  '(3) 賠率寫在設定不寫在程式,改了不必重新部署,改動進 admin_audit_logs。';

-- +goose Down
DROP TABLE platform.chance_draws;
DROP TABLE platform.loot_box_items;
DROP TABLE platform.loot_boxes;
DROP TABLE platform.giveaway_entries;
DROP TABLE platform.giveaways;
ALTER TABLE platform.xp_events DROP COLUMN pet_instance_id;
DROP TABLE platform.pet_equipment;
DROP TABLE platform.pet_states;
ALTER TABLE platform.item_instances
  DROP CONSTRAINT item_instances_id_owner_uq,
  DROP CONSTRAINT item_instances_locked_reason_check,
  DROP COLUMN locked_reason;
