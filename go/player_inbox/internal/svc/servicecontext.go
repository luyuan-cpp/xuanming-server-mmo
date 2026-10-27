// Package svc 是 player_inbox 的依赖装配:etcd 客户端、mmorpg_player_inbox 连接池、核心 Redis、
// Kafka 生产者、scene 节点镜像、data_service 客户端,以及 inbox 的 Store / 在线提醒 / 后台循环。
package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"player_inbox/generated/pb/game"
	"player_inbox/internal/config"
	"player_inbox/internal/constants"
	"player_inbox/internal/inbox"

	dspb "proto/data_service"

	"shared/authoritysql"
	"shared/drainproof"
	"shared/kafkacmd"
	"shared/leader"
	"shared/safego"
	"shared/scenenode"

	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

const (
	etcdDialTimeout      = 5 * time.Second
	mysqlPingTimeout     = 5 * time.Second
	coreRedisPingTimeout = 3 * time.Second
	// coreRedisReadTimeout / coreRedisWriteTimeout:核心 Redis 单次 socket 读 / 写的兜底上限(评审 L7)。
	// 客户端开了 ContextTimeoutEnabled,带截止时间的 ctx(请求预算 Timeout − 500ms、OwnerEpochReadTimeout 500ms、
	// 提醒预算、选主续期 2s)取两者较早者;不带截止时间的调用(选主竞选的 SETNX 用的是循环 ctx)靠它封顶。
	// 只用到 GET / SET NX / EVAL 这类单键命令,正常往返是毫秒级,1s 足够宽。必须为正:-1 / -2 会关掉 socket 截止,
	// 连 ctx 截止也一起失效(shared/leader GoRedisMaxCallDuration 的注释)。
	coreRedisReadTimeout  = time.Second
	coreRedisWriteTimeout = time.Second
	// loopsStopTimeout 只限定旧 Stop 适配器的等待预算；到期必须 HOLD，不能关闭仍有历史工作的资源。
	loopsStopTimeout = 2 * time.Second

	// MaintenanceLeaderKey 是后台循环选主锁(核心 Redis)。
	MaintenanceLeaderKey = "player_inbox:maintenance:leader"
	// sceneWatcherName 进 scenenode_nodes{kind} 这一个 label。
	sceneWatcherName = "scene"
)

