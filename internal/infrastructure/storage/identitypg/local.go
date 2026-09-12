package identitypg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/platform/identity"
	"github.com/danicotech/hestia/internal/shared/secret"
)

// 本地登入(provider='local',migration 00031)。
//
// 存在理由見 identity.ProviderLocal:裁判的動作必須記在平台帳號上,
// 但辦賽事的人不該為此被迫去接 Discord。
//
// 帳號由 cmd/admin 建立,這裡只負責驗證與發 session —— 發出來的東西
// 與 Discord 登入的完全同型,所以後續沒有任何一支 RPC 需要分辨來源。

// LocalLogin 以登入名 + 通行碼換一個 session。
//
// # 四條失敗路徑,一個回應
//
//	查無此登入名 / 這個身分沒有 secret_hash / 通行碼錯 / 帳號已軟刪除
//
// 全部回 identity.ErrInvalidCredentials,而且**做完等量的工**:
// 前三條在回傳前都跑一次完整的雜湊(查無此人跑誘餌),第四條的檢查
// 刻意排在 Verify 之後。只統一回應而不補平時間,登入端點就是一支
// 慢一點的帳號列舉器 —— 而這裡能列舉出來的是握有判勝負與發獎能力的帳號。
func (s *Service) LocalLogin(
	ctx context.Context, loginName, passcode string, dev identity.DeviceInfo,
) (*identity.Session, error) {
	// 正規化的權威在 identity(寫入端 cmd/admin 用的是同一支):
	// 兩邊各寫一份的話,"Judge" 建出來的帳號會登不進去,而症狀是
	// 「通行碼不正確」—— 沒有人查得到真正的原因。
	name := identity.NormalizeLoginName(loginName)
	code := secret.NormalizePasscode(passcode)

	row, err := s.q.GetLocalIdentity(ctx, name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.hasher.VerifyDummy(code)
		return nil, identity.ErrInvalidCredentials
	case err != nil:
		return nil, fmt.Errorf("查本地身分: %w", err)
	}

	// provider='local' 卻沒有 secret_hash:資料不完整(建帳中途失敗)。
	// 一樣跑誘餌,不讓「有這個名字但沒密碼」變成一種可觀測的狀態。
	if row.SecretHash == nil {
		s.hasher.VerifyDummy(code)
		return nil, identity.ErrInvalidCredentials
	}

	ok, err := s.hasher.Verify(*row.SecretHash, code)
	if err != nil {
		// 雜湊本身壞了是需要有人去看的系統問題,但**不能**讓使用者知道 ——
		// 「這個登入名的雜湊格式不對」等於承認這個登入名存在。
		// 現場的症狀是這個人永遠登不進去,用 admin create-judge --reset-passcode 修。
		s.log.Error("本地身分的 secret_hash 讀不懂,該帳號無法登入",
			"identity_id", row.IdentityID, "err", err)
		return nil, identity.ErrInvalidCredentials
	}
	if !ok {
		return nil, identity.ErrInvalidCredentials
	}

	// 軟刪除的檢查排在驗證**之後**:排在前面的話,已註銷的登入名會少做一次
	// 雜湊而回得比較快,於是註銷過的帳號反而變得可辨識。
	if row.UserDeletedAt != nil {
		return nil, identity.ErrInvalidCredentials
	}

	sess, err := s.localLoginTx(ctx, row.UserID, dev)
	if err != nil {
		// issueSession 會在取鎖後重讀 users(併發軟刪除的那道防線),
		// 它回的 ErrAccountDeleted 在這條路徑上必須折成同一個錯誤。
		if errors.Is(err, identity.ErrAccountDeleted) {
			return nil, identity.ErrInvalidCredentials
		}
		return nil, err
	}
	return sess, nil
}

// localLoginTx 是 LocalLogin 的單一 transaction 部分:發 session + 更新 last_seen_at。
//
// 沒有「首次登入」分支 —— local 身分不會自助註冊(見 identity.ProviderLocal),
// 所以這裡不建 users、不發註冊禮:帳號是 cmd/admin 事先建好的。
func (s *Service) localLoginTx(
	ctx context.Context, userID int64, dev identity.DeviceInfo,
) (*identity.Session, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	sess, err := s.issueSession(ctx, qtx, userID, dev, nil)
	if err != nil {
		return nil, err
	}
	if err := qtx.TouchLoginUser(ctx, userID); err != nil {
		return nil, fmt.Errorf("更新 last_seen_at: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return sess, nil
}
