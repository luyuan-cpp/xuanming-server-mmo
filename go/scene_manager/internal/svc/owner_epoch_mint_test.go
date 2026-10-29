package svc

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"scene_manager/internal/config"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// ---------------------------------------------------------------------------
// 铸造专用通道(owner_epoch_mint.go,docs/design/zone-redis-regression-guard-20261022.md §4.3.2 G3b)
//
// miniredis 有 EVAL、没有 WAITAOF:WAITAOF 在这里恒回错误回复,正好走「WAITAOF 被拒」那一支(真 Redis 上
// 是 appendonly no / Redis < 7.2)。份数满足 / 不足的判定用纯函数 judgeWaitAOFReply 覆盖,真 Redis 上的端到端
// 验证见设计 §8.1 G3b 与 §8.2 D2。
// ---------------------------------------------------------------------------

// pipelineRecorder 是挂在铸造专用客户端上的 go-redis hook:记下每条含 EVAL / WAITAOF 的 pipeline 的命令名、
// 每条单发的 EVAL / WAITAOF;failFirst > 0 时前 failFirst 条这样的 pipeline 不发出去,逐条命令按传输错误(io.EOF)
// 失败 —— 模拟「EVAL 结果未知」。别的命令不记也不拦:go-redis 建连时的 HELLO 等握手命令也会经过克隆来的 hook。
type pipelineRecorder struct {
	mu        sync.Mutex
	pipelines [][]string
	singles   []string
	failFirst int
}

func isMintCommand(name string) bool { return name == "eval" || name == "waitaof" }

func (r *pipelineRecorder) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (r *pipelineRecorder) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if isMintCommand(cmd.Name()) {
			r.mu.Lock()
			r.singles = append(r.singles, cmd.Name())
			r.mu.Unlock()
		}
		return next(ctx, cmd)
	}
}

func (r *pipelineRecorder) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		names := make([]string, len(cmds))
		relevant := false
		for i, c := range cmds {
			names[i] = c.Name()
			relevant = relevant || isMintCommand(names[i])
		}
		if !relevant {
			return next(ctx, cmds)
		}
		r.mu.Lock()
		r.pipelines = append(r.pipelines, names)
		fail := r.failFirst > 0
		if fail {
			r.failFirst--
		}
		r.mu.Unlock()
		if fail {
			for _, c := range cmds {
				c.SetErr(io.EOF)
			}
			return io.EOF
		}
		return next(ctx, cmds)
	}
}

func (r *pipelineRecorder) recordedPipelines() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.pipelines...)
}

func (r *pipelineRecorder) recordedSingles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.singles...)
}

func intPtr(v int) *int { return &v }

// newRecordedMinter 起一个连 miniredis 的铸造通道,并挂上 pipelineRecorder。
func newRecordedMinter(t *testing.T, mint config.OwnerEpochMintConfig) (*RedisOwnerEpochMinter, *pipelineRecorder, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	m, err := NewRedisOwnerEpochMinter(redis.RedisConf{Host: mr.Addr(), Type: redis.NodeType}, mint, func() uint64 { return 42 })
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	rec := &pipelineRecorder{}
	m.client.AddHook(rec)
	return m, rec, mr
}

const testIncrScript = `return redis.call("INCR", KEYS[1])`

// 铸造 EVAL 与 WAITAOF 在同一个 pipeline 里发(go-redis 的一条 pipeline 只占一条连接):WAITAOF 等的是本连接
// 最后一次写的偏移,分两次发就可能落到不同连接上。EVAL 的回复原样交回,WAITAOF 被拒记 Error。
func TestEvalAndConfirmSendsEvalAndWaitAOFInOnePipeline(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{"k"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Reply)
	assert.Equal(t, 1, res.Attempts)
	assert.Equal(t, [][]string{{"eval", "waitaof"}}, rec.recordedPipelines())
	assert.Equal(t, MintDurabilityError, res.Durability, "miniredis 不认 WAITAOF:错误回复 = 落没落盘不知道")
	require.Error(t, res.DurableErr)
	v, _ := mr.Get("k")
	assert.Equal(t, "1", v)
}

