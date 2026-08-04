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
//     which this codebase does not have. A truly dead node may retain a stale
//     player_count; that mapping is already unusable and is rebuilt urgently.
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
				if count := readInt64(svcCtx, fmt.Sprintf(InstancePlayerCountKey, sceneId)); count > 0 {
					if _, err := svcCtx.Redis.Zscore(nodeLoadKey(zoneId), curNode); err == nil {
						logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d old node %s is still registered with %d players; refusing cross-node hot migration",
							zoneId, confId, sceneId, curNode, count)
						continue
					}
				}
				urgent = append(urgent, channelMigration{
					ConfId: confId, SceneId: sceneId,
					OldNode: curNode, NewNode: target, Reason: reasonNodeGone,
				})
				continue
			}
			if count := readInt64(svcCtx, fmt.Sprintf(InstancePlayerCountKey, sceneId)); count > 0 {
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
//  3. Update per-node scene_count counters.
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

	// Use reassignSceneNode so node:zone:{zoneId}:{nodeId}:scenes stays in sync
	// (otherwise node-death reconciliation would miss migrated channels).
	// reassignSceneNode logs its own Set error but doesn't surface it; verify
	// the mapping landed before declaring success.
	reassignSceneNode(svcCtx, zoneId, sceneId, oldNode, newNode)
	if cur, _ := svcCtx.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId)); cur != newNode {
		logx.Errorf("[Rebalance] zone=%d conf=%d scene=%d: scene->node mapping did not stick (got %q, want %q)",
			zoneId, confId, sceneId, cur, newNode)
		// The ECS entity on newNode is effectively orphaned until the
		// next rebalance tick retries. Since CreateScene is idempotent
		// this is safe; we just wasted an RPC.
		return false
	}

	// Agones 名额跟着频道走。不转移的话旧 GameServer 的计数永远不还
	// (容量凭空蒸发)、新的没记账(容量被超卖),两个方向都会让
	// FleetAutoscaler 算错。先占新的再还旧的,见 TransferAgonesRoomForScene。
	TransferAgonesRoomForScene(ctx, svcCtx, sceneId, newNode, zoneId)

	if oldNode != "" {
		if _, err := svcCtx.Redis.Decr(nodeSceneCountKey(zoneId, oldNode)); err != nil {
			logx.Errorf("[Rebalance] failed to decrement scene_count on old node %s: %v", oldNode, err)
		}
	}
	if _, err := svcCtx.Redis.Incr(nodeSceneCountKey(zoneId, newNode)); err != nil {
		logx.Errorf("[Rebalance] failed to increment scene_count on new node %s: %v", newNode, err)
	}

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

// cascadeMirrorsOnSourceMigration force-destroys mirrors that were co-located
// with a source scene that just moved nodes. Each mirror is destroyed via
// the standard force path so player_count residuals, reverse indexes, and
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
