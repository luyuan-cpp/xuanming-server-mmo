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
//  3. Scan with a cursor, cap each call's work, so a huge key-space on
//     Redis doesn't block the LoadReporter goroutine.

const (
	orphanScanBatch int64 = 256
	orphanKeyPrefix       = "world_channels:zone:"
)

// CleanupOrphanWorldChannels scans Redis for world_channels:* keys whose
// (zone, confId) is not in the active set and deletes them. Returns the
// number of channel sets removed. Intended to run once on startup from
// fullSync; calling it repeatedly is safe but pointless.
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

	var cursor uint64
	removed := 0
	for {
		keys, next, err := svcCtx.Redis.Scan(cursor, orphanKeyPrefix+"*", orphanScanBatch)
		if err != nil {
			logx.Errorf("[OrphanCleanup] scan failed at cursor=%d: %v", cursor, err)
			return removed
		}
		for _, key := range keys {
			zone, confId, ok := parseWorldChannelsKey(key)
			if !ok {
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
			if deleteOrphanChannel(ctx, svcCtx, zone, confId, key) {
				removed++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	if removed > 0 {
		logx.Infof("[OrphanCleanup] removed %d orphan world_channels sets", removed)
	}
	return removed
}

// parseWorldChannelsKey parses "world_channels:zone:{z}:{confId}" into its
// components. Returns false for malformed keys so a stray entry doesn't
// blow up the whole cleanup pass.
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
	// (C++ 那边可能仍在 SavePlayerToRedis),就整组留到下一次 fullSync 再清 ——
	// 这里的清理会 Del 掉 scene:{id}:node 与人数键,属于"销毁"一类,同样受
	// 屏障约束。整组跳过而不是逐条跳过,是因为 setKey 只能在全部成员都处理完
	// 之后才删,半清理会留下一个指向已删映射的集合。
	for _, m := range members {
		sceneId, err := strconv.ParseUint(m, 10, 64)
		if err != nil || sceneId == 0 {
			continue
		}
		nodeId, _ := svcCtx.Redis.Get(fmt.Sprintf("scene:%d:node", sceneId))
		if nodeId != "" && !IsNodeAlive(svcCtx, zone, nodeId) &&
			reentryBarrierBlocks(svcCtx, zone, nodeId, barrierSiteOrphanCleanup) {
			logx.Infof("[OrphanCleanup] zone=%d conf=%d: 场景 %d 的属主节点 %s 刚判死,整组推迟到下一次 fullSync",
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
		svcCtx.Redis.Del(fmt.Sprintf(InstancePlayerCountKey, sceneId))
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

	// 期望频道数是自动伸缩的权威。不清的话这张图将来被加回 World 表时,
	// 会直接复活上次伸缩到的数量(比如伸到过 8),而不是回到配置种子。
	svcCtx.Redis.Hdel(worldDesiredChannelsKey(zone), strconv.FormatUint(confId, 10))
	svcCtx.Redis.Del(worldDrainingSetKey(zone, confId))

	if _, err := svcCtx.Redis.Del(setKey); err != nil {
		logx.Errorf("[OrphanCleanup] del(%s) failed: %v", setKey, err)
		return false
	}
	logx.Infof("[OrphanCleanup] removed orphan channel set zone=%d conf=%d scenes=%d",
		zone, confId, len(members))
	return true
}
