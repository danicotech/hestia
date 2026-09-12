-- 裁判動作 query(schemas/20,migrations/activity/00001)。
-- 呼叫端是 internal/core/activity/match 的 Repository[TX] 埠 —— 六個裁判動作
-- (開盤、封盤、開打、判勝負、棄賽、設直播)全部跑在該埠的同一個 tx 裡。
--
-- ── 沿用既有 query,不重寫(專案規則 9)──────────────────────
--
--   稽核    audit.sql 的 InsertAdminAudit → platform.admin_audit_logs 的唯一寫入位置。
--           本檔**沒有**任何 admin_audit_logs 的 INSERT:MarkMatchReady / MarkMatchLive /
--           MarkMatchFinished / SetMatchStreamURL / SetTournamentPlayerStatus 的稽核,
--           全由 adapter 在同一個 tx 裡緊接著呼叫 InsertAdminAudit 寫入。
--           埠的 RecordJudgeAction(封盤專用)也直接對應到那一支,沒有第二份。
--   outbox  ledger.sql 的 InsertOutboxEvent → 對應埠的 AppendEvents(adapter 逐筆呼叫,
--           與 bettingpg 同一個做法)。領域變更與事件同 tx 由呼叫端的 InTx 保證。
--   封盤    handicap 的 LockMatchHandicaps(activity_handicap.sql)是 matches 封盤
--           UPDATE 的唯一權威。本檔刻意沒有封盤敘述 —— 埠的 GetMatchForJudge
--           就是「讓武套件寫完之後回來重讀」的那一支。
--
-- ── 命名 ────────────────────────────────────────────────────────
--
-- 生成的 db package 與 platform / tournament / handicap / betting 的查詢同居一個
-- 命名空間,所以泛用名一律帶 Judge / Match / Player 前後綴。特別注意
-- LockMatchForHandicap / GetMatchForHandicap 已被 activity_handicap.sql 用掉,
-- 本檔用 *ForJudge 收尾 —— 兩者形狀不同(那邊要 per_rank_gap,這邊要雙方 fencer_id),
-- 不能也不該收斂成同一支。
--
-- ── 鎖序 ────────────────────────────────────────────────────────
--
-- activity_tournament.sql 的檔頭定了「賽事列 → tournament_players → fencers → matches」,
-- 並在第 3 條寫下「matches 只有裁判破壞性操作那一組會寫,所以構不成環」。
-- 本套件推翻了那個前提:它會寫 matches,而且兩個入口的取鎖方向天生相反 ——
--
--   ReportResult    matches(LockMatchForJudge)→ tournament_players(敗方標淘汰)→ fencers
--   WithdrawPlayer  tournament_players(LockPlayerForJudge)→ matches(未完賽場次)
--
-- 兩位裁判同時對同一場的兩位選手做這兩件事,就是一個真實可達的 40P01。
-- 解法與 activity_betting.sql 的 LockMatchForSettle 同一個形式:**每個裁判動作的
-- 第一步先取該屆的 advisory xact lock**,讓同屆的裁判動作彼此完全串行化,
-- 列鎖的方向就不再構成環。
--
--   1. LockTournamentForJudgingByMatch / ...ByPlayer   ← adapter 必須先呼叫,不可省
--   2. matches 列鎖(LockMatchForJudge / LockMatchAtForJudge / LockUnfinishedMatchesOfPlayer)
--      或 tournament_players 列鎖(LockPlayerForJudge)
--   3. JudgeTournamentByID 取賽事列的 FOR SHARE —— 把階段釘住,
--      整個 tx 期間 UpdateTournamentPhase 不會在腳下把 in_progress 換掉
--      (與 tournament.sql 的 LockTournamentShared 同一個機制,只是這裡順帶要讀 config)
--   4. tournament_players → fencers(SetTournamentPlayerStatus → BumpFencerRecord),
--      與報名 / 評段路徑同向
--   5. 跨 schema(派彩走 Ledger.ApplyInTx)時 activity 的鎖永遠先取、platform 後取
--
-- advisory lock 與抽籤那一組(LockTournamentExclusive)不互斥,但兩者階段互斥
-- (抽籤只在 drawing、裁判動作只在 in_progress,而 in_progress 退不回 drawing),
-- 所以不會同時在飛。第 3 步的 FOR SHARE 是這件事在 DB 側的保險。

