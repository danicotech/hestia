package tournament

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
)

// slug 的長度界線。
//
// 下限 3 是為了擋掉手滑送出的單字元;上限 64 是「還能放進網址與公告標題」
// 的實際界線 —— 沒有 DB 約束擋這件事(欄位是 TEXT),所以這裡是唯一的那道。
const (
	minSlugLen = 3
	maxSlugLen = 64
)

// slugPattern 是賽事代號的形狀:小寫英數,以單一連字號分段。
//
// 為什麼不接受大寫與底線:slug 會出現在網址與 Discord 公告裡,而網址的
// 大小寫在不同地方的處理不一致(有的正規化、有的不會)。收斂成一種寫法,
// 「2026-Baiye 跟 2026-baiye 是不是同一屆」這個問題就不存在。
//
// 首尾與連續的連字號一併擋掉:它們產生的是看起來一樣但其實不同的代號。
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// CreateParams 是開一屆新賽事。
type CreateParams struct {
	// CommunityPublicID 是這屆賽事掛在哪個社群底下。
	CommunityPublicID string
	// Slug 是網址用的短代號,全域唯一,**建立後不可改**
	// (它會出現在已經發出去的公告網址裡)。
	Slug string
	Name string
	// SignupBonus 是報名獎勵(平台代幣),0 = 不發。負數會被拒絕。
	SignupBonus int64
	// Config 是規則旋鈕,每一項留 nil 就用預設值(見 NewConfig)。
	Config ConfigOverrides
	// ActorUserID 是執行的裁判。必填 —— 這個動作要進 admin_audit_logs。
	ActorUserID int64
	Reason      string
}

// CreateResult 是一屆新賽事開出來的結果。
type CreateResult struct {
	View
	// HandicapItemCount 是本屆複製進去的讓武項目數。
	//
	// 回這個數字是為了讓「目錄有沒有進去」在建立當下就有答案。
	HandicapItemCount int
}

// Create 開一屆新賽事,停在報名期。
//
// # 它同時做了兩件事,而那是刻意的
//
// 建立賽事列與複製讓武目錄由 Repo.Create 在**同一個 transaction** 內完成。
// 拆成兩支方法(先建賽事、再安裝項目)看起來更乾淨,代價是多出一個
// 「賽事存在但目錄是空的」的中間狀態,而那個狀態沒有任何東西會發現 ——
// 它要等到選手走到選讓武那一步才顯現,那時候已經報名、評段、抽籤完了。
//
// # 階段固定是 signup
//
// 不開成參數:一屆從「已經在評段中」開始的賽事是沒有意義的資料,
// 而讓呼叫端指定等於把階段機的第一步交出去。要跳過報名期走 AdvancePhase,
// 那條路上有樂觀鎖也有稽核紀錄。
func (s *Service) Create(ctx context.Context, p CreateParams) (*CreateResult, error) {
	if p.ActorUserID == 0 {
		return nil, ErrActorRequired
	}
	slug, err := ValidateSlug(p.Slug)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: 賽事名稱必填", ErrInvalidRequest)
	}
	// signup_bonus 有 CHECK (>= 0)。在這裡先擋是為了讓錯誤訊息是人話:
	// 撞 CHECK 會讓整筆 tx 進 aborted,而回給裁判的是資料庫的用語。
	if p.SignupBonus < 0 {
		return nil, fmt.Errorf("%w: 報名獎勵不可為負數,得到 %d", ErrInvalidRequest, p.SignupBonus)
	}
	// 空字串與「查無此社群」分開:前者是請求漏填,後者是填了但不存在。
	community := strings.TrimSpace(p.CommunityPublicID)
	if community == "" {
		return nil, fmt.Errorf("%w: 缺少社群", ErrInvalidRequest)
	}

	// 設定壞掉在這裡是致命的(與 ParseConfig 相反,理由見 NewConfig)。
	cfg, err := NewConfig(p.Config)
	if err != nil {
		return nil, err
	}
	raw, err := MarshalConfig(cfg)
	if err != nil {
		return nil, err
	}

	// 讓武目錄的唯一權威是 handicap 套件內嵌的那一份。在這裡取而不是讓
	// adapter 自己去讀:「這一屆要裝哪些項目」是領域決策,adapter 只負責寫。
	items, err := handicap.SeedItems()
	if err != nil {
		return nil, fmt.Errorf("讀讓武項目目錄: %w", err)
	}

	t, err := s.repo.Create(ctx, CreateTournamentParams{
		CommunityPublicID: community,
		Slug:              slug,
		Name:              name,
		SignupBonus:       p.SignupBonus,
		ConfigRaw:         raw,
		HandicapItems:     items,
		ActorUserID:       p.ActorUserID,
		Reason:            p.Reason,
	})
	if err != nil {
		return nil, err
	}

	// 回傳的 Config 用剛剛算出來的那一份,不重新解析 t.ConfigRaw:
	// 兩者必然相同(MarshalConfig 與 ParseConfig 互為反函式,config_test 釘住),
	// 而重新解析只會多一條「解析失敗時要怎麼辦」的分支。
	return &CreateResult{
		View:              View{Tournament: t, Config: cfg},
		HandicapItemCount: len(items),
	}, nil
}

// ValidateSlug 檢查並正規化賽事代號。
//
// 正規化只做去空白與轉小寫:那兩件事是打字的意外,拒絕它們只會讓裁判
// 重打一次同樣的東西。其餘一律拒絕而不是「幫他修掉」—— 靜靜把
// `2026--baiye` 改成 `2026-baiye` 的話,他存下來的網址會是打不開的那個。
func ValidateSlug(s string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(s))
	if n := len(slug); n < minSlugLen || n > maxSlugLen {
		return "", fmt.Errorf("%w: 長度要在 %d..%d 之間,得到 %d",
			ErrInvalidSlug, minSlugLen, maxSlugLen, n)
	}
	if !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("%w: 只能用小寫英數與單一連字號(如 2026-baiye-shifeng),得到 %q",
			ErrInvalidSlug, slug)
	}
	return slug, nil
}
