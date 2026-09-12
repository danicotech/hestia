-- +goose Up
-- schemas/21 落地:觀眾投票驅動浮動賠率 + 串關下注。
--
-- ── 為什麼是投票驅動而不是彩池 ────────────────────────────────
--
-- 彩池(pari-mutuel)做不了串關:彩池的賠率要等封盤後才算得出來,
-- 而串關必須在下注當下就知道每一腿的賠率才能乘起來。
--
-- 所以改成:觀眾投票 → 票數推導隱含機率 → 賠率在下注當下鎖定寫進 bet_legs。
-- 之後票數再怎麼跑,都不影響已經成立的注單。
--
-- ── 浮點數在這裡同樣禁止 ──────────────────────────────────────
--
-- 賠率會參與乘法(串關 = 各腿賠率連乘),用 float 等於把誤差直接乘進派彩金額。
-- 所以賠率存整數毫分(odds_milli,1.43 → 1430),抽水存基點(vig_bps,8% → 800)。
-- 這是「金額一律 BIGINT」的直接延伸,不是另一條規則。

-- ── 觀眾投票 ──────────────────────────────────────────────────
CREATE TABLE activity.votes (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  match_id   BIGINT NOT NULL REFERENCES activity.matches(id),
  -- 弱參照 platform.users。
  -- **只用來擋灌票 —— API 永不回傳誰投給誰,只回票數。**
  -- 這不是隱私加分項:投票會直接動賠率,公開投票人等於公開可操縱的標的。
  user_id    BIGINT NOT NULL,
  side       SMALLINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT votes_side_check CHECK (side IN (1, 2)),
  -- 一場一票。改票走 UPSERT,不是再插一列
  CONSTRAINT votes_match_user_uq UNIQUE (match_id, user_id)
);

-- 算賠率要數票:每場每邊各幾票
CREATE INDEX votes_match_side_idx ON activity.votes (match_id, side);

COMMENT ON TABLE activity.votes IS
  '觀眾投票,驅動浮動賠率。隱含機率 = (該方票數 + SMOOTHING) / (總票數 + 2×SMOOTHING);'
  '賠率 = (1 − VIG) / 隱含機率,夾在 [MIN_ODDS, MAX_ODDS]。參數在 tournaments.config。';

-- ── 注單 ──────────────────────────────────────────────────────
CREATE TABLE activity.bets (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id     TEXT   NOT NULL UNIQUE,
  tournament_id BIGINT NOT NULL REFERENCES activity.tournaments(id),
  -- 下注要動平台代幣,所以下注者**必須**有平台帳號 —— 這裡是 NOT NULL。
  -- 對比 tournament_players.user_id 允許 NULL:參賽不需帳號,動錢才需要。
  -- 這條分界線就是「雙軌身分」在資料上的樣子。
  user_id       BIGINT NOT NULL,
  stake         BIGINT NOT NULL,
  -- 下注當下算定:stake × ∏(各腿賠率),受 MAX_PARLAY 封頂。
  -- 結算時直接發這個數字,不重算 —— 選手看到的「可能贏得 X」必須等於實際入帳。
  potential_payout BIGINT NOT NULL,
  status        TEXT NOT NULL DEFAULT 'open',

  -- 稽核鏈:注單 ↔ 帳本雙向可查。
  -- 不設 FK:token_entries 是按月分區表,分區表無法由 DB 保證全域唯一
  -- (CLAUDE.md 規則 5 的既有例外,同一個理由)。
  ledger_stake_entry_id  BIGINT,
  ledger_refund_entry_id BIGINT,

  -- 有腿因棄賽 void 而重算過 potential_payout。
  -- 存這個旗標是為了對帳時看得出「這筆金額為什麼跟原始承諾不同」。
  payout_recalculated BOOLEAN NOT NULL DEFAULT false,

  settled_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT bets_stake_check CHECK (stake > 0),
  CONSTRAINT bets_potential_payout_check CHECK (potential_payout > 0),
  CONSTRAINT bets_status_check CHECK (status IN ('open', 'won', 'lost', 'void')),
  CONSTRAINT bets_settled_at_check
    CHECK ((status = 'open') = (settled_at IS NULL))
);

CREATE INDEX bets_user_created_idx ON activity.bets (user_id, created_at DESC);
-- 結算時只掃未結的注單。已結的注單會愈積愈多,部分索引讓這條永遠很小
CREATE INDEX bets_open_idx ON activity.bets (status) WHERE status = 'open';

COMMENT ON TABLE activity.bets IS
  '注單。扣款經 Ledger(reason=bet_stake),與 INSERT 本列必須同一個 tx —— '
  '這依賴 Ledger.ApplyInTx(外部傳入 tx)。activity 仍只透過 interface 動錢,邊界沒破。';

-- ── 串關的每一腿 ──────────────────────────────────────────────
CREATE TABLE activity.bet_legs (
  id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  bet_id   BIGINT NOT NULL REFERENCES activity.bets(id) ON DELETE CASCADE,
  match_id BIGINT NOT NULL REFERENCES activity.matches(id),
  side     SMALLINT NOT NULL,
  -- 賠率 ×1000 存整數。下注當下鎖定,之後票數再跑都不改這個值
  odds_milli BIGINT NOT NULL,
  result   TEXT NOT NULL DEFAULT 'pending',

  CONSTRAINT bet_legs_side_check CHECK (side IN (1, 2)),
  CONSTRAINT bet_legs_odds_check CHECK (odds_milli > 1000),
  CONSTRAINT bet_legs_result_check
    CHECK (result IN ('pending', 'won', 'lost', 'void')),
  -- 同一注單不可重複押同一場(押完 p1 再押 p2 = 穩賺,必須擋死)
  CONSTRAINT bet_legs_bet_match_uq UNIQUE (bet_id, match_id)
);

-- 結算一場比賽時要撈出所有押到它的腿
CREATE INDEX bet_legs_match_result_idx
  ON activity.bet_legs (match_id, result) WHERE result = 'pending';

COMMENT ON TABLE activity.bet_legs IS
  '串關的每一腿。同輪比賽彼此不共用選手,所以各腿天然無相關 —— 單淘汰的結構性保證。'
  '棄賽時該腿標 void 並從乘積中移除,剩餘腿仍用各自鎖定的 odds_milli 重算。';

-- +goose Down
DROP TABLE activity.bet_legs;
DROP TABLE activity.bets;
DROP TABLE activity.votes;
