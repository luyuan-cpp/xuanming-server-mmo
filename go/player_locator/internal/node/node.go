package node

// 本文件中的 allocator 逻辑在 guild / friend / player_locator / scene_manager
// 四个服务里是逐字相同的副本(只有 import 包名不同)。
// 若需修改 allocator 行为,请同步修改另外三个服务的对应文件:
//   - go/guild/internal/node/node.go
//   - go/friend/internal/node/node.go
//   - go/scene_manager/internal/noderegistry/registry.go

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"

	"player_locator/internal/config"
	proto_common "proto/common/base"
	"shared/snowflake"
)

// nodeIDMin 是合法 node_id 的下界。0 永远作为 "未分配 / 非法值" 保留。
const nodeIDMin uint32 = 1

// nodeIDMax 与 Snowflake worker id 位宽对齐(shared/snowflake.NodeMask)。
var nodeIDMax = uint32(snowflake.NodeMask)

type Node struct {
	Info       *proto_common.NodeInfo
	client     *clientv3.Client
	leaseID    clientv3.LeaseID
	cancelFunc context.CancelFunc
}

func rpcPrefix(nodeType uint32) string {
	return fmt.Sprintf("%s.rpc", proto_common.ENodeType_name[int32(nodeType)])
}

func rpcPath(prefix string, zoneId, nodeType, nodeId uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/node_id/%d", prefix, zoneId, nodeType, nodeId)
}

// allocationKey 是跨 zone 的全局占位 key —— 路径里不带 zone,
// 因此两个 zone 的实例不可能同时拿到同一个 (node_type, node_id)。
func allocationKey(prefix string, nodeType, nodeID uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/%d", prefix, nodeType, nodeID)
}

func allocationKeyPrefix(prefix string, nodeType uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/", prefix, nodeType)
}

func NewNode(nodeType uint32, ip string, port uint32) (*Node, error) {
	cfg := config.AppConfig

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Registry.Etcd.Hosts,
		DialTimeout: cfg.Registry.Etcd.DialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd connect failed: %w", err)
	}

	grant, err := client.Grant(context.Background(), cfg.Node.LeaseTTL)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("etcd grant failed: %w", err)
	}

	info := &proto_common.NodeInfo{
		NodeType: nodeType,
		Endpoint: &proto_common.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		ZoneId:       cfg.Node.ZoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(proto_common.ENodeProtocolType_PROTOCOL_GRPC),
		NodeUuid:     uuid.New().String(),
	}

	prefix := rpcPrefix(nodeType)
	nodeID, err := allocateNodeID(context.Background(), client, prefix, info, grant.ID)
	if err != nil {
		_, _ = client.Revoke(context.Background(), grant.ID)
		client.Close()
		return nil, err
	}
	info.NodeId = nodeID

	return &Node{
		Info:    info,
		client:  client,
		leaseID: grant.ID,
	}, nil
}

// allocateNodeID 在 (node_type) 下找空闲 node_id 并通过 etcd Txn CAS 占住。
// 详细设计见 go/guild/internal/node/node.go 同名函数。
func allocateNodeID(ctx context.Context, client *clientv3.Client, prefix string, info *proto_common.NodeInfo, leaseID clientv3.LeaseID) (uint32, error) {
	usedIDs, err := scanUsedNodeIDs(ctx, client, prefix, info.NodeType)
	if err != nil {
		return 0, err
	}

	for id := nodeIDMin; id <= nodeIDMax; id++ {
		if usedIDs[id] {
			continue
		}
		ok, err := tryClaimNodeID(ctx, client, prefix, id, info, leaseID)
		if err != nil {
			logx.Errorf("player_locator allocator: txn failed for node_id=%d: %v", id, err)
			continue
		}
		if ok {
			return id, nil
		}
		usedIDs[id] = true
	}
	return 0, fmt.Errorf("no available node_id in [%d, %d]", nodeIDMin, nodeIDMax)
}

