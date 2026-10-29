package kafka

// runDBOp 的执行顺序与上报口径(G1 ③ 核锁前移、L-04 §2.3 第 8 条),以及 handleTask 级的回归:
// 失租的写不发布共享缓存。
//
// 替身:fakeLease 实现 writeLease,把调用记进同一份顺序日志,可注入 beginSQL / verify 错误、可交出已取消的 ctx;
// healthStores 在 fakeStores 上加 storeHealth 计数;Redis 命令经 go-redis Hook 记进同一份顺序日志(只记 MGET / SET)。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"db/internal/locker"
	"db/internal/metrics"
	"db/internal/storeerr"
	db_proto "proto/db"
	"shared/placement"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 兼容既有只断言错误的用例；结局专项用例直接调用生产接口。
func runDBOpError(ctx context.Context, rc redis.Cmdable, task *db_proto.DBTask, msg proto.Message, handler dbOpHandler, route *placementRoute, lease writeLease) error {
	_, err := runDBOp(ctx, rc, task, msg, handler, route, lease)
	return err
}
func (l *fakeLease) runTokenScript(ctx context.Context, script *redis.Script, keys []string, args ...interface{}) (int64, error) {
	if err := l.verify(ctx); err != nil {
		return 0, err
	}
	l.log.add("publish")
	if l.token == nil {
		var err error
		l.token, err = locker.NewRedisLocker(l.rc).TryLock(ctx, "fixture", time.Minute)
		if err != nil {
			return 0, err
		}
	}
	return l.token.RunTokenScript(ctx, script, keys, append([]interface{}{int64(60000)}, args...)...)
}

// orderLog 是一份并发安全的事件顺序日志。
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// redisCmdLogger 是只记指定命令名的 go-redis Hook。
type redisCmdLogger struct {
	log   *orderLog
	names map[string]bool
}

func (h redisCmdLogger) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h redisCmdLogger) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		// fakeLease 首次运行真实 Lua 前创建测试令牌；这条 SET NX 是夹具准备，
		// 不是缓存发布。仍记录所有其他 SET，避免漏掉绕开原子发布的回归。
		if cmd.Name() == "set" && len(cmd.Args()) > 1 && cmd.Args()[1] == "distributed:lock:fixture" {
			return next(ctx, cmd)
		}
		if h.names[cmd.Name()] {
			h.log.add(cmd.Name())
		}
		return next(ctx, cmd)
	}
}

func (h redisCmdLogger) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// fakeLease 实现 writeLease。
type fakeLease struct {
	rc        *redis.Client
	token     *locker.TryLockResult
	log       *orderLog
	ctx       context.Context
	beginErr  error
	verifyErr error
}

func (l *fakeLease) beginSQL() (context.Context, func(error), error) {
	l.log.add("beginSQL")
	if l.beginErr != nil {
		return nil, nil, l.beginErr
	}
	ctx := l.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, func(error) { l.log.add("finish") }, nil
}

func (l *fakeLease) verify(context.Context) error {
	l.log.add("verify")
	return l.verifyErr
}

// healthStores 是带 storeHealth 计数的 fakeStores。
type healthStores struct {
	*fakeStores
	mu       sync.Mutex
	connLost map[uint32]int
	healthy  map[uint32]int
}

func newHealthStores() *healthStores {
	return &healthStores{fakeStores: newFakeStores(), connLost: map[uint32]int{}, healthy: map[uint32]int{}}
}

func (s *healthStores) ReportConnLost(storageID uint32, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connLost[storageID]++
}

func (s *healthStores) ReportHealthy(storageID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy[storageID]++
}

func (s *healthStores) CoolingDown(uint32) bool { return false }

func (s *healthStores) counts(storageID uint32) (connLost, healthy int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connLost[storageID], s.healthy[storageID]
}

var _ storeHealth = (*healthStores)(nil)

const runDBOpTestKey uint64 = 9601

type runDBOpFixture struct {
	mr     *miniredis.Miniredis
	rc     *redis.Client
	log    *orderLog
	stores *healthStores

	mu           sync.Mutex
	handlerCalls int
	handlerCtx   context.Context
}

