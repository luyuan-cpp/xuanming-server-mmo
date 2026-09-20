package scenenode

import (
	"context"
	"errors"
	"testing"

	smpb "proto/scene_manager"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"
)

// fakeReader 是 LocationReader 的测试替身:不起 Redis,直接给原始字节 / 错误。
type fakeReader struct {
	raw     []byte
	err     error
	gotKey  string
	callCnt int
}

func (f *fakeReader) GetLocation(_ context.Context, key string) ([]byte, error) {
	f.gotKey = key
	f.callCnt++
	return f.raw, f.err
}

// fakeSceneClient 嵌入接口:本包只负责把客户端交出去,不调它的任何方法,
// 嵌入的 nil 接口被调用会 panic —— 正好钉死"Resolve 不发 RPC"这条契约。
type fakeSceneClient struct {
	smpb.SceneNodeGrpcClient
}

func mustMarshalLoc(t *testing.T, loc *smpb.PlayerLocation) []byte {
	t.Helper()
	raw, err := proto.Marshal(loc)
	if err != nil {
		t.Fatalf("marshal PlayerLocation: %v", err)
	}
	return raw
}

// syncedWatcher 返回一个"首次全量同步已完成"的 watcher,并按需灌入节点。
// 直接写 synced 是同包测试的特权:生产路径只有 fullSync 成功才会置位。
func syncedWatcher(entries map[string]NodeEntry) *Watcher {
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	for key, entry := range entries {
		w.Upsert(key, entry)
	}
	w.synced.Store(true)
	return w
}

func newTestMetrics() *Metrics {
	return NewMetrics(prometheus.NewRegistry(), "guild")
}

func resolveCount(m *Metrics, result string) float64 {
	return testutil.ToFloat64(m.resolve.WithLabelValues("guild", result))
}

// 键不存在 = 不在线;顺带钉住跨运行时契约 key 的拼法。
func TestResolveNotOnline(t *testing.T) {
	reader := &fakeReader{}
	m := newTestMetrics()
	l := &Locator{Reader: reader, Watcher: syncedWatcher(nil), Metrics: m,
		Dial: func(string) (smpb.SceneNodeGrpcClient, error) { return fakeSceneClient{}, nil }}

	_, err := l.Resolve(context.Background(), 42)
	if !errors.Is(err, ErrNotOnline) {
		t.Fatalf("err = %v, want ErrNotOnline", err)
	}
	if !IsNoHolder(err) {
		t.Fatal("不在线属于「没人持有」一族")
	}
	if reader.gotKey != "player:42:location" {
		t.Fatalf("读的键 = %q, want player:42:location", reader.gotKey)
	}
	if reader.callCnt != 1 {
		t.Fatalf("一次 Resolve 应当只读一次 Redis,实得 %d", reader.callCnt)
	}
	if got := resolveCount(m, ResolveNotOnline); got != 1 {
		t.Fatalf("not_online 计数 = %v, want 1", got)
	}
}

// 位置键写坏了是数据面事故,绝不能折成"玩家不在线"静静吞掉。
func TestResolveMalformedBytesIsNotNotOnline(t *testing.T) {
	m := newTestMetrics()
	l := &Locator{Reader: &fakeReader{raw: []byte("not a proto")}, Watcher: syncedWatcher(nil), Metrics: m}

	_, err := l.Resolve(context.Background(), 42)
	if err == nil {
		t.Fatal("坏字节必须报错")
	}
	if errors.Is(err, ErrNotOnline) || IsNoHolder(err) {
		t.Fatalf("坏字节不得归为「没人持有」: %v", err)
	}
	if got := resolveCount(m, ResolveError); got != 1 {
		t.Fatalf("error 计数 = %v, want 1", got)
	}
}

// Redis 故障同理:是故障,不是"不在线"。
func TestResolveReaderErrorIsNotNotOnline(t *testing.T) {
	sentinel := errors.New("redis down")
	l := &Locator{Reader: &fakeReader{err: sentinel}, Watcher: syncedWatcher(nil), Metrics: newTestMetrics()}

	_, err := l.Resolve(context.Background(), 42)
	if !errors.Is(err, sentinel) {
		t.Fatalf("底层错误必须可追溯: %v", err)
	}
	if IsNoHolder(err) {
		t.Fatalf("Redis 故障不得归为「没人持有」: %v", err)
	}
}

