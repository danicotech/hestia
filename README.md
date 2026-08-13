# hestia

平台核心:身分、代幣帳本、通知管線、獎勵兌換。Go + PostgreSQL。

灶火永不熄 —— 這個 repo 裝的是**跨活動、不隨賽事替換**的東西。辦第幾場比賽都不該動到它,而這裡的資料會一直累積下去。玩家在夏季盃賺到的代幣,公會戰要能拿出來用,所以帳本必須住在這一層。

> **現況:骨架階段。** 目錄與模組邊界已經定案,產品程式碼還沒進來。
> 所有設計決策的來源是 [docs/架構規劃書.md](docs/架構規劃書.md),有衝突時以那份文件的較晚章節為準。

## 四個 repo 的分工

| Repo | 語言 | 生命週期 | 職責 |
|---|---|---|---|
| **hestia**(這裡) | Go | 長壽 | 身分、代幣帳本、通知管線、兌換、契約定義 |
| [stentor](https://github.com/danicotech/stentor) | TypeScript | 長壽 | Discord 互動閘道 |
| [themis](https://github.com/danicotech/themis) | Go | 逐活動 | 活動規則、狀態機、結算 |
| [theatron](https://github.com/danicotech/theatron) | TypeScript | 逐活動 | 活動前端 + 共用元件庫 |

切線是「長壽 vs 逐活動」,不是「後端 vs 前端」。

## 職責邊界

| 做 | 不做 |
|---|---|
| 帳號、登入、token 簽發 | 賽事規則、階段狀態機 |
| Discord 帳號綁定對應 | 賽程樹、抽籤、晉級 |
| 代幣帳本(append-only)與對帳 | 賠率、投票、注單 |
| Outbox 與整合派送(email / SMS / IG) | 任何 Discord 顯示格式 |
| 獎勵兌換 | 畫面 |
| 平台 API 與渲染契約的定義 | |

## 目錄

分層優先,模組次之。完整規範見 [docs/專案結構.md](docs/專案結構.md)。

```text
cmd/server/                進入點。wire 的 injector 住這裡
config/                    設定結構與載入
e2e/                       黑箱測試,不准 import internal/
internal/
├── shared/                error、logger、clock、id、冪等鍵。不認識任何領域概念
├── domain/                實體、狀態機、領域錯誤、port 介面。不 import 其他層
│   ├── platform/          identity / ledger / outbox / redeem
│   └── activity/          M1 暫住,第二個活動出現時抽成 themis
├── core/                  用例與交易邊界。只認 domain 的 port
│   ├── platform/
│   └── activity/
├── cron/                  對帳、outbox dispatcher、過期清理
├── infrastructure/        adapter,實作 domain 的 port
│   ├── storage/
│   │   ├── postgres/      pgxpool、sqlc 生成的查詢、repository、交易
│   │   └── redis/         快取、扇出、速率限制
│   ├── queue/             asynq
│   └── notifier/          email / sms / meta,同一個介面後面
└── interface/
    ├── http/{dto,handler,middleware,router}
    ├── websocket/         即時推播
    └── grpc/              目前沒有消費者,保留位置
contracts/
├── platform/              CI 從 Go 型別匯出的 OpenAPI(發布物)
└── render/                渲染契約,手寫 —— stentor 與 themis 共同認的顯示描述
migrations/{platform,activity}
deploy/                    compose 與部署設定
docs/
```

依賴方向只有一個:`interface`、`cron` → `core` → `domain` ← `infrastructure`。
只有 `cmd/` 知道哪個實作接到哪個介面,那是 wire 的工作。

## 三條不能破的線

**一、`platform/` 不能 import `activity/`。**
每一層底下都切了這兩個模組。反過來可以,而且只能透過 `domain/platform` 宣告的 port。repo 怎麼切都還能反悔,這條破了就回不去了 —— 之後抽 themis 時,是把每一層的 `activity/` 子套件一起搬走,不是重寫。

**二、資料庫憑證只有 hestia 有。**
stentor 和 theatron 的容器裡不該出現任何連線字串。這是實質的安全邊界:Bot 暴露在 Discord 那一側,它被打下來時不該連著資料庫。

**三、帳本是真實來源,餘額是快取。**
`token_entries` 只增不改,`users.token_balance` 在同一個 transaction 內更新,另外排對帳 job 驗 `SUM(entries) = balance`,不符就告警。所有會動到錢的 API 都必須收 `Idempotency-Key`。

## 資料庫

單一 Postgres,兩個 schema:`platform` 與 `activity`。**現在不拆成兩個資料庫**,理由只有一個 ——

> 下注 = 扣代幣(平台)+ 建注單(活動),這兩件事必須原子。

同一個 DB 就是一個 transaction 的事;拆開就要 saga 與補償交易。跨層存取一律走 in-process interface:

```go
type Ledger interface {
    Debit(ctx context.Context, userID int64, amount int64, idem string) error
}
```

之後真的要拆,把實作換成 HTTP client,呼叫端一行不用改。**該拆的時機是「要同時跑兩個活動」的那天**,在那之前拆是純成本。

## 契約

| 契約 | 住哪 | 誰依賴 | 誰是來源 |
|---|---|---|---|
| 平台 API | `contracts/platform/` | 其餘三個 | Go 的 dto 型別,由 huma 產生 |
| 渲染契約 | `contracts/render/` | stentor、themis | 手寫的 spec |
| 活動 API | `themis/contracts/<活動>/` | theatron | themis 的 Go 型別 |

**平台 API 是 code-first。** 改 Go 的 dto 就是改契約,CI 負責匯出 spec 進 `contracts/`,並在匯出結果與版控內容不一致時擋下來。TS 那側規則不變:禁止手寫 API 型別,只能用生成的 client。

**渲染契約是唯一手寫的一份**,因為它的消費者是 TypeScript,而 Go 型別只是它的實作之一 —— 這份契約必須先於任何實作存在。

放這裡是關鍵。渲染契約若放進 stentor,每加一個活動都要去改 Bot 的 repo,分 repo 的意義就沒了。

## 命名

代號只活在開發者之間,跨過那條線就回到直白:

- 環境變數用 `PLATFORM_DATABASE_URL`,**不是** `HESTIA_DATABASE_URL`。凌晨三點看 log 的人不該還要查神話辭典。
- 容器與映像可以叫 `hestia`。
- 錯誤訊息與使用者介面一律白話,代號絕不外露。
- Go module:`github.com/danicotech/hestia`。

## 預定的環境變數

| 變數 | 用途 |
|---|---|
| `PLATFORM_DATABASE_URL` | Postgres。**只有這個 repo 該有它** |
| `PLATFORM_REDIS_URL` | 快取、扇出、任務佇列 |
| `PLATFORM_HTTP_ADDR` | 監聽位址 |
| `PLATFORM_LOG_LEVEL` | |
| `PLATFORM_JWT_SIGNING_KEY` | access / refresh token 簽章 |
| `PLATFORM_ACCESS_TOKEN_TTL` | 短效,分鐘級 |
| `PLATFORM_REFRESH_TOKEN_TTL` | 長效,web 放 httpOnly cookie |
| `PLATFORM_SERVICE_TOKEN_BOT` | stentor 用它呼叫,並帶 `X-Acting-User` 代理使用者 |
| `PLATFORM_SMTP_URL` | 通知,尚未實作 |
| `PLATFORM_SMS_PROVIDER` | 通知,尚未實作 |
| `PLATFORM_META_APP_ID` / `PLATFORM_META_ACCESS_TOKEN` | IG / FB 發文,尚未實作 |

## 本機環境

`deploy/` 之後放 compose,M0 階段只需要兩個容器:Postgres 與 Redis,都掛 volume 與 healthcheck。應用程式先在容器外跑,省掉每次改動都要重建映像。

## 預定的工具鏈

| 用途 | 選擇 |
|---|---|
| HTTP router | `gin` |
| API 層 | `huma v2`(`humagin` adapter)—— OpenAPI 3.1、驗證、錯誤格式都由型別產生 |
| DI | `wire` —— 編譯期組裝,接錯線是編譯錯誤 |
| Driver | `pgx/v5` + `pgxpool` |
| Query | `sqlc`(寫真 SQL,拿型別安全的 Go code,不用 ORM) |
| Migration | `goose` |
| 任務佇列 | `asynq` |
| 測試 | `testcontainers-go`,對真的 Postgres 跑 |

明確避開:用 ORM 處理金流。代幣扣款和注單結算是全系統最容易出錯的地方,需要精準控制每一句 SQL、鎖的範圍和交易邊界。

## 進度

| 里程碑 | 內容 | 狀態 |
|---|---|---|
| M0 | 骨架、compose、CI 規則、領域文件 | 進行中 |
| M1 | 辦成第一場比賽(hestia + stentor 兩個 repo) | |
| 第二個活動 | `internal/activity/` 抽成 themis,前端抽成 theatron | |
