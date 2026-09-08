# proto —— 對外契約的唯一來源

ConnectRPC:**一套 proto 同時提供 gRPC、gRPC-Web 與 HTTP/JSON**(2026-09-14 決策)。
Go server stub 由 buf 生成到 `gen/`,未來的 TS client 也從同一份 proto 生成。
**禁止手寫共用型別**(專案鐵則 6)——想改契約就改 proto,然後重跑生成。

```
proto/hestia/platform/v1/
  common.proto          共用列舉與檢視型別(UserProfile / Balance / ShopItem / Entitlement / Redemption / LedgerAdjustment)
  auth.proto            AuthService        Discord OAuth 登入、換發、登出(handler 目前 unimplemented)
  me.proto              MeService          我的檔案 / 餘額(單一與全部)/ 權益 / 工單 / 改時區
  daily.proto           DailyService       每日簽到
  shop.proto            ShopService        商品列表 / 購買 / 自助退款 / 取消工單
  admin_economy.proto   AdminEconomyService 發點 / 扣點 / 萬能退款 / 處理工單 / 列出分錄
  activity.proto        ActivityService    Discord 活動記錄寫入(只收服務身分)
  notification.proto    NotificationService outbox → Discord 頻道的拉取/確認(只收服務身分)
```

## 重新生成(改完 proto 一定要跑)

`buf` 不需要安裝,兩個 protoc 插件走 `go.mod` 的 `tool` 指示詞(Go 1.24+),
所以任何一台裝了 Go 的機器跑出來的結果都一樣:

```bash
# 在 hestia/ 目錄下
go run github.com/bufbuild/buf/cmd/buf@v1.58.0 lint
go run github.com/bufbuild/buf/cmd/buf@v1.58.0 generate
```

生成結果(`gen/`)**要進版控**:CI 與其他 repo 不必為了編譯而先跑 buf,
而且 diff 會顯示契約變更的實際影響。

改契約前先確認沒有破壞既有 client:

```bash
go run github.com/bufbuild/buf/cmd/buf@v1.58.0 breaking --against '.git#branch=main'
```

## 前端契約(瀏覽器端 client 必讀)

登入流程把 OAuth state 綁在 HttpOnly cookie 上(擋 login CSRF / session fixation),
所以瀏覽器端的 Connect client **必須**設 `credentials: "include"`,
否則 `CompleteDiscordLogin` 永遠回 `permission_denied`。

```ts
const transport = createConnectTransport({
  baseUrl: "/",
  fetch: (input, init) => fetch(input, { ...init, credentials: "include" }),
});
```

## 契約鐵則(寫新 RPC 前讀這段)

1. **對外只出現 `public_id`**(ULID / TEXT),永不出現內部 BIGINT id。
   欄位命名 `xxx_public_id`,例外只有已知 schema 缺口(見 `admin_economy.proto` 的 `entry_ref`)。
2. **金額一律 `int64`。** protobuf 的 JSON 編碼會把 64 位整數寫成字串以保精度,
   這是刻意的;絕不用 float / double / decimal string。
3. **動錢的 RPC 一律有 `idempotency_key`**,唯二例外(簽到、工單狀態轉移)
   在訊息註解裡寫明它們的權威是 UNIQUE 約束 / 單向狀態機——一個概念一個權威。
4. 時間一律 `google.protobuf.Timestamp`,可空的用 `optional`。
5. 動錢與管理端的 RPC 要能「查得到才叫得動」:`Refund` 用 `entry_ref` 定址,
   所以必須有 `ListEntries` 產出它。加需要識別子的操作時,先確認識別子有來源。
6. 沒有 buf.lock:本模組不依賴任何 BSR 模組(只用內建的 well-known types),
   生成完全離線可重現。哪天真的加了外部依賴,`buf dep update` 會自己生出 buf.lock。

## 錯誤成因(`Hestia-Error-Reason`)—— 對外契約

每個錯誤除了 connect code,還帶一個**機器可讀且穩定**的 reason,放在錯誤的
metadata 裡(header 名 `Hestia-Error-Reason`)。三種協定都讀得到:
Connect 走 HTTP header,gRPC 與 gRPC-Web 走 trailer,connect-go 兩邊都會
併進 `connect.Error.Meta()`。

**為什麼要有**:code 太粗。`failed_precondition` 同時代表「這個 Discord 帳號
還沒綁定」「餘額不足」「超過限購」「退款窗口關了」,四種情況的處置完全不同
(引導登入 / 顯示餘額 / 顯示上限 / 告知已過期)。沒有 reason,呼叫端只能比對
**中文訊息**——那等於把人類可讀的文案變成 API 契約,我們改一次措辭它就靜靜地
退化成通用訊息。

**怎麼讀**(TypeScript / connect-es):

```ts
import { ConnectError } from "@connectrpc/connect";

try { await client.claim({}); }
catch (e) {
  const err = ConnectError.from(e);
  switch (err.metadata.get("hestia-error-reason")) {
    case "actor_not_linked":     return promptLink();      // 引導綁定
    case "insufficient_balance": return showBalance();
    case "daily_already_claimed":return showAlreadyClaimed();
    default:                     return showGeneric(err);  // 沒有 reason 也要能收
  }
}
```

三條使用規則:

