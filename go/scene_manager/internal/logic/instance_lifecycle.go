package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

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

// cleanupIdleInstances destroys instances of this process's zone (orchestrated_zones.go) that
// have had 0 players for longer than the timeout.
func cleanupIdleInstances(ctx context.Context, svcCtx *svc.ServiceContext, instanceTimeout, mirrorTimeout int64) {
	zones := orchestratedZones(svcCtx)
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

		// Check player count. 读失败当 0:下面的 ClaimIdleSceneTeardown 在脚本里再判一次 SCARD。
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
//   - Uses ClaimIdleSceneTeardown (Lua CAS) to prevent the destroy-while-
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
// 返回 (true, nil):认领到了。场景状态已删、死节点的计数与反向索引已在同一步改好;级联、Agones 归还等其余副作用
// 由拆除任务保证做完,而且只做一次(scene_teardown_job.go)。
// 返回 (false, nil):属主已不是 deadNode(读完之后被改派,或映射已被别的路径删掉),一个键都没动。
// 映射不存在时不当作可认领:删映射的那条路径负责其余清理,这里再抹会与它双还 Agones 名额。
// 返回 (false, err):Redis 出错(结论未知),或 deadNode 为空(什么都没做);两种都已记日志。认领脚本其实已执行时,
// 拆除任务在待办里,由扫描做完副作用;再次收尾时认领落空,按 stale 计。
// deadNode 为空时拒绝而不是照常销毁:空串表示「谁是属主都拆」,会退化成修复前的无条件销毁。
func destroyOrphanOfDeadNode(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, deadNode string) (bool, error) {
	if deadNode == "" {
		err := fmt.Errorf("destroyOrphanOfDeadNode: empty dead node for scene %d (zone %d)", sceneId, zoneId)
		logx.Errorf("[InstanceLifecycle] %v; refusing to force-destroy without an expected owner", err)
		return false, err
	}
	return destroyInstanceInternal(ctx, svcCtx, zoneId, sceneId, true, deadNode, "node_death")
}

// destroyInstanceInternal 返回本次是否认领到了这次销毁。认领在脚本里一步提交:删场景的状态键、扣属主的 scene_count、
// 摘属主的反向索引、写拆除任务。之后的副作用由拆除任务保证做完(runInstanceTeardown;这次没做完的由扫描补,见
// scene_teardown_job.go)。所以认领到就回 true,不论副作用这一拍是否做完:销毁已经发生,调用方不该再重试。
// false 且 err 为 nil 时什么都没做:非 force 时场景里来了人,或属主在读与认领之间变了;force 时场景已被别的认领拿走,
// 或属主已不是 expectedOwner。非 force 认领到一个映射已不在的残留时也回 true:active 集合里的条目摘掉了,
// agones_gs 还在就归还。
//
// force=false:luaAtomicDestroyInstance,有人就放弃(空闲回收,经 destroyInstance)。
// force=true:luaAtomicDestroyInstanceForce 不看人数。expectedOwner 为空时谁是属主都拆(explicit / cascade /
// source_migrated / source_scaled_in 经 destroyInstanceForce);非空时只拆属主仍为它的场景(node_death,经
// destroyOrphanOfDeadNode)。expectedOwner 只对 force 生效。
//
// reason ∈ idle | explicit | cascade | node_death | source_migrated | source_scaled_in,作为
// scene_manager_instance_destroyed_total 与 Agones 归还的 reason。
//
// 调用方一律经上面三个包装进来,不要直接传 force / expectedOwner(两个相邻的 string 参数写反也能编过)。
// 镜像级联(cascadeDestroyMirrors)也经 destroyInstanceForce 递归进来。
func destroyInstanceInternal(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, force bool, expectedOwner, reason string) (bool, error) {
	// 镜像子集合 scene:{id}:mirrors 是**提交前必须读到**的(2026-10-22/23):它决定级联范围,销毁脚本不碰它,
	// 提交之后才发现读不到就没有补救了。提交后仍以重读为准,预读只在重读失败时充当快照(见 cascadeDestroyMirrors)。
	// 读不到就本轮不销毁、什么都不改,回 (false, err)。各调用方的后果:
	//   - 空闲扫描:下一拍重来。
	//   - 显式 DestroyScene:经 destroyInstanceForce 把错误回给 RPC 调用方重试。
	//   - 级联(cascadeDestroyMirrors / cascadeMirrorsOnSourceMigration):保留源的子集合索引,没销毁的镜像
	//     仍可被发现,之后由空闲销毁收走。
	//   - 死节点收尾:reconcileDeadNodeScenes 把失败成员留在节点的反向索引与收尾范围里,不删死节点的计数,
	//     下一拍只重试它们;本进程满放弃期限(10 分钟)仍做不完才放弃,失败成员留在反向索引里
	//     (reentry_barrier.go「被屏障推迟的死节点收尾」的「有界重试」,2026-10-30)。
	// 预读在原子脚本之前、只读不写,期望属主的比对语义不变。
	//
	// 属主、agones_gs、成员、镜像源、镜像标记都不在这里预读,一律取认领应答:扫描接手一个没做完的任务时
	// 这些键早已删了,只有认领时的快照可用。脚本里的读与删同一步,不存在「提交了却不知道镜像源」的窗口。
	preMirrorChildren, err := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteInstanceDestroy, fmt.Sprintf("mirror set of scene %d", sceneId), err)
		return false, err
	}

	// Core atomic destroy — unless force=true, aborts if the scene picked
	// up a player in the meantime.
	//
	// force 路径原来是"快照读 + 七连 DEL",快照与删除之间没有原子性,并发的销毁(空闲回收的非 force 脚本、
	// explicit / 级联 / 死节点收尾的 force 脚本,或领导者降级窗口里的新旧领导者)会各自拿到非空快照,把节点
	// scene_count 双减、Agones 名额双还。改成 Lua 原子认领:读与删同脚本,只有第一个认领成功的执行副作用,
	// 其余静默退出。是否认领只看 Claimed:空闲回收清扫残留时认领成功而属主为空。
	var claim sceneClaim
	if !force {
		claim, err = ClaimIdleSceneTeardown(svcCtx, zoneId, sceneId)
		if err != nil {
			logx.Errorf("[InstanceLifecycle] AtomicDestroy failed for scene %d: %v", sceneId, err)
			return false, err
		}
	} else {
		claim, err = AtomicDestroyForce(svcCtx, zoneId, sceneId, expectedOwner, reason)
		if err != nil {
			logx.Errorf("[InstanceLifecycle] AtomicDestroyForce failed for scene %d: %v", sceneId, err)
			return false, err
		}
	}
	if claim.Foreign {
		// 场景登记在别的 zone:它的键只由那个 zone 的 scene_manager 读写,认领脚本一个键都没动。不该发生(各条销毁
		// 路径只从本 zone 的索引里找场景,显式 DestroyScene 在入口拒绝别的 zone),回错误让它显形。
		err := fmt.Errorf("%w: scene %d, destroy requested for zone %d", errSceneInOtherZone, sceneId, zoneId)
		logx.Errorf("[InstanceLifecycle] %v; nothing changed", err)
		return false, err
	}
	if !claim.Claimed {
		// 空闲回收:场景里来了人(脚本已经重置了空闲时钟),或属主在读与认领之间变了,下一拍公平地重判。
		// force:别的认领已经拿走了它(级联 / 计数 / 归还归那次),或者带了期望属主而属主已不是它 —— 场景归别人了,
		// 一个键都没动。
		return false, nil
	}

	// 认领已提交,剩下的副作用记在拆除任务里:这里当场做,没做完的(进程退出、某一步失败)由扫描补。
	job := sceneTeardownJob{ZoneID: zoneId, SceneID: sceneId, Kind: teardownKindInstance, Reason: reason, Claim: claim}
	completeSceneTeardown(ctx, svcCtx, job, preMirrorChildren, false)
	return true, nil
}

