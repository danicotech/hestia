-- +goose Up
-- QA(shop 審查)發現:退款路徑以 (reason, ref_type, ref_id) 找原分錄,
-- 只有 (reason, created_at) 可用會倒掃同 reason 分錄——且發生在持履約物列鎖的 tx 內。
-- 部分索引讓 ref 反查 O(log n),縮短動錢路徑的鎖持有時間。
CREATE INDEX token_entries_ref_idx
  ON platform.token_entries (ref_type, ref_id)
  WHERE ref_type IS NOT NULL;

-- +goose Down
DROP INDEX platform.token_entries_ref_idx;
