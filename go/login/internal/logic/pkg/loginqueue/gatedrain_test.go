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

// drainingIDsOf 列出带排空标记的候选。
func drainingIDsOf(cs []GateCandidate) []uint32 {
	out := make([]uint32, 0, len(cs))
	for _, c := range cs {
		if c.Draining {
			out = append(out, c.NodeID)
		}
	}
	return out
}

// 标记之后该 gate 仍在候选集里(容量记账要数到它上面的人),只是带上排空标记;挑 gate 时有合格的就不选它。
func TestGateDrain_MarkedGateStaysAndIsNotPicked(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 600))

	cs := candidates(1, 2, 3)
	MarkDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.Equal(t, []uint32{1, 2, 3}, nodeIDsOf(cs), "排空中的 gate 不从候选集里剔除")
	assert.Equal(t, []uint32{2}, drainingIDsOf(cs))
	assert.ElementsMatch(t, []uint32{1, 3}, nodeIDsOf(eligibleGates(cs)))
}

// 没有任何标记时谁都不带标记。
func TestGateDrain_NoMarksMarksNobody(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	cs := candidates(1, 2, 3)
	MarkDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.Empty(t, drainingIDsOf(cs))
}

// 取消标记之后重新接客。
func TestGateDrain_ClearRestoresGate(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 600))
	require.NoError(t, ClearGateDraining(ctx, rdb, 2))

	cs := candidates(1, 2)
	MarkDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.Empty(t, drainingIDsOf(cs))
}

// 标记带 TTL:过期后 gate 自动不再算排空。
// 这是有意的 fail-safe —— 标记的人中途挂了不能让这部分容量永久蒸发。
func TestGateDrain_MarkExpiresAndGateComesBack(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, time.Now().Unix(), 60))
	cs := candidates(1, 2)
	MarkDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	require.Equal(t, []uint32{2}, drainingIDsOf(cs))

	mr.FastForward(61 * time.Second)
	fresh := candidates(1, 2)
	MarkDrainingGates(fresh, DrainingGates(ctx, rdb, fresh))
	assert.Empty(t, drainingIDsOf(fresh))
}

// **全部**被标记时必须照样能挑到 gate,不能返回空:分档退到「连得上的」,在全部排空中的 gate 里挑。
// 那意味着运维把整个 zone 都标了 —— 拒绝所有登录比把玩家分到一台待缩容的
// gate 更糟(后者只是稍后被改派,前者是直接进不去游戏)。
func TestGateDrain_AllDrainingStillPicks(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	for _, id := range []uint32{1, 2, 3} {
		require.NoError(t, MarkGateDraining(ctx, rdb, id, time.Now().Unix(), 600))
	}

	cs := candidates(1, 2, 3)
	MarkDrainingGates(cs, DrainingGates(ctx, rdb, cs))
	assert.ElementsMatch(t, []uint32{1, 2, 3}, nodeIDsOf(eligibleGates(cs)),
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
	MarkDrainingGates(cs, draining)
	assert.Empty(t, drainingIDsOf(cs))
}

// 空候选集 / nil 客户端不能 panic。
func TestGateDrain_DegenerateInputs(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, DrainingGates(ctx, nil, candidates(1)))
	MarkDrainingGates(nil, map[uint32]bool{1: true})

	rdb, _ := newDrainRedis(t)
	assert.Nil(t, DrainingGates(ctx, rdb, nil))
	assert.Error(t, MarkGateDraining(ctx, nil, 1, 0, 60))
	assert.Error(t, ClearGateDraining(ctx, nil, 1))
}

// 排空不改 PickGate 的选择语义:只是不参与挑选。剩两台时随机二选一恰好等于取人数最少的那台,结果是确定的。
func TestGateDrain_PickGateSkipsDrainingLeastLoaded(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()

	// 最闲的那台(id=1, 0 人)正在排空,应当选次闲的 id=3。
	require.NoError(t, MarkGateDraining(ctx, rdb, 1, time.Now().Unix(), 600))

	cs := []GateCandidate{
		{NodeID: 1, PlayerCount: 0},
		{NodeID: 2, PlayerCount: 500},
		{NodeID: 3, PlayerCount: 100},
	}
	MarkDrainingGates(cs, DrainingGates(ctx, rdb, cs))

	best, err := PickGate(cs)
	require.NoError(t, err)
	assert.EqualValues(t, 3, best.NodeID)
}

// 回归(2026-11-06):没排空的 gate 全部停写、只有排空中的健康时,挑排空中的那台。
// 此前候选集先剔排空、停写那层再在卡住的 gate 里回落,票签给一台多半连不上的 gate。
func TestGateDrain_HealthyDrainingGateBeatsStale(t *testing.T) {
	cs := []GateCandidate{
		{NodeID: 1, PlayerCount: 300, Draining: true},
		{NodeID: 2, PlayerCount: 0, Stale: true},
		{NodeID: 3, PlayerCount: 10, Stale: true},
	}
	for range 50 {
		best, err := PickGate(cs)
		require.NoError(t, err)
		assert.EqualValues(t, 1, best.NodeID, "连得上的排空中 gate 胜过卡住的 gate")
	}
}
