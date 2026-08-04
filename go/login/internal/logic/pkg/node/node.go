// internal/node/node.go
package node

import (
	"context"
	"fmt"
	"login/internal/config"
	etcd "login/internal/logic/pkg/etcd"
	login_proto "proto/common/base"
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

func NewNode(nodeType uint32, ip string, port uint32, ttl int64) *Node {
	client, err := etcd.NewClient()
	if err != nil {
		return nil
	}

	reg, err := etcd.NewNodeRegistry(client, ttl)
	if err != nil {
		return nil
	}

	info := &login_proto.NodeInfo{
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
		ZoneId:       config.AppConfig.Node.ZoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(login_proto.ENodeProtocolType_PROTOCOL_GRPC),
		NodeUuid:     uuid.New().String(),
	}

	allocator := NewNodeAllocator(client, GetRpcPrefix(nodeType)).WithRegistry(reg)
	nodeID, err := allocator.TryAllocateNodeID(context.Background(), info, reg.Lease)
	if err != nil {
		return nil
	}

	info.NodeId = uint32(nodeID)

	return &Node{
		Info:   info,
		reg:    reg,
		Client: client,
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
	if _, err := n.Client.Txn(context.Background()).
		Then(clientv3.OpDelete(infoKey), clientv3.OpDelete(allocKey)).
		Commit(); err != nil {
		logx.Errorf("Failed to delete node keys from etcd: %v", err)
		return fmt.Errorf("failed to delete node keys: %v", err)
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

// KeepAlive starts a cancellable keep-alive loop.
func (n *Node) KeepAlive() error {
	n.keepAliveCtx, n.cancelFunc = context.WithCancel(context.Background())
	n.reg.KeepAlive(n.keepAliveCtx)
	return nil
}
