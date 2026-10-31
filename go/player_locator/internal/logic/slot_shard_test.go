package logic

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"player_locator/internal/svc"
	"shared/redisslot"
)

// 「成员登记 + 段租约」分片的行为测试:公平份额、无主段接管、退出与回归、读失败。
// 周期由用例逐个驱动(cycle),成员到期看注入的时钟,段租约到期看 miniredis 的 FastForward。

func newTestShard(sc *svc.ServiceContext, role, self string, clock *testClock) *slotShard {
	s := newSlotShard(sc.PlayerRedis, role, self, nil)
	s.now = clock.Now
	return s
}

// settle 让各副本按固定顺序跑够几轮,持有量收敛。
func settle(ctx context.Context, shards ...*slotShard) {
	for round := 0; round < 3; round++ {
		for _, s := range shards {
			s.cycle(ctx)
		}
	}
}

// assertEverySlotOwnedOnce 断言各副本持有的 slot 合起来正好覆盖全部 slot,且互不重叠。
func assertEverySlotOwnedOnce(t *testing.T, shards ...*slotShard) {
	t.Helper()
	owners := make([]int, redisslot.SlotCount)
	for _, s := range shards {
		require.True(t, slices.IsSorted(s.OwnedSlots()), "OwnedSlots 必须升序")
		for _, slot := range s.OwnedSlots() {
			owners[slot]++
		}
	}
	for slot, count := range owners {
		require.Equalf(t, 1, count, "slot %d 的持有者数不对", slot)
	}
}

func TestSlotShard_SingleReplicaHoldsEverySlot(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	s := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())

	s.cycle(context.Background())

	assertEverySlotOwnedOnce(t, s)
	members, err := mr.ZMembers(shardMembersKey(shardRoleLeaseMonitor))
	require.NoError(t, err)
	assert.Equal(t, []string{"replica-a"}, members)
	value, err := mr.Get(shardSegmentKey(shardRoleLeaseMonitor, 0))
	require.NoError(t, err)
	assert.Equal(t, "replica-a", value)
	assert.Equal(t, shardLeaseTTL, mr.TTL(shardSegmentKey(shardRoleLeaseMonitor, 0)))
}

// 公平份额:任意副本数下,各副本都不超过 ceil(256 / N),合起来正好覆盖全部 slot。
func TestSlotShard_FairShareConverges(t *testing.T) {
	for _, n := range []int64{2, 3, 5} {
		sc, _ := newTestSvcCtx(t)
		clock := newTestClock()
		shards := make([]*slotShard, 0, n)
		for i := int64(0); i < n; i++ {
			shards = append(shards, newTestShard(sc, shardRoleLeaseMonitor, "replica-"+string(rune('a'+i)), clock))
		}

		settle(context.Background(), shards...)

		assertEverySlotOwnedOnce(t, shards...)
		for _, s := range shards {
			assert.LessOrEqual(t, len(s.OwnedSlots()), fairShare(n)*slotsPerSegment, "N=%d", n)
		}
	}
}

// 新副本加入:老副本把超出份额的段放出来,新副本接管,没有段被两边同时持有。
func TestSlotShard_NewReplicaGetsItsShare(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	clock := newTestClock()
	first := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", clock)
	first.cycle(ctx)
	require.Len(t, first.OwnedSlots(), redisslot.SlotCount)

	second := newTestShard(sc, shardRoleLeaseMonitor, "replica-b", clock)
	second.cycle(ctx)
	assert.Empty(t, second.OwnedSlots(), "成员登记了,段都还在别人手里")

	first.cycle(ctx)
	assert.Len(t, first.OwnedSlots(), 128*slotsPerSegment, "超过份额的释放")
	second.cycle(ctx)
	assert.Len(t, second.OwnedSlots(), 128*slotsPerSegment, "无主的接管")
	assertEverySlotOwnedOnce(t, first, second)
}