func scanUsedNodeIDs(ctx context.Context, client *clientv3.Client, prefix string, nodeType uint32) (map[uint32]bool, error) {
	used := make(map[uint32]bool)

	allocPrefix := allocationKeyPrefix(prefix, nodeType)
	allocResp, err := client.Get(ctx, allocPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get alloc prefix: %w", err)
	}
	for _, kv := range allocResp.Kvs {
		tail := strings.TrimPrefix(string(kv.Key), allocPrefix)
		if id, err := strconv.ParseUint(tail, 10, 32); err == nil {
			used[uint32(id)] = true
		}
	}

	rpcResp, err := client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get rpc prefix: %w", err)
	}
	for _, kv := range rpcResp.Kvs {
		key := string(kv.Key)
		if strings.Contains(key, "/allocated/") {
			continue
		}
		var ni proto_common.NodeInfo
		if err := protojson.Unmarshal(kv.Value, &ni); err != nil {
			continue
		}
		if ni.NodeId >= nodeIDMin {
			used[ni.NodeId] = true
		}
	}

	return used, nil
}

func tryClaimNodeID(ctx context.Context, client *clientv3.Client, prefix string, nodeID uint32, info *proto_common.NodeInfo, leaseID clientv3.LeaseID) (bool, error) {
	allocKey := allocationKey(prefix, info.NodeType, nodeID)
	rpcKey := rpcPath(prefix, info.ZoneId, info.NodeType, nodeID)

	info.NodeId = nodeID
	value, err := protojson.Marshal(info)
	if err != nil {
		return false, fmt.Errorf("marshal node info: %w", err)
	}

	txnResp, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(allocKey), "=", 0)).
		Then(
			clientv3.OpPut(allocKey, info.NodeUuid, clientv3.WithLease(leaseID)),
			clientv3.OpPut(rpcKey, string(value), clientv3.WithLease(leaseID)),
		).
		Commit()
	if err != nil {
		return false, err
	}
	return txnResp.Succeeded, nil
}

func (n *Node) KeepAlive() error {
	ctx, cancel := context.WithCancel(context.Background())
	n.cancelFunc = cancel

	ch, err := n.client.KeepAlive(ctx, n.leaseID)
	if err != nil {
		cancel()
		return fmt.Errorf("keep alive failed: %w", err)
	}

	go n.watchKeepAlive(ctx, ch)
	return nil
}

