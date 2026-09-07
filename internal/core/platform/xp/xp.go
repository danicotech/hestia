// Package xp 是 XP 寫入與投影的對外介面(schemas/06)。
//
// 設計核心:XP 與代幣二分 —— XP 只由事件累積決定等級,代幣可花費。
// XP 不進帳本、不經 Ledger,這是 schemas/06 的刻意設計:XP 錯了從 xp_events
// 重算即可,沒有對帳需求;混進帳本會淹沒金流稽核。
//
// M1 範圍:只收資料 —— 不算 level(恆 0)、不發升級訊息、不做排行榜。
// xp_events 是事實、user_xp 是投影;投影必須可重算(Rebuild)是全域慣例。
package xp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SourceAdmin 是管理員手動調整的事件來源 key(migration 00015 seed)。
// 它是唯一允許負數 Amount 的來源:XP 只增不減是設計核心(schemas/06),
// 一般 emitter 沒有扣 XP 的正當理由,負數僅限管理員修正。
const SourceAdmin = "admin"

// SourceRule 是單一事件來源的每社群規則(xp_rulesets.config.sources[key],
// 形狀定於 schemas/01 增補 A)。registry(xp_event_types)管全域存在與開關,
// 這裡管數值/冷卻/上限 —— 分工不重複。
type SourceRule struct {
	XP              int64 `json:"xp"`
	XPPerMinute     int64 `json:"xp_per_minute"`
	CooldownSeconds int64 `json:"cooldown_seconds"` // > 0 才啟用冷卻
	DailyCap        int64 `json:"daily_cap"`        // > 0 才啟用每日上限;缺省 = 不設限
}

// Config 是 xp_rulesets.config 的 Go 形狀。
// curve 欄位 M1 不讀(level 恆 0,schemas/06),等級功能上線時再補型別。
type Config struct {
	Sources map[string]SourceRule `json:"sources"`
}

// ParseConfig 解析 xp_rulesets.config。raw 為空(community 尚未指派 ruleset,
// M1 可能如此)時回安全預設:無冷卻、無 cap —— 寧可多記事件(事後可重算裁減),
// 不可漏記(XP 是補不回來的歷史)。
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("解析 xp_rulesets.config: %w", err)
	}
	return c, nil
}

// AwardParams 是一次 XP 入帳請求。
type AwardParams struct {
	UserID      int64
	CommunityID int64
	SpaceID     *int64 // 來源空間,可選(重算/拆池時用)
	Source      string // xp_event_types.key
	Amount      int64  // 由 emitter 依規則算好;admin 來源可為負(手動增減)
	RefID       *string
}

// AwardResult 是入帳結果。冷卻與 cap 攔下是高頻的正常路徑(每則冷卻窗內的
// 發言都會發生),用旗標而非 error 表達;error 保留給呼叫端用錯(來源不存在
// /停用、參數不合法)與基礎設施故障。
type AwardResult struct {
	Awarded    int64 // 實際入帳量;daily_cap 可能裁小,0 = 本次未入帳
	OnCooldown bool  // 冷卻期內被攔,未入帳
	Capped     bool  // 觸及 daily_cap(裁小或裁到 0)
	XP         int64 // user_xp.xp 現值(入帳後;被攔時為攔下當時值)
}

// 錯誤語意:呼叫端據此決定回應方式。FK 也擋得住不存在的 source,
// 但先查 registry 才能給出可讀錯誤而不是資料庫約束錯誤。
var (
	ErrUnknownSource  = errors.New("xp source 不存在於 xp_event_types")
	ErrSourceDisabled = errors.New("xp source 已全域停用")
	ErrInvalidParams  = errors.New("xp 參數不合法")
)

// Service 是 XP 的對外介面。M1 為 in-process 實作。
type Service interface {
	// Award 在單一 transaction 內完成:source 檢查 → 冷卻 → daily_cap 裁量
	// → INSERT xp_events + 更新 user_xp 投影。
	Award(ctx context.Context, p AwardParams) (*AwardResult, error)
	// Rebuild 從 xp_events 重算 user_xp 並覆寫,回傳重算後的 xp。
	// 投影必須可重算是全域慣例(曲線調整、事後裁減都靠它)。
	Rebuild(ctx context.Context, userID, communityID int64) (int64, error)
}
