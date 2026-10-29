package kafka

// 玩家持久 schema 世代校验(persist_schema.go,docs/design/mixed-version-persist-field-protection.md §6.2)的单测。
// C1b 在行锁下的实际拒写由 player_row_fence_integration_test.go 的真库用例覆盖;这里覆盖判定函数、
// 进 SQL 之前的 C0 / C1、读失败回执、引导关闭标记与延后原因。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"db/internal/metrics"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	playerpb "proto/common/database"
	db_proto "proto/db"
	"shared/generated/persistschema"
)

const persistSchemaTestZone = 7

// resetPersistBootstrap 把引导期状态还原成开放。生产路径只允许开放 → 关闭,测试之间要显式复位。
func resetPersistBootstrap(t *testing.T) {
	t.Helper()
	persistBootstrapClosed.Store(false)
	t.Cleanup(func() { persistBootstrapClosed.Store(false) })
}

func playerTableMsgType() string {
	return string((&playerpb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName())
}

func playerWriteTask(t *testing.T, gen uint32) *db_proto.DBTask {
	t.Helper()
	body, err := proto.Marshal(&playerpb.PlayerDatabase{PlayerId: 42, PersistSchemaGen: gen})
	require.NoError(t, err)
	return &db_proto.DBTask{Key: 42, Op: db_proto.DBOp_DB_OP_WRITE, MsgType: playerTableMsgType(), Body: body, TaskId: fmt.Sprintf("persist-gen-%d", gen)}
}

func TestEffectiveGenTreatsZeroAsOne(t *testing.T) {
	assert.EqualValues(t, 1, effectiveGen(0))
	assert.EqualValues(t, 1, effectiveGen(1))
	assert.EqualValues(t, 9, effectiveGen(9))
}

func TestRequirePersistSchemaField(t *testing.T) {
	require.NoError(t, requirePersistSchemaField(&playerpb.PlayerDatabase{}))
	require.NoError(t, requirePersistSchemaField(&playerpb.PlayerDatabase_1{}))
	require.Error(t, requirePersistSchemaField(&db_proto.DBTask{}), "没有世代字段的消息必须被启动期检查拒绝")
}

func TestCheckWriteSchema(t *testing.T) {
	resetPersistBootstrap(t)
	gen := persistschema.Gen

	t.Run("non player table is ignored", func(t *testing.T) {
		kind, err := checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &db_proto.TaskResult{})
		require.NoError(t, err)
		assert.Empty(t, kind)
	})
	t.Run("read op is ignored", func(t *testing.T) {
		kind, err := checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_READ}, &playerpb.PlayerDatabase{PersistSchemaGen: gen + 1})
		require.NoError(t, err)
		assert.Empty(t, kind)
	})
	t.Run("unstamped write during bootstrap is accepted and counted", func(t *testing.T) {
		before := metricCount(t, "db_persist_schema_unstamped_write_total", nil)
		kind, err := checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &playerpb.PlayerDatabase{})
		require.NoError(t, err)
		assert.Empty(t, kind)
		assert.Equal(t, before+1, metricCount(t, "db_persist_schema_unstamped_write_total", nil))
	})
	t.Run("current generation passes", func(t *testing.T) {
		kind, err := checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &playerpb.PlayerDatabase_1{PersistSchemaGen: gen})
		require.NoError(t, err)
		assert.Empty(t, kind)
	})
	t.Run("newer generation is deferred (C1)", func(t *testing.T) {
		_, err := checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &playerpb.PlayerDatabase{PersistSchemaGen: gen + 1})
		require.ErrorIs(t, err, errSchemaTooNew)
	})
	t.Run("unstamped write after bootstrap closed is rejected (C0)", func(t *testing.T) {
		persistBootstrapClosed.Store(true)
		t.Cleanup(func() { persistBootstrapClosed.Store(false) })
		kind, err := checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &playerpb.PlayerDatabase{})
		require.NoError(t, err)
		assert.Equal(t, writePersistUnstamped, kind)
		// 盖了戳的写不受 C0 影响。
		kind, err = checkWriteSchema(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, &playerpb.PlayerDatabase{PersistSchemaGen: gen})
		require.NoError(t, err)
		assert.Empty(t, kind)
	})
}

