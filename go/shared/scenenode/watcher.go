// Package scenenode 是 Go 侧「找到持有某个玩家的 C++ scene 节点、并拿到它的 gRPC
// 客户端」这件事的共享实现(docs/design/guild-phase2/04-asset-channel.md §4.18)。
//
// 定位链路是两段,写者都不是本包的调用方:
//
//	player:{id}:location   Redis,scene_manager 写,值是 PlayerLocation protobuf
//	SceneNodeService.rpc/  etcd,scene 节点自己注册,值里含 grpcEndpoint
//
// 职责边界:本包只做「定位 + 拨号」,不含任何业务语义(不知道资产、不知道帮会),
// 也不做重试与退避 —— 那是 assetop 重投循环的职责。本包返回的每一种失败都告诉
// 调用方「现在没有节点能收这次调用」,由调用方决定是等下一轮还是放弃。
//
// 形状照两处现成实现抄来,解析语义逐字保留:
//   - go/match/internal/discovery/node_watcher.go —— etcd list-watch 内存镜像、
//     跳过 /allocated/ 占位 key、只认 grpcEndpoint、同身份多条注册即拒选;
//   - go/scene_manager/internal/logic/scene_node_client.go —— 按 endpoint 缓存
//     gRPC 连接、内网 insecure、可替换拨号器(测试指向 bufconn)。
//
// **match 不迁移**:battle 选点(EndpointOfNode / PickRandom)留在 match,本包只
// 承载 scene 侧共用的那部分;日后是否合并由单独一批决定。
//
// 线程模型:Watcher 与 ConnCache 的所有导出方法都可并发调用;Locator 自身没有
// 可变状态,字段在装配完成后不得再改。
package scenenode

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
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

// Watcher 对一个 etcd 前缀做 list-watch,维护 key -> NodeEntry 镜像。
//
// 契约:
//   - Run 之前镜像为空,Synced() 为 false。此时任何查询都返回「未注册」,
//     调用方必须按 fail-closed 处理(Locator 就是这么做的),不得当成「没节点
//     所以随便挑一个」;
//   - 镜像是最终一致的:etcd 事件到达前的短窗口里查到的可能是上一版。资产通道
//     容得下这点滞后,因为 scene 侧还有 seq 窗口与账本做最终判定。
type Watcher struct {
	name     string // 日志/指标用途("scene")
	prefix   string // 例如 SceneNodeRpcPrefix
	mu       sync.RWMutex
	nodes    map[string]NodeEntry
	synced   atomic.Bool                  // 首次 fullSync 成功后置 true,之后不再回落
	onCount  func(kind string, count int) // 镜像规模变化上报(指标)
	onRemove func(entry NodeEntry)        // 节点消失回调(清连接缓存)
}

// NewWatcher 构造 watcher;onCount / onRemove 可为 nil。
// onCount 的第一个参数是 name,让一个服务同时 watch 多个前缀时指标能分开。
// 两个回调都在 watch 循环的 goroutine 上同步调用,实现必须短、不得阻塞。
func NewWatcher(name, prefix string, onCount func(string, int), onRemove func(NodeEntry)) *Watcher {
	return &Watcher{
		name:     name,
		prefix:   prefix,
		nodes:    make(map[string]NodeEntry),
		onCount:  onCount,
		onRemove: onRemove,
	}
}

// Run 阻塞式跑 list-watch 循环:fullSync 建镜像 → watch 增量;watch 断开重来。
// 调用方负责放进 goroutine(safego.Go),并用 ctx 控制退出。
//
// etcd 为 nil 直接返回并记 ERROR:没有 etcd 就没有节点发现,镜像永远为空、
// Synced() 永远为 false,定位一律失败(fail-closed)。这里不 panic,也不静默
// 当成"没有节点",让服务起得来但资产调用明确失败,便于运维定位。
func (w *Watcher) Run(ctx context.Context, etcd *clientv3.Client) {
	if etcd == nil {
		logx.Errorf("[scenenode:%s] 未配置 etcd 客户端,节点镜像将始终为空,定位一律失败", w.name)
		return
	}
	for {
		rev, err := w.fullSync(ctx, etcd)
		if err != nil {
			logx.Errorf("[scenenode:%s] 全量同步失败: %v,3s 后重试", w.name, err)
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
		logx.Infof("[scenenode:%s] watch 中断,重新同步", w.name)
	}
}

// Synced 报告首次全量同步是否已经成功。false = 镜像还不可信。
// watch 中断重连期间保持 true:镜像仍是上一版全量 + 增量,比"当成没有节点"更接近事实。
func (w *Watcher) Synced() bool {
	return w.synced.Load()
}

func (w *Watcher) fullSync(ctx context.Context, etcd *clientv3.Client) (int64, error) {
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
	// 同一个 key 换了 endpoint(节点重启换端口)同样要清:旧 endpoint 的连接
	// 不再有任何持有者,不清就永久滞留在连接缓存里,后续拨到它必失败。
	if w.onRemove != nil {
		for key, entry := range prev {
			nextEntry, still := next[key]
			if !still || nextEntry.Endpoint != entry.Endpoint {
				w.onRemove(entry)
			}
		}
	}
	w.synced.Store(true)
	w.reportCount(count)
	logx.Infof("[scenenode:%s] 全量同步完成: %d 节点, rev=%d", w.name, count, resp.Header.Revision)
	return resp.Header.Revision, nil
}

func (w *Watcher) watch(ctx context.Context, etcd *clientv3.Client, rev int64) {
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
				logx.Infof("[scenenode:%s] watch channel 关闭", w.name)
				return
			}
			if watchResp.Err() != nil {
				logx.Errorf("[scenenode:%s] watch 出错: %v", w.name, watchResp.Err())
				return
			}
			for _, ev := range watchResp.Events {
				w.handleEvent(ev)
			}
		}
	}
}

