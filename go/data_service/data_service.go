package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	dskafka "data_service/internal/kafka"
	"data_service/internal/metrics"
	"data_service/internal/noderegistry"
	"data_service/internal/server"
	"data_service/internal/store"
	"data_service/internal/svc"
	base "proto/common/base"
	"proto/data_service"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/safego"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/data_service.yaml", "the config file")

// migrateOnly 是生产用的显式建表入口:`data_service -f <yaml> -migrate` 对全局库四张表
// 跑一次 store.MigrateSchema(与 Schema.AutoMigrate=true 的启动路径是同一段代码),
// 跑完即退出,不起 gRPC、不连 Redis。生产把 AutoMigrate 设 false,部署阶段单进程跑这个。
var migrateOnly = flag.Bool("migrate", false, "run the global-DB schema migration (proto2mysql) and exit")

// killSwitchEtcdDialTimeout 与 scene_manager 建 etcd 客户端时的取值一致(5s)。
// 只影响 killswitch 自己的 watch 连接,拨不通也只是退回全放行。
const killSwitchEtcdDialTimeout = 5 * time.Second

// kafkaStartRetryInterval:Kafka 不可达(EnsureTopics 失败)时后台重试的间隔,
// 与 go/match/match_service.go 的对局结果消费者同口径。
const kafkaStartRetryInterval = 30 * time.Second

// kafkaShutdownGrace:关停时等两条消费者退出的上限。txlog 的 flushOnShutdown 自己用
// 5s ctx 插最后一批,再加一次 offset 提交,8s 够;超时就放弃等待照常关池 —— 未提交的
// offset 会在下次启动时重放,而 tx_id 主键 / snapshot_guid 去重让重放是幂等的。
const kafkaShutdownGrace = 8 * time.Second

// nodeRegisterEtcdDialTimeout:C++ 约定注册用的 etcd 客户端拨号超时,与 killswitch 那把同值。
const nodeRegisterEtcdDialTimeout = 5 * time.Second

// nodeRegisterListenWait:等 gRPC 监听端口真正 accept 的上限。zrpc 的 Start() 阻塞在 Serve 里,
// 没有"已监听"回调,只能从旁边拨一下端口;监听失败 zrpc 自己会 panic,这个上限只是兜底,
// 到点仍拨不通同样 panic —— 不能让一个"永远没注册"的 data_service 装作健康跑下去。
const nodeRegisterListenWait = 30 * time.Second

