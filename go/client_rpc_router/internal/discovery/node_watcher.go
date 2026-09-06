// Package discovery 维护路由服对各目标业务服务(login / match / chat / …)的
// etcd list-watch 内存镜像,以及按 endpoint 缓存的 gRPC 连接。
//
// 本文件复制自 go/match/internal/discovery/node_watcher.go(match 对 scene / battle
// 的同一套 list-watch + 连接缓存),只增加了 PickRandomInZone(设计决策 D33 的
// zone 过滤)。两份副本后续收成 go/shared 一份(设计文档 §5「后续」);
// 改 watch / 连接缓存行为时请同步另一份。
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// nodeRegistrationJSON 对应 etcd 里 protojson 序列化的 NodeInfo
// (Go noderegistry 与 C++ MessageToJsonString 都是 camelCase 字段)。
type nodeRegistrationJSON struct {
	NodeId   uint32 `json:"nodeId"`
	NodeType uint32 `json:"nodeType"`
	NodeUuid string `json:"nodeUuid"`
	ZoneId   uint32 `json:"zoneId"`
	Endpoint struct {
		IP   string `json:"ip"`
		Port uint32 `json:"port"`
	} `json:"endpoint"`
	GrpcEndpoint struct {
		IP   string `json:"ip"`
		Port uint32 `json:"port"`
	} `json:"grpcEndpoint"`
}

// NodeEntry 是 watch 镜像里的一个节点。
type NodeEntry struct {
	NodeId   uint32
	ZoneId   uint32
	NodeUuid string
	// Endpoint 是 gRPC 地址 "ip:port"。只认 grpcEndpoint,绝不回退
	// endpoint(那是 raw TCP protobuf 口,用 gRPC 拨会协议错乱,
	// 先例见 scene_manager resolveFromKnownNodes 的注释)。
	Endpoint string
}

// NodeWatcher 对一个 etcd 前缀做 list-watch,维护 key -> NodeEntry 镜像。
type NodeWatcher struct {
	name     string // 日志/指标用途(节点类型名,如 "LoginNodeService")
	prefix   string // 例如 "LoginNodeService.rpc/"
	mu       sync.RWMutex
	nodes    map[string]NodeEntry
	onCount  func(kind string, count int) // 镜像规模变化上报(指标)
	onRemove func(entry NodeEntry)        // 节点消失回调(清连接缓存)
}

// NewNodeWatcher 构造 watcher;onCount / onRemove 可为 nil。
func NewNodeWatcher(name, prefix string, onCount func(string, int), onRemove func(NodeEntry)) *NodeWatcher {
	return &NodeWatcher{
		name:     name,
		prefix:   prefix,
		nodes:    make(map[string]NodeEntry),
		onCount:  onCount,
		onRemove: onRemove,
	}
}

// Name 返回 watcher 名(节点类型名)。
func (w *NodeWatcher) Name() string { return w.name }

