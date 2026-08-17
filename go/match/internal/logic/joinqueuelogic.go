package logic

import (
	"context"
	"strconv"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"shared/safego"
)

// tipErr 组装客户端提示(照 friend/guild 的 per-service tip 模式)。
func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

type JoinQueueLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewJoinQueueLogic(ctx context.Context, svcCtx *svc.ServiceContext) *JoinQueueLogic {
	return &JoinQueueLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// JoinQueue 玩家入队(设计文档 §5.4):
//   - 咨询性查 battle:lock:{player_id},存在即拒(权威判定仍在 scene 的 InBattleComp);
//   - MATCH_MODE_PVE_SOLO 即时开战:不入队,直接走 gather 开局管线;
//   - MATCH_MODE_PVE_TEAM 按 battle_config_id 查凑满人数,FIFO 凑单;
//   - MATCH_MODE_1V1 两人凑对;
//   - 5v5/3v3 一期未开放,切磋走 ChallengePlayer,均拒绝直接入队。
func (l *JoinQueueLogic) JoinQueue(in *matchpb.JoinQueueRequest) (*matchpb.JoinQueueResponse, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	modeName := in.Mode.String()
	if playerId == 0 {
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}

	// 一期只做单人入队 FIFO 凑单,预组队(party_member_ids)二期接入。
	if len(in.PartyMemberIds) > 1 {
		l.Infof("[match] JoinQueue 携带预组队成员 %d 人,一期忽略,仅本人入队 player=%d",
			len(in.PartyMemberIds), playerId)
	}

	// 模式与凑满人数。
	var required uint32
	switch in.Mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
		required = 1
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		required = l.svcCtx.Config.PveTeamSizeFor(in.BattleConfigId)
		if required == 0 {
			metrics.ObserveJoinQueue(modeName, "no_team_size")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrTeamSizeNotConfigured,
				ErrorMessage: tipErr(constants.ErrTeamSizeNotConfigured, "该副本未开放组队"),
			}, nil
		}
	case matchpb.MatchMode_MATCH_MODE_1V1:
		required = 2
	default:
		// 5v5/3v3 一期未开放;切磋(PVP_CHALLENGE)点名成局,不走排队入口。
		metrics.ObserveJoinQueue(modeName, "mode_not_open")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrModeNotOpen,
			ErrorMessage: tipErr(constants.ErrModeNotOpen, "该匹配模式未开放"),
		}, nil
	}

	// 咨询性检查战斗锁。
	locked, err := isPlayerBattleLocked(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue 查战斗锁失败 player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if locked {
		metrics.ObserveJoinQueue(modeName, "in_battle")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInBattle,
			ErrorMessage: tipErr(constants.ErrInBattle, "战斗尚未结束,无法排队"),
		}, nil
	}

	// 每玩家最多一单在途(设计决策 D4)。
	existing, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue 读 ticket 失败 player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if existing != nil {
		metrics.ObserveJoinQueue(modeName, "already_queued")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrAlreadyQueued,
			QueueTicket:  existing.Ticket,
			ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中"),
		}, nil
	}

	ticket := &queueTicket{
		Ticket:       uuid.New().String(),
		Mode:         int32(in.Mode),
		Config:       in.BattleConfigId,
		EnqueuedAtMs: nowMs(),
	}

	// PVE solo 即时开战(伪匹配):不入队,ticket 直接进 matched 态走 gather。
	if in.Mode == matchpb.MatchMode_MATCH_MODE_PVE_SOLO {
		ticket.State = ticketStateMatched
		if err := writeTicket(l.svcCtx, playerId, ticket); err != nil {
			l.Errorf("[match] JoinQueue 写 ticket 失败 player=%d: %v", playerId, err)
			metrics.ObserveJoinQueue(modeName, "internal")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrInternal,
				ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
			}, nil
		}
		svcCtx := l.svcCtx
		mode := in.Mode
		config := in.BattleConfigId
		safego.Go("match.gather.pve_solo", func() {
			RunGather(svcCtx, mode, config, []uint64{playerId}, false)
		})
		l.Infof("[match] PVE solo 即时开战 player=%d config=%d ticket=%s", playerId, config, ticket.Ticket)
		metrics.ObserveJoinQueue(modeName, "ok")
		return &matchpb.JoinQueueResponse{QueueTicket: ticket.Ticket}, nil
	}

	// FIFO 入队:先写 ticket 再入队,保证 matcher 弹出时票据一定可见。
	ticket.State = ticketStateQueued
	if err := writeTicket(l.svcCtx, playerId, ticket); err != nil {
		l.Errorf("[match] JoinQueue 写 ticket 失败 player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if _, err := l.svcCtx.Redis.Rpush(matchQueueKey(int32(in.Mode), in.BattleConfigId),
		strconv.FormatUint(playerId, 10)); err != nil {
		l.Errorf("[match] JoinQueue 入队失败 player=%d: %v", playerId, err)
		deleteTicket(l.svcCtx, playerId)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	l.Infof("[match] 入队成功 player=%d mode=%s config=%d required=%d ticket=%s",
		playerId, modeName, in.BattleConfigId, required, ticket.Ticket)
	metrics.ObserveJoinQueue(modeName, "ok")
	return &matchpb.JoinQueueResponse{QueueTicket: ticket.Ticket}, nil
}
