-- 活動記錄(schemas/10-activity-logs.md、schemas/11-messages.md)。
--
-- 貫穿本檔的兩件事:
--   1. **寫入時彙總**:事實與 activity_daily / message_stats 在同一個 tx。
--   2. **冪等靠天然唯一鍵**,不另設冪等鍵表(專案第 9 條)。
--      有 UNIQUE 的用 ON CONFLICT;沒有 UNIQUE 的(voice_sessions /
--      presence_spans / reaction_events 這三張表沒有天然鍵的約束)
--      用 advisory lock 把同鍵的併發串行化,再「查了才寫」。

-- ══ 解析:外部識別碼 → 內部 id ══

-- name: ResolveIdentityUser :one
-- X-Acting-User 的解析。查不到就是查不到 —— 呼叫端絕不可在這裡建帳號,
-- 建帳號的權威在 OAuth 登入流程(否則會產生沒有 privacy 設定列的殘缺使用者)。
SELECT user_id FROM platform.identities
WHERE provider = $1 AND provider_user_id = $2;

-- name: GetSpaceByExternalID :one
-- guild snowflake → (space_id, community_id)。community_id 是 activity_daily 的 PK 之一。
SELECT id, community_id FROM platform.community_spaces
WHERE provider = $1 AND external_id = $2;

-- name: GetSpaceChannel :one
-- 頻道註冊表(schemas/01 增補 C)= 白名單權威。
-- 查無列 = 未註冊:log_messages 視為 false(白名單制),grant_xp 視為 true
-- (欄位預設值;白名單管的是「內容要不要落地」,不是「要不要計分」)。
SELECT log_messages, grant_xp FROM platform.space_channels
WHERE space_id = $1 AND external_id = $2;

-- name: GetOptOutLogging :one
-- 查無列 = 從未設定 = false(不退出)。
SELECT opt_out_logging FROM platform.user_privacy_settings WHERE user_id = $1;

-- name: LockActivityKey :exec
-- 沒有 UNIQUE 約束可用時的併發防線:把「同一個天然鍵」的併發寫入串行化,
-- 讓「先查再寫」不會兩個 tx 同時通過檢查。tx 結束自動釋放。
-- hash 碰撞只會造成無關鍵之間偶爾互等,不影響正確性。
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- ══ 彙總層 ══

-- name: BumpActivityDaily :exec
-- 一句話完成「有就加、沒有就建」。date 由事件時間在 UTC 下取日,
-- 不用使用者時區:投影必須可重算(users.timezone 會變,拿它當分桶鍵
-- 會讓同一批事實在不同時間重算出不同結果)。
INSERT INTO platform.activity_daily (
  user_id, community_id, date,
  messages, reactions, voice_seconds, online_seconds,
  first_seen_at, last_seen_at
) VALUES (
  sqlc.arg(user_id), sqlc.arg(community_id),
  (sqlc.arg(occurred_at)::timestamptz AT TIME ZONE 'UTC')::date,
  sqlc.arg(messages), sqlc.arg(reactions),
  sqlc.arg(voice_seconds), sqlc.arg(online_seconds),
  sqlc.arg(occurred_at)::timestamptz, sqlc.arg(occurred_at)::timestamptz
)
ON CONFLICT (user_id, community_id, date) DO UPDATE SET
  messages       = platform.activity_daily.messages       + EXCLUDED.messages,
  reactions      = platform.activity_daily.reactions      + EXCLUDED.reactions,
  voice_seconds  = platform.activity_daily.voice_seconds  + EXCLUDED.voice_seconds,
  online_seconds = platform.activity_daily.online_seconds + EXCLUDED.online_seconds,
  -- LEAST / GREATEST 在 Postgres 會忽略 NULL,所以舊列的空值不會吃掉新值。
  first_seen_at  = LEAST(platform.activity_daily.first_seen_at, EXCLUDED.first_seen_at),
  last_seen_at   = GREATEST(platform.activity_daily.last_seen_at, EXCLUDED.last_seen_at);

-- name: AddActivityDailyXP :exec
-- XP 入帳後回填當日彙總。單獨一句是因為 XP 入帳走 xp.Service(自己開 tx),
-- 不在事實那個 tx 裡(理由見 activitylogpg 的註解)。
UPDATE platform.activity_daily
SET xp_gained = xp_gained + sqlc.arg(xp_gained)
WHERE user_id = sqlc.arg(user_id)
  AND community_id = sqlc.arg(community_id)
  AND date = (sqlc.arg(occurred_at)::timestamptz AT TIME ZONE 'UTC')::date;

-- ══ 訊息 ══

-- name: ClaimMessage :one
-- **訊息冪等的權威**:message_stats.message_id 是 PK,而且不論頻道白名單或
-- opt-out 都會寫這一列 —— 所以它是唯一「每則訊息必定存在一次」的鍵。
-- 回傳空列 = 這則訊息先前已處理過,整筆跳過。
-- 併發同一則訊息時,後到的那個 tx 會卡在 PK 上等待,commit 後回 0 列。
INSERT INTO platform.message_stats (message_id, user_id)
VALUES ($1, $2)
ON CONFLICT (message_id) DO NOTHING
RETURNING message_id;

-- name: EnsureMessageStats :exec
-- reaction 用:沒見過的訊息也要有 stats 列才能累加 reaction_count。
INSERT INTO platform.message_stats (message_id, user_id)
VALUES ($1, $2)
ON CONFLICT (message_id) DO NOTHING;

-- name: MessageStatsExists :one
SELECT EXISTS (
  SELECT 1 FROM platform.message_stats WHERE message_id = $1
) AS found;