-- ══ 動作入口:串行化 ════════════════════════════════════════════

-- name: LockTournamentForJudgingByMatch :exec
-- 裁判動作的第一步(以場次為入口:開盤、封盤、開打、判勝負、設直播)。
--
-- 為什麼是 advisory 而不是賽事列鎖:賽事列這一刻不該被改,取 FOR UPDATE 會把
-- 同屆的報名/評段(FOR SHARE)也一起擋掉,而那些跟裁判動作沒有任何共用資料;
-- 取 FOR SHARE 又互不排斥,擋不住上面那個 ReportResult × WithdrawPlayer 的環。
-- advisory 是純粹的互斥語意,隨 tx 結束自動釋放 —— 不需要也不該手動解鎖。
--
-- 型別:hashtext() 回 int4,走 pg_advisory_xact_lock(int4, int4) 這個簽名。
-- ::int 的範圍檢查是刻意的:tournament_id 真的超過 2^31 時當場 22003 失敗,
-- 而不是悄悄截斷成別屆的 key(那會讓兩屆賽事共用一把鎖,或更糟:漏鎖)。
--
-- 子查詢查無此場次時回 NULL,pg_advisory_xact_lock 是 strict 的 → 不取鎖、不報錯,
-- 接著的 LockMatchForJudge 回 0 列 → adapter 回 ErrMatchNotFound。
-- 這一步讀到的 tournament_id 不必是「鎖後真值」:matches.tournament_id 永不改變。
SELECT pg_advisory_xact_lock(
  hashtext('tournament_judge'),
  (SELECT m.tournament_id FROM activity.matches m
    WHERE m.public_id = sqlc.arg(match_public_id)::text)::int);

-- name: LockTournamentForJudgingByPlayer :exec
-- 同上,以選手為入口(棄賽)。棄賽會沿著晉級鏈判掉好幾場,取鎖方向與
-- 以場次為入口的動作相反 —— 這一句就是讓那件事不可能同時發生的東西。
SELECT pg_advisory_xact_lock(
  hashtext('tournament_judge'),
  (SELECT p.tournament_id FROM activity.tournament_players p
    WHERE p.public_id = sqlc.arg(player_public_id)::text)::int);

-- ══ 場次讀取 ════════════════════════════════════════════════════
--
-- 以下三支與五支 Mark*/Set*/Seat* 的回傳欄位**逐欄一致**(埠註解:寫入方法要回
-- 「與 LockMatch 同形狀的更新後列」)。形狀一致不是整齊,是必要的:
-- resolve 的晉級級聯會把 SeatMatchPlayer 的結果直接餵回判定路徑,
-- 少一欄就會在第二圈才炸,而且炸在一個與成因無關的地方。
--
-- 雙方一律 LEFT JOIN activity.tournament_players:p1/p2 可為 NULL(等上一輪),
-- INNER JOIN 會讓「對手未定的場次」整列查不到,而棄賽路徑正需要看到它們。
-- 因此鎖一律寫 FOR UPDATE OF m —— Postgres 禁止鎖外連接的可空側,
-- 裸 FOR UPDATE 會在執行期報錯(activity_handicap.sql 的 LockMatchForHandicap
-- 已經踩過並記錄這一坑)。
--
-- 刻意**不** JOIN activity.fencers:match.Player 需要的 fencer_id 就是
-- tournament_players.fencer_id 這一欄,多接一張表只是為了同一個值繞一圈。
-- 判勝負要寫的 fencers 列由 BumpFencerRecord 用這個 id 直接定址。

-- name: LockMatchForJudge :one
-- 整條生命週期的**列鎖序列化點**。裁判連點兩次「判定勝負」、或一邊開打一邊封盤,
-- 都在這裡排成序;與 handicap 的 LockMatchForHandicap 鎖的是同一列,
-- 所以「一邊買讓武一邊封盤」也一併被擋住。查無 0 列 → adapter 回 ErrMatchNotFound。
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
WHERE m.public_id = sqlc.arg(match_public_id)::text
FOR UPDATE OF m;

