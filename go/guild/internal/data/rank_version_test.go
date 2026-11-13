package data

// 带版本的写榜(guild_repo.go「写榜协议」)。只连 miniredis,不连库:MySQL 那一半(写分事务里版本 +1、重建读快照、
// 复核回库)由 rank_zone_integration_test.go(integration 标签)覆盖;这里把快照与复核来源直接作为 reconcileRanks 的参数给进去。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publishVersioned 走写分路径的写榜半段(publishRank):zone 是原始区,按合服映射解析到最终区。
func publishVersioned(t *testing.T, repo *GuildRepo, zone uint32, guildID uint64, score int64, version uint64) {
	t.Helper()
	require.NoError(t, repo.publishRank(context.Background(), guildID, zone, score, version))
}

// seedVersioned 用同一个条件写入脚本直接写进 zone 这张榜,不解析合服映射:用来摆出源区旧键上的条目。
func seedVersioned(t *testing.T, repo *GuildRepo, zone uint32, guildID uint64, score int64, version uint64) {
	t.Helper()
	item, err := newRankItem(guildID, score, version)
	require.NoError(t, err)
	require.NoError(t, rankUpsertScript.Run(context.Background(), repo.rdb, rankZoneKeys(zone), item.upsertArgs()...).Err())
}

// buryInZone 只在 zone 这一个区给帮会立墓碑(并摘掉它在这个区的榜项)。
func buryInZone(t *testing.T, repo *GuildRepo, zone uint32, guildID uint64) {
	t.Helper()
	require.NoError(t, rankBuryScript.Run(context.Background(), repo.rdb, rankZoneKeys(zone), guildID, rankTombstoneTTL.Milliseconds()).Err())
}

// buried 返回 zone 墓碑里的帮会(不论是否到期)。
func buried(t *testing.T, repo *GuildRepo, zone uint32) []string {
	t.Helper()
	members, err := repo.rdb.ZRange(context.Background(), zoneRankTombstoneKey(zone), 0, -1).Result()
	require.NoError(t, err)
	return members
}

// rankedScore 返回帮会在 zone 榜上的分数与版本,不在榜上时 ok = false。
// 同时核对两个键一起变:榜上有的项必须记着版本,榜上没有的项版本 HASH 里也不能有。
func rankedScore(t *testing.T, repo *GuildRepo, zone uint32, guildID uint64) (score float64, version string, ok bool) {
	t.Helper()
	ctx := context.Background()
	member := fmt.Sprint(guildID)
	score, err := repo.rdb.ZScore(ctx, zoneRankKey(zone), member).Result()
	if err == redis.Nil {
		recorded, err := repo.rdb.HExists(ctx, zoneRankVersionKey(zone), member).Result()
		require.NoError(t, err)
		require.False(t, recorded, "guild %d 不在 zone %d 的榜上,版本 HASH 里也不该有它", guildID, zone)
		return 0, "", false
	}
	require.NoError(t, err)
	version, err = repo.rdb.HGet(ctx, zoneRankVersionKey(zone), member).Result()
	require.NoError(t, err, "guild %d 在 zone %d 的榜上,必须记着版本", guildID, zone)
	return score, version, true
}

// staticRankLookup 是 reconcileRanks 的复核来源替身:rows 就是「回库读到的当前权威」,calls 记下被复核过的帮会。
type staticRankLookup struct {
	rows  map[uint64]rankRow
	calls []uint64
}

func (s *staticRankLookup) lookup(_ context.Context, guildIDs []uint64) (map[uint64]rankRow, error) {
	s.calls = append(s.calls, guildIDs...)
	found := make(map[uint64]rankRow)
	for _, id := range guildIDs {
		if row, ok := s.rows[id]; ok {
			found[id] = row
		}
	}
	return found, nil
}

// ── 写分 ─────────────────────────────────────────────────────