// Run 阻塞式跑 list-watch 循环:fullSync 建镜像 → watch 增量;watch 断开重来。
// 调用方负责放进 goroutine(safego.Go)。
func (w *NodeWatcher) Run(ctx context.Context, etcd *clientv3.Client) {
	for {
		rev, err := w.fullSync(ctx, etcd)
		if err != nil {
			logx.Errorf("[NodeWatcher:%s] 全量同步失败: %v,3s 后重试", w.name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		w.watch(ctx, etcd, rev)
		if ctx.Err() != nil {
			return
		}
		logx.Infof("[NodeWatcher:%s] watch 中断,重新同步", w.name)
	}
}

func (w *NodeWatcher) fullSync(ctx context.Context, etcd *clientv3.Client) (int64, error) {
	resp, err := etcd.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, fmt.Errorf("etcd get %s: %w", w.prefix, err)
	}

	next := make(map[string]NodeEntry, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		entry, ok := w.parse(key, kv.Value)
		if !ok {
			continue
		}
		next[key] = entry
	}

	w.mu.Lock()
	prev := w.nodes
	w.nodes = next
	count := len(next)
	w.mu.Unlock()

	// 差集兜底:watch 重建窗口里错过的 DELETE,靠新旧快照比对清连接缓存。
	// 同一个 key 换了 endpoint(节点重启后拿到新端口)同样要清:旧 endpoint 的
	// 连接不再有任何持有者,不清就会永久滞留在连接缓存里,后续拨号命中它必失败。
	if w.onRemove != nil {
		for _, entry := range diffRemoved(prev, next) {
			w.onRemove(entry)
		}
	}
	w.reportCount(count)
	logx.Infof("[NodeWatcher:%s] 全量同步完成: %d 节点, rev=%d", w.name, count, resp.Header.Revision)
	return resp.Header.Revision, nil
}

func (w *NodeWatcher) watch(ctx context.Context, etcd *clientv3.Client, rev int64) {
	watchCh := etcd.Watch(ctx, w.prefix,
		clientv3.WithPrefix(),
		clientv3.WithRev(rev+1),
	)
	for {
		select {
		case <-ctx.Done():
			return
		case watchResp, ok := <-watchCh:
			if !ok {
				logx.Infof("[NodeWatcher:%s] watch channel 关闭", w.name)
				return
			}
			if watchResp.Err() != nil {
				logx.Errorf("[NodeWatcher:%s] watch 出错: %v", w.name, watchResp.Err())
				return
			}
			for _, ev := range watchResp.Events {
				w.handleEvent(ev)
			}
		}
	}
}

func (w *NodeWatcher) handleEvent(ev *clientv3.Event) {
	key := string(ev.Kv.Key)
	switch ev.Type {
	case clientv3.EventTypePut:
		entry, ok := w.parse(key, ev.Kv.Value)
		if !ok {
			return
		}
		w.Upsert(key, entry)
	case clientv3.EventTypeDelete:
		w.mu.Lock()
		entry, existed := w.nodes[key]
		if existed {
			delete(w.nodes, key)
		}
		count := len(w.nodes)
		w.mu.Unlock()
		if !existed {
			return
		}
		if w.onRemove != nil {
			w.onRemove(entry)
		}
		logx.Infof("[NodeWatcher:%s] 节点下线: zone=%d node=%d endpoint=%s",
			w.name, entry.ZoneId, entry.NodeId, entry.Endpoint)
		w.reportCount(count)
	}
}

// Upsert 把一条节点登记写进镜像(etcd PUT 事件的落库路径;测试也用它直接
// 灌节点,不必起 etcd)。同 key 重复登记且 endpoint 变了(重启换端口)会先
// 通过 onRemove 废弃旧连接。
func (w *NodeWatcher) Upsert(key string, entry NodeEntry) {
	w.mu.Lock()
	prev, existed := w.nodes[key]
	w.nodes[key] = entry
	count := len(w.nodes)
	w.mu.Unlock()
	// 重启后 endpoint 可能变了,旧连接必须废弃。
	if existed && w.onRemove != nil && prev.Endpoint != entry.Endpoint {
		w.onRemove(prev)
	}
	if !existed {
		logx.Infof("[NodeWatcher:%s] 节点上线: zone=%d node=%d endpoint=%s",
			w.name, entry.ZoneId, entry.NodeId, entry.Endpoint)
	}
	w.reportCount(count)
}

func (w *NodeWatcher) parse(key string, value []byte) (NodeEntry, bool) {
	// noderegistry 会在同前缀下写 "/allocated/" 占位 key,不是节点注册,跳过。
	if strings.Contains(key, "/allocated/") {
		return NodeEntry{}, false
	}
	var reg nodeRegistrationJSON
	if err := json.Unmarshal(value, &reg); err != nil {
		logx.Errorf("[NodeWatcher:%s] 解析注册值失败 key=%s: %v", w.name, key, err)
		return NodeEntry{}, false
	}
	if reg.GrpcEndpoint.Port == 0 {
		// 没有 gRPC endpoint 的节点无法作为转发目标,记日志后忽略。
		logx.Errorf("[NodeWatcher:%s] 节点缺少 grpcEndpoint,忽略: key=%s zone=%d node=%d",
			w.name, key, reg.ZoneId, reg.NodeId)
		return NodeEntry{}, false
	}
	return NodeEntry{
		NodeId:   reg.NodeId,
		ZoneId:   reg.ZoneId,
		NodeUuid: reg.NodeUuid,
		Endpoint: fmt.Sprintf("%s:%d", reg.GrpcEndpoint.IP, reg.GrpcEndpoint.Port),
	}, true
}

// diffRemoved 返回「新快照里已不再持有」的旧条目:key 整个消失,或 key 还在但
// endpoint 变了(旧 endpoint 的连接需要回收)。抽成纯函数是为了能单测。
func diffRemoved(prev, next map[string]NodeEntry) []NodeEntry {
	var removed []NodeEntry
	for key, entry := range prev {
		nextEntry, still := next[key]
		if !still || nextEntry.Endpoint != entry.Endpoint {
			removed = append(removed, entry)
		}
	}
	return removed
}

func (w *NodeWatcher) reportCount(count int) {
	if w.onCount != nil {
		w.onCount(w.name, count)
	}
}

// PickRandom 随机取一个节点(全局池节点类型的选法:随机,负载感知不在本期)。
func (w *NodeWatcher) PickRandom() (NodeEntry, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if len(w.nodes) == 0 {
		return NodeEntry{}, false
	}
	idx := rand.Intn(len(w.nodes))
	for _, entry := range w.nodes {
		if idx == 0 {
			return entry, true
		}
		idx--
	}
	return NodeEntry{}, false
}

// PickRandomInZone 只在 zone_id 相同的节点里随机取一个(zone-scoped 节点类型
// 的选法,设计决策 D33:login 只服务本 zone,跨 zone 打过去会话对不上)。
// 该 zone 没有实例返回 false —— 不回退到其它 zone。
func (w *NodeWatcher) PickRandomInZone(zoneId uint32) (NodeEntry, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	candidates := make([]NodeEntry, 0, len(w.nodes))
	for _, entry := range w.nodes {
		if entry.ZoneId == zoneId {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		return NodeEntry{}, false
	}
	return candidates[rand.Intn(len(candidates))], true
}

// Count 返回镜像里的节点数量。
func (w *NodeWatcher) Count() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.nodes)
}

