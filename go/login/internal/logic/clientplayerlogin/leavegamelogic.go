package clientplayerloginlogic

import (
	"context"
	"login/internal/svc"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
)

type LeaveGameLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeaveGameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeaveGameLogic {
	return &LeaveGameLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *LeaveGameLogic) LeaveGame(in *login_proto.LeaveGameRequest) (*login_proto.LoginEmptyResponse, error) {
	resp := &login_proto.LoginEmptyResponse{}

	sessionDetails, ok := getSessionDetailsForAction(l.ctx, "leave game")
	if !ok {
		return resp, nil
	}

	cleanupLoginSessionState(l.ctx, l.svcCtx, sessionDetails.SessionId, "leave")

	// Remove player session from Redis (replaces Centre notification)
	deletePlayerSession(l.ctx, l.svcCtx, sessionDetails.PlayerId, sessionDetails.SessionId)

	return resp, nil
}
