package logic

// 缩容与镜像 Scene 的关系。
//
// 镜像与源频道是共置的(scene-creation-architecture.md 的 mirror co-location):
// 源频道被销毁,镜像就变成谁也进不去的孤儿。节点死亡那条路径是强制级联销毁镜像的,
// 但**缩容是可选动作** —— 宁可少省一个频道,也不要把镜像里的玩家踢下线。

import (
	"context"
	"strconv"
	"testing"

	"scene_manager/internal/svc"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 最闲的频道托着镜像 -> 不缩它,改缩下一个够闲又没镜像的。
func TestAutoscale_SkipsMirrorSourceAndDrainsAnotherChannel(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	ctx := context.Background()
	// 承载节点必须注册成存活,否则排空会走"节点已不在"的短路当场清理完。
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	// 101 最闲但是镜像源;102 也够闲且没镜像;103 撑着人数不让缩到 0。
	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 10, 102: 20, 103: 900})
	_, err := sc.Redis.Sadd(sceneMirrorsKey(101), "9001")
	require.NoError(t, err)

	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.Equal(t, 1, in)

	drainingSet, _ := sc.Redis.Smembers(worldDrainingSetKey(testZoneId, autoscaleTestConfID))
	assert.NotContains(t, drainingSet, "101", "镜像源频道不该被排空")
	assert.Contains(t, drainingSet, "102", "应当退而排空下一个够闲且无镜像的频道")
}

// 所有够闲的频道都是镜像源 -> 这一轮不缩容。
func TestAutoscale_NoScaleInWhenEveryIdleChannelIsAMirrorSource(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	ctx := context.Background()
	// 承载节点必须注册成存活,否则排空会走"节点已不在"的短路当场清理完。
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 10, 102: 20, 103: 900})
	_, err := sc.Redis.Sadd(sceneMirrorsKey(101), "9001")
	require.NoError(t, err)
	_, err = sc.Redis.Sadd(sceneMirrorsKey(102), "9002")
	require.NoError(t, err)

	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	assert.Equal(t, 0, in)

	members, _ := sc.Redis.Smembers(worldChannelsKey(testZoneId, autoscaleTestConfID))
	assert.Len(t, members, 3, "一个频道都不该被摘掉")
}

// 读镜像集合失败时 fail-closed:当作有镜像,不缩容。
// 缩容是省钱的可选动作,查不清就别动,比误伤镜像里的玩家划算。
func TestAutoscale_MirrorQueryFailureBlocksScaleIn(t *testing.T) {
	sc, mr := newAutoscaleCtx(t)
	ctx := context.Background()
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 10, 103: 900})

	// 让 Redis 对 SMEMBERS 报错:mirrors 键换成非集合类型。
	require.NoError(t, mr.Set(sceneMirrorsKey(101), "not-a-set"))

	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	assert.Equal(t, 0, in, "查不清镜像时必须放弃缩容")
}

// 排空窗口里新生的镜像,收尾时必须被级联销毁,mirrors 键也要清掉。
// pickScaleInVictim 只保证"开始排空那一刻"没有镜像。
func TestAutoscale_DrainCascadesMirrorsBornDuringTheDrainWindow(t *testing.T) {
	sc, _ := newAutoscaleCtx(t)
	ctx := context.Background()
	// 承载节点必须注册成存活,否则排空会走"节点已不在"的短路当场清理完。
	sc.Redis.Zadd(testLoadKey(), 0, "10")

	seedChannels(t, sc, testZoneId, autoscaleTestConfID, map[uint64]int64{101: 10, 103: 900})

	_, in := AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)
	require.Equal(t, 1, in)
	require.NoError(t, requireDraining(sc, 101))

	// 排空进行中,有人以 101 为源建了个镜像。
	const mirrorID uint64 = 9100
	_, err := sc.Redis.Sadd(sceneMirrorsKey(101), strconv.FormatUint(mirrorID, 10))
	require.NoError(t, err)
	require.NoError(t, sc.Redis.Set(sceneNodeKey(mirrorID), "10"))
	require.NoError(t, sc.Redis.Set(sceneZoneKey(mirrorID), strconv.FormatUint(uint64(testZoneId), 10)))
	require.NoError(t, sc.Redis.Set(sceneSourceKey(mirrorID), "101"))

	// 人走干净,下一拍收尾。
	require.NoError(t, sc.Redis.Set("instance:101:player_count", "0"))
	AutoscaleWorldChannelsForZone(ctx, sc, testZoneId)

	mirrorNode, _ := sc.Redis.Get(sceneNodeKey(mirrorID))
	assert.Equal(t, "", mirrorNode, "源频道被缩掉后,镜像必须一起销毁,不能留成孤儿")

	mirrorsLeft, _ := sc.Redis.Smembers(sceneMirrorsKey(101))
	assert.Empty(t, mirrorsLeft, "scene:{id}:mirrors 键不能泄漏")
}

// requireDraining 断言某频道已进入排空态。
func requireDraining(sc *svc.ServiceContext, sceneID uint64) error {
	if exists, _ := sc.Redis.Exists(sceneDrainingKey(sceneID)); !exists {
		return assertDrainErr(sceneID)
	}
	return nil
}

type drainAssertError uint64

func (e drainAssertError) Error() string {
	return "scene " + strconv.FormatUint(uint64(e), 10) + " is not draining"
}

func assertDrainErr(sceneID uint64) error { return drainAssertError(sceneID) }