-- name: LockMatchAtForJudge :one
-- 依位置讀晉級目標並鎖住該列。取鎖順序恆為 round 遞增(晉級只會往後走),
-- 所以同一個 tx 內的多次呼叫不會與自己形成環。
-- tournament_id 一起進 WHERE 而不是只用 (round, slot):那兩欄只在一屆內唯一。
-- 查無 0 列 → adapter 回 ErrMatchNotFound,service 再轉成 ErrAdvanceTargetMissing。
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
  AND m.round = sqlc.arg(round)::int
  AND m.slot = sqlc.arg(slot)::int
FOR UPDATE OF m;

-- name: GetMatchForJudge :one
-- 無鎖重讀,欄位與 LockMatchForJudge 完全一致。
-- 只在本 tx 已經持有該列鎖之後使用:封盤那句 UPDATE 由 handicap 套件執行,
-- 回來要看得到它寫下的 status='locked' 與 handicap_locked_at。
-- 再加一次 FOR UPDATE 在同一個 tx 裡沒有任何效果,只會多一次寫 xmax 的代價。
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
WHERE m.public_id = sqlc.arg(match_public_id)::text;

-- name: GetMatchForJudgeByID :one
-- 以**內部 id** 無鎖重讀,欄位與 GetMatchForJudge 逐字相同。
--
-- 為什麼需要這一支:狀態轉移的參數只帶內部 id,而轉移影響 0 列時必須分得出
-- 「查無此場次」與「狀態已經被別人改了」—— 兩者對裁判的意思完全不同。
-- 沒有這支的話,adapter 得自己維護一份 tx 內的 id → public_id 對照表,
-- 那是把 DB 查得到的事實搬進記憶體再維護一次(規則 9)。
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
WHERE m.id = sqlc.arg(id)::bigint;

-- name: LockUnfinishedMatchesOfPlayer :many
-- 棄賽路徑:這位選手所有 status <> 'done' 的場次,依 (round, slot) 遞增。
--
-- 排序不是排版:棄賽要一場一場往下判(每判完一場,晉級鏈可能又生出下一場
-- 不戰而勝),順序不定會讓級聯行為不可重現 —— 同一筆棄賽兩次跑出不同的
-- WalkoverMatches,而那是要發 Discord 公告的東西。
-- 順帶也是防死鎖的:兩位選手同時棄賽時,兩個 tx 以同一個順序取列鎖
-- (LockRows 節點在 Sort 之上,鎖確實按排序後的順序取)。
--
-- 含對手未定的場次:呼叫端要看得到它們才判斷得出「這場現在還判不了」,
-- 留給晉級鏈接手(matches_winner_is_participant_check 也不允許現在就寫勝者)。
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
ORDER BY m.round, m.slot
FOR UPDATE OF m;

-- ══ 賽事 ════════════════════════════════════════════════════════

-- name: JudgeTournamentByID :one
-- 讀賽事,含 TotalRounds。查無 0 列 → adapter 回 tournament.ErrTournamentNotFound。
--
-- FOR SHARE 把階段釘住:RequirePhase(in_progress) 通過之後,整個 tx 期間
-- UpdateTournamentPhase 改不動這一列(它是 UPDATE,與 FOR SHARE 互斥)。
-- 沒有這把鎖,「檢查時是 in_progress、派彩時已經 finished」是一個真實的縫。
-- 與其他 FOR SHARE 持有者(報名、評段)彼此不互斥,所以不會拖慢任何東西。
--
-- total_rounds = MAX(matches.round),即 bracket.Shape 的輸入;沒有場次時 COALESCE 成 0
-- (service 據此回 ErrBracketMissing)。用純量子查詢而不是 JOIN + GROUP BY:
-- 外層一旦有聚合就不能加 FOR SHARE,而子查詢是另一個查詢層級,不受這條限制。
-- 走 matches_tournament_round_idx,只取索引的末端。
SELECT t.id, t.public_id, t.slug, t.name, t.phase, t.config,
       COALESCE((SELECT max(m.round) FROM activity.matches m
                  WHERE m.tournament_id = t.id), 0)::int AS total_rounds
FROM activity.tournaments t
WHERE t.id = sqlc.arg(tournament_id)::bigint
FOR SHARE;

