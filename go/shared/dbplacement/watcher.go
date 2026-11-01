package dbplacement

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/authoritysql"
	"shared/safego"
)

const (
	watchPoint = "dbplacement.watch"

	// syncTimeout 是一次全量读的时限,启动时也是这个。
	syncTimeout = 5 * time.Second
	// resyncBackoff 是 Watch 断开之后全量读失败的重试间隔。
	resyncBackoff = 2 * time.Second
	// resyncEvery 是定期全量读的间隔。Watch 正常时它什么都不改;漏了事件时它把窗口限制在这么久以内。
	// 漏掉翻转的后果是这个进程一直连冻结的旧集群,写不进去,所以值得多读这一次。
	resyncEvery = 30 * time.Second
)

// 这几类拒绝的理由写在 applyPut、applyDelete、noteRevisionRegress 上。
const (
	rejectBadValue        = "bad_value"
	rejectVersionRegress  = "version_regress"
	rejectDeleted         = "deleted_after_move"
	rejectRevisionRegress = "revision_regress"
)

type watchClient interface {
	keyReader
	Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan
}

type entry struct {
	placement   Placement
	modRevision int64
}

// Watcher 跟着 etcd 维护全部逻辑库的映射,实现 authoritysql.PlacementResolver。
//
// etcd 故障时继续用最后一次读到的映射:连接照常建,落点错了也写不进去(旧集群已冻结,闸门拒绝)。
// 读路径的连接池(Reader 权限)不经闸门,这段时间可能读到冻结时的数据,手册里写明。
type Watcher struct {
	cfg    Config
	client watchClient
	closer io.Closer
	cancel context.CancelFunc
	done   chan struct{}

	syncTimeout   time.Duration
	resyncBackoff time.Duration
	resyncEvery   time.Duration

	mu      sync.RWMutex
	entries map[string]entry
	// broken 是读到坏值、之前又没有好值的逻辑库:不知道它在哪,不能回退到默认集群。
	broken map[string]bool
	// tracked 是这个进程连过的逻辑库。有键的库读到就上报版本,没有键的库靠它在第一次连接时报一个 0。
	tracked map[string]bool
}

// Start 读一次全量映射,然后在后台跟 Watch。配置没启用时返回 nil,nil 的方法都能调。
//
// ctx 只约束启动这一次读;读不到就报错拒绝启动:分不清「没有映射」和「读不到」的进程可能连到已经搬走的库。
// 常驻服务不用 Close,进程退出时回收。
func Start(ctx context.Context, cfg Config) (*Watcher, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client, err := Dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("db placement: connect etcd: %w", err)
	}
	ensureCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	err = EnsureDefaultCluster(ensureCtx, client, cfg)
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	w := newWatcher(cfg, client)
	w.closer = client
	if err := w.start(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return w, nil
}

func newWatcher(cfg Config, client watchClient) *Watcher {
	return &Watcher{
		cfg:           cfg,
		client:        client,
		syncTimeout:   syncTimeout,
		resyncBackoff: resyncBackoff,
		resyncEvery:   resyncEvery,
		entries:       map[string]entry{},
		broken:        map[string]bool{},
		tracked:       map[string]bool{},
	}
}

func (w *Watcher) start(ctx context.Context) error {
	rev, err := w.fullSync(ctx)
	if err != nil {
		return fmt.Errorf("db placement: initial read: %w", err)
	}
	setKnownClusters(w.cfg.SqlClusters)
	runCtx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	safego.Go(watchPoint, func() {
		defer close(w.done)
		w.run(runCtx, rev)
	})
	return nil
}

// Close 停掉后台 Watch。之后 Current 和 ResolvePlacement 还能调,回答最后知道的映射。
func (w *Watcher) Close() error {
	if w == nil {
		return nil
	}
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
	if w.closer != nil {
		return w.closer.Close()
	}
	return nil
}

// Current 返回逻辑库当前的映射。没有键时是默认集群、版本 0。只读到过坏值时报错。
func (w *Watcher) Current(logicalDB string) (Placement, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.currentLocked(logicalDB)
}

func (w *Watcher) currentLocked(logicalDB string) (Placement, error) {
	if w.broken[logicalDB] {
		return Placement{}, fmt.Errorf("db placement: the mapping of %s cannot be parsed", logicalDB)
	}
	if e, ok := w.entries[logicalDB]; ok {
		return e.placement, nil
	}
	return Placement{Cluster: w.cfg.DefaultCluster, State: StateActive}, nil
}

