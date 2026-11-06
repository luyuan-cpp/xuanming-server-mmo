package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"login/internal/config"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// waitBound 只是「循环卡死」时的失败上界,不是时序假设:所有等待都等的是明确的信号。
const waitBound = 5 * time.Second

const testPrefix = "GateNodeService.rpc/zone/1/node_type/4/"

// fakeKV 记录每次 Get 的 ctx,按 resps 顺序应答(用完重复最后一个);err 非 nil 时一律失败。
type fakeKV struct {
	clientv3.KV
	mu    sync.Mutex
	resps []*clientv3.GetResponse
	err   error
	ctxs  []context.Context
	calls chan struct{} // 每次 Get 发一个信号
}

func newFakeKV(resps ...*clientv3.GetResponse) *fakeKV {
	return &fakeKV{resps: resps, calls: make(chan struct{}, 64)}
}

func (f *fakeKV) Get(ctx context.Context, _ string, _ ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	var resp *clientv3.GetResponse
	if len(f.resps) > 0 {
		resp = f.resps[0]
		if len(f.resps) > 1 {
			f.resps = f.resps[1:]
		}
	}
	err := f.err
	f.mu.Unlock()
	f.calls <- struct{}{}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (f *fakeKV) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ctxs)
}

func getResp(rev int64, kvs ...nodeEvent) *clientv3.GetResponse {
	resp := &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: rev}}
	for _, kv := range kvs {
		resp.Kvs = append(resp.Kvs, &mvccpb.KeyValue{Key: []byte(kv.key), Value: kv.value, ModRevision: kv.modRev})
	}
	return resp
}

// fakeWatcher 是一轮 watch 用的 Watcher。block=true 时 Watch 一直等到 ctx 结束(模拟建流卡住)。
type fakeWatcher struct {
	clientv3.Watcher
	ch      chan clientv3.WatchResponse
	block   bool
	rev     atomic.Int64
	closed  atomic.Bool
	watched chan struct{} // Watch 被调用时关闭
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{ch: make(chan clientv3.WatchResponse), watched: make(chan struct{})}
}

func (w *fakeWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	w.rev.Store(clientv3.OpGet(key, opts...).Rev())
	close(w.watched)
	if w.block {
		<-ctx.Done()
		closed := make(chan clientv3.WatchResponse)
		close(closed)
		return closed
	}
	return w.ch
}

func (w *fakeWatcher) Close() error {
	w.closed.Store(true)
	return nil
}

// fakeTimers 由测试手动驱动:tick 推静默检查,retry 推重试,armed 收到每次建流登记的时限触发器。
type fakeTimers struct {
	tick  chan time.Time
	retry chan time.Time
	armed chan func()
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{tick: make(chan time.Time), retry: make(chan time.Time), armed: make(chan func(), 64)}
}

func (f *fakeTimers) timers() watchTimers {
	return watchTimers{
		after:     func(time.Duration) <-chan time.Time { return f.retry },
		newTicker: func(time.Duration) (<-chan time.Time, func()) { return f.tick, func() {} },
		afterFunc: func(_ time.Duration, fn func()) func() bool {
			var done atomic.Bool
			f.armed <- func() {
				if done.CompareAndSwap(false, true) {
					fn()
				}
			}
			return func() bool { return done.CompareAndSwap(false, true) }
		},
	}
}

type watchHarness struct {
	nw       *NodeWatcher
	kv       *fakeKV
	clk      *fakeClock
	timers   *fakeTimers
	watchers chan *fakeWatcher // newWatcher 依次取用
	cancel   context.CancelFunc
	done     chan struct{}
}

func setDiscoveryTimeout(t *testing.T) {
	t.Helper()
	prev := config.AppConfig.Timeouts.ServiceDiscoveryTimeout
	config.AppConfig.Timeouts.ServiceDiscoveryTimeout = 5 * time.Second
	t.Cleanup(func() { config.AppConfig.Timeouts.ServiceDiscoveryTimeout = prev })
}

