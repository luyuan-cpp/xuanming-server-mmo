package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	dskafka "data_service/internal/kafka"
	"data_service/internal/metrics"
	"data_service/internal/noderegistry"
	"data_service/internal/routereconcile"
	"data_service/internal/routetoolcli"
	"data_service/internal/server"
	"data_service/internal/store"
	"data_service/internal/svc"
	base "proto/common/base"
	databasepb "proto/common/database"
	"proto/data_service"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/drainproof"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/safego"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/data_service.yaml", "the config file")
var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式 typed 排空 runtime 原件")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有排空原件目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime 原文件 SHA256")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空与危险观察的私有目录")

// migrateOnly 是生产用的显式建表入口:`data_service -f <yaml> -migrate` 对全局库跑一次
// store.MigrateSchema(proto 驱动的五张 + 手写 DDL 的路由表,并种 route_meta / route_reconcile 行;与
// Schema.AutoMigrate=true 的启动路径是同一段代码),跑完即退出,不起 gRPC、不连 Redis。
// 生产把 AutoMigrate 设 false,部署阶段单进程跑这个。
var migrateOnly = flag.Bool("migrate", false, "run the global-DB schema migration (proto2mysql) and exit")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

// routeTool 是 route_tool 的主程序入口(映射持久化第 1b-1 批,2026-10-18):`data_service -f <yaml> -route-tool <子命令> [参数]`
// 跑一个运维子命令后退出,不起 gRPC、不连别的依赖。K8s 镜像只构建单一入口 /app/service(deploy/k8s/Dockerfile.go-svc),
// 生产上只能这样跑 route_tool(一次性 Job / Pod,runbook §9.4)。逻辑在 internal/routetoolcli,与 cmd/route_tool 同一份。
var routeTool = flag.Bool("route-tool", false, "run a route_tool subcommand and exit: data_service -f <yaml> -route-tool <subcommand> [flags]")

// entryMode 是 main 的三种入口。
type entryMode uint8

const (
	entryServe entryMode = iota
	entryMigrate
	entryRouteTool
)

// selectEntry 按 flag 选入口(纯函数,data_service_entry_test.go 表驱动钉住)。args 是 flag.Args()(第一个非 flag 参数起)。
//   - -migrate 与 -route-tool 同时给:拒绝(一次只做一件事);
//   - -route-tool 但没有子命令:拒绝;
//   - 没有 -route-tool 却带位置参数:拒绝 —— 以前这些参数被静默忽略,`data_service -f x.yaml audit` 会被当成一次普通启动、
//     起一个服务进发现池,而运维以为自己跑的是审计。
func selectEntry(migrate, routeTool bool, args []string) (entryMode, error) {
	switch {
	case migrate && routeTool:
		return entryServe, errors.New("-migrate and -route-tool are mutually exclusive")
	case routeTool && len(args) == 0:
		return entryServe, errors.New("-route-tool needs a subcommand: data_service -f <yaml> -route-tool <subcommand> [flags]")
	case routeTool:
		return entryRouteTool, nil
	case len(args) > 0:
		return entryServe, fmt.Errorf("unexpected positional arguments %q; route_tool subcommands need -route-tool "+
			"(data_service -f <yaml> -route-tool %s ...)", args, args[0])
	case migrate:
		return entryMigrate, nil
	default:
		return entryServe, nil
	}
}

// killSwitchEtcdDialTimeout 与 scene_manager 建 etcd 客户端时的取值一致(5s)。
// 只影响 killswitch 自己的 watch 连接,拨不通也只是退回全放行。
const killSwitchEtcdDialTimeout = 5 * time.Second

// kafkaStartRetryInterval:Kafka 不可达(EnsureTopics 失败)时后台重试的间隔,
// 与 go/match/match_service.go 的对局结果消费者同口径。
const kafkaStartRetryInterval = 30 * time.Second

