package handicappg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// GetBudget 取某人在某場的預算。
//
// 沒有列一律 ErrNoBudget:同段對決、請求者是高段位方、根本不是這場的選手,
// 三者對呼叫端是同一件事(你不能在這場買讓武),分得更細等於洩漏額外資訊。
func (r *Repo) GetBudget(ctx context.Context, matchID, playerID int64) (*handicap.Budget, error) {
	row, err := r.q.GetMatchBudget(ctx, db.GetMatchBudgetParams{
		MatchID:  matchID,
		PlayerID: playerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match=%d player=%d: %w", matchID, playerID, handicap.ErrNoBudget)
		}
		return nil, fmt.Errorf("讀預算 match=%d player=%d: %w", matchID, playerID, err)
	}
	return &handicap.Budget{
		MatchID:        row.MatchID,
		PlayerID:       row.PlayerID,
		PlayerPublicID: row.PlayerPublicID,
		Budget:         row.Budget,
		Spent:          row.Spent,
	}, nil
}

// FindMatchBudget 取某場唯一的預算列(施加者的)。
func (r *Repo) FindMatchBudget(ctx context.Context, matchID int64) (*handicap.Budget, error) {
	row, err := r.q.FindMatchBudgetByMatch(ctx, matchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("match=%d: %w", matchID, handicap.ErrNoBudget)
		}
		return nil, fmt.Errorf("讀本場預算 match=%d: %w", matchID, err)
	}
	return &handicap.Budget{
		MatchID:        row.MatchID,
		PlayerID:       row.PlayerID,
		PlayerPublicID: row.PlayerPublicID,
		Budget:         row.Budget,
		Spent:          row.Spent,
	}, nil
}

// InsertBudget 建立預算列;已存在時回 ErrBudgetExists,**不覆蓋**。
//
// # 為什麼這一支要開 savepoint
//
// Postgres 的 23505 會把**整個 transaction** 打成 aborted,之後任何敘述都回
// 25P02。而 ErrBudgetExists 的唯一用途,就是讓呼叫端接著在同一個 tx 裡
// GetBudget 去比對既有金額與當下段位差(見 handicap.Service.GrantBudget)——
// 不開 savepoint 的話那個分支永遠跑不到,sentinel 等於在說謊。
//
// 所以撞鍵時回捲到 savepoint:失敗的那一句被抹掉,外層 tx 完好如初。
// 只有這一支需要這樣做 —— 其他約束違反(FK、CHECK)對呼叫端都是終局,
// 沒有人要在同一個 tx 裡從那裡復原,為它們也包 savepoint 只是多兩次往返。
func (r *Repo) InsertBudget(ctx context.Context, matchID, playerID, budget int64) (*handicap.Budget, error) {
	arg := db.InsertMatchBudgetParams{MatchID: matchID, PlayerID: playerID, Budget: budget}

	q := r.q
	var sp pgx.Tx
	if r.tx != nil {
		// pgx 的巢狀 Begin 就是 SAVEPOINT。不在 tx 內(直接走池)時不需要:
		// 隱式 transaction 隨那一句結束,沒有東西會被打成 aborted。
		var err error
		if sp, err = r.tx.Begin(ctx); err != nil {
			return nil, fmt.Errorf("建 savepoint: %w", err)
		}
		q = r.q.WithTx(sp)
	}

	row, err := q.InsertMatchBudget(ctx, arg)
	if err != nil {
		if sp != nil {
			_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT:外層 tx 繼續可用
		}
		// 唯一鍵衝突不吞:match_budgets 上唯一的 UNIQUE 是主鍵 (match_id, player_id),
		// 撞到就是「這個人在這場已經有預算了」。呼叫端收到 ErrBudgetExists 後會去比對
		// 既有金額與當下段位差 —— 在這裡 DO NOTHING 會讓「抽籤後有人改了段位」
		// 變成默默覆蓋,而已經花掉的 spent 會對不上新的 budget。
		if pgErrCode(err) == codeUniqueViolation {
			return nil, fmt.Errorf("match=%d player=%d: %w", matchID, playerID, handicap.ErrBudgetExists)
		}
		return nil, fmt.Errorf("建立預算 match=%d player=%d: %w", matchID, playerID, err)
	}
	if sp != nil {
		if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
			return nil, fmt.Errorf("釋放 savepoint: %w", err)
		}
	}
	return &handicap.Budget{
		MatchID:        row.MatchID,
		PlayerID:       row.PlayerID,
		PlayerPublicID: row.PlayerPublicID,
		Budget:         row.Budget,
		Spent:          row.Spent,
	}, nil
}

// SetSpent 覆寫 spent(不是加減)。
//
// 兩種失敗都要翻譯成明確的 sentinel:
//
//	0 列        → ErrNoBudget。預算列不存在,這次寫入什麼也沒做。
//	23514 CHECK → ErrBudgetInconsistent。見下方說明。
func (r *Repo) SetSpent(ctx context.Context, matchID, playerID, spent int64) error {
	n, err := r.q.SetMatchBudgetSpent(ctx, db.SetMatchBudgetSpentParams{
		Spent:    spent,
		MatchID:  matchID,
		PlayerID: playerID,
	})
	if err != nil {
		// match_budgets_spent_check(0 <= spent <= budget)觸發。
		//
		// 為什麼翻成 ErrBudgetInconsistent 而不是 ErrInsufficientBP:
		// budget 一經 InsertBudget 就不再變動,而所有寫入都序列化在場次列的鎖底下,
		// 規則層在同一個 tx 內已經用剛讀到的 budget/spent 做過 CanAfford。
		// 所以 DB 在這裡說「超支」,唯一可能是規則層與 DB 對同一份資料的認知不一致
		// —— 那是寫入路徑的 bug,不是使用者「BP 不夠」這種正常結果。
		// 翻成 ErrInsufficientBP 會讓這個 bug 長年偽裝成一則使用者看得懂的提示。
		if violates(err, codeCheckViolation, constraintBudgetSpentCheck) {
			return fmt.Errorf("%w: DB 擋下 spent=%d(match=%d player=%d,超出 budget 或為負)",
				handicap.ErrBudgetInconsistent, spent, matchID, playerID)
		}
		return fmt.Errorf("寫入 spent match=%d player=%d: %w", matchID, playerID, err)
	}
	if n == 0 {
		return fmt.Errorf("match=%d player=%d: %w", matchID, playerID, handicap.ErrNoBudget)
	}
	return nil
}
