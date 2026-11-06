// Package discovery 维护路由服对各目标业务服务(login / match / chat / …)的
// etcd list-watch 内存镜像,以及按 endpoint 缓存的 gRPC 连接。
//
// 本文件复制自 go/match/internal/discovery/node_watcher.go(match 对 scene / battle
// 的同一套 list-watch + 连接缓存),只增加了路由服自己的两种选法:PickRandomInZone
// (设计决策 D33 的 zone 过滤)与 PickRandomForTrack(按会话的发布轨道选全局实例,
// docs/design/release-track-gray-canary.md §2.4)。两份副本后续收成 go/shared 一份
// (设计文档 §5「后续」);改 watch / 连接缓存行为时请同步另一份。
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shared/etcdwatch"
	"shared/releasetrack"

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
	// ReleaseTrack 是 NodeInfo.release_track;没写(空串)按 stable。
	ReleaseTrack string `json:"releaseTrack"`
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
	// Track 是节点的发布轨道,解析注册值时已校验;轨道无效的节点不进镜像。
	Track releasetrack.Track
}

// NodeWatcher 对一个 etcd 前缀做 list-watch,维护 key -> NodeEntry 镜像。
type NodeWatcher struct {
	name     string // 日志/指标用途(节点类型名,如 "LoginNodeService")
	prefix   string // 例如 "LoginNodeService.rpc/"
	mu       sync.RWMutex
	nodes    map[string]NodeEntry
	onCount  func(kind string, count int) // 镜像规模变化上报(指标)
	onRemove func(entry NodeEntry)        // 节点消失回调(清连接缓存)

	// index 是选点用的只读索引,镜像每变一次就在 mu 写锁里整体重建、原子替换(见 rebuildIndexLocked)。
	// 节点上下线才变,选点是每个请求一次:以前每个请求都在读锁里把全服镜像拷一遍再过滤,开销随全服实例数增长
	// (缺口清单第 28 条);现在选点无锁读这份索引,O(1)。
	index atomic.Pointer[pickIndex]
}

// pickIndex 是某一时刻镜像的选点视图,建好之后不再修改。
type pickIndex struct {
	byZone map[uint32][]NodeEntry
	// trackGroups 每条发布轨道至多一组、且只放非空组。按轨道选点仍交给 releasetrack.Select 判断
	// (「canary 没有实例才退回 stable」由它在组之间决定),规则只有那一份,两条轨道不并进同一个列表。
	trackGroups []trackGroup
}

type trackGroup struct {
	track   releasetrack.Track
	entries []NodeEntry
}

func buildPickIndex(nodes map[string]NodeEntry) *pickIndex {
	index := &pickIndex{byZone: make(map[uint32][]NodeEntry)}
	byTrack := make(map[releasetrack.Track][]NodeEntry)
	for _, entry := range nodes {
		index.byZone[entry.ZoneId] = append(index.byZone[entry.ZoneId], entry)
		byTrack[entry.Track] = append(byTrack[entry.Track], entry)
	}
	for track, entries := range byTrack {
		index.trackGroups = append(index.trackGroups, trackGroup{track: track, entries: entries})
	}
	return index
}

// rebuildIndexLocked 按当前镜像重建选点索引。调用方持 mu 写锁:索引的替换顺序与镜像的修改顺序一致。
func (w *NodeWatcher) rebuildIndexLocked() {
	w.index.Store(buildPickIndex(w.nodes))
}

// NewNodeWatcher 构造 watcher;onCount / onRemove 可为 nil。
func NewNodeWatcher(name, prefix string, onCount func(string, int), onRemove func(NodeEntry)) *NodeWatcher {
	w := &NodeWatcher{
		name:     name,
		prefix:   prefix,
		nodes:    make(map[string]NodeEntry),
		onCount:  onCount,
		onRemove: onRemove,
	}
	w.rebuildIndexLocked() // 还没被共享,不用加锁;保证 index 永不为 nil
	return w
}

// Name 返回 watcher 名(节点类型名)。
func (w *NodeWatcher) Name() string { return w.name }

