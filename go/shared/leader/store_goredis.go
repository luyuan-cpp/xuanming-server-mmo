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

var _ CallBounder = (*goRedisStore)(nil)

// MaxCallDuration 按客户端初始化后的真实选项计算,见 GoRedisMaxCallDuration。
func (s *goRedisStore) MaxCallDuration() time.Duration {
	return GoRedisMaxCallDuration(s.c.Options())
}

// GoRedisMaxCallDuration 返回 go-redis v9 客户端单次命令 socket 写 + 读这一段的上界,不是整次调用的
// 最坏耗时:写 socket 之前的连接池排队、重试退避、拨号受调用方 ctx 截止时间约束,心跳用
// CallBudget(ctx 超时, 本值) 把两段相加后才作为 FenceAfter 的 maxCall。
// opts 必须是初始化后的选项,即 (*redis.Client).Options() 的返回值:
// 初始化会把 ReadTimeout 的 0 换成默认 3s、-1 换成 0(不设截止)、-2 换成 -1(禁用截止),
// WriteTimeout 的 0 换成与 ReadTimeout 相同、-1/-2 同样换成 0/-1。
//
//   - 任一 < 0:internal/pool/conn.go 的 WithReader / WithWriter 只在 timeout >= 0 时才设 socket
//     截止时间,于是 ctx 截止时间也用不上(即使开了 ContextTimeoutEnabled),返回 UnboundedCall;
//   - ContextTimeoutEnabled:socket 读写截止时间取 ctx 截止时间(超时为 0 时直接取它,> 0 时取两者
//     较早者),返回 0,单次调用由调用方的 ctx 超时封顶;
//   - 否则 socket 读写传的是 context.Background(),只受读写超时约束:任一为 0 即无上界,返回
//     UnboundedCall;其余返回 ReadTimeout+WriteTimeout(一次写 + 一次读)。
//
// 以上按 go-redis v9.16.0 / v9.17.3 核对(两版 Options.init 与 Conn.deadline 行为一致),升级时须复核。
// 上界只计命令本身的一轮读写。可重试错误(如 EOF)触发的重发要先过重试退避与取连接的 ctx 检查,
// ctx 过期后不再发起,截止时间之后最多只剩在途的一轮,已由 CallBudget 的加法覆盖;新建连接时的
// 握手(HELLO/AUTH)还有各自的读写轮次,不在其内。
func GoRedisMaxCallDuration(opts *redis.Options) time.Duration {
	if opts.ReadTimeout < 0 || opts.WriteTimeout < 0 {
		return UnboundedCall
	}
	if opts.ContextTimeoutEnabled {
		return 0
	}
	if opts.ReadTimeout == 0 || opts.WriteTimeout == 0 {
		return UnboundedCall
	}
	return opts.ReadTimeout + opts.WriteTimeout
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
