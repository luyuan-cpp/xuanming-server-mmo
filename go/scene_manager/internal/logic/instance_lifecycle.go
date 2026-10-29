package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// StartInstanceLifecycleManager periodically scans active instances and
// destroys those that have been idle (0 players) longer than the configured timeout.
// Mirrors use MirrorIdleTimeoutSeconds (shorter) instead of the regular instance
// timeout, because every entry re-initializes NPCs — empty mirrors are pure waste.
func StartInstanceLifecycleManager(ctx context.Context, svcCtx *svc.ServiceContext) {
	instanceTimeout := svcCtx.Config.InstanceIdleTimeoutSeconds
	mirrorTimeout := resolveMirrorTimeout(svcCtx)

	if instanceTimeout <= 0 && mirrorTimeout <= 0 {
		logx.Info("[InstanceLifecycle] Both InstanceIdleTimeoutSeconds and MirrorIdleTimeoutSeconds=0, auto-destroy disabled")
		return
	}

	intervalSec := svcCtx.Config.InstanceCheckIntervalSeconds
	if intervalSec <= 0 {
		intervalSec = 30
	}
	interval := time.Duration(intervalSec) * time.Second

	logx.Infof("[InstanceLifecycle] Started: check every %ds, instance idle %ds, mirror idle %ds",
		intervalSec, instanceTimeout, mirrorTimeout)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 空闲副本销毁是变更动作,多副本时只有领导者执行,
			// 避免两个实例并发对同一实例走销毁链路(计数双减)。
			if !isLeader() {
				continue
			}
			cleanupIdleInstances(ctx, svcCtx, instanceTimeout, mirrorTimeout)
		}
	}
}

// resolveMirrorTimeout returns the effective idle timeout for mirror scenes.
// A 0 value falls back to InstanceIdleTimeoutSeconds so operators can opt out
// of the shorter schedule by leaving the new field unset.
func resolveMirrorTimeout(svcCtx *svc.ServiceContext) int64 {
	if t := svcCtx.Config.MirrorIdleTimeoutSeconds; t > 0 {
		return t
	}
	return svcCtx.Config.InstanceIdleTimeoutSeconds
}

// cleanupIdleInstances iterates over all known zones and destroys
// instances that have had 0 players for longer than the timeout.
func cleanupIdleInstances(ctx context.Context, svcCtx *svc.ServiceContext, instanceTimeout, mirrorTimeout int64) {
	zones := GetActiveZones()
	for _, zoneId := range zones {
		cleanupZoneIdleInstances(ctx, svcCtx, zoneId, instanceTimeout, mirrorTimeout)
	}
}

// cleanupZoneIdleInstances scans the active-instance sorted set for a single zone
// and destroys instances that have been idle longer than their per-type timeout.
// Mirror scenes (scene:{id}:mirror == "1") use mirrorTimeout; the rest use instanceTimeout.
// A non-positive timeout for a given kind means "never auto-destroy this kind".
func cleanupZoneIdleInstances(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, instanceTimeout, mirrorTimeout int64) {
	instKey := activeInstancesKey(zoneId)

	// Get all active instances with their creation timestamps.
	pairs, err := svcCtx.Redis.ZrangeWithScores(instKey, 0, -1)
	if err != nil {
		logx.Errorf("[InstanceLifecycle] Failed to list active instances: %v", err)
		return
	}

	if len(pairs) == 0 {
		return
	}

	now := time.Now().Unix()
	destroyed := 0

	for _, p := range pairs {
		// 降级即停:isLeader 只在 tick 入口查过一次,长循环里领导权可能
		// 中途易主;剩余条目由新领导者的下一拍接手,少清一轮无害。
		if !isLeader() {
			return
		}
		sceneId, err := strconv.ParseUint(p.Key, 10, 64)
		if err != nil {
			continue
		}

		// Check player count.
		playerCountStr, _ := svcCtx.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
		playerCount := 0
		if playerCountStr != "" {
			fmt.Sscanf(playerCountStr, "%d", &playerCount)
		}

		if playerCount > 0 {
			// Instance is active, update its "last active" time.
			// (Re-add with current timestamp to reset the idle clock.)
			svcCtx.Redis.Zadd(instKey, now, p.Key)
			continue
		}

		// Pick the right idle budget for this scene kind.
		isMirror := false
		if flag, _ := svcCtx.Redis.Get(sceneMirrorFlagKey(sceneId)); flag == "1" {
			isMirror = true
		}
		timeoutSec := instanceTimeout
		kind := "instance"
		if isMirror {
			timeoutSec = mirrorTimeout
			kind = "mirror"
		}

		if timeoutSec <= 0 {
			// Auto-destroy disabled for this kind.
			continue
		}

		// Player count is 0. Check how long it has been idle.
		lastActiveTime := int64(p.Score)
		idleDuration := now - lastActiveTime

		if idleDuration < timeoutSec {
			continue
		}

		// Idle timeout exceeded — destroy this instance.
		logx.Infof("[InstanceLifecycle] Destroying idle %s %d (idle %ds > timeout %ds)",
			kind, sceneId, idleDuration, timeoutSec)

		destroyInstance(ctx, svcCtx, zoneId, sceneId)
		destroyed++
	}

	if destroyed > 0 {
		logx.Infof("[InstanceLifecycle] Zone %d cleanup done: destroyed %d idle instances", zoneId, destroyed)
	}
}

