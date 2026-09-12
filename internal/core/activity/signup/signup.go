// Package signup 是《百業試鋒》的報名與選手身分。
//
// # 雙軌身分
//
// 報名**不需要**平台帳號。門檻要夠低:百業的成員未必都綁過 Discord OAuth,
// 而「先去登入再回來報名」會直接勸退一部分人(grill Q12)。
//
// 代價是活動層要自己認一組身分,於是身分變成兩軌:
//
//	選手    → 遊戲ID → 活動層 session
//	下注者  → 平台帳號 Bearer → 因為要動平台代幣
//
// 兩者可以是同一人:選手事後 BindPlatformAccount 即可。**領獎必須先綁**
// (grill Q24)—— 獎金走 Ledger,要有收款對象。
//
// # 報名只要遊戲ID,登入也只要遊戲ID(2026-09-13 定案)
//
// 這推翻了先前兩個決定:報名的必填欄位只剩 GameID,登入不再驗通行碼。
//
// 代價是清楚的、而且是被接受的:遊戲ID 全服唯一且公開(對戰表上就印著),
// 所以任何人知道某位選手的遊戲ID 就能以他的身分登入、花掉他的 BP、
// 改他的讓武選擇。換來的是報名到登入之間沒有任何要抄、會抄錯、會弄丟的東西。
//
// 既然沒有密碼可換,「把某個人擋在外面」的唯一槓桿就是他的狀態:
// 只有 tournament.PlayerActive 登得進來(見 Service.Login)。
//
// 通行碼本身沒有消失,只是不再給選手看:passcode_hash 照舊產生並寫入
// (NOT NULL),而它的同伴 passcode_issued_at 是已發出 session 的作廢依據。
// 所以裁判的 RegeneratePasscode 現在的意思是「把這個人所有 session 踢掉」,
// 見 UpdatePasscodeParams。
//
// # 這個套件不發 session
//
// Login 只負責「證明你是誰」,換發 cookie 是 transport 層的事。
// 把簽發也塞進來會讓核心邏輯依賴簽章金鑰,而「這個遊戲ID 現在能不能登入」
// 本來可以在沒有任何金鑰的情況下被測完。
package signup

import (
	"context"
	"errors"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/shared/secret"
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
	// **三條失敗路徑回的是同一個錯誤**:查無此遊戲ID、這個人不是 active
	// (棄賽或已淘汰)、遊戲ID 格式不合。訊息裡不含任何能分辨它們的線索。
	//
	// 分得出來的代價比想像中大:登入頁會變成一支「這個 ID 報名了沒 / 他是不是
	// 被棄賽了」的查詢介面,而被裁判擋在外面這件事不該由一個匿名請求問得出來。
	//
	// 名字保留 Credentials(而不是改成 ErrLoginRejected)是因為 errmap 那一列
	// 與它的 reason 字串是對外契約的一部分,改名會動到別人的檔案而換不到任何東西。
	ErrInvalidCredentials = errors.New("這個遊戲ID 目前無法登入")
	// ErrAlreadyRegistered 是這個遊戲ID 在這一屆已經報過名了
	// (UNIQUE (tournament_id, fencer_id) 撞鍵)。
	ErrAlreadyRegistered = errors.New("這個遊戲ID 已報名本屆賽事")
	// ErrGameIDRequired 是遊戲ID 空白。
	ErrGameIDRequired = errors.New("遊戲ID 必填")
	// ErrInvalidGameID 是遊戲ID 含控制字元或過長。
	ErrInvalidGameID = errors.New("遊戲ID 格式不正確")
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
	// 這是系統問題不是使用者問題,所以它**不會**被回給使用者。
	// 登入不再比對雜湊(見套件註解),所以現在只有換發那條路徑碰得到它。
	//
	// 值就是 secret.ErrMalformedHash 本人(不是同文字的第二個 sentinel):
	// 雜湊實作搬進 shared 之後,Verify 回的是那一個,這裡另起一個會讓
	// errors.Is(err, signup.ErrMalformedHash) 從此永遠是 false。
	ErrMalformedHash = secret.ErrMalformedHash
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
	//
	// 登入不再用到它(2026-09-13),但這欄位仍然必填:passcode_hash 是 NOT NULL,
	// 而它的同伴 passcode_issued_at 是選手 session 的作廢依據 —— 沒有這一份
	// 初始值,RegeneratePasscode 就沒有東西可以「換掉」。
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

// UpdatePasscodeParams 是重新產生通行碼。
//
// **這支現在真正的用途是「把一位選手現有的 session 全部踢掉」。**
// 登入只要遊戲ID,沒有人在比對通行碼,所以換掉的雜湊誰也不會讀到;
// 真正起作用的是一起被推進的 passcode_issued_at —— 選手 session 把簽發當下的
// 這個值寫進 token,驗證時逐微秒比對(見 internal/core/activity/session)。
//
// 它與 WithdrawPlayer 是「把人擋在外面」的兩道槓桿,管的時段不同:
// 棄賽擋住**之後**的登入,換發踢掉**現在**還活著的 session。
// 要立刻讓某人完全出不去也進不來,兩個都要做。
//
// 實作仍然必須同時更新兩欄:只改 issued_at 會讓那個欄位變成一支沒有意義的
// 時鐘,而換掉的雜湊是「這次換發真的換掉了一個秘密」唯一的痕跡。
// 這個動作要寫進 platform.admin_audit_logs(同一個 transaction),
// 但**絕不可**把明碼或雜湊寫進稽核紀錄。
type UpdatePasscodeParams struct {
	TournamentID int64
	PlayerID     int64
	PasscodeHash string
	// ActorUserID 是執行的裁判,**必為非 0**。
	//
	// 沒有「選手自助重取」這條路。遊戲ID 是公開資訊(對戰表上就有),
	// 而這個動作現在的效果是「把這個人所有 session 踢掉」——
	// 自助等於「輸入任何人的遊戲ID 就能把他登出」。
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

	// PlayerByGameID 依遊戲ID 取本屆的選手(登入用)。
	//
	// 查無回 tournament.ErrPlayerNotFound —— 呼叫端**不得**把這個錯誤原樣回給
	// 使用者,Login 會把它折成 ErrInvalidCredentials。
	//
	// 實作**不該**過濾 status:「誰登得進來」的權威只有 Service.Login 一處,
	// 因為它同時要負責把「非 active」與「查無此人」折成同一個答案。
	// 這也是它不回 Credential(通行碼雜湊)的原因 —— 沒有東西要比對了。
	PlayerByGameID(ctx context.Context, tournamentID int64, gameID string) (tournament.Player, error)

	// PlayerByPublicID 在指定賽事內以 public_id 查選手。查無回 tournament.ErrPlayerNotFound。
	PlayerByPublicID(ctx context.Context, tournamentID int64, publicID string) (tournament.Player, error)

	// UpdatePasscode 換發通行碼,見 UpdatePasscodeParams。
	UpdatePasscode(ctx context.Context, p UpdatePasscodeParams) error

	// BindPlatformAccount 綁定平台帳號並回傳更新後的選手,見 BindParams。
	BindPlatformAccount(ctx context.Context, p BindParams) (tournament.Player, error)
}
