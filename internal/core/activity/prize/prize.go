// Package prize 是《百業試鋒》賽事獎金的發放。
//
// # 這裡全部是真錢
//
// 獎金一律經 platform 的 Ledger 入帳(reason = tournament_prize),活動層
// 不碰任何帳本表。本套件因此比照 ledger-invariants:金額全程 int64、
// 只發正數分錄、絕不改舊紀錄、動錢一律冪等。
//
// # 冪等鍵是推出來的,不是呼叫端傳進來的
//
// 一位選手的一種獎項只該發一次,而「哪一位選手的哪一種獎」本身就是一把
// 自然鍵 —— 於是冪等鍵是 activity.prize:<賽事public_id>:<選手public_id>:<獎項>,
// 與 betting 的派彩/退款同一個模式(見 betting.Service.moveMoney)。
//
// 為什麼不用裁判送進來的 idempotency_key:發獎不是一次就會結束的動作。
// 十個人裡有兩個還沒綁平台帳號,那兩筆發不出去;等他們綁好,裁判會再按一次
// 「發獎」—— 而那一次必然帶著新的冪等鍵(舊的那把重送只會原樣回放舊結果,
// 新綁的兩人永遠拿不到)。以呼叫端的鍵當權威,這第二次就會把前八個人再發一遍。
// 推出來的鍵沒有這個問題:已發的自動回放,只有新綁的那兩筆真的動錢。
//
// 這與帳本鐵則 4 的那條例外同源:有天然唯一鍵可擋時,它比外部冪等鍵更可靠
// (每日簽到靠 UNIQUE(user_id, claim_date) 擋,不靠冪等鍵)。
//
// # 為什麼沒有 transaction
//
// 本套件**不寫任何活動層的表**:名次從對戰表推出、綁定狀態從選手列讀出,
// 兩者都是讀。唯一的寫入是帳本,而每一筆入帳自己就是原子且冪等的。
// 沒有第二個寫入要跟它同生共死,就不需要把 tx 拉進來(對比 betting:
// 扣款與建注單必須同 tx,所以它才需要 ApplyInTx 與型別參數 TX)。
//
// 中途斷線的後果因此是「發到一半」,而不是「一筆都沒發」—— 重跑一次就會
// 把剩下的補完,已發的原樣回放。對一個裁判會手動重按的操作來說,可續跑
// 比全有全無更有用。
//
// # 季軍發不出來
//
// 單淘汰沒有季軍賽:bracket 建的樹只有主線,四強的兩個敗者之間沒有任何
// 比賽可以分出高下。所以 config 設了 prizes.third 又要發獎時,這裡回
// ErrThirdPlaceUndecidable 而不是自己挑一個(用籤位、用段位、用勝場數
// 都是發明規則,而發明出來的規則會直接變成某個人的錢)。
package prize

import (
	"context"
	"errors"

	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/core/platform/ledger"
)

// Kind 是獎項。字面值同時是 tournaments.config.prizes 的欄位名
// 與 proto 的 PrizeAward.prize_kind —— 三處用同一組字串,轉換層不需要對照表。
type Kind string

const (
	// KindChampion 冠軍:決賽勝者。
	KindChampion Kind = "champion"
	// KindRunnerUp 亞軍:決賽敗者(對手棄賽造成的不戰而勝也算,他確實是打到決賽的那一位)。
	KindRunnerUp Kind = "runner_up"
	// KindThird 季軍。單淘汰推不出來,見套件註解與 ErrThirdPlaceUndecidable。
	KindThird Kind = "third"
	// KindParticipation 參賽獎:發給所有報名者,含已淘汰與已棄賽。
	KindParticipation Kind = "participation"
)

// UnboundReason 是未綁定平台帳號時的 skipped_reason。
//
// 導出是為了讓 transport 與測試比對同一份字串:獎金走 Ledger,沒有平台
// user_id 就沒有收款對象。這不是靜默跳過 —— 該筆仍會出現在回應裡,
// 帶著選手的 public_id 與顯示名,裁判知道要去找誰。
const UnboundReason = "尚未綁定平台帳號,無法入帳;請該選手綁定後再發一次獎"

// refTypePlayer 是分錄的 ref_type:指回 activity.tournament_players 的那一列。
// 與 match.AuditTargetPlayer 同一個字面值,兩者指的是同一張表。
const refTypePlayer = match.AuditTargetPlayer

// Award 是一筆發獎結果。
//
// PlayerID 是內部 id,只給 adapter 寫稽核與對帳用;對外回應一律只出現
// PlayerPublicID(鐵則 5)。
type Award struct {
	PlayerID       int64
	PlayerPublicID string
	DisplayName    string
	Kind           Kind
	Amount         int64
	// Granted = 這筆獎金已經在帳本上(含本次入帳與冪等回放兩種情況)。
	Granted bool
	// SkippedReason 在 Granted = false 時說明原因,目前只有 UnboundReason 一種。
	SkippedReason string
	// EntryID 是帳本分錄 id;0 = 沒發成。稽核鏈:發獎結果 ↔ token_entries。
	EntryID int64
	// Replayed = 這一筆是冪等回放,本次沒有真的動錢。
	Replayed bool
}

