-- 讓武 BP query(schemas/20-activity-tournament.md,migrations/activity/00002 + 00004)。
--
-- BP 不走 Ledger、不需冪等鍵:它每輪重發、沒花完即作廢、不可交易、不可累積,
-- 四個性質沒一個符合貨幣(理由寫在 00002_handicap.sql 的檔頭)。因此這裡是
-- activity schema 內部的純 CRUD,沒有任何 token_entries 的影子。
--
-- 但「不是錢」不代表可以亂:BP 超支會直接毀掉一場比賽的公正性,所以這組查詢
-- 有兩道防線 ——
--   1. 寫入路徑一律先鎖場次列(FOR UPDATE OF m),以 matches 那一列為序列化點;
--   2. spent 是覆寫而非增量,權威永遠是 RecalcMatchBudgetSpent 的 SUM。
--
-- 命名一律 *Handicap* / *MatchBudget* 前綴:生成的 db package 與 platform 的查詢
-- 同居一個命名空間,GetItem / ListItems 這種名字撞得到。

-- ── 場次 ────────────────────────────────────────────────────────

-- name: LockMatchForHandicap :one
-- 整個 handicap package 的**序列化點**。
--
-- 選購、退選、封盤三條寫入路徑的第一步都是這一句,所以它們對同一場比賽彼此互斥:
-- 連點兩次買同一項不會同時通過餘額檢查(第二個請求會等到第一個 commit 之後才讀到
-- 新的 spent),一邊買一邊封盤也不會出現「檢查時沒封盤、寫入時已封盤」的縫隙。
--
-- FOR UPDATE OF m 是必要的寫法,不是精簡:
--   * p1/p2 走 LEFT JOIN(對手未定時為 NULL),Postgres 禁止對外連接的可空側加鎖;
--   * 賽事與選手列是共享讀的參考資料,鎖了只會讓不同場次互相卡住。
--
-- per_rank_gap 取自 tournaments.config;缺鍵時回 0,由 bp 套件退回預設值 8
-- (預設值的權威在 bp.DefaultPerRankGap,不在這裡再寫一份)。
SELECT
  m.id, m.public_id, m.tournament_id, m.status, m.handicap_open, m.handicap_locked_at,
  m.p1_player_id, m.p2_player_id,
  p1.public_id AS p1_player_public_id,
  p2.public_id AS p2_player_public_id,
  p1.rank_level AS p1_rank_level,
  p2.rank_level AS p2_rank_level,
  COALESCE(NULLIF(t.config ->> 'bp_per_rank_gap', ''), '0')::bigint AS per_rank_gap
FROM activity.matches m
JOIN activity.tournaments t ON t.id = m.tournament_id
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id
WHERE m.public_id = sqlc.arg(match_public_id)
FOR UPDATE OF m;

-- name: GetMatchForHandicap :one
-- 唯讀路徑(MyBudget / MatchHandicaps)的無鎖版本,欄位與 LockMatchForHandicap 完全一致。
--
-- 一次撈齊而不是分三次查,是因為讓武的每個判斷(狀態、段位差、逐屆設定)都同時用到
-- 這三者;分開查會讓它們來自不同的快照,出現「用舊段位算新預算」這種查不出來的錯。
SELECT
  m.id, m.public_id, m.tournament_id, m.status, m.handicap_open, m.handicap_locked_at,
  m.p1_player_id, m.p2_player_id,
  p1.public_id AS p1_player_public_id,
  p2.public_id AS p2_player_public_id,
  p1.rank_level AS p1_rank_level,
  p2.rank_level AS p2_rank_level,
  COALESCE(NULLIF(t.config ->> 'bp_per_rank_gap', ''), '0')::bigint AS per_rank_gap
FROM activity.matches m
JOIN activity.tournaments t ON t.id = m.tournament_id
LEFT JOIN activity.tournament_players p1 ON p1.id = m.p1_player_id
LEFT JOIN activity.tournament_players p2 ON p2.id = m.p2_player_id
WHERE m.public_id = sqlc.arg(match_public_id);