// nodeRegisterListenPoll:上面那个等待循环的拨号间隔。
const nodeRegisterListenPoll = 100 * time.Millisecond

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	if *migrateOnly {
		os.Exit(runMigration(c))
	}

	svcCtx := svc.NewServiceContext(c)
	defer svcCtx.Close()

	// ── Kafka 落库消费者(transaction_log / player_snapshot)────────────
	// Kafka 不可达绝不能拖死 data_service:Load/Save 是玩家数据热路径。启动失败只记日志,
	// 后台每 30s 重试;消费者内部 DB 故障时停下不提交(见 internal/kafka 包注释)。
	kafkaCtx, kafkaCancel := context.WithCancel(context.Background())
	waitKafka := startKafkaConsumers(kafkaCtx, c.Kafka, svcCtx)
	// 这个 defer 注册在 `defer svcCtx.Close()` **之后**,所以按 LIFO 先于它执行:
	// 先取消消费者、等它们把手里那批 flush 完(有界),最后才关连接池。反过来的话
	// txlog 的 flushOnShutdown 只会拿到一个已经关掉的池,等于从来没写过。
	defer func() {
		kafkaCancel()
		if !waitKafka(kafkaShutdownGrace) {
			logx.Errorf("[kafka] consumers did not stop within %s; closing the stores anyway (unflushed rows will be replayed from the uncommitted offsets)", kafkaShutdownGrace)
		}
	}()

	// Start Prometheus /metrics endpoint. Empty addr → no-op.
	// Surfaces per-RPC outcome / latency, lock contention, version mismatch,
	// rollback counts. See internal/metrics/metrics.go.
	metrics.Start(c.MetricsListenAddr)

	// ── 热关停(killswitch)────────────────────────────────────────────
	// data_service 是玩家权威数据的读写入口,真出事时最缺的是"秒级止血阀":
	// 往 etcd 前缀 /mmorpg/killswitch/ 下写一个 key 就能把某个方法立刻短路掉
	// (例如 RollbackAll 这种放大面极大的运维接口),而不必走一遍
	// 构建-发布-滚动更新。规则布局与优先级见 shared/killswitch 包注释。
	//
	// New 出来立刻可用(规则为空 = 全放行),Start 非阻塞、etcd 为 nil 也合法,
	// 所以整段接线不会让 data_service 的启动多出任何一个失败点。
	ks := killswitch.New(killswitch.Config{})
	ksCtx, ksCancel := context.WithCancel(context.Background())
	defer ksCancel()
	etcdCli := newKillSwitchEtcdClient(c.Etcd)
	if etcdCli != nil {
		defer etcdCli.Close()
	}
	ks.Start(ksCtx, etcdCli)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		data_service.RegisterDataServiceServer(grpcServer, server.NewDataServiceServer(svcCtx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 拦截器顺序是有意的(先加的在外层):
	//   grpcstats → killswitch → serverbase → handler
	// grpcstats 放最外层,被关停的请求也照样计入总量(否则一开闸就像"没人调用");
	// killswitch 命中即短路,handler 根本不会被调用;
	// serverbase 放最内层,只观测真正跑过 handler 的结果,不会把"被人为关停"
	// 误记成一次业务故障。
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	s.AddUnaryInterceptors(ks.UnaryServerInterceptor())
	// in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把失败塞进
	// 响应体的 `uint32 error_code` —— gRPC status 恒 OK,go-zero 自带的指标拦截器
	// 会把每一次"Redis 挂了""快照库写不进去"都记成一次成功请求,监控上看不出
	// 任何比例变化,直到玩家数据已经丢了才有人发现。这层把响应体里的码读出来
	// 定性:故障打日志 + 计数,业务拒绝只计数。它不改响应内容、不吞错,对调用方无感。
	//
	// 传 ErrorCodeClassifier 而**不是** TipClassifier,依据是 proto 与 handler 的
	// 事实:data_service 走的是自己的私有码表(internal/constants/error_codes.go,
	// 0..17),由 `uint32 error_code` 字段承载,与 friend/guild 那套 TipInfoMessage
	// 码表完全无关 —— 数值区间还高度重叠(本表的 1 是 Redis 失败,tip 的 1 是
	// kSuccess),混用会得到垃圾定性。serverbase 在没有 ErrorCodeClassifier 时
	// 对私有码只敢记成 VerdictBizReject,所以这份集合必须由本服务显式给出。
	//
	// 归进"服务端内部故障"的三个码,逐个都有产码点为证:
	//   ErrCodeRedis(1)          —— 玩家数据 Redis 读写失败(data_logic.go 多处)
	//   ErrCodeSnapshotDBError(11) —— 快照/流水 MySQL 出错,或 store 干脆没配起来
	//   ErrCodeRollbackFailed(12)  —— 回滚 fence 拿不到/释放函数为 nil,服务端自身走死
	// 其余非 0 码刻意**不算**故障,免得刷出满屏假告警:
	//   LockConflict(2) / VersionMismatch(3)  正常并发竞争,重试即可
	//   NotFound(4) / SnapshotNotFound(10) / ZoneNotFound(15)  查无此物
	//   PlayerOnline(13)   回滚前置条件不满足,是规则拒绝
	//   InvalidRequest(14) 调用方参数问题
	//   NotImplemented(16) 刻意的 fail-closed(fence 未配置就明确失败,不装成成功),
	//                      是确定性的配置态而非运行期故障,告警只会变噪音
	//   ResultTruncated(17) 命中数超上限的容量拒绝 —— 按 serverbase 的既定口径,
	//                      "满"属于规则拒绝不是故障
	// 号段发号(AllocateIdSegment)再加两个故障码:
	//   ErrCodeIdSegmentDBError(18)   —— id_segment 表读写失败 / store 没配起来,发号源停摆
	//   ErrCodeIdSegmentExhausted(19) —— 值域 2^55 见底的 fail-closed;理论上万年不会发生,
	//                                    发生即事故,必须当故障告警而不是"业务拒绝"
	//   ErrCodeIdSegmentUnknownTag(20) —— 生产(AllowAutoSeed=false)下 id_segment 缺行:漏配
	//                                    BootstrapTags,或全局库被重置 / 从备份恢复 —— 后者是
	//                                    "从 1 重发覆盖别人角色行"的前夜,必须告警而不是当参数错
	// 合服那四个码(21..24,见 constants)刻意不在这份集合里,而且这层拦截器也看不到它们:
	// RegisterPlayerZone 的响应体是 emptypb.Empty、RemapHomeZoneForMerge 的拒绝走 gRPC status,
	// 都没有 in-band 的 error_code 字段可读。它们的可见性由 handler 里的 logx.Errorf 提供
	// (每一条都带调用方身份),外加 grpcstats 那层按 gRPC code 统计的失败率。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{
		ErrorCodeClassifier: serverbase.FaultCodeSet(
			constants.ErrCodeRedis,
			constants.ErrCodeSnapshotDBError,
			constants.ErrCodeRollbackFailed,
			constants.ErrCodeIdSegmentDBError,
			constants.ErrCodeIdSegmentExhausted,
			constants.ErrCodeIdSegmentUnknownTag,
		),
	}))
	defer s.Stop()

	// ── C++ 约定的 etcd 注册(DataServiceNodeService.rpc)──────────────
	// go-zero 只写自己的 dataservice.rpc 键,C++ 看不见;scene 靠本注册发现 data_service
	// 并调 AllocateIdSegment 领 item / txlog / snapshot 号段,发现不到就永远过不了
	// DependencyGate。所以这条注册**必须成功**:失败 = 启动致命(panic,与 scene_manager
	// 同一语义),不能让一个"C++ 找不到"的 data_service 装作健康跑下去。
	//
	// 与 scene_manager 只差时序:那边在 Start() 之前注册;这里等端口真正 accept 之后才写
	// key —— C++ 一看到 NodeInfo 就会拨,先注册后监听会让它吃一次 connection refused。
	// zrpc.Start() 阻塞在 Serve 里且没有"已监听"回调,所以放一条 goroutine 旁路拨端口。
	//
	// etcd 客户端刻意不与 killswitch 共用:那把的契约是 fail-open(nil 合法、永不 panic),
	// 这把的契约恰好相反(建不出来就拒启)。两把 defer 的 LIFO 顺序保证先注销再关客户端。
	regCli := mustNewNodeRegistryEtcdClient(c.Etcd)
	defer regCli.Close()
	registrar := startNodeRegistration(c, regCli)
	defer registrar.close()

	fmt.Println("\n=============================================================")
	fmt.Println("  DATA_SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Mode:        %s\n", c.Mode)
	if len(c.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	}
	// node_id 要等端口 accept 后由注册 goroutine 打日志([NodeRegistry] ...);横幅只能给出路径与租约。
	fmt.Printf("  c++ node:    DataServiceNodeService.rpc/zone/%d/node_type/%d (lease_ttl=%ds, advertised %s:%d)\n",
		c.ZoneId, uint32(base.ENodeType_DataServiceNodeService), c.LeaseTTL, registrar.host, registrar.port)
	fmt.Printf("  redis:       %s\n", c.MappingRedis.Host)
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:     %s/metrics\n", c.MetricsListenAddr)
	}
	if len(c.Kafka.Brokers) > 0 {
		// 打**有效** topic 名(带代号后缀):排障时第一件要核对的就是它与 C++ 生产者一致。
		fmt.Printf("  kafka:       %v (%s, %s)\n", c.Kafka.Brokers,
			c.Kafka.EffectiveTransactionLogTopic(), c.Kafka.EffectiveSnapshotTopic())
	}
	fmt.Println("=============================================================")
	s.Start()
}

