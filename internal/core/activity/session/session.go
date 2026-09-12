// Package session 是《百業試鋒》選手那一軌的 session。
//
// # 它在雙軌身分裡的位置
//
// 報名不需要平台帳號(grill Q12),所以選手的憑證不是 Discord OAuth 而是
// 遊戲ID + 通行碼。signup.Service.Login 只回答「你是不是這位選手」,
// **換發憑證是這個套件的事**:
//
//	signup.Login  證明身分(比對通行碼雜湊)
//	session.Issue 簽發 session token(本套件)
//	session.Verify 每次請求驗證(本套件)
//
// 分成兩個套件是刻意的:通行碼驗證本來就能在沒有任何簽章金鑰的情況下被測完,
// 把簽發塞進 signup 會讓它平白多一個相依。
//
// # 這裡不寫第二套 HMAC
//
// 「我們自己簽的 token 長什麼樣」的唯一權威是 identity.Signer
// (v1.<claims>.<sig>,用途混進被簽訊息,所以跨用途一定簽章不符)。
// 本套件只是帶著自己的用途常數與 claims 去用它 —— 見 identity.Signer.Sign
// 的註解,那裡寫了為什麼是「匯出通用原語」而不是「在平台層加活動層函式」。
//
// # 無狀態,但不是不可撤銷
//
// session 不落任何儲存:沒有 session 表就沒有清理 job、沒有重啟遺失。
// 撤銷靠的是 claims 裡的 pat 與 tournament_players.passcode_issued_at 對帳
// (見 Service.Verify)—— 裁判重新產生通行碼的那一刻,用舊碼登入拿到的
// session 就失效了,不需要一張會被忘記清的撤銷清單。
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// TokenType 是這組 token 的用途標籤,會被混進被簽的訊息。
//
// 值必須與平台的 access / state 不同,而且 identity 會主動擋掉那兩個保留字
// (checkCustomTyp)—— 於是「拿 access token 當選手 session 用」在結構上
// 就是簽章不符,不靠任何欄位比對。
const TokenType = "activity_player_session"

// TTL 是選手 session 的壽命。
//
// 12 小時的尺度是「一個賽事晚上」:選手傍晚登入選讓武、打完決賽大概就是這個
// 長度。再長沒有意義(通行碼隨時能重新登入),再短會讓人在第三輪之間被登出。
//
// 刻意**沒有** refresh 那一半:平台 session 需要輪替是因為它的 refresh token
// 活 30 天,而這裡最長效的憑證是通行碼本身,由裁判掌握。少一個長效憑證就少
// 一個要保護的東西。
//
// 這是 token 壽命的唯一權威。transport 那個同名的 fallback 常數只在
// 「實作沒回到期時間」時才會被用到,兩者不是同一個概念。
const TTL = 12 * time.Hour

// Identity 是一個選手 session 證明的身分。
//
// 兩個欄位都是**對外識別字**,沒有任何內部 BIGINT id(專案鐵則 5):
// token 由瀏覽器持有,內部 id 進 token 就等於進了對外契約。
// 內部 id 由呼叫端每次用 public_id 反查 —— 那也順便讓「選手被刪掉/棄賽後
// 舊 token 還能用」不可能發生。
//
// 這個型別與 transport.ActivityIdentity 長得一樣但刻意各自宣告:
// core 不 import transport(那是反向依賴),而活動層遲早搬去 themis。
// 兩者之間的轉接是組裝層那幾行 struct literal。
type Identity struct {
	// TournamentSlug 是這個 session 屬於哪一屆。
	//
	// 必要而不是冗餘:通行碼是逐屆發的,一個 session 只該對一屆有效。
	// 少了它,舊屆的 token 就能拿來操作新一屆的讓武。
	TournamentSlug string
	// PlayerPublicID 是選手的 public_id(ULID)。
	PlayerPublicID string
}

// Valid 回報這是不是一個完整的身分。兩個欄位缺一不可。
func (id Identity) Valid() bool {
	return id.TournamentSlug != "" && id.PlayerPublicID != ""
}

// claims 是被簽的內容。
//
// 欄位名一律三個字母,不是為了省那幾個 byte,而是因為它們會出現在每一個
// 請求的 cookie 裡;短名字讓 token 穩定落在幾百 bytes 而不是逼近上限。
//
// 不匯出:claims 的形狀是這個套件的內部決定,對外的契約是 Identity。
type claims struct {
	Tnt string `json:"tnt"` // tournament slug
	Plr string `json:"plr"` // player public_id(ULID)
	// Pat 是簽發當下的 passcode_issued_at(UnixMicro)。
	//
	// 這是「換發通行碼即失效」的全部機制,見 Service.Verify。
	// 用微秒而不是秒:Postgres 的 timestamptz 就是微秒精度,而截到秒的話,
	// 同一秒內的兩次換發會產生相同的值 —— 那一格的舊 session 會活下來。
	Pat int64 `json:"pat"`
	Iat int64 `json:"iat"`
	Exp int64 `json:"exp"`
}

// 錯誤語意。
//
// ErrPasscodeRotated 與 ErrPlayerGone **包著 identity.ErrInvalidToken**:
// 對呼叫端來說這三件事都是「這張憑證不能用了,請重新登入」,errmap 已經
// 把 ErrInvalidToken 映射成 Unauthenticated,不需要第二套錯誤字典。
// 各自有名字則是為了伺服器端分得出成因 —— 分得出來但不說出去。
var (
	// ErrPasscodeRotated 是這張 session 簽發之後,通行碼被重新產生過。
	ErrPasscodeRotated = fmt.Errorf("通行碼已重新產生,舊 session 失效: %w", identity.ErrInvalidToken)
	// ErrPlayerGone 是 token 裡的選手在這一屆已經查不到(棄賽、被刪、換屆)。
	ErrPlayerGone = fmt.Errorf("選手不在這一屆: %w", identity.ErrInvalidToken)
	// ErrInvalidIdentity 是簽發時給了不完整的身分。
	//
	// 刻意**不包** identity.ErrInvalidToken:簽發端是我們自己的程式碼,
	// 這是 bug 不是憑證問題,不該被映射成「請重新登入」。
	ErrInvalidIdentity = errors.New("活動層身分不完整")
	// ErrPasscodeTimeUnavailable 是 Repo 回了零值時間。
	//
	// passcode_issued_at 是 NOT NULL DEFAULT now(),所以零值只可能是實作
	// 有問題。**寧可簽不出來也不簽一張永遠不會失效的 token** ——
	// pat 對不上任何東西的 session 等於沒有撤銷機制。
	ErrPasscodeTimeUnavailable = errors.New("取不到通行碼簽發時間")
)

// Repo 是本套件對儲存層的唯一需求。
//
// 只有一個方法是刻意的:這一層不需要選手的任何其他欄位(顯示名稱、段位、
// 狀態都由各 handler 自己去查),多要一個欄位就是多一份會過期的副本。
type Repo interface {
	// PasscodeIssuedAt 取本屆某位選手目前的 activity.tournament_players.passcode_issued_at。
	//
	// 查無(slug 不存在、選手不存在、public_id 不屬於這一屆)一律回
	// tournament.ErrPlayerNotFound —— 呼叫端會把它折成 ErrPlayerGone,
	// **不得**讓呼叫端分辨是哪一種查無(那會讓 token 變成一支查詢介面)。
	//
	// 實作必須原樣回傳資料庫的值,不可用本地時鐘補任何東西:
	// 這個值會被逐位元組比對,差一微秒就是全部選手當場登出。
	PasscodeIssuedAt(ctx context.Context, tournamentSlug, playerPublicID string) (time.Time, error)
}
