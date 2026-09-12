-- +goose Up
-- 補 activity.handicap_items 的 public_id。00002 漏了這欄。
--
-- 為什麼不沿用 token_entries.entry_ref 那個例外:
--
-- 鐵則 5 的唯一例外(對外不露內部 BIGINT id)是為 token_entries 開的,理由很具體 ——
-- 它是整個系統最大的表、而且按月分區,為每一列背 26 字元 ULID 不划算,分區表也無法
-- 由 DB 保證全域唯一。
--
-- handicap_items 兩個條件都不符合:逐屆 34 列、不分區。既然沒有那個代價,就不該
-- 繼承那個例外 —— 例外一旦被當成先例引用,鐵則就開始鬆動。
--
-- 三步式(加欄位 → 補值 → 收緊)是為了讓這支在有資料的環境也能跑。
-- 現在表是空的,補值那步是 no-op。
ALTER TABLE activity.handicap_items ADD COLUMN public_id TEXT;

-- 補值只會在已有資料的環境生效。前綴標明來源,日後查得出這些不是正常產生的 ULID。
UPDATE activity.handicap_items SET public_id = 'LEGACY' || id::text WHERE public_id IS NULL;

ALTER TABLE activity.handicap_items
  ALTER COLUMN public_id SET NOT NULL,
  ADD CONSTRAINT handicap_items_public_id_key UNIQUE (public_id);

COMMENT ON COLUMN activity.handicap_items.public_id IS
  'ULID,對外唯一識別。proto 的 HandicapItem.public_id 與 SelectRequest.item_public_id 都指這個。';

-- +goose Down
ALTER TABLE activity.handicap_items
  DROP CONSTRAINT handicap_items_public_id_key,
  DROP COLUMN public_id;
