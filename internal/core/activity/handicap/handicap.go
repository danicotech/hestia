// Package handicap 是讓武 BP 的選購 ——《百業試鋒》的核心玩法。
//
// # 讓武不是放水
//
// 低段位者花 BP 換到的是「高手必須遵守的正式比賽條件」,不是要求高手故意失誤。
// 受限方仍應以取勝為目的全力應戰。這句話不只是文案 —— 它決定了這個 package
// 只有「施加限制」與「退掉限制」,沒有任何關於勝負、認輸、放水的邏輯。
//
// # BP 不是貨幣,所以這裡沒有冪等鍵
//
// 平台鐵則是「動錢一律要 Idempotency-Key」。這裡刻意沒有,因為 BP 的四個性質
// 沒有一個符合貨幣:每輪依段位差重發、沒花完即作廢、不可交易、不可累積。
// 重複扣一次 BP 的後果是使用者看到數字不對(選手自己封盤前退掉即可),
// 不是對不上帳(那個無法事後修復)。詳見 migrations/activity/00002_handicap.sql。
//
// 真正動錢的是報名獎勵、賽事獎金與下注派彩,那三處才走 Ledger 與冪等鍵。
//
// # 系統不做互斥檢查
//
// 只要 BP 夠就能買,同一項目也能重複買(handicap_selections 刻意沒有 UNIQUE 約束)。
// victory 類同時生效會互相矛盾(「撐過 90 秒即獲勝」+「時間結束即勝利」),
// 但那由裁判臨場判 —— 這裡只回一個**非阻擋式**的 advisory 字串,購買照樣成立。
// 擋下來才是真的壞事:系統擋不完所有矛盾組合,擋一半會讓選手誤以為沒被擋的都相容。
//
// # 封盤前後的可見性由伺服器守
//
// 封盤前只有施加者本人看得到自己買了什麼;封盤後雙方與觀眾全部看得到。
// 這條界線寫在 MatchHandicaps 的組裝邏輯裡,不是前端隱藏 ——
// 前端隱藏等於沒隱藏,對手打開 devtools 就知道自己會被禁什麼。
//
// 唯一的例外是 RefereeMatchHandicaps:裁判要執行的規則就寫在買下的項目裡
// (「禁用奇術」的 referee_note 要求裁判確認後返還另一項的 BP),封盤前看不到
// 內容的話那條規則在場上沒有人執行得了。例外開在一支**另外命名**的方法上,
// 不是給 MatchHandicaps 加一個 bool 參數 —— 前者漏掛權限是編譯得過但
// 呼叫點顯眼的錯,後者是一個容易被複製貼上帶過去的 true。
//
// # 分層
//
// 這個 package 只認標準庫、activity/bp 與 activity/rules(抽選池的定義在那裡)。
// 持久化以 Repository 介面表達,
// 方法刻意貼著單一 SQL 敘述設計,讓 pg adapter 不必在裡面再做決策。
package handicap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/rules"
)

// Category 是讓武項目的六大分類,字面值即資料庫 handicap_items.category 的 CHECK 值。
type Category string

const (
	// CategoryWeapon 武器限制:指定/隨機武學、禁切武器、禁特效。
	CategoryWeapon Category = "weapon"
	// CategorySkill 技能限制:禁技能、禁奇術、禁普攻、禁處決、改鍵位。
	CategorySkill Category = "skill"
	// CategoryDefense 防禦與位移限制:禁移動、禁解控、禁迴避、禁卸勢、禁防禦、禁跳躍。
	CategoryDefense Category = "defense"
	// CategoryPoison 下毒:讓守關元、打亂方向鍵、互換按鍵、讓半血。
	CategoryPoison Category = "poison"
	// CategoryVictory 勝利條件:改寫本場的判定方式。同時買多項會互相矛盾,見 AdvisoryVictoryConflict。
	CategoryVictory Category = "victory"
	// CategoryRule 規則限制:禁追擊、待角落、先手權、外觀、台詞。
	CategoryRule Category = "rule"
)

