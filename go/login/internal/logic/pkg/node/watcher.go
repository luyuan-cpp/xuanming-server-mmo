package node

import (
	"context"
	"fmt"
	"login/internal/config"
	login_proto "proto/common/base"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// watchRetryDelay:全量同步失败后的重试间隔。期间镜像逐渐过期,读路径按 cacheStaleAfter 回落直读。
const watchRetryDelay = 3 * time.Second

// silenceCheckEvery:watch 循环检查「是否静默过久」的节拍,只是一次加锁读时间戳。
const silenceCheckEvery = time.Second

// gateDirectoryReads 记 Nodes 每次是读镜像还是回落直读 etcd(source 取 readSource* 常量,封闭集合)。
// 稳态应几乎全是 cache;etcd_stale 持续不为 0 说明 etcd 不可达或 watch 反复卡住,登录又回到了逐请求打 etcd。
var gateDirectoryReads = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "gate_directory",
	Name:      "reads_total",
	Help:      "Gate directory reads for AssignGate / queue dispatch, by source: cache, etcd_unsynced, etcd_stale.",
	Labels:    []string{"source"},
})

// watchTimers 是 watch 循环用到的全部计时原语,显式注入,单测用手动驱动的实现替换,不碰真实墙钟。
type watchTimers struct {
	// after 是全量同步失败后的重试等待。
	after func(time.Duration) <-chan time.Time
	// newTicker 返回静默检查节拍与停止函数。
	newTicker func(time.Duration) (<-chan time.Time, func())
	// afterFunc 在 d 之后调用 f,返回的 stop 在 f 尚未触发时阻止它并返回 true(同 time.Timer.Stop)。
	afterFunc func(d time.Duration, f func()) (stop func() bool)
}

var realWatchTimers = watchTimers{
	after: time.After,
	newTicker: func(d time.Duration) (<-chan time.Time, func()) {
		t := time.NewTicker(d)
		return t.C, t.Stop
	},
	afterFunc: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
}

// NodeWatcher 读一个 etcd 前缀下的节点注册(login 用它看本 zone 的 gate)。两种读法,用途不同:
//
//   - FetchAllNodes / FetchAllNodesCtx:每次一次线性一致的前缀 Range,结果最新。gate 排空监视用它:
//     那条路径要 fail-closed(读不到就本轮不判),不能拿可能过期的镜像当事实。
//   - Nodes:读 Run 维护的进程内 list-watch 镜像;镜像还没同步、或有节点超过 cacheStaleAfter 没刷新时,
//     回落到 FetchAllNodesCtx。AssignGate 快速通道与排队 dispatcher 用它 —— 它们本来就容忍 gate 人数
//     约 10s 的上报滞后,没必要每个请求打一次 etcd(容量模型 A0-7)。读镜像时每台节点带停写判定
//     (WatchedNode.Stale,见 nodeDataStaleAfter),直读没有历史可比,不判。
//
// Run 不起时 Nodes 永远回落直读,行为与引入镜像之前完全相同(也就没有停写判定)。
type NodeWatcher struct {
	kv clientv3.KV
	// newWatcher 每轮 watch 建一个独立的 Watcher,用完 Close(见 watch)。
	newWatcher func() clientv3.Watcher
	prefix     string
	cache      *nodeCache
	timers     watchTimers
}

// NewNodeWatcher creates a new NodeWatcher instance.
func NewNodeWatcher(client *clientv3.Client, prefix string) *NodeWatcher {
	return newNodeWatcher(client, func() clientv3.Watcher { return clientv3.NewWatcher(client) },
		prefix, time.Now, realWatchTimers)
}

func newNodeWatcher(kv clientv3.KV, newWatcher func() clientv3.Watcher, prefix string,
	now func() time.Time, timers watchTimers) *NodeWatcher {
	return &NodeWatcher{
		kv:         kv,
		newWatcher: newWatcher,
		prefix:     prefix,
		cache:      newNodeCache(prefix, now),
		timers:     timers,
	}
}

// FetchAllNodes 直读 etcd,超时取 ServiceDiscoveryTimeout。不接 ctx 的调用方(排空监视)用它。
func (nw *NodeWatcher) FetchAllNodes() ([]*login_proto.NodeInfo, error) {
	return nw.FetchAllNodesCtx(context.Background())
}

// FetchAllNodesCtx 直读 etcd:一次线性一致的前缀 Range。时长上界取 ctx 截止时间与
// ServiceDiscoveryTimeout 中较早的那个,请求被取消时随之放弃,不在 etcd 抖动时堆积 goroutine。
func (nw *NodeWatcher) FetchAllNodesCtx(ctx context.Context) ([]*login_proto.NodeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, config.AppConfig.Timeouts.ServiceDiscoveryTimeout)
	defer cancel()

	resp, err := nw.kv.Get(ctx, nw.prefix, clientv3.WithPrefix())
	if err != nil {
		logx.Errorf("Failed to get keys with prefix %s: %v", nw.prefix, err)
		return nil, fmt.Errorf("failed to fetch nodes: %w", err)
	}

	nodes := make([]*login_proto.NodeInfo, 0, len(resp.Kvs))
	for _, item := range resp.Kvs {
		if info, ok := nw.cache.parse(nodeEvent{key: string(item.Key), value: item.Value}); ok {
			nodes = append(nodes, info)
		}
	}
	return nodes, nil
}

