package logic

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 大世界频道按人数自动扩缩容。
//
// 口径(见 docs/design/world-channel-autoscale.md):
//   - 人数 = `instance:{sceneId}:player_count`,**按频道**算,不是按进程。
//   - 扩容:该地图**所有**频道都 >= ScaleOutPlayerThreshold 才加一个频道。
//     要求"所有"而不是"任一",是因为负载不均时(新频道空、老频道满)按
//     "任一"会连续扩出一堆空频道。
//   - 缩容:某频道 < ScaleInPlayerThreshold 时把它排空 —— 玩家被强制改派到
//     同图的其它频道(大世界)—— 然后销毁。
//   - 每个大世界地图**至少保留 1 个频道**,永远不会缩到 0。
//
// 为什么频道数要落在 Redis 而不是配置:
//
//	`initWorldScenesForZone` 会按"期望频道数"把缺的频道补齐,它在 fullSync 和
//	每次节点 PUT 事件时都会跑。如果期望值仍然直接读 `ChannelCountFor(confId)`
//	这个静态配置,自动缩容刚摘掉的频道会在几秒内被它重新建出来 —— 缩容根本
//	不成立。所以期望值下沉到 Redis,配置只作为**初始种子**,扩缩容改的是
//	Redis 里那份,两边共用同一个权威。
const (
	// WorldDesiredChannelsKeyFmt: hash confId -> 期望频道数。
	WorldDesiredChannelsKeyFmt = "world_channels:desired:zone:%d"
	// SceneDrainingKeyFmt: 该频道正在排空(已从路由集合摘除,等玩家走干净)。
	// 带 TTL,防止进程在排空中途挂掉后留下永久标记。
	SceneDrainingKeyFmt = "scene:%d:draining"
	// WorldAutoscaleCooldownKeyFmt: 每个 (zone, confId) 的冷却窗口。
	// 扩完不马上缩、缩完不马上扩,避免在阈值边缘抖动。
	WorldAutoscaleCooldownKeyFmt = "world_channels:cooldown:zone:%d:%d"
	// WorldDrainingSetKeyFmt: 正在排空的频道集合(SET of sceneId)。
	// 需要单独一个索引:排空的第一步就是把频道从 world_channels 摘掉,
	// 摘掉之后 GetAllWorldChannels 就再也看不到它,没有这个集合就没法
	// 在后续 tick 里找回来继续收敛。
	WorldDrainingSetKeyFmt = "world_channels:draining:zone:%d:%d"
)

func worldDrainingSetKey(zoneID uint32, confID uint64) string {
	return fmt.Sprintf(WorldDrainingSetKeyFmt, zoneID, confID)
}

func worldDesiredChannelsKey(zoneID uint32) string {
	return fmt.Sprintf(WorldDesiredChannelsKeyFmt, zoneID)
}

func sceneDrainingKey(sceneID uint64) string {
	return fmt.Sprintf(SceneDrainingKeyFmt, sceneID)
}

func worldAutoscaleCooldownKey(zoneID uint32, confID uint64) string {
	return fmt.Sprintf(WorldAutoscaleCooldownKeyFmt, zoneID, confID)
}

// DesiredWorldChannelCount 返回某地图当前的期望频道数。
//
// 第一次访问时用配置里的 ChannelCountFor 播种。之后以 Redis 为准 ——
// 改配置不会覆盖已经自动伸缩过的值(否则一次重启就把伸缩结果抹掉了)。
// 运维想强制回到配置值,删掉这个 hash field 即可。
func DesiredWorldChannelCount(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) int {
	key := worldDesiredChannelsKey(zoneID)
	field := strconv.FormatUint(confID, 10)

	if raw, err := svcCtx.Redis.Hget(key, field); err == nil && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 {
			return n
		}
	}

	seed := svcCtx.Config.ChannelCountFor(confID)
	if seed < 1 {
		seed = 1
	}
	// Hsetnx:并发的多个 SceneManager 实例同时播种时,只有一个赢,
	// 其余读到已有值。不能用 Hset —— 那会把别人已经伸缩过的值冲掉。
	if _, err := svcCtx.Redis.Hsetnx(key, field, strconv.Itoa(seed)); err != nil {
		logx.Errorf("[WorldAutoscale] seed desired channels zone=%d conf=%d failed: %v", zoneID, confID, err)
		return seed
	}
	if raw, err := svcCtx.Redis.Hget(key, field); err == nil && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 {
			return n
		}
	}
	return seed
}

