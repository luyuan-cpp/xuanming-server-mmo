package loginqueue

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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

// beforeRedisCmdHook 在指定命令发出前执行 fn,用来把"判定途中标记被改"这种并发时序
// 钉在确定的位置上,不靠 sleep 碰运气。fail 非空时命令不发到服务端、直接以 fail 失败
// (go-redis 的 Client.Process 会把 hook 返回的错误 SetErr 到 cmd 上),用来模拟单条命令的
// 网络 / 超时错误,同一轮里的其他命令照常执行。
type beforeRedisCmdHook struct {
	cmd  string // go-redis 的 Cmder.Name() 是小写命令名
	fn   func()
	fail error
}

func (h beforeRedisCmdHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h beforeRedisCmdHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.cmd {
			if h.fn != nil {
				h.fn()
			}
			if h.fail != nil {
				return h.fail
			}
		}
		return next(ctx, cmd)
	}
}

func (h beforeRedisCmdHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// draining 在 GET 之后、读 TTL 之前被删(k8s_gate_drain.ps1 清标记 / 运维取消):
// gate 已重新接客,不能再补写 drained —— 旧实现在这里把 TTL<=0 回落成 1h 补写,
// drained 比 draining 活得久。还要顺手清掉残留的 drained,与"没在排空"分支一致。
func TestEvaluateDrainingGates_DrainingDeletedMidEvaluationSkipsDrainedMark(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 600))
	// 上一轮留下、还没被清掉的 drained。
	require.NoError(t, mr.Set(gateDrainedKey(2), DrainReasonBelowThreshold))

	fired := 0
	rdb.AddHook(beforeRedisCmdHook{cmd: "ttl", fn: func() {
		fired++
		mr.Del(gateDrainingKey(2))
	}})

	gates := []GateOnline{{NodeID: 2, PlayerCount: 0}}
	drained := EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)

	require.Equal(t, 1, fired, "删除必须恰好发生在读 TTL 之前,否则这条用例没测到竞态")
	assert.Empty(t, drained, "draining 已撤的 gate 不能算作可缩容")
	assert.False(t, mr.Exists(gateDrainingKey(2)))
	assert.False(t, mr.Exists(gateDrainedKey(2)), "draining 已撤,drained 既不能补写也不能残留")
}

// 读 draining TTL 失败(网络抖动 / 超时)时跳过本轮写入 —— 旧实现在这里回落成 1h 补写,
// drained 会比 draining 活得久。下一轮再判即可;判定循环也不能因此去碰 draining 本身。
func TestEvaluateDrainingGates_DrainingTTLReadErrorSkipsDrainedMark(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 600))

	fired := 0
	rdb.AddHook(beforeRedisCmdHook{
		cmd:  "ttl",
		fn:   func() { fired++ },
		fail: errors.New("injected ttl failure"),
	})

	gates := []GateOnline{{NodeID: 2, PlayerCount: 0}}
	drained := EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)

	require.Equal(t, 1, fired, "TTL 必须真的以错误返回,否则这条用例没测到失败分支")
	assert.Empty(t, drained, "TTL 读失败的 gate 本轮不能算作可缩容")
	assert.False(t, mr.Exists(gateDrainedKey(2)), "TTL 读失败时不能补写 drained")
	assert.True(t, mr.Exists(gateDrainingKey(2)), "判定循环不碰 draining 本身")
}

// draining 拿不到正的剩余 TTL 时跳过写入,不能补一个猜的 TTL。
func TestEvaluateDrainingGates_DrainingWithoutPositiveTTLSkipsDrainedMark(t *testing.T) {
	cases := []struct {
		name string
		ttl  time.Duration // 0 = 不设过期
	}{
		// 有人手工 SET 漏了 EX:违反 draining 必带 TTL 的约定,drained 不能跟着永不过期。
		{name: "no expiry", ttl: 0},
		// 剩余不足 1s,TTL 按秒取整为 0;go-redis 的 Set 过期传 0 = 永不过期,绝不能写下去。
		{name: "sub-second remaining", ttl: 500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rdb, mr := newDrainRedis(t)
			ctx := context.Background()
			now := time.Now().Unix()

			require.NoError(t, mr.Set(gateDrainingKey(2), strconv.FormatInt(now, 10)))
			if tc.ttl > 0 {
				mr.SetTTL(gateDrainingKey(2), tc.ttl)
			}

			gates := []GateOnline{{NodeID: 2, PlayerCount: 0}}
			drained := EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now)

			assert.Empty(t, drained)
			assert.False(t, mr.Exists(gateDrainedKey(2)), "拿不到正的剩余 TTL 时不能写 drained")
			assert.True(t, mr.Exists(gateDrainingKey(2)), "判定循环不碰 draining 本身")
		})
	}
}

// 正常路径:drained 的 TTL 取 draining 的剩余 TTL,不是固定值。
func TestEvaluateDrainingGates_DrainedTTLFollowsDrainingRemainingTTL(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, now, 600))
	mr.FastForward(100 * time.Second)

	gates := []GateOnline{{NodeID: 2, PlayerCount: 0}}
	require.Len(t, EvaluateDrainingGates(ctx, rdb, gates, GateDrainPolicy{DrainedBelowPlayers: 0}, now), 1)

	assert.Equal(t, 500*time.Second, mr.TTL(gateDrainedKey(2)))
}