// kafkaShutdownGrace 只限定调用方等待；到期返回 HOLD，不能据此关闭仍有在途 SQL 的池。
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
	entry, err := selectEntry(*migrateOnly, *routeTool, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "data_service: %v\n", err)
		os.Exit(2)
	}
	if entry == entryRouteTool {
		// route_tool 的 stdout 只许有一行统计(给脚本解析):版本行改打到 stderr,其余与下面同理(先于读配置,Job 日志可追溯)。
		// 配置由 routetoolcli 自己用 conf.Load 读:conf.MustLoad 出错时直接退出 1,破坏 route_tool「失败一律 2」的约定。
		fmt.Fprintln(os.Stderr, buildinfo.StartupLine("data_service"))
		if *showVersion {
			return
		}
		os.Exit(routetoolcli.Run(context.Background(), append([]string{"-f", *configFile}, flag.Args()...), os.Stdout, os.Stderr))
	}
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。-migrate 形态同样先打,迁移 Job 日志可追溯。
	fmt.Println(buildinfo.StartupLine("data_service"))
	if *showVersion {
		return
	}

	var c config.Config
	conf.MustLoad(*configFile, &c)

	if entry == entryMigrate {
		os.Exit(runMigration(c))
	}
	var globalErr error
	c, globalErr = c.ResolveGlobalAuthority(os.Getenv)
	if globalErr != nil {
		fmt.Fprintf(os.Stderr, "data_service全球SQL装配拒绝: %v\n", globalErr)
		os.Exit(1)
	}

	// RouteStore 段(映射提交点,docs/design/player-route-mapping-durability-proposal.md §4、§11.2):
	// 任何一项非法都拒启,与 Mode 无关;拒绝而不纠正,理由同 PlayerNameConfig.Validate(Mode 拼错被悄悄当成
	// redis,运维以为切了提交点而实际没切)。-migrate 不读它,所以放在那之后。
	c.RouteStore = c.RouteStore.Normalize()
	if err := c.RouteStore.Validate(); err != nil {
		logx.Errorf("[startup] invalid RouteStore config; refusing to start: %v", err)
		fmt.Fprintf(os.Stderr, "data_service refuses to start: invalid RouteStore config: %v\n", err)
		logx.Close()
		os.Exit(1)
	}
	if c.RouteStore.DBFirst() {
		logx.Infof("[startup] RouteStore.Mode=db_first: fence_shards=%d max_open_conn=%d max_idle_conn=%d publish_attempts=%d "+
			"waitaof_numlocal=%d waitaof_numreplicas=%d waitaof_timeout_ms=%d",
			c.RouteStore.FenceShards, c.RouteStore.MaxOpenConn, c.RouteStore.MaxIdleConn, c.RouteStore.PublishAttempts,
			c.RouteStore.NumLocal(), c.RouteStore.NumReplicas(), c.RouteStore.WaitAOFTimeoutMs)
		// go-zero 的 logx 没有 WARN 级别(同 rollback_logic.go 的取舍),取 ERROR:这条是运维必须看见的前提,淹在 Info 里等于没有。
		logx.Errorf("[startup] WARN %s", routeStoreDBFirstWarning)
	}

	// Kafka 段 player_snapshot 消费者的批量 / 并发旋钮与 lag 探针(G2a):越界一律拒启,拒绝而不纠正(同 RouteStore)。
	// 与 Brokers 是否为空无关:一份"开 Kafka 才炸"的配置不该等到开的那一刻才被发现。-migrate 在它之前已返回,不受影响。
	c.Kafka = c.Kafka.Normalize()
	if err := c.Kafka.ValidateConsumers(c.SnapshotMySQL.MaxOpenConn); err != nil {
		logx.Errorf("[startup] invalid Kafka consumer config; refusing to start: %v", err)
		fmt.Fprintf(os.Stderr, "data_service refuses to start: invalid Kafka consumer config: %v\n", err)
		logx.Close()
		os.Exit(1)
	}

	if err := runDataService(c); err != nil {
		fmt.Fprintf(os.Stderr, "data_service: %v\n", err)
		_ = logx.Close()
		os.Exit(1)
	}
}

