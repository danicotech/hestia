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
