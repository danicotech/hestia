-- +goose Up
-- schemas/20(2026-09-13 增補)+ schemas/27 落地:多回合制、開賽前設定確認、
-- 讓武項目的穩定識別與參數、封盤時的抽選結果、違規紀錄。
--
-- ── 為什麼這些都是「時間戳與欄位」,不是新的 status ────────────
--
-- 2026-09-13 grill 多出了好幾個時間點:設定確認、每回合的正式決鬥開始。
-- 它們全部落在 locked / live 之內。狀態機的分支只在「能不能買讓武、能不能下注」
-- 兩件事上,而那兩件事的答案沒有變多 —— 所以 matches.status 的五個值不動。
--
-- ── 三步式(加欄位 → 補值 → 收緊)────────────────────────────
--
-- 與 00004 同一個理由:讓這支在有資料的環境也能跑。開發庫此刻有一屆賽事
-- (報名期、無場次),正式機日後也會帶著資料升版。

-- ═══ activity.matches ═══════════════════════════════════════════

-- 每個 ADD COLUMN 自己一句:sqlc 的 catalog 對「一個 ALTER 加多個欄位」的處理與 Postgres 不同,
-- 會漏掉欄位;拆開對 Postgres 沒有差別。本檔與 00007 通篇如此。

-- bracket = 晉級樹上的一場;third_place = 季軍戰(準決賽兩位敗者加打,勝者不晉級)。
ALTER TABLE activity.matches ADD COLUMN kind TEXT NOT NULL DEFAULT 'bracket';
-- 開賽前設定確認(grill Q1/Q9):裁判看完整張清單按一次「都確認了」。
-- 清單本身是推導值(該場未作廢的 handicap_selections JOIN handicap_items.referee_note
-- + 抽選結果),不另存 —— 所以這裡只有「誰、什麼時候」。
ALTER TABLE activity.matches ADD COLUMN setup_confirmed_at TIMESTAMPTZ;
-- 弱參照 platform.users(裁判必有平台帳號)。
ALTER TABLE activity.matches ADD COLUMN setup_confirmed_by BIGINT;

-- 已經開打過的場次視為「當時確認過」:started_at 之前沒有這道關卡,
-- 不能讓下面那條 CHECK 把歷史資料判成違規。開發庫此刻是 no-op。
UPDATE activity.matches
SET setup_confirmed_at = started_at
WHERE started_at IS NOT NULL AND setup_confirmed_at IS NULL;

ALTER TABLE activity.matches
  ADD CONSTRAINT matches_kind_check CHECK (kind IN ('bracket', 'third_place')),
  -- 封盤前沒有清單可確認:清單來自封盤時定案的讓武內容。
  ADD CONSTRAINT matches_setup_confirmed_status_check
    CHECK (setup_confirmed_at IS NULL OR status IN ('locked', 'live', 'done')),
  -- **未確認不得開打**,DB 層也擋(schemas/20 待確認 ⑤,已同意)。
  -- 應用層先擋並給得出人話;這條是最後一道,讓「跳過確認直接開打」在結構上不可能。
  ADD CONSTRAINT matches_started_requires_setup_check
    CHECK (started_at IS NULL OR setup_confirmed_at IS NOT NULL);

COMMENT ON COLUMN activity.matches.kind IS
  'bracket = 晉級樹上的一場;third_place = 季軍戰。季軍戰不屬於晉級樹,round 記為決賽那一輪、slot 另給,勝者不晉級。';
COMMENT ON COLUMN activity.matches.started_at IS
  '= 第一回合的正式決鬥開始(match_rounds 第 1 列的 started_at)。這一刻同時關下注、啟動第一回合計時;前置過程在它之前。';

