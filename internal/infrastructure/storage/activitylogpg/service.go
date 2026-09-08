// Package activitylogpg 是 activitylog.Service 與 activitylog.ActorResolver 的
// Postgres 實作(schemas/10-activity-logs.md、schemas/11-messages.md)。
//
// 五個 RPC 的共同骨架都一樣:
//
//	驗參數 → 開 tx → 解析 guild → 冪等判斷 → 寫事實 → 更新彙總 → commit
//	→(僅語音/訊息)發 XP → 回填 activity_daily.xp_gained
//
// 兩個必須先講清楚的取捨:
//
//   - **冪等**:三張事實表(voice_sessions / presence_spans / reaction_events)
//     的天然鍵**已經有 UNIQUE 索引**(migration 00024,`*_natural_uq`),
//     那就是這個不變量的**權威**。它保護所有路徑 —— backfill 腳本、別的服務、
//     人工修資料都繞不過去,而應用層的防線只保護「走這份程式碼的路徑」。
//
//     那為什麼還留著 `pg_advisory_xact_lock(天然鍵的 hash)`?**它解的不是同一個
//     問題,所以不是第二個權威。** UNIQUE 保證「不會有兩列」;advisory lock 保證
//     「同鍵的併發拿到的是好答案」。少了它,兩個同鍵請求會雙雙看到 ErrNoRows、
//     雙雙 INSERT,其中一個吃到 23505 —— 那會變成 Internal 錯誤,而正確的回應
//     是 `deduplicated=true` 加上既有那列的資料。換句話說:
//
//     UNIQUE          =  正確性的權威(唯一)
//     advisory lock   =  併發下的回應品質(縱深防禦 / 使用者體驗)
//
//     要收斂掉 lock 的話,唯一的作法是把 23505 當成「重送」處理(撞鍵後
//     rollback → 重讀既有列 → 回 deduplicated),與 ledgerpg 撞冪等鍵後的
//     做法同一個範式。那是可以做的簡化,但不是 bug 修正 —— 現況沒有兩個
//     互相矛盾的權威,只有一個權威加一層讓錯誤路徑變好看的鎖。
//
//     訊息不受此限:它用 message_stats 的 PK(見 ClaimMessage 的註解)。
//
//   - **XP 與事實同一個 tx**:走 xppg.Service.AwardInTx(2026-09-08 補上的變體,
//     同 ledgerpg.ApplyInTx 的範式),事實 + 彙總 + XP 三者同生共死。先前的
//     「事實 commit 後另開 tx 發 XP」留有「事實在、XP 沒發」的窗口,雖然方向安全
//     (XP 是投影可重算),但能關就關。代價是 XP 入帳失敗會讓整筆事實 rollback,
//     這是刻意的:呼叫端重送時冪等鍵沒被佔走,重試會真的重試(先前的設計下
//     重送只會撞到冪等,使用者永遠拿不到那筆 XP)。
package activitylogpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/core/platform/activitylog"
	"github.com/danicotech/hestia/internal/core/platform/xp"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 設定鍵(economy_configs)。權威在 DB,程式只認 key 名。
const (
	cfgExcerptMaxChars = "message_excerpt_max_chars"
	cfgVoiceMinPeers   = "voice_xp_min_peers"
)

// defaultExcerptMaxChars 是讀不到設定時的保底值(與 seed 相同)。
// 保底值刻意偏保守:設定壞掉時寧可少存內容,不要多存。
const defaultExcerptMaxChars = 200

// presence 的合法狀態(schemas/10)。
var presenceStatuses = map[string]bool{
	"online": true, "idle": true, "dnd": true, "offline": true,
}

// XPAwarder 是活動記錄對 XP 的最小依賴:**在呼叫端的 tx 內**入帳。
//
// 刻意不是 xp.Service:這裡需要的是同 tx 綁定的變體(xppg.Service.AwardInTx),
// 而它刻意不在 xp.Service interface 上(interface 要保持可攜,未來的 HTTP 版
// 沒有 tx 可傳)。宣告最小介面是既有範式 —— dailypg / admineconpg 對
// ledger.ApplyInTx 也是這樣做的。
type XPAwarder interface {
	AwardInTx(ctx context.Context, tx pgx.Tx, p xp.AwardParams) (*xp.AwardResult, error)
}

// Service 是活動記錄的 Postgres 實作。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// xp 可為 nil:XP 服務還沒接上時只記錄不發放,不讓整條寫入路徑掛掉。
	xp  XPAwarder
	log *slog.Logger
}

var (
	_ activitylog.Service       = (*Service)(nil)
	_ activitylog.ActorResolver = (*Service)(nil)
)

// New 建立活動記錄服務。xpSvc 可為 nil(不發 XP);log 為 nil 時用 slog.Default()。
func New(pool *pgxpool.Pool, xpSvc XPAwarder, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, q: db.New(pool), xp: xpSvc, log: log}
}

// ── 身分解析 ──────────────────────────────────────────────────────