func runDataService(c config.Config) (runErr error) {
	runtime, root, relay, err := serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
	if err != nil {
		return err
	}
	evidenceDir := *maintenanceControlDir
	if runtime != nil {
		evidenceDir = runtime.Directory
	}
	if c.RouteStore.ReconcileEnabled() {
		if err := authoritysql.CheckPrivateControlDirectory(evidenceDir); err != nil {
			return fmt.Errorf("路由危险原件目录不可用: %w", err)
		}
	}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	controlCtx, cancelControl := context.WithCancel(context.Background())
	defer cancelControl()
	dangerNotice := make(chan error, 1)
	var dangerOnce sync.Once
	var dangerErr error
	var svcCtx *svc.ServiceContext
	var kafkaOwner *dskafka.ConsumerOwner
	onDanger := func(ctx context.Context, observation *store.RouteEmergencyError) error {
		root.CloseAdmission()
		if svcCtx != nil {
			svcCtx.CloseRootAndRelayIngress()
		}
		if kafkaOwner != nil {
			kafkaOwner.BeginDrain()
		}
		dangerOnce.Do(func() {
			root.Hold()
			relay.Hold()
			if observation == nil || svcCtx == nil {
				dangerErr = drainproof.ErrHold
			} else {
				record := struct {
					Schema                string
					WriterKey, InstanceID []byte
					Generation            string
					Observation           *store.RouteEmergencyError
				}{
					"DATA_SERVICE_ROUTE_DANGER_OBSERVATION_V1", svcCtx.Writer.WriterKey(), svcCtx.Writer.InstanceID(), svcCtx.Writer.Generation(), observation}
				raw, encodeErr := json.Marshal(record)
				dangerErr = encodeErr
				if encodeErr == nil {
					path := filepath.Join(evidenceDir, "route-danger-"+hex.EncodeToString(svcCtx.Writer.InstanceID())+".observation.json")
					dangerErr = authoritysql.WritePrivateControlFile(path, raw)
				}
				dangerErr = errors.Join(observation, dangerErr)
			}
			dangerNotice <- dangerErr
		})
		return dangerErr
	}
	svcCtx = svc.NewServiceContextWithAdmission(c, root, relay, onDanger)
	// 启动自检仍 fail-closed；尚未启动任何 worker 时可安全关闭三个空门禁后回收。
	cleanupUnstarted := func() error {
		svcCtx.CloseRootAndRelayIngress()
		relay.CloseAdmission()
		return svcCtx.CloseAndReport()
	}
	if missing := svcCtx.MissingStores(); c.Store.Required && len(missing) > 0 {
		return errors.Join(fmt.Errorf("Store.Required=true，依赖未就绪: %v", missing), cleanupUnstarted())
	}
	if err := svcCtx.RouteReadGateUnassembled(); err != nil {
		return errors.Join(err, cleanupUnstarted())
	}

	kafkaOwner, err = dskafka.NewConsumerOwner(relay, svcCtx.RelayIngress)
	if err != nil {
		return errors.Join(err, cleanupUnstarted())
	}
	kafkaStart := startKafkaConsumers(context.Background(), c.Kafka, svcCtx, kafkaOwner)
	var sourceMu sync.Mutex
	sourceClosed := false
	sourceMerged := false
	routeStarted := false
	var stopRoute func(context.Context) error
	startRoute := func() {
		sourceMu.Lock()
		defer sourceMu.Unlock()
		if routeStarted || sourceClosed || root.Closed() {
			return
		}
		stopRoute = startRouteReconciler(svcCtx)
		routeStarted = true
	}
	// prepare-only boot 不提前启动会发起业务写的定时 owner；正式 Open 后再启动。
	if runtime == nil || runtime.Request.RunPhase != databasepb.SqlWriterDrainRun_SQL_RELEASED {
		startRoute()
	}
	closeSource := func(ctx context.Context) error {
		sourceMu.Lock()
		defer sourceMu.Unlock()
		if sourceClosed {
			return nil
		}
		root.CloseAdmission()
		var result error
		if stopRoute != nil {
			if err := stopRoute(ctx); err != nil {
				root.Hold()
				result = errors.Join(result, err)
			}
		}
		result = errors.Join(result, root.Wait(ctx))
		if result != nil {
			return result
		}
		if svcCtx.RouteReconciler != nil && routeStarted && !sourceMerged {
			digest, err := svcCtx.RouteReconciler.SettledDigest(ctx)
			if err != nil {
				return err
			}
			if err = root.MergeClosedSettled(digest); err != nil {
				return err
			}
			sourceMerged = true
		}
		sourceClosed = true
		return nil
	}
	var relayMu sync.Mutex
	relayClosed := false
	var stopRPC func(context.Context) error
	var controlDone chan struct{}
	stopAuxiliary := func(context.Context) error { return nil }
	closeRelay := func(ctx context.Context) error {
		relayMu.Lock()
		defer relayMu.Unlock()
		if relayClosed {
			return svcCtx.CloseAndReport()
		}
		result := closeSource(ctx)
		svcCtx.RelayIngress.CloseAdmission()
		kafkaOwner.BeginDrain()
		digest, err := kafkaStart.StopAndReport(ctx)
		result = errors.Join(result, err)
		result = errors.Join(result, svcCtx.RelayIngress.Wait(ctx))
		result = errors.Join(result, relay.WaitIdle(ctx))
		result = errors.Join(result, stopAuxiliary(ctx))
		if result != nil {
			relay.Hold()
			return errors.Join(drainproof.ErrHold, result)
		}
		if err = relay.MergeSettled(digest); err != nil {
			return err
		}
		relay.CloseAdmission()
		if err = relay.Wait(ctx); err != nil {
			return err
		}
		if err = svcCtx.CloseAndReport(); err != nil {
			return err
		}
		relayClosed = true
		return nil
	}

	finalized := false
	var finalErr error
	finishRuntime := func(ctx context.Context) error {
		if finalized {
			return finalErr
		}
		finalized = true
		root.CloseAdmission()
		svcCtx.RelayIngress.CloseAdmission()
		kafkaOwner.BeginDrain()
		cancelControl()
		if controlDone != nil {
			select {
			case <-controlDone:
			case <-ctx.Done():
				finalErr = errors.Join(drainproof.ErrHold, ctx.Err())
				return finalErr
			}
		}
		finalErr = closeRelay(ctx)
		if stopRPC != nil {
			finalErr = errors.Join(finalErr, stopRPC(ctx))
		}
		return finalErr
	}
	defer func() {
		if !finalized {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			runErr = errors.Join(runErr, finishRuntime(ctx))
			cancel()
		}
	}()

	// 连接池指标(mmorpg_sql_pool_*{sql_client="data_service"},G2b):一个 store 一个独立池,只报装配成功的,
	// 每次抓取时现取 sql.DBStats。player_route 只在 db_first 时声明(redis 模式不装配 route store)。
	// 不能放进 NewServiceContext:单测会多次构造 ServiceContext,默认注册表重复注册会报错。注册失败只记日志。
	metrics.RegisterSQLPools(svcCtx.SQLPoolSamples, c.RouteStore.DBFirst())

	// HTTP 导出仅观察指标与并发安全的 DB.Stats，不派生业务写；不作为资产发行者 closure。
	// 空地址保持禁用。
	metrics.Start(c.MetricsListenAddr)
	// 回档帮会闸的 27 条告警序列启动即预置为 0(docs/design/guild-phase2/07-rollback-fail-closed.md §7.9.1):
	// 放行回档 / 写后分歧一辈子可能只发生一次,序列不预先存在的话 `increase(...) > 0` 恰好漏掉那一次。
	metrics.PrimeRollbackGuildCheck()
	// 映射提交点的两组序列(route_register_total / route_publish_total)只在 db_first 时有数,同理预置为 0。
	if c.RouteStore.DBFirst() {
		metrics.PrimeRoute()
	}

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
	etcdCli := newKillSwitchEtcdClient(c.Etcd)
	// watch 只更新本地拒绝规则，不发行资产、不使用 SQL，也不能重新打开生命周期门禁。
	ks.Start(ksCtx, etcdCli)
	var rpcServer *grpc.Server
	var rpcServerMu sync.Mutex
	serverReady := make(chan struct{})

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		rpcServerMu.Lock()
		rpcServer = grpcServer
		rpcServerMu.Unlock()
		close(serverReady)
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
	relayMethods := dataServiceRelayMethods()
	s.AddUnaryInterceptors(serverbase.PublicationAdmission(root, relay, relayMethods, svcCtx.RelayIngress))
	s.AddUnaryInterceptors(holdDataServiceFaults(root, relay, relayMethods))
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
	//   ErrCodeRedis(1)          —— 玩家数据 Redis 读写失败(data_logic.go 多处),也含玩家数据路由失败(映射缺席 / 映射库
	//                               故障或熔断 / 回源失败 / zone 未配集群;logic.clientForPlayer、写门禁与 snapshot_logic.go 第 3 步)。
	//                               刻意不另分码:4 不在本集合里,改 4 会让路由故障哑掉。原因只在 `[DataRoute]` 日志里,本拦截器只带 method/code
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
	// (24 ErrCodeMergeFenceMissing 自 2026-10-12 RemapHomeZoneForMerge 停用起不再产生,号不复用。)
	// 22 自 2026-10-19 写门禁起也以 in-band error_code 出现在 SavePlayerData / SetPlayerField / DeletePlayerData 与 Rollback*
	// 的响应里,这层拦截器能看到。仍刻意不进集合:那是围栏在正常工作(规则拒绝,口径同 RegisterPlayerZone 的拒绝)。
	// 可见性来自 data_service_player_lock_total{outcome="merge_fenced"} 与 `[PlayerWriteLock]` 的 ERROR 日志。
	// 映射持久化的 ErrCodeRouteStoreError(30)同理:只出现在 gRPC status message 里(Unavailable),这层看不到。
	// 产码点有两类(2026-10-19 更正:此前写的「只用于 RegisterPlayerZone」已不成立):
	//   - db_first 下 RegisterPlayerZone 的库这一段故障 —— 可见性来自 data_service_route_register_total{outcome="store_error"}
	//     与 `[RegisterPlayerZone][db_first]` 的 ERROR 日志;
	//   - 第 3 批读方门禁(ReadRequiresReady=true)下 GetPlayerHomeZone / BatchGetPlayerHomeZone 缓存不可信、回源全局库又失败
	//     (routing.ErrRouteReadUnavailable)—— 可见性来自 data_service_route_read_total{result="sql_error"}。
	// in-band 入口(Load / Save / SetPlayerField 等)的回源失败折成 ErrCodeRedis(1),不出 30;原因在 `[DataRoute]` 日志里。
	// 回档帮会闸三个码(27..29,docs/design/guild-phase2/07-rollback-fail-closed.md §7.3.3)里算故障的两个:
	//   ErrCodeRollbackGuildCheckFailed(28)       —— 帮会检查没做成(guild 未配 / 不可达 / 超时 / 预算耗尽…),
	//                                                此时**所有**回档都被拒,不告警就等于回档工具悄悄失灵
	//   ErrCodeRollbackGuildDivergedAfterWrite(29) —— 数据已写、写后复查发现新分歧或复查失败,紧急人工
	// ErrCodeRollbackGuildDivergence(27) 刻意不算:那是闸在正常工作(规则拒绝,口径同 PlayerOnline)。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{
		ErrorCodeClassifier: dataServiceFaultClassifier(),
	}))

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
	registrar := startNodeRegistration(c, regCli)
	stopAuxiliary = func(ctx context.Context) error {
		ksCancel()
		result := registrar.closeAndReport(ctx)
		if result != nil {
			return result
		}
		if etcdCli != nil {
			result = errors.Join(result, etcdCli.Close())
		}
		result = errors.Join(result, regCli.Close())
		return result
	}

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
	fmt.Printf("  route:       RouteStore.Mode=%s\n", c.RouteStore.Mode)
	if c.RouteStore.DBFirst() {
		fmt.Printf("  WARN:        %s\n", routeStoreDBFirstWarning)
	}
	fmt.Printf("  reconcile:   %s\n", routeReconcileBanner(c.RouteStore, svcCtx.RouteReconciler))
	fmt.Printf("  read gate:   RouteStore.ReadRequiresReady=%t\n", c.RouteStore.ReadRequiresReady)
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:     %s/metrics\n", c.MetricsListenAddr)
	}
	if len(c.Kafka.Brokers) > 0 {
		// 打**有效** topic 名(带代号后缀):排障时第一件要核对的就是它与 C++ 生产者一致。
		fmt.Printf("  kafka:       %v (%s, %s)\n", c.Kafka.Brokers,
			c.Kafka.EffectiveTransactionLogTopic(), c.Kafka.EffectiveSnapshotTopic())
	}
	fmt.Println("=============================================================")
	serveDone := make(chan struct{})
	serveResult := make(chan error, 1)
	go func() {
		var result error
		defer func() {
			if recovered := recover(); recovered != nil {
				result = fmt.Errorf("data_service gRPC 启动 panic: %v", recovered)
			}
			serveResult <- result
			close(serveDone)
		}()
		s.Start()
	}()
	stopRPC = func(ctx context.Context) error {
		rpcServerMu.Lock()
		server := rpcServer
		rpcServerMu.Unlock()
		if server == nil {
			return drainproof.ErrHold
		}
		done := make(chan struct{})
		go func() { server.GracefulStop(); proc.Shutdown(); <-serveDone; close(done) }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		}
	}
	select {
	case <-serverReady:
	case err := <-serveResult:
		return errors.Join(err, drainproof.ErrHold)
	case <-signalCtx.Done():
		return drainproof.ErrHold
	}
	controlDone = make(chan struct{})
	controlResult := make(chan error, 1)
	go func() {
		defer close(controlDone)
		result := serverbase.DriveDrainRuntime(controlCtx, runtime, *maintenanceControlDir, root, relay, svcCtx.BindDrainRuntime, closeSource, closeRelay,
			func(ctx context.Context) (*serverbase.PreparedPublication, error) {
				bound := svcCtx.DrainRuntime
				if bound == nil {
					return nil, drainproof.ErrHold
				}
				digest, raw, err := kafkaStart.PreparedFrontier(ctx, bound)
				if err != nil {
					return nil, err
				}
				return &serverbase.PreparedPublication{Digest: digest, Raw: raw, WithReady: func(openCtx context.Context, commit func() ([]byte, error)) ([]byte, error) {
					return kafkaStart.WithReady(openCtx, raw, commit)
				}}, nil
			})
		if result == nil && runtime != nil && runtime.Request.RunPhase == databasepb.SqlWriterDrainRun_SQL_RELEASED {
			startRoute()
		}
		controlResult <- result
	}()
	shutdownRuntime := func(reason error) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return errors.Join(reason, finishRuntime(ctx))
	}
	for {
		select {
		case result := <-controlResult:
			controlResult = nil
			if result != nil {
				return shutdownRuntime(result)
			}
			if relay.Closed() {
				return shutdownRuntime(nil)
			}
		case err := <-dangerNotice:
			return shutdownRuntime(errors.Join(drainproof.ErrHold, err))
		case err := <-kafkaOwner.Errors():
			return shutdownRuntime(errors.Join(drainproof.ErrHold, err))
		case err := <-registrar.failed:
			return shutdownRuntime(err)
		case <-signalCtx.Done():
			return shutdownRuntime(nil)
		case err := <-serveResult:
			return shutdownRuntime(errors.Join(errors.New("gRPC 服务已退出"), err))
		}
	}
}

