package logic

import (
	"context"
	"time"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// explicitDestroyFollowUpTimeout 是显式 DestroyScene 认领之后那些副作用(镜像级联、C++ DestroyScene、
// Agones 归还)的总预算。它们不挂在 RPC 请求的 ctx 上:认领一旦提交,调用方超时或取消后再重试只会得到
// 「认领落空 = 已销毁」,不会补做,所以必须由本次调用做完。预算留得比 zrpc 服务端超时(8s)宽,级联多个
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
// respect player_count — an explicit DestroyScene means "tear it down now",
// whoever owns the scene at that moment.
//
// 经 destroyInstanceForce(reason=explicit)原子认领、不比对属主:属主、Agones 名额、人数都以脚本认领时
// 读到的为准,级联、反向索引、计数、名额归还与其它 force 路径共用一份实现。2026-10-23 前这里是独立的
// 「快照读 + 逐条 DEL」实现,问题见 docs/PROGRESS.md 同日「死节点收尾与显式 DestroyScene 的销毁改为按
// 认领时属主」条目。
//
// 错误语义(调用方可以重试):
//   - 认领前先 SMEMBERS 一次镜像子集合,只当探活:此刻读不到就什么都不改、回错误。它关不住全部窗口 ——
//     级联在认领成功之后另读一次子集合,那次读失败的处理在 destroyInstanceInternal 里。
//   - 认领脚本报错时结论未知:多半场景原样未动,重试会正常销毁;若脚本其实已执行、只是回包丢了,重试时
//     认领落空,按幂等成功返回,而级联、C++ 销毁、反向索引、计数与 Agones 归还都不会再执行。这与其它
//     force 路径是同一残余(同上 PROGRESS 条目 R5)。
//   - 映射与 agones_gs 都已不存在时视为已被销毁(幂等成功):不再级联镜像,也不记 explicit 指标。
func (l *DestroySceneLogic) DestroyScene(in *scene_manager.DestroySceneRequest) (*base.Empty, error) {
	if _, err := l.svcCtx.Redis.Smembers(sceneMirrorsKey(in.SceneId)); err != nil {
		l.Logger.Errorf("DestroyScene: reading mirrors of scene %d (zone %d) failed, nothing changed: %v", in.SceneId, in.ZoneId, err)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), explicitDestroyFollowUpTimeout)
	defer cancel()
	if _, err := destroyInstanceForce(ctx, l.svcCtx, in.ZoneId, in.SceneId, "explicit"); err != nil {
		return nil, err
	}
	return &base.Empty{}, nil
}
