// Package node 按 C++ 约定把 player_locator 注册进 etcd:
// "PlayerLocatorNodeService.rpc/zone/{zoneId}/node_type/{nodeType}/node_id/{nodeId}",值是 protojson 序列化的 NodeInfo。
//
// 本包是 shared/noderegistry 的薄适配层(docs/design/infra-reconnect-overview.md R3、R4,2026-11-19 迁移):
// node_id 的 CAS 分配、租约、keepalive、失租重注册(退避带抖动、可被 Close 打断)、按 uuid 条件注销与注册层指标
// 全部由 shared 实现,语义以 shared 为准(见其包注释「与既有副本的关系」)。这里只做四件事:
//   - 保留 player_locator 的调用面(NewNode / KeepAlive / EtcdClient / Close / Info),并持有注册用的 etcd 客户端;
//   - 按本服务的字段填法生成 NodeInfo(BuildValue 回调);
//   - 把 WithLeaseBoundMarkers 收集的前缀交给 shared 的 Spec.LeaseBoundMarkers:能力键与 NodeInfo 同一笔注册事务、
//     同一个租约,首注册、重夺原号、换号三条路径都带上(docs/design/player-locator-gate-session-index.md §6);
//   - 在碰 etcd 之前读发布轨道,取值非法就返回带环境变量名的错误。
//
// 失租后原 node_id 已归别的实例时换新号继续服务(ReallocateNewID),与迁移前的副本同一语义:player_locator 没有
// 「我必须是 node N」的持久化语义,node_id 只是发现路径里的编号,不喂给任何发号器。
package node

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/config"
	proto_common "proto/common/base"
	"shared/etcdwatch"
	sharedregistry "shared/noderegistry"
	"shared/releasetrack"
)

// Node 是一次成功注册的句柄,只能由 NewNode 构造。
type Node struct {
	// Info 是注册成功那一刻写进 etcd 的 NodeInfo 快照,NodeId / NodeUuid 已回填 shared 分到的值。NodeUuid 进程内
	// 不变,是选主锁值、分片成员值与能力键的身份;NodeId 换号后不更新(shared 会记一行 node_id changed)。调用方只读。
	Info *proto_common.NodeInfo

	client *clientv3.Client
	reg    *sharedregistry.Registration
}

// Option 是 NewNode 的可选项。
type Option func(*nodeOptions)

type nodeOptions struct {
	markerPrefixes []string
}

// WithLeaseBoundMarkers 让每个前缀各多写一个「前缀 + NodeUuid」键,与 NodeInfo 同一个注册事务、同一个租约,
// 随租约到期或 Close 撤销一起消失(shared 的 Spec.LeaseBoundMarkers,约束见其包注释「租约绑定标记」)。
// node 包只把它们当不透明的键,不知道业务含义;player_locator 用它登记「会话按 gate 建索引」的能力键。
func WithLeaseBoundMarkers(prefixes ...string) Option {
	return func(o *nodeOptions) {
		o.markerPrefixes = append(o.markerPrefixes, prefixes...)
	}
}

func rpcPrefix(nodeType uint32) string {
	return proto_common.ENodeType_name[int32(nodeType)] + ".rpc"
}

// NewNode 注册本进程并返回句柄;返回的句柄尚未续租,调用方随后必须 KeepAlive()。
//
// 发布轨道在第一步读,不靠入口传参:取值非法时在碰 etcd 之前就返回错误,进程随之拒启;漏写轨道的 canary 进程会被
// 当成 stable(与 shared/noderegistry 同一理由)。其余校验以 shared 为准:ZoneId、port 为 0、LeaseTTL ≤ 0、
// 标记前缀为空或重复都拒注册,etcd 里不留任何东西。
func NewNode(nodeType uint32, ip string, port uint32, opts ...Option) (*Node, error) {
	track, err := releasetrack.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("release track: %w", err)
	}

	cfg := config.AppConfig
	var o nodeOptions
	for _, opt := range opts {
		opt(&o)
	}

	// keepalive:续租流、热关停 watch 与会话对账都走这个客户端,不能卡在半开连接上(etcdwatch 包注释,R1)。
	client, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{
		Endpoints:   cfg.Registry.Etcd.Hosts,
		DialTimeout: cfg.Registry.Etcd.DialTimeout,
	}))
	if err != nil {
		return nil, fmt.Errorf("etcd connect failed: %w", err)
	}

	template := newNodeInfo(nodeType, cfg.Node.ZoneId, ip, port, track)
	reg, err := sharedregistry.Register(context.Background(), client, sharedregistry.Spec{
		Prefix:            rpcPrefix(nodeType),
		NodeType:          nodeType,
		ZoneId:            cfg.Node.ZoneId,
		LeaseTTL:          cfg.Node.LeaseTTL,
		BuildValue:        nodeInfoValueBuilder(template),
		OnReclaimFailed:   sharedregistry.ReallocateNewID,
		LeaseBoundMarkers: o.markerPrefixes,
	})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	info := proto.Clone(template).(*proto_common.NodeInfo)
	info.NodeId = reg.NodeID()
	info.NodeUuid = reg.NodeUUID
	return &Node{Info: info, client: client, reg: reg}, nil
}

