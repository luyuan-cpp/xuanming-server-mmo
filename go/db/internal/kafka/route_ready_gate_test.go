package kafka

// D1 第 3 批 go/db 读方门禁的单测(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.1、§5.3、§13.2 第 1–4 条)。
//
// 读脚本本身调 INFO,miniredis 不支持 INFO 的 server / replication 段,所以选库 / 复核 / 就绪闸一律注入
// fakeRouteReadyReader:映射键的值照样从 miniredis 用 MGET 读(选库判定与今天同一份数据),就绪标记与实例身份由用例设定。
// 脚本在真 Redis 上的执行语义见 route_ready_integration_test.go(integration tag)。
//
// 不依赖墙钟:就绪闸的刷新由用例直接调 read + apply 或逐拍喂 ticks;汇总日志用注入的时钟。require.Eventually 只用来等
// 另一个 goroutine 走到「停在闸前」(gate.waits),不参与判定。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	db_config "db/internal/config"
	"db/internal/metrics"
	db_proto "proto/db"
	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
)

// routeReadyTestIdentity 是假实例的身份(run_id / master_replid 各 40 个小写十六进制字符)。
var routeReadyTestIdentity = placement.RedisInstanceIdentity{
	RunID:        strings.Repeat("a", 40),
	Role:         "master",
	MasterReplID: strings.Repeat("b", 40),
}

// routeReadyTestMarker 编码一个合法的就绪标记,身份取参数(集成测试也用它)。
func routeReadyTestMarker(t *testing.T, runID, replID string) string {
	t.Helper()
	raw, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{
		Generation:   "0123abcd",
		RedisRunID:   runID,
		MasterReplID: replID,
		SetAtMs:      1,
		WatermarkMs:  1,
		Leader:       "leader-1",
	})
	require.NoError(t, err)
	return raw
}

// fakeRouteReadyReader 代替读脚本:映射键从 rc 用 MGET 读,标记与身份由用例设定;err 非 nil 时模拟脚本执行出错。
type fakeRouteReadyReader struct {
	rc redis.Cmdable

	mu       sync.Mutex
	marker   string // "" = 标记不在
	identity placement.RedisInstanceIdentity
	err      error
	calls    [][]string
}

func newFakeRouteReadyReader(t *testing.T, rc redis.Cmdable) *fakeRouteReadyReader {
	t.Helper()
	f := &fakeRouteReadyReader{rc: rc}
	f.setTrusted(t)
	return f
}

func (f *fakeRouteReadyReader) set(marker string, identity placement.RedisInstanceIdentity, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marker, f.identity, f.err = marker, identity, err
}

func (f *fakeRouteReadyReader) setTrusted(t *testing.T) {
	t.Helper()
	f.set(routeReadyTestMarker(t, routeReadyTestIdentity.RunID, routeReadyTestIdentity.MasterReplID), routeReadyTestIdentity, nil)
}

func (f *fakeRouteReadyReader) setAbsent() { f.set("", routeReadyTestIdentity, nil) }

func (f *fakeRouteReadyReader) callKeys() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

func (f *fakeRouteReadyReader) readRouteReady(ctx context.Context, mappingKeys ...string) (placement.RouteReadyRead, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), mappingKeys...))
	marker, identity, err := f.marker, f.identity, f.err
	f.mu.Unlock()
	if err != nil {
		return placement.RouteReadyRead{}, err
	}
	r := placement.RouteReadyRead{MarkerPresent: marker != "", MarkerRaw: marker, Identity: identity, Values: []any{}}
	if len(mappingKeys) > 0 {
		vals, err := f.rc.MGet(ctx, mappingKeys...).Result()
		if err != nil {
			return placement.RouteReadyRead{}, err
		}
		r.Values = vals
	}
	return r, nil
}

// routeReadyUntrustedCase 是一种「副本不可信」:标记原值 + 实例身份 → 期望的 verdict 名。
type routeReadyUntrustedCase struct {
	name     string
	marker   string
	identity placement.RedisInstanceIdentity
	verdict  string
}

// routeReadyUntrustedCases 覆盖 VerifyRouteReady 除 Trusted 以外的全部结论(重启 / 切主各一格身份不符)。
func routeReadyUntrustedCases(t *testing.T) []routeReadyUntrustedCase {
	t.Helper()
	id := routeReadyTestIdentity
	trusted := routeReadyTestMarker(t, id.RunID, id.MasterReplID)
	other := strings.Repeat("c", 40)
	replica := id
	replica.Role = "slave"
	noRunID := id
	noRunID.RunID = ""
	return []routeReadyUntrustedCase{
		{name: "absent", marker: "", identity: id, verdict: "absent"},
		{name: "malformed", marker: "garbage", identity: id, verdict: "malformed"},
		{name: "newer_version", marker: `{"v":2}`, identity: id, verdict: "newer_version"},
		{name: "identity_unavailable", marker: trusted, identity: noRunID, verdict: "identity_unavailable"},
		{name: "not_master", marker: trusted, identity: replica, verdict: "not_master"},
		{name: "identity_mismatch_after_restart", marker: routeReadyTestMarker(t, other, id.MasterReplID), identity: id,
			verdict: "identity_mismatch"},
		{name: "identity_mismatch_after_failover", marker: routeReadyTestMarker(t, id.RunID, other), identity: id,
			verdict: "identity_mismatch"},
	}
}

// routeReadyCommandRecorder 记下一个 go-redis 客户端发出的每条命令的参数(含 pipeline 里的)。
type routeReadyCommandRecorder struct {
	mu   sync.Mutex
	cmds [][]any
}

func (r *routeReadyCommandRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *routeReadyCommandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.record(cmd)
		return next(ctx, cmd)
	}
}

