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
// 是 appendonly no / Redis < 7.2)。份数满足 / 不足的判定用纯函数 judgeWaitAOFReply 覆盖;单实例真 Redis 上的端到端用例
// 在 owner_epoch_mint_realredis_test.go(设 MMORPG_TEST_REDIS_ADDR 才跑),切主演练见设计 §8.1 G3b 与 §8.2 D2。
// ---------------------------------------------------------------------------

// pipelineRecorder 是挂在铸造专用客户端上的 go-redis hook:记下每条含 EVAL / WAITAOF 的 pipeline 的命令名、
// 每条单发的 EVAL / WAITAOF;faults 非空时先按顺序把这些故障作用在接下来的几条这样的 pipeline 上(见 pipelineFault);
// failFirst > 0 时前 failFirst 条这样的 pipeline 不发出去,逐条命令按传输错误(io.EOF)
// 失败 —— 模拟「EVAL 结果未知」;replies 非空时前几条按顺序以这些错误回复失败,同样不发出去 —— 模拟服务端回 MOVED 等。
// 别的命令不记也不拦:go-redis 建连时的 HELLO 等握手命令也会经过克隆来的 hook。
type pipelineRecorder struct {
	mu        sync.Mutex
	pipelines [][]string
	singles   []string
	faults    []pipelineFault
	failFirst int
	replies   []error
}

// pipelineFault 是 pipelineRecorder.faults 里的一种故障。
type pipelineFault uint8

const (
	// faultEOF:不发出去,逐条命令按传输错误(io.EOF)失败 —— 模拟「发出去了、结果未知」,同 failFirst。
	faultEOF pipelineFault = iota
	// faultUnsent:不发出去,也不给任何一条命令设错误或值,只返回 goredis.ErrPoolTimeout。go-redis 9.17 拿不到连接(池等待超时、
	// 拨号失败、新连接握手失败)时就是这样:MaxRetries=0 的 generalProcessPipeline 不调 setCmdsErr,错误只从 Pipelined 返回。
	faultUnsent
)

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
		faulted, fault := false, faultEOF
		if len(r.faults) > 0 {
			faulted, fault, r.faults = true, r.faults[0], r.faults[1:]
		}
		fail := faulted && fault == faultEOF
		if !faulted && r.failFirst > 0 {
			fail = true
			r.failFirst--
		}
		var reply error
		if !faulted && !fail && len(r.replies) > 0 {
			reply, r.replies = r.replies[0], r.replies[1:]
		}
		r.mu.Unlock()
		if faulted && fault == faultUnsent {
			return goredis.ErrPoolTimeout
		}
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
	assert.False(t, errors.Is(err, ErrOwnerEpochMintResultUnknown), "首发就被拒:确定没执行,不是结果未知")
	assert.Equal(t, 1, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), 1)
}

// 首发结果未知之后的重发被 Redis 拒绝(例如 OOM):这一次的拒绝说明不了首发有没有执行,必须仍报结果未知,
// 调用方才会按「可能已写」回滚,而不是当成没写。
func TestEvalAndConfirmRejectAfterUnknownStaysUnknown(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.faults = []pipelineFault{faultEOF}
	rec.replies = []error{testRedisError("OOM command not allowed when used memory > 'maxmemory'.")}

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOwnerEpochMintResultUnknown)
	assert.Equal(t, 2, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), 2)
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
	assert.ErrorIs(t, err, ErrOwnerEpochMintResultUnknown, "调用方据此按「可能已写」回滚")
	assert.Equal(t, ownerEpochMintAttempts, res.Attempts)
	assert.Len(t, rec.recordedPipelines(), ownerEpochMintAttempts)
}

// 连接都没拿到时 pipeline 一个字节都没发出去,go-redis 不给命令设错误或值、只从 Pipelined 返回错误。必须回
// ErrOwnerEpochMintNotSent,不能当成「EVAL 有回复、回复是 nil」交回 err == nil(以前的行为:铸造报「CAS 返回值非法」,
// 回滚被当成结果未知、不执行也不兜底);也不在通道里重发,剩下的预算留给调用方。
func TestEvalAndConfirmUnsentPipelineIsNotSentAndNotResent(t *testing.T) {
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.faults = []pipelineFault{faultUnsent}

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.ErrorIs(t, err, ErrOwnerEpochMintNotSent)
	assert.ErrorIs(t, err, goredis.ErrPoolTimeout, "原因留在包装链里")
	assert.Nil(t, res.Reply)
	assert.Equal(t, 0, res.Attempts, "一次都没到达 Redis")
	assert.Len(t, rec.recordedPipelines(), 1, "确定没发出去的不在通道里重发")
	assert.False(t, mr.Exists(testMintKey))
}

