-- +goose Up
-- schemas/20 落地:讓武 BP —— 項目定義、每場預算、選購紀錄。
--
-- ── BP 為什麼不進平台帳本 ─────────────────────────────────────
--
-- BP(Balance Point,讓武調平點數)看起來像貨幣,但它不是:
--
--   每輪依段位差重新發放 → 帳本是 append-only,「重新發」只能靠一堆正負分錄湊
--   沒花完即作廢         → 帳本沒有「作廢」這個動作
--   不可交易、不可累積   → 貨幣的核心性質它一個都沒有
--   不需冪等             → 重複扣一次 BP 的後果是使用者看到數字不對,不是對不上帳
--
-- 硬塞進 token_entries 會讓帳本鐵則(append-only、退款走新正數分錄)為了一個
-- 不是錢的東西彎腰。所以 BP 完全活在 activity schema,不碰 Ledger interface。
--
-- 真正要走 Ledger 的只有三件事:報名獎勵、賽事獎金、下注的投注與派彩。
--
-- 這個決定沿用 grill Q5a 對「戰備值」已經下過的同一個判斷,不是新發明。

-- ── 讓武項目定義 ──────────────────────────────────────────────
CREATE TABLE activity.handicap_items (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- 逐屆一套:價格要能逐屆調,而且上屆的選購紀錄必須永遠指向上屆的價格
  tournament_id BIGINT NOT NULL REFERENCES activity.tournaments(id),
  category      TEXT   NOT NULL,
  name          TEXT   NOT NULL,
  -- 給選手看的
  description   TEXT,
  -- 給裁判看的:怎麼確認對手真的遵守了。
  -- 「禁用迴避」很好認,「對手未攻擊時必須待在角落」就需要講清楚判準。
  referee_note  TEXT,
  cost          BIGINT NOT NULL,
  -- 只供 UI 標示「(可重複)」,**不作強制**。
  -- 09-12 定案:所有項目都可重複購買,點數夠就行。這欄存在只是為了呈現原始清單的措辭。
  repeatable    BOOLEAN NOT NULL DEFAULT false,
  sort_order    INT     NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT handicap_items_category_check CHECK (category IN (
    'weapon',   -- 武器限制
    'skill',    -- 技能限制
    'defense',  -- 防禦與位移限制
    'poison',   -- 下毒
    'victory',  -- 勝利條件
    'rule'      -- 規則限制
  )),
  CONSTRAINT handicap_items_cost_check CHECK (cost > 0),
  CONSTRAINT handicap_items_tournament_category_name_uq
    UNIQUE (tournament_id, category, name)
);

CREATE INDEX handicap_items_tournament_idx
  ON activity.handicap_items (tournament_id, category, sort_order);

COMMENT ON TABLE activity.handicap_items IS
  '讓武項目:低段位者花 BP 施加在高段位者身上的正式比賽限制。'
  '與 platform 的收藏品是不同概念 —— 不可交易、逐屆定義、只在賽內有意義。';

COMMENT ON COLUMN activity.handicap_items.category IS
  '系統刻意不做互斥檢查(09-12 定案)。victory 類同時選多項會互相矛盾,'
  '但那由裁判臨場判 —— 前端只給非阻擋式提示,不擋購買。';

-- ── BP 預算(每場、每人)──────────────────────────────────────
CREATE TABLE activity.match_budgets (
  match_id   BIGINT NOT NULL REFERENCES activity.matches(id),
  -- 只有低段位者有列。同段或高段者不建列 = 0 BP,前端顯示「本場無讓武」。
  -- 不建列而不是建一筆 budget=0,是為了讓「有沒有讓武」這件事在資料上就分得出來。
  player_id  BIGINT NOT NULL REFERENCES activity.tournament_players(id),

  -- budget = (對手 rank_level − 自己 rank_level) × config.bp_per_rank_gap
  -- 差一段 8、差二段 16、差三段 24。高段位者永不獲得反向補償。
  budget     BIGINT NOT NULL,
  -- 衍生資料:必須恆等於 SUM(cost) FROM handicap_selections WHERE NOT voided。
  -- 冗餘存放是因為選購頁每次點擊都要檢查餘額,每次 SUM 不划算。
  -- 測試必須在每次寫入後驗證兩者一致(schemas/20 待確認 ②)。
  spent      BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (match_id, player_id),
  CONSTRAINT match_budgets_budget_check CHECK (budget > 0),
  -- 這條是 BP 不會超支的最後一道保險。應用層也擋,但 DB 說了算
  CONSTRAINT match_budgets_spent_check CHECK (spent >= 0 AND spent <= budget)
);

COMMENT ON TABLE activity.match_budgets IS
  'BP 預算。每輪配對後依當下段位差重算發放,該場有效,賽後作廢 —— 不跨輪累積、不找零。'
  '不進平台帳本、不走 Ledger、不需冪等:它不是貨幣。';

-- ── 讓武選擇 ──────────────────────────────────────────────────
CREATE TABLE activity.handicap_selections (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id  TEXT   NOT NULL UNIQUE,
  match_id   BIGINT NOT NULL REFERENCES activity.matches(id),
  -- 施加者(低段位者)。限制生效在對手身上,但紀錄掛在花錢的人名下
  player_id  BIGINT NOT NULL REFERENCES activity.tournament_players(id),
  item_id    BIGINT NOT NULL REFERENCES activity.handicap_items(id),
  -- 購買當下的價格快照。項目改價不影響已成立的選擇
  cost       BIGINT NOT NULL,
  -- 需要指定內容的項目填這裡:
  -- 「指定對手武學:XX」「互換的兩個按鍵:X 與 Y」「開局要講的八個字:……」
  target_note TEXT,
  -- 封盤**前**可退(退回 BP,改 match_budgets.spent)。封盤後不可再改
  voided     BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT handicap_selections_cost_check CHECK (cost > 0),
  CONSTRAINT handicap_selections_budget_fkey
    FOREIGN KEY (match_id, player_id)
    REFERENCES activity.match_budgets (match_id, player_id)
);

-- 刻意沒有 UNIQUE (match_id, player_id, item_id):
-- 重複購買同一項目是設計,不是 bug(09-12:「點數夠 都可以購買同個商品」)。
-- 前端以數量 ×N 聚合呈現,不是勾選框。
CREATE INDEX handicap_selections_match_player_idx
  ON activity.handicap_selections (match_id, player_id) WHERE NOT voided;

COMMENT ON TABLE activity.handicap_selections IS
  '每次購買一列。退費只是 BP 內部的事(改 spent + 標 voided),不經 Ledger。'
  '外鍵指向 match_budgets 的複合鍵 —— 沒有預算的人連一列都插不進來。';

-- +goose Down
DROP TABLE activity.handicap_selections;
DROP TABLE activity.match_budgets;
DROP TABLE activity.handicap_items;