// Nodes 返回本前缀下的全部节点:镜像新鲜就读镜像,否则回落直读(见类型注释)。
// 回落直读时没有历史可比,判不了停写,Stale 一律为 false,与引入停写判定之前的行为相同。
// 返回的切片归调用方;其中的 Info 只读,不得修改(镜像会把同一份交给别的调用方)。
func (nw *NodeWatcher) Nodes(ctx context.Context) ([]WatchedNode, error) {
	nodes, ok, source := nw.cache.view()
	gateDirectoryReads.Inc(source)
	if ok {
		return nodes, nil
	}
	infos, err := nw.FetchAllNodesCtx(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WatchedNode, len(infos))
	for i, info := range infos {
		out[i] = WatchedNode{Info: info}
	}
	return out, nil
}

// Run 阻塞式跑 list-watch:全量同步建镜像 → 从同步时的 revision 往后 watch 增量;watch 出错、被压缩、
// 丢 leader、建流超时,或镜像里有节点静默超过 cacheResyncAfter,就重新全量同步。ctx 取消即返回。
// 调用方负责放进 goroutine,并在停服时取消 ctx。
func (nw *NodeWatcher) Run(ctx context.Context) {
	for {
		rev, err := nw.fullSync(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logx.Errorf("[NodeWatcher:%s] full sync failed: %v; retrying after %s (reads fall back to etcd once the mirror is older than %s)",
				nw.prefix, err, watchRetryDelay, cacheStaleAfter)
			select {
			case <-ctx.Done():
				return
			case <-nw.timers.after(watchRetryDelay):
				continue
			}
		}
		nw.watch(ctx, rev)
		if ctx.Err() != nil {
			return
		}
	}
}

func (nw *NodeWatcher) fullSync(ctx context.Context) (int64, error) {
	syncCtx, cancel := context.WithTimeout(ctx, config.AppConfig.Timeouts.ServiceDiscoveryTimeout)
	defer cancel()
	resp, err := nw.kv.Get(syncCtx, nw.prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, fmt.Errorf("etcd get %s: %w", nw.prefix, err)
	}
	kvs := make([]nodeEvent, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		kvs = append(kvs, nodeEvent{key: string(kv.Key), value: kv.Value, modRev: kv.ModRevision})
	}
	// 没有 gate 的 zone 每 cacheResyncAfter 就同步一次,只在首次或节点数变化时打日志,不刷屏。
	if prev, first := nw.cache.replaceAll(kvs); first || prev != len(kvs) {
		logx.Infof("[NodeWatcher:%s] full sync complete: %d keys (was %d), rev=%d", nw.prefix, len(kvs), prev, resp.Header.Revision)
	}
	return resp.Header.Revision, nil
}

// watch 消费一条 watch 流直到它不再可信。返回即意味着调用方要重新全量同步。
func (nw *NodeWatcher) watch(ctx context.Context, rev int64) {
	// WithRequireLeader:所连成员失去 leader 时流报错,而不是一直静默地不给事件。
	watchCtx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
	defer cancel()
	// 每轮用独立的 Watcher,返回时 Close,把这一轮的 gRPC watch 流整个拆掉。共用 client 内置的 Watcher 时,
	// 相同 metadata 的 ctx 会复用同一条流:流一旦卡在半开连接上,重试会一直落回它(评审 A2)。
	w := nw.newWatcher()
	defer func() { _ = w.Close() }()

	// Watch 要等服务端确认建流才返回,这段时间静默检测还没开始,所以建流单独给一个时限:超时就取消、重同步。
	stop := nw.timers.afterFunc(cacheResyncAfter, cancel)
	watchCh := w.Watch(watchCtx, nw.prefix, clientv3.WithPrefix(), clientv3.WithRev(rev+1))
	if !stop() {
		logx.Errorf("[NodeWatcher:%s] watch not established within %s; resyncing", nw.prefix, cacheResyncAfter)
		return
	}

	tick, stopTick := nw.timers.newTicker(silenceCheckEvery)
	defer stopTick()
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-watchCh:
			if !ok {
				logx.Infof("[NodeWatcher:%s] watch channel closed; resyncing", nw.prefix)
				return
			}
			if err := resp.Err(); err != nil {
				logx.Errorf("[NodeWatcher:%s] watch error: %v; resyncing", nw.prefix, err)
				return
			}
			events := make([]nodeEvent, 0, len(resp.Events))
			for _, ev := range resp.Events {
				events = append(events, nodeEvent{
					key:     string(ev.Kv.Key),
					value:   ev.Kv.Value,
					modRev:  ev.Kv.ModRevision,
					deleted: ev.Type == clientv3.EventTypeDelete,
				})
			}
			nw.cache.apply(events)
		case <-tick:
			// 健康时镜像里每台 gate 每个上报周期都会被刷新;有一台静默过久,要么它漏报 / 还没开始上报,
			// 要么流卡住了。两种情况重新全量同步都对:前者只是多一次 Range,后者把镜像救回来。
			// 空镜像按距上次事件或同步计,于是空 zone 周期性重同步。
			if silent, count := nw.cache.silentFor(); silent > cacheResyncAfter {
				if count > 0 {
					logx.Infof("[NodeWatcher:%s] a mirrored node has not been refreshed for %s (%d nodes); resyncing", nw.prefix, silent, count)
				}
				return
			}
		}
	}
}
