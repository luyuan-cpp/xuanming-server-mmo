package node

// 本文件中的 allocator 逻辑在 guild / friend / player_locator / scene_manager
// 四个服务里是逐字相同的副本(只有 import 包名不同)。
// 若需修改 allocator 行为,请同步修改另外三个服务的对应文件:
//   - go/guild/internal/node/node.go
//   - go/friend/internal/node/node.go
//   - go/scene_manager/internal/noderegistry/registry.go
//
// 有意的差异(2026-10-18,只此一处):tryClaimNodeID / allocateNodeID 末尾多一个可选的 extraOps 钩子
// (变参,缺省为空,不传时与其它副本逐字同义),reRegister 第 1 步的 Txn 也带上它。player_locator 用它把
// 「会话按 gate 建索引」的能力键与 NodeInfo 放进同一个注册事务、挂同一个租约(NewNode 的 WithLeaseBoundMarkers,
// docs/design/player-locator-gate-session-index.md §6):对账 leader 据此判断有没有不维护索引的旧版写者,
// 两者同生同灭才不会出现「有 NodeInfo 没能力键」的误判窗口。同步 allocator 修改时保留这个钩子即可;
// 将来收进 go/shared 时把它做成公共选项。
//
// 2026-11-05 登记(发布轨道,docs/design/release-track-gray-canary.md §2.1):各副本都在注册入口第一步读
// releasetrack.FromEnv,取值非法就在碰 etcd 之前返回错误;合法的轨道经 newNodeInfo 写进 NodeInfo.release_track,
// 重注册沿用同一个 Info。与 shared/noderegistry 同一做法,同步修改时保留。

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
	"shared/nodeinfo"
	"shared/releasetrack"
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
	// markerPrefixes 见 WithLeaseBoundMarkers;构造后只读。
	markerPrefixes []string
}

// Option 是 NewNode 的可选项。
type Option func(*nodeOptions)

type nodeOptions struct {
	markerPrefixes []string
}

// WithLeaseBoundMarkers 让每个前缀各多写一个「前缀 + NodeUuid」键(值 "1"),与 NodeInfo 同一个注册事务、
// 同一个租约:首注册、换 node_id 重新分配、租约丢失后的重注册三处都带上,随租约到期或 Close 撤销一起消失。
// node 包只把它们当不透明的键,不知道业务含义;player_locator 用它登记「会话按 gate 建索引」的能力键。
func WithLeaseBoundMarkers(prefixes ...string) Option {
	return func(o *nodeOptions) {
		o.markerPrefixes = append(o.markerPrefixes, prefixes...)
	}
}

// leaseBoundMarkerOps 把标记前缀展开成挂在 lease 上的 Put(纯函数,便于单测)。
func leaseBoundMarkerOps(prefixes []string, nodeUUID string, lease clientv3.LeaseID) []clientv3.Op {
	if len(prefixes) == 0 {
		return nil
	}
	ops := make([]clientv3.Op, 0, len(prefixes))
	for _, prefix := range prefixes {
		ops = append(ops, clientv3.OpPut(prefix+nodeUUID, "1", clientv3.WithLease(lease)))
	}
	return ops
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

// NewNode 注册本进程并返回句柄。发布轨道在第一步读,不靠入口传参:取值非法时在碰 etcd 之前就返回错误,
// 进程随之拒启;漏写轨道的 canary 进程会被当成 stable(与 shared/noderegistry 同一理由)。
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

	info := newNodeInfo(nodeType, cfg.Node.ZoneId, ip, port, track)

	prefix := rpcPrefix(nodeType)
	markers := leaseBoundMarkerOps(o.markerPrefixes, info.NodeUuid, grant.ID)
	nodeID, err := allocateNodeID(context.Background(), client, prefix, info, grant.ID, markers...)
	if err != nil {
		_, _ = client.Revoke(context.Background(), grant.ID)
		client.Close()
		return nil, err
	}
	info.NodeId = nodeID
	logx.Infof("player_locator node registered: zone=%d node_type=%d node_id=%d uuid=%s release_track=%s",
		info.ZoneId, nodeType, nodeID, info.NodeUuid, track)

	return &Node{
		Info:           info,
		client:         client,
		leaseID:        grant.ID,
		markerPrefixes: o.markerPrefixes,
	}, nil
}