// ResolvePlacement 实现 authoritysql.PlacementResolver。
func (w *Watcher) ResolvePlacement(logicalDB string) (string, uint64, error) {
	p, err := w.Current(logicalDB)
	if err != nil {
		return "", 0, err
	}
	cluster, ok := w.cfg.SqlClusters[p.Cluster]
	if !ok {
		return "", 0, fmt.Errorf("db placement: %s is on cluster %q, which SqlClusters does not list", logicalDB, p.Cluster)
	}
	w.track(logicalDB)
	return cluster.Hosts, p.Version, nil
}

// OpenOptions 给 authoritysql.Open 用。nil Watcher(没启用映射)时是空的。
func (w *Watcher) OpenOptions() []authoritysql.OpenOption {
	if w == nil {
		return nil
	}
	return []authoritysql.OpenOption{authoritysql.WithPlacement(w)}
}

// Resolver 给 authoritysql.PinPlacedSession 用。nil Watcher 时返回 nil 接口,调用方按没有映射处理。
func (w *Watcher) Resolver() authoritysql.PlacementResolver {
	if w == nil {
		return nil
	}
	return w
}

func (w *Watcher) track(logicalDB string) {
	w.mu.RLock()
	tracked := w.tracked[logicalDB]
	w.mu.RUnlock()
	if tracked {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tracked[logicalDB] = true
	w.reportLocked(logicalDB)
}

// reportLocked 上报版本。只要进程配了映射就报,不等它连过这个库:按需开池的进程可能还没连过,audit 不能因为这个报它落后。
// 映射指向本进程不认识的集群时不更新:这个进程实际连不过去,指标停在旧版本,dbmove audit 才看得出来。
func (w *Watcher) reportLocked(logicalDB string) {
	p, err := w.currentLocked(logicalDB)
	if err != nil {
		return
	}
	if _, ok := w.cfg.SqlClusters[p.Cluster]; !ok {
		return
	}
	setVersion(logicalDB, p.Version)
}

func (w *Watcher) run(ctx context.Context, rev int64) {
	for {
		w.watch(ctx, rev)
		for {
			if ctx.Err() != nil {
				return
			}
			next, err := w.fullSync(ctx)
			if err == nil {
				rev = next
				break
			}
			countSyncFail()
			logx.Errorf("[dbplacement] resync failed, keeping the last known mapping: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.resyncBackoff):
			}
		}
	}
}

// watch 从 rev 之后消费事件。返回表示要重新全量读(或者 ctx 结束)。
// WithRequireLeader:连的 etcd 成员失去 leader 时 Watch 报错,而不是不声不响地停住。
func (w *Watcher) watch(ctx context.Context, rev int64) {
	watchCtx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
	defer cancel()
	ch := w.client.Watch(watchCtx, KeyPrefix, clientv3.WithPrefix(), clientv3.WithRev(rev+1))
	ticker := time.NewTicker(w.resyncEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.fullSync(ctx); err != nil {
				countSyncFail()
				logx.Errorf("[dbplacement] periodic read failed, keeping the last known mapping: %v", err)
			}
		case resp, ok := <-ch:
			if !ok {
				logx.Info("[dbplacement] watch channel closed; resyncing")
				return
			}
			if err := resp.Err(); err != nil {
				countSyncFail()
				logx.Errorf("[dbplacement] watch failed; resyncing: %v", err)
				return
			}
			for _, ev := range resp.Events {
				if ev == nil || ev.Kv == nil {
					continue
				}
				name, ok := LogicalDBOfKey(string(ev.Kv.Key))
				if !ok {
					continue
				}
				switch ev.Type {
				case clientv3.EventTypePut:
					w.applyPut(name, ev.Kv.Value, ev.Kv.ModRevision)
				case clientv3.EventTypeDelete:
					w.applyDelete(name, ev.Kv.ModRevision)
				}
			}
		}
	}
}