func (r *routeReadyCommandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			r.record(cmd)
		}
		return next(ctx, cmds)
	}
}

func (r *routeReadyCommandRecorder) record(cmd redis.Cmder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, append([]any(nil), cmd.Args()...))
}

func (r *routeReadyCommandRecorder) snapshot() [][]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]any(nil), r.cmds...)
}

// isRouteReadyScriptCall 报告一条命令是不是读脚本(EVAL 原文或 EVALSHA 摘要)。
func isRouteReadyScriptCall(args []any) bool {
	if len(args) < 2 {
		return false
	}
	name, _ := args[0].(string)
	if name != "eval" && name != "evalsha" {
		return false
	}
	return args[1] == placement.RouteReadyReadScript || args[1] == routeReadyReadScript.Hash()
}

// refreshGate 用当前读方的结论刷新一次闸(代替 run 的一拍)。
func refreshGate(g *routeReadyGate) {
	g.apply(g.read(context.Background(), time.Second))
}

// ---------------------------------------------------------------------------
// 开关关着:与引入之前逐字节相同
// ---------------------------------------------------------------------------

// Placement.RequireRouteReady=false(缺省):选库与复核仍各是一条两键 MGET,一次读脚本都不发。
func TestRouteReadyOff_LookupAndRecheckStayPlainMGET(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	require.Nil(t, w.placement.ready, "the default router carries no route ready reader")
	client, ok := w.redisClient.(*redis.Client)
	require.True(t, ok)
	rec := &routeReadyCommandRecorder{}
	client.AddHook(rec)

	const key uint64 = 9581
	setRedisValue(t, mr, placement.Key(key), "102:3")
	w.handleTask(makeWriteTask(t, key, placementTestMsgType, 10), true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1)
	assert.Equal(t, "zone_102_db", calls[0].dbName)
	mgets := 0
	for _, args := range rec.snapshot() {
		if isRouteReadyScriptCall(args) {
			t.Fatalf("Placement.RequireRouteReady=false must never run the route ready read script: %v", args)
		}
		if name, _ := args[0].(string); name == "mget" && len(args) == 3 &&
			args[1] == placement.Key(key) && args[2] == placement.HomeZoneKey(key) {
			mgets++
		}
	}
	assert.Equal(t, 2, mgets, "lookup and recheck are each one MGET of the two mapping keys, as before")
}

// ---------------------------------------------------------------------------
// 逐条任务:Trusted 照今天判定,其余延后
// ---------------------------------------------------------------------------

// Trusted:读到的两把键交给原来的判定,结果与今天相同;选库与复核各读一次脚本,KEYS 是 MGET 的顺序。
func TestRouteReady_TrustedRoutesOnTheReadValues(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	reader := newFakeRouteReadyReader(t, w.redisClient)
	w.placement.ready = reader
	const key uint64 = 9591
	setRedisValue(t, mr, placement.Key(key), "102:3")
	setRedisValue(t, mr, placement.HomeZoneKey(key), "1")
	trustedBefore := metricCount(t, "db_route_ready_verdict_total", map[string]string{"verdict": "trusted"})

	wt := makeWriteTask(t, key, placementTestMsgType, 10)
	session := attachKafkaClaim(t, w, wt)
	w.handleTask(wt, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1)
	assert.Equal(t, "zone_102_db", calls[0].dbName)
	cursor, ok := appliedCursorOf(t, w, key)
	require.True(t, ok)
	assert.Equal(t, "v2:0:10", cursor)
	assert.True(t, cacheExists(t, w, key))
	assert.Equal(t, []int64{9}, session.marked())
	want := []string{placement.Key(key), placement.HomeZoneKey(key)}
	assert.Equal(t, [][]string{want, want}, reader.callKeys(), "lookup and recheck each read the two mapping keys in MGET order")
	assert.Equal(t, trustedBefore+2, metricCount(t, "db_route_ready_verdict_total", map[string]string{"verdict": "trusted"}))

	// 访客读:与开关关着时同一格(读 home 库)。
	const visitor uint64 = 9592
	setRedisValue(t, mr, placement.HomeZoneKey(visitor), "2")
	rt := makeReadTask(t, visitor, placementTestMsgType, "preload")
	rt.seq = 5
	w.handleTask(rt, true)
	readCalls := h.callsForKey(visitor)
	require.Len(t, readCalls, 1)
	assert.Equal(t, "zone_2_db", readCalls[0].dbName)
	result, ok := readTaskResult(t, w, rt.dbTask.TaskId)
	require.True(t, ok)
	assert.True(t, result.Success)
}

