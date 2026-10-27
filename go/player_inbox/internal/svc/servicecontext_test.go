package svc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"shared/drainproof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"player_inbox/generated/pb/game"
	"player_inbox/internal/config"
	"player_inbox/internal/inbox"
	base "proto/common/base"
	dspb "proto/data_service"

	"shared/leader"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// noConnDriver 只为构造 *sql.DB:装配与关停只用到池对象本身,真去连就报错。
type noConnDriver struct{}

func (noConnDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("svc test driver: no real connections")
}

const fakeDriverName = "player_inbox_svc_test"

func init() { sql.Register(fakeDriverName, noConnDriver{}) }

func openFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(fakeDriverName, "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// dbClosed:关闭之后 sql.DB 回固定的 "sql: database is closed";没关时这个假驱动回的是另一条错误。
func dbClosed(db *sql.DB) bool {
	err := db.PingContext(context.Background())
	return err != nil && strings.Contains(err.Error(), "database is closed")
}

func redisClosed(c *redis.Client) bool {
	return errors.Is(c.Ping(context.Background()).Err(), redis.ErrClosed)
}

func testConfig() config.Config {
	var c config.Config
	c.AccountRoleSchema = "fixture_directory"
	c.Etcd.Hosts = []string{"127.0.0.1:2379"}
	c.MySQL = configMySQL()
	c.CoreRedis = config.RedisConf{Host: "127.0.0.1:6379"}
	c.Kafka = config.KafkaConf{Brokers: []string{"127.0.0.1:9092"}, WriteTimeoutMs: 2000}
	c.PlayerInbox = config.PlayerInboxConf{
		EnqueueTypes:                []string{"PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN"},
		TerminalRetentionDays:       30,
		SweepIntervalSeconds:        600,
		NotifyResendIntervalSeconds: 30,
		GaugeIntervalSeconds:        60,
		ReconcileIntervalMinutes:    60,
		ReconcileWindowHours:        24,
	}
	return c
}

// TestCoreRedisOptionsBoundEveryCall 钉住核心 Redis 客户端的超时契约(评审 L7):
//   - 开 ContextTimeoutEnabled:请求预算(Timeout − 500ms)、OwnerEpochReadTimeout、提醒预算才真正管得住 socket 读写,
//     否则 go-redis 用 context.Background() 读写,只受 3s 缺省读超时约束,in-band 回故障的契约会被击穿;
//   - 读 / 写超时显式为正:不带截止时间的调用(选主竞选的 SETNX 用的是循环 ctx)靠它封顶;-1 / -2 会关掉 socket 截止,
//     连 ctx 截止也一起失效(go/shared/leader GoRedisMaxCallDuration 的注释);
//   - PoolSize 原样取配置(Redis 连接总账读同一个键),不落回 go-redis 缺省。
//
// 并核对选主心跳在这套选项下仍能保证「key 过期之前自我降级」:收件箱用 leader 的缺省 TTL(30s),
// 节拍 = TTL/3、续期 ctx = min(2s, TTL/8)(go/shared/leader/leader.go startHeartbeat)。
func TestCoreRedisOptionsBoundEveryCall(t *testing.T) {
	opts := coreRedisOptions(config.RedisConf{Host: "127.0.0.1:1", DB: 0, PoolSize: 20})
	if !opts.ContextTimeoutEnabled {
		t.Fatal("核心 Redis 必须开 ContextTimeoutEnabled")
	}
	if opts.ReadTimeout <= 0 || opts.WriteTimeout <= 0 || opts.ReadTimeout != coreRedisReadTimeout || opts.WriteTimeout != coreRedisWriteTimeout {
		t.Fatalf("ReadTimeout = %v WriteTimeout = %v, want 显式正值 %v / %v", opts.ReadTimeout, opts.WriteTimeout,
			coreRedisReadTimeout, coreRedisWriteTimeout)
	}
	if opts.PoolSize != 20 || opts.Addr != "127.0.0.1:1" {
		t.Fatalf("PoolSize = %d Addr = %q, want 取自配置", opts.PoolSize, opts.Addr)
	}

	client := redis.NewClient(opts) // 不发命令就不连
	defer client.Close()
	effective := client.Options()
	if effective.ReadTimeout != coreRedisReadTimeout || effective.WriteTimeout != coreRedisWriteTimeout || effective.PoolSize != 20 {
		t.Fatalf("初始化后的选项被改写: read=%v write=%v pool=%d", effective.ReadTimeout, effective.WriteTimeout, effective.PoolSize)
	}
	if got := leader.GoRedisMaxCallDuration(effective); got != 0 {
		t.Fatalf("GoRedisMaxCallDuration = %v, want 0(客户端认 ctx 截止时间)", got)
	}
	const ttl = 30 * time.Second
	maxCall := leader.CallBudget(min(2*time.Second, ttl/8), leader.GoRedisMaxCallDuration(effective))
	if fence, ok := leader.FenceAfter(ttl, ttl/3, maxCall); !ok || fence != 2*(ttl/3) || 2*(ttl/3)+maxCall >= ttl {
		t.Fatalf("FenceAfter(ttl=%v, maxCall=%v) = (%v, %v):选主心跳必须能在 key 过期前自我降级", ttl, maxCall, fence, ok)
	}
}

// recordingDeps 是不连任何真实外部依赖的 externalDeps:记录每个入口被调了几次,产物留给断言。
type recordingDeps struct {
	t        *testing.T
	calls    map[string]int
	etcd     *clientv3.Client
	db       *sql.DB
	rds      *redis.Client
	conn     *grpc.ClientConn
	failAt   string // 这个入口返回错误
	registry *prometheus.Registry
}

func newRecordingDeps(t *testing.T) *recordingDeps {
	return &recordingDeps{t: t, calls: map[string]int{}, registry: prometheus.NewRegistry()}
}

var errInjected = errors.New("injected dial failure")

func (r *recordingDeps) deps() externalDeps {
	return externalDeps{
		openEtcd: func([]string) (*clientv3.Client, error) {
			r.calls["etcd"]++
			if r.failAt == "etcd" {
				return nil, errInjected
			}
			r.etcd = clientv3.NewCtxClient(context.Background())
			return r.etcd, nil
		},
		openMySQL: func(config.MySQLConf) (*sql.DB, error) {
			r.calls["mysql"]++
			if r.failAt == "mysql" {
				return nil, errInjected
			}
			r.db = openFakeDB(r.t)
			return r.db, nil
		},
		// 只验证装配/回收；不冒充任何真实SQL原角色围栏证据。
		newRoleAuthority: func(context.Context, *sql.DB, string) (inbox.RoleAuthority, error) {
			if r.failAt == "role" {
				return nil, errInjected
			}
			return assemblyRoles{}, nil
		},
		openCoreRedis: func(config.RedisConf) (*redis.Client, error) {
			r.calls["redis"]++
			if r.failAt == "redis" {
				return nil, errInjected
			}
			// 指向不监听的地址:装配不发命令,关停后 Ping 回 ErrClosed 用来判断「已关」。
			r.rds = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
			return r.rds, nil
		},
		openDataService: func(zrpc.RpcClientConf) (*grpc.ClientConn, error) {
			r.calls["dataservice"]++
			if r.failAt == "dataservice" {
				return nil, errInjected
			}
			conn, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				r.t.Fatal(err)
			}
			r.conn = conn
			return conn, nil
		},
		registerer: r.registry,
	}
}

