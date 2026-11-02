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

		// Check player count. 读失败当 0:下面的 AtomicDestroyIfIdle 在脚本里再判一次 SCARD。
		playerCount := readScenePlayerCount(svcCtx, sceneId)

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

// destroyInstanceForce force-destroys a scene even if it still has members,
// whoever owns it now. Callers: the DestroyScene RPC
// (explicit, surfaces err to its caller) and the mirror cascades —
// cascadeDestroyMirrors (reason=cascade when a source instance is destroyed,
// source_scaled_in when a world channel finishes draining) and
// world_rebalance's cascadeMirrorsOnSourceMigration (source_migrated). The
// cascades keep the source's scene:{id}:mirrors index whenever a child returns
// an error (2026-10-23): the child was not destroyed and must stay discoverable.
// Node-death reconciliation does NOT use it: it must only destroy
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
// 非 force 认领到一个映射已不在的残留时也回 true:active 集合里的条目摘掉了,agones_gs 还在就归还。
//
// force=false:luaAtomicDestroyInstance 按人数 CAS 认领,有人就放弃(空闲回收,经 destroyInstance)。
// force=true:luaAtomicDestroyInstanceForce 不看人数。expectedOwner 为空时不比对属主(explicit /
// cascade / source_migrated / source_scaled_in 经 destroyInstanceForce);非空时
// 只抹属主仍为它的场景(node_death,经 destroyOrphanOfDeadNode)。expectedOwner 只对 force 生效。
//
// reason ∈ idle | explicit | cascade | node_death | source_migrated | source_scaled_in,作为
// scene_manager_instance_destroyed_total 的 label。
//
// 调用方一律经上面三个包装进来,不要直接传 force / expectedOwner(两个相邻的 string 参数写反也能编过)。
// 镜像级联(cascadeDestroyMirrors)也经 destroyInstanceForce 递归进来。
func destroyInstanceInternal(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, force bool, expectedOwner, reason string) (bool, error) {
	sceneIdStr := fmt.Sprintf("%d", sceneId)

	// Snapshot everything we need BEFORE the atomic wipe:
	//   - nodeId so we can notify C++ + fix counters
	//   - mirror source (if any) so we can SREM from scene:{src}:mirrors
	//   - list of mirrors whose source is this scene, so we cascade them
	//   - whether we were a mirror (for the Prometheus kind label)
	sceneNodeKey := fmt.Sprintf("scene:%d:node", sceneId)
	nodeId, _ := svcCtx.Redis.Get(sceneNodeKey)
	// Agones 模式:这个 Scene 占用的是哪个 GameServer 的房间名额。
	// 非 Agones 模式下这个键不存在,后续归还是 no-op。
	agonesGs, _ := svcCtx.Redis.Get(sceneAgonesGsKey(sceneId))
	// force 销毁时场景里剩的成员,要从节点集合里摘掉。空闲回收走到销毁时 SCARD 已确认是 0。
	var residualMembers []string
	mirrorFlag, _ := svcCtx.Redis.Get(sceneMirrorFlagKey(sceneId))
	kind := "instance"
	if mirrorFlag == "1" {
		kind = "mirror"
	}
	// 上面三项读错都无害:nodeId 与 agonesGs 以下面原子脚本的返回值为准;mirrorFlag 只决定指标 label。
	// 下面两项是**提交前必须读到**的(2026-10-22/23):两个销毁脚本都会 DEL scene:{id}:source,
	// 而子集合 scene:{id}:mirrors 决定级联范围,提交之后才发现读不到就没有补救了。
	//   - 镜像源:读错时以前当成「不是镜像」,提交后就不知道该从哪个源的子集合里摘掉自己,源的
	//     scene:{src}:mirrors 永久挂着死 id(channelHasMirrors 恒真,那个频道永远缩不了容)。
	//     scene:{id}:source 只在建镜像时写一次、之后不变,所以提交前读到的就是脚本删掉的那个值。
	//   - 镜像子集合:提交后仍以重读为准,预读只在重读失败时充当快照(见 cascadeDestroyMirrors)。
	// 任一读不到就本轮不销毁、什么都不改,回 (false, err)。各调用方的后果:
	//   - 空闲扫描:下一拍重来。
	//   - 显式 DestroyScene:经 destroyInstanceForce 把错误回给 RPC 调用方重试。
	//   - 级联(cascadeDestroyMirrors / cascadeMirrorsOnSourceMigration):保留源的子集合索引,没销毁的镜像
	//     仍可被发现,之后由空闲销毁收走。
	//   - 死节点收尾:**不重试**。reconcileDeadNodeScenes 把失败成员留在节点的反向索引里,只在同一 node_id
	//     再次判死、或人数经 LeaveScene 扣到 0 并过空闲超时后由空闲销毁处理。有界重试是 docs/PROGRESS.md
	//     2026-10-23「死节点收尾与显式 DestroyScene 的销毁改为按认领时属主」条目登记的残余 R1。
	// 宁可让死节点上的副本多留一阵,也不在不知道镜像源 / 子集合时提交:那会留下再也找不到的悬空索引。
	// 预读都在原子脚本之前、只读不写,期望属主的比对语义不变。
	sourceStr, err := svcCtx.Redis.Get(sceneSourceKey(sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteInstanceDestroy, fmt.Sprintf("mirror source of scene %d", sceneId), err)
		return false, err
	}
	sourceId, _ := strconv.ParseUint(sourceStr, 10, 64)
	preMirrorChildren, err := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteInstanceDestroy, fmt.Sprintf("mirror set of scene %d", sceneId), err)
		return false, err
	}

	// Core atomic destroy — unless force=true, aborts if the scene picked
	// up a player in the meantime.
	if !force {
		claim, err := AtomicDestroyIfIdle(svcCtx, zoneId, sceneId)
		if err != nil {
			logx.Errorf("[InstanceLifecycle] AtomicDestroy failed for scene %d: %v", sceneId, err)
			return false, err
		}
		if !claim.Claimed {
			// 场景里来了人。脚本已经重置了空闲时钟,下一拍公平地重判。
			return false, nil
		}
		// 脚本读到的值才是权威:上面那次快照读和脚本之间可能被别的路径改过。
		nodeId = claim.NodeID
		agonesGs = claim.AgonesGs
	} else {
		// Force path:原来是"快照读 + 七连 DEL",快照与删除之间没有原子性,
		// 并发的销毁(空闲回收的非 force 脚本、explicit / 级联 / 死节点收尾的
		// force 脚本,或领导者降级窗口里的新旧领导者)会各自拿到非空快照,把节点
		// scene_count 双减、Agones 名额双还。改成 Lua 原子认领:读与删同
		// 脚本,只有第一个拿到非空返回并执行副作用,其余静默退出。
		atomicNode, atomicGs, atomicMembers, err := AtomicDestroyForce(svcCtx, zoneId, sceneId, expectedOwner)
		if err != nil {
			logx.Errorf("[InstanceLifecycle] AtomicDestroyForce failed for scene %d: %v", sceneId, err)
			return false, err
		}
		if atomicNode == "" {
			// 别的进程已抢先抹掉并负责了级联/计数/归还 —— 本次是重复销毁;
			// 或者带了期望属主而属主已不是它 —— 场景归别人了,一个键都没动。
			return false, nil
		}
		nodeId = atomicNode
		agonesGs = atomicGs
		residualMembers = atomicMembers
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
	cascadeDestroyMirrors(ctx, svcCtx, zoneId, sceneId, preMirrorChildren, "cascade")

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

		removeResidualSceneMembers(svcCtx, sceneId, zoneId, nodeId, residualMembers)
	}

	// 归还 Agones 房间名额。
	//
	// 走到这里说明认领成功了,所以名额只会被还一次:agones_gs 和映射在认领脚本里
	// 一起删掉,之后的销毁拿不到它,回包丢失重发的只是同一份快照。
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

