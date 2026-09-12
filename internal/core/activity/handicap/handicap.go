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
// 重複扣一次 BP 的後果是使用者看到數字不對(裁判封盤前退掉即可),
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
// # 分層
//
// 這個 package 只認標準庫與 activity/bp。持久化以 Repository 介面表達,
// 方法刻意貼著單一 SQL 敘述設計,讓 pg adapter 不必在裡面再做決策。
package handicap

import (
	"context"
	"errors"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
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
	Ref         string
	Category    Category
	Name        string
	Description string
	// RefereeNote 是給裁判看的執行說明。seed 目前全為空 —— 待補,不是錯誤。
	RefereeNote string
	Cost        int64
	// Repeatable 只供 UI 標示「(可重複)」,**不作強制**:所有項目都能重複買。
	Repeatable bool
	SortOrder  int32
}

// RequiresTargetNote 回報買這一項時是否必須填指定內容。
//
// 這是**推導值不是欄位**:handicap_items 沒有對應的欄位,判斷完全由項目語意決定,
// 規則寫在 items.go 的 targetNoteRequired。
func (i Item) RequiresTargetNote() bool { return RequiresTargetNote(i.Category, i.Name) }

// ItemSpec 是 seed JSON 裡的一列項目定義(尚未綁定到任何一屆賽事)。
type ItemSpec struct {
	Category    Category `json:"category"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	// RefereeNote 在 seed 裡目前全是 null(待御風羽補),所以是指標而不是字串 ——
	// 「還沒寫」與「寫了空字串」在裁判手冊補齊的過程中是兩件事。
	RefereeNote *string `json:"referee_note"`
	Cost        int64   `json:"cost"`
	Repeatable  bool    `json:"repeatable"`
	SortOrder   int32   `json:"sort_order"`
}

// Match 是讓武需要知道的那部分場次資料。
//
// adapter 以 matches JOIN tournaments(取 config 的 bp_per_rank_gap)
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
	// PerRankGap 取自 tournaments.config.bp_per_rank_gap;0 時 bp 套件會退回預設值 8。
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
	// ItemRef / ItemName / Category 是項目快照,由 adapter JOIN handicap_items 填入,
	// 前端顯示一張清單時不必再逐項回查。
	ItemRef  string
	ItemName string
	Category Category
	// Cost 是購買當下的價格快照。項目改價不影響已成立的選擇。
	Cost       int64
	TargetNote string
	Voided     bool
	CreatedAt  time.Time
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

// ── 錯誤 ────────────────────────────────────────────────────────
//
// 全部是 package 層級 sentinel,transport 用 errors.Is 分辨後決定 HTTP 狀態碼與文案。
// 沒有任何一個是「重試就會好」的 —— 讓武的失敗都是條件不滿足,不是暫時性故障。
var (
	// ErrInvalidRequest 表示請求參數不合法(缺 id、target_note 過長等)。
	ErrInvalidRequest = errors.New("請求參數不合法")
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

	// LockHandicaps 封盤:handicap_open = false、handicap_locked_at = at、status = 'locked'。
	//
	// status 一起改是 DB 約束逼出來的(matches_locked_at_check:
	// handicap_locked_at 非 NULL 時 status 必須是 locked/live/done),不是這裡多管閒事。
	LockHandicaps(ctx context.Context, matchID int64, at time.Time) error
}