// runMigration 是 -migrate 的实现:不设超时(存量大表的 MODIFY COLUMN 会重建表,
// 时间由表决定),成功 0、失败 1、迁移锁被另一个实例持有 3(migrateExitLockBusy,可重试)。
// 输出走 stdout/stderr,方便部署脚本直接读退出码。
func runMigration(c config.Config) int {
	cfg := svc.MySQLConfigOf(c)
	tags := c.IdSegment.EffectiveBootstrapTags()
	fmt.Printf("data_service schema migration: %s/%s (id_segment bootstrap tags: %v)\n", cfg.Host, cfg.DBName, tags)
	// 号段行在生产只能由这条路径创建(IdSegment.AllowAutoSeed 生产恒 false),所以 -migrate
	// 必须带完整的 BootstrapTags;迁移顺带把 player / guild 的 max_id 抬到消费表最大号 +1
	// (同库能查到时),那条 Error 日志出现就意味着库曾被重置 / 从备份恢复,按运维手册处理。
	if err := store.MigrateSchema(context.Background(), cfg, store.MigrateOptions{BootstrapTags: tags}); err != nil {
		fmt.Fprintf(os.Stderr, "schema migration FAILED: %v\n", err)
		return migrateExitCode(err)
	}
	fmt.Printf("schema migration OK: transaction_log, player_snapshot, rollback_audit_log synced from proto; id_segment bootstrapped for %v (existing rows untouched, player/guild floors checked); route tables player_route, zone_merge_fence, zone_merged_into, route_meta, route_reconcile in place (route_meta / route_reconcile row id=1 seeded if absent)\n", tags)
	return 0
}

