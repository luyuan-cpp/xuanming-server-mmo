// Package logic保留聊天纯校验、错误码和指标；授权、永久原件和窗口读写收口于SQL深接口。
package logic

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"chat/internal/authority"
	"chat/internal/constants"
	"chat/internal/session"
	"chat/internal/svc"
	chatpb "proto/chat"
	base "proto/common/base"
	cgpb "proto/content_guard"
	"shared/contentguard"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const (
	maxRequestIdBytes    = 64
	maxRetractReasonSize = 128
	maxReportDetailSize  = 200
	asyncRecheckTimeout  = 4 * time.Second
	asyncRetractTimeout  = 3 * time.Second
	platformRejectReason = "platform_async_reject"
)

type rejection struct {
	code    uint32
	outcome string
}

var (
	rejectNoSession          = rejection{constants.ErrInvalidParameter, svc.OutcomeNoSession}
	rejectBadRequest         = rejection{constants.ErrInvalidParameter, svc.OutcomeBadRequest}
	rejectChannelUnavailable = rejection{constants.ErrChannelUnavailable, svc.OutcomeChannelUnavailable}
	rejectTooLong            = rejection{constants.ErrMessageTooLong, svc.OutcomeTooLong}
	rejectRateLimited        = rejection{constants.ErrRateLimited, svc.OutcomeRateLimited}
	rejectStorage            = rejection{constants.ErrStorage, svc.OutcomeStorageError}
)

type ChatLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewChatLogic(ctx context.Context, sc *svc.ServiceContext) *ChatLogic {
	return &ChatLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: sc}
}

func (l *ChatLogic) SendChat(in *chatpb.SendChatRequest) (*chatpb.SendChatResponse, error) {
	msg := in.GetMessage()
	label := svc.ChannelLabel(msg.GetChannel())
	reject := func(r rejection) (*chatpb.SendChatResponse, error) {
		svc.ObserveSend(label, r.outcome)
		return &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: r.code}}, nil
	}
	actor, ok := session.PlayerID(l.ctx)
	if !ok {
		return reject(rejectNoSession)
	}
	if msg == nil {
		return reject(rejectBadRequest)
	}
	if r, ok := channelAccepted(msg.Channel, actor, msg.TargetPlayerId); !ok {
		return reject(r)
	}
	if len(msg.Content) > l.svcCtx.Config.MaxContentBytes || strings.TrimSpace(msg.Content) == "" {
		return reject(rejectTooLong)
	}
	if len(in.RequestId) > maxRequestIdBytes || proto.Size(in) > l.svcCtx.Config.MaxContentBytes+8192 {
		return reject(rejectBadRequest)
	}
	if l.ctx.Err() != nil || l.svcCtx.Authority == nil {
		return reject(rejectStorage)
	}
	result, err := l.svcCtx.Authority.Send(l.ctx, proto.Clone(in).(*chatpb.SendChatRequest))
	if err != nil {
		if errors.Is(err, authority.ErrOriginal) {
			return reject(rejectNoSession)
		}
		if errors.Is(err, authority.ErrInvalid) || errors.Is(err, authority.ErrConflict) {
			return reject(rejectBadRequest)
		}
		l.Error("Chat SQL send unconfirmed; keep the original request_id for replay")
		return reject(rejectStorage)
	}
	if result.Tip != 0 {
		// 按拒因分别记入低基数 send_total 指标。
		outcome := svc.OutcomeRateLimited
		switch {
		case result.Cooldown:
			outcome = svc.OutcomeWorldCooldown
		case result.Tip == constants.ErrContentRejected:
			outcome = svc.OutcomeContentRejected
		case result.Tip == constants.ErrContentCheckUnavailable:
			outcome = svc.OutcomeContentUnavailable
		case result.Tip == constants.ErrLevelTooLow:
			outcome = svc.OutcomeLevelTooLow
		case result.Tip == constants.ErrDuplicateSpam:
			outcome = svc.OutcomeDuplicateSpam
		case result.Tip == constants.ErrPrivateTargetLimit:
			outcome = svc.OutcomePrivateTargetLimit
		case result.Tip == constants.ErrContactRejected:
			outcome = svc.OutcomeContactRejected
		}
		svc.ObserveSend(label, outcome)
		return &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: result.Tip}}, nil
	}
	outcome := svc.OutcomeOK
	if result.Duplicate {
		outcome = svc.OutcomeDuplicate
	}
	svc.ObserveSend(label, outcome)
	// 世界频道首次提交:推给全部在线玩家(docs/design/chat-world-channel-fanout.md §4)。
	l.svcCtx.PublishWorld(l.ctx, result.Message)
	if result.PendingPlatformReview && result.Message != nil {
		l.scheduleAsyncWorldPlatformRecheck(result.Message, result.PlatformReviewContent)
	}
	return &chatpb.SendChatResponse{}, nil
}

