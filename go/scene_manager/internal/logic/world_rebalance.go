package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// rebalanceReason describes why a channel is being moved. Used only for logs
// so operators can distinguish "the old node went away" from "we just found
// a better home".
type rebalanceReason string

const (
	reasonNodeGone   rebalanceReason = "node_gone"   // old node is dead / no longer world-hosting
	reasonBetterHome rebalanceReason = "better_home" // opportunistic; old node still alive
	// reasonNodeDraining:老节点活着但自报排空(NodeInfo.admission = CLOSED,计划下线)。频道有人也迁:老节点的那份实例
	// 在住户走光之前一直保留,住户按疏散链路存盘后到新属主上重新载入(docs/design/scene-node-planned-drain.md §4.3)。
	// 由排空迁移协程执行(node_drain.go),不占每拍的迁移名额。
	reasonNodeDraining rebalanceReason = "node_draining"
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
	// 排空中节点的频道由排空迁移协程迁(node_drain.go):有时限,不占本拍名额,也不受 rebalance 关闭影响。
	// 这里只负责把协程拉起来 —— watch 事件之外的兜底(本副本刚当选、协程意外退出),规划结果里也不再执行它们。
	ensureDrainMigrationsForZone(ctx, svcCtx, zoneId)

	urgent, opportunistic, budget := PlanWorldChannelRebalance(svcCtx, zoneId, confIds)
	urgent = withoutDrainingMoves(urgent)

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

// withoutDrainingMoves 去掉 node_draining 项:它们归排空迁移协程(node_drain.go),这里再跑一遍只会与协程
// 抢同一个频道(CAS 让输的一边白建一次实体)。
func withoutDrainingMoves(list []channelMigration) []channelMigration {
	kept := list[:0:0]
	for _, c := range list {
		if c.Reason != reasonNodeDraining {
			kept = append(kept, c)
		}
	}
	return kept
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
	urgent, opportunistic = planChannelMoves(svcCtx, zoneId, confIds)
	return urgent, opportunistic, budget
}

// planChannelMoves 是规划器本体:按当前开放的世界节点重算每个频道应在哪,列出要迁的。不看每拍名额,
// rebalance(PlanWorldChannelRebalance)与排空迁移(node_drain.go planDrainMigrations)共用。
//
// 一致性哈希只在 placementRing 上做:开放节点里不在外出构建上的那些,一个都没有才退回全部开放节点
// (docs/design/scene-node-planned-drain.md §4.6);liveSet 是全部开放节点。没在排空的节点上的频道照旧只在没人时
// 才迁(better_home)。属主排空中(活着、自报 CLOSED)的频道列为 node_draining 紧急项,有人也迁(§4.3);属主不在
// 开放集合里、也不是排空中的,照旧按「已死 / 不再托主世界」处理。
func planChannelMoves(svcCtx *svc.ServiceContext, zoneId uint32, confIds []uint64) (urgent, opportunistic []channelMigration) {
	if len(confIds) == 0 {
		return nil, nil
	}
	openNodes := getNodesForPurpose(svcCtx, zoneId, constants.NodePurposeWorld)
	if len(openNodes) == 0 {
		return nil, nil
	}
	view := snapshotNodeAdmission(zoneId)
	nodes := append([]string(nil), view.placementRing(openNodes)...)
	sort.Strings(nodes)
	liveSet := make(map[string]struct{}, len(openNodes))
	for _, n := range openNodes {
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
			// 规划所需的每一次读(属主、人数、老节点是否还在负载集)读不到就不把这个频道排进计划
			// (2026-10-23)。读错当「没有属主」会排出一次注定被 CAS 驳回、却先在新节点上建了实体的
			// 迁移;人数读错当 0 会把有人的频道从活属主身上搬走 —— reassignSceneNode 的 CAS 只比属主、
			// 不看人数,同一 scene_id 两个节点上都有玩家。
			curNode, err := svcCtx.Redis.Get(sceneNodeKey(zoneId, sceneId))
			if err != nil {
				noteReadUnknown(zoneId, readUnknownSiteRebalancePlan, fmt.Sprintf("owner of scene %d (conf %d)", sceneId, confId), err)
				continue
			}
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
				// 计划下线(自报排空、仍在负载集):有人也迁。老节点的那份实例要等住户走光才销毁,住户按疏散链路存盘、
				// 到新属主上重新载入 —— 搬的是人,不是内存里的场景状态。老节点活着,不过再入屏障(屏障管的是死节点的残留写)。
				if curNode != "" && view.isDraining(curNode) && IsNodeAlive(svcCtx, zoneId, curNode) {
					urgent = append(urgent, channelMigration{
						ConfId: confId, SceneId: sceneId,
						OldNode: curNode, NewNode: target, Reason: reasonNodeDraining,
					})
					continue
				}
				// "不在 world pool" 同时包含两种情况：进程真死了，以及进程
				// 仍活着但 role 刚被改成非 world。后者若还有玩家，直接换
				// scene:{id}:node 就是未实现的跨节点热迁移，必须 fail-closed。
				count, err := readScenePlayerCountChecked(svcCtx, zoneId, sceneId)
				if err != nil {
					noteReadUnknown(zoneId, readUnknownSiteRebalancePlan, fmt.Sprintf("player count of scene %d (conf %d)", sceneId, confId), err)
					continue
				}
				if count > 0 {
					_, zerr := svcCtx.Redis.Zscore(nodeLoadKey(zoneId), curNode)
					if zerr == nil {
						logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d old node %s is still registered with %d players; refusing cross-node hot migration",
							zoneId, confId, sceneId, curNode, count)
						continue
					}
					// 只有 redis.Nil 才说明老节点确实不在负载集;读错时它可能还活着、还托着这些玩家。
					if !errors.Is(zerr, redis.Nil) {
						noteReadUnknown(zoneId, readUnknownSiteRebalancePlan,
							fmt.Sprintf("load-set membership of node %s (scene %d has %d players)", curNode, sceneId, count), zerr)
						continue
					}
				}
				// 再入屏障:老节点刚判死时,若它是重注册失败走了冲突关停,仍在有界
				// (C++ kDrainBudget 15s)疏散里 SavePlayer。此刻迁频道 = 新节点
				// CreateScene 并接管归属,老节点的最终存盘会被 TiDB 写权行拒掉而丢进度
				// (迟到写不会覆盖新档,正确性由写权行兜底)。
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
			count, err := readScenePlayerCountChecked(svcCtx, zoneId, sceneId)
			if err != nil {
				noteReadUnknown(zoneId, readUnknownSiteRebalancePlan, fmt.Sprintf("player count of scene %d (conf %d)", sceneId, confId), err)
				continue
			}
			if count > 0 {
				continue
			}
			opportunistic = append(opportunistic, channelMigration{
				ConfId: confId, SceneId: sceneId,
				OldNode: curNode, NewNode: target, Reason: reasonBetterHome,
			})
		}
	}
	return urgent, opportunistic
}