// Valid 回報這個分類是否為六大類之一。
func (c Category) Valid() bool {
	switch c {
	case CategoryWeapon, CategorySkill, CategoryDefense, CategoryPoison, CategoryVictory, CategoryRule:
		return true
	default:
		return false
	}
}

// 場次狀態的字面值。
//
// 這裡刻意**不**定義一個匯出的 MatchStatus 型別 —— 場次狀態的權威屬於賽程
// (activity.matches 的 CHECK 約束),讓武只是它的讀者。在這裡再造一個枚舉,
// 就是同一個概念兩個權威位置(專案鐵則 9)。Match.Status 因此是純字串。
const (
	statusPending = "pending"
	statusReady   = "ready"
	statusLocked  = "locked"
	statusLive    = "live"
	statusDone    = "done"
)

// AdvisoryVictoryConflict 是 victory 類買到第二項時回的非阻擋式提醒。
//
// 回了這個字串**仍然要成功購買**。它是給選手看的一句話,不是錯誤。
const AdvisoryVictoryConflict = "此類項目同時生效可能互相矛盾,請與裁判確認"

// maxTargetNoteRunes 是指定內容的長度上限。
//
// 這不是規則的一部分,是防呆:target_note 封盤後會原文貼進 Discord 公告,
// 沒有上限等於給了選手一個對全場廣播任意長度文字的管道。
const maxTargetNoteRunes = 200

// ── 型別 ────────────────────────────────────────────────────────

// Item 是一個可購買的讓武項目(activity.handicap_items 一列)。
type Item struct {
	ID           int64
	TournamentID int64
	// Ref 是對外識別字串,即 handicap_items.public_id(ULID,migration 00004 補上)。
	//
	// 沒有沿用 token_entries.entry_ref 那個「以十進位 id 當 ref」的先例:
	// 那個例外的成立條件是「最大的表 + 分區表無法由 DB 保證全域唯一」,
	// handicap_items 逐屆 34 列又不分區,兩個條件都不符合。
	Ref string
	// Key 是穩定識別(<category>.<snake_case>,如 weapon.designated_art)。
	// 改名不改 key:目錄同步與程式規則一律以它定址(migration 00006)。
	Key         string
	Category    Category
	Name        string
	Description string
	// RefereeNote 是給裁判看的執行說明(「怎麼確認對手真的遵守了」)。
	// 空字串 = 這一項還沒寫 —— 待補,不是錯誤。
	RefereeNote string
	Cost        int64
	// Repeatable 只供 UI 標示「(可重複)」,**不作強制**:所有項目都能重複買。
	Repeatable bool
	SortOrder  int32
	// Params 是項目自己的參數(handicap_items.params),鍵的定義見 ItemParams。
	Params ItemParams
}

// RequiresTargetNote 回報買這一項時是否必須填指定內容。
//
// 這是**推導值不是欄位**:handicap_items 沒有對應的欄位,判斷完全由項目語意決定,
// 規則寫在 items.go 的 targetNoteRequired。
func (i Item) RequiresTargetNote() bool { return RequiresTargetNote(i.Key) }

// ItemSpec 是 seed JSON 裡的一列項目定義(尚未綁定到任何一屆賽事)。
type ItemSpec struct {
	Category Category `json:"category"`
	// Key 見 Item.Key。loadSeed 驗它唯一、形狀合法且前綴等於 Category
	// (與 DB 的 handicap_items_key_format_check 同一條規則)。
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// RefereeNote 是指標而不是字串:目錄補齊的過程中,「還沒寫」與
	// 「寫了空字串」是兩件事。
	RefereeNote *string    `json:"referee_note"`
	Cost        int64      `json:"cost"`
	Repeatable  bool       `json:"repeatable"`
	SortOrder   int32      `json:"sort_order"`
	Params      ItemParams `json:"params"`
}