// 真实的取连接失败:地址上已经没有 Redis(拨号失败)。不靠替身,钉住 go-redis 9.17 真实的行为与上一条一致。
func TestEvalAndConfirmDialFailureIsNotSent(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	m, err := NewRedisOwnerEpochMinter(playerredis.Config{Addrs: []string{addr}}, config.OwnerEpochMintConfig{}, func() uint64 { return 42 })
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := m.EvalAndConfirm(ctx, testIncrScript, []string{testMintKey})
	require.ErrorIs(t, err, ErrOwnerEpochMintNotSent)
	assert.Nil(t, res.Reply)
	assert.Equal(t, 0, res.Attempts)
}

// 已有一次结果未知的发送(首发可能已生效)之后,重发连连接都没拿到:仍按结果未知返回,不能报成「一个字节都没改」。
func TestEvalAndConfirmUnsentResendAfterUnknownStaysUnknown(t *testing.T) {
	m, rec, _ := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	rec.faults = []pipelineFault{faultEOF, faultUnsent}

	res, err := m.EvalAndConfirm(context.Background(), testIncrScript, []string{testMintKey})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrOwnerEpochMintNotSent), "首发结果未知,不能报成没发")
	assert.ErrorIs(t, err, ErrOwnerEpochMintResultUnknown)
	assert.Equal(t, 1, res.Attempts, "只有首发那一次可能到达过 Redis")
	assert.Len(t, rec.recordedPipelines(), 2)
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
	m, rec, mr := newRecordedMinter(t, config.OwnerEpochMintConfig{})
	require.Error(t, m.Probe(context.Background()), "miniredis 不认 WAITAOF,探针必须失败")
	assert.Empty(t, rec.recordedSingles(), "探针不单发 WAITAOF")
	assert.Equal(t, [][]string{{"set", "waitaof"}}, rec.recordedPipelines(),
		"先在同一条连接上写探针键,WAITAOF 才有偏移可等(新连接偏移为 0,不先写就查不出副本没开 AOF)")
	probeKey := ownerEpochMintProbeKeyPrefix + "{0}"
	assert.True(t, mr.Exists(probeKey), "单实例第一个候选就是本节点")
	assert.Greater(t, mr.TTL(probeKey), time.Duration(0), "探针键带 TTL,自然消失")

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
	assert.Equal(t, int((max(ack.timeout, ownerEpochMintProbeWaitAOFTimeout) + ownerEpochMintReadTimeoutSlack).Milliseconds()), got.ReadTimeoutMs,
		"读超时盖住铸造与启动探针里最长的 WAITAOF 等待,服务端超时先于客户端读超时到期")
	assert.Greater(t, got.ReadTimeoutMs, int(ownerEpochMintProbeWaitAOFTimeout.Milliseconds()),
		"探针的 WAITAOF 在 pipeline 里,go-redis 按客户端 ReadTimeout 设读截止,读超时短于探针等待时探针会误报成连接问题")
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

// 池上限按 Little 定律定(ownerEpochMintPoolSize 的注释):每节点 = ⌈每个 SM 的铸造速率 × WAITAOF 等待上限 / 最少主节点数⌉。
// 钉住的是算式本身:谁改了速率目标、等待上限(config.MaxOwnerEpochMintWaitAOFTimeoutMs)或主节点下限而没重算字面量,这里变红;
// 字面量变了还要同步 k8s_deploy.ps1 连接总账的 PlayerFixed(契约测试从源码读这一行)。
func TestOwnerEpochMintPoolSizeFollowsLittlesLaw(t *testing.T) {
	concurrentPerSM := ownerEpochMintTargetRatePerSecond * config.MaxOwnerEpochMintWaitAOFTimeoutMs // 次/s × 毫秒
	perNode := (concurrentPerSM + ownerEpochMintMinPlayerRedisMasters*1000 - 1) / (ownerEpochMintMinPlayerRedisMasters * 1000)
	assert.Equal(t, perNode, ownerEpochMintPoolSize,
		"ownerEpochMintPoolSize 必须等于 ⌈%d/s × %dms / %d 个主节点⌉", ownerEpochMintTargetRatePerSecond,
		config.MaxOwnerEpochMintWaitAOFTimeoutMs, ownerEpochMintMinPlayerRedisMasters)
}
