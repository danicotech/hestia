// Package watch 是即時戰況推播的扇出樞紐。
//
// # 為什麼扇出交給 Postgres 的 LISTEN / NOTIFY
//
// 這個決定是為了**正確性**,不是為了省事。
//
// 用 in-process hub 直接發布的話,服務必須在 commit **之後**才 Publish,
// 而「在 commit 之前就 Publish」是非常容易寫出來的一行 —— 把 Publish 放在
// 領域邏輯旁邊看起來完全合理。它的症狀是觀眾看到一個後來被 rollback 的賽果:
// 冠軍橫幅跳出來、幾秒後又消失,而資料庫裡從頭到尾沒有那件事。
//
// NOTIFY 沒有這個縫:Postgres 只在交易 commit 時才把通知送出去,rollback 的
// 交易不會送出任何東西。這個保證由資料庫給,不靠任何人記得把 Publish 放對位置。
// (watchpg 的整合測試直接釘住這一條。)
//
// Redis 雖然已經在 compose 裡,但這件事 Postgres 原生做得到,而且 Redis 給不了
// 上面那個「只在 commit 時送出」的保證 —— 多一個依賴換一個更弱的性質不划算(規則 9)。
//
// # 兩層:信封在資料庫裡飛,完整內容在記憶體裡組
//
// NOTIFY 的 payload 有 8000 bytes 上限,而封盤那則的讓武清單會超過。
// 所以跨交易飛的只有 Envelope(賽事 slug、種類、參照 id),完整內容由
// watchpg 收到信封後讀出來,組成 Update 餵進 Hub。
//
// # 這個套件是純 Go
//
// 沒有 pgx、沒有 protobuf、沒有 I/O。Hub 的每一條性質(慢訂閱者不拖垮別人、
// 關閉時不 panic、不洩漏 goroutine)都能用純 Go 測到滿,而那些性質正是
// 「推播會不會在決賽當下整個停擺」的全部。
package watch

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/danicotech/hestia/internal/core/activity/betting"
	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/handicap"
	"github.com/danicotech/hestia/internal/core/activity/match"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

// Channel 是 LISTEN / NOTIFY 的頻道名,**唯一權威**。
//
// 兩端都從這裡取:watchpg 的 LISTEN 用它,SQL 那側的 pg_notify 也用它
// (刻意做成參數而不是 SQL 裡的字面值,不然頻道名就會有兩個權威位置,
// 而打錯一個字的症狀是「推播完全沒有反應,但每一支測試都過」)。
//
// 只有一個頻道、不按賽事分頻道:同時進行的賽事最多一屆,分頻道只會讓
// LISTEN 得在賽事開跑時動態增減,而那是一個沒有人會記得維護的生命週期。
// 過濾由 Hub 依 slug 做,成本是一次 map 查詢。
const Channel = "activity_watch"

// MaxEnvelopeBytes 是信封的長度上限。
//
// Postgres 的 NOTIFY payload 上限是 8000 bytes,超過會讓**整個交易失敗** ——
// 也就是說一個過長的信封會讓裁判判不了勝負。信封只有 slug + 種類 + 一個 ULID,
// 正常情況約 60 bytes,離上限非常遠;這個檢查存在是為了讓
// 「有人日後往信封裡加欄位」在測試裡就炸,而不是在決賽當天炸。
const MaxEnvelopeBytes = 4000

// Kind 是一則變化的種類。值會出現在 NOTIFY 的信封裡,所以**字面值是契約**:
// 改名等於舊版本的伺服器與新版本的發送端之間互不認得(滾動更新期間會發生)。
type Kind string

const (
	// KindMatch 場次狀態改變:開盤、開打、判定勝負、設直播連結。
	KindMatch Kind = "match"
	// KindHandicapLocked 讓武封盤,完整清單公開。
	KindHandicapLocked Kind = "handicap_locked"
	// KindOdds 票數變動導致賠率變動。
	KindOdds Kind = "odds"
	// KindPhase 賽事階段改變。
	KindPhase Kind = "phase"
	// KindChampion 冠軍產生。
	KindChampion Kind = "champion"
)

// Envelope 是跨交易飛的那個小信封。
//
// 欄位名刻意只有一個字母:它要擠進 NOTIFY 的 8000 bytes,而且每一則變化都會
// 送一次。可讀性由這裡的註解負責,不由 JSON 鍵名負責。
//
// **SQL 側有一份鏡像**:activity_betting.sql 的 NotifyWatchOdds 用
// json_build_object 組同樣三個鍵(投票路徑拿不到 slug 與 public_id,
// 要在 Go 裡組就得多一次往返,而那是每一票都要付的代價)。
// 鍵名對不上的話推播會靜靜失效,所以 watchpg 的整合測試直接解析 SQL 送出的
// 信封並與這裡的型別比對 —— 鏡像有一份被改到,測試就會紅。
type Envelope struct {
	// Tournament 是賽事 slug,也是 Hub 的分組鍵。
	Tournament string `json:"t"`
	// Kind 是變化種類。
	Kind Kind `json:"k"`
	// Ref 是參照 id:場次的 public_id(match / handicap_locked / odds / champion),
	// 或空字串(phase —— 賽事本身已經由 Tournament 指定)。
	Ref string `json:"r"`
}

// Valid 回報這個信封路由得到某個地方。
//
// 沒有 slug 就沒有人收得到(Hub 依 slug 分組),沒有 Kind 就不知道要讀什麼。
// 兩者缺一的信封不該被送出去,也不該在收到時當成正常訊息處理。
func (e Envelope) Valid() bool {
	if e.Tournament == "" || e.Kind == "" {
		return false
	}
	// phase 之外的每一種都指名一個場次;缺 Ref 就讀不出內容。
	return e.Kind == KindPhase || e.Ref != ""
}