// 两项份数都配 0:只发 EVAL,结论 Disabled。
func TestEvalAndConfirmDisabledSendsOnlyEval(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)})

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{"k"})
	require.NoError(t, err)
	assert.Equal(t, MintDurabilityDisabled, res.Durability)
	assert.NoError(t, res.DurableErr)
	assert.Equal(t, [][]string{{"eval"}}, rec.recordedPipelines())
}

// 脚本的错误回复是确定性的:不重发,返回 error。
func TestEvalAndConfirmScriptErrorIsNotResent(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})

	res, err := m.EvalAndConfirm(context.Background(), `return redis.error_reply("boom")`, []string{"k"})
	require.Error(t, err)
	assert.Equal(t, 1, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), 1)
}

// EVAL 结果未知(传输错误)时在新连接上重发整条 pipeline;这里首发根本没发出去,所以脚本只执行了一次。
func TestEvalAndConfirmResendsWholePipelineWhenEvalResultUnknown(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.failFirst = 1

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{"k"})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Attempts)
	assert.Equal(t, [][]string{{"eval", "waitaof"}, {"eval", "waitaof"}}, rec.recordedPipelines())
	v, _ := mr.Get("k")
	assert.Equal(t, "1", v)
}

// 重发次数有上限,耗尽按「结果未知」返回 error(调用方回 3,不当作没写)。
func TestEvalAndConfirmGivesUpAfterBoundedAttempts(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.failFirst = 100

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{"k"})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrOwnerEpochMintNotSent))
	assert.Equal(t, ownerEpochMintAttempts, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), ownerEpochMintAttempts)
}

// 剩余预算扣掉回包余量后不足 1ms:一条都不发(WAITAOF 的 timeout 0 是无限阻塞),Redis 一个字节都没改。
func TestEvalAndConfirmNotSentWhenBudgetTooShort(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), ownerEpochMintDeadlineMargin/2)
	defer cancel()

	_, err := m.EvalAndConfirm(ctx, testIncrScript, []string{"k"})
	require.ErrorIs(t, err, ErrOwnerEpochMintNotSent)
	assert.Empty(t, rec.recordedPipelines())
	assert.False(t, mr.Exists("k"))
}

// 启动探针:开启时发一次 WAITAOF,被拒即报错(svc 据此拒启);关闭时一条命令都不发。
func TestProbe(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	require.Error(t, m.Probe(context.Background()), "miniredis 不认 WAITAOF,探针必须失败")
	assert.Equal(t, []string{"waitaof"}, rec.recordedSingles())

	off, offRec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)})
	require.NoError(t, off.Probe(context.Background()))
	assert.Empty(t, offRec.recordedSingles())
	assert.False(t, off.WaitsForAOF())
}

// testRedisError 实现 goredis.Error:模拟 Redis 的错误回复。
type testRedisError string

func (e testRedisError) Error() string { return string(e) }
func (testRedisError) RedisError()     {}

func TestJudgeWaitAOFReply(t *testing.T) {
	reply := func(vals ...any) *goredis.Cmd {
		cmd := goredis.NewCmd(context.Background(), "WAITAOF")
		cmd.SetVal(vals)
		return cmd
	}
	failed := func(err error) *goredis.Cmd {
		cmd := goredis.NewCmd(context.Background(), "WAITAOF")
		cmd.SetErr(err)
		return cmd
	}
	cases := []struct {
		name                  string
		cmd                   *goredis.Cmd
		numLocal, numReplicas int
		want                  MintDurability
		wantUnexpectedShape   bool
	}{
		{"local_and_replica_acked", reply(int64(1), int64(1)), 1, 1, MintDurabilityConfirmed, false},
		{"more_replicas_than_needed", reply(int64(1), int64(2)), 1, 1, MintDurabilityConfirmed, false},
		{"single_instance_local_only", reply(int64(1), int64(0)), 1, 0, MintDurabilityConfirmed, false},
		{"replica_missing", reply(int64(1), int64(0)), 1, 1, MintDurabilityNotDurable, false},
		{"local_fsync_missing", reply(int64(0), int64(1)), 1, 1, MintDurabilityNotDurable, false},
		{"wrong_arity", reply(int64(1)), 1, 0, MintDurabilityError, true},
		{"negative_count", reply(int64(-1), int64(0)), 1, 0, MintDurabilityError, true},
		{"redis_rejected", failed(testRedisError("ERR WAITAOF cannot be used when numlocal is set but appendonly is disabled.")), 1, 0, MintDurabilityError, false},
		{"transport_error", failed(io.EOF), 1, 0, MintDurabilityError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := judgeWaitAOFReply(tc.cmd, tc.numLocal, tc.numReplicas)
			assert.Equal(t, tc.want, got)
			if tc.want == MintDurabilityConfirmed {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantUnexpectedShape, errors.Is(err, errUnexpectedWaitAOFReply))
		})
	}
}

