package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/danicotech/hestia/internal/core/platform/activitylog"
)

// 服務對服務認證(stentor → hestia)。
//
// 為什麼需要一條有別於使用者 Bearer 的路徑:stentor 沒有 DB 憑證(專案鐵則 2),
// Discord 的語音/訊息/reaction 只能經 API 寫進來。那些請求沒有登入使用者的
// session —— 呼叫者是**服務**,而它代表某個 Discord 使用者做事。
//
// 三個設計決定:
//
//  1. **服務身分與使用者身分是不同的東西。** 兩者走不同的 header,而且
//     ctx 上分開存。活動記錄 RPC 只收服務身分;使用者 RPC 只收使用者身分。
//     一個請求不能同時是兩者 —— 同時帶兩種憑證直接拒絕,不做「擇一採用」的
//     猜測(猜錯的後果是提權)。
//
//  2. **可輪替、常數時間比對。** 同時可以有多把有效 token(輪替期間新舊並存,
//     否則換 token 必然中斷服務)。比對先取 SHA-256 再用 subtle.ConstantTimeCompare:
//     摘要長度固定,所以連「token 有多長」都不會從時間洩漏,而且絕不用 ==。
//
//  3. **行為主體用 X-Acting-User: discord:<snowflake>**,由 hestia 查 identities
//     解成內部 user_id。查不到就是查不到 —— 這裡不建帳號。

const (
	// headerServiceToken 是服務憑證的專用 header。
	//
	// 為什麼不共用 Authorization:共用的話伺服器得先「猜」這是哪一種憑證,
	// 而猜測邏輯就是提權漏洞的溫床。分開之後,「使用者 token 冒充服務」與
	// 「服務 token 冒充使用者」在結構上都不可能 —— 它們連 header 都不同。
	headerServiceToken = "X-Service-Token"

	// headerActingUser 是「這次呼叫代表誰」,格式 `<provider>:<外部使用者 id>`。
	headerActingUser = "X-Acting-User"
)

// minServiceTokenLength 是設定檢查的下限。
// 32 字元 ≈ 128 bit 以上的熵(base64/hex 皆然),低於這個長度的 token
// 靠猜就有機會 —— 而它拿到的是「代表任何使用者寫入」的能力。
const minServiceTokenLength = 32

// ServiceToken 是一把服務憑證。Name 只用於稽核與除錯,**絕不進 event_logs 的
// 任何欄位**(連名字都不進:那等於公告目前有效的 token 有哪幾把)。
type ServiceToken struct {
	// Name 是呼叫方服務名,如 "stentor"。輪替時新舊兩把可以同名。
	Name string
	// Token 是憑證明文,由設定注入(env / secret manager),不在本套件讀取。
	Token string

	// Delegable 是**這一把**憑證可以代打的 procedure(全名,如
	// platformv1connect.DailyServiceClaimProcedure)。
	//
	// 為什麼要 per-token scope(QA 2026-09-08):在此之前 Name 只被當布林用 ——
	// 任何一把有效的服務 token 自動取得**完整**代打白名單,包含
	// ShopService/Purchase 與 DailyService/Claim。也就是說,任何一把憑證的
	// 爆炸半徑 = 全部已綁定使用者的餘額。發第二把 token 給別的用途
	// (例如只想讓某個內部工具讀 /balance)時,它會連買東西的權限一起拿到。
	//
	// **空 = 不能代打任何使用者 RPC(fail closed)。** 只收服務身分的 RPC
	// (ActivityService / NotificationService)不受這個清單約束 ——
	// 那是「服務自己的能力」,不是「代表某個人動他的東西」,兩者的風險不同。
	//
	// 每一條都必須在全域代打白名單(delegatedProcedures)內,啟動時斷言:
	// scope 是全域白名單的**收窄**,不是繞過它的第二條路。
	// 要給滿的話用 AllDelegableProcedures() —— 那樣至少是有人明確打字給的。
	Delegable []string
}

// AllDelegableProcedures 回傳全域代打白名單的全部 procedure(排序後)。
//
// 給組裝端當「這把 token 要滿權限」的明確寫法。刻意不做成 Delegable 的預設值:
// 預設值是不會被 review 的東西,而這裡給出去的是「代表任何已綁定使用者花錢」。
func AllDelegableProcedures() []string {
	out := make([]string, 0, len(delegatedProcedures))
	for procedure := range delegatedProcedures {
		out = append(out, procedure)
	}
	sort.Strings(out)
	return out
}

// serviceVerifier 驗證服務憑證。持有的是 SHA-256 摘要而不是明文:
// 就算行程記憶體被 dump,拿到的也不是可直接使用的憑證。
type serviceVerifier struct {
	entries []serviceEntry
}

type serviceEntry struct {
	name   string
	digest [sha256.Size]byte
	// delegable 是這把憑證的代打 scope;nil = 不能代打。
	delegable map[string]struct{}
}

