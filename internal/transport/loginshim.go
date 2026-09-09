package transport

import (
	"net/http"
	"net/url"
)

// 前端上線前的暫時路由:站台根目錄的 /login 轉到 OAuth 起點。
//
// 為什麼需要它:Discord 的 `/bind` 只能給連結,不能代使用者走 OAuth
// (state 綁在瀏覽器的 HttpOnly cookie 上,bot 代開的連結沒有那份 cookie)。
// 而 stentor 指向的是 `{WEB_BASE_URL}/login` —— 那是**前端**的位置,
// 前端(theatron)還不存在,所以那個連結今天會 404。
//
// 為什麼放在 hestia 而不是別處:它必須在**前端那個 origin** 上才有意義,
// 而前端 origin 現在只有 hestia 在服務(`/api` 底下是 API,其餘留給前端)。
//
// **theatron 上線時整個檔案刪掉。** 那時 /login 是一個真的頁面,由前端服務;
// 網址不變,所以已經發出去的連結不會壞。這不是「兩個權威位置」,是接力:
// 同一個網址在任何時刻只有一個實作。
const loginShimPath = "/login"

// withLoginShim 包在 mountAt 外面(前綴還沒被剝掉的那一層),
// 因為 /login 刻意**不在** basePath 底下 —— 它是前端的網址,不是 API 的。
func withLoginShim(basePath string, next http.Handler) http.Handler {
	target := basePath + browserStartPath
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != loginShimPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 查詢字串原封不動帶過去(?redirect=… 要留著)。
		// 不在這裡驗 redirect:start 已經用 identity.CleanRedirect 驗了,
		// 在這裡再寫一次規則,兩份遲早會不一致——而不一致的那一份會是
		// open redirect 的破口。
		to := &url.URL{Path: target, RawQuery: r.URL.RawQuery}
		// 303:原請求是 GET,但語意是「你要的東西在別處」,
		// 而且明確不希望瀏覽器把它記成永久搬遷(這條路之後會由前端接手)。
		http.Redirect(w, r, to.String(), http.StatusSeeOther)
	})
}