// 乱序到达:版本 2 先写进榜,提交在前的版本 1 后到,被丢弃;更新的版本照常写入。
func TestPublishRank_OlderVersionArrivingLateIsRejected(t *testing.T) {
	repo, _ := newRankRepo(t)
	publishVersioned(t, repo, 3, 301, 50, 2)
	publishVersioned(t, repo, 3, 301, 10, 1)

	score, version, ok := rankedScore(t, repo, 3, 301)
	require.True(t, ok)
	assert.Equal(t, float64(50), score, "旧版本迟到不得覆盖新版本")
	assert.Equal(t, "2", version)

	publishVersioned(t, repo, 3, 301, 70, 3)
	score, version, _ = rankedScore(t, repo, 3, 301)
	assert.Equal(t, float64(70), score)
	assert.Equal(t, "3", version)
}

// 同一版本只对应一个已提交的分数:重写是幂等的,还能修好被手工改过的分数。
func TestPublishRank_SameVersionRewritesTheScore(t *testing.T) {
	repo, _ := newRankRepo(t)
	publishVersioned(t, repo, 3, 301, 50, 2)
	zadd(t, repo, 3, 301, 999) // 有人手工改了榜上的分数,版本没动

	publishVersioned(t, repo, 3, 301, 50, 2)

	score, _, _ := rankedScore(t, repo, 3, 301)
	assert.Equal(t, float64(50), score)
}

// 版本超出脚本能精确比较的范围就拒绝写,不冒险比错。
func TestPublishRank_RefusesVersionsBeyondTheExactRange(t *testing.T) {
	repo, _ := newRankRepo(t)
	require.Error(t, repo.publishRank(context.Background(), 301, 3, 10, maxRankVersion+1))
	_, _, ok := rankedScore(t, repo, 3, 301)
	assert.False(t, ok)
}

// 写分、建帮上榜、解散摘榜都不拿榜维护锁:别的副本正持着锁重建或折叠,也不等它。
// 改之前写分要等这把锁(最多 5 秒),这里的 200ms 预算一定会超时。
func TestRankWritesDoNotWaitForTheMaintenanceLock(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.rdb.Set(ctx, guildRankLockKey(3), "rebuild-elsewhere", time.Minute).Err())

	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	require.NoError(t, repo.AddNewGuildToRank(short, 301, 3))
	require.NoError(t, repo.publishRank(short, 301, 3, 40, 1))
	require.NoError(t, repo.publishRank(short, 302, 3, 20, 1))
	require.NoError(t, repo.removeFromAllRanks(short, 302, 3, true, []uint32{3}))

	score, _, ok := rankedScore(t, repo, 3, 301)
	require.True(t, ok)
	assert.Equal(t, float64(40), score)
	_, _, ok = rankedScore(t, repo, 3, 302)
	assert.False(t, ok)
	assert.Equal(t, "rebuild-elsewhere", repo.rdb.Get(ctx, guildRankLockKey(3)).Val(), "别人的锁原样留着")
}

// 建帮上榜晚到:初始版本 0 输给已经写进来的推分。
func TestAddNewGuildToRank_ArrivingAfterAScoreUpdateIsIgnored(t *testing.T) {
	repo, _ := newRankRepo(t)
	publishVersioned(t, repo, 3, 301, 40, 1)

	require.NoError(t, repo.AddNewGuildToRank(context.Background(), 301, 3))

	score, version, ok := rankedScore(t, repo, 3, 301)
	require.True(t, ok)
	assert.Equal(t, float64(40), score)
	assert.Equal(t, "1", version)
}

// 写分按最终区进榜:合服之后源区帮会的分数写进目标区。
func TestPublishRank_WritesTheFinalZone(t *testing.T) {
	repo, _ := newRankRepo(t)
	bindMergedZones(t, repo, map[uint32]uint32{10: 20})

	publishVersioned(t, repo, 10, 1001, 30, 1)

	_, _, ok := rankedScore(t, repo, 20, 1001)
	assert.True(t, ok)
	_, _, ok = rankedScore(t, repo, 10, 1001)
	assert.False(t, ok)
}

// ── 解散 ─────────────────────────────────────────────────────

// 解散后迟到的写入不复活:解散前已提交、还没写榜的推分在摘榜之后才到,被墓碑拒绝;建帮上榜晚到同理。
func TestRemoveDisbandedGuild_LateWritesDoNotResurrectIt(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	publishVersioned(t, repo, 3, 301, 40, 1)

	require.NoError(t, repo.removeFromAllRanks(ctx, 301, 3, true, []uint32{3}))
	_, _, ok := rankedScore(t, repo, 3, 301)
	require.False(t, ok)

	publishVersioned(t, repo, 3, 301, 90, 2) // 解散前提交的推分,这时才到
	require.NoError(t, repo.AddNewGuildToRank(ctx, 301, 3))
	_, _, ok = rankedScore(t, repo, 3, 301)
	assert.False(t, ok, "已解散的帮会不能被迟到的写入写回榜上")
	assert.Equal(t, []string{"301"}, buried(t, repo, 3))
}

