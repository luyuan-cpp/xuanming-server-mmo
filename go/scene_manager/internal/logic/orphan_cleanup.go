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
//  3. Point lookups only, never a keyspace SCAN (2026-10-16): candidate zones
//     come from the zone registry scene_manager:zones ∪ the zones with live
//     nodes, candidate (zone, confId) groups from the fields of
//     world_channels:desired:zone:{z}. The old SCAN world_channels:zone:*
//     walked the whole shared Redis on the LoadReporter goroutine — hours at
//     launch scale.
//
// 发现覆盖范围(2026-10-16):生产代码里往频道集合 SADD 的唯一入口是 initWorldScenesForZone,它先经
// DesiredWorldChannelCount 播种 world_channels:desired:zone:{z} 的字段、后 SADD,而播种前会登记 zone
// (zone_registry.go)。所以「有频道集合」基本蕴含「有期望数字段、zone 已登记」。覆盖不到的组合:
//   - DesiredWorldChannelCount 的 HSETNX 出错兜底路径(直接返回种子值、不写字段)建出来的集合;
//   - 升级前遗留、zone 又从未登记的数据(补登记见 docs/ops/scene-node-role-split.md §6.1)。
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

	registered, err := listRegisteredZones(svcCtx)
	if err != nil {
		logx.Errorf("[OrphanCleanup] read zone registry failed; checking zones with live nodes only: %v", err)
	}

	removed := 0
	for _, zone := range unionZones(registered, GetActiveZones()) {
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
	if draining, err := svcCtx.Redis.Smembers(worldDrainingSetKey(zone, confId)); err == nil && len(draining) > 0 {
		logx.Infof("[OrphanCleanup] zone=%d conf=%d: also cleaning %d draining channel(s)",
			zone, confId, len(draining))
		members = append(members, draining...)
	}

	// 再入屏障:整组一起判、一起跳过。只要还有一个频道的属主节点刚判死
	// (C++ 那边可能仍在 SavePlayer),就整组留到下一次 fullSync 再清 ——
	// 这里的清理会 Del 掉 scene:{id}:node 与人数键,属于"销毁"一类,同样受
	// 屏障约束。整组跳过而不是逐条跳过,是因为 setKey 只能在全部成员都处理完
	// 之后才删,半清理会留下一个指向已删映射的集合。
	//
	// 推迟摘除态(GO-6:已判死,但 death_at 没落地、节点还留在负载集)也要过屏障:这时
	// IsNodeAlive 为真,只看它会让整组直接进下面的删除循环(对死节点发 DestroyScene 失败被忽略,
	// 然后删 scene:{id}:node),在老节点 drain 期间改写归属。
	for _, m := range members {
		sceneId, err := strconv.ParseUint(m, 10, 64)
		if err != nil || sceneId == 0 {
			continue
		}
		nodeId, _ := svcCtx.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
		if nodeId != "" && (!IsNodeAlive(svcCtx, zone, nodeId) || isNodeDetachDeferred(zone, nodeId)) &&
			reentryBarrierBlocks(svcCtx, zone, nodeId, barrierSiteOrphanCleanup) {
			logx.Infof("[OrphanCleanup] zone=%d conf=%d: scene %d owner node %s just declared dead; deferring entire group until next fullSync",
				zone, confId, sceneId, nodeId)
			return false
		}
	}

	for _, m := range members {
		sceneId, err := strconv.ParseUint(m, 10, 64)
		if err != nil || sceneId == 0 {
			continue
		}
		nodeKey := fmt.Sprintf("scene:%d:node", sceneId)
		nodeId, _ := svcCtx.Redis.Get(nodeKey)
		sceneStr := m

		// Best-effort DestroyScene on the hosting node. If the node is
		// dead the RPC would hang on the gRPC dial timeout; skip.
		if nodeId != "" && IsNodeAlive(svcCtx, zone, nodeId) {
			if err := RequestNodeDestroyScene(ctx, svcCtx, zone, nodeId, sceneId); err != nil {
				logx.Infof("[OrphanCleanup] DestroyScene(node=%s, scene=%d) failed (ignored): %v",
					nodeId, sceneId, err)
			}
			svcCtx.Redis.Decr(nodeSceneCountKey(zone, nodeId))
		}

		// Drop everything scene-scoped, including the reverse indexes and
		// mirror flags introduced for co-location / cascade destroy.
		if nodeId != "" {
			svcCtx.Redis.Srem(nodeScenesKey(zone, nodeId), sceneStr)
		}
		if src, _ := svcCtx.Redis.Get(sceneSourceKey(sceneId)); src != "" {
			if srcId, err := strconv.ParseUint(src, 10, 64); err == nil {
				svcCtx.Redis.Srem(sceneMirrorsKey(srcId), sceneStr)
			}
		}
		svcCtx.Redis.Del(nodeKey)
		svcCtx.Redis.Del(sceneZoneKey(sceneId))
		svcCtx.Redis.Del(instanceMembersKey(sceneId))
		svcCtx.Redis.Del(fmt.Sprintf(SceneMirrorFlagKeyFmt, sceneId))
		svcCtx.Redis.Del(sceneSourceKey(sceneId))
		svcCtx.Redis.Del(sceneMirrorsKey(sceneId))

		// Agones 名额必须在删映射之前读出来并归还,否则这份容量在 GameServer 的
		// rooms Counter 上永久占着 —— 映射一删就再没人知道该向谁还了。
		if gs, _ := svcCtx.Redis.Get(sceneAgonesGsKey(sceneId)); gs != "" {
			ReleaseAgonesRoomForScene(ctx, svcCtx, sceneId, gs, "orphan_channel_cleanup")
		}
		svcCtx.Redis.Del(sceneAgonesGsKey(sceneId))
		svcCtx.Redis.Del(sceneDrainingKey(sceneId))
	}

	svcCtx.Redis.Del(worldDrainingSetKey(zone, confId))
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
