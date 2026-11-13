package assetopwake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/redis/go-redis/v9"

	"shared/redisslot"
)

const (
	// subscribeConfirmTimeout 是等一组 SSUBSCRIBE 全部确认的上限:Redis 连不上时尽快失败、交给订阅循环的退避。
	subscribeConfirmTimeout = 5 * time.Second
	// healthCheckInterval:订阅连接空闲这么久就发一次 PING;再过一个间隔还没有任何回包,就当连接已死(半开),
	// 关掉重订。取 go-redis 自带健康检查的默认间隔(3 秒),最坏约两个间隔发现。
	healthCheckInterval = 3 * time.Second
)

// errHealthCheckTimeout:健康检查的 PING 发出后一个间隔内没有任何回包。
var errHealthCheckTimeout = errors.New("assetopwake: no reply to health check ping; connection presumed dead")

// goRedisBus 用 go-redis v9 实现 bus。UniversalClient 覆盖单机与集群两种部署(clusterredis.New 的两种返回值)。
//
// 订阅不用 go-redis 的 PubSub.Channel / ChannelWithSubscriptions,自己收包。原因是 go-redis v9.17.3 在集群上自己重连
// 分片订阅时会连错节点,而且不报错(2026-11-12 读源码确认):
//   - 断线或健康检查失败后它自己重连(pubsub.go 的 conn),选节点只看普通频道 c.channels,分片频道 c.schannels 不算;
//     频道表为空时 ClusterClient 随机挑一个节点(osscluster.go 的 pubSub,c.nodes.Random()),在那里重发 SSUBSCRIBE;
//   - 不持有那个 slot 的节点回 -MOVED。Channel 的收包循环把出错的回包吞掉接着读(pubsub.go 的 initAllChan),
//     MOVED 也不算坏连接(error.go 的 isBadConn),连接留着,之后只剩 PING 的回音:订阅就此静默失聪,直到进程重启。
//
// 所以这里遇到任何错误都关掉整条订阅,由订阅循环按退避重新 SSubscribe(重新按 slot 选属主),不依赖 go-redis 的自动重订;
// 空闲时的健康检查也自己做。迁槽时服务端推来的 sunsubscribe 走同一条路(设计 §4.6)。
type goRedisBus struct {
	c redis.UniversalClient
}

// groups:集群客户端按 slot 分组(一条 SSUBSCRIBE 只能订同一个 slot 的频道;64 片恰好落在 64 个不同的 slot,
// 即 64 组、每组一条连接);其余客户端(单机)全部频道一组、一条连接。
// 单机只开一条是有意的:部署脚本的 Redis 连接总账按「每个订阅副本 1 条 Pub/Sub 连接」记,单实例上每副本开 64 条
// 会把连接数抬高 64 倍。集群上的订阅连接按主节点另算(设计 §5.1、§7)。
func (b goRedisBus) groups(channels []string) [][]string {
	if _, clustered := b.c.(*redis.ClusterClient); clustered {
		return groupBySlot(channels)
	}
	return [][]string{append([]string(nil), channels...)}
}

// groupBySlot 按 slot 分组:组内保持输入顺序,组按各自第一个频道出现的顺序排。
func groupBySlot(channels []string) [][]string {
	index := make(map[uint16]int, len(channels))
	var groups [][]string
	for _, channel := range channels {
		slot := redisslot.HashSlot(channel)
		i, ok := index[slot]
		if !ok {
			i = len(groups)
			index[slot] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], channel)
	}
	return groups
}

// subscribe 发 SSUBSCRIBE 并等到组内每个频道都确认。ClusterClient 按第一个频道的 slot 选属主(读的是缓存的拓扑,
// 可能落后;落后时属主回 -MOVED,这里报错,订阅循环先 refreshTopology 再按退避重订)。
func (b goRedisBus) subscribe(ctx context.Context, channels []string) (subscription, error) {
	if len(channels) == 0 {
		return nil, errors.New("assetopwake: empty subscription group")
	}
	ps := b.c.SSubscribe(ctx, channels...)
	sub := newGoRedisSubscription(ctx, ps, healthCheckInterval)
	if err := confirmSubscribed(ctx, ps, channels, subscribeConfirmTimeout); err != nil {
		_ = sub.close()
		return nil, fmt.Errorf("assetopwake: ssubscribe %s not confirmed: %w", describeGroup(channels), err)
	}
	return sub, nil
}

// refreshTopology:ClusterClient 的 ReloadState 是异步的懒重读(同一时刻只跑一次),调用本身不阻塞。
func (b goRedisBus) refreshTopology(ctx context.Context) {
	if cc, ok := b.c.(*redis.ClusterClient); ok {
		cc.ReloadState(ctx)
	}
}

