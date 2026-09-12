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
}

// Bets 是本套件對下注的最小依賴:判勝負時結算、棄賽時全額退款。
//
// 簽名刻意與 betting.Service 的方法逐字相同,*betting.Service[TX] 不需要任何
// 包裝就滿足它(與 betting.Ledger[TX] 對 *ledgerpg.Service 是同一個模式)。
//
// 兩個方法都要求場次**已經是 done**:所以呼叫順序永遠是先寫 matches 再結算,
// 而且兩者在同一個 tx 裡 —— 結算讀到的是我們剛寫下的勝者。
type Bets[TX any] interface {
	// SettleMatchInTx 結算所有押到這場的注單(派彩 / 判輸)。
	// 場次是 walkover 時會回 betting.ErrWalkoverMatch,原樣上拋。
	SettleMatchInTx(ctx context.Context, tx TX, matchPublicID string) (*betting.SettleResult, error)

	// VoidMatchInTx 作廢這場的所有腿並退款。只對 result_kind = 'walkover' 成立。
	//
	// 賠率是按「真的打一場」算的,不戰而勝不該讓任何人贏錢,
	// 這也堵住了串通棄賽套利的口子(schemas/21)。
	VoidMatchInTx(ctx context.Context, tx TX, matchPublicID string) (*betting.SettleResult, error)
}