func newRunDBOpFixture(t *testing.T) *runDBOpFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	log := &orderLog{}
	rc.AddHook(redisCmdLogger{log: log, names: map[string]bool{"mget": true, "set": true}})
	return &runDBOpFixture{mr: mr, rc: rc, log: log, stores: newHealthStores()}
}

// route 建一条选中本 zone 库、选库时既无落点记录也无 home_zone 的路由;复核时同样读不到记录即通过。
func (f *runDBOpFixture) route(op string) *placementRoute {
	router := &placementRouter{rc: f.rc, zone: testZoneID, stores: f.stores}
	return &placementRoute{router: router, op: op, key: runDBOpTestKey, storageID: testZoneID, store: proto2mysql.NewDB()}
}

func (f *runDBOpFixture) task(op string) *db_proto.DBTask {
	opCode := db_proto.DBOp_DB_OP_UNSPECIFIED
	switch op {
	case "read":
		opCode = db_proto.DBOp_DB_OP_READ
	case "write":
		opCode = db_proto.DBOp_DB_OP_WRITE
	}
	task := &db_proto.DBTask{Key: runDBOpTestKey, Op: opCode, MsgType: placementTestMsgType, TaskId: "run-db-op-" + op}
	if op == "read" {
		task.WhereCase = fmt.Sprintf("player_id='%d'", runDBOpTestKey)
	}
	return task
}

// handler 返回一个记录调用与 ctx、执行 hook 后返回 err 的 SQL 半程替身。
func (f *runDBOpFixture) handler(err error, hook func()) dbOpHandler {
	return func(ctx context.Context, _ *proto2mysql.DB, _ *db_proto.DBTask, _ proto.Message) (writeOutcome, error) {
		f.mu.Lock()
		f.handlerCalls++
		f.handlerCtx = ctx
		f.mu.Unlock()
		f.log.add("handler")
		if hook != nil {
			hook()
		}
		return writeOutcome{kind: writeApplied}, err
	}
}

func (f *runDBOpFixture) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handlerCalls
}

func (f *runDBOpFixture) cacheExists(task *db_proto.DBTask) bool {
	return f.mr.Exists(buildCacheKey(task))
}

func connLostSQLError() error {
	return fmt.Errorf("db write failed: %w",
		&storeerr.Error{Kind: storeerr.KindConnLost, Err: errors.New("write tcp 10.0.0.9:3306: connection reset by peer")})
}

func TestRunDBOp_WriteRequiresLease(t *testing.T) {
	f := newRunDBOpFixture(t)
	task := f.task("write")
	err := runDBOpError(context.Background(), f.rc, task, &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), nil)
	require.Error(t, err)
	assert.Zero(t, f.calls(), "a write without an ordering lease must not run any SQL")
	assert.False(t, f.cacheExists(task))
}

func TestRunDBOp_LostBeforeSQLSendsNothing(t *testing.T) {
	f := newRunDBOpFixture(t)
	task := f.task("write")
	lease := &fakeLease{log: f.log, rc: f.rc, beginErr: fmt.Errorf("%w: injected", errOrderingLeaseLost)}
	err := runDBOpError(context.Background(), f.rc, task, &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), lease)
	require.ErrorIs(t, err, errOrderingLeaseLost)
	assert.Zero(t, f.calls())
	assert.Equal(t, []string{"beginSQL"}, f.log.snapshot())
}

