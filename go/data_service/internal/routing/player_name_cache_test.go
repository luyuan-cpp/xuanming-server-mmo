package routing

import (
	"context"
	"testing"
	"time"

	"shared/clusterredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// newGuardedNameCache 用与线上同一种句柄(控制集群守卫 ControlClusterKeys)接 miniredis。
// 守卫按键规则判定,与拓扑无关:单实例上不带 tag 的多键命令同样被拒,所以这里能复现集群上的拒绝。
func newGuardedNameCache(t *testing.T) (*PlayerNameCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rds, err := clusterredis.New(clusterredis.Config{Addrs: []string{mr.Addr()}}, clusterredis.ControlClusterKeys)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rds.Close() })
	return NewPlayerNameCache(rds), mr
}

// 两个 id 的键落在同一个槽:按槽分组的 MGET(MGetBySlot)会把它们放进一条不带 tag 的多键命令,控制集群守卫拒绝,
// 一批 500 个 id 几乎必然遇到。单键 GET 的管道必须照常读出两个名字(回归:别把批量读改回 MGET / MGetBySlot)。
func TestMGetPlayerNamesWorksWhenKeysShareASlotUnderTheControlGuard(t *testing.T) {
	cache, _ := newGuardedNameCache(t)
	ctx := context.Background()
	first, second := uint64(1), uint64(0)
	slot := redisslot.HashSlot(PlayerNameKey(first))
	for id := uint64(2); id < 1_000_000; id++ {
		if redisslot.HashSlot(PlayerNameKey(id)) == slot {
			second = id
			break
		}
	}
	require.NotZero(t, second, "no second id on the same slot within the search range")
	require.NoError(t, cache.SetPlayerNames(ctx, map[uint64]string{first: "alpha", second: "beta"}, time.Hour))

	hits, absent, err := cache.MGetPlayerNames(ctx, []uint64{first, second})
	require.NoError(t, err)
	require.Equal(t, map[uint64]string{first: "alpha", second: "beta"}, hits)
	require.Empty(t, absent)
}

// 三态:正缓存、负缓存、未命中;0 与重复 id 跳过;负缓存不盖真名字(SETNX);DEL 之后回到未命中。
func TestMGetPlayerNamesThreeStates(t *testing.T) {
	cache, _ := newGuardedNameCache(t)
	ctx := context.Background()
	require.NoError(t, cache.SetPlayerNames(ctx, map[uint64]string{10: "named"}, time.Hour))
	require.NoError(t, cache.SetPlayerNamesAbsent(ctx, []uint64{10, 20}, time.Minute))

	hits, absent, err := cache.MGetPlayerNames(ctx, []uint64{0, 10, 10, 20, 30})
	require.NoError(t, err)
	require.Equal(t, map[uint64]string{10: "named"}, hits)
	require.Equal(t, map[uint64]bool{20: true}, absent)

	require.NoError(t, cache.DelPlayerName(ctx, 20))
	hits, absent, err = cache.MGetPlayerNames(ctx, []uint64{20})
	require.NoError(t, err)
	require.Empty(t, hits)
	require.Empty(t, absent)
}

// 缓存挂了:整批报错、不给部分结果,调用方降级成全部未命中。
func TestMGetPlayerNamesFailsTheWholeBatchWhenRedisIsDown(t *testing.T) {
	cache, mr := newGuardedNameCache(t)
	mr.Close()
	hits, absent, err := cache.MGetPlayerNames(context.Background(), []uint64{1, 2})
	require.Error(t, err)
	require.Nil(t, hits)
	require.Nil(t, absent)
}