-- ═══ activity.match_rounds(逐回合結果)════════════════════════
--
-- 三局兩勝(config.format.best_of)逼出來的:單回合勝敗盤與比分盤都要結算逐回合結果,
-- 「撐過 90 秒即獲勝」贏的也是**該回合**(grill Q17)。
--
-- 回合列在裁判按「正式決鬥開始」時建,**不預建**:三局兩勝可能只打兩回合,
-- 預建第三列會留一個永遠空的。所以 started_at 建列即有值(NOT NULL),
-- 「還沒開始的回合」= 沒有列,不是 started_at IS NULL。
-- 這比 schemas/20 草案寫的 NULL 更嚴 —— 草案那句「NULL = 還沒開始」描述的狀態在這個建列時機下不會存在。
CREATE TABLE activity.match_rounds (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  match_id   BIGINT NOT NULL REFERENCES activity.matches(id),
  round_no   INT    NOT NULL,
  -- 正式決鬥開始 = 計時起點。前置過程(讓守關元、讓半血)在它之前,不計入。
  -- 計時器是衍生值:前端算 now() − started_at,伺服器不存倒數狀態。
  started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  -- 必須是該場 p1 或 p2 之一:DB 無法跨表 CHECK,由程式與測試守。
  winner_player_id BIGINT REFERENCES activity.tournament_players(id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT match_rounds_round_no_check CHECK (round_no >= 1),
  CONSTRAINT match_rounds_match_round_uq UNIQUE (match_id, round_no),
  -- 結束與勝者同生共死:沒有「結束了但沒有勝者」也沒有「有勝者但沒結束」的回合。
  CONSTRAINT match_rounds_finished_winner_check
    CHECK ((finished_at IS NULL) = (winner_player_id IS NULL)),
  CONSTRAINT match_rounds_order_check
    CHECK (finished_at IS NULL OR finished_at >= started_at)
);

COMMENT ON TABLE activity.match_rounds IS
  '逐回合結果。回合比數是衍生值(COUNT GROUP BY winner_player_id),整場勝者 = 先拿到 ⌈best_of/2⌉ 勝的一方;'
  'matches.winner_player_id 仍存但每次寫入時重算核對(與 match_budgets.spent 同一套紀律)。';

-- ═══ activity.handicap_items:穩定識別 + 參數 ═══════════════════

-- 穩定識別,例 weapon.designated_art。改名不改 key:2026-09-13 御風羽審定時
-- 「16 級武庫」改成「46 級」,以 (category, name) 當鍵就對不上了 ——
-- 目錄同步、程式裡的 targetNoteRequired 等規則之後都改用它。
ALTER TABLE activity.handicap_items ADD COLUMN key TEXT;
-- 項目自己的參數:{"seconds": 90}(撐過 90 秒)、{"draw": "wuxue"}(隨機武學)、
-- {"draw": "direction_keys"}(打亂方向鍵)、{"applies_to_both": true}(46 級武庫)。
-- 數字不再只活在名稱文字裡。每個鍵的意義登記在程式的驗證器裡,不接受未知鍵。
ALTER TABLE activity.handicap_items ADD COLUMN params JSONB NOT NULL DEFAULT '{}'::jsonb;

-- 補值:既有列以 (category, sort_order) 對回 catalogue.json 的 key。
-- 這張對照表是 2026-09-13 的目錄快照,只為了讓「升版前就開好的賽事」拿到 key;
-- 之後開的賽事由 InsertHandicapItems 直接帶 key 進來,不再經過這裡。
-- 對不上的列會留 NULL,下一句 SET NOT NULL 當場失敗 —— 那是要的:目錄不一致
-- 必須在升版時被看見,不是靜靜留一個沒有 key 的項目給日後的同步指令去踩。
UPDATE activity.handicap_items AS i
SET key = k.key
FROM (VALUES
  ('weapon',  10, 'weapon.designated_art'),
  ('weapon',  20, 'weapon.random_art'),
  ('weapon',  30, 'weapon.no_switch'),
  ('weapon',  40, 'weapon.armory_46'),
  ('weapon',  50, 'weapon.no_effects'),
  ('skill',   10, 'skill.ban_two'),
  ('skill',   20, 'skill.ban_one'),
  ('skill',   30, 'skill.ban_arcane_all'),
  ('skill',   40, 'skill.no_basic_attack'),
  ('skill',   50, 'skill.no_execution'),
  ('skill',   60, 'skill.remap'),
  ('skill',   70, 'skill.ban_arcane_one'),
  ('defense', 10, 'defense.no_move_keys'),
  ('defense', 20, 'defense.no_cc_break'),
  ('defense', 30, 'defense.no_dodge'),
  ('defense', 40, 'defense.no_parry'),
  ('defense', 50, 'defense.no_block'),
  ('defense', 60, 'defense.no_jump'),
  ('poison',  10, 'poison.concede_guard'),
  ('poison',  20, 'poison.scramble_keys'),
  ('poison',  30, 'poison.swap_keys'),
  ('poison',  40, 'poison.concede_half_hp'),
  ('victory', 10, 'victory.survive_90s'),
  ('victory', 20, 'victory.survive_120s'),
  ('victory', 30, 'victory.opponent_half_hp'),
  ('victory', 40, 'victory.timeout_win'),
  ('victory', 50, 'victory.guard_break_win'),
  ('rule',    10, 'rule.no_chase'),
  ('rule',    20, 'rule.stay_in_corner'),
  ('rule',    30, 'rule.first_move'),
  ('rule',    40, 'rule.designated_appearance'),
  ('rule',    50, 'rule.trash_talk'),
  ('rule',    60, 'rule.loser_request'),
  ('rule',    70, 'rule.opening_line')
) AS k(category, sort_order, key)
WHERE i.category = k.category AND i.sort_order = k.sort_order AND i.key IS NULL;

-- 參數同樣補給既有列。只有四項帶參數,其餘維持 '{}'。
UPDATE activity.handicap_items SET params = '{"draw": "wuxue"}'::jsonb           WHERE key = 'weapon.random_art';
UPDATE activity.handicap_items SET params = '{"applies_to_both": true}'::jsonb   WHERE key = 'weapon.armory_46';
UPDATE activity.handicap_items SET params = '{"draw": "direction_keys"}'::jsonb  WHERE key = 'poison.scramble_keys';
UPDATE activity.handicap_items SET params = '{"seconds": 90}'::jsonb             WHERE key = 'victory.survive_90s';
UPDATE activity.handicap_items SET params = '{"seconds": 120}'::jsonb            WHERE key = 'victory.survive_120s';

ALTER TABLE activity.handicap_items
  ALTER COLUMN key SET NOT NULL,
  ADD CONSTRAINT handicap_items_tournament_key_uq UNIQUE (tournament_id, key),
  -- key 的形狀:<category>.<snake_case>。前綴必須等於 category 欄位 ——
  -- 兩個地方各寫一次分類,漂移時這條會當場擋下來。
  ADD CONSTRAINT handicap_items_key_format_check
    CHECK (key ~ '^[a-z]+\.[a-z0-9_]+$' AND split_part(key, '.', 1) = category);

COMMENT ON COLUMN activity.handicap_items.key IS
  '穩定識別(<category>.<snake_case>)。改名不改 key。目錄同步與程式規則一律以它定址,不再從 (category, name) 推導。';
COMMENT ON COLUMN activity.handicap_items.params IS
  '項目自己的參數(seconds / draw / applies_to_both)。未知鍵由程式的驗證器拒絕,JSONB 不是垃圾桶。';

-- ═══ activity.handicap_selections:抽選結果 ═══════════════════

-- 系統抽選的結果(抽到的武學名稱、方向鍵新對應)。只有 handicap_items.params.draw
-- 非空的項目會有值。抽選在**封盤那一刻**、與 LockMatchHandicaps 同一個 tx(grill Q13):
-- 封盤不可逆 ⇒ 抽選結果不可逆,封盤公示因此是完整的。
ALTER TABLE activity.handicap_selections ADD COLUMN draw_result TEXT;
ALTER TABLE activity.handicap_selections ADD COLUMN drawn_at TIMESTAMPTZ;

ALTER TABLE activity.handicap_selections
  ADD CONSTRAINT handicap_selections_draw_check
    CHECK ((draw_result IS NULL) = (drawn_at IS NULL));

-- ═══ activity.match_violations(違規紀錄)═════════════════════
--
-- 只記事實,不做規則引擎(grill Q21:「違規由裁判判斷,不用搞這麼複雜」)。
-- 判該回合 = 裁判把該回合勝者填成對方(match_rounds);判整場 = 走 ReportResult。
-- 這張表是那些判決的**說明**,不觸發任何後果。
--
-- 為什麼不用 admin_audit_logs:它的 actor_user_id 是 NOT NULL 外鍵到 platform.users,
-- 而違規者是**選手**,選手不一定有平台帳號。裁判「記了一筆違規」這個動作本身
-- 仍然寫稽核(actor = 裁判),兩張表各記各的事。
CREATE TABLE activity.match_violations (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id  TEXT   NOT NULL UNIQUE,
  match_id   BIGINT NOT NULL REFERENCES activity.matches(id),
  -- 第幾回合;NULL = 開賽前(例:設定確認時發現沒改到)。
  -- 不設 FK 到 match_rounds:違規發生時該回合的列未必已建。
  round_no   INT,
  -- 違規者,**可以是任一方**:「雙方使用 46 級武庫」買方自己也可能違規(grill Q6)。
  player_id  BIGINT NOT NULL REFERENCES activity.tournament_players(id),
  -- 違反哪一項;NULL = 不對應特定項目(違反通則)。
  item_id    BIGINT REFERENCES activity.handicap_items(id),
  -- 裁判怎麼判。**只是紀錄,不是觸發器**。固定值是為了讓賽果公告判斷得出
  -- 「這一筆要不要寫進公告」(round_loss / match_loss 才寫,grill Q14)。
  ruling     TEXT   NOT NULL,
  -- 必填:沒有理由的紀錄事後分不出「依規則」與「裁判不喜歡」。
  note       TEXT   NOT NULL,
  -- 弱參照 platform.users(裁判)。
  recorded_by BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT match_violations_ruling_check
    CHECK (ruling IN ('warning', 'round_loss', 'match_loss', 'none')),
  CONSTRAINT match_violations_round_no_check CHECK (round_no IS NULL OR round_no >= 1),
  CONSTRAINT match_violations_note_check CHECK (length(btrim(note)) > 0)
);

-- 沒有 UNIQUE:同一人同一項在同一回合可以違規兩次,那是兩列。
CREATE INDEX match_violations_match_idx  ON activity.match_violations (match_id, created_at);
CREATE INDEX match_violations_player_idx ON activity.match_violations (player_id);

COMMENT ON TABLE activity.match_violations IS
  '違規紀錄:哪一場、第幾回合、誰、違反哪一項、裁判怎麼判、為什麼。不自動觸發任何後果。'
  '「第幾次」不存 —— 那是 COUNT 算得出來的衍生值,而且規則不進系統,由裁判當場判。';

-- +goose Down
DROP TABLE activity.match_violations;

ALTER TABLE activity.handicap_selections
  DROP CONSTRAINT handicap_selections_draw_check,
  DROP COLUMN drawn_at,
  DROP COLUMN draw_result;

ALTER TABLE activity.handicap_items
  DROP CONSTRAINT handicap_items_key_format_check,
  DROP CONSTRAINT handicap_items_tournament_key_uq,
  DROP COLUMN params,
  DROP COLUMN key;

DROP TABLE activity.match_rounds;

ALTER TABLE activity.matches
  DROP CONSTRAINT matches_started_requires_setup_check,
  DROP CONSTRAINT matches_setup_confirmed_status_check,
  DROP CONSTRAINT matches_kind_check,
  DROP COLUMN setup_confirmed_by,
  DROP COLUMN setup_confirmed_at,
  DROP COLUMN kind;