// newServiceVerifier 建立驗證器。設定不合法時回 error 讓服務**開不起來** ——
// 一把空的或太短的 token 悄悄生效,比服務啟動失敗糟糕得多。
func newServiceVerifier(tokens []ServiceToken) (*serviceVerifier, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	v := &serviceVerifier{entries: make([]serviceEntry, 0, len(tokens))}
	seen := map[[sha256.Size]byte]struct{}{}
	for i, t := range tokens {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return nil, fmt.Errorf("服務 token #%d 沒有名字", i)
		}
		if len(t.Token) < minServiceTokenLength {
			// 刻意不把 token 印進錯誤訊息(錯誤會進 log)。
			return nil, fmt.Errorf("服務 token %q 長度不足 %d 字元", name, minServiceTokenLength)
		}
		digest := sha256.Sum256([]byte(t.Token))
		if _, dup := seen[digest]; dup {
			return nil, fmt.Errorf("服務 token %q 與前面某一把重複", name)
		}
		seen[digest] = struct{}{}

		// scope 的啟動斷言:每一條都必須在全域代打白名單內。
		//
		// 為什麼在這裡炸而不是執行期忽略:寫錯的 scope 有兩種後果 ——
		// 打錯字的那條會靜靜地不生效(功能壞掉但沒人知道),而「白名單外的
		// procedure」代表有人以為 scope 可以擴權。兩者都必須在上線前發現。
		delegable := make(map[string]struct{}, len(t.Delegable))
		for _, procedure := range t.Delegable {
			if _, ok := delegatedProcedures[procedure]; !ok {
				return nil, fmt.Errorf(
					"服務 token %q 的代打 scope 含 %s,但它不在全域代打白名單內"+
						"(scope 只能收窄白名單,不能繞過它)", name, procedure)
			}
			delegable[procedure] = struct{}{}
		}
		v.entries = append(v.entries, serviceEntry{
			name: name, digest: digest, delegable: delegable,
		})
	}
	return v, nil
}

// verify 比對呈上的 token。回傳對應的設定項(名字 + 代打 scope)。
//
// 迴圈**不提早跳出**:比完全部才回答,所以耗時只跟設定了幾把 token 有關,
// 跟「猜中第幾把」「猜對前幾個字元」都無關。
func (v *serviceVerifier) verify(presented string) (*serviceEntry, bool) {
	if v == nil || presented == "" {
		return nil, false
	}
	digest := sha256.Sum256([]byte(presented))
	var match *serviceEntry
	for i := range v.entries {
		if subtle.ConstantTimeCompare(digest[:], v.entries[i].digest[:]) == 1 {
			match = &v.entries[i]
		}
	}
	return match, match != nil
}

// canDelegate 回報這把憑證能不能代打某支 procedure。
func (e *serviceEntry) canDelegate(procedure string) bool {
	if e == nil {
		return false
	}
	_, ok := e.delegable[procedure]
	return ok
}

// X-Acting-User 的兩種錯誤。**兩個 sentinel,不是一個加訊息。**
//
// QA 2026-09-08 發現的資料落地洩漏就出在這裡:原本「不支援的 provider」是
// `fmt.Errorf("%w(不支援的 provider %q)", …, provider)`,而 provider 那一段
// **完全由呼叫端控制**。它被包成 ownError(= 本套件產生、訊息可信)之後,
// summary.go 會把訊息寫進 event_logs.response.message,也原樣回給呼叫端 ——
// 帶一個 `X-Acting-User: <任意字串>:1` 就能把任意內容寫進保留 180 天的稽核表。
//
// 所以規則是:**錯誤訊息永遠不引用呼叫端輸入。** 要讓呼叫端分辨兩種情況,
// 就給兩個 sentinel(各自有穩定的 reason,見 errmap.go),而不是把值塞進文案。
// 想知道當時到底送了什麼,靠 request_id 去 log 撈 —— log 不是資料表。
var (
	// errActingUserFormat:不是 `<provider>:<id>` 的形狀。
	errActingUserFormat = errors.New(headerActingUser + " 格式必須是 <provider>:<id>")
	// errActingUserProvider:形狀對,但 provider 不在白名單內。
	errActingUserProvider = errors.New(headerActingUser + " 的 provider 不受支援")
)

// parseActingUser 解析 `discord:123456789`。
//
// 只認白名單內的 provider:讓呼叫端自由填 provider 等於把 identities 的查詢
// 條件交給外部控制。目前只有 discord(Twitch/YouTube 上線時在這裡加一行)。
//
// 回傳的一律是**裸的 sentinel**,絕不 wrap raw 或它的任何片段(見上面的說明)。
func parseActingUser(raw string) (provider, externalID string, err error) {
	raw = strings.TrimSpace(raw)
	provider, externalID, found := strings.Cut(raw, ":")
	if !found || provider == "" || externalID == "" {
		return "", "", errActingUserFormat
	}
	if provider != activitylog.ProviderDiscord {
		return "", "", errActingUserProvider
	}
	return provider, externalID, nil
}
