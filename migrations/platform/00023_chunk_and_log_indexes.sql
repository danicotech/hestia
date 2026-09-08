-- +goose Up
-- schemas/12 的落地補強(索引與唯一約束,不新增欄位)。

-- chunk 去重的最後保證。應用層已有 advisory lock + 水位線 + NOT EXISTS 三層,
-- 但那些都在應用層——DB 層沒有任何東西擋得住「有人寫了第二套聚合程式」。
--
-- NULLS NOT DISTINCT 是必要的(PG15+):主頻道的 thread_id 是 NULL,
-- 普通 UNIQUE 對 NULL 不生效,等於最常見的那一半完全沒擋到。
CREATE UNIQUE INDEX conversation_chunks_segment_uq
  ON platform.conversation_chunks (space_id, channel_id, thread_id, started_at)
  NULLS NOT DISTINCT;

-- 聚合每輪按 (space, channel, 時間) 掃 message_logs,現有索引是 (user_id, created_at)。
-- 分區表上建索引會自動散到各分區。
CREATE INDEX message_logs_space_channel_idx
  ON platform.message_logs (space_id, channel_id, created_at);

-- 水位線查詢:每個 (space, channel, thread) 取最新的 ended_at。
CREATE INDEX conversation_chunks_watermark_idx
  ON platform.conversation_chunks (space_id, channel_id, thread_id, ended_at DESC);

-- +goose Down
DROP INDEX platform.conversation_chunks_watermark_idx;
DROP INDEX platform.message_logs_space_channel_idx;
DROP INDEX platform.conversation_chunks_segment_uq;
