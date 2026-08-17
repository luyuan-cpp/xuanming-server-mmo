package leader

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// goRedisStore 把 go-redis v9 的 *redis.Client 适配成 Store。
// login 迁移它的 loginqueue dispatcher 选主时用这个适配器,锁语义不变
// (脚本与 login pkg/locker 逐字一致)。
type goRedisStore struct {
	c *redis.Client
}

// NewGoRedisStore 用 go-redis v9 客户端构造锁存储。
func NewGoRedisStore(c *redis.Client) Store {
	return &goRedisStore{c: c}
}

func (s *goRedisStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return s.c.SetNX(ctx, key, value, ttl).Result()
}

func (s *goRedisStore) EvalInt(ctx context.Context, script string, keys []string, args ...string) (int64, error) {
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