type assemblyRoles struct{}

func (assemblyRoles) LockCurrent(context.Context, *sql.Tx, uint64) (*dspb.AccountRoleOwnership, error) {
	return nil, errors.New("装配替身禁止业务调用")
}
func (assemblyRoles) VerifyAdmission(context.Context, *sql.Tx, *dspb.AccountRoleOwnership, *base.RoleAdmissionIdentity) error {
	return errors.New("装配替身禁止业务调用")
}

// TestNewServiceContextRejectsBadAllowlistBeforeDialing:白名单写错时一条外部连接都不建(先自检、后拨号)。
func TestNewServiceContextRejectsBadAllowlistBeforeDialing(t *testing.T) {
	logtest.Discard(t)
	rec := newRecordingDeps(t)
	c := testConfig()
	c.PlayerInbox.EnqueueTypes = []string{"PLAYER_INBOX_MSG_NO_SUCH_TYPE"}
	sc, err := newServiceContext(c, rec.deps())
	if err == nil || sc != nil {
		t.Fatalf("白名单含未知类型应拒绝: sc=%v err=%v", sc, err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("自检失败前不应拨任何依赖,实际调用: %v", rec.calls)
	}
}

func TestNewServiceContextRejectsMissingOriginalRoleContract(t *testing.T) {
	logtest.Discard(t)
	rec := newRecordingDeps(t)
	rec.failAt = "role"
	if sc, err := newServiceContext(testConfig(), rec.deps()); sc != nil || !errors.Is(err, errInjected) {
		t.Fatalf("原目录契约缺失须拒启: %v %v", sc, err)
	}
	if !dbClosed(rec.db) || rec.etcd.Ctx().Err() == nil || rec.calls["redis"] != 0 {
		t.Fatal("拒启须释放已开资源且不继续装配")
	}
}

// TestNewServiceContextClosesOpenedResourcesOnFailure:任何一步失败都返回错误,且已建的资源在返回前关闭
// (启动致命路径不泄漏 etcd / MySQL / Redis 连接),失败点之后的入口不再调用。
func TestNewServiceContextClosesOpenedResourcesOnFailure(t *testing.T) {
	logtest.Discard(t)

	rec := newRecordingDeps(t)
	rec.failAt = "redis"
	if sc, err := newServiceContext(testConfig(), rec.deps()); err == nil || sc != nil || !errors.Is(err, errInjected) {
		t.Fatalf("Redis 失败应返回包着原因的错误: sc=%v err=%v", sc, err)
	}
	if rec.etcd.Ctx().Err() == nil {
		t.Error("etcd 客户端没有关闭")
	}
	if !dbClosed(rec.db) {
		t.Error("MySQL 连接池没有关闭")
	}
	if rec.calls["dataservice"] != 0 {
		t.Errorf("Redis 失败之后不应再建 data_service 客户端: %v", rec.calls)
	}

	rec = newRecordingDeps(t)
	rec.failAt = "dataservice"
	if sc, err := newServiceContext(testConfig(), rec.deps()); err == nil || sc != nil || !errors.Is(err, errInjected) {
		t.Fatalf("data_service 失败应返回错误(不再 MustNewClient 直接退出进程): sc=%v err=%v", sc, err)
	}
	if !redisClosed(rec.rds) || !dbClosed(rec.db) || rec.etcd.Ctx().Err() == nil {
		t.Error("data_service 失败时 Redis / MySQL / etcd 都应已关闭")
	}
}

// TestNewServiceContextWiresDependencies:成功路径把各依赖接好 —— 在线提醒器拿到生成的事件号
// (player_inbox/generated/pb/game,随仓库跟踪,干净检出即可编译)、写同一个 Kafka writer、查同一份节点镜像;
// 后台任务是节点镜像与选主循环两项;Stop 关闭全部连接。
func TestNewServiceContextWiresDependencies(t *testing.T) {
	logtest.Discard(t)
	rec := newRecordingDeps(t)
	sc, err := newServiceContext(testConfig(), rec.deps())
	if err != nil {
		t.Fatalf("newServiceContext: %v", err)
	}
	for _, name := range []string{"etcd", "mysql", "redis", "dataservice"} {
		if rec.calls[name] != 1 {
			t.Errorf("%s 入口应恰好调用一次,got %d", name, rec.calls[name])
		}
	}
	if sc.Etcd != rec.etcd || sc.DB != rec.db || sc.CoreRedis != rec.rds || sc.dataServiceConn != rec.conn {
		t.Fatal("ServiceContext 持有的连接不是入口建出来的那些")
	}
	if sc.Registry == nil || sc.Store == nil || sc.Ledgers == nil || sc.Loops == nil || sc.DataService == nil ||
		sc.SceneNodes == nil || sc.Kafka == nil {
		t.Fatalf("有依赖没装配: %+v", sc)
	}
	if sc.Store.Registry() != sc.Registry {
		t.Error("Store 与 ServiceContext 应共用同一份类型注册表")
	}

	n := sc.notifier
	if n == nil || n.EventID == 0 || n.EventID != uint32(game.PlayerInboxNotifyEventEventId) {
		t.Fatalf("在线提醒的事件号 = %v, want 生成的 PlayerInboxNotifyEventEventId(%d)", n, game.PlayerInboxNotifyEventEventId)
	}
	writer, ok := n.Writer.(*relayMessageWriter)
	if !ok || writer.writer != sc.Kafka || writer.admission != sc.RelayAdmission || n.Nodes != sc.SceneNodes || n.Locations == nil {
		t.Error("在线提醒器必须写同一个 Kafka writer、查同一份 scene 节点镜像")
	}

	if len(sc.background) != 2 || sc.background[0].name != "player_inbox.scenenode.watcher" || sc.background[1].name != "player_inbox.maintenance" {
		t.Errorf("后台任务 = %+v, want 节点镜像 + 选主循环", sc.background)
	}
	if sc.stopTimeout != loopsStopTimeout {
		t.Errorf("stopTimeout = %v, want %v", sc.stopTimeout, loopsStopTimeout)
	}
	sc.Stop() // 没 Start 也要关连接
	if !redisClosed(rec.rds) || !dbClosed(rec.db) || rec.etcd.Ctx().Err() == nil {
		t.Error("Stop 应关闭 Redis / MySQL / etcd")
	}
	if rec.conn.GetState() != connectivity.Shutdown {
		t.Errorf("Stop 应关闭 data_service 连接,state = %v", rec.conn.GetState())
	}
	sc.Stop() // 幂等
}

// fakeTask 是可观察的后台任务:记录启动次数;退出时记下 DB 是否已经被关(用来核对「先停任务、再关连接」)。
type fakeTask struct {
	started        atomic.Int32
	exited         atomic.Bool
	dbClosedAtExit atomic.Bool
	ignoreCancel   chan struct{} // 非 nil:无视 ctx,直到它被关闭才返回
	db             *sql.DB
}

func (f *fakeTask) run(ctx context.Context) {
	f.started.Add(1)
	if f.ignoreCancel != nil {
		<-f.ignoreCancel
	} else {
		<-ctx.Done()
	}
	if f.db != nil {
		f.dbClosedAtExit.Store(dbClosed(f.db))
	}
	f.exited.Store(true)
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartStopOrder 钉住关停顺序(设计 §10):Start 只生效一次;Stop 先取消后台任务并等它们退出,再关连接;Stop 幂等。
func TestStartStopOrder(t *testing.T) {
	logtest.Discard(t)
	db := openFakeDB(t)
	a, b := &fakeTask{db: db}, &fakeTask{db: db}
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New(), RelayIngress: drainproof.New(), DB: db, stopTimeout: 5 * time.Second,
		background: []backgroundTask{{name: "test.a", run: a.run}, {name: "test.b", run: b.run}}}

	sc.Start(context.Background())
	sc.Start(context.Background()) // 第二次不再起任务
	waitFor(t, func() bool { return a.started.Load() == 1 && b.started.Load() == 1 }, "两项后台任务都启动")

	sc.Stop()
	if !a.exited.Load() || !b.exited.Load() {
		t.Fatal("Stop 返回时后台任务必须已经退出(有界等待在关连接之前)")
	}
	if a.dbClosedAtExit.Load() || b.dbClosedAtExit.Load() {
		t.Fatal("后台任务退出时连接池已被关闭:关停顺序反了(任务的最后一轮会撞上已关的连接)")
	}
	if !dbClosed(db) {
		t.Fatal("Stop 之后连接池应已关闭")
	}
	sc.Stop() // 幂等,不 panic
	if a.started.Load() != 1 || b.started.Load() != 1 {
		t.Fatalf("任务启动次数 a=%d b=%d, want 各 1", a.started.Load(), b.started.Load())
	}
}

// TestStartAfterStopIsNoop:已经 Stop 的 ServiceContext 再 Start 不起任何任务(关停竞态里不复活后台循环)。
func TestStartAfterStopIsNoop(t *testing.T) {
	logtest.Discard(t)
	task := &fakeTask{}
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New(), RelayIngress: drainproof.New(), stopTimeout: time.Second, background: []backgroundTask{{name: "test.task", run: task.run}}}
	sc.Stop()
	sc.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	if task.started.Load() != 0 {
		t.Fatal("Stop 之后 Start 不应启动任务")
	}
}

