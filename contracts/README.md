# contracts

平台 API 與渲染契約的唯一來源。四個 repo 跨兩種語言,契約一定會漂,所以規則寫死:

> **禁止手寫共用型別。** 所有 client 都從這裡生成。

## platform/

身分、代幣帳本、通知、兌換的 API。stentor / themis / theatron 都依賴它。

Go server 骨架用 `oapi-codegen` 生在同 repo;TS client 由 CI 發成 npm 套件,其他 repo 用版本相依安裝。

## render/

Discord 顯示描述。stentor 與 themis 共同認的那份契約 —— 活動服務回傳「要顯示什麼」,不是 Discord 的格式:

```json
{ "title": "阿凱 對 小美",
  "fields": [{"k":"賠率","v":"1.43 / 2.58"},{"k":"票數","v":"18"}],
  "actions": [{"id":"summer-cup:bet:5:1","label":"押 阿凱","style":"primary"}] }
```

`custom_id` 的前綴(`summer-cup`)就是路由鍵。加一個新活動 = 註冊一個前綴 + 實作這份契約,stentor 一行都不用改。

**這份契約必須住在 hestia。** 放進 stentor 的話,每辦一個新活動都要去改 Bot 的 repo,分 repo 就白分了。
