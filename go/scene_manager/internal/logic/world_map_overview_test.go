package logic

import (
	"context"
	"strconv"
	"testing"
	"time"

	scenepb "proto/scene"
	"scene_manager/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestWorldChannelRouting_ConfigValidation(t *testing.T) {
	// 默认合法（Fill-First + 软上限 200）。
	var def config.Config
	assert.NoError(t, def.ValidateWorldChannelRouting())
	assert.Equal(t, config.WorldChannelPickPolicyFillFirst, def.EffectiveChannelPickPolicy())
	assert.Equal(t, int64(200), def.EffectiveChannelSoftFillThreshold())

	// least_loaded 合法且软上限为 0。
	ll := config.Config{WorldChannelRouting: config.WorldChannelRoutingConfig{PickPolicy: config.WorldChannelPickPolicyLeastLoaded}}
	assert.NoError(t, ll.ValidateWorldChannelRouting())
	assert.Equal(t, int64(0), ll.EffectiveChannelSoftFillThreshold())

	// 非法策略名拒启。
	badPolicy := config.Config{WorldChannelRouting: config.WorldChannelRoutingConfig{PickPolicy: "random"}}
	assert.Error(t, badPolicy.ValidateWorldChannelRouting())

	// 负软上限拒启。
	badSoft := config.Config{WorldChannelRouting: config.WorldChannelRoutingConfig{SoftFillThreshold: -1}}
	assert.Error(t, badSoft.ValidateWorldChannelRouting())

	// 自动缩容开启时，软上限必须落在 [2*ScaleIn, ScaleOut) 迟滞带内。
	tooLow := config.Config{
		WorldChannelRouting: config.WorldChannelRoutingConfig{SoftFillThreshold: 150},
		WorldAutoscale:      config.WorldAutoscaleConfig{Enabled: true, ScaleInPlayerThreshold: 100, ScaleOutPlayerThreshold: 2000},
	}
	assert.Error(t, tooLow.ValidateWorldChannelRouting())

	tooHigh := config.Config{
		WorldChannelRouting: config.WorldChannelRoutingConfig{SoftFillThreshold: 2000},
		WorldAutoscale:      config.WorldAutoscaleConfig{Enabled: true, ScaleInPlayerThreshold: 100, ScaleOutPlayerThreshold: 2000},
	}
	assert.Error(t, tooHigh.ValidateWorldChannelRouting())

	okHysteresis := config.Config{
		WorldChannelRouting: config.WorldChannelRoutingConfig{SoftFillThreshold: 200},
		WorldAutoscale:      config.WorldAutoscaleConfig{Enabled: true, ScaleInPlayerThreshold: 100, ScaleOutPlayerThreshold: 2000},
	}
	assert.NoError(t, okHysteresis.ValidateWorldChannelRouting())
}

func TestReserveBestWorldChannelForEnter_FillFirstHonorsLineNoAndFallsBack(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "node-1")
	sc.Config.WorldChannelRouting.SoftFillThreshold = 3
	const confID uint64 = 2
	// sceneB 数值更大但被编为 1 线，sceneA 数值更小但被编为 2 线，验证按线号而非 scene_id 优先聚人。
	const sceneA, sceneB uint64 = 9001, 9002
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")
	mr.SAdd(worldChannelsKey(testZoneId, confID), strconv.FormatUint(sceneA, 10), strconv.FormatUint(sceneB, 10))
	mr.Set(sceneNodeKey(testZoneId, sceneA), "10")
	mr.Set(sceneNodeKey(testZoneId, sceneB), "10")
	mr.HSet(worldChannelLineNoKey(testZoneId, confID), strconv.FormatUint(sceneB, 10), "1")
	mr.HSet(worldChannelLineNoKey(testZoneId, confID), strconv.FormatUint(sceneA, 10), "2")

	// 1 线已有 2 人 (<3)，2 线为空：Fill-First 仍优先把新玩家聚进 1 线。
	seedSceneMembers(mr, sceneB, 2)
	sid, nodeID, err := ReserveBestWorldChannelForEnter(context.Background(), sc, confID, testZoneId, 5001)
	require.NoError(t, err)
	assert.Equal(t, sceneB, sid)
	assert.Equal(t, "10", nodeID)

	// 现在 1 线已达软上限 3 人，下一名玩家自然溢出到 2 线。
	sid, _, err = ReserveBestWorldChannelForEnter(context.Background(), sc, confID, testZoneId, 5002)
	require.NoError(t, err)
	assert.Equal(t, sceneA, sid)

	// 两条线都 >= 软上限(1 线 4 人、2 线 3 人)时，回退至人数更少的 2 线。
	seedSceneMembers(mr, sceneB, 4)
	seedSceneMembers(mr, sceneA, 3)
	sid, _, err = ReserveBestWorldChannelForEnter(context.Background(), sc, confID, testZoneId, 5003)
	require.NoError(t, err)
	assert.Equal(t, sceneA, sid)
}