func newWatchHarness(t *testing.T, kv *fakeKV) *watchHarness {
	t.Helper()
	setDiscoveryTimeout(t)
	h := &watchHarness{
		kv:       kv,
		clk:      newFakeClock(),
		timers:   newFakeTimers(),
		watchers: make(chan *fakeWatcher, 16),
		done:     make(chan struct{}),
	}
	h.nw = newNodeWatcher(kv, func() clientv3.Watcher {
		select {
		case w := <-h.watchers:
			return w
		case <-time.After(waitBound):
			panic("test did not provide a watcher for this round")
		}
	}, testPrefix, h.clk.now, h.timers.timers())
	return h
}

func (h *watchHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		h.nw.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(waitBound):
			t.Errorf("Run did not return after ctx cancel")
		}
	})
}

func (h *watchHarness) addWatcher() *fakeWatcher {
	w := newFakeWatcher()
	h.watchers <- w
	return w
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitBound):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitWatchEstablished 等这一轮 Watch 被调用,并消费掉它登记的建流时限(不触发),让循环进入事件处理。
func (h *watchHarness) waitWatchEstablished(t *testing.T, w *fakeWatcher) func() {
	t.Helper()
	waitSignal(t, w.watched, "Watch call")
	select {
	case fire := <-h.timers.armed:
		return fire
	case <-time.After(waitBound):
		t.Fatal("watch did not arm a create timeout")
		return nil
	}
}

// send 把一条响应交给 watch 循环。第二次发送一条空响应作屏障:无缓冲通道上它能被收下,
// 说明循环已经处理完上一条、回到了 select。
func send(t *testing.T, w *fakeWatcher, resp clientv3.WatchResponse) {
	t.Helper()
	for _, r := range []clientv3.WatchResponse{resp, {}} {
		select {
		case w.ch <- r:
		case <-time.After(waitBound):
			t.Fatal("watch loop is not receiving")
		}
	}
}

func putEvent(key string, nodeID, playerCount uint32) *clientv3.Event {
	return &clientv3.Event{Type: clientv3.EventTypePut, Kv: &mvccpb.KeyValue{Key: []byte(key), Value: gateJSON(nodeID, playerCount)}}
}

// 全量同步后从 Header.Revision+1 开始 watch(不漏 Get 与 Watch 之间的变更);事件进镜像;Nodes 读镜像不再打 etcd。
func TestNodeWatcher_SyncThenWatchFromNextRevision(t *testing.T) {
	kv := newFakeKV(getResp(7, put(testPrefix+"node_id/1", 1, 10)))
	h := newWatchHarness(t, kv)
	w := h.addWatcher()
	h.start(t)

	waitSignal(t, kv.calls, "full sync")
	h.waitWatchEstablished(t, w)
	if got := w.rev.Load(); got != 8 {
		t.Fatalf("watch start revision = %d, want 8 (Get revision 7 + 1)", got)
	}

	send(t, w, clientv3.WatchResponse{Events: []*clientv3.Event{putEvent(testPrefix+"node_id/2", 2, 20)}})
	nodes, err := h.nw.Nodes(context.Background())
	if err != nil || fmt.Sprint(nodeIDs(nodes)) != "[1 2]" {
		t.Fatalf("Nodes = (%v, %v), want [1 2] from the mirror", nodeIDs(nodes), err)
	}
	if n := kv.getCount(); n != 1 {
		t.Fatalf("etcd Get calls = %d, want 1 (reads served from the mirror)", n)
	}
}

