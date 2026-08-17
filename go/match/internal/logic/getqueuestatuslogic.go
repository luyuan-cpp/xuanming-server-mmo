package logic

import (
	"context"

	"match/internal/svc"

	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetQueueStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetQueueStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetQueueStatusLogic {
	return &GetQueueStatusLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetQueueStatus 查询排队状态。QueueState 五态映射:
//
//	ticket 不存在            -> QUEUE_STATE_NOT_QUEUED
//	state=queued             -> QUEUE_STATE_QUEUED
//	state=matched            -> QUEUE_STATE_MATCHED(gather 管线执行中)
//	state=ready              -> QUEUE_STATE_READY(战斗已建,等 BattleStartS2C)
//	QUEUE_STATE_ENTERING     -> 场景匹配(5v5/3v3)的进场态,回合制无进场步骤,
//	                            一期不产生;保留枚举以兼容二期场景匹配。
func (l *GetQueueStatusLogic) GetQueueStatus(in *matchpb.GetQueueStatusRequest) (*matchpb.GetQueueStatusResponse, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if playerId == 0 {
		return &matchpb.GetQueueStatusResponse{
			State: matchpb.QueueState_QUEUE_STATE_NOT_QUEUED,
		}, nil
	}

	ticket, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] GetQueueStatus 读 ticket 失败 player=%d: %v", playerId, err)
		return nil, err
	}
	if ticket == nil {
		return &matchpb.GetQueueStatusResponse{
			State: matchpb.QueueState_QUEUE_STATE_NOT_QUEUED,
		}, nil
	}

	state := matchpb.QueueState_QUEUE_STATE_UNSPECIFIED
	switch ticket.State {
	case ticketStateQueued:
		state = matchpb.QueueState_QUEUE_STATE_QUEUED
	case ticketStateMatched:
		state = matchpb.QueueState_QUEUE_STATE_MATCHED
	case ticketStateReady:
		state = matchpb.QueueState_QUEUE_STATE_READY
	default:
		l.Errorf("[match] 未知 ticket 状态 %q player=%d,按 NOT_QUEUED 返回", ticket.State, playerId)
		state = matchpb.QueueState_QUEUE_STATE_NOT_QUEUED
	}

	var queuedSeconds uint32
	if ticket.EnqueuedAtMs > 0 {
		if elapsed := nowMs() - ticket.EnqueuedAtMs; elapsed > 0 {
			queuedSeconds = uint32(elapsed / 1000)
		}
	}

	return &matchpb.GetQueueStatusResponse{
		State: state,
		// 一期不做等待时长预估(需要历史凑单速率统计,二期随负载上报一起做)。
		EstimatedWaitSeconds: 0,
		QueuedSeconds:        queuedSeconds,
	}, nil
}