func TestWaitAOFBudgetMs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	margin := 100 * time.Millisecond
	configured := 1500 * time.Millisecond

	got, ok := waitAOFBudgetMs(now, time.Time{}, false, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(1500), got, "没有截止时间:取配置值")

	got, ok = waitAOFBudgetMs(now, now.Add(5*time.Second), true, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(1500), got, "剩余充裕:取配置值")

	got, ok = waitAOFBudgetMs(now, now.Add(900*time.Millisecond), true, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(800), got, "剩余不足:取剩余 − 余量")

	got, ok = waitAOFBudgetMs(now, now.Add(margin+1900*time.Microsecond), true, configured, margin)
	assert.True(t, ok)
	assert.Equal(t, int64(1), got, "先截断成整毫秒")

	_, ok = waitAOFBudgetMs(now, now.Add(margin+900*time.Microsecond), true, configured, margin)
	assert.False(t, ok, "不足 1ms 不得发送(timeout 0 = 无限阻塞)")

	_, ok = waitAOFBudgetMs(now, now.Add(-time.Second), true, configured, margin)
	assert.False(t, ok, "已过截止")
}

func TestOwnerEpochMintRedisOptions(t *testing.T) {
	ack := mintAOFAck{numLocal: 1, numReplicas: 1, timeout: 1500 * time.Millisecond}
	opts, err := ownerEpochMintRedisOptions(redis.RedisConf{Host: "redis.infra:6379", Type: redis.NodeType, User: "u", Pass: "p"}, ack)
	require.NoError(t, err)
	assert.Equal(t, "redis.infra:6379", opts.Addr)
	assert.Equal(t, "u", opts.Username)
	assert.Equal(t, "p", opts.Password)
	assert.Equal(t, 0, opts.DB)
	assert.Equal(t, ownerEpochMintPoolSize, opts.PoolSize)
	assert.Equal(t, 0, opts.MinIdleConns)
	assert.Equal(t, -1, opts.MaxRetries, "整条 pipeline 的自动重发必须关掉,重发由 EvalAndConfirm 自己按「EVAL 结果未知」判")
	assert.Equal(t, ack.timeout+ownerEpochMintReadTimeoutSlack, opts.ReadTimeout, "WAITAOF 的服务端超时先于客户端读超时到期")
	assert.True(t, opts.ContextTimeoutEnabled)
	assert.Nil(t, opts.TLSConfig)

	_, err = ownerEpochMintRedisOptions(redis.RedisConf{Host: "a:1,b:2", Type: redis.ClusterType}, ack)
	require.Error(t, err, "cluster 句柄拒绝:WAITAOF 按单实例连接的写偏移定义,ClusterClient 的 pipeline 会把无键的 WAITAOF 发到别的节点")

	_, err = ownerEpochMintRedisOptions(redis.RedisConf{Host: " ", Type: redis.NodeType}, ack)
	require.Error(t, err)
}

func TestNewRedisOwnerEpochMinterRejectsBadInput(t *testing.T) {
	conn := redis.RedisConf{Host: "127.0.0.1:6379", Type: redis.NodeType}
	_, err := NewRedisOwnerEpochMinter(conn, config.OwnerEpochMintConfig{}, nil)
	require.Error(t, err, "墙钟不得为 nil")

	_, err = NewRedisOwnerEpochMinter(conn, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(2)}, WallClockMs)
	require.Error(t, err, "numlocal 只能是 0 或 1")

	_, err = NewRedisOwnerEpochMinter(conn, config.OwnerEpochMintConfig{WaitAOFTimeoutMs: -1}, WallClockMs)
	require.Error(t, err)
}