func TestResolveNodeNotRegistered(t *testing.T) {
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{SceneId: 5, NodeId: "9", ZoneId: 1, OwnerEpoch: 3})
	m := newTestMetrics()
	l := &Locator{Reader: &fakeReader{raw: raw}, Watcher: syncedWatcher(nil), Metrics: m}

	_, err := l.Resolve(context.Background(), 42)
	if !errors.Is(err, ErrNodeUnknown) {
		t.Fatalf("err = %v, want ErrNodeUnknown", err)
	}
	if got := resolveCount(m, ResolveNodeUnknown); got != 1 {
		t.Fatalf("node_unknown 计数 = %v, want 1", got)
	}
}

// 09-18 起的新语义:node_id 为空且 owner_epoch != 0 = 跨 zone 交接已放行、
// 目标 zone 还没落点。没人持有,调用方应当重试,不是"节点未注册"的故障。
func TestResolveAwaitingPlacement(t *testing.T) {
	// 只用 node_id + owner_epoch:这两个字段今天就在生成树里。判定不看
	// pending_scene_conf_id(它是目标 zone 选频道用的,与"谁持有"无关)。
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{NodeId: "", ZoneId: 2, OwnerEpoch: 7})
	m := newTestMetrics()
	l := &Locator{Reader: &fakeReader{raw: raw}, Watcher: syncedWatcher(nil), Metrics: m}

	_, err := l.Resolve(context.Background(), 42)
	if !errors.Is(err, ErrAwaitingPlacement) {
		t.Fatalf("err = %v, want ErrAwaitingPlacement", err)
	}
	if !IsNoHolder(err) {
		t.Fatal("等待落点属于「没人持有」一族,调用方应得到重试")
	}
	if errors.Is(err, ErrNotOnline) || errors.Is(err, ErrNodeUnknown) {
		t.Fatalf("等待落点不得与不在线 / 节点未注册混为一谈: %v", err)
	}
	if got := resolveCount(m, ResolveAwaitingPlacement); got != 1 {
		t.Fatalf("awaiting_placement 计数 = %v, want 1", got)
	}
	if got := resolveCount(m, ResolveNodeUnknown); got != 0 {
		t.Fatalf("不该同时记 node_unknown,实得 %v", got)
	}
}

// 空 node_id 且没有 epoch:旧记录或写了一半,同样没有持有者 —— fail-closed 归到
// node_unknown,仍然是重试语义,但指标上与"传送在途"分开。
func TestResolveEmptyNodeWithoutEpoch(t *testing.T) {
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{NodeId: "", ZoneId: 2})
	m := newTestMetrics()
	l := &Locator{Reader: &fakeReader{raw: raw}, Watcher: syncedWatcher(nil), Metrics: m}

	_, err := l.Resolve(context.Background(), 42)
	if !errors.Is(err, ErrNodeUnknown) {
		t.Fatalf("err = %v, want ErrNodeUnknown", err)
	}
	if errors.Is(err, ErrAwaitingPlacement) {
		t.Fatalf("没有 epoch 的空 node_id 不是「等待落点」: %v", err)
	}
}

// 镜像还没建起来时查什么都"未注册",必须 fail-closed 并说清真实原因。
func TestResolveWatcherNotSynced(t *testing.T) {
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{NodeId: "7", ZoneId: 1, OwnerEpoch: 3})
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	w.Upsert(rpcKey(1, 7), NodeEntry{NodeId: 7, ZoneId: 1, Endpoint: "10.0.0.1:9201"})
	m := newTestMetrics()
	l := &Locator{Reader: &fakeReader{raw: raw}, Watcher: w, Metrics: m,
		Dial: func(string) (smpb.SceneNodeGrpcClient, error) { return fakeSceneClient{}, nil }}

	if _, err := l.Resolve(context.Background(), 42); !errors.Is(err, ErrNodeUnknown) {
		t.Fatalf("未同步的镜像必须拒绝定位: %v", err)
	}
	// 启动窗口里镜像没建起来是正常现象,不能计进"节点掉线"那一档,
	// 否则每次重启都会把 node_unknown 抬高一次。
	if got := resolveCount(m, ResolveMirrorUnsynced); got != 1 {
		t.Fatalf("mirror_unsynced = %v, want 1", got)
	}
	if got := resolveCount(m, ResolveNodeUnknown); got != 0 {
		t.Fatalf("不该记 node_unknown,实得 %v", got)
	}
}

