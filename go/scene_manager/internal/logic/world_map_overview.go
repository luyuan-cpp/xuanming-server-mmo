package logic

import (
	"fmt"
	"sort"
	"time"

	scenepb "proto/scene"

	"scene_manager/internal/config"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 世界地图总览读模型与双层潮汐(docs/design/world-map-play-v2.md §2.3–§2.4)。
//
// 为什么复用分线目录周期发布：
//   - 所有分线目录刚在同一拍由领导者算完，无需重复扫描成员集合与节点存活；
//   - 与 world_channel_directory:{ctl:<zone>}:<conf> 共用 {ctl:<zone>} hash tag，
//     C++ scene 节点可在同槽一次 MGET 内同时取回本图目录与全服总览；
//   - 灵潮与今日旺地按墙钟确定性推导，领导者切换或跨 Go/Java 实现天然同口径，无额外持久化状态。
const (
	// WorldMapOverviewKeyFmt: STRING per zone，值是 scenepb.WorldMapOverview 的序列化字节，带 TTL。
	//
	// **跨语言契约，渲染出来的键不许单边改**：C++ scene 节点按同一键形只读（PlayerSceneSystem::WorldMapOverviewKey）。
	// 渲染结果：world_map_overview:{ctl:<zone 十进制>}。
	WorldMapOverviewKeyFmt = "world_map_overview:%s"

	// 东八区 05:00 跨日边界 = UTC 21:00，对应 +3h 偏移再按 86400s 取整。
	dailyHotspotOffsetSeconds int64 = 3 * 3600
	secondsPerDay             int64 = 86400
)

func worldMapOverviewKey(zoneID uint32) string {
	return fmt.Sprintf(WorldMapOverviewKeyFmt, sceneControlTag(zoneID))
}

// worldTideAt 按墙钟确定性计算当前所属灵潮轮次。纯函数。
func worldTideAt(nowUnix int64, candidates []uint32, intervalSeconds, durationSeconds int64) *scenepb.WorldTideSnapshot {
	if len(candidates) == 0 || intervalSeconds <= 0 {
		return &scenepb.WorldTideSnapshot{}
	}
	if nowUnix < 0 {
		nowUnix = 0
	}
	if durationSeconds < 0 {
		durationSeconds = 0
	}
	if durationSeconds > intervalSeconds {
		durationSeconds = intervalSeconds
	}
	roundIndex := nowUnix / intervalSeconds
	startAt := roundIndex * intervalSeconds
	endAt := startAt + durationSeconds
	nextStartAt := startAt + intervalSeconds
	sceneConfID := candidates[uint64(roundIndex)%uint64(len(candidates))]
	return &scenepb.WorldTideSnapshot{
		Active:         nowUnix < endAt,
		RoundId:        uint64(startAt),
		SceneConfigId:  sceneConfID,
		StartsAtMs:     uint64(startAt) * 1000,
		EndsAtMs:       uint64(endAt) * 1000,
		NextStartsAtMs: uint64(nextStartAt) * 1000,
	}
}

// dailyHotspotAt 按东八区 05:00 跨日确定性计算今日旺地地图。纯函数。
func dailyHotspotAt(nowUnix int64, candidates []uint32) uint32 {
	if len(candidates) == 0 {
		return 0
	}
	if nowUnix < 0 {
		nowUnix = 0
	}
	dayIndex := (nowUnix + dailyHotspotOffsetSeconds) / secondsPerDay
	return candidates[uint64(dayIndex)%uint64(len(candidates))]
}

// classifyWorldMapHeat 将地图总在线人数映射为三档热度。纯函数。
func classifyWorldMapHeat(totalPlayers, crowdedThreshold uint32) scenepb.WorldMapHeatLevel {
	if totalPlayers == 0 {
		return scenepb.WorldMapHeatLevel_WORLD_MAP_HEAT_LEVEL_QUIET
	}
	if crowdedThreshold == 0 {
		crowdedThreshold = 1
	}
	if totalPlayers >= crowdedThreshold {
		return scenepb.WorldMapHeatLevel_WORLD_MAP_HEAT_LEVEL_CROWDED
	}
	return scenepb.WorldMapHeatLevel_WORLD_MAP_HEAT_LEVEL_ACTIVE
}

// pickRecommendedChannel 从已按线号升序排好的分线目录里挑出推荐落脚线（最小可用线号）。纯函数。
func pickRecommendedChannel(directory *scenepb.SceneChannelDirectory) (uint64, uint32) {
	if directory == nil {
		return 0, 0
	}
	var fallbackScene uint64
	var fallbackNo uint32
	for _, ch := range directory.GetChannels() {
		if ch.GetSceneId() == 0 {
			continue
		}
		switch ch.GetState() {
		case scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH,
			scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY:
			return ch.GetSceneId(), ch.GetChannelNo()
		case scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL:
			if fallbackScene == 0 {
				fallbackScene = ch.GetSceneId()
				fallbackNo = ch.GetChannelNo()
			}
		}
	}
	return fallbackScene, fallbackNo
}

func isMerchantActiveOnMap(merchant *scenepb.MysteryMerchantState, confID uint32, nowMs uint64) bool {
	if merchant == nil || merchant.GetSceneConfigId() != confID {
		return false
	}
	return nowMs >= merchant.GetAppearAtMs() && nowMs < merchant.GetExpireAtMs()
}

// buildWorldMapOverview 由本拍各图分线目录、神秘商人快照与墙钟拼出全服总览。纯函数。
func buildWorldMapOverview(zoneID uint32, cfg config.WorldMapPlayConfig,
	dirsByConf map[uint32]*scenepb.SceneChannelDirectory, merchant *scenepb.MysteryMerchantState,
	now time.Time) *scenepb.WorldMapOverview {
	nowUnix := now.Unix()
	nowMs := uint64(now.UnixMilli())
	tideCandidates := cfg.EffectiveTideCandidateMapIds()
	tide := worldTideAt(nowUnix, tideCandidates, cfg.EffectiveTideIntervalSeconds(), cfg.EffectiveTideDurationSeconds())
	dailyHotspot := dailyHotspotAt(nowUnix, tideCandidates)
	crowdedThreshold := cfg.EffectiveCrowdedPlayerThreshold()

	openMaps := cfg.EffectiveOpenWorldMapIds()
	maps := make([]*scenepb.WorldMapOverviewEntry, 0, len(openMaps))
	for _, confID := range openMaps {
		dir, ok := dirsByConf[confID]
		if !ok || dir == nil {
			continue
		}
		var totalPlayers uint32
		var activeChannels uint32
		for _, ch := range dir.GetChannels() {
			totalPlayers += ch.GetPlayerCount()
			if ch.GetState() != scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING &&
				ch.GetState() != scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE {
				activeChannels++
			}
		}
		recSceneID, recChannelNo := pickRecommendedChannel(dir)
		isTide := tide.GetActive() && tide.GetSceneConfigId() == confID
		if tide.GetSceneConfigId() == confID {
			tide.RecommendedSceneId = recSceneID
			tide.RecommendedChannelNo = recChannelNo
		}
		maps = append(maps, &scenepb.WorldMapOverviewEntry{
			SceneConfigId:        confID,
			TotalPlayerCount:     totalPlayers,
			ChannelCount:         activeChannels,
			HeatLevel:            classifyWorldMapHeat(totalPlayers, crowdedThreshold),
			RecommendedSceneId:   recSceneID,
			RecommendedChannelNo: recChannelNo,
			IsTideMap:            isTide,
			HasMysteryMerchant:   isMerchantActiveOnMap(merchant, confID, nowMs),
		})
	}
	sort.Slice(maps, func(i, j int) bool { return maps[i].SceneConfigId < maps[j].SceneConfigId })

	return &scenepb.WorldMapOverview{
		ZoneId:                    zoneID,
		UpdatedAtMs:               nowMs,
		ServerTimeMs:              nowMs,
		Maps:                      maps,
		Tide:                      tide,
		DailyHotspotSceneConfigId: dailyHotspot,
	}
}

func publishWorldMapOverview(svcCtx *svc.ServiceContext, zoneID uint32,
	dirsByConf map[uint32]*scenepb.SceneChannelDirectory, now time.Time) error {
	merchant, _, err := readMysteryMerchantState(svcCtx, zoneID)
	if err != nil {
		// 商人标记只是展示徽章，读错时降级为无商人，不阻塞整份地图总览发布。
		logx.Errorf("[WorldMapOverview] zone=%d: failed to read mystery merchant state, continuing without badge: %v", zoneID, err)
		merchant = nil
	}
	overview := buildWorldMapOverview(zoneID, svcCtx.Config.WorldMapPlay, dirsByConf, merchant, now)
	if len(overview.GetMaps()) == 0 {
		return nil
	}
	payload, err := proto.Marshal(overview)
	if err != nil {
		return fmt.Errorf("marshal world map overview failed: %w", err)
	}
	ttlSeconds := worldChannelDirectoryTTLSeconds(
		svcCtx.Config.ChannelSwitch.EffectiveDirectoryRefreshSeconds(),
		svcCtx.Config.EffectiveLeaderLockTTLSeconds(),
	)
	if err := svcCtx.Redis.Setex(worldMapOverviewKey(zoneID), string(payload), ttlSeconds); err != nil {
		return fmt.Errorf("write world map overview failed: %w", err)
	}
	return nil
}