// destroyInstance removes all Redis state for an instance and notifies
// the C++ scene node to destroy the ECS entity. Idempotent and
// concurrency-safe:
//   - Uses AtomicDestroyIfIdle (Lua CAS) to prevent the destroy-while-
//     entering race: if a player entered between the caller's "idle"
//     check and this call, the script leaves the scene untouched and we
//     return early without issuing the C++ DestroyScene RPC.
//   - Cascades into any mirrors whose source is this scene, reading
//     scene:{id}:mirrors only after the claim succeeds (the destroy scripts
//     never touch that set), so an aborted idle CAS leaves the mirrors alone.
//   - Un-links this scene from node:zone:{zoneId}:{nodeId}:scenes and (if it is itself
//     a mirror) from scene:{sourceId}:mirrors.
//
// The force variants are destroyInstanceForce (no owner check) and
// destroyOrphanOfDeadNode (expected owner = the dead node); see
// destroyInstanceInternal for the shared contract.
func destroyInstance(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) {
	destroyInstanceInternal(ctx, svcCtx, zoneId, sceneId, false, "", "idle")
}

// destroyInstanceForce force-destroys a scene even if its player_count is
// still non-zero, whoever owns it now. Callers: the DestroyScene RPC
// (explicit, surfaces err to its caller), world_rebalance (source_migrated)
// and world_autoscale (source_scaled_in), which ignore the result. The
// mirror cascade does not come through here: destroyInstanceInternal
// recurses into itself with force=true, expectedOwner="", reason=cascade.
// Node-death reconciliation does NOT use it either: it must only destroy
// scenes still owned by the dead node, see destroyOrphanOfDeadNode.
func destroyInstanceForce(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, reason string) (bool, error) {
	if reason == "" {
		reason = "explicit"
	}
	return destroyInstanceInternal(ctx, svcCtx, zoneId, sceneId, true, "", reason)
}