// runInstanceTeardown 是副本 / 镜像认领之后的副作用(拆除任务 teardownKindInstance),认领方与扫描共用,可以重跑:
// 镜像级联、C++ DestroyScene、从镜像源的子集合摘掉自己、摘剩余成员、还 Agones 名额。属主的 scene_count 与反向索引
// 已经在认领脚本里改好。preMirrorChildren 是认领前预读的镜像子集合,只在提交后重读失败时充当快照;扫描接手时为 nil。
//
// 属主为空:认领的是映射已经不在的残留(空闲回收清扫)。不发 C++ 销毁、不摘成员,其余照做。
//
// 回错误:有步骤这次没做成(级联没做完、C++ DestroyScene 发不出去、从源的子集合摘自己失败、拿归还名额这一步结局未知),
// 任务留给扫描重做;做成了的步骤重跑不会重复生效。
func runInstanceTeardown(ctx context.Context, svcCtx *svc.ServiceContext, job sceneTeardownJob, preMirrorChildren []string) error {
	zoneId, sceneId, claim := job.ZoneID, job.SceneID, job.Claim
	var deferred []string

	// Cascade destroy: any mirror whose source is this scene has to die with its source.
	//
	// 级联只在认领成功之后做:脚本从不碰子集合 scene:{id}:mirrors(KEYS 里的 scene:{id}:mirror 是「是不是镜像」的
	// 标志位),放在认领之前的话,空闲 CAS 放弃时镜像已经全被 force 销毁、索引也已删,源活下来了,镜像里的玩家成孤儿。
	if !cascadeDestroyMirrors(ctx, svcCtx, zoneId, sceneId, preMirrorChildren, "cascade") {
		deferred = append(deferred, "mirror cascade")
	}

	// Notify C++ node to destroy the ECS scene entity (skip if node is already dead — the entity died with the
	// process). 重发是安全的:C++ 找不到这个 scene_id 的实体只打一条 WARN。
	nodeId := claim.NodeID
	if nodeId != "" && IsNodeAlive(svcCtx, zoneId, nodeId) {
		if err := RequestNodeDestroyScene(ctx, svcCtx, zoneId, nodeId, sceneId); err != nil {
			logx.Errorf("[InstanceLifecycle] Failed to call DestroyScene on node %s for scene %d: %v", nodeId, sceneId, err)
			deferred = append(deferred, "DestroyScene on node "+nodeId)
		}
	}

	// 从镜像源的子集合摘掉自己。漏摘会让源的 channelHasMirrors 恒真,那个频道永远缩不了容,所以失败要重做。
	if err := unlinkFromSourceMirrors(svcCtx, zoneId, sceneId, claim.Source); err != nil {
		deferred = append(deferred, "unlink from the source's mirror set")
	}

	// 场景里剩的成员,从节点集合里摘掉;单个成员失败由成员对账收掉(scene_member_reconcile.go)。
	if nodeId != "" {
		removeResidualSceneMembers(svcCtx, sceneId, zoneId, nodeId, claim.Members)
	}

	// 归还 Agones 房间名额,一个任务至多一次(releaseTeardownRoom)。节点已经死了的情况同样要还:GameServer 对象可能
	// 还在(Agones 还没判 Unhealthy),它身上的 rooms 计数不减就会一直占着容量。ReleaseRoom 对 "GameServer 已消失"
	// 返回成功。
	if err := releaseTeardownRoom(ctx, svcCtx, job); err != nil {
		deferred = append(deferred, "take the Agones release step")
	}
	return teardownDeferred(deferred)
}

