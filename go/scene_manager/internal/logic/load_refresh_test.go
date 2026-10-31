package logic

// load_refresh.go 的判定函数与边界用例(用到新符号;行为用例见 load_refresh_scope_test.go)。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"scene_manager/internal/metrics"
)

// allLoadRefreshOutcomesForTest 是 load_refresh_total 的全部 outcome。
var allLoadRefreshOutcomesForTest = []string{
	metrics.LoadRefreshOutcomeOK, metrics.LoadRefreshOutcomePartial, metrics.LoadRefreshOutcomeReadFailed,
	metrics.LoadRefreshOutcomeRegistryFailed, metrics.LoadRefreshOutcomeWriteFailed, metrics.LoadRefreshOutcomeLostLeadership,
}

// loadRefreshTicksForTest 是 load_refresh_total 各 outcome 之和,即领导者记过的刷新拍数。
func loadRefreshTicksForTest(t *testing.T) float64 {
	t.Helper()
	sum := 0.0
	for _, outcome := range allLoadRefreshOutcomesForTest {
		sum += loadRefreshOutcomeCountForTest(t, outcome)
	}
	return sum
}

// redisReplyErrorForTest 模拟 go-redis 的逐条回复错误类型(与 proto.RedisError 一样带 RedisError() 方法)。
type redisReplyErrorForTest string

func (e redisReplyErrorForTest) Error() string { return string(e) }

func (redisReplyErrorForTest) RedisError() {}

// pipeline 里一条计数 GET 的判定。熔断拒绝时命令没执行、err 也不回填,结果是 ("", nil):
// 它绝不能被读成「计数为 0、读取成功」。
func TestClassifyCounterReply(t *testing.T) {
	someErr := errors.New("first failed command in the batch")
	cases := []struct {
		name          string
		val           string
		err, batchErr error
		wantCount     int64
		wantOK        bool
	}{
		{"breaker open, command never ran", "", nil, breaker.ErrServiceUnavailable, 0, false},
		{"breaker open, wrapped", "", nil, fmt.Errorf("pipeline: %w", breaker.ErrServiceUnavailable), 0, false},
		{"not executed and no batch error", "", nil, nil, 0, false},
		{"key missing", "", redis.Nil, redis.Nil, 0, true},
		{"value present while another command failed", "7", nil, someErr, 7, true},
		{"value present", "42", nil, nil, 42, true},
		{"command error", "", errors.New("WRONGTYPE"), someErr, 0, false},
		{"malformed value", "abc", nil, nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classifyCounterReply(c.val, c.err, c.batchErr)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantCount, got)
		})
	}
}

// memberCountReplyForTest 是一条已执行的 SCARD,判定只看它存不存在,取值由 err 参数表达。
type memberCountReplyForTest struct{}

func (memberCountReplyForTest) Result() (int64, error) { return 0, nil }

// 一条 SCARD 可不可信:键不存在回 0 是可信的「没有人」,熔断拒绝留下的零值不是。
func TestClassifyMemberCountReply(t *testing.T) {
	someErr := errors.New("first failed command in the batch")
	executed := memberCountReplyForTest{}
	cases := []struct {
		name          string
		reply         memberCountReply
		err, batchErr error
		wantOK        bool
	}{
		{"callback never ran", nil, nil, nil, false},
		{"breaker open, command never ran", executed, nil, breaker.ErrServiceUnavailable, false},
		{"breaker open, wrapped", executed, nil, fmt.Errorf("pipeline: %w", breaker.ErrServiceUnavailable), false},
		{"key missing reads as nobody", executed, nil, nil, true},
		{"count present while another command failed", executed, nil, someErr, true},
		{"command error", executed, errors.New("WRONGTYPE"), someErr, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.wantOK, classifyMemberCountReply(c.reply, c.err, c.batchErr))
		})
	}
}

