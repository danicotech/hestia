-- 小遊戲、開箱、抽獎、寵物的查詢(schemas/25)。
--
-- 檔名是 play 不是 chance:抽獎(giveaway)本身沒有隨機以外的共通點,
-- 但這四個功能在使用者眼裡是同一件事 ——「可以玩的東西」。
--
-- 注意:activity_ 前綴保留給活動層(themis),這裡不使用。

-- ── 抽籤留痕(三處共用)───────────────────────────────────────

-- name: InsertChanceDraw :one
-- 每一次隨機都要留痕:產出速率、黑箱質疑、賠率變更的稽核都靠它。
INSERT INTO platform.chance_draws
  (community_id, user_id, kind, ref, odds_snapshot, seed, outcome, stake, payout)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, created_at;

-- name: CountDrawsToday :one
-- 每日次數上限(schemas/25:20 次/人/日)。用 UTC 當日,與 XP 的 daily_cap 一致 ——
-- 兩個「今天」用不同定義會讓人在某個時區看到兩者不同步。
SELECT count(*)::bigint FROM platform.chance_draws
WHERE user_id = $1
  AND created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';

-- name: SumFaucetSince :one
-- 產出速率:這段時間系統淨吐出多少點。負數 = 淨回收(水槽正常運作)。
SELECT COALESCE(SUM(payout - stake), 0)::bigint AS net
FROM platform.chance_draws
WHERE community_id = $1 AND created_at >= $2;

-- ── 開箱 ──────────────────────────────────────────────────────

-- name: GetLootBoxByPublicID :one
SELECT id, public_id, community_id, name, cost_currency, cost_amount, daily_limit, enabled
FROM platform.loot_boxes WHERE public_id = $1;

-- name: ListLootBoxes :many
SELECT public_id, name, cost_currency, cost_amount, daily_limit
FROM platform.loot_boxes WHERE community_id = $1 AND enabled ORDER BY id;

-- name: LockLootBoxItems :many
-- **FOR UPDATE**:有限獎池的 remaining 要在同一個 transaction 裡讀了再扣,
-- 否則兩個人同時抽最後一件,兩個人都會拿到。
SELECT id, reward_kind, reward_ref, amount, weight, remaining
FROM platform.loot_box_items
WHERE box_id = $1
ORDER BY id
FOR UPDATE;

-- name: DecrementLootBoxItem :execrows
-- 條件帶 remaining > 0:就算呼叫端算錯,DB 也不會讓它變成負數。
UPDATE platform.loot_box_items
SET remaining = remaining - 1
WHERE id = $1 AND remaining IS NOT NULL AND remaining > 0;

-- ── 抽獎活動 ──────────────────────────────────────────────────

-- name: CreateGiveaway :one
INSERT INTO platform.giveaways
  (public_id, community_id, space_id, title, prize_kind, prize_ref, prize_amount,
   winner_count, entry_cost, opens_at, closes_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11)
RETURNING id, public_id, title, closes_at;

-- name: GetGiveawayByPublicID :one
SELECT id, public_id, community_id, title, prize_kind, prize_ref, prize_amount,
       winner_count, entry_cost, status, opens_at, closes_at, created_by
FROM platform.giveaways WHERE public_id = $1;

-- name: ListOpenGiveaways :many
SELECT g.public_id, g.title, g.entry_cost, g.winner_count, g.closes_at,
       (SELECT count(*) FROM platform.giveaway_entries e WHERE e.giveaway_id = g.id)::bigint AS entries
FROM platform.giveaways g
WHERE g.community_id = $1 AND g.status = 'open' AND g.closes_at > now()
ORDER BY g.closes_at;

-- name: EnterGiveaway :execrows
-- 一人一次靠 UNIQUE 擋,不靠應用層先查再寫(那有競態,而且是兩份真相)。
INSERT INTO platform.giveaway_entries (giveaway_id, user_id)
VALUES ($1, $2)
ON CONFLICT (giveaway_id, user_id) DO NOTHING;

