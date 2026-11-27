package logic

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	cgpb "proto/content_guard"
	matchpb "proto/match"

	"shared/contentguard"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// spectateChatRoomInfo 是从权威成局原件提取的不可变房间事实，供 Send / Pull 共同执法。
type spectateChatRoomInfo struct {
	battleID     uint64
	visibility   battlepb.ESpectateVisibility
	chatEnabled  bool
	ended        bool
	deadlineMs   uint64
	participants map[uint64]struct{}
}

// loadSpectateChatRoomFn 读 SQL 权威成局原件；单测可替换而不起 TiDB（与 loadSpectateBattleFn 同一接缝）。
var loadSpectateChatRoomFn = loadSpectateChatRoom

func loadSpectateChatRoom(ctx context.Context, svcCtx *svc.ServiceContext, battleID uint64, now uint64) (*spectateChatRoomInfo, error) {
	if svcCtx == nil || svcCtx.Authority == nil {
		return nil, nil
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	f, err := svcCtx.Authority.Formation(qctx, battleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(f.CreateRequest) == 0 {
		return nil, nil
	}
	create := &battlepb.CreateBattleRequest{}
	if err := proto.Unmarshal(f.CreateRequest, create); err != nil {
		return nil, err
	}
	participants := make(map[uint64]struct{}, len(create.Players))
	for _, p := range create.Players {
		if p != nil && !p.GetAiControlled() && p.GetPlayerId() != 0 {
			participants[p.GetPlayerId()] = struct{}{}
		}
	}
	ended := authority.FormationOutcomeDecided(f) || (f.DeadlineMs > 0 && now > f.DeadlineMs)
	return &spectateChatRoomInfo{
		battleID:     f.BattleId,
		visibility:   create.GetSpectateVisibility(),
		chatEnabled:  create.GetSpectateChatEnabled(),
		ended:        ended,
		deadlineMs:   f.DeadlineMs,
		participants: participants,
	}, nil
}

type SpectateChatLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewSpectateChatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpectateChatLogic {
	return &SpectateChatLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func spectateChatTip(code uint32) *base.TipInfoMessage {
	if code == 0 {
		return nil
	}
	return &base.TipInfoMessage{Id: code}
}

// SendBattleSpectateChat 局外人观战交流发言：
//   - 权威判定成局冻结开关（spectate_chat_enabled == false 时硬拒绝，但绝不影响正常观战）；
//   - 全房唯一公开室，服务端按参战者集合标注 role（参战者 / 观战者）；
//   - 自由文本强过 ContentGuard（fail-closed：不可达或命禁拒发，辱骂打码后放行）；
//   - 单槽 Redis Lua（或单测内存兜底）实施个人冷却与单房每秒总流速封顶。
func (l *SpectateChatLogic) SendBattleSpectateChat(in *matchpb.SendBattleSpectateChatRequest) (*matchpb.SendBattleSpectateChatResponse, error) {
	now := nowMs()
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrInternal),
			ServerTimeMs: now,
		}, nil
	}
	battleID := in.GetBattleId()
	cfg := l.svcCtx.Config.SpectateChat
	trimmed := strings.TrimSpace(in.GetContent())
	if battleID == 0 || trimmed == "" || !utf8.ValidString(trimmed) || len(trimmed) > cfg.EffectiveMaxContentBytes() {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(uint32(table.CommonError_kInvalidParameter)),
			BattleId:     battleID,
			ServerTimeMs: now,
		}, nil
	}
	if !cfg.IsEnabled() {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrSpectateChatDisabled),
			BattleId:     battleID,
			Enabled:      false,
			ServerTimeMs: now,
		}, nil
	}
	room, err := loadSpectateChatRoomFn(l.ctx, l.svcCtx, battleID, now)
	if err != nil {
		l.Errorf("[spectate_chat] load formation failed battle=%d: %v", battleID, err)
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrInternal),
			BattleId:     battleID,
			ServerTimeMs: now,
		}, nil
	}
	if room == nil {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrBattleNotWatchable),
			BattleId:     battleID,
			BattleEnded:  true,
			ServerTimeMs: now,
		}, nil
	}
	if !isSpectatePublic(room.visibility) || !room.chatEnabled {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrSpectateChatDisabled),
			BattleId:     battleID,
			Enabled:      false,
			BattleEnded:  room.ended,
			ServerTimeMs: now,
		}, nil
	}
	if room.ended {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrSpectateChatEnded),
			BattleId:     battleID,
			Enabled:      true,
			BattleEnded:  true,
			ServerTimeMs: now,
		}, nil
	}

	// 自由文本强制走内容安全审核（fail-closed）：仅放行 Pass 与有效 Masked。
	finalContent := trimmed
	if l.svcCtx.ContentGuard != nil {
		res := l.svcCtx.ContentGuard.Check(l.ctx, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, playerID, trimmed)
		switch res.Verdict {
		case contentguard.Pass:
		case contentguard.Masked:
			if strings.TrimSpace(res.Masked) == "" || len(res.Masked) > cfg.EffectiveMaxContentBytes() {
				return &matchpb.SendBattleSpectateChatResponse{
					ErrorMessage: spectateChatTip(constants.ErrSpectateChatContentRejected),
					BattleId:     battleID,
					Enabled:      true,
					ServerTimeMs: now,
				}, nil
			}
			finalContent = res.Masked
		case contentguard.Unavailable:
			return &matchpb.SendBattleSpectateChatResponse{
				ErrorMessage: spectateChatTip(constants.ErrSpectateChatCheckUnavailable),
				BattleId:     battleID,
				Enabled:      true,
				ServerTimeMs: now,
			}, nil
		default:
			return &matchpb.SendBattleSpectateChatResponse{
				ErrorMessage: spectateChatTip(constants.ErrSpectateChatContentRejected),
				BattleId:     battleID,
				Enabled:      true,
				ServerTimeMs: now,
			}, nil
		}
	}

	role := matchpb.BattleSpectateChatRole_BATTLE_SPECTATE_CHAT_ROLE_SPECTATOR
	cooldownMs := cfg.EffectiveSpectatorCooldownMs()
	if _, isParticipant := room.participants[playerID]; isParticipant {
		role = matchpb.BattleSpectateChatRole_BATTLE_SPECTATE_CHAT_ROLE_PARTICIPANT
		cooldownMs = cfg.EffectiveParticipantCooldownMs()
	}
	ttlSec := spectateTTLSeconds
	if room.deadlineMs > now {
		remain := int((room.deadlineMs-now)/1000) + 300
		if remain > 60 && remain < ttlSec {
			ttlSec = remain
		}
	}
	senderName := challengePublicName(l.ctx, l.svcCtx, playerID)
	sendRes, err := l.svcCtx.ActiveSpectateChat().Send(
		l.ctx, battleID, playerID, senderName, role, finalContent, now, ttlSec, cooldownMs, cfg.EffectiveRoomRateLimitPerSec(),
	)
	if err != nil {
		l.Errorf("[spectate_chat] store send failed battle=%d player=%d: %v", battleID, playerID, err)
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrInternal),
			BattleId:     battleID,
			Enabled:      true,
			ServerTimeMs: now,
		}, nil
	}
	if sendRes.RoomClosed {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrSpectateChatEnded),
			BattleId:     battleID,
			Enabled:      true,
			BattleEnded:  true,
			ServerTimeMs: now,
		}, nil
	}
	if sendRes.HitCooldown {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage:    spectateChatTip(constants.ErrSpectateChatCooldown),
			BattleId:        battleID,
			Enabled:         true,
			CooldownUntilMs: sendRes.CooldownUntilMs,
			ServerTimeMs:    now,
		}, nil
	}
	if sendRes.HitRoomBusy {
		return &matchpb.SendBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrSpectateChatRoomBusy),
			BattleId:     battleID,
			Enabled:      true,
			ServerTimeMs: now,
		}, nil
	}
	return &matchpb.SendBattleSpectateChatResponse{
		BattleId:        battleID,
		Enabled:         true,
		BattleEnded:     false,
		CooldownUntilMs: sendRes.CooldownUntilMs,
		Message:         sendRes.Message,
		ServerTimeMs:    now,
	}, nil
}

