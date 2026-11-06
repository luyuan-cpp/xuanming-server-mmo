// capacity.go — per-zone admission ceiling computation.
//
// Two questions this file answers:
//
//  1. How many players can zone Z host concurrently? (zoneCapacity)
//  2. How many free slots does zone Z have RIGHT NOW? (freeSlots)
//
// The capacity number is config-first, with a fallback derived from the
// gates' observed PlayerCount * SoftCapMultiplier. Operators should pin a
// real number in YAML for production; the multiplier exists so a freshly
// deployed zone (PlayerCount=0 across all gates) doesn't permanently lock
// new players out of the queue.
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
	"strconv"
)

// CapacityProvider exposes the inputs capacity calculation needs. The
// concrete implementation in svc wires GateWatcher + ZoneCapacityOverride
// + the queue's in-flight placeholder count.
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
	// ZoneCapacity is the configured ceiling for zoneID, or 0 when
	// unconfigured (caller falls back to softCap formula).
	ZoneCapacity(zoneID uint32) uint32
}

// FreeSlots reports how many additional players the zone can accept right
// now. May return 0 when the answer is "queue must hold them".
//
// The formula is:
//
//	online    = sum(candidate.PlayerCount for candidate in zone)
//	cap       = ZoneCapacity(zone) || ceil(max(online, 1) * softCapMultiplier)
//	admitted  = live members of admitted:zset:zone:{zone} (expired ones dropped first)
//	free      = max(0, cap - online - admitted)
//
// We use max(online, 1) in the fallback so a brand-new zone (online=0)
// gets a non-zero capacity from the multiplier. Pinning ZoneCapacity in
// config remains the strongly recommended path for production.
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

	capacity = provider.ZoneCapacity(zoneID)
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

// ZoneCapacityFromMap is a small helper for the typical config layout
// (`map[string]uint32` keyed by zone_id). Returns 0 when missing.
func ZoneCapacityFromMap(m map[string]uint32, zoneID uint32) uint32 {
	if m == nil {
		return 0
	}
	if v, ok := m[strconv.FormatUint(uint64(zoneID), 10)]; ok {
		return v
	}
	return 0
}