// runMigration 是 -migrate 的实现:不设超时(存量大表的 MODIFY COLUMN 会重建表,
// 时间由表决定),成功 0、失败 1。输出走 stdout/stderr,方便部署脚本直接读退出码。
func runMigration(c config.Config) int {
	cfg := svc.MySQLConfigOf(c)
	tags := c.IdSegment.EffectiveBootstrapTags()
	fmt.Printf("data_service schema migration: %s/%s (id_segment bootstrap tags: %v)\n", cfg.Host, cfg.DBName, tags)
	// 号段行在生产只能由这条路径创建(IdSegment.AllowAutoSeed 生产恒 false),所以 -migrate
	// 必须带完整的 BootstrapTags;迁移顺带把 player / guild 的 max_id 抬到消费表最大号 +1
	// (同库能查到时),那条 Error 日志出现就意味着库曾被重置 / 从备份恢复,按运维手册处理。
	if err := store.MigrateSchema(context.Background(), cfg, store.MigrateOptions{BootstrapTags: tags}); err != nil {
		fmt.Fprintf(os.Stderr, "schema migration FAILED: %v\n", err)
		return 1
	}
	fmt.Printf("schema migration OK: transaction_log, player_snapshot, rollback_audit_log synced from proto; id_segment bootstrapped for %v (existing rows untouched, player/guild floors checked)\n", tags)
	return 0
}