// ServiceContext 持有全部依赖。Start 最多一次；维护关闭不以取消或超时冒称成功。
type ServiceContext struct {
	Config config.Config

	// 同一 boot 的 SQL 身份与两个独立角色；P1 不关闭 RELAY 或 SQL 池。
	WriterConfig      authoritysql.WriterConfig
	RootAdmission     *drainproof.Session
	RelayAdmission    *drainproof.Session
	RelayIngress      *drainproof.Session
	DrainRuntime      *drainproof.Runtime
	lifecycleMu       sync.Mutex
	resourceCloseOnce sync.Once
	resourceCloseErr  error

	// Etcd 供 noderegistry 注册、scene 节点镜像与 killswitch 共用(Etcd.Key 为空,go-zero 不写自己的 key)。
	Etcd *clientv3.Client
	// DB 是 mmorpg_player_inbox 的连接池(设计 §10 的独立连接池)。表由 schemamigrate 建,本包不碰 DDL。
	DB *sql.DB
	// CoreRedis 是核心 Redis(owner_epoch / location / 选主锁)。
	CoreRedis *redis.Client
	// Kafka 写在线提醒(scene-cmd_g<N>,显式分区)。
	Kafka *kafka.Writer
	// SceneNodes 是 SceneNodeService.rpc/ 前缀的节点镜像。
	SceneNodes *scenenode.Watcher
	// DataService 玩家存在校验与对账读账本。
	DataService dspb.DataServiceClient

	Registry *inbox.Registry
	Store    *inbox.Store
	// Ledgers 是对账读已落盘账本的适配器(运维 ReconcilePlayer 与定时对账共用)。
	Ledgers inbox.LedgerReader
	Loops   *inbox.Loops

	// dataServiceConn 是 DataService 底下的 gRPC 连接，须晚于真实 RELAY 前沿闭合才能关闭。
	dataServiceConn *grpc.ClientConn
	// notifier 是装进 Store 的在线提醒器(装配单测核对它拿到的生成事件号)。
	notifier *inbox.KafkaNotifier
	// background 是 Start 起的后台任务(scene 节点镜像、选主后台循环);装配时填好,单测可以换成假任务。
	background []backgroundTask
	// stopTimeout 是 Stop 等根准入已接收调用返回的预算(生产为 loopsStopTimeout)。
	stopTimeout time.Duration

	startOnce sync.Once
	mu        sync.Mutex
	stopped   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// backgroundTask 是 Start 起的一项后台任务。run 必须在 ctx 结束后返回；返回不代替持久队列终局。
type backgroundTask struct {
	name string // safego 的标识(panic 日志里看得到)
	run  func(ctx context.Context)
}

// externalDeps 是 NewServiceContext 建外部连接的入口(AGENTS §11.2 显式依赖)。生产为 productionDeps();
// 单测注入假实现,只验证装配顺序、失败时的回收与接线,不连真实的 etcd / MySQL / 核心 Redis / data_service。
type externalDeps struct {
	openEtcd         func(hosts []string) (*clientv3.Client, error)
	openMySQL        func(config.MySQLConf) (*sql.DB, error)
	newRoleAuthority func(context.Context, *sql.DB, string) (inbox.RoleAuthority, error)
	// openCoreRedis 建核心 Redis 客户端并探活;返回错误时自己负责关掉已建的客户端。
	openCoreRedis   func(config.RedisConf) (*redis.Client, error)
	openDataService func(zrpc.RpcClientConf) (*grpc.ClientConn, error)
	// registerer 是 scene 节点镜像指标(scenenode_nodes)的注册表。
	registerer prometheus.Registerer
}

// productionDeps 是生产用的外部连接入口。
func productionDeps() externalDeps {
	return externalDeps{
		openEtcd: func(hosts []string) (*clientv3.Client, error) {
			return clientv3.New(clientv3.Config{Endpoints: hosts, DialTimeout: etcdDialTimeout})
		},
		openMySQL: OpenMySQL,
		newRoleAuthority: func(ctx context.Context, db *sql.DB, schema string) (inbox.RoleAuthority, error) {
			return inbox.NewSQLRoleAuthority(ctx, db, schema)
		},
		openCoreRedis: openCoreRedis,
		openDataService: func(c zrpc.RpcClientConf) (*grpc.ClientConn, error) {
			// 不用 MustNewClient:它在配置错误时直接退出进程,跳过本函数「返回错误、关闭已建资源」的契约。
			cli, err := zrpc.NewClient(c)
			if err != nil {
				return nil, err
			}
			return cli.Conn(), nil
		},
		registerer: prometheus.DefaultRegisterer,
	}
}

// coreRedisOptions 是核心 Redis 客户端的选项(评审 L7):
//   - ContextTimeoutEnabled:go-redis 缺省用 context.Background() 做 socket 读写,只受读写超时(缺省 3s + 3s)约束,
//     500ms 的 owner_epoch 预算、整请求预算都管不住 socket 读,依赖变慢时 go-zero 超时拦截器会抢先回 DeadlineExceeded,
//     in-band 回 kPlayerInboxStoreUnavailable 的契约被击穿。开了之后 socket 截止时间取 ctx 截止与读写超时的较早者;
//   - 读 / 写超时显式写出(见 coreRedisReadTimeout),不赌 go-redis 缺省;
//   - PoolSize 取配置(config.Validate 要求正整数):k8s_deploy.ps1 的 Redis 连接总账镜像同一个键。
//
// 选主心跳(shared/leader,经 GoRedisMaxCallDuration 读这套选项)的单次续期最坏耗时因此从 2s + 读写超时之和(8s)
// 降到续期 ctx 本身的 2s,FenceAfter 门槛仍是 20s、降级最晚在 22s 完成(TTL 30s),不必调选主参数
// (servicecontext_test 的 TestCoreRedisOptionsBoundEveryCall 钉住)。
func coreRedisOptions(c config.RedisConf) *redis.Options {
	return &redis.Options{
		Addr:                  c.Host,
		Password:              c.Password,
		DB:                    c.DB,
		PoolSize:              c.PoolSize,
		ContextTimeoutEnabled: true,
		ReadTimeout:           coreRedisReadTimeout,
		WriteTimeout:          coreRedisWriteTimeout,
	}
}

// openCoreRedis建提醒/选主/STALE诊断客户端并探活；它不裁决消费身份。错误不含密码。
func openCoreRedis(c config.RedisConf) (*redis.Client, error) {
	client := redis.NewClient(coreRedisOptions(c))
	pingCtx, cancel := context.WithTimeout(context.Background(), coreRedisPingTimeout)
	err := client.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping core Redis %s (DB %d) failed (owner_epoch / location live there and "+
			"must be the same instance as C++ scene / scene_manager): %w", c.Host, c.DB, err)
	}
	// 生效值:核对 ConfigMap 镜像的 PoolSize 是否真的被加载,以及超时契约是否生效。
	opts := client.Options()
	logx.Infof("Core Redis client %s DB=%d pool_size=%d read_timeout=%v write_timeout=%v context_timeout_enabled=%v",
		c.Host, c.DB, opts.PoolSize, opts.ReadTimeout, opts.WriteTimeout, opts.ContextTimeoutEnabled)
	return client, nil
}