// destroyOrphanOfDeadNode 是死节点收尾(reason=node_death)专用的 force 销毁:不看人数(持有这些玩家的
// C++ 进程已经没了,节点计数上的残留只能钳掉),但只有 scene:{id}:node 仍等于 deadNode 时才销毁。
//
// 期望属主必须在原子脚本里比:收尾读到「属主是死节点」之后、销毁之前,只要有写者经 reassignSceneNode
// 把实例改派到活节点并 CreateScene,不比对的收尾就会抹掉新属主的场景、向新节点发 DestroyScene、扣新节点的
// 计数,玩家刚进的场景被拆掉。2026-10-22 修复时这样的写者是 resolveScene 的死节点自愈;10-23 起它只重建
// 世界频道、不再改派实例,实例眼下没有这类写者,比对作为纵深防御保留(将来任何改派实例的写者都受它保护)。
//
// 返回 (true, nil):销毁执行了,副作用(级联、计数、Agones 归还)也已执行,且只由本次执行。
// 返回 (false, nil):属主已不是 deadNode(读完之后被改派,或映射已被别的路径删掉),一个键都没动。
// 映射不存在时不当作可认领:删映射的那条路径负责其余清理,这里再抹会与它双还 Agones 名额。
// 返回 (false, err):Redis 出错(结论未知),或 deadNode 为空(什么都没做);两种都已记日志。
// deadNode 为空时拒绝而不是照常销毁:空串在脚本里表示「不比对」,会退化成修复前的无条件销毁。
func destroyOrphanOfDeadNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, deadNode string) (bool, error) {
	if deadNode == "" {
		err := fmt.Errorf("destroyOrphanOfDeadNode: empty dead node for scene %d (zone %d)", sceneId, zoneId)
		logx.Errorf("[InstanceLifecycle] %v; refusing to force-destroy without an expected owner", err)
		return false, err
	}
	return destroyInstanceInternal(ctx, svcCtx, zoneId, sceneId, true, deadNode, "node_death")
}

