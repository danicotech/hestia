-- +goose Up
-- schemas/23 落地:幣別作用域,讓點數可以是「平台通用」或「社群限定」。
--
-- 為什麼現在做:user_balances 與 token_entries 沒有 community_id,所以點數
-- 跨**所有社群**共用——在 A 社群狂簽到存的點,能直接去 B 社群把周邊買走。
-- 而 user_xp 有 community_id,各社群獨立;兩者不一致且沒有文件解釋。
--
-- 為什麼是現在而不是之後:此刻全庫只有 1 種幣、1 個社群、2 筆分錄。
-- 等到有十個社群、幾十萬筆分錄,這就不是加欄位而是資料搬遷。
--
-- 行為上這次是**零變化**:coin 定為 global,所有既有列的 community_id 為 NULL,
-- 語意與改動前完全相同。社群限定幣要等到真的發一種新幣時才會啟用。

-- ── currencies ────────────────────────────────────────────────
ALTER TABLE platform.currencies
  ADD COLUMN scope TEXT NOT NULL DEFAULT 'global',
  ADD CONSTRAINT currencies_scope_check CHECK (scope IN ('global', 'community'));

-- 看起來多餘(code 已是 PK),但它不是為了唯一性:
-- 它讓 user_balances / token_entries 能用複合外鍵把 scope 帶過去,
-- 好讓「scope 與 community_id 必須一致」寫得成宣告式的 CHECK。
ALTER TABLE platform.currencies
  ADD CONSTRAINT currencies_code_scope_uq UNIQUE (code, scope);

COMMENT ON COLUMN platform.currencies.scope IS
  'global = 平台通用幣,餘額與分錄的 community_id 為 NULL;'
  'community = 社群限定點數,必須帶 community_id。'
  '一旦有餘額就改不動(複合外鍵擋著)——把流通中的幣改作用域是資料搬遷,不該是一句 UPDATE。';

-- ── user_balances ─────────────────────────────────────────────
ALTER TABLE platform.user_balances
  ADD COLUMN community_id BIGINT REFERENCES platform.communities(id),
  -- currencies.scope 的受控副本。看似違反「一個概念一個權威位置」,
  -- 但複合外鍵保證它不可能與來源不一致——它不是第二個權威,
  -- 是把權威帶進 CHECK 看得到的範圍。替代方案是 trigger,而 trigger 裡的
  -- 邏輯沒有人在 review schema 時看得到。
  ADD COLUMN scope TEXT NOT NULL DEFAULT 'global';

-- PK 要含 community_id,但 PG 的主鍵不能含 NULL,而全域幣的 community_id 就是 NULL。
-- 解法與 user_roles 一致(schemas/03:「PG 不支援表達式主鍵,故用代理鍵」)。
ALTER TABLE platform.user_balances DROP CONSTRAINT user_balances_pkey;
ALTER TABLE platform.user_balances
  ADD COLUMN id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY;

CREATE UNIQUE INDEX user_balances_uq
  ON platform.user_balances (user_id, currency, COALESCE(community_id, 0));

-- 單欄外鍵由複合外鍵取代(後者已蘊含 currency 合法)
ALTER TABLE platform.user_balances DROP CONSTRAINT user_balances_currency_fkey;
ALTER TABLE platform.user_balances
  ADD CONSTRAINT user_balances_currency_scope_fkey
    FOREIGN KEY (currency, scope) REFERENCES platform.currencies (code, scope),
  ADD CONSTRAINT user_balances_scope_community_check
    CHECK ((scope = 'global') = (community_id IS NULL));

-- 排行榜索引跟著分社群。全域幣的 community_id 為 NULL,btree 照樣排得動。
DROP INDEX platform.user_balances_currency_balance_idx;
CREATE INDEX user_balances_leaderboard_idx
  ON platform.user_balances (currency, COALESCE(community_id, 0), balance DESC);

-- ── token_entries ─────────────────────────────────────────────
ALTER TABLE platform.token_entries
  ADD COLUMN community_id BIGINT REFERENCES platform.communities(id),
  ADD COLUMN scope TEXT NOT NULL DEFAULT 'global';

ALTER TABLE platform.token_entries DROP CONSTRAINT token_entries_currency_fkey;
ALTER TABLE platform.token_entries
  ADD CONSTRAINT token_entries_currency_scope_fkey
    FOREIGN KEY (currency, scope) REFERENCES platform.currencies (code, scope),
  ADD CONSTRAINT token_entries_scope_community_check
    CHECK ((scope = 'global') = (community_id IS NULL));

-- 分區鍵不變(仍按 created_at 月分區):community_id 不適合當分區鍵——
-- 社群數量會長期變動,而且查詢幾乎都帶時間範圍。

-- 對帳與個人查詢現在都要帶社群維度
CREATE INDEX token_entries_user_currency_community_idx
  ON platform.token_entries (user_id, currency, COALESCE(community_id, 0), created_at DESC);

COMMENT ON COLUMN platform.token_entries.community_id IS
  '對帳式因此改為依 (user_id, currency, community_id) 分組。'
  '不分組的 SUM 會永遠等於總額而失去檢查價值——'
  '.claude/skills/ledger-invariants 已同步更新。';

-- +goose Down
DROP INDEX platform.token_entries_user_currency_community_idx;
ALTER TABLE platform.token_entries
  DROP CONSTRAINT token_entries_scope_community_check,
  DROP CONSTRAINT token_entries_currency_scope_fkey;
ALTER TABLE platform.token_entries
  ADD CONSTRAINT token_entries_currency_fkey
    FOREIGN KEY (currency) REFERENCES platform.currencies(code);
ALTER TABLE platform.token_entries DROP COLUMN scope, DROP COLUMN community_id;

DROP INDEX platform.user_balances_leaderboard_idx;
CREATE INDEX user_balances_currency_balance_idx
  ON platform.user_balances (currency, balance DESC);
ALTER TABLE platform.user_balances
  DROP CONSTRAINT user_balances_scope_community_check,
  DROP CONSTRAINT user_balances_currency_scope_fkey;
ALTER TABLE platform.user_balances
  ADD CONSTRAINT user_balances_currency_fkey
    FOREIGN KEY (currency) REFERENCES platform.currencies(code);
DROP INDEX platform.user_balances_uq;
ALTER TABLE platform.user_balances DROP CONSTRAINT user_balances_pkey;
ALTER TABLE platform.user_balances DROP COLUMN id;
ALTER TABLE platform.user_balances ADD PRIMARY KEY (user_id, currency);
ALTER TABLE platform.user_balances DROP COLUMN scope, DROP COLUMN community_id;

ALTER TABLE platform.currencies
  DROP CONSTRAINT currencies_code_scope_uq,
  DROP CONSTRAINT currencies_scope_check,
  DROP COLUMN scope;
