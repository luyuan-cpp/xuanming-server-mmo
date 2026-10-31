package data

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shared/placement"
)

// bindMergedZones 给 repo 接一份固定的合服映射。用真缓存,不写替身:不 Start 就不起 goroutine。
func bindMergedZones(t *testing.T, repo *GuildRepo, merges map[uint32]uint32) {
	t.Helper()
	cache, err := placement.NewMergedZoneCache(context.Background(), func(context.Context) (map[uint32]uint32, error) {
		return merges, nil
	}, placement.MergedZoneCacheOptions{})
	require.NoError(t, err)
	repo.BindMergedZones(cache)
}

func newRankRepo(t *testing.T) (*GuildRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewGuildRepo(rdb, nil, time.Minute), mr
}

func zadd(t *testing.T, repo *GuildRepo, zone uint32, guildID uint64, score float64) {
	t.Helper()
	require.NoError(t, repo.rdb.ZAdd(context.Background(), zoneRankKey(zone), redis.Z{Score: score, Member: guildID}).Err())
}

func zscore(t *testing.T, repo *GuildRepo, zone uint32, guildID uint64) float64 {
	t.Helper()
	score, err := repo.rdb.ZScore(context.Background(), zoneRankKey(zone), fmt.Sprint(guildID)).Result()
	require.NoError(t, err, "guild %d 应该在 zone %d 的榜上", guildID, zone)
	return score
}

func TestZoneHelpersResolveMergedZones(t *testing.T) {
	repo, _ := newRankRepo(t)

	// 没绑缓存等于没有合服。
	assert.Equal(t, uint32(5), repo.FinalZone(5))
	assert.True(t, repo.SameZone(5, 5))
	assert.False(t, repo.SameZone(5, 6))
	assert.Equal(t, []uint32{5}, repo.ZoneMembers(5))

	bindMergedZones(t, repo, map[uint32]uint32{4: 5, 5: 6})
	assert.Equal(t, uint32(6), repo.FinalZone(4), "沿链解析到底")
	assert.Equal(t, uint32(6), repo.FinalZone(6))
	assert.Equal(t, uint32(7), repo.FinalZone(7))
	assert.Equal(t, uint32(0), repo.FinalZone(0), "0 表示不按区,不解析")
	assert.True(t, repo.SameZone(4, 6))
	assert.True(t, repo.SameZone(4, 5))
	assert.False(t, repo.SameZone(4, 7))
	assert.Equal(t, []uint32{4, 5, 6}, repo.ZoneMembers(4))
	assert.Equal(t, []uint32{4, 5, 6}, repo.ZoneMembers(6))
	assert.Equal(t, []uint32{7}, repo.ZoneMembers(7))
}

func TestZoneHelpersOnNilRepoMeanNoMerges(t *testing.T) {
	var repo *GuildRepo
	assert.Equal(t, uint32(5), repo.FinalZone(5))
	assert.True(t, repo.SameZone(5, 5))
}

func TestFoldMergedZoneRanks_MovesOldEntriesAndDeletesTheOldKey(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	zadd(t, repo, 5, 501, 10)
	zadd(t, repo, 5, 502, 20)
	zadd(t, repo, 6, 601, 30)

	folded, err := repo.FoldMergedZoneRanks(ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, folded)
	assert.EqualValues(t, 0, repo.rdb.Exists(ctx, zoneRankKey(5)).Val(), "旧键要删掉")
	assert.Equal(t, float64(10), zscore(t, repo, 6, 501))
	assert.Equal(t, float64(20), zscore(t, repo, 6, 502))
	assert.Equal(t, float64(30), zscore(t, repo, 6, 601), "目标区原有的条目不动")
	assert.EqualValues(t, 0, repo.rdb.Exists(ctx, guildRankLockKey).Val(), "维护锁要释放")
}

// 合服之后的新分数已经写进最终区的榜:旧榜里同一个公会的旧分数不能盖掉它。
func TestFoldMergedZoneRanks_NewScoreIsNotOverwrittenByOldScore(t *testing.T) {
	repo, _ := newRankRepo(t)
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	zadd(t, repo, 5, 501, 10)
	zadd(t, repo, 6, 501, 999)

	_, err := repo.FoldMergedZoneRanks(context.Background())

	require.NoError(t, err)
	assert.Equal(t, float64(999), zscore(t, repo, 6, 501))
}