// Marshal 把信封編成 NOTIFY 的 payload。
func (e Envelope) Marshal() (string, error) {
	if !e.Valid() {
		return "", fmt.Errorf("信封不完整:tournament=%q kind=%q ref=%q", e.Tournament, e.Kind, e.Ref)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("編碼推播信封: %w", err)
	}
	if len(raw) > MaxEnvelopeBytes {
		// 超長的信封會讓 NOTIFY 失敗,而 NOTIFY 與領域變更同一個交易 ——
		// 也就是說它會讓裁判的動作整個失敗。在這裡擋下來,理由見 MaxEnvelopeBytes。
		return "", fmt.Errorf("推播信封過長(%d > %d bytes)", len(raw), MaxEnvelopeBytes)
	}
	return string(raw), nil
}

// ParseEnvelope 解 NOTIFY 收到的 payload。
func ParseEnvelope(payload string) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return Envelope{}, fmt.Errorf("解析推播信封: %w", err)
	}
	if !e.Valid() {
		return Envelope{}, fmt.Errorf("信封不完整:tournament=%q kind=%q ref=%q", e.Tournament, e.Kind, e.Ref)
	}
	return e, nil
}

// eventRef 是從 outbox payload 裡取出路由所需的三個欄位。
//
// 只解這幾個鍵而不是整個 MatchEvent:信封只需要「哪一屆、哪一場」,
// 而完整內容本來就要重讀(payload 可能大到塞不進 NOTIFY)。
type eventRef struct {
	TournamentSlug     string `json:"tournament_slug"`
	MatchPublicID      string `json:"match_public_id"`
	FinalMatchPublicID string `json:"final_match_public_id"`
}

// topicKinds 是 outbox topic → 推播種類的**唯一**對照表。
//
// 為什麼跟著 topic 走而不是另外在 adapter 裡判斷:這五個 topic 就是
// 「一場比賽的生命週期會對外講的全部的話」(match/events.go),推播要送的
// 也正是同一組時刻。分成兩份判斷,漏的那一則不會有任何錯誤訊息 ——
// 只會是「那個變化在畫面上不會即時出現」,而那要等到活動當天才有人發現。
// 有一支測試釘住「每個 match.Topic* 都在這張表裡」。
var topicKinds = map[string]Kind{
	match.TopicHandicapOpened: KindMatch,
	match.TopicHandicapLocked: KindHandicapLocked,
	match.TopicMatchStarted:   KindMatch,
	match.TopicMatchFinished:  KindMatch,
	match.TopicChampion:       KindChampion,
}

// EnvelopeForEvent 從一則 outbox 事件推出信封。
//
// ok = false 表示這個 topic 與即時戰況無關(例如帳本或通知的事件),
// 呼叫端直接跳過 —— 那不是錯誤。payload 解不開或缺 slug 也回 false:
// 一則路由不出去的通知只會在收端被丟掉,沒有理由讓它害裁判的動作失敗。
func EnvelopeForEvent(topic string, payload []byte) (Envelope, bool) {
	kind, ok := topicKinds[topic]
	if !ok {
		return Envelope{}, false
	}
	var ref eventRef
	if err := json.Unmarshal(payload, &ref); err != nil {
		return Envelope{}, false
	}
	e := Envelope{Tournament: ref.TournamentSlug, Kind: kind, Ref: ref.MatchPublicID}
	if kind == KindChampion {
		// 冠軍那則的場次欄位叫 final_match_public_id(ChampionEvent 的形狀)。
		e.Ref = ref.FinalMatchPublicID
	}
	if !e.Valid() {
		return Envelope{}, false
	}
	return e, true
}

// Champion 是冠軍橫幅需要的快照。
//
// 與其他幾種變化不同,這一則自己帶顯示名與段位:冠軍橫幅是獨立於對戰表的
// 一塊畫面,不該為了顯示一個名字再回頭查對戰表(理由見 watch.proto)。
type Champion struct {
	FinalMatchPublicID  string
	PlayerPublicID      string
	DisplayName         string
	Rank                bp.Rank
	RunnerUpPublicID    string
	RunnerUpDisplayName string
}

// Update 是送給訂閱者的一則變化。
//
// 一則只填一個欄位,由 Kind 指明是哪一個 —— 這裡不用 interface 也不用
// 各自一個型別,是因為它會被扇出到成百上千個 channel,型別斷言與額外配置
// 在那條路徑上沒有價值;而「填了哪一個」由 Kind 一個欄位回答,
// 讀的人不必去比對五個指標。
//
// 型別全部沿用既有的 core 檢視(match.Match / handicap.MatchHandicaps /
// betting.MatchOdds):對戰表頁查詢回的與推播送的是同一個東西,
// 定義兩份就會有兩個版本(專案鐵則 9)。
type Update struct {
	// Tournament 是賽事 slug,Hub 的分組鍵。
	Tournament string
	Kind       Kind
	// EmittedAt 是伺服器組出這則變化的時刻。
	EmittedAt time.Time

	// Match 在 Kind == KindMatch 時非 nil。
	Match *match.Match
	// Handicaps 在 Kind == KindHandicapLocked 時非 nil。
	Handicaps *handicap.MatchHandicaps
	// Odds 在 Kind == KindOdds 時非 nil。**MyVote 恆為 SideNone** ——
	// 推播是廣播,一則訊息要送給所有訂閱者,不可能因人而異。
	Odds *betting.MatchOdds
	// Phase 在 Kind == KindPhase 時有值。
	Phase tournament.Phase
	// Champion 在 Kind == KindChampion 時非 nil。
	Champion *Champion
}