// 整批是否按「没执行」处理。
func TestPipelineBatchRejected(t *testing.T) {
	someErr := errors.New("boom")
	cases := []struct {
		name     string
		batchErr error
		cmdErrs  []error
		want     bool
	}{
		{"executed cleanly", nil, []error{nil, nil}, false},
		{"redis.Nil is the first error", redis.Nil, []error{redis.Nil, nil}, false},
		{"breaker open", breaker.ErrServiceUnavailable, []error{nil, nil}, true},
		{"breaker open even if a command carries an error", breaker.ErrServiceUnavailable, []error{someErr}, true},
		{"error landed on no command", someErr, []error{nil, nil}, true},
		{"error landed on no command (callback never ran)", someErr, []error{nil}, true},
		{"commands carry their own errors", someErr, []error{nil, someErr}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, pipelineBatchRejected(c.batchErr, c.cmdErrs))
		})
	}
}

// 传输层失败(记 read_failed)与个别键的回复错误(只记 partial)分开,判定与批怎么切无关。
func TestPipelineHitTransportError(t *testing.T) {
	wrongType := redisReplyErrorForTest("WRONGTYPE Operation against a key holding the wrong kind of value")
	cases := []struct {
		name    string
		cmdErrs []error
		want    bool
	}{
		{"all ok", []error{nil, nil}, false},
		{"key missing is a reply", []error{redis.Nil, nil}, false},
		{"per-key reply error", []error{nil, wrongType}, false},
		{"wrapped reply error", []error{fmt.Errorf("get: %w", wrongType)}, false},
		{"every command carries a reply error (single-node batch)", []error{wrongType, wrongType}, false},
		{"connection dropped mid-batch", []error{nil, nil, io.EOF, io.EOF}, true},
		{"read timeout", []error{context.DeadlineExceeded, context.DeadlineExceeded}, true},
		{"errors left by different retry attempts", []error{io.EOF, errors.New("dial tcp: connection refused")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, pipelineHitTransportError(c.cmdErrs))
		})
	}
}

// 多种情况同时出现时只记一个 outcome;告警口径的三种排在前面。
func TestLoadRefreshReportOutcomePriority(t *testing.T) {
	cases := []struct {
		report loadRefreshReport
		want   string
	}{
		{loadRefreshReport{}, metrics.LoadRefreshOutcomeOK},
		{loadRefreshReport{nodeReadErrors: 1}, metrics.LoadRefreshOutcomePartial},
		{loadRefreshReport{nodeReadErrors: 1, lostLeadership: true}, metrics.LoadRefreshOutcomeLostLeadership},
		{loadRefreshReport{lostLeadership: true, registryFailed: true}, metrics.LoadRefreshOutcomeRegistryFailed},
		{loadRefreshReport{registryFailed: true, readTransportFailed: true, nodeReadErrors: 3}, metrics.LoadRefreshOutcomeReadFailed},
		{loadRefreshReport{readTransportFailed: true, writeFailed: true}, metrics.LoadRefreshOutcomeWriteFailed},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.report.outcome(), "%+v", c.report)
	}
}

// 读失败的非成员以哨兵分加入;哨兵分远高于任何真实负载分,且不进每节点指标。
// 本批只有这一个节点、它读到的是逐条回复错误:仍只记 partial —— read_failed 只记传输层失败,不能因为
// 坏键恰好独占一批(或部署里只有 1 个节点)就升级成 read_failed。
func TestRefreshLoadScores_NonMemberReadFailureUsesSentinelScore(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-966", 1, "966")
	failedKey := nodeMembersKey(1, "966")
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "SCARD" && len(args) > 0 && args[0] == failedKey {
			return "ERR injected counter read failure"
		}
		return ""
	})
	readFailedBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeReadFailed)
	partialBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomePartial)

	refreshLoadScores(sc)

	score, ok := loadScoreForTest(t, mr, 1, "966")
	require.True(t, ok)
	assert.Equal(t, float64(nodeLoadScoreUnknown), score)
	_, exported := nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")["1/966"]
	assert.False(t, exported, "哨兵分不得作为负载导出")
	assert.Equal(t, partialBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomePartial),
		"逐条回复错误不论批多大都记 partial")
	assert.Equal(t, readFailedBefore, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeReadFailed))
}

