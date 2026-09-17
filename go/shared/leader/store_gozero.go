package leader

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// goZeroStore 把 go-zero 的 *redis.Redis 适配成 Store。
// scene_manager / db / data_service 这批用 go-zero Redis 客户端的服务走这里。
type goZeroStore struct {
	r *redis.Redis
}

// NewGoZeroStore 用 go-zero Redis 客户端构造锁存储。
func NewGoZeroStore(r *redis.Redis) Store {
	return &goZeroStore{r: r}
}

var _ CallBounder = (*goZeroStore)(nil)

// MaxCallDuration 报告 socket 写 + 读这一段的上界(不是整次调用的最坏耗时:写之前的连接池排队、
// 重试退避、拨号受续期 ctx 截止时间约束,心跳用 CallBudget 把续期 ctx 超时加上去;新建连接时的
// HELLO / AUTH 握手读写仍不在其内)。只能按版本写死:go-zero 的 core/stores/redis/redisclientmanager.go
// 用 red.NewClient 建客户端时不设 ReadTimeout / WriteTimeout / ContextTimeoutEnabled,
// go-redis 取默认读 3s + 写 3s 且 socket 读写不认 ctx 截止时间;*redis.Redis 又不暴露底层
// 选项,没法像 goRedisStore 那样现算。
//
// 版本以调用方模块实际链接的为准,不是 shared/go.mod 的 require:当前唯一调用方 scene_manager
// 选中 go-zero v1.10.0 + go-redis v9.17.3(scene_manager/go.mod),已核对与 v1.9.2 + v9.16.0 一致。
// 任何调用 NewGoZeroStore 的模块改动 go-zero 或 go-redis 版本时都必须重新核对这里。
func (s *goZeroStore) MaxCallDuration() time.Duration {
	return 6 * time.Second
}

func (s *goZeroStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	// go-zero 的 SetnxEx 只有秒粒度;不足 1 秒向上取整,避免 TTL=0 变成永不过期。
	seconds := int(ttl / time.Second)
	if time.Duration(seconds)*time.Second < ttl || seconds < 1 {
		seconds++
	}
	return s.r.SetnxExCtx(ctx, key, value, seconds)
}

func (s *goZeroStore) EvalInt(ctx context.Context, script string, keys []string, args ...string) (int64, error) {
	ifaceArgs := make([]any, len(args))
	for i, a := range args {
		ifaceArgs[i] = a
	}
	raw, err := s.r.EvalCtx(ctx, script, keys, ifaceArgs...)
	if err != nil {
		return 0, err
	}
	n, ok := raw.(int64)
	if !ok {
		return 0, fmt.Errorf("leader: unexpected eval result type %T (%v)", raw, raw)
	}
	return n, nil
}
