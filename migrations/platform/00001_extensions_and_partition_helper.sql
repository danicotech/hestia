-- +goose Up
-- 執行期的 schema/extension 前置由 migrations.Apply() 負責(goose 版本表依賴 platform
-- schema 先存在,migration 自己來不及建)。下面兩行「宣告」是給 sqlc 的解析器認識
-- schema 用的,執行期因 IF NOT EXISTS 而為 no-op —— 職責不同,不是重複邏輯。
CREATE SCHEMA IF NOT EXISTS platform;
CREATE EXTENSION IF NOT EXISTS vector;

-- ══ 分區輔助 ══
-- 大量成長的表按月分區,到期 DROP PARTITION(grill Q17)。
-- 每張分區表都有 *_default 分區當安全網:漏建月分區時寫入不失敗。
-- 代價:若 DEFAULT 裡已有某月的資料,直接 CREATE PARTITION 會被 PG 拒絕。
-- 因此本函式在建立分區前,先把 DEFAULT 中屬於該月的列搬出來,再 ATTACH —— 自我修復,不會永久卡死。

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform.create_month_partition(parent regclass, month date)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  start_d      date := date_trunc('month', month)::date;
  end_d        date := (date_trunc('month', month) + interval '1 month')::date;
  part_name    text := parent::text || '_' || to_char(start_d, 'YYYY_MM');
  default_name text := parent::text || '_default';
  part_col     name;
  has_default  boolean;
BEGIN
  IF to_regclass(part_name) IS NOT NULL THEN
    RETURN;  -- 冪等:已存在就跳過
  END IF;

  -- 取得分區鍵欄位名(本專案皆為單一欄位的 RANGE 分區)
  SELECT a.attname INTO part_col
  FROM pg_partitioned_table pt
  JOIN pg_attribute a ON a.attrelid = pt.partrelid AND a.attnum = ANY (pt.partattrs)
  WHERE pt.partrelid = parent;

  has_default := to_regclass(default_name) IS NOT NULL;

  -- 鎖序必須與寫入路徑同向(父表 → 分區):INSERT 先鎖父表再鎖路由分區,
  -- 這裡若先鎖 default 再要父表鎖,撞上高峰期寫入會互相死鎖(QA 實測指出)。
  -- 先取得父表 AE 鎖後,新寫入無法開始,搬移期間 default 天然靜止。
  EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', parent::text);

  IF has_default THEN
    -- 把 DEFAULT 中屬於本月的迷途列搬到暫存表。
    -- ⚠ append-only 例外聲明:這句 DELETE 動到 token_entries 的 default 分區,
    -- 但它是「同 tx 分區搬移」—— 列以原 id 原內容經父表插回新分區(OVERRIDING SYSTEM VALUE),
    -- 任何時點 SUM(amount) 不變、無列消失。這是唯一被授權的例外,僅限本函式。
    EXECUTE 'DROP TABLE IF EXISTS pg_temp.part_move_buf';
    EXECUTE format(
      'CREATE TEMP TABLE part_move_buf AS
         WITH moved AS (
           DELETE FROM %s WHERE %I >= %L AND %I < %L RETURNING *
         ) SELECT * FROM moved',
      default_name, part_col, start_d, part_col, end_d);
  END IF;

  EXECUTE format(
    'CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
    part_name, parent, start_d, end_d);

  IF has_default THEN
    -- 經由父表插回 → 自動路由進新分區;OVERRIDING 保留原 id
    EXECUTE format(
      'INSERT INTO %s OVERRIDING SYSTEM VALUE SELECT * FROM pg_temp.part_move_buf',
      parent);
    EXECUTE 'DROP TABLE IF EXISTS pg_temp.part_move_buf';
  END IF;
END $$;
-- +goose StatementEnd

-- 排程器唯一需要呼叫的入口:每天一次,為 platform schema 內所有分區表補齊本月與下月分區。
-- 表清單動態取自 pg_partitioned_table,新增分區表不用改這裡(避免重複維護清單)。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform.ensure_month_partitions()
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  t regclass;
BEGIN
  FOR t IN
    SELECT pt.partrelid::regclass
    FROM pg_partitioned_table pt
    JOIN pg_class c ON c.oid = pt.partrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'platform'
  LOOP
    PERFORM platform.create_month_partition(t, now()::date);
    PERFORM platform.create_month_partition(t, (now() + interval '1 month')::date);
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS platform.ensure_month_partitions();
DROP FUNCTION IF EXISTS platform.create_month_partition(regclass, date);