// migrateWorldChannel performs the create-swap-destroy dance for a single
// channel. Steps:
//
//  0. 先在目标的 GameServer 上占 Agones 名额(docs/design/agones-scene-node-high-density.md §13.10):不把频道搬进
//     Agones 认为已经满了的进程。老节点已不在(reasonNodeGone)时频道必须搬走,计划的目标满了就让 GSA 在本 zone
//     另挑一台有空位的,落点改成它;换更好的家(reasonBetterHome)只认计划的目标,满了这次不搬。占不到就不迁,
//     什么都没发;之后任何一步没成都放弃这次占的名额。
//     然后分配这次迁移的放置代次(docs/design/world-channel-placement-generation.md):下面的建场景、CAS、拆老实体、
//     落空回收都带它,节点据此认出晚到的旧请求。分配失败就不迁。
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
//     带本次的代次:频道之后若又迁回老节点(更新的代次),晚到的这次拆除会被节点当成过期的忽略。
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

	// 老节点已不在或正在计划下线时频道必须搬走:计划的目标满了就让 GSA 另挑一台有空位的。
	mustMove := reason == reasonNodeGone || reason == reasonNodeDraining
	target, placement, ok := reserveAgonesRoomForWorldChannel(ctx, svcCtx, zoneId, sceneId, newNode, mustMove)
	if !ok {
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: no Agones room capacity for moving it to %s (%s); not migrating this round",
			zoneId, confId, sceneId, newNode, reason)
		return false
	}
	if target == oldNode {
		// 只在回落分配挑中了老节点时出现:GSA 按 etcd 里登记的 Pod 地址认节点,负载集却已判它不在。搬过去等于没搬,这次不搬。
		abandonAgonesPlacement(ctx, svcCtx, zoneId, placement, "migrate_target_is_owner")
		return false
	}
	if target != newNode {
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: planned target %s is full; moving it to %s instead (%s)",
			zoneId, confId, sceneId, newNode, target, reason)
		newNode = target
	}

	generation, err := allocatePlacementGeneration(svcCtx, zoneId)
	if err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: not migrating to %s (%s): %v",
			zoneId, confId, sceneId, newNode, reason, err)
		abandonAgonesPlacement(ctx, svcCtx, zoneId, placement, "placement_generation_unavailable")
		return false
	}
	if _, err := RequestNodeCreateSceneAtGeneration(ctx, svcCtx, zoneId, newNode, uint32(confId), sceneId, generation); err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: CreateScene on new node %s failed (%s): %v",
			zoneId, confId, sceneId, newNode, reason, err)
		abandonAgonesPlacement(ctx, svcCtx, zoneId, placement, "create_scene_failed")
		return false
	}

	// Snapshot dependent mirrors BEFORE swapping the mapping. After
	// reassignSceneNode runs the source points at newNode, and any new
	// mirror for this source would correctly land on newNode — but the
	// already-existing mirrors on oldNode are now stranded.
	// 读不到就不知道迁走之后要级联哪些镜像:这次迁移作废,归属不动,回收刚建的实体,下一拍重新规划。
	dependentMirrors, err := svcCtx.Redis.Smembers(sceneMirrorsKey(zoneId, sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteRebalancePlan, fmt.Sprintf("mirror set of scene %d before migrating it to %s", sceneId, newNode), err)
		settleLostChannelMove(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation, placement)
		return false
	}

	// 归属改写走 reassignSceneNode 的 CAS:scene:{id}:node、两侧反向索引
	// (node-death reconciliation 靠它找到迁走的频道)与两侧 scene_count 在
	// 一个脚本里一起改,返回 true 就是全部落地,不用再回读核对。
	//
	// 返回 false 有三类竞态,都意味着本次迁移作废:
	//   - 计划阶段到这里隔了一次 CreateScene RPC,老节点的死亡时刻可能刚写进
	//     Redis,第二道屏障判定拦下;
	//   - 计划是陈旧的:本进程不持 zone 锁,任意副本上的 initWorldScenesForZone
	//     或 EnterScene 的懒改派可能已经把频道派到别的节点;
	//   - 代次过期:别的写者在本次分配代次之后、提交之前,以更新的代次回收过这个频道的预建实体。
	// 这些情况下 newNode 上刚建的实体都没有归属,按本次的代次回收掉,第 0 步占的名额放弃;计数没动
	// (settleLostChannelMove;属主恰是 newNode 时实体留着、名额照常提交)。
	if !reassignSceneNode(svcCtx, zoneId, sceneId, oldNode, newNode, generation) {
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: ownership not updated (reentry barrier, owner changed concurrently, stale placement generation, or Redis failure); migration invalidated",
			zoneId, confId, sceneId)
		settleLostChannelMove(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation, placement)
		return false
	}

	// Agones 名额跟着频道走:提交第 0 步在 newNode 上占好的那一条,改成了就删旧条目(commitReservedAgonesRoom)。
	// 不转的话旧 GameServer 的名额永远不还、新的没记账,两个方向都会让 FleetAutoscaler 算错。
	// scene_count 已在 reassignSceneNode 的脚本里挪好,这里不再调。
	commitReservedAgonesRoom(ctx, svcCtx, zoneId, sceneId, newNode, placement)

	// Best-effort cleanup of the old scene entity. Skip when the old node
	// is already gone — nothing to clean up — or when oldNode appears dead
	// to avoid long RPC timeouts during the urgent path.
	if oldNode != "" && IsNodeAlive(svcCtx, zoneId, oldNode) {
		if err := RequestNodeDestroySceneAtGeneration(ctx, svcCtx, zoneId, oldNode, sceneId, generation); err != nil {
			logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: DestroyScene on old node %s failed (%s, ignored): %v",
				zoneId, confId, sceneId, oldNode, reason, err)
		}
	}

	cascadeMirrorsOnSourceMigration(ctx, svcCtx, zoneId, sceneId, dependentMirrors, oldNode, newNode)

	logx.Infof("[Rebalance] zone=%d conf=%d scene=%d migrated %s -> %s (%s) cascaded_mirrors=%d",
		zoneId, confId, sceneId, oldNode, newNode, reason, len(dependentMirrors))
	return true
}