// 无主段接管:一个副本不再心跳,它的段租约到期、成员到期之后,存活副本的份额变大,一个周期内接管全部无主段。
// 存活副本自己的段一直在续约,不受影响。
func TestSlotShard_SurvivorTakesOverOrphanedSegments(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	clock := newTestClock()
	survivor := newTestShard(sc, shardRoleGateSweep, "replica-a", clock)
	lost := newTestShard(sc, shardRoleGateSweep, "replica-b", clock)
	settle(ctx, survivor, lost)
	require.Len(t, survivor.OwnedSlots(), 128*slotsPerSegment)
	ownedBefore := slices.Clone(survivor.OwnedSlots())

	// lost 从这里起不再跑周期。20s 后 survivor 续约一次:份额还是 128,无主段还没有。
	mr.FastForward(20 * time.Second)
	clock.Advance(20 * time.Second)
	survivor.cycle(ctx)
	assert.Equal(t, ownedBefore, survivor.OwnedSlots())

	// 再过 11s:lost 的段租约与成员都到期了,survivor 自己的段因为刚续过约还在。
	mr.FastForward(11 * time.Second)
	clock.Advance(11 * time.Second)
	survivor.cycle(ctx)

	assertEverySlotOwnedOnce(t, survivor)
	members, err := mr.ZMembers(shardMembersKey(shardRoleGateSweep))
	require.NoError(t, err)
	assert.Equal(t, []string{"replica-a"}, members, "过期成员被心跳清掉")
}

// 副本全部退出后再来一个:段都到期了,它一个周期内持有全部 slot,没有永远没人扫的段。
func TestSlotShard_NextReplicaTakesEverythingAfterAllLeft(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	clock := newTestClock()
	settle(ctx, newTestShard(sc, shardRoleGateSweep, "replica-a", clock), newTestShard(sc, shardRoleGateSweep, "replica-b", clock))

	mr.FastForward(shardLeaseTTL + time.Second)
	clock.Advance(shardMemberTTL + time.Second)
	next := newTestShard(sc, shardRoleGateSweep, "replica-c", clock)
	next.cycle(ctx)

	assertEverySlotOwnedOnce(t, next)
}

// 不健康的副本退出分片:释放全部段、撤销成员登记,其余副本下一周期接管;恢复后回来,份额重新分。
func TestSlotShard_UnhealthyReplicaLeavesAndRejoins(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	clock := newTestClock()
	var healthy atomic.Bool
	healthy.Store(true)
	sick := newSlotShard(sc.PlayerRedis, shardRoleLeaseMonitor, "replica-sick", healthy.Load)
	sick.now = clock.Now
	other := newTestShard(sc, shardRoleLeaseMonitor, "replica-other", clock)
	settle(ctx, sick, other)
	require.Len(t, sick.OwnedSlots(), 128*slotsPerSegment)

	healthy.Store(false)
	sick.cycle(ctx)
	assert.Empty(t, sick.OwnedSlots())
	members, err := mr.ZMembers(shardMembersKey(shardRoleLeaseMonitor))
	require.NoError(t, err)
	assert.Equal(t, []string{"replica-other"}, members)

	other.cycle(ctx)
	assertEverySlotOwnedOnce(t, other)

	healthy.Store(true)
	settle(ctx, sick, other)
	assertEverySlotOwnedOnce(t, sick, other)
	assert.Len(t, sick.OwnedSlots(), 128*slotsPerSegment)
}

// 别的副本持有的段不碰:不续、不接管、值不变;它的租约没了才接管。
func TestSlotShard_LeavesSegmentHeldByAnotherAlone(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	key := shardSegmentKey(shardRoleLeaseMonitor, 7)
	require.NoError(t, mr.Set(key, "replica-other"))
	mr.SetTTL(key, shardLeaseTTL)
	s := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())

	s.cycle(ctx)

	assert.Len(t, s.OwnedSlots(), redisslot.SlotCount-slotsPerSegment)
	assert.NotContains(t, s.OwnedSlots(), uint16(7*slotsPerSegment))
	value, err := mr.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "replica-other", value)

	mr.Del(key)
	s.cycle(ctx)
	assertEverySlotOwnedOnce(t, s)
}

// 持有的段被别人抢走(本副本的租约曾经过期)时不再算持有。
func TestSlotShard_LostSegmentIsDropped(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	s := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())
	s.cycle(ctx)
	require.NoError(t, mr.Set(shardSegmentKey(shardRoleLeaseMonitor, 0), "replica-other"))

	s.cycle(ctx)

	assert.NotContains(t, s.OwnedSlots(), uint16(0))
	assert.Len(t, s.OwnedSlots(), redisslot.SlotCount-slotsPerSegment)
}

// segmentReadFailure 让这个客户端读不到指定的段(模拟副本到不了那个主节点):pipeline 照常发,这些 GET 按传输错误返回。
type segmentReadFailure struct {
	keys map[string]struct{}
}

