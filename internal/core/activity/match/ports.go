package match

import (
	"context"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
)

// Handicaps 是本套件對讓武的最小依賴:發預算與封盤,兩者都要掛在**我們的** tx 上。
//
// 為什麼是窄介面而不是直接吃 *handicap.Service:
//
//  1. handicap.Service 自己開 transaction(Repository.WithTx),而開盤與封盤
//     必須與「改 matches.status」「寫稽核」「發 outbox」同生共死。
//     tx 一旦由被呼叫方開,同生共死就做不到了。
//  2. 介面只宣告我們真的會用的兩個方法,讓武套件日後長出多少 API 都與這裡無關。
//
// 實作端有兩條路都成立:給 handicap.Service 加上 *InTx 變體,
// 或在 adapter 裡用「綁在既有 tx 上的 handicap.Repository」建一個 Service。
// 本套件對此沒有意見 —— 它只要求「這兩件事發生在我的 tx 裡」。
type Handicaps[TX any] interface {
	// GrantBudgetInTx 依段位差發 BP,只給低段位者。
	//
	// 回 (nil, nil) 表示**本場無讓武**(同段對決),那不是錯誤:
	// 有沒有讓武這件事在資料上就該分得出來,不是靠 budget = 0 去解讀。
	GrantBudgetInTx(ctx context.Context, tx TX, matchPublicID string) (*handicap.Budget, error)

	// LockInTx 封盤,回傳封盤當下的完整清單(此時已對所有人公開)。
	//
	// 它同時會把 matches 推進到 locked(handicap_open = false、
	// handicap_locked_at = now())—— 那句 UPDATE 的權威在讓武套件,
	// 本套件不再寫第二份。重複封盤回 handicap.ErrAlreadyLocked。
	LockInTx(ctx context.Context, tx TX, matchPublicID string) (*handicap.MatchHandicaps, error)

	// RefundSelectionInTx 裁判代退一筆讓武選擇,BP 退回該場預算。
	//
	// 與另外兩個方法同一個理由要掛在我們的 tx 上:退點本身是讓武套件的事
	// (標 voided + 改 spent,不經 Ledger),但**稽核紀錄是我們寫的**,
	// 兩者必須同生共死 —— 退了點卻沒記上是誰退的,正是這條規則最不能出現的狀態。
	//
	// 封盤後回 handicap.ErrHandicapLocked,原樣上拋:封盤不可逆是既有定案,
	// 這條路不打開那條線(本套件也刻意不在上面補一個「裁判可以」的分支)。
	// 已經退過的回 handicap.ErrSelectionAlreadyVoided。
	RefundSelectionInTx(ctx context.Context, tx TX, selectionPublicID string) (*handicap.RefundResult, error)

	// RefereeViewInTx 是裁判端的讓武檢視(不套用封盤前的揭露限制),唯讀。
	//
	// 開賽前設定確認清單是它的推導值(MatchHandicaps.Checklist):清單 = 該場未作廢的
	// 讓武項目的 referee_note + 抽選結果。推導只在讓武套件寫一次,本套件只是把
	// 檢視拿回來 —— 在這裡再 JOIN 一次 handicap_items 就是同一份清單的第二個版本。
	RefereeViewInTx(ctx context.Context, tx TX, matchPublicID string) (*handicap.MatchHandicaps, error)
}

// Bets 是本套件對下注的最小依賴:開盤時建盤口、第一回合開始時關盤、
// 每回合結算單回合盤口、判勝負時結算整場、棄賽時全額退款。
//
// 簽名刻意與 betting.Service 的方法逐字相同,*betting.Service[TX] 不需要任何
// 包裝就滿足它(與 betting.Ledger[TX] 對 *ledgerpg.Service 是同一個模式)。
//
// 結算類的方法都要求對應的結果**已經寫進資料庫**(場次 done、回合 finished):
// 所以呼叫順序永遠是先寫 matches / match_rounds 再結算,而且在同一個 tx 裡 ——
// 結算讀到的是我們剛寫下的勝者。
type Bets[TX any] interface {
	// OpenMarketsInTx 依 config.betting.markets 替這場建盤口(開盤 = ready 時)。
	// 本屆 betting.enabled = false 時回空切片,那不是錯誤。
	OpenMarketsInTx(ctx context.Context, tx TX, matchPublicID string) ([]betting.Market, error)

	// CloseMarketsInTx 關掉這場所有還開著的盤口(第一回合正式決鬥開始那一刻,
	// schemas/21「第一回合開打即關」),回關掉的盤口數。
	CloseMarketsInTx(ctx context.Context, tx TX, matchPublicID string) (int, error)

	// SettleRoundInTx 結算押到這場第 roundNo 回合的單回合盤口(回合勝負、時長)。
	// 要求該回合已經 finished。
	SettleRoundInTx(ctx context.Context, tx TX, matchPublicID string, roundNo int) (*betting.SettleResult, error)

	// SettleMatchInTx 結算所有押到這場整場結果的注單(勝負盤、比分盤)。
	// 場次是 walkover 時會回 betting.ErrWalkoverMatch,原樣上拋。
	SettleMatchInTx(ctx context.Context, tx TX, matchPublicID string) (*betting.SettleResult, error)

	// VoidMatchInTx 作廢這場的所有腿並退款。只對 result_kind = 'walkover' 成立。
	//
	// 賠率是按「真的打一場」算的,不戰而勝不該讓任何人贏錢,
	// 這也堵住了串通棄賽套利的口子(schemas/21)。
	VoidMatchInTx(ctx context.Context, tx TX, matchPublicID string) (*betting.SettleResult, error)
}
