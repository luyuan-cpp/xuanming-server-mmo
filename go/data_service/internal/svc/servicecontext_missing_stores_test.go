package svc

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"data_service/internal/config"
	"data_service/internal/routereconcile"
	"data_service/internal/routing"
	"data_service/internal/store"
	"shared/accountdirectory"
	"shared/serviceauth"

	"github.com/alicebob/miniredis/v2"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// MissingStores 是 Store.Required 拒启判定的唯一依据:四个 store 各自独立上报,顺序固定(日志与告警可比对)。
// 这里只需要"是不是 nil",所以直接用零值结构,不连库。
func TestMissingStoresReportsEachNilStore(t *testing.T) {
	all := []string{"account_directory", "snapshot", "transaction_log", "id_segment", "player_name"}
	if got := (&ServiceContext{}).MissingStores(); !reflect.DeepEqual(got, all) {
		t.Fatalf("a context with no stores must report all four in order, got %v", got)
	}
}

// 下面几个桩只为让 MissingStores 看到"非 nil":嵌入接口本身(零值)即满足接口,方法从不会被调用 ——
// MissingStores 只判空,调到任何方法都是测试写错了,nil 接口会当场 panic 暴露出来。
type stubAccountDirectory struct{ accountdirectory.Directory }
type stubSnapshotStore struct{ SnapshotStore }
type stubTxLogStore struct{ TransactionLogStore }
type stubIdSegmentStore struct{ IdSegmentStore }
type stubPlayerNameStore struct{ PlayerNameStore }

// stubPlayerRouteStore 是 PlayerRouteStore 的最小实现(RegisterHome + 第 3 批的三个回源读 + Close),不嵌入接口:
// 方法都写实,以免哪天 MissingStores 之外的路径误调它时 nil 接口 panic 掩盖真正的问题。
type stubPlayerRouteStore struct{}

func (stubPlayerRouteStore) RegisterHome(context.Context, store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	return store.RouteRegisterResult{}, nil
}
func (stubPlayerRouteStore) GetRoutes(context.Context, []uint64) (map[uint64]store.RouteRow, error) {
	return map[uint64]store.RouteRow{}, nil
}
func (stubPlayerRouteStore) ScanRoutesByHome(context.Context, uint32, store.RouteScanCursor, int) ([]store.RouteRow, error) {
	return nil, nil
}
func (stubPlayerRouteStore) Close() error { return nil }

// 接口一变,编译期先炸:装配处把 *store.RouteStore 直接当 PlayerRouteStore 用(写入点 + 回源读者 + Close)。
var _ PlayerRouteStore = (*store.RouteStore)(nil)

