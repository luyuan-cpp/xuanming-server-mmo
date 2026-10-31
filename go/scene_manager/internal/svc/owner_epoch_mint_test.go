package svc

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"scene_manager/internal/config"
	"shared/playerredis"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// 失败 —— 模拟「EVAL 结果未知」;replies 非空时前几条按顺序以这些错误回复失败,同样不发出去 —— 模拟服务端回 MOVED 等。
// 别的命令不记也不拦:go-redis 建连时的 HELLO 等握手命令也会经过克隆来的 hook。
type pipelineRecorder struct {
	mu        sync.Mutex
	pipelines [][]string
	singles   []string
	failFirst int
	replies   []error
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
		var reply error
		if !fail && len(r.replies) > 0 {
			reply, r.replies = r.replies[0], r.replies[1:]
		}
		r.mu.Unlock()
		if reply != nil {
			for _, c := range cmds {
				c.SetErr(reply)
			}
			return reply
		}
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
	m, err := NewRedisOwnerEpochMinter(playerredis.Config{Addrs: []string{mr.Addr()}}, mint, func() uint64 { return 42 })
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	rec := &pipelineRecorder{}
	m.client.AddHook(rec)
	return m, rec, mr
}

const (
	testIncrScript = `return redis.call("INCR", KEYS[1])`
	// testMintKey 带玩家 tag:铸造通道发送前按玩家域的键形规则核对。
	testMintKey = "player:{42}:owner_epoch"
)

// 铸造 EVAL 与 WAITAOF 在同一个 pipeline 里发(go-redis 的一条 pipeline 只占一条连接):WAITAOF 等的是本连接
// 最后一次写的偏移,分两次发就可能落到不同连接上。EVAL 的回复原样交回,WAITAOF 被拒记 Error。
func TestEvalAndConfirmSendsEvalAndWaitAOFInOnePipeline(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Reply)
	assert.Equal(t, 1, res.Attempts)
	assert.Equal(t, [][]string{{"eval", "waitaof"}}, rec.recordedPipelines())
	assert.Equal(t, MintDurabilityError, res.Durability, "miniredis 不认 WAITAOF:错误回复 = 落没落盘不知道")
	require.Error(t, res.DurableErr)
	v, _ := mr.Get(testMintKey)
	assert.Equal(t, "1", v)
}

// 两项份数都配 0:只发 EVAL,结论 Disabled。
func TestEvalAndConfirmDisabledSendsOnlyEval(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)})

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.NoError(t, err)
	assert.Equal(t, MintDurabilityDisabled, res.Durability)
	assert.NoError(t, res.DurableErr)
	assert.Equal(t, [][]string{{"eval"}}, rec.recordedPipelines())
}

// 脚本的错误回复是确定性的:不重发,返回 error。
func TestEvalAndConfirmScriptErrorIsNotResent(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})

	res, err := m.EvalAndConfirm(context.Background(), `return redis.error_reply("boom")`, []string{testMintKey})
	require.Error(t, err)
	assert.Equal(t, 1, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), 1)
}

// EVAL 结果未知(传输错误)时在新连接上重发整条 pipeline;这里首发根本没发出去,所以脚本只执行了一次。
func TestEvalAndConfirmResendsWholePipelineWhenEvalResultUnknown(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.failFirst = 1

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Attempts)
	assert.Equal(t, [][]string{{"eval", "waitaof"}, {"eval", "waitaof"}}, rec.recordedPipelines())
	v, _ := mr.Get(testMintKey)
	assert.Equal(t, "1", v)
}

