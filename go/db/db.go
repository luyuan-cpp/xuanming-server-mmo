package main

import (
	"bytes"
	"context"
	"db/internal/config"
	dbdrain "db/internal/drain"
	"db/internal/kafka"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	"db/internal/saveoutbox"
	server "db/internal/server/db"
	"db/internal/svc"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	db_grpc "proto/db"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/drainproof"
	"shared/grpcstats"
	"shared/kafkautil"
	"shared/killswitch"
	"shared/placement"
	"shared/serverbase"
	"strings"
	"syscall"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	databasepb "proto/common/database"
)

var configFile = flag.String("f", "etc/db.yaml", "the config file")

var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "正常boot受控排空私有目录；不靠重启冒旧boot")
var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式typed维护runtime Receipt路径；本模式不serve/EnsureTopics/bootstrap")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有stop/report目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime Receipt原文件SHA256")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

// killSwitchEtcdDialTimeout 与 scene_manager 建 etcd 客户端时的取值一致(5s)。
// 只影响 killswitch 自己的 watch 连接,拨不通也只是退回全放行。
const killSwitchEtcdDialTimeout = 5 * time.Second

// placementCapabilityTimeout 是启动期首次写能力标记的预算:一条 SET,只为不让 Redis 卡住拖住启动。
// 之后的续写由 kafka.KeepPlacementCapability 负责,预算见那里。
const placementCapabilityTimeout = 5 * time.Second

