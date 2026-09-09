package transport

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// 裝置足跡的取值與清理。兩個欄位都是**外界給的字串**,進 sessions 表之前
// 由入口層負責把它們變安全:UA 截斷、IP 只在對端可信時才採信標頭。

// maxUserAgentBytes 是寫進 sessions.user_agent 的上限。
//
// User-Agent 是 client 完全可控的字串,而且沒有任何協定上的長度限制。
// 不截斷等於「任何人都能決定我們資料庫裡一列有多大」——這和 event_logs
// 摘要那個問題是同一件事,只是換一張表。200 bytes 足以辨識瀏覽器與裝置,
// 那正是這個欄位唯一的用途。
const maxUserAgentBytes = 200

// forwardedHeaders 是可能帶著真實 client IP 的標頭,依可信度排序。
//
// CF-Connecting-IP 排前面的理由:Cloudflare 對每個經過它的請求都會**覆寫**
// 這個標頭,所以只要對端確實是 cloudflared,它的值就是邊緣看到的真實來源,
// 而且是單一值、沒有 XFF 那種「這條鏈哪一段可信」的歧義。
var forwardedHeaders = []string{"CF-Connecting-IP", "X-Forwarded-For"}

// deviceInfo 從請求萃取裝置足跡。
//
// peerAddr 是連線對端(host:port);trusted 是願意相信其轉發標頭的來源網段。
func deviceInfo(peerAddr string, header http.Header, trusted []netip.Prefix) DeviceInfo {
	ua, _ := truncateUTF8(header.Get("User-Agent"), maxUserAgentBytes)
	return DeviceInfo{
		UserAgent: ua,
		IP:        clientIP(peerAddr, header, trusted),
	}
}

// clientIP 決定要記哪個 IP。
//
// 預設用連線對端位址:那是 TCP 層的事實,偽造不了。轉發標頭
// (CF-Connecting-IP / X-Forwarded-For)是純粹的請求標頭,任何人都能自己填,
// 所以只有在**對端本身落在 trusted 網段內**時才採信——判準是「這個封包從誰那裡
// 來的」,不是「服務綁在哪個位址」。
//
// 為什麼是網段清單而不是一個開關:開關式的「信任代理」在服務同時能被代理和
// 其他來源連到時就破功了(區網裡任何人自己填一個 CF-Connecting-IP 就能決定
// 稽核紀錄的來源),而稽核紀錄裡一個能自選的「來源」比沒有更糟。比對對端讓
// 這件事由封包本身決定,反向代理跟服務不同機也照樣成立。
func clientIP(peerAddr string, header http.Header, trusted []netip.Prefix) string {
	host := peerHost(peerAddr)
	if peerIsTrusted(host, trusted) {
		for _, name := range forwardedHeaders {
			if ip := leftmostIP(header.Get(name)); ip != "" {
				return ip
			}
		}
	}
	return host
}

// peerHost 去掉對端位址的 port:sessions.ip 存的是位址,不是連線。
func peerHost(peerAddr string) string {
	if host, _, err := net.SplitHostPort(peerAddr); err == nil {
		return host
	}
	return peerAddr
}

// peerIsTrusted 判斷對端是否落在任何一個可信網段內。
// trusted 為空(預設)時永遠是 false——沒設定就不信任何轉發標頭。
func peerIsTrusted(host string, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	// IPv4-mapped IPv6(::ffff:127.0.0.1)要先攤平,否則對不上 IPv4 網段;
	// 雙堆疊監聽下這是常態而不是例外。
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// leftmostIP 取標頭的最左值(整條鏈都可信時,那是原始 client)。
// 解析不出合法 IP 就整個放棄回空字串,讓呼叫端退回連線對端——
// 寧可記一個「不夠精確但為真」的 IP,也不要記一段來路不明的字串。
func leftmostIP(value string) string {
	first, _, _ := strings.Cut(value, ",")
	first = strings.TrimSpace(first)
	if first == "" {
		return ""
	}
	// 有些代理會寫成 host:port。
	if host, _, err := net.SplitHostPort(first); err == nil {
		first = host
	}
	addr, err := netip.ParseAddr(strings.Trim(first, "[]"))
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

// ParseTrustedProxies 解析 HESTIA_TRUSTED_PROXIES:逗號分隔的 CIDR 或裸 IP,
// 空字串回 nil(= 不信任任何轉發標頭,這是安全的預設)。
//
// 允許裸 IP 是因為最常見的設定就是單一位址(本機的 cloudflared),
// 逼人寫 /32 只會製造打錯的機會。
//
// 解析失敗一律回錯而不是略過:設定打錯時「靜靜地少信任一段」會表現成
// 「來源 IP 全部變成代理的位址」,那是幾個月後才會有人發現的那種壞法。
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range strings.Split(s, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, fmt.Errorf("可信代理網段 %q 解析失敗: %w", item, err)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("可信代理位址 %q 解析失敗: %w", item, err)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}
