package loginqueue

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── 纯判定函数 ──────────────────────────────────────────────────────────

func TestEvaluateGateDrain_BelowThreshold(t *testing.T) {
	st := EvaluateGateDrain(0, 1000, 1010, GateDrainPolicy{DrainedBelowPlayers: 0, DeadlineSeconds: 600})
	assert.True(t, st.Drained)
	assert.Equal(t, DrainReasonBelowThreshold, st.Reason)
}

// 阈值是"<=",相等也算排空。
func TestEvaluateGateDrain_ThresholdIsInclusive(t *testing.T) {
	st := EvaluateGateDrain(5, 1000, 1010, GateDrainPolicy{DrainedBelowPlayers: 5, DeadlineSeconds: 600})
	assert.True(t, st.Drained)
	assert.Equal(t, DrainReasonBelowThreshold, st.Reason)
}

func TestEvaluateGateDrain_StillBusyIsNotDrained(t *testing.T) {
	st := EvaluateGateDrain(300, 1000, 1010, GateDrainPolicy{DrainedBelowPlayers: 5, DeadlineSeconds: 600})
	assert.False(t, st.Drained)
	assert.EqualValues(t, 10, st.WaitedSec)
}

func TestEvaluateGateDrain_DeadlineReleasesWithResidents(t *testing.T) {
	st := EvaluateGateDrain(42, 1000, 1600, GateDrainPolicy{DrainedBelowPlayers: 5, DeadlineSeconds: 600})
	assert.True(t, st.Drained)
	assert.Equal(t, DrainReasonDeadline, st.Reason)
	assert.EqualValues(t, 42, st.Online, "残留人数必须带出来,缩容会断掉他们")
}

// DeadlineSeconds=0 = 永不超时放行,只认人走干净。
// 这是"绝不主动断玩家"的运营口径,不能被当成"立刻放行"。
func TestEvaluateGateDrain_ZeroDeadlineNeverReleasesOnTime(t *testing.T) {
	policy := GateDrainPolicy{DrainedBelowPlayers: 0, DeadlineSeconds: 0}
	st := EvaluateGateDrain(1, 1000, 1000+86400, policy)
	assert.False(t, st.Drained, "deadline=0 必须表示永不超时放行")
}

// 时钟回拨不能把等待时间算成负数直接放行。
func TestEvaluateGateDrain_ClockSkewDoesNotReleaseEarly(t *testing.T) {
	st := EvaluateGateDrain(100, 2000, 1000, GateDrainPolicy{DrainedBelowPlayers: 5, DeadlineSeconds: 10})
	assert.False(t, st.Drained)
	assert.EqualValues(t, 0, st.WaitedSec)
}

// ── Redis 侧的一轮判定 ──────────────────────────────────────────────────

func TestEvaluateDrainingGates_MarksDrainedOnlyForDrainingGates(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 600))

	gates := []GateOnline{{NodeID: 1, PlayerCount: 0}, {NodeID: 2, PlayerCount: 0}}
	drained := EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)

	require.Len(t, drained, 1)
	assert.EqualValues(t, 2, drained[0].NodeID)

	assert.True(t, mr.Exists(gateDrainedKey(2)))
	assert.False(t, mr.Exists(gateDrainedKey(1)), "没标 draining 的 gate 不该被判定")
}

// 还有人 -> 不打 drained 标记。
func TestEvaluateDrainingGates_BusyGateIsNotMarked(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 600))

	gates := []GateOnline{{NodeID: 2, PlayerCount: 500}}
	drained := EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0, DeadlineSeconds: 600}, now)

	assert.Empty(t, drained)
	assert.False(t, mr.Exists(gateDrainedKey(2)))
}

// 取消排空后,残留的 drained 标记必须被清掉 ——
// 不清的话下次这台 gate 会被误判成"随时可以删"。
func TestEvaluateDrainingGates_ClearsStaleDrainedMark(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 600))
	gates := []GateOnline{{NodeID: 2, PlayerCount: 0}}
	EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)
	require.True(t, mr.Exists(gateDrainedKey(2)))

	// 运维取消了缩容。
	require.NoError(t, ClearGateDraining(ctx, rdb, 2))
	EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)

	assert.False(t, mr.Exists(gateDrainedKey(2)), "cancelled drain must not leave a stale drained mark")
}

// drained 标记不能比 draining 标记活得久:draining 一过期 gate 就重新接客了。
func TestEvaluateDrainingGates_DrainedMarkDoesNotOutliveDrainingMark(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 60))
	gates := []GateOnline{{NodeID: 2, PlayerCount: 0}}
	EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)
	require.True(t, mr.Exists(gateDrainedKey(2)))

	mr.FastForward(61 * time.Second)
	assert.False(t, mr.Exists(gateDrainingKey(2)))
	assert.False(t, mr.Exists(gateDrainedKey(2)))
}

// 超时放行时理由必须是 deadline,让运维知道这次缩容会断人。
func TestEvaluateDrainingGates_DeadlineReasonIsRecorded(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now-700, 3600))

	gates := []GateOnline{{NodeID: 2, PlayerCount: 30}}
	drained := EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0, DeadlineSeconds: 600}, now)

	require.Len(t, drained, 1)
	assert.Equal(t, DrainReasonDeadline, drained[0].Reason)

	stored, err := rdb.Get(ctx, gateDrainedKey(2)).Result()
	require.NoError(t, err)
	assert.Equal(t, DrainReasonDeadline, stored)
}

// 空输入 / nil 客户端不 panic。
func TestEvaluateDrainingGates_DegenerateInputs(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, EvaluateDrainingGates(ctx, nil, []GateOnline{{NodeID: 1}}, GateDrainPolicy{}, 0))

	rdb, _ := newDrainRedis(t)
	assert.Nil(t, EvaluateDrainingGates(ctx, rdb, nil, GateDrainPolicy{}, 0))

	// monitor 在参数不全时必须直接返回,不起 goroutine。
	StartGateDrainMonitor(ctx, nil, nil, GateDrainPolicy{}, 0)
}
