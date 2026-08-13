# internal

分層優先,模組次之。完整規範見 [docs/專案結構.md](../docs/專案結構.md),這裡只放每天要記得的部分。

```text
shared/           基礎設施無關的工具。不認識「代幣」也不認識「賽事」
domain/           實體、狀態機、領域錯誤、port 介面。不 import 任何其他層
core/             用例與交易邊界。只認 domain 的 port
cron/             排程與背景工作。呼叫 core,不自己寫規則
infrastructure/   adapter,實作 domain 的 port
interface/        http / websocket / grpc
```

依賴方向:

```text
interface ──┐
            ├──▶ core ──▶ domain ◀── infrastructure
cron ───────┘
```

只有 `cmd/` 知道哪個實作接到哪個介面,那是 wire 的工作。

## 兩條會被 CI 擋的線

**一、`domain` 不 import 任何其他層。** 沒有 gin、沒有 pgx、沒有 huma。

**二、`platform/` 不得 import `activity/`。**

每一層底下都切了 `platform/` 與 `activity/`。反向可以,但只能透過 `domain/platform` 宣告的 port(例如 `Ledger`)。

`activity/` 是暫住的 —— 第二個活動要辦的時候,把每一層的 `activity/` 子套件一起搬到 [themis](https://github.com/danicotech/themis)。守住這條線的話那是搬檔案;破了就是重寫。這是整個專案唯一真正回不去的決定。

## 分不清 domain 還是 core 的時候

問:**這條規則沒有資料庫也成立嗎?**

- 「賠率上限是 12」→ `domain`
- 「扣款、寫帳本、建注單要在同一個交易裡」→ `core`