// AwardParams 是一次發獎請求。
//
// 刻意沒有 IdempotencyKey:冪等鍵由賽事與選手推出,理由見套件註解。
// 入口層仍然要求客戶端帶 idempotency_key(契約沒變),只是領域層用不到它。
type AwardParams struct {
	// TournamentSlug 指定哪一屆。
	TournamentSlug string
	// ActorUserID 是執行的裁判,寫進每一筆分錄的 actor_id ——
	// 「這筆獎金是誰發的」因此在帳本上就答得出來。
	ActorUserID int64
}

// AwardResult 是一次發獎的完整結果。
type AwardResult struct {
	// TournamentID 給 adapter 寫稽核用(target_id)。
	TournamentID int64
	// Awards 依 冠軍 → 亞軍 → 參賽獎(依 Repository 的選手順序)排列。
	// 獎金設為 0 的獎項不會出現 —— 0 = 不發,發一筆 0 元的分錄是帳本不允許的。
	Awards []Award
	// Replayed = 本次完全沒有動錢(所有該發的都已經發過了)。
	//
	// 沒有任何可發的獎項時為 false:那不是「重放」,而是「本來就沒事可做」。
	Replayed bool
}

// 錯誤語意:transport 以 errors.Is 分辨後決定狀態碼與文案。
//
// 階段、賽事不存在、設定不合法、尚未抽籤都不在這裡重複定義 ——
// 那些分別是 tournament.ErrWrongPhase / ErrTournamentNotFound /
// ErrConfigMalformed 與 match.ErrBracketMissing,原樣上拋。
// 帳本的 ErrInFlight / ErrIdempotencyConflict 同理。
var (
	// ErrInvalidRequest 表示請求參數不合法。
	ErrInvalidRequest = errors.New("請求參數不合法")

	// ErrThirdPlaceUndecidable 表示 config 設了季軍獎金,但賽制推不出季軍。
	//
	// 單淘汰沒有季軍賽,四強的兩個敗者之間沒有比過。要發季軍獎必須先有
	// 一場能分出勝負的比賽 —— 那是賽制問題,不是這裡能替裁判決定的事。
	ErrThirdPlaceUndecidable = errors.New("單淘汰賽制沒有季軍賽,推不出季軍")

	// ErrFinalNotDecided 表示決賽還沒分出勝負,名次不成立。
	ErrFinalNotDecided = errors.New("決賽尚未分出勝負,無法決定名次")

	// ErrBracketBroken 表示對戰表本身不完整(找不到決賽、決賽缺一方、
	// 勝者不在選手名單裡)。這是資料異常而不是使用者錯誤,必須失敗出聲:
	// 猜一個冠軍出來就是把獎金發給錯的人。
	ErrBracketBroken = errors.New("對戰表資料異常,無法決定名次")

	// ErrLedgerStateConflict 表示帳本回了預期外的結果(一筆 Op 卻不是一筆分錄)。
	ErrLedgerStateConflict = errors.New("帳本回應與預期不符")
)

// Repository 是本套件對儲存層的全部需求:三個唯讀查詢。
//
// 三個方法的簽名刻意與既有的讀取埠逐字相同
// (tournament.Repo.TournamentBySlug、transport.ActivityReader 的
// ListPlayers / ListMatches),所以已經實作那些查詢的 adapter 不必為發獎
// 多寫一句 SQL —— 同一份查詢有第二個版本,遲早會有一邊漏掉棄賽者。
//
// 沒有 tx 參數:本套件只讀不寫,理由見套件註解。
type Repository interface {
	// TournamentBySlug 查一屆賽事。查無回 tournament.ErrTournamentNotFound。
	TournamentBySlug(ctx context.Context, slug string) (tournament.Tournament, error)

	// ListPlayers 列出本屆**全部**參賽者,含已淘汰與已棄賽 ——
	// 參賽獎發給所有報名者,少一種狀態就是少發一個人。
	ListPlayers(ctx context.Context, tournamentID int64) ([]tournament.Player, error)

	// ListMatches 列出本屆全部場次,依 (round, slot) 遞增。尚未抽籤時回空切片。
	ListMatches(ctx context.Context, tournamentID int64) ([]match.Match, error)
}

// Ledger 是本套件對帳本的最小依賴。
//
// 只宣告 Apply 一個方法:發獎不查餘額(入帳不可能餘額不足)、不對帳。
// 也刻意**不用** ApplyInTx —— 本套件沒有自己的 tx 要跟動錢綁在一起,
// 而 ledger.Ledger 的 Apply 是可攜的那一支(未來帳本拆成 HTTP 服務時,
// 這個介面一行不用改)。*ledgerpg.Service 直接滿足它。
type Ledger interface {
	Apply(ctx context.Context, p ledger.ApplyParams) (*ledger.ApplyResult, error)
}