// 核锁与共享缓存写在同一令牌 Lua 中；失租时缓存不写，正常顺序为 SQL → 复核(MGET) → 令牌发布。
func TestRunDBOp_VerifyBeforeCachePublication(t *testing.T) {
	t.Run("lease lost at verify: no cache", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		task := f.task("write")
		lease := &fakeLease{log: f.log, rc: f.rc, verifyErr: fmt.Errorf("%w: owner changed", errOrderingLeaseLost)}
		err := runDBOpError(context.Background(), f.rc, task, &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), lease)
		require.ErrorIs(t, err, errOrderingLeaseLost)
		assert.False(t, f.cacheExists(task), "a write whose lease is lost must not publish the shared cache")
		assert.Equal(t, []string{"beginSQL", "handler", "finish", "mget", "verify"}, f.log.snapshot())
	})
	t.Run("verify interrupted by a canceled context: no cache, not a lease loss", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		task := f.task("write")
		lease := &fakeLease{log: f.log, rc: f.rc, verifyErr: fmt.Errorf("verify ordering lease: %w", context.Canceled)}
		err := runDBOpError(context.Background(), f.rc, task, &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), lease)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errOrderingLeaseLost)
		assert.Contains(t, err.Error(), "verify ordering lease")
		assert.False(t, f.cacheExists(task))
	})
	t.Run("normal order", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		task := f.task("write")
		lease := &fakeLease{log: f.log, rc: f.rc}
		err := runDBOpError(context.Background(), f.rc, task, &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), lease)
		require.NoError(t, err)
		assert.True(t, f.cacheExists(task))
		assert.Equal(t, []string{"beginSQL", "handler", "finish", "mget", "verify", "publish"}, f.log.snapshot())
	})
}

// handleTask 级回归(旧代码下为红:旧实现先写缓存、后核锁):SQL 之后排序锁被别的实例接管,核锁发现易主 ——
// 共享缓存不写、游标不推进,任务进重试队列,延后原因记 lease_lost;Kafka 在重试副本落盘之后才 ACK。
func TestHandleTask_LeaseLostAfterSQLDoesNotPublishCache(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)
	const key uint64 = 9602
	wt := makeWriteTask(t, key, placementTestMsgType, 10)
	session := attachKafkaClaim(t, w, wt)
	lockKey := orderingLockRedisKey(w, wt.dbTask)
	h.setAfterCall(func(recordedCall) {
		// 锁在 SQL 期间过期并被别的实例拿走。
		require.NoError(t, mr.Set(lockKey, "taken-over-by-another-instance"))
	})
	deferredBefore := metricCount(t, "db_task_deferred_total", map[string]string{"op": "write", "reason": metrics.DeferLeaseLost})

	w.handleTask(wt, true)

	require.Len(t, h.callsForKey(key), 1, "the SQL did run")
	assert.False(t, cacheExists(t, w, key), "the shared cache must not be published after the lease is lost")
	_, hasCursor := appliedCursorOf(t, w, key)
	assert.False(t, hasCursor, "the applied cursor must not advance")
	assert.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey), "the write is kept for a retry")
	assert.Equal(t, deferredBefore+1,
		metricCount(t, "db_task_deferred_total", map[string]string{"op": "write", "reason": metrics.DeferLeaseLost}))
	assert.Equal(t, []int64{9}, session.marked(), "Kafka ACKs only after the retry copy is durable")
	value, err := mr.Get(lockKey)
	require.NoError(t, err)
	assert.Equal(t, "taken-over-by-another-instance", value, "the successor's lock must not be deleted")
}

// 写任务的 handler 拿到的是租约交出的 ctx(只随失租取消);读任务拿到的是调用方的 ctx。
func TestRunDBOp_HandlerGetsLeaseContextForWrites(t *testing.T) {
	type ctxKey struct{}
	callerCtx := context.WithValue(context.Background(), ctxKey{}, "caller")
	leaseCtx := context.WithValue(context.Background(), ctxKey{}, "lease")

	t.Run("write", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		lease := &fakeLease{log: f.log, rc: f.rc, ctx: leaseCtx}
		require.NoError(t, runDBOpError(callerCtx, f.rc, f.task("write"), &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), lease))
		assert.Equal(t, "lease", f.handlerCtx.Value(ctxKey{}))
	})
	t.Run("read", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		require.NoError(t, runDBOpError(callerCtx, f.rc, f.task("read"), &db_proto.TaskResult{}, f.handler(nil, nil), f.route("read"), nil))
		assert.Equal(t, "caller", f.handlerCtx.Value(ctxKey{}))
	})
}