// 连续合服:4→5→6 的两张旧榜都直接并进最终区 6,不经过中间的 5。
func TestFoldMergedZoneRanks_ChainFoldsIntoTheFinalZone(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{4: 5, 5: 6})
	zadd(t, repo, 4, 401, 1)
	zadd(t, repo, 5, 501, 2)

	folded, err := repo.FoldMergedZoneRanks(ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, folded)
	assert.Equal(t, float64(1), zscore(t, repo, 6, 401))
	assert.Equal(t, float64(2), zscore(t, repo, 6, 501))
	assert.EqualValues(t, 0, repo.rdb.Exists(ctx, zoneRankKey(4), zoneRankKey(5)).Val())
}

// 幂等:再跑一轮什么都不做;旧副本后来又写回旧键的条目,下一轮接着并。
func TestFoldMergedZoneRanks_IsIdempotentAndPicksUpLateWrites(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	zadd(t, repo, 5, 501, 10)
	_, err := repo.FoldMergedZoneRanks(ctx)
	require.NoError(t, err)

	folded, err := repo.FoldMergedZoneRanks(ctx)
	require.NoError(t, err)
	assert.Zero(t, folded)
	assert.Equal(t, float64(10), zscore(t, repo, 6, 501))

	zadd(t, repo, 5, 502, 20)
	folded, err = repo.FoldMergedZoneRanks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, folded)
	assert.Equal(t, float64(20), zscore(t, repo, 6, 502))
}

// 榜很长时也要并完才删旧键。
func TestFoldMergedZoneRanks_LargeRank(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	const members = 2*rankFoldPageSize + 7
	z := make([]redis.Z, 0, members)
	for i := range members {
		z = append(z, redis.Z{Score: float64(i), Member: 100000 + i})
	}
	require.NoError(t, repo.rdb.ZAdd(ctx, zoneRankKey(5), z...).Err())

	folded, err := repo.FoldMergedZoneRanks(ctx)

	require.NoError(t, err)
	assert.Equal(t, members, folded)
	assert.EqualValues(t, members, repo.rdb.ZCard(ctx, zoneRankKey(6)).Val())
	assert.EqualValues(t, 0, repo.rdb.Exists(ctx, zoneRankKey(5)).Val())
}

// 稳态:没有旧键时不碰维护锁。别人正持着锁也不等,直接返回。
func TestFoldMergedZoneRanks_SteadyStateTakesNoLock(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	require.NoError(t, repo.rdb.Set(ctx, guildRankLockKey, "someone-else", time.Minute).Err())

	done, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	folded, err := repo.FoldMergedZoneRanks(done)

	require.NoError(t, err)
	assert.Zero(t, folded)
}

// 没绑映射(单测、工具)不碰 Redis:rdb 为 nil 也不 panic。
func TestFoldMergedZoneRanks_NoMergesTouchesNothing(t *testing.T) {
	repo := NewGuildRepo(nil, nil, time.Minute)
	folded, err := repo.FoldMergedZoneRanks(context.Background())
	require.NoError(t, err)
	assert.Zero(t, folded)
}

// 抢不到维护锁:这一轮报错,旧榜原样留着,下一轮再来。锁只防并发,不是错误状态。
func TestFoldMergedZoneRanks_LockBusyLeavesOldRankIntact(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	zadd(t, repo, 5, 501, 10)
	require.NoError(t, repo.rdb.Set(ctx, guildRankLockKey, "someone-else", time.Minute).Err())

	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := repo.FoldMergedZoneRanks(short)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "err = %v", err)
	assert.Equal(t, float64(10), zscore(t, repo, 5, 501))
	assert.Equal(t, "someone-else", repo.rdb.Get(ctx, guildRankLockKey).Val(), "别人的锁不能动")
}

// Redis 挂了:这一轮报错,不当成没有旧榜。
func TestFoldMergedZoneRanks_RedisFailureIsReported(t *testing.T) {
	repo, mr := newRankRepo(t)
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	mr.Close()
	_, err := repo.FoldMergedZoneRanks(context.Background())
	require.Error(t, err)
}

// 读榜按最终区:合服之后问源区、问目标区都是同一张榜。
func TestRankReadsResolveToTheFinalZone(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	zadd(t, repo, 6, 601, 30)

	for _, zone := range []uint32{5, 6} {
		entries, total, err := repo.GetGuildRankPage(ctx, zone, 1, 10)
		require.NoError(t, err)
		assert.EqualValues(t, 1, total, "zone=%d", zone)
		require.Len(t, entries, 1, "zone=%d", zone)
		assert.Equal(t, uint64(601), entries[0].GuildID)

		entry, err := repo.GetGuildRank(ctx, 601, zone)
		require.NoError(t, err)
		assert.EqualValues(t, 1, entry.Rank, "zone=%d", zone)
	}
}
