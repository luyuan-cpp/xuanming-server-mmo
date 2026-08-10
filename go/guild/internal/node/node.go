package node

// 本文件中的 allocator 逻辑在 guild / friend / player_locator / scene_manager
// 四个服务里是逐字相同的副本(只有 import 包名不同)。
// 若需修改 allocator 行为,请同步修改另外三个服务的对应文件:
//   - go/friend/internal/node/node.go
//   - go/player_locator/internal/node/node.go
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

	"guild/internal/config"
	base "proto/common/base"
	"shared/snowflake"
)

// nodeIDMin 是合法 node_id 的下界。0 永远作为 "未分配 / 非法值" 保留,
// 不会被 allocator 分配出来。这样上游 `if info.NodeId == 0` 这种哨兵判定才有意义,
// 也避免 Snowflake worker id=0 看起来像 "合法但未初始化" 的歧义状态。
const nodeIDMin uint32 = 1

// nodeIDMax 与 Snowflake worker id 位宽对齐(shared/snowflake.NodeMask)。
// 超过这个范围的 node_id 喂给 Snowflake.NewNode 会 panic,所以分配阶段就拒掉。
var nodeIDMax = uint32(snowflake.NodeMask)

type Node struct {
	Info       *base.NodeInfo
	client     *clientv3.Client
	leaseID    clientv3.LeaseID
	cancelFunc context.CancelFunc
}

// rpcPrefix 是该 node_type 在 etcd 里的根前缀,例如 "GuildNodeService.rpc"。
func rpcPrefix(nodeType uint32) string {
	return fmt.Sprintf("%s.rpc", base.ENodeType_name[int32(nodeType)])
}

// rpcPath 是 per-zone 服务发现 key,C++ 端按这个路径 watch NodeInfo。
// 路径里保留 zone 段是为了兼容现有 C++ watcher,以及保留 "按 zone 列出 NodeInfo" 的能力。
func rpcPath(prefix string, zoneId, nodeType, nodeId uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/node_id/%d", prefix, zoneId, nodeType, nodeId)
}

// allocationKey 是跨 zone 的全局占位 key。
// 它的存在性(version != 0)就是 (node_type, node_id) 这对资源被占用的真相;
// 同一个 lease 同时挂在 allocationKey 和 rpcPath 上,进程退出 / lease 过期时一起消失。
// 关键点:这个 key 路径里**不带 zone**,所以两个不同 zone 的实例不可能同时拿到同一个 (node_type, node_id)。
// 这正是修复 Snowflake worker id 跨 zone 冲突的核心。
func allocationKey(prefix string, nodeType, nodeID uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/%d", prefix, nodeType, nodeID)
}

// allocationKeyPrefix 用于一次性 Get 出所有 (node_type) 已占 node_id。
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

	info := &base.NodeInfo{
		NodeType: nodeType,
		Endpoint: &base.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		ZoneId:       cfg.Node.ZoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC),
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

// allocateNodeID 在 (node_type) 下找一个空闲 node_id 并通过 etcd Txn CAS 占住。
//
// 设计要点:
//  1. 同时扫两个前缀构建 used 集合:
//     - 新 schema 的 allocation 前缀(跨 zone 全局占位)
//     - 旧 schema 的 rpcPrefix(跨所有 zone 的 per-zone rpcPath)
//     第二个前缀是为了滚动升级 —— 升级期间老实例只写 rpcPath、没有 allocation key,
//     新启动的实例必须把它们的 node_id 也算成 "已占",否则会重复分配。
//  2. node_id 从 1 开始,0 保留为非法值。
//  3. Txn 同时 Put allocationKey 和 rpcPath,同一个 lease,保证 "占位" 和 "服务发现" 原子地一起生效。
//  4. CAS 条件仅检查 allocationKey 不存在;如果它已存在(无论 value 是谁的 uuid),都不能强占。
//  5. CAS 失败继续尝试下一个 ID,直到 nodeIDMax(对齐 Snowflake 位宽)。
func allocateNodeID(ctx context.Context, client *clientv3.Client, prefix string, info *base.NodeInfo, leaseID clientv3.LeaseID) (uint32, error) {
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
			// 单次 Txn 失败(网络抖动等)不影响其它候选,记日志后继续。
			logx.Errorf("guild allocator: txn failed for node_id=%d: %v", id, err)
			continue
		}
		if ok {
			return id, nil
		}
		// CAS 没成功:被并发抢走,标记成 used 后继续往后找。
		usedIDs[id] = true
	}
	return 0, fmt.Errorf("no available node_id in [%d, %d]", nodeIDMin, nodeIDMax)
}

