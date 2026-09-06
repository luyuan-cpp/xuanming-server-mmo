// Package svc 是路由服的装配层:配置、etcd 客户端、按目标节点类型的发现镜像与拨号函数。
package svc

import (
	"context"
	"sort"
	"time"

	"client_rpc_router/generated/pb/game"
	"client_rpc_router/internal/config"
	"client_rpc_router/internal/discovery"
	"client_rpc_router/internal/metrics"

	base "proto/common/base"

	"shared/safego"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// ServiceContext 是 Forward 逻辑需要的全部依赖。路由服无状态:这里没有任何
// 会话/玩家态,只有配置、发现镜像与连接。
type ServiceContext struct {
	Config config.Config

	// Etcd 只用于注册自己与发现目标;Build 出来的上下文(测试)没有它。
	Etcd *clientv3.Client

	// ZoneScoped:转发时只挑同 zone 实例的目标节点类型(设计决策 D33)。
	ZoneScoped map[base.ENodeType]struct{}

	// Targets:每个需要转发的目标节点类型一个 etcd list-watch 镜像。键集合在
	// 启动时由生成路由表决定(TargetNodeTypes),运行期不变。
	Targets map[base.ENodeType]*discovery.NodeWatcher

	// Dial 按 endpoint 取 gRPC 连接。生产用 discovery.DialEndpoint(按 endpoint 缓存,
	// 节点下线由 watcher onRemove 清理);测试注入 bufconn 拨号器。
	Dial func(endpoint string) (*grpc.ClientConn, error)

	// ForwardTimeout 是单次向目标 Invoke 的超时(来自 ForwardTimeoutMs)。
	ForwardTimeout time.Duration
}

// TargetNodeTypes 从路由表收集需要发现的目标节点类型:所属 service 是客户端协议
// 且目标不是 Battle(战斗只走直连,D33,路由服不为它建 watch)。按枚举值升序,
// 让启动日志与指标顺序稳定。
func TargetNodeTypes(table map[uint32]game.RouteEntry) []base.ENodeType {
	seen := make(map[base.ENodeType]struct{})
	for _, entry := range table {
		if !entry.ClientProtocol || entry.NodeType == base.ENodeType_BattleNodeService {
			continue
		}
		seen[entry.NodeType] = struct{}{}
	}
	result := make([]base.ENodeType, 0, len(seen))
	for nodeType := range seen {
		result = append(result, nodeType)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// Build 组装不依赖外部进程的部分(配置校验、zone 集合、每目标类型一个 watcher、
// 拨号函数)。dial 为 nil 时用 discovery.DialEndpoint。拆出来是为了让 Forward 的
// 测试不起 etcd 就能拿到完整上下文。
func Build(c config.Config, dial func(endpoint string) (*grpc.ClientConn, error)) (*ServiceContext, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	zoneScoped, err := c.ResolveZoneScopedNodeTypes()
	if err != nil {
		return nil, err
	}
	if dial == nil {
		dial = discovery.DialEndpoint
	}

	targets := make(map[base.ENodeType]*discovery.NodeWatcher)
	for _, nodeType := range TargetNodeTypes(game.RouteTable) {
		// 前缀与 noderegistry.rpcPrefix 同一写法:<枚举名>.rpc/,gate 与各 Go 服务都按它注册。
		name := base.ENodeType_name[int32(nodeType)]
		targets[nodeType] = discovery.NewNodeWatcher(name, name+".rpc/",
			metrics.SetTargets,
			func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) })
	}

	return &ServiceContext{
		Config:         c,
		ZoneScoped:     zoneScoped,
		Targets:        targets,
		Dial:           dial,
		ForwardTimeout: c.ForwardTimeout(),
	}, nil
}

// NewServiceContext 是生产入口:Build + etcd 客户端。任何一步失败直接 panic
// (配置错 / etcd 不可达都不该带病起服,与 match 同口径)。
func NewServiceContext(c config.Config) *ServiceContext {
	sc, err := Build(c, nil)
	if err != nil {
		panic("client_rpc_router 配置非法: " + err.Error())
	}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}
	sc.Etcd = etcdCli
	return sc
}

// RunWatchers 为每个目标节点类型起一个 list-watch goroutine(panic 由 safego 兜住)。
func (sc *ServiceContext) RunWatchers(ctx context.Context) {
	for nodeType, watcher := range sc.Targets {
		w := watcher
		safego.Go("client_rpc_router.watch."+base.ENodeType_name[int32(nodeType)], func() {
			w.Run(ctx, sc.Etcd)
		})
	}
}

// IsZoneScoped 报告目标节点类型是否只挑同 zone 实例。
func (sc *ServiceContext) IsZoneScoped(nodeType base.ENodeType) bool {
	_, ok := sc.ZoneScoped[nodeType]
	return ok
}
