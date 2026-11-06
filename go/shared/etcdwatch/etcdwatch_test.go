package etcdwatch

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/metadata"
)

// fakeWatcher 只记下 Watch 收到的 ctx 与参数,不连 etcd。
type fakeWatcher struct {
	ctx context.Context
	key string
}

func (f *fakeWatcher) Watch(ctx context.Context, key string, _ ...clientv3.OpOption) clientv3.WatchChan {
	f.ctx, f.key = ctx, key
	return make(chan clientv3.WatchResponse)
}
func (f *fakeWatcher) RequestProgress(context.Context) error { return nil }
func (f *fakeWatcher) Close() error                          { return nil }

func TestOpenRequiresLeader(t *testing.T) {
	w := &fakeWatcher{}
	_, _, stop := Open(context.Background(), w, time.Hour, "prefix/")
	defer stop()
	if w.key != "prefix/" {
		t.Fatalf("要按原 key watch: %q", w.key)
	}
	want, _ := metadata.FromOutgoingContext(clientv3.WithRequireLeader(context.Background()))
	got, ok := metadata.FromOutgoingContext(w.ctx)
	if !ok || len(want) == 0 {
		t.Fatalf("watch 的 ctx 必须带 RequireLeader 元数据: %v", got)
	}
	for k, v := range want {
		if g := got.Get(k); len(g) != len(v) || len(g) == 0 || g[0] != v[0] {
			t.Fatalf("RequireLeader 元数据 %s = %v,要 %v", k, g, v)
		}
	}
}

// stop 必须取消 watch 的子 ctx(否则每轮都泄漏一条 watch 子流),但不能取消调用方的 ctx。
func TestStopCancelsOnlyTheWatchContext(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	w := &fakeWatcher{}
	_, _, stop := Open(parent, w, time.Hour, "k")
	if w.ctx.Err() != nil {
		t.Fatal("stop 之前 watch 的 ctx 不能被取消")
	}
	stop()
	if w.ctx.Err() == nil {
		t.Fatal("stop 之后 watch 的 ctx 必须被取消")
	}
	if parent.Err() != nil {
		t.Fatal("stop 不能取消调用方的 ctx")
	}
}

func TestResyncFiresAfterInterval(t *testing.T) {
	_, resync, stop := Open(context.Background(), &fakeWatcher{}, 10*time.Millisecond, "k")
	defer stop()
	select {
	case <-resync:
	case <-time.After(5 * time.Second):
		t.Fatal("到了间隔 resync 必须可读")
	}
}

func TestWithKeepAliveKeepsExplicitValues(t *testing.T) {
	got := WithKeepAlive(clientv3.Config{})
	if got.DialKeepAliveTime != KeepAliveTime || got.DialKeepAliveTimeout != KeepAliveTimeout {
		t.Fatalf("没配时补默认 keepalive: %v / %v", got.DialKeepAliveTime, got.DialKeepAliveTimeout)
	}
	explicit := WithKeepAlive(clientv3.Config{DialKeepAliveTime: 30 * time.Second, DialKeepAliveTimeout: 7 * time.Second})
	if explicit.DialKeepAliveTime != 30*time.Second || explicit.DialKeepAliveTimeout != 7*time.Second {
		t.Fatalf("显式配置不能被覆盖: %v / %v", explicit.DialKeepAliveTime, explicit.DialKeepAliveTimeout)
	}
}
