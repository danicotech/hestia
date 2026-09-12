-- +goose Up
-- 00025 seed 的用途是照 schemas/22 寫的,但**漏了現在真正在發的四種公告**。
--
-- notification.discordChannels 目前的對應是:
--   daily.claimed      → daily
--   shop.purchased     → shop
--   market.sold        → market
--   redemption.handled → redemptions
--
-- 這四個 key 一個都不在 00025 的 seed 裡,所以即使設好了頻道,
-- 這四種公告仍然找不到落點。補上它們。
--
-- 保持細分而不是全部併進 announcements:簽到訊息量遠大於兌換核可,
-- 併在一起會讓低頻但重要的公告被洗掉。要合併的部署把兩個用途指向
-- 同一個頻道即可 —— 那是設定,不是結構。

INSERT INTO platform.channel_purposes (key, name, description) VALUES
  ('daily',       '每日簽到',   '有人完成每日簽到。量最大,建議獨立一個閒聊/機器人頻道'),
  ('shop',        '商店購買',   '有人在商店買了東西'),
  ('market',      '市集成交',   '玩家之間的交易成交'),
  ('redemptions', '兌換核可',   '實體獎品兌換被核可。量少但重要,不建議與簽到共用頻道')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM platform.channel_purposes
WHERE key IN ('daily', 'shop', 'market', 'redemptions');
