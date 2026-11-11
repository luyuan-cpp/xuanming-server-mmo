// capacity.go — per-zone admission ceiling computation.
//
// Two questions this file answers:
//
//  1. How many players can zone Z host concurrently? (zoneCapacity)
//  2. How many free slots does zone Z have RIGHT NOW? (freeSlots)
//
// 区容量只有一个来源(2026-11-10 起):Java 网关的区服目录 zone_config.capacity,运维经 /admin/zones 改,
// 经 etcd 发布给本区 login(zonepolicy 包;CapacityProvider.ZoneCapacity 读它)。区服列表的负载档用的是同一个数。
// 此前 login 另有一份 Queue.ZoneCapacityOverride 配置,两处各写各的,已删。读不到区服策略时退回软上限
// (gate 上报的在线人数 × SoftCapMultiplier),免得 zone 刚部署、在线为 0 时把新玩家永远挡在队列里。
//
// 这个数再按本区 gate 的承载封顶:取它与「在接客的 gate 到水位(自报上限的 90%)一共能接的人数」
// (gatepick.AdmissionCapacity)中较小的那个。区容量与 gate 台数写在两处、没有机械关联(区服目录缺省 45000,
// 默认清单每区 3 台 × 20000,到水位 54000;只有 1 台时 18000),容量比 gate 承载大时,排队照样放人,玩家连上 gate 才被
// GateMaxConnections 无声拒掉。封顶之后 gate 到水位时排队就开始拦人;gate 坏了、被排空时承载随之下降,排队跟着收紧。
//
// Free-slots calculation deliberately includes in-flight placeholders
// (`admitted:zset:zone:{zoneId}`, see Queue.AdmittedCount):
//
//	freeSlots = capacity - online - admitted
//
// Without the `admitted` term, a single dispatcher tick would over-issue
// gate tokens until the next PlayerCount refresh propagates from
// gate → etcd → GateWatcher (the C++ gate reports every 10s).
package loginqueue

import (
	"context"
	"fmt"
	"time"

	"shared/gatepick"

	"github.com/zeromicro/go-zero/core/logx"
)

// capacityCapLogEvery:「配置的容量被 gate 承载压低」那条日志的最小间隔。这是一种持续的配置状态,快速通道每次 AssignGate、
// dispatcher 每拍都会算到它;持续多久看 login_queue_capacity 与 login_queue_configured_capacity,日志只负责给出两个数。
const capacityCapLogEvery = 10 * time.Minute

var capacityCapLog logThrottle

// CapacityProvider exposes the inputs capacity calculation needs. The
// concrete implementation in svc wires GateWatcher + the zone policy read
// from etcd (zone_config.capacity).
type CapacityProvider interface {
	// CandidatesForZone returns every gate serving zoneID (deduplicated by
	// client address), in arbitrary order. Gates marked draining stay in the
	// list with GateCandidate.Draining set (2026-11-06): FreeSlots counts
	// their players; PickGate / gatePicker tier them via eligibleGates.
	// 生产实现(svc 的 gateWatcherCapacityProvider)读 gate 目录的进程内 list-watch 镜像,镜像不新鲜时
	// 回落一次 etcd 前缀读(2026-10-18 起;此前每次都直读 etcd),再加一次排空标记 MGET:
	// dispatcher 每个非空拍只调一次;快速通道每次 AssignGate 调一到两次(FreeSlots、签票各一次);
	// 排队取走放行时每次调一次(NewAdmitSigner 复核放行时选定的 gate,2026-11-05 起)。
	CandidatesForZone(ctx context.Context, zoneID uint32) ([]GateCandidate, error)
	// ZoneCapacity is zone_config.capacity for zoneID, or 0 when the zone
	// policy is unknown (caller falls back to the softCap formula).
	ZoneCapacity(zoneID uint32) uint32
}

