package dbplacement

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	guildDB = "mmorpg_guild"
	mailDB  = "mmorpg_mail"
)

func testConfig() Config {
	return Config{
		DefaultCluster: "main",
		SqlClusters:    map[string]SqlCluster{"main": {Hosts: "10.0.0.1:4000"}, "b": {Hosts: "10.0.0.2:4000"}},
		EtcdHosts:      []string{"127.0.0.1:2379"},
	}
}

func placementValue(t *testing.T, p Placement) []byte {
	t.Helper()
	raw, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeEtcd 是内存里的 etcd:全量读读当前键值,Watch 只把测试塞进去的响应转出来。
type fakeEtcd struct {
	mu     sync.Mutex
	rev    int64
	kvs    map[string]*mvccpb.KeyValue
	getErr error
	opened chan chan clientv3.WatchResponse
}

func newFakeEtcd() *fakeEtcd {
	return &fakeEtcd{kvs: map[string]*mvccpb.KeyValue{}, opened: make(chan chan clientv3.WatchResponse, 8)}
}

func (f *fakeEtcd) put(logicalDB string, raw []byte) *clientv3.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	kv := &mvccpb.KeyValue{Key: []byte(Key(logicalDB)), Value: raw, ModRevision: f.rev}
	f.kvs[logicalDB] = kv
	return &clientv3.Event{Type: clientv3.EventTypePut, Kv: kv}
}

func (f *fakeEtcd) del(logicalDB string) *clientv3.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	delete(f.kvs, logicalDB)
	return &clientv3.Event{Type: clientv3.EventTypeDelete, Kv: &mvccpb.KeyValue{Key: []byte(Key(logicalDB)), ModRevision: f.rev}}
}

func (f *fakeEtcd) failGets(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeEtcd) Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	resp := &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: f.rev}}
	for _, kv := range f.kvs {
		resp.Kvs = append(resp.Kvs, kv)
	}
	return resp, nil
}

func (f *fakeEtcd) Watch(context.Context, string, ...clientv3.OpOption) clientv3.WatchChan {
	ch := make(chan clientv3.WatchResponse, 8)
	f.opened <- ch
	return ch
}

func startFakeWatcher(t *testing.T, etcd *fakeEtcd, cfg Config) (*Watcher, chan clientv3.WatchResponse) {
	t.Helper()
	w := newWatcher(cfg, etcd)
	w.resyncBackoff = time.Millisecond
	w.resyncEvery = time.Hour
	if err := w.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, nextWatch(t, etcd)
}

func nextWatch(t *testing.T, etcd *fakeEtcd) chan clientv3.WatchResponse {
	t.Helper()
	select {
	case ch := <-etcd.opened:
		return ch
	case <-time.After(5 * time.Second):
		t.Fatal("watch was not opened")
		return nil
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func resolvedVersion(w *Watcher, logicalDB string) (string, uint64) {
	addr, version, err := w.ResolvePlacement(logicalDB)
	if err != nil {
		return "", 0
	}
	return addr, version
}

func TestWatcherDefaultsToConfiguredClusterWithoutKey(t *testing.T) {
	w, _ := startFakeWatcher(t, newFakeEtcd(), testConfig())
	addr, version, err := w.ResolvePlacement(guildDB)
	if err != nil || addr != "10.0.0.1:4000" || version != 0 {
		t.Fatalf("没有键时走默认集群、版本 0: addr=%s version=%d err=%v", addr, version, err)
	}
	p, err := w.Current(guildDB)
	if err != nil || p.State != StateActive || p.Cluster != "main" {
		t.Fatalf("Current: %+v %v", p, err)
	}
}

func TestWatcherFollowsFlipThroughWatch(t *testing.T) {
	etcd := newFakeEtcd()
	w, ch := startFakeWatcher(t, etcd, testConfig())
	ch <- clientv3.WatchResponse{Events: []*clientv3.Event{etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 0, State: StateMoving}))}}
	waitFor(t, "state MOVING", func() bool {
		p, _ := w.Current(guildDB)
		return p.State == StateMoving
	})
	if _, version := resolvedVersion(w, guildDB); version != 0 {
		t.Fatalf("状态变化不动版本: %d", version)
	}
	ch <- clientv3.WatchResponse{Events: []*clientv3.Event{etcd.put(guildDB, placementValue(t, Placement{Cluster: "b", Version: 1, State: StateActive}))}}
	waitFor(t, "flip", func() bool {
		addr, version := resolvedVersion(w, guildDB)
		return addr == "10.0.0.2:4000" && version == 1
	})
	// 别的逻辑库不受影响。
	if addr, version := resolvedVersion(w, mailDB); addr != "10.0.0.1:4000" || version != 0 {
		t.Fatalf("别的库跟着变了: %s %d", addr, version)
	}
}

