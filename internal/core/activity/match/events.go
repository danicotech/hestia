package match

import (
	"encoding/json"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/bracket"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// outbox topic。五則公告裡有三則(開盤、封盤、賽果)是使用者點名要的,
// 所以它們不是「順便發的通知」,而是這條生命週期的產出之一。
const (
	// TopicHandicapOpened 開盤提醒。低段位者這時才拿到 BP,公告要提醒他
	// 「你有 N BP 還沒花」—— 沒有這則,BP 常常放到封盤都沒動。
	TopicHandicapOpened = "match.handicap_opened"
	// TopicHandicapLocked 封盤公示。**最重要的一則**:讓武內容在這一刻
	// 對所有人公開,而公告就是對手與觀眾的唯一資訊來源。
	// payload 帶完整清單,是「參加者不混亂」的主要解方。
	TopicHandicapLocked = "match.handicap_locked"
	// TopicMatchStarted 開打(同時關閉下注)。
	TopicMatchStarted = "match.started"
	// TopicMatchFinished 賽果 + 晉級。不戰而勝也走這則,靠 result.kind 區分。
	TopicMatchFinished = "match.finished"
	// TopicChampion 冠軍。決賽判定時額外發一則 —— 冠軍是一屆賽事的結論,
	// 讓它混在某一場的賽果公告裡會被滑過去。
	TopicChampion = "tournament.champion"
)

// SideInfo 是公告裡「一位選手」要顯示的全部欄位。
//
// 段位同時給 level 與名稱:level 是權威(算 BP 用),名稱是逐屆可調的措辭
// (tournaments.config),前端不該自己寫死中文。
type SideInfo struct {
	PlayerPublicID string  `json:"player_public_id"`
	DisplayName    string  `json:"display_name"`
	RankLevel      bp.Rank `json:"rank_level"`
	RankName       string  `json:"rank_name"`
	RankTitle      string  `json:"rank_title,omitempty"`
}

// HandicapItemInfo 是讓武清單裡的一項。
//
// 每買一次一項,**不聚合**:同一項買三次就是三筆。聚合成 ×3 是呈現方式,
// 由 renderer 決定;把它做進 payload 等於在這裡替所有渠道決定了排版。
type HandicapItemInfo struct {
	ItemRef  string            `json:"item_ref"`
	Name     string            `json:"name"`
	Category handicap.Category `json:"category"`
	Cost     int64             `json:"cost"`
	// TargetNote 是「指定對手武學:XX」這類需要填內容的項目的內容。
	TargetNote string `json:"target_note,omitempty"`
}

// HandicapInfo 是公告裡的讓武區塊。
//
// 為什麼 payload 要自己夠用:renderer 收到事件時再回查資料庫,公告的正確性
// 就取決於「查的時候資料還在不在、改了沒」。封盤公示尤其不能這樣 ——
// 它公示的是**封盤當下**那一份清單。
type HandicapInfo struct {
	// HolderPlayerPublicID 是施加者(低段位方)。空 = 本場無讓武。
	HolderPlayerPublicID string `json:"holder_player_public_id,omitempty"`
	HolderDisplayName    string `json:"holder_display_name,omitempty"`
	// ConstrainedPlayerPublicID 是受限方(高段位方)。
	ConstrainedPlayerPublicID string `json:"constrained_player_public_id,omitempty"`
	ConstrainedDisplayName    string `json:"constrained_display_name,omitempty"`
	Budget                    int64  `json:"budget"`
	Spent                     int64  `json:"spent"`
	// Remaining 是還沒花掉的 BP。開盤提醒要的就是這個數字。
	Remaining int64              `json:"remaining"`
	Items     []HandicapItemInfo `json:"items"`
}

// ResultInfo 是賽果區塊。
type ResultInfo struct {
	Kind   ResultKind `json:"kind"`
	Winner *SideInfo  `json:"winner,omitempty"`
	Loser  *SideInfo  `json:"loser,omitempty"`
	// NextMatchPublicID 是勝者晉級到的場次;空 = 這是決賽。
	NextMatchPublicID string `json:"next_match_public_id,omitempty"`
	NextRoundLabel    string `json:"next_round_label,omitempty"`
	// SettledBetCount / VoidedBetCount 是本場影響的注單數。
	// 公告寫得出「本場結算 12 張注單」,觀眾才知道派彩確實發生了。
	SettledBetCount int `json:"settled_bet_count,omitempty"`
	VoidedBetCount  int `json:"voided_bet_count,omitempty"`
}

// MatchEvent 是四則場次公告共用的 payload。
//
// 共用一個形狀而不是一則一個結構:四則公告顯示的主體是同一場比賽,
// 差別只在多帶哪一個區塊。分成四個結構的話,「雙方顯示名」這種每一則都要的
// 欄位就會被複製四次,而 renderer 得為每一則寫一套解析。
type MatchEvent struct {
	TournamentSlug string `json:"tournament_slug"`
	TournamentName string `json:"tournament_name"`
	MatchPublicID  string `json:"match_public_id"`
	Round          int    `json:"round"`
	Slot           int    `json:"slot"`
	// RoundLabel 是「八強」「決賽」這類顯示名,由伺服器算 ——
	// 同一個 round 編號在 8 人賽是四強、在 32 人賽是三十二強,前端沒有足夠資訊判斷。
	RoundLabel string        `json:"round_label"`
	IsFinal    bool          `json:"is_final"`
	Status     Status        `json:"status"`
	StreamURL  string        `json:"stream_url,omitempty"`
	P1         *SideInfo     `json:"p1,omitempty"`
	P2         *SideInfo     `json:"p2,omitempty"`
	Handicap   *HandicapInfo `json:"handicap,omitempty"`
	Result     *ResultInfo   `json:"result,omitempty"`
}

// ChampionEvent 是冠軍公告。
type ChampionEvent struct {
	TournamentSlug string    `json:"tournament_slug"`
	TournamentName string    `json:"tournament_name"`
	Champion       SideInfo  `json:"champion"`
	RunnerUp       *SideInfo `json:"runner_up,omitempty"`
	FinalMatchID   string    `json:"final_match_public_id"`
	TotalRounds    int       `json:"total_rounds"`
}

// eventCtx 是組 payload 需要的上下文:賽事、設定(段位措辭)、對戰表形狀。
//
// 一次讀齊再傳下去,而不是每組一則公告就查一次 —— 同一個 tx 裡的四則公告
// 必須描述同一個快照,分開查就有機會描述到兩個。
type eventCtx struct {
	t   *Tournament
	cfg tournament.Config
	b   *bracket.Bracket
}

// newEventCtx 解析賽事設定並還原對戰表形狀。
//
// ParseConfig 的錯誤刻意不上拋:它保證回傳的設定每個欄位都補過預設值,
// 而「config 裡某個段位名稱打錯字」不該讓一場已經打完的比賽無法判定勝負。
// 壞掉的設定會在讀賽事的路徑上被看見,不需要在這裡再擋一次。
func newEventCtx(t *Tournament) eventCtx {
	cfg, _ := tournament.ParseConfig(t.ConfigRaw)
	return eventCtx{t: t, cfg: cfg, b: bracketOf(t.TotalRounds)}
}

// bracketOf 依總輪數還原一張「只有形狀」的對戰表。
//
// 晉級目標與輪次名稱都是純結構性質(相鄰兩 slot 匯進下一輪同一場、
// 剩餘人數決定輪次名),與選手是誰無關。在這裡還原形狀而不是自己再寫一次
// slot/2 的映射,是因為那個映射的權威在 bracket 套件 —— 寫第二份就會有第二個版本,
// 而「晉級到錯的位置」是人工看對戰表看不出來的 bug。
func bracketOf(totalRounds int) *bracket.Bracket {
	if totalRounds < 0 {
		totalRounds = 0
	}
	return &bracket.Bracket{Size: 1 << totalRounds, TotalRounds: totalRounds}
}

// side 把一位選手轉成公告用的顯示資料。未就座時回 nil。
func (e eventCtx) side(p Player) *SideInfo {
	if !p.Seated() {
		return nil
	}
	s := &SideInfo{
		PlayerPublicID: p.PublicID,
		DisplayName:    p.DisplayName,
		RankLevel:      p.Rank,
	}
	if ri, ok := e.cfg.Rank(p.Rank); ok {
		s.RankName, s.RankTitle = ri.Name, ri.Title
	}
	return s
}

// base 組出四則公告共用的部分。
func (e eventCtx) base(m *Match) MatchEvent {
	return MatchEvent{
		TournamentSlug: e.t.Slug,
		TournamentName: e.t.Name,
		MatchPublicID:  m.PublicID,
		Round:          m.Round,
		Slot:           m.Slot,
		RoundLabel:     e.b.RoundLabel(m.Round),
		IsFinal:        m.Round == e.t.TotalRounds,
		Status:         m.Status,
		StreamURL:      m.StreamURL,
		P1:             e.side(m.P1),
		P2:             e.side(m.P2),
	}
}

// budgetInfo 是開盤時的讓武區塊:只有預算,還沒有任何選擇。
// budget 為 nil(同段對決)時回 nil —— 公告因此不會出現一個「0 BP」的空區塊。
func (e eventCtx) budgetInfo(m *Match, b *handicap.Budget) *HandicapInfo {
	if b == nil {
		return nil
	}
	holder, constrained := m.P1, m.P2
	if b.PlayerID == m.P2.ID {
		holder, constrained = m.P2, m.P1
	}
	return &HandicapInfo{
		HolderPlayerPublicID:      holder.PublicID,
		HolderDisplayName:         holder.DisplayName,
		ConstrainedPlayerPublicID: constrained.PublicID,
		ConstrainedDisplayName:    constrained.DisplayName,
		Budget:                    b.Budget,
		Spent:                     b.Spent,
		Remaining:                 b.Remaining(),
		Items:                     []HandicapItemInfo{},
	}
}

// lockedInfo 是封盤時的讓武區塊:完整清單。
//
// 這是整個套件裡 payload 最重要的一則。清單來自 handicap 封盤當下回傳的檢視,
// 不重新查一次:封盤與公告必須描述同一個瞬間。
func (e eventCtx) lockedInfo(m *Match, v *handicap.MatchHandicaps) *HandicapInfo {
	if v == nil || v.HolderPlayerPublicID == "" {
		return nil // 本場無讓武(同段對決)
	}
	holder, constrained := m.P1, m.P2
	if v.HolderPlayerPublicID == m.P2.PublicID {
		holder, constrained = m.P2, m.P1
	}
	info := &HandicapInfo{
		HolderPlayerPublicID:      holder.PublicID,
		HolderDisplayName:         holder.DisplayName,
		ConstrainedPlayerPublicID: constrained.PublicID,
		ConstrainedDisplayName:    constrained.DisplayName,
		Items:                     make([]HandicapItemInfo, 0, len(v.Selections)),
	}
	if v.Budget != nil {
		info.Budget, info.Spent = v.Budget.Budget, v.Budget.Spent
		info.Remaining = v.Budget.Remaining()
	}
	for _, s := range v.Selections {
		info.Items = append(info.Items, HandicapItemInfo{
			ItemRef:    s.ItemRef,
			Name:       s.ItemName,
			Category:   s.Category,
			Cost:       s.Cost,
			TargetNote: s.TargetNote,
		})
	}
	return info
}

// event 把 payload 包成 outbox 事件。
//
// json.Marshal 對這些結構不可能失敗(純量、字串與它們的切片),
// 忽略錯誤不會掩蓋任何東西 —— 與 betting.eventFor 同一個判斷。
func event(topic string, payload any) Event {
	raw, _ := json.Marshal(payload)
	return Event{Topic: topic, Payload: raw}
}