// saveProtocolGateTimeout 是启动期存盘协议世代门禁的预算:一条 SET 或一次 GET 加 64 个 ZCARD 的管道。
const saveProtocolGateTimeout = 5 * time.Second

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("db"))
	if *showVersion {
		return
	}

	// Load config
	conf.MustLoad(*configFile, &config.AppConfig)
	// 回填 optional 段的默认值(迁移超时、大字段闸上限)。见 config.Normalize 注释。
	config.AppConfig.Normalize()

	// 重试路由(docs/design/db-retry-owner-routing.md):配置键 ServerConfig.Kafka.RetryRouting,环境变量 DB_RETRY_ROUTING
	// 优先(只用于单 zone 金丝雀)。拼错一律拒启、不静默回落;生效值回写进配置,消费者按它装配。
	// 启动日志 "retry routing: mode=… source=…" 与指标 db_retry_routing_mode 是切换 / 回滚时的核对依据。
	retryRouting, retryRoutingSource, err := config.ResolveRetryRouting(
		config.AppConfig.ServerConfig.Kafka.RetryRouting, os.Getenv(config.RetryRoutingEnvVar))
	if err != nil {
		panic(fmt.Sprintf("retry routing rejected: %v", err))
	}
	config.AppConfig.ServerConfig.Kafka.RetryRouting = retryRouting
	// 重试路由与入队落盘确认(WAITAOF)的取值校验。conf.Load 在 Normalize 之前已自动调过一次(没写的键放行),
	// 这里按回填默认值、合并环境变量之后的生效值再判一次,非法即拒启。
	if err := config.AppConfig.Validate(); err != nil {
		panic(fmt.Sprintf("config rejected: %v", err))
	}
	logx.Infof("retry routing: mode=%s source=%s", retryRouting, retryRoutingSource)

	// Derive zone-specific Kafka topic and MySQL database name from ZoneId
	if config.AppConfig.ZoneId == 0 {
		panic("ZoneId must be set in config (> 0)")
	}
	config.AppConfig.ServerConfig.Kafka.Topic = config.DbTaskTopicForGeneration(
		config.AppConfig.ZoneId, config.AppConfig.ServerConfig.Kafka.TopicGeneration)
	config.AppConfig.ServerConfig.Database.DBName = config.ZoneDBName(config.AppConfig.ZoneId)

	// Ensure the db_task topic exists with the desired partition count BEFORE
	// the sarama consumer joins. If the topic doesn't exist when sarama
	// connects, the broker auto-creates it with num.partitions=1 and sarama
	// permanently assigns this consumer partition 0 only — any later
	// partition expansion strands the other partitions with no consumer until
	// the next process restart. Round 10 of the 45k stress (2026-05-31) caught this with
	// ~19.5k preload tasks stuck on partitions 1..9 while only ~764 on
	// partition 0 drained, capping SceneManager.EnterScene throughput at
	// ~4/s and timing out 78% of robots on "scene ready".
	var maintenanceRuntime *drainproof.Runtime
	if *maintenancePlan != "" {
		maintenanceRuntime, err = drainproof.LoadRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
		if err != nil {
			panic(fmt.Sprintf("维护runtime原件拒绝: %v", err))
		}
	} else if *maintenanceDir != "" || *maintenanceSHA != "" {
		panic("维护参数必须完整提供")
	}
	if maintenanceRuntime == nil {
		if err := kafkautil.EnsureTopics(
			config.AppConfig.ServerConfig.Kafka.Brokers,
			[]kafkautil.TopicSpec{{
				Name:           config.AppConfig.ServerConfig.Kafka.Topic,
				Partitions:     config.AppConfig.ServerConfig.Kafka.PartitionCnt,
				RetentionMs:    config.AppConfig.ServerConfig.Kafka.RetentionMs,
				RetentionBytes: config.AppConfig.ServerConfig.Kafka.RetentionBytes,
			}},
		); err != nil {
			panic(fmt.Sprintf("Kafka db-task partition contract rejected: %v", err))
		}

	}

	ctx := svc.NewServiceContext()

	// Initialize database BEFORE the Kafka consumer starts pulling tasks:
	// 消费任务落到 worker 就要从落点库注册表拿库(种子就是 proto_sql.DB),若此时还没建好,
	// 启动窗口内的每条消息都会失败重排(offset 不提交,重启可恢复,但整个窗口在
	// 空转打错误日志)。先建库连接再放消费者进来,窗口从机制上不存在。
	//
	// InitDB 默认**不跑任何 DDL**,并且会断言实际连上的 DATABASE() 落在外部
	// 注入的白名单里 —— 不在就拒启,而不是静默建库把玩家数据写进去。
	// 建表/补列由部署阶段的 `go run ./cmd/migrate -command up` 负责,
	// 运维步骤见 go/db/README.md。
	if err := proto_sql.InitDB(); err != nil {
		panic(fmt.Sprintf("database init rejected: %v", err))
	}

	// 落点库注册表(docs/design/player-storage-placement.md §6.1):本 zone 库登记为落点 ZoneId,
	// 其余落点库在第一次被指向时按需打开(只 Ping + 只读 schema 闸,从不建库)。
	// 必须在 InitDB 之后(拿本 zone 库做种子)、消费者启动之前(第一条任务就要选库)。
	stores, err := proto_sql.NewStoreRegistryFromConfig()
	if err != nil {
		panic(fmt.Sprintf("placement store registry rejected: %v", err))
	}
	defer func() {
		if err := stores.Close(); err != nil {
			logx.Errorf("DB物理池关闭失败，不能据此宣称UNKNOWN撤销: %v", err)
		}
	}()
	// 连接池指标(mmorpg_sql_pool_*{sql_client="db"},G2b):按本 zone 库 / 额外 zone 家族库 / 非 zone 落点库三类聚合,
	// 每次抓取时现取 sql.DBStats。必须在 metrics.Start 之前注册;注册失败只记日志,不挡落库。
	metrics.RegisterSQLPools(stores.PoolSamples)
	// 库级冷却(L-04):db_store_cooldown_stores 在每次抓取时按注册表现算,同样必须在 metrics.Start 之前注册。
	metrics.RegisterStoreCooldown(stores.CooldownCounts)
	// go-redis 连接池(db_redis_pool_*{redis_client=main|placement}):每次抓取时现取 PoolStats。Placement.Redis 缺省时
	// PlacementRedis 就是 RedisClient 本身,按指针身份判断,同一对象只导出 main 一份(汇总脚本跨 redis_client 求和不翻倍)。
	metrics.RegisterRedisPools(ctx.RedisClient, ctx.PlacementRedis)

	// 装配大字段三档闸(必须在消费者启动之前:第一条消息落到 worker 就要过闸)
	kafka.InitBlobGuard(config.AppConfig.ServerConfig.BlobGuard)

	// 行内围栏模式:启动期设置一次、运行中不可改,必须在构造消费者之前(设计 db-write-fencing-mysql.md §4.7、§8)。
	fencingMode := config.AppConfig.ServerConfig.Fencing.EffectiveMode()
	if err := kafka.SetFencingMode(fencingMode); err != nil {
		panic(fmt.Sprintf("fencing mode rejected: %v", err))
	}
	kafka.SetFencingEpochOverride(config.AppConfig.ServerConfig.Fencing.EpochOverride)
	if fencingMode == config.FencingModeLegacy && config.AppConfig.ServerConfig.Fencing.EpochOverride {
		logx.Errorf("Fencing.EpochOverride ignored in legacy mode")
	}

	logx.Infof("player row fencing: mode=%s epoch_override=%v", fencingMode, config.AppConfig.ServerConfig.Fencing.EpochOverride && fencingMode == config.FencingModeDual)

	if maintenanceRuntime != nil {
		writerConfig, err := proto_sql.CurrentWriterConfig()
		if err != nil {
			panic(fmt.Sprintf("维护writer身份HOLD: %v", err))
		}
		if err = bindMaintenanceRuntime(maintenanceRuntime, writerConfig); err != nil {
			panic(fmt.Sprintf("维护同boot身份HOLD: %v", err))
		}
	}
	if maintenanceRuntime != nil && maintenanceRuntime.Request.RunPhase != databasepb.SqlWriterDrainRun_SQL_RELEASED {
		maintenanceCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := dbdrain.Run(maintenanceCtx, maintenanceRuntime, config.AppConfig, ctx.RedisClient, ctx.PlacementRedis, stores); err != nil {
			panic(fmt.Sprintf("维护排空HOLD: %v", err))
		}
		return
	}

	// ── 存盘协议世代门禁(data-overwrite-rollback-audit-20261018.md §2.2)──────────────────────────
	// 现行 scene / login 只说严格提交屏障,只有 dual 写两表 SQL ACK、跑补投。dual 在消费者与补投启动之前写 zone 级世代标记;
	// legacy 先确认本 zone 没有严格世代证据(标记或非空补投桶)才启动,运行中周期复查,发现即走正常停机并以非零码退出,
	// 由编排重启后停在本门禁。失败一律拒启(fail-closed):判不清世代时放 legacy 进来,就是「第二次登录起全部失败」。
	// exitCode 的 defer 必须先于下面所有 defer 注册,才能在消费者等全部停完之后最后执行。
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()
	protocolConflict := make(chan error, 1)
	if maintenanceRuntime == nil {
		gateCtx, gateCancel := context.WithTimeout(context.Background(), saveProtocolGateTimeout)
		if fencingMode == config.FencingModeDual {
			err = saveoutbox.AnnounceStrictGeneration(gateCtx, ctx.RedisClient, config.AppConfig.ZoneId)
		} else {
			err = saveoutbox.CheckLegacyAllowed(gateCtx, ctx.RedisClient, config.AppConfig.ZoneId)
		}
		gateCancel()
		if err != nil {
			panic(fmt.Sprintf("save protocol generation gate rejected startup (mode=%s): %v", fencingMode, err))
		}
		if fencingMode == config.FencingModeLegacy {
			// 复查协程在消费者停完之后才取消(defer 后进先出):停机排空期间仍续写 legacy 存活标记,dual 的补投继续等。
			watchCtx, stopWatch := context.WithCancel(context.Background())
			defer stopWatch()
			go saveoutbox.WatchLegacy(watchCtx, ctx.RedisClient, config.AppConfig.ZoneId, saveoutbox.LegacyWatchInterval, func(err error) {
				select {
				case protocolConflict <- err:
				default:
				}
			})
			logx.Infof("save protocol generation: mode=legacy zone=%d no strict evidence; rechecking every %s",
				config.AppConfig.ZoneId, saveoutbox.LegacyWatchInterval)
		} else {
			logx.Infof("save protocol generation: mode=dual %s=%s", saveoutbox.GenerationKey(config.AppConfig.ZoneId), saveoutbox.GenerationStrict)
		}

		// 玩家持久 schema 引导关闭标记(docs/design/mixed-version-persist-field-protection.md §6.2 C0):消费者启动前读一次,
		// 读不清就拒启;运行中周期复查,只会从开放变成关闭。复查协程随进程停机退出(defer 后进先出,晚于消费者停完)。
		schemaCtx, schemaCancel := context.WithTimeout(context.Background(), saveProtocolGateTimeout)
		err = kafka.LoadPersistBootstrapState(schemaCtx, ctx.RedisClient, config.AppConfig.ZoneId)
		schemaCancel()
		if err != nil {
			panic(fmt.Sprintf("persist schema bootstrap state rejected startup: %v", err))
		}
		schemaWatchCtx, stopSchemaWatch := context.WithCancel(context.Background())
		defer stopSchemaWatch()
		go kafka.WatchPersistBootstrap(schemaWatchCtx, ctx.RedisClient, config.AppConfig.ZoneId)

	} else if fencingMode != config.FencingModeDual {
		panic("维护新boot要求dual，不进行legacy/bootstrap切换")
	}

	// Initialize Kafka consumer
	kafkaConsumer, err := kafka.NewKeyOrderedKafkaConsumer(
		config.AppConfig,
		ctx.RedisClient,
		ctx.PlacementRedis,
		stores,
	)
	if err != nil {
		panic(fmt.Sprintf("failed to init Kafka consumer: %v", err))
	}
	defer kafkaConsumer.Stop()

	// Start Kafka consumer
	if err := kafkaConsumer.Start(); err != nil {
		panic(fmt.Sprintf("failed to start Kafka consumer: %v", err))
	}

	// 父快照Lua中保存的原始任务意图跨scene崩溃恢复；Kafka确认后仍等两表SQL双ACK才清理。
	// 装配使用可写主父缓存Redis，与只读OwnerEpochRedis区分；跨实例拓扑未支持时拒启。
	// 补投与消费同看本实例的读方就绪闸(kafkaConsumer.RouteGateOpen)：闸关着时整拍跳过、不计次(r2-rr#5)。
	saveReplay, err := saveoutbox.Start(context.Background(), config.AppConfig, ctx.RedisClient, ctx.PlacementRedis, stores, kafkaConsumer)
	if err != nil {
		panic(fmt.Sprintf("save outbox recovery rejected: %v", err))
	}
	defer saveReplay.Stop()

	// Start Prometheus /metrics endpoint (no-op when MetricsListenAddr empty).
	metrics.Start(config.AppConfig.MetricsListenAddr)

	// ── 能力标记(player-storage-placement.md §4.3)──────────────────────────
	// 合服 pin 模式与搬库工具动手前检查各 zone 的 db:capability:zone:{Z};没有标记 = 那个 zone 此刻没有新版
	// go/db 在按落点选库,工具拒绝执行。写在库注册表与消费者都已就绪之后:标记承诺的是
	// 「这个 zone 正在跑的 go/db 按落点选库」,早于这一刻写就是提前承诺。
	// 标记带 TTL、由心跳续写(见 kafka.KeepPlacementCapability):回退到旧版 / 进程退出后它在 TTL 内自然消失,
	// 不会替已经不在跑的新版作证。第 4 步之前回退本 zone 的 go/db 时,若等不及 TTL,在 mapping Redis DB 0 上
	// DEL 这把键再动工具。
	// 失败只打 ERROR 不拒启:缺标记的后果是工具拒绝动手(安全方向),为它拒启等于停掉整个 zone 的存盘。
	capabilityCtx, capabilityCancel := context.WithTimeout(context.Background(), placementCapabilityTimeout)
	if err := kafka.MarkPlacementCapability(capabilityCtx, ctx.PlacementRedis, config.AppConfig.ZoneId); err != nil {
		logx.Errorf("[placement] Failed to write capability marker (startup allowed; heartbeat will backfill when Redis recovers, merge pin / database move tools will reject this zone until then): key=%s err=%v",
			placement.CapabilityKey(config.AppConfig.ZoneId), err)
	} else {
		logx.Infof("[placement] capability marked: %s=%s ttl=%s refresh=%s",
			placement.CapabilityKey(config.AppConfig.ZoneId), placement.CapabilityRoutingV1,
			kafka.PlacementCapabilityTTL, kafka.PlacementCapabilityRefreshInterval)
	}
	capabilityCancel()
	// 心跳在进程退出时停(defer 先于 kafkaConsumer.Stop 执行):先停止承诺能力,再停消费者。
	// 不主动 DEL:同 zone 的其他副本可能仍在跑,交给 TTL 收尾。
	capabilityHeartbeatCtx, stopCapabilityHeartbeat := context.WithCancel(context.Background())
	defer stopCapabilityHeartbeat()
	go kafka.KeepPlacementCapability(capabilityHeartbeatCtx, ctx.PlacementRedis, config.AppConfig.ZoneId)

	// ── 热关停(killswitch)────────────────────────────────────────────
	// db 是玩家权威数据的写入方,真出事时最缺的是"秒级止血阀":往 etcd 前缀
	// /mmorpg/killswitch/ 下写一个 key 就能把某个方法立刻短路掉,而不必走一遍
	// 构建-发布-滚动更新。规则布局与优先级见 shared/killswitch 包注释。
	//
	// New 出来立刻可用(规则为空 = 全放行),Start 非阻塞、etcd 为 nil 也合法,
	// 所以整段接线不会让 db 的启动多出任何一个失败点。
	ks := killswitch.New(killswitch.Config{})
	ksCtx, ksCancel := context.WithCancel(context.Background())
	defer ksCancel()
	etcdCli := newKillSwitchEtcdClient(config.AppConfig.Etcd)
	if etcdCli != nil {
		defer etcdCli.Close()
	}
	ks.Start(ksCtx, etcdCli)

	controlCtx, stopControl := context.WithCancel(context.Background())
	defer stopControl()
	rootAdmission := drainproof.New()
	if maintenanceRuntime != nil {
		rootAdmission = drainproof.NewPrepared()
	}
	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		db_grpc.RegisterDbServer(grpcServer, server.NewDbServer(ctx))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 拦截器顺序是有意的(先加的在外层):
	//   grpcstats → killswitch → serverbase → handler
	// grpcstats 放最外层,被关停的请求也照样计入总量(否则一开闸就像"没人调用");
	// killswitch 命中即短路,handler 根本不会被调用;
	// serverbase 放最内层,只观测真正跑过 handler 的结果,不会把"被人为关停"
	// 误记成一次业务故障。
	s.AddUnaryInterceptors(func(callCtx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		done, err := rootAdmission.Enter()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "维护入口关闭")
		}
		defer done()
		return handler(callCtx, req)
	})
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	s.AddUnaryInterceptors(ks.UnaryServerInterceptor())
	// in-band 故障拦截器。
	//
	// 这里**刻意不传任何 Classifier**,依据是 proto 事实而不是照抄别的服务:
	// db 的 gRPC 面只有 db.Test 一个方法(见 proto/db/db.proto),它的
	// TestResponse 里既没有 `uint32 error_code`,也没有 `TipInfoMessage
	// error_message` —— 压根没有 in-band 业务码字段,serverbase 会判成
	// SourceNone 一律记成成功。传 Classifier 只会是自欺欺人的装饰。
	//
	// 那为什么还挂?两条真实收益:
	//   1. handler 返回 error 时记一条 transport_error + 耗时,与其他服务同口径;
	//   2. 以后 db 真加出带业务码的 RPC 时,这层已经在链上,不会再漏一遍
	//      "gRPC status 恒 OK、故障全部静默"的老坑。
	// db 真正的业务失败走的是 Kafka db_task 那条链,由 internal/metrics 的
	// db_task_result_total 统计,不在本拦截器的观测范围内。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{}))
	defer s.Stop()

	// 真正把 gRPC 服务跑起来。fc9377336(asynq→kafka)之后这里只剩 MustNewServer
	// 没有 Start:进程打印 "STARTED SUCCESSFULLY" 却什么端口都不监听,也不向 etcd
	// 注册 db.rpc。本地 compose 没探针所以一直没暴露;K8s(deploy/k8s/manifests/go-svc/db.yaml)
	// 的 grpc readiness/liveness 探 6000 端口永远超时,db Pod 永远 0/1 并被反复重启
	// (2026-09-03 kind 实跑)。go-zero 的 Start() 是阻塞的(内部先注册 etcd 再 Serve,
	// 出错走 logx.Must 直接退出),放到 goroutine 里,主 goroutine 仍按下面的信号退出。
	go s.Start()
	if maintenanceRuntime != nil {
		go func() {
			prepareCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			digest, err := kafkaConsumer.ReadyFrontier(prepareCtx)
			cancel()
			if err != nil {
				logx.Errorf("维护新boot consumer准备HOLD: %v", err)
				return
			}
			ready, err := maintenanceRuntime.Readiness("SOURCE", digest)
			if err != nil {
				logx.Errorf("维护新boot正式身份HOLD: %v", err)
				return
			}
			if err = maintenanceRuntime.AwaitOpen(context.Background(), ready, func(ctx context.Context) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if err := rootAdmission.OpenPrepared(); err != nil {
					return nil, err
				}
				return rootAdmission.SnapshotDigest(), nil
			}); err != nil {
				logx.Errorf("维护新boot开放HOLD: %v", err)
			}
		}()
	}

	if *maintenanceControlDir != "" {
		go func() {
			runtime, err := drainproof.WaitRuntime(controlCtx, *maintenanceControlDir)
			if err != nil {
				if controlCtx.Err() == nil {
					rootAdmission.CloseAdmission()
					logx.Errorf("维护控制输入HOLD: %v", err)
				}
				return
			}
			writer, err := proto_sql.CurrentWriterConfig()
			if err == nil {
				err = bindMaintenanceRuntime(runtime, writer)
			}
			if err != nil {
				rootAdmission.CloseAdmission()
				logx.Errorf("维护同boot控制身份HOLD: %v", err)
				return
			}
			if err = dbdrain.RunExisting(controlCtx, runtime, config.AppConfig, ctx.RedisClient, ctx.PlacementRedis, stores, kafkaConsumer, saveReplay, func(ctx context.Context) ([]byte, error) {
				rootAdmission.CloseAdmission()
				if err := rootAdmission.Wait(ctx); err != nil {
					return nil, err
				}
				return rootAdmission.SettledDigest(ctx)
			}); err != nil {
				rootAdmission.CloseAdmission()
				logx.Errorf("维护同boot排空HOLD: %v", err)
			}
		}()
	}

	// Wait for shutdown signal
	fmt.Println("\n=============================================================")
	fmt.Println("  DB SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  ZoneId:      %d\n", config.AppConfig.ZoneId)
	fmt.Printf("  Listen:      %s\n", config.AppConfig.ListenOn)
	fmt.Printf("  Mode:        %s\n", config.AppConfig.Mode)
	if len(config.AppConfig.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", config.AppConfig.Etcd.Hosts)
	}
	fmt.Printf("  redis:       %s\n", config.AppConfig.ServerConfig.RedisClient.Hosts)
	placementRedisHosts := "(RedisClient)"
	if pr := config.AppConfig.Placement.Redis; pr != nil {
		placementRedisHosts = fmt.Sprintf("%s db=%d", pr.Hosts, pr.DB)
	}
	pc := config.AppConfig.Placement
	// 读方门禁开关与闸间隔也打在 placement 这一行(db.yaml:这一行是核对依据):回退「先关读方」之后,运维看这一行确认
	// requireRouteReady=false 已生效,不必靠「闸的 open/closed 日志没出现」反推。间隔打生效值,与 newRouteReadyGating
	// 同口径(0 取缺省);开关关着时间隔不生效,照样打出来便于核对 ConfigMap 渲染。
	routeReadyGateIntervalMs := pc.RouteReadyGateIntervalMs
	if routeReadyGateIntervalMs == 0 {
		routeReadyGateIntervalMs = config.DefaultPlacementRouteReadyGateIntervalMs
	}
	fmt.Printf("  placement:   redis=%s required=%v storeFamilies=%v zoneStorePool=%d/%d nonZoneStorePool=%d/%d extraStoreIdleTimeout=%v requireRouteReady=%v routeReadyGateInterval=%v\n",
		placementRedisHosts, pc.Required, pc.StoreFamiliesAllowed(),
		pc.ZoneStoreMaxOpenConn, pc.ZoneStoreMaxIdleConn, pc.NonZoneStoreMaxOpenConn, pc.NonZoneStoreMaxIdleConn,
		time.Duration(pc.ExtraStoreConnMaxIdleSeconds)*time.Second,
		pc.RequireRouteReady, time.Duration(routeReadyGateIntervalMs)*time.Millisecond)
	fmt.Printf("  kafka:       %v\n", config.AppConfig.ServerConfig.Kafka.Brokers)
	fmt.Printf("  kafka topic: %s\n", config.AppConfig.ServerConfig.Kafka.Topic)
	redisCfg := config.AppConfig.ServerConfig.RedisClient
	fmt.Printf("  retry:       routing=%s (%s) waitaof=%d/%d/%dms\n", retryRouting, retryRoutingSource,
		redisCfg.NumLocal(), redisCfg.NumReplicas(), redisCfg.WaitAOFTimeoutMs)
	fmt.Printf("  mysql:       %s@%s/%s\n", config.AppConfig.ServerConfig.Database.User, config.AppConfig.ServerConfig.Database.Hosts, config.AppConfig.ServerConfig.Database.DBName)
	fmt.Println("=============================================================")
	// 不要对 quit 调 signal.Stop / signal.Reset:Linux 上 go-zero 收到 SIGTERM 后会在约 5.5 秒后向自身重发一次
	// (core/proc 的 gracefulStop)。quit 仍在注册时信号只进这个通道,不会走默认处理杀掉进程;一旦摘掉,第二次
	// SIGTERM 会在 kafkaConsumer.Stop 排空途中直接结束进程(Stop 的顺序与上限见 KeyOrderedKafkaConsumer.Stop)。
	// Windows 上 go-zero 的 proc 是空实现,本机 go_services.ps1 发的 Ctrl+C 以 os.Interrupt(SIGINT)到达这里。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
		rootAdmission.CloseAdmission()
		fmt.Println("Shutting down gracefully...")
	case err := <-protocolConflict:
		// 走与信号相同的停机路径(全部 defer),最后以非零码退出;重启后停在启动门禁,直到配置改为 dual。
		logx.Errorf("SAVE-PROTOCOL-CONFLICT legacy go/db is running in a strict save-barrier zone, shutting down: %v", err)
		fmt.Println("Shutting down: save protocol generation conflict (set ServerConfig.Fencing.Mode=dual)...")
		exitCode = 1
	}
}