// 重发次数有上限,耗尽按「结果未知」返回 error(调用方回 3,不当作没写)。
func TestEvalAndConfirmGivesUpAfterBoundedAttempts(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.failFirst = 100

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
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

	_, err := m.EvalAndConfirm(ctx, testIncrScript, []string{testMintKey})
	require.ErrorIs(t, err, ErrOwnerEpochMintNotSent)
	assert.Empty(t, rec.recordedPipelines())
	assert.False(t, mr.Exists(testMintKey))
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

func TestOwnerEpochMintClientConfig(t *testing.T) {
	ack := mintAOFAck{numLocal: 1, numReplicas: 1, timeout: 1500 * time.Millisecond}
	conn := playerredis.Config{Addrs: []string{"redis.infra:6379"}, Username: "u", Password: "p", PoolSizePerNode: 7, ReadTimeoutMs: 100, DisableContextTimeout: true}
	got := ownerEpochMintClientConfig(conn, ack)
	assert.Equal(t, []string{"redis.infra:6379"}, got.Addrs)
	assert.Equal(t, "u", got.Username)
	assert.Equal(t, "p", got.Password)
	assert.Equal(t, ownerEpochMintPoolSize, got.PoolSizePerNode, "专用池固定大小,不跟玩家域句柄的配置走")
	assert.Equal(t, int((ack.timeout + ownerEpochMintReadTimeoutSlack).Milliseconds()), got.ReadTimeoutMs, "WAITAOF 的服务端超时先于客户端读超时到期")
	assert.Equal(t, int(ownerEpochMintWriteTimeout.Milliseconds()), got.WriteTimeoutMs)
	assert.False(t, got.DisableContextTimeout, "取连接排队与读写都受调用方 ctx 截止约束")
	require.NoError(t, got.Validate())

	cluster := ownerEpochMintClientConfig(playerredis.Config{Addrs: []string{"a:1", "b:2"}, Cluster: true}, mintAOFAck{timeout: time.Duration(config.MaxOwnerEpochMintWaitAOFTimeoutMs) * time.Millisecond})
	assert.True(t, cluster.Cluster, "集群形态原样保留:EVAL 与 WAITAOF 发给持有 slot 的主节点")
	require.NoError(t, cluster.Validate(), "配置上限下的超时仍在 playerredis 的合法范围内")
}

// 键形不合规是确定性的编程错误:一条都不发。专用客户端不挂守卫,这条由 EvalAndConfirm 自己守。
func TestEvalAndConfirmRejectsBadKeysBeforeSending(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})

	_, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{"untagged"})
	require.ErrorIs(t, err, playerredis.ErrUntaggedKey)
	assert.False(t, errors.Is(err, ErrOwnerEpochMintNotSent))

	_, err = m.EvalAndConfirm(context.Background(), testIncrScript, []string{"player:{42}:owner_epoch", "player:{1}:owner_epoch"})
	require.ErrorIs(t, err, playerredis.ErrCrossSlot)

	_, err = m.EvalAndConfirm(context.Background(), testIncrScript, nil)
	require.Error(t, err)

	assert.Empty(t, rec.recordedPipelines())
	assert.False(t, mr.Exists("untagged"))
}

// MOVED / TRYAGAIN / CLUSTERDOWN / LOADING / READONLY 表示没执行:重发,不计入结果未知的次数(Attempts 仍是 1,
// 脚本回 0 时调用方不会误以为首发可能已生效)。
func TestEvalAndConfirmResendsNotExecutedWithoutCountingAttempts(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.replies = []error{
		testRedisError("MOVED 3999 127.0.0.1:6381"),
		testRedisError("TRYAGAIN Multiple keys request during rehashing of slot"),
		testRedisError("CLUSTERDOWN The cluster is down"),
		testRedisError("LOADING Redis is loading the dataset in memory"),
		testRedisError("READONLY You can't write against a read only replica."),
	}

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), 6)
	assert.Equal(t, int64(1), res.Reply)
	v, _ := mr.Get(testMintKey)
	assert.Equal(t, "1", v, "前面几次都没执行,脚本只跑了一次")
}

// 一直重定向:次数有上限,回 error,不无限等。
func TestEvalAndConfirmNotExecutedHasALimit(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	for range ownerEpochMintNotExecutedLimit + 5 {
		rec.replies = append(rec.replies, testRedisError("TRYAGAIN Multiple keys request during rehashing of slot"))
	}

	_, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.Error(t, err)
	assert.Len(t, rec.recordedPipelines(), ownerEpochMintNotExecutedLimit+1)
	assert.False(t, mr.Exists(testMintKey))
}

// 重定向的等待受 ctx 约束:ctx 结束就不再等,按错误返回。
func TestEvalAndConfirmNotExecutedStopsWhenContextEnds(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)})
	rec.replies = []error{testRedisError("MOVED 3999 127.0.0.1:6381")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.EvalAndConfirm(ctx, testIncrScript, []string{testMintKey})
	require.Error(t, err)
	assert.Len(t, rec.recordedPipelines(), 1)
}

// ASK:这一次去目标节点问,同一条 pipeline 前置 ASKING。目标节点用临时客户端连,这里就是同一个 miniredis。
func TestEvalAndConfirmAskGoesToTargetNode(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(0), WaitAOFNumReplicas: intPtr(0)})
	rec.replies = []error{testRedisError("ASK 3999 " + mr.Addr())}

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), 1, "第二次发在临时客户端上,不经主客户端的 hook")
	v, _ := mr.Get(testMintKey)
	assert.Equal(t, "1", v)
}

func TestNewRedisOwnerEpochMinterRejectsBadInput(t *testing.T) {
	conn := playerredis.Config{Addrs: []string{"127.0.0.1:6379"}}
	_, err := NewRedisOwnerEpochMinter(conn, config.OwnerEpochMintConfig{}, nil)
	require.Error(t, err, "墙钟不得为 nil")

	_, err = NewRedisOwnerEpochMinter(conn, config.OwnerEpochMintConfig{WaitAOFNumLocal: intPtr(2)}, WallClockMs)
	require.Error(t, err, "numlocal 只能是 0 或 1")

	_, err = NewRedisOwnerEpochMinter(conn, config.OwnerEpochMintConfig{WaitAOFTimeoutMs: -1}, WallClockMs)
	require.Error(t, err)

	_, err = NewRedisOwnerEpochMinter(playerredis.Config{}, config.OwnerEpochMintConfig{}, WallClockMs)
	require.Error(t, err, "玩家域配置没有地址")
}