-- name: LockGiveawayForDraw :one
-- 開獎要序列化:兩個人同時按開獎會抽出兩組贏家,而獎品只有一份。
SELECT id, community_id, prize_kind, prize_ref, prize_amount, winner_count, status
FROM platform.giveaways WHERE id = $1 FOR UPDATE;

-- name: PickGiveawayWinners :many
-- 隨機取 N 位。random() 在這裡夠用:抽獎的公平性由「誰都不能改參加名單」
-- 保證(UNIQUE + append),不是由亂數品質保證;而且結果會連同 seed 進 chance_draws。
SELECT e.id, e.user_id
FROM platform.giveaway_entries e
WHERE e.giveaway_id = $1
ORDER BY random()
LIMIT $2;

-- name: MarkGiveawayWinners :execrows
UPDATE platform.giveaway_entries SET won = true
WHERE giveaway_id = $1 AND user_id = ANY($2::bigint[]);

-- name: CloseGiveaway :exec
UPDATE platform.giveaways SET status = 'drawn', drawn_at = now() WHERE id = $1;

-- ── 寵物 ──────────────────────────────────────────────────────

-- name: ListPets :many
SELECT ii.public_id, COALESCE(ps.nickname, d.name) AS name, d.rarity, d.icon_url,
       ps.xp, ps.deployed
FROM platform.pet_states ps
JOIN platform.item_instances ii ON ii.id = ps.item_instance_id
JOIN platform.item_definitions d ON d.id = ii.definition_id
WHERE ps.owner_id = $1
ORDER BY ps.deployed DESC, ps.xp DESC;

-- name: GetPetByPublicID :one
SELECT ps.item_instance_id, ps.owner_id, ps.xp, ps.deployed,
       COALESCE(ps.nickname, d.name) AS name
FROM platform.pet_states ps
JOIN platform.item_instances ii ON ii.id = ps.item_instance_id
JOIN platform.item_definitions d ON d.id = ii.definition_id
WHERE ii.public_id = $1;

-- name: UndeployAllPets :exec
-- 換出戰前先全部收起來。部分唯一索引保證「同時只有一隻」,
-- 但那是最後防線 —— 直接撞索引會得到約束錯誤而不是可讀訊息。
UPDATE platform.pet_states SET deployed = false, updated_at = now()
WHERE owner_id = $1 AND deployed;

-- name: DeployPet :execrows
UPDATE platform.pet_states SET deployed = true, updated_at = now()
WHERE item_instance_id = $1 AND owner_id = $2;

-- name: RenamePet :execrows
UPDATE platform.pet_states SET nickname = $3, updated_at = now()
WHERE item_instance_id = $1 AND owner_id = $2;

-- name: AddPetXP :one
UPDATE platform.pet_states SET xp = xp + $2, updated_at = now()
WHERE item_instance_id = $1
RETURNING xp;

-- name: GetDeployedPetID :one
SELECT item_instance_id, xp FROM platform.pet_states
WHERE owner_id = $1 AND deployed;

-- name: CreatePetState :exec
-- 發到寵物時建狀態列。owner_id 是 item_instances.owner_id 的受控副本
-- (複合外鍵保證一致),存它是為了「同時只能出戰一隻」那條部分唯一索引。
INSERT INTO platform.pet_states (item_instance_id, owner_id)
VALUES ($1, $2)
ON CONFLICT (item_instance_id) DO NOTHING;

-- name: GrantItemToUser :one
-- 開箱與抽獎共用的發物品路徑。
INSERT INTO platform.item_instances
  (public_id, definition_id, owner_id, bound, acquired_via, acquired_at)
SELECT $1, d.id, $2, d.bind_on_acquire, $4, now()
FROM platform.item_definitions d
WHERE d.public_id = $3
RETURNING id, public_id, (SELECT category FROM platform.item_definitions WHERE public_id = $3) AS category;
