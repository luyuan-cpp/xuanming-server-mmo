// Package noderegistry 按 C++ NodeInfo 约定把路由服注册进 etcd:
// gate 按前缀 "ClientRpcRouterNodeService.rpc/zone/{zoneId}/node_type/{nodeType}/node_id/{nodeId}"
// watch 到 protojson 序列化的 NodeInfo(ProtocolType=PROTOCOL_GRPC),据此建 gRPC channel。
//
// 本文件复制自 go/match/internal/noderegistry/registry.go(只改了日志前缀与注释)。
// 同一份 allocator 逻辑目前在 guild / friend / player_locator / scene_manager / match /
// client_rpc_router 六处是逐字相同的副本,后续收成 go/shared 一份(设计文档 §5「后续」);
// 若需修改 allocator 行为,请同步修改:
//   - go/guild/internal/node/node.go
//   - go/friend/internal/node/node.go
//   - go/player_locator/internal/node/node.go
//   - go/scene_manager/internal/noderegistry/registry.go
//   - go/match/internal/noderegistry/registry.go
package noderegistry

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	base "proto/common/base"
	"shared/snowflake"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// nodeIDMin 是合法 node_id 的下界。0 永远作为 "未分配 / 非法值" 保留。
const nodeIDMin uint32 = 1

// nodeIDMax 与 Snowflake worker id 位宽对齐(shared/snowflake.NodeMask)。
var nodeIDMax = uint32(snowflake.NodeMask)

// NodeRegistration 管理带租约与 keepalive 的 etcd 注册。
type NodeRegistration struct {
	client   *clientv3.Client
	leaseID  clientv3.LeaseID
	ttl      int64
	prefix   string // rpcPrefix(nodeType),用于构造两个 key
	rpcKey   string // 当前生效的 rpcPath(随 reRegister 可能变,如果 node_id 换了)
	allocKey string // 当前生效的 allocation key(随 node_id 一起变)
	value    string // 当前 rpcPath 写入的 NodeInfo protojson
	cancelFn context.CancelFunc
	Info     *base.NodeInfo
}

// rpcPrefix 返回节点类型的 etcd key 前缀,如 "ClientRpcRouterNodeService.rpc"。
func rpcPrefix(nodeType uint32) string {
	return fmt.Sprintf("%s.rpc", base.ENodeType_name[int32(nodeType)])
}

// rpcPath 是 per-zone 服务发现 key,C++ 端按这个路径 watch NodeInfo。
func rpcPath(prefix string, zoneId, nodeType, nodeId uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/node_id/%d", prefix, zoneId, nodeType, nodeId)
}

// allocationKey 是跨 zone 的全局占位 key —— 路径里不带 zone,
// 因为两个 zone 的实例不可能同时拿到同一个 (node_type, node_id)。
func allocationKey(prefix string, nodeType, nodeID uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/%d", prefix, nodeType, nodeID)
}

func allocationKeyPrefix(prefix string, nodeType uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/", prefix, nodeType)
}

// Register 在 etcd 里以租约写入 NodeInfo,node_id 用原子 CAS 分配。
func Register(cli *clientv3.Client, nodeType, zoneId uint32, ip string, port uint32, ttl int64) (*NodeRegistration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 申请租约
	leaseResp, err := cli.Grant(ctx, ttl)
	if err != nil {
		return nil, fmt.Errorf("etcd lease grant failed: %w", err)
	}

	prefix := rpcPrefix(nodeType)
	info := &base.NodeInfo{
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
	}

	// CAS 分配 node_id
	nodeID, err := allocateNodeID(ctx, cli, prefix, info, leaseResp.ID)
	if err != nil {
		_, _ = cli.Revoke(context.Background(), leaseResp.ID)
		return nil, fmt.Errorf("node ID allocation failed: %w", err)
	}
	info.NodeId = nodeID

	rpcKey := rpcPath(prefix, zoneId, nodeType, nodeID)
	allocKey := allocationKey(prefix, nodeType, nodeID)
	value, err := protojson.Marshal(info)
	if err != nil {
		_, _ = cli.Revoke(context.Background(), leaseResp.ID)
		return nil, fmt.Errorf("protojson marshal failed: %w", err)
	}

	return &NodeRegistration{
		client:   cli,
		leaseID:  leaseResp.ID,
		ttl:      ttl,
		prefix:   prefix,
		rpcKey:   rpcKey,
		allocKey: allocKey,
		value:    string(value),
		Info:     info,
	}, nil
}

