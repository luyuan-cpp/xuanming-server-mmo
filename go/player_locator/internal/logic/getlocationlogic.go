package logic

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"player_locator/internal/svc"
	pb "proto/player_locator"
)

type GetLocationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetLocationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLocationLogic {
	return &GetLocationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetLocation 已停用(fail-closed)。它读的是历史遗留的玩家位置键,生产入场链从不写那把键,全仓也没有生产调用方;
// 键本身已删除(docs/design/redis-player-store-cluster.md §5.2)。协议里的方法留到下一次 proto 生成时一起删。
// 位置权威是 scene_manager 的 player:{<id>}:location。
func (l *GetLocationLogic) GetLocation(in *pb.PlayerId) (*pb.PlayerLocation, error) {
	l.Errorf("GetLocation rejected: deprecated legacy endpoint (uid=%d); "+
		"authoritative location is owned by scene_manager", in.GetUid())
	return nil, fmt.Errorf("GetLocation is deprecated and disabled; authoritative location is owned by scene_manager")
}
