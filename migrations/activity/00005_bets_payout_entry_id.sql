-- +goose Up
-- 補 activity.bets 的派彩分錄 id。00003 有扣款與退款,獨缺派彩。
--
-- 為什麼補這一欄:
--
-- 稽核鏈的用途是「拿著注單就能跳到帳本分錄」。原本三種金流裡有兩種做得到
-- (bet_stake、bet_refund),派彩卻只能反過來從 token_entries 用
-- ref_type='bet' + ref_id 回查。
--
-- 那個不對稱在對帳時最傷:派彩是三者中金額最大、也最可能被質疑的一筆
-- (「我明明中了為什麼沒收到」),偏偏它是唯一需要反向查的。
--
-- 空間代價是每張注單多 8 bytes;注單量級遠小於 token_entries,不值得為此省。
ALTER TABLE activity.bets ADD COLUMN ledger_payout_entry_id BIGINT;

COMMENT ON COLUMN activity.bets.ledger_payout_entry_id IS
  '派彩分錄 id。與 ledger_stake_entry_id / ledger_refund_entry_id 構成完整稽核鏈:'
  '三種金流都能從注單直接定址到帳本分錄,不必反查。'
  '不設 FK —— token_entries 是按月分區表,無法由 DB 保證全域唯一(同 00003 的理由)。';

-- +goose Down
ALTER TABLE activity.bets DROP COLUMN ledger_payout_entry_id;
