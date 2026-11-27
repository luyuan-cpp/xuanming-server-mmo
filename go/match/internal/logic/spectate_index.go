package logic

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"match/internal/svc"

	redv9 "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// 全服观战索引拆桶(docs/design/redis-cluster-all-domains-20261110.md §4.9)。观战大厅有三把全服 ZSET:活跃集合、L2 配额、
// 赛事局索引。原来各是一个键,在 match 私有集群上各是一个单键热点,加主节点也分不开。现在每把按 battle_id % 16 拆成 16 个 ZSET,
// 每个桶自带 hash tag,16 个桶落 16 个不同的 slot:
//
//	spectate:battles:active:{sba:<k>}    活跃集合:member=battle_id,score=created_at_ms(spectate.go)
//	spectate:ranked:active:{srk:<k>}     高段论剑排位直播索引:member=battle_id,score=created_at_ms(防 PVE 稀释,spectate.go)
//	spectate:bcast:active:{sbc:<k>}      全服 L2 配额:member=battle_id,score=战斗期限毫秒(spectate_tier.go)
//	spectate:featured:active:{sft:<k>}   赛事局索引:member=battle_id,score=标记到期毫秒(spectate_tier.go)
//
// 写(ZADD / ZREM,见 add / remove)只碰本场所在的那个桶,键一律经 keyFor / bucketKey 构造。跨桶的读与清理把 16 条命令
// 放进一个 pipeline(集群客户端按节点拆开),在进程里合并;列表读另有每副本 1 秒的缓存(svc.SpectateLobbyCache)。

// spectateIndexBuckets 是每把全服观战索引的桶数。上线后改桶数要停写迁移:成员是按旧桶数落的桶,换了桶数之后 ZADD / ZREM
// 找不到它,只剩按分数的定期清理能摘掉,L2 配额也会少算。
const spectateIndexBuckets = 16

// spectateIndex 是一把拆桶的全服观战索引:键前缀与桶的 hash tag 前缀。
type spectateIndex struct {
	prefix string
	tag    string
}

var (
	spectateActiveIndex    = spectateIndex{prefix: "spectate:battles:active", tag: "sba"}
	spectateRankedIndex    = spectateIndex{prefix: "spectate:ranked:active", tag: "srk"}
	spectateBroadcastIndex = spectateIndex{prefix: "spectate:bcast:active", tag: "sbc"}
	spectateFeaturedIndex  = spectateIndex{prefix: "spectate:featured:active", tag: "sft"}
)

// spectateIndexBucket 是一场战斗所在的桶号。battle_id 是发号器打散过的值,低位均匀。
func spectateIndexBucket(battleId uint64) int {
	return int(battleId % spectateIndexBuckets)
}

// bucketKey 是第 bucket 个桶的键,形如 spectate:battles:active:{sba:3}。三把索引的键只在这里拼。
func (ix spectateIndex) bucketKey(bucket int) string {
	return fmt.Sprintf("%s:{%s:%d}", ix.prefix, ix.tag, bucket)
}

// keyFor 是一场战斗所在桶的键。
func (ix spectateIndex) keyFor(battleId uint64) string {
	return ix.bucketKey(spectateIndexBucket(battleId))
}

// add 把一场战斗写进它所在的桶(ZADD,member = battle_id 十进制;已在桶里就更新分数)。
func (ix spectateIndex) add(ctx context.Context, rds redv9.UniversalClient, battleId uint64, score int64) error {
	return rds.ZAdd(ctx, ix.keyFor(battleId), redv9.Z{Score: float64(score), Member: strconv.FormatUint(battleId, 10)}).Err()
}

// remove 把一场战斗从它所在的桶里摘掉(ZREM);不在桶里也算成功。
func (ix spectateIndex) remove(ctx context.Context, rds redv9.UniversalClient, battleId uint64) error {
	return rds.ZRem(ctx, ix.keyFor(battleId), strconv.FormatUint(battleId, 10)).Err()
}

