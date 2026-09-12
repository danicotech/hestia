# migrations

`goose`。同一個 Postgres,兩個 schema:

| 目錄 | schema | 版本表 | 內容 |
|---|---|---|---|
| `platform/` | `platform` | `platform.goose_db_version` | users、token_entries、idempotency_keys、outbox_events、rewards |
| `activity/` | `activity` | `activity.goose_db_version` | fencers、tournaments、tournament_players、matches、handicap_items、match_budgets、handicap_selections、votes、bets、bet_legs |

分開放不是為了現在,是為了之後 `internal/core/activity/` 抽成 themis 的那天 —— 到時候這個目錄整包跟著搬走。
**各自一張版本表**也是同一個理由:抽走之後兩邊是不同 binary 在跑,共用版本表會讓其中一邊看到另一邊的版本號而誤判。

`Apply()` 依序跑 platform 再跑 activity。順序不影響 DDL 能不能過(activity 對 platform 是弱參照,不設 FK),但「平台先於活動」是這個專案的依賴方向,寫死在程式裡比寫在註解裡可靠。

## 兩件不能忘的事

**金額一律 `BIGINT`。** 不要 float、不要 double、不要帶小數的 numeric。

**帳本只增不改。** `token_entries` 沒有 UPDATE、沒有 DELETE。餘額是同一個 transaction 裡更新的快取,對帳 job 負責驗證兩邊一致。
