package logic

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"

	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

type SetLocationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetLocationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetLocationLogic {
	return &SetLocationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SetLocation 已停用(fail-closed)。
//
// 它是历史遗留 RPC:写的是一把生产入场链从不写的位置键,**绕过整条 CAS 链**,
// 不校验会话存在性/session_id/session_version 中的任何一项。那把键已删除,读者也已删除
// (docs/design/redis-player-store-cluster.md §5.2),协议里的方法留到下一次 proto 生成时一起删。
//
// 生产位置权威在 scene_manager 的 player:{<id>}:location。显式拒绝,防止未来有人经 etcd 发现后接上。
// 若真需要写位置,走 scene_manager 的 EnterScene/LeaveScene 闸口。
func (l *SetLocationLogic) SetLocation(in *pb.PlayerLocation) (*common.Empty, error) {
	l.Errorf("SetLocation rejected: deprecated legacy endpoint (uid=%d server=%s); "+
		"authoritative location is owned by scene_manager", in.GetUid(), in.GetServerId())
	return nil, fmt.Errorf("SetLocation is deprecated and disabled; use scene_manager EnterScene/LeaveScene")
}