// watchKeepAlive 消费 keep-alive 应答;channel 关闭(= lease 终态丢失,etcd
// 不可达超过 TTL 或 lease 被撤销)时进入重注册,而不是像旧实现那样只打一条
// INFO 就永远放弃 —— 那会让进程活着但注册已蒸发:登录链路的 etcd 发现再也
// 看不到本实例,且没有任何自愈或告警路径。scene_manager 的同源副本
// (noderegistry/registry.go)早已实现该自愈,这里对齐。
func (n *Node) watchKeepAlive(ctx context.Context, ch <-chan *clientv3.LeaseKeepAliveResponse) {
	for {
		select {
		case ka := <-ch:
			if ka == nil {
				logx.Error("player_locator node lease lost, attempting re-registration")
				n.reRegister(ctx)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// reRegister 在 lease 丢失后重新注册。安全点与 scene_manager 版一致:
//
//	lease 丢失 → 老 allocationKey/rpcKey 已被 etcd 自动清掉 → 期间别的实例
//	可能已抢占同一个 node_id。绝不能盲 Put 老 key(会覆盖别人的 NodeInfo,
//	把流量路由到错误节点);只能 CAS "allocationKey 仍不存在" 重夺,
//	失败就换一个全新 node_id —— player_locator 没有"我必须是 node N"的
//	持久化语义,换 ID 安全(Snowflake worker id 由 shared/snowflakealloc
//	独立分配,与 NodeInfo.NodeId 解耦)。
func (n *Node) reRegister(ctx context.Context) {
	prefix := rpcPrefix(n.Info.NodeType)
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		grant, err := n.client.Grant(ctx, config.AppConfig.Node.LeaseTTL)
		if err != nil {
			logx.Errorf("player_locator re-grant lease failed: %v, retrying in %v", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		n.leaseID = grant.ID

		allocKey := allocationKey(prefix, n.Info.NodeType, n.Info.NodeId)
		rpcKey := rpcPath(prefix, n.Info.ZoneId, n.Info.NodeType, n.Info.NodeId)
		value, merr := protojson.Marshal(n.Info)
		if merr != nil {
			logx.Errorf("player_locator re-register marshal node info failed: %v", merr)
			return
		}

		// 步骤 1:CAS 重夺原 node_id —— 只有 allocationKey 仍不存在才能写。
		txnResp, err := n.client.Txn(ctx).
			If(clientv3.Compare(clientv3.Version(allocKey), "=", 0)).
			Then(
				clientv3.OpPut(allocKey, n.Info.NodeUuid, clientv3.WithLease(n.leaseID)),
				clientv3.OpPut(rpcKey, string(value), clientv3.WithLease(n.leaseID)),
			).
			Commit()
		if err != nil {
			logx.Errorf("player_locator re-claim txn failed: %v, retrying in %v", err, backoff)
			_, _ = n.client.Revoke(context.Background(), n.leaseID)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		if !txnResp.Succeeded {
			// 步骤 2:原 node_id 已被别的实例占用,分配全新 id。
			logx.Errorf("player_locator original node_id=%d taken by another instance, allocating new one",
				n.Info.NodeId)
			newID, err := allocateNodeID(ctx, n.client, prefix, n.Info, n.leaseID)
			if err != nil {
				logx.Errorf("player_locator re-allocate node_id failed: %v, retrying in %v", err, backoff)
				_, _ = n.client.Revoke(context.Background(), n.leaseID)
				time.Sleep(backoff)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			n.Info.NodeId = newID
			logx.Infof("player_locator re-registered with new node_id=%d", newID)
		} else {
			logx.Infof("player_locator re-claimed original node_id=%d", n.Info.NodeId)
		}

		// 步骤 3:重启 keep-alive 看护(channel 再断会再次走 reRegister)。
		ch, err := n.client.KeepAlive(ctx, n.leaseID)
		if err != nil {
			logx.Errorf("player_locator restart keepalive failed: %v", err)
			return
		}
		go n.watchKeepAlive(ctx, ch)
		logx.Info("player_locator re-registration completed")
		return
	}
}

// EtcdClient 暴露本节点注册用的 etcd 客户端,供同进程内其他需要 etcd 的组件
// 复用(目前是 shared/killswitch 的规则 watch)。
//
// 刻意只读不转移所有权:客户端的生命周期仍由 Node 管,Close() 时一并关闭。
// 复用而不是另开一条连接,是因为再建一个 clientv3 就多一份 TCP 连接、一份
// keep-alive 心跳和一份要独立收尾的资源,而它们连的是同一个集群。
//
// 调用方必须能接受返回 nil(Node 未初始化 / 已关闭)—— killswitch 对 nil
// 客户端的语义就是"全部放行",这正是 fail-open 想要的。
func (n *Node) EtcdClient() *clientv3.Client {
	if n == nil {
		return nil
	}
	return n.client
}

func (n *Node) Close() error {
	if n.cancelFunc != nil {
		n.cancelFunc()
	}
	if n.client == nil {
		return nil
	}

	prefix := rpcPrefix(n.Info.NodeType)
	rpcKey := rpcPath(prefix, n.Info.ZoneId, n.Info.NodeType, n.Info.NodeId)
	allocKey := allocationKey(prefix, n.Info.NodeType, n.Info.NodeId)

	if _, err := n.client.Txn(context.Background()).
		Then(clientv3.OpDelete(rpcKey), clientv3.OpDelete(allocKey)).
		Commit(); err != nil {
		logx.Errorf("player_locator node close: delete keys failed: %v", err)
	}

	if _, err := n.client.Revoke(context.Background(), n.leaseID); err != nil {
		logx.Errorf("player_locator node close: revoke lease failed: %v", err)
	}

	return n.client.Close()
}
