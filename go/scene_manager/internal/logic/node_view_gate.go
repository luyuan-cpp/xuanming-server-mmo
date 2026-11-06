package logic

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/liveview"

	"github.com/zeromicro/go-zero/core/logx"
)

// 存活集合理性闸门(决策清单 B1-1,规则与阈值见 shared/liveview):fullSync 拿到 etcd 快照之后、换掉 knownNodes 之前过这道闸门。
//
// 为什么要:etcd 回档、重建或连错时 Range 照样成功,只是回空或残缺。照单全收的话,本副本的注册表镜像一下子少掉一批节点,
// 领导者随后的陈旧清扫把它们当成死节点处理:写 death_at、摘负载集,屏障之后强制销毁它们名下的实例场景,世界频道也被改派。
// C++ 节点失租后会马上重新拿租约、按原 node_id 重新注册(etcd_service.cpp OnKeepAliveResponse),这类残缺通常几秒内自愈。
//
// 怎么判:按 zone 比。基线 = 本副本旧镜像里该 zone 的节点 ∪(领导者)该 zone 负载集里的成员。负载集是领导者最近一次刷新时
// 认为活着的节点;进程刚启动、旧镜像为空时只有它能当基线,而陈旧清扫正是在这时动手。
//   - 整个快照一个节点都没有、基线不空:一直压住(节点一注册就不再是空集);
//   - 某个 zone 基线不少于 4 个、缺席超过一半:压住,缺席数连续 nodeViewHold 不变才放行,变了(有节点回来或又少了)从头计时。
//
// 压住 = fullSync 回错,外层每 3s 重试:knownNodes 不换、不清扫、不补频道、不 rebalance,本副本的镜像与 Redis 都停在压住之前。
// 代价是压住期间 watch 和每拍刷新都停,新注册的节点也看不到,所以 nodeViewHold 取 60s:够失租的节点重新注册,
// 又不让控制面停太久。压住超过 2 分钟由 SceneManagerNodeViewHeld 报,放行由 SceneManagerNodeViewReleased 报。
// 按 zone 选主(容量模型 A1)之后一个副本只编排本 zone,停的也只是本 zone。
//
// 只拦 fullSync:watch 的 DELETE 是 etcd 里真实发生的删除,照旧逐个按消失处理(handleWatchEvent)。
const nodeViewHold = 60 * time.Second

// nodeViewGate 是每个 zone 一份的闸门状态。只在 load reporter 的 goroutine 上用;锁只为单测里直接调 fullSync 的用例。
type nodeViewGate struct {
	mu    sync.Mutex
	zones map[uint32]*liveview.Gate
	now   func() time.Time
}

var sceneNodeViewGate = &nodeViewGate{zones: make(map[uint32]*liveview.Gate), now: time.Now}

// check 判断这份快照能不能用。baseline 与 seen 都按 zone 分、按 node_id 去重;snapshotEmpty 表示快照里一个能解析的注册都没有。
// 不能用时回错误,调用方不换 knownNodes、本轮 fullSync 作废。
func (g *nodeViewGate) check(baseline, seen map[uint32]map[string]struct{}, snapshotEmpty bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	var held []string
	for _, zoneID := range zoneKeysOf(baseline) {
		known := baseline[zoneID]
		missing := 0
		for nodeID := range known {
			if _, ok := seen[zoneID][nodeID]; !ok {
				missing++
			}
		}
		gate := g.zones[zoneID]
		if gate == nil {
			gate = liveview.NewGate(nodeViewHold)
			g.zones[zoneID] = gate
		}
		obs := liveview.Observation{Known: len(known), Missing: missing, Empty: snapshotEmpty}
		switch verdict := gate.Observe(obs, now); verdict {
		case liveview.HoldEmpty, liveview.HoldDrop:
			held = append(held, fmt.Sprintf("zone=%d %s known=%d missing=%d", zoneID, verdict, obs.Known, obs.Missing))
		case liveview.Release:
			metrics.ObserveNodeViewReleased()
			logx.Errorf("[LoadReporter] node view gate released for zone %d: the missing node count stayed unchanged for %v, "+
				"treating the missing nodes as gone from this full sync on: known=%d missing=%d", zoneID, nodeViewHold, obs.Known, obs.Missing)
		}
	}
	// 基线里已经没有的 zone(节点全部收尾完了)不再需要闸门状态。
	for zoneID := range g.zones {
		if _, ok := baseline[zoneID]; !ok {
			delete(g.zones, zoneID)
		}
	}
	metrics.SetNodeViewHolding(len(held) > 0)
	if len(held) > 0 {
		return fmt.Errorf("scene node view held by the liveness gate (empty snapshot, or more than half of a zone's known nodes "+
			"missing; a drop is released after the missing count stays unchanged for %v): %s", nodeViewHold, strings.Join(held, "; "))
	}
	return nil
}