// 写:副本不可信时一条 SQL 都不发(缺键绝不当「没有落点」按 legacy 落本 zone 库)、延后、不耗预算,永远不进死信;
// 副本恢复可信后同一条写照常落库。
func TestRouteReady_UntrustedWriteDefersWithoutBudget(t *testing.T) {
	for _, tc := range routeReadyUntrustedCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			w, _, h := newTestWorker(t, 0)
			reader := newFakeRouteReadyReader(t, w.redisClient)
			reader.set(tc.marker, tc.identity, nil)
			w.placement.ready = reader
			// 不写任何映射键:开关关着时这条写会按 legacy 落 zone_1_db。
			const key uint64 = 9601
			task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
			task.RetryCount = 2 // 再耗一次就会在认领时进死信
			require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

			guard := map[string]string{"op": "write", "outcome": metrics.PlacementRouteNotReady}
			deferred := map[string]string{"op": "write", "reason": metrics.DeferRouteNotReady}
			verdict := map[string]string{"verdict": tc.verdict}
			guardBefore, deferredBefore := metricCount(t, "db_placement_guard_total", guard), metricCount(t, "db_task_deferred_total", deferred)
			verdictBefore := metricCount(t, "db_route_ready_verdict_total", verdict)

			const rounds = 4
			for round := 0; round < rounds; round++ {
				claimed := claimRetry(t, w)
				require.NotNil(t, claimed, "round %d: an untrusted replica must never dead-letter a write", round)
				w.handleTask(claimed, true)
				assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "round %d", round)
				assert.Zero(t, redisListLen(t, w, w.retryProcessingKey), "round %d", round)
				assert.Zero(t, redisListLen(t, w, w.retryDeadQueueKey), "round %d", round)
				assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "round %d: an untrusted replica is not a failed attempt", round)
			}
			assert.Empty(t, h.callsForKey(key), "nothing may reach MySQL while the replica is untrusted")
			_, hasCursor := appliedCursorOf(t, w, key)
			assert.False(t, hasCursor)
			assert.Equal(t, guardBefore+rounds, metricCount(t, "db_placement_guard_total", guard))
			assert.Equal(t, deferredBefore+rounds, metricCount(t, "db_task_deferred_total", deferred))
			assert.Equal(t, verdictBefore+rounds, metricCount(t, "db_route_ready_verdict_total", verdict))

			reader.setTrusted(t)
			claimed := claimRetry(t, w)
			require.NotNil(t, claimed)
			w.handleTask(claimed, true)
			calls := h.callsForKey(key)
			require.Len(t, calls, 1)
			assert.Equal(t, "zone_1_db", calls[0].dbName)
			cursor, ok := appliedCursorOf(t, w, key)
			require.True(t, ok)
			assert.Equal(t, "v2:0:10", cursor)
			assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
		})
	}
}

// 读:副本不可信照常耗预算;用完按既有语义静默丢弃(不进死信、不回失败回执,§13.2 第 4 条),也不碰 MySQL。
func TestRouteReady_UntrustedReadConsumesBudget(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	reader := newFakeRouteReadyReader(t, w.redisClient)
	reader.setAbsent()
	w.placement.ready = reader
	const key uint64 = 9611
	setRedisValue(t, mr, placement.HomeZoneKey(key), "2")
	rt := makeReadTask(t, key, placementTestMsgType, "preload")
	rt.dbTask.RetryCount = 2
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, rt.dbTask, 5, 0))
	abandonedBefore := metricCount(t, "db_task_read_abandoned_total", map[string]string{"reason": metrics.ReadAbandonedBudgetExhausted})

	claimed := claimRetry(t, w)
	require.NotNil(t, claimed)
	w.handleTask(claimed, true)
	assert.Equal(t, int32(3), oldestReadyTask(t, w).RetryCount, "an untrusted replica consumes one read attempt")

	assert.Nil(t, claimRetry(t, w), "the exhausted read is dropped at claim time")
	assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
	assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
	assert.Zero(t, redisListLen(t, w, w.retryDeadQueueKey), "reads are never dead-lettered")
	assert.Equal(t, abandonedBefore+1, metricCount(t, "db_task_read_abandoned_total",
		map[string]string{"reason": metrics.ReadAbandonedBudgetExhausted}))
	_, answered := readTaskResult(t, w, rt.dbTask.TaskId)
	assert.False(t, answered, "budget exhaustion keeps the existing silent drop")
	assert.Empty(t, h.callsForKey(key))
}

// 读脚本执行出错与今天的 MGET 失败同口径:lookup_error,耗预算(fail-closed),不碰 MySQL。
func TestRouteReady_ScriptErrorIsALookupError(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	reader := newFakeRouteReadyReader(t, w.redisClient)
	reader.set("", routeReadyTestIdentity, errors.New("ERR Error running script: INFO is not allowed"))
	w.placement.ready = reader
	const key uint64 = 9621
	task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
	task.RetryCount = 1
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))
	lookupErr := map[string]string{"op": "write", "outcome": metrics.PlacementLookupError}
	before := metricCount(t, "db_placement_guard_total", lookupErr)

	claimed := claimRetry(t, w)
	require.NotNil(t, claimed)
	w.handleTask(claimed, true)
	assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "a failed read script consumes one attempt, like a failed MGET")
	assert.Equal(t, before+1, metricCount(t, "db_placement_guard_total", lookupErr))
	assert.Empty(t, h.callsForKey(key))
}

