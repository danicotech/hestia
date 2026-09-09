-- 社群與空間的管理端查詢(cmd/admin)。
--
-- 為什麼這幾支不在 activitylog.sql:那支是**執行期**的讀取路徑
-- (guild → space,每則訊息都會走)。這裡是管理端的建立與列出,
-- 頻率、權限、失敗後果都不同,混在一起會讓「哪些查詢在熱路徑上」看不出來。
--
-- 目前唯一的呼叫端是 CLI。之後補管理 RPC 時直接複用這些查詢,
-- 不要另外寫一份近義的 SQL(專案第 9 條)。

-- name: CreateCommunity :one
INSERT INTO platform.communities (public_id, name)
VALUES ($1, $2)
RETURNING id, public_id, name;

-- name: GetCommunityByPublicID :one
SELECT id, public_id, name FROM platform.communities WHERE public_id = $1;

-- name: ListCommunities :many
SELECT id, public_id, name, xp_ruleset_id, created_at
FROM platform.communities ORDER BY id;

-- name: GetSpaceByProviderExternalID :one
-- 註冊前先查:CLI 重跑不該噴 UNIQUE violation,也不該無聲改掉既有設定。
SELECT id, public_id, community_id, name FROM platform.community_spaces
WHERE provider = $1 AND external_id = $2;

-- name: CreateSpace :one
INSERT INTO platform.community_spaces (public_id, community_id, provider, external_id, name)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, public_id, community_id, name;

-- name: ListSpaces :many
SELECT s.id, s.public_id, s.community_id, s.provider, s.external_id, s.name,
       c.name AS community_name, c.public_id AS community_public_id
FROM platform.community_spaces s
JOIN platform.communities c ON c.id = s.community_id
ORDER BY s.id;

-- name: UpsertSpaceChannel :one
-- 這裡用 upsert 而不是「先查再建」,是因為重跑的語意不同:再跑一次
-- register-channel 帶不同旗標,意思就是「改成這樣」。
-- archived_at 一併清掉:重新註冊等於重新啟用。
INSERT INTO platform.space_channels (space_id, external_id, name, kind, log_messages, grant_xp)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (space_id, external_id) DO UPDATE SET
  name         = EXCLUDED.name,
  kind         = EXCLUDED.kind,
  log_messages = EXCLUDED.log_messages,
  grant_xp     = EXCLUDED.grant_xp,
  archived_at  = NULL
RETURNING id, external_id, name, kind, log_messages, grant_xp;

-- name: ListSpaceChannels :many
SELECT id, external_id, name, kind, log_messages, grant_xp, archived_at
FROM platform.space_channels WHERE space_id = $1 ORDER BY id;