// destroyUnownedMigrationTarget 回收一次作废迁移在 newNode 上按放置代次 generation 预建的场景实体(尽力而为)。
// rebalance、懒改派与 init 的死节点改派共用。
//
// **确认** newNode 不是当前属主才拆,确认在回收脚本里做(reclaimUnownedWorldChannelEntity):
//   - 并发的另一个写者可能恰好也把频道派到了 newNode。CreateScene 按 scene_id 幂等,
//     newNode 上那个实体就是它的活场景,销毁就是把在用频道拆掉;
//   - reassignSceneNode 结论未知时(问结局也出错)返回 false,但脚本可能已经执行;比对读到属主就是 newNode
//     就保留。比对本身出错就无从确认,宁可留一个空实体,也不冒拆掉活场景的险。
//
// 比对之后、DestroyScene 到达之前,别的写者把归属改到 newNode 也拆不到它的实体:回收脚本先把代次高水位抬到 generation,
// 那次改派的代次一定更新,节点把这次拆除当成过期的忽略(docs/design/world-channel-placement-generation.md)。
//
// 返回 true 只在比对确认 newNode 就是现任属主、实体留着的时候;比对出错(不知道)与已回收都回 false。
func destroyUnownedMigrationTarget(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32,
	confId, sceneId uint64, newNode string, generation uint64) bool {

	reclaim, err := reclaimUnownedWorldChannelEntity(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation)
	switch {
	case err != nil:
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: could not check the owner before reclaiming the entity on %s; left in place: %v",
			zoneId, confId, sceneId, newNode, err)
	case reclaim.Kept:
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: another writer already moved it to %s; keeping that entity",
			zoneId, confId, sceneId, newNode)
		return true
	case reclaim.DestroyErr != nil:
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: reclaiming the unowned entity on %s failed (owner=%q): %v",
			zoneId, confId, sceneId, newNode, reclaim.Owner, reclaim.DestroyErr)
	default:
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: reclaimed unowned entity on %s at placement generation %d (owner=%q)",
			zoneId, confId, sceneId, newNode, generation, reclaim.Owner)
	}
	return false
}

