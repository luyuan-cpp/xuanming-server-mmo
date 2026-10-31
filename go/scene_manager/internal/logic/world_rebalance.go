package logic

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// rebalanceReason describes why a channel is being moved. Used only for logs
// so operators can distinguish "the old node went away" from "we just found
// a better home".
type rebalanceReason string

const (
	reasonNodeGone   rebalanceReason = "node_gone"   // old node is dead / no longer world-hosting
	reasonBetterHome rebalanceReason = "better_home" // opportunistic; old node still alive
)

// channelMigration describes one proposed channel move. Exposed for
// observability and for tests that want to assert the planner's output
// without spinning up a real gRPC scene-node fleet.
type channelMigration struct {
	ConfId  uint64
	SceneId uint64
	OldNode string
	NewNode string
	Reason  rebalanceReason
}

// RebalanceWorldChannelsForZone redistributes empty world channels after a
// world-hosting node joins or leaves the pool. It is safe to call repeatedly:
// each invocation recomputes the expected (confId, sceneId) -> node mapping
// from scratch, then moves channels that diverge from the expected mapping
// subject to two safety rules:
//
//  1. Never migrate a channel with online players while its old process is
//     still registered. Live migration would require cross-node state transfer
//     which this codebase does not have. A truly dead node may retain stale
//     members; that mapping is already unusable and is rebuilt urgently.
//  2. Respect MaxRebalanceMigrationsPerTick so a big scale event (say, +4
//     pods) does not trigger N*channels simultaneous CreateScene/DestroyScene
//     RPCs and knock the cluster over.
//
// Channels currently mapped to a DEAD or non-world-hosting node are always
// prioritised (those mappings are already broken for new players); empty
// channels on still-live nodes are only moved if the budget allows.
func RebalanceWorldChannelsForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confIds []uint64) {
	urgent, opportunistic, budget := PlanWorldChannelRebalance(svcCtx, zoneId, confIds)

	// Publish queue depth every invocation so dashboards see "queue is
	// stable at 0" when nothing changed, not just when migrations fire.
	metrics.SetRebalancePending(zoneId, string(reasonNodeGone), len(urgent))
	metrics.SetRebalancePending(zoneId, string(reasonBetterHome), len(opportunistic))

	if budget == 0 {
		return // explicitly disabled
	}
	if len(urgent) == 0 && len(opportunistic) == 0 {
		return
	}

	migrated, skipped := 0, 0
	run := func(list []channelMigration) {
		for _, c := range list {
			// 降级即停:降级窗口里新旧领导者会各按自己的计划迁移。归属改写
			// 有 CAS(reassignSceneNode),同一频道只有一边能落地,输的一边回收
			// 自己预建的实体、不动名额与计数;逐条重查仍能省掉那些白跑的
			// CreateScene / DestroyScene。剩余的由新领导者重新规划。
			if !isLeader() {
				logx.Infof("[Rebalance] zone=%d: lost leadership mid-run, aborting; successor will re-plan", zoneId)
				return
			}
			if migrated >= budget {
				return
			}
			if migrateWorldChannel(ctx, svcCtx, zoneId, c.ConfId, c.SceneId, c.OldNode, c.NewNode, c.Reason) {
				migrated++
				metrics.ObserveRebalanceMigration(zoneId, string(c.Reason), "migrated")
			} else {
				skipped++
				metrics.ObserveRebalanceMigration(zoneId, string(c.Reason), "failed")
			}
		}
	}
	run(urgent)
	run(opportunistic)

	pending := len(urgent) + len(opportunistic) - migrated - skipped
	if migrated > 0 || len(urgent) > 0 {
		logx.Infof("[Rebalance] zone=%d urgent=%d opportunistic=%d migrated=%d skipped=%d pending=%d budget=%d",
			zoneId, len(urgent), len(opportunistic), migrated, skipped, pending, budget)
	}

	// After a successful run the pending gauge still reflects the plan we
	// just executed against; refresh it with the post-migration residue
	// so queries see reality, not the pre-run snapshot.
	metrics.SetRebalancePending(zoneId, string(reasonNodeGone), max0(len(urgent)-migrated))
	metrics.SetRebalancePending(zoneId, string(reasonBetterHome),
		max0(len(urgent)+len(opportunistic)-migrated-max0(len(urgent)-migrated)))
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// PlanWorldChannelRebalance returns the urgent and opportunistic migration
// lists that RebalanceWorldChannelsForZone would act on, plus the effective
// per-tick budget. Split out from the action path so tests can assert the
// planner in isolation (and so /metrics or an admin RPC can expose pending
// migration pressure).
//
// The returned slices are ordered for deterministic execution: urgent
// before opportunistic, within each list by (confId, sceneId).
func PlanWorldChannelRebalance(svcCtx *svc.ServiceContext, zoneId uint32, confIds []uint64) (urgent, opportunistic []channelMigration, budget int) {
	if len(confIds) == 0 {
		return nil, nil, 0
	}
	budget = svcCtx.Config.MaxRebalanceMigrationsPerTick
	if budget == 0 {
		return nil, nil, 0
	}
	if budget < 0 {
		budget = 10
	}

	nodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
	if len(nodes) == 0 {
		return nil, nil, budget
	}
	sort.Strings(nodes)
	liveSet := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		liveSet[n] = struct{}{}
	}

	for _, confId := range confIds {
		members, err := svcCtx.Redis.Smembers(worldChannelsKey(zoneId, confId))
		if err != nil {
			continue
		}
		// Sort members so tests (and logs) see deterministic order within
		// a single confId. Smembers order is implementation-defined.
		sort.Strings(members)
		for _, m := range members {
			sceneId, _ := strconv.ParseUint(m, 10, 64)
			if sceneId == 0 {
				continue
			}
			curNode, _ := svcCtx.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
			target := assignNodeByHash(sceneId, nodes)
			if curNode == target {
				continue
			}
			if curNode != "" && isKnownNodeIdentityAmbiguous(zoneId, curNode) {
				logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d old node %s has duplicate registrations; refusing ownership migration",
					zoneId, confId, sceneId, curNode)
				continue
			}
			if _, alive := liveSet[curNode]; !alive {
				// "不在 world pool" 同时包含两种情况：进程真死了，以及进程
				// 仍活着但 role 刚被改成非 world。后者若还有玩家，直接换
				// scene:{id}:node 就是未实现的跨节点热迁移，必须 fail-closed。
				if count := readScenePlayerCount(svcCtx, sceneId); count > 0 {
					if _, err := svcCtx.Redis.Zscore(nodeLoadKey(zoneId), curNode); err == nil {
						logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d old node %s is still registered with %d players; refusing cross-node hot migration",
							zoneId, confId, sceneId, curNode, count)
						continue
					}
				}
				// 再入屏障:老节点刚判死时,它还在 emergency relocate drain 里
				// SavePlayer(C++ kDrainBudget)。此刻迁频道 = 新节点
				// CreateScene 并接管归属,与老节点的最终存盘互相覆盖。
				// 屏障未到就本轮不排进迁移计划,下一拍(事件或周期 tick)再来。
				// 放在计划阶段而不是执行阶段,是为了连那次白跑的 CreateScene RPC
				// 都省掉;migrateWorldChannel 里的 reassignSceneNode 还有第二道
				// 同样的判定兜住竞态。
				if reentryBarrierBlocks(svcCtx, zoneId, curNode, barrierSiteRebalance) {
					continue
				}
				urgent = append(urgent, channelMigration{
					ConfId: confId, SceneId: sceneId,
					OldNode: curNode, NewNode: target, Reason: reasonNodeGone,
				})
				continue
			}
			if count := readScenePlayerCount(svcCtx, sceneId); count > 0 {
				continue
			}
			opportunistic = append(opportunistic, channelMigration{
				ConfId: confId, SceneId: sceneId,
				OldNode: curNode, NewNode: target, Reason: reasonBetterHome,
			})
		}
	}
	return urgent, opportunistic, budget
}

