// Package activityerr 放**多個活動層套件都必須產生**的錯誤 sentinel。
//
// # 為什麼需要一個專門的套件
//
// 活動層的相依是有方向的:match 位在最上層(它 import betting、handicap、
// tournament、bracket、bp 來編排一場比賽的生命週期),betting 則誰都不 import。
// 「場次」這個概念的擁有者是 match,但 handicap 與 betting 也都要回報
// 「你給的場次不存在」—— 而它們不能 import match,那會成環。
//
// 結果是同一句話被宣告了三次。那不只是重複:在入口層,一個 sentinel 對一個
// reason,三份宣告就會變成三個對外錯誤代碼,呼叫端得分辨三種其實一模一樣的
// 情況;而在領域層,呼叫端得寫 errors.Is(err, a) || errors.Is(err, b) 才判得出
// 一件事 —— 那正是 signup 當初出現過的形狀。
//
// 所以這裡只收**沒有任何可 import 的擁有者**的 sentinel。
//
// # 收錄標準(請嚴格遵守,否則這裡會變成雜物間)
//
// 一個 sentinel 要進來,兩個條件都必須成立:
//
//  1. 不只一個活動層套件會**產生**它(只有一個套件產生的,留在那個套件裡);
//  2. 它的概念擁有者位在相依圖上層,因而產生者 import 不到。
//
// 條件 2 是關鍵:tournament 位在下層,誰都 import 得到,所以
// 「賽事不存在」留在 tournament,不搬進來。
package activityerr

import "errors"

var (
	// ErrMatchNotFound 表示場次不存在。
	ErrMatchNotFound = errors.New("場次不存在")

	// ErrPlayersNotSet 表示場次雙方尚未確定(等上一輪打完),
	// 所以還不能開盤、發預算或下注。
	ErrPlayersNotSet = errors.New("場次雙方尚未確定")
)
