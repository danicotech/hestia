-- +goose Up
-- schemas/21(2026-09-13 增補)落地:盤口變成資料列,投票與注單腿改掛在盤口上。
--
-- ── 盤口為什麼是表,不是程式裡的 enum ────────────────────────
--
-- 使用者要的是「串關用時間投注、比分投注、這場投注、單場勝敗等等方式」,
-- 而且「規則要能做成 db 格式讓後端未來自由改動」。哪些盤口存在、每條線在哪,
-- 由 tournaments.config.betting.markets 決定,場次進入 ready 時依清單建列。
-- **結算邏輯每種 kind 一個,在程式裡**(schemas/28:形狀在程式、數字在資料)。
--
-- ── 同一場只能一腿(grill Q18)──────────────────────────────
--
-- 四種盤口讓相關性從「需要留意」變成「同一件事押兩次」:押「A 贏整場」+「比分 2:0」,
-- 比分已經包含誰贏。沒有莊家在對面收錢,相乘出來的超額派彩直接從代幣供給出。
-- 所以 bet_legs 的 UNIQUE (bet_id, match_id) **不動** —— 它本來就是這條規則。
-- 既有的安全論證(同輪比賽不共用選手 → 各腿無相關)因此完整保留。
--
-- ── 三步式 ──────────────────────────────────────────────────
--
-- votes / bet_legs 現在直接掛 match_id + side。升版:建盤口 → 為既有場次補
-- match_winner 盤口 → 加欄位回填 → 收緊。開發庫此刻 votes / bets 皆為 0 列,
-- 但這支會跟著 binary 走到正式機,所以對有資料也必須正確。

-- ═══ activity.markets ══════════════════════════════════════════

CREATE TABLE activity.markets (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id  TEXT   NOT NULL UNIQUE,
  match_id   BIGINT NOT NULL REFERENCES activity.matches(id),
  -- match_winner 整場勝敗 / round_winner 單回合勝敗 / duration 該回合時長 / score 回合比數。
  kind       TEXT   NOT NULL,
  -- round_winner 與 duration 是每回合一個盤口;match_winner 與 score 為 NULL。
  round_no   INT,
  -- 該盤口的參數。目前只有 duration 用:{"line_seconds": 90}。
  params     JSONB  NOT NULL DEFAULT '{}'::jsonb,
  -- open 收注中 / closed 已關盤未結算 / settled 已結算 / void 作廢(沒打到的回合、棄賽)。
  status     TEXT   NOT NULL DEFAULT 'open',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  settled_at TIMESTAMPTZ,

  CONSTRAINT markets_kind_check
    CHECK (kind IN ('match_winner', 'round_winner', 'duration', 'score')),
  CONSTRAINT markets_round_no_check
    CHECK ((kind IN ('round_winner', 'duration')) = (round_no IS NOT NULL)),
  CONSTRAINT markets_status_check
    CHECK (status IN ('open', 'closed', 'settled', 'void')),
  CONSTRAINT markets_settled_at_check
    CHECK ((status IN ('settled', 'void')) = (settled_at IS NOT NULL)),
  -- 給 bet_legs 的複合外鍵用:讓「腿的 match_id 必須等於盤口的 match_id」由 DB 保證,
  -- 而不是程式裡的一句比對。
  CONSTRAINT markets_id_match_uq UNIQUE (id, match_id)
);

-- 同 kind 可以有多條線(schemas/28 待確認 ②,已同意),以 line_seconds 區分;
-- round_no 為 NULL 的盤口以 0 參與唯一性。
CREATE UNIQUE INDEX markets_match_kind_line_uq
  ON activity.markets (match_id, kind, COALESCE(round_no, 0), COALESCE(params ->> 'line_seconds', ''));

-- 依場次列盤口(GetOdds、關盤、結算都從場次進來)。
CREATE INDEX markets_match_idx ON activity.markets (match_id, status);

COMMENT ON TABLE activity.markets IS
  '盤口。哪些存在由 tournaments.config.betting.markets 決定,場次進入 ready 時建列;'
  '結果(outcome)由 kind + config 推導,不存:match_winner/round_winner 是 p1/p2,duration 是 over/under,score 是 p1_2_0 這類。';

-- ── 為既有場次補 match_winner 盤口 ─────────────────────────────
--
-- 條件:已經開過盤(status ≥ ready)、或已有投票 / 注單腿指到它(即使狀態異常)。
-- public_id 用 LEGACY 前綴(與 00004 同一個做法):SQL 生不出 ULID,前綴標明來源。
-- 狀態對齊場次:done → settled(settled_at = finished_at)、live → closed、其餘 open。
INSERT INTO activity.markets (public_id, match_id, kind, status, settled_at)
SELECT 'LEGACYMK' || m.id::text, m.id, 'match_winner',
       CASE m.status WHEN 'done' THEN 'settled' WHEN 'live' THEN 'closed' ELSE 'open' END,
       CASE m.status WHEN 'done' THEN COALESCE(m.finished_at, now()) ELSE NULL END
FROM activity.matches m
WHERE m.status IN ('ready', 'locked', 'live', 'done')
   OR EXISTS (SELECT 1 FROM activity.votes v WHERE v.match_id = m.id)
   OR EXISTS (SELECT 1 FROM activity.bet_legs l WHERE l.match_id = m.id);

-- ═══ activity.votes:一場每盤口一票 ═════════════════════════════

-- 每個 ADD COLUMN 自己一句(00006 檔頭的理由:sqlc 的 catalog 會漏掉多欄位 ALTER 裡的欄位)。
ALTER TABLE activity.votes ADD COLUMN market_id BIGINT REFERENCES activity.markets(id);
ALTER TABLE activity.votes ADD COLUMN outcome TEXT;

