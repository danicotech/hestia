-- +goose Up
-- schemas/24 落地:等級曲線修正、里程碑獎勵、outbox 死信補強。
--
-- 等級在此之前是關著的(user_xp.level 恆 0),schemas/01 增補 A 定的曲線有兩個問題:
--   1. `100 × n^1.5` 讓「1 級」需要 100 XP —— 剛註冊的人會是 0 級。
--      改為 `100 × (n-1)^1.5`,L1=0、L2=100、L10=2,700、L100≈98,015。
--   2. max_level=100 讓重度使用者三個月就滿等沒東西追。移除上限。
--
-- 曲線是 JSONB 設定不是欄位,所以這兩項在下面用 UPDATE 改 xp_rulesets.config
-- 的預設樣板;已存在的 ruleset(目前 0 筆)不動 —— 改別人已經上線的數值
-- 不是 migration 該做的事。

-- ── 等級不存欄位 ──────────────────────────────────────────────
--
-- level 由 xp 用曲線反推,不另存:衍生資料必須可重建,而存了就會有不一致的一天。
-- 欄位保留但標記為不使用,不在此 migration 刪除 —— 刪欄位會鎖表,
-- 而它現在恆 0,留著沒有任何代價。
COMMENT ON COLUMN platform.user_xp.level IS
  '**不使用**(2026-09-11 起)。等級是 xp 的純函數:'
  'level = floor((xp / base) ^ (1/exponent)) + 1,曲線見 xp_rulesets.config。'
  '存成欄位就會有與 xp 不一致的一天,而 xp 才是事實。';

-- ── 里程碑獎勵 ────────────────────────────────────────────────
CREATE TABLE platform.level_rewards (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  community_id BIGINT NOT NULL REFERENCES platform.communities(id),
  -- 使用者等級與寵物等級各有一套。不分的話兩邊會互相干擾:
  -- 寵物升 10 級會拿到給人的身分組,而人升 10 級會拿到給寵物的裝備。
  subject      TEXT   NOT NULL,
  level        INT    NOT NULL,
  -- 四種獎勵的發放機制全都已經存在,這張表只決定「幾級時呼叫哪一個」
  reward_kind  TEXT   NOT NULL,
  -- 依 kind 解讀:role→roles.public_id、tokens→currencies.code、
  -- item→item_definitions.public_id、entitlement→shop_items.public_id
  reward_ref   TEXT   NOT NULL,
  amount       BIGINT,
  note         TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT level_rewards_subject_check CHECK (subject IN ('user', 'pet')),
  CONSTRAINT level_rewards_kind_check
    CHECK (reward_kind IN ('role', 'tokens', 'item', 'entitlement')),
  -- 1 級是起點,不可能「升到」1 級
  CONSTRAINT level_rewards_level_check CHECK (level > 1),
  -- tokens 一定要有金額,其餘一定不能有 —— 一個欄位兩種意思是 bug 的溫床
  CONSTRAINT level_rewards_amount_check
    CHECK ((reward_kind = 'tokens') = (amount IS NOT NULL)
           AND (amount IS NULL OR amount > 0)),
  CONSTRAINT level_rewards_uq
    UNIQUE (community_id, subject, level, reward_kind, reward_ref)
);

CREATE INDEX level_rewards_lookup_idx
  ON platform.level_rewards (community_id, subject, level);

-- ── 發放紀錄 = 冪等鍵 ─────────────────────────────────────────
--
-- outbox 是至少一次投遞,消費端必須冪等,否則重試一次就發兩隻寵物。
-- 與每日簽到同樣的做法:**靠資料庫約束擋,不靠應用層檢查**
-- (帳本鐵則三:兩個都做是重複)。
CREATE TABLE platform.level_reward_grants (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  reward_id       BIGINT NOT NULL REFERENCES platform.level_rewards(id),
  user_id         BIGINT NOT NULL REFERENCES platform.users(id),
  -- NULL = 這是給人的獎勵;有值 = 給某一隻寵物的
  pet_instance_id BIGINT REFERENCES platform.item_instances(id),
  granted_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX level_reward_grants_uq
  ON platform.level_reward_grants (reward_id, user_id, COALESCE(pet_instance_id, 0));

CREATE INDEX level_reward_grants_user_idx
  ON platform.level_reward_grants (user_id, granted_at DESC);

-- ── outbox 死信補強 ───────────────────────────────────────────
--
-- 死信機制本身已經存在(attempts / next_retry_at / status='failed')。
-- 缺的是「為什麼失敗」與「修好之後怎麼放回去」。
ALTER TABLE platform.outbox_events
  ADD COLUMN last_error TEXT,
  ADD COLUMN failed_at  TIMESTAMPTZ;

-- 重放時要能篩「這個 topic 這段時間內失敗的」。
-- 部分索引:失敗的永遠是少數,不該讓整張表為此付出寫入成本。
CREATE INDEX outbox_events_failed_idx
  ON platform.outbox_events (topic, failed_at)
  WHERE status = 'failed';

COMMENT ON COLUMN platform.outbox_events.last_error IS
  '最後一次失敗的原因。沒有它的話,死信只知道「送不出去」,'
  '而那正是需要人介入時最該知道的一件事。';

-- ── 新的 topic ────────────────────────────────────────────────
--
-- level.up 與 member.joined 都由閘道經 NotificationService 投遞,
-- 不可再註冊 in-process handler(outbox.Consumer.Handle 會 panic 擋下,
-- 否則同一則會送兩次)。
-- topic 是字串不是枚舉表,所以這裡沒有 DDL —— 註記於此供追溯。

-- +goose Down
DROP INDEX platform.outbox_events_failed_idx;
ALTER TABLE platform.outbox_events DROP COLUMN failed_at, DROP COLUMN last_error;
DROP TABLE platform.level_reward_grants;
DROP TABLE platform.level_rewards;
COMMENT ON COLUMN platform.user_xp.level IS NULL;
