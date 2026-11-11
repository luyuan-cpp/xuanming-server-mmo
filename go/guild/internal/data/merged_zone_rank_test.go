package data

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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

// 折叠按版本合并:合服之后写进最终区的分数版本更新,旧榜里同一个公会的旧分数不能盖掉它。
func TestFoldMergedZoneRanks_NewerVersionInTheFinalZoneIsKept(t *testing.T) {
	repo, mr := newRankRepo(t)
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	seedVersioned(t, repo, 5, 501, 10, 1)
	seedVersioned(t, repo, 6, 501, 999, 2)

	folded, err := repo.FoldMergedZoneRanks(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, folded, "旧项比目标区旧,照样从源区摘掉")
	score, version, _ := rankedScore(t, repo, 6, 501)
	assert.Equal(t, float64(999), score)
	assert.Equal(t, "2", version)
	assert.False(t, mr.Exists(zoneRankKey(5)))
	assert.False(t, mr.Exists(zoneRankVersionKey(5)))
}

// 反过来,源区的版本更新(映射缓存还没刷新的副本合服之后又往源区写过)就盖掉目标区的旧分。
// 改之前用 ZADD NX,目标区已有这个公会就不动,这里会留下更旧的 20。
func TestFoldMergedZoneRanks_NewerVersionInTheSourceZoneWins(t *testing.T) {
	repo, _ := newRankRepo(t)
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	seedVersioned(t, repo, 6, 501, 20, 2)
	seedVersioned(t, repo, 5, 501, 50, 3)

	_, err := repo.FoldMergedZoneRanks(context.Background())

	require.NoError(t, err)
	score, version, _ := rankedScore(t, repo, 6, 501)
	assert.Equal(t, float64(50), score)
	assert.Equal(t, "3", version)
}

// 已解散的帮会:源区旧键上还留着它的项(摘榜失败过),折叠时被墓碑挡住,不并进目标区,源区的项照样摘掉。
func TestFoldMergedZoneRanks_DoesNotResurrectADisbandedGuild(t *testing.T) {
	repo, mr := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	seedVersioned(t, repo, 5, 501, 10, 1)
	buryInZone(t, repo, 6, 501) // 解散时墓碑立在合服组,目标区有

	_, err := repo.FoldMergedZoneRanks(ctx)

	require.NoError(t, err)
	_, _, ok := rankedScore(t, repo, 6, 501)
	assert.False(t, ok)
	assert.False(t, mr.Exists(zoneRankKey(5)))
}

// afterHeadReadHook 在被测客户端第一次成功读完源区榜头之后跑一次 inject。
type afterHeadReadHook struct {
	srcKey string
	once   sync.Once
	fired  bool
	inject func()
}

func (h *afterHeadReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterHeadReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *afterHeadReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		// 折叠里单条发出、带源区键的脚本只有读榜头这一个;合并与摘除都在 pipeline 里。
		if err == nil && (cmd.Name() == "eval" || cmd.Name() == "evalsha") && slices.Contains(cmd.Args(), any(h.srcKey)) {
			h.once.Do(func() {
				h.fired = true
				h.inject()
			})
		}
		return err
	}
}

// 折叠读完一页之后,映射缓存还没刷新的副本又往源区写了更新的版本:按版本摘不掉它,它留在源区,随后再并,不丢。
// 改之前整页并完就整键 DEL 源区,这次写入会跟着旧键一起被删掉,目标区留下的是旧分 10。
func TestFoldMergedZoneRanks_KeepsAnEntryRewrittenDuringTheFold(t *testing.T) {
	mr := miniredis.RunT(t)
	reader := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	writer := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})
	repo := NewGuildRepo(reader, nil, time.Minute)
	other := NewGuildRepo(writer, nil, time.Minute)
	bindMergedZones(t, repo, map[uint32]uint32{5: 6})
	seedVersioned(t, other, 5, 501, 10, 1)
	seedVersioned(t, other, 5, 502, 20, 1)
	hook := &afterHeadReadHook{srcKey: zoneRankKey(5), inject: func() { seedVersioned(t, other, 5, 501, 77, 2) }}
	reader.AddHook(hook)

	folded, err := repo.FoldMergedZoneRanks(context.Background())

	require.NoError(t, err)
	require.True(t, hook.fired)
	assert.Equal(t, 2, folded)
	score, version, ok := rankedScore(t, other, 6, 501)
	require.True(t, ok)
	assert.Equal(t, float64(77), score, "读完之后写进源区的新版本最终并进目标区")
	assert.Equal(t, "2", version)
	score, _, _ = rankedScore(t, other, 6, 502)
	assert.Equal(t, float64(20), score)
	assert.False(t, mr.Exists(zoneRankKey(5)))
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
	const members = 2*rankBatchSize + 7
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