// ItemParams 是讓武項目的參數(handicap_items.params JSONB)。
//
// **這裡是 params 鍵的唯一定義處。** JSONB 不是垃圾桶:解析時未知鍵一律回錯
// (seed 與 DB 讀回都驗),所以任何新鍵都得先在這裡登記並說明意義。
//
//	seconds          「撐過 N 秒即獲勝」的 N。0 = 這一項沒有秒數。
//	draw             封盤時系統要代抽的東西:DrawWuxue / DrawDirectionKeys。空 = 不抽。
//	applies_to_both  對施加方同樣生效(「雙方使用 46 級武庫」),設定確認清單要看兩邊。
type ItemParams struct {
	Seconds       int64  `json:"seconds,omitempty"`
	Draw          string `json:"draw,omitempty"`
	AppliesToBoth bool   `json:"applies_to_both,omitempty"`
}

// Draw 的合法值。字面值即 catalogue.json 與 DB 裡的字串。
const (
	// DrawWuxue 從賽事 config 的 handicap.draw_pools.wuxue 抽一個武學名稱(「隨機武學」)。
	// 值就是池名:池的名字只在 rules 定義一次,這裡引用而不是再寫一個 "wuxue"。
	DrawWuxue = rules.PoolWuxue
	// DrawDirectionKeys 對上下左右抽一個非恆等的排列(「打亂方向鍵」)。
	// 不需要候選清單:四個鍵是固定的。
	DrawDirectionKeys = "direction_keys"
)

// Validate 檢查值域。鍵的存在與否由 UnmarshalJSON 的 DisallowUnknownFields 擋。
func (p ItemParams) Validate() error {
	if p.Seconds < 0 {
		return fmt.Errorf("%w: seconds 不得為負,實際 %d", ErrInvalidItemParams, p.Seconds)
	}
	switch p.Draw {
	case "", DrawWuxue, DrawDirectionKeys:
		return nil
	default:
		return fmt.Errorf("%w: 未知的 draw %q", ErrInvalidItemParams, p.Draw)
	}
}

// UnmarshalJSON 嚴格解析:未知鍵回錯、值域不合回錯。
//
// 掛在型別上而不是另寫一支 parse 函式,是為了讓 seed(經 ItemSpec)與 adapter
// (經 ParseItemParams)走**同一條**驗證 —— 兩條路遲早會有一條漏掉新鍵。
func (p *ItemParams) UnmarshalJSON(data []byte) error {
	type raw ItemParams // 去掉方法,免得遞迴
	var r raw
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidItemParams, err)
	}
	// 一份 JSON 只該有一個值:後面還有東西代表不是單一物件。
	if dec.More() {
		return fmt.Errorf("%w: params 後面有多餘內容", ErrInvalidItemParams)
	}
	out := ItemParams(r)
	if err := out.Validate(); err != nil {
		return err
	}
	*p = out
	return nil
}

// ParseItemParams 把 DB 讀回的 JSONB 文字解成 ItemParams。空輸入視同 {}。
func ParseItemParams(data []byte) (ItemParams, error) {
	var p ItemParams
	if len(bytes.TrimSpace(data)) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(data, &p); err != nil {
		// json.Unmarshal 先做整段語法檢查才呼叫 UnmarshalJSON,那一層的錯還沒被包過。
		if errors.Is(err, ErrInvalidItemParams) {
			return ItemParams{}, err
		}
		return ItemParams{}, fmt.Errorf("%w: %w", ErrInvalidItemParams, err)
	}
	return p, nil
}

// JSON 是寫進 DB 的形狀。omitempty 讓沒有參數的項目存成 {},而不是三個零值 ——
// 那樣 DB 裡的每一列都會長出 "seconds": 0,讀的人得先知道 0 代表「沒有」。
func (p ItemParams) JSON() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("序列化讓武參數: %w", err)
	}
	return string(b), nil
}