// L-04 §2.3 第 8 条:只把 SQL 这一步的结果报给注册表。连接断开报 ConnLost;SQL 成功报一次 Healthy;
// SQL 之后的复核、缓存回写、核锁失败都来自 Redis,不报 ConnLost。
func TestRunDBOp_ReportsOnlySQLOutcome(t *testing.T) {
	cases := []struct {
		name         string
		sqlErr       error
		afterSQL     func(t *testing.T, f *runDBOpFixture)
		publishFails bool
		verifyErr    error
		wantErr      bool
		wantConnLost int
		wantHealthy  int
	}{
		{name: "sql conn lost", sqlErr: connLostSQLError(), wantErr: true, wantConnLost: 1},
		{name: "success", wantHealthy: 1},
		{name: "recheck finds the placement moved", wantErr: true, wantHealthy: 1,
			afterSQL: func(t *testing.T, f *runDBOpFixture) {
				require.NoError(t, f.mr.Set(placement.Key(runDBOpTestKey), "102:3"))
			}},
		{name: "recheck read fails on redis", wantErr: true, wantHealthy: 1,
			afterSQL: func(_ *testing.T, f *runDBOpFixture) { f.mr.SetError("ERR injected redis failure") }},
		{name: "cache publication fails", publishFails: true, wantErr: true, wantHealthy: 1},
		{name: "verify finds the lease lost", verifyErr: fmt.Errorf("%w: owner changed", errOrderingLeaseLost),
			wantErr: true, wantHealthy: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunDBOpFixture(t)
			publishRC := f.rc
			if tc.publishFails {
				broken := miniredis.RunT(t)
				broken.SetError("ERR injected cache failure")
				brokenRC := redis.NewClient(&redis.Options{Addr: broken.Addr()})
				t.Cleanup(func() { _ = brokenRC.Close() })
				publishRC = brokenRC
			}
			var hook func()
			if tc.afterSQL != nil {
				hook = func() { tc.afterSQL(t, f) }
			}
			lease := &fakeLease{log: f.log, rc: publishRC, verifyErr: tc.verifyErr}
			err := runDBOpError(context.Background(), publishRC, f.task("write"), &db_proto.TaskResult{},
				f.handler(tc.sqlErr, hook), f.route("write"), lease)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			connLost, healthy := f.stores.counts(testZoneID)
			assert.Equal(t, tc.wantConnLost, connLost, "ReportConnLost calls")
			assert.Equal(t, tc.wantHealthy, healthy, "ReportHealthy calls")
		})
	}
}

// SQL 的 ctx 被本进程取消(写:失租;读:claim 收回)时什么都不报;写任务按取消原因(失租)返回,不带 storeerr 标记
// (否则会被记成 conn_lost、豁免重试预算)。
func TestRunDBOp_CanceledSQLIsNotReported(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		// 与生产的租约同形:sqlCtx 只随失租取消,取消原因包着 errOrderingLeaseLost。
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(fmt.Errorf("%w: injected", errOrderingLeaseLost))
		lease := &fakeLease{log: f.log, rc: f.rc, ctx: ctx}
		sqlErr := connLostSQLError()
		err := runDBOpError(context.Background(), f.rc, f.task("write"), &db_proto.TaskResult{},
			f.handler(sqlErr, nil), f.route("write"), lease)
		require.Error(t, err)
		assert.ErrorIs(t, err, errOrderingLeaseLost, "a write canceled by lease loss is reported as lease loss")
		assert.NotErrorIs(t, err, sqlErr, "the SQL error is attached as text only, not kept in the error chain")
		assert.Equal(t, storeerr.KindNone, storeerr.KindOf(err), "a canceled SQL must not carry a store-side tag")
		connLost, healthy := f.stores.counts(testZoneID)
		assert.Zero(t, connLost)
		assert.Zero(t, healthy)
		assert.Equal(t, []string{"beginSQL", "handler", "finish"}, f.log.snapshot(), "no recheck, verify or cache after a canceled SQL")
	})
	t.Run("read", func(t *testing.T) {
		f := newRunDBOpFixture(t)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		err := runDBOpError(canceled, f.rc, f.task("read"), &db_proto.TaskResult{},
			f.handler(connLostSQLError(), nil), f.route("read"), nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, storeerr.KindNone, storeerr.KindOf(err))
		connLost, healthy := f.stores.counts(testZoneID)
		assert.Zero(t, connLost)
		assert.Zero(t, healthy)
	})
}