// NewServiceContext 建全部依赖。任何一项建不出来都返回错误(启动致命);已建的资源在返回前关闭。
func NewServiceContext(c config.Config) (*ServiceContext, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("player_inbox SQL boot 装配失败: %w", err)
	}
	return NewServiceContextWithAdmission(c, writer, drainproof.New(), drainproof.New())
}

// 新 boot 的 prepared 门禁在构造时传入，Kafka 发布不会持有被替换的旧门禁。
func NewServiceContextWithAdmission(c config.Config, writer authoritysql.WriterConfig, root, relay *drainproof.Session) (*ServiceContext, error) {
	if len(writer.InstanceID()) != 16 || len(writer.WriterKey()) != 16 || root == nil || relay == nil || root == relay {
		return nil, errors.New("player_inbox SQL boot 与 SOURCE/RELAY 装配缺失或角色复用")
	}
	deps := productionDeps()
	deps.openMySQL = func(c config.MySQLConf) (*sql.DB, error) {
		return openMySQL(c, authoritysql.Writer(writer))
	}
	return newServiceContextWithAdmission(c, deps, writer, root, relay)
}

// 测试入口保留原外部依赖接缝；生产必须走上面的显式 WriterConfig 装配。
func newServiceContext(c config.Config, deps externalDeps) (*ServiceContext, error) {
	return newServiceContextWithAdmission(c, deps, authoritysql.WriterConfig{}, drainproof.New(), drainproof.New())
}