-- ══ 選手 ════════════════════════════════════════════════════════
--
-- match.Player 的欄位全部落在 tournament_players 上(fencer_id 也是),
-- 所以這兩支不 JOIN 任何東西。與 activity_tournament.sql 的 GetPlayerByPublicID
-- 形狀不同(那邊帶 game_id 與整張報名表),不收斂成同一支 ——
-- 收斂的代價是每次判勝負都多讀一次 fencers 與六個報名欄位。
-- passcode_hash 一如全域慣例,這裡一欄都不碰。

-- name: LockPlayerForJudge :one
-- 棄賽路徑的入口,鎖住這位選手。查無 0 列 → adapter 回 ErrPlayerNotFound。
-- 不帶 tournament_id:棄賽的入口只有 player public_id(埠簽名),而 public_id
-- 全域唯一;越屆防護在動作入口的 advisory lock 與 RequirePhase 那一側。
SELECT id, public_id, tournament_id, fencer_id, display_name, rank_level, status
FROM activity.tournament_players
WHERE public_id = sqlc.arg(player_public_id)::text
FOR UPDATE;

-- name: SetTournamentPlayerStatus :one
-- 改參賽者狀態,回傳更新後的選手(形狀與 LockPlayerForJudge 一致)。
--
-- 刻意不加 AND status <> @status:eliminated → eliminated 在連鎖判定裡是可達的
-- 無害重複,回 0 列會讓 adapter 誤報「查無此選手」。真正要擋的重複棄賽,
-- 由 service 在 LockPlayerForJudge 之後判(ErrAlreadyWithdrawn)。
--
-- status 值域由 tournament_players_status_check 守(active/eliminated/withdrawn),
-- 傳錯值是 23514 而不是默默寫進去。
--
-- 稽核:**只有 withdrawn 要寫**(棄賽是裁判的動作;eliminated 是判勝負的後果,
-- 已被 MarkMatchFinished 那筆稽核涵蓋)。這條規則是確定的,adapter 照做即可,
-- 不需要在這裡多一個參數讓 SQL 去判斷。
UPDATE activity.tournament_players
SET status = sqlc.arg(status)::text,
    updated_at = now()
WHERE id = sqlc.arg(player_id)::bigint
RETURNING id, public_id, tournament_id, fencer_id, display_name, rank_level, status;

-- name: BumpFencerRecord :execrows
-- 跨屆戰績的**增量**更新(每判定一場 ±1),不是覆寫。
--
-- 口徑由 activity_tournament.sql 的 RecalcFencerStats 定死,這裡必須與它一致:
--   wins    winner_player_id 指到自己的場次數                    → 勝者 +1
--   losses  自己有上場、已分勝負、而勝者不是自己(walkover 也算)→ 敗者 +1
-- 所以不戰而勝一樣走這支,棄賽者照樣 losses +1 —— 戰績要誠實反映結果,
-- 生涯頁再用 result_kind 區分顯示。
--
-- 用增量而不是先讀後寫:增量在 SQL 裡是原子的,不需要為它多一道列鎖;
-- 真值隨時可用 RecalcFencerStats + ApplyFencerStats 對帳。
-- fencers_counts_check(wins >= 0 AND losses >= 0)讓寫歪的負 delta 當場被擋下來。
-- 鎖序 tournament_players → fencers:呼叫端必須先做完 SetTournamentPlayerStatus。
-- :execrows 讓 adapter 分得出「查無此 fencer」(0 列)。
UPDATE activity.fencers
SET wins = wins + sqlc.arg(wins_delta)::int,
    losses = losses + sqlc.arg(losses_delta)::int,
    updated_at = now()
WHERE id = sqlc.arg(fencer_id)::bigint;

-- ══ 場次狀態轉移 ════════════════════════════════════════════════
--
-- 五支寫入都是「UPDATE ... WHERE 舊狀態 → CTE 接回雙方快照」。
--
-- 為什麼狀態條件要進 WHERE:service 已經在列鎖下檢查過一次,這一層是**防併發**
-- 的第二道 —— 更精確地說,是讓「檢查」與「寫入」變成同一個原子動作。
-- 影響 0 列時 adapter 必須當成失敗(重讀一次分辨是查無此場次還是狀態已變),
-- 絕不可當成成功:默默成功的後果是開盤公告發第二次、或是一場沒封盤就開打。
--
-- 為什麼用 CTE 而不是 UPDATE ... RETURNING:RETURNING 看不到 join 進來的
-- 雙方快照,而埠要求回「與 LockMatch 同形狀的更新後列」。拆成 UPDATE + 再 SELECT
-- 兩次往返也能拼出來,但那是兩個語句 —— 中間那一瞬間別人看得到半成品。

