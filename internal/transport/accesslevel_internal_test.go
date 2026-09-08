package transport

import (
	"strings"
	"testing"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
)

// 存取層級清單的**負向**測試。
//
// 為什麼需要:啟動斷言原本只有 delegatedProcedures 那份被反過來驗過
// (「壞設定真的開不起來」)。serviceIdentityServices 與 subjectlessServices
// 的斷言沒有負向測試 —— 也就是說,那道斷言自己壞掉了我們不會知道,
// 而它守的正是 fail-open 的方向(漏登記 = 任何登入使用者都能呼叫)。
//
// 這些測試直接改全域清單再呼叫 verifyProcedureCoverage:要測的就是
// 「這份設定寫錯時會不會炸」。同一個 package 的測試是循序跑的,
// 每個 case 都在 defer 裡還原。

// swapStrings 暫時換掉一份服務清單,回傳還原函式。
func swapStrings(target *[]string, next []string) func() {
	prev := *target
	*target = next
	return func() { *target = prev }
}

// 只收服務身分的服務漏登記 → 啟動就要炸。
//
// 這是最重要的一條:漏掉 ActivityService 的話,任何登入使用者都能偽造活動
// 記錄(而活動記錄會變成 XP);漏掉 NotificationService 的話,任何登入使用者
// 都能把待送公告認領走。
func TestVerifyRejectsMissingServiceIdentityRegistration(t *testing.T) {
	cases := map[string][]string{
		"漏了 ActivityService":     {platformv1connect.NotificationServiceName},
		"漏了 NotificationService": {platformv1connect.ActivityServiceName},
		"兩個都漏":                   nil,
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			defer swapStrings(&serviceIdentityServices, list)()
			// subjectlessServices 是 serviceIdentityServices 的子集,
			// 拿掉母集合時要一併清掉,否則炸的會是另一條斷言。
			defer swapStrings(&subjectlessServices, nil)()

			err := verifyProcedureCoverage()
			if err == nil {
				t.Fatal("漏登記卻通過了 —— 這道斷言守的正是 fail open 的方向")
			}
			if !strings.Contains(err.Error(), "serviceIdentityServices") {
				t.Fatalf("錯誤訊息沒指出問題在哪:%v", err)
			}
		})
	}
}

// serviceIdentityServices 裡列了不存在的服務 → 炸。
// (改了 proto 卻留下舊字串 = 那個服務其實沒被保護。)
func TestVerifyRejectsUnknownServiceIdentityEntry(t *testing.T) {
	defer swapStrings(&serviceIdentityServices, append(
		[]string{"hestia.platform.v1.GhostService"},
		platformv1connect.ActivityServiceName,
		platformv1connect.NotificationServiceName,
	))()

	err := verifyProcedureCoverage()
	if err == nil {
		t.Fatal("不存在的服務名竟然通過")
	}
	if !strings.Contains(err.Error(), "GhostService") {
		t.Fatalf("錯誤訊息沒指出是哪一個:%v", err)
	}
}

// subjectlessServices 必須是 serviceIdentityServices 的子集。
//
// 為什麼是硬條件:「沒有使用者主體」只在「只收服務身分」的前提下有意義。
// 把一個使用者 RPC 列進去,結果會是「服務憑證打得到、卻不必說明代表誰」——
// 那正是代打路徑最不能出現的形狀(服務代表「沒有人」動別人的東西)。
func TestVerifyRejectsSubjectlessOutsideServiceIdentity(t *testing.T) {
	defer swapStrings(&subjectlessServices, []string{
		platformv1connect.MeServiceName,
	})()

	err := verifyProcedureCoverage()
	if err == nil {
		t.Fatal("非服務身分的服務被列為無主體,卻通過了")
	}
	if !strings.Contains(err.Error(), "serviceIdentityServices") {
		t.Fatalf("錯誤訊息沒指出子集關係:%v", err)
	}
}

// subjectlessServices 裡列了不存在的服務 → 炸。
// (留下舊字串的後果是「那個服務突然開始要求 X-Acting-User」,呼叫端全壞。)
func TestVerifyRejectsUnknownSubjectlessEntry(t *testing.T) {
	defer swapStrings(&subjectlessServices, []string{"hestia.platform.v1.GhostService"})()

	err := verifyProcedureCoverage()
	if err == nil {
		t.Fatal("不存在的服務名竟然通過")
	}
	if !strings.Contains(err.Error(), "GhostService") {
		t.Fatalf("錯誤訊息沒指出是哪一個:%v", err)
	}
}

// 出貨的設定本身要通過(上面那些測試才證明得了「炸的是壞設定」)。
func TestShippedAccessLevelsVerify(t *testing.T) {
	if err := verifyProcedureCoverage(); err != nil {
		t.Fatalf("出貨的存取層級設定應該通過:%v", err)
	}
	// 子集關係在出貨設定上成立。
	for _, svc := range subjectlessServices {
		found := false
		for _, s := range serviceIdentityServices {
			if s == svc {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 被列為無主體,卻不在 serviceIdentityServices 裡", svc)
		}
	}
}

// **已知的殘留風險**(QA 2026-09-08 提出,本輪評估後不修):
//
// 上面那道「屬於只收服務身分的服務卻沒登記」的斷言,判準是**服務名前綴**
// (Activity / Notification)。未來一個必須只收服務、但沒用這些前綴命名的
// 服務(例:DiscordIngestService)會安靜地落在「任何登入使用者都能呼叫」
// 那一側 —— 正是這道斷言要防的方向。
//
// 正解是用 proto option(如 `(hestia.access_level) = SERVICE_ONLY`)當權威,
// 由 descriptor 反查,命名就不再承載授權語意。沒有現在做的理由見交付回報:
// 它要新增一個 extension proto 並改動每一份服務定義,而那些檔案正在被其他
// 工作並行修改。
//
// 這個測試把風險釘成可執行的形式:今天所有「只收服務」的服務都用了那兩個
// 前綴,所以斷言有效;哪天有人加了不合前綴的,這裡會失敗並指向上面的說明。
func TestServiceOnlyServicesStillMatchNamePrefixes(t *testing.T) {
	for _, svc := range serviceIdentityServices {
		short := svc[strings.LastIndex(svc, ".")+1:]
		if !strings.HasPrefix(short, "Activity") && !strings.HasPrefix(short, "Notification") {
			t.Fatalf("%s 只收服務身分,但服務名不符合啟動斷言的前綴判準"+
				"——那道斷言對它是失效的。請改用 proto option 當權威"+
				"(見本測試上方的說明),或替它加一條前綴。", svc)
		}
	}
}