// setDesiredWorldChannelCount 写回期望频道数,并钳到 [min, max]。
func setDesiredWorldChannelCount(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, n int) {
	minCh := svcCtx.Config.WorldAutoscale.MinChannelsPerMap
	if minCh < 1 {
		minCh = 1 // 硬下限:每个大世界地图至少 1 个频道,不接受配置成 0
	}
	maxCh := svcCtx.Config.WorldAutoscale.MaxChannelsPerMap
	if maxCh > 0 && n > maxCh {
		n = maxCh
	}
	if n < minCh {
		n = minCh
	}
	if err := svcCtx.Redis.Hset(worldDesiredChannelsKey(zoneID), strconv.FormatUint(confID, 10), strconv.Itoa(n)); err != nil {
		logx.Errorf("[WorldAutoscale] set desired channels zone=%d conf=%d=%d failed: %v", zoneID, confID, n, err)
	}
}

// StartWorldAutoscaler 起一个周期性扩缩容循环。
// IntervalSeconds<=0 或 Enabled=false 时直接返回,不起 goroutine。
func StartWorldAutoscaler(ctx context.Context, svcCtx *svc.ServiceContext) {
	cfg := svcCtx.Config.WorldAutoscale
	if !cfg.Enabled {
		logx.Info("[WorldAutoscale] disabled")
		return
	}
	interval := cfg.CheckIntervalSeconds
	if interval <= 0 {
		interval = 30
	}

	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		logx.Infof("[WorldAutoscale] started: interval=%ds scale_out>=%d scale_in<%d min=%d max=%d",
			interval, cfg.ScaleOutPlayerThreshold, cfg.ScaleInPlayerThreshold,
			cfg.MinChannelsPerMap, cfg.MaxChannelsPerMap)

		for {
			select {
			case <-ctx.Done():
				logx.Info("[WorldAutoscale] stopped")
				return
			case <-ticker.C:
				// 多副本时只有领导者做伸缩决策 —— 两份 autoscaler 并发对同一批
				// zone 扩缩正是部署清单曾用 replicas:1 + Recreate 兜的口子,
				// 现在由 shared/leader 选主收敛(接线见 scene_manager_service.go)。
				if !isLeader() {
					continue
				}
				for _, zoneID := range GetActiveZones() {
					AutoscaleWorldChannelsForZone(ctx, svcCtx, zoneID)
				}
			}
		}
	}()
}

// channelLoad 是一个频道的人数快照。
type channelLoad struct {
	sceneID   string
	sceneID64 uint64
	players   int64
}

// AutoscaleWorldChannelsForZone 对一个 zone 跑一轮扩缩容。
// 返回 (扩容次数, 开始排空的频道数),导出是为了让单测直接驱动一轮。
func AutoscaleWorldChannelsForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) (int, int) {
	// 先收尾上一轮还没排干净的频道,再考虑新的伸缩决策。
	sweepDrainingWorldChannels(ctx, svcCtx, zoneID)

	scaledOut, scaledIn := 0, 0
	for _, confID := range worldConfIds() {
		out, in := autoscaleOneWorldMap(ctx, svcCtx, zoneID, confID)
		scaledOut += out
		scaledIn += in
	}
	return scaledOut, scaledIn
}

