package transport

import (
	"net/http"
	"net/netip"
	"testing"
)

// clientIP 的判準是「這個封包從誰那裡來的」。從外面(transport_test)測不到
// 對端位址——httptest 的對端永遠是回送位址,而這裡要驗的正是「對端不同時
// 結果要不同」。所以這一組用套件內測試,直接餵各種對端。

func mustProxies(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", s, err)
	}
	return p
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		peer    string
		trusted string
		header  http.Header
		want    string
	}{{
		name: "沒設可信網段時完全不看標頭",
		peer: "127.0.0.1:51000", trusted: "",
		header: hdr("CF-Connecting-IP", "203.0.113.7", "X-Forwarded-For", "203.0.113.7"),
		want:   "127.0.0.1",
	}, {
		// 這是修好的那個 bug:cloudflared 在本機,不信任它就等於
		// 每一列 session 都記 127.0.0.1,欄位形同不存在。
		name: "可信對端的 CF-Connecting-IP 會被採用",
		peer: "127.0.0.1:51000", trusted: "127.0.0.1,::1",
		header: hdr("CF-Connecting-IP", "203.0.113.7"),
		want:   "203.0.113.7",
	}, {
		// Cloudflare 對每個經過它的請求都覆寫 CF-Connecting-IP,
		// XFF 則可能還串著上游填的值,兩者衝突時以前者為準。
		name: "CF-Connecting-IP 優先於 X-Forwarded-For",
		peer: "127.0.0.1:51000", trusted: "127.0.0.1",
		header: hdr("CF-Connecting-IP", "203.0.113.7", "X-Forwarded-For", "198.51.100.9"),
		want:   "203.0.113.7",
	}, {
		name: "只有 XFF 時取最左值",
		peer: "127.0.0.1:51000", trusted: "127.0.0.1",
		header: hdr("X-Forwarded-For", "203.0.113.7, 70.41.3.18, 150.172.238.178"),
		want:   "203.0.113.7",
	}, {
		// 重點案例:同一組標頭、同一份設定,只有對端不同。
		// 區網裡的人自己填一個 CF-Connecting-IP 不該影響稽核紀錄。
		name: "不可信對端送的標頭一律忽略",
		peer: "192.168.1.50:51000", trusted: "127.0.0.1,::1",
		header: hdr("CF-Connecting-IP", "203.0.113.7"),
		want:   "192.168.1.50",
	}, {
		name: "網段形式的可信來源",
		peer: "172.18.0.3:51000", trusted: "172.16.0.0/12",
		header: hdr("CF-Connecting-IP", "203.0.113.7"),
		want:   "203.0.113.7",
	}, {
		name: "網段外的鄰居不因同網段家族而被信任",
		peer: "172.32.0.1:51000", trusted: "172.16.0.0/12",
		header: hdr("CF-Connecting-IP", "203.0.113.7"),
		want:   "172.32.0.1",
	}, {
		// 雙堆疊監聽下 IPv4 連線的對端常以 ::ffff:a.b.c.d 出現,
		// 不攤平就永遠對不上 IPv4 網段,設定看起來沒生效。
		name: "IPv4-mapped IPv6 對端能對上 IPv4 網段",
		peer: "[::ffff:127.0.0.1]:51000", trusted: "127.0.0.1",
		header: hdr("CF-Connecting-IP", "203.0.113.7"),
		want:   "203.0.113.7",
	}, {
		name: "IPv6 對端與 IPv6 網段",
		peer: "[::1]:51000", trusted: "::1",
		header: hdr("CF-Connecting-IP", "2001:db8::5"),
		want:   "2001:db8::5",
	}, {
		// 寧可記一個不夠精確但為真的 IP,也不要把來路不明的字串寫進 DB。
		name: "可信對端送垃圾值時退回對端位址",
		peer: "127.0.0.1:51000", trusted: "127.0.0.1",
		header: hdr("CF-Connecting-IP", "不是IP"),
		want:   "127.0.0.1",
	}, {
		name: "CF 標頭是垃圾時會退到 XFF",
		peer: "127.0.0.1:51000", trusted: "127.0.0.1",
		header: hdr("CF-Connecting-IP", "garbage", "X-Forwarded-For", "203.0.113.7"),
		want:   "203.0.113.7",
	}, {
		name: "標頭帶 port 也要去掉",
		peer: "127.0.0.1:51000", trusted: "127.0.0.1",
		header: hdr("X-Forwarded-For", "203.0.113.7:44321"),
		want:   "203.0.113.7",
	}, {
		name: "對端沒有 port 時原樣使用",
		peer: "127.0.0.1", trusted: "",
		header: hdr(),
		want:   "127.0.0.1",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := clientIP(tc.peer, tc.header, mustProxies(t, tc.trusted))
			if got != tc.want {
				t.Fatalf("clientIP(%q, trusted=%q) = %q, want %q",
					tc.peer, tc.trusted, got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()

	t.Run("空字串是 nil 而不是錯", func(t *testing.T) {
		t.Parallel()
		got, err := ParseTrustedProxies("   ")
		if err != nil || got != nil {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("裸 IP 補成單一位址網段", func(t *testing.T) {
		t.Parallel()
		got := mustProxies(t, "127.0.0.1, ::1")
		want := []string{"127.0.0.1/32", "::1/128"}
		if len(got) != len(want) {
			t.Fatalf("got %v", got)
		}
		for i := range want {
			if got[i].String() != want[i] {
				t.Fatalf("[%d] = %s, want %s", i, got[i], want[i])
			}
		}
	})

	// 設定打錯時「靜靜地少信任一段」會表現成「來源 IP 全變成代理位址」,
	// 那是幾個月後才有人發現的壞法,所以一律回錯讓服務起不來。
	for _, bad := range []string{"不是IP", "10.0.0.0/99", "127.0.0.1/32/8", "example.com"} {
		t.Run("拒絕 "+bad, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseTrustedProxies(bad); err == nil {
				t.Fatalf("%q 應該回錯", bad)
			}
		})
	}

	// 一段有效、一段無效時不能只吃有效的那段——那等於默默套用了
	// 一份跟操作者寫的不一樣的設定。
	t.Run("整串裡有一個壞的就整串失敗", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseTrustedProxies("127.0.0.1, 壞的"); err == nil {
			t.Fatal("應該回錯")
		}
	})
}
