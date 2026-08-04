package scenemanagerlogic

import (
	"context"

	"proto/scene_manager"
	"scene_manager/internal/logic"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type EnterSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewEnterSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EnterSceneLogic {
	return &EnterSceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Login/player_locator requests a player to enter a scene
func (l *EnterSceneLogic) EnterScene(in *scene_manager.EnterSceneRequest) (*scene_manager.EnterSceneResponse, error) {
	return logic.NewEnterSceneLogic(l.ctx, l.svcCtx).EnterScene(in)
}