// routeStoreDBFirstWarning 是 RouteStore.Mode=db_first 时启动日志与横幅里的那条 WARN(规格 d 节,M4)。
// 第 2 批起 merge_zone 以 -route-source=db 库先写并在 zone_merge_fence 持围栏;仍要提醒的是:旧版 merge_zone 二进制直写 Redis、
// 不写库(对账 leader 看不见它写的东西),P4 起一律禁用;切换与回退顺序按提案 §13.4。
// 2026-10-20 补「围栏真源」一句:db 源下 merge:in_progress 只是镜像,运维不得再按 redis 源的旧流程手工 GET / DEL 它(runbook §15)。
const routeStoreDBFirstWarning = "RouteStore.Mode=db_first: merge_zone 写模式只许第 2 批新版二进制,且显式 -route-source=db " +
	"-route-db-dsn=<本全局库>(旧版直写 Redis、不写 zone_merge_fence,一律禁用);P4 解除冻结窗口之前不跑合服写模式;" +
	"合服围栏以 zone_merge_fence 为真源,merge:in_progress 只是镜像,不要手工 SET / DEL(merge-zone-runbook §15);" +
	"ArmReadyMarker 与读方开关按 proposal §13.4 切换,回退先关读方、再 Arm=false"

// startRouteReconciler 返回真实停止新拍并 join 原任务的接缝；不把 bool Wait 或租约 TTL 当成功。
func startRouteReconciler(svcCtx *svc.ServiceContext) func(context.Context) error {
	rec := svcCtx.RouteReconciler
	if rec == nil {
		return func(context.Context) error { return nil }
	}
	metrics.PrimeRouteReconcile(rec.ArmEnabled())
	stop := rec.StartWithReport(context.Background())
	logx.Infof("[startup] route reconcile leader started: leader_id=%s arm_ready_marker=%t", rec.LeaderID(), rec.ArmEnabled())
	return stop
}

