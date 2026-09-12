// Package handicappg 是 handicap.Repository 的 Postgres 實作。
//
// # 原子性邊界
//
// 讓武的每一條寫入路徑(選購、退選、封盤、發預算)都是「讀場次 → 判斷 →
// 寫兩張表」。兩張表指 handicap_selections 與 match_budgets.spent:
// spent 是 selections 的衍生值,兩者分屬不同 transaction 的瞬間就是一個
// 對不上帳的窗口。所以規則層一律把整條路徑包在 WithTx 裡,adapter 只保證
// 「同一個 Repository 實例上的所有呼叫都在同一個 tx」。
//
// # 鎖序
//
// 只有一把鎖:activity.matches 的那一列(LockMatch 的 FOR UPDATE OF m)。
// 每條寫入路徑的第一步都是它,所以不存在兩把鎖的取得順序,也就不可能死鎖。
// match_budgets 與 handicap_selections 刻意不另外加鎖 —— 它們的併發安全
// 完全由「先鎖場次」這條紀律提供,多一把鎖只會多一個死鎖的機會。
//
// 這條紀律對正確性是必要的:GetBudget 是無鎖讀,只有在場次列已被本 tx 鎖住時,
// 它讀到的 spent 才保證不會在檢查與寫入之間被別人改掉(READ COMMITTED 下,
// 阻塞在 FOR UPDATE 的 tx 解鎖後,後續敘述會取得新快照而讀到對方 commit 的值)。
//
// # BP 不經 Ledger,所以這裡沒有冪等鍵
//
// BP 每輪重發、沒花完即作廢、不可交易、不可累積,四個性質沒一個符合貨幣
// (理由寫在 migrations/activity/00002_handicap.sql 的檔頭)。因此整個 package
// 沒有 token_entries、沒有 idempotency_keys、沒有 outbox —— 純 activity schema 的 CRUD。
//
// # 兩個變體:自己開 tx 與綁在別人的 tx 上
//
// New(pool) 的 WithTx 自己開 tx;BindTx(tx) 的 WithTx 就地執行。
// 兩者存在的理由見 BindTx 的說明。
package handicappg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// ErrNestedTx 表示在 WithTx 的 fn 裡又呼叫了 WithTx。
//
// pgx 的 tx 不可重入,巢狀只會得到一個語意不明的「子交易」或直接卡住。
// 這是呼叫端的 bug,所以出聲而不是默默攤平 —— 攤平的話,內層的失敗會不會
// 回滾外層的寫入就變成一個要看實作才知道的問題。
//
// 例外是 BindTx 變體:在它上面「巢狀」正是設計(見 BindTx)。
var ErrNestedTx = errors.New("handicappg: 巢狀 WithTx")

// mode 決定 WithTx 的行為。三個值對應三種持有 tx 的方式。
type mode uint8

const (
	// modeOwner:手上是連線池,WithTx 自己開 tx 並負責 commit / rollback。
	modeOwner mode = iota
	// modeInTx:已經在某個 tx 內,再開一層是 bug → ErrNestedTx。
	modeInTx
	// modeBound:綁在呼叫端的 tx 上,WithTx 就地執行、不 commit 也不 rollback。
	modeBound
)

// Repo 是 handicap.Repository 的 Postgres 實作。
type Repo struct {
	// pool 只在 modeOwner 有值:另外兩個 mode 手上已經是 tx,不該再從池子拿連線。
	pool *pgxpool.Pool
	q    *db.Queries
	// tx 在 tx 內時有值。留著它只有一個用途:InsertBudget 需要開 savepoint
	// 才能讓「撞鍵後繼續用同一個 tx」成立(見 InsertBudget)。
	tx   pgx.Tx
	mode mode
}

var _ handicap.Repository = (*Repo)(nil)

// New 建立走連線池的 Repository:WithTx 自己開 transaction。
//
// 這是 HTTP / gRPC handler 直接呼叫讓武服務時用的變體。
func New(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool, q: db.New(pool), mode: modeOwner}
}

// BindTx 把 Repository 綁在呼叫端已經開好的 transaction 上。
//
// 它的 WithTx **就地執行 fn**,不開新 tx、不 commit、不 rollback ——
// 於是讓武的寫入與呼叫端的寫入共存亡:外層 rollback 時預算與選擇一起消失。
//
// 為什麼需要這個變體:賽程(match)套件要在自己的 tx 裡推進場次狀態並順手
// 呼叫 handicap 的 GrantBudget / Lock。那兩個方法內部一定會呼叫 WithTx,
// 若那時去開第二個 tx,就會出現「場次已改成 ready 但預算還沒 commit」的中間態,
// 而且第二個 tx 會在 FOR UPDATE 同一列時卡死在自己人手上。
//
// **在這個變體上巢狀是正常路徑,不是 bug。** 傳進 fn 的 Repository 仍是
// modeInTx(再往下一層才會回 ErrNestedTx)—— 一層是設計,兩層還是 bug。
func BindTx(tx pgx.Tx) *Repo {
	return &Repo{q: db.New(tx), tx: tx, mode: modeBound}
}

// WithTx 在單一 transaction 中執行 fn。行為依變體而定,見 mode 的三個值。
func (r *Repo) WithTx(ctx context.Context, fn func(ctx context.Context, repo handicap.Repository) error) error {
	switch r.mode {
	case modeInTx:
		return ErrNestedTx

	case modeBound:
		// 呼叫端的 tx 已經開著,就地跑。錯誤原樣往上丟,由開 tx 的人決定回滾。
		return fn(ctx, &Repo{q: r.q, tx: r.tx, mode: modeInTx})

	default: // modeOwner
		tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("開 transaction: %w", err)
		}
		// commit 成功後的 Rollback 是 no-op(pgx 回 ErrTxClosed),所以無條件 defer
		// 比在每個錯誤分支各寫一次可靠 —— 少寫一次就是一條洩漏連線的路徑。
		defer func() { _ = tx.Rollback(ctx) }()

		if err := fn(ctx, &Repo{q: r.q.WithTx(tx), tx: tx, mode: modeInTx}); err != nil {
			return err // 規則層的 sentinel 原樣透傳,不再包裝
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}
}

// ── 錯誤翻譯 ────────────────────────────────────────────────────
//
// 裸的 *pgconn.PgError 一旦冒到 transport 層,就只能變成 500 ——
// 但「預算已存在」「已經封盤」對使用者都是 4xx 的正常結果。
// 翻譯集中在這裡,每支方法只負責挑對 sentinel。

// pgErrCode 回傳 Postgres 的 SQLSTATE;不是 PgError 時回空字串。
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// violates 回報 err 是否為指定約束的違反。
//
// 連約束名一起比對而不是只看 SQLSTATE:同一張表日後可能長出第二個同類約束,
// 那時「任何 23503 都當成沒有預算」會把新約束的違反翻譯成一個誤導的錯誤。
func violates(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}

// SQLSTATE 與約束名。字面值集中一處,免得散在各方法裡拼錯又不會有人發現。
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"

	constraintBudgetSpentCheck = "match_budgets_spent_check"
	constraintSelectionBudget  = "handicap_selections_budget_fkey"
)

// deref 把可空欄位攤成零值。
//
// 讓武的 core 型別刻意不用指標:P1PlayerID = 0 就是「該側尚未確定」,
// 呼叫端只要比 0 而不必先判 nil。轉換的責任在 adapter,不在每個讀取端。
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