// 复核时副本变得不可信:按「路由变了」作废重试 —— 不标记游标、不写共享缓存、不发读结果,读写都不耗预算;
// 延后原因与 outcome 记 route_not_ready(不记 recheck_moved)。恢复可信后照常落库。用重试来源的任务(RetryCount=2),
// 耗了预算就会变成 3。
func TestRouteReady_RecheckUntrustedRetriesLikeMoved(t *testing.T) {
	const key uint64 = 9631

	t.Run("write", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		reader := newFakeRouteReadyReader(t, w.redisClient)
		w.placement.ready = reader
		setRedisValue(t, mr, placement.Key(key), "102:3")
		h.setAfterCall(func(recordedCall) { reader.setAbsent() })
		notReady := map[string]string{"op": "write", "outcome": metrics.PlacementRouteNotReady}
		moved := map[string]string{"op": "write", "outcome": metrics.PlacementRecheckMoved}
		deferred := map[string]string{"op": "write", "reason": metrics.DeferRouteNotReady}
		notReadyBefore, movedBefore := metricCount(t, "db_placement_guard_total", notReady), metricCount(t, "db_placement_guard_total", moved)
		deferredBefore := metricCount(t, "db_task_deferred_total", deferred)

		task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
		task.RetryCount = 2
		require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)

		require.Len(t, h.callsForKey(key), 1, "the SQL did run on the selected store")
		_, hasCursor := appliedCursorOf(t, w, key)
		assert.False(t, hasCursor, "a write whose route cannot be rechecked must not publish its cursor")
		assert.False(t, cacheExists(t, w, key), "nor the shared cache")
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
		assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "the recheck retry does not consume the budget")
		assert.Equal(t, notReadyBefore+1, metricCount(t, "db_placement_guard_total", notReady))
		assert.Equal(t, movedBefore, metricCount(t, "db_placement_guard_total", moved))
		assert.Equal(t, deferredBefore+1, metricCount(t, "db_task_deferred_total", deferred))

		h.setAfterCall(nil)
		reader.setTrusted(t)
		claimed = claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)
		calls := h.callsForKey(key)
		require.Len(t, calls, 2)
		assert.Equal(t, "zone_102_db", calls[1].dbName)
		cursor, ok := appliedCursorOf(t, w, key)
		require.True(t, ok)
		assert.Equal(t, "v2:0:10", cursor)
		assert.Zero(t, redisListLen(t, w, w.retryQueueKey))
		assert.Zero(t, redisListLen(t, w, w.retryProcessingKey))
	})

	t.Run("read", func(t *testing.T) {
		w, mr, h := newTestWorker(t, 0)
		reader := newFakeRouteReadyReader(t, w.redisClient)
		w.placement.ready = reader
		setRedisValue(t, mr, placement.Key(key), "102:3")
		h.setAfterCall(func(recordedCall) { reader.setAbsent() })
		rt := makeReadTask(t, key, placementTestMsgType, "preload")
		rt.dbTask.RetryCount = 2
		require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, rt.dbTask, 5, 0))
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed)
		w.handleTask(claimed, true)

		require.Len(t, h.callsForKey(key), 1, "the read did run on the selected store")
		_, ok := readTaskResult(t, w, rt.dbTask.TaskId)
		assert.False(t, ok, "a result whose route cannot be rechecked must never be published")
		assert.False(t, cacheExists(t, w, key))
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
		assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "the recheck retry does not consume the read budget either")
	})
}

// 回归(与冻结同口径):重试来源的写在副本不可信期间每轮都被重排,不打 ERROR。
func TestRouteReady_RetryRescheduleLogsDebugNotError(t *testing.T) {
	logs := logtest.NewCollector(t)
	w, _, _ := newTestWorker(t, 0)
	reader := newFakeRouteReadyReader(t, w.redisClient)
	reader.setAbsent()
	w.placement.ready = reader
	task := makeWriteTask(t, 9641, placementTestMsgType, 10).dbTask
	require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

	for round := 0; round < 3; round++ {
		claimed := claimRetry(t, w)
		require.NotNil(t, claimed, "round %d", round)
		w.handleTask(claimed, true)
		require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "round %d: still deferred", round)
	}
	assert.Zero(t, countLogLines(t, logs, "error", "retry task rescheduled"),
		"an untrusted replica rescheduled every round must not flood ERROR")
	assert.Zero(t, countLogLines(t, logs, "error", "worker process task failed"))
}

// 回归(B3G-4):复核时副本不可信(errRouteNotReady 包在 errPlacementMoved 里)不逐条打 ERROR「worker process task failed」
// —— Redis 重启 / 切主 / 标记被撤期间已派进 worker 的每条任务都会走到这里,汇总由 confirm 里的 noteRouteNotReady 负责。
// 对照组:副本可信、落点在 SQL 之后真的变了(recheck_moved),同一条路径照旧逐条打 ERROR,也证明日志收集本身是通的。
// 红证据:handleTask 的条件去掉 errRouteNotReady 那一项,untrusted_at_recheck 数到 1。
func TestRouteReady_RecheckUntrustedLogsNoError(t *testing.T) {
	const key uint64 = 9661
	cases := []struct {
		name      string
		afterSQL  func(t *testing.T, mr *miniredis.Miniredis, reader *fakeRouteReadyReader)
		wantError int
	}{
		{
			name:      "untrusted_at_recheck",
			afterSQL:  func(_ *testing.T, _ *miniredis.Miniredis, reader *fakeRouteReadyReader) { reader.setAbsent() },
			wantError: 0,
		},
		{
			name: "moved_control",
			afterSQL: func(t *testing.T, mr *miniredis.Miniredis, _ *fakeRouteReadyReader) {
				setRedisValue(t, mr, placement.Key(key), "1000000:4")
			},
			wantError: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)
			w, mr, h := newTestWorker(t, 0)
			reader := newFakeRouteReadyReader(t, w.redisClient)
			w.placement.ready = reader
			setRedisValue(t, mr, placement.Key(key), "102:3")
			h.setAfterCall(func(recordedCall) { tc.afterSQL(t, mr, reader) })
			task := makeWriteTask(t, key, placementTestMsgType, 10).dbTask
			task.RetryCount = 2
			require.NoError(t, saveToRetryQueue(w.ctx, w.redisClient, w.retryQueueKey, task, 10, 0))

			claimed := claimRetry(t, w)
			require.NotNil(t, claimed)
			w.handleTask(claimed, true)

			require.Len(t, h.callsForKey(key), 1, "the SQL did run; what fails is the post-write recheck")
			require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "both are retried")
			assert.Equal(t, int32(2), oldestReadyTask(t, w).RetryCount, "neither consumes the budget")
			assert.Equal(t, tc.wantError, countLogLines(t, logs, "error", "worker process task failed"))
		})
	}
}