// routeReconcileBanner 是启动横幅里对账 leader 那一行:装配了就是 on + 标识 + Arm 取值;没装配写明原因。
// rec 取具体指针类型而不是接口:nil 的 *Reconciler 转成接口就不再等于 nil(「非 nil 接口包着 nil 指针」)。
func routeReconcileBanner(rs config.RouteStoreConfig, rec *routereconcile.Reconciler) string {
	switch {
	case !rs.DBFirst():
		return "off (redis mode)"
	case !rs.Reconcile.On():
		return "off (RouteStore.Reconcile.Enabled=false)"
	case rec == nil:
		return "NOT ASSEMBLED (see the [ServiceContext] errors above)"
	}
	return fmt.Sprintf("leader=on arm_ready=%t leader_id=%s", rec.ArmEnabled(), rec.LeaderID())
}

// migrateExitLockBusy 与 go/schemamigrate 的 ExitLockBusy 同值:迁移锁被另一个实例(另一个副本的启动路径,
// 或上一次还没结束的迁移 Job)持有。K8s 的迁移 Job 按这个码重试,而不是当成迁移本身失败。
const migrateExitLockBusy = 3

// migrateExitCode 把迁移错误映射成 -migrate 的退出码。
func migrateExitCode(err error) int {
	if errors.Is(err, store.ErrMigrateLockBusy) {
		return migrateExitLockBusy
	}
	return 1
}

// kafkaStartup 拥有首次启动与所有重试本身；P3 必须等 EnsureTopics 返回，不能只等已经创建的 consumer。
type kafkaStartup struct {
	owner      *dskafka.ConsumerOwner
	mu         sync.Mutex
	consumers  *dskafka.Consumers
	startErr   error
	configured bool
	cancel     context.CancelFunc
	done       chan struct{}
}

