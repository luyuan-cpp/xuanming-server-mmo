package node

import (
	"context"
	"fmt"
	login_proto "proto/common/base"

	"login/internal/logic/pkg/etcd"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

type NodeAllocator struct {
	Client *clientv3.Client
	Prefix string
	// Registry 用来把 CAS 直写的 allocKey / infoKey 补登记进重注册清单。
	// 可为 nil(测试),此时只是失去失租后的重注册能力。
	Registry *etcd.NodeRegistry
}

func NewNodeAllocator(client *clientv3.Client, prefix string) *NodeAllocator {
	return &NodeAllocator{Client: client, Prefix: prefix}
}

// WithRegistry 绑定 NodeRegistry,让分配成功的 key 进入重注册清单。
func (na *NodeAllocator) WithRegistry(reg *etcd.NodeRegistry) *NodeAllocator {
	na.Registry = reg
	return na
}

// buildAllocationKey 返回 (nodeType, nodeID) 的全局唯一 CAS 键。键里刻意不带 zone,
// 两个 zone 不可能同时占到同一对 (nodeType, nodeID)。最早是为了堵 snowflake 种子撞号:
// 当时 zone 3、zone 4 的 login 在 z3 租约短暂丢失后都拿到了 node_id=1。
func buildAllocationKey(prefix string, nodeType, nodeID uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/%d", prefix, nodeType, nodeID)
}

// TryAllocateNodeID atomically assigns a globally-unique node_id for the given
// (node_type) across all zones. Reuses free slots (lease expired) by scanning
// the allocation registry for gaps.
//
// 不变量:同一 node_type 下,所有活着的节点 node_id 互不相同,不分 zone。这条最早是给
// snowflake PlayerId 种子定的;PlayerId 现在只由发号器发(svc/player_id_minter.go),
// 与 node_id 无关,不变量照旧保留。
func (na *NodeAllocator) TryAllocateNodeID(ctx context.Context, info *login_proto.NodeInfo, leaseID clientv3.LeaseID) (uint32, error) {
	// Scan the global allocation registry for this node_type. We use a
	// dedicated sub-prefix so this lookup is O(live nodes of one type),
	// not O(all services + all zones + all node_ids).
	allocPrefix := fmt.Sprintf("%s/allocated/node_type/%d/node_id/", na.Prefix, info.NodeType)
	resp, err := na.Client.Get(ctx, allocPrefix, clientv3.WithPrefix())
	if err != nil {
		return 0, err
	}

	usedIDs := make(map[uint32]bool)
	maxID := uint32(0)
	for _, kv := range resp.Kvs {
		var id uint32
		// Key tail after allocPrefix is the numeric node_id.
		if _, err := fmt.Sscanf(string(kv.Key[len(allocPrefix):]), "%d", &id); err != nil {
			continue
		}
		usedIDs[id] = true
		if id > maxID {
			maxID = id
		}
	}

	searchRange := maxID + 10

	// Search upward for an available ID. CAS on the global allocation key
	// protects against two concurrently-starting nodes racing on the same id.
	for id := uint32(0); id < searchRange; id++ {
		if usedIDs[id] {
			continue
		}
		ok, err := na.putIfAbsent(ctx, id, info, leaseID)
		if err != nil {
			continue
		}
		if ok {
			return id, nil
		}
	}

	return 0, fmt.Errorf("failed to allocate node ID")
}

// putIfAbsent atomically claims (nodeType, nodeID) globally, then publishes the
// full per-zone NodeInfo record under the same lease so both evaporate together
// when the node dies.
//
// Both writes share the same leaseID, so a partial failure (e.g. process killed
// between them) is self-cleaning: the allocation key expires with the lease
// and the slot becomes reusable on next startup scan.
func (na *NodeAllocator) putIfAbsent(ctx context.Context, nodeID uint32, info *login_proto.NodeInfo, leaseID clientv3.LeaseID) (bool, error) {
	allocKey := buildAllocationKey(na.Prefix, info.NodeType, nodeID)
	infoKey := BuildRpcPath(na.Prefix, info.ZoneId, info.NodeType, nodeID)

	info.NodeId = nodeID
	result, err := protojson.Marshal(info)
	if err != nil {
		logx.Errorf("Error marshaling: %v", err)
		return false, err
	}
	resultStr := string(result)

	// The allocation key's value is the node uuid, purely for diagnostics —
	// the key's existence is what guards uniqueness.
	txn := na.Client.Txn(ctx)
	txnResp, err := txn.If(
		clientv3.Compare(clientv3.Version(allocKey), "=", 0),
	).Then(
		clientv3.OpPut(allocKey, info.NodeUuid, clientv3.WithLease(leaseID)),
		clientv3.OpPut(infoKey, resultStr, clientv3.WithLease(leaseID)),
	).Commit()

	if err != nil {
		return false, err
	}
	if txnResp.Succeeded && na.Registry != nil {
		// 这两个 key 是上面那次 Txn 直接写的(CAS 本身就是 node_id 唯一性的闸,不能拆成两次 Put),
		// 绕过了 NodeRegistry.RegisterNode。这里补登记,否则失租后 reRegister 无东西可重放,
		// 节点会从服务发现里永久消失而无人知情。
		na.Registry.TrackKey(allocKey, info.NodeUuid)
		na.Registry.TrackKey(infoKey, resultStr)
	}
	return txnResp.Succeeded, nil
}