func TestWatcherResyncsAfterWatchBreaks(t *testing.T) {
	etcd := newFakeEtcd()
	w, ch := startFakeWatcher(t, etcd, testConfig())
	// Watch 断开期间翻转:事件丢了,靠重连时的全量读追上。
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "b", Version: 1, State: StateActive}))
	etcd.failGets(errors.New("etcd is down"))
	close(ch)
	if addr, _ := resolvedVersion(w, guildDB); addr != "10.0.0.1:4000" {
		t.Fatalf("etcd 不可用时继续用最后知道的映射: %s", addr)
	}
	etcd.failGets(nil)
	nextWatch(t, etcd)
	if addr, version := resolvedVersion(w, guildDB); addr != "10.0.0.2:4000" || version != 1 {
		t.Fatalf("重连之后没有追上: %s %d", addr, version)
	}
}

func TestWatcherPeriodicReadCatchesMissedEvent(t *testing.T) {
	etcd := newFakeEtcd()
	w := newWatcher(testConfig(), etcd)
	w.resyncEvery = time.Millisecond
	if err := w.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "b", Version: 1, State: StateActive}))
	waitFor(t, "periodic read", func() bool {
		_, version := resolvedVersion(w, guildDB)
		return version == 1
	})
}

func TestWatcherStartFailsWhenEtcdUnreadable(t *testing.T) {
	etcd := newFakeEtcd()
	etcd.failGets(errors.New("etcd is down"))
	if err := newWatcher(testConfig(), etcd).start(context.Background()); err == nil {
		t.Fatal("读不到映射必须拒绝启动:库可能已经搬走了")
	}
}

func TestWatcherRejectsBadAndBackwardMappings(t *testing.T) {
	flipped := Placement{Cluster: "b", Version: 3, State: StateActive}
	tests := []struct {
		name   string
		change func(t *testing.T, etcd *fakeEtcd, w *Watcher) *clientv3.Event
	}{
		{"坏值不覆盖好值", func(t *testing.T, etcd *fakeEtcd, w *Watcher) *clientv3.Event {
			return etcd.put(guildDB, []byte("{not json"))
		}},
		{"版本回退", func(t *testing.T, etcd *fakeEtcd, w *Watcher) *clientv3.Event {
			return etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 2, State: StateActive}))
		}},
		{"同版本换集群", func(t *testing.T, etcd *fakeEtcd, w *Watcher) *clientv3.Event {
			return etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 3, State: StateActive}))
		}},
		{"搬过的库的键被删", func(t *testing.T, etcd *fakeEtcd, w *Watcher) *clientv3.Event {
			return etcd.del(guildDB)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			etcd := newFakeEtcd()
			etcd.put(guildDB, placementValue(t, flipped))
			w := newWatcher(testConfig(), etcd)
			if _, err := w.fullSync(context.Background()); err != nil {
				t.Fatal(err)
			}
			ev := test.change(t, etcd, w)
			switch ev.Type {
			case clientv3.EventTypePut:
				w.applyPut(guildDB, ev.Kv.Value, ev.Kv.ModRevision)
			case clientv3.EventTypeDelete:
				w.applyDelete(guildDB, ev.Kv.ModRevision)
			}
			if addr, version := resolvedVersion(w, guildDB); addr != "10.0.0.2:4000" || version != 3 {
				t.Fatalf("映射被改了: %s %d", addr, version)
			}
		})
	}
}

func TestWatcherSkipsReplayedOlderEvent(t *testing.T) {
	etcd := newFakeEtcd()
	older := etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 0, State: StateMoving}))
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 0, State: StateActive}))
	w := newWatcher(testConfig(), etcd)
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 全量读之后 Watch 补发的旧事件。
	w.applyPut(guildDB, older.Kv.Value, older.Kv.ModRevision)
	if p, _ := w.Current(guildDB); p.State != StateActive {
		t.Fatalf("旧事件盖掉了新状态: %+v", p)
	}
}

func TestWatcherBrokenKeyDoesNotFallBackToDefault(t *testing.T) {
	etcd := newFakeEtcd()
	etcd.put(guildDB, []byte("{not json"))
	w := newWatcher(testConfig(), etcd)
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.ResolvePlacement(guildDB); err == nil {
		t.Fatal("读不懂的映射不能回退到默认集群")
	}
	if addr, _ := resolvedVersion(w, mailDB); addr != "10.0.0.1:4000" {
		t.Fatalf("别的库不受影响: %s", addr)
	}
	ev := etcd.put(guildDB, placementValue(t, Placement{Cluster: "b", Version: 1, State: StateActive}))
	w.applyPut(guildDB, ev.Kv.Value, ev.Kv.ModRevision)
	if addr, _, err := w.ResolvePlacement(guildDB); err != nil || addr != "10.0.0.2:4000" {
		t.Fatalf("修好之后要恢复: %s %v", addr, err)
	}
}

