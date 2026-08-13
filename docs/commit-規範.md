# Commit 規範

Conventional Commits。四個 repo 用同一套,這份是完整版,另外三個 repo 只放格式與自己的 scope 清單。

## 格式

```text
<type>(<scope>): <subject>

- 為什麼要改
- 有什麼副作用或前提

<footer>
```

```text
feat(ledger): 帳本寫入改用 append-only

- 餘額改成同交易內更新的快取,真實來源是 token_entries
- 併發下注時靠 SELECT ... FOR UPDATE 擋住重複扣款
- 對帳 job 另外一個 PR
```

**冒號前面沒有空格。** `feat(ledger) : xxx` 會被 commitlint 擋下來 —— 它不認得這個 header,整行會被當成沒有 type。

## type

| type | 什麼時候用 |
|---|---|
| `feat` | 新增使用者看得到的能力 |
| `fix` | 修正錯誤行為 |
| `refactor` | 行為不變的內部改動 |
| `perf` | 效能改善 |
| `docs` | 只有文件 |
| `test` | 只有測試 |
| `build` | 建置、相依、Dockerfile |
| `ci` | workflow 與 CI 設定 |
| `chore` | 以上都不是的雜項。**如果你常用到它,通常是分類想偷懶** |
| `revert` | 回滾 |

`style` 不用 —— 格式一律交給 formatter,不該有專門的 commit。

## scope

用**這次改動落在哪一層或哪個領域**,不要用檔名。

| scope | 範圍 |
|---|---|
| `identity` | 帳號、token、Discord 綁定 |
| `ledger` | 代幣帳本 |
| `outbox` | 事件外送箱與派送 |
| `notify` | email / sms / meta |
| `redeem` | 獎勵兌換 |
| `activity` | M1 暫住的活動層 |
| `http` | `interface/http`,含 dto、handler、router、middleware |
| `ws` | websocket |
| `cron` | 排程與背景工作 |
| `storage` | postgres / redis 的 adapter |
| `contracts` | 契約與匯出 |
| `migrations` | schema |
| `config` | 設定 |
| `e2e` | 端對端測試 |
| `deps` | 相依升級 |
| `ci` | workflow |

跨多層的改動不要硬塞 scope,直接省略:`refactor: 把交易邊界從 repository 上移到 core`。

## subject

- **祈使句、現在式。** 「加上冪等鍵檢查」,不是「加上了」或「將會加上」
- **不加句號**
- **72 字元以內**(commitlint 預設放到 100,但 72 才能在 `git log --oneline` 一行看完)
- 中文英文都可以,但同一個 repo 裡保持一致
- 講**做了什麼**,不是**改了哪個檔案** —— 檔案清單 git 自己有

## body

用條列。每一條回答的是**為什麼**,不是**改了什麼**:

```text
- 原本的實作在多實例下會重複派送同一筆事件
- 改用 FOR UPDATE SKIP LOCKED,多個 dispatcher 同時跑也安全
```

body 可以省略。**但如果這個改動半年後的你會想問「當初為什麼這樣做」,就一定要寫。**

## footer

```text
BREAKING CHANGE: /v1/bets 的 stake 欄位改成必填
Refs: #42
```

破壞性改動兩種寫法都行:type 後面加 `!`(`feat(http)!: ...`),或 footer 寫 `BREAKING CHANGE:`。**兩個都寫更好** —— `!` 給掃 log 的人看,footer 給產 changelog 的工具看。

## 一個 commit 一件事

判準:**這個 commit 的 subject 需要用「並且」或「順便」嗎?** 需要的話就該拆。

順手修好的東西自己一個 commit。它會在某天被 `git bisect` 找上,那時你會慶幸它沒跟一個 feature 綁在一起。

## 怎麼擋

規範靠自律撐不過三個月,做成檢查才有用。

**CI**(四個 repo 都適用,Go 專案也一樣 —— 這是跑在 CI 容器裡的一次性指令,不會變成專案的相依):

```bash
npx --yes @commitlint/cli --extends @commitlint/config-conventional \
  --from origin/main --to HEAD
```

**本機**:用 `commit-msg` hook 擋在推之前。Go 專案不想碰 Node 的話,`conventional-commit-linter` 這類單一執行檔的工具也可以。

**PR 標題也要照這個格式。** squash merge 之後 PR 標題就是主線上的 commit message —— 這是最常見的破口:每個 commit 都很乾淨,合進去卻變成 `Update README (#12)`。
