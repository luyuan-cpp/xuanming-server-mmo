// Package logic保留聊天纯校验、错误码和指标；授权、永久原件和窗口读写收口于SQL深接口。
package logic

import (
	"context"
	"errors"
	"strings"

	"chat/internal/authority"
	"chat/internal/constants"
	"chat/internal/session"
	"chat/internal/svc"
	chatpb "proto/chat"
	base "proto/common/base"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const maxRequestIdBytes = 64

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
	if _, r, ok := resolveLogKey(msg.Channel, actor, msg.TargetPlayerId); !ok {
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
		l.Error("聊天SQL发送未确认，保留原request_id重放")
		return reject(rejectStorage)
	}
	if result.Tip != 0 {
		svc.ObserveSend(label, svc.OutcomeRateLimited)
		return &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: result.Tip}}, nil
	}
	outcome := svc.OutcomeOK
	if result.Duplicate {
		outcome = svc.OutcomeDuplicate
	}
	svc.ObserveSend(label, outcome)
	return &chatpb.SendChatResponse{}, nil
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
	if _, r, ok := resolveLogKey(in.Channel, actor, in.PeerPlayerId); !ok {
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
		l.Error("聊天SQL读取未确认，未返回窗口内容")
		return reject(rejectStorage)
	}
	svc.ObservePull(label, svc.OutcomeOK)
	return &chatpb.PullChatHistoryResponse{Messages: messages}, nil
}

// 这里只决定纯参数资格；身份由 authority 校验，私聊分区保持双方player_id历史语义。
func resolveLogKey(channel chatpb.ChatChannelType, self, peer uint64) (string, rejection, bool) {
	switch channel {
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD:
		return "world-v2", rejection{}, true
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE:
		if peer == 0 || peer == self {
			return "", rejectBadRequest, false
		}
		return "private-v2", rejection{}, true
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_TEAM, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_SYSTEM, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_UNSPECIFIED:
		return "", rejectChannelUnavailable, false
	default:
		return "", rejectBadRequest, false
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