// Run 阻塞式跑 list-watch 循环:fullSync 建镜像 → watch 增量;watch 断开重来。
// 调用方负责放进 goroutine(safego.Go)。
func (w *NodeWatcher) Run(ctx context.Context, etcd *clientv3.Client) {
	for {
		rev, err := w.fullSync(ctx, etcd)
		if err != nil {
			logx.Errorf("[NodeWatcher:%s] Full synchronization failed: %v; retrying after 3s", w.name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		periodic := w.watch(ctx, etcd, rev)
		if ctx.Err() != nil {
			return
		}
		if periodic {
			logx.Debugf("[NodeWatcher:%s] Periodic resynchronization", w.name)
			continue
		}
		logx.Infof("[NodeWatcher:%s] Watch interrupted; synchronizing again", w.name)
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
	w.rebuildIndexLocked()
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
	logx.Infof("[NodeWatcher:%s] Full synchronization complete: %d nodes, rev=%d", w.name, count, resp.Header.Revision)
	return resp.Header.Revision, nil
}

// watch 从 rev+1 起消费事件,返回即表示需要重新全量同步。periodic 为真表示是到了定期对账的点
// (etcdwatch.ResyncInterval),不是 watch 出了问题。watch 带 RequireLeader,返回前取消子 ctx
// (docs/design/infra-reconnect-overview.md §4 R1)。
func (w *NodeWatcher) watch(ctx context.Context, etcd *clientv3.Client, rev int64) (periodic bool) {
	watchCh, resync, stop := etcdwatch.Open(ctx, etcd, etcdwatch.ResyncInterval, w.prefix,
		clientv3.WithPrefix(),
		clientv3.WithRev(rev+1),
	)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-resync:
			return true
		case watchResp, ok := <-watchCh:
			if !ok {
				logx.Infof("[NodeWatcher:%s] Watch channel closed", w.name)
				return false
			}
			if watchResp.Err() != nil {
				logx.Errorf("[NodeWatcher:%s] Watch error: %v", w.name, watchResp.Err())
				return false
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
			w.rebuildIndexLocked()
		}
		count := len(w.nodes)
		w.mu.Unlock()
		if !existed {
			return
		}
		if w.onRemove != nil {
			w.onRemove(entry)
		}
		logx.Infof("[NodeWatcher:%s] Node offline: zone=%d node=%d endpoint=%s",
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
	if !existed || prev != entry {
		w.rebuildIndexLocked()
	}
	count := len(w.nodes)
	w.mu.Unlock()
	// 重启后 endpoint 可能变了,旧连接必须废弃。
	if existed && w.onRemove != nil && prev.Endpoint != entry.Endpoint {
		w.onRemove(prev)
	}
	if !existed {
		logx.Infof("[NodeWatcher:%s] Node online: zone=%d node=%d endpoint=%s release_track=%s",
			w.name, entry.ZoneId, entry.NodeId, entry.Endpoint, entry.Track)
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
		logx.Errorf("[NodeWatcher:%s] Failed to parse registration value key=%s: %v", w.name, key, err)
		return NodeEntry{}, false
	}
	if reg.GrpcEndpoint.Port == 0 {
		// 没有 gRPC endpoint 的节点无法作为转发目标,记日志后忽略。
		logx.Errorf("[NodeWatcher:%s] Node missing grpcEndpoint; ignoring: key=%s zone=%d node=%d",
			w.name, key, reg.ZoneId, reg.NodeId)
		return NodeEntry{}, false
	}
	track, err := releasetrack.Parse(reg.ReleaseTrack)
	if err != nil {
		// 认不出的轨道既不能当 stable(stable 会话会被送进它),也不能当 canary,只能整条跳过。
		logx.Errorf("[NodeWatcher:%s] Node has an unknown release track; ignoring: key=%s zone=%d node=%d: %v",
			w.name, key, reg.ZoneId, reg.NodeId, err)
		return NodeEntry{}, false
	}
	return NodeEntry{
		NodeId:   reg.NodeId,
		ZoneId:   reg.ZoneId,
		NodeUuid: reg.NodeUuid,
		Endpoint: fmt.Sprintf("%s:%d", reg.GrpcEndpoint.IP, reg.GrpcEndpoint.Port),
		Track:    track,
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

// PickRandomForTrack 是全局池节点类型的选法:先按会话的发布轨道留下可用的实例
// (releasetrack.Select:canary 会话有 canary 就只用 canary、没有退回 stable;stable 会话只用 stable),
// 再在其中均匀随机取一个(负载感知不在本期)。没有可用实例返回 false。
// Select 在轨道组之间判断(每条轨道至多一组、组都非空),所以「退回」的语义与按实例过滤时完全相同。
func (w *NodeWatcher) PickRandomForTrack(session releasetrack.Track) (NodeEntry, bool) {
	chosen := releasetrack.Select(session, w.index.Load().trackGroups, func(group trackGroup) releasetrack.Track { return group.track })
	if len(chosen) == 0 {
		return NodeEntry{}, false
	}
	return pickRandom(chosen[0].entries)
}

// PickRandomInZone 只在 zone_id 相同的节点里随机取一个(zone-scoped 节点类型
// 的选法,设计决策 D33:login 只服务本 zone,跨 zone 打过去会话对不上)。不看发布轨道。
// 该 zone 没有实例返回 false —— 不回退到其它 zone。
func (w *NodeWatcher) PickRandomInZone(zoneId uint32) (NodeEntry, bool) {
	return pickRandom(w.index.Load().byZone[zoneId])
}

func pickRandom(candidates []NodeEntry) (NodeEntry, bool) {
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

	logx.Infof("[discovery] Connected to node %s", endpoint)
	return conn, nil
}

// RemoveEndpointConn 关闭并移除某 endpoint 的缓存连接。
func RemoveEndpointConn(endpoint string) {
	connMu.Lock()
	defer connMu.Unlock()
	if conn, ok := connCache[endpoint]; ok {
		_ = conn.Close()
		delete(connCache, endpoint)
		logx.Infof("[discovery] Removed node connection %s", endpoint)
	}
}
