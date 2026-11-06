package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// Orphan cleanup handles world channel sets that no longer correspond to
// any confId in World.json. Scenarios:
//   - Designers removed a map from World.json (rename/delete).
//   - A channel was allocated under an old WorldChannelCount value and is
//     no longer in use but the Redis keys linger.
//   - Zone decommissioned without cleaning up its world_channels:*.
//
// Cleanup deletes: the SET key itself, per-scene mappings, per-scene
// player counters, and best-effort DestroyScene RPC to the hosting node
// if that node is still alive.
//
// Safety rules:
//  1. Never run when worldConfIds() is empty — that's almost certainly a
//     table load failure, not an intentional wipe.
//  2. Only delete channels whose confId is NOT in the current valid set.
//     Misaligned channels (wrong node) are handled by the rebalancer.
//  3. Point lookups only, never a keyspace SCAN (2026-10-16): the only zone is
//     this process's own (orchestrated_zones.go; 2026-11-05 the zone registry
//     scene_manager:zones was removed, docs/design/control-plane-zone-scoping-20261105.md §4.3),
//     candidate (zone, confId) groups come from the fields of
//     world_channels:desired:zone:{z}. The old SCAN world_channels:zone:*
//     walked the whole shared Redis on the LoadReporter goroutine — hours at
//     launch scale.
//
// 发现覆盖范围(2026-10-16):生产代码里往频道集合 SADD 的唯一入口是 initWorldScenesForZone,它先经
// DesiredWorldChannelCount 播种 world_channels:desired:zone:{z} 的字段、后 SADD。所以「有频道集合」基本蕴含
// 「有期望数字段」。覆盖不到的组合:
//   - 2026-10-23 之前 DesiredWorldChannelCount 的 HSETNX 出错兜底路径(直接返回种子值、不写字段)建出来的
//     集合;该兜底已改为回 error、本轮不补建,新数据不再产生这种组合;
//   - 升级前遗留的数据(项目未上线,首次上线前清空,不会遇到)。
//
// 这些只会残留成垃圾键,不影响路由。反过来,「集合已空、期望数字段还在」的残留以前 SCAN 找不到,现在能清掉。
// 期望数字段是发现索引,所以 deleteOrphanChannel 把删它放到最后一步。

const orphanKeyPrefix = "world_channels:zone:"

// CleanupOrphanWorldChannels finds (zone, confId) channel groups whose confId
// is no longer in World.json and deletes them. Returns the number of groups
// removed. Runs from the leader's fullSync; calling it repeatedly is safe.
func CleanupOrphanWorldChannels(ctx context.Context, svcCtx *svc.ServiceContext) int {
	validConfIds := worldConfIds()
	if len(validConfIds) == 0 {
		logx.Info("[OrphanCleanup] World table empty; skipping (refusing to wipe on empty conf set)")
		return 0
	}
	confIdSet := make(map[uint64]struct{}, len(validConfIds))
	for _, id := range validConfIds {
		confIdSet[id] = struct{}{}
	}

	// 只查本进程编排的 zone(orchestrated_zones.go,docs/design/control-plane-zone-scoping-20261105.md §4.3):同组别的 zone
	// 的期望数 Hash 在同一个控制域实例里,由那个 zone 的领导者清。
	removed := 0
	for _, zone := range orchestratedZones(svcCtx) {
		desiredKey := worldDesiredChannelsKey(zone)
		fields, err := svcCtx.Redis.Hkeys(desiredKey)
		if err != nil {
			logx.Errorf("[OrphanCleanup] hkeys(%s) failed; skipping zone %d this round: %v", desiredKey, zone, err)
			continue
		}
		for _, field := range fields {
			confId, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				logx.Errorf("[OrphanCleanup] malformed conf field %q in %s; skipping", field, desiredKey)
				continue
			}
			// Delete only when the conf is not in World.json. Channels
			// for a valid conf in a "departed" zone are left alone on
			// purpose: if the zone rejoins, rebalance will re-home them
			// without data loss. If it never rejoins, the channel's
			// hosting node will also be gone from the live set and a
			// future World.json edit (removing this conf) triggers the
			// real delete. This is the conservative path.
			if _, validConf := confIdSet[confId]; validConf {
				continue
			}
			if deleteOrphanChannel(ctx, svcCtx, zone, confId, worldChannelsKey(zone, confId)) {
				removed++
			}
		}
	}

	if removed > 0 {
		logx.Infof("[OrphanCleanup] removed %d orphan world channel groups", removed)
	}
	return removed
}

