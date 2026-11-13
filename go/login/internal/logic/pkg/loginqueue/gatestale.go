// gatestale.go — 把本 login 判为停写的 gate 写成 Redis 标记,给没有连续观察历史的读者用(scene_manager 跨 zone 重定向)。
//
// login 的 gate 目录镜像对本 zone 的 gate 做 list-watch,按 ModRevision 与本进程的单调时钟判停写(node.WatchedNode.Stale)。
// scene_manager 跨 zone 重定向每次直读目标 zone 的 etcd,本进程没有那个 zone 的历史时判不出停写(gatepick.RevisionTracker
// 要两次读),多副本还各自从头学:目标 zone 的 gate 刚卡住时,落到没有历史的副本上的那次传送会把票签给卡住的 gate。
// 做法同 Kubernetes 节点控制器:观察者用自己的时钟判,消费者只读结果,不比较两台机器的墙钟。
//
// 写法:每个 login 副本每个上报周期给判为停写的 gate 写一次 gatepick.StaleKey(gate:{gm:<zone>}:<id>:stale),过期
// gatepick.StaleMarkTTL,从不主动删 ——
// gate 恢复之后不再续写,标记至多 StaleMarkTTL 后自然过期。多副本各写各的,写的是同一个值;任一副本判为停写就有标记,
// 误判的上界与本进程挑 gate 相同(健康 gate 的停写时长至多约 25s,小于 StaleAfter)。正常时一台都不停写,一个字节都不写。
// 设计见 docs/design/gate-load-balancing-design.md「满载与停写的 gate」。
package loginqueue

import (
	"context"

	"shared/gatepick"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/metric"
)

// 停写标记写入的结局,用作 login_gate_pick_stale_marks_total 的 outcome 标签(封闭集合)。
const (
	staleMarksWritten = "written" // 按台计:这一拍写成的标记数
	staleMarksFailed  = "error"   // 按次计:这一拍的写入失败了
)

var gateStaleMarks = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "gate_pick",
	Name:      "stale_marks_total",
	Help: "Stale-gate marks (gate:{gm:<zone>}:<id>:stale) this login published for readers without their own history " +
		"(scene_manager cross-zone redirects), by outcome: 'written' counts marks written; 'error' counts publish " +
		"rounds that failed. Zero while every gate of the zone is healthy.",
	Labels: []string{"outcome"},
})

// PublishStaleMarks 给 zoneID 里 nodeIDs 的每台 gate 写停写标记(一次 pipeline,每条 SET 带 gatepick.StaleMarkTTL)。
// pipeline 里只有 SET:它是幂等的,集群客户端在连接出错后整批重发也只是把同一个值再写一遍(非幂等命令不许进 pipeline,
// 见 clusterredis.ErrNonIdempotentInPipeline)。nodeIDs 为空或 rdb 为 nil 时什么都不做。写失败返回错误,由调用方记日志;它不影响本进程挑 gate(挑 gate 读的是自己的镜像),
// 读者读不到标记时退回只靠自己的历史,与没有这条标记时一样。
func PublishStaleMarks(ctx context.Context, rdb redis.UniversalClient, zoneID uint32, nodeIDs []uint32) error {
	if rdb == nil || len(nodeIDs) == 0 {
		return nil
	}
	pipe := rdb.Pipeline()
	for _, id := range nodeIDs {
		pipe.Set(ctx, gatepick.StaleKey(zoneID, id), "1", gatepick.StaleMarkTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		gateStaleMarks.Inc(staleMarksFailed)
		return err
	}
	gateStaleMarks.Add(float64(len(nodeIDs)), staleMarksWritten)
	return nil
}
