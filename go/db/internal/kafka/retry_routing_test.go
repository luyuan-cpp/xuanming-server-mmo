package kafka

// 重试只交给分区属主(retry_routing.go,docs/design/db-retry-owner-routing.md)的单测。
//
// 全部跑在 miniredis 上;重试收据的租约到期时刻取 Redis 的 TIME,用 mr.SetTime 推进,不依赖墙钟。
// 消费者都是直接拼的(不连 Kafka):worker 没有 goroutine 在消费,派发的任务留在各自 taskCh 里,由用例逐条 handleTask /
// processTaskBatch,执行顺序因此是确定的。替身复用 key_ordered_consumer_test.go 的 recordingHarness / makeWriteTask、
// l04_deferral_test.go 的 drainDispatched、consumer_shutdown_test.go 的 shutdownFixture / newStopTestConsumer、
// instrumentation_test.go 的 metricCount、retry_ownership_test.go 的 retryOwnershipFixture。

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	db_config "db/internal/config"
	db_locker "db/internal/locker"
	"db/internal/metrics"
	db_proto "proto/db"

	"github.com/alicebob/miniredis/v2"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const routingTestTopic = "test-db-task-routing"

// isClosed 非阻塞地判断 channel 是否已关闭。
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func routeCount(t *testing.T, mode, action string) float64 {
	t.Helper()
	return metricCount(t, "db_retry_route_total", map[string]string{"mode": mode, "action": action})
}

func deadLetterCount(t *testing.T, reason string) float64 {
	t.Helper()
	return metricCount(t, "db_task_dead_letter_total", map[string]string{"reason": reason})
}

// ─────────────────────────── claimLease / ownedPartitions ───────────────────────────

func TestClaimLease_EnterExitIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newClaimLease(3, ctx)
	assert.True(t, isClosed(l.idle()), "a fresh lease has nothing in flight")
	assert.True(t, l.live())

	require.True(t, l.enter())
	busy := l.idle()
	assert.False(t, isClosed(busy), "idle must stay open while a retry is in flight")
	require.True(t, l.enter())
	assert.Equal(t, 2, l.inflightCount())
	l.exit()
	assert.False(t, isClosed(busy), "one retry is still in flight")
	l.exit()
	assert.True(t, isClosed(busy), "the last exit closes the idle channel taken while busy")
	assert.True(t, isClosed(l.idle()))
	assert.Zero(t, l.inflightCount())

	// 下溢:记 ERROR、钳到 0,不 panic,也不重复关闭 channel。
	assert.NotPanics(t, l.exit)
	assert.Zero(t, l.inflightCount())
	assert.True(t, isClosed(l.idle()))

	// 归零之后还能再进出(新 channel)。
	require.True(t, l.enter())
	assert.False(t, isClosed(l.idle()))
	l.exit()
	assert.True(t, isClosed(l.idle()))

	l.close()
	assert.False(t, l.enter(), "a closed lease takes no new claims")
	assert.False(t, l.live())
	assert.Nil(t, l.ctx.Err(), "close does not cancel the claim context")

	revocable, revoke := context.WithCancel(context.Background())
	l2 := newClaimLease(1, revocable)
	assert.False(t, isClosed(l2.revoked()))
	revoke()
	assert.False(t, l2.enter(), "a revoked claim takes no new claims")
	assert.False(t, l2.live())
	assert.True(t, isClosed(l2.revoked()))

	l3 := newClaimLease(0, nil)
	assert.True(t, l3.live(), "a nil context never revokes")
	assert.Nil(t, l3.revoked())
	require.True(t, l3.enter())
	l3.exit()
}

func TestClaimLease_NilReceiverSafe(t *testing.T) {
	var l *claimLease
	assert.NotPanics(t, func() {
		assert.False(t, l.enter())
		l.exit()
		l.close()
		assert.False(t, l.live())
		assert.Zero(t, l.inflightCount())
		assert.True(t, isClosed(l.idle()), "a nil lease is always idle")
		assert.Nil(t, l.revoked())
	})
}

func TestOwnedPartitions_NilReceiverSafe(t *testing.T) {
	var o *ownedPartitions
	before := metricCount(t, "db_claim_acquired_total", map[string]string{})
	assert.NotPanics(t, func() {
		l := o.acquire(2, context.Background())
		require.NotNil(t, l, "a nil table still hands out a usable, unregistered lease")
		assert.Equal(t, int32(2), l.partition)
		assert.True(t, l.live())
		require.True(t, l.enter())
		l.exit()
		assert.Nil(t, o.live())
		assert.Nil(t, o.lookup(2))
		o.release(l)
		assert.False(t, l.live(), "release still closes the lease")
		o.release(nil)
	})
	assert.Equal(t, before, metricCount(t, "db_claim_acquired_total", map[string]string{}),
		"a nil table does not count claims")
}

// 修订稿 §4 / §6:acquire(非 nil 接收者)每次计一次 db_claim_acquired_total;db_owned_partitions 跟着存活数走。
func TestOwnedPartitions_AcquireCountsClaimAcquired(t *testing.T) {
	ctx := context.Background()
	o := newOwnedPartitions()
	owned := func() float64 { return metricCount(t, "db_owned_partitions", map[string]string{}) }
	before := metricCount(t, "db_claim_acquired_total", map[string]string{})

	a := o.acquire(0, ctx)
	b := o.acquire(1, ctx)
	b2 := o.acquire(1, ctx) // 同分区的新一代覆盖旧条目
	assert.Equal(t, before+3, metricCount(t, "db_claim_acquired_total", map[string]string{}))
	assert.Equal(t, float64(2), owned())

	o.release(b) // 已被覆盖:表不变
	assert.Equal(t, float64(2), owned())
	assert.Same(t, b2, o.lookup(1))
	o.release(b2)
	assert.Equal(t, float64(1), owned())
	o.release(a)
	assert.Equal(t, float64(0), owned())
}

func TestOwnedPartitions_ReleaseOnlyRemovesOwnLease(t *testing.T) {
	ctx := context.Background()
	o := newOwnedPartitions()
	a := o.acquire(5, ctx)
	b := o.acquire(5, ctx)

	o.release(a)
	assert.Same(t, b, o.lookup(5), "releasing the older generation must not remove the newer one")
	assert.False(t, a.live())
	assert.True(t, b.live())

	o.release(b)
	assert.Nil(t, o.lookup(5))
}

func TestOwnedPartitions_LiveExcludesDeadLeases(t *testing.T) {
	bg := context.Background()
	revocable, revoke := context.WithCancel(bg)
	o := newOwnedPartitions()
	l3 := o.acquire(3, bg)
	l1 := o.acquire(1, bg)
	o.acquire(0, revocable)
	l2 := o.acquire(2, bg)

	revoke()
	l2.close()

	live := o.live()
	require.Len(t, live, 2)
	assert.Same(t, l1, live[0], "live leases are sorted by partition")
	assert.Same(t, l3, live[1])
	assert.Nil(t, o.lookup(0), "a revoked claim is not an owner")
	assert.Nil(t, o.lookup(2), "a closed claim is not an owner")
	assert.Same(t, l1, o.lookup(1))
}

// 16 个 goroutine × 1000 次并发登记 / 摘除 / 查询(配合 -race)。
func TestOwnedPartitions_ConcurrentAcquireRelease(t *testing.T) {
	ctx := context.Background()
	o := newOwnedPartitions()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			p := int32(g % 4)
			for i := 0; i < 1000; i++ {
				l := o.acquire(p, ctx)
				if l.enter() {
					l.exit()
				}
				_ = o.live()
				_ = o.lookup(p)
				o.release(l)
			}
		}(g)
	}
	wg.Wait()
	assert.Empty(t, o.live())
	for p := int32(0); p < 4; p++ {
		assert.Nil(t, o.lookup(p))
	}
	assert.Equal(t, float64(0), metricCount(t, "db_owned_partitions", map[string]string{}))
}

// ─────────────────────────── 键名与模式 ───────────────────────────

