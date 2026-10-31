package playersync

import (
	"context"
	"reflect"

	"github.com/redis/go-redis/v9"
)

// goRedisEvaler 把 go-redis v9 的客户端适配成 Evaler。
// 各服务的玩家域句柄(PlayerRedis)走这里;单机 / 哨兵 / 集群客户端都实现 UniversalClient。
type goRedisEvaler struct {
	c redis.UniversalClient
}

// NewGoRedisEvaler 用 go-redis v9 客户端构造 Evaler。c 必须是玩家域句柄(见包注释)。
// c 为 nil 时返回 nil,NewBumper 随之返回 nil(bump 全部是无操作)。
// 带类型的 nil 指针(如 (*redis.Client)(nil))同样算 nil。
func NewGoRedisEvaler(c redis.UniversalClient) Evaler {
	if c == nil {
		return nil
	}
	// 参数是接口:(*redis.Client)(nil) 装进来之后 c != nil,上面那行拦不住。放过去会得到非 nil 的
	// Bumper,第一次 Eval 在 go-redis 内部空指针 panic,而且发生在调用方异步的 bump 协程里
	// (guild 传的就是具体类型 *redis.Client)。按反射判指针是否为 nil,三种客户端
	// (Client / ClusterClient / Ring)与日后的包装类型一并覆盖,不必逐个列类型。
	if v := reflect.ValueOf(c); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil
	}
	return goRedisEvaler{c: c}
}

func (e goRedisEvaler) Eval(ctx context.Context, script string, keys []string, args ...string) error {
	return e.c.Eval(ctx, script, keys, toAny(args)...).Err()
}