-- name: MarkMatchReady :one
-- 開盤:pending → ready、handicap_open = true。
-- 0 列 = 這場不存在、或已經開過盤(ready/locked/live/done)→ ErrAlreadyOpen。
-- handicap_open 與 status 同一句寫完:分兩句的話,中間那一瞬間的
-- 「ready 但沒開盤」會被讓武的購買路徑讀成 ErrHandicapNotOpen。
WITH upd AS (
  UPDATE activity.matches
  SET status = 'ready',
      handicap_open = true,
      updated_at = now()
  WHERE id = sqlc.arg(match_id)::bigint
    AND status = 'pending'
  RETURNING *
)
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
FROM upd m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id;

-- name: MarkMatchLive :one
-- 開打:locked → live、started_at = now()。這一步同時關閉下注。
--
-- 只從 locked 進來(ready 不行):每一場都必須走過封盤,即使同段對決沒有任何讓武 ——
-- 封盤是「內容已公開」與下注視窗的唯一分界點。0 列 → ErrNotLocked。
--
-- 時間用 DB 的 now() 而不是參數:core 沒有第二個時鐘(與 bets.settled_at、
-- MarkOutboxRetry 同一個理由)。app/DB 時鐘偏移會讓 started_at 排在 finished_at 之後。
WITH upd AS (
  UPDATE activity.matches
  SET status = 'live',
      started_at = now(),
      updated_at = now()
  WHERE id = sqlc.arg(match_id)::bigint
    AND status = 'locked'
  RETURNING *
)
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
FROM upd m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id;

-- name: MarkMatchFinished :one
-- 判定勝負。winner_player_id + status='done' + finished_at + result_kind
-- **同一句 UPDATE 寫完**:分成兩句的話,中間那一瞬間的「done 但沒有勝者」
-- 會被 betting 的結算路徑讀成資料異常,而那條路徑正在同一個 tx 的下一行。
--
-- 守門是 status <> 'done' 而不是 status = 'live':正常判勝負確實只從 live 進來
-- (service 擋 ErrNotLive),但棄賽的不戰而勝會落在 pending / ready / locked 的場次上
-- ——「還沒開打就被判掉」正是 walkover 的定義。0 列 = 這場已完賽(或不存在),
-- 那是重複判定,必須失敗。
--
-- winner 必須是場上兩人之一:service 會先擋(ErrWinnerNotInMatch,給得出人話),
-- matches_winner_is_participant_check 是最後一道 —— 級聯判定寫錯側時當場 23514,
-- 而不是讓一位不在場上的人出現在賽果公告裡。
--
-- handicap_open 刻意不動:讓武的購買路徑本來就以 status = 'ready' 為閘門
-- (handicap/service.go),而 matches 的讓武欄位只能有一個權威(handicap 套件)。
WITH upd AS (
  UPDATE activity.matches
  SET winner_player_id = sqlc.arg(winner_player_id)::bigint,
      status = 'done',
      finished_at = now(),
      result_kind = sqlc.arg(result_kind)::text,
      updated_at = now()
  WHERE id = sqlc.arg(match_id)::bigint
    AND status <> 'done'
  RETURNING *
)
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
FROM upd m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id;

-- name: SetMatchStreamURL :one
-- 設定或清除直播連結。空字串經 NULLIF 存成 NULL ——「沒有連結」只能有一種
-- 表示法,兩種並存的話之後每個讀取端都要各判一次(與 InsertTournamentPlayer
-- 的幾個選填欄位同一個處理)。
--
-- 六個動作裡唯一沒有狀態閘門的一支:連結會變、會補、會打錯,賽後補 VOD 也是常態,
-- 所以 done 一樣可以設。0 列 = 這場不存在 → ErrMatchNotFound。
WITH upd AS (
  UPDATE activity.matches
  SET stream_url = NULLIF(sqlc.arg(stream_url)::text, ''),
      updated_at = now()
  WHERE id = sqlc.arg(match_id)::bigint
  RETURNING *
)
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
FROM upd m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id;

