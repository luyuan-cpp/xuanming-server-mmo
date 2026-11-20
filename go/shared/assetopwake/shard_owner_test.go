package assetopwake

// 只订本副本负责的片(docs/design/redis-cluster-all-domains-20261110.md §4.6):BindShardOwner、管理循环与
// assetop_wake_subscribed_shards 的计数。全部走 fakeBus,不连 Redis。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ownedSet 是可在用例里随时改的「本副本负责哪些片」。
type ownedSet struct {
	mu     sync.Mutex
	shards map[int]bool
}

func newOwnedSet(shards ...int) *ownedSet {
	o := &ownedSet{}
	o.set(shards...)
	return o
}

func (o *ownedSet) set(shards ...int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.shards = map[int]bool{}
	for _, shard := range shards {
		o.shards[shard] = true
	}
}

func (o *ownedSet) owns(shard int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.shards[shard]
}

// gaugeCounter 在 fakeCounter 之上记下最近一次报出的已订片数。
type gaugeCounter struct {
	*fakeCounter
	subscribed atomic.Int64
}

func (c *gaugeCounter) SetWakeSubscribedShards(n int) { c.subscribed.Store(int64(n)) }

func waitGauge(t *testing.T, c *gaugeCounter, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for c.subscribed.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("等待%s超时:已订片数 %d,期望 %d", what, c.subscribed.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func startOwnedSubscriber(t *testing.T, bus *fakeBus, owned *ownedSet, counter Counter) *Subscriber {
	t.Helper()
	s := newTestSubscriber(t, bus, Config{}, newFakeWaker(), counter)
	if err := s.BindShardOwner(owned.owns); err != nil {
		t.Fatalf("BindShardOwner: %v", err)
	}
	s.ownerCheck = 5 * time.Millisecond
	s.Start(context.Background())
	return s
}

// 集群形状(一片一组):只订负责的片;负责的片变了,新片起订阅、不再负责的片退订,仍负责的片不重订。
func TestSubscribesOnlyOwnedShardsAndFollowsOwnershipChanges(t *testing.T) {
	bus := newFakeBus()
	bus.grouping = oneGroupPerChannel
	owned := newOwnedSet(0, 1)
	counter := &gaugeCounter{fakeCounter: newFakeCounter()}
	s := startOwnedSubscriber(t, bus, owned, counter)
	defer stopWithin(t, s)

	shard0 := bus.waitSubscribes(t, ShardChannel(0), 1)
	bus.waitSubscribes(t, ShardChannel(1), 1)
	waitGauge(t, counter, 2, "订上两片")

	owned.set(1, 2)
	bus.waitSubscribes(t, ShardChannel(2), 1)
	waitClosed(t, shard0, "不再负责的 0 号片退订")
	waitGauge(t, counter, 2, "换主之后仍是两片")
	if n := bus.subscribeCount(ShardChannel(1)); n != 1 {
		t.Fatalf("仍负责的 1 号片不该重订,订了 %d 次", n)
	}
	for shard := 3; shard < ShardCount; shard++ {
		if n := bus.subscribeCount(ShardChannel(uint64(shard))); n != 0 {
			t.Fatalf("不负责的 %d 号片被订了 %d 次", shard, n)
		}
	}

	stopWithin(t, s)
	waitGauge(t, counter, 0, "Stop 之后归 0")
}

// 单机形状(全部频道一组):组随负责的片变化,集合变了就按新集合整组换一条订阅。
func TestStandaloneGroupFollowsTheOwnedChannelSet(t *testing.T) {
	bus := newFakeBus()
	owned := newOwnedSet(5, 9)
	s := startOwnedSubscriber(t, bus, owned, nil)
	defer stopWithin(t, s)

	first := bus.waitSubscribes(t, ShardChannel(5), 1)
	if len(first.channels) != 2 || first.channels[0] != ShardChannel(5) || first.channels[1] != ShardChannel(9) {
		t.Fatalf("单机一组应只订负责的 5、9 号片,实际 %v", first.channels)
	}

	owned.set(9)
	second := bus.waitSubscribes(t, ShardChannel(9), 1)
	waitClosed(t, first, "旧集合的那条订阅关掉")
	if len(second.channels) != 1 || second.channels[0] != ShardChannel(9) {
		t.Fatalf("新的一组应只有 9 号片,实际 %v", second.channels)
	}
}

// 一片都不负责(成员表里有别的副本、本副本还没注册完):不订任何频道,交给负责的副本。
func TestOwningNoShardSubscribesNothing(t *testing.T) {
	bus := newFakeBus()
	owned := newOwnedSet()
	s := startOwnedSubscriber(t, bus, owned, nil)
	defer stopWithin(t, s)

	time.Sleep(20 * s.ownerCheck) // 管理循环重算了很多轮
	bus.mu.Lock()
	attempts := bus.attempts
	bus.mu.Unlock()
	if attempts != 0 {
		t.Fatalf("一片都不负责时不该订阅,实际订了 %d 次", attempts)
	}

	owned.set(7)
	bus.waitSubscribes(t, ShardChannel(7), 1)
}

// 没绑负责者:订全部片,gauge 报 64(与改造前的行为一致)。
func TestWithoutShardOwnerEveryShardIsSubscribed(t *testing.T) {
	bus := newFakeBus()
	bus.grouping = oneGroupPerChannel
	counter := &gaugeCounter{fakeCounter: newFakeCounter()}
	s := newTestSubscriber(t, bus, Config{}, newFakeWaker(), counter)
	s.Start(context.Background())
	defer stopWithin(t, s)

	for shard := range ShardCount {
		bus.waitSubscribes(t, ShardChannel(uint64(shard)), 1)
	}
	waitGauge(t, counter, ShardCount, "订上全部片")
}

func TestBindShardOwnerRules(t *testing.T) {
	var none *Subscriber
	if err := none.BindShardOwner(func(int) bool { return true }); err != nil {
		t.Fatalf("唤醒关着(nil 接收者)应是空操作,得到 %v", err)
	}

	s := newTestSubscriber(t, newFakeBus(), Config{}, newFakeWaker(), nil)
	if err := s.BindShardOwner(nil); err == nil {
		t.Fatal("owns 为 nil 必须拒绝")
	}
	s.Start(context.Background())
	defer stopWithin(t, s)
	if err := s.BindShardOwner(func(int) bool { return true }); err == nil {
		t.Fatal("Start 之后不许再绑")
	}
}