// 延后原因与重试预算:选库时只包 errRouteNotReady(写豁免、读不豁免);复核时同时包着 errPlacementMoved(读写都豁免),
// 原因都记 route_not_ready;失租优先(既有优先级不变)。
func TestRouteNotReady_DeferReasonAndRetryBudget(t *testing.T) {
	atLookup := fmt.Errorf("placement lookup: key=1 msgType=x: %w", routeNotReadyError{verdict: placement.RouteReadyAbsent})
	atRecheck := fmt.Errorf("%w: %w: key=1", errPlacementMoved, routeNotReadyError{verdict: placement.RouteReadyIdentityMismatch})
	require.ErrorIs(t, atLookup, errRouteNotReady)
	require.ErrorIs(t, atRecheck, errRouteNotReady)
	require.ErrorIs(t, atRecheck, errPlacementMoved)
	assert.Equal(t, "absent", routeNotReadyVerdict(atLookup))
	assert.Equal(t, "identity_mismatch", routeNotReadyVerdict(atRecheck))
	assert.Equal(t, "unknown", routeNotReadyVerdict(errors.New("other")))
	assert.Contains(t, atLookup.Error(), "verdict=absent")

	assert.Equal(t, metrics.DeferRouteNotReady, deferReason(atLookup))
	assert.Equal(t, metrics.DeferRouteNotReady, deferReason(atRecheck), "the recheck error names the untrusted replica, not a move")
	assert.Equal(t, metrics.DeferLeaseLost, deferReason(fmt.Errorf("%w: %w", errOrderingLeaseLost, atLookup)))

	write, read := &db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &db_proto.DBTask{Op: db_proto.DBOp_DB_OP_READ}
	assert.True(t, retryBudgetExempt(write, atLookup), "an untrusted replica is not a failed write")
	assert.False(t, retryBudgetExempt(read, atLookup), "reads consume their budget")
	assert.True(t, retryBudgetExempt(write, atRecheck))
	assert.True(t, retryBudgetExempt(read, atRecheck), "a recheck retry is like a moved route for reads too")
}

// 不可信延后的 INFO 汇总:第一次立即打一条,之后至多每个周期一条,周期内的累计按「op/出处/verdict」排序汇总;
// 没有新延后时 flush 不打。
func TestRouteNotReadySummary_AtMostOneLinePerInterval(t *testing.T) {
	clock := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	var lines []string
	s := newRouteNotReadySummary(func() time.Time { return clock }, time.Minute, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	s.note("write/lookup/absent")
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "first occurrence: write/lookup/absent=1")

	clock = clock.Add(10 * time.Second)
	s.note("write/lookup/absent")
	s.note("read/lookup/identity_mismatch")
	s.note("write/lookup/absent")
	clock = clock.Add(20 * time.Second)
	s.flush()
	require.Len(t, lines, 1, "nothing more within the interval")

	clock = clock.Add(30 * time.Second)
	s.flush()
	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], "last 1m0s: read/lookup/identity_mismatch=1 write/lookup/absent=2")

	clock = clock.Add(5 * time.Minute)
	s.flush()
	require.Len(t, lines, 2, "a flush without new deferrals prints nothing")
	s.note("write/recheck/absent")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[2], "last 5m0s: write/recheck/absent=1")
}

// ---------------------------------------------------------------------------
// 进程级就绪闸
// ---------------------------------------------------------------------------

// 闸的状态跟着每一次刷新走:初始关着;Trusted 放行;不可信或读脚本出错关闸(fail-closed,出错计 verdict=script_error);
// 每次刷新都唤醒等待者;仪表与 verdict 计数同步更新。nil 闸恒放行。
func TestRouteReadyGate_StateFollowsEachRefresh(t *testing.T) {
	reader := newFakeRouteReadyReader(t, nil)
	g := newRouteReadyGate(reader, time.Second)
	assert.False(t, g.opened(), "the gate starts closed until the startup probe")
	gateValue := func() float64 { return metricCount(t, "db_route_ready_gate", map[string]string{}) }
	verdicts := func() map[string]float64 {
		out := map[string]float64{}
		for _, v := range placement.RouteReadyVerdictNames() {
			out[v] = metricCount(t, "db_route_ready_verdict_total", map[string]string{"verdict": v})
		}
		return out
	}
	woken := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	wake := g.nextRefresh()
	require.False(t, woken(wake))
	require.NoError(t, g.probe(context.Background()))
	assert.True(t, g.opened())
	assert.True(t, woken(wake), "every refresh wakes the waiters")
	assert.Equal(t, float64(1), gateValue())

	reader.setAbsent()
	before := verdicts()
	wake = g.nextRefresh()
	refreshGate(g)
	assert.False(t, g.opened())
	assert.True(t, woken(wake))
	assert.Equal(t, float64(0), gateValue())
	assert.Equal(t, before["absent"]+1, verdicts()["absent"])

	// 标记本身可信、读却出错:出错必须关闸,不能沿用任何结论。先放行一次,证明是这次出错把闸关上的。
	reader.setTrusted(t)
	refreshGate(g)
	require.True(t, g.opened())
	reader.set(routeReadyTestMarker(t, routeReadyTestIdentity.RunID, routeReadyTestIdentity.MasterReplID),
		routeReadyTestIdentity, errors.New("dial tcp: connection refused"))
	before = verdicts()
	scriptErrBefore := metricCount(t, "db_route_ready_verdict_total", map[string]string{"verdict": routeReadyGateStateScriptError})
	wake = g.nextRefresh()
	refreshGate(g)
	assert.False(t, g.opened(), "a failed read keeps the gate closed")
	assert.True(t, woken(wake))
	assert.Equal(t, before, verdicts(), "a failed read is none of placement's verdicts")
	assert.Equal(t, scriptErrBefore+1, metricCount(t, "db_route_ready_verdict_total", map[string]string{"verdict": routeReadyGateStateScriptError}),
		"a failed read is counted as script_error, so it is told apart from an absent marker")

	reader.setTrusted(t)
	refreshGate(g)
	assert.True(t, g.opened())
	assert.Equal(t, float64(1), gateValue())

	var none *routeReadyGate
	assert.True(t, none.opened())
	assert.Nil(t, none.nextRefresh())
	assert.False(t, (&KeyOrderedKafkaConsumer{}).retryClaimsPaused())
}

