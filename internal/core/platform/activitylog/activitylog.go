// Package activitylog 是活動記錄寫入的契約(schemas/10-activity-logs.md、
// schemas/11-messages.md)。實作在 infrastructure/storage/activitylogpg。
//
// 為什麼平台層要有這個介面:stentor 沒有 DB 憑證(專案鐵則 2),Discord 的
// 語音/訊息/reaction 只能經 hestia 的 API 寫入。入口層(transport)不能碰
// pgxpool,所以寫入能力必須以介面注入。
//
// 三個貫穿本套件的設計:
//
//   - **寫入時彙總**(schemas/10):事實(voice_sessions / message_logs /
//     reaction_events)與彙總(activity_daily / message_stats)在**同一個
//     transaction**。原始事件到期刪掉後統計仍然正確,而且任何時點都不會出現
//     「事實寫了但統計沒動」的中間態。
//
//   - **記錄與計分是兩件事**(schemas/02):頻道沒開 log_messages、使用者
//     opt_out_logging,都只影響「內容要不要落地」,**不影響計數與 XP**。
//     否則退出記錄就等於退出被統計,而那會讓「退出」變成逃避的手段。
//
//   - **冪等靠天然唯一鍵**,不另設冪等鍵表(專案第 9 條:一個概念一個權威)。
//     Discord 會重送事件,重送的結果是 Deduplicated=true 的**成功**,不是錯誤。
package activitylog

import (
	"context"
	"errors"
	"time"
)

// ProviderDiscord 是目前唯一的活動來源 provider(identities.provider /
// community_spaces.provider 的值域)。Twitch/YouTube 之後照樣走這套介面。
const ProviderDiscord = "discord"

// 訊息舊版本的 kind(message_revisions.kind)。
const (
	RevisionEdited  = "edited"
	RevisionDeleted = "deleted"
)

// 錯誤語意。入口層據此決定回應的 connect code(transport/errmap.go)。
var (
	// ErrActorNotLinked 是「X-Acting-User 指的 Discord 使用者還沒綁定平台帳號」。
	//
	// 這是一個**刻意可辨識**的錯誤:這裡絕不自動建帳號。建帳號的權威在 OAuth
	// 登入流程,偷偷在這裡建會產出沒有 public_id 規則、沒有 privacy 設定列的
	// 殘缺使用者,而且從此沒有人知道它是怎麼來的。呼叫端收到這個錯誤的正確
	// 反應是引導使用者去登入綁定,不是重試。
	ErrActorNotLinked = errors.New("這個 Discord 使用者尚未綁定平台帳號")

	// ErrSpaceNotRegistered 是「guild 沒有註冊成 community_space」。
	// 同樣不自動建:空間屬於哪個 community 是人的決定,不是事件流的副作用。
	ErrSpaceNotRegistered = errors.New("guild 尚未註冊為 community_space")

	// ErrInvalidRequest 是參數本身不合法(缺 message_id、時間為零值…)。
	ErrInvalidRequest = errors.New("活動記錄參數不合法")
)

// ActorResolver 把 provider 的外部使用者 id 解成內部 user_id。
//
// 放在 core 而不是 transport 的理由:入口層的服務認證攔截器要用它
// (X-Acting-User),而實作在 storage —— storage 不能 import transport,
// 型別放 core 兩邊才接得起來(同 readmodel 的處理)。
//
// 實作契約:查不到回 ErrActorNotLinked,**不得建立任何使用者**。
type ActorResolver interface {
	ResolveActor(ctx context.Context, provider, providerUserID string) (userID int64, err error)
}

// VoiceSessionParams 是一段語音在席。
type VoiceSessionParams struct {
	UserID    int64
	GuildID   string
	ChannelID string
	// JoinedAt 是冪等鍵的一部分,重送必須是同一個值。
	JoinedAt        time.Time
	LeftAt          *time.Time
	DurationSeconds *int32
	SelfMuted       *bool
	SelfDeafened    *bool
	// PeerCountAvg 是期間平均在席人數。schemas/10 明列的唯一非金額浮點欄位;
	// 它只用於「要不要發 XP」的門檻判斷,不參與任何金額計算。
	PeerCountAvg float32
}

// VoiceSessionResult 是語音記錄的結果。
type VoiceSessionResult struct {
	SpaceID             int64
	Deduplicated        bool
	Closed              bool
	VoiceSecondsCounted int32
	XPAwarded           int64
}

