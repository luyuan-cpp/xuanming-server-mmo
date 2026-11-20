// Package node 按 C++ 约定把 guild 注册进 etcd:C++ 节点与路由服 watch
// "GuildNodeService.rpc/zone/{zoneId}/node_type/{nodeType}/node_id/{nodeId}",值是 protojson 序列化的 NodeInfo。
//
// 本包是 shared/noderegistry 的薄适配层(docs/design/infra-reconnect-overview.md R3、R4,2026-11-19 迁移):
// node_id 的 CAS 分配、租约、keepalive、失租重注册(退避带抖动、可被 Close 打断)、按 uuid 条件注销与注册层指标
// 全部由 shared 实现,语义以 shared 为准(见其包注释「与既有副本的关系」)。这里只做三件事:
//   - 保留 guild 的调用面(NewNode / KeepAlive / Close / Info),并持有注册用的 etcd 客户端,Close 时一并关闭;
//   - 按 guild 的字段填法生成 NodeInfo(BuildValue 回调);
//   - 在碰 etcd 之前读发布轨道,取值非法就返回带环境变量名的错误。
//
// 失租后原 node_id 已归别的实例时换新号继续服务(ReallocateNewID),与迁移前的副本同一语义:node_id 只是发现路径里的
// 编号,guild_id 等永久 ID 由 data_service 发号器发,guild 不以 node_id 派生任何持久身份或 per-node topic。
package node

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"guild/internal/config"
	base "proto/common/base"
	"shared/etcdwatch"
	sharedregistry "shared/noderegistry"
	"shared/releasetrack"
)

// Node 是一次成功注册的句柄,只能由 NewNode 构造。
type Node struct {
	// Info 是注册成功那一刻写进 etcd 的 NodeInfo 快照,NodeId / NodeUuid 已回填 shared 分到的值,供启动日志与
	// killswitch 认实例用。换号后不更新(shared 会记一行 node_id changed);NodeUuid 进程内不变。调用方只读。
	Info *base.NodeInfo

	client *clientv3.Client
	reg    *sharedregistry.Registration
}

// rpcPrefix 是该 node_type 在 etcd 里的根前缀,例如 "GuildNodeService.rpc"(由枚举名派生,不手写)。
func rpcPrefix(nodeType uint32) string {
	return base.ENodeType_name[int32(nodeType)] + ".rpc"
}

// NewNode 注册本进程并返回句柄;返回的句柄尚未续租,调用方随后必须 KeepAlive()。
//
// 发布轨道在第一步读,不靠入口传参:取值非法时在碰 etcd 之前就返回错误,进程随之拒启;漏写轨道的 canary 进程会被
// 当成 stable,接到 stable 的流量(与 shared/noderegistry 同一理由)。其余校验以 shared 为准:ZoneId、port 为 0
// 或 LeaseTTL ≤ 0 都拒注册,etcd 里不留任何东西。
func NewNode(nodeType uint32, ip string, port uint32) (*Node, error) {
	track, err := releasetrack.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("release track: %w", err)
	}

	cfg := config.AppConfig
	// keepalive:续租流与重注册不会卡在半开连接上(etcdwatch 包注释,R1)。
	client, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{
		Endpoints:   cfg.Registry.Etcd.Hosts,
		DialTimeout: cfg.Registry.Etcd.DialTimeout,
	}))
	if err != nil {
		return nil, fmt.Errorf("etcd connect failed: %w", err)
	}

	template := newNodeInfo(nodeType, cfg.Node.ZoneId, ip, port, track)
	reg, err := sharedregistry.Register(context.Background(), client, sharedregistry.Spec{
		Prefix:          rpcPrefix(nodeType),
		NodeType:        nodeType,
		ZoneId:          cfg.Node.ZoneId,
		LeaseTTL:        cfg.Node.LeaseTTL,
		BuildValue:      nodeInfoValueBuilder(template),
		OnReclaimFailed: sharedregistry.ReallocateNewID,
	})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	info := proto.Clone(template).(*base.NodeInfo)
	info.NodeId = reg.NodeID()
	info.NodeUuid = reg.NodeUUID
	return &Node{Info: info, client: client, reg: reg}, nil
}

// newNodeInfo 构造本进程写进 etcd 的 NodeInfo 模板,node_id 与 node_uuid 由 shared 分配后经 BuildValue 写入。
// 抽成函数是为了单测不连 etcd 就能核对轨道;换号重注册时以同一个模板重建注册值,轨道不会丢。
func newNodeInfo(nodeType, zoneId uint32, ip string, port uint32, track releasetrack.Track) *base.NodeInfo {
	return &base.NodeInfo{
		NodeType: nodeType,
		Endpoint: &base.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		// 路由服只从 grpcEndpoint 选择 gRPC 目标；Endpoint 保留既有服务发现兼容字段。
		GrpcEndpoint: &base.EndpointComp{Ip: ip, Port: port},
		ZoneId:       zoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC),
		NodeUuid:     uuid.New().String(),
		ReleaseTrack: track.String(),
	}
}

// nodeInfoValueBuilder 返回 shared 的 BuildValue 回调:克隆模板,写入 shared 给的 nodeId / nodeUuid,
// 按 protojson 默认的 lowerCamelCase 字段名序列化(与 C++ MessageToJsonString 一致)。
// 注册时在调用方 goroutine、换号时在 keepalive goroutine 上调用;模板构造后只读,不与 Info 共用对象。
func nodeInfoValueBuilder(template *base.NodeInfo) func(nodeID uint32, nodeUUID string) ([]byte, error) {
	return func(nodeID uint32, nodeUUID string) ([]byte, error) {
		info := proto.Clone(template).(*base.NodeInfo)
		info.NodeId = nodeID
		info.NodeUuid = nodeUUID
		return protojson.Marshal(info)
	}
}

// KeepAlive 启动后台续租;失租后按 shared 的语义重注册:先 CAS 重夺原 node_id,原号已归别的实例就换新号。
// 幂等,Close 之后调用无效果。
func (n *Node) KeepAlive() {
	n.reg.KeepAlive()
}

// Close 停止续租,分配键仍归本实例(uuid 相同)时删除双 key,再撤销租约(shared 自带时限,停机不会被 etcd 卡住),
// 最后关闭注册用的 etcd 客户端。必须先于 gRPC Stop 调用:先从发现里消失,在途请求还能处理完。
func (n *Node) Close() error {
	n.reg.Close()
	return n.client.Close()
}