// newNodeInfo 构造本进程写进 etcd 的 NodeInfo,node_id 由随后的分配步骤填。
// 抽成函数是为了单测不连 etcd 就能核对轨道;重注册沿用同一个 Info,轨道不会丢。
func newNodeInfo(nodeType, zoneId uint32, ip string, port uint32, track releasetrack.Track) *proto_common.NodeInfo {
	return &proto_common.NodeInfo{
		NodeType: nodeType,
		Endpoint: &proto_common.EndpointComp{
			Ip:   ip,
			Port: port,
		},
		ZoneId:       zoneId,
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(proto_common.ENodeProtocolType_PROTOCOL_GRPC),
		NodeUuid:     uuid.New().String(),
		ReleaseTrack: track.String(),
	}
}

// allocateNodeID 在 (node_type) 下找空闲 node_id 并通过 etcd Txn CAS 占住。
// 详细设计见 go/guild/internal/node/node.go 同名函数。
// extraOps 原样转给 tryClaimNodeID(见文件头「有意的差异」),缺省为空。
func allocateNodeID(ctx context.Context, client *clientv3.Client, prefix string, info *proto_common.NodeInfo, leaseID clientv3.LeaseID, extraOps ...clientv3.Op) (uint32, error) {
	usedIDs, err := scanUsedNodeIDs(ctx, client, prefix, info.NodeType)
	if err != nil {
		return 0, err
	}

	for id := nodeIDMin; id <= nodeIDMax; id++ {
		if usedIDs[id] {
			continue
		}
		ok, err := tryClaimNodeID(ctx, client, prefix, id, info, leaseID, extraOps...)
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
		// 宽松解析(忽略未知字段,D77):新版本实例写入的新字段不能让它的 node_id 漏出 used 集合。
		if err := nodeinfo.Unmarshal(kv.Value, &ni); err != nil {
			continue
		}
		if ni.NodeId >= nodeIDMin {
			used[ni.NodeId] = true
		}
	}

	return used, nil
}

// extraOps 与 allocKey / rpcKey 在同一个 Txn 的 Then 里提交(见文件头「有意的差异」),缺省为空。
func tryClaimNodeID(ctx context.Context, client *clientv3.Client, prefix string, nodeID uint32, info *proto_common.NodeInfo, leaseID clientv3.LeaseID, extraOps ...clientv3.Op) (bool, error) {
	allocKey := allocationKey(prefix, info.NodeType, nodeID)
	rpcKey := rpcPath(prefix, info.ZoneId, info.NodeType, nodeID)

	info.NodeId = nodeID
	value, err := protojson.Marshal(info)
	if err != nil {
		return false, fmt.Errorf("marshal node info: %w", err)
	}

	txnResp, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(allocKey), "=", 0)).
		Then(append([]clientv3.Op{
			clientv3.OpPut(allocKey, info.NodeUuid, clientv3.WithLease(leaseID)),
			clientv3.OpPut(rpcKey, string(value), clientv3.WithLease(leaseID)),
		}, extraOps...)...).
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

		// 能力键等标记挂在新租约上,与 NodeInfo 同一事务重建(见文件头「有意的差异」)。
		markers := leaseBoundMarkerOps(n.markerPrefixes, n.Info.NodeUuid, n.leaseID)

		// 步骤 1:CAS 重夺原 node_id —— 只有 allocationKey 仍不存在才能写。
		txnResp, err := n.client.Txn(ctx).
			If(clientv3.Compare(clientv3.Version(allocKey), "=", 0)).
			Then(append([]clientv3.Op{
				clientv3.OpPut(allocKey, n.Info.NodeUuid, clientv3.WithLease(n.leaseID)),
				clientv3.OpPut(rpcKey, string(value), clientv3.WithLease(n.leaseID)),
			}, markers...)...).
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
			newID, err := allocateNodeID(ctx, n.client, prefix, n.Info, n.leaseID, markers...)
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

	// WithLeaseBoundMarkers 的标记键随这次撤销消失。删 NodeInfo 与撤销之间短暂「有标记没 NodeInfo」,
	// 对账 leader 只把「有 NodeInfo 没标记」判为旧写者,这个窗口无害;撤销失败时标记随租约到期消失。
	if _, err := n.client.Revoke(context.Background(), n.leaseID); err != nil {
		logx.Errorf("player_locator node close: revoke lease failed: %v", err)
	}

	return n.client.Close()
}
