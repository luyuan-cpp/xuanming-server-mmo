package authority

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shared/rolefence"

	"github.com/stretchr/testify/require"
)

// actorVerifyFixture 是不连库的缓存夹具:时钟手动拨,read 只计数并按 fail 决定结果。
type actorVerifyFixture struct {
	now   time.Duration
	reads int
	fail  error
}

func (f *actorVerifyFixture) read(context.Context, actorVerifyKey) error {
	f.reads++
	return f.fail
}

func (f *actorVerifyFixture) cache(ttl time.Duration, maxEntries int) *actorVerifyCache {
	return newActorVerifyCache(ttl, maxEntries, func() time.Duration { return f.now })
}

var verifyKeyA = actorVerifyKey{player: 11, account: "owner", revision: 7}

func TestActorVerifyWithinTTLDoesNotReadAgain(t *testing.T) {
	f := &actorVerifyFixture{}
	c := f.cache(30*time.Second, 16)
	for range 3 {
		require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
		f.now += 10 * time.Second
	}
	require.Equal(t, 1, f.reads, "TTL 内只读一次")
}

func TestActorVerifyExpiredReadsAgain(t *testing.T) {
	f := &actorVerifyFixture{}
	c := f.cache(30*time.Second, 16)
	require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
	f.now += 30 * time.Second // 恰好到期算过期:窗口不超过 TTL
	require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
	require.Equal(t, 2, f.reads)
}

// 到期时间从读之前算起:读得慢不能把窗口拉长。
func TestActorVerifyExpiryCountsFromBeforeTheRead(t *testing.T) {
	f := &actorVerifyFixture{}
	c := f.cache(30*time.Second, 16)
	slow := func(ctx context.Context, key actorVerifyKey) error {
		f.now += 20 * time.Second
		return f.read(ctx, key)
	}
	require.NoError(t, c.verify(context.Background(), verifyKeyA, slow))
	f.now += 10 * time.Second
	require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
	require.Equal(t, 2, f.reads)
}

func TestActorVerifyDifferentIdentityDoesNotHit(t *testing.T) {
	for name, other := range map[string]actorVerifyKey{
		"revision": {player: 11, account: "owner", revision: 8},
		"account":  {player: 11, account: "new-owner", revision: 7},
		"player":   {player: 12, account: "owner", revision: 7},
	} {
		t.Run(name, func(t *testing.T) {
			f := &actorVerifyFixture{}
			c := f.cache(30*time.Second, 16)
			require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
			require.NoError(t, c.verify(context.Background(), other, f.read))
			require.Equal(t, 2, f.reads, "身份任何一维不同都不能命中")
		})
	}
}

func TestActorVerifyFailureIsNotCached(t *testing.T) {
	f := &actorVerifyFixture{fail: ErrOriginal}
	c := f.cache(30*time.Second, 16)
	require.ErrorIs(t, c.verify(context.Background(), verifyKeyA, f.read), ErrOriginal)
	require.ErrorIs(t, c.verify(context.Background(), verifyKeyA, f.read), ErrOriginal)
	require.Equal(t, 2, f.reads, "失败不写缓存,每次都重读")
	f.fail = nil
	require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read), "改好之后下一次就通过")
	require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
	require.Equal(t, 3, f.reads)
}

// SQL 故障同样不写缓存,错误原样返回给调用方判断。
func TestActorVerifyStorageErrorPassesThroughUncached(t *testing.T) {
	boom := errors.New("sql down")
	f := &actorVerifyFixture{fail: boom}
	c := f.cache(30*time.Second, 16)
	require.ErrorIs(t, c.verify(context.Background(), verifyKeyA, f.read), boom)
	require.Empty(t, c.expires)
}

func TestActorVerifyZeroTTLReadsEveryTime(t *testing.T) {
	f := &actorVerifyFixture{}
	c := f.cache(0, 16)
	for range 3 {
		require.NoError(t, c.verify(context.Background(), verifyKeyA, f.read))
	}
	require.Equal(t, 3, f.reads)
	require.Empty(t, c.expires, "TTL=0 不写缓存")
}