// migrateWorldChannel performs the create-swap-destroy dance for a single
// channel. Steps:
//
//  1. CreateScene on target node (C++ is idempotent by sceneId).
//  2. Rewrite scene:{sid}:node in Redis. Any EnterScene that races this
//     write sees either oldNode (still valid, scene still exists) or
//     newNode (also valid after step 1) — both serve the same sceneId.
//     这一步是 CAS(reassignSceneNode),归属已不是 oldNode 就整次作废,
//     并回收第 1 步在 newNode 上建的实体(destroyUnownedMigrationTarget)。
//  3. Update per-node scene_count counters —— 与第 2 步在同一个 Lua 脚本里完成。
//  4. Best-effort DestroyScene on the old node. A failure here is logged
//     but not fatal: the old scene will be cleaned up on the next node
//     restart or by the lifecycle manager's stale-scene sweep.
//  5. Cascade-destroy any mirrors that were co-located with this channel
//     on the OLD node. The whole point of mirror co-location is reusing
//     the source's resident world data; once the source moves, mirrors
//     stranded on the old node have lost their reason to exist (and are
//     just chewing up the old node's memory until idle timeout). We
//     destroy them eagerly so the old node can drain. Players that need
//     a mirror again will get a fresh one co-located on newNode.
//
// Returns true if steps 1-3 succeeded (i.e. the channel is now reachable
// via newNode). Step 4 / 5 failures do not flip the return.
func migrateWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32,
	confId, sceneId uint64, oldNode, newNode string, reason rebalanceReason) bool {

	if newNode == "" || newNode == oldNode {
		return false
	}

	if _, err := RequestNodeCreateScene(ctx, svcCtx, zoneId, newNode, uint32(confId), sceneId); err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: CreateScene on new node %s failed (%s): %v",
			zoneId, confId, sceneId, newNode, reason, err)
		return false
	}

	// Snapshot dependent mirrors BEFORE swapping the mapping. After
	// reassignSceneNode runs the source points at newNode, and any new
	// mirror for this source would correctly land on newNode — but the
	// already-existing mirrors on oldNode are now stranded.
	dependentMirrors, _ := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneId))

	// 归属改写走 reassignSceneNode 的 CAS:scene:{id}:node、两侧反向索引
	// (node-death reconciliation 靠它找到迁走的频道)与两侧 scene_count 在
	// 一个脚本里一起改,返回 true 就是全部落地,不用再回读核对。
	//
	// 返回 false 有两类竞态,都意味着本次迁移作废:
	//   - 计划阶段到这里隔了一次 CreateScene RPC,老节点的死亡时刻可能刚写进
	//     Redis,第二道屏障判定拦下;
	//   - 计划是陈旧的:本进程不持 zone 锁,任意副本上的 initWorldScenesForZone
	//     或 EnterScene 的懒改派可能已经把频道派到别的节点。
	// 两种情况下 newNode 上刚建的实体都没有归属,回收掉;名额与计数都没动。
	if !reassignSceneNode(svcCtx, zoneId, sceneId, oldNode, newNode) {
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: ownership not updated (reentry barrier, owner changed concurrently, or Redis failure); migration invalidated",
			zoneId, confId, sceneId)
		destroyUnownedMigrationTarget(ctx, svcCtx, zoneId, confId, sceneId, newNode)
		return false
	}

	// Agones 名额跟着频道走。不转移的话旧 GameServer 的计数永远不还
	// (容量凭空蒸发)、新的没记账(容量被超卖),两个方向都会让
	// FleetAutoscaler 算错。先占新的再还旧的,见 TransferAgonesRoomForScene。
	// scene_count 已在 reassignSceneNode 的脚本里挪好,这里不再调。
	TransferAgonesRoomForScene(ctx, svcCtx, sceneId, newNode, zoneId)

	// Best-effort cleanup of the old scene entity. Skip when the old node
	// is already gone — nothing to clean up — or when oldNode appears dead
	// to avoid long RPC timeouts during the urgent path.
	if oldNode != "" && IsNodeAlive(svcCtx, zoneId, oldNode) {
		if err := RequestNodeDestroyScene(ctx, svcCtx, zoneId, oldNode, sceneId); err != nil {
			logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: DestroyScene on old node %s failed (%s, ignored): %v",
				zoneId, confId, sceneId, oldNode, reason, err)
		}
	}

	cascadeMirrorsOnSourceMigration(ctx, svcCtx, zoneId, sceneId, dependentMirrors, oldNode, newNode)

	logx.Infof("[Rebalance] zone=%d conf=%d scene=%d migrated %s -> %s (%s) cascaded_mirrors=%d",
		zoneId, confId, sceneId, oldNode, newNode, reason, len(dependentMirrors))
	return true
}