// newNodeInfo 构造本进程写进 etcd 的 NodeInfo 模板,node_id 与 node_uuid 由 shared 分配后经 BuildValue 写入。
// 抽成函数是为了单测不连 etcd 就能核对轨道;换号重注册时以同一个模板重建注册值,轨道不会丢。
// GrpcEndpoint 与 Endpoint 同值:shared 拒绝 grpcEndpoint 端口为 0 的注册值,路由服也只从它选 gRPC 目标。
func newNodeInfo(nodeType, zoneId uint32, ip string, port uint32, track releasetrack.Track) *proto_common.NodeInfo {
	return &proto_common.NodeInfo{
		NodeType: nodeType,
		Endpoint: &proto_common.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		GrpcEndpoint: &proto_common.EndpointComp{Ip: ip, Port: port},
		ZoneId:       zoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(proto_common.ENodeProtocolType_PROTOCOL_GRPC),
		NodeUuid:     uuid.New().String(),
		ReleaseTrack: track.String(),
	}
}

// nodeInfoValueBuilder 返回 shared 的 BuildValue 回调:克隆模板,写入 shared 给的 nodeId / nodeUuid,
// 按 protojson 默认的 lowerCamelCase 字段名序列化(与 C++ MessageToJsonString 一致)。
// 注册时在调用方 goroutine、换号时在 keepalive goroutine 上调用;模板构造后只读,不与 Info 共用对象。
func nodeInfoValueBuilder(template *proto_common.NodeInfo) func(nodeID uint32, nodeUUID string) ([]byte, error) {
	return func(nodeID uint32, nodeUUID string) ([]byte, error) {
		info := proto.Clone(template).(*proto_common.NodeInfo)
		info.NodeId = nodeID
		info.NodeUuid = nodeUUID
		return protojson.Marshal(info)
	}
}

// KeepAlive 启动后台续租;失租后按 shared 的语义重注册:先 CAS 重夺原 node_id,原号已归别的实例就换新号,
// 能力键随新租约一起重写。幂等,Close 之后调用无效果。
func (n *Node) KeepAlive() {
	n.reg.KeepAlive()
}

// EtcdClient 暴露本节点注册用的 etcd 客户端,供同进程内其他需要 etcd 的组件复用(shared/killswitch 的规则 watch、
// 会话对账的集群视图)。
//
// 刻意只读不转移所有权:客户端的生命周期仍由 Node 管,Close() 时一并关闭。复用而不是另开一条连接,是因为再建一个
// clientv3 就多一份 TCP 连接、一份 keep-alive 心跳和一份要独立收尾的资源,而它们连的是同一个集群。
//
// 调用方必须能接受返回 nil(Node 未初始化)—— killswitch 对 nil 客户端的语义就是"全部放行",这正是 fail-open 想要的。
func (n *Node) EtcdClient() *clientv3.Client {
	if n == nil {
		return nil
	}
	return n.client
}

// Close 停止续租,分配键仍归本实例(uuid 相同)时删除双 key,再撤销租约(能力键随之消失;shared 自带时限),
// 最后关闭 etcd 客户端。先删 NodeInfo 再撤租约,中间只会出现「有能力键没 NodeInfo」,对账 leader 不会把它判成旧写者。
func (n *Node) Close() error {
	n.reg.Close()
	return n.client.Close()
}