// 墓碑的清理不要任务:墓碑键带 TTL(一个区一天没人解散就整个过期);到期的墓碑不再拦写入,下一次立墓碑时顺手删掉。
// 墓碑的到期按 Redis 服务器时间算,这里用 miniredis 的 SetTime 拨钟。
func TestRankTombstonesExpireWithoutACleanupJob(t *testing.T) {
	repo, mr := newRankRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.removeFromAllRanks(ctx, 301, 3, true, []uint32{3}))
	assert.Equal(t, rankTombstoneTTL, mr.TTL(zoneRankTombstoneKey(3)))

	mr.SetTime(time.Now().Add(rankTombstoneTTL + time.Minute))
	publishVersioned(t, repo, 3, 301, 95, 3)
	_, _, ok := rankedScore(t, repo, 3, 301)
	assert.True(t, ok, "过了保留期的墓碑不再拦写入(那时已不可能还有迟到的写入)")

	require.NoError(t, repo.removeFromAllRanks(ctx, 302, 3, true, []uint32{3}))
	assert.Equal(t, []string{"302"}, buried(t, repo, 3), "立新墓碑时删掉本区已到期的")

	mr.FastForward(rankTombstoneTTL + time.Second)
	assert.False(t, mr.Exists(zoneRankTombstoneKey(3)), "一个区一天没人解散,墓碑键自己过期")
}

// 墓碑立在帮会的整个合服组:映射缓存还没刷新的副本把迟到的推分写进原始区,刷新了的写进最终区,两边都被拦住。
func TestRemoveDisbandedGuild_BuriesEveryZoneOfItsMergeGroup(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{10: 20})
	publishVersioned(t, repo, 10, 1001, 40, 1) // 原始区 10,进最终区 20 的榜

	require.NoError(t, repo.removeFromAllRanks(ctx, 1001, 10, true, []uint32{10}))

	assert.Equal(t, []string{"1001"}, buried(t, repo, 10))
	assert.Equal(t, []string{"1001"}, buried(t, repo, 20))
	seedVersioned(t, repo, 10, 1001, 90, 2) // 还不知道合服的副本,写进原始区
	publishVersioned(t, repo, 10, 1001, 90, 2)
	_, _, ok := rankedScore(t, repo, 10, 1001)
	assert.False(t, ok)
	_, _, ok = rankedScore(t, repo, 20, 1001)
	assert.False(t, ok)
}

// 摘榜走 zone 清单里的每一张分区榜(guild 表出现过的区,从 SQL 推出,不扫 Redis):合服折叠之前,条目可能还在源区的旧键上。
// 别的帮会不动。
func TestRemoveDisbandedGuild_SweepsEveryZoneRank(t *testing.T) {
	repo, _ := newRankRepo(t)
	seedVersioned(t, repo, 3, 301, 40, 1)
	seedVersioned(t, repo, 7, 301, 30, 1)
	seedVersioned(t, repo, 7, 701, 30, 1)

	require.NoError(t, repo.removeFromAllRanks(context.Background(), 301, 3, true, []uint32{3, 7}))

	_, _, ok := rankedScore(t, repo, 3, 301)
	assert.False(t, ok)
	_, _, ok = rankedScore(t, repo, 7, 301)
	assert.False(t, ok)
	_, _, ok = rankedScore(t, repo, 7, 701)
	assert.True(t, ok)
}

// 帮会还活着(降级 / 手工清榜)只摘不立墓碑:之后的推分照常让它回到榜上。
func TestRemoveLiveGuild_LeavesNoTombstone(t *testing.T) {
	repo, mr := newRankRepo(t)
	publishVersioned(t, repo, 3, 301, 40, 1)

	require.NoError(t, repo.removeFromAllRanks(context.Background(), 301, 3, false, []uint32{3}))

	assert.False(t, mr.Exists(zoneRankTombstoneKey(3)))
	publishVersioned(t, repo, 3, 301, 50, 2)
	score, _, ok := rankedScore(t, repo, 3, 301)
	require.True(t, ok)
	assert.Equal(t, float64(50), score)
}