// destroyInstanceInternal 返回本次是否真的执行了销毁(认领成功并跑完副作用)。false 且 err 为 nil 时
// 什么都没做:非 force 时场景里来了人;force 时场景已被别的进程抹掉,或属主已不是 expectedOwner。
//
// force=false:luaAtomicDestroyInstance 按人数 CAS 认领,有人就放弃(空闲回收,经 destroyInstance)。
// force=true:luaAtomicDestroyInstanceForce 不看人数。expectedOwner 为空时不比对属主(explicit /
// source_migrated / source_scaled_in 经 destroyInstanceForce;cascade 由本函数的级联段直接递归);非空时
// 只抹属主仍为它的场景(node_death,经 destroyOrphanOfDeadNode)。expectedOwner 只对 force 生效。
//
// reason ∈ idle | explicit | cascade | node_death | source_migrated | source_scaled_in,作为
// scene_manager_instance_destroyed_total 的 label。
//
// 包外的新调用方经上面三个包装进来,不要直接传 force / expectedOwner(两个相邻的 string 参数写反也能编过);
// 本函数内唯一的直接递归是镜像级联。
func destroyInstanceInternal(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, force bool, expectedOwner, reason string) (bool, error) {
	sceneIdStr := fmt.Sprintf("%d", sceneId)

	// Snapshot everything we need BEFORE the atomic wipe:
	//   - nodeId so we can notify C++ + fix counters
	//   - residual player count so we can drain the per-node aggregate
	//   - mirror source (if any) so we can SREM from scene:{src}:mirrors
	//   - list of mirrors whose source is this scene, so we cascade them
	//   - whether we were a mirror (for the Prometheus kind label)
	sceneNodeKey := fmt.Sprintf("scene:%d:node", sceneId)
	nodeId, _ := svcCtx.Redis.Get(sceneNodeKey)
	// Agones 模式:这个 Scene 占用的是哪个 GameServer 的房间名额。
	// 非 Agones 模式下这个键不存在,后续归还是 no-op。
	agonesGs, _ := svcCtx.Redis.Get(sceneAgonesGsKey(sceneId))
	residualStr, _ := svcCtx.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	residual, _ := strconv.ParseInt(residualStr, 10, 64)
	sourceStr, _ := svcCtx.Redis.Get(sceneSourceKey(sceneId))
	sourceId, _ := strconv.ParseUint(sourceStr, 10, 64)
	mirrorFlag, _ := svcCtx.Redis.Get(sceneMirrorFlagKey(sceneId))
	kind := "instance"
	if mirrorFlag == "1" {
		kind = "mirror"
	}

	// Core atomic destroy — unless force=true, aborts if the scene picked
	// up a player in the meantime.
	destroyed := true
	if !force {
		atomicNode, atomicGs, err := AtomicDestroyIfIdle(svcCtx, zoneId, sceneId)
		if err != nil {
			logx.Errorf("[InstanceLifecycle] AtomicDestroy failed for scene %d: %v", sceneId, err)
			return false, err
		}
		if atomicNode == "" {
			// Scene picked up a player between our check and the script;
			// reseed its idle clock so next tick re-evaluates fairly.
			svcCtx.Redis.Zadd(activeInstancesKey(zoneId), time.Now().Unix(), sceneIdStr)
			destroyed = false
		} else {
			nodeId = atomicNode
			// 脚本读到的值才是权威:上面那次快照读和脚本之间可能被别的
			// 路径改过。
			if atomicGs != "" {
				agonesGs = atomicGs
			}
		}
	} else {
		// Force path:原来是"快照读 + 七连 DEL",快照与删除之间没有原子性,
		// 两个并发的 force 销毁(空闲清理 vs 死节点 reconcile,或领导者降级
		// 窗口里新旧领导者清理同一死节点)会各自拿到非空快照,把节点
		// scene_count 双减、Agones 名额双还。改成 Lua 原子认领:读与删同
		// 脚本,只有赢家拿到非空返回并执行副作用,输家静默退出。
		atomicNode, atomicGs, atomicResidual, err := AtomicDestroyForce(svcCtx, zoneId, sceneId, expectedOwner)
		if err != nil {
			logx.Errorf("[InstanceLifecycle] AtomicDestroyForce failed for scene %d: %v", sceneId, err)
			return false, err
		}
		if atomicNode == "" && atomicGs == "" {
			// 别的进程已抢先抹掉并负责了级联/计数/归还 —— 本次是重复销毁;
			// 或者带了期望属主而属主已不是它 —— 场景归别人了,一个键都没动。
			return false, nil
		}
		nodeId = atomicNode
		agonesGs = atomicGs
		residual = atomicResidual
	}

	if !destroyed {
		return false, nil
	}

	// Cascade destroy: any mirror whose source is this scene has to die with
	// its source.
	//
	// 这一段原来放在 AtomicDestroyIfIdle **之前**,理由写的是"原子脚本会把
	// scene:{id}:mirrors 抹掉,所以必须先读"。这个前提是错的:脚本的 KEYS 里
	// 是 scene:{id}:mirror(是不是镜像的**标志位**),不是 scene:{id}:mirrors
	// (镜像**子集合**),见 scene_atomic.go:245-252。脚本从不碰子集合。
	//
	// 而放在前面的代价是实打实的:CAS 会在"检查到销毁之间有玩家进场"时放弃销毁
	// (destroyed=false 直接 return),但那时所有镜像已经被 force 销毁、
	// mirrors 索引也已经被 Del —— 源场景活下来了,它的镜像却全没了,而且
	// 因为索引也没了,再也没有任何路径能发现这些镜像曾经存在。镜像里的玩家
	// 就此成为孤儿。挪到确认销毁之后,放弃销毁时镜像原样保留。
	mirrorChildren, _ := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneId))
	for _, mid := range mirrorChildren {
		childId, err := strconv.ParseUint(mid, 10, 64)
		if err != nil || childId == sceneId {
			continue
		}
		// Mirrors usually live in the same zone as their source but read
		// scene:{child}:zone to be safe — cross-zone mirroring is uncommon
		// but we don't want cascade to silently skip ZREM from the wrong
		// active set.
		childZoneStr, _ := svcCtx.Redis.Get(sceneZoneKey(childId))
		childZone, _ := strconv.ParseUint(childZoneStr, 10, 32)
		if childZone == 0 {
			childZone = uint64(zoneId)
		}
		logx.Infof("[InstanceLifecycle] Cascade-destroying mirror %d (source %d)", childId, sceneId)
		// Force-destroy: the source is going away, "still has players"
		// doesn't rescue the mirror.
		destroyInstanceInternal(ctx, svcCtx, uint32(childZone), childId, true, "", "cascade")
	}
	// Drop the mirrors index for this scene now that its children are gone.
	svcCtx.Redis.Del(sceneMirrorsKey(sceneId))

	// Notify C++ node to destroy the ECS scene entity (skip if node is
	// already dead — the entity died with the process).
	if nodeId != "" && IsNodeAlive(svcCtx, zoneId, nodeId) {
		if err := RequestNodeDestroyScene(ctx, svcCtx, zoneId, nodeId, sceneId); err != nil {
			logx.Errorf("[InstanceLifecycle] Failed to call DestroyScene on node %s for scene %d: %v", nodeId, sceneId, err)
		}
	}

	// Unlink reverse indexes. Both SREMs are best-effort: the node-death
	// reconciliation loop double-checks scene:{id}:node before destroying
	// again, so a stale entry in node:zone:{zoneId}:{nodeId}:scenes is harmless.
	if nodeId != "" {
		svcCtx.Redis.Srem(nodeScenesKey(zoneId, nodeId), sceneIdStr)
	}
	if sourceId > 0 {
		svcCtx.Redis.Srem(sceneMirrorsKey(sourceId), sceneIdStr)
	}

	// Decrement node counters.
	if nodeId != "" {
		sceneCountKey := nodeSceneCountKey(zoneId, nodeId)
		svcCtx.Redis.Incrby(sceneCountKey, -1)

		if residual > 0 {
			playerCountKey := nodePlayerCountKey(zoneId, nodeId)
			newVal, err := svcCtx.Redis.Incrby(playerCountKey, -residual)
			if err == nil && newVal < 0 {
				svcCtx.Redis.Set(playerCountKey, "0")
			}
		}
	}

	// 归还 Agones 房间名额。
	//
	// 走到这里说明这次 destroy 真的执行了(destroyed==true),所以名额只会
	// 被还一次:重复 Destroy 的第二次调用在脚本里读不到 scene:{id}:node,
	// 拿不到 agonesGs,也就不会重复减。
	//
	// 节点已经死了的情况同样要还:GameServer 对象可能还在(Agones 还没判
	// Unhealthy),它身上的 rooms 计数不减就会一直占着容量。ReleaseRoom 对
	// "GameServer 已消失" 返回成功,所以这里无脑调用是安全的。
	if agonesGs != "" {
		ReleaseAgonesRoomForScene(ctx, svcCtx, sceneId, agonesGs, reason)
	}

	metrics.ObserveInstanceDestroyed(zoneId, kind, reason)
	return true, nil
}

