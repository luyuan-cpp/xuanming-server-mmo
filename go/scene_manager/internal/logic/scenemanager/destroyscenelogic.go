package scenemanagerlogic

import (
	"context"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/logic"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DestroySceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDestroySceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DestroySceneLogic {
	return &DestroySceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Destroy a scene
func (l *DestroySceneLogic) DestroyScene(in *scene_manager.DestroySceneRequest) (*base.Empty, error) {
	return logic.NewDestroySceneLogic(l.ctx, l.svcCtx).DestroyScene(in)
}