-- name: LockMatchHandicaps :execrows
-- 封盤。status 一起改成 'locked' 不是多管閒事,是 matches_locked_at_check 逼出來的:
-- handicap_locked_at 非 NULL 時 status 必須是 locked/live/done,只改時間戳會被 DB 擋掉。
--
-- handicap_locked_at IS NULL 是**封盤不可逆**的 DB 側保證:重複封盤影響 0 列,
-- 呼叫端據此回 ErrAlreadyLocked。默默成功會讓 Discord 公告每呼叫一次就再發一次。
UPDATE activity.matches
SET handicap_open = false,
    handicap_locked_at = sqlc.arg(locked_at),
    status = 'locked',
    updated_at = now()
WHERE id = sqlc.arg(match_id) AND handicap_locked_at IS NULL;

-- ── 讓武項目 ────────────────────────────────────────────────────

-- name: ListHandicapItems :many
-- 排序即前端的呈現順序:先分類,類內依 sort_order,最後以 id 收尾 ——
-- sort_order 允許重複,沒有第三個鍵的話同分項目的順序會隨執行計畫變動,
-- 選購頁每次重整就換一次位置。走 handicap_items_tournament_idx。
SELECT id, public_id, tournament_id, category, name, description, referee_note,
       cost, repeatable, sort_order
FROM activity.handicap_items
WHERE tournament_id = sqlc.arg(tournament_id)
ORDER BY category, sort_order, id;

-- name: GetHandicapItemByPublicID :one
-- tournament_id 一起進 WHERE 而不是查到再比對:項目逐屆一套、價格逐屆可調,
-- 拿上一屆的 public_id 買這一屆的場次必須是「找不到」,不是「找到但不給用」。
SELECT id, public_id, tournament_id, category, name, description, referee_note,
       cost, repeatable, sort_order
FROM activity.handicap_items
WHERE tournament_id = sqlc.arg(tournament_id) AND public_id = sqlc.arg(public_id);

-- name: InsertHandicapItems :one
-- 把種子定義一次實例化到某一屆,回**實際插入**的列數。
--
-- ON CONFLICT (tournament_id, category, name) DO NOTHING:重跑安裝回 0,不改價格。
-- 改價是裁判的明確動作,不該由「不小心重跑一次 seed」造成。
--
-- unnest 批次而不是迴圈單筆:34 次往返換一次,而且整批同一個敘述 ——
-- 中途失敗不會留下半套項目。public_id(ULID)由 adapter 產生後傳入,SQL 生不出 ULID。
--
-- NULLIF(..., '') 把空字串收斂成 NULL:text[] 參數的元素表達不了 NULL,
-- 而「還沒寫」的 referee_note 在 DB 裡就該是 NULL(seed 目前全為 null,待裁判補)。
--
-- 八個單引數 unnest 併排在子查詢裡,而不是 unnest(a, b, ...) AS v(...):
-- sqlc 的 catalog 只認單引數 unnest,多引數形式會編譯失敗。兩者語意相同 ——
-- select list 裡的多個集合回傳函數自 PG10 起同步展開(長度一致時逐列對齊)。
WITH inserted AS (
  INSERT INTO activity.handicap_items (
    tournament_id, public_id, category, name, description, referee_note,
    cost, repeatable, sort_order
  )
  SELECT
    sqlc.arg(tournament_id), v.public_id, v.category, v.name,
    NULLIF(v.description, ''), NULLIF(v.referee_note, ''),
    v.cost, v.repeatable, v.sort_order
  FROM (
    SELECT
      unnest(sqlc.arg(public_ids)::text[])     AS public_id,
      unnest(sqlc.arg(categories)::text[])     AS category,
      unnest(sqlc.arg(names)::text[])          AS name,
      unnest(sqlc.arg(descriptions)::text[])   AS description,
      unnest(sqlc.arg(referee_notes)::text[])  AS referee_note,
      unnest(sqlc.arg(costs)::bigint[])        AS cost,
      unnest(sqlc.arg(repeatables)::boolean[]) AS repeatable,
      unnest(sqlc.arg(sort_orders)::int[])     AS sort_order
  ) AS v
  ON CONFLICT (tournament_id, category, name) DO NOTHING
  RETURNING id
)
SELECT count(*)::bigint AS inserted FROM inserted;