func TestRunDBOp_ObservesSQLStage(t *testing.T) {
	f := newRunDBOpFixture(t)
	before := stageCount(t, "write", metrics.StageSQL)
	require.NoError(t, runDBOpError(context.Background(), f.rc, f.task("write"), &db_proto.TaskResult{},
		f.handler(nil, nil), f.route("write"), &fakeLease{log: f.log, rc: f.rc}))
	assert.Equal(t, before+1, stageCount(t, "write", metrics.StageSQL))
}

func TestRunDBOpRejectedOutcomesAreHealthyAndNeverPublish(t *testing.T) {
	for _, kind := range []string{writeOlder, writeStaleOwner, writeLegacyAfterSeq, writeSchemaRegression} {
		t.Run(kind, func(t *testing.T) {
			f := newRunDBOpFixture(t)
			task := f.task("write")
			lease := &fakeLease{log: f.log, rc: f.rc}
			handler := func(context.Context, *proto2mysql.DB, *db_proto.DBTask, proto.Message) (writeOutcome, error) {
				f.log.add("handler")
				return writeOutcome{kind: kind, storedEpoch: 7}, nil
			}
			outcome, err := runDBOp(context.Background(), f.rc, task, &db_proto.TaskResult{}, handler, f.route("write"), lease)
			require.NoError(t, err)
			require.Equal(t, kind, outcome.kind)
			require.Equal(t, uint64(7), outcome.storedEpoch)
			require.False(t, f.cacheExists(task))
			lost, healthy := f.stores.counts(testZoneID)
			require.Zero(t, lost)
			require.Equal(t, 1, healthy)
			if kind == writeOlder {
				require.Equal(t, []string{"beginSQL", "handler", "finish", "mget", "verify"}, f.log.snapshot())
			} else {
				require.Equal(t, []string{"beginSQL", "handler", "finish"}, f.log.snapshot())
			}
		})
	}
}

func TestRunDBOpOlderMustConfirmBeforeLeaseVerification(t *testing.T) {
	for _, phase := range []string{"placement", "lease"} {
		t.Run(phase, func(t *testing.T) {
			f := newRunDBOpFixture(t)
			task := f.task("write")
			lease := &fakeLease{log: f.log, rc: f.rc}
			if phase == "placement" {
				require.NoError(t, f.mr.Set(placement.Key(runDBOpTestKey), "102:3"))
			} else {
				lease.verifyErr = errOrderingLeaseLost
			}
			handler := func(context.Context, *proto2mysql.DB, *db_proto.DBTask, proto.Message) (writeOutcome, error) {
				return writeOutcome{kind: writeOlder}, nil
			}
			_, err := runDBOp(context.Background(), f.rc, task, &db_proto.TaskResult{}, handler, f.route("write"), lease)
			require.Error(t, err)
			require.False(t, f.cacheExists(task))
			if phase == "placement" {
				require.NotContains(t, f.log.snapshot(), "verify")
			} else {
				require.ErrorIs(t, err, errOrderingLeaseLost)
			}
		})
	}
}

func TestRunDBOpCanceledSuccessfulHandlerCannotPublish(t *testing.T) {
	f := newRunDBOpFixture(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errOrderingLeaseLost)
	_, err := runDBOp(context.Background(), f.rc, f.task("write"), &db_proto.TaskResult{}, f.handler(nil, nil), f.route("write"), &fakeLease{log: f.log, rc: f.rc, ctx: ctx})
	require.ErrorIs(t, err, errOrderingLeaseLost)
	require.False(t, f.cacheExists(f.task("write")))
	require.Equal(t, []string{"beginSQL", "handler", "finish"}, f.log.snapshot())
}
