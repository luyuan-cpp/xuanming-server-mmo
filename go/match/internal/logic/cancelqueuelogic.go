package logic

import (
	"context"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"match/internal/constants"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelQueueLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelQueueLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelQueueLogic {
	return &CancelQueueLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CancelQueue 取消排队。原请求编号或票据至少给一个;撤销在 SQL 提交,与 Join 争同一玩家行锁,
// 已提交的撤销留永久墓碑,迟到的原 Join 不能复活。
//   - 带原请求编号且命中整队发起:撤这支整队(CancelTeamLaunchForActor);
//   - 本人的票属于整队排队:撤这一单并通知全队(CancelTeamQueueForActor);
//   - 其余:单排票撤销(CancelIntent)。已成局的票撤不动,开局结局由成局原件决定。
func (l *CancelQueueLogic) CancelQueue(in *matchpb.CancelQueueRequest) (*base.Empty, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if playerId == 0 {
		return &base.Empty{}, nil
	}
	if in.RequestId != "" {
		if parsed, e := uuid.Parse(in.RequestId); e != nil || parsed == uuid.Nil || parsed.String() != in.RequestId {
			return nil, status.Error(codes.InvalidArgument, "匹配请求编号无效")
		}
	}
	if in.RequestId == "" && in.QueueTicket == "" {
		return nil, status.Error(codes.InvalidArgument, "撤销需要原请求编号或票据")
	}
	if in.RequestId != "" {
		launch, e := l.svcCtx.Authority.TeamLaunch(l.ctx, playerId, in.RequestId)
		if e != nil {
			return nil, e
		}
		if launch != nil {
			members, e := l.svcCtx.Authority.CancelTeamLaunchForActor(l.ctx, playerId, launch)
			if e == nil && len(members) > 0 {
				if port := l.svcCtx.PartyTeam(); port != nil {
					port.PartyEnded(launch.TeamId, members, constants.PartyEndCanceled, playerId)
				}
			}
			return &base.Empty{}, e
		}
	}
	ticket, e := l.svcCtx.Authority.Ticket(l.ctx, playerId)
	if e != nil {
		return nil, e
	}
	if ticket != nil && ticket.TeamId != 0 && (in.QueueTicket == "" || in.QueueTicket == ticket.Ticket) && (in.RequestId == "" || in.RequestId == ticket.RequestId) {
		members, e := l.svcCtx.Authority.CancelTeamQueueForActor(l.ctx, playerId, ticket)
		if e == nil && len(members) > 0 {
			if port := l.svcCtx.PartyTeam(); port != nil {
				port.PartyEnded(ticket.TeamId, members, constants.PartyEndCanceled, playerId)
			}
		}
		return &base.Empty{}, e
	}
	return &base.Empty{}, l.svcCtx.Authority.CancelIntent(l.ctx, playerId, in.QueueTicket, in.RequestId)
}
