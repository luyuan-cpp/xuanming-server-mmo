package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"db/internal/config"
	dbdrain "db/internal/drain"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	server "db/internal/server/db"
	"db/internal/svc"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	db_grpc "proto/db"
	storepb "proto/player_store"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/drainproof"
	"shared/grpcstats"
	"shared/killswitch"
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
	// 回填 optional 段的默认值(迁移超时、连接池)。见 config.Normalize 注释。
	config.AppConfig.Normalize()

	// conf.Load 在 Normalize 之前已自动调过一次;这里按回填默认值之后的生效值再判一次,非法即拒启。
	if err := config.AppConfig.Validate(); err != nil {
		panic(fmt.Sprintf("config rejected: %v", err))
	}

	// Derive the zone-specific database name from ZoneId
	if config.AppConfig.ZoneId == 0 {
		panic("ZoneId must be set in config (> 0)")
	}
	config.AppConfig.ServerConfig.Database.DBName = config.ZoneDBName(config.AppConfig.ZoneId)

	var maintenanceRuntime *drainproof.Runtime
	if *maintenancePlan != "" {
		var err error
		maintenanceRuntime, err = drainproof.LoadRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
		if err != nil {
			panic(fmt.Sprintf("维护runtime原件拒绝: %v", err))
		}
	} else if *maintenanceDir != "" || *maintenanceSHA != "" {
		panic("维护参数必须完整提供")
	}
	ctx := svc.NewServiceContext()

	// InitDB 默认**不跑任何 DDL**,并且会断言实际连上的 DATABASE() 落在外部
	// 注入的白名单里 —— 不在就拒启,而不是静默建库把玩家数据写进去。
	// 建表/补列由部署阶段的 `go run ./cmd/migrate -command up` 负责,
	// 运维步骤见 go/db/README.md。
	if err := proto_sql.InitDB(); err != nil {
		panic(fmt.Sprintf("database init rejected: %v", err))
	}

	// 落点库注册表(docs/design/player-storage-placement.md §6.1):本 zone 库登记为落点 ZoneId,
	// 其余落点库在第一次被指向时按需打开(只 Ping + 只读 schema 闸,从不建库)。
	// 必须在 InitDB 之后(拿本 zone 库做种子)。
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
		if err := dbdrain.Run(maintenanceCtx, maintenanceRuntime, stores); err != nil {
			panic(fmt.Sprintf("维护排空HOLD: %v", err))
		}
		return
	}

	// Start Prometheus /metrics endpoint (no-op when MetricsListenAddr empty).
	metrics.Start(config.AppConfig.MetricsListenAddr)

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
	// 存盘(docs/design/player-save-direct-commit.md)。
	playerStoreServer, err := newPlayerStoreServer(&config.AppConfig, stores)
	if err != nil {
		panic(fmt.Sprintf("player store rejected: %v", err))
	}
	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		db_grpc.RegisterDbServer(grpcServer, server.NewDbServer(ctx))
		storepb.RegisterPlayerStoreServer(grpcServer, playerStoreServer)
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
	// 存盘接口的结局由 internal/metrics 的 db_player_store_requests_total 统计。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{}))
	defer s.Stop()

	// 真正把 gRPC 服务跑起来。fc9377336(asynq→kafka)之后这里只剩 MustNewServer
	// 没有 Start:进程打印 "STARTED SUCCESSFULLY" 却什么端口都不监听,也不向 etcd
	// 注册 db.rpc。本地 compose 没探针所以一直没暴露;K8s(deploy/k8s/manifests/go-svc/db.yaml)
	// 的 grpc readiness/liveness 探 6000 端口永远超时,db Pod 永远 0/1 并被反复重启
	// (2026-09-03 kind 实跑)。go-zero 的 Start() 是阻塞的(内部先注册 etcd 再 Serve,
	// 出错走 logx.Must 直接退出),放到 goroutine 里,主 goroutine 仍按下面的信号退出。
	go s.Start()
	unregister, err := registerPlayerStoreNode(&config.AppConfig)
	if err != nil {
		panic(fmt.Sprintf("player store node registration failed: %v", err))
	}
	defer unregister()
	if maintenanceRuntime != nil {
		go func() {
			// 没有消费者要追平,存盘接口开放之前没有别的前沿。
			digest := sha256.Sum256([]byte("DB_PLAYER_STORE_READY_V1"))
			ready, err := maintenanceRuntime.Readiness("SOURCE", digest[:])
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
			if err = dbdrain.RunExisting(controlCtx, runtime, stores, func(ctx context.Context) ([]byte, error) {
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
	pc := config.AppConfig.Placement
	fmt.Printf("  placement:   storeFamilies=%v zoneStorePool=%d/%d nonZoneStorePool=%d/%d extraStoreIdleTimeout=%v\n",
		pc.StoreFamiliesAllowed(),
		pc.ZoneStoreMaxOpenConn, pc.ZoneStoreMaxIdleConn, pc.NonZoneStoreMaxOpenConn, pc.NonZoneStoreMaxIdleConn,
		time.Duration(pc.ExtraStoreConnMaxIdleSeconds)*time.Second)
	fmt.Printf("  mysql:       %s@%s/%s\n", config.AppConfig.ServerConfig.Database.User, config.AppConfig.ServerConfig.Database.Hosts, config.AppConfig.ServerConfig.Database.DBName)
	fmt.Println("=============================================================")
	// 不要对 quit 调 signal.Stop / signal.Reset:Linux 上 go-zero 收到 SIGTERM 后会在约 5.5 秒后向自身重发一次
	// (core/proc 的 gracefulStop)。quit 仍在注册时信号只进这个通道,不会走默认处理杀掉进程。
	// Windows 上 go-zero 的 proc 是空实现,本机 go_services.ps1 发的 Ctrl+C 以 os.Interrupt(SIGINT)到达这里。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	rootAdmission.CloseAdmission()
	fmt.Println("Shutting down gracefully...")
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