// parseWorldChannelsKey parses "world_channels:zone:{z}:{confId}" into its
// components. Returns false for malformed keys so a stray entry doesn't
// blow up the whole cleanup pass.
//
// 2026-10-16 起生产代码不再调用它(孤儿清理改为点查,不再 SCAN 键名)。保留是为了守住键前缀约束:
// 别的键(线号表、分线目录)不得被解析成频道集合,否则仍在滚动窗口里跑 SCAN 的旧版本 scene_manager
// 会误删它们(TestWorldChannelDirectoryKeyFmt_CrossLanguageContract / TestParseWorldChannelsKey 在用)。
func parseWorldChannelsKey(key string) (zone uint32, confId uint64, ok bool) {
	if !strings.HasPrefix(key, orphanKeyPrefix) {
		return 0, 0, false
	}
	rest := key[len(orphanKeyPrefix):]
	sep := strings.IndexByte(rest, ':')
	if sep <= 0 {
		return 0, 0, false
	}
	z, err := strconv.ParseUint(rest[:sep], 10, 32)
	if err != nil {
		return 0, 0, false
	}
	c, err := strconv.ParseUint(rest[sep+1:], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return uint32(z), c, true
}

// deleteOrphanChannel tears down one orphan channel set: every scene in
// the set plus the set key itself. Returns true on full success.
func deleteOrphanChannel(ctx context.Context, svcCtx *svc.ServiceContext, zone uint32, confId uint64, setKey string) bool {
	members, err := svcCtx.Redis.Smembers(setKey)
	if err != nil {
		logx.Errorf("[OrphanCleanup] smembers(%s) failed: %v", setKey, err)
		return false
	}

	// 正在排空的频道已经从 world_channels 摘掉了,只看 setKey 会把它们整个漏掉:
	// scene:{id}:node、Agones 名额、排空标记全都留成永久垃圾,而且这个 confId
	// 已经不在 World 表里,sweepDrainingWorldChannels 也不会再扫到它。
	// 读不到排空集合就整组留到下一次 fullSync:以前读错被当成「没有排空中的频道」,本函数末尾随后
	// 删掉排空集合,那些频道从此再也找不到(2026-10-22)。
	draining, err := svcCtx.Redis.Smembers(worldDrainingSetKey(zone, confId))
	if err != nil {
		noteReadUnknown(zone, readUnknownSiteOrphanCleanup, fmt.Sprintf("draining set of conf %d", confId), err)
		return false
	}
	if len(draining) > 0 {
		logx.Infof("[OrphanCleanup] zone=%d conf=%d: also cleaning %d draining channel(s)",
			zone, confId, len(draining))
		members = append(members, draining...)
	}

	// 再入屏障:整组一起判、一起跳过。只要还有一个频道的属主节点刚判死
	// (C++ 那边可能仍在 SavePlayer),就整组留到下一次 fullSync 再清 ——
	// 这里的清理会 Del 掉 scene:{id}:node 与成员集合,属于"销毁"一类,同样受
	// 屏障约束。整组一起判,是为了让「这一组这一轮拆不拆」只有一个结论。
	//
	// 推迟摘除态(GO-6:已判死,但 death_at 没落地、节点还留在负载集)也要过屏障:这时
	// IsNodeAlive 为真,只看它会让整组直接进下面的删除循环(对死节点发 DestroyScene 失败被忽略,
	// 然后删 scene:{id}:node),在老节点 drain 期间改写归属。
	//
	// 两段式(2026-10-23):先把整组每个频道要用的读全部做完(属主),同时过屏障;任一读不到
	// 或被屏障挡住就整组留到下一次 fullSync,此时一个键都没动。以前边读边删,读到一半失败就留下
	// 「集合里还列着、映射已删」的半清理组:任意副本上的 EnterScene 会把映射已删的频道当陈旧频道懒改派,
	// 把刚清掉的孤儿复活。
	plan := make([]orphanSceneTeardown, 0, len(members))
	planned := make(map[uint64]struct{}, len(members))
	for _, m := range members {
		sceneId, err := strconv.ParseUint(m, 10, 64)
		if err != nil || sceneId == 0 {
			continue
		}
		// 崩溃窗口里同一个频道可能同时在频道集合与排空集合里:只拆一次。
		if _, dup := planned[sceneId]; dup {
			continue
		}
		planned[sceneId] = struct{}{}
		// 属主读错当「没有属主」会绕过屏障,而下面会删掉映射。
		nodeId, err := svcCtx.Redis.Get(sceneNodeKey(sceneId))
		if err != nil {
			noteReadUnknown(zone, readUnknownSiteOrphanCleanup, fmt.Sprintf("owner of scene %d (conf %d)", sceneId, confId), err)
			return false
		}
		if nodeId != "" && (!IsNodeAlive(svcCtx, zone, nodeId) || isNodeDetachDeferred(zone, nodeId)) &&
			reentryBarrierBlocks(svcCtx, zone, nodeId, barrierSiteOrphanCleanup) {
			logx.Infof("[OrphanCleanup] zone=%d conf=%d: scene %d owner node %s just declared dead; deferring entire group until next fullSync",
				zone, confId, sceneId, nodeId)
			return false
		}
		// 镜像源不在这里读:认领脚本在删除之前读出它、随快照返回(scene_atomic.go luaSceneClaimCommit)。
		plan = append(plan, orphanSceneTeardown{sceneId: sceneId, nodeId: nodeId})
	}

	drainingKey := worldDrainingSetKey(zone, confId)
	complete := true
	for _, s := range plan {
		if !tearDownOrphanScene(ctx, svcCtx, zone, confId, setKey, drainingKey, s) {
			complete = false
		}
	}
	if !complete {
		// 已拆掉的频道都已从集合里摘掉,剩下的成员映射都还在,下一次 fullSync 接着拆。
		return false
	}

	svcCtx.Redis.Del(drainingKey)
	// 线号表与分线目录(world_channel_directory.go)同样按 (zone, conf) 存。这张图已不在 World 表里,
	// 目录发布循环不会再碰它们:线号表没有 TTL,不删就是永久垃圾;目录有 TTL,顺手删只是让客户端
	// 不必再等它过期。两把键刻意不用 world_channels:zone: 前缀(滚动窗口里仍在跑的旧版本 scene_manager
	// 按这个前缀 SCAN,会把它们当成频道集合;merge_zone 也按这个前缀清源 zone),所以要在这里点名删。
	svcCtx.Redis.Del(worldChannelLineNoKey(zone, confId))
	svcCtx.Redis.Del(worldChannelDirectoryKey(zone, confId))

	if _, err := svcCtx.Redis.Del(setKey); err != nil {
		logx.Errorf("[OrphanCleanup] del(%s) failed: %v", setKey, err)
		return false
	}

	// 期望频道数是自动伸缩的权威。不清的话这张图将来被加回 World 表时,
	// 会直接复活上次伸缩到的数量(比如伸到过 8),而不是回到配置种子。
	//
	// 必须**最后**删(2026-10-16):期望数字段是 CleanupOrphanWorldChannels 的发现索引。先删它、再删集合
	// 时中途失败,这组孤儿就再也找不到了;放在最后,前面任何一步失败都能在下一次 fullSync 被重新发现,
	// 对已空的集合重跑是幂等的。
	desiredField := strconv.FormatUint(confId, 10)
	if _, err := svcCtx.Redis.Hdel(worldDesiredChannelsKey(zone), desiredField); err != nil {
		logx.Errorf("[OrphanCleanup] hdel(%s, %s) failed; the group will be retried on next fullSync: %v",
			worldDesiredChannelsKey(zone), desiredField, err)
		return false
	}
	logx.Infof("[OrphanCleanup] removed orphan channel set zone=%d conf=%d scenes=%d",
		zone, confId, len(members))
	return true
}

// orphanSceneTeardown 是孤儿频道组里一个频道的拆除计划:第一段读出来的属主。
type orphanSceneTeardown struct {
	sceneId uint64
	nodeId  string // 读到的属主;空串 = 映射已经没了
}

// tearDownOrphanScene 拆掉一个孤儿频道,返回它是否已拆完(或本来就没什么可拆)。认领之后的副作用由拆除任务
// 保证做完(runWorldOrphanTeardown),所以认领到就回 true。
//
// 场景键由 atomicTeardownScene 按第一段读到的属主一次性认领并删除,同一步里把它从频道集合与排空集合
// 摘掉:第一段读完属主到这里之间,属主若被改写(有 → 别的,或没有 → 有;任意副本上的懒改派等),
// 一个键都不动,回 false 留给下一次 fullSync;集合里列着的频道映射始终都还在 —— 整组没拆完时也不会留下
// 「列着、映射却已删」的成员。
func tearDownOrphanScene(ctx context.Context, svcCtx *svc.ServiceContext, zone uint32, confId uint64, setKey, drainingKey string, s orphanSceneTeardown) bool {
	req := sceneTeardownRequest{
		ZoneID: zone, SceneID: s.sceneId, Kind: teardownKindWorldOrphan, Reason: "orphan_channel_cleanup",
		ExpectedOwner: s.nodeId,
		// 只扣活属主的 scene_count:死属主的计数键由死节点收尾整个删掉,理由同 finishDrainedChannel。
		CountOwner: s.nodeId != "" && IsNodeAlive(svcCtx, zone, s.nodeId),
		UnlistKeys: []string{setKey, drainingKey},
	}
	claim, err := atomicTeardownScene(svcCtx, req)
	if err != nil {
		logx.Errorf("[OrphanCleanup] zone=%d conf=%d: atomic teardown of scene %d (expected owner %q) failed, outcome unknown; retrying next fullSync: %v",
			zone, confId, s.sceneId, s.nodeId, err)
		return false
	}
	if !claim.Claimed {
		logx.Infof("[OrphanCleanup] zone=%d conf=%d: owner of scene %d changed since it was read (expected %q); retrying next fullSync",
			zone, confId, s.sceneId, s.nodeId)
		return false
	}

	completeSceneTeardown(ctx, svcCtx, newSceneTeardownJob(req, claim), nil, false)
	return true
}

// runWorldOrphanTeardown 是孤儿频道认领之后的副作用(拆除任务 teardownKindWorldOrphan),认领方与扫描共用,可以重跑:
// 向属主发 DestroyScene、从镜像源的子集合摘掉自己、删自己的子集合、排空标记与放置代次、摘剩余成员、还 Agones 名额。
// 场景自己的状态键、属主的反向索引与计数、频道集合与排空集合的成员资格已由认领脚本处理。
func runWorldOrphanTeardown(ctx context.Context, svcCtx *svc.ServiceContext, job sceneTeardownJob) error {
	zone, sceneID, nodeID := job.ZoneID, job.SceneID, job.Claim.NodeID
	var deferred []string
	// 属主已死时跳过:RPC 只会卡在 gRPC 拨号超时上,实体随进程没了。
	if nodeID != "" && IsNodeAlive(svcCtx, zone, nodeID) {
		if err := RequestNodeDestroyScene(ctx, svcCtx, zone, nodeID, sceneID); err != nil {
			logx.Errorf("[OrphanCleanup] DestroyScene(node=%s, scene=%d) failed, left for the teardown sweep: %v", nodeID, sceneID, err)
			deferred = append(deferred, "DestroyScene on node "+nodeID)
		}
	}
	if err := unlinkFromSourceMirrors(svcCtx, zone, sceneID, job.Claim.Source); err != nil {
		deferred = append(deferred, "unlink from the source's mirror set")
	}
	svcCtx.Redis.Del(sceneMirrorsKey(sceneID))
	svcCtx.Redis.Del(sceneDrainingKey(sceneID))
	if err := deletePlacementGeneration(svcCtx, sceneID); err != nil {
		deferred = append(deferred, "delete the placement generation")
	}
	if nodeID != "" {
		removeResidualSceneMembers(svcCtx, sceneID, zone, nodeID, job.Claim.Members)
	}
	if err := releaseTeardownRoom(ctx, svcCtx, job); err != nil {
		deferred = append(deferred, "take the Agones release step")
	}
	return teardownDeferred(deferred)
}