// allocateNodeID 在 (node_type) 下找空闲 node_id 并通过 etcd Txn CAS 占住。
// 详细设计见 go/guild/internal/node/node.go 同名函数。
func allocateNodeID(ctx context.Context, cli *clientv3.Client, prefix string, info *base.NodeInfo, leaseID clientv3.LeaseID) (uint32, error) {
	usedIDs, err := scanUsedNodeIDs(ctx, cli, prefix, info.NodeType)
	if err != nil {
		return 0, err
	}

	for id := nodeIDMin; id <= nodeIDMax; id++ {
		if usedIDs[id] {
			continue
		}
		ok, err := tryClaimNodeID(ctx, cli, prefix, id, info, leaseID)
		if err != nil {
			logx.Errorf("client_rpc_router allocator: txn failed for node_id=%d: %v", id, err)
			continue
		}
		if ok {
			return id, nil
		}
		usedIDs[id] = true
	}
	return 0, fmt.Errorf("no available node_id in [%d, %d]", nodeIDMin, nodeIDMax)
}

func scanUsedNodeIDs(ctx context.Context, cli *clientv3.Client, prefix string, nodeType uint32) (map[uint32]bool, error) {
	used := make(map[uint32]bool)

	allocPrefix := allocationKeyPrefix(prefix, nodeType)
	allocResp, err := cli.Get(ctx, allocPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get alloc prefix: %w", err)
	}
	for _, kv := range allocResp.Kvs {
		tail := strings.TrimPrefix(string(kv.Key), allocPrefix)
		if id, err := strconv.ParseUint(tail, 10, 32); err == nil {
			used[uint32(id)] = true
		}
	}

	rpcResp, err := cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get rpc prefix: %w", err)
	}
	for _, kv := range rpcResp.Kvs {
		key := string(kv.Key)
		if strings.Contains(key, "/allocated/") {
			continue
		}
		var ni base.NodeInfo
		if err := protojson.Unmarshal(kv.Value, &ni); err != nil {
			continue
		}
		if ni.NodeId >= nodeIDMin {
			used[ni.NodeId] = true
		}
	}

	return used, nil
}