// Match 是讓武需要知道的那部分場次資料。
//
// adapter 以 matches JOIN tournaments(取 config 的 bp.per_rank_gap)
// JOIN tournament_players ×2(取雙方段位與 public_id)一次撈齊 ——
// 讓武的每個判斷都同時要用到狀態、段位與設定,分三次查會讓它們來自不同的快照。
type Match struct {
	ID           int64
	PublicID     string
	TournamentID int64
	// Status 是 activity.matches.status 的原值:pending / ready / locked / live / done。
	Status string
	// HandicapOpen 是裁判手動開盤的開關,不綁時鐘。
	HandicapOpen bool
	// LockedAt 為 nil 表示尚未封盤。封盤不可逆。
	LockedAt *time.Time
	// P1PlayerID / P2PlayerID 為 0 表示該側尚未確定(等上一輪)。
	P1PlayerID       int64
	P2PlayerID       int64
	P1PlayerPublicID string
	P2PlayerPublicID string
	P1Rank           bp.Rank
	P2Rank           bp.Rank
	// PerRankGap 取自 tournaments.config.bp.per_rank_gap;0 時 bp 套件會退回預設值 8。
	PerRankGap int64
}

// Budget 是某人在某場的 BP 預算(activity.match_budgets 一列)。
//
// **只有低段位者有列**。同段或高段者不建列 = 本場無讓武,
// 這樣「有沒有讓武」在資料上就分得出來,不必靠 budget=0 這種需要解讀的值。
type Budget struct {
	MatchID        int64
	PlayerID       int64
	PlayerPublicID string
	Budget         int64
	// Spent 是衍生資料,恆等於未作廢 selections 的 cost 總和。
	Spent int64
}

// Remaining 是剩餘可用 BP,永不為負。
func (b Budget) Remaining() int64 { return bp.Remaining(b.Budget, b.Spent) }

// Selection 是一次已成立的讓武選擇(activity.handicap_selections 一列 + 項目快照)。
//
// 每買一次一列 —— 同一項目買三次就是三列,不是一列 qty=3。
// 這樣每次購買都留有自己的價格快照與指定內容。
type Selection struct {
	ID            int64
	PublicID      string
	MatchID       int64
	MatchPublicID string
	PlayerID      int64
	ItemID        int64
	// ItemRef / ItemKey / ItemName / Category / ItemParams 是項目快照,
	// 由 adapter JOIN handicap_items 填入,前端顯示一張清單時不必再逐項回查。
	ItemRef    string
	ItemKey    string
	ItemName   string
	Category   Category
	ItemParams ItemParams
	// ItemRefereeNote 是項目的執行說明,跟著選擇走:設定確認清單(Checklist)
	// 就是它加上抽選結果。對外只到裁判端,選手端的轉換不輸出它。
	ItemRefereeNote string
	// ItemSortOrder 是項目在目錄裡的順序,Checklist 依它排 —— 裁判逐項對照時
	// 看到的順序要跟目錄一致,不是誰先買誰在前。
	ItemSortOrder int32
	// Cost 是購買當下的價格快照。項目改價不影響已成立的選擇。
	Cost       int64
	TargetNote string
	Voided     bool
	CreatedAt  time.Time
	// DrawResult / DrawnAt 是封盤時系統抽選的結果,只有 ItemParams.Draw 非空的
	// 項目會有值。nil = 尚未封盤(或這一項不抽)。兩者同生共死(DB CHECK)。
	DrawResult *string
	DrawnAt    *time.Time
}

// NewSelection 是寫入一列 handicap_selections 所需的全部欄位。
//
// public_id(ULID)由 adapter 產生 —— core 不認識任何 ULID 套件,
// 而 SQL 也生不出 ULID,所以這是唯一合理的分工。
type NewSelection struct {
	MatchID    int64
	PlayerID   int64
	ItemID     int64
	Cost       int64
	TargetNote string
}

// ── 對外結果型別 ────────────────────────────────────────────────

// SelectParams 是一次購買請求。
//
// 刻意沒有冪等鍵(見 package 說明)。PlayerID 來自活動層 session,
// **不由請求帶** —— 讓請求指定施加者等於讓任何人花別人的 BP。
type SelectParams struct {
	MatchPublicID string
	PlayerID      int64
	ItemRef       string
	TargetNote    string
}

// SelectResult 是一次購買的結果。
type SelectResult struct {
	Selection Selection
	// Budget 是買完後的預算,前端不必再查一次。
	Budget Budget
	// Advisory 是非阻擋式提醒;非空**不代表失敗**,購買已經成立。
	Advisory string
}

// VoidParams 是一次退選請求。PlayerID 同樣來自 session。
type VoidParams struct {
	SelectionPublicID string
	PlayerID          int64
}