func TestActorVerifyOverflowClearsTheWholeCache(t *testing.T) {
	f := &actorVerifyFixture{}
	c := f.cache(30*time.Second, 2)
	first := actorVerifyKey{player: 1, account: "a", revision: 1}
	second := actorVerifyKey{player: 2, account: "a", revision: 1}
	third := actorVerifyKey{player: 3, account: "a", revision: 1}
	require.NoError(t, c.verify(context.Background(), first, f.read))
	require.NoError(t, c.verify(context.Background(), second, f.read))
	require.Len(t, c.expires, 2)
	// 过期条目重读后写回,条目数没有增加,不算超限。
	f.now += 31 * time.Second
	require.NoError(t, c.verify(context.Background(), first, f.read))
	require.Len(t, c.expires, 2)
	require.NoError(t, c.verify(context.Background(), third, f.read))
	require.Len(t, c.expires, 1, "超限整张清掉,只留下新条目")
	require.NoError(t, c.verify(context.Background(), first, f.read))
	require.Equal(t, 5, f.reads, "被清掉的条目下次要重读")
}

// 带 -race 跑才有鉴别力:多个 goroutine 同时命中、写入、触发清空。
func TestActorVerifyIsSafeForConcurrentUse(t *testing.T) {
	var clock atomic.Int64
	var reads atomic.Int64
	c := newActorVerifyCache(time.Second, 8, func() time.Duration { return time.Duration(clock.Load()) })
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				clock.Add(int64(time.Millisecond))
				key := actorVerifyKey{player: uint64(i%32 + worker), account: "a", revision: 1}
				_ = c.verify(context.Background(), key, func(context.Context, actorVerifyKey) error {
					reads.Add(1)
					return nil
				})
			}
		}()
	}
	wg.Wait()
	require.Positive(t, reads.Load())
	require.LessOrEqual(t, len(c.expires), 8)
}

// 校验走目录 RPC,不碰 SQL:Store 没有 db 也能判。TTL 内不重复打 RPC,这是全服事务量最大的路径。
func TestVerifyActorReadsDirectoryOverRPCAndCaches(t *testing.T) {
	states := newFixtureRoleStates()
	var now time.Duration
	s := &Store{states: states, actors: newActorVerifyCache(30*time.Second, 16, func() time.Duration { return now })}
	ctx := actor(t, 11, "original-11", 7)
	for range 3 {
		player, owned, err := s.verifyActor(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 11, player)
		require.Equal(t, role{account: "original-11", revision: 7}, owned)
	}
	require.Equal(t, 1, states.readCount(), "TTL 内只打一次 RPC")

	now += 31 * time.Second
	_, _, err := s.verifyActor(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, states.readCount(), "过期后重读")
}

func TestReadActorRejectsEveryNonOwnerState(t *testing.T) {
	key := actorVerifyKey{player: 11, account: "owner", revision: 7}
	for _, tc := range []struct {
		name    string
		prepare func(*fixtureRoleStates)
	}{
		{"目录里没有", func(f *fixtureRoleStates) { f.remove(11) }},
		{"已删除", func(f *fixtureRoleStates) { f.set(11, "", 8, 0) }},
		{"换了账号", func(f *fixtureRoleStates) { f.set(11, "buyer", 8, 0) }},
		{"账号回到原主但修订号变了", func(f *fixtureRoleStates) { f.set(11, "owner", 9, 0) }},
		{"修订号落后", func(f *fixtureRoleStates) { f.set(11, "owner", 6, 0) }},
		{"被冻结", func(f *fixtureRoleStates) { f.set(11, "owner", 7, 99) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := newFixtureRoleStates()
			tc.prepare(states)
			require.ErrorIs(t, (&Store{states: states}).readActor(context.Background(), key), ErrOriginal)
		})
	}
	states := newFixtureRoleStates()
	states.set(11, "owner", 7, 0)
	require.NoError(t, (&Store{states: states}).readActor(context.Background(), key))
	// 还在投票的准备令牌不是最终冻结:聊天不写资产,不替投票拦写入。
	states.setPreparing(11, "owner", 7, 99)
	require.NoError(t, (&Store{states: states}).readActor(context.Background(), key))
}

// 读不到权威状态是故障,不是"原身份失效":不能把存储抖动回成玩家归属变了,也不能写缓存。
func TestVerifyActorDirectoryFailureIsNotARejectionAndNotCached(t *testing.T) {
	states := newFixtureRoleStates()
	states.err = rolefence.ErrUnavailable
	s := &Store{states: states, actors: newActorVerifyCache(30*time.Second, 16, func() time.Duration { return 0 })}
	ctx := actor(t, 11, "original-11", 7)
	_, _, err := s.verifyActor(ctx)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.NotErrorIs(t, err, ErrOriginal)
	states.err = nil
	_, _, err = s.verifyActor(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, states.readCount(), "失败不进缓存,恢复之后马上重读")
}