func startKafkaConsumers(ctx context.Context, kc config.KafkaConfig, svcCtx *svc.ServiceContext, owner *dskafka.ConsumerOwner) *kafkaStartup {
	retryCtx, cancel := context.WithCancel(ctx)
	out := &kafkaStartup{owner: owner, configured: len(kc.Brokers) > 0, cancel: cancel, done: make(chan struct{})}
	if !out.configured {
		close(out.done)
		return out
	}
	if svcCtx.TxLogStore == nil || svcCtx.SnapshotStore == nil {
		out.startErr = errors.New("Kafka 未装配：transaction_log / snapshot store 缺失")
		close(out.done)
		return out
	}
	for _, name := range []string{dskafka.ConsumerTxLog, dskafka.ConsumerSnapshot} {
		metrics.SetKafkaConsumerUp(name, false)
		metrics.PrimeKafkaConsumer(name)
	}
	txTopic, snapshotTopic := kc.EffectiveTransactionLogTopic(), kc.EffectiveSnapshotTopic()
	start := func() error {
		cs, err := dskafka.Start(ctx, dskafka.StartConfig{
			Owner:              owner,
			Brokers:            kc.Brokers,
			TxLogTopic:         txTopic,
			TxLogPartitions:    kc.TransactionLogPartitions,
			TxLogGroup:         kc.TransactionLogConsumerGroup,
			SnapshotTopic:      snapshotTopic,
			SnapshotPartitions: kc.SnapshotPartitions,
			SnapshotGroup:      kc.SnapshotConsumerGroup,
			RetentionMs:        kc.RetentionMs,
			// kc 已在 main 里 Normalize + ValidateConsumers 过(越界拒启),这里只做单位换算。
			Snapshot: dskafka.SnapshotConsumerConfig{
				BatchMaxRows:       kc.SnapshotBatchMaxRows,
				BatchMaxBytes:      kc.SnapshotBatchMaxBytes,
				FlushInterval:      time.Duration(kc.SnapshotFlushIntervalMs) * time.Millisecond,
				MaxInflightFlushes: kc.SnapshotMaxInflightFlushes,
			},
			LagProbeInterval: time.Duration(kc.LagProbeIntervalMs) * time.Millisecond,
		}, svcCtx.TxLogStore, svcCtx.SnapshotStore)
		if err != nil {
			return err
		}
		out.mu.Lock()
		out.consumers = cs
		out.mu.Unlock()
		return nil
	}
	safego.Go("data_service.kafka.start", func() {
		defer close(out.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				err := errors.New("Kafka 启动 owner panic")
				out.mu.Lock()
				out.startErr = err
				out.mu.Unlock()
				owner.RecordUnknown(err)
				panic(recovered)
			}
		}()
		for attempt := 1; retryCtx.Err() == nil && !owner.Draining(); attempt++ {
			err := start()
			out.mu.Lock()
			out.startErr = err
			out.mu.Unlock()
			if err == nil {
				return
			}
			logx.Errorf("[kafka] 启动失败 attempt=%d，%s 后重试: %v", attempt, kafkaStartRetryInterval, err)
			timer := time.NewTimer(kafkaStartRetryInterval)
			select {
			case <-retryCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
	return out
}
func (s *kafkaStartup) StopAndReport(ctx context.Context) ([]byte, error) {
	s.owner.BeginDrain()
	s.cancel()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
	}
	s.mu.Lock()
	consumers, startErr := s.consumers, s.startErr
	s.mu.Unlock()
	if consumers != nil {
		return consumers.StopAndReport(ctx)
	}
	if s.configured {
		return nil, errors.Join(drainproof.ErrHold, startErr)
	}
	return s.owner.SettledDigest(ctx)
}
func (s *kafkaStartup) PreparedFrontier(ctx context.Context, runtime *drainproof.Runtime) ([]byte, []byte, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-s.done:
	}
	s.mu.Lock()
	consumers, startErr := s.consumers, s.startErr
	s.mu.Unlock()
	if consumers == nil {
		return nil, nil, errors.Join(drainproof.ErrHold, startErr)
	}
	return consumers.PreparedFrontier(ctx, runtime, "SOURCE")
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
		logx.Info("[killswitch] data_service etcd Hosts not configured; kill switch inactive (all requests allowed)")
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
		logx.Errorf("[killswitch] data_service failed to create etcd client; kill switch inactive (all requests allowed): %v", err)
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

	mu        sync.Mutex
	nr        *noderegistry.NodeRegistration
	closed    bool
	cancel    context.CancelFunc
	done      chan struct{}
	failed    chan error
	startErr  error
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func (r *nodeRegistrar) set(nr *noderegistry.NodeRegistration) {
	r.mu.Lock()
	r.nr = nr
	r.mu.Unlock()
}
func (r *nodeRegistrar) fail(err error) {
	r.mu.Lock()
	if r.startErr == nil {
		r.startErr = err
	}
	r.mu.Unlock()
	select {
	case r.failed <- err:
	default:
	}
}
func (r *nodeRegistrar) close() {
	if err := r.closeAndReport(context.Background()); err != nil {
		logx.Errorf("节点注册关闭 HOLD: %v", err)
	}
}

// 注册 goroutine 自身必须 join。内部 keepalive 只维护发现租约，不能开门或发行资产。
func (r *nodeRegistrar) closeAndReport(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.closeOnce.Do(func() {
		r.closeDone = make(chan struct{})
		go func() {
			defer close(r.closeDone)
			if r.done != nil {
				<-r.done
			}
			r.mu.Lock()
			nr, startErr := r.nr, r.startErr
			r.mu.Unlock()
			r.closeErr = startErr
			if nr != nil {
				if owner, ok := any(nr).(interface{ CloseAndReport(context.Context) error }); ok {
					r.closeErr = errors.Join(r.closeErr, owner.CloseAndReport(context.Background()))
				} else {
					// Close 内注销错误保留原日志；控制面晚完成不能重开已永久关闭的业务门禁。
					nr.Close()
				}
			}
		}()
	})
	select {
	case <-ctx.Done():
		return errors.Join(drainproof.ErrHold, ctx.Err())
	case <-r.closeDone:
		return r.closeErr
	}
}

// startNodeRegistration 起一条 goroutine:等本进程的 gRPC 端口 accept,再按 C++ 约定注册
// DataServiceNodeService 的 NodeInfo 并开始 keepalive。失败交还 main 做真实收尾并拒绝启动。
func startNodeRegistration(c config.Config, cli *clientv3.Client) *nodeRegistrar {
	listenHost, port := parseListenOn(c.ListenOn)
	ctx, cancel := context.WithCancel(context.Background())
	r := &nodeRegistrar{host: advertisedHost(listenHost), port: port, cancel: cancel, done: make(chan struct{}), failed: make(chan error, 1)}

	// 拨号地址与对外地址是两回事:0.0.0.0 监听时本机只能拨 loopback,而 NodeInfo 里
	// 必须写别的 Pod / 进程连得上的 IP。
	dialHost := listenHost
	if isUnspecifiedHost(dialHost) {
		dialHost = "127.0.0.1"
	}
	dialAddr := net.JoinHostPort(dialHost, strconv.FormatUint(uint64(port), 10))

	// 不把注册失败吞成健康；panic 与显式错误都交给 main 结束本次服务。
	go func() {
		defer close(r.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				r.fail(fmt.Errorf("节点注册 owner panic: %v", recovered))
			}
		}()
		if err := waitForListeningContext(ctx, dialAddr, nodeRegisterListenWait); err != nil {
			if ctx.Err() == nil {
				r.fail(fmt.Errorf("data_service 监听端口未就绪，不能注册: %w", err))
			}
			return
		}
		nr, err := noderegistry.Register(cli, uint32(base.ENodeType_DataServiceNodeService), c.ZoneId, r.host, port, c.LeaseTTL)
		if err != nil {
			r.fail(fmt.Errorf("DataService 节点注册失败: %w", err))
			return
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
	return waitForListeningContext(context.Background(), addr, timeout)
}
func waitForListeningContext(parent context.Context, addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	dialer := net.Dialer{Timeout: nodeRegisterListenPoll * 5}
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			return conn.Close()
		}
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		timer := time.NewTimer(nodeRegisterListenPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
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

func dataServiceRelayMethods() map[string]bool {
	return map[string]bool{
		"/data_service.DataService/ReadAccountDirectory":          true,
		"/data_service.DataService/CreateAccountRole":             true,
		"/data_service.DataService/LookupAccountRole":             true,
		"/data_service.DataService/RepairAccountRole":             true,
		"/data_service.DataService/RemoveAccountRoles":            true,
		"/data_service.DataService/BeginAccountRoleTrade":         true,
		"/data_service.DataService/ConfirmAccountRoleTradeFreeze": true,
		"/data_service.DataService/CancelAccountRoleTrade":        true,
		"/data_service.DataService/TransferAccountRole":           true,
		"/data_service.DataService/RegisterRoleAdmission":         true,
		"/data_service.DataService/CloseRoleAdmissions":           true,
		"/data_service.DataService/AckRoleTradeDrain":             true,
		"/data_service.DataService/LoadPlayerData":                true,
		"/data_service.DataService/SavePlayerData":                true,
		"/data_service.DataService/GetPlayerField":                true,
		"/data_service.DataService/SetPlayerField":                true,
		"/data_service.DataService/RegisterPlayerZone":            true,
		"/data_service.DataService/GetPlayerHomeZone":             true,
		"/data_service.DataService/BatchGetPlayerHomeZone":        true,
		"/data_service.DataService/DeletePlayerData":              true,
		"/data_service.DataService/ListPlayerSnapshots":           true,
		"/data_service.DataService/GetPlayerSnapshotDiff":         true,
		"/data_service.DataService/QueryTransactionLog":           true,
		"/data_service.DataService/AllocateIdSegment":             true,
		"/data_service.DataService/ReservePlayerName":             true,
		"/data_service.DataService/ReleasePlayerName":             true,
		"/data_service.DataService/BatchGetPlayerName":            true,
		"/data_service.DataService/GetPlayerAssetOpLedger":        true,
		"/data_service.DataService/GetPlayerInboxLedger":          true,
	}
}
func dataServiceFaultClassifier() serverbase.Classifier {
	return serverbase.FaultCodeSet(
		constants.ErrCodeRedis,
		constants.ErrCodeSnapshotDBError,
		constants.ErrCodeRollbackFailed,
		constants.ErrCodeIdSegmentDBError,
		constants.ErrCodeIdSegmentExhausted,
		constants.ErrCodeIdSegmentUnknownTag,
		constants.ErrCodeRollbackGuildCheckFailed,
		constants.ErrCodeRollbackGuildDivergedAfterWrite,
	)
}

// DS 的 SQL/Redis 不定结果常以 error_code 返回；不能将 Go error=nil 当作 SOURCE/RELAY 已成功。
func holdDataServiceFaults(root, relay *drainproof.Session, methods map[string]bool) grpc.UnaryServerInterceptor {
	classify := serverbase.Options{ErrorCodeClassifier: dataServiceFaultClassifier()}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		response, err := next(ctx, req)
		if err == nil {
			verdict := classify.Classify(serverbase.ExtractBizCode(response))
			if verdict == serverbase.VerdictFault || verdict == serverbase.VerdictUnknown {
				gate := root
				if methods[info.FullMethod] {
					gate = relay
				}
				gate.Hold()
			}
		}
		return response, err
	}
}

func (s *kafkaStartup) WithReady(ctx context.Context, raw []byte, commit func() ([]byte, error)) ([]byte, error) {
	s.mu.Lock()
	consumers := s.consumers
	s.mu.Unlock()
	if consumers == nil {
		return nil, drainproof.ErrHold
	}
	return consumers.WithReady(ctx, raw, commit)
}