// fullSync 读全部映射并合并,返回读到的 etcd revision。
func (w *Watcher) fullSync(ctx context.Context) (int64, error) {
	readCtx, cancel := context.WithTimeout(ctx, w.syncTimeout)
	defer cancel()
	resp, err := w.client.Get(readCtx, KeyPrefix, clientv3.WithPrefix())
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		name, ok := LogicalDBOfKey(string(kv.Key))
		if !ok {
			continue
		}
		seen[name] = true
		w.noteRevisionRegress(name, kv.ModRevision)
		w.applyPut(name, kv.Value, kv.ModRevision)
	}
	w.mu.RLock()
	var gone []string
	for name := range w.entries {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	for name := range w.broken {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	w.mu.RUnlock()
	for _, name := range gone {
		w.applyDelete(name, resp.Header.Revision)
	}
	return resp.Header.Revision, nil
}

// noteRevisionRegress 记全量读读到比已知更小的 ModRevision。Watch 的旧事件在 applyPut 里跳过是正常的,全量读读的是当前值,
// 读到更小只可能是 etcd 从备份恢复时没有 --bump-revision:这时之后写的每个键都比进程记下的小,applyPut 全部跳过,
// 翻转也跟不上(写不进去,不会写错地方)。计数并打日志,否则这种跳过完全看不见。
func (w *Watcher) noteRevisionRegress(logicalDB string, modRevision int64) {
	w.mu.RLock()
	cur, known := w.entries[logicalDB]
	w.mu.RUnlock()
	if known && modRevision < cur.modRevision {
		countRejected(rejectRevisionRegress)
		logx.Errorf("[dbplacement] the mod revision of %s went backwards (have %d, read %d): etcd was restored without --bump-revision. This process ignores the mapping until it restarts or the revision catches up",
			logicalDB, cur.modRevision, modRevision)
	}
}

// applyPut 把一条键值并进映射。
//
//   - ModRevision 不比已有的大就跳过,不计数:全量读和 Watch 的事件会有重叠,旧事件重放是常态。
//     所以 etcd 回档之后的小 revision 也走到这里,先于下面的版本判定,db_placement_rejected_total{version_regress} 不涨。
//     回档时用 --bump-revision(手册 §6),revision 才比回档之前大,回退的值走到下面的版本判定。全量读单独记这种回退,见 noteRevisionRegress。
//   - 坏值不覆盖好值。之前没有好值时标成 broken:不知道它在哪,建连报错,不回退到默认集群。
//   - 版本回退、同版本换集群不接受:版本只在翻转时加 1,这两种都不是工具写的(手改或 etcd 回档)。
//     留在原来的映射上最坏是写不进去,跟着走可能回到一个没人冻结的旧集群。
func (w *Watcher) applyPut(logicalDB string, raw []byte, modRevision int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, known := w.entries[logicalDB]
	if known && modRevision <= cur.modRevision {
		return
	}
	next, err := Decode(raw)
	if err != nil {
		countRejected(rejectBadValue)
		logx.Errorf("[dbplacement] ignoring a bad mapping of %s: %v", logicalDB, err)
		if !known {
			w.broken[logicalDB] = true
		}
		return
	}
	if known && (next.Version < cur.placement.Version || (next.Version == cur.placement.Version && next.Cluster != cur.placement.Cluster)) {
		countRejected(rejectVersionRegress)
		logx.Errorf("[dbplacement] ignoring a mapping of %s that goes backwards: have cluster=%s version=%d, got cluster=%s version=%d",
			logicalDB, cur.placement.Cluster, cur.placement.Version, next.Cluster, next.Version)
		return
	}
	w.entries[logicalDB] = entry{placement: next, modRevision: modRevision}
	delete(w.broken, logicalDB)
	if !known || next.Version != cur.placement.Version || next.State != cur.placement.State {
		logx.Infof("[dbplacement] %s: cluster=%s version=%d state=%s", logicalDB, next.Cluster, next.Version, next.State)
	}
	w.reportLocked(logicalDB)
}

// applyDelete 处理键被删。版本 0 的记录删了等于回到默认集群;搬过的库不接受:默认集群已经不是它的家了。
func (w *Watcher) applyDelete(logicalDB string, modRevision int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.broken, logicalDB)
	cur, known := w.entries[logicalDB]
	if !known || modRevision <= cur.modRevision {
		return
	}
	if cur.placement.Version != 0 {
		countRejected(rejectDeleted)
		logx.Errorf("[dbplacement] ignoring the deletion of %s: it was moved (version %d), the default cluster is not its home", logicalDB, cur.placement.Version)
		return
	}
	delete(w.entries, logicalDB)
	logx.Infof("[dbplacement] %s: mapping deleted, back to the default cluster", logicalDB)
	w.reportLocked(logicalDB)
}
