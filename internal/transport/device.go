package transport

import (
	"net"
	"net/http"
	"strings"
)

// 裝置足跡的取值與清理。兩個欄位都是**外界給的字串**,進 sessions 表之前
// 由入口層負責把它們變安全:UA 截斷、IP 不輕信 header。

// maxUserAgentBytes 是寫進 sessions.user_agent 的上限。
//
// User-Agent 是 client 完全可控的字串,而且沒有任何協定上的長度限制。
// 不截斷等於「任何人都能決定我們資料庫裡一列有多大」——這和 event_logs
// 摘要那個問題是同一件事,只是換一張表。200 bytes 足以辨識瀏覽器與裝置,
// 那正是這個欄位唯一的用途。
const maxUserAgentBytes = 200

// deviceInfo 從請求萃取裝置足跡。
//
// peerAddr 是 connect 給的連線對端(host:port);trustProxy 決定要不要
// 相信 X-Forwarded-For。
func deviceInfo(peerAddr string, header http.Header, trustProxy bool) DeviceInfo {
	ua, _ := truncateUTF8(header.Get("User-Agent"), maxUserAgentBytes)
	return DeviceInfo{
		UserAgent: ua,
		IP:        clientIP(peerAddr, header, trustProxy),
	}
}

// clientIP 決定要記哪個 IP。
//
// 預設用連線對端位址:那是 TCP 層的事實,偽造不了。
// X-Forwarded-For **只有在明確開啟時**才讀——它是純粹的請求標頭,
// 任何人都能自己填一個,直接信任等於讓來源 IP 變成可自選欄位
// (稽核紀錄裡一個能自選的「來源」比沒有更糟)。開啟的前提是
// 服務只接受來自可信反向代理的連線,由部署保證。
func clientIP(peerAddr string, header http.Header, trustProxy bool) string {
	if trustProxy {
		if ip := leftmostForwarded(header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(peerAddr); err == nil {
		return host
	}
	return peerAddr
}

// leftmostForwarded 取 X-Forwarded-For 的最左值(整條鏈都可信時,那是原始 client)。
// 解析不出合法 IP 就整個放棄回空字串,讓呼叫端退回連線對端——
// 寧可記一個「不夠精確但為真」的 IP,也不要記一段來路不明的字串。
func leftmostForwarded(value string) string {
	first, _, _ := strings.Cut(value, ",")
	first = strings.TrimSpace(first)
	if first == "" {
		return ""
	}
	// 有些代理會寫成 host:port。
	if host, _, err := net.SplitHostPort(first); err == nil {
		first = host
	}
	ip := net.ParseIP(strings.Trim(first, "[]"))
	if ip == nil {
		return ""
	}
	return ip.String()
}
