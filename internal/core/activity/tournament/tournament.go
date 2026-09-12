package tournament

import (
	"context"
	"errors"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
)

// PlayerStatus 是參賽者在本屆的狀態。字面值等於 tournament_players.status 的 CHECK 值域。
type PlayerStatus string

const (
	// PlayerActive 仍在賽中(含尚未開打)。
	PlayerActive PlayerStatus = "active"
	// PlayerEliminated 已被淘汰。
	PlayerEliminated PlayerStatus = "eliminated"
	// PlayerWithdrawn 棄賽。棄賽者不進抽籤;抽籤後棄賽走「對手不戰而勝」。
	PlayerWithdrawn PlayerStatus = "withdrawn"
)

// Valid 回報狀態是否在枚舉內。
func (s PlayerStatus) Valid() bool {
	return s == PlayerActive || s == PlayerEliminated || s == PlayerWithdrawn
}

// Tournament 是 activity.tournaments 的一列。
//
// ConfigRaw 刻意保持原始 JSONB 位元組而不在這裡解析:port 的形狀要貼著 SQL
// 能直接做到的事,解析是 ParseConfig 的職責。Service.Get 會一併把解析結果給你。
type Tournament struct {
	ID          int64
	PublicID    string
	Slug        string
	Name        string
	CommunityID int64
	Phase       Phase
	ConfigRaw   []byte
	SignupBonus int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Player 是 activity.tournament_players 的一列(含 JOIN fencers 帶出的 game_id)。
//
// **刻意不含 passcode_hash**。通行碼只有 signup 套件需要,而 Player 會一路被
// 傳到 transport 層去組回應 —— 一個不在結構裡的欄位,不可能被誰不小心序列化出去。
//
// UserID 是指標而不是 0 代表未綁:0 與「未綁定」在這裡是兩件事,
// 而領獎必須先綁定,把兩者混為一談的代價是把獎金發給不存在的使用者。
type Player struct {
	ID           int64
	PublicID     string
	TournamentID int64
	FencerID     int64
	UserID       *int64
	// GameID 來自 fencers,是全服唯一的自然鍵,也是登入帳號。
	GameID string
	// DisplayName 是報名當下的快照。之後改名不影響已公布的對戰表。
	DisplayName string
	DiscordName string
	// Rank 是裁判評定的本屆段位;bp.RankUnspecified = 尚未評定。
	// 未評定者不得進入抽籤。
	Rank     bp.Rank
	RankedAt *time.Time
	RankedBy *int64
	// SelfRatedRank 是報名者自評,只是評段的參考起點,不參與任何計算。
	SelfRatedRank    bp.Rank
	LadderRank       string
	LadderScore      int32
	ArtsNote         string
	AvailabilityNote string
	// SeedNo 是抽籤後的籤位(1-based);0 = 未抽。
	SeedNo    int32
	Status    PlayerStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ranked 回報這位選手是否已評段。
func (p Player) Ranked() bool { return p.Rank.Valid() }

// Bound 回報是否已綁定平台帳號。領獎前必須為 true。
func (p Player) Bound() bool { return p.UserID != nil && *p.UserID != 0 }

// 錯誤語意:transport 層用 errors.Is 決定回 404 / 409 / 400。
//
// 這一組刻意全部是 sentinel 而不是「帶 code 的自訂 error 型別」:
// 呼叫端要做的只有分流,而 errors.Is 已經夠用;自訂型別的額外欄位
// 在第一個 fmt.Errorf 包裝之後就沒有人會去取了。
var (
	// ErrTournamentNotFound 是 slug 查無此賽事。
	ErrTournamentNotFound = errors.New("賽事不存在")
	// ErrPlayerNotFound 是這屆賽事裡查無此選手。
	ErrPlayerNotFound = errors.New("選手不存在")
	// ErrPhaseConflict 是階段更新影響 0 列 —— 讀到的階段與寫入當下的階段不同。
	//
	// 這是樂觀鎖,不是重試就會好的暫時性錯誤:兩個裁判同時按「進入下一階段」時
	// 只能有一個成功,輸的那個必須知道自己什麼都沒做到,而不是收到一個
	// 「成功」然後以為賽事被自己推進了兩階。
	ErrPhaseConflict = errors.New("賽事階段已被其他操作改變")
	// ErrPlayersUnranked 是還有 active 選手沒評段就要抽籤。
	// 未評段者算不出 BP,硬抽的結果是那些場次全部沒有讓武。
	ErrPlayersUnranked = errors.New("尚有選手未評定段位")
	// ErrRanksLocked 是抽籤之後還想改段位。
	// BP 依段位差算出,抽完籤改段位會讓已發的 match_budgets 對不上;
	// 要改必須先退回 ranked 階段(對戰表會一併作廢)再重抽。
	ErrRanksLocked = errors.New("抽籤後不可再改段位,請先退回段位公布階段")
	// ErrInvalidRank 是段位不在 1..4。
	ErrInvalidRank = errors.New("段位不合法")
	// ErrNoDrawablePlayers 是沒有任何可抽籤的選手。
	ErrNoDrawablePlayers = errors.New("沒有可抽籤的選手")
	// ErrSamePlayer 是交換籤位時指到同一個人。
	ErrSamePlayer = errors.New("交換籤位需要兩位不同的選手")
	// ErrPlayerNotSeeded 是交換籤位時對方還沒有籤位。
	ErrPlayerNotSeeded = errors.New("選手尚未抽到籤位")
	// ErrInvalidSeed 是抽籤種子字串解析失敗。
	ErrInvalidSeed = errors.New("抽籤種子不合法")
	// ErrActorRequired 是破壞性操作沒帶裁判身分。
	// 這些動作全部要進 admin_audit_logs,沒有 actor 就記不出「是誰做的」。
	ErrActorRequired = errors.New("這個操作必須指明執行的裁判")
)

// ── Repository port ────────────────────────────────────────────
//
// 以下是本套件對儲存層的全部需求。實作在 storage/*pg,本套件只認 context
// 與標準庫(core 是契約層)。
//
// # 稽核紀錄由實作在同一個 transaction 內寫
//
// 帶 ActorUserID 的參數結構全部是裁判的破壞性操作,依 schemas/20 必須寫進
// platform.admin_audit_logs。刻意**不**另開一個 Auditor port 讓 service 事後補寫:
// 那樣一來「資料改了但稽核沒記上」就是一個能真實發生的狀態,而抽籤種子一旦漏記,
// 就再也無法向人證明那張對戰表是怎麼抽出來的。
// 綁在同一個 transaction 裡,這個狀態在結構上不存在。

// UpdatePhaseParams 是一次階段推進。
type UpdatePhaseParams struct {
	TournamentID int64
	// From 是讀取當下的階段,實作必須放進 WHERE(樂觀鎖)。
	// 影響 0 列時回 ErrPhaseConflict。
	From Phase
	To   Phase
	// ActorUserID 是執行的裁判(弱參照 platform.users)。
	ActorUserID int64
	// Reason 進稽核紀錄的 reason 欄位。
	Reason string
}

// RollbackParams 是 drawing → ranked 的回頭路。
//
// 實作必須在**單一 transaction** 內做三件事:
//  1. WHERE phase = 'drawing' 把 phase 改回 'ranked'(影響 0 列回 ErrPhaseConflict)
//  2. 刪掉本屆所有 matches
//  3. 把本屆所有 tournament_players.seed_no 設回 NULL
//
// 合成一個方法而不是拆成三個,是因為只做到一半的狀態(階段退回了但舊對戰表還在)
// 會讓觀眾看到一張已經不作數的表,而且下一次抽籤會撞 UNIQUE (tournament_id, round, slot)。
type RollbackParams struct {
	TournamentID int64
	ActorUserID  int64
	Reason       string
}

// SeatAssignment 是一位選手抽到的籤位。SeedNo 為 1-based。
type SeatAssignment struct {
	PlayerID int64
	SeedNo   int32
}

// MatchSeat 是對戰表上的一個節點。
//
// P1PlayerID / P2PlayerID 為 0 代表「尚未確定」(等上一輪),實作寫 NULL。
// 不用指標是為了讓 port 的形狀跟 bracket.Match 一致 —— 0 不是合法的 player id,
// 這個約定在 bracket 套件已經成立,兩邊用同一套才不會有轉換錯誤。
type MatchSeat struct {
	Round      int32
	Slot       int32
	P1PlayerID int64
	P2PlayerID int64
}

// ReplaceDrawParams 是一次抽籤的完整寫入(重抽也是走這裡)。
//
// 實作必須在**單一 transaction** 內:
//  1. 刪掉本屆既有的 matches、清空既有的 seed_no(重抽要先清乾淨,
//     否則 UNIQUE (tournament_id, seed_no) 會在中途撞上)
//  2. 依 Seats 寫回 seed_no
//  3. 依 Matches 建列,public_id 由實作產生(core 不碰 ULID 生成)
//  4. 寫 admin_audit_logs,after 欄位**必須包含 Seed**
type ReplaceDrawParams struct {
	TournamentID int64
	// Seed 是這次抽籤用的亂數種子(16 位小寫 hex)。
	//
	// 記錄它是整個抽籤流程唯一的可驗證性來源:裁判被質疑「這籤是不是喬過的」時,
	// 拿種子重跑一次 Draw 就會得到位元相同的對戰表。沒有種子就只能請人相信。
	Seed string
	// Seats 依 SeedNo 遞增排序。
	Seats []SeatAssignment
	// Matches 依 (Round, Slot) 排序。
	Matches []MatchSeat
	// ByePlayerIDs 是首輪輪空者。他們已經被填進第二輪的 Matches 裡,
	// 這個欄位只給稽核紀錄用 —— 輪空是隨機落的,要留得下痕跡。
	ByePlayerIDs []int64
	ActorUserID  int64
	Reason       string
}

// SwapSeedsParams 是交換兩位選手的籤位。
//
// 實作必須在**單一 transaction** 內同時更新兩件事:
//   - tournament_players.seed_no 互換(注意 UNIQUE (tournament_id, seed_no)
//     是立即檢查的,單一 UPDATE 直接對調會撞;要先騰出一個空位)
//   - matches 裡對這兩人的引用互換(p1_player_id / p2_player_id)
//
// 只改籤號不改對戰表,兩者就會互相矛盾 —— 而矛盾的那一刻沒有任何報錯,
// 只是對戰表上的名字跟籤位表對不起來,等到有人發現通常已經開打了。
type SwapSeedsParams struct {
	TournamentID int64
	PlayerAID    int64
	PlayerBID    int64
	ActorUserID  int64
	Reason       string
}

// SetPlayerRankParams 是裁判評定或修改一位選手的段位。
type SetPlayerRankParams struct {
	TournamentID int64
	PlayerID     int64
	Rank         bp.Rank
	// ActorUserID 寫進 tournament_players.ranked_by,同時是稽核紀錄的 actor。
	ActorUserID int64
	// Reason 進稽核紀錄。改段位是異議流程的結果,理由必須留得下來
	// (schemas/20:不做線上申訴,異議走 Discord,裁判後台直接改)。
	Reason string
}

// Repo 是本套件對儲存層的需求。每個方法都對應一到兩句 SQL。
type Repo interface {
	// TournamentBySlug 查一屆賽事。查無回 ErrTournamentNotFound。
	TournamentBySlug(ctx context.Context, slug string) (Tournament, error)

	// CountUnrankedActivePlayers 數還有幾位 active 選手沒評段。
	// 抽籤前的守門條件,回 0 才能抽。
	CountUnrankedActivePlayers(ctx context.Context, tournamentID int64) (int64, error)

	// ListDrawablePlayers 列出可以進抽籤的選手:status = 'active' 且 rank_level 非 NULL。
	// 順序不影響結果(抽籤本來就會洗牌),但實作應以 id 遞增回傳,
	// 好讓「同一個種子抽出同一張表」這個保證不依賴資料庫的回傳順序。
	ListDrawablePlayers(ctx context.Context, tournamentID int64) ([]Player, error)

	// PlayerByPublicID 在指定賽事內以 public_id 查選手。查無回 ErrPlayerNotFound。
	PlayerByPublicID(ctx context.Context, tournamentID int64, publicID string) (Player, error)

	// UpdatePhase 推進階段。影響 0 列回 ErrPhaseConflict。
	UpdatePhase(ctx context.Context, p UpdatePhaseParams) error

	// RollbackToRanked 執行 drawing → ranked 的回頭路,見 RollbackParams。
	RollbackToRanked(ctx context.Context, p RollbackParams) error

	// ReplaceDraw 寫入一次抽籤結果,見 ReplaceDrawParams。
	ReplaceDraw(ctx context.Context, p ReplaceDrawParams) error

	// SwapSeeds 交換兩位選手的籤位,見 SwapSeedsParams。
	SwapSeeds(ctx context.Context, p SwapSeedsParams) error

	// SetPlayerRank 寫入段位並回傳更新後的選手。
	SetPlayerRank(ctx context.Context, p SetPlayerRankParams) (Player, error)
}