// 连接在读批中途断开(传输层失败):受影响的节点保留旧分,记 read_failed。用真实客户端走一遍,钉住
// 「go-redis 回填到命令上的网络错误不是 Redis 回复错误」这一判定前提。
func TestRefreshLoadScores_ConnectionDropDuringReadIsReadFailed(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	// 977 排在 978 前面,先读;978 的 GET 上断开连接。
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-977", 1, "977")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-978", 1, "978")
	_, err := mr.ZAdd(nodeLoadKey(1), 100, "978")
	require.NoError(t, err)
	seedNodeCountersForTest(t, mr, 1, "977", "2", "")
	seedNodeCountersForTest(t, mr, 1, "978", "5", "")
	dropKey := nodeSceneCountKey(1, "978")
	// 不回复、关掉连接:客户端在这条 GET 上读到 EOF(或连接被重置),go-redis 把它回填到这条及后续命令,
	// 重试几轮后放弃。写批只有 SET / ZADD,不受影响。钩子里不调 mr.*(同 installRedisCommandRecorderForTest)。
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd == "GET" && len(args) > 0 && args[0] == dropKey {
			c.Close()
			return true
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	readFailedBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeReadFailed)
	partialBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomePartial)

	refreshLoadScores(sc)

	score, ok := loadScoreForTest(t, mr, 1, "978")
	require.True(t, ok, "读失败不得摘掉成员资格")
	assert.Equal(t, 100.0, score, "传输层读失败保留旧分")
	// 977 是否读成功取决于这次错误是否可重试(可重试时最后一轮里它在断点之前、已读到值;不可重试时
	// go-redis 把错误回填到整批),这里只要求它仍在负载集里。
	_, ok = loadScoreForTest(t, mr, 1, "977")
	assert.True(t, ok)
	assert.Equal(t, readFailedBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeReadFailed))
	assert.Equal(t, partialBefore, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomePartial))
}

// 读的途中失去领导权:写之前再判一次,一条写都不发(降级后的写正是单写者不变量要防的)。
func TestRefreshLoadScores_LosingLeadershipDuringReadsWritesNothing(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	var leading atomic.Bool
	leading.Store(true)
	t.Cleanup(SetLeaderCheckForTest(leading.Load))
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-967", 1, "967")
	_, err := mr.ZAdd(nodeLoadKey(1), 100, "967")
	require.NoError(t, err)
	seedNodeCountersForTest(t, mr, 1, "967", "5", "")
	// 钩子里只改自己的状态,不调 mr.*(见 installRedisCommandRecorderForTest)。
	rec := installRedisCommandRecorderForTest(t, mr, func(cmd string, _ []string) string {
		if cmd == "GET" {
			leading.Store(false)
		}
		return ""
	})
	lostBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeLostLeadership)

	refreshLoadScores(sc)

	assert.Zero(t, rec.count("ZADD", ""))
	assert.Zero(t, rec.count("SET", ""))
	score, ok := loadScoreForTest(t, mr, 1, "967")
	require.True(t, ok)
	assert.Equal(t, 100.0, score)
	assert.Equal(t, lostBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeLostLeadership))
}

