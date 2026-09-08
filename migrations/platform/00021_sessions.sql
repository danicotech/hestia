-- +goose Up
-- schemas/02 增補 F(2026-09-08 定案):使用者登入 session。
-- 與 identities.*_token_enc(Discord 發給我們的憑證)是不同概念,不共用欄位。
--
-- 約束的設計原則:重用偵測是安全關鍵路徑,它的正確性不靠應用層自律——
-- 能用 DB 約束保證的(唯一鏈、同人鏈、撤銷單向)都由 DB 保證。
CREATE TABLE platform.sessions (
  id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  public_id          TEXT        NOT NULL UNIQUE,          -- ULID:裝置清單、「登出這台」
  user_id            BIGINT      NOT NULL REFERENCES platform.users(id),
  refresh_token_hash BYTEA       NOT NULL UNIQUE,          -- SHA-256;絕不存明文
  issued_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at         TIMESTAMPTZ NOT NULL,                 -- 30 天,每次 refresh 滑動
  last_used_at       TIMESTAMPTZ,
  revoked_at         TIMESTAMPTZ,
  revoked_reason     TEXT,
  user_agent         TEXT,                                 -- 裝置清單顯示(截斷)
  ip                 INET,                                 -- 稽核
  -- 輪替鏈:refresh 發新 token 時指向被取代的那列。已輪替的 token 再被使用 =
  -- token 被竊(合法客戶端不會用舊的)→ 沿鏈撤銷整台裝置(reuse_detected)。
  -- 刻意不留 grace window:refresh 回應在網路上遺失後的重送會被判定為重用 → 強制重新登入。
  -- 代價是行動網路換手時偶發重登;換來的是不必把 refresh token 以可還原形式保存
  -- (只存 hash 是本表最重要的安全性質)。這是被 schema 鎖死的產品行為,不是實作選項。
  rotated_from       BIGINT,

  -- 撤銷是單向且成對的:兩欄要嘛都空要嘛都有值,理由限定值域
  -- (無效值會讓「為什麼被登出」這個問題答不出來)。
  CONSTRAINT sessions_revocation_paired
    CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
  CONSTRAINT sessions_revoked_reason_domain
    CHECK (revoked_reason IS NULL OR revoked_reason IN
      ('logout', 'rotated', 'admin_ban', 'reuse_detected', 'account_deleted')),
  -- 子列必然晚於父列產生,故合法輪替恆滿足 rotated_from < id;而 id 嚴格遞減的鏈
  -- 在數學上不可能成環——這一條同時排除自我指向與任意長度的循環(沿鏈走訪必終止)。
  -- 耦合注意:依賴 IDENTITY 全域嚴格遞增(seqcache=1),若日後調大 cache 需重新檢視。
  CONSTRAINT sessions_no_rotation_cycle CHECK (rotated_from IS NULL OR rotated_from < id),

  -- 複合 FK:輪替鏈不得跨使用者,否則「沿鏈全撤」會撤到別人的裝置。
  -- ON DELETE SET NULL(不是 CASCADE):清理 job 刪的正是鏈上最舊的列,
  -- CASCADE 會連帶刪掉目前仍有效的子 session = 靜默把人登出;NO ACTION 則讓
  -- 清理批次撞 FK 違規整批失敗(QA 實測)。斷開指標是唯一安全的選擇。
  CONSTRAINT sessions_rotated_from_fkey
    FOREIGN KEY (rotated_from, user_id) REFERENCES platform.sessions(id, user_id)
    ON DELETE SET NULL (rotated_from),
  -- 複合 FK 的被參照側需要這個唯一性(id 已是 PK,本約束只為讓上面的 FK 成立)
  CONSTRAINT sessions_id_user_uq UNIQUE (id, user_id)
);

-- 有效裝置清單 / 封鎖時全撤
CREATE INDEX sessions_active_idx ON platform.sessions (user_id) WHERE revoked_at IS NULL;

-- 保留期清理:條件是 COALESCE(revoked_at, expires_at) < 截止線,
-- 表達式索引讓它走 index 而不是全表掃描(QA:分開的 expires_at 索引救不了 revoked_at 路徑)。
CREATE INDEX sessions_retention_idx ON platform.sessions (COALESCE(revoked_at, expires_at));

