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
			// 规划所需的每一次读(属主、人数、老节点是否还在负载集)读不到就不把这个频道排进计划
			// (2026-10-23)。读错当「没有属主」会排出一次注定被 CAS 驳回、却先在新节点上建了实体的
			// 迁移;人数读错当 0 会把有人的频道从活属主身上搬走 —— reassignSceneNode 的 CAS 只比属主、
			// 不看人数,同一 scene_id 两个节点上都有玩家。
			curNode, err := svcCtx.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
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
				// "不在 world pool" 同时包含两种情况：进程真死了，以及进程
				// 仍活着但 role 刚被改成非 world。后者若还有玩家，直接换
				// scene:{id}:node 就是未实现的跨节点热迁移，必须 fail-closed。
				count, err := readScenePlayerCountChecked(svcCtx, sceneId)
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
			count, err := readScenePlayerCountChecked(svcCtx, sceneId)
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
	return urgent, opportunistic, budget
}

// migrateWorldChannel performs the create-swap-destroy dance for a single
// channel. Steps:
//
//  0. 分配这次迁移的放置代次(docs/design/world-channel-placement-generation.md):下面的建场景、CAS、拆老实体、
//     落空回收都带它,节点据此认出晚到的旧请求。分配失败就不迁,什么都没发。
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

	generation, err := allocatePlacementGeneration(svcCtx, zoneId)
	if err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: not migrating to %s (%s): %v",
			zoneId, confId, sceneId, newNode, reason, err)
		return false
	}
	if _, err := RequestNodeCreateSceneAtGeneration(ctx, svcCtx, zoneId, newNode, uint32(confId), sceneId, generation); err != nil {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: CreateScene on new node %s failed (%s): %v",
			zoneId, confId, sceneId, newNode, reason, err)
		return false
	}

	// Snapshot dependent mirrors BEFORE swapping the mapping. After
	// reassignSceneNode runs the source points at newNode, and any new
	// mirror for this source would correctly land on newNode — but the
	// already-existing mirrors on oldNode are now stranded.
	// 读不到就不知道迁走之后要级联哪些镜像:这次迁移作废,归属不动,回收刚建的实体,下一拍重新规划。
	dependentMirrors, err := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneId))
	if err != nil {
		noteReadUnknown(zoneId, readUnknownSiteRebalancePlan, fmt.Sprintf("mirror set of scene %d before migrating it to %s", sceneId, newNode), err)
		destroyUnownedMigrationTarget(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation)
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
	// 这些情况下 newNode 上刚建的实体都没有归属,按本次的代次回收掉;名额与计数都没动。
	if !reassignSceneNode(svcCtx, zoneId, sceneId, oldNode, newNode, generation) {
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: ownership not updated (reentry barrier, owner changed concurrently, stale placement generation, or Redis failure); migration invalidated",
			zoneId, confId, sceneId)
		destroyUnownedMigrationTarget(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation)
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
func destroyUnownedMigrationTarget(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32,
	confId, sceneId uint64, newNode string, generation uint64) {

	reclaim, err := reclaimUnownedWorldChannelEntity(ctx, svcCtx, zoneId, confId, sceneId, newNode, generation)
	switch {
	case err != nil:
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: could not check the owner before reclaiming the entity on %s; left in place: %v",
			zoneId, confId, sceneId, newNode, err)
	case reclaim.Kept:
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: another writer already moved it to %s; keeping that entity",
			zoneId, confId, sceneId, newNode)
	case reclaim.DestroyErr != nil:
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: reclaiming the unowned entity on %s failed (owner=%q): %v",
			zoneId, confId, sceneId, newNode, reclaim.Owner, reclaim.DestroyErr)
	default:
		logx.Infof("[Rebalance] zone=%d conf=%d scene=%d: reclaimed unowned entity on %s at placement generation %d (owner=%q)",
			zoneId, confId, sceneId, newNode, generation, reclaim.Owner)
	}
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
	//     子集合、或这里读不到它的 zone 时都 fail-closed),它还活着,必须留在索引里。
	// 集合被摘空后 Redis 自动回收键。
	handled := make([]any, 0, len(mirrorIds))
	for _, m := range mirrorIds {
		mid, err := strconv.ParseUint(m, 10, 64)
		if err != nil || mid == sourceSceneId {
			continue
		}
		// 读不到镜像的 zone 就不猜:本轮不拆,留在索引里。
		mirrorZone, zoneErr := lookupSceneZone(svcCtx, mid)
		if zoneErr != nil {
			noteReadUnknown(zoneId, readUnknownSiteMirrorCascade,
				fmt.Sprintf("zone of mirror %d (source %d migrated), keeping it and its index entry", mid, sourceSceneId), zoneErr)
			continue
		}
		if mirrorZone != 0 && mirrorZone != zoneId {
			logx.Infof("[Rebalance] mirror %d of source %d is registered in zone %d, not %d; unlinking it and leaving it to that zone",
				mid, sourceSceneId, mirrorZone, zoneId)
			handled = append(handled, m)
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
		svcCtx.Redis.Srem(sceneMirrorsKey(sourceSceneId), handled...)
	}
}