func autoscaleOneWorldMap(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) (int, int) {
	cfg := svcCtx.Config.WorldAutoscale

	channels, err := GetAllWorldChannels(ctx, svcCtx, confID, zoneID)
	if err != nil || len(channels) == 0 {
		return 0, 0
	}

	loads := make([]channelLoad, 0, len(channels))
	for _, sceneID := range channels {
		// 正在排空的频道不参与决策:它已经从路由集合里摘掉了,
		// 它身上剩的人正在被改派走。
		if exists, _ := svcCtx.Redis.Exists(sceneDrainingKey(sceneID)); exists {
			continue
		}
		loads = append(loads, channelLoad{
			sceneID:   strconv.FormatUint(sceneID, 10),
			sceneID64: sceneID,
			players:   readScenePlayerCount(svcCtx, sceneID),
		})
	}
	if len(loads) == 0 {
		return 0, 0
	}

	// 人少的排前面,缩容挑第一个、扩容看最后一个。
	sort.Slice(loads, func(i, j int) bool { return loads[i].players < loads[j].players })

	if inCooldown(svcCtx, zoneID, confID) {
		return 0, 0
	}

	minCh := cfg.MinChannelsPerMap
	if minCh < 1 {
		minCh = 1
	}

	// ── 缩容 ──────────────────────────────────────────────────────────
	// 条件:最闲的频道人数 < 阈值,且缩掉之后还剩至少 minCh 个频道,
	// 且其余频道装得下它的人(不能把人并过去反而把别人推过扩容线)。
	lightest := loads[0]
	if len(loads) > minCh && lightest.players < cfg.ScaleInPlayerThreshold {
		if victim, ok := pickScaleInVictim(svcCtx, loads, cfg); ok {
			if beginDrainWorldChannel(ctx, svcCtx, zoneID, confID, victim) {
				markCooldown(svcCtx, zoneID, confID)
				return 0, 1
			}
		} else {
			logx.Infof("[WorldAutoscale] zone=%d conf=%d: channel %s has %d players (<%d) but no channel "+
				"is safe to drain (no headroom, or it is a mirror source); keeping them all",
				zoneID, confID, lightest.sceneID, lightest.players, cfg.ScaleInPlayerThreshold)
		}
	}

	// ── 扩容 ──────────────────────────────────────────────────────────
	// 要求**所有**频道都到线。loads 已按人数升序,查最闲的那个就够。
	if lightest.players >= cfg.ScaleOutPlayerThreshold {
		desired := DesiredWorldChannelCount(svcCtx, zoneID, confID)
		if cfg.MaxChannelsPerMap > 0 && desired >= cfg.MaxChannelsPerMap {
			logx.Errorf("[WorldAutoscale] zone=%d conf=%d: every channel is at/over %d players but "+
				"MaxChannelsPerMap=%d is reached; the map is over capacity",
				zoneID, confID, cfg.ScaleOutPlayerThreshold, cfg.MaxChannelsPerMap)
			metrics.ObserveWorldAutoscale(zoneID, "scale_out", "max_reached")
			return 0, 0
		}

		setDesiredWorldChannelCount(svcCtx, zoneID, confID, desired+1)
		// 立刻把新频道建出来,不等下一次 fullSync —— 玩家现在就挤着。
		// waitForLock=true:跑在 autoscaler 自己的 goroutine 上,等锁无害,
		// 而期望频道数已 +1,尽快落地。
		initWorldScenesForZone(ctx, svcCtx, zoneID, []uint64{confID}, true)
		markCooldown(svcCtx, zoneID, confID)

		logx.Infof("[WorldAutoscale] zone=%d conf=%d: all %d channel(s) >= %d players, scaled out to %d",
			zoneID, confID, len(loads), cfg.ScaleOutPlayerThreshold, desired+1)
		metrics.ObserveWorldAutoscale(zoneID, "scale_out", "ok")
		return 1, 0
	}

	return 0, 0
}

