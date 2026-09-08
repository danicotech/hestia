package readpg_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/core/platform/shop"
)

// 兩級退出設定(schemas/02-identity.md)。要證明的:
//   - **沒有設定列不是錯誤**,回預設值 —— 第一次用 /privacy 的人必然沒有列;
//   - 兩個布林各自可設,沒帶的維持原值(不是被重設為 false);
//   - 併發改同一個人的兩項設定不會互相覆蓋;
//   - 軟刪除的帳號讀不到也寫不進。

func boolPtr(b bool) *bool { return &b }

func TestPrivacyDefaultsWhenNoRow(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "隱私預設")

	v, err := svc.Privacy(ctx, userID)
	if err != nil {
		t.Fatalf("沒有設定列不該是錯誤:%v", err)
	}
	if v.OptOutLogging || v.OptOutAICorpus {
		t.Errorf("預設應該兩者皆 false,拿到 %+v", v)
	}
	if v.UpdatedAt != nil {
		t.Errorf("從未設定過的人 UpdatedAt 應該是 nil,拿到 %v", *v.UpdatedAt)
	}
}

// 第一次寫入建列;沒指定的那一項落在預設值 false。
func TestSetPrivacyCreatesRow(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "隱私建列")

	v, err := svc.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{
		OptOutLogging: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("SetPrivacy: %v", err)
	}
	if !v.OptOutLogging || v.OptOutAICorpus {
		t.Fatalf("想要 logging=true corpus=false,拿到 %+v", v)
	}
	if v.UpdatedAt == nil {
		t.Error("建列後 UpdatedAt 不該是 nil")
	}

	// 讀回來要一致(權威只有一份:user_privacy_settings)。
	got, err := svc.Privacy(ctx, userID)
	if err != nil {
		t.Fatalf("Privacy: %v", err)
	}
	if got.OptOutLogging != v.OptOutLogging || got.OptOutAICorpus != v.OptOutAICorpus {
		t.Errorf("讀回來 %+v 與寫入結果 %+v 不一致", got, v)
	}
}

// 第二次寫入只動指定的那一項,另一項維持原值 —— 這是 optional 布林的重點:
// 「沒帶」不等於「設成 false」,否則使用者關掉一個旗標會把另一個一起關掉。
func TestSetPrivacyPartialUpdateKeepsOther(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "隱私部分更新")

	if _, err := svc.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{
		OptOutLogging:  boolPtr(true),
		OptOutAICorpus: boolPtr(true),
	}); err != nil {
		t.Fatalf("初始設定: %v", err)
	}

	v, err := svc.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{
		OptOutLogging: boolPtr(false), // 只關掉這一項
	})
	if err != nil {
		t.Fatalf("SetPrivacy: %v", err)
	}
	if v.OptOutLogging {
		t.Error("opt_out_logging 應該被關掉")
	}
	if !v.OptOutAICorpus {
		t.Error("沒帶的 opt_out_ai_corpus 被連帶重設了 —— 這正是 optional 要防的事")
	}
}

func TestSetPrivacyRejectsEmptyUpdate(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "隱私空更新")
	if _, err := svc.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{}); !errors.Is(err, shop.ErrInvalidRequest) {
		t.Fatalf("想要 ErrInvalidRequest,拿到 %v", err)
	}
}

// 併發:兩個請求各改一項,兩項都要留下來。
// 用真資料庫測(ON CONFLICT DO UPDATE 的重讀語意),mock 掉就什麼都沒測到。
func TestSetPrivacyConcurrentDoesNotClobber(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "隱私併發")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	updates := []readmodel.PrivacyUpdate{
		{OptOutLogging: boolPtr(true)},
		{OptOutAICorpus: boolPtr(true)},
	}
	for i, up := range updates {
		wg.Add(1)
		go func(i int, up readmodel.PrivacyUpdate) {
			defer wg.Done()
			_, errs[i] = svc.SetPrivacy(context.Background(), userID, up)
		}(i, up)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("併發寫入 %d: %v", i, err)
		}
	}

	v, err := svc.Privacy(ctx, userID)
	if err != nil {
		t.Fatalf("Privacy: %v", err)
	}
	if !v.OptOutLogging || !v.OptOutAICorpus {
		t.Errorf("併發的兩項設定互相覆蓋了:%+v", v)
	}
}

// 軟刪除的帳號:讀不到(回預設值,不是別人的資料),也寫不進。
func TestPrivacyIgnoresSoftDeletedUser(t *testing.T) {
	ctx := setup(t)
	userID, _ := newUser(t, "隱私軟刪除")
	if _, err := svc.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{
		OptOutLogging: boolPtr(true),
	}); err != nil {
		t.Fatalf("初始設定: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE platform.users SET deleted_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("軟刪除: %v", err)
	}

	v, err := svc.Privacy(ctx, userID)
	if err != nil {
		t.Fatalf("軟刪除後讀取不該報錯:%v", err)
	}
	if v.OptOutLogging || v.UpdatedAt != nil {
		t.Errorf("軟刪除的帳號不該讀得到自己的設定:%+v", v)
	}
	if _, err := svc.SetPrivacy(ctx, userID, readmodel.PrivacyUpdate{
		OptOutLogging: boolPtr(false),
	}); !errors.Is(err, shop.ErrUserNotFound) {
		t.Fatalf("想要 ErrUserNotFound,拿到 %v", err)
	}
}

func TestPrivacyRejectsInvalidUserID(t *testing.T) {
	ctx := setup(t)
	if _, err := svc.Privacy(ctx, 0); !errors.Is(err, shop.ErrUserNotFound) {
		t.Errorf("Privacy(0):想要 ErrUserNotFound,拿到 %v", err)
	}
	if _, err := svc.SetPrivacy(ctx, -1, readmodel.PrivacyUpdate{
		OptOutLogging: boolPtr(true),
	}); !errors.Is(err, shop.ErrUserNotFound) {
		t.Errorf("SetPrivacy(-1):想要 ErrUserNotFound,拿到 %v", err)
	}
}

// 不存在的使用者寫入 = NotFound,而不是 FK 違反(那會變成 Internal)。
func TestSetPrivacyUnknownUser(t *testing.T) {
	ctx := setup(t)
	if _, err := svc.SetPrivacy(ctx, 1<<40, readmodel.PrivacyUpdate{
		OptOutLogging: boolPtr(true),
	}); !errors.Is(err, shop.ErrUserNotFound) {
		t.Fatalf("想要 ErrUserNotFound,拿到 %v", err)
	}
}
