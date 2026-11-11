package logic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// scene 节点计划排空在 scene_manager 一侧的视图与编排(docs/design/scene-node-planned-drain.md §4.2、§4.3、§4.6)。
//
// 数据来源只有一处:本副本的 etcd 镜像 knownNodes(load_reporter.go)里每台 scene 节点自报的 NodeInfo.admission
// (CLOSED = 排空中)与 NodeInfo.build(部署构建标识)。两者都只是选点提示,不参与任何提交判定:
//   - 提示过时最多让本副本白发一次 CreateScene,被 scene 的建场景准入拒掉后按建场景失败回滚;
//   - 或者让一名玩家被派到一台随后就排空的节点上,多搬一次。
//
// 排空中的节点仍然活着、仍是它名下场景的属主:IsNodeAlive 不看这里。

// nodeAdmissionView 是某个 zone 在某一刻的排空与构建快照。按值传递,只读。
type nodeAdmissionView struct {
	draining map[string]bool   // node_id -> 排空中
	build    map[string]string // node_id -> 构建标识(空串 = 没上报)
	// outgoing 是外出构建:本 zone 里至少有一台排空中节点的构建。滚动升级时被排空的永远是被换下的那一代,
	// 回滚时也一样,所以「避开外出构建」就是「优先选这次要换上的版本」,不需要知道哪个版本更新(§4.6)。
	// 空串(没上报构建)不算外出构建:没有依据就不偏好。
	outgoing map[string]bool
}

// snapshotNodeAdmission 从 etcd 镜像取 zoneID 的排空与构建快照。同一 (zone, node) 有多条注册(身份歧义,
// 选点本来就拒绝它)时任一条自报排空就按排空算,方向是「不往这里放」。
func snapshotNodeAdmission(zoneID uint32) nodeAdmissionView {
	view := nodeAdmissionView{
		draining: make(map[string]bool),
		build:    make(map[string]string),
		outgoing: make(map[string]bool),
	}
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	for _, entry := range knownNodes {
		if entry.reg.ZoneId != zoneID {
			continue
		}
		view.build[entry.nodeID] = entry.reg.Build
		if entry.reg.Draining {
			view.draining[entry.nodeID] = true
			if entry.reg.Build != "" {
				view.outgoing[entry.reg.Build] = true
			}
		}
	}
	return view
}

// isDraining 报告节点是否自报排空中。
func (v nodeAdmissionView) isDraining(nodeID string) bool {
	return v.draining[nodeID]
}

// isPreferred 报告节点是否是首选落点:没在排空,且构建不是外出构建。
func (v nodeAdmissionView) isPreferred(nodeID string) bool {
	if v.draining[nodeID] {
		return false
	}
	build := v.build[nodeID]
	return build == "" || !v.outgoing[build]
}

// preferredFirst 把首选落点排在前面,两组内部各自保持原顺序(调用方按负载升序传入时,结果仍是组内负载升序)。
// 不过滤任何节点:排空中的节点由调用方先去掉。
func (v nodeAdmissionView) preferredFirst(nodes []string) []string {
	if len(v.outgoing) == 0 {
		return nodes
	}
	ordered := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if v.isPreferred(n) {
			ordered = append(ordered, n)
		}
	}
	for _, n := range nodes {
		if !v.isPreferred(n) {
			ordered = append(ordered, n)
		}
	}
	return ordered
}

// placementRing 是迁频道、补频道按一致性哈希挑目标时用的节点集合:开放节点里的首选落点,一个都没有才退回全部开放节点。
// openNodes 必须已去掉排空中的节点(getNodesForPurpose 的返回值就是)。
// 滚动期间哈希环只含新构建,外出构建上没人的频道会被 rebalance 顺带迁走,以后排空它们时就没有人要搬(§4.6)。
func (v nodeAdmissionView) placementRing(openNodes []string) []string {
	if len(v.outgoing) == 0 {
		return openNodes
	}
	ring := make([]string, 0, len(openNodes))
	for _, n := range openNodes {
		if v.isPreferred(n) {
			ring = append(ring, n)
		}
	}
	if len(ring) == 0 {
		return openNodes
	}
	return ring
}

// ── 排空迁移协程(§4.3) ─────────────────────────────────────────────────────────────

// drainMigrationRetryInterval:一轮迁移一个都没成(目标节点满、CreateScene 失败、CAS 落空)时,隔多久再规划一次。
// 节点排空有时限(scene 的计划排空预算),所以不按 rebalance 的周期等;一秒一次对 Redis 只是一轮 SMEMBERS / GET。
const drainMigrationRetryInterval = time.Second

var (
	drainWorkersMu sync.Mutex
	// drainWorkers 记着正在跑的排空迁移协程,键 = drainWorkerKey(zone, node)。每个 (zone, node) 至多一条。
	drainWorkers = make(map[string]struct{})
)

func drainWorkerKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf("%d/%s", zoneID, nodeID)
}

