package playerredis

import (
	"context"
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

// 「同一节点、同一条连接连发一条 pipeline」的支持(docs/design/zone-redis-regression-guard-20261022.md §4.3.3)。
//
// 铸造 owner_epoch 的 EVAL 与紧随的 WAITAOF 必须落在持有该 slot 的主节点的同一条连接上:WAITAOF 等的是本连接最后一次写的
// 复制偏移。ClusterClient.Pipelined 按键分组,无键的 WAITAOF 会被发到任意节点,确认的是另一台机器的偏移,所以这条通道直接
// 拿节点客户端发 pipeline,重定向由调用方按 ClassifyNotExecuted 处理。

// NewPinned 构造这条通道专用的客户端。与 New 只差一处:不挂 GuardHook。
//
// GuardHook 的管道规则拒绝 EVAL,理由是 ClusterClient 的管道遇到 EOF / 超时会整批自动重发。这里的 pipeline 只发给节点客户端
// (MasterForKey 的返回值,MaxRetries=-1),不会自动重发,那条理由不成立。键的规则仍然要守:调用方发送前自己调 CheckKeys。
func NewPinned(c Config) (redis.UniversalClient, error) {
	return newClient(c)
}

// MasterForKey 返回持有 key 所在 slot 的主节点客户端,单实例就是它自己。路由表来自 ClusterClient 的缓存,可能落后,
// 发现 MOVED 时调用方要 ReloadTopology 后重取。
func MasterForKey(ctx context.Context, c redis.UniversalClient, key string) (*redis.Client, error) {
	switch cc := c.(type) {
	case *redis.Client:
		return cc, nil
	case *redis.ClusterClient:
		return cc.MasterForKey(ctx, key)
	default:
		return nil, ErrUnsupportedClient
	}
}

// ForEachMaster 对每个主节点调用 fn,单实例只调一次。Cluster 下各节点并行,返回其中一个错误。
func ForEachMaster(ctx context.Context, c redis.UniversalClient, fn func(ctx context.Context, node *redis.Client) error) error {
	switch cc := c.(type) {
	case *redis.Client:
		return fn(ctx, cc)
	case *redis.ClusterClient:
		return cc.ForEachMaster(ctx, fn)
	default:
		return ErrUnsupportedClient
	}
}

// ReloadTopology 请求刷新路由表。go-redis 在后台刷新,本函数立即返回,单实例什么都不做。
func ReloadTopology(ctx context.Context, c redis.UniversalClient) {
	if cc, ok := c.(*redis.ClusterClient); ok {
		cc.ReloadState(ctx)
	}
}

// NewNodeClient 建一个直连 addr 的单连接客户端,用完由调用方 Close。ASK 重定向的目标节点用它:ClusterClient 不暴露按地址取节点。
func NewNodeClient(c Config, addr string) *redis.Client {
	opts := nodeOptions(c, addr)
	opts.PoolSize = 1
	opts.MaxActiveConns = 1
	return redis.NewClient(opts)
}

// NotExecuted 是「服务端回了错误,命令确定没有执行」这类错误的分类。底层类型数的是分类数(现 4 种)。
type NotExecuted uint8

const (
	// NotExecutedNo:不属于这一类(业务错误回复、网络错误、nil)。
	NotExecutedNo NotExecuted = iota
	// NotExecutedMoved:slot 已归别的节点。刷新路由表后重发。
	NotExecutedMoved
	// NotExecutedAsk:slot 正在迁移,这次去目标节点问。pipeline 要前置 ASKING。
	NotExecutedAsk
	// NotExecutedRetryLater:TRYAGAIN / CLUSTERDOWN / LOADING / READONLY。退避后重发,READONLY 说明节点已降级,同样要刷新路由表。
	NotExecutedRetryLater
)

// ClassifyNotExecuted 判断 err 属于哪一类;MOVED / ASK 时第二个返回值是目标地址。
func ClassifyNotExecuted(err error) (NotExecuted, string) {
	var reply redis.Error
	if !errors.As(err, &reply) {
		return NotExecutedNo, ""
	}
	fields := strings.Fields(reply.Error())
	if len(fields) == 0 {
		return NotExecutedNo, ""
	}
	switch fields[0] {
	case "MOVED":
		return NotExecutedMoved, fields[len(fields)-1]
	case "ASK":
		return NotExecutedAsk, fields[len(fields)-1]
	case "TRYAGAIN", "CLUSTERDOWN", "LOADING", "READONLY":
		return NotExecutedRetryLater, ""
	default:
		return NotExecutedNo, ""
	}
}