// RefundResult 是一次退選的完整結果(自助與裁判代退共用同一個形狀)。
//
// 帶上場次與被退掉的那一筆,是因為裁判代退的呼叫端要拿它去寫稽核紀錄:
// 請求只帶 selection_public_id,而稽核要記「哪一場、退了什麼、值多少 BP」。
// 讓呼叫端自己再查一次,就是同一份資料在兩個時間點各讀一次 —— 中間那一瞬間
// 的差異會變成一筆說謊的稽核紀錄。
type RefundResult struct {
	// MatchID 是內部 id,給同 tx 的呼叫端當稽核 target(對外一律用 public_id)。
	MatchID       int64
	MatchPublicID string
	// Selection 是被退掉的那一筆,Voided 已為 true。
	Selection Selection
	// Budget 是退款後的預算,Spent 已經扣掉那一筆。
	Budget Budget
}

// MyBudget 是選手自己看到的本場讓武狀態。
type MyBudget struct {
	// HasBudget 為 false 時 Budget 是零值 —— 表示同段對決或自己是高段位方。
	HasBudget  bool
	Budget     Budget
	Selections []Selection
	// Editable 還能不能改。
	Editable bool
	// LockedReason 是 Editable 為 false 時的人話說明,前端直接顯示。
	LockedReason string
}

// MatchHandicaps 是一場比賽的讓武全貌。
//
// 可見性在這裡決定,不在前端:Revealed 為 false 時 Selections 只會包含請求者
// 自己買的(非施加者本人則為空),且 Budget 為 nil —— 對手在封盤前連
// 「已經花了多少」都不該知道,那會洩漏對方還剩幾 BP 可用。
//
// RefereeMatchHandicaps 是那句話唯一的例外:它在 Revealed 為 false 時也填滿
// Selections 與 Budget。所以 **Revealed 不能拿來當「這份內容可以公開嗎」**——
// 它的意思一直是「已封盤」,兩者只在選手與觀眾那條路上剛好一致。
type MatchHandicaps struct {
	MatchPublicID string
	Status        string
	LockedAt      *time.Time
	// HolderPlayerPublicID 是施加者(低段位方);空 = 本場無讓武。
	HolderPlayerPublicID string
	// ConstrainedPlayerPublicID 是受限方(高段位方);空 = 本場無讓武。
	ConstrainedPlayerPublicID string
	Budget                    *Budget
	Selections                []Selection
	// Revealed = 已封盤、內容對所有人公開。
	Revealed bool
}

// ChecklistEntry 是「開賽前設定確認」清單的一列(schemas/20 裁判動線)。
//
// 清單是**推導值,不另存**:matches.setup_confirmed_at 只記「誰、什麼時候」按了確認,
// 內容永遠從當下的選擇重算。這裡就是那個重算。
type ChecklistEntry struct {
	SelectionPublicID string
	ItemKey           string
	ItemName          string
	// RefereeNote 是裁判要執行的說明;空 = 目錄還沒寫。
	RefereeNote string
	// TargetNote 是買方指定的內容(哪個武學、哪兩個鍵)。
	TargetNote string
	// DrawResult 是封盤時抽出的結果;nil = 這一項不抽。
	DrawResult *string
	// AppliesToBoth 為 true 時裁判要看兩邊,只看對手是漏的。
	AppliesToBoth bool
}