// cascadeDestroyMirrors 在源场景的销毁**已经提交**之后,强制销毁以它为源的全部镜像,再删掉它的子集合索引
// scene:{sceneId}:mirrors。reason 是子镜像销毁的指标 label(cascade / source_scaled_in)。
//
// 子集合以提交后的重读为准:预读与提交之间可能又有镜像挂上来。重读失败时(2026-10-22)销毁已经不能
// 撤回,于是按调用方提交前预读的快照 preRead 级联,并且**不删**索引 —— 预读之后才挂上的镜像只有这个
// 索引还记得。任一子镜像销毁回 error(读不到它的状态,本轮没销毁)时同样不删索引。
// 保留下来的子镜像各自空闲后由空闲销毁收走,销毁时会把自己从这个集合 SREM 掉
// (见 destroyInstanceInternal 末尾按镜像源 SREM),集合空了 Redis 自动回收键。
func cascadeDestroyMirrors(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, preRead []string, reason string) {
	children, err := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneId))
	keepIndex := err != nil
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteMirrorCascade,
			fmt.Sprintf("mirror set of destroyed scene %d (cascading the %d pre-read children instead, keeping the index)", sceneId, len(preRead)),
			err)
		children = preRead
	}
	for _, mid := range children {
		childId, parseErr := strconv.ParseUint(mid, 10, 64)
		if parseErr != nil || childId == sceneId {
			continue
		}
		// Mirrors usually live in the same zone as their source but read
		// scene:{child}:zone to be safe — cross-zone mirroring is uncommon
		// but we don't want cascade to silently skip ZREM from the wrong
		// active set. 读不到就不猜:按错的 zone 销毁会从别的 zone 的索引与计数里扣。
		childZoneStr, zoneErr := svcCtx.Redis.Get(sceneZoneKey(childId))
		if zoneErr != nil {
			noteReadUnknown(zoneId, readUnknownSiteMirrorCascade,
				fmt.Sprintf("zone of mirror %d (source %d), keeping it and the index", childId, sceneId), zoneErr)
			keepIndex = true
			continue
		}
		childZone, _ := strconv.ParseUint(childZoneStr, 10, 32)
		if childZone == 0 {
			childZone = uint64(zoneId)
		}
		logx.Infof("[InstanceLifecycle] Cascade-destroying mirror %d (source %d, reason %s)", childId, sceneId, reason)
		// Force-destroy: the source is going away, "still has players"
		// doesn't rescue the mirror.
		if _, destroyErr := destroyInstanceForce(ctx, svcCtx, uint32(childZone), childId, reason); destroyErr != nil {
			keepIndex = true
		}
	}
	// Drop the mirrors index for this scene now that its children are gone.
	// 子集合没读到、或有子镜像本轮没销毁时不删,理由见上。
	if !keepIndex {
		svcCtx.Redis.Del(sceneMirrorsKey(sceneId))
	}
}

// lookupSceneNode resolves the node currently hosting a scene, returning ""
// when the mapping is missing. Used by the member helpers; a missing
// mapping is a soft error (the per-node set keeps at most one stale member).
// 读错也回 "":这里只决定要不要顺带摘节点集合,不摘只是多留一个成员,由对账收掉(scene_member_reconcile.go)。
func lookupSceneNode(svcCtx *svc.ServiceContext, sceneId uint64) string {
	nodeId, _ := svcCtx.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sceneId))
	return nodeId
}
