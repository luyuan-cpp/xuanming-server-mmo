// Package controlredis 是 scene_manager 的控制集群句柄(docs/design/redis-cluster-all-domains-20261110.md §4.1、§5.1;
// 2026-11-12 全部 Redis 上 Cluster 第 2b 批取代 go-zero 的 *redis.Redis)。
//
// 底层是 shared/clusterredis.New(…, ControlClusterKeys)给出的 redis.UniversalClient:
//   - 单实例与 Redis Cluster 走同一条代码路径;
//   - 发送前守卫核对键:带 tag 的键只能是登记过的族 tag,多键命令(MGET、多键 DEL、EVAL 的 KEYS)每个键带同一个 tag,
//     玩家域的 tag 一律拒绝(clusterredis.ErrWrongDomainTag)。键发错句柄当场报错,不会悄悄读写到另一个域;
//   - 节点客户端不自动重发结果未知的命令。go-zero 句柄写死 MaxRetries=3,非幂等的 Lua 在 EOF / 超时后会被原样重放
//     (断线重连清单 R8,docs/design/infra-reconnect-overview.md);
//   - 管道里不能放 EVAL、INCR 这类非幂等命令,守卫整批拒绝(clusterredis.ErrNonIdempotentInPipeline):集群客户端在管道中途
//     遇到 EOF / 超时会把整批重发。要放进管道的只有 GET、SET、SCARD、ZADD 这类可以重放的命令。
//
// 方法名、参数与返回形状沿用换句柄之前的 go-zero *redis.Redis(v1.10.0),全包约 170 处调用与它们「缺键怎么判」的逻辑不用逐个改写:
//   - Get 把缺键翻成 ("", nil);Mget、Hmget 的缺项是 "";Exists、Sismember、SetnxEx、Hsetnx 回 bool;
//   - Hget、Zscore 不翻:缺键(或字段、成员)回 redis.Nil;
//   - Zscore 与 ZrangeWithScores 的分数截成 int64;Zadd 回「是不是新成员」;
//   - 不带 ctx 的方法用 context.Background(),单次读写由句柄的读写超时约束(clusterredis.Config 的 ReadTimeoutMs / WriteTimeoutMs)。
//
// 本包只做形状适配,不加重试、熔断或缓存。
package controlredis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shared/clusterredis"

	"github.com/redis/go-redis/v9"
)

// Client 是控制集群句柄。用 New 构造;并发安全(底层 UniversalClient 并发安全)。
type Client struct {
	rdb redis.UniversalClient
}

// Pair 是有序集合里的一项:成员与截成 int64 的分数(与 go-zero 的 redis.Pair 同形)。
type Pair struct {
	Key   string
	Score int64
}

// New 按配置构造控制集群句柄。不连 Redis:启动时的探活由调用方 Ping。返回的句柄由调用方 Close。
func New(c clusterredis.Config) (*Client, error) {
	rdb, err := clusterredis.New(c, clusterredis.ControlClusterKeys)
	if err != nil {
		return nil, err
	}
	return &Client{rdb: rdb}, nil
}

// UniversalClient 返回底层客户端,给只收 redis.UniversalClient 的组件用(shared/leader 的锁存储)。
func (c *Client) UniversalClient() redis.UniversalClient { return c.rdb }

// AddHook 在键守卫之后追加一个钩子。单测用它在两条命令之间注入故障或另一个写者;生产不用。
func (c *Client) AddHook(h redis.Hook) { c.rdb.AddHook(h) }

// Ping 探一次活。
func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// Close 关闭连接池。
func (c *Client) Close() error { return c.rdb.Close() }

// Do 单独发一条任意命令。带围栏的写(EVAL)不能进管道时用它逐条发(logic/load_refresh.go)。
func (c *Client) Do(ctx context.Context, args ...any) *redis.Cmd { return c.rdb.Do(ctx, args...) }

