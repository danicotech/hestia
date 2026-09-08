package transport

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danicotech/hestia/gen/hestia/platform/v1/platformv1connect"
)

// 這一檔是套件內測試:驗的是憑證比對本身,不經 HTTP。
// 走 HTTP 的那一側(哪個 RPC 收哪種身分)在 serviceauth_test.go。

const (
	tokenA = "stentor-token-aaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB = "stentor-token-bbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestServiceVerifier(t *testing.T) {
	v, err := newServiceVerifier([]ServiceToken{
		{Name: "stentor", Token: tokenA},
		{Name: "stentor-next", Token: tokenB},
	})
	if err != nil {
		t.Fatalf("newServiceVerifier: %v", err)
	}

	// 輪替期間新舊並存:兩把都必須有效,否則換 token 一定中斷服務。
	for _, c := range []struct{ token, name string }{
		{tokenA, "stentor"},
		{tokenB, "stentor-next"},
	} {
		got, ok := v.verify(c.token)
		if !ok || got.name != c.name {
			t.Fatalf("verify(%q) = (%+v, %v),期望名字 %q", c.token, got, ok, c.name)
		}
	}

	// 錯的、空的、只差一個字元的、前綴正確的,全部要拒絕。
	for _, bad := range []string{
		"", " ", "wrong", tokenA + "x", tokenA[:len(tokenA)-1],
		strings.ToUpper(tokenA), " " + tokenA,
	} {
		if _, ok := v.verify(bad); ok {
			t.Fatalf("verify(%q) 竟然通過", bad)
		}
	}
}

// 沒有設定任何 token = 服務認證關閉,任何憑證都不通過(fail closed)。
func TestServiceVerifierNilRejectsEverything(t *testing.T) {
	v, err := newServiceVerifier(nil)
	if err != nil {
		t.Fatalf("newServiceVerifier: %v", err)
	}
	if v != nil {
		t.Fatalf("沒有設定 token 時應該回 nil verifier")
	}
	if _, ok := v.verify(tokenA); ok {
		t.Fatal("nil verifier 竟然接受了 token")
	}
}

// 設定不合法要在啟動時就炸,不能悄悄生效。
func TestServiceVerifierRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		tokens []ServiceToken
	}{
		{"空 token", []ServiceToken{{Name: "stentor", Token: ""}}},
		{"太短", []ServiceToken{{Name: "stentor", Token: "short"}}},
		{"沒名字", []ServiceToken{{Name: " ", Token: tokenA}}},
		{"重複", []ServiceToken{
			{Name: "a", Token: tokenA}, {Name: "b", Token: tokenA},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := newServiceVerifier(c.tokens); err == nil {
				t.Fatal("期望設定錯誤,卻通過了")
			}
		})
	}
}

// 錯誤訊息不可以含 token 明文——它會進 log。
func TestServiceVerifierErrorNeverLeaksToken(t *testing.T) {
	_, err := newServiceVerifier([]ServiceToken{{Name: "stentor", Token: "tooshort"}})
	if err == nil {
		t.Fatal("期望錯誤")
	}
	if strings.Contains(err.Error(), "tooshort") {
		t.Fatalf("錯誤訊息洩漏 token:%v", err)
	}
}

func TestParseActingUser(t *testing.T) {
	provider, id, err := parseActingUser("discord:123456789012345678")
	if err != nil {
		t.Fatalf("parseActingUser: %v", err)
	}
	if provider != "discord" || id != "123456789012345678" {
		t.Fatalf("解析結果 = (%q, %q)", provider, id)
	}

	// 形狀不對 → 格式錯。
	for _, bad := range []string{"", "discord", "discord:", ":123"} {
		if _, _, err := parseActingUser(bad); !errors.Is(err, errActingUserFormat) {
			t.Fatalf("parseActingUser(%q) err = %v,期望格式錯誤", bad, err)
		}
	}
	// 形狀對但 provider 不在白名單 → 另一個 sentinel(呼叫端才分得出差別)。
	for _, bad := range []string{"twitch:123", "DISCORD:123"} {
		if _, _, err := parseActingUser(bad); !errors.Is(err, errActingUserProvider) {
			t.Fatalf("parseActingUser(%q) err = %v,期望 provider 不支援", bad, err)
		}
	}
}