// 回归(B3G-5):闸的翻转日志按 routeReadyGateLogEvery 限速。读脚本时好时坏时闸每次刷新都翻一次,不能每拍一条 ERROR + INFO:
// 启动后第一次翻转立即打;距上一条翻转日志不满一个周期的翻转只累加,满周期后的下一次刷新按当时状态打一条、带 flips=N;
// 翻转停下之后最后一个状态至多晚一个周期打出来;没有翻转时「仍关着」提醒照旧每周期一条。状态与唤醒每次刷新照常更新。
// 时钟注入(g.now),不依赖墙钟。红证据:apply 去掉翻转限速那一格,闪断期间的 ERROR 数到 11。
func TestRouteReadyGate_FlipLogsAtMostOncePerInterval(t *testing.T) {
	logs := logtest.NewCollector(t)
	base := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	reader := newFakeRouteReadyReader(t, nil)
	g := newRouteReadyGate(reader, time.Second)
	var clock time.Time
	g.now = func() time.Time { return clock }
	// at 在 base+sec 秒刷新一次:trusted 放行,否则读脚本出错(闸关、state=script_error)。
	at := func(sec int, trusted bool) {
		t.Helper()
		clock = base.Add(time.Duration(sec) * time.Second)
		if trusted {
			reader.setTrusted(t)
		} else {
			reader.set("", routeReadyTestIdentity, errors.New("i/o timeout"))
		}
		wake := g.nextRefresh()
		refreshGate(g)
		require.Equal(t, trusted, g.opened(), "t=%ds: the gate state follows every refresh regardless of the log limit", sec)
		require.True(t, closedWithin(wake, time.Second), "t=%ds: every refresh wakes the waiters", sec)
	}
	closedLines := func() int { return countLogLines(t, logs, "error", "route ready gate closed") }
	reopenLines := func() int { return countLogLines(t, logs, "info", "route ready gate reopened") }
	stillLines := func() int { return countLogLines(t, logs, "info", "route ready gate still closed") }

	at(0, true)
	require.Equal(t, 1, countLogLines(t, logs, "info", "route ready gate open"))
	at(1, false)
	require.Equal(t, 1, closedLines(), "the first flip after startup is logged at once")
	assert.Equal(t, 1, countLogLines(t, logs, "error", "deferred as route_not_ready"),
		"the closed line says where the tasks already in the workers go")

	// 闪断:t=2..21 每秒翻一次(开、关交替),共 20 次,都在上一条翻转日志(t=1)之后一个周期之内。
	for sec := 2; sec <= 21; sec++ {
		at(sec, sec%2 == 0)
	}
	assert.Equal(t, 1, closedLines(), "flips within one interval are counted, not logged")
	assert.Zero(t, reopenLines())

	at(60, false)
	assert.Equal(t, 1, closedLines(), "59s after the last flip line: still within the interval")
	assert.Zero(t, stillLines(), "pending flips are reported by the flip line, not by the still-closed reminder")
	at(61, false)
	require.Equal(t, 2, closedLines(), "one interval after the last flip line the current state is logged")
	assert.Equal(t, 1, countLogLines(t, logs, "error", "flips=20"), "the line carries the flips counted meanwhile")

	// 恢复:t=62 重开被限速,翻转停下之后至多晚一个周期打出来,带上最后一次关了多久(t=21 关到 t=62)。
	at(62, true)
	assert.Zero(t, reopenLines())
	at(121, true)
	require.Equal(t, 1, reopenLines())
	assert.Equal(t, 1, countLogLines(t, logs, "info", "reopened after 41s closed"))
	assert.Equal(t, 1, countLogLines(t, logs, "info", "(flips=1)"))

	// 没有翻转时照旧:开着不打;关上(距上一条翻转日志满周期)立即打;一直关着每周期一条「仍关着」。
	at(300, true)
	at(400, false)
	require.Equal(t, 3, closedLines(), "a flip after a quiet interval is logged at once")
	at(459, false)
	assert.Zero(t, stillLines())
	at(460, false)
	assert.Equal(t, 1, stillLines())
	assert.Equal(t, 3, closedLines())
	assert.Equal(t, 1, reopenLines())
}

