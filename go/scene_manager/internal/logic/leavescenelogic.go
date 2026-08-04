package logic

import (
	"context"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LeaveSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeaveSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeaveSceneLogic {
	return &LeaveSceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// LeaveScene handles a player leaving a scene (or pre-transfer cleanup).
func (l *LeaveSceneLogic) LeaveScene(in *scene_manager.LeaveSceneRequest) (*base.Empty, error) {
	// 1. Check if player is actually in this scene (idempotency)
	currentLoc, err := GetPlayerLocation(l.ctx, l.svcCtx, in.PlayerId)
	if err != nil || currentLoc == nil {
		l.Logger.Infof("Player %d requested leave scene %d but has no location info", in.PlayerId, in.SceneId)
		return &base.Empty{}, nil
	}

	if currentLoc.SceneId != in.SceneId {
		l.Logger.Infof("Player %d requested leave scene %d but is in scene %d, ignoring",
			in.PlayerId, in.SceneId, currentLoc.SceneId)
		return &base.Empty{}, nil
	}

	// 2. Delete player location from Redis
	if err := DeletePlayerLocation(l.ctx, l.svcCtx, in.PlayerId); err != nil {
		l.Logger.Errorf("Failed to delete player %d location: %v", in.PlayerId, err)
		return &base.Empty{}, nil
	}

	// 3. Decrement instance player count.
	// For main world scenes the key is unused by lifecycle cleanup (harmless).
	DecrInstancePlayerCount(l.svcCtx, currentLoc.ZoneId, in.SceneId)

	l.Logger.Infof("Player %d left scene %d on node %s", in.PlayerId, in.SceneId, currentLoc.NodeId)
	return &base.Empty{}, nil
}
