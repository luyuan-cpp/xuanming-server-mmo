package loginqueue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDrainRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func candidates(ids ...uint32) []GateCandidate {
	out := make([]GateCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, GateCandidate{NodeID: id, IP: "10.0.0.1", Port: 18000, ZoneID: 1})
	}
	return out
}

func nodeIDsOf(cs []GateCandidate) []uint32 {
	out := make([]uint32, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.NodeID)
	}
	return out
}

// 标记之后该 gate 不再出现在候选集里。
func TestGateDrain_MarkedGateIsFilteredOut(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 600))

	cs := candidates(1, 2, 3)
	kept := FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.ElementsMatch(t, []uint32{1, 3}, nodeIDsOf(kept))
}

// 没有任何标记时原样返回。
func TestGateDrain_NoMarksKeepsEveryone(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	cs := candidates(1, 2, 3)
	kept := FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.ElementsMatch(t, []uint32{1, 2, 3}, nodeIDsOf(kept))
}

// 取消标记之后重新接客。
func TestGateDrain_ClearRestoresGate(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 600))
	require.NoError(t, ClearGateDraining(ctx, rdb, 2))

	cs := candidates(1, 2)
	kept := FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.ElementsMatch(t, []uint32{1, 2}, nodeIDsOf(kept))
}

// 标记带 TTL:过期后 gate 自动回到候选集。
// 这是有意的 fail-safe —— 标记的人中途挂了不能让这部分容量永久蒸发。
func TestGateDrain_MarkExpiresAndGateComesBack(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 60))
	cs := candidates(1, 2)
	require.Len(t, FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs)), 1)

	mr.FastForward(61 * time.Second)
	assert.Len(t, FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs)), 2)
}

// **全部**被标记时必须放行,不能返回空。
// 那意味着运维把整个 zone 都标了 —— 拒绝所有登录比把玩家分到一台待缩容的
// gate 更糟(后者只是稍后被改派,前者是直接进不去游戏)。
func TestGateDrain_AllDrainingFallsBackToEveryone(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	for _, id := range []uint32{1, 2, 3} {
		require.NoError(t, MarkGateDraining(ctx, rdb, id, time.Now().Unix(), 600))
	}

	cs := candidates(1, 2, 3)
	kept := FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.ElementsMatch(t, []uint32{1, 2, 3}, nodeIDsOf(kept),
		"refusing every login is worse than assigning to a draining gate")
}

// Redis 挂了要**放行**而不是拦截:抖动一下不能让所有玩家都进不来。
func TestGateDrain_RedisFailureIsFailOpen(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 600))
	mr.Close() // Redis 没了

	cs := candidates(1, 2, 3)
	draining := DrainingGates(ctx, rdb, cs)
	assert.Nil(t, draining, "query failure must be treated as nobody draining")
	assert.Len(t, FilterDrainingGates(cs, draining), 3)
}

// 空候选集 / nil 客户端不能 panic。
func TestGateDrain_DegenerateInputs(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, DrainingGates(ctx, nil, candidates(1)))
	assert.Empty(t, FilterDrainingGates(nil, map[uint32]bool{1: true}))

	rdb, _ := newDrainRedis(t)
	assert.Nil(t, DrainingGates(ctx, rdb, nil))
	assert.Error(t, MarkGateDraining(ctx, nil, 1, 0, 60))
	assert.Error(t, ClearGateDraining(ctx, nil, 1))
}

// 排空标记不影响 PickGate 的选择语义:过滤发生在候选集构造阶段,
// PickGate 仍然只按人数挑最闲的。
func TestGateDrain_PickGateStillPicksLeastLoadedAfterFilter(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	// 最闲的那台(id=1, 0 人)正在排空,应当选次闲的 id=3。
	require.NoError(t, MarkGateDraining(ctx, rdb, 1, time.Now().Unix(), 600))

	cs := []GateCandidate{
		{NodeID: 1, PlayerCount: 0},
		{NodeID: 2, PlayerCount: 500},
		{NodeID: 3, PlayerCount: 100},
	}
	kept := FilterDrainingGates(cs, DrainingGates(ctx, rdb, cs))

	best, err := PickGate(kept)
	require.NoError(t, err)
	assert.EqualValues(t, 3, best.NodeID)
}