// Checklist 依 Selections 組出設定確認清單:未作廢的每一筆一列,依分類、購買順序排。
//
// 分類順序沿用 SQL 的 ORDER BY category(字面值排序),與選購頁的項目順序一致 ——
// 裁判對著清單找項目時,兩張表長得一樣才不必來回對。分類內維持 Selections 的順序
// (adapter 依 created_at, id 排,即購買順序)。
func (mh *MatchHandicaps) Checklist() []ChecklistEntry {
	sels := make([]Selection, 0, len(mh.Selections))
	for _, s := range mh.Selections {
		if !s.Voided {
			sels = append(sels, s)
		}
	}
	// 分類 → 項目在目錄裡的順序 → 購買順序(SliceStable 保住最後那層):
	// 裁判逐項對照時看到的順序要跟目錄一致,不是誰先買誰在前。
	sort.SliceStable(sels, func(i, j int) bool {
		if sels[i].Category != sels[j].Category {
			return sels[i].Category < sels[j].Category
		}
		return sels[i].ItemSortOrder < sels[j].ItemSortOrder
	})

	out := make([]ChecklistEntry, 0, len(sels))
	for _, s := range sels {
		out = append(out, ChecklistEntry{
			SelectionPublicID: s.PublicID,
			ItemKey:           s.ItemKey,
			ItemName:          s.ItemName,
			RefereeNote:       s.ItemRefereeNote,
			TargetNote:        s.TargetNote,
			DrawResult:        s.DrawResult,
			AppliesToBoth:     s.ItemParams.AppliesToBoth,
		})
	}
	return out
}

// SyncReport 是一次目錄同步的結果,給 CLI 印出來、也寫進稽核的 after。
type SyncReport struct {
	TournamentID int64 `json:"tournament_id"`
	// Updated 是實際更新的列數(= seed 中在該屆有對應 key 的項數)。
	Updated int `json:"updated"`
	// ExtraKeys 是該屆有、seed 沒有的 key:同步不會動它們。
	ExtraKeys []string `json:"extra_keys,omitempty"`
}

// DrawPools 是封盤抽選要的候選清單來源。
//
// 池的內容與驗證的權威在 rules 套件(tournaments.config 的 handicap.draw_pools),
// 這裡只宣告「給我一個名字,回一份清單」—— rules.Config 逐字元滿足它。
// 用介面而不是直接吃 rules.Config,是讓 core 的 fake 不必解析整份 config。
type DrawPools interface {
	DrawPool(name string) ([]string, bool)
}

