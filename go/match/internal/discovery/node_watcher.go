// Package discovery 维护 match 对 scene / battle 节点的 etcd list-watch 内存镜像,
// 以及按 endpoint 缓存的 gRPC 连接。照 scene_manager 的
// LoadReporter(list-watch)+ scene_node_client(连接缓存)模式裁剪:
// match 只读发现结果,不写 Redis 负载分,也没有领导者语义。
package discovery

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	base "proto/common/base"
	"shared/etcdwatch"
	"shared/nodeinfo"
	"shared/releasetrack"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NodeEntry 是 watch 镜像里的一个节点。
type NodeEntry struct {
	NodeId         uint32
	ZoneId         uint32
	NodeUuid       string
	PhysicalRegion uint32
	ClientHost     string
	ClientPort     uint32
	// Endpoint 是 gRPC 地址 "ip:port"。只认 grpcEndpoint,绝不回退
	// endpoint(那是 raw TCP protobuf 口,用 gRPC 拨会协议错乱,
	// 先例见 scene_manager resolveFromKnownNodes 的注释)。
	Endpoint string
	// Admission 是节点自报的可分配状态(NodeInfo.admission,R6):只有 battle 填,其它节点恒为 UNSPECIFIED。
	// 只是选点提示(battleplace 排除 CLOSED),建房能否成功仍由 battle 的准入判定。
	Admission base.NodeAdmission
	// Track 是节点的发布轨道(NodeInfo.release_track,解析时已校验)。
	Track releasetrack.Track
}

// NodeWatcher 对一个 etcd 前缀做 list-watch,维护 key -> NodeEntry 镜像。
type NodeWatcher struct {
	name     string // 日志/指标用途("scene" / "battle")
	prefix   string // 例如 "SceneNodeService.rpc/"
	mu       sync.RWMutex
	nodes    map[string]NodeEntry
	onCount  func(kind string, count int) // 镜像规模变化上报(指标)
	onRemove func(entry NodeEntry)        // 节点消失回调(清连接缓存)
	// stableOnly 为真时 canary 节点不进镜像,见 StableOnly。
	stableOnly bool
}

// StableOnly 让镜像只收 stable 节点(docs/design/release-track-gray-canary.md §2.5),返回 w 以便接在构造之后。
// match 对 battle、match、player_inbox 的调用都是服务间调用,匹配池归属也只能落在 stable 上:
// 若把 canary 的 MatchNodeService 算进 HRW 成员表,落到它名下的池 stable 既不撮合也不恢复,这些池就停了。
// 在解析时就丢掉,所以快照、随机选点和按节点号查找都看不到 canary。scene 按 zone 选,不用它。必须在 Run 之前调用。
func (w *NodeWatcher) StableOnly() *NodeWatcher {
	w.stableOnly = true
	return w
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
	count := len(w.nodes)
	w.mu.Unlock()
	// 重启后 endpoint 可能变了,旧连接必须废弃。
	if existed && w.onRemove != nil && prev.Endpoint != entry.Endpoint {
		w.onRemove(prev)
	}
	if !existed {
		logx.Infof("[NodeWatcher:%s] Node online: zone=%d node=%d endpoint=%s",
			w.name, entry.ZoneId, entry.NodeId, entry.Endpoint)
	}
	w.reportCount(count)
}

