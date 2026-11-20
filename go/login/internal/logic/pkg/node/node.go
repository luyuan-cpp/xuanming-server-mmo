// Package node 把 login 注册进 etcd(C++ gate 按 "LoginNodeService.rpc/zone/{zoneId}/node_type/{nodeType}/node_id/{nodeId}"
// 发现 login),并提供 gate 目录 watch 用的前缀与客户端工厂。
//
// 注册是 shared/noderegistry 的薄适配层(docs/design/infra-reconnect-overview.md R3、R4,2026-11-19 迁移):
// node_id 的 CAS 分配、租约、keepalive、失租重注册(退避带抖动、可被 Close 打断)、按 uuid 条件注销与注册层指标
// 全部由 shared 实现,语义以 shared 为准(见其包注释「与既有副本的关系」)。这里只保留 login 的调用面、NodeInfo 的
// 填法,并在碰 etcd 之前读发布轨道。
//
// 失租后原 node_id 已归别的实例时退出进程(ExitProcess),与迁移前同一语义:交给编排器重启、拿新号,不在进程内换号。
// 迁移前的分配器从 0 开始分号,0 在 C++ 与 Kafka 命令过滤里都是「未分配」的哨兵;shared 从 1 开始,这一处随迁移改正。
package node

import (
	"context"
	"fmt"
	"login/internal/config"
	etcd "login/internal/logic/pkg/etcd"
	login_proto "proto/common/base"
	sharedregistry "shared/noderegistry"
	"shared/releasetrack"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NewEtcdClient creates a new etcd client using the shared config.
func NewEtcdClient() (*clientv3.Client, error) {
	return etcd.NewClient()
}

// Node 是一次成功注册的句柄,只能由 NewNode 构造。
type Node struct {
	// Info 是注册成功那一刻写进 etcd 的 NodeInfo 快照,NodeId / NodeUuid 已回填 shared 分到的值。
	// login 不在进程内换号(原号被占即退出),所以 NodeId 在进程生命周期里不变。调用方只读。
	Info *login_proto.NodeInfo

	client *clientv3.Client
	reg    *sharedregistry.Registration
}

// GetRpcPrefix returns the RPC path prefix for a given NodeType.
func GetRpcPrefix(nodeType uint32) string {
	return fmt.Sprintf("%s.rpc", login_proto.ENodeType_name[int32(nodeType)])
}

// BuildRpcPrefix generates the path without node_id.
func BuildRpcPrefix(serviceName string, zoneId, nodeType uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/", serviceName, zoneId, nodeType)
}

// NewNode 注册本进程,任何一步失败都返回 nil,调用方随后拒启;拒绝原因记在日志里。返回的句柄尚未续租,
// 调用方随后必须 KeepAlive()。
//
// 发布轨道(docs/design/release-track-gray-canary.md §2.1)在第一步读,不靠入口传参:取值非法时在碰 etcd 之前
// 就放弃注册;漏写轨道的 canary 进程会被当成 stable(与 shared/noderegistry 同一理由)。其余校验以 shared 为准:
// ZoneId、port 为 0 或 ttl ≤ 0 都拒注册,etcd 里不留任何东西。
func NewNode(nodeType uint32, ip string, port uint32, ttl int64) *Node {
	track, err := releasetrack.FromEnv()
	if err != nil {
		logx.Errorf("login node: refusing to register, invalid release track: %v", err)
		return nil
	}

	client, err := etcd.NewClient()
	if err != nil {
		logx.Errorf("login node: refusing to register, etcd client: %v", err)
		return nil
	}

	template := newNodeInfo(nodeType, config.AppConfig.Node.ZoneId, ip, port, track)
	reg, err := sharedregistry.Register(context.Background(), client, sharedregistry.Spec{
		Prefix:          GetRpcPrefix(nodeType),
		NodeType:        nodeType,
		ZoneId:          config.AppConfig.Node.ZoneId,
		LeaseTTL:        ttl,
		BuildValue:      nodeInfoValueBuilder(template),
		OnReclaimFailed: sharedregistry.ExitProcess,
	})
	if err != nil {
		logx.Errorf("login node: refusing to register: %v", err)
		_ = client.Close()
		return nil
	}

	info := proto.Clone(template).(*login_proto.NodeInfo)
	info.NodeId = reg.NodeID()
	info.NodeUuid = reg.NodeUUID
	return &Node{Info: info, client: client, reg: reg}
}

// newNodeInfo 构造本进程写进 etcd 的 NodeInfo 模板,node_id 与 node_uuid 由 shared 分配后经 BuildValue 写入。
// 抽成函数是为了单测不连 etcd 就能核对轨道。
func newNodeInfo(nodeType, zoneId uint32, ip string, port uint32, track releasetrack.Track) *login_proto.NodeInfo {
	return &login_proto.NodeInfo{
		NodeType: nodeType,
		Endpoint: &login_proto.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		GrpcEndpoint: &login_proto.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		ZoneId:       zoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(login_proto.ENodeProtocolType_PROTOCOL_GRPC),
		NodeUuid:     uuid.New().String(),
		ReleaseTrack: track.String(),
	}
}

// nodeInfoValueBuilder 返回 shared 的 BuildValue 回调:克隆模板,写入 shared 给的 nodeId / nodeUuid,
// 按 protojson 默认的 lowerCamelCase 字段名序列化(与 C++ MessageToJsonString 一致)。模板构造后只读。
func nodeInfoValueBuilder(template *login_proto.NodeInfo) func(nodeID uint32, nodeUUID string) ([]byte, error) {
	return func(nodeID uint32, nodeUUID string) ([]byte, error) {
		info := proto.Clone(template).(*login_proto.NodeInfo)
		info.NodeId = nodeID
		info.NodeUuid = nodeUUID
		return protojson.Marshal(info)
	}
}

// KeepAlive 启动后台续租;失租后先 CAS 重夺原 node_id,原号已归别的实例就退出进程。幂等,Close 之后调用无效果。
func (n *Node) KeepAlive() {
	n.reg.KeepAlive()
}

// Close 停止续租,分配键仍归本实例(uuid 相同)时删除双 key,再撤销租约让 node_id 立即可复用(shared 自带时限,
// 停机不会被 etcd 卡住),最后关闭注册用的 etcd 客户端。
func (n *Node) Close() error {
	n.reg.Close()
	return n.client.Close()
}
