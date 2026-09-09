package transport

import (
	"fmt"
	"net/http"
	"strings"
)

// 掛載前綴(HESTIA_BASE_PATH)。
//
// 部署形狀(2026-09-09 定案):前端 https://arena.gengflow.com/,後端掛在
// **同一個 host 的 /api 底下**。同源是刻意的 —— session cookie 因此不跨網域,
// 不需要 SameSite=None(那等於自願放棄 CSRF 的第一道防線),也不需要 CORS 憑證。
//
// 前綴影響三件事,少做任何一件都會壞:
//
//   - 路由:全部端點(ConnectRPC、healthz、瀏覽器登入路由)都要在前綴底下。
//   - cookie 的 Path:寫死 "/" 會讓 cookie 送給同一個 host 上的其他應用;
//     寫死 "/auth" 會讓瀏覽器不把 state cookie 送到 ConnectRPC 的回呼(procedure
//     全名不是 /auth 開頭),登入流程靜默失敗。
//   - 稽核的 action:event_logs 記的必須是 procedure 全名。前綴是部署細節,
//     讓它滲進資料表會使同一支 RPC 在不同部署下長成兩個值(專案鐵則 9)。
//
// **不受影響**的是啟動時的存取層級斷言(verifyProcedureCoverage)與攔截器的
// 清單比對:兩者都用 proto descriptor / connect 的 Spec().Procedure,那是生成碼
// 裡的常數,與 URL 無關。前綴由 http.StripPrefix 在進 connect mux 之前剝掉,
// 所以 req.Spec().Procedure 永遠是 /hestia.platform.v1.X/Y。這一點有測試釘住 ——
// 若哪天它變成從 URL 解析,清單比對就會全部落空,那是 fail open。

// maxBasePathLen 是前綴長度上限。它是部署設定,不是使用者輸入,
// 但一個荒謬的長度只會是設定寫錯,早點炸比較好。
const maxBasePathLen = 64

// NormalizeBasePath 檢查並正規化掛載前綴。
//
// 回傳值的形狀是**唯一**的:空字串(掛在根)或 "/xxx"(開頭有斜線、結尾沒有)。
// 呼叫端因此可以無條件寫 basePath+"/healthz",不必到處判斷要不要補斜線。
//
// 為什麼是「回 error」而不是「靜靜修正」:前綴寫錯的後果是整個 API 掛在錯的
// 位置,而那在測試環境很可能看起來「只是 404」。設定錯誤要在啟動時炸。
// 唯一容忍的是結尾斜線("/api/"),因為它沒有第二種可能的意思。
func NormalizeBasePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", nil
	}
	if len(p) > maxBasePathLen {
		return "", fmt.Errorf("前綴過長(上限 %d):%d", maxBasePathLen, len(p))
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("前綴必須以 / 開頭:%q", p)
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c < 0x21 || c == 0x7f:
			// 控制字元與空白:header 注入與各家瀏覽器解析差異的來源。
			return "", fmt.Errorf("前綴含控制字元或空白:%q", p)
		case c == '?' || c == '#' || c == '\\' || c == '%':
			// query / fragment / 反斜線 / 百分比編碼都不是路徑前綴該有的東西。
			return "", fmt.Errorf("前綴含不合法字元 %q:%q", string(c), p)
		}
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("前綴的路徑片段不合法:%q", p)
		}
	}
	return p, nil
}

// stripBasePath 把前綴從路徑上剝掉,回傳剩下的部分。
// ok=false 代表這條路徑根本不在前綴底下(呼叫端據此決定 404 或記成非 RPC 路徑)。
func stripBasePath(path, basePath string) (string, bool) {
	if basePath == "" {
		return path, true
	}
	rest, ok := strings.CutPrefix(path, basePath)
	if !ok {
		return path, false
	}
	if rest == "" {
		return "/", true
	}
	// 邊界必須落在路徑片段上:前綴 /api 不該吃掉 /apifoo。
	if rest[0] != '/' {
		return path, false
	}
	return rest, true
}

// mountAt 把 handler 掛到前綴底下:剝掉前綴再往內送。
//
// 前綴為空時**原樣回傳** —— 「沒設前綴」必須與加這個功能之前一模一樣,
// 連一層包裝都不要多(既有測試因此不會走到任何新程式碼)。
//
// 為什麼不用 http.StripPrefix:它只比字串前綴,/api 會吃掉 /apifoo 並把
// "foo" 往內送。那條路目前只會 404,但「前綴的邊界必須落在路徑片段上」
// 是這一層該負責的事,不該靠內層剛好不匹配來補。
func mountAt(basePath string, next http.Handler) http.Handler {
	if basePath == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := stripBasePath(r.URL.Path, basePath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		r2 := *r
		u := *r.URL
		u.Path = rest
		// RawPath 是「原始編碼形式」的快取,Path 一改它就不再對應;
		// 清掉讓 EscapedPath() 重新由 Path 推導(與 http.StripPrefix 同樣做法)。
		u.RawPath = ""
		r2.URL = &u
		next.ServeHTTP(w, &r2)
	})
}
