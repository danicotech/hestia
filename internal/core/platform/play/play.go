// Package play 是小遊戲、開箱、抽獎、寵物的契約層(schemas/25)。
//
// 這四個在架構上不是同一件事(一個是水槽、一個是水龍頭、一個是轉移、
// 一個是養成),但在使用者眼裡是同一類:「可以玩的東西」。放同一個套件
// 是因為它們共用抽籤機制與同一組錯誤語意,不是因為它們像。
package play

import (
	"context"
	"errors"
	"time"
)

// 錯誤語意:呼叫端(入口層)據此決定回哪個狀態碼。
var (
	// ErrNotFound 是箱子/抽獎/寵物不存在。
	ErrNotFound = errors.New("找不到指定的項目")
	// ErrDisabled 是箱子被關掉了。
	ErrDisabled = errors.New("這個項目目前停用")
	// ErrDailyLimit 是超過每日次數上限。
	ErrDailyLimit = errors.New("今天的次數已經用完")
	// ErrPoolEmpty 是有限獎池抽完了。營運狀態,不是程式錯誤。
	ErrPoolEmpty = errors.New("獎池已經抽完")
	// ErrAlreadyEntered 是重複參加抽獎。
	ErrAlreadyEntered = errors.New("已經參加過了")
	// ErrClosed 是抽獎已結束或已開獎。
	ErrClosed = errors.New("這場抽獎已經結束")
	// ErrNotOwner 是操作別人的寵物。
	ErrNotOwner = errors.New("這不是你的寵物")
	// ErrInvalidParams 是參數不合法。
	ErrInvalidParams = errors.New("參數不合法")
)

// ── 小遊戲 ────────────────────────────────────────────────────

// GameResult 是一局小遊戲的結果,已經完成扣款與派彩。
type GameResult struct {
	Game   string
	Choice string
	Result string
	// Stake 是投注額,Payout 是回收(含本金)。
	// Net = Payout - Stake,負數是正常的(期望值 < 1)。
	Stake   int64
	Payout  int64
	Balance int64 // 結算後的餘額
	Seed    string
}

// Net 是這一局的淨變動。
func (r GameResult) Net() int64 { return r.Payout - r.Stake }

// ── 開箱 ──────────────────────────────────────────────────────

// LootBoxView 是箱子的對外檢視。**不揭露獎池內容的機率**:
// 那是刻意的營運決定還是刻意隱瞞,由呈現層與說明文件決定,
// 而不是由「讀取側剛好有這個欄位」決定。
type LootBoxView struct {
	PublicID   string
	Name       string
	Currency   string
	Cost       int64
	DailyLimit int32 // 0 = 不限
}

// OpenResult 是開一次箱子的結果。
type OpenResult struct {
	BoxName    string
	RewardKind string // tokens / item / entitlement
	RewardName string // 顯示用名稱
	Amount     int64  // tokens 時的金額
	Balance    int64  // 結算後餘額
	Seed       string
}

// ── 抽獎活動 ──────────────────────────────────────────────────

// GiveawayView 是進行中的抽獎。
type GiveawayView struct {
	PublicID    string
	Title       string
	EntryCost   int64
	WinnerCount int32
	Entries     int64
	ClosesAt    time.Time
}

// DrawResult 是開獎結果。
type DrawResult struct {
	Title   string
	Winners []Winner
}

// Winner 是一位得獎者。
type Winner struct {
	UserPublicID string
	DisplayName  string
}

// ── 寵物 ──────────────────────────────────────────────────────

// PetListItem 是「我的寵物」清單的一列。
type PetListItem struct {
	PublicID string
	Name     string
	Rarity   string
	IconURL  string
	XP       int64
	Level    int32
	Deployed bool
}

// Service 是這四個功能的對外介面。
//
// 放一個介面而不是四個:它們共用同一組錯誤語意與同一個抽籤留痕,
// 拆開只會讓入口層拿四個欄位指向同一個實作。
type Service interface {
	// PlayGame 跑一局小遊戲。扣款、派彩、留痕在同一個 transaction。
	//
	// idempotencyKey 必填(動錢一律冪等,帳本鐵則三)。Discord 的
	// interaction id 是天然的鍵:同一次點擊重送只會結算一次。
	PlayGame(ctx context.Context, p GameParams) (*GameResult, error)

	ListBoxes(ctx context.Context, communityID int64) ([]LootBoxView, error)
	OpenBox(ctx context.Context, p OpenParams) (*OpenResult, error)

	ListGiveaways(ctx context.Context, communityID int64) ([]GiveawayView, error)
	EnterGiveaway(ctx context.Context, userID int64, publicID string) error
	DrawGiveaway(ctx context.Context, publicID string) (*DrawResult, error)

	ListPets(ctx context.Context, userID int64) ([]PetListItem, error)
	DeployPet(ctx context.Context, userID int64, petPublicID string) error
	RenamePet(ctx context.Context, userID int64, petPublicID, nickname string) error
}

// GameParams 是一局小遊戲的請求。
type GameParams struct {
	UserID         int64
	CommunityID    int64
	Game           string
	Choice         string
	Stake          int64
	IdempotencyKey string
}

// OpenParams 是一次開箱的請求。
type OpenParams struct {
	UserID         int64
	BoxPublicID    string
	IdempotencyKey string
}

// MaxDrawsPerDay 是每人每日的抽籤次數上限(schemas/25 定案:20)。
//
// 小遊戲 + 開箱 + 賽事下注三個管道共用這個數字。一個欄位同時處理
// 兩件事:經濟失控,以及「有人一天輸光然後去檢舉」。
const MaxDrawsPerDay = 20
