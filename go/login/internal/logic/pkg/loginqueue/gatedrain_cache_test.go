package loginqueue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 手动推进的时钟,缓存过期不依赖真实时间。
type drainTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *drainTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *drainTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newDrainTestClock() *drainTestClock {
	return &drainTestClock{now: time.Unix(1_800_000_000, 0)}
}

// 缓存期内同一 zone 只查一次 Redis;标记在缓存期内打上,过期后的下一次才看得到。
func TestDrainMarkCache_OneQueryPerZonePerRefresh(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	clock := newDrainTestClock()
	cache := NewDrainMarkCache(rdb, time.Second, clock.Now)
	gates := candidates(1, 2)

	assert.Empty(t, cache.Draining(ctx, 1, gates))
	queries := mr.CommandCount()

	require.NoError(t, MarkGateDraining(ctx, rdb, 2, clock.Now().Unix(), 600))
	afterMark := mr.CommandCount()
	clock.Advance(999 * time.Millisecond)
	for i := 0; i < 5; i++ {
		assert.Empty(t, cache.Draining(ctx, 1, gates), "缓存期内沿用上次的结果")
	}
	assert.Equal(t, afterMark, mr.CommandCount(), "缓存期内不再查 Redis")
	assert.Greater(t, afterMark, queries)

	clock.Advance(time.Millisecond)
	assert.Equal(t, map[uint32]bool{2: true}, cache.Draining(ctx, 1, gates), "满 refresh 后重新查,看到新标记")
	assert.Equal(t, afterMark+1, mr.CommandCount(), "过期后只查一次")
}

// 不同 zone 各缓存各的,互不顶替。
func TestDrainMarkCache_ZonesAreCachedSeparately(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()
	clock := newDrainTestClock()
	cache := NewDrainMarkCache(rdb, time.Second, clock.Now)
	zone2 := []GateCandidate{{NodeID: 7, IP: "10.0.0.2", Port: 18000, ZoneID: 2}}

	assert.Empty(t, cache.Draining(ctx, 1, candidates(1)))
	require.NoError(t, MarkGateDraining(ctx, rdb, 7, clock.Now().Unix(), 600))

	assert.Equal(t, map[uint32]bool{7: true}, cache.Draining(ctx, 2, zone2), "zone 2 第一次查,不受 zone 1 的缓存影响")
}

// 查询失败按「没人在排空」放行,且空结果同样缓存:Redis 出问题时不让每个请求都去撞一次。
func TestDrainMarkCache_FailureIsFailOpenAndCached(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	clock := newDrainTestClock()
	cache := NewDrainMarkCache(rdb, time.Second, clock.Now)
	gates := candidates(1, 2)
	require.NoError(t, MarkGateDraining(ctx, rdb, 2, clock.Now().Unix(), 600))

	mr.SetError("ERR simulated failure") // 不用 LOADING 之类前缀:go-redis 会把它们当可重试错误重发
	assert.Empty(t, cache.Draining(ctx, 1, gates), "查询失败按没人在排空放行")
	mr.SetError("")
	assert.Empty(t, cache.Draining(ctx, 1, gates), "失败的空结果在缓存期内沿用")

	clock.Advance(time.Second)
	assert.Equal(t, map[uint32]bool{2: true}, cache.Draining(ctx, 1, gates), "过期后重新查,排空标记恢复")
}

// refresh <= 0 不缓存,每次都查:svc 的单测靠它在两次调用之间打标记。
func TestDrainMarkCache_ZeroRefreshQueriesEveryTime(t *testing.T) {
	rdb, _ := newDrainRedis(t)
	ctx := context.Background()
	cache := NewDrainMarkCache(rdb, 0, nil)
	gates := candidates(1, 2)

	assert.Empty(t, cache.Draining(ctx, 1, gates))
	require.NoError(t, MarkGateDraining(ctx, rdb, 1, time.Now().Unix(), 600))
	assert.Equal(t, map[uint32]bool{1: true}, cache.Draining(ctx, 1, gates))
}

// 没有 Redis、没有候选、nil 缓存:都不打排空标记,也不建缓存条目。
func TestDrainMarkCache_NoRedisNoCandidatesOrNilCache(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()

	var nilCache *DrainMarkCache
	assert.Nil(t, nilCache.Draining(ctx, 1, candidates(1)))
	assert.Nil(t, NewDrainMarkCache(nil, time.Second, nil).Draining(ctx, 1, candidates(1)))

	cache := NewDrainMarkCache(rdb, time.Second, nil)
	before := mr.CommandCount()
	assert.Nil(t, cache.Draining(ctx, 9, nil))
	assert.Equal(t, before, mr.CommandCount(), "候选为空不查 Redis")
	assert.Empty(t, cache.zones, "候选为空不建缓存条目,条目数不超过真正有 gate 的 zone 数")
}

// 同一 zone 并发调用:过期时只有一个去查,其余等它的结果(-race 下也要干净)。
func TestDrainMarkCache_ConcurrentCallersShareOneQuery(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()
	clock := newDrainTestClock()
	cache := NewDrainMarkCache(rdb, time.Second, clock.Now)
	gates := candidates(1, 2, 3)
	require.NoError(t, MarkGateDraining(ctx, rdb, 3, clock.Now().Unix(), 600))
	before := mr.CommandCount()

	var wg sync.WaitGroup
	results := make([]map[uint32]bool, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = cache.Draining(ctx, 1, gates)
		}(i)
	}
	wg.Wait()

	for _, got := range results {
		assert.Equal(t, map[uint32]bool{3: true}, got)
	}
	assert.Equal(t, before+1, mr.CommandCount(), "16 个并发调用只查一次")
}