// 一个 zone 的节点数超过单批上限时照样全部刷新(读写都分成多批,批与批之间不丢节点,跨批的读结果回填到
// 对的节点上)。
func TestRefreshLoadScores_SpansMultiplePipelineBatches(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	const zone = uint32(99)
	// 读每批 512 个节点,共 3 批(第 3 批是 991024..991026);写 = 每节点一条 SET + 一条 ZADD,共 1028 条、2 批,
	// 991026 的类型镜像是第 1027 条,落在第 2 个写批。
	total := loadRefreshPipelineMaxCmds + 3
	// 一千多个节点的每节点序列不留给后面的用例。
	t.Cleanup(metrics.ResetNodeSeries)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("99%04d", i)
		registerKnownNodeForTest(t, "SceneNodeService.rpc/batch-"+id, zone, id)
	}
	// 第 1 批与最后一批各钉一个不同的计数:批内回填若错位到别的节点,分数会对不上。
	require.NoError(t, mr.Set(nodeSceneCountKey(zone, "990000"), "4"))
	require.NoError(t, mr.Set(nodeSceneCountKey(zone, "991026"), "9"))
	okBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeOK)

	refreshLoadScores(sc)

	assert.Len(t, loadSetMembersForTest(t, mr, zone), total)
	score, ok := loadScoreForTest(t, mr, zone, "990000")
	require.True(t, ok)
	assert.Equal(t, 4.0, score)
	score, ok = loadScoreForTest(t, mr, zone, "991026")
	require.True(t, ok)
	assert.Equal(t, 9.0, score, "最后一个读批的结果回填到对的节点(1·9 + 0.01·0)")
	score, ok = loadScoreForTest(t, mr, zone, "991025")
	require.True(t, ok)
	assert.Zero(t, score, "相邻节点不得拿到 991026 的计数")
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(zone, "991026")), "第 2 个写批照常执行")
	assert.Equal(t, okBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeOK))
}

// 写批里一条 ZADD 失败:只有那个 zone 的分数没更新,同一 pipeline 里别的 zone 的 ZADD 与全部类型镜像照常
// 生效(pipeline 不是事务);记 write_failed;没写进 Redis 的分数不作为每节点指标导出。
func TestRefreshLoadScores_ZaddFailureKeepsOtherZonesWritten(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-972", 1, "972")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-973", 2, "973")
	_, err := mr.ZAdd(nodeLoadKey(1), 100, "972")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(2), 100, "973")
	require.NoError(t, err)
	seedNodeCountersForTest(t, mr, 1, "972", "3", "")
	seedNodeCountersForTest(t, mr, 2, "973", "4", "")
	failedKey := nodeLoadKey(2)
	var injected atomic.Bool // 只注入一次,远低于 go-zero 熔断阈值
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "ZADD" && len(args) > 0 && args[0] == failedKey && injected.CompareAndSwap(false, true) {
			return "ERR injected zadd failure"
		}
		return ""
	})
	writeFailedBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeWriteFailed)

	refreshLoadScores(sc)

	require.True(t, injected.Load(), "前提:注入的 ZADD 失败确实发生了")
	score, ok := loadScoreForTest(t, mr, 1, "972")
	require.True(t, ok)
	assert.Equal(t, 3.0, score, "同一 pipeline 里别的 zone 的 ZADD 照常生效")
	score, ok = loadScoreForTest(t, mr, 2, "973")
	require.True(t, ok, "ZADD 失败不连带摘掉成员")
	assert.Equal(t, 100.0, score, "失败的 ZADD 不更新分数")
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(1, "972")), "类型镜像照常写")
	assert.True(t, mr.Exists(nodeSceneNodeTypeKey(2, "973")), "失败 zone 的类型镜像也照常写")
	assert.Equal(t, writeFailedBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeWriteFailed))
	series := nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")
	assert.Equal(t, 3.0, series["1/972"])
	_, exported := series["2/973"]
	assert.False(t, exported, "没写进 Redis 的分数不得作为每节点指标导出")
}