// ── 重建 ─────────────────────────────────────────────────────

// 重建不覆盖并发的新写入:快照读完之后又提交、先写进榜的分数(版本更新)不会被快照里的旧分盖回去。
// 改之前重建建临时键再整键 RENAME,这一项会回到快照里的 10。
func TestReconcileRanks_DoesNotOverwriteAWriteMadeAfterTheSnapshot(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	snapshot := []rankRow{{guildID: 301, zoneID: 3, score: 10, version: 1}}
	publishVersioned(t, repo, 3, 301, 99, 2) // 快照之后提交的推分,在合并之前到了 Redis

	lookup := &staticRankLookup{}
	require.NoError(t, repo.reconcileRanks(ctx, snapshot, lookup.lookup))

	score, version, ok := rankedScore(t, repo, 3, 301)
	require.True(t, ok)
	assert.Equal(t, float64(99), score)
	assert.Equal(t, "2", version)
	assert.Empty(t, lookup.calls, "在快照里、又在它该在的区的条目不用回库复核")

	// 另一种先后:合并在前,推分后到,照常写入。
	publishVersioned(t, repo, 3, 301, 120, 3)
	score, _, _ = rankedScore(t, repo, 3, 301)
	assert.Equal(t, float64(120), score)
}

// 快照之后才建的帮会不被重建删除:它不在快照里,删之前回库复核;库里有、又属于这个区,就留着。
func TestReconcileRanks_KeepsAGuildCreatedAfterTheSnapshot(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	snapshot := []rankRow{{guildID: 301, zoneID: 3, score: 10, version: 1}}
	require.NoError(t, repo.AddNewGuildToRank(ctx, 302, 3)) // 快照之后才建、已上榜

	lookup := &staticRankLookup{rows: map[uint64]rankRow{302: {guildID: 302, zoneID: 3, score: 0, version: 0}}}
	require.NoError(t, repo.reconcileRanks(ctx, snapshot, lookup.lookup))

	_, _, ok := rankedScore(t, repo, 3, 302)
	assert.True(t, ok, "快照之后建的帮会不能被重建删掉")
	assert.Equal(t, []uint64{302}, lookup.calls, "不在快照里的条目先回库复核再处理")
	_, _, ok = rankedScore(t, repo, 3, 301)
	assert.True(t, ok)
}

// 快照之后才解散的帮会:快照里还有它,但墓碑已立,合并时不写回去。
func TestReconcileRanks_DoesNotResurrectAGuildDisbandedAfterTheSnapshot(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	seedVersioned(t, repo, 3, 301, 40, 1)
	snapshot := []rankRow{{guildID: 301, zoneID: 3, score: 40, version: 1}}
	require.NoError(t, repo.removeFromAllRanks(ctx, 301, 3, true, []uint32{3}))

	require.NoError(t, repo.reconcileRanks(ctx, snapshot, (&staticRankLookup{}).lookup))

	_, _, ok := rankedScore(t, repo, 3, 301)
	assert.False(t, ok)
}

// 解散后没摘干净的幽灵(含本次改动之前写的、没记版本的项):不在快照里,回库也查不到,摘掉并立墓碑,
// 之后迟到的写入也写不回来。区 3 还有活帮会 309,所以在 zone 清单里(清单从快照推出,不扫 Redis)。
func TestReconcileRanks_RemovesGhostsOfDisbandedGuilds(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	seedVersioned(t, repo, 3, 303, 70, 4)
	zadd(t, repo, 3, 304, 5)
	snapshot := []rankRow{{guildID: 309, zoneID: 3, score: 1, version: 1}}

	lookup := &staticRankLookup{}
	require.NoError(t, repo.reconcileRanks(ctx, snapshot, lookup.lookup))

	for _, id := range []uint64{303, 304} {
		_, _, ok := rankedScore(t, repo, 3, id)
		assert.False(t, ok, "guild %d", id)
	}
	assert.ElementsMatch(t, []string{"303", "304"}, buried(t, repo, 3), "幽灵要立墓碑")
	assert.ElementsMatch(t, []uint64{303, 304}, lookup.calls)
	publishVersioned(t, repo, 3, 303, 80, 5)
	_, _, ok := rankedScore(t, repo, 3, 303)
	assert.False(t, ok)
	assert.Equal(t, []string{"309"}, repo.rdb.ZRange(ctx, zoneRankKey(3), 0, -1).Val(), "榜上只剩活帮会")
}