func TestSelectOrRecoverWorldChannel_FillFirst(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	sc.Config.WorldChannelCount = 2
	sc.Config.WorldChannelRouting.SoftFillThreshold = 5
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	initWorldScenesForZone(context.Background(), sc, testZoneId, []uint64{1001}, true)
	lines, err := GetAllWorldChannels(context.Background(), sc, 1001, testZoneId)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	sorted := sortedUniqueSceneIDs(lines)

	// 1 线有 4 人 (<5)，2 线 0 人 → 仍选 1 线。
	seedSceneMembersVia(sc, sorted[0], 4)
	best, _, err := SelectOrRecoverWorldChannel(context.Background(), sc, 1001, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, sorted[0], best)

	// 1 线达到 5 人 → 溢出到 2 线。
	seedSceneMembersVia(sc, sorted[0], 5)
	best, _, err = SelectOrRecoverWorldChannel(context.Background(), sc, 1001, testZoneId)
	require.NoError(t, err)
	assert.Equal(t, sorted[1], best)
}

func TestWorldTideAndDailyHotspot_Deterministic(t *testing.T) {
	candidates := []uint32{2, 5}
	// 第 0 小时内：第 0 秒激活，第 2399 秒激活，第 2400 秒休潮，第 3600 秒切到下一张图。
	t0 := worldTideAt(0, candidates, 3600, 2400)
	assert.True(t, t0.GetActive())
	assert.Equal(t, uint32(2), t0.GetSceneConfigId())
	assert.Equal(t, uint64(0), t0.GetStartsAtMs())
	assert.Equal(t, uint64(2400_000), t0.GetEndsAtMs())
	assert.Equal(t, uint64(3600_000), t0.GetNextStartsAtMs())

	tRest := worldTideAt(2400, candidates, 3600, 2400)
	assert.False(t, tRest.GetActive())
	assert.Equal(t, uint32(2), tRest.GetSceneConfigId())

	tNext := worldTideAt(3600, candidates, 3600, 2400)
	assert.True(t, tNext.GetActive())
	assert.Equal(t, uint32(5), tNext.GetSceneConfigId())
	assert.Equal(t, uint64(3600), tNext.GetRoundId())

	// 东八区 05:00 跨日（UTC 21:00）验证：2026-11-22 20:59:59 UTC 与 21:00:00 UTC 必须换图。
	beforeReset := time.Date(2026, 11, 22, 20, 59, 59, 0, time.UTC).Unix()
	atReset := time.Date(2026, 11, 22, 21, 0, 0, 0, time.UTC).Unix()
	assert.NotEqual(t, dailyHotspotAt(beforeReset, candidates), dailyHotspotAt(atReset, candidates))
}

