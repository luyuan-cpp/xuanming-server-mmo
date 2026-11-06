// internal/node/node.go
package node

import (
	"context"
	"fmt"
	"login/internal/config"
	etcd "login/internal/logic/pkg/etcd"
	login_proto "proto/common/base"
	"shared/releasetrack"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// NewEtcdClient creates a new etcd client using the shared config.
func NewEtcdClient() (*clientv3.Client, error) {
	return etcd.NewClient()
}

type Node struct {
	Info         *login_proto.NodeInfo
	reg          *etcd.NodeRegistry
	Client       *clientv3.Client
	cancelFunc   context.CancelFunc // Cancels the KeepAlive goroutine
	keepAliveCtx context.Context    // Controls the KeepAlive lifecycle
}

// GetRpcPrefix returns the RPC path prefix for a given NodeType.
func GetRpcPrefix(nodeType uint32) string {
	return fmt.Sprintf("%s.rpc", login_proto.ENodeType_name[int32(nodeType)])
}

// BuildRpcPrefix generates the path without node_id.
func BuildRpcPrefix(serviceName string, zoneId, nodeType uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/", serviceName, zoneId, nodeType)
}

// BuildRpcPath generates the full path including node_id.
func BuildRpcPath(serviceName string, zoneId, nodeType, nodeId uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/node_id/%d", serviceName, zoneId, nodeType, nodeId)
}

// NewNode 注册本进程,任何一步失败都返回 nil,调用方随后拒启。
//
// 发布轨道(docs/design/release-track-gray-canary.md §2.1)在第一步读,不靠入口传参:取值非法时在碰 etcd 之前
// 就放弃注册;漏写轨道的 canary 进程会被当成 stable(与 shared/noderegistry 同一理由)。本函数出错只回 nil,
// 所以拒绝原因记在日志里。失租重注册重放的是分配时写下的同一份值,轨道不会丢。
func NewNode(nodeType uint32, ip string, port uint32, ttl int64) *Node {
	track, err := releasetrack.FromEnv()
	if err != nil {
		logx.Errorf("login node: refusing to register, invalid release track: %v", err)
		return nil
	}

	client, err := etcd.NewClient()
	if err != nil {
		return nil
	}

	reg, err := etcd.NewNodeRegistry(client, ttl)
	if err != nil {
		return nil
	}

	info := newNodeInfo(nodeType, config.AppConfig.Node.ZoneId, ip, port, track)

	allocator := NewNodeAllocator(client, GetRpcPrefix(nodeType)).WithRegistry(reg)
	nodeID, err := allocator.TryAllocateNodeID(context.Background(), info, reg.Lease)
	if err != nil {
		return nil
	}

	info.NodeId = uint32(nodeID)
	logx.Infof("login node registered: zone=%d node_type=%d node_id=%d uuid=%s release_track=%s",
		info.ZoneId, nodeType, info.NodeId, info.NodeUuid, track)

	return &Node{
		Info:   info,
		reg:    reg,
		Client: client,
	}
}

// newNodeInfo 构造本进程写进 etcd 的 NodeInfo,node_id 由随后的分配步骤填。
// 抽成函数是为了单测不连 etcd 就能核对轨道。
func newNodeInfo(nodeType, zoneId uint32, ip string, port uint32, track releasetrack.Track) *login_proto.NodeInfo {
	return &login_proto.NodeInfo{
		NodeId:   uint32(time.Now().UnixNano()),
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

// Close stops the KeepAlive goroutine, deletes the node key + global
// allocation key, and revokes the lease so the slot is reusable immediately.
func (n *Node) Close() error {
	if n.cancelFunc != nil {
		n.cancelFunc()
	}

	prefix := GetRpcPrefix(n.Info.NodeType)
	infoKey := BuildRpcPath(prefix, n.Info.ZoneId, n.Info.NodeType, n.Info.NodeId)
	allocKey := buildAllocationKey(prefix, n.Info.NodeType, n.Info.NodeId)

	// Delete both atomically: the per-zone info key (consumed by service
	// discovery watchers) and the global allocation key (guards uniqueness
	// across zones). Falling back to lease revocation handles either-or.
	// 只删本实例的键:失租后原 node_id 可能已被别的实例接管、本进程还没退出就停机,无条件删会把对方的发现键删掉,
	// 而对方不 watch 自己的键,会一直不可见(docs/design/infra-reconnect-overview.md §4 R3,与 shared/noderegistry 同一做法)。
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	resp, err := n.Client.Txn(ctx).
		If(clientv3.Compare(clientv3.Value(allocKey), "=", n.Info.NodeUuid)).
		Then(clientv3.OpDelete(infoKey), clientv3.OpDelete(allocKey)).
		Commit()
	if err != nil {
		logx.Errorf("Failed to delete node keys from etcd: %v", err)
		return fmt.Errorf("failed to delete node keys: %v", err)
	}
	if !resp.Succeeded {
		logx.Infof("Node keys of node_id=%d no longer belong to this instance; left in place", n.Info.NodeId)
	}

	// Revoke the lease — defense in depth: even if the deletes above are
	// partially observed, lease revoke removes any leftovers.
	err := n.reg.RevokeLease()
	if err != nil {
		logx.Errorf("Failed to revoke lease: %v", err)
		return fmt.Errorf("failed to revoke lease: %v", err)
	}

	return nil
}

// closeTimeout 是 Close 里删键的预算:停机不能被 etcd 卡住。
const closeTimeout = 5 * time.Second

// KeepAlive starts a cancellable keep-alive loop.
func (n *Node) KeepAlive() error {
	n.keepAliveCtx, n.cancelFunc = context.WithCancel(context.Background())
	n.reg.KeepAlive(n.keepAliveCtx)
	return nil
}