// startKafkaConsumers 启动两条落库消费者,返回一个"等它们停下"的有界等待函数(给关停用;
// 没起来时是 no-op)。失败只记日志并每 30s 后台重试,**永不阻塞启动**。
// Brokers 为空 = 本地无 Kafka 的合法形态,直接不消费。两个 store 任一为 nil(MySQL 不可达
// 或迁移失败)时也不启动:消费者没有落库目标,启动了只会立刻停下。
func startKafkaConsumers(ctx context.Context, kc config.KafkaConfig, svcCtx *svc.ServiceContext) func(time.Duration) bool {
	noop := func(time.Duration) bool { return true }

	if len(kc.Brokers) == 0 {
		logx.Info("[kafka] Kafka.Brokers empty; transaction_log / player_snapshot consumers disabled")
		return noop
	}
	if svcCtx.TxLogStore == nil || svcCtx.SnapshotStore == nil {
		logx.Errorf("[kafka] consumers NOT started: transaction log / snapshot store unavailable (MySQL unreachable or schema migration failed); restart data_service after the database recovers")
		return noop
	}

	// 先把两条 kafka_consumer_up 序列按 0 注册出来,再做第一次尝试。
	// Prometheus 的 *Vec 只有 WithLabelValues 之后才有那个 child series:不预注册的话
	// "EnsureTopics 一直失败 → 消费者从未启动"这个最该告警的状态下,
	// kafka_consumer_up{consumer="..."} 干脆不存在,`== 0` 的告警永远不会触发,
	// 监控上和"服务没部署"长得一模一样。
	metrics.SetKafkaConsumerUp(dskafka.ConsumerTxLog, false)
	metrics.SetKafkaConsumerUp(dskafka.ConsumerSnapshot, false)

	txTopic := kc.EffectiveTransactionLogTopic()
	snapshotTopic := kc.EffectiveSnapshotTopic()

	var (
		mu        sync.Mutex
		consumers *dskafka.Consumers
	)
	start := func() error {
		cs, err := dskafka.Start(ctx, dskafka.StartConfig{
			Brokers:            kc.Brokers,
			TxLogTopic:         txTopic,
			TxLogPartitions:    kc.TransactionLogPartitions,
			TxLogGroup:         kc.TransactionLogConsumerGroup,
			SnapshotTopic:      snapshotTopic,
			SnapshotPartitions: kc.SnapshotPartitions,
			SnapshotGroup:      kc.SnapshotConsumerGroup,
			RetentionMs:        kc.RetentionMs,
		}, svcCtx.TxLogStore, svcCtx.SnapshotStore)
		if err != nil {
			return err
		}
		mu.Lock()
		consumers = cs
		mu.Unlock()
		return nil
	}

	// 连第一次尝试都放进 goroutine:EnsureTopics 对半死不活的 broker 会走完 sarama 的
	// 30s 拨号超时加元数据重试,同步做等于让一个纯审计组件把 gRPC 服务器的启动
	// (以及 LoadPlayerData / SavePlayerData)按住几十秒。
	safego.Go("data_service.kafka.start", func() {
		for attempt := 1; ; attempt++ {
			if err := start(); err == nil {
				if attempt > 1 {
					logx.Info("[kafka] consumers started on retry; backlog will be consumed from the earliest offset")
				}
				return
			} else {
				logx.Errorf("[kafka] consumers not started (attempt %d), retrying in %s (Load/Save unaffected; topics=%s,%s): %v",
					attempt, kafkaStartRetryInterval, txTopic, snapshotTopic, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(kafkaStartRetryInterval):
			}
		}
	})

	return func(timeout time.Duration) bool {
		mu.Lock()
		cs := consumers
		mu.Unlock()
		return cs.Wait(timeout)
	}
}

// newKillSwitchEtcdClient 按 RpcServerConf.Etcd 建一个只给热关停 watch 用的
// etcd 客户端。**返回 nil 是合法结果**,调用方直接把 nil 传给
// killswitch.Start 即可(它会打一条 Info 后全部放行)。
//
// 为什么不复用 go-zero 服务注册内部那个客户端:go-zero 没把它暴露出来。
//
// 为什么任何失败都只记日志不 panic:fail-open 是 killswitch 的铁律 ——
// 管控组件自身故障绝不能拖垮业务。data_service 是玩家权威数据的读写入口,
// 因为一个"止血阀连不上 etcd"就拒启,等于用小故障换一次全服数据不可用。
//
// 注意 clientv3.New 不带 WithBlock,不会在这里真的去拨号,因此也不会拖慢启动;
// 连不通的后果只是 killswitch 的全量同步失败并按 ResyncBackoff 重试(期间放行)。
func newKillSwitchEtcdClient(cfg discov.EtcdConf) *clientv3.Client {
	if len(cfg.Hosts) == 0 {
		logx.Info("[killswitch] data_service 未配置 etcd Hosts,热关停不生效(全部放行)")
		return nil
	}

	c := clientv3.Config{
		Endpoints:   cfg.Hosts,
		DialTimeout: killSwitchEtcdDialTimeout,
	}
	if cfg.HasAccount() {
		c.Username = cfg.User
		c.Password = cfg.Pass
	}

	cli, err := clientv3.New(c)
	if err != nil {
		logx.Errorf("[killswitch] data_service 建 etcd 客户端失败,热关停不生效(全部放行): %v", err)
		return nil
	}
	return cli
}

// mustNewNodeRegistryEtcdClient 按 RpcServerConf.Etcd 建 C++ 约定注册专用的 etcd 客户端。
// 与 newKillSwitchEtcdClient 的契约相反:这里任何失败都 panic —— 没有这条注册 scene 就
// 永远发现不了 data_service,"起来了但没人找得到"比起不来更糟(K8s 会把后者标成
// CrashLoopBackOff,前者看起来一切正常)。
//
// clientv3.New 不带 WithBlock 不会真的拨号,连不通要到 noderegistry.Register 的 Grant
// (10s 超时)才暴露,并同样以 panic 结束。
func mustNewNodeRegistryEtcdClient(cfg discov.EtcdConf) *clientv3.Client {
	if len(cfg.Hosts) == 0 {
		panic("data_service: Etcd.Hosts is empty; C++ convention registration (DataServiceNodeService.rpc) is mandatory because scene discovers data_service through it")
	}
	c := clientv3.Config{
		Endpoints:   cfg.Hosts,
		DialTimeout: nodeRegisterEtcdDialTimeout,
	}
	if cfg.HasAccount() {
		c.Username = cfg.User
		c.Password = cfg.Pass
	}
	cli, err := clientv3.New(c)
	if err != nil {
		panic("data_service: failed to create etcd client for C++ convention registration: " + err.Error())
	}
	return cli
}

// nodeRegistrar 持有异步完成的 C++ 约定注册,让关停路径能拿到它并注销。
// 注册跑在 goroutine 里(要等端口 accept),关停可能先于注册完成:closed 标记让
// 晚到的注册结果当场注销,不会留下一把要等满 LeaseTTL 才过期的孤儿 key。
type nodeRegistrar struct {
	host string // 写进 NodeInfo 的对外 IP(见 advertisedEndpoint)
	port uint32

	mu     sync.Mutex
	nr     *noderegistry.NodeRegistration
	closed bool
}

func (r *nodeRegistrar) set(nr *noderegistry.NodeRegistration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		nr.Close()
		return
	}
	r.nr = nr
}

