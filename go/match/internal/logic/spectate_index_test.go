package logic

// 全服观战索引拆桶(spectate_index.go)与观战列表 1 秒缓存(svc.SpectateLobbyCache)的 miniredis 测试:
// 跨桶合并排序截断、全服下标落桶、逐桶清理、拆桶后抢 L2 配额的并发退回、缓存 1 秒内不回源、同一周期只回源一次、
// 回源失败不缓存、摘掉的成员缓存同时忘掉。桶号映射与 slot 分布见 keys_test.go。时间一律用假时钟,不看墙钟。

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"match/internal/svc"

	matchpb "proto/match"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// spectateIndexSize 是一把索引全部桶的成员数之和。
func spectateIndexSize(t *testing.T, svcCtx *svc.ServiceContext, ix spectateIndex) int {
	t.Helper()
	counts, err := ix.counts(context.Background(), svcCtx.MatchRedis)
	require.NoError(t, err)
	return int(sumSpectateBuckets(counts))
}

// spectateIndexMembers 直接从 miniredis 收一把索引全部桶里的成员(按字典序),不经生产读路径。
func spectateIndexMembers(t *testing.T, mr *miniredis.Miniredis, ix spectateIndex) []string {
	t.Helper()
	members := []string{}
	for bucket := 0; bucket < spectateIndexBuckets; bucket++ {
		key := ix.bucketKey(bucket)
		if !mr.Exists(key) {
			continue
		}
		got, err := mr.ZMembers(key)
		require.NoError(t, err)
		members = append(members, got...)
	}
	sort.Strings(members)
	return members
}

func seedSpectateIndex(t *testing.T, mr *miniredis.Miniredis, ix spectateIndex, scores map[uint64]float64) {
	t.Helper()
	for battleId, score := range scores {
		_, err := mr.ZAdd(ix.keyFor(battleId), score, strconv.FormatUint(battleId, 10))
		require.NoError(t, err)
	}
}

func pairKeys(pairs []redis.Pair) []string {
	keys := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		keys = append(keys, pair.Key)
	}
	return keys
}

// ---- 跨桶读 ----

// 列表读跨桶合并:各桶取前 n,合并后按分数降序截断成 n,同分按成员倒序(与 ZREVRANGE 同序);按下限读时下限含等号。
// 前 n 名全在同一个桶里也取得全:每个桶取的是前 n,不是只取每桶第一名。
func TestSpectateIndexTopMergesBucketsAndTruncates(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	ctx := context.Background()
	// 0 号桶 1600、1616、1632;1 号桶 1601、1617(与 1616 同分);2 号桶 1602。
	seedSpectateIndex(t, mr, spectateActiveIndex, map[uint64]float64{1600: 100, 1616: 95, 1632: 60, 1601: 97, 1617: 95, 1602: 96})

	top4, err := spectateActiveIndex.top(ctx, svcCtx.MatchRedis, 4)
	require.NoError(t, err)
	require.Equal(t, []string{"1600", "1601", "1602", "1617"}, pairKeys(top4))
	require.Equal(t, int64(95), top4[3].Score)

	all, err := spectateActiveIndex.top(ctx, svcCtx.MatchRedis, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"1600", "1601", "1602", "1617", "1616", "1632"}, pairKeys(all))

	fresh, err := spectateActiveIndex.topFrom(ctx, svcCtx.MatchRedis, 96, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"1600", "1601", "1602"}, pairKeys(fresh))

	seedSpectateIndex(t, mr, spectateFeaturedIndex, map[uint64]float64{3200: 30, 3216: 20, 3232: 10, 3201: 5})
	featured, err := spectateFeaturedIndex.top(ctx, svcCtx.MatchRedis, 3)
	require.NoError(t, err)
	require.Equal(t, []string{"3200", "3216", "3232"}, pairKeys(featured), "前 3 名都在 0 号桶")
}

// 随机选场的全服下标按各桶数量的累计和落桶:0 号桶占 [0, c0),1 号桶接着排;空桶跳过,越界与负数都取不到。
func TestLocateSpectateIndexMapsGlobalIndexToBucket(t *testing.T) {
	counts := []int64{2, 0, 3, 1}
	for _, c := range []struct {
		index  int64
		bucket int
		local  int64
	}{
		{0, 0, 0}, {1, 0, 1}, {2, 2, 0}, {4, 2, 2}, {5, 3, 0}, {6, -1, 0}, {-1, -1, 0},
	} {
		bucket, local := locateSpectateIndex(counts, c.index)
		require.Equal(t, c.bucket, bucket, "index=%d", c.index)
		require.Equal(t, c.local, local, "index=%d", c.index)
	}
}

