package kafka

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 同一个 zone 可以有多个 db 实例之后,重试收据必须有明确的归属(retry_ownership.go)。
// 这些用例跑在 miniredis 上:租约到期时刻取 Redis 的 TIME,用 mr.SetTime 推进,不依赖墙钟、不 sleep。

const (
	retryTestTopic = "test-db-task"
	retryTestReady = "kafka:retry:queue:test-db-task"
	retryTestLease = 30 * time.Second
)

type retryOwnershipFixture struct {
	mr   *miniredis.Miniredis
	rc   *redis.Client
	ctx  context.Context
	base time.Time
}

func newRetryOwnershipFixture(t *testing.T) *retryOwnershipFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	base := time.Unix(1_800_000_000, 0)
	mr.SetTime(base)
	return &retryOwnershipFixture{mr: mr, rc: rc, ctx: context.Background(), base: base}
}

func (f *retryOwnershipFixture) owner(instanceID string) *retryOwnership {
	return newRetryOwnership(f.rc, retryTestTopic, retryTestReady, instanceID, retryTestLease)
}

// advance 把 Redis 的时钟拨到「基准 + d」。
func (f *retryOwnershipFixture) advance(d time.Duration) { f.mr.SetTime(f.base.Add(d)) }

func (f *retryOwnershipFixture) list(t *testing.T, key string) []string {
	t.Helper()
	values, err := f.rc.LRange(f.ctx, key, 0, -1).Result()
	require.NoError(t, err)
	return values
}