func (h segmentReadFailure) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h segmentReadFailure) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h segmentReadFailure) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			if _, unreachable := h.keys[cmdArg(cmd, 1)]; unreachable && cmd.Name() == "get" {
				cmd.SetErr(errors.New("i/o timeout"))
			}
		}
		return err
	}
}

// 一个副本到不了某个主节点:它持有的、落在那个主节点上的段续不了约,到期后没有它的份额挡着,读得全的副本接管。
// 如果它还算成员,读得全的副本只能持满自己的份额,那些段就一直没人扫。恢复之后份额重新分。
func TestSlotShard_PartitionedReplicaGivesUpItsShare(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	clock := newTestClock()
	const role = shardRoleGateSweep
	healthy := newTestShard(sc, role, "replica-a", clock)
	partitioned := newSlotShard(secondClient(t, sc, mr).PlayerRedis, role, "replica-b", nil)
	partitioned.now = clock.Now
	settle(ctx, healthy, partitioned)
	require.Len(t, partitioned.OwnedSlots(), 128*slotsPerSegment)

	// 到不了的那部分段都在 partitioned 手里:它持有的段的后半截。
	held := partitioned.OwnedSlots()
	unreachable := map[string]struct{}{}
	for _, slot := range held[len(held)/2:] {
		unreachable[shardSegmentKey(role, int(slot)/slotsPerSegment)] = struct{}{}
	}
	partitioned.rdb.AddHook(segmentReadFailure{keys: unreachable})

	// 租约 30s,一周期 11s:第三轮时它到不了的段已经过期,读得全的副本接管,读得到的段它自己一直续着。
	for round := 0; round < 3; round++ {
		mr.FastForward(11 * time.Second)
		clock.Advance(11 * time.Second)
		partitioned.cycle(ctx)
		healthy.cycle(ctx)
	}

	assertEverySlotOwnedOnce(t, healthy, partitioned)
	assert.Len(t, partitioned.OwnedSlots(), 64*slotsPerSegment, "只留读得到的段")
	members, err := mr.ZMembers(shardMembersKey(role))
	require.NoError(t, err)
	assert.Equal(t, []string{"replica-a"}, members, "读不全的副本不算成员")

	// 恢复:重新登记,份额重新分,多出来的放给它。
	for key := range unreachable {
		delete(unreachable, key)
	}
	settle(ctx, partitioned, healthy)
	assertEverySlotOwnedOnce(t, healthy, partitioned)
	assert.Len(t, partitioned.OwnedSlots(), 128*slotsPerSegment)
}

// 所有副本都读不全(主节点真挂了):成员数归零也按 1 算,各自接管读得到的无主段,不至于全体放手。
func TestSlotShard_AllReplicasPartitionedStillHoldWhatTheyCanRead(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	s := newSlotShard(secondClient(t, sc, mr).PlayerRedis, shardRoleGateSweep, "replica-a", nil)
	s.rdb.AddHook(segmentReadFailure{keys: map[string]struct{}{shardSegmentKey(shardRoleGateSweep, 0): {}}})

	s.cycle(ctx)

	assert.Len(t, s.OwnedSlots(), redisslot.SlotCount-slotsPerSegment)
	assert.False(t, mr.Exists(shardMembersKey(shardRoleGateSweep)), "读不全的副本不算成员")
}

// 读不到持有者(这个副本到不了那个主节点):这一周期不算持有,不扫;下一周期读到了,段还是自己的,不必重抢。
func TestSlotShard_ReadFailureMeansNotHeldThisCycle(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	s := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())
	s.cycle(ctx)
	require.Len(t, s.OwnedSlots(), redisslot.SlotCount)

	var failing atomic.Bool
	failing.Store(true)
	rec := newCommandRecorder()
	rec.fail = func(cmd redis.Cmder) error {
		if failing.Load() && cmd.Name() == "get" {
			return errors.New("i/o timeout")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)

	s.cycle(ctx)
	assert.Empty(t, s.OwnedSlots())
	assert.Zero(t, rec.count("set"), "读失败的段不算无主,不抢")

	failing.Store(false)
	s.cycle(ctx)
	assertEverySlotOwnedOnce(t, s)
	assert.Zero(t, rec.count("set"), "段一直是自己的,续约就行")
}