func (l *ChatLogic) scheduleAsyncWorldPlatformRecheck(msg *chatpb.ChatMessage, rawContent string) {
	checker := l.svcCtx.PlatformCheck
	if checker == nil && l.svcCtx.ContentGuard != nil && l.svcCtx.ContentGuard.Enabled() {
		checker = l.svcCtx.ContentGuard.CheckPlatform
	}
	if checker == nil || l.svcCtx.Authority == nil || msg == nil || rawContent == "" {
		return
	}
	sc := l.svcCtx
	senderID := msg.SenderPlayerId
	sendTimeMs := msg.SendTimeMs
	safego.Go("chat.world_platform_recheck", func() {
		ctx, cancel := context.WithTimeout(context.Background(), asyncRecheckTimeout)
		defer cancel()
		res := checker(ctx, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, senderID, rawContent)
		if res.Verdict != contentguard.Reject {
			if res.Verdict == contentguard.Unavailable {
				logx.Errorf("[chat] Async platform moderation unavailable, sender=%d send_time_ms=%d", senderID, sendTimeMs)
			}
			return
		}
		// 撤回另起一个时限:送检可能已经把上面那个时限耗得差不多了,平台判了违规却因为没时间落库而留着这条消息是最坏的结局。
		retractCtx, cancelRetract := context.WithTimeout(context.Background(), asyncRetractTimeout)
		defer cancelRetract()
		retracted, _, err := sc.Authority.RetractWorld(retractCtx, senderID, sendTimeMs, platformRejectReason)
		if err != nil {
			logx.Errorf("[chat] metric=chat_world_platform_reject_not_retracted sender=%d send_time_ms=%d: %v", senderID, sendTimeMs, err)
			return
		}
		if retracted {
			sc.PublishWorldRetract(retractCtx, &chatpb.ChatRetractedNotify{
				Channel:        chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
				SenderPlayerId: senderID,
				SendTimeMs:     sendTimeMs,
				Reason:         platformRejectReason,
			})
		}
	})
}

func (l *ChatLogic) ReportChat(in *chatpb.ReportChatRequest) (*chatpb.ReportChatResponse, error) {
	actor, ok := session.PlayerID(l.ctx)
	if !ok {
		return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter}}, nil
	}
	if in == nil || in.GetTargetSenderPlayerId() == 0 || in.GetSendTimeMs() <= 0 ||
		in.GetReason() == 0 || in.GetReason() > 4 || len(in.GetDetail()) > maxReportDetailSize || !utf8.ValidString(in.GetDetail()) {
		return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter}}, nil
	}
	if _, ok := channelAccepted(in.GetChannel(), actor, in.GetTargetSenderPlayerId()); !ok {
		return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter}}, nil
	}
	if l.ctx.Err() != nil || l.svcCtx.Authority == nil {
		return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage}}, nil
	}
	tip, err := l.svcCtx.Authority.ReportMessage(l.ctx, proto.Clone(in).(*chatpb.ReportChatRequest))
	if err != nil {
		if errors.Is(err, authority.ErrOriginal) || errors.Is(err, authority.ErrInvalid) {
			return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter}}, nil
		}
		l.Error("Chat report SQL write failed")
		return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage}}, nil
	}
	if tip != 0 {
		return &chatpb.ReportChatResponse{ErrorMessage: &base.TipInfoMessage{Id: tip}}, nil
	}
	return &chatpb.ReportChatResponse{}, nil
}

// RetractWorldChat 是运维处置入口:先核运维口令(ops_auth.go),口令没配时整个入口停用。
// 服务自己的异步平台补审不走这里,直接调 Authority.RetractWorld。
func (l *ChatLogic) RetractWorldChat(in *chatpb.RetractWorldChatRequest) (*chatpb.RetractWorldChatResponse, error) {
	if err := l.authorizeOps(chatpb.ClientPlayerChat_RetractWorldChat_FullMethodName); err != nil {
		return nil, err
	}
	if in == nil || in.GetSenderPlayerId() == 0 || in.GetSendTimeMs() <= 0 ||
		len(in.GetReason()) > maxRetractReasonSize || !utf8.ValidString(in.GetReason()) {
		return &chatpb.RetractWorldChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter}}, nil
	}
	if l.ctx.Err() != nil || l.svcCtx.Authority == nil {
		return &chatpb.RetractWorldChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage}}, nil
	}
	retracted, tip, err := l.svcCtx.Authority.RetractWorld(l.ctx, in.GetSenderPlayerId(), in.GetSendTimeMs(), strings.TrimSpace(in.GetReason()))
	if err != nil {
		if errors.Is(err, authority.ErrInvalid) {
			return &chatpb.RetractWorldChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter}}, nil
		}
		l.Error("Chat retract SQL write failed")
		return &chatpb.RetractWorldChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage}}, nil
	}
	if tip != 0 {
		return &chatpb.RetractWorldChatResponse{ErrorMessage: &base.TipInfoMessage{Id: tip}}, nil
	}
	if retracted {
		l.svcCtx.PublishWorldRetract(l.ctx, &chatpb.ChatRetractedNotify{
			Channel:        chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
			SenderPlayerId: in.GetSenderPlayerId(),
			SendTimeMs:     in.GetSendTimeMs(),
			Reason:         strings.TrimSpace(in.GetReason()),
		})
	}
	return &chatpb.RetractWorldChatResponse{Retracted: retracted}, nil
}

