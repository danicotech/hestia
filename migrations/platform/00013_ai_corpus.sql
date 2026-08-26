-- +goose Up
-- 對話 chunk:不對單則訊息做 embedding,合併後才有檢索價值(grill Q16)
-- 只有 embedding 向量欄位延後(維度取決於嵌入模型選型,待決 #6):
--   選定後 ALTER TABLE ADD COLUMN embedding halfvec(N);
-- embed_model / embedded_at 現在就建 —— 換模型時要靠它們判斷哪些 chunk 需重算
-- M1 先累積 text,向量隨時可從原文重算(衍生資料可重建原則)
CREATE TABLE platform.conversation_chunks (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  space_id             BIGINT      NOT NULL REFERENCES platform.community_spaces(id),
  channel_id           TEXT        NOT NULL,
  thread_id            TEXT,
  started_at           TIMESTAMPTZ NOT NULL,
  ended_at             TIMESTAMPTZ NOT NULL,
  participant_user_ids BIGINT[]    NOT NULL,    -- 去識別化:只存內部 id,不存暱稱
  message_count        INT         NOT NULL,
  text                 TEXT        NOT NULL,
  embed_model          TEXT,                    -- NULL = 尚未嵌入
  embedded_at          TIMESTAMPTZ
);
CREATE INDEX ON platform.conversation_chunks (space_id, started_at DESC);
-- 不建 HNSW:一年幾萬列順序掃描夠快,等變慢再建(grill Q17)

-- +goose Down
DROP TABLE platform.conversation_chunks;
