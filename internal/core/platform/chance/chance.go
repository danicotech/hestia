// Package chance 是開箱、小遊戲、賽事下注共用的抽籤機制(schemas/25)。
//
// 為什麼要共用:三處都會產生隨機結果,各寫各的就無法回答
// 「這個月系統總共吐出多少點」—— 而那是唯一能在通膨之前發現通膨的方法。
//
// 這個套件只有純函數與型別。抽籤留痕(chance_draws)與動錢由儲存層負責,
// 因為那兩件事都要在同一個 transaction 裡,而 core 不碰 tx。
package chance

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
)

// Kind 是抽籤的場景。對應 chance_draws.kind。
type Kind string

const (
	KindLootBox  Kind = "loot_box"
	KindMinigame Kind = "minigame"
	KindBetting  Kind = "betting"
)

var (
	// ErrNoOutcomes 是獎池空了(全部有限項目都抽完)。
	// 這不是程式錯誤,是營運狀態 —— 呼叫端該告訴使用者「這個箱子已經開完了」。
	ErrNoOutcomes = errors.New("獎池裡沒有可抽的項目")
	// ErrInvalidStake 是投注額不合法。
	ErrInvalidStake = errors.New("投注額不合法")
	// ErrDailyLimit 是超過每日次數上限。
	ErrDailyLimit = errors.New("今天的次數已經用完")
)

// Outcome 是獎池裡的一個可能結果。
//
// Weight 是**相對權重不是百分比**:加一個新獎品時不必重算其他所有項目,
// 而那正是每次都會算錯的地方。實際機率 = Weight / Σ(Weight)。
type Outcome struct {
	// Ref 讓呼叫端認得抽到的是哪一項(loot_box_items.id、遊戲的結果代號)。
	Ref    string
	Weight int64
	// Remaining < 0 表示無限。0 表示已抽完,不參與抽籤。
	Remaining int
}

// Draw 是一次抽籤的完整紀錄,直接對應 chance_draws 的欄位。
type Draw struct {
	Kind Kind
	Ref  string
	// OddsSnapshot 是抽籤**當下**的賠率表,不是指向設定的參照。
	//
	// 賠率會改。存參照的話,三個月後查這筆紀錄看到的是今天的賠率,
	// 那份紀錄就失去了證明力 —— 與帳本 append-only 是同一個原則。
	OddsSnapshot []Outcome
	Seed         string
	Winner       string
	Stake        int64
	Payout       int64
}

// Pick 從獎池抽一個結果,回傳中選項的索引。
//
// 只有 Remaining != 0 的項目參與:有限獎池抽完就退出,
// 這是 max_supply(「全世界只有 1 個」)在抽獎場景的實現。
//
// 亂數一律用 crypto/rand。**絕不降級到 math/rand**:可預測的抽獎結果
// 等於可被利用,而且它不會報錯,只會安靜地變成別人的提款機。
func Pick(outcomes []Outcome) (index int, seed string, err error) {
	var total int64
	eligible := make([]int, 0, len(outcomes))
	for i, o := range outcomes {
		if o.Remaining == 0 || o.Weight <= 0 {
			continue
		}
		eligible = append(eligible, i)
		total += o.Weight
	}
	if total <= 0 {
		return 0, "", ErrNoOutcomes
	}

	// 種子與抽籤結果一起存進 chance_draws:有人質疑黑箱時拿得出來。
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return 0, "", fmt.Errorf("產生亂數種子: %w", err)
	}
	seed = hex.EncodeToString(raw)

	n, err := rand.Int(rand.Reader, big.NewInt(total))
	if err != nil {
		return 0, "", fmt.Errorf("抽籤: %w", err)
	}
	roll := n.Int64()
	for _, i := range eligible {
		roll -= outcomes[i].Weight
		if roll < 0 {
			return i, seed, nil
		}
	}
	// 走到這裡代表權重總和與迴圈不一致,是程式錯誤而不是資料問題。
	return 0, "", errors.New("抽籤未命中任何項目(權重計算錯誤)")
}

// RollBelow 回傳 [0, n) 的均勻亂數,附帶種子。小遊戲用。
func RollBelow(n int64) (roll int64, seed string, err error) {
	if n <= 0 {
		return 0, "", fmt.Errorf("範圍必須為正: %w", ErrInvalidStake)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return 0, "", fmt.Errorf("產生亂數種子: %w", err)
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, "", fmt.Errorf("抽籤: %w", err)
	}
	return v.Int64(), hex.EncodeToString(raw), nil
}