-- ── BP 預算 ─────────────────────────────────────────────────────

-- name: GetMatchBudget :one
-- 沒有列 = 本場無讓武(同段對決)、或這個人是高段位方、或根本不是這場的選手。
-- 三者對呼叫端是同一件事,一律 ErrNoBudget —— 分得更細等於洩漏「這場的預算持有者
-- 不是你」以外的資訊。
--
-- JOIN tournament_players 只為了帶回 public_id:對外永不出現內部 BIGINT id(鐵則 5)。
SELECT b.match_id, b.player_id, p.public_id AS player_public_id, b.budget, b.spent
FROM activity.match_budgets b
JOIN activity.tournament_players p ON p.id = b.player_id
WHERE b.match_id = sqlc.arg(match_id) AND b.player_id = sqlc.arg(player_id);

-- name: FindMatchBudgetByMatch :one
-- 一場最多一列:只有低段位方會被建列(高段位方永不獲得反向補償),
-- 所以「這場的預算」等於「施加者的預算」。LIMIT 1 是對這個不變量的防守,
-- 不是在多列裡挑一列 —— 真的出現兩列代表發預算的路徑有 bug。
SELECT b.match_id, b.player_id, p.public_id AS player_public_id, b.budget, b.spent
FROM activity.match_budgets b
JOIN activity.tournament_players p ON p.id = b.player_id
WHERE b.match_id = sqlc.arg(match_id)
LIMIT 1;

-- name: InsertMatchBudget :one
-- 刻意**沒有** ON CONFLICT:複合主鍵 (match_id, player_id) 撞鍵時要讓 23505 冒上來,
-- adapter 轉成 ErrBudgetExists,呼叫端再去比對既有金額與當下段位差是否一致。
-- 吞掉衝突(DO NOTHING / DO UPDATE)會讓「抽籤後有人改了段位」變成默默覆蓋,
-- 而已經花掉的 spent 會對不上新的 budget。
--
-- 用 CTE 而不是單純 RETURNING:RETURNING 看不到 join 進來的 public_id,
-- 而呼叫端拿到的 Budget 必須是完整的(少一個欄位就得多一次往返)。
WITH ins AS (
  INSERT INTO activity.match_budgets (match_id, player_id, budget)
  VALUES (sqlc.arg(match_id), sqlc.arg(player_id), sqlc.arg(budget))
  RETURNING match_id, player_id, budget, spent
)
SELECT ins.match_id, ins.player_id, p.public_id AS player_public_id, ins.budget, ins.spent
FROM ins
JOIN activity.tournament_players p ON p.id = ins.player_id;

-- name: SetMatchBudgetSpent :execrows
-- **覆寫,不是增量**。權威值永遠是 RecalcMatchBudgetSpent 的 SUM;
-- 用 spent = spent + cost 的話,任何一次漏算都會永遠留在資料裡,而且無法事後分辨。
--
-- match_budgets_spent_check(0 <= spent <= budget)是超支的最後一道保險:
-- 應用層已經擋過,但呼叫順序一旦有 bug,DB 會當場擋下來,而不是讓比賽帶著錯的 BP 開打。
-- :execrows 讓 adapter 分得出「預算列不存在」(0 列)與「寫成功」。
UPDATE activity.match_budgets
SET spent = sqlc.arg(spent),
    updated_at = now()
WHERE match_id = sqlc.arg(match_id) AND player_id = sqlc.arg(player_id);

-- ── 讓武選擇 ────────────────────────────────────────────────────

-- name: ListHandicapSelections :many
-- 只回**未作廢**的:退掉的項目對餘額與公開清單都不存在,
-- 讓呼叫端每次自己補 WHERE NOT voided 遲早會漏一處。
--
-- 依 created_at 排序 = 購買順序,同時以 id 收尾:created_at 不唯一,
-- 同一次連點會落在同一個微秒,沒有第二個鍵清單順序就不穩定。
--
-- JOIN handicap_items 帶回名稱與分類快照,前端顯示一張清單不必逐項回查;
-- 但 cost 一律取 s.cost(購買當下的價格快照),項目改價不影響已成立的選擇。
-- 順帶帶回 match 的 public_id,免得同一個 Selection 型別在不同查詢路徑有不同的完整度。
SELECT s.id, s.public_id, s.match_id, m.public_id AS match_public_id,
       s.player_id, s.item_id,
       i.public_id AS item_ref, i.name AS item_name, i.category,
       s.cost, s.target_note, s.voided, s.created_at