func newServiceContextWithAdmission(c config.Config, deps externalDeps, writer authoritysql.WriterConfig, root, relay *drainproof.Session) (*ServiceContext, error) {
	sc := &ServiceContext{RelayIngress: drainproof.New(), Config: c, WriterConfig: writer, RootAdmission: root, RelayAdmission: relay, stopTimeout: loopsStopTimeout}
	ok := false
	defer func() {
		if !ok {
			sc.closeResources()
		}
	}()

	// 先做不连任何东西的自检:注册表与白名单不对时,一条外部连接都不该建。
	registry, err := inbox.NewRegistry()
	if err != nil {
		return nil, fmt.Errorf("player_inbox: type registry self-check failed: %w", err)
	}
	sc.Registry = registry
	allowed, err := config.ParseEnqueueTypes(c.PlayerInbox.EnqueueTypes)
	if err != nil {
		return nil, err
	}

	sc.Etcd, err = deps.openEtcd(c.Etcd.Hosts)
	if err != nil {
		return nil, fmt.Errorf("player_inbox: create etcd client: %w", err)
	}

	sc.DB, err = deps.openMySQL(c.MySQL)
	if err != nil {
		return nil, fmt.Errorf("player_inbox: %w", err)
	}
	roleCtx, roleCancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	roles, err := deps.newRoleAuthority(roleCtx, sc.DB, c.AccountRoleSchema)
	roleCancel()
	if err != nil {
		return nil, fmt.Errorf("player_inbox: SQL角色原件不可用: %w", err)
	}

	sc.CoreRedis, err = deps.openCoreRedis(c.CoreRedis)
	if err != nil {
		return nil, fmt.Errorf("player_inbox: %w", err)
	}

	sc.Kafka = &kafka.Writer{
		Addr: kafka.TCP(c.Kafka.Brokers...),
		// scene-cmd_gN 必须按 node_id % P 落到**指定分区**:消费端 assign 的就是那一个分区,落错分区 = 目标 scene
		// 永远收不到且 Kafka 一个错都不报;kafka-go 的 Writer 只问 Balancer,所以必须用 CommandPartitionBalancer
		// (照 go/match;docs/design/control-plane-topic-partitioning-20260908.md)。
		Balancer:               &kafkacmd.CommandPartitionBalancer{Fallback: &kafka.Hash{}},
		AllowAutoTopicCreation: false,
		BatchTimeout:           10 * time.Millisecond,
		MaxAttempts:            1,
		WriteTimeout:           time.Duration(c.Kafka.WriteTimeoutMs) * time.Millisecond,
		RequiredAcks:           kafka.RequireOne,
		Async:                  false,
	}

	nodeMetrics := scenenode.NewMetrics(deps.registerer, "player_inbox")
	sc.SceneNodes = scenenode.NewWatcher(sceneWatcherName, scenenode.SceneNodeRpcPrefix, nodeMetrics.SetNodes, nil)

	sc.dataServiceConn, err = deps.openDataService(c.DataServiceRpc)
	if err != nil {
		return nil, fmt.Errorf("player_inbox: create data_service client: %w", err)
	}
	sc.DataService = dspb.NewDataServiceClient(sc.dataServiceConn)

	// 事件号来自 player_inbox/generated/pb/game(proto-gen 产物,随仓库跟踪):干净检出不跑生成也能编译。
	sc.notifier = &inbox.KafkaNotifier{
		Locations: scenenode.GoRedisReader{Client: sc.CoreRedis},
		Nodes:     sc.SceneNodes,
		Writer:    &relayMessageWriter{admission: relay, writer: sc.Kafka},
		EventID:   uint32(game.PlayerInboxNotifyEventEventId),
	}
	sc.Store, err = inbox.NewStore(inbox.StoreDeps{
		DB:       sc.DB,
		Registry: registry,
		Allowed:  allowed,
		Players:  &DataServicePlayers{Client: sc.DataService, Timeout: constants.HomeZoneLookupTimeout},
		Epochs:   &RedisEpochs{Client: sc.CoreRedis},
		Notifier: sc.notifier,
		Now:      time.Now,
		Roles:    roles,
	})
	if err != nil {
		return nil, fmt.Errorf("player_inbox: build store: %w", err)
	}
	sc.Store.RelayAdmission = relay
	sc.Ledgers = &DataServiceLedgers{Client: sc.DataService, Timeout: constants.LedgerReadTimeout}

	elector := leader.New(leader.NewGoRedisStore(sc.CoreRedis), MaintenanceLeaderKey, leader.Options{ID: leaderID()})
	sc.Loops, err = inbox.NewLoops(sc.Store, sc.Ledgers, loopConfigFrom(c.PlayerInbox), elector.Run, time.Now)
	if err != nil {
		return nil, fmt.Errorf("player_inbox: build maintenance loops: %w", err)
	}
	sc.Loops.RootAdmission = root
	sc.Loops.RelayAdmission = relay
	sc.background = []backgroundTask{
		{name: "player_inbox.scenenode.watcher", run: func(ctx context.Context) { sc.SceneNodes.Run(ctx, sc.Etcd) }},
		{name: "player_inbox.maintenance", run: sc.Loops.Run},
	}

	logx.Infof("[player_inbox] Enqueue allowlist: %d type(s) enabled %v (empty = all disabled, design §3.3)",
		len(allowed), c.PlayerInbox.EnqueueTypes)
	ok = true
	return sc, nil
}

// relayMessageWriter 只适配既有 MessageWriter；每次真实 Kafka 发布归属同一 RELAY。
// broker ACK 只结束本次发送，不代表玩家 APPLIED 或持久收件箱已排空。
// 写入返回错误或 panic 时保留未知结果；取消和后续重试不会清除它。
type relayMessageWriter struct {
	admission *drainproof.Session
	writer    inbox.MessageWriter
}

func (w *relayMessageWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	if w == nil || w.admission == nil || w.writer == nil {
		return drainproof.ErrHold
	}
	finish, err := w.admission.Enter()
	if err != nil {
		return err
	}
	settled := false
	defer func() {
		if !settled {
			w.admission.Hold()
		}
		finish()
	}()
	err = w.writer.WriteMessages(ctx, msgs...)
	settled = err == nil
	return err
}