// MessageParams 是一則訊息。作者是 UserID(來自 X-Acting-User),
// 批次裡的每一則都是同一位作者。
type MessageParams struct {
	ClientRef     string
	ChannelID     string
	ChannelKind   string
	ThreadID      *string
	MessageID     string
	Content       string
	ContentLength int32 // 0 = 由實作從 Content 算(字元數)
	HasAttachment bool
	ReplyTo       *string
	CreatedAt     time.Time
}

// MessagesParams 是一次批次寫入。
type MessagesParams struct {
	UserID   int64
	GuildID  string
	Messages []MessageParams
}

// MessageResult 是單筆訊息的結果。Err != nil 表示這一筆失敗,
// **其餘筆數不受影響**(每筆各自一個 transaction)。
type MessageResult struct {
	ClientRef     string
	MessageID     string
	Deduplicated  bool
	ContentStored bool
	XPAwarded     int64
	Err           error
}

// MessagesResult 是批次結果。SpaceID 供入口層寫進 event_logs.space_id。
type MessagesResult struct {
	SpaceID int64
	Results []MessageResult
}

// RevisionParams 是一次編輯/刪除前的舊版本。
type RevisionParams struct {
	UserID    int64
	GuildID   string
	ChannelID string
	MessageID string
	// PreviousContent 是**舊**內容;白名單或 opt-out 擋下時不落地。
	PreviousContent string
	Kind            string // RevisionEdited / RevisionDeleted
	CapturedAt      time.Time
}

// RevisionResult 是舊版本記錄的結果。
type RevisionResult struct {
	SpaceID       int64
	Deduplicated  bool
	ExcerptStored bool
}

// ReactionParams 是一次按/取消表情。
type ReactionParams struct {
	UserID    int64
	GuildID   string
	MessageID string
	Emoji     string
	// Removed=false 是按下,true 是取消。
	Removed    bool
	OccurredAt time.Time
	// MessageAuthorID 是被按訊息作者的 provider 外部 id。
	// 只有在 hestia 還沒見過那則訊息時才需要(message_stats.user_id NOT NULL)。
	MessageAuthorID string
}

// ReactionResult 是 reaction 記錄的結果。
type ReactionResult struct {
	SpaceID      int64
	Deduplicated bool
	StatsUpdated bool
}

// PresenceParams 是一段線上狀態區間。
//
// **不可作為任何發放的依據**(schemas/10):需特權 intent、Bot 重啟漏資料、
// 隱身看不到,而且掛機就能刷。這個型別存在只是為了「Discord 線上時長」的顯示。
type PresenceParams struct {
	UserID    int64
	GuildID   string
	Status    string // online / idle / dnd / offline
	StartedAt time.Time
	EndedAt   *time.Time
}

// PresenceResult 是 presence 記錄的結果。
type PresenceResult struct {
	SpaceID              int64
	Deduplicated         bool
	Closed               bool
	OnlineSecondsCounted int32
}

// Service 是活動記錄的寫入介面。
//
// 每個方法的一次寫入 = 一個 transaction(RecordMessages 是每筆一個)。
// 事實與彙總同 tx;XP 入帳是**例外**,理由見實作註解。
type Service interface {
	// RecordVoiceSession 記語音在席並累加 activity_daily.voice_seconds。
	// 達門檻(economy_configs.voice_xp_min_peers)且頻道允許時發 discord_voice XP。
	RecordVoiceSession(ctx context.Context, p VoiceSessionParams) (*VoiceSessionResult, error)
	// RecordMessages 批次記訊息。回傳的切片與輸入等長、順序一致。
	// 只有「整批共通的前提」(guild 未註冊、參數全空)才回 error;
	// 單筆問題放在 MessageResult.Err。
	RecordMessages(ctx context.Context, p MessagesParams) (*MessagesResult, error)
	// RecordRevision 記編輯/刪除前的舊版本。
	RecordRevision(ctx context.Context, p RevisionParams) (*RevisionResult, error)
	// RecordReaction 記按/取消表情並更新 activity_daily.reactions 與
	// message_stats.reaction_count。
	RecordReaction(ctx context.Context, p ReactionParams) (*ReactionResult, error)
	// RecordPresence 記線上狀態區間。**不發任何 XP 或點數**。
	RecordPresence(ctx context.Context, p PresenceParams) (*PresenceResult, error)
}
