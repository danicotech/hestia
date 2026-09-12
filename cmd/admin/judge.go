package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/secret"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// create-judge:建立一個不必經過 Discord 就能登入的裁判帳號。
//
// # 為什麼這件事必須從 CLI 做
//
// 建裁判帳號需要一個已經存在的裁判(或管理員)—— 這是先有雞還是先有蛋。
// 第一個裁判只能由握有資料庫的人建,而握有資料庫的人本來就在終端機前面。
// 做成 RPC 反而得先發明一條「還沒有任何管理員時誰能呼叫它」的規則,
// 那條規則會是整個系統最弱的一環。
//
// # judgeRoleKey / judgePasscodeLength
//
// 角色鍵對齊 migration 00030 建立的那一列;它同時是 tournament.judge 權限的唯一來源。

const judgeRoleKey = "judge"

// judgePasscodeLength 是裁判通行碼的長度。
//
// 比選手的 6 碼長一倍是刻意的:選手通行碼保護的是「別人不能改你的讓武選擇」,
// 而且所有破壞性操作還有裁判這道人工關卡;裁判通行碼後面**沒有**任何人工關卡,
// 它直接換到判勝負、封盤與發獎的能力。32^12 ≈ 2^60,線上猜測在任何時間尺度上
// 都不可行,而裁判只需要抄一次(抄完就存進密碼管理器)。
//
// 用的是同一支產生器、同一個字元集(排除 0/O、1/I/l),只有長度不同。
const judgePasscodeLength = 12

func createJudge(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("create-judge", flag.ContinueOnError)
	login := fs.String("login", "", "登入名(a-z 0-9 . _ -,大小寫不敏感)")
	name := fs.String("name", "", "顯示名稱(選填,預設用登入名)")
	reset := fs.Bool("reset-passcode", false, "帳號已存在時重新產生通行碼,而不是報錯")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loginName, err := identity.ValidateLoginName(*login)
	if err != nil {
		if *login == "" {
			return errors.New("--login 必填")
		}
		return err
	}
	display := *name
	if display == "" {
		display = loginName
	}

	// 迭代數用預設值(secret.DefaultHashIterations):建帳號是一次性動作,
	// 慢一點沒有人在意,而這裡調快等於把裁判的通行碼變弱。
	hasher, err := secret.NewHasher(0)
	if err != nil {
		return fmt.Errorf("建立雜湊器: %w", err)
	}
	passcode, err := secret.GenerateCode(judgePasscodeLength)
	if err != nil {
		return fmt.Errorf("產生通行碼: %w", err)
	}
	hash, err := hasher.Hash(passcode)
	if err != nil {
		return fmt.Errorf("雜湊通行碼: %w", err)
	}

	if *reset {
		done, err := resetJudgePasscode(ctx, pool, loginName, hash)
		if err != nil {
			return err
		}
		if done {
			printJudgeCredentials("已重新產生通行碼", loginName, passcode)
			return nil
		}
		// 沒有既有帳號就往下走建立流程(--reset-passcode 等於「確保它可用」)。
	}

	userPublicID, err := createJudgeAccount(ctx, pool, loginName, display, hash)
	if err != nil {
		return err
	}
	fmt.Printf("已建立裁判帳號:\n  使用者  %s\n", userPublicID)
	printJudgeCredentials("", loginName, passcode)
	return nil
}

// createJudgeAccount 在**單一 transaction** 內做完四件事:
// 建 users → 建 identities(provider='local')→ 預設隱私設定 → 授予 judge 角色。
//
// 一個 transaction 不是為了效率,是因為中間任何一步失敗留下的殘骸都會咬人:
// 只有 users 沒有 identities = 一個登不進去的幽靈帳號(而登入名已經被佔走);
// 有身分沒有角色 = 登得進去但每個按鈕都是 permission denied,
// 而第二次執行會因為登入名已存在而失敗,於是這個人就卡在半途了。
func createJudgeAccount(ctx context.Context, pool *pgxpool.Pool, loginName, display, hash string) (string, error) {
	publicID, err := ulid.New()
	if err != nil {
		return "", fmt.Errorf("產生 public_id: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("開啟交易: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	user, err := q.InsertLoginUser(ctx, db.InsertLoginUserParams{
		PublicID:    publicID,
		DisplayName: optional(display),
	})
	if err != nil {
		return "", fmt.Errorf("建立使用者: %w", err)
	}

	// 唯一權威是 UNIQUE(provider, provider_user_id) —— 不先查再寫:
	// 先查後寫在併發下擋不住,而這裡撞鍵的代價只是整個 tx 回滾重來。
	if _, err := q.InsertLocalIdentity(ctx, db.InsertLocalIdentityParams{
		UserID:     user.ID,
		LoginName:  loginName,
		Username:   optional(display),
		SecretHash: hash,
	}); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("登入名 %q 已經有人用了;要重設通行碼請加 --reset-passcode", loginName)
		}
		return "", fmt.Errorf("建立本地身分: %w", err)
	}

	// 隱私設定與 OAuth 首登走同一支:預設列缺席會讓之後每個讀取點
	// 都得處理「沒有這一列」的情況,而那正是第二種預設值的來源。
	if err := q.EnsurePrivacySettings(ctx, user.ID); err != nil {
		return "", fmt.Errorf("建立隱私設定: %w", err)
	}

	n, err := q.GrantRoleByKey(ctx, db.GrantRoleByKeyParams{
		UserID: user.ID, RoleKey: judgeRoleKey,
	})
	if err != nil {
		return "", fmt.Errorf("授予 %s 角色: %w", judgeRoleKey, err)
	}
	// 0 列 = roles 裡沒有 judge(migration 00030 沒跑)。這時候繼續下去會建出
	// 一個登得進去卻什麼都不能做的帳號,而症狀出現在幾天後的賽事現場。
	if n == 0 {
		return "", fmt.Errorf("找不到 %s 角色,請先跑 go run ./cmd/migrate", judgeRoleKey)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("提交: %w", err)
	}
	return user.PublicID, nil
}

// resetJudgePasscode 換掉既有本地身分的通行碼;回報是否真的換到了一列。
//
// 刻意**不撤銷既有 session**:那是另一個動作(而且 identity 已經有
// RevokeAllForUser 這個權威),在這裡順手做等於把兩個概念綁在一起,
// 之後「只想補發通行碼」就沒有不踢人的做法了。
func resetJudgePasscode(ctx context.Context, pool *pgxpool.Pool, loginName, hash string) (bool, error) {
	n, err := db.New(pool).UpdateLocalSecret(ctx, db.UpdateLocalSecretParams{
		LoginName: loginName, SecretHash: hash,
	})
	if err != nil {
		return false, fmt.Errorf("更新通行碼: %w", err)
	}
	return n > 0, nil
}

// printJudgeCredentials 是明碼通行碼**唯一**一次出現的地方。
//
// DB 裡只有雜湊,沒有任何路徑能把它取回來 —— 弄丟了就只能
// create-judge --reset-passcode 換一組新的。
func printJudgeCredentials(headline, loginName, passcode string) {
	if headline != "" {
		fmt.Printf("%s:\n", headline)
	}
	fmt.Printf("  登入名  %s\n  通行碼  %s\n", loginName, passcode)
	fmt.Println("  ↑ 通行碼只會出現這一次(資料庫只存雜湊),請立刻交給本人並從畫面上清掉。")
}

// isUniqueViolation 判斷錯誤是不是唯一鍵違反(23505)。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