func tryClaimNodeID(ctx context.Context, cli *clientv3.Client, prefix string, nodeID uint32, info *base.NodeInfo, leaseID clientv3.LeaseID) (bool, error) {
	allocKey := allocationKey(prefix, info.NodeType, nodeID)
	rpcKey := rpcPath(prefix, info.ZoneId, info.NodeType, nodeID)

	info.NodeId = nodeID
	value, err := protojson.Marshal(info)
	if err != nil {
		return false, fmt.Errorf("marshal node info: %w", err)
	}

	txnResp, err := cli.Txn(ctx).
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

// KeepAlive 起后台 goroutine 维持 etcd 租约,租约丢失时重新注册。
func (nr *NodeRegistration) KeepAlive() {
	ctx, cancel := context.WithCancel(context.Background())
	nr.cancelFn = cancel

	ch, err := nr.client.KeepAlive(ctx, nr.leaseID)
	if err != nil {
		logx.Errorf("[NodeRegistry] KeepAlive start failed: %v", err)
		return
	}

	go func() {
		for {
			select {
			case ka := <-ch:
				if ka == nil {
					logx.Error("[NodeRegistry] lease lost, attempting re-registration")
					nr.reRegister(ctx)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// reRegister 在 lease 丢失后重新注册到 etcd。
//
// 关键安全点:
//
//	lease 丢失 → 老 allocationKey/rpcKey 会被 etcd 自动清掉 → 期间另一个实例可能已经
//	抢占了同一个 node_id。此时盲目 Put 老 key 会覆盖别人的 NodeInfo,
//	C++ watcher 把客户端路由到错误节点。
//
// 流程:
//  1. 拿新 lease。
//  2. 先尝试 "CAS 重夺原 node_id":要求 allocationKey 当前不存在
//     (它本来就该不存在,因为老 lease 已经被 revoke / expired)。
//     用同一个 Txn 重新 Put allocationKey + rpcKey,挂新 lease。
//  3. CAS 失败 → 原 node_id 已被其他 uuid 占用,绝不覆盖,改调
//     allocateNodeID 拿全新 node_id。路由服无状态,没有 "我必须是 node N" 的
//     持久化语义,换 ID 是安全的。
//  4. 拿到新 / 重夺到旧 node_id 之后,重新启动 KeepAlive。
func (nr *NodeRegistration) reRegister(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		resp, err := nr.client.Grant(ctx, nr.ttl)
		if err != nil {
			logx.Errorf("[NodeRegistry] re-grant lease failed: %v, retrying in %v", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		nr.leaseID = resp.ID

		// 步骤 1:试着 CAS 重夺原 node_id —— allocationKey 不存在才能写入。
		txnResp, err := nr.client.Txn(ctx).
			If(clientv3.Compare(clientv3.Version(nr.allocKey), "=", 0)).
			Then(
				clientv3.OpPut(nr.allocKey, nr.Info.NodeUuid, clientv3.WithLease(nr.leaseID)),
				clientv3.OpPut(nr.rpcKey, nr.value, clientv3.WithLease(nr.leaseID)),
			).
			Commit()
		if err != nil {
			logx.Errorf("[NodeRegistry] re-claim txn failed: %v, retrying in %v", err, backoff)
			_, _ = nr.client.Revoke(context.Background(), nr.leaseID)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		if !txnResp.Succeeded {
			// 步骤 2:原 node_id 已被别的实例占用 —— 决不能 Put 覆盖。
			// 改成 allocate 一个全新的 node_id。
			logx.Errorf("[NodeRegistry] original node_id=%d taken by another instance, allocating new one",
				nr.Info.NodeId)
			newID, err := allocateNodeID(ctx, nr.client, nr.prefix, nr.Info, nr.leaseID)
			if err != nil {
				logx.Errorf("[NodeRegistry] re-allocate node_id failed: %v, retrying in %v", err, backoff)
				_, _ = nr.client.Revoke(context.Background(), nr.leaseID)
				time.Sleep(backoff)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			// allocateNodeID 已经把 NodeId 写回 nr.Info 并完成双 key Put,
			// 这里只需要同步更新本地缓存的 key/value。
			nr.Info.NodeId = newID
			nr.rpcKey = rpcPath(nr.prefix, nr.Info.ZoneId, nr.Info.NodeType, newID)
			nr.allocKey = allocationKey(nr.prefix, nr.Info.NodeType, newID)
			if v, err := protojson.Marshal(nr.Info); err == nil {
				nr.value = string(v)
			}
			logx.Infof("[NodeRegistry] re-registered with new node_id=%d", newID)
		} else {
			logx.Infof("[NodeRegistry] re-claimed original node_id=%d", nr.Info.NodeId)
		}

		// 步骤 3:重启 KeepAlive。
		ch, err := nr.client.KeepAlive(ctx, nr.leaseID)
		if err != nil {
			logx.Errorf("[NodeRegistry] restart keepalive failed: %v", err)
			return
		}

		go func() {
			for {
				select {
				case ka := <-ch:
					if ka == nil {
						logx.Error("[NodeRegistry] lease lost again, re-registering")
						nr.reRegister(ctx)
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		logx.Info("[NodeRegistry] re-registration completed")
		return
	}
}

// Close 停止 keepalive,删除两个 key(rpcPath + allocationKey),并 Revoke lease。
func (nr *NodeRegistration) Close() {
	if nr.cancelFn != nil {
		nr.cancelFn()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := nr.client.Txn(ctx).
		Then(
			clientv3.OpDelete(nr.rpcKey),
			clientv3.OpDelete(nr.allocKey),
		).
		Commit(); err != nil {
		logx.Errorf("[NodeRegistry] failed to delete keys: %v", err)
	}
	if _, err := nr.client.Revoke(ctx, nr.leaseID); err != nil {
		logx.Errorf("[NodeRegistry] failed to revoke lease: %v", err)
	}
}