// 键名是运维排障与 tools/merge_zone(另一个模块,按字面量镜像)直接查的东西,钉死。
func TestRetryPartitionReadyKeys(t *testing.T) {
	const topic = "db_task_zone_1"
	assert.Equal(t, "kafka:retry:queue:db_task_zone_1", retryReadyKey(topic))
	assert.Equal(t, fmt.Sprintf("kafka:retry:queue:%s", topic), retryReadyKey(topic),
		"NewKeyOrderedKafkaConsumer used to build the shared ready key with this format")
	assert.Equal(t, "kafka:dead:queue:db_task_zone_1", retryDeadKey(topic))
	assert.Equal(t, "kafka:retry:queue:db_task_zone_1:p", retryPartitionReadyKeyPrefix(topic),
		"tools/merge_zone dbRetryPartitionQueuePrefix pins the same literal")
	assert.Equal(t, "kafka:retry:queue:db_task_zone_1:p3", retryPartitionReadyKey(topic, 3))
	assert.Equal(t, []string{
		"kafka:retry:queue:db_task_zone_1:p0", "kafka:retry:queue:db_task_zone_1:p1", "kafka:retry:queue:db_task_zone_1:p2",
	}, retryPartitionReadyKeys(topic, 3))
	assert.Nil(t, retryPartitionReadyKeys(topic, 0))
	assert.Nil(t, retryPartitionReadyKeys(topic, -1))

	assert.Equal(t, "kafka:retry:queue:db_task_zone_1:p*", retryPartitionReadyKeyPattern(topic),
		"tools/merge_zone dbRetryPartitionQueuePattern pins the same literal")
	assert.Equal(t, `kafka:retry:queue:t\*\?\[x\]\\:p*`, retryPartitionReadyKeyPattern(`t*?[x]\`))

	for _, tc := range []struct {
		key  string
		want int32
		ok   bool
	}{
		{key: "kafka:retry:queue:db_task_zone_1:p0", want: 0, ok: true},
		{key: "kafka:retry:queue:db_task_zone_1:p9", want: 9, ok: true},
		{key: "kafka:retry:queue:db_task_zone_1:p12", want: 12, ok: true},
		{key: "kafka:retry:queue:db_task_zone_1:p2147483647", want: 2147483647, ok: true},
		{key: "kafka:retry:queue:db_task_zone_1:p2147483648"},
		{key: "kafka:retry:queue:db_task_zone_1:p01"},
		{key: "kafka:retry:queue:db_task_zone_1:p-1"},
		{key: "kafka:retry:queue:db_task_zone_1:p+1"},
		{key: "kafka:retry:queue:db_task_zone_1:p"},
		{key: "kafka:retry:queue:db_task_zone_1:p1:x"},
		{key: "kafka:retry:queue:db_task_zone_11:p1"},
		{key: "kafka:retry:queue:db_task_zone_1_g2:p1"},
		{key: "kafka:retry:queue:db_task_zone_1"},
	} {
		got, ok := parseRetryPartitionReadyKey(topic, tc.key)
		assert.Equalf(t, tc.ok, ok, "parse %q", tc.key)
		if tc.ok {
			assert.Equalf(t, tc.want, got, "parse %q", tc.key)
		}
	}
}

func TestResolveConsumerRetryRouting(t *testing.T) {
	for _, tc := range []struct {
		routing    string
		partitions int32
		want       string
		wantErr    bool
	}{
		{routing: "", partitions: 10, want: db_config.RetryRoutingShared},
		{routing: "shared", partitions: 0, want: db_config.RetryRoutingShared},
		{routing: "owner", partitions: 10, want: db_config.RetryRoutingOwner},
		{routing: "owner", partitions: 0, wantErr: true},
		{routing: "Owner", partitions: 10, wantErr: true},
	} {
		got, err := resolveConsumerRetryRouting(tc.routing, tc.partitions)
		if tc.wantErr {
			assert.Errorf(t, err, "routing=%q partitions=%d", tc.routing, tc.partitions)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}

	// 配置取值 → 指标 label 的映射(两边分属 yaml 与 stress_summarize / 告警两个契约)。
	assert.Equal(t, metrics.RetryModeOwner, retryRouteModeOf(db_config.RetryRoutingOwner, true))
	assert.Equal(t, metrics.RetryModeShared, retryRouteModeOf(db_config.RetryRoutingOwner, false),
		"owner without partition keys behaves as shared")
	assert.Equal(t, metrics.RetryModeShared, retryRouteModeOf("", true))
	assert.Equal(t, metrics.RetryModeShared, retryRouteModeOf(db_config.RetryRoutingShared, true))
}

// ─────────────────────────── claimFrom(从 retry_ownership_test 移来) ───────────────────────────

// 从 p 键认领:收据进本实例 processing,登记表的租约在同一个原子操作里被续上;共享收件箱不受影响。
func TestRetryOwnership_ClaimFromUsesGivenListAndRenewsLease(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	p2Key := retryPartitionReadyKey(retryTestTopic, 2)
	require.NoError(t, f.rc.LPush(f.ctx, p2Key, "p2-one").Err())
	require.NoError(t, f.rc.LPush(f.ctx, retryTestReady, "shared-one").Err())

	f.advance(5 * time.Second)
	payload, err := a.claimFrom(f.ctx, p2Key)
	require.NoError(t, err)
	assert.Equal(t, []byte("p2-one"), payload)
	assert.Equal(t, []string{"p2-one"}, f.list(t, a.processingKey))
	assert.Empty(t, f.list(t, p2Key))
	assert.Equal(t, []string{"shared-one"}, f.list(t, retryTestReady), "claimFrom must not touch the shared inbox")

	score, err := f.rc.ZScore(f.ctx, retryInstancesKey(retryTestTopic), "a").Result()
	require.NoError(t, err, "claiming registers the instance in the same atomic step")
	wantExpiry := f.base.Add(5*time.Second + retryTestLease).UnixMilli()
	assert.InDelta(t, float64(wantExpiry), score, 1, "the lease is renewed from Redis TIME at claim time")

	_, err = a.claimFrom(f.ctx, p2Key)
	assert.ErrorIs(t, err, redis.Nil)
	_, err = a.claimFrom(f.ctx, "")
	require.Error(t, err, "an empty ready key is refused instead of falling back to the shared key")
	assert.NotErrorIs(t, err, redis.Nil)
}

// claim() 与 claimFrom(readyKey) 等价:同一个脚本、同一个 processing、同一份租约。
func TestRetryOwnership_ClaimDelegatesToReadyKey(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	assert.Equal(t, retryReadyKey(retryTestTopic), a.readyKey)
	require.NoError(t, f.rc.LPush(f.ctx, retryTestReady, "one", "two").Err())

	first, err := a.claim(f.ctx)
	require.NoError(t, err)
	second, err := a.claimFrom(f.ctx, a.readyKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), first, "both claim from the tail of the same list")
	assert.Equal(t, []byte("two"), second)
	assert.ElementsMatch(t, []string{"one", "two"}, f.list(t, a.processingKey))
	assert.Empty(t, f.list(t, retryTestReady))
	assert.True(t, f.registered(t, "a"))

	_, err = a.claim(f.ctx)
	assert.ErrorIs(t, err, redis.Nil)
	_, err = a.claimFrom(f.ctx, a.readyKey)
	assert.ErrorIs(t, err, redis.Nil)
}

// ─────────────────────────── 路由:替身 ───────────────────────────

// routingFixture:一个 miniredis(时钟钉在 base)+ 一个 recordingHarness,上面可以拼多个消费者(多个实例共用 Redis)。
type routingFixture struct {
	mr   *miniredis.Miniredis
	rc   *redis.Client
	ctx  context.Context
	h    *recordingHarness
	base time.Time
}

func newRoutingFixture(t *testing.T) *routingFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	base := time.Unix(1_800_000_000, 0)
	mr.SetTime(base)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &routingFixture{mr: mr, rc: rc, ctx: ctx, h: newHarness(t), base: base}
}

// consumer 拼一个只跑重试循环的消费者:instanceID 决定本实例的 processing 列表与登记名,mode 为 shared / owner,
// partitions 个 worker 经生产的 buildWorker 建出(路由字段随之透传)。worker 没有 goroutine,派发的任务留在 taskCh 里。
func (f *routingFixture) consumer(instanceID, mode string, partitions int32) *KeyOrderedKafkaConsumer {
	ready := retryReadyKey(routingTestTopic)
	owner := newRetryOwnership(f.rc, routingTestTopic, ready, instanceID, retryTestLease)
	c := &KeyOrderedKafkaConsumer{
		redisClient:          f.rc,
		topic:                routingTestTopic,
		partitionCount:       partitions,
		subShardCount:        1,
		isOfflineExpand:      true,
		workers:              map[int32]*worker{},
		wg:                   &sync.WaitGroup{},
		ctx:                  f.ctx,
		locker:               db_locker.NewRedisLocker(f.rc),
		retryQueueKey:        ready,
		retryProcessingKey:   owner.processingKey,
		retryDeadQueueKey:    retryDeadKey(routingTestTopic),
		retryConsumeInterval: time.Hour,
		retryMaxTimes:        3,
		retryOwner:           owner,
		placement:            newTestPlacementRouter(f.rc),
		now:                  time.Now,

		retryRouting:       mode,
		retryPartitionKeys: retryPartitionReadyKeys(routingTestTopic, partitions),
		owned:              newOwnedPartitions(),
	}
	for p := int32(0); p < partitions; p++ {
		c.workers[p] = c.buildWorker(p)
	}
	return c
}

func (f *routingFixture) push(t *testing.T, key string, payloads ...[]byte) {
	t.Helper()
	for _, p := range payloads {
		require.NoError(t, f.rc.LPush(f.ctx, key, p).Err())
	}
}

func (f *routingFixture) list(t *testing.T, key string) [][]byte {
	t.Helper()
	values, err := f.rc.LRange(f.ctx, key, 0, -1).Result()
	require.NoError(t, err)
	out := make([][]byte, 0, len(values))
	for _, v := range values {
		out = append(out, []byte(v))
	}
	return out
}

func (f *routingFixture) llen(t *testing.T, key string) int64 {
	t.Helper()
	n, err := f.rc.LLen(f.ctx, key).Result()
	require.NoError(t, err)
	return n
}

func (f *routingFixture) cursor(t *testing.T, key uint64) (string, bool) {
	t.Helper()
	raw, err := f.rc.Get(f.ctx, appliedSeqKey(routingTestTopic, key, placementTestMsgType)).Result()
	if err == redis.Nil {
		return "", false
	}
	require.NoError(t, err)
	return raw, true
}

func pKey(p int32) string { return retryPartitionReadyKey(routingTestTopic, p) }

// routedPayload 是 key 的一条写(版本 seq)以原始分区 origin 编码出的 v2 重试载荷;retryCount 写进载荷里的 DBTask。
func routedPayload(t *testing.T, key, seq uint64, origin int32, retryCount int32) ([]byte, *db_proto.DBTask) {
	t.Helper()
	task := makeWriteTask(t, key, placementTestMsgType, seq).dbTask
	task.RetryCount = retryCount
	payload, err := encodeRetryPayload(task, seq, origin, true)
	require.NoError(t, err)
	return payload, task
}

// decodeRetry 解出载荷里的 DBTask 与 v2 头。
func decodeRetry(t *testing.T, payload []byte) (*db_proto.DBTask, uint64, int32, bool) {
	t.Helper()
	seq, partition, hasPartition, taskBytes := unwrapRetryPayload(payload)
	var task db_proto.DBTask
	require.NoError(t, proto.Unmarshal(taskBytes, &task))
	return &task, seq, partition, hasPartition
}

func assertNoDispatch(t *testing.T, c *KeyOrderedKafkaConsumer) {
	t.Helper()
	for p, w := range c.workers {
		assert.Zerof(t, len(w.taskCh), "worker %d must not receive anything", p)
	}
}

func TestDBTaskOp_InvalidRetryIsDurablyQuarantined(t *testing.T) {
	for _, mode := range []string{db_config.RetryRoutingOwner, db_config.RetryRoutingShared} {
		for _, tc := range invalidDBTaskOpPayloads(t) {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				f := newRoutingFixture(t)
				c := f.consumer("db-enum-test", mode, 1)
				lease := c.owned.acquire(0, f.ctx)
				t.Cleanup(func() { c.owned.release(lease) })
				payload := wrapRetryPayload(7, 0, tc.raw)
				f.push(t, c.retryQueueKey, payload)
				before := deadLetterCount(t, metrics.DeadLetterMalformedPayload)

				c.consumeRetryQueue()

				assert.Equal(t, [][]byte{payload}, f.list(t, c.retryDeadQueueKey), "保留完整原收据以便排查")
				assert.Zero(t, f.llen(t, c.retryQueueKey))
				assert.Zero(t, f.llen(t, c.retryProcessingKey))
				assert.Empty(t, f.h.snapshot(), "非法重试不得执行 SQL")
				assertNoDispatch(t, c)
				assert.Equal(t, before+1, deadLetterCount(t, metrics.DeadLetterMalformedPayload),
					"未知操作优先于重试预算耗尽归为载荷错误")
			})
		}
	}
}

// ─────────────────────────── 路由:owner 模式 ───────────────────────────

// (a) owner、没有租约:收件箱按载荷里的原始分区分拣进 p3 / p1,字节不变、RetryCount 不变、不执行。
func TestRetryRouting_ForwardSortsByOriginPartition(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 4)
	p3, _ := routedPayload(t, 9601, 11, 3, 0)
	p1, _ := routedPayload(t, 9602, 12, 1, 0)
	f.push(t, c.retryQueueKey, p3, p1)
	before := routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteForwarded)

	c.consumeRetryQueue()

	assert.Equal(t, [][]byte{p3}, f.list(t, pKey(3)), "the payload moves byte-for-byte")
	assert.Equal(t, [][]byte{p1}, f.list(t, pKey(1)))
	assert.Zero(t, f.llen(t, pKey(0)))
	assert.Zero(t, f.llen(t, pKey(2)))
	assert.Zero(t, f.llen(t, c.retryQueueKey))
	assert.Zero(t, f.llen(t, c.retryProcessingKey), "forwarding never goes through processing")
	task, _, _, _ := decodeRetry(t, f.list(t, pKey(3))[0])
	assert.Zero(t, task.RetryCount, "forwarding does not consume the retry budget")
	assert.Empty(t, f.h.snapshot())
	assertNoDispatch(t, c)
	assert.Equal(t, before+2, routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteForwarded))
}

// (h) 旧格式(v1 / 裸 DBTask)与越界分区(9、负数)进死信:owner 模式由分拣脚本处理,shared 模式沿用认领之后的原路径,
// 两种模式的终态与死信原因计数相同。
func TestRetryRouting_ForwardDeadLettersLegacyAndOutOfRange(t *testing.T) {
	for _, mode := range []string{db_config.RetryRoutingOwner, db_config.RetryRoutingShared} {
		t.Run(mode, func(t *testing.T) {
			f := newRoutingFixture(t)
			c := f.consumer("db-a:6000", mode, 4)
			v1, err := encodeRetryPayload(makeWriteTask(t, 9603, placementTestMsgType, 7).dbTask, 7, 0, false)
			require.NoError(t, err)
			raw, err := encodeRetryPayload(makeWriteTask(t, 9604, placementTestMsgType, 8).dbTask, 0, 0, false)
			require.NoError(t, err)
			outOfRange, _ := routedPayload(t, 9605, 9, 9, 0)
			negative, _ := routedPayload(t, 9606, 10, -1, 0)
			f.push(t, c.retryQueueKey, v1, raw, outOfRange, negative)
			legacyBefore := deadLetterCount(t, metrics.DeadLetterLegacyPayload)
			invalidBefore := deadLetterCount(t, metrics.DeadLetterInvalidPartition)
			routeDeadBefore := routeCount(t, retryRouteModeOf(mode, true), metrics.RetryRouteDead)

			c.consumeRetryQueue()

			assert.ElementsMatch(t, [][]byte{v1, raw, outOfRange, negative}, f.list(t, c.retryDeadQueueKey))
			for p := int32(0); p < 4; p++ {
				assert.Zerof(t, f.llen(t, pKey(p)), "p%d", p)
			}
			assert.Zero(t, f.llen(t, c.retryQueueKey))
			assert.Zero(t, f.llen(t, c.retryProcessingKey))
			assert.Empty(t, f.h.snapshot())
			assert.Equal(t, legacyBefore+2, deadLetterCount(t, metrics.DeadLetterLegacyPayload))
			assert.Equal(t, invalidBefore+2, deadLetterCount(t, metrics.DeadLetterInvalidPartition))
			assert.Equal(t, routeDeadBefore+4, routeCount(t, retryRouteModeOf(mode, true), metrics.RetryRouteDead))
		})
	}
}

// 分拣脚本动手之前核对键类型:某个 p 键类型不对时整体报错、一条不动(Redis 脚本没有回滚,不能 RPOP 之后丢在半路)。
func TestRetryRouting_ForwardRefusesWrongTypeWithoutLoss(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 2)
	require.NoError(t, f.rc.Set(f.ctx, pKey(1), "not-a-list", 0).Err())
	payload, _ := routedPayload(t, 9607, 13, 0, 0)
	f.push(t, c.retryQueueKey, payload)

	c.consumeRetryQueue()

	assert.Equal(t, [][]byte{payload}, f.list(t, c.retryQueueKey), "the inbox keeps the payload")
	assert.Zero(t, f.llen(t, pKey(0)))
	assert.Zero(t, f.llen(t, c.retryDeadQueueKey))
}

// (b) A 持 {0}、B 持 {1},共用一个 Redis:A 只派出 P0 的任务、p1 原样;B 再派出 P1 的。执行完回执归零。
func TestRetryRouting_OwnerExecutesOnlyOwnedPartitions(t *testing.T) {
	f := newRoutingFixture(t)
	a := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 2)
	b := f.consumer("db-b:6000", db_config.RetryRoutingOwner, 2)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	leaseA := a.owned.acquire(0, ctxA)
	leaseB := b.owned.acquire(1, ctxB)
	pl0, t0 := routedPayload(t, 9611, 21, 0, 0)
	pl1, t1 := routedPayload(t, 9612, 22, 1, 0)
	f.push(t, pKey(0), pl0)
	f.push(t, pKey(1), pl1)
	executedBefore := routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteExecuted)

	a.consumeRetryQueue()
	gotA := drainDispatched(a.workers[0])
	require.Len(t, gotA, 1)
	assert.Equal(t, t0.TaskId, gotA[0].dbTask.TaskId)
	assert.True(t, gotA[0].fromRetry)
	assert.Equal(t, int32(0), gotA[0].originPartition)
	assert.Equal(t, uint64(21), gotA[0].seq)
	assert.Equal(t, int32(1), gotA[0].dbTask.RetryCount, "the claim spends one attempt in memory")
	assert.True(t, gotA[0].claimCtx == ctxA, "the task carries its claim lease's context")
	assert.Empty(t, drainDispatched(a.workers[1]))
	assert.Equal(t, [][]byte{pl1}, f.list(t, pKey(1)), "A must not touch a partition it does not own")
	assert.Equal(t, 1, leaseA.inflightCount())

	b.consumeRetryQueue()
	gotB := drainDispatched(b.workers[1])
	require.Len(t, gotB, 1)
	assert.Equal(t, t1.TaskId, gotB[0].dbTask.TaskId)
	assert.Empty(t, drainDispatched(b.workers[0]))
	assert.Zero(t, f.llen(t, pKey(0)))
	assert.Zero(t, f.llen(t, pKey(1)))
	assert.Equal(t, [][]byte{pl0}, f.list(t, a.retryProcessingKey))
	assert.Equal(t, [][]byte{pl1}, f.list(t, b.retryProcessingKey))
	assert.Equal(t, executedBefore+2, routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteExecuted))

	a.workers[0].handleTask(gotA[0], true)
	b.workers[1].handleTask(gotB[0], true)
	assert.Len(t, f.h.callsForKey(9611), 1)
	assert.Len(t, f.h.callsForKey(9612), 1)
	cursorA, ok := f.cursor(t, 9611)
	require.True(t, ok)
	assert.Equal(t, "v2:0:21", cursorA)
	assert.Zero(t, f.llen(t, a.retryProcessingKey), "the receipt is acked after success")
	assert.Zero(t, f.llen(t, b.retryProcessingKey))
	assert.Zero(t, leaseA.inflightCount())
	assert.True(t, isClosed(leaseA.idle()))
	assert.True(t, isClosed(leaseB.idle()))
}

// (c) 热备(没有任何租约):p0 里的收据不被认领,只分拣收件箱。
func TestRetryRouting_HotStandbyOnlyForwards(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-hot:6000", db_config.RetryRoutingOwner, 2)
	pl0, _ := routedPayload(t, 9621, 31, 0, 0)
	inbox, _ := routedPayload(t, 9622, 32, 1, 0)
	f.push(t, pKey(0), pl0)
	f.push(t, c.retryQueueKey, inbox)

	c.consumeRetryQueue()

	assert.Equal(t, [][]byte{pl0}, f.list(t, pKey(0)), "a hot standby never claims")
	assert.Equal(t, [][]byte{inbox}, f.list(t, pKey(1)), "but it still forwards the inbox")
	assert.Zero(t, f.llen(t, c.retryQueueKey))
	assert.Zero(t, f.llen(t, c.retryProcessingKey))
	assertNoDispatch(t, c)
	assert.Empty(t, f.h.snapshot())
}

// (d) 派发之后、执行之前 claim 被撤销:handleTask 入口复核,不调 handler,原字节(RetryCount 未加)回到 p{P},租约归零。
func TestRetryRouting_RevokedWhileQueuedRequeuesUntouched(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 1)
	claimCtx, revoke := context.WithCancel(context.Background())
	defer revoke()
	lease := c.owned.acquire(0, claimCtx)
	pl, _ := routedPayload(t, 9631, 41, 0, 0)
	f.push(t, pKey(0), pl)

	c.consumeRetryQueue()
	queued := drainDispatched(c.workers[0])
	require.Len(t, queued, 1)
	require.Equal(t, 1, lease.inflightCount())

	revoke()
	before := routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteRequeuedRevoked)
	c.workers[0].handleTask(queued[0], true)

	assert.Empty(t, f.h.callsForKey(9631), "a revoked claim must not reach the database")
	assert.Equal(t, [][]byte{pl}, f.list(t, pKey(0)), "the receipt returns byte-for-byte (RetryCount not spent)")
	assert.Zero(t, f.llen(t, c.retryProcessingKey))
	assert.Zero(t, lease.inflightCount())
	assert.True(t, isClosed(lease.idle()))
	assert.Equal(t, before+1, routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteRequeuedRevoked))
	_, hasCursor := f.cursor(t, 9631)
	assert.False(t, hasCursor)
}

// (e) handler 执行中 claim 被撤销:已经开始的重试持锁跑完(processCtx 是 w.ctx),handler 恰好一次,游标推进、收据 ack。
func TestRetryRouting_RevokedDuringHandlerCompletes(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 1)
	claimCtx, revoke := context.WithCancel(context.Background())
	defer revoke()
	lease := c.owned.acquire(0, claimCtx)
	pl, _ := routedPayload(t, 9641, 51, 0, 0)
	f.push(t, pKey(0), pl)
	f.h.setAfterCall(func(call recordedCall) {
		if call.key == 9641 {
			revoke()
		}
	})

	c.consumeRetryQueue()
	queued := drainDispatched(c.workers[0])
	require.Len(t, queued, 1)
	c.workers[0].handleTask(queued[0], true)

	assert.Len(t, f.h.callsForKey(9641), 1)
	cursor, ok := f.cursor(t, 9641)
	require.True(t, ok, "a retry revoked after its SQL started must still finish")
	assert.Equal(t, "v2:0:51", cursor)
	assert.Zero(t, f.llen(t, c.retryProcessingKey), "the receipt is acked")
	assert.Zero(t, f.llen(t, pKey(0)))
	assert.Zero(t, f.llen(t, c.retryQueueKey))
	assert.True(t, isClosed(lease.idle()))
}

// (f) Kafka 源失败的去向:owner 进 p{origin},shared 进共享收件箱;重试再失败按同一规则写回,RetryCount 持久化。
func TestRetryRouting_KafkaFailureDestinationByMode(t *testing.T) {
	for _, mode := range []string{db_config.RetryRoutingOwner, db_config.RetryRoutingShared} {
		t.Run(mode, func(t *testing.T) {
			f := newRoutingFixture(t)
			c := f.consumer("db-a:6000", mode, 2)
			w := c.workers[1]
			wt := makeKafkaWriteTask(t, 9651, placementTestMsgType, 60)
			wt.originPartition = 1
			wt.kafkaMsg.Partition = 1
			taskID := fmt.Sprintf("k=%d:t=%s:seq=%d", 9651, placementTestMsgType, 60)
			want := c.retryQueueKey
			other := pKey(1)
			if mode == db_config.RetryRoutingOwner {
				want, other = other, want
			}

			f.h.programFailure(taskID, 1)
			w.handleTask(wt, true)
			require.Equal(t, int64(1), f.llen(t, want), "the Kafka-origin failure is enqueued here")
			assert.Zero(t, f.llen(t, other))
			task, seq, origin, hasOrigin := decodeRetry(t, f.list(t, want)[0])
			assert.Equal(t, uint64(61), seq)
			assert.Equal(t, int32(1), origin)
			assert.True(t, hasOrigin)
			assert.Zero(t, task.RetryCount)

			// 重试再失败:写回同一个目的地,RetryCount 已经花掉的一次写进载荷。
			lease := c.owned.acquire(1, context.Background())
			f.h.programFailure(taskID, 1)
			c.consumeRetryQueue()
			queued := drainDispatched(w)
			require.Len(t, queued, 1)
			w.handleTask(queued[0], true)
			require.Equal(t, int64(1), f.llen(t, want))
			assert.Zero(t, f.llen(t, other))
			task, _, _, _ = decodeRetry(t, f.list(t, want)[0])
			assert.Equal(t, int32(1), task.RetryCount, "the spent attempt is persisted")
			assert.Zero(t, f.llen(t, c.retryProcessingKey))
			assert.True(t, isClosed(lease.idle()))
			assert.Empty(t, f.h.callsForKey(9651))
		})
	}
}

// (i) 收据放错了 p 键(origin=2 的放在 p0):原字节挪回 p2,不执行。
func TestRetryRouting_MisroutedMovesToOriginPartition(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 4)
	lease := c.owned.acquire(0, context.Background())
	pl, _ := routedPayload(t, 9671, 81, 2, 0)
	f.push(t, pKey(0), pl)
	before := routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteMisrouted)

	c.consumeRetryQueue()

	assert.Equal(t, [][]byte{pl}, f.list(t, pKey(2)))
	assert.Zero(t, f.llen(t, pKey(0)))
	assert.Zero(t, f.llen(t, c.retryProcessingKey))
	assertNoDispatch(t, c)
	assert.Empty(t, f.h.snapshot())
	assert.True(t, isClosed(lease.idle()))
	assert.Equal(t, before+1, routeCount(t, metrics.RetryModeOwner, metrics.RetryRouteMisrouted))
}

// (j) A 认领后崩溃(任务不处理、租约不再续):B 接手分区 0,租约过期后一拍内回收进收件箱 → 分拣 → 执行。
func TestRetryRouting_OrphanReclaimForwardExecute(t *testing.T) {
	f := newRoutingFixture(t)
	a := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 1)
	b := f.consumer("db-b:6000", db_config.RetryRoutingOwner, 1)
	a.owned.acquire(0, context.Background())
	pl, _ := routedPayload(t, 9681, 91, 0, 0)
	f.push(t, pKey(0), pl)

	a.consumeRetryQueue()
	require.Len(t, drainDispatched(a.workers[0]), 1, "A claims, then crashes before processing")
	require.Equal(t, [][]byte{pl}, f.list(t, a.retryProcessingKey))

	b.owned.acquire(0, context.Background()) // 再平衡之后分区 0 归 B
	f.mr.SetTime(f.base.Add(retryTestLease + time.Second))
	b.retryOwner.tick(f.ctx) // 续上自己的租约,回收 A 的孤儿收据进收件箱
	require.Equal(t, [][]byte{pl}, f.list(t, b.retryQueueKey))
	require.Zero(t, f.llen(t, a.retryProcessingKey))

	b.consumeRetryQueue() // 同一拍:分拣进 p0 并认领
	queued := drainDispatched(b.workers[0])
	require.Len(t, queued, 1, "a receipt forwarded this tick is claimable in the same tick")
	b.workers[0].handleTask(queued[0], true)

	cursor, ok := f.cursor(t, 9681)
	require.True(t, ok)
	assert.Equal(t, "v2:0:91", cursor)
	for _, key := range []string{b.retryQueueKey, pKey(0), a.retryProcessingKey, b.retryProcessingKey, b.retryDeadQueueKey} {
		assert.Zerof(t, f.llen(t, key), "list %s", key)
	}
}

// ─────────────────────────── 路由:shared 模式 ───────────────────────────

// (g) shared 消费者兜底排空 p*(从 owner 回滚的路径):本实例没有该分区的 claim 时计 executed_nonowner,有时计 executed。
func TestRetryRouting_SharedModeDrainsPartitionedLists(t *testing.T) {
	f := newRoutingFixture(t)
	c := f.consumer("db-s:6000", db_config.RetryRoutingShared, 4)
	pl, _ := routedPayload(t, 9661, 71, 2, 0)
	f.push(t, pKey(2), pl)
	nonOwnerBefore := routeCount(t, metrics.RetryModeShared, metrics.RetryRouteExecutedNonOwner)
	executedBefore := routeCount(t, metrics.RetryModeShared, metrics.RetryRouteExecuted)

	c.consumeRetryQueue()
	queued := drainDispatched(c.workers[2])
	require.Len(t, queued, 1)
	assert.Equal(t, int32(2), queued[0].originPartition)
	assert.Zero(t, f.llen(t, pKey(2)))
	assert.Equal(t, nonOwnerBefore+1, routeCount(t, metrics.RetryModeShared, metrics.RetryRouteExecutedNonOwner))
	c.workers[2].handleTask(queued[0], true)
	cursor, ok := f.cursor(t, 9661)
	require.True(t, ok)
	assert.Equal(t, "v2:2:71", cursor)
	assert.Zero(t, f.llen(t, c.retryProcessingKey))

	c.owned.acquire(2, context.Background())
	pl2, _ := routedPayload(t, 9662, 72, 2, 0)
	f.push(t, pKey(2), pl2)
	c.consumeRetryQueue()
	require.Len(t, drainDispatched(c.workers[2]), 1)
	assert.Equal(t, executedBefore+1, routeCount(t, metrics.RetryModeShared, metrics.RetryRouteExecuted),
		"a shared instance holding the origin partition's claim counts as executed")
}

// ─────────────────────────── 混跑 ───────────────────────────

// mixedRun 是 shared 与 owner 两个实例共用一个 Redis 的混跑:过程中翻转 owner 实例的租约,并按固定种子随机注入一次失败。
type mixedRun struct {
	f       *routingFixture
	shared  *KeyOrderedKafkaConsumer
	owner   *KeyOrderedKafkaConsumer
	leases  map[int32]*claimLease
	revokes map[int32]context.CancelFunc
}

const mixedPartitions int32 = 2

func newMixedRun(t *testing.T, keys int, writesPerKey uint64, seed int64) *mixedRun {
	t.Helper()
	f := newRoutingFixture(t)
	m := &mixedRun{
		f:       f,
		shared:  f.consumer("db-shared:6000", db_config.RetryRoutingShared, mixedPartitions),
		owner:   f.consumer("db-owner:6000", db_config.RetryRoutingOwner, mixedPartitions),
		leases:  map[int32]*claimLease{},
		revokes: map[int32]context.CancelFunc{},
	}
	rng := rand.New(rand.NewSource(seed))
	for k := 0; k < keys; k++ {
		key := uint64(9700 + k)
		origin := int32(key % uint64(mixedPartitions))
		for seq := uint64(1); seq <= writesPerKey; seq++ {
			payload, task := routedPayload(t, key, seq, origin, 0)
			if seq%2 == 1 {
				f.push(t, retryReadyKey(routingTestTopic), payload)
			} else {
				f.push(t, pKey(origin), payload)
			}
			if rng.Intn(3) == 0 {
				f.h.programFailure(task.TaskId, 1)
			}
		}
	}
	return m
}

// setOwnerLeases 让 owner 实例此刻恰好持有 want 这些分区:不在 want 里的租约撤销并摘除,缺的新登记。
func (m *mixedRun) setOwnerLeases(want ...int32) {
	keep := map[int32]bool{}
	for _, p := range want {
		keep[p] = true
	}
	for p, l := range m.leases {
		if !keep[p] {
			m.revokes[p]()
			m.owner.owned.release(l)
			delete(m.leases, p)
			delete(m.revokes, p)
		}
	}
	for _, p := range want {
		if _, ok := m.leases[p]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.leases[p] = m.owner.owned.acquire(p, ctx)
		m.revokes[p] = cancel
	}
}

// process 把两个实例各 worker 通道里的任务按批交给 processTaskBatch(与子 shard 的批量合并同一路径)。
func (m *mixedRun) process() {
	for _, c := range []*KeyOrderedKafkaConsumer{m.shared, m.owner} {
		for p := int32(0); p < mixedPartitions; p++ {
			if batch := drainDispatched(c.workers[p]); len(batch) > 0 {
				c.workers[p].processTaskBatch(batch, true)
			}
		}
	}
}

// receiptLists 是收据可能停留的全部列表。
func (m *mixedRun) receiptLists() []string {
	keys := []string{retryReadyKey(routingTestTopic), retryDeadKey(routingTestTopic),
		m.shared.retryProcessingKey, m.owner.retryProcessingKey, retryLegacyProcessingKey(routingTestTopic)}
	for p := int32(0); p < mixedPartitions; p++ {
		keys = append(keys, pKey(p))
	}
	return keys
}

func (m *mixedRun) drained(t *testing.T) bool {
	t.Helper()
	for _, key := range m.receiptLists() {
		if m.f.llen(t, key) > 0 {
			return false
		}
	}
	for _, c := range []*KeyOrderedKafkaConsumer{m.shared, m.owner} {
		for _, w := range c.workers {
			if len(w.taskCh) > 0 {
				return false
			}
		}
	}
	return true
}

// assertReceiptsUnique 断言每条载荷(按 TaskId)在全部收据列表里合计至多一份。
func (m *mixedRun) assertReceiptsUnique(t *testing.T, step string) {
	t.Helper()
	seen := map[string]string{}
	for _, key := range m.receiptLists() {
		for _, payload := range m.f.list(t, key) {
			task, _, _, _ := decodeRetry(t, payload)
			if where, dup := seen[task.TaskId]; dup {
				t.Fatalf("after %s: receipt %s is both in %s and %s", step, task.TaskId, where, key)
			}
			seen[task.TaskId] = key
		}
	}
}

// run 循环到全部列表与通道为空;checkEachStep 时每一步之后核对收据唯一。返回循环拍数。
func (m *mixedRun) run(t *testing.T, checkEachStep bool) int {
	t.Helper()
	leaseCycle := [][]int32{{}, {0}, {0, 1}, {1}}
	for i := 0; i < 300; i++ {
		if i%3 == 0 {
			m.setOwnerLeases(leaseCycle[(i/3)%len(leaseCycle)]...)
			if checkEachStep {
				m.assertReceiptsUnique(t, fmt.Sprintf("tick %d lease flip", i))
			}
		}
		m.shared.consumeRetryQueue()
		if checkEachStep {
			m.assertReceiptsUnique(t, fmt.Sprintf("tick %d shared", i))
		}
		m.owner.consumeRetryQueue()
		if checkEachStep {
			m.assertReceiptsUnique(t, fmt.Sprintf("tick %d owner", i))
		}
		// 刚登记的租约中途被撤销:一部分已派发的 owner 任务会在 handleTask 入口被原样搬回。
		if i%5 == 4 {
			m.setOwnerLeases()
		}
		m.process()
		if checkEachStep {
			m.assertReceiptsUnique(t, fmt.Sprintf("tick %d process", i))
		}
		if m.drained(t) {
			return i + 1
		}
	}
	t.Fatal("mixed routing did not drain within 300 ticks")
	return 0
}

// (k) shared 与 owner 混跑、过程中翻转租约、随机注入失败:最终全部列表为空、没有死信,每个 key 的游标与最后一次
// 落库都是最大 seq(同 TestConcurrentWorkers_FinalStateIsGlobalMax 的判据)。
func TestRetryRouting_MixedModesConverge(t *testing.T) {
	const keys = 12
	const writes = 4
	m := newMixedRun(t, keys, writes, 20261016)
	ticks := m.run(t, false)
	t.Logf("mixed routing drained in %d ticks", ticks)

	assert.Zero(t, m.f.llen(t, retryDeadKey(routingTestTopic)), "no write may be dead-lettered")
	for k := 0; k < keys; k++ {
		key := uint64(9700 + k)
		cursor, ok := m.f.cursor(t, key)
		require.Truef(t, ok, "key %d has no cursor", key)
		assert.Equalf(t, fmt.Sprintf("v2:%d:%d", key%uint64(mixedPartitions), writes), cursor, "key %d", key)
		calls := m.f.h.callsForKey(key)
		require.NotEmptyf(t, calls, "key %d had no effective writes", key)
		assert.Equalf(t, uint64(writes), extractSeqFromTaskID(calls[len(calls)-1].taskID),
			"key %d: the last applied write must be the global max", key)
	}
	for _, l := range m.leases {
		assert.True(t, isClosed(l.idle()))
	}
}

// (l) 没有 Kafka 重投时,每一步之后 {收件箱, p*, 各 processing, 旧共享 processing, 死信} 合计每条载荷至多一份。
func TestRetryRouting_ReceiptUniqueness(t *testing.T) {
	m := newMixedRun(t, 6, 3, 7)
	m.assertReceiptsUnique(t, "seed")
	m.run(t, true)
}

// ─────────────────────────── 回执 ───────────────────────────

// 认领之后的每个出口都把租约的在途数还回去(漏一个,每次再平衡都要等满排空时限)。
func TestRetryRouting_RetryDoneOnEveryExit(t *testing.T) {
	type env struct {
		f      *routingFixture
		c      *KeyOrderedKafkaConsumer
		lease  *claimLease
		revoke context.CancelFunc
	}
	setup := func(t *testing.T) *env {
		f := newRoutingFixture(t)
		c := f.consumer("db-a:6000", db_config.RetryRoutingOwner, 2)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return &env{f: f, c: c, lease: c.owned.acquire(0, ctx), revoke: cancel}
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, e *env)
	}{
		{name: "superseded", run: func(t *testing.T, e *env) {
			older, _ := routedPayload(t, 9801, 5, 0, 0)
			newer, _ := routedPayload(t, 9801, 6, 0, 0)
			e.f.push(t, pKey(0), older, newer)
			e.c.consumeRetryQueue()
			batch := drainDispatched(e.c.workers[0])
			require.Len(t, batch, 2)
			require.Equal(t, 2, e.lease.inflightCount())
			e.c.workers[0].processTaskBatch(batch, true)
			assert.Len(t, e.f.h.callsForKey(9801), 1, "the older write is superseded")
		}},
		{name: "handler panic", run: func(t *testing.T, e *env) {
			dbOpHandlers[db_proto.DBOp_DB_OP_WRITE] = func(context.Context, *proto2mysql.DB, *db_proto.DBTask, proto.Message) (writeOutcome, error) {
				panic("injected handler panic")
			}
			pl, _ := routedPayload(t, 9802, 5, 0, 0)
			e.f.push(t, pKey(0), pl)
			e.c.consumeRetryQueue()
			batch := drainDispatched(e.c.workers[0])
			require.Len(t, batch, 1)
			e.c.workers[0].handleTask(batch[0], true)
			assert.Equal(t, int64(1), e.f.llen(t, pKey(0)), "panic recovery still requeues the receipt")
		}},
		{name: "lease revoked while dispatch blocked", run: func(t *testing.T, e *env) {
			e.c.workers[0].taskCh = make(chan *workerTask) // 没人读:派发 select 堵住
			pl, _ := routedPayload(t, 9803, 5, 0, 0)
			e.f.push(t, pKey(0), pl)
			done := make(chan struct{})
			go func() {
				defer close(done)
				e.c.consumeRetryQueue()
			}()
			require.Eventually(t, func() bool { return e.lease.inflightCount() == 1 }, 5*time.Second, 5*time.Millisecond)
			e.revoke()
			waitDone(t, done, "owner retry loop after revocation")
			assert.Equal(t, [][]byte{pl}, e.f.list(t, pKey(0)), "the undispatched receipt goes back untouched")
			assert.Zero(t, e.f.llen(t, e.c.retryProcessingKey))
		}},
		{name: "malformed payload", run: func(t *testing.T, e *env) {
			bad := wrapRetryPayload(1, 0, []byte{0x0a, 0xff})
			e.f.push(t, pKey(0), bad)
			e.c.consumeRetryQueue()
			assert.Equal(t, [][]byte{bad}, e.f.list(t, e.c.retryDeadQueueKey))
		}},
		{name: "write retries exhausted", run: func(t *testing.T, e *env) {
			pl, _ := routedPayload(t, 9805, 5, 0, 3)
			e.f.push(t, pKey(0), pl)
			e.c.consumeRetryQueue()
			assert.Equal(t, [][]byte{pl}, e.f.list(t, e.c.retryDeadQueueKey))
		}},
		{name: "read retries exhausted", run: func(t *testing.T, e *env) {
			read := makeReadTask(t, 9806, placementTestMsgType, "exhausted").dbTask
			read.RetryCount = 3
			pl, err := encodeRetryPayload(read, 5, 0, true)
			require.NoError(t, err)
			e.f.push(t, pKey(0), pl)
			e.c.consumeRetryQueue()
			assert.Zero(t, e.f.llen(t, e.c.retryDeadQueueKey), "reads are never dead-lettered")
			assert.Zero(t, e.f.llen(t, e.c.retryProcessingKey))
		}},
		{name: "misrouted", run: func(t *testing.T, e *env) {
			pl, _ := routedPayload(t, 9807, 5, 1, 0)
			e.f.push(t, pKey(0), pl)
			e.c.consumeRetryQueue()
			assert.Equal(t, [][]byte{pl}, e.f.list(t, pKey(1)))
		}},
		{name: "worker shutdown leftovers", run: func(t *testing.T, e *env) {
			pl, _ := routedPayload(t, 9808, 5, 0, 0)
			e.f.push(t, pKey(0), pl)
			e.c.consumeRetryQueue()
			require.Equal(t, 1, e.lease.inflightCount())
			e.c.workers[0].discardQueuedRetries()
			assert.Zero(t, len(e.c.workers[0].taskCh))
			assert.Equal(t, [][]byte{pl}, e.f.list(t, e.c.retryProcessingKey), "the receipt waits for the shutdown release")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			tc.run(t, e)
			assert.Zero(t, e.lease.inflightCount(), "every exit must call retryDone exactly once")
			assert.True(t, isClosed(e.lease.idle()))
		})
	}
}

// ─────────────────────────── ConsumeClaim 排空等在途重试 ───────────────────────────

// waitLease 等 ConsumeClaim 为分区 0 登记租约。
func waitLease(t *testing.T, c *KeyOrderedKafkaConsumer) *claimLease {
	t.Helper()
	var lease *claimLease
	require.Eventually(t, func() bool {
		lease = c.owned.lookup(0)
		return lease != nil
	}, 5*time.Second, 5*time.Millisecond, "ConsumeClaim must register its claim lease before the main loop")
	return lease
}

// 再平衡(会话取消)时排空在同一截止内等租约的在途重试归零:在途不归零,ConsumeClaim 不返回。
//
// 这条用例只钉「排空等在途重试」,钉不住「排空第一步是 lease.close()」:drainAfterStop 的 acker.stop 先取消了
// claimCtx,即使漏掉 close,live() / enter() 也已经是 false。close 由 MessagesClosedDrainWaitsForInflightRetries
// (claimCtx 不取消)与 ShutdownDrainWaitsForInflightRetries 钉住。
func TestConsumeClaim_DrainWaitsForInflightRetries(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.owned = newOwnedPartitions()
	done := f.startClaim()
	lease := waitLease(t, f.c)
	require.True(t, lease.enter(), "simulate an owner retry dispatched under this claim")

	f.cancelSession()
	select {
	case err := <-done:
		t.Fatalf("ConsumeClaim returned (%v) while an owner retry was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	assert.Error(t, lease.ctx.Err(), "a rebalance revokes the claim, so live() / enter() below would be false even without lease.close()")
	assert.False(t, lease.live(), "the drain closes the lease first")
	assert.False(t, lease.enter(), "no new claims after the drain started")

	lease.exit()
	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
	assert.Nil(t, f.c.owned.lookup(0), "the lease is released when ConsumeClaim returns")
}

// 消息通道关闭而会话仍在(sarama 分区消费者遇到 ErrOffsetOutOfRange 自行关闭)时,排空第一步先 lease.close():
// 不再接新的 owner 重试认领,再等在途重试归零。这一路 claimCtx 不取消,所以 enter() 失败只能是 close 起的作用;
// 漏掉 close 时排空期间还能认领新的重试,排空要等满 claimDrainTimeout。
func TestConsumeClaim_MessagesClosedDrainWaitsForInflightRetries(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.owned = newOwnedPartitions()
	done := f.startClaim()
	lease := waitLease(t, f.c)
	require.True(t, lease.enter(), "simulate an owner retry dispatched under this claim")

	close(f.claim.messages) // 会话不取消、不撤拉取
	select {
	case err := <-done:
		t.Fatalf("ConsumeClaim returned (%v) while an owner retry was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.Eventually(t, func() bool { return !lease.live() }, 2*time.Second, 5*time.Millisecond,
		"the closed-messages drain must close the lease before waiting")
	assert.NoError(t, lease.ctx.Err(), "the session is still alive: the claim is not revoked on this path")
	entered := lease.enter()
	if entered {
		lease.exit() // 回归时把多认领的一条还回去,别让下面的等待卡满 claimDrainTimeout
	}
	assert.False(t, entered, "no new owner retry may be claimed once the drain started")

	lease.exit()
	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
	assert.Nil(t, f.c.owned.lookup(0), "the lease is released when ConsumeClaim returns")
}

// 在途重试一直不归零:排空仍按 drainTimeout 准时返回。
func TestConsumeClaim_DrainBoundedWhenRetryStuck(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.owned = newOwnedPartitions()
	f.c.drainTimeout = 200 * time.Millisecond
	done := f.startClaim()
	lease := waitLease(t, f.c)
	require.True(t, lease.enter())
	defer lease.exit()

	started := time.Now()
	f.cancelSession()
	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
	assert.GreaterOrEqual(t, time.Since(started), 150*time.Millisecond, "the drain waits for the in-flight retry up to its timeout")
}

// 停机(撤拉取)时 drainClaimForShutdown 同样先 close 租约再等在途重试;停机排空期间 claimCtx 不取消,
// 在途的重试照常跑完。
func TestConsumeClaim_ShutdownDrainWaitsForInflightRetries(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.owned = newOwnedPartitions()
	done := f.startClaim()
	lease := waitLease(t, f.c)
	require.True(t, lease.enter())

	f.c.stopPulling()
	select {
	case err := <-done:
		t.Fatalf("ConsumeClaim returned (%v) during shutdown while an owner retry was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	assert.False(t, lease.live(), "the shutdown drain closes the lease first")
	assert.NoError(t, lease.ctx.Err(), "the shutdown drain does not revoke the claim: dispatched retries finish")

	lease.exit()
	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
}

// 停机排空也有上限:在途重试不归零时按 shutdownDrainTimeout 返回(R1 的接缝:计时器一路必须保留)。
func TestConsumeClaim_ShutdownDrainBoundedWhenRetryStuck(t *testing.T) {
	f := newShutdownFixture(t)
	f.c.owned = newOwnedPartitions()
	f.c.shutdownDrainTimeout = 200 * time.Millisecond
	done := f.startClaim()
	lease := waitLease(t, f.c)
	require.True(t, lease.enter())
	defer lease.exit()

	f.c.stopPulling()
	require.NoError(t, waitClaimResultWithin(t, done, 2*time.Second))
}

// ─────────────────────────── 搬回工具 ───────────────────────────

func TestRequeuePartitionedRetries_MovesV2AndIsIdempotent(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()
	const topic = "db_task_zone_1"
	p := func(n int32) string { return retryPartitionReadyKey(topic, n) }

	p0a, _ := routedPayload(t, 9901, 1, 0, 0)
	p0b, _ := routedPayload(t, 9902, 2, 0, 1)
	p3v2, _ := routedPayload(t, 9903, 3, 3, 0)
	legacy, err := encodeRetryPayload(makeWriteTask(t, 9904, placementTestMsgType, 4).dbTask, 4, 0, false)
	require.NoError(t, err)
	stray, _ := routedPayload(t, 9905, 5, 12, 0)
	require.NoError(t, rc.LPush(ctx, p(0), p0a, p0b).Err())
	require.NoError(t, rc.LPush(ctx, p(3), p3v2, legacy).Err())
	require.NoError(t, rc.LPush(ctx, p(12), stray).Err()) // PartitionCnt=4 之外的多余键

	// batch=1:每个键分多批搬,覆盖循环。
	report, err := RequeuePartitionedRetries(ctx, rc, topic, 4, 1)
	require.NoError(t, err)
	assert.Equal(t, map[int32]int64{0: 2, 1: 0, 2: 0, 3: 1}, report.Moved)
	assert.Equal(t, map[string]int64{p(12): 1}, report.Stray)
	assert.Equal(t, int64(1), report.Dead)

	inbox, err := rc.LRange(ctx, retryReadyKey(topic), 0, -1).Result()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{string(p0a), string(p0b), string(p3v2), string(stray)}, inbox, "payloads move byte-for-byte")
	dead, err := rc.LRange(ctx, retryDeadKey(topic), 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{string(legacy)}, dead)
	for _, key := range []string{p(0), p(3), p(12)} {
		n, err := rc.Exists(ctx, key).Result()
		require.NoError(t, err)
		assert.Zerof(t, n, "%s must be empty", key)
	}

	// 可重入:再跑一次什么都不动。
	again, err := RequeuePartitionedRetries(ctx, rc, topic, 4, 100)
	require.NoError(t, err)
	assert.Equal(t, map[int32]int64{0: 0, 1: 0, 2: 0, 3: 0}, again.Moved)
	assert.Empty(t, again.Stray)
	assert.Zero(t, again.Dead)
	n, err := rc.LLen(ctx, retryReadyKey(topic)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(4), n)

	t.Run("wrong type fails closed", func(t *testing.T) {
		require.NoError(t, rc.Set(ctx, p(1), "not-a-list", 0).Err())
		defer rc.Del(ctx, p(1))
		pl, _ := routedPayload(t, 9906, 6, 2, 0)
		require.NoError(t, rc.LPush(ctx, p(2), pl).Err())
		_, err := RequeuePartitionedRetries(ctx, rc, topic, 4, 100)
		require.Error(t, err)
		assert.Contains(t, err.Error(), p(1))
		got, err := rc.LRange(ctx, p(2), 0, -1).Result()
		require.NoError(t, err)
		assert.Equal(t, []string{string(pl)}, got, "keys after the failing one are left untouched")
	})

	t.Run("arguments", func(t *testing.T) {
		_, err := RequeuePartitionedRetries(ctx, rc, "", 4, 100)
		assert.Error(t, err)
		_, err = RequeuePartitionedRetries(ctx, rc, topic, 0, 100)
		assert.Error(t, err)
		_, err = RequeuePartitionedRetries(ctx, rc, topic, 4, 0)
		assert.Error(t, err)
		_, err = RequeuePartitionedRetries(ctx, rc, topic, 4, RequeueMaxBatch+1)
		assert.Error(t, err)

		// 工具(cmd/retry_requeue)在做任何读写之前用同一个校验;边界值放行,缺省值在范围内。
		assert.NoError(t, ValidateRequeueBatch(1))
		assert.NoError(t, ValidateRequeueBatch(RequeueMaxBatch))
		assert.NoError(t, ValidateRequeueBatch(RequeueDefaultBatch))
		assert.Error(t, ValidateRequeueBatch(0))
		assert.Error(t, ValidateRequeueBatch(RequeueMaxBatch+1))
		assert.Equal(t, 100, RequeueDefaultBatch, "the runbook documents -batch 100 as the default")
		assert.Equal(t, 1000, RequeueMaxBatch, "the runbook documents -batch in [1, 1000]")
	})
}

func TestRetryQueueDepths_Report(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()
	const topic = "db_task_zone_1"
	p := func(n int32) string { return retryPartitionReadyKey(topic, n) }

	require.NoError(t, rc.LPush(ctx, retryReadyKey(topic), "s1", "s2").Err())
	require.NoError(t, rc.LPush(ctx, p(0), "a").Err())
	require.NoError(t, rc.LPush(ctx, p(2), "b", "c", "d").Err())
	require.NoError(t, rc.LPush(ctx, p(7), "stray").Err())
	require.NoError(t, rc.LPush(ctx, retryDeadKey(topic), "x").Err())
	require.NoError(t, rc.LPush(ctx, retryLegacyProcessingKey(topic), "old").Err())
	require.NoError(t, rc.ZAdd(ctx, retryInstancesKey(topic),
		redis.Z{Score: 1, Member: "db-a:6000"}, redis.Z{Score: 2, Member: "db-b:6000"}).Err())
	require.NoError(t, rc.LPush(ctx, retryInstanceProcessingKey(topic, "db-a:6000"), "r1", "r2").Err())
	// 别的 topic 的 p 键不能被扫进来。
	require.NoError(t, rc.LPush(ctx, retryPartitionReadyKey("db_task_zone_11", 0), "other").Err())

	report, err := RetryQueueDepths(ctx, rc, topic, 4)
	require.NoError(t, err)
	assert.Equal(t, int64(2), report.Shared)
	assert.Equal(t, int64(1), report.Dead)
	assert.Equal(t, int64(1), report.LegacyProcessing)
	assert.Equal(t, map[string]int64{p(0): 1, p(1): 0, p(2): 3, p(3): 0, p(7): 1}, report.Partitioned)
	assert.Equal(t, int64(5), report.PartitionedTotal())
	assert.Equal(t, map[string]int64{"db-a:6000": 2, "db-b:6000": 0}, report.Instances)

	_, err = RetryQueueDepths(ctx, rc, "", 4)
	assert.Error(t, err)
	_, err = RetryQueueDepths(ctx, rc, topic, 0)
	assert.Error(t, err)
}

// ─────────────────────────── Stop 等重试协程 ───────────────────────────

// blockingClaimHook 让第一次重试认领(retryClaimScript 的 EVAL)真正执行完、收据已进 processing 之后停住,
// 直到 release 关闭:模拟「Stop 进行到一半,重试协程还在它最后一次认领里」。
type blockingClaimHook struct {
	once    sync.Once
	claimed chan struct{}
	release chan struct{}
}

func (h *blockingClaimHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *blockingClaimHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *blockingClaimHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if cmd.Name() != "eval" || len(args) < 2 {
			return err
		}
		if script, ok := args[1].(string); !ok || script != retryClaimScript {
			return err
		}
		first := false
		h.once.Do(func() { first = true })
		if first {
			close(h.claimed)
			<-h.release
		}
		return err
	}
}

// 修订稿 §2 / rulings 第 13 条:重试协程纳入 c.wg,Stop 的归还(release)由构造保证晚于它的最后一次认领 —— 否则
// 认领脚本先续租,会把刚摘掉的实例重新登记回去,收据留在一个没人回收的 processing 里。
func TestStop_RetryLoopJoinsWaitGroup(t *testing.T) {
	newHarness(t)
	c, _, rc := newStopTestConsumer(t)
	c.retryConsumeInterval = 10 * time.Millisecond
	hook := &blockingClaimHook{claimed: make(chan struct{}), release: make(chan struct{})}
	rc.AddHook(hook)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hook.release) }) }
	t.Cleanup(release)

	payload, _ := routedPayload(t, 9951, 3, 0, 0)
	require.NoError(t, rc.LPush(context.Background(), c.retryQueueKey, payload).Err())
	require.NoError(t, c.Start())
	waitDone(t, hook.claimed, "the retry loop's claim")

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the retry loop was still inside its last claim")
	case <-time.After(100 * time.Millisecond):
	}
	_, err := rc.ZScore(context.Background(), retryInstancesKey(c.topic), c.retryOwner.instanceID).Result()
	assert.NoError(t, err, "the instance stays registered until the retry loop has exited")

	release()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the retry loop was released")
	}
	_, err = rc.ZScore(context.Background(), retryInstancesKey(c.topic), c.retryOwner.instanceID).Result()
	assert.ErrorIs(t, err, redis.Nil, "the shutdown release deregisters the instance after the retry loop exited")
	processing, err := rc.LLen(context.Background(), c.retryProcessingKey).Result()
	require.NoError(t, err)
	assert.Zero(t, processing)
	ready, err := rc.LRange(context.Background(), c.retryQueueKey, 0, -1).Result()
	require.NoError(t, err)
	assert.Contains(t, ready, string(payload), "the claimed receipt is returned to the shared inbox")
}

func TestRetryDepthsIncludesUnregisteredProcessingAndMode(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()
	ctx := context.Background()
	require.NoError(t, rc.LPush(ctx, retryInstanceProcessingKey(routingTestTopic, "orphan"), "原收据").Err())
	require.NoError(t, rc.HSet(ctx, retryInstanceModesKey(routingTestTopic), "orphan", "owner").Err())
	report, err := RetryQueueDepths(ctx, rc, routingTestTopic, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), report.OrphanProcessing[retryInstanceProcessingKey(routingTestTopic, "orphan")])
	require.Equal(t, []string{"orphan"}, report.ModesWithoutLease)
	require.False(t, report.LooksUnused())
	require.Equal(t, int64(1), rc.LLen(ctx, retryInstanceProcessingKey(routingTestTopic, "orphan")).Val(), "只读盘点不能删孤儿")
}
