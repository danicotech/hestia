package identity

import (
	"errors"
	"fmt"
	"strings"
)

// 本地登入(provider='local')的登入名規則。
//
// 這裡只放「什麼形狀算是登入名」,不放驗證通行碼的邏輯 —— 那是雜湊的事
// (internal/shared/secret),與活動層的選手通行碼共用同一份實作。

// LoginNameMinLen / LoginNameMaxLen 是登入名長度界線。
//
// 下限 3 是為了擋掉手滑輸入(單字元登入名也讓暴力猜測的名稱空間小得可笑);
// 上限 64 對齊 identities.provider_user_id 的實際用途 —— 它同時是
// UNIQUE(provider, provider_user_id) 的一半,沒有理由讓它變成一段長文。
const (
	LoginNameMinLen = 3
	LoginNameMaxLen = 64
)

// ErrInvalidLoginName 是登入名不符規則(**只在建立帳號時**回給管理者)。
//
// 登入路徑不回這個:那條路上任何失敗都只能是 ErrInvalidCredentials,
// 否則「這個名字格式不合法」與「這個名字不存在」就成了兩種可分辨的回應。
var ErrInvalidLoginName = errors.New("登入名格式不正確")

// NormalizeLoginName 把輸入整理成可比對的登入名:去空白、轉小寫。
//
// 為什麼一定要正規化:UNIQUE(provider, provider_user_id) 是**位元組相等**,
// 不正規化的話 "Judge" 與 "judge" 是兩個帳號,而使用者會認為它們是同一個。
// 更糟的是這種重複只在有人真的打錯大小寫時才顯形,而那時兩個帳號都已經
// 各自累積了稽核紀錄。
//
// 大小寫折疊用 strings.ToLower(而不是只在查詢時 lower(...)):正規化的權威
// 必須在寫入前,索引才用得到,也才只有一個答案。
func NormalizeLoginName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateLoginName 正規化並檢查形狀,回傳可直接入庫的值。
//
// 字元集限制成 a-z 0-9 . _ -:登入名會被念出來、被貼進 Discord、被寫在
// 指令列上。允許空白與全形字元只會製造「明明看起來一樣卻登不進去」的客訴,
// 而它換來的表達力是零 —— 顯示名稱要好看,那是 users.display_name 的事。
func ValidateLoginName(s string) (string, error) {
	name := NormalizeLoginName(s)
	if len(name) < LoginNameMinLen || len(name) > LoginNameMaxLen {
		return "", fmt.Errorf("%w: 長度要在 %d~%d 之間",
			ErrInvalidLoginName, LoginNameMinLen, LoginNameMaxLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return "", fmt.Errorf("%w: 只能用 a-z 0-9 . _ -,出現了 %q", ErrInvalidLoginName, r)
		}
	}
	return name, nil
}