func (f *retryOwnershipFixture) registered(t *testing.T, instanceID string) bool {
	t.Helper()
	_, err := f.rc.ZScore(f.ctx, retryInstancesKey(retryTestTopic), instanceID).Result()
	if err == redis.Nil {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestRetryOwnership_KeyNames(t *testing.T) {
	assert.Equal(t, "kafka:retry:instances:t", retryInstancesKey("t"))
	assert.Equal(t, "kafka:retry:processing:t", retryLegacyProcessingKey("t"))
	assert.Equal(t, "kafka:retry:processing:t:db-0:6000", retryInstanceProcessingKey("t", "db-0:6000"),
		"tools/merge_zone 的 P4 门禁按同一套键名查(dbRetryInstanceProcessingKey),两边必须一致")
}

func TestResolveRetryInstanceID(t *testing.T) {
	for _, tc := range []struct {
		name, configured, hostname, listenOn, want string
		wantErr                                    bool
	}{
		{name: "configured_wins", configured: " db-a ", hostname: "h", listenOn: "0.0.0.0:6000", want: "db-a"},
		{name: "hostname_and_port", hostname: "db-7f9c-x2x", listenOn: "0.0.0.0:6000", want: "db-7f9c-x2x:6000"},
		{name: "ipv6_listen", hostname: "h", listenOn: "[::]:6001", want: "h:6001"},
		{name: "listen_without_host_part", hostname: "h", listenOn: "6000", want: "h:6000"},
		{name: "no_hostname", listenOn: "0.0.0.0:6000", wantErr: true},
		{name: "no_listen", hostname: "h", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRetryInstanceID(tc.configured, tc.hostname, tc.listenOn)
			if tc.wantErr {
				require.Error(t, err, "没有可用的实例名时必须拒绝,不能用空名字登记")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRetryLeaseDuration(t *testing.T) {
	assert.Equal(t, 30*time.Second, retryLeaseDuration(0), "未配置取默认")
	assert.Equal(t, 30*time.Second, retryLeaseDuration(-5))
	assert.Equal(t, 10*time.Second, retryLeaseDuration(3), "过短的租约会把一次停顿误判成实例已死")
	assert.Equal(t, 45*time.Second, retryLeaseDuration(45))
}

// 认领:收据进**自己的** processing 列表,同时登记 / 续上租约;ready 空了返回 redis.Nil。
func TestRetryOwnership_ClaimLandsInOwnProcessingListAndRegistersLease(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	require.NoError(t, f.rc.LPush(f.ctx, retryTestReady, "one").Err())

	payload, err := a.claim(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), payload)
	assert.Equal(t, []string{"one"}, f.list(t, a.processingKey))
	assert.Empty(t, f.list(t, retryTestReady))
	assert.Empty(t, f.list(t, retryLegacyProcessingKey(retryTestTopic)), "新版不再往旧的共享列表里认领")
	assert.True(t, f.registered(t, "a"), "认领与续租在同一个原子操作里:持有收据的实例一定在登记表里")

	_, err = a.claim(f.ctx)
	assert.ErrorIs(t, err, redis.Nil)
}

// 二进制载荷经过 Lua 认领后必须逐字节不变(重试载荷是 protobuf)。
func TestRetryOwnership_ClaimIsBinarySafe(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	raw := []byte{0x00, 0xff, 0x0a, 0x80, 0x7f, 0x00, 0xc3, 0x28}
	require.NoError(t, f.rc.LPush(f.ctx, retryTestReady, raw).Err())

	payload, err := a.claim(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, raw, payload)
}

// 启动:只收回同名实例(上一次的自己)遗留的收据,**不碰**别的活实例的在途收据 —— 这正是以前
// 「启动时整表搬回 ready」在多实例下出问题的地方。
func TestRetryOwnership_StartRecoversOwnReceiptsOnly(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a, b := f.owner("a"), f.owner("b")
	require.NoError(t, b.renewLease(f.ctx))
	require.NoError(t, f.rc.LPush(f.ctx, b.processingKey, "b-in-flight").Err())
	require.NoError(t, f.rc.LPush(f.ctx, a.processingKey, "a-left-1", "a-left-2").Err())

	require.NoError(t, a.start(f.ctx))

	assert.ElementsMatch(t, []string{"a-left-1", "a-left-2"}, f.list(t, retryTestReady))
	assert.Empty(t, f.list(t, a.processingKey))
	assert.Equal(t, []string{"b-in-flight"}, f.list(t, b.processingKey), "别的活实例正在处理的收据不能被搬走")
	assert.True(t, f.registered(t, "a"))
	assert.True(t, f.registered(t, "b"))
}

// 孤儿回收:租约过期的实例,它名下的收据回到 ready,搬空后从登记表里摘掉。
func TestRetryOwnership_ReclaimsExpiredInstance(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a, b := f.owner("a"), f.owner("b")
	require.NoError(t, b.renewLease(f.ctx))
	require.NoError(t, f.rc.LPush(f.ctx, b.processingKey, "b-1", "b-2").Err())

	f.advance(retryTestLease + time.Second) // b 崩溃了,租约过期
	require.NoError(t, a.renewLease(f.ctx))
	result, err := a.reclaimOnce(f.ctx)
	require.NoError(t, err)

	assert.Equal(t, 2, result.orphansMoved)
	assert.Equal(t, 1, result.instancesDone)
	assert.ElementsMatch(t, []string{"b-1", "b-2"}, f.list(t, retryTestReady))
	assert.Empty(t, f.list(t, b.processingKey))
	assert.False(t, f.registered(t, "b"), "搬空之后才摘:登记表里不留已经没有收据的死实例")
	assert.True(t, f.registered(t, "a"))
}

// 活着的实例(租约未过期)不会被回收;刚好到期的那一刻算过期。
func TestRetryOwnership_LiveLeaseIsNotReclaimed(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a, b := f.owner("a"), f.owner("b")
	require.NoError(t, b.renewLease(f.ctx))
	require.NoError(t, f.rc.LPush(f.ctx, b.processingKey, "b-1").Err())

	f.advance(retryTestLease - time.Second)
	result, err := a.reclaimOnce(f.ctx)
	require.NoError(t, err)
	assert.Zero(t, result.orphansMoved)
	assert.Equal(t, []string{"b-1"}, f.list(t, b.processingKey))
	assert.True(t, f.registered(t, "b"))

	// 回收脚本自己也复核租约:列出过期名单之后、动手之前对方续上了,就一条都不动。
	moved, leased, err := a.drainInstance(f.ctx, "b", true)
	require.NoError(t, err)
	assert.True(t, leased)
	assert.Zero(t, moved)

	f.advance(retryTestLease) // 到期时刻本身算过期(score <= now)
	result, err = a.reclaimOnce(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.orphansMoved)
}

// 自己的租约过期了也不自己回收自己:下一次续租 / 认领会续上,自己搬自己只会制造重复执行。
func TestRetryOwnership_DoesNotReclaimItself(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	require.NoError(t, a.renewLease(f.ctx))
	require.NoError(t, f.rc.LPush(f.ctx, a.processingKey, "a-in-flight").Err())

	f.advance(retryTestLease + time.Second)
	result, err := a.reclaimOnce(f.ctx)
	require.NoError(t, err)
	assert.Zero(t, result.orphansMoved)
	assert.Equal(t, []string{"a-in-flight"}, f.list(t, a.processingKey))
}

// 旧版(单实例时代)的共享 processing 列表:新版每拍把它搬回 ready,不再往里写。
func TestRetryOwnership_DrainsLegacySharedProcessingList(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	legacy := retryLegacyProcessingKey(retryTestTopic)
	require.NoError(t, f.rc.LPush(f.ctx, legacy, "old-1", "old-2").Err())

	require.NoError(t, a.start(f.ctx))

	assert.ElementsMatch(t, []string{"old-1", "old-2"}, f.list(t, retryTestReady))
	assert.Empty(t, f.list(t, legacy))
}

// 不变量:被回收并摘出登记表的实例如果其实还活着,它下一次认领会在同一个原子操作里把自己登记回去 ——
// 不存在「不在登记表里、却持有收据」的列表(那样的收据没人会来回收)。
func TestRetryOwnership_ReclaimedButAliveInstanceReRegistersOnClaim(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a, b := f.owner("a"), f.owner("b")
	require.NoError(t, a.renewLease(f.ctx))

	f.advance(retryTestLease + time.Second) // a 长时间停顿,租约过期
	require.NoError(t, b.renewLease(f.ctx))
	_, err := b.reclaimOnce(f.ctx)
	require.NoError(t, err)
	require.False(t, f.registered(t, "a"), "a 名下没有收据,被直接摘出登记表")

	require.NoError(t, f.rc.LPush(f.ctx, retryTestReady, "late").Err())
	payload, err := a.claim(f.ctx) // a 醒来继续干活
	require.NoError(t, err)
	assert.Equal(t, []byte("late"), payload)
	assert.True(t, f.registered(t, "a"), "认领必须先把自己登记回去")

	f.advance(2*retryTestLease + 2*time.Second) // a 这次真的崩了
	require.NoError(t, b.renewLease(f.ctx))
	result, err := b.reclaimOnce(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.orphansMoved, "那条收据仍然回得到 ready,没有变成孤儿")
	assert.Equal(t, []string{"late"}, f.list(t, retryTestReady))
}

// 停机:没做完的收据立刻还回 ready 并摘掉租约,别的实例不必等租约过期。
func TestRetryOwnership_ReleaseReturnsReceiptsAndDropsLease(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a := f.owner("a")
	require.NoError(t, f.rc.LPush(f.ctx, retryTestReady, "one", "two").Err())
	_, err := a.claim(f.ctx)
	require.NoError(t, err)
	_, err = a.claim(f.ctx)
	require.NoError(t, err)
	require.Len(t, f.list(t, a.processingKey), 2)

	require.NoError(t, a.release(f.ctx))

	assert.ElementsMatch(t, []string{"one", "two"}, f.list(t, retryTestReady))
	assert.Empty(t, f.list(t, a.processingKey))
	assert.False(t, f.registered(t, "a"))
}

// 一次回收有上限(脚本执行期间 Redis 是阻塞的),但循环会把一个大列表搬完。
func TestRetryOwnership_ReclaimMovesListsLargerThanOneBatch(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a, b := f.owner("a"), f.owner("b")
	require.NoError(t, b.renewLease(f.ctx))
	total := retryReclaimBatch*2 + 7
	for i := 0; i < total; i++ {
		require.NoError(t, f.rc.LPush(f.ctx, b.processingKey, "r").Err())
	}

	f.advance(retryTestLease + time.Second)
	result, err := a.reclaimOnce(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, total, result.orphansMoved)
	assert.Len(t, f.list(t, retryTestReady), total)
	assert.False(t, f.registered(t, "b"))
}

// run:每拍续租并回收;逐拍驱动,不依赖墙钟。ticks 关闭后返回。
func TestRetryOwnership_RunRenewsLeaseAndReclaimsPerTick(t *testing.T) {
	f := newRetryOwnershipFixture(t)
	a, b := f.owner("a"), f.owner("b")
	require.NoError(t, a.renewLease(f.ctx))
	require.NoError(t, b.renewLease(f.ctx))
	require.NoError(t, f.rc.LPush(f.ctx, b.processingKey, "b-1").Err())

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.run(f.ctx, ticks)
	}()

	f.advance(retryTestLease + time.Second) // 两边的租约都过期了
	ticks <- time.Time{}                    // a 这一拍:先续上自己的,再回收 b
	close(ticks)
	<-done

	assert.Equal(t, []string{"b-1"}, f.list(t, retryTestReady))
	assert.False(t, f.registered(t, "b"))
	score, err := f.rc.ZScore(f.ctx, retryInstancesKey(retryTestTopic), "a").Result()
	require.NoError(t, err)
	wantExpiry := f.base.Add(2*retryTestLease + time.Second).UnixMilli()
	assert.InDelta(t, float64(wantExpiry), score, 1, "续租后的到期时刻 = Redis 当前时间 + 租约")
}

// 接线:消费者认领走归属登记,进死信等后续动作作用在本实例的 processing 列表上。
func TestRetryConsumer_ClaimsThroughOwnershipIntoInstanceList(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	owner := newRetryOwnership(w.redisClient, w.topic, w.retryQueueKey, "db-a:6000", retryTestLease)
	// 没有可信的原始分区的旧格式载荷:认领后会被原样移进死信,正好用来观察认领与搬运落在哪个列表上。
	task := makeWriteTask(t, 5401, "taskpb.TaskResult", 7).dbTask
	payload, err := encodeRetryPayload(task, 7, 0, false)
	require.NoError(t, err)
	require.NoError(t, w.redisClient.LPush(w.ctx, w.retryQueueKey, payload).Err())

	c := &KeyOrderedKafkaConsumer{
		redisClient:        w.redisClient,
		topic:              w.topic,
		partitionCount:     4,
		workers:            map[int32]*worker{0: w},
		ctx:                w.ctx,
		retryQueueKey:      w.retryQueueKey,
		retryProcessingKey: owner.processingKey,
		retryDeadQueueKey:  w.retryDeadQueueKey,
		retryMaxTimes:      3,
		retryOwner:         owner,
	}
	c.consumeRetryQueue()

	assert.Empty(t, h.callsForKey(task.Key))
	for key, want := range map[string]int64{
		w.retryQueueKey:      0,
		owner.processingKey:  0,
		w.retryProcessingKey: 0, // 旧的共享列表:新版不往里写
		w.retryDeadQueueKey:  1,
	} {
		n, err := w.redisClient.LLen(w.ctx, key).Result()
		require.NoError(t, err)
		assert.Equalf(t, want, n, "list %s", key)
	}
	_, err = w.redisClient.ZScore(w.ctx, retryInstancesKey(w.topic), "db-a:6000").Result()
	assert.NoError(t, err, "认领过的实例必须在登记表里")
}