// db_first 下 player_route 是必需的:K8s 各档 Store.Required=true,它缺席时 main 必须拒启 ——
// 这条是"db_first 装配失败即拒启"的唯一判据(规格 d 节 MissingStores)。redis 模式根本不装配它,
// 不能算缺,否则今天所有档位都会拒启。顺序断言与上一个用例同口径:player_route 恒在末尾。
func TestMissingStoresReportsPlayerRouteOnlyUnderDBFirst(t *testing.T) {
	fourReady := func(c config.Config, route PlayerRouteStore) *ServiceContext {
		return &ServiceContext{
			Config:           c,
			SnapshotStore:    stubSnapshotStore{},
			AccountDirectory: stubAccountDirectory{}, AccountDirectoryAuth: &serviceauth.Verifier{},
			TxLogStore:       stubTxLogStore{},
			IdSegmentStore:   stubIdSegmentStore{},
			PlayerNameStore:  stubPlayerNameStore{},
			PlayerRouteStore: route,
		}
	}
	withMode := func(mode string) config.Config {
		var c config.Config
		c.RouteStore.Mode = mode
		return c
	}

	// 第 1b-2 批起 db_first 缺省还要求对账 leader(Reconcile.Enabled 没配 = true):这里的 ServiceContext 都没有 leader,
	// 所以 db_first 的期望末尾多一项 route_reconciler;leader 自己的判定见 TestMissingStoresReportsRouteReconcilerOnlyWhenEnabled。
	cases := []struct {
		name string
		ctx  *ServiceContext
		want []string
	}{
		{"db_first without route store", fourReady(withMode(config.RouteStoreModeDBFirst), nil), []string{"player_route", "route_reconciler"}},
		{"db_first with route store", fourReady(withMode(config.RouteStoreModeDBFirst), stubPlayerRouteStore{}), []string{"route_reconciler"}},
		{"redis without route store", fourReady(withMode(config.RouteStoreModeRedis), nil), nil},
		// 空 Mode = 没写 RouteStore 段(Normalize 之前的形态):等同 redis,同样不能算缺。
		{"empty mode without route store", fourReady(withMode(""), nil), nil},
		// 全缺时 player_route、route_reconciler 依次排在四个老 store 之后,告警比对的顺序不变。
		{"db_first with nothing assembled", &ServiceContext{Config: withMode(config.RouteStoreModeDBFirst)},
			[]string{"account_directory", "snapshot", "transaction_log", "id_segment", "player_name", "player_route", "route_reconciler"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ctx.MissingStores(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MissingStores() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── 对账 leader 的装配(第 1b-2 批,规格 spec_D1_batch1b2_2_3.md §4.2「构造失败只打 ERROR」)──────────────

// stubReconcileStore / stubReconcileCache 只为让 newRouteReconciler 装配成功:嵌入接口本身(零值)即满足接口,
// 构造不连接、不起 goroutine,方法从不会被调用(调到就是测试写错了,nil 接口会当场 panic)。Close 写实:装配失败的分支会关 cache。
type stubReconcileStore struct{ routereconcile.Store }

type stubReconcileCache struct {
	routereconcile.Cache
	closed *int
}

func (c stubReconcileCache) Close() error {
	if c.closed != nil {
		*c.closed++
	}
	return nil
}

// reconcileTestConfig 返回 Normalize 过的配置(NewServiceContext 交给 newRouteReconciler 的就是这个形态)。
func reconcileTestConfig(mode string, enabled *bool, arm bool) config.Config {
	var c config.Config
	c.RouteStore = config.RouteStoreConfig{Mode: mode}
	c.RouteStore.Reconcile.Enabled = enabled
	c.RouteStore.Reconcile.ArmReadyMarker = arm
	c.RouteStore = c.RouteStore.Normalize()
	return c
}

// TestNewRouteReconcilerOnlyWhenEnabled:redis 模式与急停(Enabled=false)不构造、不建 Redis 客户端、不是错误;db_first 而
// route store 没装配起来、mapping Redis 不合用(Type=cluster)、leader 标识生成失败一律报错(调用方据此不装配,MissingStores 上报);
// 装配成功时不连接,leader 标识带本机名(取不到用 unknown),ArmReadyMarker 透传。
func TestNewRouteReconcilerOnlyWhenEnabled(t *testing.T) {
	built, closed := 0, 0
	dangerOwner := func(context.Context, *store.RouteEmergencyError) error {
		t.Error("纯装配测试不得执行危险关闭owner")
		return errors.New("仅装配，不执行危险关闭")
	}
	deps := routeReconcilerDeps{
		onDanger: dangerOwner,
		newCache: func(redis.RedisConf, config.RouteStoreConfig) (routereconcile.Cache, error) {
			built++
			return stubReconcileCache{closed: &closed}, nil
		},
		hostname: func() (string, error) { return "ds-0", nil },
		random:   strings.NewReader(strings.Repeat("r", 64)),
	}
	off := false

	for name, c := range map[string]config.Config{
		"redis mode":             reconcileTestConfig(config.RouteStoreModeRedis, nil, false),
		"db_first Enabled=false": reconcileTestConfig(config.RouteStoreModeDBFirst, &off, false),
	} {
		if r, err := newRouteReconciler(c, stubReconcileStore{}, deps); r != nil || err != nil || built != 0 {
			t.Fatalf("%s: reconciler=%v err=%v built=%d, want nothing built and no error", name, r, err, built)
		}
	}

	dbFirst := reconcileTestConfig(config.RouteStoreModeDBFirst, nil, false)
	if r, err := newRouteReconciler(dbFirst, nil, deps); r != nil || err == nil || built != 0 {
		t.Fatalf("db_first without a route store: reconciler=%v err=%v built=%d, want an error before building the redis client", r, err, built)
	}

	missingOwner := deps
	missingOwner.onDanger = nil
	if r, err := newRouteReconciler(dbFirst, stubReconcileStore{}, missingOwner); r != nil || err == nil || built != 0 {
		t.Fatalf("missing danger owner: reconciler=%v err=%v built=%d, want an error before building the redis client", r, err, built)
	}

	r, err := newRouteReconciler(reconcileTestConfig(config.RouteStoreModeDBFirst, nil, true), stubReconcileStore{}, deps)
	if err != nil || r == nil {
		t.Fatalf("db_first: reconciler=%v err=%v", r, err)
	}
	if !strings.HasPrefix(r.LeaderID(), "route_leader@ds-0:") || !r.ArmEnabled() || built != 1 || closed != 0 {
		t.Fatalf("leader_id=%q arm=%t built=%d closed=%d", r.LeaderID(), r.ArmEnabled(), built, closed)
	}

	deps.hostname = func() (string, error) { return "", errors.New("no hostname") }
	if r, err := newRouteReconciler(dbFirst, stubReconcileStore{}, deps); err != nil || r == nil || !strings.HasPrefix(r.LeaderID(), "route_leader@unknown:") {
		t.Fatalf("a missing hostname must not refuse: reconciler=%v err=%v", r, err)
	}

	cluster := dbFirst
	cluster.MappingRedis = redis.RedisConf{Host: closedLoopbackAddr(t), Type: redis.ClusterType}
	if r, err := newRouteReconciler(cluster, stubReconcileStore{}, productionRouteReconcilerDeps(dangerOwner)); r != nil || err == nil {
		t.Fatalf("a cluster mapping redis must be refused: reconciler=%v err=%v", r, err)
	}
	node := dbFirst
	node.MappingRedis = redis.RedisConf{Host: closedLoopbackAddr(t), Type: redis.NodeType}
	if r, err := newRouteReconciler(node, stubReconcileStore{}, productionRouteReconcilerDeps(dangerOwner)); r == nil || err != nil {
		t.Fatalf("the production deps must assemble without connecting: reconciler=%v err=%v", r, err)
	}

	deps.random = strings.NewReader("") // leader 标识的随机后缀读不出
	if r, err := newRouteReconciler(dbFirst, stubReconcileStore{}, deps); r != nil || err == nil || closed != 1 {
		t.Fatalf("a failing random source: reconciler=%v err=%v closed=%d, want an error and the built client closed", r, err, closed)
	}
}

// TestMissingStoresReportsRouteReconcilerOnlyWhenEnabled:route_reconciler 只在 ReconcileEnabled(db_first 且 Enabled 不是 false)
// 时必需;redis 模式、急停都不算缺(回归:redis 模式下 MissingStores 仍不含 player_route)。顺序固定在 player_route 之后。
func TestMissingStoresReportsRouteReconcilerOnlyWhenEnabled(t *testing.T) {
	off := false
	deps := routeReconcilerDeps{
		onDanger: func(context.Context, *store.RouteEmergencyError) error {
			t.Error("MissingStores纯装配测试不得执行危险关闭owner")
			return errors.New("仅装配，不执行危险关闭")
		},
		newCache: func(redis.RedisConf, config.RouteStoreConfig) (routereconcile.Cache, error) {
			return stubReconcileCache{}, nil
		},
		hostname: func() (string, error) { return "ds-1", nil },
		random:   strings.NewReader(strings.Repeat("s", 16)),
	}
	rec, err := newRouteReconciler(reconcileTestConfig(config.RouteStoreModeDBFirst, nil, false), stubReconcileStore{}, deps)
	if err != nil || rec == nil {
		t.Fatalf("setup: %v", err)
	}
	ready := func(c config.Config, route PlayerRouteStore, r *routereconcile.Reconciler) *ServiceContext {
		return &ServiceContext{
			Config:           c,
			SnapshotStore:    stubSnapshotStore{},
			AccountDirectory: stubAccountDirectory{}, AccountDirectoryAuth: &serviceauth.Verifier{},
			TxLogStore:       stubTxLogStore{},
			IdSegmentStore:   stubIdSegmentStore{},
			PlayerNameStore:  stubPlayerNameStore{},
			PlayerRouteStore: route,
			RouteReconciler:  r,
		}
	}
	dbFirst := reconcileTestConfig(config.RouteStoreModeDBFirst, nil, false)
	cases := []struct {
		name string
		ctx  *ServiceContext
		want []string
	}{
		{"db_first with route store and reconciler", ready(dbFirst, stubPlayerRouteStore{}, rec), nil},
		{"db_first with route store, reconciler missing", ready(dbFirst, stubPlayerRouteStore{}, nil), []string{"route_reconciler"}},
		{"db_first with both missing", ready(dbFirst, nil, nil), []string{"player_route", "route_reconciler"}},
		{"db_first Enabled=false", ready(reconcileTestConfig(config.RouteStoreModeDBFirst, &off, false), stubPlayerRouteStore{}, nil), nil},
		{"redis mode", ready(reconcileTestConfig(config.RouteStoreModeRedis, nil, false), nil, nil), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ctx.MissingStores(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MissingStores() = %v, want %v", got, tc.want)
			}
		})
	}
}

// db_first 装配前的 WAITAOF 能力探针(routing.ProbeWaitAOF):mapping Redis 确认不了落盘就不装配 route store,
// 让配置错误挡在启动期(Store.Required=true 时经 MissingStores 拒启),而不是留到每一次 W1 都失败、
// 再经 go-zero 熔断器拖累同一 Redis 上的登录读。这里只用一个必然连不上的地址,不需要真 Redis:
//   - 默认 WaitAOFNumLocal=1 → 探针失败 → newPlayerRouteStore 返回 nil(在开库之前就返回,不碰 MySQL);
//   - 两项都显式写 0 → 不发 WAITAOF,探针直接放行(连地址都不碰)。
func TestNewPlayerRouteStoreRefusedWhenWaitAOFProbeFails(t *testing.T) {
	var c config.Config
	c.RouteStore = config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}.Normalize()
	c.MappingRedis = redis.RedisConf{Host: closedLoopbackAddr(t), Type: "node"}

	if err := routing.ProbeWaitAOF(context.Background(), c.MappingRedis, c.RouteStore); err == nil {
		t.Fatal("WaitAOFNumLocal=1 against an unreachable mapping redis must fail the probe")
	}
	if got := newPlayerRouteStore(c, store.MySQLConfig{}, true); got != nil {
		t.Fatalf("route store must not be assembled when the WAITAOF probe fails, got %T", got)
	}

	zero := 0
	c.RouteStore.WaitAOFNumLocal, c.RouteStore.WaitAOFNumReplicas = &zero, &zero
	if err := routing.ProbeWaitAOF(context.Background(), c.MappingRedis, c.RouteStore); err != nil {
		t.Fatalf("WaitAOF 0/0 sends no WAITAOF and must not touch the mapping redis, got %v", err)
	}
}

// closedLoopbackAddr 返回一个刚关掉的本机端口:连它必然被拒绝。
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// ── 第 3 批读方门禁的装配(规格 §5.2、§13.2 第 2 条)──────────────────────────────

// countingRouteReader 记 ScanRoutesByHome 被调了几次:用来证明 routeRouterOptions 给的选项真的把回源读者装进了 Router。
type countingRouteReader struct {
	stubPlayerRouteStore
	scans int
}

func (c *countingRouteReader) ScanRoutesByHome(context.Context, uint32, store.RouteScanCursor, int) ([]store.RouteRow, error) {
	c.scans++
	return nil, nil
}

// TestRouteRouterOptionsInjectsStoreAsWriterAndReader:装配好的 route store 同时作为写入点与回源读者注入 Router;
// 没装配(nil)时一个选项都不给(不包出「非 nil 接口包着 nil 指针」)。注入效果用 db_first 下的 R7 观察:
// GetAllPlayerIDsInZone 一律读库,读者在就走它,读者不在就 fail-closed。
func TestRouteRouterOptionsInjectsStoreAsWriterAndReader(t *testing.T) {
	if got := routeRouterOptions(nil); got != nil {
		t.Fatalf("a nil route store must yield no router options, got %d", len(got))
	}

	mr := miniredis.RunT(t)
	var c config.Config
	c.MappingRedis = redis.RedisConf{Host: mr.Addr(), Type: "node"}
	zero := 0
	c.RouteStore = config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst, WaitAOFNumLocal: &zero, WaitAOFNumReplicas: &zero}

	reader := &countingRouteReader{}
	opts := routeRouterOptions(reader)
	if len(opts) != 2 {
		t.Fatalf("an assembled route store must be injected as both writer and reader, got %d options", len(opts))
	}
	r := routing.NewRouter(c, opts...)
	t.Cleanup(r.Close)
	if _, err := r.GetAllPlayerIDsInZone(context.Background(), 7); err != nil {
		t.Fatalf("R7 with an injected reader: %v", err)
	}
	if reader.scans != 1 {
		t.Fatalf("R7 must read through the injected route reader, scans=%d", reader.scans)
	}

	bare := routing.NewRouter(c, routeRouterOptions(nil)...)
	t.Cleanup(bare.Close)
	if _, err := bare.GetAllPlayerIDsInZone(context.Background(), 7); !errors.Is(err, routing.ErrRouteReadUnavailable) {
		t.Fatalf("db_first without a route reader must fail closed with ErrRouteReadUnavailable, got %v", err)
	}
}

// TestProbeRouteReadyReadAtStartup:开关关着时不碰 mapping Redis(连不上的地址也放行);开关打开时读脚本跑不起来
// (miniredis 不支持 INFO server,即「INFO 在脚本里不可用」;或连不上)一律回 false,newPlayerRouteStore 随之不装配
// route store —— 在开库之前就返回(MySQLConfig 是零值,碰库会是另一种失败)。
func TestProbeRouteReadyReadAtStartup(t *testing.T) {
	var c config.Config
	c.MappingRedis = redis.RedisConf{Host: closedLoopbackAddr(t), Type: "node"}
	zero := 0
	c.RouteStore = config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst, WaitAOFNumLocal: &zero, WaitAOFNumReplicas: &zero}.Normalize()
	if !probeRouteReadyReadAtStartup(c) {
		t.Fatal("ReadRequiresReady=false must skip the probe entirely")
	}

	c.RouteStore.ReadRequiresReady = true
	if probeRouteReadyReadAtStartup(c) {
		t.Fatal("an unreachable mapping redis must fail the route ready read probe")
	}

	mr := miniredis.RunT(t)
	c.MappingRedis = redis.RedisConf{Host: mr.Addr(), Type: "node"}
	if probeRouteReadyReadAtStartup(c) {
		t.Fatal("a mapping redis that rejects INFO inside scripts must fail the probe")
	}
	if got := newPlayerRouteStore(c, store.MySQLConfig{}, true); got != nil {
		t.Fatalf("route store must not be assembled when the route ready read probe fails, got %T", got)
	}
}