// ---------------------------------------------------------------------------
// gRPC 连接缓存(按 endpoint 复用;节点下线时由 watcher onRemove 清理)
// ---------------------------------------------------------------------------

var (
	connMu    sync.RWMutex
	connCache = make(map[string]*grpc.ClientConn)
)

// DialEndpoint 返回缓存的 gRPC 连接或新建一条。内网 overlay,insecure
// transport(与 scene_manager defaultNodeDialer 同口径)。
func DialEndpoint(endpoint string) (*grpc.ClientConn, error) {
	connMu.RLock()
	conn, ok := connCache[endpoint]
	connMu.RUnlock()
	if ok {
		return conn, nil
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", endpoint, err)
	}

	connMu.Lock()
	if existing, ok := connCache[endpoint]; ok {
		connMu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	connCache[endpoint] = conn
	connMu.Unlock()

	logx.Infof("[discovery] 已连接节点 %s", endpoint)
	return conn, nil
}

// RemoveEndpointConn 关闭并移除某 endpoint 的缓存连接。
func RemoveEndpointConn(endpoint string) {
	connMu.Lock()
	defer connMu.Unlock()
	if conn, ok := connCache[endpoint]; ok {
		_ = conn.Close()
		delete(connCache, endpoint)
		logx.Infof("[discovery] 已移除节点连接 %s", endpoint)
	}
}