func (w *NodeWatcher) parse(key string, value []byte) (NodeEntry, bool) {
	// noderegistry 会在同前缀下写 "/allocated/" 占位 key,不是节点注册,跳过。
	if strings.Contains(key, "/allocated/") {
		return NodeEntry{}, false
	}
	// 值是 protojson 序列化的 NodeInfo(C++ MessageToJsonString 与 Go noderegistry 都是 camelCase、枚举写名字)。
	// 经 shared/nodeinfo 解析:未知字段与未知枚举名被忽略(滚动期间新版本节点多带的字段不让整台节点从镜像里消失,D77),
	// 枚举按名字或数字都认。以前用 encoding/json 镜像结构解析,枚举字段要另写一套 protojson 的取值规则,改走统一入口。
	var reg base.NodeInfo
	if err := nodeinfo.Unmarshal(value, &reg); err != nil {
		logx.Errorf("[NodeWatcher:%s] Failed to parse registration value key=%s: %v", w.name, key, err)
		return NodeEntry{}, false
	}
	grpcEndpoint := reg.GetGrpcEndpoint()
	if grpcEndpoint.GetPort() == 0 {
		// 没有 gRPC endpoint 的节点无法作为 gather 对端,记日志后忽略。
		logx.Errorf("[NodeWatcher:%s] Node missing grpcEndpoint; ignoring: key=%s zone=%d node=%d",
			w.name, key, reg.GetZoneId(), reg.GetNodeId())
		return NodeEntry{}, false
	}
	track, err := releasetrack.Parse(reg.GetReleaseTrack())
	if err != nil {
		// 认不出的轨道既不能当 stable 也不能当 canary,整条跳过。
		logx.Errorf("[NodeWatcher:%s] Node has an unknown release track; ignoring: key=%s zone=%d node=%d: %v",
			w.name, key, reg.GetZoneId(), reg.GetNodeId(), err)
		return NodeEntry{}, false
	}
	if w.stableOnly && track.IsCanary() {
		return NodeEntry{}, false
	}
	return NodeEntry{
		NodeId:         reg.GetNodeId(),
		ZoneId:         reg.GetZoneId(),
		NodeUuid:       reg.GetNodeUuid(),
		PhysicalRegion: reg.GetPhysicalRegion(),
		ClientHost:     reg.GetClientEndpoint().GetIp(),
		ClientPort:     reg.GetClientEndpoint().GetPort(),
		Endpoint:       fmt.Sprintf("%s:%d", grpcEndpoint.GetIp(), grpcEndpoint.GetPort()),
		Admission:      reg.GetAdmission(),
		Track:          track,
	}, true
}

// Snapshot 返回独立副本；全局匹配根据真实机房选择执行节点，不把逻辑zone当地域。
func (w *NodeWatcher) Snapshot() []NodeEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]NodeEntry, 0, len(w.nodes))
	for _, n := range w.nodes {
		out = append(out, n)
	}
	return out
}

func (w *NodeWatcher) reportCount(count int) {
	if w.onCount != nil {
		w.onCount(w.name, count)
	}
}

// EntryOf 按 (zone_id, node_id) 找节点的完整登记(Endpoint + NodeUuid 等)。
// nodeId 是 PlayerLocation.node_id 的十进制字符串形态。
// 同一身份出现两条注册说明租约/部署链分叉,任何一条都不能被安全选中
// (与 scene_manager resolveFromKnownNodes 的歧义拒绝语义一致)。
//
// 用途:给 scene 节点发 Kafka 命令要填 target_instance_id(AGENTS.md §7 #2 防僵尸),
// 取的就是这里的 NodeUuid。本函数不替调用方判空:拿到空 NodeUuid 时调用方必须不发
// (fail-closed,team-system.md §F.3)。
func (w *NodeWatcher) EntryOf(zoneId uint32, nodeId string) (NodeEntry, error) {
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
func (w *NodeWatcher) EndpointOf(zoneId uint32, nodeId string) (string, error) {
	entry, err := w.EntryOf(zoneId, nodeId)
	if err != nil {
		return "", err
	}
	return entry.Endpoint, nil
}

// EntryOfNode 按 node_id 找节点的完整登记(battle 节点是全局池不分 zone,
// 观战路径按 SpectateBattleRecord.battle_node_id 定位对端,并取 NodeUuid 作 battle
// 观战账本要求的 target_instance_id;歧义拒绝语义与 EntryOf 一致:同一 node_id
// 出现多条注册说明租约/部署链分叉,拒选)。与 EntryOf 一样不替调用方判空 NodeUuid。
func (w *NodeWatcher) EntryOfNode(nodeId uint32) (NodeEntry, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var found NodeEntry
	matches := 0
	for _, entry := range w.nodes {
		if entry.NodeId != nodeId {
			continue
		}
		matches++
		if matches > 1 && found.Endpoint != entry.Endpoint {
			return NodeEntry{}, fmt.Errorf("节点身份歧义 node=%d: 多个 endpoint 注册", nodeId)
		}
		found = entry
	}
	if matches > 1 {
		return NodeEntry{}, fmt.Errorf("节点身份歧义 node=%d: %d 条注册", nodeId, matches)
	}
	if matches == 0 {
		return NodeEntry{}, fmt.Errorf("节点未注册 node=%d", nodeId)
	}
	return found, nil
}

// PickRandom 在镜像里等概率随机取一个节点;镜像为空返回 false。
func (w *NodeWatcher) PickRandom() (NodeEntry, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	candidates := make([]NodeEntry, 0, len(w.nodes))
	for _, entry := range w.nodes {
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return NodeEntry{}, false
	}
	return candidates[rand.Intn(len(candidates))], true
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