-- name: SeatMatchPlayer :one
-- 晉級:把勝者寫進目標場次的一側。is_p1 由 bracket.Advance 算出,不在 SQL 裡
-- 重推一次 slot/2 的映射 —— 那個映射的權威在 bracket 套件,第二份會長歪,
-- 而「晉級到錯的位置」是人工看對戰表看不出來的 bug。
--
-- 一支 query 用 CASE 處理兩側,而不是 SeatP1 / SeatP2 兩支:兩支之間唯一的差別
-- 就是這個布林,分開寫等於把回傳形狀再抄一遍。
--
-- 兩道守門:
--   status = 'pending'  目標場次還在等人。開盤(ready)的前提是雙方已確定,
--                       所以「有空位」與「非 pending」不可能同時成立;真同時成立
--                       代表對戰表壞了,那時候 0 列整筆失敗比寫進去安全。
--   該側 IS NULL        不覆寫已經坐著的人。輪空者在建樹時就被填進第二輪的
--                       某一側(bracket.Build 用的是與 Advance 同一個 feedsInto),
--                       所以合法的晉級永遠落在另一側 —— 撞上就是資料完整性失效。
-- 刻意不帶 actor:晉級是判定勝負的**後果**,不是獨立的裁判動作,理由已經在
-- MarkMatchFinished 那筆稽核裡。為後果各記一筆只會讓稽核表一半的列沒有資訊量。
WITH upd AS (
  UPDATE activity.matches
  SET p1_player_id = CASE WHEN sqlc.arg(is_p1)::boolean
                          THEN sqlc.arg(player_id)::bigint ELSE p1_player_id END,
      p2_player_id = CASE WHEN sqlc.arg(is_p1)::boolean
                          THEN p2_player_id ELSE sqlc.arg(player_id)::bigint END,
      updated_at = now()
  WHERE id = sqlc.arg(match_id)::bigint
    AND status = 'pending'
    AND CASE WHEN sqlc.arg(is_p1)::boolean
             THEN p1_player_id ELSE p2_player_id END IS NULL
  RETURNING *
)
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
FROM upd m
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id;

-- ══ 即時戰況推播 ════════════════════════════════════════════════

-- name: NotifyActivityWatch :exec
-- 把一則變化的「信封」送進 LISTEN / NOTIFY 頻道(internal/core/activity/watch)。
--
-- ── 為什麼扇出用 NOTIFY 而不是行程內的 hub ──────────────────────
--
-- **NOTIFY 只在交易 commit 時才送出,rollback 的交易一個字都不會送。**
-- 行程內 hub 給不了這個保證:服務得記得在 commit 之後才發布,而「在 commit 之前
-- 發布」是很容易寫出來的一行 —— 症狀是觀眾看到一個後來被 rollback 的賽果。
-- 交給 Postgres 之後,那個 bug 在結構上不可能發生。
--
-- 所以這一支的呼叫位置是固定的:**與領域變更同一個交易**,就寫在 AppendEvents
-- 寫 outbox 的旁邊(matchpg)。搬出去就失去上面那個唯一的理由。
--
-- ── 為什麼只送信封 ──────────────────────────────────────────────
--
-- NOTIFY 的 payload 上限是 8000 bytes,而封盤那則的讓武清單會超過 ——
-- 超過不是「被截斷」,是**整個交易失敗**,也就是裁判封不了盤。
-- 所以這裡只送 {賽事 slug, 種類, 參照 id},完整內容由 watchpg 收到後自己讀。
--
-- ── 為什麼頻道名是參數 ──────────────────────────────────────────
--
-- pg_notify(text, text) 收的是值不是識別字,所以頻道名可以由呼叫端帶。
-- 權威因此只有一個:internal/core/activity/watch 的 Channel 常數,LISTEN 那端
-- 也從它取。寫成 SQL 裡的字面值就會有兩份,而打錯一個字的症狀是
-- 「推播完全沒有反應,但每一支測試都過」。
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);
