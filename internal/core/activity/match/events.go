package match

import (
	"encoding/json"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/bracket"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/rules"
)

// outbox topic。五則公告裡有三則(開盤、封盤、賽果)是使用者點名要的,
// 所以它們不是「順便發的通知」,而是這條生命週期的產出之一。
//
// 回合的兩則(TopicRoundStarted / TopicRoundFinished)**只推即時戰況、不進 outbox**
// (Event.Announced 回 false):沒有任何消費者要貼它 —— 使用者要的公告是賽果與
// 晉級,回合比數寫在賽果那則裡;而進了 outbox 沒人認領的事件會退避到 failed。
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
	// TopicRoundStarted 一回合的正式決鬥開始(計時起點)。只推即時戰況。
	TopicRoundStarted = "match.round_started"
	// TopicRoundFinished 一回合結束、比數更新。只推即時戰況。
	// 整場因此定案時,TopicMatchFinished 會在同一個 tx 裡緊接著發。
	TopicRoundFinished = "match.round_finished"
)

// Announced 回報這則事件要不要進 outbox(對外公告)。
//
// 回合事件只推即時戰況:它們在 AppendEvents 裡一樣經 NOTIFY 送出(與領域變更同 tx),
// 但不寫 outbox_events —— 那張表上每一列都要有人認領,而回合沒有公告可貼。
func (e Event) Announced() bool {
	switch e.Topic {
	case TopicRoundStarted, TopicRoundFinished:
		return false
	default:
		return true
	}
}

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

// ScoreInfo 是公告裡的回合比數(例「2:1」)。
//
// 只在多回合制或有回合紀錄時出現;不戰而勝沒有回合,這一塊是 nil。
type ScoreInfo struct {
	P1Wins int `json:"p1_wins"`
	P2Wins int `json:"p2_wins"`
	// Text 是「2:1」的字面(P1 在前),renderer 直接貼。
	Text string `json:"text"`
	// WinsNeeded 是本屆的獲勝門檻(⌈best_of/2⌉),公告寫得出「三局兩勝」。
	WinsNeeded int `json:"wins_needed"`
}

// ViolationInfo 是賽果公告裡的一筆判負原因(grill Q14)。
//
// 只有 ruling ∈ {round_loss, match_loss} 的違規會進公告;警告與不判的不揭露。
// 違規本身**不單發公告**。
type ViolationInfo struct {
	PlayerPublicID string `json:"player_public_id"`
	DisplayName    string `json:"display_name"`
	// RoundNo 0 = 開賽前。
	RoundNo int    `json:"round_no,omitempty"`
	Ruling  Ruling `json:"ruling"`
	// ItemName 是違反的讓武項目名稱;空 = 違反通則。
	ItemName string `json:"item_name,omitempty"`
	Note     string `json:"note"`
}