func TestSchemaRegressed(t *testing.T) {
	row := func(g uint32) proto.Message { return &playerpb.PlayerDatabase{PlayerId: 42, PersistSchemaGen: g} }
	assert.True(t, schemaRegressed(row(2), row(3)), "低世代 body 不能盖高世代行")
	assert.False(t, schemaRegressed(row(3), row(3)))
	assert.False(t, schemaRegressed(row(4), row(3)))
	assert.False(t, schemaRegressed(row(0), row(1)), "0 按 1 处理:未盖戳的 body 对世代 1 的行不算倒退")
	assert.False(t, schemaRegressed(row(1), row(0)))
	assert.True(t, schemaRegressed(row(0), row(2)), "未盖戳的 body 对世代 2 的行算倒退")
	assert.False(t, schemaRegressed(&db_proto.TaskResult{}, &db_proto.TaskResult{}), "没有世代字段的表不判定")
}

func TestCheckReadSchema(t *testing.T) {
	gen := persistschema.Gen
	task := &db_proto.DBTask{Op: db_proto.DBOp_DB_OP_READ, TaskId: "r"}
	require.NoError(t, checkReadSchema(task, &playerpb.PlayerDatabase{}), "行不存在(全零)或未盖戳按世代 1")
	require.NoError(t, checkReadSchema(task, &playerpb.PlayerDatabase{PersistSchemaGen: gen}))
	require.ErrorIs(t, checkReadSchema(task, &playerpb.PlayerDatabase_1{PersistSchemaGen: gen + 1}), errSchemaTooNew)
	require.NoError(t, checkReadSchema(task, &db_proto.TaskResult{}))
}

// C0 / C1 在 processTaskWithoutLock 里、进 runDBOp 之前判定:不需要落点与租约,也就不会把没发 SQL 的结局报给注册表。
func TestProcessTaskWithoutLockAppliesWriteSchemaChecksBeforeSQL(t *testing.T) {
	resetPersistBootstrap(t)
	ctx := context.Background()

	_, err := processTaskWithoutLock(ctx, nil, playerWriteTask(t, persistschema.Gen+1), nil, nil)
	require.ErrorIs(t, err, errSchemaTooNew, "世代超前的写必须在 SQL 之前延后")

	persistBootstrapClosed.Store(true)
	outcome, err := processTaskWithoutLock(ctx, nil, playerWriteTask(t, 0), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, writePersistUnstamped, outcome.kind, "引导期关闭后未盖戳的写是确定性拒写")

	// 盖了当前世代的写通过世代检查,随后因测试没给落点而在 runDBOp 失败 —— 证明没被世代检查拦下。
	_, err = processTaskWithoutLock(ctx, nil, playerWriteTask(t, persistschema.Gen), nil, nil)
	require.Error(t, err)
	assert.False(t, errors.Is(err, errSchemaTooNew))
	assert.Contains(t, err.Error(), "no placement route")
}

func TestSchemaTooNewDeferralKeepsWriteBudget(t *testing.T) {
	cause := fmt.Errorf("wrapped: %w", errSchemaTooNew)
	assert.Equal(t, metrics.DeferSchemaTooNew, deferReason(cause))
	assert.True(t, retryBudgetExempt(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_WRITE}, cause), "写的世代超前不耗重试预算")
	assert.False(t, retryBudgetExempt(&db_proto.DBTask{Op: db_proto.DBOp_DB_OP_READ}, cause), "读不走延后,不享受豁免")
}

func TestFailReadSchemaTooNewAnswersFailureAtOnce(t *testing.T) {
	mr, rc, rec := newNotifyFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &worker{ctx: ctx, redisClient: rc}
	dbTask := &db_proto.DBTask{Key: 42, Op: db_proto.DBOp_DB_OP_READ, MsgType: playerTableMsgType(), TaskId: "t-schema", ReplyInstance: testReplyInstance}
	before := metricCount(t, "db_task_read_abandoned_total", map[string]string{"reason": metrics.ReadAbandonedSchemaTooNew})

	w.failReadSchemaTooNew(&workerTask{}, dbTask, fmt.Errorf("%w: row_gen=9", errSchemaTooNew))

	assertNotifiedOnce(t, rec, wireTaskResultInstancePrefix+testReplyInstance, "t-schema")
	res := readNotifyResult(t, mr, "t-schema")
	assert.False(t, res.GetSuccess())
	assert.Equal(t, metrics.ReadAbandonedSchemaTooNew, res.GetError())
	assert.Equal(t, before+1, metricCount(t, "db_task_read_abandoned_total", map[string]string{"reason": metrics.ReadAbandonedSchemaTooNew}))
	assert.False(t, mr.Exists(buildCacheKey(dbTask)), "截断副本不得回填子表缓存")
}

func newPersistBootstrapRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return mr, rc
}

func TestReadPersistBootstrapClosed(t *testing.T) {
	ctx := context.Background()
	mr, rc := newPersistBootstrapRedis(t)
	key := PersistBootstrapClosedKey(persistSchemaTestZone)
	assert.Equal(t, "db:persist_schema:bootstrap_closed:zone:7", key)

	closed, err := readPersistBootstrapClosed(ctx, rc, persistSchemaTestZone)
	require.NoError(t, err)
	assert.False(t, closed, "没有标记 = 引导期开放")

	require.NoError(t, mr.Set(key, PersistBootstrapClosedValue))
	closed, err = readPersistBootstrapClosed(ctx, rc, persistSchemaTestZone)
	require.NoError(t, err)
	assert.True(t, closed)

	require.NoError(t, mr.Set(key, "yes"))
	_, err = readPersistBootstrapClosed(ctx, rc, persistSchemaTestZone)
	require.Error(t, err, "不认识的取值不能猜")

	mr.Del(key)
	mr.HSet(key, "f", "v")
	_, err = readPersistBootstrapClosed(ctx, rc, persistSchemaTestZone)
	require.Error(t, err, "类型不对必须报错")

	mr.Close()
	_, err = readPersistBootstrapClosed(ctx, rc, persistSchemaTestZone)
	require.Error(t, err, "Redis 不可达必须报错,不能当成开放")
}

func TestLoadPersistBootstrapState(t *testing.T) {
	resetPersistBootstrap(t)
	ctx := context.Background()
	require.Error(t, LoadPersistBootstrapState(ctx, nil, persistSchemaTestZone))

	mr, rc := newPersistBootstrapRedis(t)
	require.NoError(t, LoadPersistBootstrapState(ctx, rc, persistSchemaTestZone))
	assert.False(t, persistBootstrapClosed.Load())
	assert.EqualValues(t, persistschema.Gen, metricCount(t, "db_persist_schema_gen", nil))
	assert.EqualValues(t, 0, metricCount(t, "db_persist_schema_bootstrap_closed", nil))

	require.NoError(t, mr.Set(PersistBootstrapClosedKey(persistSchemaTestZone), "garbage"))
	require.Error(t, LoadPersistBootstrapState(ctx, rc, persistSchemaTestZone), "判不清就拒启")

	require.NoError(t, mr.Set(PersistBootstrapClosedKey(persistSchemaTestZone), PersistBootstrapClosedValue))
	require.NoError(t, LoadPersistBootstrapState(ctx, rc, persistSchemaTestZone))
	assert.True(t, persistBootstrapClosed.Load())
	assert.EqualValues(t, 1, metricCount(t, "db_persist_schema_bootstrap_closed", nil))
}

func TestRefreshPersistBootstrapOnlyCloses(t *testing.T) {
	resetPersistBootstrap(t)
	ctx := context.Background()
	mr, rc := newPersistBootstrapRedis(t)
	key := PersistBootstrapClosedKey(persistSchemaTestZone)

	require.NoError(t, refreshPersistBootstrap(ctx, rc, persistSchemaTestZone))
	assert.False(t, persistBootstrapClosed.Load())

	require.NoError(t, mr.Set(key, "garbage"))
	require.Error(t, refreshPersistBootstrap(ctx, rc, persistSchemaTestZone))
	assert.False(t, persistBootstrapClosed.Load(), "复查失败维持现状")

	require.NoError(t, mr.Set(key, PersistBootstrapClosedValue))
	require.NoError(t, refreshPersistBootstrap(ctx, rc, persistSchemaTestZone))
	assert.True(t, persistBootstrapClosed.Load())

	// 标记被误删、或改成别的值,都不会让引导期重新开放。
	mr.Del(key)
	require.NoError(t, refreshPersistBootstrap(ctx, rc, persistSchemaTestZone))
	assert.True(t, persistBootstrapClosed.Load())
	require.NoError(t, mr.Set(key, "garbage"))
	require.NoError(t, refreshPersistBootstrap(ctx, rc, persistSchemaTestZone), "已关闭时不再读 Redis")
	assert.True(t, persistBootstrapClosed.Load())
}