// claimDedup 即 SET key 1 NX EX ttl。
func (b goRedisBus) claimDedup(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return b.c.SetNX(ctx, key, "1", ttl).Result()
}

// pubSubConn 是 *redis.PubSub 上本包用到的方法,抽出来是为了单测确认与收包循环。
type pubSubConn interface {
	ReceiveTimeout(ctx context.Context, timeout time.Duration) (interface{}, error)
	Ping(ctx context.Context, payload ...string) error
	Close() error
}

var _ pubSubConn = (*redis.PubSub)(nil)

// confirmSubscribed 读到 channels 每一个的 ssubscribe 确认才返回 nil,总共最多等 timeout。
// 一条 SSUBSCRIBE 的各个确认在回包里是连续的(服务端执行完这条命令才处理别人的发布),
// 所以确认之前出现任何别的回包都算异常;服务端拒绝(MOVED、CROSSSLOT、未知命令、ACL)是一个错误回包。
func confirmSubscribed(ctx context.Context, conn pubSubConn, channels []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pending := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		pending[channel] = struct{}{}
	}
	for len(pending) > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out after %v with %d of %d channel(s) unconfirmed", timeout, len(pending), len(channels))
		}
		reply, err := conn.ReceiveTimeout(ctx, remaining)
		if err != nil {
			return err
		}
		confirmation, ok := reply.(*redis.Subscription)
		if !ok || confirmation.Kind != "ssubscribe" {
			return fmt.Errorf("unexpected reply %s while waiting for ssubscribe confirmations", describeReply(reply))
		}
		delete(pending, confirmation.Channel)
	}
	return nil
}

// goRedisSubscription 是一条已确认的分片订阅连接,只由一个订阅循环使用(pingOutstanding 不加锁)。
type goRedisSubscription struct {
	conn pubSubConn
	// stopCloseOnDone 撤掉「ctx 结束就关连接」的登记;close 时调用,免得订阅关掉之后闭包还挂在 ctx 上。
	stopCloseOnDone func() bool
	idle            time.Duration
	// pingOutstanding:发出的健康检查 PING 还没等到任何回包。
	pingOutstanding bool
}

// newGoRedisSubscription 包一条订阅连接,并登记 ctx 结束时关掉它:收包阻塞在读上,只有关连接能让它立刻返回,
// Stop 不用等一个健康检查间隔。
func newGoRedisSubscription(ctx context.Context, conn pubSubConn, idle time.Duration) *goRedisSubscription {
	return &goRedisSubscription{
		conn:            conn,
		stopCloseOnDone: context.AfterFunc(ctx, func() { _ = conn.Close() }),
		idle:            idle,
	}
}

func (s *goRedisSubscription) receive(ctx context.Context) (string, error) {
	for {
		reply, err := s.conn.ReceiveTimeout(ctx, s.idle)
		if err != nil {
			if ctx.Err() != nil || !isTimeout(err) {
				return "", err
			}
			// 空闲了一个间隔。上一个 PING 也没换来任何回包:连接已死,交给订阅循环重订。
			if s.pingOutstanding {
				return "", errHealthCheckTimeout
			}
			if err := s.conn.Ping(ctx); err != nil {
				return "", fmt.Errorf("assetopwake: health check ping: %w", err)
			}
			s.pingOutstanding = true
			continue
		}
		// 任何回包都说明连接活着。
		s.pingOutstanding = false
		payload, isMessage, err := classifyReply(reply)
		if err != nil {
			return "", err
		}
		if isMessage {
			return payload, nil
		}
	}
}

func (s *goRedisSubscription) close() error {
	s.stopCloseOnDone()
	return s.conn.Close()
}

// classifyReply 把订阅连接上的一个回包分成三类:唤醒消息(返回负载)、可以忽略的回包(PING 的回音、订阅确认)、
// 订阅已不可用(返回错误;服务端退订是 errServerUnsubscribed)。
func classifyReply(reply interface{}) (payload string, isMessage bool, err error) {
	switch m := reply.(type) {
	case *redis.Message:
		return m.Payload, true, nil
	case *redis.Subscription:
		if m.Kind == "sunsubscribe" {
			return "", false, fmt.Errorf("%w: channel=%s", errServerUnsubscribed, m.Channel)
		}
		return "", false, nil
	case *redis.Pong:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("assetopwake: unexpected pubsub reply %s", describeReply(reply))
	}
}

// isTimeout 报告 err 是不是读超时(连接空闲)。go-redis 的 ReceiveTimeout 在超时上不重连,连接照常可用。
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func describeReply(reply interface{}) string {
	return fmt.Sprintf("%T", reply)
}