// loopConfigFrom 把配置换算成 inbox.LoopConfig。合法区间由 config.Validate 先拦,inbox.NewLoops 再兜底。
func loopConfigFrom(p config.PlayerInboxConf) inbox.LoopConfig {
	return inbox.LoopConfig{
		TerminalRetention: time.Duration(p.TerminalRetentionDays) * 24 * time.Hour,
		SweepInterval:     time.Duration(p.SweepIntervalSeconds) * time.Second,
		NotifyInterval:    time.Duration(p.NotifyResendIntervalSeconds) * time.Second,
		GaugeInterval:     time.Duration(p.GaugeIntervalSeconds) * time.Second,
		ReconcileInterval: time.Duration(p.ReconcileIntervalMinutes) * time.Minute,
		ReconcileWindow:   time.Duration(p.ReconcileWindowHours) * time.Hour,
	}
}

// leaderID 是选主竞选者标识,只进锁值前缀与日志。注册 etcd 之前就要竞选,所以用主机名(K8s 下即 Pod 名)。
func leaderID() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "player_inbox"
}

// Start 起后台任务:scene 节点镜像与后台循环(选主 → 任期内跑循环)。ctx 取消即退出。
// 重复调用只生效一次;Stop 之后再 Start 不起任何任务。
func (sc *ServiceContext) Start(ctx context.Context) {
	sc.startOnce.Do(func() {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if sc.stopped {
			return
		}
		loopCtx, cancel := context.WithCancel(ctx)
		sc.cancel = cancel
		for _, task := range sc.background {
			sc.wg.Add(1)
			safego.Go(task.name, func() {
				defer sc.wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						if sc.RelayAdmission != nil {
							sc.RelayAdmission.Hold()
						}
						panic(recovered)
					}
				}()
				task.run(loopCtx)
			})
		}
	})
}

// BindDrainRuntime 只绑定已消费的同一 SQL boot；不重新读取 grant 或替换实际门禁。
// main 仍须将根门禁装入 RPC，并单独保留 Fetch/Ack 与历史补投的 RELAY 接收链。
func (sc *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if runtime == nil || sc.DrainRuntime != nil || sc.RootAdmission == nil || sc.RelayAdmission == nil ||
		sc.RootAdmission == sc.RelayAdmission || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(sc.WriterConfig.InstanceID(), sc.WriterConfig.WriterKey()); err != nil {
		return err
	}
	sc.DrainRuntime = runtime
	return nil
}

// CloseSource 只关闭新增根请求，并阻止未启动的后台任务在关闭后启动。
// 已运行的提醒、补投和回执链继续使用同 boot 的 RELAY、Kafka 与 SQL；不取消它们来制造空闲。
func (sc *ServiceContext) CloseSource(ctx context.Context) error {
	if sc.RootAdmission == nil {
		return drainproof.ErrHold
	}
	sc.mu.Lock()
	sc.stopped = true
	sc.mu.Unlock()
	sc.RootAdmission.CloseAdmission()
	if err := sc.RootAdmission.Wait(ctx); err != nil {
		return err
	}
	return nil // main 已按客户端/Admin SOURCE 与 Fetch/Ack/终局 RELAY 分流；sweep 同 RootAdmission。
}

// StopAndReport 停止取新并等待真实本机调用、轮次和通知 CAS；未知结局保留 HOLD。
func (sc *ServiceContext) StopAndReport(ctx context.Context) error {
	if err := sc.CloseSource(ctx); err != nil {
		return err
	}
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if sc.RelayAdmission == nil || sc.RelayIngress == nil {
		return drainproof.ErrHold
	}
	sc.RelayIngress.CloseAdmission()
	if err := sc.RelayIngress.Wait(ctx); err != nil {
		return err
	}
	sc.mu.Lock()
	cancel := sc.cancel
	sc.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	joined := make(chan struct{})
	go func() { sc.wg.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		sc.RelayAdmission.Hold()
		return errors.Join(drainproof.ErrHold, ctx.Err())
	}
	if err := sc.RelayAdmission.WaitIdle(ctx); err != nil {
		return err
	}
	if sc.Kafka != nil {
		if err := sc.Kafka.Close(); err != nil {
			sc.RelayAdmission.Hold()
			return err
		}
	}
	sc.RelayAdmission.CloseAdmission()
	if err := sc.RelayAdmission.Wait(ctx); err != nil {
		return err
	}
	// PENDING/DELIVERED 是冻结 SQL 库中的已知持久待办，不等同本机未返回 RPC。
	// 只封存真实已接纳调用的结局；离线玩家待办由 P9 新 boot 按原幂等合同继续。
	sc.WriterConfig.Seal()
	sc.resourceCloseOnce.Do(func() { sc.resourceCloseErr = sc.closeResourcesReport() })
	return sc.resourceCloseErr
}

