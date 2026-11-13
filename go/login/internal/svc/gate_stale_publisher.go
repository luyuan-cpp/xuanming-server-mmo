package svc

// gate_stale_publisher.go — 停写标记的发布循环:每个上报周期读一次 gate 目录镜像,把判为停写的本 zone gate 写成
// gatepick.StaleKey(loginqueue.PublishStaleMarks),给 scene_manager 跨 zone 重定向读(它每次直读目标 zone 的 etcd,
// 没有连续观察的历史,第一次读时判不出停写)。为什么由 login 发布、为什么不比较墙钟,见 loginqueue/gatestale.go。

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/node"

	"shared/gatepick"
	"shared/safego"
)

// gateStalePublishErrorLogEvery:发布失败日志的最小间隔。每个副本每 ReportPeriod 发布一次,Redis 不可用时不节流就是每 10s 一条。
const gateStalePublishErrorLogEvery = 30 * time.Second

// startGateStalePublisher 起停写标记的发布循环。GateWatcher 按本 login 的 zone 前缀建,只发布本 zone 的 gate,
// 标记键里的 zone 就是本 login 的 Node.ZoneId(读者 scene_manager 按目标 zone 拼同一组键)。
// 读镜像(NodeWatcher.Nodes)而不是直读:停写只有镜像判得出;镜像不新鲜时 Nodes 回落直读 etcd,那一拍 Stale 恒为 false,
// 什么都不写,读者退回只靠自己的历史,等于放行。每个 login 副本各跑一份:写的是同一个值,不需要选主。
func (s *ServiceContext) startGateStalePublisher() {
	if s.GateWatcher == nil || s.ControlRedis == nil {
		logx.Errorf("[GateStale] publisher NOT started (gate watcher present=%v, redis present=%v): "+
			"cross-zone redirects into this zone can only judge stale gates from their own read history",
			s.GateWatcher != nil, s.ControlRedis != nil)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.gateStaleStop = cancel
	nodes, rdb, zoneID := s.GateWatcher.Nodes, s.ControlRedis, config.AppConfig.Node.ZoneId
	s.background.Add(1)
	safego.Go("login.gate_stale_publisher", func() {
		defer s.background.Done()
		ticker := time.NewTicker(gatepick.ReportPeriod)
		defer ticker.Stop()
		var lastErrorLog time.Time // 只有这个 goroutine 读写
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				safego.Run("login.gate_stale_publisher", func() {
					err := publishGateStaleMarks(ctx, nodes, rdb, zoneID)
					if err != nil && time.Since(lastErrorLog) >= gateStalePublishErrorLogEvery {
						lastErrorLog = time.Now()
						logx.Errorf("[GateStale] publishing stale gate marks failed: %v; cross-zone redirects into this zone "+
							"fall back to their own read history; further reports are suppressed for %s, "+
							"see login_gate_pick_stale_marks_total{outcome=\"error\"}", err, gateStalePublishErrorLogEvery)
					}
				})
			}
		}
	})
}

// publishGateStaleMarks 是发布循环一拍的全部逻辑。读节点以函数注入(生产传 NodeWatcher.Nodes),接线测试不依赖 etcd。
// 一拍的时限是一个上报周期:读镜像回落直读 etcd、或 Redis 卡住时,不把下一拍也拖进去。
func publishGateStaleMarks(ctx context.Context, nodes func(context.Context) ([]node.WatchedNode, error), rdb redis.UniversalClient, zoneID uint32) error {
	tickCtx, cancel := context.WithTimeout(ctx, gatepick.ReportPeriod)
	defer cancel()
	watched, err := nodes(tickCtx)
	if err != nil {
		return err
	}
	return loginqueue.PublishStaleMarks(tickCtx, rdb, zoneID, staleGateNodeIDs(watched))
}

// staleGateNodeIDs 取镜像判为停写的 gate 的 node_id,保持原顺序;没有时返回 nil。
func staleGateNodeIDs(watched []node.WatchedNode) []uint32 {
	var ids []uint32
	for _, w := range watched {
		if w.Stale && w.Info != nil {
			ids = append(ids, w.Info.NodeId)
		}
	}
	return ids
}
