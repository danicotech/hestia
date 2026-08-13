# e2e

黑箱測試。起完整服務、連真的 Postgres 與 Redis、打真的 HTTP。

**不准 import `internal/`。** 只能透過 HTTP 回應和資料庫狀態斷言。

這條看起來像潔癖,其實是 e2e 唯一的價值來源:重構內部結構的時候它不用改,所以它能告訴你「行為有沒有變」。一旦它 import 了內部型別,重構就要連帶改測試,那它就只是一套比較慢的單元測試。

用 build tag 隔開,預設的 `go test ./...` 不會跑到:

```go
//go:build e2e
```

## 先寫哪幾個

不要從 happy path 開始 —— 那些在開發時本來就會一直手動驗到。從**單元測試抓不到、而且出錯最貴**的地方開始:

1. 同時送兩筆相同 `Idempotency-Key` 的下注 → 只能成立一筆,回傳結果一致
2. 同一場比賽併發結算兩次 → 帳本只能動一次
3. 下注扣款後寫 outbox 失敗 → 整筆 rollback,餘額不變
4. 對帳:一連串隨機操作之後,`SUM(token_entries.amount)` 必須等於 `users.token_balance`

第 4 條可以寫成 property-based 的形式,亂數跑幾百輪。代幣算錯是這個系統最貴的 bug,值得這個投資。
