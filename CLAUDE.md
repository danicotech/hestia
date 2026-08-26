# hestia

平台核心:身分、代幣帳本、通知管線、兌換。灶火永不熄 —— 這個 repo 跨活動長壽。

## 邊界

- `internal/core/platform` **不能** import `internal/core/activity`。反向只能透過 interface(如 `Ledger`)。
- M1 期間活動層暫住這裡,第二個活動時抽成 `themis`。**現在守住邊界,之後就只是搬資料夾。**
- 唯一持有 DB 憑證的服務。

## 技術選型(已定,不要換)

| 用途 | 選擇 |
|---|---|
| Driver | `pgx/v5` + `pgxpool` |
| Query | `sqlc`(寫真 SQL,生成型別安全 Go code) |
| Migration | `goose`,可 embed 進 binary |
| 測試 | `testcontainers-go`,對真的 Postgres 跑,不 mock |
| DB | PostgreSQL 為唯一真實來源;Redis(compose 內建,host:6380)只作快取/扇出/速率限制,**絕不放金額真值**(規劃書 §6)。Elastic/Mongo/RabbitMQ 不加 |

`migrations/platform/` 與 `migrations/activity/` 分開,對應兩個 schema。

## 帳本鐵則

1. **append-only** —— `token_entries` 永不 UPDATE / DELETE。
2. **退款是新的正數分錄**(`reason='refund'`,`ref_id` 指向原交易),不是改舊的。
3. **餘額是快取**(`user_balances`),同 transaction 更新,另有對帳 job 驗證 `SUM(entries) = balance`。
4. **動錢一律要 `Idempotency-Key`**。唯一例外是每日簽到 —— 它靠 `UNIQUE(user_id, claim_date)` 擋,比冪等鍵更可靠。
5. **鎖的順序固定**:多方交易時依 `user_id` 由小到大取鎖,避免死鎖。
6. **領域變更與 outbox 事件同一個 transaction**,消費用 `FOR UPDATE SKIP LOCKED`。

## 資料模型

[../notes/platform-schema-v1.md](../notes/platform-schema-v1.md) 是 DDL 草稿,決策理由在 [../notes/2026-08-24-會員與點數系統-grill.md](../notes/2026-08-24-會員與點數系統-grill.md)。

**schema 有數處刻意的反常規決策**,不要「修正」成標準做法:
- 用 `UNIQUE` 約束當防連點,不靠應用層檢查
- 餘額用獨立表而非 `users` 的欄位(多幣別時零 migration)
- 向量用 `halfvec`,且一年幾萬列時**不建 HNSW 索引**
- 衍生資料必須可重建(餘額←帳本、XP←事件、向量←原文)

## 相關 skill

Go 的 16 個 `golang-*` skill 已安裝。寫 DB 相關程式碼時 `golang-database` 最對症(transaction、isolation level、`SELECT FOR UPDATE`、pgx)。