// eachSpectateBucket 给一把索引的 16 个桶各发一条命令,全部放进一个 pipeline 发出,按桶号返回各自的命令。
// 任一条失败就整体失败:命令可能根本没发出去(守卫拒绝、取不到连接),不能把没执行的空结果当成读到了空桶。
// 管道里只放幂等命令(ZCARD、ZRANGE 族、ZREM、ZREMRANGEBYSCORE):集群客户端在管道内部会自动重发整批,
// 非幂等命令进管道会被句柄的守卫拒绝(clusterredis.ErrNonIdempotentInPipeline)。
func eachSpectateBucket[C any](ctx context.Context, rds redv9.UniversalClient, ix spectateIndex,
	cmd func(pipe redv9.Pipeliner, key string) C) ([]C, error) {
	cmds := make([]C, spectateIndexBuckets)
	_, err := rds.Pipelined(ctx, func(pipe redv9.Pipeliner) error {
		for bucket := range cmds {
			cmds[bucket] = cmd(pipe, ix.bucketKey(bucket))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cmds, nil
}

// counts 返回各桶的成员数,下标是桶号。
func (ix spectateIndex) counts(ctx context.Context, rds redv9.UniversalClient) ([]int64, error) {
	cmds, err := eachSpectateBucket(ctx, rds, ix, func(pipe redv9.Pipeliner, key string) *redv9.IntCmd {
		return pipe.ZCard(ctx, key)
	})
	if err != nil {
		return nil, err
	}
	return spectateBucketInts(cmds), nil
}

// top 返回全服分数最高的 n 个成员:全服前 n 一定落在各桶的前 n 里,所以各桶取前 n,合并后排序再截断。
func (ix spectateIndex) top(ctx context.Context, rds redv9.UniversalClient, n int) ([]svc.SpectateIndexMember, error) {
	if n <= 0 {
		return nil, nil
	}
	cmds, err := eachSpectateBucket(ctx, rds, ix, func(pipe redv9.Pipeliner, key string) *redv9.ZSliceCmd {
		return pipe.ZRevRangeWithScores(ctx, key, 0, int64(n)-1)
	})
	if err != nil {
		return nil, err
	}
	return mergeSpectateBuckets(cmds, n), nil
}

// topFrom 同 top,只看分数不低于 minScore 的成员(ZREVRANGEBYSCORE minScore +inf)。
func (ix spectateIndex) topFrom(ctx context.Context, rds redv9.UniversalClient, minScore int64, n int) ([]svc.SpectateIndexMember, error) {
	if n <= 0 {
		return nil, nil
	}
	cmds, err := eachSpectateBucket(ctx, rds, ix, func(pipe redv9.Pipeliner, key string) *redv9.ZSliceCmd {
		return pipe.ZRevRangeByScoreWithScores(ctx, key, &redv9.ZRangeBy{Min: strconv.FormatInt(minScore, 10), Max: "+inf", Count: int64(n)})
	})
	if err != nil {
		return nil, err
	}
	return mergeSpectateBuckets(cmds, n), nil
}

// at 按全服下标取一个成员。counts 是同一轮读出的各桶成员数;下标落在累计和的哪一段就去那个桶按桶内下标取,
// 全服下标均匀,取到每个成员的概率就均匀。并发增删让桶变小时可能取不到,返回空,调用方重试。
func (ix spectateIndex) at(ctx context.Context, rds redv9.UniversalClient, counts []int64, index int64) ([]svc.SpectateIndexMember, error) {
	bucket, local := locateSpectateIndex(counts, index)
	if bucket < 0 {
		return nil, nil
	}
	zs, err := rds.ZRangeWithScores(ctx, ix.bucketKey(bucket), local, local).Result()
	if err != nil {
		return nil, err
	}
	return appendSpectateIndexMembers(nil, zs), nil
}

// locateSpectateIndex 把全服下标映射成 (桶号, 桶内下标):桶按号排开,0 号桶占 [0, counts[0]),1 号桶接着排,依此类推。
// 越界返回 (-1, 0)。
func locateSpectateIndex(counts []int64, index int64) (int, int64) {
	if index < 0 {
		return -1, 0
	}
	for bucket, count := range counts {
		if index < count {
			return bucket, index
		}
		index -= count
	}
	return -1, 0
}

// removeByScore 每个桶都清掉分数不高于 maxScore 的成员,返回清掉的总数。
func (ix spectateIndex) removeByScore(ctx context.Context, rds redv9.UniversalClient, maxScore int64) (int64, error) {
	cmds, err := eachSpectateBucket(ctx, rds, ix, func(pipe redv9.Pipeliner, key string) *redv9.IntCmd {
		return pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(maxScore, 10))
	})
	if err != nil {
		return 0, err
	}
	return sumSpectateBuckets(spectateBucketInts(cmds)), nil
}

// removeGarbage 摘掉一个解析不出 battle_id 的坏成员:算不出它在哪个桶,16 个桶各发一条 ZREM。只有手工写坏的数据才会走到这里。
func (ix spectateIndex) removeGarbage(ctx context.Context, rds redv9.UniversalClient, member string) error {
	_, err := eachSpectateBucket(ctx, rds, ix, func(pipe redv9.Pipeliner, key string) *redv9.IntCmd {
		return pipe.ZRem(ctx, key, member)
	})
	return err
}

// removeSpectateIndexGarbage 摘掉索引里的坏成员,本副本的列表缓存也忘掉它。失败只记日志:坏成员下次读到再摘。
func removeSpectateIndexGarbage(s *svc.ServiceContext, ix spectateIndex, member string) {
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if err := ix.removeGarbage(ctx, s.MatchRedis, member); err != nil {
		logx.Errorf("[spectate] Failed to remove invalid member %q from %s: %v", member, ix.prefix, err)
	}
	s.SpectateLobby.Forget(member)
}

// mergeSpectateBuckets 合并各桶读出的成员:分数降序,同分按成员倒序(与 ZREVRANGE 同序),截断成 n 个。
func mergeSpectateBuckets(cmds []*redv9.ZSliceCmd, n int) []svc.SpectateIndexMember {
	var merged []svc.SpectateIndexMember
	for _, cmd := range cmds {
		merged = appendSpectateIndexMembers(merged, cmd.Val())
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].Member > merged[j].Member
	})
	if len(merged) > n {
		merged = merged[:n]
	}
	return merged
}

// appendSpectateIndexMembers 把一次 ZSET 读出的成员追加到 members 后面。分数都是毫秒时刻,取整不丢精度。
func appendSpectateIndexMembers(members []svc.SpectateIndexMember, zs []redv9.Z) []svc.SpectateIndexMember {
	for _, z := range zs {
		members = append(members, svc.SpectateIndexMember{Member: fmt.Sprint(z.Member), Score: int64(z.Score)})
	}
	return members
}

func spectateBucketInts(cmds []*redv9.IntCmd) []int64 {
	values := make([]int64, len(cmds))
	for bucket, cmd := range cmds {
		values[bucket] = cmd.Val()
	}
	return values
}

func sumSpectateBuckets(values []int64) int64 {
	var total int64
	for _, value := range values {
		total += value
	}
	return total
}