// settleLostChannelMove 收拾一次没改成的频道换节点(先占名额、先建后改的落空一半):按放置代次回收 newNode 上预建的实体
// (destroyUnownedMigrationTarget),并处理换节点之前预占的 Agones 名额(docs/design/agones-scene-node-high-density.md §13.10):
//   - 比对确认属主恰是 newNode(别的写者挑了同一个节点,或 CAS 其实已执行):实体留着,名额照常提交 —— 别的写者已提交的,
//     记录已在这台上,这次的条目放弃;没提交的,这次就是那一份(commitReservedAgonesRoom)。两边都按条目,只会留一条;
//   - 其余(属主不是 newNode,或比对出错不知道):放弃这次的条目。比对出错而 CAS 其实已执行时,旧条目仍记着,总数不少,
//     之后由对账转过来。
//
// rebalance、懒改派与 init 的死节点改派共用;resolveScene 的死节点自愈落空后还要按属主定应答,另写(adoptOwnerAfterLostReassign)。
// placement 为 nil(没开 Agones)时只回收实体。
func settleLostChannelMove(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32, confId, sceneId uint64,
	newNode string, generation uint64, placement *agonesPlacement) {
	if destroyUnownedMigrationTarget(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation) {
		commitReservedAgonesRoom(ctx, svcCtx, zoneId, sceneId, newNode, placement)
		return
	}
	abandonAgonesPlacement(ctx, svcCtx, zoneId, placement, "channel_move_lost")
}