// ensureDrainMigration 为排空中的节点启动频道迁移协程(已在跑就什么都不做)。只有领导者做:频道归属改写是变更动作,
// 跟随者当选时周期 rebalance 会补启动。调用方:watch 看到节点转为排空(handleWatchEvent)、周期 rebalance
// (RebalanceWorldChannelsForZone 规划出 node_draining 项时)。
func ensureDrainMigration(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) {
	if !isLeader() {
		return
	}
	confIDs := worldConfIds()
	if len(confIDs) == 0 {
		return
	}
	key := drainWorkerKey(zoneID, nodeID)
	drainWorkersMu.Lock()
	if _, running := drainWorkers[key]; running {
		drainWorkersMu.Unlock()
		return
	}
	drainWorkers[key] = struct{}{}
	drainWorkersMu.Unlock()

	safego.Go(SafePointDrainMigration, func() {
		defer func() {
			drainWorkersMu.Lock()
			delete(drainWorkers, key)
			drainWorkersMu.Unlock()
		}()
		runDrainMigration(ctx, svcCtx, zoneID, nodeID, confIDs)
	})
}

// ensureDrainMigrationsForZone 为 zone 里每台活着的排空中节点确保有一条迁移协程。周期 rebalance 调它兜底:
// watch 事件漏了、本副本刚当选、协程意外退出。协程发现名下已没有世界频道就立刻退出,代价是一轮规划读。
func ensureDrainMigrationsForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) {
	if !isLeader() {
		return
	}
	for nodeID := range snapshotNodeAdmission(zoneID).draining {
		if IsNodeAlive(svcCtx, zoneID, nodeID) {
			ensureDrainMigration(ctx, svcCtx, zoneID, nodeID)
		}
	}
}

// runDrainMigration 反复规划并逐个迁走 nodeID 名下的世界频道,直到:名下没有世界频道、节点不再排空或已不在负载集
// (死了,改由死节点收尾)、本副本不再是领导者、ctx 结束。
//
// 迁移就是 rebalance 的 migrateWorldChannel(reason = node_draining):先占名额、分配放置代次、目标上 CreateScene、
// CAS 改归属、老节点 DestroyScene(带代次)。老节点收到 DestroyScene 后疏散住户(BeginSceneDrain),住户带着原频道号
// 发疏散请求,解析到新属主(enterscenelogic.go resolveEvacuationTarget)。
// 不占 MaxRebalanceMigrationsPerTick:下线有时限,对后端的保护在 scene 侧按玩家限速,不在频道粒度。
func runDrainMigration(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, nodeID string, confIDs []uint64) {
	logx.Infof("[Drain] zone=%d node=%s: node is draining; migrating its world channels", zoneID, nodeID)
	for {
		if ctx.Err() != nil {
			return
		}
		if !isLeader() {
			logx.Infof("[Drain] zone=%d node=%s: lost leadership; the next leader resumes the migration", zoneID, nodeID)
			return
		}
		if !snapshotNodeAdmission(zoneID).isDraining(nodeID) || !IsNodeAlive(svcCtx, zoneID, nodeID) {
			logx.Infof("[Drain] zone=%d node=%s: no longer a live draining node; stopping migration", zoneID, nodeID)
			return
		}
		plan := planDrainMigrations(svcCtx, zoneID, nodeID, confIDs)
		if len(plan) == 0 {
			logx.Infof("[Drain] zone=%d node=%s: no world channel left on the node", zoneID, nodeID)
			return
		}
		migrated := 0
		for _, c := range plan {
			if ctx.Err() != nil || !isLeader() {
				return
			}
			if migrateWorldChannel(ctx, svcCtx, zoneID, c.ConfId, c.SceneId, c.OldNode, c.NewNode, c.Reason) {
				migrated++
				metrics.ObserveRebalanceMigration(zoneID, string(c.Reason), "migrated")
			} else {
				metrics.ObserveRebalanceMigration(zoneID, string(c.Reason), "failed")
			}
		}
		logx.Infof("[Drain] zone=%d node=%s: planned=%d migrated=%d", zoneID, nodeID, len(plan), migrated)
		if migrated == len(plan) {
			continue // 这一轮全部成功:立刻重新规划,确认名下已空(期间可能又补建了频道)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(drainMigrationRetryInterval):
		}
	}
}

// planDrainMigrations 返回 nodeID 名下需要迁走的世界频道(reason = node_draining)。与 rebalance 共用规划器,
// 但不受 MaxRebalanceMigrationsPerTick 约束,也不受它为 0(关闭 rebalance)影响:计划排空是运维动作,不是负载优化。
func planDrainMigrations(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string, confIDs []uint64) []channelMigration {
	urgent, _ := planChannelMoves(svcCtx, zoneID, confIDs)
	var plan []channelMigration
	for _, c := range urgent {
		if c.Reason == reasonNodeDraining && c.OldNode == nodeID {
			plan = append(plan, c)
		}
	}
	return plan
}