// close 注销(删两把 key + Revoke 租约)。未注册完成时只打标记,由 set 收尾。
func (r *nodeRegistrar) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.nr != nil {
		r.nr.Close()
		r.nr = nil
	}
}

// startNodeRegistration 起一条 goroutine:等本进程的 gRPC 端口 accept,再按 C++ 约定注册
// DataServiceNodeService 的 NodeInfo 并开始 keepalive。任何一步失败都 panic(启动致命)。
func startNodeRegistration(c config.Config, cli *clientv3.Client) *nodeRegistrar {
	listenHost, port := parseListenOn(c.ListenOn)
	r := &nodeRegistrar{host: advertisedHost(listenHost), port: port}

	// 拨号地址与对外地址是两回事:0.0.0.0 监听时本机只能拨 loopback,而 NodeInfo 里
	// 必须写别的 Pod / 进程连得上的 IP。
	dialHost := listenHost
	if isUnspecifiedHost(dialHost) {
		dialHost = "127.0.0.1"
	}
	dialAddr := net.JoinHostPort(dialHost, strconv.FormatUint(uint64(port), 10))

	// 刻意用裸 go 而不是 safego.Go:safego 会把 panic 兜住变成一个指标继续跑,而这里的
	// panic 就是要让进程死(CrashLoopBackOff 可见),与 scene_manager 注册失败即 panic 同一语义。
	go func() {
		if err := waitForListening(dialAddr, nodeRegisterListenWait); err != nil {
			panic("data_service: gRPC port never accepted within " + nodeRegisterListenWait.String() +
				", cannot register DataService node in etcd: " + err.Error())
		}
		nr, err := noderegistry.Register(cli, uint32(base.ENodeType_DataServiceNodeService), c.ZoneId, r.host, port, c.LeaseTTL)
		if err != nil {
			panic("failed to register DataService node in etcd: " + err.Error())
		}
		nr.KeepAlive()
		logx.Infof("[NodeRegistry] DataService registered: DataServiceNodeService.rpc zone=%d node_type=%d node_id=%d uuid=%s endpoint=%s:%d lease_ttl=%ds",
			nr.Info.ZoneId, nr.Info.NodeType, nr.Info.NodeId, nr.Info.NodeUuid, r.host, port, c.LeaseTTL)
		r.set(nr)
	}()
	return r
}

