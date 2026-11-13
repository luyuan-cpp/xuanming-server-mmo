// Package svc 是路由服的装配层:配置、etcd 客户端、按目标节点类型的发现镜像与拨号函数。
package svc

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"client_rpc_router/generated/pb/game"
	"client_rpc_router/internal/config"
	"client_rpc_router/internal/discovery"
	"client_rpc_router/internal/metrics"

	pb "proto/client_rpc_router"
	base "proto/common/base"
	loginproto "proto/login"

	"shared/etcdwatch"
	"shared/killswitch"
	"shared/releasetrack"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
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

	// Peers 是路由服自己这类节点(ClientRpcRouterNodeService)的发现镜像,只用来算本路由服在同轨道路由服里的名次,
	// 决定全服池目标的挑选范围(discovery/subset.go)。不拨号、不进转发。
	Peers *discovery.NodeWatcher

	// KillSwitch 是热关停(docs/design/rpc-kill-switch.md §7)。路由服按**目标方法**(路由表里的 FullMethod)判,
	// 不按本跳的 Forward 判:Forward 只是转发信封,受保护(通配规则不覆盖),与 scene 的 ProcessClientPlayerMessage 同理。
	// 会话收尾 LeaveGame / Disconnect(gate 合成的断线通知也经 Forward)受保护。Build 时建好,规则为空即全放行;
	// 生产入口再 Start(连 etcd)并在注册后 AddInstance。
	KillSwitch *killswitch.Switch

	// 以下由 subsetMu 保护。selfUUID 在注册成功后由 SetSubsetIdentity 设置,为空时全服池目标在全部实例里挑;
	// subsetRank / subsetPeers 是上一次下发给目标 watcher 的名次,没变就不重建索引、不打日志。
	subsetMu    sync.Mutex
	selfUUID    string
	selfTrack   releasetrack.Track
	subsetRank  int
	subsetPeers int
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
	// 空闲回收只关「最后一次取用距今满 IdleConnTimeout」的连接;在途转发用的连接在 ForwardTimeout 之内一定被取用过。
	// 转发期限不短于回收时长时,一次慢转发正用着的连接可能被当成空闲关掉。
	if c.ForwardTimeout() >= discovery.IdleConnTimeout {
		return nil, fmt.Errorf("ForwardTimeoutMs (%d) must be shorter than the idle connection timeout (%s)",
			c.ForwardTimeoutMs, discovery.IdleConnTimeout)
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

	selfName := base.ENodeType_name[int32(base.ENodeType_ClientRpcRouterNodeService)]
	sc := &ServiceContext{
		Config:         c,
		ZoneScoped:     zoneScoped,
		Targets:        targets,
		Dial:           dial,
		ForwardTimeout: c.ForwardTimeout(),
		KillSwitch: killswitch.New(killswitch.Config{
			NodeTypes: []string{selfName},
			Protected: []string{
				pb.ClientRpcRouter_Forward_FullMethodName,
				loginproto.ClientPlayerLogin_LeaveGame_FullMethodName,
				loginproto.ClientPlayerLogin_Disconnect_FullMethodName,
			},
		}),
	}
	// 同类路由服每变一次(上线、下线、定期全量对账)都重算一次名次;onCount 在 watcher 解锁之后调用。
	sc.Peers = discovery.NewNodeWatcher(selfName, selfName+".rpc/",
		func(string, int) { sc.refreshSubsets() }, nil)
	return sc, nil
}

// NewServiceContext 是生产入口:Build + etcd 客户端。任何一步失败直接 panic
// (配置错 / etcd 不可达都不该带病起服,与 match 同口径)。
func NewServiceContext(c config.Config) *ServiceContext {
	sc, err := Build(c, nil)
	if err != nil {
		panic("client_rpc_router invalid configuration: " + err.Error())
	}
	// keepalive:节点镜像的 watch 不会因为半开连接静默停住(etcdwatch 包注释)。
	etcdCli, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	}))
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}
	sc.Etcd = etcdCli
	return sc
}

// RunWatchers 为每个目标节点类型和同类路由服各起一个 list-watch goroutine,并起空闲连接回收(panic 由 safego 兜住)。
func (sc *ServiceContext) RunWatchers(ctx context.Context) {
	for nodeType, watcher := range sc.Targets {
		w := watcher
		safego.Go("client_rpc_router.watch."+base.ENodeType_name[int32(nodeType)], func() {
			w.Run(ctx, sc.Etcd)
		})
	}
	safego.Go("client_rpc_router.watch.peers", func() {
		sc.Peers.Run(ctx, sc.Etcd)
	})
	// 本路由服的挑选范围或分到的 zone 变了之后,不再被挑的目标的连接靠它关掉(discovery.IdleConnTimeout)。
	safego.Loop(ctx, "client_rpc_router.idle_conn_reclaim", discovery.IdleConnSweepInterval, func(context.Context) {
		metrics.ObserveIdleConnSweep(discovery.CloseIdleConns())
	})
}

// SetSubsetIdentity 在本路由服注册成功、拿到 node_uuid 之后调用一次(track 是本进程的发布轨道)。
// 从此全服池目标只在本路由服那段弧上挑(discovery/subset.go)。调用之前在全部实例里挑:只多连几条,不影响正确性;
// 服务在注册之后才开始接请求,生产上这段时间里没有转发。
func (sc *ServiceContext) SetSubsetIdentity(nodeUUID string, track releasetrack.Track) {
	sc.subsetMu.Lock()
	sc.selfUUID = nodeUUID
	sc.selfTrack = track
	sc.subsetMu.Unlock()
	sc.refreshSubsets()
}

// refreshSubsets 按 Peers 镜像的当前内容重算本路由服的名次,有变化就下发给每个全服池目标的 watcher。
// 可能被 Peers 的 watch goroutine 与 SetSubsetIdentity 同时调用:subsetMu 让两次重算依次进行,
// 每次都在锁里读镜像,后完成的那次读到的一定不旧于先完成的,最终下发的是最新的名次。
func (sc *ServiceContext) refreshSubsets() {
	sc.subsetMu.Lock()
	defer sc.subsetMu.Unlock()
	if sc.selfUUID == "" {
		return
	}
	rank, peers := discovery.PeerRank(sc.Peers.Entries(), sc.selfUUID, sc.selfTrack)
	if rank == sc.subsetRank && peers == sc.subsetPeers {
		return
	}
	sc.subsetRank, sc.subsetPeers = rank, peers
	for nodeType, watcher := range sc.Targets {
		if sc.IsZoneScoped(nodeType) {
			continue
		}
		watcher.SetSubset(rank, peers, sc.Config.GlobalTargetSubsetSize)
	}
	logx.Infof("[subset] Global target pick range updated: release_track=%s rank=%d peers=%d subset_size=%d",
		sc.selfTrack, rank, peers, sc.Config.GlobalTargetSubsetSize)
}

// IsZoneScoped 报告目标节点类型是否只挑同 zone 实例。
func (sc *ServiceContext) IsZoneScoped(nodeType base.ENodeType) bool {
	_, ok := sc.ZoneScoped[nodeType]
	return ok
}
