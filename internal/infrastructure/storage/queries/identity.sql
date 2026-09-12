-- 身分層(schemas/02):OAuth2 登入、身分綁定、登入 session(增補 F)。
--
-- 兩個 token 概念不要混:
--   identities.access_token_enc / refresh_token_enc = Discord 發給我們的憑證(加密存)
--   sessions.refresh_token_hash                     = 使用者登入我們系統的憑證(只存 SHA-256)
--
-- 「有效 session」在本檔案只有一個定義,所有查詢一律照抄:
--   revoked_at IS NULL AND expires_at > now() AND users.deleted_at IS NULL
-- 第三個條件 schema 表達不了(跨表),只能靠查詢契約——漏掉就是「軟刪除的帳號
-- 仍能無限滑動續期」。

-- name: GetLoginIdentity :one
-- 登入時查既有綁定。刻意不過濾 deleted_at:軟刪除的使用者仍佔著
-- UNIQUE(provider, provider_user_id),必須讓應用層看見並明確拒絕,
-- 而不是查不到就再建一個 user(那會撞唯一鍵)。
SELECT i.id            AS identity_id,
       i.username      AS identity_username,
       u.id            AS user_id,
       u.public_id     AS user_public_id,
       u.deleted_at    AS user_deleted_at
FROM platform.identities i
JOIN platform.users u ON u.id = i.user_id
WHERE i.provider = sqlc.arg(provider) AND i.provider_user_id = sqlc.arg(provider_user_id);

-- name: InsertLoginUser :one
-- 首次 OAuth 登入建立內部使用者(內部 id 是唯一權威,provider 帳號只是掛在上面的憑證)。
INSERT INTO platform.users (public_id, display_name, avatar_url, email, last_seen_at)
VALUES (sqlc.arg(public_id), sqlc.narg(display_name), sqlc.narg(avatar_url), sqlc.narg(email), now())
RETURNING id, public_id;

-- name: InsertLoginIdentity :one
INSERT INTO platform.identities
  (user_id, provider, provider_user_id, username, access_token_enc, refresh_token_enc, scope, expires_at)
VALUES (sqlc.arg(user_id), sqlc.arg(provider), sqlc.arg(provider_user_id), sqlc.narg(username),
        sqlc.narg(access_token_enc), sqlc.narg(refresh_token_enc), sqlc.narg(scope),
        sqlc.narg(expires_at))
RETURNING id;

-- name: UpdateLoginIdentity :exec
-- 既有綁定重新登入:更新 provider 側的顯示名與憑證(內部 users 的資料不覆蓋——
-- display_name 之後可由使用者自訂,provider 的名字權威在 identities.username)。
UPDATE platform.identities
SET username          = sqlc.narg(username),
    access_token_enc  = sqlc.narg(access_token_enc),
    refresh_token_enc = sqlc.narg(refresh_token_enc),
    scope             = sqlc.narg(scope),
    expires_at        = sqlc.narg(expires_at)
WHERE id = sqlc.arg(id);

-- name: EnsurePrivacySettings :exec
-- 預設列(兩級 optout 都是 false)。ON CONFLICT DO NOTHING:重跑不炸。
INSERT INTO platform.user_privacy_settings (user_id) VALUES (sqlc.arg(user_id))
ON CONFLICT (user_id) DO NOTHING;

-- name: TouchLoginUser :exec
UPDATE platform.users SET last_seen_at = now() WHERE id = sqlc.arg(id);

-- name: GetUserIDByPublicID :one
-- access token 驗簽通過後把 public_id 解成內部 id(順帶擋掉已軟刪除的帳號)。
SELECT id FROM platform.users
WHERE public_id = sqlc.arg(public_id) AND deleted_at IS NULL;

-- name: GetUserPublicID :one
-- 輪替時要把 public_id 放進新的 access token;順帶再擋一次軟刪除的帳號。
SELECT public_id FROM platform.users
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- ── session(schemas/02 增補 F)────────────────────────────────────────────

-- name: LockUserSessions :exec
-- 同一使用者的 session 多列操作(輪替、全撤、重用偵測)一律先拿這把 xact advisory lock。
-- 全系統同一把鎖、同一個取得時機 ⇒ 併發的多列操作被完全串行化,死鎖在結構上不可能,
-- 同時關掉「封鎖進行中又發出新 session」的競態窗。
SELECT pg_advisory_xact_lock(hashtext('user_sessions'), (sqlc.arg(user_id)::bigint & 2147483647)::int);