// 启动探针:不可信不是错误(闸关着,等标记);读脚本执行出错即拒启 —— Start 第一步就返回,什么都没起。
func TestRouteReadyGate_ProbeErrorRefusesToStart(t *testing.T) {
	reader := newFakeRouteReadyReader(t, nil)
	reader.setAbsent()
	g := newRouteReadyGate(reader, time.Second)
	require.NoError(t, g.probe(context.Background()), "an untrusted replica is not a startup error")
	assert.False(t, g.opened())

	failing := newFakeRouteReadyReader(t, nil)
	failing.set("", routeReadyTestIdentity, errors.New("ERR This Redis command is not allowed from script"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &KeyOrderedKafkaConsumer{ctx: ctx, routeGate: newRouteReadyGate(failing, time.Second)}
	err := c.Start()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "route ready")
	assert.False(t, c.isStarted(), "a failed probe refuses to start before any worker or consume loop")
	assert.False(t, c.routeGate.opened())
}

// run 每收到一个 tick 刷新一次;ctx 取消即返回。tick 由用例逐拍喂,用 nextRefresh 通道同步「这一拍已刷完」。
func TestRouteReadyGate_RunRefreshesOnEachTick(t *testing.T) {
	reader := newFakeRouteReadyReader(t, nil)
	g := newRouteReadyGate(reader, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.run(ctx, ticks)
	}()
	beat := func() {
		wake := g.nextRefresh()
		ticks <- time.Time{}
		require.True(t, closedWithin(wake, 5*time.Second), "the refresh after a tick must complete")
	}

	reader.setAbsent()
	beat()
	assert.False(t, g.opened())
	reader.setTrusted(t)
	beat()
	assert.True(t, g.opened())
	reader.setAbsent()
	beat()
	assert.False(t, g.opened(), "a replica that becomes untrusted closes the gate on the next refresh")

	cancel()
	require.True(t, closedWithin(done, 5*time.Second), "run must return once its context is canceled")
}

// 装配:开关关着什么都不建;打开时读与闸共用落点 Redis 客户端,间隔取配置(0 取缺省),越界或缺客户端拒绝。
func TestNewRouteReadyGating(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	reader, gate, err := newRouteReadyGating(db_config.PlacementConfig{}, rc)
	require.NoError(t, err)
	assert.Nil(t, reader, "switch off: lookup stays MGET")
	assert.Nil(t, gate, "switch off: consumption is never paused")

	for _, tc := range []struct {
		intervalMs int
		want       time.Duration
	}{
		{intervalMs: 0, want: time.Second},
		{intervalMs: 200, want: 200 * time.Millisecond},
		{intervalMs: 10000, want: 10 * time.Second},
	} {
		reader, gate, err := newRouteReadyGating(db_config.PlacementConfig{RequireRouteReady: true, RouteReadyGateIntervalMs: tc.intervalMs}, rc)
		require.NoError(t, err, "interval %d", tc.intervalMs)
		require.NotNil(t, gate)
		assert.Equal(t, tc.want, gate.interval)
		_, isRedis := reader.(redisRouteReadyReader)
		assert.True(t, isRedis, "production reads run the script on the placement Redis")
		assert.False(t, gate.opened(), "the gate starts closed until the startup probe")
	}
	for _, intervalMs := range []int{199, 10001, -1} {
		_, _, err := newRouteReadyGating(db_config.PlacementConfig{RequireRouteReady: true, RouteReadyGateIntervalMs: intervalMs}, rc)
		assert.Error(t, err, "interval %d", intervalMs)
	}
	_, _, err = newRouteReadyGating(db_config.PlacementConfig{RequireRouteReady: true}, nil)
	assert.Error(t, err)
}

// 生产读方在 miniredis 上:KEYS 是「标记在前、映射键按调用方顺序」,先 EVALSHA、NOSCRIPT 后回退 EVAL;
// miniredis 拒绝脚本里的 INFO server,读必须报错(绝不当成「空副本」放行),启动探针据此拒启。
func TestRedisRouteReadyReader_KeysAndFailClosedOnMiniredis(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rec := &routeReadyCommandRecorder{}
	rc.AddHook(rec)
	reader := redisRouteReadyReader{rc: rc}

	_, err := reader.readRouteReady(context.Background(), "player:placement:7", "player:zone:7")
	require.Error(t, err, "a script that cannot read the instance identity must fail the read")

	var scripts [][]any
	for _, args := range rec.snapshot() {
		if isRouteReadyScriptCall(args) {
			scripts = append(scripts, args)
		}
	}
	require.Len(t, scripts, 2, "EVALSHA first, then EVAL after NOSCRIPT")
	assert.Equal(t, "evalsha", scripts[0][0])
	assert.Equal(t, "eval", scripts[1][0])
	for _, args := range scripts {
		require.Len(t, args, 6)
		assert.EqualValues(t, 3, args[2], "numkeys")
		assert.Equal(t, []any{placement.RouteReadyKey, "player:placement:7", "player:zone:7"}, args[3:])
	}

	g := newRouteReadyGate(reader, time.Second)
	require.Error(t, g.probe(context.Background()), "the startup probe refuses a Redis where the script cannot run")
	assert.False(t, g.opened())
}

// ConsumeClaim:闸关着时不从 claim 取消息 —— 不执行、不标记位点、不往重试队列推,消息留在 Kafka;再刷新一次仍不可信
// 就醒来复查、再停下;放行后照常消费。
func TestConsumeClaim_RouteReadyGateClosedTakesNoMessages(t *testing.T) {
	f := newShutdownFixture(t)
	reader := newFakeRouteReadyReader(t, f.w.redisClient)
	reader.setAbsent()
	f.w.placement.ready = reader
	gate := newRouteReadyGate(reader, time.Second)
	require.NoError(t, gate.probe(context.Background()))
	require.False(t, gate.opened())
	f.c.routeGate = gate
	task := f.addWrite(t, 0, false)
	done := f.startClaim()

	require.Eventually(t, func() bool { return gate.waits.Load() >= 1 }, 5*time.Second, time.Millisecond,
		"the claim loop must park on the closed gate")
	nothingTaken := func(stage string) {
		t.Helper()
		assert.Empty(t, f.h.snapshot(), "%s: nothing may run while the gate is closed", stage)
		assert.Equal(t, 1, len(f.claim.messages), "%s: the message stays in Kafka", stage)
		assert.Empty(t, f.session.marked(), "%s: no offset is marked", stage)
		assert.Zero(t, redisListLen(t, f.w, f.w.retryQueueKey), "%s: nothing is pushed into the retry queue", stage)
	}
	nothingTaken("parked")

	waits := gate.waits.Load()
	refreshGate(gate)
	require.Eventually(t, func() bool { return gate.waits.Load() > waits }, 5*time.Second, time.Millisecond,
		"an untrusted refresh wakes the loop, which rechecks and parks again")
	nothingTaken("after an untrusted refresh")

	reader.setTrusted(t)
	refreshGate(gate)
	require.Eventually(t, func() bool { return len(f.session.marked()) == 1 }, 5*time.Second, time.Millisecond,
		"once the gate opens the message is consumed and its offset marked")
	calls := f.h.callsForKey(task.Key)
	require.Len(t, calls, 1)
	assert.Equal(t, "zone_1_db", calls[0].dbName)

	f.c.stopPulling()
	require.NoError(t, waitClaimResult(t, done))
	assert.Equal(t, []int64{0}, f.session.marked())
}

// 停机撤拉取时闸还关着:claim 照常走停机收尾返回,不卡在闸上。
func TestConsumeClaim_RouteReadyGateClosedStillShutsDown(t *testing.T) {
	f := newShutdownFixture(t)
	reader := newFakeRouteReadyReader(t, f.w.redisClient)
	reader.setAbsent()
	gate := newRouteReadyGate(reader, time.Second)
	require.NoError(t, gate.probe(context.Background()))
	f.c.routeGate = gate
	f.addWrite(t, 0, false)
	done := f.startClaim()
	require.Eventually(t, func() bool { return gate.waits.Load() >= 1 }, 5*time.Second, time.Millisecond)

	f.c.stopPulling()
	require.NoError(t, waitClaimResult(t, done))
	assert.Empty(t, f.h.snapshot())
	assert.Empty(t, f.session.marked())
	assert.Equal(t, 1, len(f.claim.messages), "the message is left for the next owner to replay")
}

// 重试循环:闸关着时本拍不认领(收据留在 ready、什么都不派发);放行后照常认领。
func TestConsumeRetryQueue_RouteReadyGateClosedClaimsNothing(t *testing.T) {
	w, mr, _ := newTestWorker(t, 0)
	seedReadyWrites(t, w, 9651, 3)
	counter := &claimCounter{}
	c := newRetryTestConsumer(t, w, mr, "db-a:6000", counter)
	reader := newFakeRouteReadyReader(t, w.redisClient)
	reader.setAbsent()
	c.routeGate = newRouteReadyGate(reader, time.Second)
	require.NoError(t, c.routeGate.probe(context.Background()))

	c.consumeRetryQueue()
	assert.Zero(t, counter.count(), "a closed gate claims nothing")
	assert.Equal(t, int64(3), redisListLen(t, w, w.retryQueueKey), "receipts stay in ready")
	assert.Empty(t, drainDispatched(w))

	reader.setTrusted(t)
	refreshGate(c.routeGate)
	c.consumeRetryQueue()
	assert.Equal(t, 3, counter.count())
	assert.Len(t, drainDispatched(w), 3)
}

// retryClaimHook 在每次认领脚本(retryClaimScript)成功返回之后同步调用 after(在执行认领的 goroutine 上)。
type retryClaimHook struct{ after func() }

func (h retryClaimHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h retryClaimHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h retryClaimHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if args := cmd.Args(); err == nil && cmd.Name() == "eval" && len(args) >= 2 {
			if script, ok := args[1].(string); ok && script == retryClaimScript {
				h.after()
			}
		}
		return err
	}
}

