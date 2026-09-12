-- 《百業試鋒》活動層的**讀取側**(對應 transport.ActivityReader)。
-- 全檔只有 SELECT,一句寫入都沒有。
--
-- ── 為什麼另立一個檔案 ────────────────────────────────────────
--
-- activity_tournament.sql 與 activity_match.sql 各自服務一個領域埠
-- (tournament.Repo / match.Repository),它們的每一支查詢都背著鎖序與階段
-- 守門的約定。觀眾頁要的是完全相反的東西:不取任何鎖、不改任何狀態、
-- 看得到已淘汰與已棄賽的人。混進去的話,那兩個檔頭的「本檔所有查詢都在
-- 鎖序約定之內」就會出現例外,而例外正是日後有人照著抄出死鎖的來源。
--
-- ── 沿用既有查詢,不重寫 ──────────────────────────────────────
--
--   單場       GetMatchForJudge(activity_match.sql)—— 無鎖重讀,欄位就是
--              對戰表要的全部。watchpg 的推播路徑也用它(規則 9:同一個畫面上的
--              同一個數字不該有兩條產生路徑)。這裡因此**沒有** MatchByPublicID。
--   單一選手   GetPlayerByPublicID(activity_tournament.sql)—— 同樣的簽名、
--              同樣的一句 SQL,adapter 直接委派給 tournamentpg。
--
-- ── 欄位順序是契約 ────────────────────────────────────────────
--
-- ListTournamentPlayers 的欄位必須與 GetPlayerByPublicID 逐字相同,
-- ListTournamentMatches / CurrentMatchOfPlayer 必須與 GetMatchForJudge 逐字相同。
-- adapter 靠 Go 的結構轉換共用同一份映射 —— 不一致會在編譯期爆掉,
-- 那正是要的:錯位在 build 時就被看見,不是執行期靜靜接錯欄位。

-- ═══ 賽事 ═══════════════════════════════════════════════════

-- name: GetTournamentByID :one
-- 依內部 id 讀一屆賽事。欄位與 GetTournamentBySlug 逐字相同(共用同一份列轉換)。
--
-- 為什麼不用 JudgeTournamentByID:那一支帶 FOR SHARE,而且回的是裁判流程要的
-- 形狀(含 TotalRounds、不含 config 以外的欄位對不上 tournament.Tournament)。
-- 觀眾讀一場比賽的所屬賽事時不該對賽事列取任何鎖 —— 一個沒有人在寫的路徑
-- 去排隊等鎖,只會在裁判正在抽籤時讓整個對戰表頁卡住。
SELECT id, public_id, slug, name, community_id, phase, config, signup_bonus,
       created_at, updated_at
FROM activity.tournaments
WHERE id = sqlc.arg(tournament_id)::bigint;

-- name: GetTournamentByPlayerPublicID :one
-- 依選手 public_id 反查他所屬的那一屆。
--
-- 存在的理由是契約而不是方便:AssignRank / SwapSeeds / RegeneratePasscode 的
-- 請求只帶 player_public_id(選手 public_id 是 ULID,全域唯一,不需要 slug 去
-- 消歧義),而領域服務的參數要 slug。這條反查就是那個落差。
-- 查無 0 列 → adapter 回 tournament.ErrPlayerNotFound:輸入是一個選手 id,
-- 「這個選手不存在」才是誠實的答案,回「賽事不存在」會讓裁判去找錯地方。
SELECT t.id, t.public_id, t.slug, t.name, t.community_id, t.phase, t.config,
       t.signup_bonus, t.created_at, t.updated_at
FROM activity.tournaments t
JOIN activity.tournament_players tp ON tp.tournament_id = t.id
WHERE tp.public_id = sqlc.arg(player_public_id)::text;

-- ═══ 選手 ═══════════════════════════════════════════════════

-- name: CountTournamentPlayers :one
-- 本屆報名人數(Tournament.player_count)。
--
-- **不**過濾 status:這個數字是「有幾個人報了名」,不是「還有幾個人在賽中」。
-- 把棄賽者排除掉的話,賽事頁的人數會在活動進行中自己往下掉,而報名人數
-- 是一個已經發生過、不會再變的事實。
SELECT count(*)::int AS player_count
FROM activity.tournament_players
WHERE tournament_id = sqlc.arg(tournament_id)::bigint;

-- name: ListTournamentPlayers :many
-- 本屆全部參賽者,**含已淘汰與已棄賽**。欄位與 GetPlayerByPublicID 逐字相同。
--
-- 排序:籤位優先,未抽籤者(seed_no IS NULL)排在最後,同組內依報名序。
--   * NULLS LAST 是 Postgres 對 ASC 的預設,這裡仍然寫出來 —— 讀的人不必
--     去查文件才知道報名期間(全部都是 NULL)的清單會不會反過來。
--   * tp.id 就是報名序:它是 BIGSERIAL,先報名的人 id 一定比較小。
--     用 created_at 的話,同一毫秒內的兩筆報名順序就不定了。
-- 排序不是排版偏好:報名頁與對戰表都靠這個順序呈現,不穩定的順序會讓
-- 同一頁重新整理兩次看到不同的排列。
SELECT tp.id, tp.public_id, tp.tournament_id, tp.fencer_id, tp.user_id,
       tp.display_name, tp.discord_name,
       tp.rank_level, tp.ranked_at, tp.ranked_by,
       tp.self_rated_level, tp.ladder_rank, tp.ladder_score,
       tp.arts_note, tp.availability_note,
       tp.seed_no, tp.status, tp.created_at, tp.updated_at,
       f.game_id
