package transport

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	platformv1 "github.com/danicotech/hestia/gen/hestia/platform/v1"
	"github.com/danicotech/hestia/internal/core/platform/activitylog"
)

// activityHandler 是 stentor 寫活動記錄的入口(schemas/10、schemas/11)。
//
// 全部 RPC 只接受服務身分:行為主體來自 X-Acting-User,**不是** request body。
// 一次呼叫只代表一個 Discord 使用者,批次是「同一位作者的多則訊息」——
// 主體只有一個權威位置,body 裡永遠不會出現「這是誰做的」。
type activityHandler struct {
	svc activitylog.Service
}

// RecordVoiceSession 記語音在席。
func (h activityHandler) RecordVoiceSession(
	ctx context.Context, req *connect.Request[platformv1.RecordVoiceSessionRequest],
) (*connect.Response[platformv1.RecordVoiceSessionResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("ActivityService")
	}
	userID, err := requireActingUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	res, err := h.svc.RecordVoiceSession(ctx, activitylog.VoiceSessionParams{
		UserID:          userID,
		GuildID:         m.GetGuildId(),
		ChannelID:       m.GetChannelId(),
		JoinedAt:        timeOf(m.GetJoinedAt()),
		LeftAt:          optTime(m.LeftAt),
		DurationSeconds: m.DurationSeconds,
		SelfMuted:       m.SelfMuted,
		SelfDeafened:    m.SelfDeafened,
		PeerCountAvg:    m.GetPeerCountAvg(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	noteSpace(ctx, res.SpaceID)
	return connect.NewResponse(&platformv1.RecordVoiceSessionResponse{
		Deduplicated:        res.Deduplicated,
		Closed:              res.Closed,
		VoiceSecondsCounted: res.VoiceSecondsCounted,
		XpAwarded:           res.XPAwarded,
	}), nil
}

// RecordMessages 批次記訊息。
//
// **單筆失敗不會讓整批失敗**:核心每筆各自一個 transaction,這裡把逐筆結果
// 原樣攤成 results。整體只有在「批次共通的前提」壞掉時才回 error
// (guild 沒註冊、批次是空的)。
func (h activityHandler) RecordMessages(
	ctx context.Context, req *connect.Request[platformv1.RecordMessagesRequest],
) (*connect.Response[platformv1.RecordMessagesResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("ActivityService")
	}
	userID, err := requireActingUser(ctx)
	if err != nil {
		return nil, err
	}
	msgs := req.Msg.GetMessages()
	params := activitylog.MessagesParams{
		UserID:   userID,
		GuildID:  req.Msg.GetGuildId(),
		Messages: make([]activitylog.MessageParams, 0, len(msgs)),
	}
	for _, m := range msgs {
		params.Messages = append(params.Messages, activitylog.MessageParams{
			ClientRef:     m.GetClientRef(),
			ChannelID:     m.GetChannelId(),
			ChannelKind:   m.GetChannelKind(),
			ThreadID:      m.ThreadId,
			MessageID:     m.GetMessageId(),
			Content:       m.GetContent(),
			ContentLength: m.GetContentLength(),
			HasAttachment: m.GetHasAttachment(),
			ReplyTo:       m.ReplyTo,
			CreatedAt:     timeOf(m.GetCreatedAt()),
		})
	}
	out, err := h.svc.RecordMessages(ctx, params)
	if err != nil {
		return nil, toConnectError(err)
	}
	noteSpace(ctx, out.SpaceID)

	resp := &platformv1.RecordMessagesResponse{
		Results: make([]*platformv1.MessageResult, 0, len(out.Results)),
	}
	for _, r := range out.Results {
		item := &platformv1.MessageResult{
			ClientRef:     r.ClientRef,
			MessageId:     r.MessageID,
			Ok:            r.Err == nil,
			Deduplicated:  r.Deduplicated,
			ContentStored: r.ContentStored,
			XpAwarded:     r.XPAwarded,
		}
		if r.Err != nil {
			// 走同一張錯誤映射表,單筆的 code 與整體失敗時的 code 一致;
			// 訊息只取本套件信任的文案(toConnectError 已經把不可信的換掉)。
			mapped := toConnectError(r.Err)
			item.ErrorCode = connect.CodeOf(mapped).String()
			// reason 也走同一張表(errmap.go 的 errorCodes)。
			// 單筆失敗是 body 裡的一列,沒有 metadata 可搭 —— 少了這個欄位,
			// 呼叫端就只能回頭比對訊息文字。未映射的內部錯誤這裡是空字串。
			item.ErrorReason = ErrorReason(mapped)
			var ce *connect.Error
			if errors.As(mapped, &ce) {
				item.ErrorMessage = ce.Message()
			}
			resp.Failed++
		} else {
			resp.Succeeded++
		}
		resp.Results = append(resp.Results, item)
	}
	return connect.NewResponse(resp), nil
}

// RecordMessageRevision 記編輯/刪除前的舊版本。
func (h activityHandler) RecordMessageRevision(
	ctx context.Context, req *connect.Request[platformv1.RecordMessageRevisionRequest],
) (*connect.Response[platformv1.RecordMessageRevisionResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("ActivityService")
	}
	userID, err := requireActingUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	kind, err := revisionKind(m.GetKind())
	if err != nil {
		return nil, err
	}
	res, err := h.svc.RecordRevision(ctx, activitylog.RevisionParams{
		UserID:          userID,
		GuildID:         m.GetGuildId(),
		ChannelID:       m.GetChannelId(),
		MessageID:       m.GetMessageId(),
		PreviousContent: m.GetPreviousContent(),
		Kind:            kind,
		CapturedAt:      timeOf(m.GetCapturedAt()),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	noteSpace(ctx, res.SpaceID)
	return connect.NewResponse(&platformv1.RecordMessageRevisionResponse{
		Deduplicated:  res.Deduplicated,
		ExcerptStored: res.ExcerptStored,
	}), nil
}

// RecordReaction 記按/取消表情。
func (h activityHandler) RecordReaction(
	ctx context.Context, req *connect.Request[platformv1.RecordReactionRequest],
) (*connect.Response[platformv1.RecordReactionResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("ActivityService")
	}
	userID, err := requireActingUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	removed, err := reactionRemoved(m.GetAction())
	if err != nil {
		return nil, err
	}
	res, err := h.svc.RecordReaction(ctx, activitylog.ReactionParams{
		UserID:          userID,
		GuildID:         m.GetGuildId(),
		MessageID:       m.GetMessageId(),
		Emoji:           m.GetEmoji(),
		Removed:         removed,
		OccurredAt:      timeOf(m.GetOccurredAt()),
		MessageAuthorID: m.GetMessageAuthorId(),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	noteSpace(ctx, res.SpaceID)
	return connect.NewResponse(&platformv1.RecordReactionResponse{
		Deduplicated: res.Deduplicated,
		StatsUpdated: res.StatsUpdated,
	}), nil
}

// RecordPresence 記線上狀態區間。
//
// 提醒(schemas/10):這條路徑**不會、也不可以**發任何 XP 或點數。
// presence 需特權 intent、Bot 重啟會整段漏掉、隱身看不到,而且掛機就能刷。
func (h activityHandler) RecordPresence(
	ctx context.Context, req *connect.Request[platformv1.RecordPresenceRequest],
) (*connect.Response[platformv1.RecordPresenceResponse], error) {
	if h.svc == nil {
		return nil, unimplemented("ActivityService")
	}
	userID, err := requireActingUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	res, err := h.svc.RecordPresence(ctx, activitylog.PresenceParams{
		UserID:    userID,
		GuildID:   m.GetGuildId(),
		Status:    m.GetStatus(),
		StartedAt: timeOf(m.GetStartedAt()),
		EndedAt:   optTime(m.EndedAt),
	})
	if err != nil {
		return nil, toConnectError(err)
	}
	noteSpace(ctx, res.SpaceID)
	return connect.NewResponse(&platformv1.RecordPresenceResponse{
		Deduplicated:         res.Deduplicated,
		Closed:               res.Closed,
		OnlineSecondsCounted: res.OnlineSecondsCounted,
	}), nil
}

// ── proto ↔ core 的小轉換 ────────────────────────────────────────

// revisionKind 把 enum 轉成 message_revisions.kind 的值域。
// UNSPECIFIED 是錯誤而不是預設:「不知道是編輯還是刪除」的紀錄毫無用處。
func revisionKind(k platformv1.MessageRevisionKind) (string, error) {
	switch k {
	case platformv1.MessageRevisionKind_MESSAGE_REVISION_KIND_EDITED:
		return activitylog.RevisionEdited, nil
	case platformv1.MessageRevisionKind_MESSAGE_REVISION_KIND_DELETED:
		return activitylog.RevisionDeleted, nil
	case platformv1.MessageRevisionKind_MESSAGE_REVISION_KIND_UNSPECIFIED:
		return "", invalidArgument("kind 必須是 EDITED 或 DELETED")
	default:
		return "", invalidArgument("kind 必須是 EDITED 或 DELETED")
	}
}

// reactionRemoved 把 enum 轉成「這是不是取消」。同樣不給 UNSPECIFIED 預設值:
// 猜錯方向會讓計數往反方向跑。
func reactionRemoved(a platformv1.ReactionAction) (bool, error) {
	switch a {
	case platformv1.ReactionAction_REACTION_ACTION_ADDED:
		return false, nil
	case platformv1.ReactionAction_REACTION_ACTION_REMOVED:
		return true, nil
	case platformv1.ReactionAction_REACTION_ACTION_UNSPECIFIED:
		return false, invalidArgument("action 必須是 ADDED 或 REMOVED")
	default:
		return false, invalidArgument("action 必須是 ADDED 或 REMOVED")
	}
}

// timeOf 把 protobuf timestamp 轉成 time.Time;nil / 零值回零值,
// 由核心的參數檢查統一擋(入口層不重複一次相同的判斷)。
func timeOf(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