// FreeSlots reports how many additional players the zone can accept right
// now. May return 0 when the answer is "queue must hold them".
//
// The formula is:
//
//	online    = sum(candidate.PlayerCount for candidate in zone)
//	ceiling   = ZoneCapacity(zone) || floor(max(online, 1) * softCapMultiplier)
//	gateCap   = gatepick.AdmissionCapacity(candidates): 在接客的 gate 到水位一共能接的人数
//	cap       = min(ceiling, gateCap);数进来的 gate 里有没声明上限的(MaxConnections=0)时 cap = ceiling
//	admitted  = live members of admitted:zset:zone:{zone} (expired ones dropped first)
//	free      = max(0, cap - online - admitted)
//
// We use max(online, 1) in the fallback so a brand-new zone (online=0)
// gets a non-zero capacity from the multiplier. The fallback only applies
// while the zone policy cannot be read (see package zonepolicy).
//
// online 数全部候选(排空中、停写的 gate 上的人也占着连接),gateCap 只数在接客的那一档:排空、卡住的 gate 上的人
// 稍后要挪到在接客的 gate 上,两边一减正好给他们留出位置。返回的 capacity 是封顶之后的值,快速通道用它算占位预算。
func FreeSlots(
	ctx context.Context,
	provider CapacityProvider,
	queue *Queue,
	zoneID uint32,
	softCapMultiplier float64,
) (free uint32, capacity uint32, online uint32, admitted uint32, err error) {
	candidates, cErr := provider.CandidatesForZone(ctx, zoneID)
	if cErr != nil {
		err = fmt.Errorf("candidates: %w", cErr)
		return
	}
	return freeSlotsFromCandidates(ctx, candidates, provider, queue, zoneID, softCapMultiplier)
}

// freeSlotsFromCandidates 是 FreeSlots 去掉取候选的那一步:按调用方已经取到的候选算,公式与错误同 FreeSlots,
// provider 在这里只用来读 ZoneCapacity。dispatcher 每个非空拍只取一次候选,算容量与批内挑 gate 共用
// 这一份(见 dispatchOnce);快速通道仍走 FreeSlots。
func freeSlotsFromCandidates(
	ctx context.Context,
	candidates []GateCandidate,
	provider CapacityProvider,
	queue *Queue,
	zoneID uint32,
	softCapMultiplier float64,
) (free uint32, capacity uint32, online uint32, admitted uint32, err error) {
	if len(candidates) == 0 {
		// No gates → no capacity. Caller surfaces this as an "error" rather
		// than queueing, since the queue draining requires a healthy gate.
		return 0, 0, 0, 0, fmt.Errorf("no gates for zone %d", zoneID)
	}
	for _, c := range candidates {
		online += c.PlayerCount
	}

	configured := provider.ZoneCapacity(zoneID)
	capacity = configured
	if capacity == 0 {
		base := online
		if base == 0 {
			base = 1
		}
		mult := softCapMultiplier
		if mult < 1.0 {
			mult = 1.0
		}
		capacity = uint32(float64(base) * mult)
	}
	capacity = capByGates(capacity, configured, candidates, zoneID)
	capacityGauge.Set(float64(capacity), labelZone(zoneID))
	configuredCapacityGauge.Set(float64(configured), labelZone(zoneID))

	admitted, err = queue.AdmittedCount(ctx, zoneID)
	if err != nil {
		err = fmt.Errorf("admitted count: %w", err)
		return
	}

	used := online + admitted
	if capacity > used {
		free = capacity - used
	} else {
		free = 0
	}
	return
}

// capByGates 把区容量 ceiling 压到本区在接客的 gate 到水位一共能接的人数以内(gatepick.AdmissionCapacity),
// 数进来的 gate 里有没声明上限的就原样返回。configured 是区服目录给的容量(0 = 读不到区服策略、ceiling 来自软上限),
// 只在它被压低时打日志:软上限本来就随在线人数涨,被 gate 承载压住是常态,不是配置问题。
func capByGates(ceiling, configured uint32, candidates []GateCandidate, zoneID uint32) uint32 {
	gateCapacity, bounded := gatepick.AdmissionCapacity(candidates, candidateState)
	if !bounded || gateCapacity >= uint64(ceiling) {
		return ceiling
	}
	if configured > 0 && capacityCapLog.allow(capacityCapLogEvery) {
		logx.Errorf("[loginqueue] zone %d admission capacity is limited by its gates: configured=%d, the serving gates "+
			"hold %d at %d%% of their max_connections (%d gate candidates); the queue starts holding players at %d. "+
			"Add gate replicas or lower zone_config.capacity (PUT /admin/zones/<id>); compare login_queue_capacity with "+
			"login_queue_configured_capacity. Further reports are suppressed for %s",
			zoneID, configured, gateCapacity, gatepick.FullPercent, len(candidates), gateCapacity, capacityCapLogEvery)
	}
	return uint32(gateCapacity)
}
