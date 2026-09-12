// Package signup 是《百業試鋒》的報名與選手身分。
//
// # 雙軌身分
//
// 報名**不需要**平台帳號。門檻要夠低:百業的成員未必都綁過 Discord OAuth,
// 而「先去登入再回來報名」會直接勸退一部分人(grill Q12)。
//
// 代價是活動層要自己發一組憑證,於是身分變成兩軌:
//
//	選手    → 遊戲ID + 通行碼 → 活動層 session
//	下注者  → 平台帳號 Bearer → 因為要動平台代幣
//
// 兩者可以是同一人:選手事後 BindPlatformAccount 即可。**領獎必須先綁**
// (grill Q24)—— 獎金走 Ledger,要有收款對象。
//
// # 這個套件不發 session
//
// Login 只負責「證明你是誰」,換發 cookie 是 transport 層的事。
// 把簽發也塞進來會讓核心邏輯依賴簽章金鑰,而驗證通行碼這件事
// 本來可以在沒有任何金鑰的情況下被測完。
package signup

import (
	"context"
	"errors"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// Fencer 是 activity.fencers 的一列:跨屆選手檔案,以遊戲ID 為自然鍵。
//
// 與 tournament.Player 的分工(schemas/26):這裡是**跨屆聚合**,
// 那裡是**報名當下的快照**。因為「段位一經確認即為本屆計算依據」,
// 本屆的 rank_level 不能被下屆覆寫,所以兩者不是重複而是兩個概念。
type Fencer struct {
	ID       int64
	PublicID string
	// GameID 全服唯一,是自然鍵也是登入帳號。
	GameID      string
	UserID      *int64
	DiscordName string
	// LastRankLevel 是最近一屆評定的段位;RankUnspecified = 從未評過。
	// 衍生資料,可從 tournament_players 重算(schemas/26)。
	LastRankLevel     bp.Rank
	LastRankedAt      *time.Time
	TournamentsPlayed int32
	Wins              int32
	Losses            int32
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// 錯誤語意:transport 層用 errors.Is 決定回 401 / 404 / 409 / 400。
var (
	// ErrInvalidCredentials 是登入失敗。
	//
	// **查無此遊戲ID 與通行碼錯誤回的是同一個錯誤**,而且訊息裡不含任何
	// 能分辨兩者的線索(schemas/20 明訂)。分得出來的話,登入頁就變成一份
	// 「誰報了名」的查詢介面 —— 報名期間那並不是公開資訊。
	//
	// 時序上的差異由 Hasher.VerifyDummy 補平,見 Service.Login。
	ErrInvalidCredentials = errors.New("遊戲ID 或通行碼不正確")
	// ErrAlreadyRegistered 是這個遊戲ID 在這一屆已經報過名了
	// (UNIQUE (tournament_id, fencer_id) 撞鍵)。
	ErrAlreadyRegistered = errors.New("這個遊戲ID 已報名本屆賽事")
	// ErrGameIDRequired 是遊戲ID 空白。
	ErrGameIDRequired = errors.New("遊戲ID 必填")
	// ErrInvalidGameID 是遊戲ID 含控制字元或過長。
	ErrInvalidGameID = errors.New("遊戲ID 格式不正確")
	// ErrDiscordNameRequired 是 Discord 名稱空白。
	// 它是裁判聯絡選手的唯一管道(排輪次、通知封盤),空著等於報了名找不到人。
	ErrDiscordNameRequired = errors.New("必須填寫 Discord 名稱")
	// ErrFieldTooLong 是某個報名表欄位超長。
	ErrFieldTooLong = errors.New("欄位過長")
	// ErrAlreadyBound 是這位選手已經綁定到**另一個**平台帳號。
	// 綁定決定獎金匯給誰,改綁必須由裁判介入並留稽核紀錄,不走這條路。
	ErrAlreadyBound = errors.New("這位選手已綁定其他平台帳號")
	// ErrUserAlreadyBound 是這個平台帳號已經綁在另一位選手身上
	// (fencers_user_id_uq)。一個帳號只能是一個人。
	ErrUserAlreadyBound = errors.New("這個平台帳號已綁定其他選手")
	// ErrUserRequired 是綁定時沒帶平台使用者。
	ErrUserRequired = errors.New("綁定需要平台帳號")
	// ErrMalformedHash 是資料庫裡的 passcode_hash 讀不懂。
	//
	// 這是系統問題不是使用者問題,所以它**不會**被回給使用者 ——
	// Login 一律把它轉成 ErrInvalidCredentials,只在伺服器端留下痕跡。
	ErrMalformedHash = errors.New("通行碼雜湊格式不正確")
)

// 報名表的欄位長度上限。
//
// 定上限不是為了資料整潔,而是因為這些欄位全部會被渲染進對戰表與 Discord 公告,
// 而 TEXT 沒有長度限制 —— 一個貼了三萬字的 arts_note 會讓選手卡爆掉。
const (
	maxGameIDLen      = 64
	maxDisplayNameLen = 64
	maxDiscordNameLen = 64
	maxLadderRankLen  = 64
	maxNoteLen        = 2000
)

// ── Repository port ────────────────────────────────────────────
//
// TournamentBySlug 與 PlayerByPublicID 的簽名**刻意與 tournament.Repo 完全相同**:
// 兩個 port 預期由同一個 pg adapter 型別滿足,簽名一致才不會為了同一句 SQL
// 寫出兩個方法(鐵則 9)。各自宣告則是介面隔離 —— signup 不該因為 tournament
// 多了一個抽籤方法就跟著要實作它。

// CreateRegistrationParams 是一次報名的完整寫入。
//
// 實作必須在**單一 transaction** 內做完:
//
//  1. 依 GameID 查 activity.fencers;沒有就建列。
//     並發時兩個請求都會查不到而都想建 —— UNIQUE (game_id) 是唯一權威,
//     撞鍵的那一邊必須重讀既有列繼續往下走,而不是把錯誤丟回給使用者。
//  2. 快照 fencers.last_rank_level 到 PreviousRank(步驟 3 之前取,
//     否則拿到的會是本屆的值)。
//  3. 建 activity.tournament_players。撞 UNIQUE (tournament_id, fencer_id)
//     回 ErrAlreadyRegistered —— 連點報名鈕由這條約束擋,不加冪等鍵
//     (一個概念一個權威,鐵則 9)。
//  4. 更新 fencers.discord_name、tournaments_played += 1。
//  5. public_id(ULID)由實作產生;core 不碰 id 生成。
//
// RequirePhase 必須進 WHERE:service 讀階段與寫入之間有時間差,
// 裁判剛好在那一瞬封閉報名的話,沒有這個條件就會漏進一筆。
// 條件不成立時回 tournament.ErrWrongPhase。
type CreateRegistrationParams struct {
	TournamentID int64
	RequirePhase tournament.Phase

	GameID      string
	DisplayName string
	DiscordName string
	// PasscodeHash 已經雜湊過。明碼永遠不進這一層。
	PasscodeHash string

	SelfRatedRank    bp.Rank
	LadderRank       string
	LadderScore      int32
	ArtsNote         string
	AvailabilityNote string
}

// Registration 是 CreateRegistration 的結果。
type Registration struct {
	Player tournament.Player
	Fencer Fencer
	// ReturningFencer = 這個遊戲ID 在**本次之前**就已經有檔案。
	ReturningFencer bool
	// PreviousRank 是接上檔案**之前**的 last_rank_level 快照;
	// RankUnspecified = 初次參賽或從未評過。這不是本屆段位。
	PreviousRank bp.Rank
}

// Credential 是登入要比對的那一列:選手加上他的通行碼雜湊。
//
// 合成一個結構是因為它們來自同一列 —— 拆成兩個方法會讀兩次同一張表,
// 而且中間有機會讀到不一致的狀態(裁判剛好在這時重新產生通行碼)。
type Credential struct {
	Player           tournament.Player
	PasscodeHash     string
	PasscodeIssuedAt time.Time
}

// UpdatePasscodeParams 是重新產生通行碼。
//
// 實作必須同時更新 passcode_hash 與 passcode_issued_at ——
// 舊碼「立即失效」靠的是雜湊被覆寫;issued_at 則讓**已經簽發出去的
// session** 一併失效(選手 session 把它寫進 token,驗證時逐微秒比對,
// 見 internal/core/activity/session)。補發通行碼的情境就是「原本那組
// 可能落到別人手上」,只換雜湊而留著舊 session 等於沒換。
// 這個動作要寫進 platform.admin_audit_logs(同一個 transaction),
// 但**絕不可**把明碼或雜湊寫進稽核紀錄。
type UpdatePasscodeParams struct {
	TournamentID int64
	PlayerID     int64
	PasscodeHash string
	// ActorUserID 是執行的裁判,**必為非 0**。
	//
	// 沒有「選手自助重取」這條路。遊戲ID 是公開資訊(對戰表上就有),
	// 自助重取等於「輸入任何人的遊戲ID 就能換掉他的通行碼」,
	// 而原持有人只會以為自己抄錯了。
	//
	// 這也是 adapter 的前提:admin_audit_logs.actor_user_id 是 NOT NULL
	// 且 FK 到 platform.users,塞 0 進去會讓整筆 transaction 失敗。
	ActorUserID int64
	Reason      string
}

// BindParams 是把活動層身分接上平台帳號。
//
// 實作必須在**單一 transaction** 內同時寫兩處:
//
//	activity.fencers.user_id             跨屆的綁定
//	activity.tournament_players.user_id  本屆的快照
//
// 只寫一處的後果是領獎時查哪一張表會得到不同答案。
// 撞 fencers_user_id_uq 時回 ErrUserAlreadyBound。
type BindParams struct {
	TournamentID int64
	PlayerID     int64
	FencerID     int64
	UserID       int64
}

// Repo 是本套件對儲存層的需求。
type Repo interface {
	// TournamentBySlug 查一屆賽事。查無回 tournament.ErrTournamentNotFound。
	TournamentBySlug(ctx context.Context, slug string) (tournament.Tournament, error)

	// CreateRegistration 寫入一次報名,見 CreateRegistrationParams。
	CreateRegistration(ctx context.Context, p CreateRegistrationParams) (Registration, error)

	// CredentialByGameID 依遊戲ID 取本屆的選手與通行碼雜湊。
	// 查無回 tournament.ErrPlayerNotFound —— 呼叫端**不得**把這個錯誤原樣回給使用者。
	CredentialByGameID(ctx context.Context, tournamentID int64, gameID string) (Credential, error)

	// PlayerByPublicID 在指定賽事內以 public_id 查選手。查無回 tournament.ErrPlayerNotFound。
	PlayerByPublicID(ctx context.Context, tournamentID int64, publicID string) (tournament.Player, error)

	// UpdatePasscode 換發通行碼,見 UpdatePasscodeParams。
	UpdatePasscode(ctx context.Context, p UpdatePasscodeParams) error

	// BindPlatformAccount 綁定平台帳號並回傳更新後的選手,見 BindParams。
	BindPlatformAccount(ctx context.Context, p BindParams) (tournament.Player, error)
}