// IncrInstancePlayerCount increments both the per-scene and per-node player
// counters. The per-node counter feeds into GetBestNode's composite load
// score so nodes with many concurrent players become less attractive even
// when they host few scenes.
//
// Call order matters: scene counter first, then per-node aggregate. The
// two writes are not atomic — a crash between them skews the per-node
// counter by at most one, which the zset score refresher smooths out.
func IncrInstancePlayerCount(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) {
	svcCtx.Redis.Incr(fmt.Sprintf(InstancePlayerCountKey, sceneId))
	if nodeId := lookupSceneNode(svcCtx, sceneId); nodeId != "" {
		svcCtx.Redis.Incr(nodePlayerCountKey(zoneId, nodeId))
	}
}

// DecrInstancePlayerCount mirrors IncrInstancePlayerCount. Both counters
// are clamped to 0 so a missed EnterScene or a stale LeaveScene can't drive
// them negative.
func DecrInstancePlayerCount(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) {
	key := fmt.Sprintf(InstancePlayerCountKey, sceneId)
	val, err := svcCtx.Redis.Incrby(key, -1)
	if err == nil && val < 0 {
		svcCtx.Redis.Set(key, "0")
	}

	if nodeId := lookupSceneNode(svcCtx, sceneId); nodeId != "" {
		nodeKey := nodePlayerCountKey(zoneId, nodeId)
		nodeVal, err := svcCtx.Redis.Incrby(nodeKey, -1)
		if err == nil && nodeVal < 0 {
			svcCtx.Redis.Set(nodeKey, "0")
		}
	}
}

// lookupSceneNode resolves the node currently hosting a scene, returning ""
// when the mapping is missing. Used by the player-count helpers; a missing
// mapping is a soft error (per-node aggregate skew of at most one event).
func lookupSceneNode(svcCtx *svc.ServiceContext, sceneId uint64) string {
	nodeId, _ := svcCtx.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sceneId))
	return nodeId
}