func TestPublishWorldChannelDirectoriesForZone_PublishesWorldMapOverview(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	// 准备 World 表：1(主城)、2(八仙岛)、3(未开放)、5(碧渊水宫)。
	t.Cleanup(SetWorldConfIdsForTest([]uint64{1, 2, 3, 5}))
	mr.ZAdd(nodeLoadKey(testZoneId), 0, "10")

	seedMap := func(confID uint64, sceneID uint64, players int) {
		mr.SAdd(worldChannelsKey(testZoneId, confID), strconv.FormatUint(sceneID, 10))
		mr.Set(sceneNodeKey(testZoneId, sceneID), "10")
		if players > 0 {
			seedSceneMembers(mr, sceneID, players)
		}
	}
	seedMap(1, 10001, 10) // >=8 -> CROWDED
	seedMap(2, 20001, 3)  // 1..7 -> ACTIVE
	seedMap(3, 30001, 20) // 不在 OpenWorldMapIds [1,2,5]，不应进入总览
	seedMap(5, 50001, 0)  // 0 -> QUIET

	now := time.Unix(3600+600, 0).UTC() // roundIndex=1 -> candidates[1]=5 灵潮激活中
	// 放一名在场的神秘商人到地图 5。
	merchant := &scenepb.MysteryMerchantState{
		ZoneId:        testZoneId,
		RoundId:       3600,
		Revision:      1,
		SceneConfigId: 5,
		SceneId:       50001,
		ChannelNo:     1,
		AppearAtMs:    3600_000,
		ExpireAtMs:    5400_000,
	}
	mBytes, err := proto.Marshal(merchant)
	require.NoError(t, err)
	mr.HSet(mysteryMerchantKey(testZoneId), MysteryMerchantStateField, string(mBytes))

	published := PublishWorldChannelDirectoriesForZone(context.Background(), sc, testZoneId, now)
	assert.Equal(t, 4, published)

	raw, err := mr.Get(worldMapOverviewKey(testZoneId))
	require.NoError(t, err)
	var overview scenepb.WorldMapOverview
	require.NoError(t, proto.Unmarshal([]byte(raw), &overview))
	assert.Equal(t, uint32(testZoneId), overview.GetZoneId())
	assert.Equal(t, uint64(now.UnixMilli()), overview.GetUpdatedAtMs())
	assert.True(t, overview.GetTide().GetActive())
	assert.Equal(t, uint32(5), overview.GetTide().GetSceneConfigId())
	assert.Equal(t, uint64(50001), overview.GetTide().GetRecommendedSceneId())
	assert.Equal(t, uint32(1), overview.GetTide().GetRecommendedChannelNo())

	// 仅包含开放图 [1, 2, 5]，不含未开放图 3。
	require.Len(t, overview.GetMaps(), 3)
	assert.Equal(t, uint32(1), overview.GetMaps()[0].GetSceneConfigId())
	assert.Equal(t, scenepb.WorldMapHeatLevel_WORLD_MAP_HEAT_LEVEL_CROWDED, overview.GetMaps()[0].GetHeatLevel())
	assert.Equal(t, uint32(10), overview.GetMaps()[0].GetTotalPlayerCount())
	assert.False(t, overview.GetMaps()[0].GetIsTideMap())

	assert.Equal(t, uint32(2), overview.GetMaps()[1].GetSceneConfigId())
	assert.Equal(t, scenepb.WorldMapHeatLevel_WORLD_MAP_HEAT_LEVEL_ACTIVE, overview.GetMaps()[1].GetHeatLevel())
	assert.False(t, overview.GetMaps()[1].GetHasMysteryMerchant())

	assert.Equal(t, uint32(5), overview.GetMaps()[2].GetSceneConfigId())
	assert.Equal(t, scenepb.WorldMapHeatLevel_WORLD_MAP_HEAT_LEVEL_QUIET, overview.GetMaps()[2].GetHeatLevel())
	assert.True(t, overview.GetMaps()[2].GetIsTideMap())
	assert.True(t, overview.GetMaps()[2].GetHasMysteryMerchant())
	assert.Equal(t, uint64(50001), overview.GetMaps()[2].GetRecommendedSceneId())
}

func TestPickMerchantChannelWithTideBias(t *testing.T) {
	candidates := []merchantChannel{
		{confID: 1, sceneID: 101, channelNo: 1},
		{confID: 2, sceneID: 201, channelNo: 1},
		{confID: 2, sceneID: 202, channelNo: 2},
		{confID: 5, sceneID: 501, channelNo: 1},
	}

	// 命中灵潮倾向（roll 20 < 80）：优先落到灵潮图 2 的最小线号 201。
	got, ok := pickMerchantChannelWithTideBias(candidates, 101, 2, 80, &scriptedMerchantRand{values: []uint64{20}})
	require.True(t, ok)
	assert.Equal(t, uint64(201), got.sceneID)

	// 上一轮恰好在 201：避开 201 后仍落在灵潮图 2 的 202。
	got, ok = pickMerchantChannelWithTideBias(candidates, 201, 2, 80, &scriptedMerchantRand{values: []uint64{10}})
	require.True(t, ok)
	assert.Equal(t, uint64(202), got.sceneID)

	// 未命中灵潮倾向（roll 95 >= 80）：从避让后的全池随机挑（下标 0 -> 101）。
	got, ok = pickMerchantChannelWithTideBias(candidates, 501, 2, 80, &scriptedMerchantRand{values: []uint64{95, 0}})
	require.True(t, ok)
	assert.Equal(t, uint64(101), got.sceneID)

	// 倾向关闭（bias=0）：不额外消耗随机数，直接全池选。
	got, ok = pickMerchantChannelWithTideBias(candidates, 0, 2, 0, &scriptedMerchantRand{values: []uint64{3}})
	require.True(t, ok)
	assert.Equal(t, uint64(501), got.sceneID)
}