// RoundInfo 是回合區塊:一回合的起訖與到目前為止的比數。
//
// 計時器是衍生值:前端算 now() − StartedAt,伺服器只給起始時間。
type RoundInfo struct {
	RoundNo    int        `json:"round_no"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Winner     *SideInfo  `json:"winner,omitempty"`
	Score      ScoreInfo  `json:"score"`
}

// ResultInfo 是賽果區塊。
type ResultInfo struct {
	Kind   ResultKind `json:"kind"`
	Winner *SideInfo  `json:"winner,omitempty"`
	Loser  *SideInfo  `json:"loser,omitempty"`
	// Score 是回合比數;不戰而勝為 nil。
	Score *ScoreInfo `json:"score,omitempty"`
	// Violations 是判負原因(只含 round_loss / match_loss),沒有就是空。
	Violations []ViolationInfo `json:"violations,omitempty"`
	// NextMatchPublicID 是勝者晉級到的場次;空 = 這是決賽或季軍戰。
	NextMatchPublicID string `json:"next_match_public_id,omitempty"`
	NextRoundLabel    string `json:"next_round_label,omitempty"`
	// ThirdPlaceMatchPublicID 非空 = 這場準決賽判定後季軍戰成形了。
	ThirdPlaceMatchPublicID string `json:"third_place_match_public_id,omitempty"`
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
	RoundLabel string `json:"round_label"`
	IsFinal    bool   `json:"is_final"`
	// Kind 讓 renderer 分得出季軍戰(它的 round 與決賽同一輪,RoundLabel 會是「決賽」)。
	Kind      MatchKind     `json:"kind"`
	Status    Status        `json:"status"`
	StreamURL string        `json:"stream_url,omitempty"`
	P1        *SideInfo     `json:"p1,omitempty"`
	P2        *SideInfo     `json:"p2,omitempty"`
	Handicap  *HandicapInfo `json:"handicap,omitempty"`
	// CurrentRound 在回合事件裡非 nil:剛開始或剛結束的那一回合。
	CurrentRound *RoundInfo  `json:"current_round,omitempty"`
	Result       *ResultInfo `json:"result,omitempty"`
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
	cfg rules.Config
	b   *bracket.Bracket
}

// newEventCtx 解析賽事設定並還原對戰表形狀。
//
// rules.Parse 的錯誤刻意不上拋:它保證回傳的設定每個欄位都補過預設值,
// 而「config 裡某個段位名稱打錯字」不該讓一場已經打完的比賽無法判定勝負。
// 壞掉的設定會在讀賽事的路徑上被看見,不需要在這裡再擋一次。
func newEventCtx(t *Tournament) eventCtx {
	cfg, _ := rules.Parse(t.ConfigRaw)
	return eventCtx{t: t, cfg: cfg, b: bracketOf(t.TotalRounds)}
}

// winsNeeded 是本屆整場勝者要先拿到幾勝(⌈best_of/2⌉)。權威在 rules,這裡只轉手。
func (e eventCtx) winsNeeded() int { return e.cfg.WinsNeeded() }

// isFinal 回報這一場是不是決賽:決賽那一輪的**晉級樹**場次。
// 季軍戰的 round 與決賽相同,但它不是決賽 —— 沒有冠軍。
func (e eventCtx) isFinal(m *Match) bool {
	return m.Round == e.t.TotalRounds && !m.IsThirdPlace()
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
	if ri, ok := e.cfg.Rank(int(p.Rank)); ok {
		s.RankName, s.RankTitle = ri.Name, ri.Title
	}
	return s
}

// base 組出四則公告共用的部分。
func (e eventCtx) base(m *Match) MatchEvent {
	kind := m.Kind
	if kind == "" {
		kind = KindBracket
	}
	return MatchEvent{
		TournamentSlug: e.t.Slug,
		TournamentName: e.t.Name,
		MatchPublicID:  m.PublicID,
		Round:          m.Round,
		Slot:           m.Slot,
		RoundLabel:     e.b.RoundLabel(m.Round),
		IsFinal:        e.isFinal(m),
		Kind:           kind,
		Status:         m.Status,
		StreamURL:      m.StreamURL,
		P1:             e.side(m.P1),
		P2:             e.side(m.P2),
	}
}

// scoreInfo 把比數翻成公告用的區塊。
func (e eventCtx) scoreInfo(s Score) ScoreInfo {
	return ScoreInfo{P1Wins: s.P1Wins, P2Wins: s.P2Wins, Text: s.String(), WinsNeeded: e.winsNeeded()}
}

// roundInfo 是回合區塊:那一回合本身,加上到目前為止(含它)的比數。
func (e eventCtx) roundInfo(m *Match, r Round, rounds []Round) *RoundInfo {
	info := &RoundInfo{
		RoundNo:    r.RoundNo,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		Score:      e.scoreInfo(ScoreOf(*m, rounds)),
	}
	if r.WinnerPlayerID != 0 {
		info.Winner = e.side(m.PlayerByID(r.WinnerPlayerID))
	}
	return info
}

// violationInfos 挑出要進賽果公告的違規(只有判負的,grill Q14)。
func violationInfos(vs []Violation) []ViolationInfo {
	var out []ViolationInfo
	for _, v := range vs {
		if !v.Ruling.Announced() {
			continue
		}
		info := ViolationInfo{
			PlayerPublicID: v.PlayerPublicID,
			DisplayName:    v.PlayerDisplayName,
			Ruling:         v.Ruling,
			ItemName:       v.ItemName,
			Note:           v.Note,
		}
		if v.RoundNo != nil {
			info.RoundNo = *v.RoundNo
		}
		out = append(out, info)
	}
	return out
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