// 回归(B3G-1):闸在本拍中途关上时,两路的认领循环每认领一条之前再看一次闸(opened,不记等待次数),当拍只认领了关闸时
// 正在认领的那一条,剩下的收据留在原列表,而不是把整拍额度用完;下一拍在闸前停下(retryClaimsPaused 记一次等待)。
// 关闸由认领脚本返回之后的钩子触发(第一条认领完、第二条认领之前刷新一次),不依赖墙钟。
// 红证据:删掉 consumeRetryQueueShared / consumeRetryQueueOwner 认领循环里的 opened 判断,同一拍认领出 3 条。
func TestConsumeRetryQueue_RouteReadyGateClosingMidTickStopsClaiming(t *testing.T) {
	for _, mode := range []string{db_config.RetryRoutingShared, db_config.RetryRoutingOwner} {
		t.Run(mode, func(t *testing.T) {
			f := newRoutingFixture(t)
			c := f.consumer("db-a:6000", mode, 1)
			lease := c.owned.acquire(0, f.ctx)
			t.Cleanup(func() { c.owned.release(lease) })
			ready := c.retryQueueKey // shared:从共享收件箱认领
			if mode == db_config.RetryRoutingOwner {
				ready = pKey(0) // owner:从租约所在分区的 p0 认领
			}
			for i := uint64(0); i < 3; i++ {
				payload, _ := routedPayload(t, 9681+i, 10, 0, 0)
				f.push(t, ready, payload)
			}
			reader := newFakeRouteReadyReader(t, nil)
			gate := newRouteReadyGate(reader, time.Second)
			require.NoError(t, gate.probe(context.Background()))
			require.True(t, gate.opened())
			c.routeGate = gate
			claims := 0
			f.rc.AddHook(retryClaimHook{after: func() {
				claims++
				if claims == 1 {
					reader.setAbsent()
					refreshGate(gate)
				}
			}})

			c.consumeRetryQueue()
			assert.Equal(t, 1, claims, "only the claim in progress when the gate closed")
			assert.Len(t, drainDispatched(c.workers[0]), 1)
			assert.Equal(t, int64(2), f.llen(t, ready), "the rest stay in the ready list")

			waits := gate.waits.Load()
			c.consumeRetryQueue()
			assert.Equal(t, 1, claims, "the next tick pauses on the closed gate")
			assert.Equal(t, waits+1, gate.waits.Load())
			assert.Equal(t, int64(2), f.llen(t, ready))
			assert.Empty(t, drainDispatched(c.workers[0]))
		})
	}
}