// PublishSystemWorldNotice 内部白名单模板全服系统播报（CLIENT_RPC_SERVER_ONLY）。
// 仅在首次提交成功时推给全服在线玩家；命中幂等键时返回 deduplicated=true 且不重复广播。
func (l *ChatLogic) PublishSystemWorldNotice(in *chatpb.PublishSystemWorldNoticeRequest) (*chatpb.PublishSystemWorldNoticeResponse, error) {
	if _, ok := authority.RenderSystemWorldNotice(in); !ok {
		return &chatpb.PublishSystemWorldNoticeResponse{
			ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter},
		}, nil
	}
	if l.ctx.Err() != nil || l.svcCtx.Authority == nil {
		return &chatpb.PublishSystemWorldNoticeResponse{
			ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage},
		}, nil
	}
	result, err := l.svcCtx.Authority.PublishSystemWorldNotice(l.ctx, proto.Clone(in).(*chatpb.PublishSystemWorldNoticeRequest))
	if err != nil {
		if errors.Is(err, authority.ErrInvalid) {
			return &chatpb.PublishSystemWorldNoticeResponse{
				ErrorMessage: &base.TipInfoMessage{Id: constants.ErrInvalidParameter},
			}, nil
		}
		l.Error("Chat system world notice SQL write failed")
		return &chatpb.PublishSystemWorldNoticeResponse{
			ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage},
		}, nil
	}
	if !result.Duplicate && result.Message != nil {
		l.svcCtx.PublishWorld(l.ctx, result.Message)
	}
	return &chatpb.PublishSystemWorldNoticeResponse{Deduplicated: result.Duplicate}, nil
}

func (l *ChatLogic) PullChatHistory(in *chatpb.PullChatHistoryRequest) (*chatpb.PullChatHistoryResponse, error) {
	label := svc.ChannelLabel(in.GetChannel())
	reject := func(r rejection) (*chatpb.PullChatHistoryResponse, error) {
		svc.ObservePull(label, r.outcome)
		return &chatpb.PullChatHistoryResponse{ErrorMessage: &base.TipInfoMessage{Id: r.code}}, nil
	}
	actor, ok := session.PlayerID(l.ctx)
	if !ok {
		return reject(rejectNoSession)
	}
	if in == nil {
		return reject(rejectBadRequest)
	}
	if r, ok := channelAccepted(in.Channel, actor, in.PeerPlayerId); !ok {
		return reject(r)
	}
	if l.ctx.Err() != nil || l.svcCtx.Authority == nil {
		return reject(rejectStorage)
	}
	limit := clampHistoryLimit(in.Limit, l.svcCtx.Config.HistoryDefaultLimit, l.svcCtx.Config.HistoryMaxLimit)
	messages, err := l.svcCtx.Authority.Pull(l.ctx, proto.Clone(in).(*chatpb.PullChatHistoryRequest), limit)
	if err != nil {
		if errors.Is(err, authority.ErrOriginal) {
			return reject(rejectNoSession)
		}
		if errors.Is(err, authority.ErrInvalid) {
			return reject(rejectBadRequest)
		}
		l.Error("Chat SQL read unconfirmed; window content not returned")
		return reject(rejectStorage)
	}
	svc.ObservePull(label, svc.OutcomeOK)
	return &chatpb.PullChatHistoryResponse{Messages: messages}, nil
}

// channelAccepted 只决定纯参数资格:这个频道开不开放、私聊的对端合不合法。身份由 authority 校验;
// 消息落在哪个流键也由 authority 决定(世界频道按发送者分段,私聊按双方 player_id),这里不再返回键名。
func channelAccepted(channel chatpb.ChatChannelType, self, peer uint64) (rejection, bool) {
	switch channel {
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD:
		return rejection{}, true
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE:
		if peer == 0 || peer == self {
			return rejectBadRequest, false
		}
		return rejection{}, true
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_TEAM, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_SYSTEM, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_UNSPECIFIED:
		return rejectChannelUnavailable, false
	default:
		return rejectBadRequest, false
	}
}
func clampHistoryLimit(requested uint32, def, max int) int {
	if requested == 0 {
		return def
	}
	if uint64(requested) > uint64(max) {
		return max
	}
	return int(requested)
}