// PipelinedCtx 把 fn 里排进管道的命令一次发出。返回值是第一条出错命令的错误(redis.Nil 也算),与 go-zero 相同;
// 逐条结果从各自的 Cmd 取。非幂等命令不能进管道,见包注释。
func (c *Client) PipelinedCtx(ctx context.Context, fn func(redis.Pipeliner) error) error {
	_, err := c.rdb.Pipelined(ctx, fn)
	return err
}

// Del 删除键,返回删掉的个数。
func (c *Client) Del(keys ...string) (int, error) {
	n, err := c.rdb.Del(context.Background(), keys...).Result()
	return int(n), err
}

// Eval 执行一段脚本,返回脚本的回复;脚本回 nil 时错误是 redis.Nil。
func (c *Client) Eval(script string, keys []string, args ...any) (any, error) {
	return c.rdb.Eval(context.Background(), script, keys, args...).Result()
}

// Exists 报告键是否存在。
func (c *Client) Exists(key string) (bool, error) {
	n, err := c.rdb.Exists(context.Background(), key).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Get 读字符串键;键不存在返回 ("", nil)。
func (c *Client) Get(key string) (string, error) {
	val, err := c.rdb.Get(context.Background(), key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return val, err
}

// Hdel 删除 hash 字段,返回是否删掉了至少一个。
func (c *Client) Hdel(key string, fields ...string) (bool, error) {
	n, err := c.rdb.HDel(context.Background(), key, fields...).Result()
	if err != nil {
		return false, err
	}
	return n >= 1, nil
}

// Hget 读 hash 字段;键或字段不存在时错误是 redis.Nil。
func (c *Client) Hget(key, field string) (string, error) {
	return c.rdb.HGet(context.Background(), key, field).Result()
}

// Hgetall 读整个 hash。
func (c *Client) Hgetall(key string) (map[string]string, error) {
	return c.rdb.HGetAll(context.Background(), key).Result()
}

// Hkeys 读 hash 的全部字段名。
func (c *Client) Hkeys(key string) ([]string, error) {
	return c.rdb.HKeys(context.Background(), key).Result()
}

// Hmget 读多个 hash 字段,缺项是 ""。
func (c *Client) Hmget(key string, fields ...string) ([]string, error) {
	vals, err := c.rdb.HMGet(context.Background(), key, fields...).Result()
	if err != nil {
		return nil, err
	}
	return toStrings(vals), nil
}

// Hset 写一个 hash 字段。
func (c *Client) Hset(key, field, value string) error {
	return c.rdb.HSet(context.Background(), key, field, value).Err()
}

// Hsetnx 字段不存在时写入,返回是否写入。
func (c *Client) Hsetnx(key, field, value string) (bool, error) {
	return c.rdb.HSetNX(context.Background(), key, field, value).Result()
}

// Incr 自增 1,返回新值。
func (c *Client) Incr(key string) (int64, error) {
	return c.rdb.Incr(context.Background(), key).Result()
}

// Mget 读多个键,缺项是 ""。键必须同一个 tag(守卫核对)。
func (c *Client) Mget(keys ...string) ([]string, error) {
	return c.MgetCtx(context.Background(), keys...)
}

// MgetCtx 同 Mget,受 ctx 约束。
func (c *Client) MgetCtx(ctx context.Context, keys ...string) ([]string, error) {
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	return toStrings(vals), nil
}

// Sadd 往集合里加成员,返回新加的个数。
func (c *Client) Sadd(key string, values ...any) (int, error) {
	n, err := c.rdb.SAdd(context.Background(), key, values...).Result()
	return int(n), err
}

// Scard 读集合大小。
func (c *Client) Scard(key string) (int64, error) {
	return c.rdb.SCard(context.Background(), key).Result()
}

// Set 写字符串键,不带过期。
func (c *Client) Set(key, value string) error {
	return c.rdb.Set(context.Background(), key, value, 0).Err()
}

// Setex 写字符串键并带过期(秒)。
func (c *Client) Setex(key, value string, seconds int) error {
	return c.rdb.Set(context.Background(), key, value, time.Duration(seconds)*time.Second).Err()
}

// SetnxEx 键不存在时写入并带过期(秒),返回是否写入。
func (c *Client) SetnxEx(key, value string, seconds int) (bool, error) {
	return c.rdb.SetNX(context.Background(), key, value, time.Duration(seconds)*time.Second).Result()
}

// Sismember 报告 value 是否在集合里。
func (c *Client) Sismember(key string, value any) (bool, error) {
	return c.rdb.SIsMember(context.Background(), key, value).Result()
}

// Smembers 读集合的全部成员。
func (c *Client) Smembers(key string) ([]string, error) {
	return c.rdb.SMembers(context.Background(), key).Result()
}

// Srem 从集合里摘成员,返回摘掉的个数。
func (c *Client) Srem(key string, values ...any) (int, error) {
	n, err := c.rdb.SRem(context.Background(), key, values...).Result()
	return int(n), err
}

// SscanCtx 按游标扫集合,返回这一批成员与下一个游标(0 = 扫完)。
func (c *Client) SscanCtx(ctx context.Context, key string, cursor uint64, match string, count int64) ([]string, uint64, error) {
	return c.rdb.SScan(ctx, key, cursor, match, count).Result()
}

// Zadd 写一个有序集合成员,返回它是不是新成员。
func (c *Client) Zadd(key string, score int64, value string) (bool, error) {
	n, err := c.rdb.ZAdd(context.Background(), key, redis.Z{Score: float64(score), Member: value}).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Zcard 读有序集合大小。
func (c *Client) Zcard(key string) (int, error) {
	n, err := c.rdb.ZCard(context.Background(), key).Result()
	return int(n), err
}

// Zrange 按下标区间读成员。
func (c *Client) Zrange(key string, start, stop int64) ([]string, error) {
	return c.rdb.ZRange(context.Background(), key, start, stop).Result()
}

// ZrangeWithScores 按下标区间读成员与分数(分数截成 int64)。
func (c *Client) ZrangeWithScores(key string, start, stop int64) ([]Pair, error) {
	vals, err := c.rdb.ZRangeWithScores(context.Background(), key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return toPairs(vals), nil
}

// ZrangebyscoreWithScoresAndLimit 按分数区间 [start, stop] 读第 page 页、每页 size 个成员与分数;size <= 0 时什么都不读。
func (c *Client) ZrangebyscoreWithScoresAndLimit(key string, start, stop int64, page, size int) ([]Pair, error) {
	if size <= 0 {
		return nil, nil
	}
	vals, err := c.rdb.ZRangeByScoreWithScores(context.Background(), key, &redis.ZRangeBy{
		Min:    strconv.FormatInt(start, 10),
		Max:    strconv.FormatInt(stop, 10),
		Offset: int64(page * size),
		Count:  int64(size),
	}).Result()
	if err != nil {
		return nil, err
	}
	return toPairs(vals), nil
}

// Zrem 从有序集合里摘成员,返回摘掉的个数。
func (c *Client) Zrem(key string, values ...any) (int, error) {
	n, err := c.rdb.ZRem(context.Background(), key, values...).Result()
	return int(n), err
}

// Zscore 读成员的分数(截成 int64);键或成员不存在时错误是 redis.Nil。
func (c *Client) Zscore(key, value string) (int64, error) {
	score, err := c.rdb.ZScore(context.Background(), key, value).Result()
	if err != nil {
		return 0, err
	}
	return int64(score), nil
}

// toStrings 把 MGET / HMGET 的回复转成字符串:缺项(nil)是 ""。
func toStrings(vals []any) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		switch s := v.(type) {
		case nil:
			out[i] = ""
		case string:
			out[i] = s
		default:
			out[i] = fmt.Sprint(s)
		}
	}
	return out
}

func toPairs(vals []redis.Z) []Pair {
	out := make([]Pair, len(vals))
	for i, z := range vals {
		member, ok := z.Member.(string)
		if !ok {
			member = fmt.Sprint(z.Member)
		}
		out[i] = Pair{Key: member, Score: int64(z.Score)}
	}
	return out
}
