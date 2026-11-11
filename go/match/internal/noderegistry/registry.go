// Package noderegistry 按 C++ 约定把 Match 注册进 etcd:C++ 节点 watch
// "MatchNodeService.rpc/zone/{zoneId}/node_type/{nodeType}/node_id/{nodeId}",
// 值是 protojson 序列化的 NodeInfo。
//
// 本包是 shared/noderegistry 的薄适配层(docs/design/infra-reconnect-overview.md R3、R4):
// node_id 的 CAS 分配、租约、keepalive、失租重注册、按 uuid 条件注销全部由 shared 实现,
// 语义以 shared 为准(见其包注释「与既有副本的关系」)。这里只做三件事:
//   - 保留本服务既有的调用面(Register / KeepAlive / Close / Info),调用方不用改;
//   - 按本服务的字段填法生成 NodeInfo(BuildValue 回调);
//   - 在碰 etcd 之前读发布轨道,取值非法就返回带环境变量名的错误。
//
// 失租后原 node_id 已归别的实例时换新号继续服务(ReallocateNewID),与迁移前的副本同一语义:
// match 不以 node_id 派生任何持久身份或 per-node topic,换号只让发现键路径变一下。
// 本包只注册 MatchNodeService;同进程的 TeamNodeService 由 match_service.go 直接调 shared/noderegistry。
//
// scene_manager / client_rpc_router / data_service 的 internal/noderegistry 是同形的适配层,改一处要同步看另外三处。
package noderegistry

import (
	"context"
	"fmt"
	"time"

	base "proto/common/base"
	sharedregistry "shared/noderegistry"
	"shared/releasetrack"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NodeRegistration 是一次成功注册的句柄,只能由 Register 构造。
type NodeRegistration struct {
	// Info 是注册成功那一刻写进 etcd 的 NodeInfo 快照,NodeId / NodeUuid 已回填 shared 分到的值,
	// 供启动横幅与日志使用。它不随后续换号更新:失租后原号被别的实例占用时 shared 会换新 node_id,
	// Info.NodeId 仍是注册时的号(换号由 OnNodeIDChanged 记日志);NodeUuid 进程内不变。
	// 调用方只读,不要改它,也不要拿它当「当前 node_id」的权威来源。
	Info *base.NodeInfo

	reg *sharedregistry.Registration
}

// rpcPrefix 返回节点类型的 etcd key 前缀,如 "MatchNodeService.rpc"(由枚举名派生,不手写)。
func rpcPrefix(nodeType uint32) string {
	return base.ENodeType_name[int32(nodeType)] + ".rpc"
}

// Register 分配 node_id 并写入 NodeInfo 与分配键;返回的句柄尚未续租,调用方随后必须 KeepAlive()。
//
// 发布轨道在这里先读,不靠入口传参:取值非法时在碰 etcd 之前就返回错误,进程随之拒启;漏写轨道的
// canary 进程会被当成 stable、接到 stable 的流量,所以不留这个口子。shared 会再读同一个环境变量,
// 并要求注册值里的轨道与它一致。
//
// 其余校验以 shared 为准:ZoneId 为 0、port 为 0、ttl ≤ 0 都拒注册,etcd 里不留任何东西。
func Register(cli *clientv3.Client, nodeType, zoneId uint32, ip string, port uint32, ttl int64) (*NodeRegistration, error) {
	track, err := releasetrack.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("release track: %w", err)
	}
	template := newNodeInfo(nodeType, zoneId, ip, port, track)
	prefix := rpcPrefix(nodeType)
	reg, err := sharedregistry.Register(context.Background(), cli, sharedregistry.Spec{
		Prefix:          prefix,
		NodeType:        nodeType,
		ZoneId:          zoneId,
		LeaseTTL:        ttl,
		BuildValue:      nodeInfoValueBuilder(template),
		OnReclaimFailed: sharedregistry.ReallocateNewID,
		OnNodeIDChanged: func(oldID, newID uint32) {
			logx.Errorf("[NodeRegistry %s] node_id changed %d -> %d after etcd lease loss; NodeRegistration.Info still reports the registration-time node_id=%d",
				prefix, oldID, newID, oldID)
		},
	})
	if err != nil {
		return nil, err
	}
	info := proto.Clone(template).(*base.NodeInfo)
	info.NodeId = reg.NodeID()
	info.NodeUuid = reg.NodeUUID
	return &NodeRegistration{Info: info, reg: reg}, nil
}

// newNodeInfo 构造本进程写进 etcd 的 NodeInfo 模板,node_id 由 shared 分配后填。
// 抽成函数是为了单测不连 etcd 就能核对轨道;换号重注册时 BuildValue 以同一个模板重建注册值,轨道不会丢。
// NodeUuid 先填一个随机值让模板自身完整;真正的注册身份由 shared 分配,BuildValue 用它覆盖,
// Register 成功后 Info 回填同一个值。
func newNodeInfo(nodeType, zoneId uint32, ip string, port uint32, track releasetrack.Track) *base.NodeInfo {
	return &base.NodeInfo{
		NodeType: nodeType,
		Endpoint: &base.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		GrpcEndpoint: &base.EndpointComp{
			Ip:   ip,
			Port: port,
		},
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
func (nr *NodeRegistration) KeepAlive() {
	nr.reg.KeepAlive()
}

// Close 停止续租,分配键仍归本实例(uuid 相同)时删除双 key,再 Revoke 租约。幂等。
// 必须先于 gRPC Stop 调用:先从发现里消失,在途请求还能处理完。
func (nr *NodeRegistration) Close() {
	nr.reg.Close()
}