// 哨兵 ZADD NX 失败同样记 write_failed(优先于同拍的 partial);同 zone 读成功的节点照常更新。
func TestRefreshLoadScores_SentinelZaddFailureIsWriteFailed(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-974", 1, "974")
	registerKnownNodeForTest(t, "SceneNodeService.rpc/scope-975", 1, "975")
	seedNodeCountersForTest(t, mr, 1, "975", "6", "")
	readFailedKey := nodeSceneCountKey(1, "974")
	var injected atomic.Bool
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		switch {
		case cmd == "GET" && len(args) > 0 && args[0] == readFailedKey:
			return "ERR injected counter read failure"
		case cmd == "ZADD" && len(args) > 1 && args[1] == "NX" && injected.CompareAndSwap(false, true):
			return "ERR injected sentinel zadd failure"
		}
		return ""
	})
	writeFailedBefore := loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeWriteFailed)

	refreshLoadScores(sc)

	require.True(t, injected.Load(), "前提:注入的哨兵 ZADD 失败确实发生了")
	score, ok := loadScoreForTest(t, mr, 1, "975")
	require.True(t, ok)
	assert.Equal(t, 6.0, score)
	assert.Equal(t, writeFailedBefore+1, loadRefreshOutcomeCountForTest(t, metrics.LoadRefreshOutcomeWriteFailed))
}

// watch PUT 走的单节点刷新不计 load_refresh_total:它只在节点注册时发生,混进「每拍刷新」会掩盖停摆
// (告警 SceneManagerLoadRefreshStalled 的口径)。
func TestUpdateNodeLoad_DoesNotCountAsRefreshTick(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	seedNodeCountersForTest(t, mr, 1, "976", "2", "")
	ticksBefore := loadRefreshTicksForTest(t)

	entry := nodeEntry{nodeID: "976"}
	entry.reg.ZoneId = 1
	updateNodeLoad(sc, entry)

	score, ok := loadScoreForTest(t, mr, 1, "976")
	require.True(t, ok, "前提:领导者的单节点刷新确实写了负载集")
	assert.Equal(t, 2.0, score)
	assert.Equal(t, ticksBefore, loadRefreshTicksForTest(t), "单节点刷新不计入 load_refresh_total")
}

// ── 登记集合读不到时的退化 ──────────────────────────────────────────────

// 清扫退回只扫快照里的 zone:照常补 death_at、摘除陈旧成员,返回 true;快照外、只在登记集合里的 zone
// 这一轮扫不到,留给下一次 fullSync。
func TestSweepStaleLoadSetMembers_RegistryReadFailureSweepsSnapshotZones(t *testing.T) {
	sc, mr := newLoadRefreshScopeCtx(t)
	_, err := mr.ZAdd(nodeLoadKey(91), 0, "9101")
	require.NoError(t, err)
	_, err = mr.ZAdd(nodeLoadKey(94), 0, "9401")
	require.NoError(t, err)
	_, err = mr.SAdd(zoneRegistryKeyForTest, "91", "94")
	require.NoError(t, err)
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "SMEMBERS" && len(args) > 0 && args[0] == zoneRegistryKeyForTest {
			return "ERR injected registry read failure"
		}
		return ""
	})
	seen := map[uint32]map[string]struct{}{91: {}}

	require.True(t, sweepStaleLoadSetMembers(sc, seen, []uint32{91}))

	assert.True(t, mr.Exists(nodeDeathAtKey(91, "9101")), "快照 zone 的陈旧成员照常先补记死亡时刻")
	assert.NotContains(t, loadSetMembersForTest(t, mr, 91), "9101")
	assert.Contains(t, loadSetMembersForTest(t, mr, 94), "9401", "登记集合读不到时,快照外的 zone 这一轮不扫")
}

