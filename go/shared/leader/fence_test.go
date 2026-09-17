package leader

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestFenceAfter(t *testing.T) {
	cases := []struct {
		name           string
		ttl, interval  time.Duration
		maxCall        time.Duration
		want           time.Duration
		wantGuaranteed bool
	}{
		{"默认 TTL + 续期 ctx 超时封顶", 30 * time.Second, 10 * time.Second, 2 * time.Second, 20 * time.Second, true},
		{"默认 TTL + go-zero 读写 6s + ctx 2s", 30 * time.Second, 10 * time.Second, 8 * time.Second, 20 * time.Second, true},
		{"maxCall 等于 interval 的边界", 30 * time.Second, 10 * time.Second, 10 * time.Second, 0, false},
		{"maxCall 超过 interval", 21 * time.Second, 7 * time.Second, 8 * time.Second, 0, false},
		{"TTL 不足 3 倍调用上限退化为 fail-closed", 12 * time.Second, 4 * time.Second, 6 * time.Second, 0, false},
		{"无上界调用不溢出", 30 * time.Second, 10 * time.Second, UnboundedCall, 0, false},
		{"单测 TTL 3s", 3 * time.Second, time.Second, 375 * time.Millisecond, 2 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, guaranteed := FenceAfter(tc.ttl, tc.interval, tc.maxCall)
			if got != tc.want || guaranteed != tc.wantGuaranteed {
				t.Fatalf("FenceAfter(%s, %s, %s) = (%s, %v),期望 (%s, %v)",
					tc.ttl, tc.interval, tc.maxCall, got, guaranteed, tc.want, tc.wantGuaranteed)
			}
			if !guaranteed {
				return
			}
			if tc.maxCall >= tc.interval || got <= tc.interval+tc.maxCall || 2*tc.interval+tc.maxCall >= tc.ttl {
				t.Fatalf("guaranteed=true 但门槛 %s 违反约束:interval=%s maxCall=%s ttl=%s",
					got, tc.interval, tc.maxCall, tc.ttl)
			}
		})
	}
}

func TestCallBudget(t *testing.T) {
	cases := []struct {
		name        string
		ctxTimeout  time.Duration
		clientBound time.Duration
		want        time.Duration
	}{
		{"客户端认 ctx:只算 ctx 超时", 2 * time.Second, 0, 2 * time.Second},
		{"不认 ctx:ctx 超时 + socket 读写上界", 2 * time.Second, 6 * time.Second, 8 * time.Second},
		{"socket 读写无上界", 2 * time.Second, UnboundedCall, UnboundedCall},
		{"负值按无上界处理", 2 * time.Second, -1, UnboundedCall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CallBudget(tc.ctxTimeout, tc.clientBound); got != tc.want {
				t.Fatalf("CallBudget(%s, %s) = %s,期望 %s", tc.ctxTimeout, tc.clientBound, got, tc.want)
			}
		})
	}
}

// 只构造客户端、读初始化后的选项,不拨号,不需要 Redis 服务。
func TestGoRedisMaxCallDuration(t *testing.T) {
	const addr = "127.0.0.1:0"
	cases := []struct {
		name string
		opts *redis.Options
		want time.Duration
	}{
		{"默认选项:读 3s + 写 3s", &redis.Options{Addr: addr}, 6 * time.Second},
		{"认 ctx 截止时间", &redis.Options{Addr: addr, ContextTimeoutEnabled: true}, 0},
		{"ReadTimeout=-1 不设截止", &redis.Options{Addr: addr, ReadTimeout: -1}, UnboundedCall},
		{"ReadTimeout=-2 禁用截止", &redis.Options{Addr: addr, ReadTimeout: -2}, UnboundedCall},
		{"认 ctx 但 ReadTimeout=-2:不设 socket 截止,ctx 截止也用不上", &redis.Options{Addr: addr, ContextTimeoutEnabled: true, ReadTimeout: -2}, UnboundedCall},
		{"认 ctx 但 WriteTimeout=-2", &redis.Options{Addr: addr, ContextTimeoutEnabled: true, WriteTimeout: -2}, UnboundedCall},
		{"认 ctx 且 ReadTimeout=-1:超时为 0 时直接取 ctx 截止", &redis.Options{Addr: addr, ContextTimeoutEnabled: true, ReadTimeout: -1}, 0},
		{"显式读写超时", &redis.Options{Addr: addr, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 250 * time.Millisecond}, 750 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := redis.NewClient(tc.opts)
			defer func() { _ = c.Close() }()
			if got := GoRedisMaxCallDuration(c.Options()); got != tc.want {
				t.Fatalf("GoRedisMaxCallDuration = %s,期望 %s", got, tc.want)
			}
		})
	}
}