// 单把段租约键类型不对:只跳过这一段,其余 255 段照常。
func TestSlotShard_UnreadableSegmentKeyOnlySkipsThatSegment(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	mr.HSet(shardSegmentKey(shardRoleLeaseMonitor, 3), "field", "not a lease")
	s := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())

	s.cycle(context.Background())

	assert.Len(t, s.OwnedSlots(), redisslot.SlotCount-slotsPerSegment)
	assert.NotContains(t, s.OwnedSlots(), uint16(3*slotsPerSegment))
}

// 成员 ZSET 读写失败:用上一次的成员数,持有的段照常续约;一次都没读到过就不接管,等主节点恢复。
func TestSlotShard_HeartbeatFailureKeepsSegmentsButDoesNotAcquireBlind(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	membersKey := shardMembersKey(shardRoleLeaseMonitor)
	var failing atomic.Bool
	rec := newCommandRecorder()
	rec.fail = func(cmd redis.Cmder) error {
		if failing.Load() && isScriptOn(cmd, membersKey) {
			return errors.New("i/o timeout")
		}
		return nil
	}
	sc.PlayerRedis.AddHook(rec)
	clock := newTestClock()
	holding := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", clock)
	holding.cycle(ctx)
	require.Len(t, holding.OwnedSlots(), redisslot.SlotCount)

	failing.Store(true)
	holding.cycle(ctx)
	assert.Len(t, holding.OwnedSlots(), redisslot.SlotCount)

	fresh := newTestShard(sc, shardRoleGateSweep, "replica-b", clock)
	freshKey := shardMembersKey(shardRoleGateSweep)
	rec.fail = func(cmd redis.Cmder) error {
		if isScriptOn(cmd, freshKey) {
			return errors.New("i/o timeout")
		}
		return nil
	}
	fresh.cycle(ctx)
	assert.Empty(t, fresh.OwnedSlots(), "不知道有多少成员时不接管")
}

// 优雅退出:释放全部段、撤销成员登记,之后周期是空操作;重复 Stop 无害。
func TestSlotShard_StopReleasesEverything(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	s := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())
	s.cycle(context.Background())

	s.Stop()
	s.Stop()

	assert.Empty(t, s.OwnedSlots())
	for c := 0; c < slotSegmentCount; c++ {
		require.False(t, mr.Exists(shardSegmentKey(shardRoleLeaseMonitor, c)), "segment %d", c)
	}
	assert.False(t, mr.Exists(shardMembersKey(shardRoleLeaseMonitor)))

	s.cycle(context.Background())
	assert.Empty(t, s.OwnedSlots(), "Stop 之后不再竞争")
	assert.False(t, mr.Exists(shardMembersKey(shardRoleLeaseMonitor)))
}

// 两个角色各一套键,互不相干:清扫退出分片不影响 LeaseMonitor。
func TestSlotShard_RolesAreIndependent(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	clock := newTestClock()
	monitor := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", clock)
	sweep := newTestShard(sc, shardRoleGateSweep, "replica-a", clock)
	monitor.cycle(ctx)
	sweep.cycle(ctx)
	assertEverySlotOwnedOnce(t, monitor)
	assertEverySlotOwnedOnce(t, sweep)

	sweep.Stop()
	monitor.cycle(ctx)

	assert.Empty(t, sweep.OwnedSlots())
	assertEverySlotOwnedOnce(t, monitor)
}

func TestFairShare(t *testing.T) {
	for members, want := range map[int64]int{1: 256, 2: 128, 3: 86, 4: 64, 5: 52, 255: 2, 256: 1, 257: 1, 1000: 1} {
		assert.Equal(t, want, fairShare(members), "members=%d", members)
		assert.GreaterOrEqual(t, int64(want)*members, int64(slotSegmentCount), "全员持满份额必须覆盖全部段")
	}
}

// 段的划分正好铺满全部 slot;段租约键落在段的第一个 slot 上,成员登记键落在 slot 0 上。
func TestSlotShardKeysLandOnTheirSegment(t *testing.T) {
	assert.Equal(t, redisslot.SlotCount, slotSegmentCount*slotsPerSegment)
	for _, role := range []string{shardRoleLeaseMonitor, shardRoleGateSweep} {
		assert.Equal(t, uint16(0), redisslot.HashSlot(shardMembersKey(role)))
		for c := 0; c < slotSegmentCount; c++ {
			assert.Equal(t, uint16(c*slotsPerSegment), redisslot.HashSlot(shardSegmentKey(role, c)))
		}
	}
}