// 孤儿清理退回只查有活节点的 zone,照常清掉其中的孤儿组;只在登记集合里的 zone 这一轮查不到。
func TestCleanupOrphanWorldChannels_RegistryReadFailureFallsBackToActiveZones(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	sc, mr := newAutoscaleCtx(t)
	const activeZone, registeredOnlyZone, deadConf = uint32(92), uint32(94), uint64(779) // 779 不在 World 表里
	registerKnownNodeForTest(t, "SceneNodeService.rpc/orphan-9200", activeZone, "9200")
	rebuildActiveZones()
	seedChannels(t, sc, activeZone, deadConf, map[uint64]int64{9201: 0})
	seedChannels(t, sc, registeredOnlyZone, deadConf, map[uint64]int64{9401: 0})
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "SMEMBERS" && len(args) > 0 && args[0] == zoneRegistryKeyForTest {
			return "ERR injected registry read failure"
		}
		return ""
	})

	assert.Equal(t, 1, CleanupOrphanWorldChannels(context.Background(), sc))

	assert.False(t, mr.Exists(worldChannelsKey(activeZone, deadConf)), "有活节点的 zone 照常清")
	assert.Equal(t, "", mr.HGet(worldDesiredChannelsKey(activeZone), "779"))
	assert.True(t, mr.Exists(worldChannelsKey(registeredOnlyZone, deadConf)), "只在登记集合里的 zone 这一轮查不到")
}

// 某个 zone 的 HKEYS 失败只跳过这个 zone(保留它的发现索引,下一轮重试),别的 zone 的孤儿组照常清。
func TestCleanupOrphanWorldChannels_HkeysFailureSkipsOnlyThatZone(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	resetNodeDeathQueuesForTest(t)
	sc, mr := newAutoscaleCtx(t)
	const failingZone, okZone, deadConf = uint32(88), uint32(89), uint64(780) // 780 不在 World 表里
	// seedChannels 经 setDesiredWorldChannelCount 写期望数字段,并把两个 zone 登记进登记集合。
	seedChannels(t, sc, failingZone, deadConf, map[uint64]int64{8801: 0})
	seedChannels(t, sc, okZone, deadConf, map[uint64]int64{8901: 0})
	failedKey := worldDesiredChannelsKey(failingZone)
	installRedisCommandRecorderForTest(t, mr, func(cmd string, args []string) string {
		if cmd == "HKEYS" && len(args) > 0 && args[0] == failedKey {
			return "ERR injected hkeys failure"
		}
		return ""
	})

	assert.Equal(t, 1, CleanupOrphanWorldChannels(context.Background(), sc))

	assert.True(t, mr.Exists(worldChannelsKey(failingZone, deadConf)), "HKEYS 失败的 zone 这一轮跳过")
	assert.Equal(t, "1", mr.HGet(failedKey, "780"), "跳过的 zone 保留发现索引,下一轮还能找到")
	assert.False(t, mr.Exists(worldChannelsKey(okZone, deadConf)), "别的 zone 照常清")
}

// 每个 outcome 在注册时预置为 0:increase() 才看得到第一次跳变,「刷新停摆」告警才能区分「没人在刷」与「指标不存在」。
func TestLoadRefreshOutcomesArePreseeded(t *testing.T) {
	metrics.ResetNodeSeries() // 触发注册
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, family := range families {
		if family.GetName() != "scene_manager_load_refresh_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == "outcome" {
					seen[pair.GetValue()] = true
				}
			}
		}
	}
	for _, outcome := range allLoadRefreshOutcomesForTest {
		assert.True(t, seen[outcome], "outcome=%s 必须预置", outcome)
	}
}

// 降级时 OnStateChange 调 ResetLeaderGauges,每节点序列一并清掉。
func TestResetLeaderGaugesDropsNodeSeries(t *testing.T) {
	metrics.ObserveNode("968", 1, 0, 1, 2, 3)
	_, exported := nodeGaugeSeriesForTest(t, "scene_manager_node_load_score")["1/968"]
	require.True(t, exported)

	metrics.ResetLeaderGauges()

	for _, name := range []string{"scene_manager_node_load_score", "scene_manager_node_player_count", "scene_manager_node_scene_count"} {
		_, exported = nodeGaugeSeriesForTest(t, name)["1/968"]
		assert.False(t, exported, "%s 降级后必须清掉", name)
	}
}