-- name: InsertSession :one
-- expires_at 用 **DB 時鐘**(應用機器時鐘漂移不該影響憑證壽命)。
-- rotated_from 撞 sessions_rotated_from_uq = 這條 session 已經被輪替過一次,
-- 也就是同一個 refresh token 被用了第二次 —— 呼叫端據此觸發重用偵測。
INSERT INTO platform.sessions
  (public_id, user_id, refresh_token_hash, expires_at, last_used_at, user_agent, ip, rotated_from)
VALUES (sqlc.arg(public_id), sqlc.arg(user_id), sqlc.arg(refresh_token_hash),
        now() + make_interval(days => sqlc.arg(ttl_days)::int),
        now(), sqlc.narg(user_agent), CAST(sqlc.narg(ip) AS inet), sqlc.narg(rotated_from))
RETURNING id, public_id, issued_at, expires_at;

-- name: GetSessionByRefreshHash :one
-- refresh 的前置讀取。這裡讀到的狀態只用來「提早回錯」與取得 user_id;
-- 「這個 token 只能換發一次」的權威是 sessions_rotated_from_uq(見 InsertSession),
-- 不是這次讀取——先查後寫在併發下擋不住,唯一索引擋得住。
SELECT s.id, s.user_id, s.expires_at, s.revoked_at, s.revoked_reason,
       s.expires_at <= now() AS is_expired,
       u.deleted_at AS user_deleted_at
FROM platform.sessions s
JOIN platform.users u ON u.id = s.user_id
WHERE s.refresh_token_hash = sqlc.arg(refresh_token_hash);

-- name: MarkSessionRotated :execrows
-- revoked_at / revoked_reason 成對寫入(CHECK sessions_revocation_paired)。
-- 回傳列數是**必須檢查**的:0 列 = 舊列在前置讀取之後被撤銷(登出/封鎖),
-- 此時整個 tx 必須回滾,不能讓新發的 session 從一條已死的鏈上長出來。
UPDATE platform.sessions
SET revoked_at = now(), revoked_reason = 'rotated', last_used_at = now()
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: SessionChainIDs :many
-- 輪替鏈:從任一列沿 rotated_from 往上(被取代者)與往下(取代者)展開整條。
-- 複合 FK 保證整條鏈同屬一個使用者,不可能撤到別人的裝置。升冪回傳。
WITH RECURSIVE chain AS (
  SELECT s.id, s.rotated_from
  FROM platform.sessions s
  WHERE s.id = sqlc.arg(session_id)
  UNION
  SELECT s.id, s.rotated_from
  FROM platform.sessions s, chain c
  WHERE s.id = c.rotated_from OR s.rotated_from = c.id
)
SELECT id FROM chain ORDER BY id;

-- name: RevokeSessionsByID :execrows
-- 重用偵測 / 封鎖:把清單內**尚未撤銷**的列一次撤掉。
-- 只動未撤銷的列不是保守,是必須:撤銷紀錄一旦寫下即為歷史
-- (trigger sessions_revocation_is_final 會擋下覆寫),而已撤銷的列本來就已失效,
-- 安全效果不變、稽核紀錄還保留了「當時是為什麼被撤的」。
-- 呼叫端傳升冪 id;併發安全另由 LockUserSessions 保證。
UPDATE platform.sessions
SET revoked_at = now(), revoked_reason = sqlc.arg(revoked_reason)::text
WHERE id = ANY(sqlc.arg(ids)::bigint[]) AND revoked_at IS NULL;

-- name: ListActiveSessionIDs :many
SELECT id FROM platform.sessions
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL
ORDER BY id;

-- name: GetSessionIDByPublicID :one
-- 「登出這台」的入口。帶 user_id 條件是越權防線(只知道 public_id 動不了別人的裝置);
-- **刻意不帶 revoked_at IS NULL**:裝置清單上拿到的 public_id 可能在使用者按下登出
-- 之前就被背景 refresh 輪替掉了(舊列 revoked、換了新 public_id)。
-- 以它為起點展開輪替鏈才能撤到那台裝置目前真正在用的那一列。
SELECT id FROM platform.sessions
WHERE public_id = sqlc.arg(public_id) AND user_id = sqlc.arg(user_id);