FROM activity.tournament_players tp
JOIN activity.fencers f ON f.id = tp.fencer_id
WHERE tp.tournament_id = sqlc.arg(tournament_id)::bigint
ORDER BY tp.seed_no ASC NULLS LAST, tp.id ASC;

-- ═══ 對戰表 ═════════════════════════════════════════════════

-- name: ListTournamentMatches :many
-- 本屆全部場次,依 (round, slot) 遞增。欄位與 GetMatchForJudge 逐字相同。
--
-- 尚未抽籤時回 0 列 —— 那不是錯誤,賽事存在、只是還沒抽(transport 靠
-- 「空清單」決定顯示「尚未抽籤」而不是 404)。
--
-- 不加任何 FOR SHARE / FOR UPDATE:這是匿名可讀的路徑,決賽當下可能有上百人
-- 同時在看。對 matches 取鎖會讓觀眾與裁判互相排隊,而觀眾一個字都不會寫。
SELECT
  m.id, m.public_id, m.tournament_id, m.round, m.slot,
  m.status, m.result_kind, m.handicap_open, m.handicap_locked_at,
  m.stream_url, m.started_at, m.finished_at, m.winner_player_id,
  m.p1_player_id,
  p1.public_id    AS p1_public_id,
  p1.fencer_id    AS p1_fencer_id,
  p1.display_name AS p1_display_name,
  p1.rank_level   AS p1_rank_level,
  p1.status       AS p1_status,
  m.p2_player_id,
  p2.public_id    AS p2_public_id,
  p2.fencer_id    AS p2_fencer_id,
  p2.display_name AS p2_display_name,
  p2.rank_level   AS p2_rank_level,
  p2.status       AS p2_status
FROM activity.matches m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id
WHERE m.tournament_id = sqlc.arg(tournament_id)::bigint
ORDER BY m.round ASC, m.slot ASC;

-- name: CurrentMatchOfPlayer :one
-- 這位選手「現在輪到要打」的那一場:未完賽的場次裡 (round, slot) 最小的一場。
-- 查無 0 列 → adapter 回 nil(不是錯誤)。三種情況都會落到 0 列:
--
--   已淘汰   敗的那一場是 done,之後的場次坐的是對手 → 沒有未完賽的場次
--   已棄賽   靠 EXISTS 那道條件擋掉(見下)
--   未抽籤   整屆一場都還沒有
--
-- **status = 'active' 的檢查不是多餘的。** 棄賽的級聯只會把「對手已就位」的
-- 場次判成不戰而勝;對手未定的那些留在 pending 等晉級鏈接手
-- (LockUnfinishedMatchesOfPlayer 的註解)。沒有這道條件的話,一位已經棄賽的
-- 選手登入後會看到「你的下一場」,而他根本不會上場。
--
-- 含對手未定(p2 為 NULL)的場次:那正是「等上一輪」的樣子,選手頁要顯示的
-- 是「下一場對手未定」而不是「沒有比賽」。
--
-- 排序與 LIMIT 1 缺一不可:一位選手同時有兩場未完賽是可能的(剛晉級、
-- 下一輪的節點已經建好),沒有排序的話「目前這一場」會在兩者之間跳動。
SELECT
  m.id, m.public_id, m.tournament_id, m.round, m.slot,
  m.status, m.result_kind, m.handicap_open, m.handicap_locked_at,
  m.stream_url, m.started_at, m.finished_at, m.winner_player_id,
  m.p1_player_id,
  p1.public_id    AS p1_public_id,
  p1.fencer_id    AS p1_fencer_id,
  p1.display_name AS p1_display_name,
  p1.rank_level   AS p1_rank_level,
  p1.status       AS p1_status,
  m.p2_player_id,
  p2.public_id    AS p2_public_id,
  p2.fencer_id    AS p2_fencer_id,
  p2.display_name AS p2_display_name,
  p2.rank_level   AS p2_rank_level,
  p2.status       AS p2_status
FROM activity.matches m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id
WHERE (m.p1_player_id = sqlc.arg(player_id)::bigint
    OR m.p2_player_id = sqlc.arg(player_id)::bigint)
  AND m.status <> 'done'
  AND EXISTS (
    SELECT 1 FROM activity.tournament_players me
    WHERE me.id = sqlc.arg(player_id)::bigint
      AND me.status = 'active'
  )
ORDER BY m.round ASC, m.slot ASC
LIMIT 1;

-- ═══ 跨屆檔案 ═══════════════════════════════════════════════

-- name: ListFencersByIDs :many
-- 批次取跨屆選手檔案(裁判評段時要看歷屆戰績)。
-- 欄位與 GetFencerByGameID 逐字相同,共用 signuppg.FencerFromRow。
--
-- 一次查一批而不是讓呼叫端在迴圈裡查:ListUnranked 一屆可能有數十人,
-- 逐筆查就是數十次 round-trip,而且那個迴圈看起來完全無害。
--
-- 查不到的 id 不會有列,adapter 因此**不**回錯誤 —— 呼叫端拿到的是一個
-- 少了那個鍵的 map,而它本來就要處理「這個人沒有跨屆資料」的情況。
SELECT id, public_id, game_id, user_id, discord_name,
       last_rank_level, last_ranked_at, tournaments_played, wins, losses,
       created_at, updated_at
FROM activity.fencers
WHERE id = ANY(sqlc.arg(fencer_ids)::bigint[]);
