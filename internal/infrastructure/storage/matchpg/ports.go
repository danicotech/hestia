package matchpg

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
)

// 本檔是 match 對另外兩個套件的接線。兩邊的形狀差很多,理由也不同:
//
//	下注  *betting.Service[pgx.Tx] 的 SettleMatchInTx / VoidMatchInTx 逐字元滿足
//	      match.Bets[pgx.Tx],所以**零包裝**直接注入(下面那行斷言就是全部)。
//	讓武  handicap.Service 自己開 transaction(Repository.WithTx),而開盤/封盤
//	      必須與改 matches.status、寫稽核、發 outbox 同生共死 —— tx 一旦由被呼叫方
//	      開,同生共死就做不到了。所以要一層把「既有的 tx」交給它的轉接。

// 編譯期斷言:下注不需要任何轉接型別。
//
// 這一行有實質作用 —— 它讓「betting 的方法簽名被改動」變成編譯錯誤,
// 而不是等到組裝 cmd/server 時才發現接不上。
var _ match.Bets[pgx.Tx] = (*betting.Service[pgx.Tx])(nil)

// BindHandicapRepo 是「把 handicap.Repository 綁在一個既有 pgx.Tx 上」的建構子。
//
// 預期由 handicappg 提供:回傳的 Repository 其 WithTx **就地執行 fn**(不再開新的
// transaction),因此 handicap.Service 的寫入路徑會落在我們這一個 tx 裡。
//
// 之所以是函式型別而不是直接 import handicappg:本套件只需要「給我一個綁在這個 tx
// 上的 repo」這一件事,宣告成它自己的形狀就不必跟著讓武 adapter 的建構子演化,
// 測試也能塞一個不必連讓武資料表的替身。組裝點(cmd/server)長這樣:
//
//	hcap := matchpg.NewHandicaps(func(tx pgx.Tx) handicap.Repository {
//	    return handicappg.BindTx(tx) // 它的 WithTx 就地執行,不再開新 tx
//	})
//
// 包一層 closure 是因為 BindTx 回的是 *handicappg.Repo 而不是介面值 ——
// 換成直接吃具體型別會讓本套件與讓武 adapter 硬綁在一起,代價比這一行大得多。
type BindHandicapRepo func(tx pgx.Tx) handicap.Repository

// Handicaps 把 handicap.Service 接到 match 的窄埠上。
type Handicaps struct {
	bind BindHandicapRepo
	opts []handicap.Option
}

var _ match.Handicaps[pgx.Tx] = (*Handicaps)(nil)

// NewHandicaps 建立轉接。opts 原樣傳給 handicap.New(目前只有時鐘,測試要可預期的
// 封盤時間時會用到)。
func NewHandicaps(bind BindHandicapRepo, opts ...handicap.Option) *Handicaps {
	return &Handicaps{bind: bind, opts: opts}
}

// GrantBudgetInTx 依段位差發 BP,寫入落在呼叫端的 tx 裡。
//
// 回 (nil, nil) 表示本場無讓武(同段對決),那不是錯誤 —— 原樣上拋,
// 由 match 決定公告怎麼寫。
func (h *Handicaps) GrantBudgetInTx(ctx context.Context, tx pgx.Tx, matchPublicID string) (*handicap.Budget, error) {
	return h.service(tx).GrantBudget(ctx, matchPublicID)
}

// LockInTx 封盤,回傳封盤當下的完整清單。
//
// 它同時會把 matches 推進到 locked —— 那句 UPDATE 的權威在讓武套件,
// match 不寫第二份。錯誤(如 handicap.ErrAlreadyLocked)原樣上拋。
func (h *Handicaps) LockInTx(ctx context.Context, tx pgx.Tx, matchPublicID string) (*handicap.MatchHandicaps, error) {
	return h.service(tx).Lock(ctx, matchPublicID)
}

// service 為這一個 tx 現做一個 handicap.Service。
//
// 每次現做而不是快取:Service 本身只有 repo 與時鐘兩個欄位,建構成本等於一次
// 結構配置;而快取就得替每個 tx 各存一份,那反而是需要清理的狀態。
func (h *Handicaps) service(tx pgx.Tx) *handicap.Service {
	return handicap.New(h.bind(tx), h.opts...)
}