// zone 清单从 SQL(快照)推出,不扫 Redis:guild 表里一个帮会都没有的区不会被访问,那里的幽灵留着,
// 等那个区再有帮会时由下一次重建摘掉(guild_repo.go rankZones)。这是不扫 Redis 换来的已知残余,只影响那张空榜的显示。
func TestReconcileRanks_ZoneListComesFromSQLNotRedis(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	zadd(t, repo, 8, 801, 5)

	lookup := &staticRankLookup{}
	require.NoError(t, repo.reconcileRanks(ctx, nil, lookup.lookup))

	assert.Empty(t, lookup.calls, "没有帮会的区不在清单里,不复核")
	assert.Equal(t, float64(5), zscore(t, repo, 8, 801))
}

// 放错区的条目(合服后源区还没折叠的旧键、撤销合服后留在目标区的项):权威分并进它该在的区,再按版本从这里摘。
func TestReconcileRanks_MovesMisplacedEntriesToTheirFinalZone(t *testing.T) {
	repo, mr := newRankRepo(t)
	ctx := context.Background()
	bindMergedZones(t, repo, map[uint32]uint32{10: 20})
	seedVersioned(t, repo, 10, 1001, 5, 1) // 源区旧键上的旧分
	row := rankRow{guildID: 1001, zoneID: 10, score: 7, version: 2}

	require.NoError(t, repo.reconcileRanks(ctx, []rankRow{row}, (&staticRankLookup{rows: map[uint64]rankRow{1001: row}}).lookup))

	score, version, ok := rankedScore(t, repo, 20, 1001)
	require.True(t, ok)
	assert.Equal(t, float64(7), score)
	assert.Equal(t, "2", version)
	_, _, ok = rankedScore(t, repo, 10, 1001)
	assert.False(t, ok, "源区旧键上的项要摘掉")
	assert.False(t, mr.Exists(zoneRankKey(10)))
}

// 修好缺项和旧分:快照比榜新就写进去,榜上缺的补上;zone_id=0 的坏行不进任何榜。
func TestReconcileRanks_RepairsMissingAndStaleEntries(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	seedVersioned(t, repo, 3, 301, 5, 1)
	snapshot := []rankRow{
		{guildID: 301, zoneID: 3, score: 50, version: 3},
		{guildID: 302, zoneID: 3, score: 20, version: 0},
		{guildID: 303, zoneID: 0, score: 99, version: 1},
	}

	require.NoError(t, repo.reconcileRanks(ctx, snapshot, (&staticRankLookup{}).lookup))

	score, version, _ := rankedScore(t, repo, 3, 301)
	assert.Equal(t, float64(50), score)
	assert.Equal(t, "3", version)
	score, _, ok := rankedScore(t, repo, 3, 302)
	require.True(t, ok)
	assert.Equal(t, float64(20), score)
	assert.Equal(t, []string{zoneRankKey(3)}, repo.rdb.Keys(ctx, zoneRankKeyPrefix+"*").Val(), "zone_id=0 的帮会不进任何榜")
}

// 手工写进来的、不是 guild_id 的成员,重建时摘掉。区 3 有活帮会 309,在 zone 清单里。
func TestReconcileRanks_RemovesGarbageMembers(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.rdb.ZAdd(ctx, zoneRankKey(3), redis.Z{Score: 1, Member: "not-a-guild"}, redis.Z{Score: 2, Member: "0042"}).Err())
	snapshot := []rankRow{{guildID: 309, zoneID: 3, score: 1, version: 1}}

	require.NoError(t, repo.reconcileRanks(ctx, snapshot, (&staticRankLookup{}).lookup))

	assert.Equal(t, []string{"309"}, repo.rdb.ZRange(ctx, zoneRankKey(3), 0, -1).Val())
}