// destroyUnownedMigrationTarget 回收一次作废迁移在 newNode 上预建的场景实体(尽力而为)。
//
// 先重读归属,**确认** newNode 不是当前属主才发 DestroyScene:
//   - 并发的另一个写者可能恰好也把频道派到了 newNode。CreateScene 按 scene_id 幂等,
//     newNode 上那个实体就是它的活场景,销毁就是把在用频道拆掉;
//   - reassignSceneNode 在 Redis 出错时返回 false,但脚本可能已经执行;重读失败
//     就无从确认,宁可留一个空实体,也不冒拆掉活场景的险。
//
// 残余窗口:重读之后、DestroyScene 到达之前,若别的写者恰好把归属改到 newNode 并先完成
// CreateScene,这次销毁会拆掉它的实体。窗口只有一次 RPC 长,且可自愈:fullSync 每轮
// initWorldScenesForZone 都对全部频道补发 CreateScene。
func destroyUnownedMigrationTarget(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32,
	confId, sceneId uint64, newNode string) {

	owner, err := svcCtx.Redis.Get(sceneNodeKey(sceneId))
	if err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: cannot re-read owner (%v); leaving the entity pre-created on %s",
			zoneId, confId, sceneId, err, newNode)
		return
	}
	if owner == newNode {
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: another writer already moved it to %s; keeping that entity",
			zoneId, confId, sceneId, newNode)
		return
	}
	if err := RequestNodeDestroyScene(ctx, svcCtx, zoneId, newNode, sceneId); err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: DestroyScene of unowned entity on %s failed (owner=%q): %v",
			zoneId, confId, sceneId, newNode, owner, err)
		return
	}
	logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: destroyed unowned entity on %s (owner=%q)",
		zoneId, confId, sceneId, newNode, owner)
}