// ── 錯誤 ────────────────────────────────────────────────────────
//
// 全部是 package 層級 sentinel,transport 用 errors.Is 分辨後決定 HTTP 狀態碼與文案。
// 沒有任何一個是「重試就會好」的 —— 讓武的失敗都是條件不滿足,不是暫時性故障。
var (
	// ErrInvalidRequest 表示請求參數不合法(缺 id、target_note 過長等)。
	ErrInvalidRequest = errors.New("請求參數不合法")
	// ErrInvalidItemParams 表示 handicap_items.params 有未登記的鍵或值域不合。
	// seed 讀到它是編譯期就該修的錯;DB 讀到它代表有人繞過程式直接改了 JSONB。
	ErrInvalidItemParams = errors.New("讓武項目參數不合法")
	// ErrCatalogueInUse 表示該屆已有人依目前的項目文字選購過(含已退掉的),
	// 改文字等於改比賽條件,同步被拒。
	ErrCatalogueInUse = errors.New("該屆已有讓武選擇,不可同步目錄")
	// ErrCatalogueMismatch 表示同步影響的列數與預期不符 —— 目錄與該屆的項目對不上。
	ErrCatalogueMismatch = errors.New("目錄同步影響列數與預期不符")
	// ErrDrawPoolEmpty 表示封盤時要抽的池是空的(或 config 沒有這個池)。
	// 整個封盤失敗:不可以封了盤卻沒抽到 —— 封盤公示必須是完整的。
	ErrDrawPoolEmpty = errors.New("抽選池是空的")
	// ErrAlreadyDrawn 表示這筆選擇已經抽過了。抽選與封盤同一個 tx、封盤不可逆,
	// 所以正常路徑不會發生;發生就是資料被動過,不能靜靜跳過。
	ErrAlreadyDrawn = errors.New("這筆讓武選擇已經抽過")
	// ErrItemNotFound 表示讓武項目不存在,或不屬於這場比賽所在的賽事。
	ErrItemNotFound = errors.New("讓武項目不存在")
	// ErrHandicapNotOpen 表示裁判尚未開盤。
	ErrHandicapNotOpen = errors.New("讓武尚未開盤")
	// ErrHandicapClosed 表示該場狀態不允許選購(如 pending、live、done)。
	ErrHandicapClosed = errors.New("此場次不在讓武選購階段")
	// ErrHandicapLocked 表示已封盤,不可再增減。封盤不可逆。
	ErrHandicapLocked = errors.New("讓武已封盤,不可再更動")
	// ErrAlreadyLocked 表示重複封盤。封盤是不可逆動作,重複呼叫要出聲而不是默默成功。
	ErrAlreadyLocked = errors.New("讓武已經封盤")
	// ErrNoBudget 表示請求者在本場沒有 BP 預算 —— 同段對決、或請求者是高段位方、
	// 或根本不是這場的選手。三者對呼叫端是同一件事:你不能在這場買讓武。
	ErrNoBudget = errors.New("本場沒有讓武預算")
	// ErrBudgetExists 表示預算列已存在(adapter 的 ON CONFLICT 訊號)。
	ErrBudgetExists = errors.New("預算已存在")
	// ErrBudgetConflict 表示已存在的預算與依當下段位重算的結果不一致 ——
	// 代表抽籤後有人改了段位。這種情況必須失敗出聲,不能默默覆蓋:
	// 覆蓋會讓已經花掉的 spent 對不上新的 budget。
	ErrBudgetConflict = errors.New("既有預算與當下段位差不一致")
	// ErrInsufficientBP 表示 BP 不足。
	ErrInsufficientBP = errors.New("BP 不足")
	// ErrTargetNoteRequired 表示這個項目必須填指定內容(如「指定對手武學」要填武學名)。
	ErrTargetNoteRequired = errors.New("此項目必須填寫指定內容")
	// ErrSelectionNotFound 表示選擇不存在。
	ErrSelectionNotFound = errors.New("讓武選擇不存在")
	// ErrNotSelectionOwner 表示這筆選擇不是請求者買的。
	ErrNotSelectionOwner = errors.New("讓武選擇不屬於該選手")
	// ErrSelectionAlreadyVoided 表示已經退過了。
	ErrSelectionAlreadyVoided = errors.New("讓武選擇已退掉")
	// ErrBudgetInconsistent 表示 match_budgets.spent 與未作廢 selections 的總和對不上。
	// 這是資料完整性失效,只可能來自 adapter 的 bug;繼續算下去會讓錯誤擴散,
	// 所以寧可整個動作失敗(與 shop.ErrLedgerStateConflict 同一個判斷)。
	ErrBudgetInconsistent = errors.New("預算已花費金額與選擇總和不一致")
)

// ── 持久化介面 ──────────────────────────────────────────────────

