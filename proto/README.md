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

hestia/activity/v1/(活動層 ——《百業試鋒》賽事系統,schemas/20、21、26):
  common.proto          共用列舉與檢視型別(Rank / TournamentPhase / MatchStatus / HandicapCategory
                        / Tournament / Player / Match / BracketRound / HandicapItem / BpBudget)
  signup.proto          SignupService      報名、通行碼登入、綁定平台帳號
  tournament.proto      TournamentService  賽事、段位、選手、對戰表、場次(匿名可讀)
  handicap.proto        HandicapService    讓武項目、BP 預算、選購與退選、封盤後公開
  betting.proto         BettingService     投票驅動賠率、下注(單場與串關)、我的注單
  judge.proto           JudgeService       裁判後台:開賽事、評段、抽籤、開/封盤、賽果、棄賽、發獎

  **這個 package 刻意不 import hestia.platform.v1。** 活動層可以引用平台層的概念,
  平台層永遠不知道活動層存在(鐵則 1)。需要平台身分時只帶 user_public_id 字串,
  由伺服器自己解 —— 不讓 proto 變成偷渡這條界線的後門。

  注意 platform/v1/activity.proto 是**同名不同義**:那是 stentor 回報語音與訊息的
  「活動記錄」,與這裡的賽事活動層無關。