// newKillSwitchEtcdClient 按 RpcServerConf.Etcd 建一个只给热关停 watch 用的
// etcd 客户端。**返回 nil 是合法结果**,调用方直接把 nil 传给
// killswitch.Start 即可(它会打一条 Info 后全部放行)。
//
// 为什么不复用 go-zero 服务注册内部那个客户端:go-zero 没把它暴露出来。
//
// 为什么任何失败都只记日志不 panic:fail-open 是 killswitch 的铁律 ——
// 管控组件自身故障绝不能拖垮业务。db 是玩家权威数据的写入方,
// 因为一个"止血阀连不上 etcd"就拒启,等于用小故障换一次全服停写。
//
// 注意 clientv3.New 不带 WithBlock,不会在这里真的去拨号,因此也不会拖慢启动;
// 连不通的后果只是 killswitch 的全量同步失败并按 ResyncBackoff 重试(期间放行)。
func newKillSwitchEtcdClient(cfg discov.EtcdConf) *clientv3.Client {
	if len(cfg.Hosts) == 0 {
		logx.Info("[killswitch] db etcd Hosts not configured; kill switch inactive (all requests allowed)")
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
		logx.Errorf("[killswitch] db failed to create etcd client; kill switch inactive (all requests allowed): %v", err)
		return nil
	}
	return cli
}

func bindMaintenanceRuntime(runtime *drainproof.Runtime, writer authoritysql.WriterConfig) error {
	physical, err := hex.DecodeString(os.Getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil {
		return err
	}
	if !bytes.Equal(physical, runtime.Plan.PhysicalClusterIdentity) || writer.AuthoritySchema() != runtime.Plan.AuthoritySchema || writer.Generation() != runtime.Request.Generation || strings.Join(writer.ProtectedSchemas(), ",") != strings.Join(runtime.Plan.ProtectedSchemas, ",") {
		return drainproof.ErrHold
	}
	return runtime.BindBoot(writer.InstanceID(), writer.WriterKey())
}