// waitForListening 反复拨 addr 直到连上(随即关掉)或超时;返回最后一次拨号错误。
func waitForListening(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", addr, nodeRegisterListenPoll*5)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(nodeRegisterListenPoll)
	}
}

// parseListenOn 把 "host:port" 拆开(与 scene_manager 的同名函数同一职责,这里用
// net.SplitHostPort 以正确处理 "[::]:9000")。port 解析失败是配置错误,直接 panic。
func parseListenOn(listenOn string) (string, uint32) {
	host, portStr, err := net.SplitHostPort(listenOn)
	if err != nil {
		panic("data_service: ListenOn must be host:port, got " + strconv.Quote(listenOn) + ": " + err.Error())
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil || port == 0 {
		panic("data_service: ListenOn port is not a valid port: " + strconv.Quote(listenOn))
	}
	return host, uint32(port)
}

// isUnspecifiedHost:""、0.0.0.0、:: 这类"监听所有网卡"的 host,不能原样写进 NodeInfo。
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// advertisedHost 决定写进 NodeInfo 的 IP:
//  1. POD_IP 环境变量(K8s Downward API,manifests/go-svc/data-service.yaml 注入)优先,
//     与 go/login/login.go 同一约定 —— 容器里 ListenOn 恒为 0.0.0.0,其它 Pod 连不上它;
//  2. 否则 ListenOn 里写了具体 host 就用它;
//  3. 否则(本地 dev 的 0.0.0.0)取本机第一个非 loopback IP,与 go-zero 给 dataservice.rpc
//     键取值的 figureOutListenOn 同一口径;拿不到(没有非 loopback 网卡)退回 127.0.0.1 ——
//     至少本机 C++ 还连得上,而不是写一个 Windows 上根本拨不通的 0.0.0.0。
func advertisedHost(listenHost string) string {
	if podIP := os.Getenv("POD_IP"); podIP != "" {
		return podIP
	}
	if !isUnspecifiedHost(listenHost) {
		return listenHost
	}
	if ip := netx.InternalIp(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}
