package logic

import (
	"context"
	"strconv"
	"time"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// StartAgonesReconcile 周期性比对三方计数:
//
//	Agones GameServer.status.counters.rooms.count   <- Agones 认为的房间数
//	Redis node:zone:{zoneId}:{nodeId}:scene_count   <- SceneManager 认为的房间数
//	(节点上报的实际 Scene 数由 C++ 侧维护,已经反映在 scene_count 里)
//
// **第一版只告警,不自动改写任何一方。**
//
// 理由:三方任意一方都可能是错的那个。Agones 计数可能因为一次失败的回滚而
// 偏高,Redis 也可能因为一次没跑完的销毁而偏高。在证据不完整的情况下自动
// "修正",很可能把对的一方改成错的,还会掩盖真正的 bug。先让漂移可见,
// 定位到根因之后再谈自动收敛。
//
// ReconcileIntervalSeconds<=0 或未启用 Agones 时直接返回,不起 goroutine。
func StartAgonesReconcile(ctx context.Context, svcCtx *svc.ServiceContext) {
	interval := svcCtx.Config.Agones.ReconcileIntervalSeconds
	if interval <= 0 {
		logx.Info("[AgonesReconcile] disabled (ReconcileIntervalSeconds <= 0)")
		return
	}
	if !AgonesEnabled(svcCtx) {
		logx.Info("[AgonesReconcile] disabled (Agones not enabled)")
		return
	}

	logx.Infof("[AgonesReconcile] started, interval=%ds", interval)

	// safego.Loop:单轮 panic 只丢那一轮(比对逻辑要读 K8s API,反序列化出意外
	// 结构是现实风险),循环继续;点位名进 safego_panic_total。
	safego.Loop(ctx, SafePointAgonesReconcile, time.Duration(interval)*time.Second,
		func(ctx context.Context) {
			// 对账虽然只告警不改写,也收敛到领导者:避免多副本重复拉
			// Agones API / 重复告警,漂移 gauge 也只由一个实例发布。
			if !isLeader() {
				return
			}
			for _, zoneID := range GetActiveZones() {
				ReconcileAgonesRoomsForZone(ctx, svcCtx, zoneID)
			}
		})
}

// ReconcileAgonesRoomsForZone 跑一轮比对并写 scene_manager_agones_counter_drift。
// 导出是为了让单测能直接驱动一轮,不用等 ticker。
func ReconcileAgonesRoomsForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) int {
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return 0
	}

	zoneLabel := agonesZoneLabel(zoneID)
	servers, err := allocator.ListGameServerRooms(ctx, agonesNamespace(svcCtx), zoneLabel)
	if err != nil {
		logx.Errorf("[AgonesReconcile] zone %d: list gameservers failed: %v", zoneID, err)
		return 0
	}

	drift := 0
	for _, gs := range servers {
		// 只比对已经在承载房间的 GameServer。Ready 且 count==0 是正常空闲态。
		if gs.PodIP == "" {
			// 拿不到 Pod 地址就没法和本地节点对上号。这本身值得记一笔:
			// 要么 Agones 版本太老,要么 Pod 还没起来。
			if gs.Count > 0 {
				logx.Errorf("[AgonesReconcile] zone %d: gs=%s has rooms=%d but no pod address; cannot cross-check",
					zoneID, gs.Name, gs.Count)
				drift++
			}
			continue
		}

		nodeID, entryZone, _, ok := FindNodeByPodIP(gs.PodIP)
		if !ok {
			if gs.Count > 0 {
				// Agones 觉得这个进程上有房间,但 SceneManager 根本不认识它。
				// 典型来源:节点死亡后 GameServer 还没被回收,或者注册过期。
				logx.Errorf("[AgonesReconcile] zone %d: gs=%s pod=%s reports rooms=%d but is not a registered scene node",
					zoneID, gs.Name, gs.PodIP, gs.Count)
				drift++
			}
			continue
		}
		if entryZone != zoneID {
			continue
		}

		redisCount := readNodeSceneCount(svcCtx, zoneID, nodeID)
		if redisCount != gs.Count {
			logx.Errorf("[AgonesReconcile] DRIFT zone=%d gs=%s node=%s: agones_rooms=%d redis_scene_count=%d capacity=%d",
				zoneID, gs.Name, nodeID, gs.Count, redisCount, gs.Capacity)
			drift++
		}
	}

	metrics.SetAgonesCounterDrift(zoneLabel, drift)
	if drift == 0 {
		logx.Debugf("[AgonesReconcile] zone %d: %d gameservers, no drift", zoneID, len(servers))
	}
	return drift
}

// readNodeSceneCount 读 node:zone:{zoneId}:{nodeId}:scene_count。读不到当 0 处理 —— 对
// reconcile 来说"读不到"和"是 0"都会体现为与 Agones 计数的差异,
// 这正是我们想暴露的。
func readNodeSceneCount(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) int64 {
	s, err := svcCtx.Redis.Get(nodeSceneCountKey(zoneID, nodeID))
	if err != nil || s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