func (sc *ServiceContext) Stop() {
	timeout := sc.stopTimeout
	if timeout <= 0 {
		timeout = loopsStopTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := sc.StopAndReport(ctx); err != nil {
		logx.Errorf("player_inbox 排空 HOLD，保留同 boot SQL/RELAY 资源: %v", err)
	}
}
func (sc *ServiceContext) closeResources() {
	if err := sc.closeResourcesReport(); err != nil {
		logx.Errorf("player_inbox 资源关闭失败: %v", err)
	}
}
func (sc *ServiceContext) closeResourcesReport() error {
	var result error
	if sc.Kafka != nil {
		result = errors.Join(result, sc.Kafka.Close())
	}
	if sc.CoreRedis != nil {
		result = errors.Join(result, sc.CoreRedis.Close())
	}
	if sc.dataServiceConn != nil {
		result = errors.Join(result, sc.dataServiceConn.Close())
	}
	if sc.DB != nil {
		result = errors.Join(result, sc.DB.Close())
	}
	if sc.Etcd != nil {
		result = errors.Join(result, sc.Etcd.Close())
	}
	return result
}

// BuildDSN 拼 mmorpg_player_inbox 的 DSN(与 go/trade BuildDSN 同口径,另加锁等待上限):
//   - sql_mode=STRICT_TRANS_TABLES:自由文本超长时报错而不是静默截断(入箱校验之外的第二道防线);
//   - transaction_isolation=READ-COMMITTED:事务之外的 autocommit 语句(补建流行、清理、提醒 CAS)也走 RC,
//     不在 RR 下拿间隙锁;显式事务本来就经 WithTxRetryConfig 固定 RC(设计 §10);
//   - innodb_lock_wait_timeout=1:锁等待上限 1 秒(设计 §10,照 data_service 的 route store),
//     等不到即 1205,由注入的可重试判定有界重试,不让请求在锁上耗光预算。
//
// 返回值含密码,只能交给 sql.Open,绝不打日志。
func BuildDSN(c config.MySQLConf) string {
	parsed := mysql.NewConfig()
	parsed.User, parsed.Passwd, parsed.Net, parsed.Addr, parsed.DBName, parsed.ParseTime = c.User, c.Password, "tcp", c.Host, c.DBName, true
	parsed.Params = map[string]string{"charset": "utf8mb4", "sql_mode": "'STRICT_TRANS_TABLES'", "transaction_isolation": "'READ-COMMITTED'", "innodb_lock_wait_timeout": "1"}
	return parsed.FormatDSN()
}

// MySQLTarget 是连接目标的可读描述(不含密码),供横幅与错误信息使用。
func MySQLTarget(c config.MySQLConf) string {
	return fmt.Sprintf("%s/%s (user=%s)", c.Host, c.DBName, c.User)
}

// OpenMySQL保留原连接入口名；全球池只接受TiDB并逐物理连接固定当前强读，失败自行关闭。
func OpenMySQL(c config.MySQLConf) (*sql.DB, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(c, authoritysql.Writer(writer))
}

// OpenSchemaMySQL只属于显式迁移入口。
func OpenSchemaMySQL(c config.MySQLConf) (*sql.DB, error) {
	permission, err := authoritysql.LoadSchemaPermission(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(c, permission)
}

func openMySQL(c config.MySQLConf, permission authoritysql.Permission) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	db, err := authoritysql.Open(ctx, BuildDSN(c), c.DBName, true, c.MaxOpenConn, permission)
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(c.MaxIdleConn)
	return db, nil
}