// TestStopIsBoundedWhenTaskHangs:有界返回 HOLD 并保留资源，不能用强关连接伪造任务已完成。
func TestStopIsBoundedWhenTaskHangs(t *testing.T) {
	logtest.Discard(t)
	db := openFakeDB(t)
	t.Cleanup(func() { _ = db.Close() })
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	task := &fakeTask{ignoreCancel: release}
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New(), RelayIngress: drainproof.New(), DB: db, stopTimeout: 50 * time.Millisecond, background: []backgroundTask{{name: "test.hang", run: task.run}}}

	sc.Start(context.Background())
	waitFor(t, func() bool { return task.started.Load() == 1 }, "任务启动")
	begin := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), sc.stopTimeout)
	defer cancel()
	if err := sc.StopAndReport(stopCtx); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("未 join 的任务必须保持 HOLD: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("Stop 用了 %v,应在 stopTimeout(50ms)附近返回", elapsed)
	}
	if dbClosed(db) {
		t.Fatal("未知在途任务仍可能访问 SQL，不能提前关连接")
	}
	releaseOnce.Do(func() { close(release) })
	waitFor(t, task.exited.Load, "放行后任务退出")
}

// TestStartFollowsParentContext:父 ctx 结束时后台任务随之退出,之后 Stop 立即返回。
func TestStartFollowsParentContext(t *testing.T) {
	logtest.Discard(t)
	task := &fakeTask{}
	sc := &ServiceContext{RootAdmission: drainproof.New(), RelayAdmission: drainproof.New(), RelayIngress: drainproof.New(), stopTimeout: 5 * time.Second, background: []backgroundTask{{name: "test.task", run: task.run}}}
	ctx, cancel := context.WithCancel(context.Background())
	sc.Start(ctx)
	waitFor(t, func() bool { return task.started.Load() == 1 }, "任务启动")
	cancel()
	waitFor(t, task.exited.Load, "父 ctx 结束后任务退出")
	sc.Stop()
}