func TestWatcherUnknownClusterFailsResolve(t *testing.T) {
	etcd := newFakeEtcd()
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "c", Version: 1, State: StateActive}))
	w := newWatcher(testConfig(), etcd)
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.ResolvePlacement(guildDB); err == nil {
		t.Fatal("映射指向本进程不认识的集群时必须报错")
	}
}

// 配了映射就上报每个有键的库,不等连过:按需开池的进程还没连过这个库时,audit 不能报它落后。
func TestVersionMetricCoversEveryMappedLogicalDB(t *testing.T) {
	const usedDB, idleDB, keylessDB = "metric_used_db", "metric_idle_db", "metric_keyless_db"
	etcd := newFakeEtcd()
	etcd.put(usedDB, placementValue(t, Placement{Cluster: "main", Version: 0, State: StateActive}))
	etcd.put(idleDB, placementValue(t, Placement{Cluster: "b", Version: 9, State: StateActive}))
	w := newWatcher(testConfig(), etcd)
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, idleDB); got != 9 {
		t.Fatalf("没连过的库也要报: %v", got)
	}
	if hasSeries(t, keylessDB) {
		t.Fatal("没有键又没连过的库无从上报")
	}
	if _, _, err := w.ResolvePlacement(keylessDB); err != nil {
		t.Fatal(err)
	}
	if !hasSeries(t, keylessDB) {
		t.Fatal("连过没有键的库要报版本 0")
	}
	ev := etcd.put(usedDB, placementValue(t, Placement{Cluster: "b", Version: 1, State: StateActive}))
	w.applyPut(usedDB, ev.Kv.Value, ev.Kv.ModRevision)
	if got := gaugeValue(t, usedDB); got != 1 {
		t.Fatalf("db_placement_version{%s} = %v, want 1", usedDB, got)
	}
	// 翻到本进程不认识的集群:指标停在旧版本,audit 才看得出这个进程没跟上。
	ev = etcd.put(usedDB, placementValue(t, Placement{Cluster: "c", Version: 2, State: StateActive}))
	w.applyPut(usedDB, ev.Kv.Value, ev.Kv.ModRevision)
	if got := gaugeValue(t, usedDB); got != 1 {
		t.Fatalf("连不过去的版本不该上报: %v", got)
	}
}

func TestKnownClusterMetric(t *testing.T) {
	w := newWatcher(Config{DefaultCluster: "known_main", SqlClusters: map[string]SqlCluster{"known_main": {Hosts: "h:1"}, "known_b": {Hosts: "h:2"}}, EtcdHosts: []string{"e:1"}}, newFakeEtcd())
	if err := w.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	for _, cluster := range []string{"known_main", "known_b"} {
		metric := &dto.Metric{}
		if err := knownClusterGauge.WithLabelValues(cluster).Write(metric); err != nil {
			t.Fatal(err)
		}
		if metric.GetGauge().GetValue() != 1 {
			t.Fatalf("%s 要上报 1: %v", cluster, metric.GetGauge().GetValue())
		}
	}
}

// etcd 回档且没有 --bump-revision:全量读读到更小的 ModRevision。映射不动(最坏写不进去),但要看得见。
func TestWatcherCountsRevisionRegressOnFullSync(t *testing.T) {
	counter := func() float64 {
		metric := &dto.Metric{}
		if err := rejectedTotal.WithLabelValues(rejectRevisionRegress).Write(metric); err != nil {
			t.Fatal(err)
		}
		return metric.GetCounter().GetValue()
	}
	etcd := newFakeEtcd()
	etcd.rev = 100
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "b", Version: 3, State: StateActive}))
	w := newWatcher(testConfig(), etcd)
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := counter()
	etcd.rev = 5
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 1, State: StateMoving}))
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := counter(); got != before+1 {
		t.Fatalf("revision 回退要计数: %v -> %v", before, got)
	}
	if addr, version := resolvedVersion(w, guildDB); addr != "10.0.0.2:4000" || version != 3 {
		t.Fatalf("映射不能跟着回退: %s %d", addr, version)
	}
	// 用 --bump-revision 恢复之后 revision 更大,回退的值走版本判定,同样不接受。
	etcd.rev = 1000
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "main", Version: 1, State: StateMoving}))
	if _, err := w.fullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if addr, version := resolvedVersion(w, guildDB); addr != "10.0.0.2:4000" || version != 3 {
		t.Fatalf("版本回退不接受: %s %d", addr, version)
	}
}

func TestNilWatcherIsSafe(t *testing.T) {
	var w *Watcher
	if w.OpenOptions() != nil || w.Resolver() != nil || w.Close() != nil {
		t.Fatal("nil Watcher 等于没有映射")
	}
	got, err := Start(context.Background(), Config{})
	if err != nil || got != nil {
		t.Fatalf("没配置时不起 Watcher: %v %v", got, err)
	}
}