// PullBattleSpectateChat 增量拉取指定战斗公开观战交流室的消息（按 seq > after_seq）。
func (l *SpectateChatLogic) PullBattleSpectateChat(in *matchpb.PullBattleSpectateChatRequest) (*matchpb.PullBattleSpectateChatResponse, error) {
	now := nowMs()
	playerID := authoritativePlayerID(l.ctx, 0)
	if l.svcCtx == nil {
		return &matchpb.PullBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrInternal),
			ServerTimeMs: now,
		}, nil
	}
	battleID := in.GetBattleId()
	if battleID == 0 {
		return &matchpb.PullBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(uint32(table.CommonError_kInvalidParameter)),
			ServerTimeMs: now,
		}, nil
	}
	cfg := l.svcCtx.Config.SpectateChat
	if !cfg.IsEnabled() {
		return &matchpb.PullBattleSpectateChatResponse{
			BattleId:     battleID,
			Enabled:      false,
			ServerTimeMs: now,
		}, nil
	}
	room, err := loadSpectateChatRoomFn(l.ctx, l.svcCtx, battleID, now)
	if err != nil {
		l.Errorf("[spectate_chat] pull load formation failed battle=%d: %v", battleID, err)
		return &matchpb.PullBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrInternal),
			BattleId:     battleID,
			ServerTimeMs: now,
		}, nil
	}
	if room == nil {
		return &matchpb.PullBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrBattleNotWatchable),
			BattleId:     battleID,
			BattleEnded:  true,
			ServerTimeMs: now,
		}, nil
	}
	if !isSpectatePublic(room.visibility) || !room.chatEnabled {
		return &matchpb.PullBattleSpectateChatResponse{
			BattleId:     battleID,
			Enabled:      false,
			BattleEnded:  room.ended,
			ServerTimeMs: now,
		}, nil
	}
	pullRes, err := l.svcCtx.ActiveSpectateChat().Pull(l.ctx, battleID, playerID, in.GetAfterSeq(), int(in.GetLimit()), now)
	if err != nil {
		l.Errorf("[spectate_chat] store pull failed battle=%d: %v", battleID, err)
		return &matchpb.PullBattleSpectateChatResponse{
			ErrorMessage: spectateChatTip(constants.ErrInternal),
			BattleId:     battleID,
			Enabled:      true,
			BattleEnded:  room.ended,
			ServerTimeMs: now,
		}, nil
	}
	return &matchpb.PullBattleSpectateChatResponse{
		BattleId:        battleID,
		Enabled:         true,
		BattleEnded:     room.ended || pullRes.RoomClosed,
		CooldownUntilMs: pullRes.CooldownUntilMs,
		LatestSeq:       pullRes.LatestSeq,
		Messages:        pullRes.Messages,
		ServerTimeMs:    now,
	}, nil
}