-- 一個 session 只能被輪替一次:UNIQUE 讓「同一個 refresh token 併發換發兩條有效鏈」
-- 在 DB 層直接不可能(第二個 tx 撞鍵 → 正是 token 被重用的訊號)。
-- 兼作 FK 父列刪除檢查的索引。
CREATE UNIQUE INDEX sessions_rotated_from_uq
  ON platform.sessions (rotated_from) WHERE rotated_from IS NOT NULL;

-- 撤銷不可逆:paired CHECK 只擋「清一半」,兩欄一起清仍能讓已撤銷 session 完整復活
-- (現實路徑不是惡意 SQL,是整列回寫時 struct 沒帶到撤銷欄位)。跨版本(OLD/NEW)的
-- 條件 CHECK 表達不了,只能用觸發器。撤銷紀錄一旦寫下即為歷史,其他欄位仍可更新。
--
-- ⚠️ 呼叫端契約(QA 實測,踩到會很痛):BEFORE ROW 觸發器的 RAISE 會**中止整個 statement
-- 並使交易 abort**。批次撤銷若碰到任何一列已撤銷的,一列都撤不掉——而輪替鏈的中間列
-- 本來就都是 'rotated',所以「沿鏈全撤」幾乎必然踩到,失敗方向正是不安全的那一側
-- (偷走 token 的人繼續持有存取權)。**所有批次撤銷一律加 `AND revoked_at IS NULL`**,
-- 這同時也是走 sessions_active_idx 的寫法。兩條安全關鍵路徑的正確寫法:
--
--   -- 封鎖/刪帳號:撤銷該使用者全部有效 session
--   UPDATE platform.sessions SET revoked_at = now(), revoked_reason = 'admin_ban'
--    WHERE user_id = $1 AND revoked_at IS NULL;
--
--   -- 重用偵測:沿輪替鏈全撤(只有鏈尾是有效的,中間列保持 'rotated')
--   WITH RECURSIVE chain AS (
--     SELECT id FROM platform.sessions WHERE id = $1
--     UNION ALL
--     SELECT s.id FROM platform.sessions s JOIN chain c ON s.rotated_from = c.id)
--   UPDATE platform.sessions SET revoked_at = now(), revoked_reason = 'reuse_detected'
--    WHERE id IN (SELECT id FROM chain) AND revoked_at IS NULL;
--
-- 語意後果(刻意接受):已標 'rotated' 的鏈中列不會被升級成 'reuse_detected'——
-- 它們早已失效,安全上無損;「這條鏈曾遭竊」記錄在鏈尾那列與 event_logs。
-- +goose StatementBegin
CREATE FUNCTION platform.sessions_revocation_is_final() RETURNS trigger AS $fn$
BEGIN
  IF OLD.revoked_at IS NOT NULL
     AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
          OR NEW.revoked_reason IS DISTINCT FROM OLD.revoked_reason) THEN
    -- 帶 CONSTRAINT/TABLE/SCHEMA:讓應用層能用與其他 CHECK 完全一致的方式
    -- (比對 PgError.ConstraintName)分辨錯誤來源,而不是比對訊息字串
    RAISE EXCEPTION '撤銷不可逆:session % 已於 % 因 % 撤銷',
      OLD.id, OLD.revoked_at, OLD.revoked_reason
      USING ERRCODE = '23514',
            CONSTRAINT = 'sessions_revocation_is_final',
            TABLE = 'sessions',
            SCHEMA = 'platform';
  END IF;
  RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER sessions_revocation_is_final
  BEFORE UPDATE ON platform.sessions
  FOR EACH ROW EXECUTE FUNCTION platform.sessions_revocation_is_final();

-- 膨脹歸宿(schemas/14:每張表都要有);保留一段時間供「這台裝置何時登入過」稽核
INSERT INTO platform.economy_configs (key, value, note) VALUES
  ('session_retention_days', '90', '已撤銷/過期 session 的保留天數,自 COALESCE(revoked_at, expires_at) 起算(schemas/02 增補 F)');

-- +goose Down
DELETE FROM platform.economy_configs
 WHERE created_by IS NULL AND key = 'session_retention_days';
DROP TABLE platform.sessions;
DROP FUNCTION IF EXISTS platform.sessions_revocation_is_final();
