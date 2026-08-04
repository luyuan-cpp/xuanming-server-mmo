package scenemanagerlogic

import (
	"context"

	"proto/scene_manager"
	"scene_manager/internal/logic"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateSceneLogic {
	return &CreateSceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Create a scene on a specific node (implemented by the Scene node)
func (l *CreateSceneLogic) CreateScene(in *scene_manager.CreateSceneRequest) (*scene_manager.CreateSceneResponse, error) {
	return logic.NewCreateSceneLogic(l.ctx, l.svcCtx).CreateScene(in)
}