1. **reason 是穩定識別字,訊息不是。** 依 reason 分支,永遠不要比對訊息文字。
2. **一定要有 default 分支。** 新增的錯誤會帶新的 reason,舊 client 不該因此壞掉。
3. **沒有 reason 是合法的。** 未映射的內部錯誤一律 `internal` + 固定文案且
   **不帶 reason**(帶了等於洩漏內部錯誤分類);參數檢查(`invalid_argument`)
   與未上線(`unimplemented`)也沒有 reason —— 它們的 code 已經說完了全部。

權威清單在 `internal/transport/errmap.go` 的 `errorCodes`(code 與 reason 同一列)。
下表是它的對外快照:

| reason | code | 意思 |
|---|---|---|
| `insufficient_balance` | failed_precondition | 餘額不足 |
| `idempotency_conflict` | aborted | 同一把冪等鍵配到不同的請求內容 |
| `idempotency_in_flight` | unavailable | 同一把冪等鍵正在處理中,稍後重試會拿到原結果 |
| `invalid_ledger_op` | invalid_argument | 帳本操作參數不合法 |
| `daily_already_claimed` | already_exists | 今天已經簽到過 |
| `timezone_change_cooldown` | failed_precondition | 剛改過時區,距上次簽到未滿冷卻 |
| `daily_ledger_state_conflict` | internal | 簽到與帳本兩個權威矛盾 |
| `daily_invalid_config` | internal | 簽到設定值有問題 |
| `shop_invalid_request` | invalid_argument | 商店請求參數不合法 |
| `user_not_found` | not_found | 使用者不存在或已註銷 |
| `item_not_found` | not_found | 商品不存在 |
| `entitlement_not_found` | not_found | 權益不存在 |
| `redemption_not_found` | not_found | 工單不存在 |
| `item_not_listed` | failed_precondition | 商品未上架 / 已下架 |
| `per_user_limit_reached` | failed_precondition | 已達每人限購 |
| `entitlement_already_revoked` | failed_precondition | 權益已撤銷 |
| `refund_window_closed` | failed_precondition | 自助退款窗口已關閉 |
| `redemption_not_pending` | failed_precondition | 工單已是終態,不能再轉移 |
| `not_entitlement_owner` | permission_denied | 這個權益不是你的 |
| `not_redemption_owner` | permission_denied | 這張工單不是你的 |
| `shop_ledger_state_conflict` | internal | 商店與帳本兩個權威矛盾 |
| `reason_required` | invalid_argument | 管理操作必須填事由 |
| `invalid_input` | invalid_argument | 管理端參數不合法 |
| `entry_not_found` | not_found | 找不到該筆分錄 |
| `entry_not_refundable` | failed_precondition | 這筆分錄不可退款 |
| `entry_already_refunded` | already_exists | 這筆分錄已退過 |
| `actor_not_linked` | failed_precondition | 這個 Discord 使用者還沒綁定平台帳號 → **引導綁定,不要重試** |
| `space_not_registered` | failed_precondition | 這個 guild / 頻道還沒註冊 |
| `activity_invalid_request` | invalid_argument | 活動記錄參數不合法 |
| `invalid_token` | unauthenticated | token 格式或簽章不對 |
| `token_expired` | unauthenticated | token 過期 |
| `session_not_found` | unauthenticated | session 不存在 |
| `session_revoked` | unauthenticated | session 已撤銷 |
| `session_expired` | unauthenticated | session 過期 |
| `token_reuse_detected` | unauthenticated | 偵測到 refresh token 重用(整串已撤銷) |
| `invalid_oauth_state` | invalid_argument | OAuth state 不合法 |
| `oauth_state_expired` | invalid_argument | OAuth state 過期 |
| `oauth_state_mismatch` | permission_denied | state 與瀏覽器 cookie 不符(login CSRF 訊號) |
| `invalid_redirect_uri` | invalid_argument | redirect_uri 不在允許清單 |
| `provider_exchange_failed` | unavailable | 與 Discord 交換憑證失敗,稍後重試 |
| `account_deleted` | permission_denied | 帳號已註銷 |
| `identity_invalid_config` | internal | 身分服務設定有問題 |
| `notification_invalid_request` | invalid_argument | 通知拉取/確認的參數不合法 |
| `mixed_credentials` | permission_denied | 同時帶了服務憑證與使用者憑證 |
| `service_on_user_rpc` | permission_denied | 這支使用者 RPC 不接受代打(契約上就不行,換憑證也沒用) |
| `delegation_not_granted` | permission_denied | 這支 RPC 可以代打,但**這把服務憑證**沒被授予 → 改部署設定的 scope |
| `acting_user_format` | invalid_argument | `X-Acting-User` 不是 `<provider>:<id>` 的形狀 |
| `acting_user_provider_unsupported` | invalid_argument | 形狀對,但 provider 不在白名單內(目前只有 `discord`) |
| `user_on_service_rpc` | permission_denied | 這支 RPC 只接受服務身分 |
| `acting_user_required` | invalid_argument | 缺少 `X-Acting-User` |
| `not_found` | not_found | 泛用的查無此物(入口層 port) |
| `unauthenticated` | unauthenticated | 沒有有效身分 |
| `permission_denied` | permission_denied | 身分有效但無權執行 |
| `canceled` | canceled | 呼叫端取消 |
| `deadline_exceeded` | deadline_exceeded | 逾時 |

改 reason 的值等同**破壞性變更**(呼叫端會依它分支)。新增錯誤請一併補這張表。
