-- +goose Up
-- schemas/28 落地:tournaments.config 從扁平的 v1 升成有 version 的 v2。
--
-- ── 為什麼要在 DB 裡升版,而不是只在程式讀取時補 ────────────────
--
-- 程式讀到 v1 會在記憶體裡補成 v2(schemas/28「從 version 1 升版」),那是讀取的
-- 相容性,**沒有副作用**。但「這屆用的是哪一套規則」必須在資料上有答案(與
-- MarshalConfig 寫完整一份而不是只寫差異是同一個理由):升版寫回一次,之後
-- 每一列都是 v2,讀取端的相容邏輯只是保險,不是常態路徑。
--
-- ── 第一屆的值是明確套的,不是 v1 的預設 ────────────────────────
--
-- 通用轉換給的是 **v1 的語意**(best_of 1、只有勝負盤、無季軍戰)—— 那是「升版前
-- 這屆實際跑的規則」。《百業試鋒》2026-09-13 grill 定案的是三局兩勝、四種盤口、
-- 季軍戰,那是**規則變更**,寫在第二句 UPDATE 裡並以 slug 指名,讓日後讀這支
-- migration 的人分得出「升版」與「改規則」是兩件事。

-- ── 通用升版(v1 → v2)────────────────────────────────────────
--
-- substring(... FROM '^-?[0-9]{1,18}$') 與 activity_betting.sql 的 BettingOddsConfig 同一道護欄:
-- config 是裁判手打的 JSONB,非數字的值不該讓整支 migration 失敗,而是退回預設。
-- jsonb_strip_nulls 把沒有來源值的鍵拿掉,讓程式的解析器用它自己的預設值 ——
-- item_max_qty 的 null 是有意義的(不限制),但「缺鍵」與「null」在解析器裡同義,所以可以一併拿掉。
UPDATE activity.tournaments
SET config = jsonb_strip_nulls(jsonb_build_object(
  'version', 2,
  'ranks',   config -> 'ranks',
  'bp',      jsonb_build_object(
               'kind', 'linear_gap',
               'per_rank_gap',
               COALESCE(substring(config ->> 'bp_per_rank_gap' FROM '^[0-9]{1,18}$'), '8')::bigint),
  'format',  jsonb_build_object(
               'best_of', 1,
               'preamble_every_round', true,
               'third_place_match', false),
  'handicap', jsonb_build_object(
               'item_max_qty', config -> 'handicap_item_max_qty',
               'draw_pools', jsonb_build_object('wuxue', '[]'::jsonb)),
  'timer',   jsonb_build_object('start', 'round_start'),
  'betting', jsonb_build_object(
               'enabled', true,
               'markets', '[{"kind": "match_winner"}]'::jsonb,
               -- v1 只要求 min_odds_milli > 0,v2 要求 >= 1000(賠率低於 1.0 沒有意義);
               -- 夾上去而不是原樣搬,不然一屆原本合法的 config 升版後會被 Validate 拒絕。
               'odds',    COALESCE(config -> 'odds', '{}'::jsonb)
                          || '{"kind": "vote_share"}'::jsonb
                          || CASE WHEN COALESCE(substring(config #>> '{odds,min_odds_milli}' FROM '^[0-9]{1,18}$'), '1050')::bigint < 1000
                               THEN '{"min_odds_milli": 1000}'::jsonb ELSE '{}'::jsonb END,
               'parlay',  '{"legs_per_match": 1}'::jsonb,
               'close_at', 'first_round_start'),
  'prizes',  config -> 'prizes'
)),
updated_at = now()
WHERE COALESCE(substring(config ->> 'version' FROM '^[0-9]{1,9}$'), '1')::int < 2;

-- ── 《百業試鋒》第一屆:2026-09-13 grill 定案的規則 ──────────────
--
-- 三局兩勝(Q20)、季軍戰(Q15)、四種盤口(Q16/Q17)、duration 線 90 秒(28 待確認 ②)。
-- draw_pools.wuxue 仍是空的:可選武學清單要御風羽提供,封盤前的驗證器會擋。
UPDATE activity.tournaments
SET config = config
  || jsonb_build_object('format', jsonb_build_object(
       'best_of', 3,
       'preamble_every_round', true,
       'third_place_match', true))
  || jsonb_build_object('betting', (config -> 'betting') || jsonb_build_object(
       'markets', '[
         {"kind": "match_winner"},
         {"kind": "round_winner"},
         {"kind": "duration", "line_seconds": 90},
         {"kind": "score"}
       ]'::jsonb)),
    updated_at = now()
WHERE slug = '2026-baiye-shifeng';

-- +goose Down
-- 降回 v1 的扁平形狀。v2 才有的鍵(format / timer / markets / parlay / close_at /
-- draw_pools)在 v1 裡沒有位置,會被丟掉 —— Down 的固有代價。
UPDATE activity.tournaments
SET config = jsonb_strip_nulls(jsonb_build_object(
  'bp_per_rank_gap', config -> 'bp' -> 'per_rank_gap',
  'ranks',           config -> 'ranks',
  'odds',            (config -> 'betting' -> 'odds') - 'kind',
  'prizes',          config -> 'prizes',
  'handicap_item_max_qty', config -> 'handicap' -> 'item_max_qty'
)),
updated_at = now()
WHERE COALESCE(substring(config ->> 'version' FROM '^[0-9]{1,9}$'), '1')::int >= 2;
