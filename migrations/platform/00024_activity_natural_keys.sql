-- +goose Up
-- 活動記錄的天然鍵去重(schemas/10、11 的落地補強,不新增欄位)。
--
-- 為什麼要:Discord 會重送事件,而這三張表原本沒有任何唯一性保證。
-- 應用層目前用 advisory lock + 先查再寫達到等價效果,但那條防線只保護
-- 「走服務程式碼的路徑」——未來的 backfill 腳本、別的服務、或人工修資料
-- 都繞得過去。冪等這種性質應該由 DB 承擔(同帳本的 idempotency_keys 邏輯)。
--
-- 分區表的 UNIQUE 必須包含分區鍵:voice_sessions 的 joined_at 與
-- presence_spans 的 started_at 都已在鍵內,不需額外處理。

-- 同一人、同一頻道、同一進入時刻只會有一段
CREATE UNIQUE INDEX voice_sessions_natural_uq
  ON platform.voice_sessions (user_id, space_id, channel_id, joined_at);

-- 同一人、同一空間、同一起始時刻只會有一段
CREATE UNIQUE INDEX presence_spans_natural_uq
  ON platform.presence_spans (user_id, space_id, started_at);

-- 同一人對同一則訊息的同一個表情只有一列(按/取消是改 added_at / removed_at,
-- 不是插新列——否則「按了取消再按」會變成無限刷參與度)
CREATE UNIQUE INDEX reaction_events_natural_uq
  ON platform.reaction_events (message_id, user_id, emoji);

-- +goose Down
DROP INDEX platform.reaction_events_natural_uq;
DROP INDEX platform.presence_spans_natural_uq;
DROP INDEX platform.voice_sessions_natural_uq;
