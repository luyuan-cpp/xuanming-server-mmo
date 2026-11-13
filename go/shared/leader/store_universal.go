package leader

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// universalStore 把 go-redis v9 的 redis.UniversalClient 适配成 Store,给拿着 shared/clusterredis 句柄(单实例或 Redis Cluster)
// 的服务用(docs/design/redis-cluster-all-domains-20261110.md §4.7)。
//
// 锁语义与 goRedisStore 逐字一致:单键 SET NX PX、单键属主校验 Lua,集群下不跨槽,也不假设多条命令走同一条连接。
// 「结果未知不重放」由 clusterredis 的构造器负责(节点客户端 MaxRetries=-1、网络错误不重发),本适配器不另做重试;
// 重放过的 SET NX 由 Elector 按锁值认出自己的写入(campaign 的属主核对),不靠客户端。
//
// 2026-11-12 全部 Redis 上 Cluster 第 5 批新增,只新增文件,leader.go 与既有两个 store 一行未改。
type universalStore struct {
	c redis.UniversalClient
}

// NewUniversalStore 用 redis.UniversalClient(shared/clusterredis.New 的返回值)构造锁存储。
// 锁键不带 hash tag 时是单键,控制集群的键守卫放行;以后锁键带 tag(如 {ctl:<z>})也只要求同一段脚本里的键同 tag。
func NewUniversalStore(c redis.UniversalClient) Store {
	return &universalStore{c: c}
}

var _ CallBounder = (*universalStore)(nil)

// MaxCallDuration 按客户端初始化后的真实选项计算,见 UniversalMaxCallDuration。
func (s *universalStore) MaxCallDuration() time.Duration {
	return UniversalMaxCallDuration(s.c)
}

func (s *universalStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return s.c.SetNX(ctx, key, value, ttl).Result()
}

func (s *universalStore) EvalInt(ctx context.Context, script string, keys []string, args ...string) (int64, error) {
	ifaceArgs := make([]any, len(args))
	for i, a := range args {
		ifaceArgs[i] = a
	}
	raw, err := s.c.Eval(ctx, script, keys, ifaceArgs...).Result()
	if err != nil {
		return 0, err
	}
	n, ok := raw.(int64)
	if !ok {
		return 0, fmt.Errorf("leader: unexpected eval result type %T (%v)", raw, raw)
	}
	return n, nil
}