// cascadeMirrorsOnSourceMigration force-destroys mirrors that were co-located
// with a source scene that just moved nodes. Each mirror is destroyed via
// the standard force path so residual members, reverse indexes, and
// node-scene counters all stay in sync. reason="source_migrated" so
// dashboards can distinguish this from idle / explicit / cascade-on-destroy.
//
// 只拆本 zone 的镜像(docs/design/control-plane-zone-scoping-20261105.md §4.3:zone z 的控制域键只由 zone z 的
// scene_manager 读写)。镜像登记在别的 zone 时不拆,由那个 zone 的空闲回收收走,只从源的子集合(本 zone 的键)里摘掉;
// 新建镜像时源在别的 zone 就不登记成子镜像(createInstance),这里给更早登记的数据兜底。
func cascadeMirrorsOnSourceMigration(ctx context.Context, svcCtx *svc.ServiceContext,
	zoneId uint32, sourceSceneId uint64, mirrorIds []string, oldNode, newNode string) {

	if len(mirrorIds) == 0 {
		return
	}
	// 只从源的子集合里摘掉本轮确实处理完的镜像(2026-10-23),不再整键删除:
	//   - 级联范围是迁移之前的快照,快照之后挂上来的镜像不在其中,整键删除会让它们从索引里消失,
	//     channelHasMirrors 看不到它们,源频道可能被缩容排空,镜像成孤儿;
	//   - 销毁回 error = 读不到它的状态,本轮没销毁(destroyInstanceInternal 提交前读不到镜像源 /
	//     子集合时 fail-closed),它还活着,必须留在索引里。
	// 集合被摘空后 Redis 自动回收键。
	// 子集合与镜像都在本 zone 的键里(docs/design/redis-cluster-all-domains-20261110.md §3.3):镜像不在本 zone(别的 zone 的,
	// 或已销毁)时认领读不到映射、一个键都不动,按已处理从索引里摘掉。原来先读 scene:<id>:zone 比 zone,那把键已删。
	handled := make([]any, 0, len(mirrorIds))
	for _, m := range mirrorIds {
		mid, err := strconv.ParseUint(m, 10, 64)
		if err != nil || mid == sourceSceneId {
			continue
		}
		logx.Infof("[Rebalance] cascading mirror %d (source %d migrated %s -> %s) in zone %d",
			mid, sourceSceneId, oldNode, newNode, zoneId)
		if _, destroyErr := destroyInstanceForce(ctx, svcCtx, zoneId, mid, "source_migrated"); destroyErr != nil {
			logx.Errorf("[Rebalance] mirror %d of source %d was not destroyed this round; keeping its index entry: %v",
				mid, sourceSceneId, destroyErr)
			continue
		}
		handled = append(handled, m)
	}
	if len(handled) > 0 {
		svcCtx.Redis.Srem(sceneMirrorsKey(zoneId, sourceSceneId), handled...)
	}
}
