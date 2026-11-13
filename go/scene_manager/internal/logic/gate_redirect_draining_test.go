package logic

// 跨 zone 重定向读 Redis 标记(gateRedisMarks)的 I/O 半边:排空与停写两种键名与下标对应、失败放行、退化输入;
// 以及停写来源的合并(unionMarks)。纯函数 signRedirectToGate 的分档见 gate_redirect_eligibility_test.go
// 与 gate_redirect_client_endpoint_test.go。

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	commonpb "proto/common/base"
	"shared/gatepick"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainingTestZone 是这些用例里目标 zone 的号。故意不用 testGateNode 里节点自报的 zone:标记键里的 zone 取 gateRedisMarks 的参数。
const drainingTestZone uint32 = 4

func drainingTestNodes(ids ...uint32) []*commonpb.NodeInfo {
	nodes := make([]*commonpb.NodeInfo, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, testGateNode(id, 100, 0, testEndpoint(fmt.Sprintf("10.0.0.%d", id), 18000), nil))
	}
	return nodes
}

// 只把被标记的那台认作排空中 / 停写:键是 gate:{gm:<zone>}:<node_id>:draining 与 …:stale;两种标记互不串位。
func TestGateRedisMarks_ReadsBothMarksByNodeID(t *testing.T) {
	mr := miniredis.RunT(t)
	rds := newTestControlRedis(t, mr.Addr())
	require.NoError(t, mr.Set(gatepick.DrainingKey(drainingTestZone, 2), strconv.FormatInt(1_800_000_000, 10)))
	require.NoError(t, mr.Set(gatepick.StaleKey(drainingTestZone, 3), "1"))

	draining, stale := gateRedisMarks(context.Background(), rds, drainingTestZone, drainingTestNodes(1, 2, 3))

	assert.Equal(t, map[uint32]bool{2: true}, draining)
	assert.Equal(t, map[uint32]bool{3: true}, stale)
}

// 同一台既排空又停写:两份各记一次。没有任何标记时两份都是空的(不是 nil:读成功了,只是没人被标)。
func TestGateRedisMarks_SameGateBothMarksAndNoMarks(t *testing.T) {
	mr := miniredis.RunT(t)
	rds := newTestControlRedis(t, mr.Addr())

	draining, stale := gateRedisMarks(context.Background(), rds, drainingTestZone, drainingTestNodes(1, 2))
	assert.Empty(t, draining)
	assert.Empty(t, stale)

	require.NoError(t, mr.Set(gatepick.DrainingKey(drainingTestZone, 1), "1800000000"))
	require.NoError(t, mr.Set(gatepick.StaleKey(drainingTestZone, 1), "1"))
	draining, stale = gateRedisMarks(context.Background(), rds, drainingTestZone, drainingTestNodes(1, 2))
	assert.Equal(t, map[uint32]bool{1: true}, draining)
	assert.Equal(t, map[uint32]bool{1: true}, stale)
}

// Redis 读失败时放行(两份都返回 nil),与 login 的 loginqueue.DrainingGates 同语义:不因读不到标记就拒绝重定向。
func TestGateRedisMarks_RedisFailureIsFailOpen(t *testing.T) {
	// 自己管理生命周期(不用 RunT):用例中途要关掉它。
	mr, err := miniredis.Run()
	require.NoError(t, err)
	rds := newTestControlRedis(t, mr.Addr())
	require.NoError(t, mr.Set(gatepick.DrainingKey(drainingTestZone, 1), "1800000000"))
	require.NoError(t, mr.Set(gatepick.StaleKey(drainingTestZone, 2), "1"))
	mr.Close()

	draining, stale := gateRedisMarks(context.Background(), rds, drainingTestZone, drainingTestNodes(1, 2))
	assert.Nil(t, draining)
	assert.Nil(t, stale)
}

// 退化输入:没有 Redis 或没有节点时不查、返回 nil。
func TestGateRedisMarks_DegenerateInputs(t *testing.T) {
	mr := miniredis.RunT(t)
	rds := newTestControlRedis(t, mr.Addr())

	draining, stale := gateRedisMarks(context.Background(), nil, drainingTestZone, drainingTestNodes(1))
	assert.Nil(t, draining)
	assert.Nil(t, stale)
	draining, stale = gateRedisMarks(context.Background(), rds, drainingTestZone, nil)
	assert.Nil(t, draining)
	assert.Nil(t, stale)
}

// 停写两个来源取并集:本进程历史判出的、目标 zone 的 login 发布的,任一成立即算;不修改入参。
func TestUnionMarks(t *testing.T) {
	local := map[uint32]bool{1: true}
	published := map[uint32]bool{2: true, 1: true}

	assert.Equal(t, map[uint32]bool{1: true, 2: true}, unionMarks(local, published))
	assert.Equal(t, map[uint32]bool{1: true}, local, "入参不得被改")
	assert.Equal(t, map[uint32]bool{2: true, 1: true}, published, "入参不得被改")

	assert.Equal(t, local, unionMarks(local, nil))
	assert.Equal(t, published, unionMarks(nil, published))
	assert.Nil(t, unionMarks(nil, nil))
	assert.Empty(t, unionMarks(map[uint32]bool{}, map[uint32]bool{}))
}

// 第一次重定向到某个 zone 时本进程没有历史,只靠目标 zone 的 login 发布的停写标记也能避开卡住的 gate(2026-11-10)。
// 修复前这里签给人最少、但已卡住的 2 号。
func TestSignRedirectToGate_PublishedStaleMarkAvoidsHungGateOnFirstRedirect(t *testing.T) {
	mr := miniredis.RunT(t)
	rds := newTestControlRedis(t, mr.Addr())
	require.NoError(t, mr.Set(gatepick.StaleKey(drainingTestZone, 2), "1"))
	busy := cappedGateNode(1, 80, 0, 30000)
	hung := cappedGateNode(2, 5, 0, 30001)
	nodes := []*commonpb.NodeInfo{busy, hung}

	var local map[uint32]bool // 本进程第一次读这个 zone:staleGateIDs 没有历史可比,一台都判不出
	draining, published := gateRedisMarks(context.Background(), rds, drainingTestZone, nodes)
	got := redirectGateIDMarked(t, nodes, gateMarks{draining: draining, stale: unionMarks(local, published)}, firstDraws)

	assert.Equal(t, uint32(1), got, "目标 zone 的 login 判为停写的 gate 不接新玩家")
}

// 标记按 zone 隔开:别的 zone 里同号 gate 的标记读不到;一个 zone 的 2N 个键共用一个 tag(同槽,一条 MGET)。
func TestGateRedisMarks_OnlyTheTargetZonesMarksCount(t *testing.T) {
	mr := miniredis.RunT(t)
	rds := newTestControlRedis(t, mr.Addr())
	require.NoError(t, mr.Set(gatepick.DrainingKey(drainingTestZone+1, 1), "1800000000"))
	require.NoError(t, mr.Set(gatepick.StaleKey(drainingTestZone+1, 2), "1"))

	draining, stale := gateRedisMarks(context.Background(), rds, drainingTestZone, drainingTestNodes(1, 2))
	assert.Empty(t, draining, "zone %d 的排空标记不算目标 zone %d 的", drainingTestZone+1, drainingTestZone)
	assert.Empty(t, stale)

	want := gatepick.MarkTag(drainingTestZone)
	for _, id := range []uint32{1, 2, 900_001} {
		for _, key := range []string{gatepick.DrainingKey(drainingTestZone, id), gatepick.StaleKey(drainingTestZone, id)} {
			assert.Contains(t, key, want, "一个 zone 的标记都带 %s", want)
		}
	}
}