FROM activity.handicap_selections s
JOIN activity.handicap_items i ON i.id = s.item_id
JOIN activity.matches m ON m.id = s.match_id
WHERE s.match_id = sqlc.arg(match_id) AND s.player_id = sqlc.arg(player_id) AND NOT s.voided
ORDER BY s.created_at, s.id;

-- name: GetHandicapSelection :one
-- 與 ListHandicapSelections 相反,這裡**含已作廢者**,voided 原樣回傳:
-- 退選路徑必須分得出「已經退過」(ErrSelectionAlreadyVoided)與「不存在」
-- (ErrSelectionNotFound)。過濾掉作廢列會讓重複退選看起來像查無此筆。
--
-- match_public_id 是退選路徑的必要欄位:呼叫端拿它去 LockMatchForHandicap,
-- 也就是「從一筆選擇找到它所屬的序列化點」。
SELECT s.id, s.public_id, s.match_id, m.public_id AS match_public_id,
       s.player_id, s.item_id,
       i.public_id AS item_ref, i.name AS item_name, i.category,
       s.cost, s.target_note, s.voided, s.created_at
FROM activity.handicap_selections s
JOIN activity.handicap_items i ON i.id = s.item_id
JOIN activity.matches m ON m.id = s.match_id
WHERE s.public_id = sqlc.arg(public_id);

-- name: InsertHandicapSelection :one
-- 每買一次一列,同一項目買三次就是三列 —— 不是一列 qty=3。
-- handicap_selections 刻意沒有 UNIQUE (match_id, player_id, item_id),所以這裡
-- **不能**加 ON CONFLICT:重複購買同一項目是設計(09-12 定案),不是要吞掉的衝突。
--
-- cost 由呼叫端傳入而不是就地 SELECT i.cost:價格快照要與餘額檢查看到的是同一個值,
-- 這裡再查一次等於在檢查與寫入之間開一道改價的縫。
-- 外鍵 handicap_selections_budget_fkey 指向 match_budgets 的複合鍵 ——
-- 沒有預算的人連一列都插不進來,這是應用層檢查之外的第二道保險。
--
-- CTE + JOIN 回填項目與場次快照,一次往返就給出完整的 Selection。
WITH ins AS (
  INSERT INTO activity.handicap_selections (
    public_id, match_id, player_id, item_id, cost, target_note
  )
  VALUES (
    sqlc.arg(public_id), sqlc.arg(match_id), sqlc.arg(player_id), sqlc.arg(item_id),
    sqlc.arg(cost), NULLIF(sqlc.arg(target_note)::text, '')
  )
  RETURNING id, public_id, match_id, player_id, item_id, cost, target_note, voided, created_at
)
SELECT s.id, s.public_id, s.match_id, m.public_id AS match_public_id,
       s.player_id, s.item_id,
       i.public_id AS item_ref, i.name AS item_name, i.category,
       s.cost, s.target_note, s.voided, s.created_at
FROM ins s
JOIN activity.handicap_items i ON i.id = s.item_id
JOIN activity.matches m ON m.id = s.match_id;

-- name: MarkHandicapSelectionVoided :execrows
-- voided 之外一律不動:退費只是 BP 內部的事(標記 + 覆寫 spent),不經 Ledger,
-- 也不刪列 —— 買過又退掉是裁判事後要看得到的事實。
--
-- AND NOT voided 讓重複退選影響 0 列。呼叫端已在場次鎖下檢查過一次,
-- 這條是同一個判斷在 DB 側的備份,不是它的替代品。
UPDATE activity.handicap_selections
SET voided = true
WHERE id = sqlc.arg(id) AND NOT voided;