-- name: ListUserDevices :many
-- 有效 session 的定義照抄檔頭三條件(含 users.deleted_at)。
SELECT s.public_id, s.user_agent, COALESCE(host(s.ip), '')::text AS ip,
       s.issued_at, s.last_used_at, s.expires_at
FROM platform.sessions s
JOIN platform.users u ON u.id = s.user_id
WHERE s.user_id = sqlc.arg(user_id)
  AND s.revoked_at IS NULL AND s.expires_at > now() AND u.deleted_at IS NULL
ORDER BY COALESCE(s.last_used_at, s.issued_at) DESC, s.id DESC;

-- name: CleanupSessions :execrows
-- 膨脹歸宿(schemas/14)。條件形態**原樣**對齊表達式索引 sessions_retention_idx,
-- 改寫成 (revoked_at < ... OR expires_at < ...) 會退化成全表掃描。
-- 起算點 COALESCE(revoked_at, expires_at) 保證只命中「已撤銷或已過期」的列:
-- 截止線早於 now(),未撤銷且未過期的列不可能落在它之前。
-- rotated_from 的子列指標由 FK 的 ON DELETE SET NULL 自動斷開,不會撞 FK。
DELETE FROM platform.sessions
WHERE COALESCE(revoked_at, expires_at) < now() - make_interval(days => sqlc.arg(retention_days)::int);

-- ── 本地登入(provider='local',migration 00031)────────────────────────────
--
-- 只有 local 身分會有 secret_hash。OAuth 身分的那一欄永遠是 NULL,
-- 所以下面每一支都把 provider = 'local' 寫進 WHERE ——
-- 少了它,一個 Discord 帳號的 provider_user_id(snowflake)就成了可猜測的登入名。

-- name: GetLocalIdentity :one
-- 本地登入的唯一讀取點。
--
-- 刻意不過濾 users.deleted_at:軟刪除的帳號必須讓應用層看見並走「憑證不正確」
-- 那條路(含誘餌雜湊),查不到就直接短路的話,已註銷的登入名會回得比較快,
-- 而那個時間差就是一支帳號列舉器。
SELECT i.id         AS identity_id,
       i.secret_hash,
       u.id         AS user_id,
       u.deleted_at AS user_deleted_at
FROM platform.identities i
JOIN platform.users u ON u.id = i.user_id
WHERE i.provider = 'local' AND i.provider_user_id = sqlc.arg(login_name);

-- name: InsertLocalIdentity :one
-- 建立本地身分。撞 UNIQUE(provider, provider_user_id) = 這個登入名已被用掉,
-- 那是唯一權威(不靠先查後寫)。
INSERT INTO platform.identities (user_id, provider, provider_user_id, username, secret_hash)
VALUES (sqlc.arg(user_id), 'local', sqlc.arg(login_name), sqlc.narg(username), sqlc.arg(secret_hash)::text)
RETURNING id;

-- name: UpdateLocalSecret :execrows
-- 重新產生通行碼(裁判忘記時的唯一修復路徑 —— 雜湊格式手寫不出來)。
-- 回傳列數要檢查:0 列 = 這個登入名沒有本地身分,不能無聲當成成功。
UPDATE platform.identities
SET secret_hash = sqlc.arg(secret_hash)::text
WHERE provider = 'local' AND provider_user_id = sqlc.arg(login_name);

-- name: GrantRoleByKey :execrows
-- 授予全域角色(community_id IS NULL)。source='manual':之後同步 Discord
-- 身分組時只撤 provider_sync,手動授予不受影響(migration 00004 的設計)。
-- ON CONFLICT DO NOTHING 對齊 user_roles_uniq,重跑不炸。
INSERT INTO platform.user_roles (user_id, role_id, community_id, source)
SELECT sqlc.arg(user_id), r.id, NULL, 'manual'
FROM platform.roles r
WHERE r.key = sqlc.arg(role_key)
ON CONFLICT (user_id, role_id, COALESCE(community_id, 0)) DO NOTHING;