// hasHeadroomFor 判断把 victim 的人并进其余频道后,是否会把某个频道推过扩容线。
//
// 这条检查不是洁癖:少了它,缩容会把人挤进别的频道触发扩容,扩容又让某个
// 频道人数掉到 100 以下触发缩容 —— 自激振荡,玩家被反复强制改派。
func hasHeadroomFor(loads []channelLoad, victim channelLoad, scaleOutThreshold int64) bool {
	var headroom int64
	for _, l := range loads {
		if l.sceneID64 == victim.sceneID64 {
			continue
		}
		if room := scaleOutThreshold - l.players; room > 0 {
			headroom += room
		}
	}
	return headroom >= victim.players
}

// beginDrainWorldChannel 开始排空一个频道。
//
// 顺序是**先摘路由再排空**:先从 world_channels 集合里 SREM,新玩家就不会再被
// 路由进来;然后才让 C++ 把残留玩家改派走。反过来做的话,刚被改派出去的玩家
// 有可能又被分回这个正在销毁的频道。
// channelHasMirrors 报告是否有镜像 Scene 以这个频道为源。
//
// 查询失败时返回 true(当作"有镜像")—— 这是刻意的 fail-closed 方向:
// 缩容是可选的省钱动作,查不清就别动,比误伤镜像里的玩家划算。
func channelHasMirrors(svcCtx *svc.ServiceContext, sceneID uint64) bool {
	members, err := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneID))
	if err != nil {
		logx.Errorf("[WorldAutoscale] cannot read mirrors of scene %d (%v); treating it as a mirror source",
			sceneID, err)
		return true
	}
	return len(members) > 0
}

// pickScaleInVictim 在升序的 loads 里挑第一个可以安全排空的频道。
//
// 三个条件都要满足:
//  1. 人数低于缩容线;
//  2. **不是镜像源**。镜像与源共置(见 scene-creation-architecture.md 的
//     mirror co-location),源频道被销毁的话镜像会变成谁也进不去的孤儿。
//     节点死亡那条路径是强制级联销毁镜像的,但缩容是可选动作 —— 宁可少省
//     一个频道,也不要把镜像里的玩家踢下线;
//  3. 其余频道装得下它的人,且不会因此把某个频道推过扩容线(否则缩完立刻扩,来回抖)。
//
// 不是只看 loads[0]:最闲的那个恰好托着镜像时,继续往下找仍然能缩容。
func pickScaleInVictim(svcCtx *svc.ServiceContext, loads []channelLoad, cfg config.WorldAutoscaleConfig) (channelLoad, bool) {
	for _, candidate := range loads {
		if candidate.players >= cfg.ScaleInPlayerThreshold {
			// loads 升序,后面的只会更多人。
			break
		}
		if channelHasMirrors(svcCtx, candidate.sceneID64) {
			logx.Infof("[WorldAutoscale] channel %s is a mirror source; not draining it", candidate.sceneID)
			continue
		}
		if !hasHeadroomFor(loads, candidate, cfg.ScaleOutPlayerThreshold) {
			continue
		}
		return candidate, true
	}
	return channelLoad{}, false
}

func beginDrainWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, victim channelLoad) bool {
	channelSetKey := worldChannelsKey(zoneID, confID)

	removed, err := svcCtx.Redis.Srem(channelSetKey, victim.sceneID)
	if err != nil {
		logx.Errorf("[WorldAutoscale] zone=%d conf=%d: SREM channel %s failed: %v",
			zoneID, confID, victim.sceneID, err)
		metrics.ObserveWorldAutoscale(zoneID, "scale_in", "error")
		return false
	}
	if removed == 0 {
		// 别的实例抢先摘掉了。不重复处理。
		return false
	}

	// 期望频道数同步减 1,否则 initWorldScenesForZone 会立刻把它补回来。
	desired := DesiredWorldChannelCount(svcCtx, zoneID, confID)
	setDesiredWorldChannelCount(svcCtx, zoneID, confID, desired-1)

	// 排空标记带 TTL:进程在排空中途挂掉时,标记会自己过期,
	// 下一轮 sweep 会重新接手,不会留下永久"半死"频道。
	ttl := svcCtx.Config.WorldAutoscale.DrainTimeoutSeconds
	if ttl <= 0 {
		ttl = 300
	}
	if err := svcCtx.Redis.Setex(sceneDrainingKey(victim.sceneID64), "1", int(ttl)); err != nil {
		logx.Errorf("[WorldAutoscale] mark draining scene %s failed: %v", victim.sceneID, err)
	}
	// 排空索引:摘掉路由之后,只有这个集合还知道它的存在。
	if _, err := svcCtx.Redis.Sadd(worldDrainingSetKey(zoneID, confID), victim.sceneID); err != nil {
		logx.Errorf("[WorldAutoscale] index draining scene %s failed: %v", victim.sceneID, err)
	}

	logx.Infof("[WorldAutoscale] zone=%d conf=%d: draining channel %s (%d players < %d); "+
		"removed from routing, relocating residents to the remaining channels",
		zoneID, confID, victim.sceneID, victim.players, svcCtx.Config.WorldAutoscale.ScaleInPlayerThreshold)
	metrics.ObserveWorldAutoscale(zoneID, "scale_in", "ok")

	// 第一次 DestroyScene:C++ 侧发现还有人就只改派、不销毁实体,
	// 由 sweep 在后续 tick 收敛。
	drainOrDestroyChannel(ctx, svcCtx, zoneID, confID, victim.sceneID64)
	return true
}

// sweepDrainingWorldChannels 收敛所有正在排空的频道。
//
// 每拍重新观察真实状态(人数),而不是记"我做到第几步了"。C++ 的
// DestroyScene 是幂等的:还有人就改派,没人就真销毁。
func sweepDrainingWorldChannels(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) {
	for _, confID := range worldConfIds() {
		members, err := svcCtx.Redis.Smembers(worldDrainingSetKey(zoneID, confID))
		if err != nil || len(members) == 0 {
			continue
		}
		for _, m := range members {
			sceneID, err := strconv.ParseUint(m, 10, 64)
			if err != nil || sceneID == 0 {
				svcCtx.Redis.Srem(worldDrainingSetKey(zoneID, confID), m)
				continue
			}
			drainOrDestroyChannel(ctx, svcCtx, zoneID, confID, sceneID)
		}
	}
}

// drainOrDestroyChannel 对一个正在排空的频道推进一步。
func drainOrDestroyChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, sceneID uint64) {
	nodeID, _ := svcCtx.Redis.Get(sceneNodeKey(sceneID))
	if nodeID == "" {
		// 映射已经没了 —— 频道已经销毁完,或者节点死亡时被清理过。
		finishDrainedChannel(ctx, svcCtx, zoneID, confID, sceneID, "")
		return
	}
	if isKnownNodeIdentityAmbiguous(zoneID, nodeID) {
		logx.Errorf("[WorldAutoscale] draining channel %d: zone=%d node=%s has duplicate registrations; refusing cleanup",
			sceneID, zoneID, nodeID)
		return
	}

	if !IsNodeAlive(svcCtx, zoneID, nodeID) {
		// 节点已死:实体随进程一起没了,玩家会走各自的掉线/重连路径。
		// 这里只负责把 Redis 状态清干净。
		logx.Infof("[WorldAutoscale] draining channel %d: node %s is gone, cleaning up state",
			sceneID, nodeID)
		finishDrainedChannel(ctx, svcCtx, zoneID, confID, sceneID, nodeID)
		return
	}

	players := readScenePlayerCount(svcCtx, sceneID)

	// 无论有没有人都调:有人时 C++ 会把他们改派到大世界并保留实体,
	// 没人时才真正销毁。幂等,可以每拍调。
	if err := RequestNodeDestroyScene(ctx, svcCtx, zoneID, nodeID, sceneID); err != nil {
		logx.Errorf("[WorldAutoscale] draining channel %d: DestroyScene on node %s failed: %v",
			sceneID, nodeID, err)
		return
	}

	if players > 0 {
		logx.Infof("[WorldAutoscale] draining channel %d: %d player(s) still relocating", sceneID, players)
		return
	}

	finishDrainedChannel(ctx, svcCtx, zoneID, confID, sceneID, nodeID)
}