UPDATE activity.votes v
SET market_id = mk.id,
    outcome   = CASE v.side WHEN 1 THEN 'p1' ELSE 'p2' END
FROM activity.markets mk
WHERE mk.match_id = v.match_id AND mk.kind = 'match_winner';

ALTER TABLE activity.votes
  ALTER COLUMN market_id SET NOT NULL,
  ALTER COLUMN outcome   SET NOT NULL,
  DROP CONSTRAINT votes_side_check,
  DROP CONSTRAINT votes_match_user_uq;
-- DROP COLUMN 各自一句(sqlc 的 catalog 對混合式 ALTER 的處理與 Postgres 不同,
-- 讓它漏掉 DROP 的話,生成的 SELECT * 結構會多一個幽靈欄位,pgx 掃描時才炸)。
ALTER TABLE activity.votes DROP COLUMN side;
-- match_id 由 market 推得,留兩份就是兩個權威。
ALTER TABLE activity.votes DROP COLUMN match_id;
-- 一場每盤口一票,改票 UPSERT。
ALTER TABLE activity.votes
  ADD CONSTRAINT votes_market_user_uq UNIQUE (market_id, user_id);

-- 算賠率要數票:每盤口每結果各幾票。舊索引跟著 side 欄位一起消失。
CREATE INDEX votes_market_outcome_idx ON activity.votes (market_id, outcome);

COMMENT ON TABLE activity.votes IS
  '觀眾投票,驅動浮動賠率。每個盤口各自投票(grill Q19):'
  'p_i = (票_i + S) / (總票 + n·S),賠率 = (1 − VIG) / p_i,夾在 [MIN, MAX]。n = 該盤口的結果數,兩路是 n=2 的特例。';

-- ═══ activity.bet_legs:押哪個盤口的哪個結果 ═══════════════════

ALTER TABLE activity.bet_legs ADD COLUMN market_id BIGINT;
ALTER TABLE activity.bet_legs ADD COLUMN outcome TEXT;

UPDATE activity.bet_legs l
SET market_id = mk.id,
    outcome   = CASE l.side WHEN 1 THEN 'p1' ELSE 'p2' END
FROM activity.markets mk
WHERE mk.match_id = l.match_id AND mk.kind = 'match_winner';

ALTER TABLE activity.bet_legs
  ALTER COLUMN market_id SET NOT NULL,
  ALTER COLUMN outcome   SET NOT NULL,
  DROP CONSTRAINT bet_legs_side_check;
-- DROP COLUMN 自己一句(同 votes 的理由)。
ALTER TABLE activity.bet_legs DROP COLUMN side;
-- match_id **保留**:它可由 market_id 推出,刻意冗餘是為了 bet_legs_bet_match_uq ——
-- DB 沒辦法對「透過 market 推出的 match」做唯一約束。這條複合外鍵讓冗餘的那一份
-- 不可能與盤口的 match_id 不一致。
ALTER TABLE activity.bet_legs
  ADD CONSTRAINT bet_legs_market_match_fkey
    FOREIGN KEY (market_id, match_id) REFERENCES activity.markets (id, match_id);

-- 結算一個盤口時要撈出押到它、還沒判定的腿(部分索引,同 bet_legs_match_result_idx 的理由)。
CREATE INDEX bet_legs_market_result_idx
  ON activity.bet_legs (market_id, result) WHERE result = 'pending';

COMMENT ON TABLE activity.bet_legs IS
  '串關的每一腿。UNIQUE (bet_id, match_id) = 同一場只能一腿(grill Q18):'
  '同場不同盤口互串會把「同一件事押兩次」的賠率相乘,而派彩從代幣供給出。跨場串不同盤口完全可以。';

-- +goose Down
-- 回到「一場一票、腿只有 side」。只有 match_winner 盤口上的票與腿轉得回去;
-- 其他盤口的資料在 v1 形狀裡沒有位置,會被丟掉 —— 這是 Down 的固有代價,不是 bug。
ALTER TABLE activity.bet_legs
  ADD COLUMN side SMALLINT;
UPDATE activity.bet_legs SET side = CASE outcome WHEN 'p1' THEN 1 WHEN 'p2' THEN 2 END;
DELETE FROM activity.bet_legs WHERE side IS NULL;
ALTER TABLE activity.bet_legs
  ALTER COLUMN side SET NOT NULL,
  ADD CONSTRAINT bet_legs_side_check CHECK (side IN (1, 2)),
  DROP CONSTRAINT bet_legs_market_match_fkey,
  DROP COLUMN outcome,
  DROP COLUMN market_id;

ALTER TABLE activity.votes
  ADD COLUMN match_id BIGINT REFERENCES activity.matches(id),
  ADD COLUMN side SMALLINT;
UPDATE activity.votes v
SET match_id = mk.match_id,
    side = CASE v.outcome WHEN 'p1' THEN 1 WHEN 'p2' THEN 2 END
FROM activity.markets mk WHERE mk.id = v.market_id AND mk.kind = 'match_winner';
DELETE FROM activity.votes WHERE match_id IS NULL OR side IS NULL;
ALTER TABLE activity.votes
  ALTER COLUMN match_id SET NOT NULL,
  ALTER COLUMN side SET NOT NULL,
  DROP CONSTRAINT votes_market_user_uq,
  DROP COLUMN outcome,
  DROP COLUMN market_id,
  ADD CONSTRAINT votes_side_check CHECK (side IN (1, 2)),
  ADD CONSTRAINT votes_match_user_uq UNIQUE (match_id, user_id);
CREATE INDEX votes_match_side_idx ON activity.votes (match_id, side);

DROP TABLE activity.markets;