// watch 流结束的三种方式都回到全量同步,并且关掉这一轮的 Watcher:通道关闭、带错误的响应、静默超时。
func TestNodeWatcher_ResyncsWhenStreamEnds(t *testing.T) {
	cases := map[string]func(t *testing.T, h *watchHarness, w *fakeWatcher){
		"channel closed": func(t *testing.T, _ *watchHarness, w *fakeWatcher) { close(w.ch) },
		"compacted error": func(t *testing.T, _ *watchHarness, w *fakeWatcher) {
			select {
			case w.ch <- clientv3.WatchResponse{CompactRevision: 5}:
			case <-time.After(waitBound):
				t.Fatal("watch loop is not receiving")
			}
		},
		"silent too long": func(t *testing.T, h *watchHarness, _ *fakeWatcher) {
			h.clk.advance(cacheResyncAfter + time.Second)
			select {
			case h.timers.tick <- time.Time{}:
			case <-time.After(waitBound):
				t.Fatal("watch loop is not ticking")
			}
		},
	}
	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			kv := newFakeKV(getResp(7, put(testPrefix+"node_id/1", 1, 10)))
			h := newWatchHarness(t, kv)
			first := h.addWatcher()
			second := h.addWatcher()
			h.start(t)
			waitSignal(t, kv.calls, "first full sync")
			h.waitWatchEstablished(t, first)

			end(t, h, first)

			waitSignal(t, kv.calls, "resync")
			h.waitWatchEstablished(t, second)
			if !first.closed.Load() {
				t.Fatal("the first round's Watcher must be closed so its stream is torn down")
			}
		})
	}
}

// 回归(评审 A2):Watch 建流卡住时(要等服务端确认才返回),时限触发后取消这一轮、关掉 Watcher、重新全量同步。
func TestNodeWatcher_ResyncsWhenWatchCreationHangs(t *testing.T) {
	kv := newFakeKV(getResp(7))
	h := newWatchHarness(t, kv)
	stuck := h.addWatcher()
	stuck.block = true
	next := h.addWatcher()
	h.start(t)

	waitSignal(t, kv.calls, "first full sync")
	fire := h.waitWatchEstablished(t, stuck)
	fire() // 建流时限到期

	waitSignal(t, kv.calls, "resync after create timeout")
	h.waitWatchEstablished(t, next)
	if !stuck.closed.Load() {
		t.Fatal("the stuck round's Watcher must be closed")
	}
}

// 全量同步失败后按重试节拍再试,不忙转;成功之后照常 watch。
func TestNodeWatcher_RetriesFullSyncAfterFailure(t *testing.T) {
	kv := newFakeKV(getResp(3))
	kv.err = errors.New("etcd down")
	h := newWatchHarness(t, kv)
	w := h.addWatcher()
	h.start(t)

	waitSignal(t, kv.calls, "failed full sync")
	kv.mu.Lock()
	kv.err = nil
	kv.mu.Unlock()
	select {
	case h.timers.retry <- time.Time{}:
	case <-time.After(waitBound):
		t.Fatal("Run is not waiting on the retry timer")
	}
	waitSignal(t, kv.calls, "retried full sync")
	h.waitWatchEstablished(t, w)
	if got := w.rev.Load(); got != 4 {
		t.Fatalf("watch start revision = %d, want 4", got)
	}
}

type ctxMarker struct{}

// 镜像不新鲜时 Nodes 回落直读,并把调用方 ctx 往下传(请求取消即放弃,不在 etcd 抖动时堆积)。
func TestNodeWatcher_NodesFallsBackToEtcdWithCallerCtx(t *testing.T) {
	cases := map[string]func(h *watchHarness){
		"unsynced": func(*watchHarness) {},
		"stale": func(h *watchHarness) {
			h.nw.cache.replaceAll([]nodeEvent{put(testPrefix+"node_id/9", 9, 0)})
			h.clk.advance(cacheStaleAfter + time.Second)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			kv := newFakeKV(getResp(5, put(testPrefix+"node_id/1", 1, 10)))
			h := newWatchHarness(t, kv)
			prepare(h)

			ctx := context.WithValue(context.Background(), ctxMarker{}, "caller")
			nodes, err := h.nw.Nodes(ctx)
			if err != nil || fmt.Sprint(nodeIDs(nodes)) != "[1]" {
				t.Fatalf("Nodes = (%v, %v), want [1] read from etcd", nodeIDs(nodes), err)
			}
			// 直读没有历史可比:判不了停写,不能把直读到的记录标成 Stale。
			if got := staleIDs(nodes); len(got) != 0 {
				t.Fatalf("stale nodes on the direct-read fallback = %v, want none", got)
			}
			kv.mu.Lock()
			defer kv.mu.Unlock()
			if len(kv.ctxs) != 1 || kv.ctxs[0].Value(ctxMarker{}) != "caller" {
				t.Fatalf("etcd Get calls = %d; the fallback must use a ctx derived from the caller's", len(kv.ctxs))
			}
			if _, ok := kv.ctxs[0].Deadline(); !ok {
				t.Fatal("the fallback read must be bounded by ServiceDiscoveryTimeout")
			}
		})
	}
}

