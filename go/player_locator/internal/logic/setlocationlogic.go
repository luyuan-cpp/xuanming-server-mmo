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
// 它是历史遗留 RPC:对 player:location:{uid} 做**绕过整条 CAS 链**的裸写 ——
// 不校验会话存在性/session_id/session_version 中的任何一项,无 TTL,还把
// Online 强制写 true。与 MarkOffline/commitLeaseExpiry 的原子 DEL 存在复活
// 竞态:一条在途 SetLocation 在 DEL 之后落地,就把已登出的玩家复活成一条
// 永不过期的 Online=true 记录,没有任何 sweeper 会再碰它。
//
// 生产位置权威在 scene_manager 的 player:{id}:location(见 keys.go 注释),
// 生产入场链从不写这套 key,全仓也没有生产调用方 —— 但端点此前一直注册
// 可达,是个活的隐患。显式拒绝,防止未来有人经 etcd 发现后接上。
// 若真需要写位置,走 scene_manager 的 EnterScene/LeaveScene 闸口。
func (l *SetLocationLogic) SetLocation(in *pb.PlayerLocation) (*common.Empty, error) {
	l.Errorf("SetLocation rejected: deprecated legacy endpoint (uid=%d server=%s); "+
		"authoritative location is owned by scene_manager", in.GetUid(), in.GetServerId())
	return nil, fmt.Errorf("SetLocation is deprecated and disabled; use scene_manager EnterScene/LeaveScene")
}
