package logic

import (
	"context"
	"fmt"
	"time"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// explicitDestroyFollowUpTimeout 是显式 DestroyScene 认领之后那些副作用(镜像级联、C++ DestroyScene、
// Agones 归还)的预算。它们不挂在 RPC 请求的 ctx 上:调用方超时或取消后,让这次调用当场做完,而不是等拆除任务
// 的扫描在租期之后补(scene_teardown_job.go;补也补得上,只是慢)。预算留得比 zrpc 服务端超时(8s)宽,级联多个
// 镜像时每个子场景各要一次 dial + RPC + ReleaseRoom。
const explicitDestroyFollowUpTimeout = 10 * time.Second

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

// DestroyScene is the explicit RPC path operators / gameplay code use to
// force-destroy a scene. Unlike the lifecycle auto-destroy it does NOT
// respect the member count — an explicit DestroyScene means "tear it down now",
// whoever owns the scene at that moment.
//
// 经 destroyInstanceForce(reason=explicit)原子认领,谁是属主都拆(先读属主、按读到的值认领,读完被改派就重读,
// 见 AtomicDestroyForce):属主、Agones 名额、人数都以脚本认领时读到的为准,级联、反向索引、计数、名额归还与其它
// force 路径共用一份实现。2026-10-23 前这里是独立的
// 「快照读 + 逐条 DEL」实现,问题见 docs/PROGRESS.md 同日「死节点收尾与显式 DestroyScene 的销毁改为按
// 认领时属主」条目。
//
// 错误语义(调用方可以重试):
//   - 认领前读不到镜像子集合或属主:destroyInstanceInternal 什么都不改、回错误(子集合的读计入
//     ownership_read_unknown_total{site="instance_destroy"})。认领成功之后级联会重读子集合,那次读失败
//     时按认领前的快照级联并保留索引(cascadeDestroyMirrors),不回错误 —— 销毁已经发生,没做完的级联由拆除任务补。
//   - 认领脚本报错时结论未知:多半场景原样未动,重试会正常销毁;若脚本其实已执行、只是回包丢了,重试时认领落空、
//     按幂等成功返回,级联、C++ 销毁与 Agones 归还由拆除任务的扫描在租期之后做完(scene_teardown_job.go)。
//   - 映射已不存在时视为已被销毁(幂等成功):不再级联镜像,也不记 explicit 指标。
//   - 只拆本 zone 的场景(docs/design/control-plane-zone-scoping-20261105.md §3.3、§4.3):请求的 zone 不是本进程的
//     zone 时在任何读写之前拒绝,与 CreateScene 同一条路由不变量;zone_id 没填(0)按本进程的 zone。场景登记在别的
//     zone(请求填的 zone 与场景不符)时认领脚本一个键都不动,回错误。
func (l *DestroySceneLogic) DestroyScene(in *scene_manager.DestroySceneRequest) (*base.Empty, error) {
	zoneID := in.ZoneId
	if zoneID == 0 {
		zoneID = l.svcCtx.Config.ZoneId
	}
	if !ownsControlZone(l.svcCtx, zoneID) {
		err := fmt.Errorf("destroy scene %d: zone %d is served by its own scene_manager (this process serves zone %d)",
			in.SceneId, zoneID, l.svcCtx.Config.ZoneId)
		l.Logger.Errorf("[DestroyScene] %v; rejected without any read or write", err)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), explicitDestroyFollowUpTimeout)
	defer cancel()
	if _, err := destroyInstanceForce(ctx, l.svcCtx, zoneID, in.SceneId, "explicit"); err != nil {
		return nil, err
	}
	return &base.Empty{}, nil
}
