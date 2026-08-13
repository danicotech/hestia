# migrations

`goose`。同一個 Postgres,兩個 schema:

| 目錄 | schema | 內容 |
|---|---|---|
| `platform/` | `platform` | users、token_entries、idempotency_keys、outbox_events、rewards |
| `activity/` | `activity` | tournaments、matches、bets、votes、loadouts、shop |

分開放不是為了現在,是為了之後 `internal/activity/` 抽成 themis 的那天 —— 到時候這個目錄整包跟著搬走。

## 兩件不能忘的事

**金額一律 `BIGINT`。** 不要 float、不要 double、不要帶小數的 numeric。

**帳本只增不改。** `token_entries` 沒有 UPDATE、沒有 DELETE。餘額是同一個 transaction 裡更新的快取,對帳 job 負責驗證兩邊一致。