// scanUsedNodeIDs 扫描"新 schema 的 allocation key"和"旧 schema 的跨 zone rpcPath",
// 合并出该 (node_type) 下所有已占用的 node_id。
func scanUsedNodeIDs(ctx context.Context, client *clientv3.Client, prefix string, nodeType uint32) (map[uint32]bool, error) {
	used := make(map[uint32]bool)

	// 1) 新 schema:<prefix>/allocated/node_type/<type>/node_id/<id>
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

	// 2) 旧 schema 兼容:<prefix>/zone/*/node_type/<type>/node_id/<id>
	// 滚动升级期间老实例只写 rpcPath,这一步把它们也纳入 used。
	// 全量升级完成后,所有实例都会双写,这步会变成 allocation 集合的子集 — 留着无害。
	rpcResp, err := client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get rpc prefix: %w", err)
	}
	for _, kv := range rpcResp.Kvs {
		// 只解析 rpcPath(含 "/node_id/"),跳过新 schema 的 allocation key(它在同一个 rpcPrefix 下,会被一并扫到)。
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

// tryClaimNodeID 在一个 Txn 里同时占住 allocationKey 和 rpcPath,挂同一个 lease。
// CAS 条件: allocationKey 当前不存在。
// 成功后,info.NodeId 会被写入,protojson 序列化进 rpcPath value(C++ 端 watch 读到的格式不变)。
func tryClaimNodeID(ctx context.Context, client *clientv3.Client, prefix string, nodeID uint32, info *base.NodeInfo, leaseID clientv3.LeaseID) (bool, error) {
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
// 不可达超过 TTL 或 lease 被撤销)时进入重注册,而不是旧实现那样只打一条日志
// 就永远放弃 —— 那会让进程活着但注册已蒸发:服务发现再也看不到本实例,
// 且无任何自愈或告警路径。与 scene_manager(noderegistry/registry.go)、
// player_locator(internal/node/node.go)的同源副本对齐。
func (n *Node) watchKeepAlive(ctx context.Context, ch <-chan *clientv3.LeaseKeepAliveResponse) {
	for {
		select {
		case ka := <-ch:
			if ka == nil {
				logx.Error("guild node lease lost, attempting re-registration")
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
//	失败就换一个全新 node_id —— guild 没有"我必须是 node N"的持久化语义,
//	换 ID 安全(Snowflake worker id 由 shared/snowflakealloc 独立分配,
//	与 NodeInfo.NodeId 解耦,见 guild.go 启动注释)。
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
			logx.Errorf("guild re-grant lease failed: %v, retrying in %v", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		n.leaseID = grant.ID

		allocKey := allocationKey(prefix, n.Info.NodeType, n.Info.NodeId)
		rpcKey := rpcPath(prefix, n.Info.ZoneId, n.Info.NodeType, n.Info.NodeId)
		value, merr := protojson.Marshal(n.Info)
		if merr != nil {
			logx.Errorf("guild re-register marshal node info failed: %v", merr)
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
			logx.Errorf("guild re-claim txn failed: %v, retrying in %v", err, backoff)
			_, _ = n.client.Revoke(context.Background(), n.leaseID)
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		if !txnResp.Succeeded {
			// 步骤 2:原 node_id 已被别的实例占用,分配全新 id。
			logx.Errorf("guild original node_id=%d taken by another instance, allocating new one",
				n.Info.NodeId)
			newID, err := allocateNodeID(ctx, n.client, prefix, n.Info, n.leaseID)
			if err != nil {
				logx.Errorf("guild re-allocate node_id failed: %v, retrying in %v", err, backoff)
				_, _ = n.client.Revoke(context.Background(), n.leaseID)
				time.Sleep(backoff)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			n.Info.NodeId = newID
			logx.Infof("guild re-registered with new node_id=%d", newID)
		} else {
			logx.Infof("guild re-claimed original node_id=%d", n.Info.NodeId)
		}

		// 步骤 3:重启 keep-alive 看护(channel 再断会再次走 reRegister)。
		ch, err := n.client.KeepAlive(ctx, n.leaseID)
		if err != nil {
			logx.Errorf("guild restart keepalive failed: %v", err)
			return
		}
		go n.watchKeepAlive(ctx, ch)
		logx.Info("guild re-registration completed")
		return
	}
}

// Close 释放该 node 占用的 etcd 资源:
//  1. 在一个 Txn 里同时删除 rpcPath(per-zone)和 allocationKey(跨 zone 占位),
//     避免出现"占位还在 / 服务发现已下线"的短暂错配。
//  2. 无论 Delete 是否成功,都 Revoke lease 作为兜底 ——
//     lease revoke 后两个 key 都会被 etcd 立刻清掉,不会出现幽灵占位。
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
		logx.Errorf("guild node close: delete keys failed: %v", err)
	}

	if _, err := n.client.Revoke(context.Background(), n.leaseID); err != nil {
		logx.Errorf("guild node close: revoke lease failed: %v", err)
	}
	return n.client.Close()
}