// 每个全服下标对应唯一一个成员、每个成员恰好被一个下标取到:全服均匀抽下标,每场被挑中的概率就均匀,与拆桶前一样。
func TestSpectateIndexAtVisitsEveryMemberOnce(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	ctx := context.Background()
	// 0 号桶 2 个、1 号桶 1 个、5 号桶 3 个、15 号桶 1 个,其余桶空。
	seedSpectateIndex(t, mr, spectateActiveIndex, map[uint64]float64{1600: 1, 1616: 2, 1601: 3, 1605: 4, 1621: 5, 1637: 6, 1615: 7})

	counts, err := spectateActiveIndex.counts(ctx, svcCtx.MatchRedis)
	require.NoError(t, err)
	total := sumSpectateBuckets(counts)
	require.Equal(t, int64(7), total)
	seen := map[string]int{}
	for index := int64(0); index < total; index++ {
		pairs, err := spectateActiveIndex.at(ctx, svcCtx.MatchRedis, counts, index)
		require.NoError(t, err)
		require.Len(t, pairs, 1, "index=%d", index)
		seen[pairs[0].Key]++
	}
	require.Len(t, seen, 7)
	for member, hits := range seen {
		require.Equal(t, 1, hits, member)
	}
	missing, err := spectateActiveIndex.at(ctx, svcCtx.MatchRedis, counts, total)
	require.NoError(t, err)
	require.Empty(t, missing, "越界取不到")
}

// 按分数清理每个桶都执行:分数不高于分界的成员全部摘掉并返回总数,分界之上的留着;坏成员 16 个桶都去找。
func TestSpectateIndexRemoveByScoreClearsEveryBucket(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	ctx := context.Background()
	// 1619 与 1603 同在 3 号桶:同一个桶里只摘分界以下的。
	seedSpectateIndex(t, mr, spectateFeaturedIndex, map[uint64]float64{1600: 10, 1601: 20, 1602: 30, 1603: 40, 1619: 15})
	_, err := mr.ZAdd(spectateFeaturedIndex.bucketKey(9), 99, "garbage")
	require.NoError(t, err)

	removed, err := spectateFeaturedIndex.removeByScore(ctx, svcCtx.MatchRedis, 20)
	require.NoError(t, err)
	require.Equal(t, int64(3), removed)
	require.Equal(t, []string{"1602", "1603", "garbage"}, spectateIndexMembers(t, mr, spectateFeaturedIndex))

	require.NoError(t, spectateFeaturedIndex.removeGarbage(ctx, svcCtx.MatchRedis, "garbage"))
	require.Equal(t, []string{"1602", "1603"}, spectateIndexMembers(t, mr, spectateFeaturedIndex))
}

// ---- L2 配额 ----