// ResolveActor 把 provider 的外部使用者 id 解成內部 user_id。
//
// **這裡永遠不建帳號。** 查不到就回 ErrActorNotLinked,讓呼叫端去引導綁定。
// 偷偷建的話會產生一個沒有 public_id 規則、沒有 privacy 設定列、沒有人知道
// 來歷的使用者,而且從此以後「使用者是怎麼來的」這個問題再也答不出來。
func (s *Service) ResolveActor(ctx context.Context, provider, providerUserID string) (int64, error) {
	if provider == "" || providerUserID == "" {
		return 0, fmt.Errorf("provider 與 provider_user_id 不可為空: %w", activitylog.ErrInvalidRequest)
	}
	id, err := s.q.ResolveIdentityUser(ctx, db.ResolveIdentityUserParams{
		Provider: provider, ProviderUserID: providerUserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%s:%s: %w", provider, providerUserID, activitylog.ErrActorNotLinked)
	}
	if err != nil {
		return 0, fmt.Errorf("查 identities: %w", err)
	}
	return id, nil
}

// spaceRef 是一次寫入所屬的空間與社群。
type spaceRef struct {
	spaceID     int64
	communityID int64
}

// resolveSpace 把 guild snowflake 解成 (space_id, community_id)。
// 同樣不自動建:空間屬於哪個 community 是人的決定,不是事件流的副作用。
func resolveSpace(ctx context.Context, q *db.Queries, guildID string) (spaceRef, error) {
	if guildID == "" {
		return spaceRef{}, fmt.Errorf("guild_id 不可為空: %w", activitylog.ErrInvalidRequest)
	}
	row, err := q.GetSpaceByExternalID(ctx, db.GetSpaceByExternalIDParams{
		Provider: activitylog.ProviderDiscord, ExternalID: guildID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return spaceRef{}, fmt.Errorf("guild %s: %w", guildID, activitylog.ErrSpaceNotRegistered)
	}
	if err != nil {
		return spaceRef{}, fmt.Errorf("查 community_spaces: %w", err)
	}
	return spaceRef{spaceID: row.ID, communityID: row.CommunityID}, nil
}

// channelPolicy 是頻道層的兩個開關(schemas/01 增補 C)。
type channelPolicy struct {
	logMessages bool
	grantXP     bool
}

// channelPolicyOf 讀頻道註冊。**未註冊的頻道不是錯誤**:
// log_messages 取白名單制的預設(false,不記內容),
// grant_xp 取欄位預設(true)—— 白名單管的是「內容要不要落地」,
// 不是「這個人算不算有參與」(schemas/02:退出記錄不等於退出計分)。
func channelPolicyOf(ctx context.Context, q *db.Queries, spaceID int64, channelID string) (channelPolicy, error) {
	if channelID == "" {
		return channelPolicy{}, fmt.Errorf("channel_id 不可為空: %w", activitylog.ErrInvalidRequest)
	}
	row, err := q.GetSpaceChannel(ctx, db.GetSpaceChannelParams{
		SpaceID: spaceID, ExternalID: channelID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return channelPolicy{logMessages: false, grantXP: true}, nil
	}
	if err != nil {
		return channelPolicy{}, fmt.Errorf("查 space_channels: %w", err)
	}
	return channelPolicy{logMessages: row.LogMessages, grantXP: row.GrantXp}, nil
}

// optOutLogging 讀使用者的記錄退出旗標。沒有設定列 = 沒退出。
func optOutLogging(ctx context.Context, q *db.Queries, userID int64) (bool, error) {
	out, err := q.GetOptOutLogging(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查 user_privacy_settings: %w", err)
	}
	return out, nil
}

// ── 語音 ──────────────────────────────────────────────────────────

// RecordVoiceSession 記一段語音在席。
//
// 語音是可靠訊號(voiceStateUpdate 不需特權 intent),所以它**可以**發 XP。
// 防掛機的門檻是 peer_count_avg ≥ economy_configs.voice_xp_min_peers,
// 外加「自己把耳朵關了就不算參與」(self_deafened)。門檻不通過只影響 XP,
// 事實照記 —— 記錄與計分是兩件事。
func (s *Service) RecordVoiceSession(
	ctx context.Context, p activitylog.VoiceSessionParams,
) (*activitylog.VoiceSessionResult, error) {
	switch {
	case p.UserID <= 0:
		return nil, fmt.Errorf("缺少行為主體: %w", activitylog.ErrInvalidRequest)
	case p.ChannelID == "":
		return nil, fmt.Errorf("channel_id 不可為空: %w", activitylog.ErrInvalidRequest)
	case p.JoinedAt.IsZero():
		return nil, fmt.Errorf("joined_at 不可為空: %w", activitylog.ErrInvalidRequest)
	case p.LeftAt != nil && p.LeftAt.Before(p.JoinedAt):
		return nil, fmt.Errorf("left_at 早於 joined_at: %w", activitylog.ErrInvalidRequest)
	}
	duration := voiceDuration(p)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	sp, err := resolveSpace(ctx, qtx, p.GuildID)
	if err != nil {
		return nil, err
	}
	policy, err := channelPolicyOf(ctx, qtx, sp.spaceID, p.ChannelID)
	if err != nil {
		return nil, err
	}

	// 去重的權威是 voice_sessions_natural_uq(migration 00024);
	// 這把 advisory lock 只是讓同鍵的併發回 deduplicated 而不是撞 23505
	// (分工見套件註解)。
	key := fmt.Sprintf("voice:%d:%d:%s:%d", p.UserID, sp.spaceID, p.ChannelID, p.JoinedAt.UTC().UnixNano())
	if err := qtx.LockActivityKey(ctx, key); err != nil {
		return nil, fmt.Errorf("取 advisory lock: %w", err)
	}

	res := &activitylog.VoiceSessionResult{SpaceID: sp.spaceID}
	existing, err := qtx.GetVoiceSession(ctx, db.GetVoiceSessionParams{
		UserID: p.UserID, SpaceID: sp.spaceID, ChannelID: p.ChannelID, JoinedAt: p.JoinedAt,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := qtx.InsertVoiceSession(ctx, db.InsertVoiceSessionParams{
			UserID: p.UserID, SpaceID: sp.spaceID, ChannelID: p.ChannelID,
			JoinedAt: p.JoinedAt, LeftAt: p.LeftAt, DurationSeconds: duration,
			SelfMuted: p.SelfMuted, SelfDeafened: p.SelfDeafened,
			PeerCountAvg: floatPtr(p.PeerCountAvg),
		}); err != nil {
			return nil, fmt.Errorf("寫 voice_sessions: %w", err)
		}
		res.VoiceSecondsCounted = derefInt32(duration)
	case err != nil:
		return nil, fmt.Errorf("查 voice_sessions: %w", err)
	case existing.LeftAt == nil && p.LeftAt != nil:
		// 先前只記了「進來」,這次補上收尾:更新同一列,秒數在此才計入。
		if err := qtx.CloseVoiceSession(ctx, db.CloseVoiceSessionParams{
			ID: existing.ID, JoinedAt: existing.JoinedAt,
			LeftAt: p.LeftAt, DurationSeconds: duration,
			SelfMuted: p.SelfMuted, SelfDeafened: p.SelfDeafened,
			PeerCountAvg: floatPtr(p.PeerCountAvg),
		}); err != nil {
			return nil, fmt.Errorf("收尾 voice_sessions: %w", err)
		}
		res.Closed = true
		res.VoiceSecondsCounted = derefInt32(duration)
	default:
		// 已經記過(而且沒有新資訊)—— 重送視為已記錄,回成功。
		res.Deduplicated = true
		return res, tx.Commit(ctx)
	}

	// 事實與彙總同 tx(schemas/10「寫入時彙總」)。
	if err := qtx.BumpActivityDaily(ctx, db.BumpActivityDailyParams{
		UserID: p.UserID, CommunityID: sp.communityID, OccurredAt: p.JoinedAt,
		VoiceSeconds: res.VoiceSecondsCounted,
	}); err != nil {
		return nil, fmt.Errorf("更新 activity_daily: %w", err)
	}

	// ── XP 也在同一個 tx 裡(套件註解:事實 + 彙總 + XP 同生共死)──
	//
	// 設定讀不到仍是降級而不是失敗:發放數值的權威在 DB,讀不到就這次不發,
	// 但事實照記 —— 記錄與計分是兩件事(這一條在改成同 tx 之後不變)。
	minPeers, cfgErr := s.intConfig(ctx, qtx, cfgVoiceMinPeers)
	rule, ruleErr := s.sourceRule(ctx, qtx, sp.communityID, xpSourceVoice)
	switch {
	case cfgErr != nil || ruleErr != nil:
		s.log.WarnContext(ctx, "語音 XP 設定讀取失敗,本次不發 XP",
			"config_err", cfgErr, "rule_err", ruleErr)
	case voiceQualifiesForXP(p, policy, res.VoiceSecondsCounted, minPeers):
		amount := rule.XPPerMinute * int64(res.VoiceSecondsCounted/60)
		awarded, err := s.award(ctx, tx, qtx, awardRequest{
			userID: p.UserID, communityID: sp.communityID, spaceID: sp.spaceID,
			source: xpSourceVoice, amount: amount,
			refID:      fmt.Sprintf("voice:%s:%d", p.ChannelID, p.JoinedAt.UTC().Unix()),
			occurredAt: p.JoinedAt,
		})
		if err != nil {
			return nil, err
		}
		res.XPAwarded = awarded
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// voiceQualifiesForXP 判斷這段語音該不該發 XP。
// 三個條件缺一不可:頻道沒被關掉發放、真的有在席秒數、
// 平均在席人數達門檻且沒有把自己拒聽(一個人對著空頻道掛機不算參與)。
func voiceQualifiesForXP(
	p activitylog.VoiceSessionParams, policy channelPolicy, counted int32, minPeers int64,
) bool {
	if !policy.grantXP || counted <= 0 {
		return false
	}
	if p.SelfDeafened != nil && *p.SelfDeafened {
		return false
	}
	return float64(p.PeerCountAvg) >= float64(minPeers)
}

// voiceDuration 取在席秒數:呼叫端給了就用,沒給但有 left_at 就自己算。
func voiceDuration(p activitylog.VoiceSessionParams) *int32 {
	if p.DurationSeconds != nil {
		return p.DurationSeconds
	}
	if p.LeftAt == nil {
		return nil
	}
	secs := int64(p.LeftAt.Sub(p.JoinedAt) / time.Second)
	return clampInt32(secs)
}

// ── 訊息 ──────────────────────────────────────────────────────────

// RecordMessages 批次記訊息。**每一筆各自一個 transaction** ——
// 這正是「單筆失敗不得讓整批失敗」的實作:一筆的 rollback 影響不到別筆。
//
// 只有整批共通的前提(guild 沒註冊、批次是空的、作者的隱私設定讀不到)
// 才回 error;單筆的問題放在各自的 MessageResult.Err。
func (s *Service) RecordMessages(
	ctx context.Context, p activitylog.MessagesParams,
) (*activitylog.MessagesResult, error) {
	if p.UserID <= 0 {
		return nil, fmt.Errorf("缺少行為主體: %w", activitylog.ErrInvalidRequest)
	}
	if len(p.Messages) == 0 {
		return nil, fmt.Errorf("messages 不可為空: %w", activitylog.ErrInvalidRequest)
	}
	sp, err := resolveSpace(ctx, s.q, p.GuildID)
	if err != nil {
		return nil, err
	}
	// 每批讀一次就好:同一批是同一位作者、同一組設定。
	optOut, err := optOutLogging(ctx, s.q, p.UserID)
	if err != nil {
		return nil, err
	}
	maxChars, err := s.intConfig(ctx, s.q, cfgExcerptMaxChars)
	if err != nil || maxChars <= 0 {
		s.log.WarnContext(ctx, "讀不到 message_excerpt_max_chars,用保底值",
			"err", err, "fallback", defaultExcerptMaxChars)
		maxChars = defaultExcerptMaxChars
	}
	// 每則訊息發多少 XP 是整批共通的(同社群同 source),讀一次就好。
	// 讀不到 ruleset = 這批不發 XP:發放數值的權威在 DB,這裡不內建預設值。
	rule, ruleErr := s.sourceRule(ctx, s.q, sp.communityID, xpSourceMessage)
	xpAmount := int64(0)
	if ruleErr == nil {
		xpAmount = rule.XP
	}

	out := &activitylog.MessagesResult{SpaceID: sp.spaceID, Results: make([]activitylog.MessageResult, len(p.Messages))}
	for i, m := range p.Messages {
		out.Results[i] = s.recordOneMessage(ctx, sp, p.UserID, optOut, int(maxChars), xpAmount, m)
	}
	return out, nil
}

// recordOneMessage 在單一 transaction 內處理一則訊息。
//
// 順序是刻意的:先 ClaimMessage(冪等的權威),再更新彙總,寫內容,最後發 XP。
// 任何一步失敗整筆 rollback —— 事實、彙總與 XP 不會出現只留一半的狀態。
//
// xpAmount 是這則訊息該發的 XP(0 = 不發:沒有 ruleset、或讀不到規則)。
// 頻道的 grant_xp 另外擋一層 —— 指令區/刷頻區只記錄不計分。
func (s *Service) recordOneMessage(
	ctx context.Context, sp spaceRef, userID int64, optOut bool, maxChars int,
	xpAmount int64, m activitylog.MessageParams,
) activitylog.MessageResult {
	out := activitylog.MessageResult{ClientRef: m.ClientRef, MessageID: m.MessageID}
	switch {
	case m.MessageID == "":
		out.Err = fmt.Errorf("message_id 不可為空: %w", activitylog.ErrInvalidRequest)
		return out
	case m.ChannelID == "":
		out.Err = fmt.Errorf("channel_id 不可為空: %w", activitylog.ErrInvalidRequest)
		return out
	case m.ChannelKind == "":
		out.Err = fmt.Errorf("channel_kind 不可為空: %w", activitylog.ErrInvalidRequest)
		return out
	case m.CreatedAt.IsZero():
		out.Err = fmt.Errorf("created_at 不可為空: %w", activitylog.ErrInvalidRequest)
		return out
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		out.Err = fmt.Errorf("開 transaction: %w", err)
		return out
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	policy, err := channelPolicyOf(ctx, qtx, sp.spaceID, m.ChannelID)
	if err != nil {
		out.Err = err
		return out
	}
	// 冪等:message_stats 的 PK 是唯一「每則訊息必定寫一次」的鍵
	// (白名單關著、使用者 opt-out 時都還是會寫這一列)。
	if _, err := qtx.ClaimMessage(ctx, db.ClaimMessageParams{
		MessageID: m.MessageID, UserID: userID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			out.Deduplicated = true
			out.Err = commitOrErr(ctx, tx)
			return out
		}
		out.Err = fmt.Errorf("認領 message_stats: %w", err)
		return out
	}

	// 計數不受白名單與 opt-out 影響(schemas/02:退出記錄不等於退出計分)。
	if err := qtx.BumpActivityDaily(ctx, db.BumpActivityDailyParams{
		UserID: userID, CommunityID: sp.communityID, OccurredAt: m.CreatedAt, Messages: 1,
	}); err != nil {
		out.Err = fmt.Errorf("更新 activity_daily: %w", err)
		return out
	}

	if policy.logMessages {
		full := m.ContentLength
		if full <= 0 {
			full = clampRuneCount(m.Content)
		}
		var excerpt *string
		if !optOut && m.Content != "" {
			excerpt = strPtr(truncateRunes(m.Content, maxChars))
		}
		if err := qtx.InsertMessageLog(ctx, db.InsertMessageLogParams{
			UserID: userID, SpaceID: sp.spaceID, ChannelID: m.ChannelID,
			ChannelKind: m.ChannelKind, ThreadID: m.ThreadID, MessageID: m.MessageID,
			Excerpt: excerpt, FullLength: &full, HasAttachment: m.HasAttachment,
			ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt,
		}); err != nil {
			out.Err = fmt.Errorf("寫 message_logs: %w", err)
			return out
		}
		out.ContentStored = excerpt != nil
	}

	// 被回覆的那則訊息:只有 hestia 見過的才累加(沒見過就沒有作者可填)。
	if m.ReplyTo != nil && *m.ReplyTo != "" {
		if err := qtx.IncrementReplyCount(ctx, *m.ReplyTo); err != nil {
			out.Err = fmt.Errorf("更新 reply_count: %w", err)
			return out
		}
	}

	// XP 在同一個 tx 裡:發不出來就整筆不留(套件註解的取捨)。
	if policy.grantXP && xpAmount > 0 {
		awarded, err := s.award(ctx, tx, qtx, awardRequest{
			userID: userID, communityID: sp.communityID, spaceID: sp.spaceID,
			source: xpSourceMessage, amount: xpAmount,
			refID: "message:" + m.MessageID, occurredAt: m.CreatedAt,
		})
		if err != nil {
			out.Err = err
			return out
		}
		out.XPAwarded = awarded
	}

	out.Err = commitOrErr(ctx, tx)
	return out
}

// ── 訊息舊版本 ────────────────────────────────────────────────────

// RecordRevision 記編輯/刪除前的舊版本。
//
// 這是全系統敏感度最高的資料,所以內容的兩道閘門都要過:頻道要在白名單內,
// 而且作者沒有 opt_out_logging。任一不成立就只留「發生過一次編輯/刪除」的事件,
// excerpt 為 NULL。
func (s *Service) RecordRevision(
	ctx context.Context, p activitylog.RevisionParams,
) (*activitylog.RevisionResult, error) {
	switch {
	case p.UserID <= 0:
		return nil, fmt.Errorf("缺少行為主體: %w", activitylog.ErrInvalidRequest)
	case p.MessageID == "":
		return nil, fmt.Errorf("message_id 不可為空: %w", activitylog.ErrInvalidRequest)
	case p.Kind != activitylog.RevisionEdited && p.Kind != activitylog.RevisionDeleted:
		return nil, fmt.Errorf("kind 必須是 edited 或 deleted: %w", activitylog.ErrInvalidRequest)
	case p.CapturedAt.IsZero():
		return nil, fmt.Errorf("captured_at 不可為空: %w", activitylog.ErrInvalidRequest)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	sp, err := resolveSpace(ctx, qtx, p.GuildID)
	if err != nil {
		return nil, err
	}
	policy, err := channelPolicyOf(ctx, qtx, sp.spaceID, p.ChannelID)
	if err != nil {
		return nil, err
	}

	res := &activitylog.RevisionResult{SpaceID: sp.spaceID}
	key := fmt.Sprintf("revision:%s:%s:%d", p.MessageID, p.Kind, p.CapturedAt.UTC().UnixNano())
	if err := qtx.LockActivityKey(ctx, key); err != nil {
		return nil, fmt.Errorf("取 advisory lock: %w", err)
	}
	dup, err := s.revisionExists(ctx, qtx, p)
	if err != nil {
		return nil, err
	}
	if dup {
		res.Deduplicated = true
		return res, tx.Commit(ctx)
	}

	optOut, err := optOutLogging(ctx, qtx, p.UserID)
	if err != nil {
		return nil, err
	}
	var excerpt *string
	if policy.logMessages && !optOut && p.PreviousContent != "" {
		maxChars, cfgErr := s.intConfig(ctx, qtx, cfgExcerptMaxChars)
		if cfgErr != nil || maxChars <= 0 {
			maxChars = defaultExcerptMaxChars
		}
		excerpt = strPtr(truncateRunes(p.PreviousContent, int(maxChars)))
	}
	if err := qtx.InsertMessageRevision(ctx, db.InsertMessageRevisionParams{
		MessageID: p.MessageID, UserID: p.UserID, Excerpt: excerpt,
		Kind: p.Kind, CapturedAt: p.CapturedAt,
	}); err != nil {
		return nil, fmt.Errorf("寫 message_revisions: %w", err)
	}
	res.ExcerptStored = excerpt != nil

	// 刪除同時在 message_logs 打上 deleted_at:那個欄位本來就是為此存在,
	// 而且「查已刪訊息」是 schemas/11 唯一需要原文的需求。
	if p.Kind == activitylog.RevisionDeleted {
		at := p.CapturedAt
		if err := qtx.MarkMessageDeleted(ctx, db.MarkMessageDeletedParams{
			MessageID: p.MessageID, DeletedAt: &at,
		}); err != nil {
			return nil, fmt.Errorf("標記 message_logs.deleted_at: %w", err)
		}
	}
	return res, tx.Commit(ctx)
}

// revisionExists 依 kind 用不同的天然鍵判重:
// 刪除只會發生一次(Discord 不給刪除時間,拿收訊時間當鍵反而擋不住重送);
// 編輯可以發生很多次,用 Discord 的 edited_timestamp 區分。
func (s *Service) revisionExists(ctx context.Context, q *db.Queries, p activitylog.RevisionParams) (bool, error) {
	if p.Kind == activitylog.RevisionDeleted {
		found, err := q.DeletionRevisionExists(ctx, p.MessageID)
		if err != nil {
			return false, fmt.Errorf("查 message_revisions: %w", err)
		}
		return found, nil
	}
	found, err := q.EditRevisionExists(ctx, db.EditRevisionExistsParams{
		MessageID: p.MessageID, CapturedAt: p.CapturedAt,
	})
	if err != nil {
		return false, fmt.Errorf("查 message_revisions: %w", err)
	}
	return found, nil
}

// ── Reaction ──────────────────────────────────────────────────────

// RecordReaction 記一次按/取消表情。
//
// 天然鍵是 (message_id, user_id, emoji):一個人對同一則訊息的同一個表情只有
// 一列,按與取消是那一列的 added_at / removed_at。取消後再按回來會復用同一列,
// 而且 **activity_daily.reactions 不會再加一次** —— 否則「按了取消再按」
// 就是一個無限刷參與度的迴圈,而 reaction 正是抽獎資格的首選訊號。
func (s *Service) RecordReaction(
	ctx context.Context, p activitylog.ReactionParams,
) (*activitylog.ReactionResult, error) {
	switch {
	case p.UserID <= 0:
		return nil, fmt.Errorf("缺少行為主體: %w", activitylog.ErrInvalidRequest)
	case p.MessageID == "":
		return nil, fmt.Errorf("message_id 不可為空: %w", activitylog.ErrInvalidRequest)
	case p.Emoji == "":
		return nil, fmt.Errorf("emoji 不可為空: %w", activitylog.ErrInvalidRequest)
	case p.OccurredAt.IsZero():
		return nil, fmt.Errorf("occurred_at 不可為空: %w", activitylog.ErrInvalidRequest)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	sp, err := resolveSpace(ctx, qtx, p.GuildID)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("reaction:%s:%d:%s", p.MessageID, p.UserID, p.Emoji)
	if err := qtx.LockActivityKey(ctx, key); err != nil {
		return nil, fmt.Errorf("取 advisory lock: %w", err)
	}

	res := &activitylog.ReactionResult{SpaceID: sp.spaceID}
	delta, countDaily, err := s.applyReaction(ctx, qtx, sp, p, res)
	if err != nil {
		return nil, err
	}
	if res.Deduplicated {
		return res, tx.Commit(ctx)
	}

	if countDaily {
		if err := qtx.BumpActivityDaily(ctx, db.BumpActivityDailyParams{
			UserID: p.UserID, CommunityID: sp.communityID,
			OccurredAt: p.OccurredAt, Reactions: 1,
		}); err != nil {
			return nil, fmt.Errorf("更新 activity_daily: %w", err)
		}
	}
	if delta != 0 {
		updated, err := s.bumpReactionCount(ctx, qtx, p, delta)
		if err != nil {
			return nil, err
		}
		res.StatsUpdated = updated
	}
	return res, tx.Commit(ctx)
}

// applyReaction 寫 reaction_events 那一側,回傳 message_stats 的增減量與
// 「這次算不算一次新的參與」。
func (s *Service) applyReaction(
	ctx context.Context, q *db.Queries, sp spaceRef,
	p activitylog.ReactionParams, res *activitylog.ReactionResult,
) (delta int32, countDaily bool, err error) {
	existing, err := q.GetReaction(ctx, db.GetReactionParams{
		MessageID: p.MessageID, UserID: p.UserID, Emoji: p.Emoji,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if p.Removed {
			// 沒按過卻收到取消:重送或亂序,當作已處理。
			res.Deduplicated = true
			return 0, false, nil
		}
		if err := q.InsertReaction(ctx, db.InsertReactionParams{
			MessageID: p.MessageID, UserID: p.UserID, SpaceID: sp.spaceID,
			Emoji: p.Emoji, AddedAt: p.OccurredAt,
		}); err != nil {
			return 0, false, fmt.Errorf("寫 reaction_events: %w", err)
		}
		return 1, true, nil
	case err != nil:
		return 0, false, fmt.Errorf("查 reaction_events: %w", err)
	case p.Removed:
		if existing.RemovedAt != nil {
			res.Deduplicated = true
			return 0, false, nil
		}
		at := p.OccurredAt
		if err := q.CloseReaction(ctx, db.CloseReactionParams{ID: existing.ID, RemovedAt: &at}); err != nil {
			return 0, false, fmt.Errorf("取消 reaction: %w", err)
		}
		return -1, false, nil
	case existing.RemovedAt == nil:
		res.Deduplicated = true
		return 0, false, nil
	default:
		// 取消後又按回來:復用同一列,但不再算一次當日參與(防刷)。
		if err := q.ReopenReaction(ctx, db.ReopenReactionParams{
			ID: existing.ID, AddedAt: p.OccurredAt,
		}); err != nil {
			return 0, false, fmt.Errorf("重開 reaction: %w", err)
		}
		return 1, false, nil
	}
}

// bumpReactionCount 更新被按訊息的 message_stats.reaction_count。
//
// message_stats.user_id 是 NOT NULL,所以「hestia 沒見過這則訊息」時沒有作者
// 可填。呼叫端可以帶 MessageAuthorID 補上;帶了但那個人還沒綁定平台帳號時
// **略過統計而不是失敗** —— reaction 的事實已經記下來了,統計之後可以重建。
func (s *Service) bumpReactionCount(
	ctx context.Context, q *db.Queries, p activitylog.ReactionParams, delta int32,
) (bool, error) {
	exists, err := q.MessageStatsExists(ctx, p.MessageID)
	if err != nil {
		return false, fmt.Errorf("查 message_stats: %w", err)
	}
	if !exists {
		if p.MessageAuthorID == "" {
			return false, nil
		}
		authorID, err := q.ResolveIdentityUser(ctx, db.ResolveIdentityUserParams{
			Provider: activitylog.ProviderDiscord, ProviderUserID: p.MessageAuthorID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("查訊息作者 identities: %w", err)
		}
		if err := q.EnsureMessageStats(ctx, db.EnsureMessageStatsParams{
			MessageID: p.MessageID, UserID: authorID,
		}); err != nil {
			return false, fmt.Errorf("建 message_stats: %w", err)
		}
	}
	if err := q.BumpMessageReactionCount(ctx, db.BumpMessageReactionCountParams{
		MessageID: p.MessageID, ReactionCount: delta,
	}); err != nil {
		return false, fmt.Errorf("更新 reaction_count: %w", err)
	}
	return true, nil
}

// ── Presence ──────────────────────────────────────────────────────

// RecordPresence 記一段線上狀態區間。
//
// **這條路徑永遠不發 XP、不發點數,也不該有人加上去。** 理由(schemas/10):
// presence 需要 GUILD_PRESENCES 特權 intent、Bot 重啟期間的變化整段漏掉、
// 隱身時看不到,而且掛機就能刷。它只能拿來顯示「Discord 線上時長」,
// 不能拿來當「參與時長」。要發放請用語音(voice_sessions)。
func (s *Service) RecordPresence(
	ctx context.Context, p activitylog.PresenceParams,
) (*activitylog.PresenceResult, error) {
	switch {
	case p.UserID <= 0:
		return nil, fmt.Errorf("缺少行為主體: %w", activitylog.ErrInvalidRequest)
	case !presenceStatuses[p.Status]:
		return nil, fmt.Errorf("status 必須是 online/idle/dnd/offline: %w", activitylog.ErrInvalidRequest)
	case p.StartedAt.IsZero():
		return nil, fmt.Errorf("started_at 不可為空: %w", activitylog.ErrInvalidRequest)
	case p.EndedAt != nil && p.EndedAt.Before(p.StartedAt):
		return nil, fmt.Errorf("ended_at 早於 started_at: %w", activitylog.ErrInvalidRequest)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	sp, err := resolveSpace(ctx, qtx, p.GuildID)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("presence:%d:%d:%d", p.UserID, sp.spaceID, p.StartedAt.UTC().UnixNano())
	if err := qtx.LockActivityKey(ctx, key); err != nil {
		return nil, fmt.Errorf("取 advisory lock: %w", err)
	}

	res := &activitylog.PresenceResult{SpaceID: sp.spaceID}
	existing, err := qtx.GetPresenceSpan(ctx, db.GetPresenceSpanParams{
		UserID: p.UserID, SpaceID: sp.spaceID, StartedAt: p.StartedAt,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := qtx.InsertPresenceSpan(ctx, db.InsertPresenceSpanParams{
			UserID: p.UserID, SpaceID: sp.spaceID, Status: p.Status,
			StartedAt: p.StartedAt, EndedAt: p.EndedAt,
		}); err != nil {
			return nil, fmt.Errorf("寫 presence_spans: %w", err)
		}
		res.OnlineSecondsCounted = presenceSeconds(p)
	case err != nil:
		return nil, fmt.Errorf("查 presence_spans: %w", err)
	case existing.EndedAt == nil && p.EndedAt != nil:
		if err := qtx.ClosePresenceSpan(ctx, db.ClosePresenceSpanParams{
			ID: existing.ID, StartedAt: existing.StartedAt, EndedAt: p.EndedAt,
		}); err != nil {
			return nil, fmt.Errorf("收尾 presence_spans: %w", err)
		}
		res.Closed = true
		res.OnlineSecondsCounted = presenceSeconds(p)
	default:
		res.Deduplicated = true
		return res, tx.Commit(ctx)
	}

	if err := qtx.BumpActivityDaily(ctx, db.BumpActivityDailyParams{
		UserID: p.UserID, CommunityID: sp.communityID,
		OccurredAt: p.StartedAt, OnlineSeconds: res.OnlineSecondsCounted,
	}); err != nil {
		return nil, fmt.Errorf("更新 activity_daily: %w", err)
	}
	return res, tx.Commit(ctx)
}

// presenceSeconds 算計入 online_seconds 的秒數。
// offline 是「不在線」的區間,長度再長也不是線上時長。
func presenceSeconds(p activitylog.PresenceParams) int32 {
	if p.EndedAt == nil || p.Status == "offline" {
		return 0
	}
	return derefInt32(clampInt32(int64(p.EndedAt.Sub(p.StartedAt) / time.Second)))
}

// ── XP ────────────────────────────────────────────────────────────

// xp_event_types 的 key(migration 00015 seed)。發明新的近義詞是錯的。
const (
	xpSourceMessage = "discord_message"
	xpSourceVoice   = "discord_voice"
)

type awardRequest struct {
	userID      int64
	communityID int64
	spaceID     int64
	source      string
	amount      int64
	refID       string
	occurredAt  time.Time
}

// award 在**呼叫端的 tx 內**發 XP 並回填 activity_daily.xp_gained,回傳實際入帳量。
//
// 失敗回 error:事實還沒 commit,所以整筆一起回滾才是誠實的結果 ——
// 呼叫端據此回錯,閘道重送時冪等鍵沒被佔走,重試會真的重試。
// (先前 XP 在事實 commit 之後才發,失敗只能吞掉記 log:那時重送只會撞冪等,
// 使用者永遠拿不到那筆 XP。同 tx 之後這個取捨反過來了。)
//
// 冷卻 / daily_cap 攔下不是失敗:AwardInTx 回 Awarded=0 與旗標,事實照留。
func (s *Service) award(ctx context.Context, tx pgx.Tx, qtx *db.Queries, r awardRequest) (int64, error) {
	if s.xp == nil || r.amount <= 0 {
		return 0, nil
	}
	spaceID := r.spaceID
	res, err := s.xp.AwardInTx(ctx, tx, xp.AwardParams{
		UserID: r.userID, CommunityID: r.communityID, SpaceID: &spaceID,
		Source: r.source, Amount: r.amount, RefID: &r.refID,
	})
	if err != nil {
		return 0, fmt.Errorf("XP 入帳: %w", err)
	}
	if res.Awarded == 0 {
		return 0, nil
	}
	// 同 tx 回填:xp_events 是權威,activity_daily.xp_gained 是投影,
	// 但兩者現在可以一起落地就沒有理由讓它們分家。
	if err := qtx.AddActivityDailyXP(ctx, db.AddActivityDailyXPParams{
		UserID: r.userID, CommunityID: r.communityID,
		OccurredAt: r.occurredAt, XpGained: res.Awarded,
	}); err != nil {
		return 0, fmt.Errorf("回填 activity_daily.xp_gained: %w", err)
	}
	return res.Awarded, nil
}

// sourceRule 讀該社群對某個 XP 來源的數值規則(schemas/01 增補 A)。
// 沒有 ruleset(M1 可能如此)= 零值 = 不發 —— 發放數值的權威在 DB,
// 這裡不內建任何「預設給多少」。
func (s *Service) sourceRule(ctx context.Context, q *db.Queries, communityID int64, source string) (xp.SourceRule, error) {
	raw, err := q.GetCommunityXpConfig(ctx, communityID)
	if err != nil {
		return xp.SourceRule{}, fmt.Errorf("讀 xp_rulesets.config: %w", err)
	}
	cfg, err := xp.ParseConfig(raw)
	if err != nil {
		return xp.SourceRule{}, err
	}
	return cfg.Sources[source], nil
}

// intConfig 讀 economy_configs 的整數值(權威在 DB,程式不寫死數字)。
func (s *Service) intConfig(ctx context.Context, q *db.Queries, key string) (int64, error) {
	raw, err := q.GetCurrentConfig(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("讀設定 %s: %w", key, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("設定 %s 不是整數 (%s): %w", key, raw, err)
	}
	return v, nil
}

// ── 小工具 ────────────────────────────────────────────────────────

func commitOrErr(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// truncateRunes 依**字元數**截斷(schemas/11 的「截斷至 200 字元」)。
// 不用 byte 數:中文一個字 3 bytes,用 byte 截會讓中文摘要只剩三分之一。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// clampRuneCount 回傳字元數,夾在 int32 上限內(full_length 是 INT)。
func clampRuneCount(s string) int32 {
	n := 0
	for range s {
		n++
	}
	return derefInt32(clampInt32(int64(n)))
}

const maxInt32 int64 = 1<<31 - 1

func clampInt32(v int64) *int32 {
	if v > maxInt32 {
		v = maxInt32
	}
	if v < 0 {
		v = 0
	}
	out := int32(v)
	return &out
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

func floatPtr(v float32) *float32 { return &v }

func strPtr(v string) *string { return &v }