// finishDrainedChannel 清掉一个排空完成的频道的全部 Redis 状态。
func finishDrainedChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, sceneID uint64, nodeID string) {
	sceneIDStr := strconv.FormatUint(sceneID, 10)

	// Agones 名额要在删映射之前读出来,否则就找不到该向谁归还了。
	agonesGs, _ := svcCtx.Redis.Get(sceneAgonesGsKey(sceneID))

	// 级联兜底:排空窗口里可能又有镜像以这个频道为源建起来了
	// (pickScaleInVictim 只保证**开始排空那一刻**没有镜像)。
	// 源频道马上要没了,留着镜像就是谁也进不去的孤儿,而且 scene:{id}:mirrors
	// 这个键会永久泄漏。既有的两条销毁路径都级联了
	// (destroyInstanceInternal 的 cascade、migrateWorldChannel 的
	// cascadeMirrorsOnSourceMigration),这里必须对齐。
	mirrorChildren, _ := svcCtx.Redis.Smembers(sceneMirrorsKey(sceneID))
	for _, mid := range mirrorChildren {
		childID, err := strconv.ParseUint(mid, 10, 64)
		if err != nil || childID == sceneID {
			continue
		}
		childZone := GetSceneZone(svcCtx, childID)
		if childZone == 0 {
			childZone = zoneID
		}
		logx.Infof("[WorldAutoscale] cascade-destroying mirror %d whose source channel %d is being scaled in",
			childID, sceneID)
		destroyInstanceForce(ctx, svcCtx, childZone, childID, "source_scaled_in")
	}
	svcCtx.Redis.Del(sceneMirrorsKey(sceneID))

	svcCtx.Redis.Del(sceneNodeKey(sceneID))
	svcCtx.Redis.Del(sceneZoneKey(sceneID))
	svcCtx.Redis.Del(fmt.Sprintf(InstancePlayerCountKey, sceneID))
	svcCtx.Redis.Del(sceneAgonesGsKey(sceneID))
	svcCtx.Redis.Del(sceneDrainingKey(sceneID))
	svcCtx.Redis.Srem(worldDrainingSetKey(zoneID, confID), sceneIDStr)
	if nodeID != "" {
		svcCtx.Redis.Srem(nodeScenesKey(zoneID, nodeID), sceneIDStr)
		svcCtx.Redis.Incrby(nodeSceneCountKey(zoneID, nodeID), -1)
	}
	if agonesGs != "" {
		ReleaseAgonesRoomForScene(ctx, svcCtx, sceneID, agonesGs, "world_channel_scale_in")
	}

	logx.Infof("[WorldAutoscale] channel %d fully drained and removed (zone=%d node=%s)",
		sceneID, zoneID, nodeID)
	metrics.ObserveWorldAutoscale(zoneID, "scale_in", "drained")
}

// readScenePlayerCount 读一个场景的在线人数。读不到当 0。
func readScenePlayerCount(svcCtx *svc.ServiceContext, sceneID uint64) int64 {
	raw, err := svcCtx.Redis.Get(fmt.Sprintf(InstancePlayerCountKey, sceneID))
	if err != nil || raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func inCooldown(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) bool {
	exists, err := svcCtx.Redis.Exists(worldAutoscaleCooldownKey(zoneID, confID))
	return err == nil && exists
}

func markCooldown(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) {
	sec := svcCtx.Config.WorldAutoscale.CooldownSeconds
	if sec <= 0 {
		sec = 120
	}
	if err := svcCtx.Redis.Setex(worldAutoscaleCooldownKey(zoneID, confID), "1", int(sec)); err != nil {
		logx.Errorf("[WorldAutoscale] set cooldown zone=%d conf=%d failed: %v", zoneID, confID, err)
	}
}