// cascadeDestroyMirrors 在源场景的销毁**已经提交**之后,强制销毁以它为源的全部镜像,再删掉它的子集合索引
// scene:{sceneId}:mirrors。reason 是子镜像销毁的指标 label(cascade / source_scaled_in)。返回是否全部做完、删掉了索引。
//
// 子集合以提交后的重读为准:预读与提交之间可能又有镜像挂上来。重读失败时(2026-10-22)销毁已经不能
// 撤回,于是按调用方提交前预读的快照 preRead 级联,并且**不删**索引 —— 预读之后才挂上的镜像只有这个
// 索引还记得。任一子镜像销毁回 error(读不到它的状态,本轮没销毁)时同样不删索引。
// 不删索引时回 false,调用方的拆除任务留给扫描重做:重做时重读子集合,只剩还没销毁的那些。子镜像在那之前空闲了
// 也会被空闲销毁收走,销毁时把自己从这个集合 SREM 掉,集合空了 Redis 自动回收键。
func cascadeDestroyMirrors(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, preRead []string, reason string) bool {
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
		// 只拆本 zone 的子镜像(docs/design/control-plane-zone-scoping-20261105.md §4.3:zone z 的控制域键只由 zone z 的
		// scene_manager 读写)。子镜像登记在别的 zone 时不拆:它的映射、active 集合、节点计数都是那个 zone 的键,由那个
		// zone 的空闲回收收走;这里的子集合是本 zone 的键,随下面的 DEL 一起删,不再列着它。新建镜像时源在别的 zone
		// 就不登记成子镜像(createInstance),这里给更早登记的数据兜底;认领脚本还会在同一步再比一次 zone。
		// 读不到它的 zone 就不猜:本轮不拆、保留索引,拆除任务留着重做。
		childZone, zoneErr := lookupSceneZone(svcCtx, childId)
		if zoneErr != nil {
			noteReadUnknown(zoneId, readUnknownSiteMirrorCascade,
				fmt.Sprintf("zone of mirror %d (source %d), keeping it and the index", childId, sceneId), zoneErr)
			keepIndex = true
			continue
		}
		if childZone != 0 && childZone != zoneId {
			logx.Infof("[InstanceLifecycle] Mirror %d of scene %d is registered in zone %d, not %d; leaving it to that zone",
				childId, sceneId, childZone, zoneId)
			continue
		}
		logx.Infof("[InstanceLifecycle] Cascade-destroying mirror %d (source %d, reason %s)", childId, sceneId, reason)
		// Force-destroy: the source is going away, "still has players"
		// doesn't rescue the mirror.
		if _, destroyErr := destroyInstanceForce(ctx, svcCtx, zoneId, childId, reason); destroyErr != nil {
			keepIndex = true
		}
	}
	// Drop the mirrors index for this scene now that its children are gone.
	// 子集合没读到、或有子镜像本轮没销毁时不删,理由见上。
	if keepIndex {
		return false
	}
	svcCtx.Redis.Del(sceneMirrorsKey(sceneId))
	return true
}

// lookupSceneNode resolves the node currently hosting a scene, returning ""
// when the mapping is missing. Used by the member helpers; a missing
// mapping is a soft error (the per-node set keeps at most one stale member).
// 读错也回 "":这里只决定要不要顺带摘节点集合,不摘只是多留一个成员,由对账收掉(scene_member_reconcile.go)。
func lookupSceneNode(svcCtx *svc.ServiceContext, sceneId uint64) string {
	nodeId, _ := svcCtx.Redis.Get(fmt.Sprintf(SceneNodeKeyFmt, sceneId))
	return nodeId
}