hestia/render/v1/(給閘道與活動服務的呈現契約,不是平台 API):
  render.proto          ActivityInteractionService  活動服務回傳「要顯示什麼」的描述
                        + Announcement(公告的渲染形式)
                        原規劃寫在 contracts/render/ 手寫 spec,改成 proto 是因為
                        鐵則 6:手寫共用型別在 Go 與 TS 兩端遲早漂移。
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
| `play_not_found` | not_found | 箱子 / 抽獎 / 寵物不存在 |
| `play_disabled` | failed_precondition | 這個項目目前停用 |
| `play_daily_limit` | resource_exhausted | 今天的抽籤次數用完了(三個賭博管道共用計數) |
| `play_pool_empty` | failed_precondition | 有限獎池已經抽完 |
| `play_already_entered` | already_exists | 這場抽獎已經報名過 |
| `play_closed` | failed_precondition | 抽獎已結束或已開獎 |
| `play_not_owner` | permission_denied | 這不是你的寵物 |
| `play_invalid_params` | invalid_argument | 遊戲選項 / 投注額 / 暱稱不合法 |
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
| `oauth_denied` | permission_denied | 使用者在 Discord 授權頁按了取消 → **不是錯誤**,顯示「你取消了登入」 |
| `oauth_callback_invalid` | invalid_argument | 回呼缺 `code` 或 `state`(不是我們發出去的那個流程) |
| `login_failed` | internal | 登入失敗且成因不對外分類(未映射錯誤在導回網址上的統一代表) |
| `notification_invalid_request` | invalid_argument | 通知拉取/確認的參數不合法 |
| `mixed_credentials` | permission_denied | 同時帶了服務憑證與使用者憑證 |
| `service_on_user_rpc` | permission_denied | 這支使用者 RPC 不接受代打(契約上就不行,換憑證也沒用) |
| `delegation_not_granted` | permission_denied | 這支 RPC 可以代打,但**這把服務憑證**沒被授予 → 改部署設定的 scope |
| `acting_user_format` | invalid_argument | `X-Acting-User` 不是 `<provider>:<id>` 的形狀 |
| `acting_user_provider_unsupported` | invalid_argument | 形狀對,但 provider 不在白名單內(目前只有 `discord`) |
| `user_on_service_rpc` | permission_denied | 這支 RPC 只接受服務身分 |
| `acting_user_required` | invalid_argument | 缺少 `X-Acting-User` |
| `activity_invalid_credentials` | unauthenticated | 遊戲ID 或通行碼不正確 → **不區分哪個錯**,避免被拿來探測誰報名了 |
| `activity_already_registered` | already_exists | 這個遊戲ID 已報名本屆 |
| `activity_game_id_required` | invalid_argument | 遊戲ID 必填 |
| `activity_invalid_game_id` | invalid_argument | 遊戲ID 格式不正確 |
| `activity_discord_name_required` | invalid_argument | 必須填寫 Discord 名稱 |
| `activity_field_too_long` | invalid_argument | 某個報名欄位超長 |
| `activity_player_already_bound` | failed_precondition | 這位選手已綁定**其他**平台帳號 → 要改綁請找裁判 |
| `activity_user_already_bound` | failed_precondition | 這個平台帳號已綁在另一位選手身上 |
| `activity_platform_account_required` | unauthenticated | 綁定(領獎的前置)需要平台帳號 → **引導登入** |
| `tournament_invalid_request` | invalid_argument | 建立賽事的請求少了必填欄位或帶了不可能成立的值 |
| `tournament_invalid_config` | invalid_argument | 賽事規則旋鈕不合法(BP 級距、賠率、獎金) → 修正該欄位再送 |
| `tournament_invalid_slug` | invalid_argument | 賽事代號格式不合法 → 只能用小寫英數與單一連字號,3–64 字 |
| `tournament_slug_taken` | already_exists | 賽事代號已被使用 → **換一個**,不是格式問題 |
| `community_not_found` | not_found | `community_public_id` 查無此社群 |
| `tournament_not_found` | not_found | 賽事不存在 |
| `activity_player_not_found` | not_found | 這屆賽事裡查無此選手 |
| `tournament_phase_conflict` | aborted | 階段已被其他裁判改變(樂觀鎖)→ **重讀後再決定**,不要盲目重試 |
| `tournament_wrong_phase` | failed_precondition | 目前賽事階段不允許這個操作 |
| `tournament_illegal_transition` | failed_precondition | 賽事階段轉換不合法(不能跳階段) |
| `tournament_ranks_locked` | failed_precondition | 抽籤後不可再改段位,要改請先退回段位公布階段 |
| `tournament_players_unranked` | failed_precondition | 尚有選手未評段,不能進入抽籤 |
| `activity_invalid_rank` | invalid_argument | 段位不合法(只有 1–4) |
| `activity_actor_required` | invalid_argument | 這個操作必須指明執行的裁判(要進 `admin_audit_logs`) |
| `tournament_invalid_seed` | invalid_argument | 抽籤種子字串解析失敗 |
| `tournament_no_drawable_players` | failed_precondition | 沒有可抽籤的選手 |
| `tournament_player_not_seeded` | failed_precondition | 這位選手尚未抽到籤位 |
| `tournament_same_player` | invalid_argument | 交換籤位需要兩位不同的選手 |
| `handicap_invalid_request` | invalid_argument | 讓武選購參數不合法 |
| `insufficient_bp` | failed_precondition | BP 不足以買下這個讓武項目 |
| `handicap_no_budget` | failed_precondition | 本場沒有讓武預算(同段或高段方本來就沒有) |
| `handicap_locked` | failed_precondition | 已封盤,讓武不可再更動 |
| `handicap_already_locked` | failed_precondition | 重複封盤 —— 封盤不可逆,所以要出聲而不是默默成功 |
| `handicap_wrong_phase` | failed_precondition | 這場不在讓武選購階段(pending / live / done) |
| `handicap_not_open` | failed_precondition | 階段對了,但裁判還沒按開盤 → **等公告**,與上一個的下一步不同 |
| `handicap_target_note_required` | invalid_argument | 此項目必須填寫指定內容(如「指定對手武學:XX」) |
| `handicap_not_selection_owner` | permission_denied | 這筆讓武選擇不是你的 |
| `handicap_item_not_found` | not_found | 讓武項目不存在 |
| `handicap_selection_not_found` | not_found | 讓武選擇不存在 |
| `handicap_selection_already_voided` | already_exists | 這筆讓武選擇已經退掉了 |
| `handicap_budget_inconsistent` | internal | 已花費金額與選擇總和對不起來(不變量壞了) |
| `bet_invalid_request` | invalid_argument | 下注 / 投票參數不合法 |
| `bet_self_bet` | permission_denied | 選手不得對自己參與的場次下注(別場可以) |
| `bet_closed` | failed_precondition | 這場已封盤,不能再下注 |
| `vote_closed` | failed_precondition | 這場已不開放投票 |
| `bet_odds_moved` | aborted | 賠率已變動 → **重新取賠率後讓使用者再確認一次**,不要靜默重送 |
| `bet_stake_too_large` | failed_precondition | 超過單筆下注上限 |
| `bet_duplicate_leg` | invalid_argument | 同一注單不可重複押同一場 |
| `bet_too_many_legs` | invalid_argument | 串關腿數過多 |
| `bet_match_not_in_tournament` | invalid_argument | 這場不屬於這屆賽事 |
| `bet_match_not_decided` | failed_precondition | 場次尚未分出勝負,不能結算 |
| `bet_walkover_match` | failed_precondition | 不戰而勝的場次要走退款路徑,不是正常結算 |
| `bet_not_walkover` | failed_precondition | 這場不是不戰而勝,不該走退款路徑 |
| `bet_ledger_state_conflict` | internal | 注單紀錄與帳本狀態矛盾 |
| `match_invalid_request` | invalid_argument | 場次操作參數不合法 |
| `activity_match_not_found` | not_found | 場次不存在 |
| `confirmation_required` | invalid_argument | 不可逆動作(封盤 / 判勝負 / 棄賽)需要二次確認 |
| `winner_not_in_match` | invalid_argument | 勝者必須是本場的選手 |
| `match_not_ready` | failed_precondition | 這場不在讓武選購階段,無法封盤 |
| `match_not_locked` | failed_precondition | 讓武尚未封盤,不能開打 |
| `match_not_live` | failed_precondition | 這場尚未開打,不能判定勝負 |
| `match_finished` | failed_precondition | 這場已完賽 |
| `match_already_open` | failed_precondition | 這場已經開盤 |
| `activity_player_already_withdrawn` | already_exists | 這位選手已經棄賽 |
| `activity_player_withdrawn` | failed_precondition | 場上有一方已棄賽,不該再走正常流程 |
| `match_players_not_set` | failed_precondition | 場次雙方尚未確定(等上一輪),還不能開盤 |
| `activity_bracket_missing` | internal | 這屆賽事尚未抽籤卻走到需要對戰表的路徑 |
| `activity_advance_target_missing` | internal | 晉級目標場次不存在(對戰表壞了) |
| `prize_invalid_request` | invalid_argument | 發獎參數不合法 |
| `prize_third_place_undecidable` | failed_precondition | 設定了季軍獎金,但單淘汰沒有季軍賽推不出季軍 → **一毛不發**,要發得先加季軍賽 |
| `prize_final_not_decided` | failed_precondition | 決賽尚未分出勝負,名次不成立 |
| `prize_bracket_broken` | internal | 對戰表資料異常,無法決定名次 |
| `prize_ledger_state_conflict` | internal | 帳本回應與預期不符 |
| `not_found` | not_found | 泛用的查無此物(入口層 port) |
| `unauthenticated` | unauthenticated | 沒有有效身分 |
| `permission_denied` | permission_denied | 身分有效但無權執行 |
| `canceled` | canceled | 呼叫端取消 |
| `deadline_exceeded` | deadline_exceeded | 逾時 |

改 reason 的值等同**破壞性變更**(呼叫端會依它分支)。新增錯誤請一併補這張表。