// Repository 是讓武需要的全部持久化能力。
//
// 每個方法都貼著單一 SQL 敘述設計,adapter 不需要在裡面做業務判斷 ——
// 規則全在 Service,adapter 只負責把行為翻成 SQL。
type Repository interface {
	// WithTx 在單一交易中執行 fn,傳入的 Repository 綁在該交易上。
	//
	// 選購、退選、封盤全都是「讀狀態 → 判斷 → 寫兩張表」,四步必須同進同出。
	// 巢狀呼叫(在 fn 裡再 WithTx)是呼叫端的 bug,adapter 應直接回錯。
	WithTx(ctx context.Context, fn func(ctx context.Context, r Repository) error) error

	// LockMatch 讀一場比賽並鎖住該列(SELECT ... FOR UPDATE)。
	//
	// matches 列是讓武的**序列化點**:選購、退選、封盤彼此互斥,
	// 因此不會出現「檢查時還沒封盤、寫入時已經封盤」的縫隙,
	// 也不會有兩次連點同時通過餘額檢查。找不到時回 activityerr.ErrMatchNotFound。
	LockMatch(ctx context.Context, matchPublicID string) (*Match, error)
	// GetMatch 是唯讀路徑用的無鎖版本。找不到時回 activityerr.ErrMatchNotFound。
	GetMatch(ctx context.Context, matchPublicID string) (*Match, error)

	// ListItems 列出某屆的全部讓武項目,依 category、sort_order 排序。
	ListItems(ctx context.Context, tournamentID int64) ([]Item, error)
	// GetItem 依 Ref 取單一項目,並確認它屬於該屆。
	// 跨屆引用(拿去年的項目買今年的場次)必須回 ErrItemNotFound —— 價格逐屆可調。
	GetItem(ctx context.Context, tournamentID int64, ref string) (*Item, error)
	// InsertItems 把項目定義實例化到某一屆,回實際插入的列數。
	//
	// 語意是 ON CONFLICT (tournament_id, category, name) DO NOTHING:重跑不改價格。
	// 改價是裁判的明確動作,不該由「不小心重跑一次 seed」造成。
	InsertItems(ctx context.Context, tournamentID int64, specs []ItemSpec) (int, error)
	// SyncItems 以 key 匹配,把 specs 的文字與參數(name / description / referee_note /
	// repeatable / sort_order / params)寫進該屆既有的項目,回實際更新的列數。
	//
	// **不動 cost**,也不補建 key 不存在的項目 —— 前者是價格逐屆定案,
	// 後者是 InsertItems 的事。前置條件(該屆沒有選擇)由 Service 在同一個 tx 內先驗。
	SyncItems(ctx context.Context, tournamentID int64, specs []ItemSpec) (int, error)
	// CountSelections 數該屆指向任何讓武項目的選擇,**含已作廢**:
	// 退掉的也是「有人依這份文字買過」。
	CountSelections(ctx context.Context, tournamentID int64) (int64, error)
	// DrawPools 取該屆 config 裡的抽選池。解析的權威在 rules 套件,adapter 只是轉接。
	DrawPools(ctx context.Context, tournamentID int64) (DrawPools, error)

	// GetBudget 取某人在某場的預算。沒有列時回 ErrNoBudget。
	GetBudget(ctx context.Context, matchID, playerID int64) (*Budget, error)
	// FindMatchBudget 取某場唯一的預算列(施加者的)。沒有列時回 ErrNoBudget。
	FindMatchBudget(ctx context.Context, matchID int64) (*Budget, error)
	// InsertBudget 建立預算列;已存在時回 ErrBudgetExists,**不覆蓋**。
	InsertBudget(ctx context.Context, matchID, playerID, budget int64) (*Budget, error)
	// SetSpent 寫入重算後的 spent(不是加減,是覆寫)。
	//
	// 覆寫而不是 spent = spent + cost,是因為權威值是 SUM(cost) WHERE NOT voided;
	// 用增量更新的話,任何一次漏算都會永遠留在資料裡。
	SetSpent(ctx context.Context, matchID, playerID, spent int64) error

	// ListSelections 列出某人在某場**未作廢**的選擇,依 created_at 排序。
	ListSelections(ctx context.Context, matchID, playerID int64) ([]Selection, error)
	// GetSelection 依 public_id 取單筆(含已作廢者,Voided 原樣回傳)。
	// 找不到時回 ErrSelectionNotFound。
	GetSelection(ctx context.Context, selectionPublicID string) (*Selection, error)
	// InsertSelection 寫入一列並回傳完整內容(含 adapter 產生的 public_id 與快照欄位)。
	InsertSelection(ctx context.Context, ns NewSelection) (*Selection, error)
	// MarkSelectionVoided 標記作廢。voided 之外的欄位一律不動 —— 退費只是 BP 內部的事。
	MarkSelectionVoided(ctx context.Context, selectionID int64) error
	// SetSelectionDraw 寫入抽選結果,at 必須等於封盤時間(同一個值,不是同一時刻)。
	//
	// 只更新 draw_result IS NULL 的列;0 列 = 已抽過 → ErrAlreadyDrawn。
	// 「一筆只抽一次」的權威是這個述詞,不是呼叫端先讀再判。
	SetSelectionDraw(ctx context.Context, selectionID int64, result string, at time.Time) error

	// LockHandicaps 封盤:handicap_open = false、handicap_locked_at = at、status = 'locked'。
	//
	// status 一起改是 DB 約束逼出來的(matches_locked_at_check:
	// handicap_locked_at 非 NULL 時 status 必須是 locked/live/done),不是這裡多管閒事。
	LockHandicaps(ctx context.Context, matchID int64, at time.Time) error
}