// 拆桶之后「数全服、没满就占」不是一步:另一场在别的桶同时占了名额(本场数过全服之后、本场的 Lua 之前写入),
// 本场复数超过 B 就退回、按满处理,全服留下的不超过 B。
func TestClaimBroadcastQuotaBacksOffWhenAnotherBucketClaimedConcurrently(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 1
	const mine, rival = uint64(883150), uint64(883151) // 14 号桶与 15 号桶
	ownKey := spectateBroadcastIndex.keyFor(mine)
	var injected atomic.Bool
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		// 只在顶层 EVAL 进来、脚本还没跑的时候写 miniredis。Lua 里的 redis.call 也经过钩子而且带着锁,那些命令只看谓词。
		if cmd == "EVAL" && len(args) >= 3 && args[2] == ownKey && injected.CompareAndSwap(false, true) {
			_, _ = mr.ZAdd(spectateBroadcastIndex.keyFor(rival), float64(testRelayDeadlineMs), strconv.FormatUint(rival, 10))
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	claim, err := claimBroadcastQuota(context.Background(), svcCtx, mine, testRelayDeadlineMs, nowMs())

	require.NoError(t, err)
	require.True(t, injected.Load(), "用例前提:对手赶在本场的 Lua 之前写进了别的桶")
	require.Equal(t, broadcastQuotaFull, claim)
	require.Equal(t, []string{strconv.FormatUint(rival, 10)}, spectateIndexMembers(t, mr, spectateBroadcastIndex), "超额的一方退回")
}

// ---- 列表缓存 ----

// 列表读经本副本的 1 秒缓存:1 秒内的请求不回源(期间新开的局还看不到),满 1 秒才重读。
func TestListWatchableBattlesServesCachedLobbyWithinOneSecond(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	clock := newFakeClock()
	svcCtx.SpectateLobby = svc.NewSpectateLobbyCache(clock.Now)
	base := nowMs()
	const older, newer = uint64(886001), uint64(886002)
	registerBattle(t, svcCtx, older, base-1000)
	ids, _ := listIds(t, svcCtx, 10)
	require.Equal(t, []uint64{older}, ids)

	registerBattle(t, svcCtx, newer, base)
	clock.Advance(svc.SpectateLobbyCacheTTL - time.Millisecond)
	ids, _ = listIds(t, svcCtx, 10)
	require.Equal(t, []uint64{older}, ids, "1 秒内不回源")

	clock.Advance(time.Millisecond)
	ids, _ = listIds(t, svcCtx, 10)
	require.Equal(t, []uint64{newer, older}, ids, "满 1 秒重读")
}

// 本副本摘掉的成员,缓存同时忘掉:同一秒里后来的列表请求不再读它的成局、不再摘第二次。
func TestListWatchableBattlesCacheForgetsEvictedMember(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	clock := newFakeClock()
	svcCtx.SpectateLobby = svc.NewSpectateLobbyCache(clock.Now)
	const live, orphan = uint64(886010), uint64(886011)
	registerBattle(t, svcCtx, live, nowMs()-1000)
	_, err := svcCtx.MatchRedis.Zadd(spectateActiveIndex.keyFor(orphan), int64(nowMs()), strconv.FormatUint(orphan, 10)) // 成局已不在
	require.NoError(t, err)
	stub := loadSpectateBattleFn
	reads := map[uint64]int{}
	loadSpectateBattleFn = func(ctx context.Context, s *svc.ServiceContext, battleId uint64) (*matchpb.SpectateBattleRecord, error) {
		reads[battleId]++
		return stub(ctx, s, battleId)
	}
	t.Cleanup(func() { loadSpectateBattleFn = stub })

	for i := 0; i < 2; i++ {
		ids, _ := listIds(t, svcCtx, 10)
		require.Equal(t, []uint64{live}, ids)
	}

	require.Equal(t, 1, reads[orphan], "摘掉之后缓存里也没有它")
	require.Equal(t, 2, reads[live])
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
}

// 同一周期里并发的请求共用一次回源:时钟不走,不论各请求怎么交错,fetch 只跑一次,大家拿到同一份结果。
func TestSpectateLobbyCacheSharesOneFetchAcrossConcurrentCallers(t *testing.T) {
	clock := newFakeClock()
	cache := svc.NewSpectateLobbyCache(clock.Now)
	var fetches atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	fetch := func() (svc.SpectateLobbySnapshot, error) {
		if fetches.Add(1) == 1 {
			close(started)
		}
		<-release
		return svc.SpectateLobbySnapshot{Active: []redis.Pair{{Key: "886020", Score: 1}}}, nil
	}
	const callers = 8
	type loaded struct {
		snapshot svc.SpectateLobbySnapshot
		err      error
	}
	results := make(chan loaded, callers)
	for i := 0; i < callers; i++ {
		go func() {
			snapshot, err := cache.Load(fetch)
			results <- loaded{snapshot: snapshot, err: err}
		}()
	}
	<-started
	close(release)
	for i := 0; i < callers; i++ {
		got := <-results
		require.NoError(t, got.err)
		require.Equal(t, []string{"886020"}, pairKeys(got.snapshot.Active))
	}
	require.EqualValues(t, 1, fetches.Load())
}

// 回源失败不缓存:下一个请求立刻重试,不会把一次 Redis 故障钉成 1 秒的错误。
func TestSpectateLobbyCacheRetriesAfterFetchError(t *testing.T) {
	cache := svc.NewSpectateLobbyCache(newFakeClock().Now)
	down := errors.New("redis down")
	calls := 0
	_, err := cache.Load(func() (svc.SpectateLobbySnapshot, error) {
		calls++
		return svc.SpectateLobbySnapshot{}, down
	})
	require.ErrorIs(t, err, down)

	snapshot, err := cache.Load(func() (svc.SpectateLobbySnapshot, error) {
		calls++
		return svc.SpectateLobbySnapshot{Active: []redis.Pair{{Key: "886030"}}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, []string{"886030"}, pairKeys(snapshot.Active))
}

// Forget 换新切片:之后的请求看不到这个成员,已经交出去的那份不变。
func TestSpectateLobbyCacheForgetKeepsHandedOutSnapshot(t *testing.T) {
	cache := svc.NewSpectateLobbyCache(newFakeClock().Now)
	first, err := cache.Load(func() (svc.SpectateLobbySnapshot, error) {
		return svc.SpectateLobbySnapshot{
			Featured: []redis.Pair{{Key: "886040"}},
			Active:   []redis.Pair{{Key: "886040"}, {Key: "886041"}},
		}, nil
	})
	require.NoError(t, err)

	cache.Forget("886040")
	second, err := cache.Load(func() (svc.SpectateLobbySnapshot, error) {
		return svc.SpectateLobbySnapshot{}, errors.New("cache still fresh; must not refetch")
	})

	require.NoError(t, err)
	require.Empty(t, second.Featured)
	require.Equal(t, []string{"886041"}, pairKeys(second.Active))
	require.Equal(t, []string{"886040", "886041"}, pairKeys(first.Active))
}