// **錯誤訊息絕不引用呼叫端輸入**(QA 2026-09-08 的資料落地洩漏)。
//
// 原本「不支援的 provider %q」會把 header 裡的任意字串包進 ownError,
// 而 ownError 的訊息會被 summary.go 寫進 event_logs.response.message
// (保留 180 天)並原樣回給呼叫端。這裡用金絲雀字串釘死那條路。
func TestActingUserErrorNeverEchoesInput(t *testing.T) {
	const canary = "LEAK-CANARY-AAAA-BBBB"
	for _, raw := range []string{
		canary + ":1",
		canary,
		":" + canary,
		canary + ":" + canary,
	} {
		_, _, err := parseActingUser(raw)
		if err == nil {
			t.Fatalf("parseActingUser(%q) 竟然通過", raw)
		}
		// sentinel 本身、包成 connect 錯誤之後、以及寫進稽核摘要之後,
		// 三個位置都不能出現金絲雀。
		mapped := toConnectError(err)
		summary := errorSummary(mapped)
		for name, text := range map[string]string{
			"sentinel": err.Error(),
			"connect":  mapped.Error(),
			"稽核摘要":     fmt.Sprint(summary),
		} {
			if strings.Contains(text, canary) {
				t.Fatalf("%s 洩漏了呼叫端輸入(raw=%q):%s", name, raw, text)
			}
		}
		// 但成因仍然分得出來 —— 那正是 reason 的用途。
		if r := ErrorReason(mapped); r != "acting_user_format" && r != "acting_user_provider_unsupported" {
			t.Fatalf("raw=%q 的 reason = %q,兩種成因都該有值", raw, r)
		}
	}
}

// ── 代打白名單的啟動斷言 ────────────────────────────────────────

// 白名單設定錯了要在**啟動時**炸 —— 代打是唯一「服務憑證能讓請求變成某個
// 使用者」的路徑,寫錯的後果是提權或功能靜默失效,兩者都不能等上線後才發現。
func TestNewRejectsBadDelegationList(t *testing.T) {
	cases := []struct {
		name      string
		procedure string
		want      string
	}{
		{
			"proto 沒有這個 procedure",
			"/hestia.platform.v1.MeService/NoSuchMethod",
			"proto 沒有這個 procedure",
		},
		{
			"與只收服務身分重疊",
			platformv1connect.ActivityServiceRecordMessagesProcedure,
			"只收服務身分",
		},
		{
			"與管理授權重疊",
			platformv1connect.AdminEconomyServiceGrantProcedure,
			"需要授權",
		},
		{
			"與公開清單重疊",
			platformv1connect.ShopServiceListItemsProcedure,
			"公開",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := addDelegatedProcedure(t, c.procedure)
			defer restore()

			_, err := New(Deps{})
			if err == nil {
				t.Fatal("設定有問題卻開得起來")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("錯誤訊息沒說明原因(期望含 %q):%v", c.want, err)
			}
		})
	}
}

// 正常設定開得起來 —— 上面那些測試才證明得了「炸的是設定錯誤本身」。
func TestNewAcceptsShippedDelegationList(t *testing.T) {
	if _, err := New(Deps{}); err != nil {
		t.Fatalf("預設設定應該開得起來:%v", err)
	}
}

// 白名單裡的每一支都必須是「使用者 RPC」:不需要授權、不只收服務、非公開。
// (啟動斷言已經擋住反例,這裡把「出貨的清單本身合格」也鎖住。)
func TestShippedDelegationListIsUserRPCsOnly(t *testing.T) {
	for procedure := range delegatedProcedures {
		if !delegableGlobally(procedure) {
			t.Fatalf("%s 在白名單裡卻不被 delegableGlobally 接受", procedure)
		}
		if _, public := publicProcedures[procedure]; public {
			t.Fatalf("%s 同時是公開 RPC,兩個權威位置", procedure)
		}
	}
}

// addDelegatedProcedure 暫時往白名單塞一條,回傳還原函式。
// 直接改全域 map 是刻意的:要測的就是「這份設定寫錯時會怎樣」。
func addDelegatedProcedure(t *testing.T, procedure string) func() {
	t.Helper()
	if _, dup := delegatedProcedures[procedure]; dup {
		t.Fatalf("%s 已經在白名單裡,這個測試就沒有意義了", procedure)
	}
	delegatedProcedures[procedure] = struct{}{}
	return func() { delete(delegatedProcedures, procedure) }
}