// 脑裂(同身份多条注册)与节点掉线在指标上必须分开:前者查租约与部署链,
// 后者查节点进程。两者对调用方都仍是 ErrNodeUnknown 族的"稍后重试"。
func TestResolveAmbiguousNodeCountsSeparately(t *testing.T) {
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{NodeId: "7", ZoneId: 1, OwnerEpoch: 3})
	// 同一 (zone=1, node=7) 身份挂在两个 etcd key 上、指向不同 endpoint:
	// 典型成因是旧租约没过期新节点已注册,或两条部署链同时在跑。
	staleKey := rpcKey(1, 7) + "-stale"
	w := NewWatcher("scene", SceneNodeRpcPrefix, nil, nil)
	w.Upsert(rpcKey(1, 7), NodeEntry{NodeId: 7, ZoneId: 1, NodeUuid: "uuid-a", Endpoint: "10.0.0.1:9201"})
	w.Upsert(staleKey, NodeEntry{NodeId: 7, ZoneId: 1, NodeUuid: "uuid-b", Endpoint: "10.0.0.2:9201"})
	w.synced.Store(true)

	m := newTestMetrics()
	l := &Locator{Reader: &fakeReader{raw: raw}, Watcher: w, Metrics: m,
		Dial: func(string) (smpb.SceneNodeGrpcClient, error) {
			t.Fatal("歧义时绝不能拨号")
			return nil, nil
		}}

	_, err := l.Resolve(context.Background(), 42)
	if !errors.Is(err, ErrNodeUnknown) || !IsNoHolder(err) {
		t.Fatalf("歧义必须仍属 ErrNodeUnknown / IsNoHolder 族: %v", err)
	}
	if got := resolveCount(m, ResolveNodeAmbiguous); got != 1 {
		t.Fatalf("node_ambiguous = %v, want 1", got)
	}
	if got := resolveCount(m, ResolveNodeUnknown); got != 0 {
		t.Fatalf("不该同时记 node_unknown,实得 %v", got)
	}
}

func TestResolveFoundPassesEndpointToDial(t *testing.T) {
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{SceneId: 5, NodeId: "7", ZoneId: 1, OwnerEpoch: 3})
	w := syncedWatcher(map[string]NodeEntry{
		rpcKey(1, 7): {NodeId: 7, ZoneId: 1, NodeUuid: "uuid-a", Endpoint: "10.0.0.1:9201"},
		rpcKey(2, 7): {NodeId: 7, ZoneId: 2, NodeUuid: "uuid-b", Endpoint: "10.0.0.2:9201"},
	})
	m := newTestMetrics()
	var dialed []string
	l := &Locator{Reader: &fakeReader{raw: raw}, Watcher: w, Metrics: m,
		Dial: func(endpoint string) (smpb.SceneNodeGrpcClient, error) {
			dialed = append(dialed, endpoint)
			return fakeSceneClient{}, nil
		}}

	target, err := l.Resolve(context.Background(), 42)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "10.0.0.1:9201" {
		t.Fatalf("拨号 endpoint = %v, want [10.0.0.1:9201](zone 1 那条)", dialed)
	}
	if target.Endpoint != "10.0.0.1:9201" || target.Client == nil {
		t.Fatalf("Target = %+v", target)
	}
	if target.Location.GetSceneId() != 5 || target.Location.GetZoneId() != 1 {
		t.Fatalf("Target.Location = %+v", target.Location)
	}
	if got := resolveCount(m, ResolveFound); got != 1 {
		t.Fatalf("found 计数 = %v, want 1", got)
	}
}

// 装配缺件必须当场报错,不能退化成"拨到某个默认节点"。
func TestResolveUnwiredFailsClosed(t *testing.T) {
	raw := mustMarshalLoc(t, &smpb.PlayerLocation{NodeId: "7", ZoneId: 1, OwnerEpoch: 3})
	w := syncedWatcher(map[string]NodeEntry{
		rpcKey(1, 7): {NodeId: 7, ZoneId: 1, Endpoint: "10.0.0.1:9201"},
	})

	noReader := &Locator{Watcher: w, Metrics: newTestMetrics()}
	if _, err := noReader.Resolve(context.Background(), 42); err == nil {
		t.Fatal("没有 Reader 必须报错")
	}

	noDialer := &Locator{Reader: &fakeReader{raw: raw}, Watcher: w, Metrics: newTestMetrics()}
	if _, err := noDialer.Resolve(context.Background(), 42); err == nil {
		t.Fatal("既没有 Dial 也没有 Conns 必须报错")
	}
}