// nodeViewBaseline 合并闸门的基线:旧镜像里的 (zone,node) ∪ 负载集成员(只有领导者传 loadSets)。
func nodeViewBaseline(prev map[uint32]map[string]struct{}, loadSets map[uint32][]string) map[uint32]map[string]struct{} {
	out := make(map[uint32]map[string]struct{}, len(prev)+len(loadSets))
	add := func(zoneID uint32, nodeID string) {
		if out[zoneID] == nil {
			out[zoneID] = make(map[string]struct{})
		}
		out[zoneID][nodeID] = struct{}{}
	}
	for zoneID, nodes := range prev {
		for nodeID := range nodes {
			add(zoneID, nodeID)
		}
	}
	for zoneID, members := range loadSets {
		for _, nodeID := range members {
			add(zoneID, nodeID)
		}
	}
	return out
}

// knownNodeIDsByZone 抄一份当前镜像里的 (zone,node),给闸门当基线。
func knownNodeIDsByZone() map[uint32]map[string]struct{} {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	out := make(map[uint32]map[string]struct{})
	for _, entry := range knownNodes {
		if out[entry.reg.ZoneId] == nil {
			out[entry.reg.ZoneId] = make(map[string]struct{})
		}
		out[entry.reg.ZoneId][entry.nodeID] = struct{}{}
	}
	return out
}

// nodeViewZones 是领导者读负载集基线、做陈旧清扫的 zone 范围,两处只经这里取。
//
// 恒为本进程编排的 zone(orchestrated_zones.go,docs/design/control-plane-zone-scoping-20261105.md §3.3、§4.3):按 zone 选主之后
// 一个副本只编排本 zone,本 zone 一个节点都没有时照样扫,最后一个节点在无领导窗口里死掉时留下的负载集条目、计数和孤儿实例
// 也收得掉。快照与旧镜像只会有本 zone,参数留着是为了让闸门调用处读起来与基线的来源对得上;原来并进来的 zone 登记集合
// scene_manager:zones 已删除:控制域按 zone 组分实例后同组别的 zone 也在同一个实例里,并进来会把它们的活节点当成陈旧成员摘掉。
func nodeViewZones(svcCtx *svc.ServiceContext, snapshotZones, prevZones []uint32) []uint32 {
	_, _ = snapshotZones, prevZones
	return orchestratedZones(svcCtx)
}

// readLoadSetBaseline 读 zones 里每个 zone 负载集的成员:闸门拿它当基线,陈旧清扫拿它当清扫范围,两者用同一份读数,
// 闸门放行的正是清扫要摘的那一份。某个 zone 读失败就不在返回值里:闸门对它只看旧镜像,清扫本轮跳过它
// (与以前 ZRANGE 失败 continue 同口径)。
func readLoadSetBaseline(svcCtx *svc.ServiceContext, zones []uint32) map[uint32][]string {
	out := make(map[uint32][]string, len(zones))
	for _, zoneID := range zones {
		pairs, err := svcCtx.Redis.ZrangeWithScores(nodeLoadKey(zoneID), 0, -1)
		if err != nil {
			logx.Errorf("[LoadReporter] read load set of zone %d failed; it is neither a liveness baseline nor swept this full sync: %v", zoneID, err)
			continue
		}
		members := make([]string, 0, len(pairs))
		for _, p := range pairs {
			members = append(members, p.Key)
		}
		out[zoneID] = members
	}
	return out
}

// zoneKeysOf 返回 map 的 zone 键,升序。
func zoneKeysOf[V any](m map[uint32]V) []uint32 {
	zones := make([]uint32, 0, len(m))
	for zoneID := range m {
		zones = append(zones, zoneID)
	}
	return unionZones(zones)
}