// cascadeMirrorsOnSourceMigration force-destroys mirrors that were co-located
// with a source scene that just moved nodes. Each mirror is destroyed via
// the standard force path so residual members, reverse indexes, and
// node-scene counters all stay in sync. reason="source_migrated" so
// dashboards can distinguish this from idle / explicit / cascade-on-destroy.
//
// Mirrors whose recorded zone differs from the source's zone are still
// destroyed in their own zone (a cross-zone mirror is a misuse of the
// optimization, but we don't want cascade to silently no-op on it).
func cascadeMirrorsOnSourceMigration(ctx context.Context, svcCtx *svc.ServiceContext,
	zoneId uint32, sourceSceneId uint64, mirrorIds []string, oldNode, newNode string) {

	if len(mirrorIds) == 0 {
		return
	}
	for _, m := range mirrorIds {
		mid, err := strconv.ParseUint(m, 10, 64)
		if err != nil || mid == sourceSceneId {
			continue
		}
		mirrorZone := zoneId
		if z := GetSceneZone(svcCtx, mid); z != 0 {
			mirrorZone = z
		}
		logx.Infof("[Rebalance] cascading mirror %d (source %d migrated %s -> %s) in zone %d",
			mid, sourceSceneId, oldNode, newNode, mirrorZone)
		destroyInstanceForce(ctx, svcCtx, mirrorZone, mid, "source_migrated")
	}
	// Drop the now-empty mirrors index so the next mirror created against
	// this source starts with a clean set.
	svcCtx.Redis.Del(sceneMirrorsKey(sourceSceneId))
}