func (w *Watcher) handleEvent(ev *clientv3.Event) {
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
		logx.Infof("[scenenode:%s] 节点下线: zone=%d node=%d endpoint=%s",
			w.name, entry.ZoneId, entry.NodeId, entry.Endpoint)
		w.reportCount(count)
	}
}

// Upsert 把一条节点登记写进镜像(etcd PUT 事件的落库路径;测试也用它直接
// 灌节点,不必起 etcd)。同 key 重复登记且 endpoint 变了(重启换端口)会先
// 通过 onRemove 废弃旧连接。
func (w *Watcher) Upsert(key string, entry NodeEntry) {
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
		logx.Infof("[scenenode:%s] 节点上线: zone=%d node=%d endpoint=%s",
			w.name, entry.ZoneId, entry.NodeId, entry.Endpoint)
	}
	w.reportCount(count)
}

func (w *Watcher) parse(key string, value []byte) (NodeEntry, bool) {
	// noderegistry 会在同前缀下写 "/allocated/" 占位 key,不是节点注册,跳过。
	if strings.Contains(key, "/allocated/") {
		return NodeEntry{}, false
	}
	var reg nodeRegistrationJSON
	if err := json.Unmarshal(value, &reg); err != nil {
		logx.Errorf("[scenenode:%s] 解析注册值失败 key=%s: %v", w.name, key, err)
		return NodeEntry{}, false
	}
	if reg.GrpcEndpoint.Port == 0 {
		// 没有 gRPC endpoint 的节点无法作为资产调用对端,记日志后忽略。
		logx.Errorf("[scenenode:%s] 节点缺少 grpcEndpoint,忽略: key=%s zone=%d node=%d",
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

func (w *Watcher) reportCount(count int) {
	if w.onCount != nil {
		w.onCount(w.name, count)
	}
}

// EntryOf 按 (zone_id, node_id) 找节点的完整登记(Endpoint + NodeUuid 等)。
// nodeId 是 PlayerLocation.node_id 的十进制字符串形态。
// 同一身份出现两条注册说明租约/部署链分叉,任何一条都不能被安全选中
// (与 match EntryOf、scene_manager resolveFromKnownNodes 的歧义拒绝语义一致):
// 猜错节点就是把资产操作发给不持有该玩家的进程,轻则白丢一次调用,重则两个
// 节点各自认为自己是持有者。宁可报错让调用方重试。
//
// 本函数不替调用方判空:拿到空 NodeUuid 时需要实例身份的调用方必须不发
// (fail-closed)。资产通道走 gRPC 直连,只用 Endpoint。
func (w *Watcher) EntryOf(zoneId uint32, nodeId string) (NodeEntry, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var found NodeEntry
	matches := 0
	for _, entry := range w.nodes {
		if entry.ZoneId != zoneId || strconv.FormatUint(uint64(entry.NodeId), 10) != nodeId {
			continue
		}
		matches++
		if matches > 1 && found.Endpoint != entry.Endpoint {
			return NodeEntry{}, fmt.Errorf("节点身份歧义 zone=%d node=%s: 多个 endpoint 注册", zoneId, nodeId)
		}
		found = entry
	}
	if matches > 1 {
		return NodeEntry{}, fmt.Errorf("节点身份歧义 zone=%d node=%s: %d 条注册", zoneId, nodeId, matches)
	}
	if matches == 0 {
		return NodeEntry{}, fmt.Errorf("节点未注册 zone=%d node=%s", zoneId, nodeId)
	}
	return found, nil
}

// EndpointOf 按 (zone_id, node_id) 找节点 gRPC 地址;查找与歧义拒绝语义见 EntryOf。
func (w *Watcher) EndpointOf(zoneId uint32, nodeId string) (string, error) {
	entry, err := w.EntryOf(zoneId, nodeId)
	if err != nil {
		return "", err
	}
	return entry.Endpoint, nil
}

// Count 返回镜像里的节点数量。
func (w *Watcher) Count() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.nodes)
}