func putEventRev(key string, nodeID, playerCount uint32, modRev int64) *clientv3.Event {
	ev := putEvent(key, nodeID, playerCount)
	ev.Kv.ModRevision = modRev
	return ev
}

// 停写判定要靠全量同步与 watch 事件都把 ModRevision 带进镜像。gate 2 在流上写过一次(ModRevision 10)之后卡住,
// 之后每次重同步拿到的都是 10:只有 watch 路径记下了 10、全量同步路径也带着 10,它的停写时长才不会被重同步清零,
// 跨过 nodeDataStaleAfter 后 Nodes 标它 Stale。gate 1 每次重同步都是新的 ModRevision,不受影响。
func TestNodeWatcher_ModRevisionReachesTheMirror(t *testing.T) {
	g1, g2 := testPrefix+"node_id/1", testPrefix+"node_id/2"
	kv := newFakeKV(
		getResp(10, putRev(g1, 1, 10, 9), putRev(g2, 2, 20, 8)),
		getResp(21, putRev(g1, 1, 10, 21), putRev(g2, 2, 20, 10)),
		getResp(31, putRev(g1, 1, 10, 31), putRev(g2, 2, 20, 10)),
		getResp(41, putRev(g1, 1, 10, 41), putRev(g2, 2, 20, 10)),
	)
	h := newWatchHarness(t, kv)
	watchers := []*fakeWatcher{h.addWatcher(), h.addWatcher(), h.addWatcher(), h.addWatcher()}
	h.start(t)

	for round, w := range watchers {
		waitSignal(t, kv.calls, fmt.Sprintf("full sync %d", round+1))
		h.waitWatchEstablished(t, w)
		if round == len(watchers)-1 {
			break
		}
		if round == 0 {
			// gate 2 最后一次写记录:之后它就卡住了。
			send(t, w, clientv3.WatchResponse{Events: []*clientv3.Event{putEventRev(g2, 2, 20, 10), putEventRev(g1, 1, 10, 11)}})
		}
		// 静默检查触发下一次重同步(每轮 13s,三轮之后 gate 2 已 39s 没写)。
		h.clk.advance(cacheResyncAfter + time.Second)
		select {
		case h.timers.tick <- time.Time{}:
		case <-time.After(waitBound):
			t.Fatal("watch loop is not ticking")
		}
	}

	nodes, err := h.nw.Nodes(context.Background())
	if err != nil || fmt.Sprint(nodeIDs(nodes)) != "[1 2]" {
		t.Fatalf("Nodes = (%v, %v), want [1 2] from the mirror", nodeIDs(nodes), err)
	}
	if got := fmt.Sprint(staleIDs(nodes)); got != "[2]" {
		t.Fatalf("stale nodes = %s, want [2]: gate 2 has rewritten nothing since ModRevision 10", got)
	}
	if n := kv.getCount(); n != 4 {
		t.Fatalf("etcd Get calls = %d, want 4 (three silence-triggered resyncs, the read served from the mirror)", n)
	}
}
