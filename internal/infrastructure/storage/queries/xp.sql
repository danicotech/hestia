-- XP query。設計依 schemas/06(含增補 E)與 schemas/01 增補 A:
-- xp_events 是事實、user_xp 是投影(必可重算)、XP 不進帳本不經 Ledger(刻意設計,
-- XP 錯了重算即可,沒有對帳需求;混進帳本會淹沒金流稽核)。
-- 冷卻用 user_xp.last_xp_at 直接查 DB(schemas/06:不上 Redis)。

-- name: GetXpEventType :one
-- source 檢查:registry 管「存在與開關」(全域);數值/冷卻/上限在 xp_rulesets.config
SELECT key, enabled FROM platform.xp_event_types WHERE key = $1;

-- name: GetCommunityXpConfig :one
-- LEFT JOIN:community 存在但 xp_ruleset_id 為 NULL(M1 可能還沒建 ruleset)時
-- 回 NULL config,呼叫端採安全預設(無冷卻、無 cap);community 不存在 → 無列(ErrNoRows)
SELECT r.config
FROM platform.communities c
LEFT JOIN platform.xp_rulesets r ON r.id = c.xp_ruleset_id
WHERE c.id = $1;

-- name: EnsureUserXpRow :exec
-- 與 EnsureBalanceRow 同模式:先保證投影列存在,才能 FOR UPDATE 串行化同 user 的併發入帳
INSERT INTO platform.user_xp (user_id, community_id)
VALUES ($1, $2)
ON CONFLICT (user_id, community_id) DO NOTHING;

-- name: LockUserXp :one
-- 一併回 DB 時鐘:冷卻比較的兩端(事件時間與 now)都用 DB 時鐘,
-- 單一時鐘來源,app/DB 時鐘偏移不影響判斷
SELECT xp, now()::timestamptz AS db_now
FROM platform.user_xp
WHERE user_id = $1 AND community_id = $2
FOR UPDATE;

-- name: LastXpEventAtBySource :one
-- 冷卻計時器:該 (user, community, source) 最近一筆事件的時間。
-- 不用 user_xp.last_xp_at 當計時器 —— 它是「最後任何入帳」的跨 source 資訊欄位,
-- 拿來計時會讓 voice 高頻入帳餓死 message 的冷卻、admin 修正也會重置計時(QA 中1)。
-- 無列(ErrNoRows)= 該 source 從未入帳 = 無冷卻。呼叫前必須已 LockUserXp(串行化)
SELECT created_at FROM platform.xp_events
WHERE user_id = $1 AND community_id = $2 AND source = $3
ORDER BY created_at DESC
LIMIT 1;

-- name: SumXpTodayBySource :one
-- daily_cap 判斷:UTC 當日該 user 該 community 該 source 的總和(schemas/01 A)。
-- 呼叫前必須已 LockUserXp,同 user 的併發入帳已串行化,SUM 不會低估
SELECT COALESCE(SUM(amount), 0)::bigint AS total
FROM platform.xp_events
WHERE user_id = $1 AND community_id = $2 AND source = $3
  AND created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';

-- name: InsertXpEvent :one
INSERT INTO platform.xp_events (user_id, community_id, space_id, source, amount, ref_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, created_at;

-- name: AddUserXp :one
-- 與 InsertXpEvent 同 tx;now() 為 tx 時間,與事件的 created_at 同值。
-- level 不動:M1 曲線未上線,恆 0(schemas/06)
UPDATE platform.user_xp
SET xp = xp + $3, last_xp_at = now(), updated_at = now()
WHERE user_id = $1 AND community_id = $2
RETURNING xp;

-- name: RebuildUserXp :one
-- 投影重算:xp ← SUM(events)、last_xp_at ← MAX(created_at),整個投影可從事實重建。
-- 呼叫前必須已 LockUserXp,否則與並行 Award 有覆寫競態(先算 SUM、後拿鎖會蓋掉新事件)
UPDATE platform.user_xp u
SET xp = s.total, last_xp_at = s.last_at, updated_at = now()
FROM (
  SELECT COALESCE(SUM(amount), 0)::bigint AS total, MAX(created_at) AS last_at
  FROM platform.xp_events
  WHERE user_id = $1 AND community_id = $2
) s
WHERE u.user_id = $1 AND u.community_id = $2
RETURNING u.xp;

-- ── 里程碑獎勵(schemas/24)────────────────────────────────────────────

-- name: ListLevelRewardsBetween :many
-- 這次升級跨過的所有里程碑。一次跳兩級以上是可能的(語音一次入帳很大),
-- 所以取區間而不是等於 —— 只看新等級會漏掉中間那些。
SELECT id, level, reward_kind, reward_ref, amount, note
FROM platform.level_rewards
WHERE community_id = $1 AND subject = $2 AND level > $3 AND level <= $4
ORDER BY level;

-- name: ClaimLevelReward :execrows
-- 冪等鍵。outbox 是至少一次投遞,重送時這裡撞鍵 → 0 列 → 呼叫端跳過發放。
-- 沒有它的話,重試一次就發兩隻寵物。
INSERT INTO platform.level_reward_grants (reward_id, user_id, pet_instance_id)
VALUES ($1, $2, sqlc.narg(pet_instance_id)::bigint)
ON CONFLICT (reward_id, user_id, COALESCE(pet_instance_id, 0)) DO NOTHING;

-- name: GrantRoleByPublicID :execrows
-- source='level_reward':與 manual / provider_sync 分開,身分組同步撤銷時
-- 不會誤刪里程碑發出去的角色。
INSERT INTO platform.user_roles (user_id, role_id, community_id, source, granted_at)
SELECT $1, r.id, $2, 'level_reward', now()
FROM platform.roles r
WHERE r.public_id = $3
ON CONFLICT (user_id, role_id, COALESCE(community_id, 0)) DO NOTHING;

-- name: GrantItemByDefinitionPublicID :one
-- 發一件物品。bound 跟著定義走:成就類的東西不該能轉手賣掉。
INSERT INTO platform.item_instances
  (public_id, definition_id, owner_id, bound, acquired_via, acquired_at)
SELECT $1, d.id, $2, d.bind_on_acquire, 'level_reward', now()
FROM platform.item_definitions d
WHERE d.public_id = $3
RETURNING id, public_id;