-- name: InsertMessageLog :exec
-- excerpt 為 NULL 的兩種情況:頻道沒開 log_messages(那時根本不呼叫本查詢)、
-- 作者 opt_out_logging(呼叫但 excerpt 傳 NULL)。full_length 照記 ——
-- 它是長度不是內容,而且是「截斷長度夠不夠」的唯一檢討依據。
-- ON CONFLICT 對應既有的 UNIQUE (message_id, created_at):ClaimMessage 已經擋過
-- 一次,這裡是資料表自己的第二道防線,不是第二個權威。
INSERT INTO platform.message_logs (
  user_id, space_id, channel_id, channel_kind, thread_id, message_id,
  excerpt, full_length, has_attachment, reply_to, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (message_id, created_at) DO NOTHING;

-- name: IncrementReplyCount :exec
-- 只有 hestia 見過的被回覆訊息才累加(message_stats.user_id 是 NOT NULL,
-- 沒見過就沒有作者可填 —— 那種情況直接略過,不猜)。
UPDATE platform.message_stats
SET reply_count = reply_count + 1, updated_at = now()
WHERE message_id = $1;

-- name: MarkMessageDeleted :exec
-- 刪除事件同時在 message_logs 打上 deleted_at(欄位本來就是為此存在)。
-- 沒有那一列(頻道沒開白名單)時什麼都不做。
UPDATE platform.message_logs
SET deleted_at = $2
WHERE message_id = $1 AND deleted_at IS NULL;

-- ══ 訊息舊版本 ══

-- name: DeletionRevisionExists :one
-- 刪除沒有可靠的事件時間(Discord 不給),所以冪等改用「一則訊息只會被刪一次」。
SELECT EXISTS (
  SELECT 1 FROM platform.message_revisions
  WHERE message_id = $1 AND kind = 'deleted'
) AS found;

-- name: EditRevisionExists :one
-- 編輯用 (message_id, captured_at):Discord 的 edited_timestamp 重送時穩定,
-- 而同一則訊息可以被編輯很多次,不能只用 message_id。
SELECT EXISTS (
  SELECT 1 FROM platform.message_revisions
  WHERE message_id = $1 AND kind = 'edited' AND captured_at = $2
) AS found;

-- name: InsertMessageRevision :exec
INSERT INTO platform.message_revisions (message_id, user_id, excerpt, kind, captured_at)
VALUES ($1, $2, $3, $4, $5);

-- ══ Reaction ══

-- name: GetReaction :one
-- 天然鍵 = (message_id, user_id, emoji)。一個人對同一則訊息的同一個表情
-- 只有一列,按/取消是那一列的 added_at / removed_at ——
-- 這正是 schemas/11 給這張表的形狀。
SELECT id, removed_at FROM platform.reaction_events
WHERE message_id = $1 AND user_id = $2 AND emoji = $3
ORDER BY id
LIMIT 1;

-- name: InsertReaction :exec
INSERT INTO platform.reaction_events (message_id, user_id, space_id, emoji, added_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ReopenReaction :exec
-- 取消後又按回來:復用同一列。activity_daily.reactions **不再加一次** ——
-- 否則按了取消再按就是無限刷參與度。
UPDATE platform.reaction_events
SET added_at = $2, removed_at = NULL
WHERE id = $1;

-- name: CloseReaction :exec
UPDATE platform.reaction_events
SET removed_at = $2
WHERE id = $1 AND removed_at IS NULL;

-- name: BumpMessageReactionCount :exec
-- delta 為 -1 時用 GREATEST 夾住 0:計數是衍生值,寧可保守也不要出現負數。
UPDATE platform.message_stats
SET reaction_count = GREATEST(0, reaction_count + $2), updated_at = now()
WHERE message_id = $1;

-- ══ 語音 ══

-- name: GetVoiceSession :one
-- 天然鍵 = (user_id, space_id, channel_id, joined_at)。
SELECT id, joined_at, left_at, duration_seconds FROM platform.voice_sessions
WHERE user_id = $1 AND space_id = $2 AND channel_id = $3 AND joined_at = $4;

-- name: InsertVoiceSession :exec
INSERT INTO platform.voice_sessions (
  user_id, space_id, channel_id, joined_at, left_at,
  duration_seconds, self_muted, self_deafened, peer_count_avg
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: CloseVoiceSession :exec
-- 帶 joined_at 讓分區裁剪生效(voice_sessions 按 joined_at 分區)。
-- COALESCE:補收尾的事件若沒帶靜音狀態,保留進場時記下的值。
UPDATE platform.voice_sessions
SET left_at          = sqlc.arg(left_at),
    duration_seconds = sqlc.arg(duration_seconds),
    self_muted       = COALESCE(sqlc.narg(self_muted), self_muted),
    self_deafened    = COALESCE(sqlc.narg(self_deafened), self_deafened),
    peer_count_avg   = COALESCE(sqlc.narg(peer_count_avg), peer_count_avg)
WHERE id = sqlc.arg(id) AND joined_at = sqlc.arg(joined_at) AND left_at IS NULL;

-- ══ Presence ══

-- name: GetPresenceSpan :one
SELECT id, started_at, ended_at FROM platform.presence_spans
WHERE user_id = $1 AND space_id = $2 AND started_at = $3;

-- name: InsertPresenceSpan :exec
INSERT INTO platform.presence_spans (user_id, space_id, status, started_at, ended_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ClosePresenceSpan :exec
UPDATE platform.presence_spans
SET ended_at = $3
WHERE id = $1 AND started_at = $2 AND ended_at IS NULL;
