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
