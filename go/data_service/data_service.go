package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/idissuer"
	dskafka "data_service/internal/kafka"
	"data_service/internal/metrics"
	"data_service/internal/noderegistry"
	"data_service/internal/routetoolcli"
	"data_service/internal/server"
	"data_service/internal/store"
	"data_service/internal/svc"
	base "proto/common/base"
	"proto/data_service"
	"shared/authoritysql"
	"shared/buildinfo"
	"shared/dbplacement"
	"shared/dirshard"
	"shared/drainproof"
	"shared/etcdwatch"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/safego"
	"shared/serverbase"
	"shared/tracing"

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
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空的私有目录")

// migrateOnly 是生产用的显式建表入口:`data_service -f <yaml> -migrate` 对目录全部片和流水库跑一次
// store.MigrateSchema(目录表、路由表和闸门表走手写 DDL,流水表由 proto 驱动),跑完即退出,不起 gRPC、不连 Redis。
// 库要提前建好。生产把 AutoMigrate 设 false,部署阶段单进程跑这个。
var migrateOnly = flag.Bool("migrate", false, "run the directory and ledger schema migration and exit")

// 发号器的运维入口(docs/ops/id-issuer-runbook.md),都是跑完即退出,都不并进 -migrate:部署每次都跑 -migrate,
// 而记录不存在可能是事故,不能让一次普通发布悄悄盖过去。首次初始化和事故恢复是两个入口,不能互相代替。
//
// idIssuerInit 给一个从没认领过的地域码初始化:在全局号源登记处认领这个码,再建两份本地记录。只在新地域第一次部署时跑。
var idIssuerInit = flag.Bool("id-issuer-init", false, "claim a never-used region code in the id registry, create its reservation records and exit")

// idIssuerRecover 在事故之后修复记录,上界只取幸存的记录;没有可信上界就拒绝,不猜起点。
var idIssuerRecover = flag.Bool("id-issuer-recover", false, "repair the id issuer records from the surviving ones after an incident and exit")

// idIssuerRebind 只和 -id-issuer-recover 一起用:允许把这个地域码的发号资格转到本部署环境和配置里的这两套 etcd 上。
var idIssuerRebind = flag.Bool("id-issuer-rebind", false, "with -id-issuer-recover: move this region code to this deployment and the configured etcd clusters")

// idIssuerRetire 让配置里的地域码永久退役:登记处留墓碑,之后这个码预留、初始化、恢复全部拒绝。
// 码还在用时只能在绑定它的那个地域执行,否则拒绝。
var idIssuerRetire = flag.Bool("id-issuer-retire", false, "retire the configured region code in the id registry forever and exit")

// idIssuerRetireUnbound 只和 -id-issuer-retire 一起用:绑定这个码的地域已经不在、没法在那里执行时,确认之后从别处退役。
var idIssuerRetireUnbound = flag.Bool("id-issuer-retire-unbound", false,
	"with -id-issuer-retire: retire a code that is in use from a deployment it is not bound to, once the bound region is confirmed gone")

// idIssuerRetireCode 只和 -id-issuer-retire 一起用:退役这个码,而不是配置里的地域码。给登记处重建时的退役码补墓碑、
// 跳过下一个可认领的码、退役一个已经不在的地域,都要操作别的码;配置里的地域码有部署注入的值把关,不能为了这个去改配置。
// 没给这个参数就退役配置里的码;显式给的值必须是 1..63(retireIssuerFactory),缺省值 -1 只是占位。
var idIssuerRetireCode = flag.Int("id-issuer-retire-code", -1,
	"with -id-issuer-retire: retire this region code (1..63) instead of the configured one")

// idRegistryOpen 开放全局号源登记处的认领。只在两种时候跑:世界第一次部署(-id-registry-next 1);
// 登记处整个丢了数据、重建做完时(-id-registry-next 等于登记处里重建中的那个值)。
var idRegistryOpen = flag.Bool("id-registry-open", false, "open the global id registry with -id-registry-next as the first claimable region code and exit")

// idRegistryRebuild 在登记处整个丢了数据之后声明重建:写入下一个可认领的码(-id-registry-next 取台账里用过的最大码 +1),
// 还不开放认领。之后在用的地域各自 -id-issuer-recover,台账里的退役码按它核对补墓碑,全部做完再 -id-registry-open(运维手册 §4.3)。
var idRegistryRebuild = flag.Bool("id-registry-rebuild", false,
	"declare the next claimable region code (-id-registry-next) of a global id registry that lost its data, without opening claims, and exit")
var idRegistryNext = flag.Int("id-registry-next", -1,
	"with -id-registry-open or -id-registry-rebuild: the next claimable region code (1 for a new world, the largest used code + 1 for a rebuild)")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

// routeTool 是 route_tool 的主程序入口:`data_service -f <yaml> -route-tool <子命令> [参数]`
// 跑一个运维子命令后退出,不起 gRPC、不连别的依赖。K8s 镜像只构建单一入口 /app/service(deploy/k8s/Dockerfile.go-svc),
// 生产上只能这样跑 route_tool(一次性 Job / Pod)。逻辑在 internal/routetoolcli,与 cmd/route_tool 同一份。
var routeTool = flag.Bool("route-tool", false, "run a route_tool subcommand and exit: data_service -f <yaml> -route-tool <subcommand> [flags]")

// entryMode 是 main 的入口。
type entryMode uint8

const (
	entryServe entryMode = iota
	entryMigrate
	entryRouteTool
	entryIdIssuerInit
	entryIdIssuerRecover
	entryIdIssuerRetire
	entryIdRegistryOpen
	entryIdRegistryRebuild
)

// entryFlags 是选入口要看的 flag。
type entryFlags struct {
	migrate             bool
	routeTool           bool
	issuerInit          bool
	issuerRecover       bool
	issuerRebind        bool
	issuerRetire        bool
	issuerRetireUnbound bool
	issuerRetireCode    bool // 给了 -id-issuer-retire-code
	registryOpen        bool
	registryRebuild     bool
}

// selectEntry 按 flag 选入口(纯函数,data_service_entry_test.go 表驱动钉住)。args 是 flag.Args()(第一个非 flag 参数起)。
//   - -migrate、-route-tool、-id-issuer-init、-id-issuer-recover、-id-issuer-retire、-id-registry-open、-id-registry-rebuild
//     同时给多个:拒绝(一次只做一件事);
//   - -id-issuer-rebind 不和 -id-issuer-recover 一起:拒绝,它只是恢复的一个确认开关;
//   - -id-issuer-retire-unbound、-id-issuer-retire-code 不和 -id-issuer-retire 一起:拒绝,它们只是退役的参数;
//   - -route-tool 但没有子命令:拒绝;
//   - 没有 -route-tool 却带位置参数:拒绝 —— 以前这些参数被静默忽略,`data_service -f x.yaml freeze` 会被当成一次普通启动、
//     起一个服务进发现池,而运维以为自己跑的是冻结。
func selectEntry(f entryFlags, args []string) (entryMode, error) {
	exclusive := 0
	for _, on := range []bool{f.migrate, f.routeTool, f.issuerInit, f.issuerRecover, f.issuerRetire, f.registryOpen, f.registryRebuild} {
		if on {
			exclusive++
		}
	}
	switch {
	case exclusive > 1:
		return entryServe, errors.New("-migrate, -route-tool, -id-issuer-init, -id-issuer-recover, -id-issuer-retire, -id-registry-open " +
			"and -id-registry-rebuild are mutually exclusive")
	case f.issuerRebind && !f.issuerRecover:
		return entryServe, errors.New("-id-issuer-rebind only works together with -id-issuer-recover")
	case f.issuerRetireUnbound && !f.issuerRetire:
		return entryServe, errors.New("-id-issuer-retire-unbound only works together with -id-issuer-retire")
	case f.issuerRetireCode && !f.issuerRetire:
		return entryServe, errors.New("-id-issuer-retire-code only works together with -id-issuer-retire")
	case f.routeTool && len(args) == 0:
		return entryServe, errors.New("-route-tool needs a subcommand: data_service -f <yaml> -route-tool <subcommand> [flags]")
	case f.routeTool:
		return entryRouteTool, nil
	case len(args) > 0:
		return entryServe, fmt.Errorf("unexpected positional arguments %q; route_tool subcommands need -route-tool "+
			"(data_service -f <yaml> -route-tool %s ...)", args, args[0])
	case f.migrate:
		return entryMigrate, nil
	case f.issuerInit:
		return entryIdIssuerInit, nil
	case f.issuerRecover:
		return entryIdIssuerRecover, nil
	case f.issuerRetire:
		return entryIdIssuerRetire, nil
	case f.registryOpen:
		return entryIdRegistryOpen, nil
	case f.registryRebuild:
		return entryIdRegistryRebuild, nil
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
	entry, err := selectEntry(entryFlags{
		migrate:             *migrateOnly,
		routeTool:           *routeTool,
		issuerInit:          *idIssuerInit,
		issuerRecover:       *idIssuerRecover,
		issuerRebind:        *idIssuerRebind,
		issuerRetire:        *idIssuerRetire,
		issuerRetireUnbound: *idIssuerRetireUnbound,
		issuerRetireCode:    flagGiven("id-issuer-retire-code"),
		registryOpen:        *idRegistryOpen,
		registryRebuild:     *idRegistryRebuild,
	}, flag.Args())
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
	// MySQL 口令(MySQLPasswordEnv):-migrate、发号器运维入口与常驻启动都连库,所以在分流之前填。K8s 上写者与迁移 Job 读运维预置
	// Secret 里的配置,口令就写在配置里、不指名变量,这里不改动它;只有 route-tool Job 挂的 ConfigMap 指名变量,那条路在 routetoolcli 里填。
	var mysqlAuthErr error
	c, mysqlAuthErr = c.ResolveMySQLPassword(os.LookupEnv)
	if mysqlAuthErr != nil {
		fmt.Fprintf(os.Stderr, "data_service refuses to start: %v\n", mysqlAuthErr)
		os.Exit(1)
	}

	if entry == entryMigrate {
		os.Exit(runMigration(c))
	}
	switch entry {
	case entryIdIssuerInit:
		os.Exit(runIdIssuerTool(c, svc.NewIdIssuer, "id issuer init", idIssuerInitOp))
	case entryIdIssuerRecover:
		rebind := *idIssuerRebind
		os.Exit(runIdIssuerTool(c, svc.NewIdIssuer, "id issuer recover", func(ctx context.Context, issuer *idissuer.Issuer) (string, error) {
			return issuer.Recover(ctx, rebind)
		}))
	case entryIdIssuerRetire:
		unbound := *idIssuerRetireUnbound
		newIssuer, codeErr := retireIssuerFactory(flagGiven("id-issuer-retire-code"), *idIssuerRetireCode)
		if codeErr != nil {
			fmt.Fprintf(os.Stderr, "data_service: %v\n", codeErr)
			os.Exit(2)
		}
		os.Exit(runIdIssuerTool(c, newIssuer, "id issuer retire", func(ctx context.Context, issuer *idissuer.Issuer) (string, error) {
			return idIssuerRetireOp(ctx, issuer, unbound)
		}))
	case entryIdRegistryOpen:
		next := *idRegistryNext
		os.Exit(runIdIssuerTool(c, svc.NewIdIssuer, "id registry open", func(ctx context.Context, issuer *idissuer.Issuer) (string, error) {
			return idRegistryOpenOp(ctx, issuer, next)
		}))
	case entryIdRegistryRebuild:
		next := *idRegistryNext
		os.Exit(runIdIssuerTool(c, svc.NewIdIssuer, "id registry rebuild", func(ctx context.Context, issuer *idissuer.Issuer) (string, error) {
			return idRegistryRebuildOp(ctx, issuer, next)
		}))
	}
	var globalErr error
	c, globalErr = c.ResolveGlobalAuthority(os.Getenv)
	if globalErr != nil {
		fmt.Fprintf(os.Stderr, "data_service global SQL wiring rejected: %v\n", globalErr)
		os.Exit(1)
	}
	// 共享 Redis 口令(SharedRedisPasswordEnv,K8s 上经 Secret redis-auth 注入)。-migrate 与发号器运维入口不连 Redis,
	// 迁移 Job 也不注入这个变量,所以放在它们之后。
	var redisAuthErr error
	c, redisAuthErr = c.ResolveSharedRedisPassword(os.LookupEnv)
	if redisAuthErr != nil {
		fmt.Fprintf(os.Stderr, "data_service refuses to start: %v\n", redisAuthErr)
		os.Exit(1)
	}
	// 控制集群段:名字读缓存常驻就连它,不分开关(Recall 用的是同一段)。-migrate 与发号器运维入口不连 Redis,所以也放在它们之后。
	if err := c.ValidateControlRedis(); err != nil {
		fmt.Fprintf(os.Stderr, "data_service refuses to start: %v\n", err)
		os.Exit(1)
	}

	// RouteStore 段:任何一项非法都拒启;拒绝而不纠正,理由同 PlayerNameConfig.Validate。-migrate 不读它,所以放在那之后。
	c.RouteStore = c.RouteStore.Normalize()
	if err := c.RouteStore.Validate(); err != nil {
		logx.Errorf("[startup] invalid RouteStore config; refusing to start: %v", err)
		fmt.Fprintf(os.Stderr, "data_service refuses to start: invalid RouteStore config: %v\n", err)
		logx.Close()
		os.Exit(1)
	}
	logx.Infof("[startup] RouteStore: max_open_conn=%d max_idle_conn=%d merged_zone_refresh_sec=%d",
		c.RouteStore.MaxOpenConn, c.RouteStore.MaxIdleConn, c.RouteStore.MergedZoneRefreshSec)

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
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	controlCtx, cancelControl := context.WithCancel(context.Background())
	defer cancelControl()
	svcCtx := svc.NewServiceContextWithAdmission(c, root, relay)
	// 启动自检仍 fail-closed；尚未启动任何 worker 时可安全关闭三个空门禁后回收。
	cleanupUnstarted := func() error {
		svcCtx.CloseRootAndRelayIngress()
		relay.CloseAdmission()
		return svcCtx.CloseAndReport()
	}
	if missing := svcCtx.MissingStores(); c.Store.Required && len(missing) > 0 {
		return errors.Join(fmt.Errorf("Store.Required=true，依赖未就绪: %v", missing), cleanupUnstarted())
	}
	if err := svcCtx.MergedZonesUnassembled(); err != nil {
		return errors.Join(err, cleanupUnstarted())
	}
	// 定时重读合服映射;CloseAndReport 里停。上面的自检没过时不起,也就不用单独收回。
	svcCtx.MergedZones.Start()
	// 删角的后台收尾走 RELAY 门禁,closeRelay 排空之前停。
	svcCtx.StartRoleRemovalResumer()
	// GM 回收的派发和重投同样走 RELAY 门禁。
	svcCtx.StartRecallChannel()

	kafkaOwner, err := dskafka.NewConsumerOwner(relay, svcCtx.RelayIngress)
	if err != nil {
		return errors.Join(err, cleanupUnstarted())
	}
	kafkaStart := startKafkaConsumers(context.Background(), c.Kafka, svcCtx, kafkaOwner)
	startAuditRetention(c.AuditRetention, svcCtx, kafkaOwner)
	// SOURCE 闭合:本服务的 SOURCE 只有 gRPC 入口这一个来源,没有后台发行者。关闭接纳、等在途 RPC 结束就闭合,
	// 闭合摘要由 root 自己的计数算出(Session.SettledDigest)。上游都走 RELAY,见 dataServiceRelayMethods。
	// 两步都幂等:排空控制线与 closeRelay 都会调到这里。
	closeSource := func(ctx context.Context) error {
		root.CloseAdmission()
		return root.Wait(ctx)
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
		result = errors.Join(result, svcCtx.StopRoleRemovalResumer(ctx))
		result = errors.Join(result, svcCtx.StopRecallChannel(ctx))
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
	// 每次抓取时现取 sql.DBStats。
	// 不能放进 NewServiceContext:单测会多次构造 ServiceContext,默认注册表重复注册会报错。注册失败只记日志。
	metrics.RegisterSQLPools(svcCtx.SQLPoolSamples)

	// HTTP 导出仅观察指标与并发安全的 DB.Stats，不派生业务写；不作为资产发行者 closure。
	// 空地址保持禁用。
	metrics.Start(c.MetricsListenAddr)
	metrics.PrimeRoute()

	// ── 热关停(killswitch)────────────────────────────────────────────
	// data_service 是玩家权威数据的读写入口,真出事时最缺的是"秒级止血阀":
	// 往 etcd 前缀 /mmorpg/killswitch/ 下写一个 key 就能把某个方法立刻短路掉
	// (例如 BatchRecallItems 这种放大面极大的运维接口),而不必走一遍
	// 构建-发布-滚动更新。规则布局与优先级见 shared/killswitch 包注释。
	//
	// New 出来立刻可用(规则为空 = 全放行),Start 非阻塞、etcd 为 nil 也合法,
	// 所以整段接线不会让 data_service 的启动多出任何一个失败点。
	//
	// NodeTypes:认 type/DataServiceNodeService/ 下的规则;注册成功后在 onRegistered 里 AddInstance
	// (docs/design/rpc-kill-switch.md §5)。受保护的方法(§8,只认精确规则):ReleasePlayerName 是 login 建角失败的回滚,
	// 挡住它名字会一直被占;AckRoleTradeDrain 是交易排空回执,挡住它排空证明提交不了。
	ks := killswitch.New(killswitch.Config{
		NodeTypes: []string{base.ENodeType_name[int32(base.ENodeType_DataServiceNodeService)]},
		Protected: []string{
			data_service.DataService_ReleasePlayerName_FullMethodName,
			data_service.DataService_AckRoleTradeDrain_FullMethodName,
		},
	})
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
	// 请求顶层带 player_id 的方法,之后各层与 handler 的日志都带上它(docs/design/distributed-tracing.md §6.2)。
	s.AddUnaryInterceptors(tracing.PlayerFieldInterceptor())
	relayMethods := dataServiceRelayMethods()
	s.AddUnaryInterceptors(serverbase.PublicationAdmission(root, relay, relayMethods, svcCtx.RelayIngress))
	s.AddUnaryInterceptors(holdDataServiceFaults(root, relay, relayMethods))
	// in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把失败塞进
	// 响应体的 `uint32 error_code` —— gRPC status 恒 OK,go-zero 自带的指标拦截器
	// 会把每一次"快照库写不进去"都记成一次成功请求,监控上看不出
	// 任何比例变化,直到玩家数据已经丢了才有人发现。这层把响应体里的码读出来
	// 定性:故障打日志 + 计数,业务拒绝只计数。它不改响应内容、不吞错,对调用方无感。
	//
	// 传 ErrorCodeClassifier 而**不是** TipClassifier,依据是 proto 与 handler 的
	// 事实:data_service 走的是自己的私有码表(internal/constants/error_codes.go,
	// 0..17),由 `uint32 error_code` 字段承载,与 friend/guild 那套 TipInfoMessage
	// 码表完全无关 —— 数值区间还高度重叠,混用会得到垃圾定性。serverbase 在没有 ErrorCodeClassifier 时
	// 对私有码只敢记成 VerdictBizReject,所以这份集合必须由本服务显式给出。
	//
	// 归进"服务端内部故障"的码,逐个都有产码点为证:
	//   ErrCodeSnapshotDBError(11) —— 快照/流水 MySQL 出错,或 store 干脆没配起来
	// 其余非 0 码刻意**不算**故障,免得刷出满屏假告警:
	//   LockConflict(2) / VersionMismatch(3)  正常并发竞争,重试即可
	//   NotFound(4) / SnapshotNotFound(10) / ZoneNotFound(15)  查无此物
	//   InvalidRequest(14) 调用方参数问题
	//   NotImplemented(16) 刻意的 fail-closed(fence 未配置就明确失败,不装成成功),
	//                      是确定性的配置态而非运行期故障,告警只会变噪音
	//   ResultTruncated(17) 命中数超上限的容量拒绝 —— 按 serverbase 的既定口径,
	//                      "满"属于规则拒绝不是故障
	// 号段发号(AllocateIdSegment)再加两个故障码:
	//   ErrCodeIdSegmentSourceError(18) —— 发号器读写预留记录失败 / 没配起来,发号源停摆
	//   ErrCodeIdSegmentExhausted(19)   —— 值域见底的 fail-closed;理论上万年不会发生,
	//                                      发生即事故,必须当故障告警而不是"业务拒绝"
	//   ErrCodeIdSegmentUnknownTag(20)  —— 漏配 IdSegment.Tags,或生产(AllowAutoSeed=false)下两份预留记录
	//                                      都不存在 —— 后者不是首次部署的话就是两套 etcd 同时丢了数据,
	//                                      从头发号会静默串档,必须告警而不是当参数错
	// ErrCodeZoneMappingConflict(21)与 ErrCodeAdminAuthRequired(23)刻意不在这份集合里,而且这层拦截器也看不到它们:
	// RegisterPlayerZone 的响应体是 emptypb.Empty,都没有 in-band 的 error_code 字段可读。它们的可见性由
	// handler 里的 logx.Errorf 提供(每一条都带调用方身份),外加 grpcstats 那层按 gRPC code 统计的失败率。
	// (22、24 随合服围栏删除,号不复用。)
	// 归属映射的 ErrCodeRouteStoreError(30)同理:只出现在 gRPC status message 里(Unavailable),这层看不到。可见性来自
	// data_service_route_register_total{outcome="store_error"} / data_service_route_read_total{result="error"}
	// 与 `[RegisterPlayerZone]` / `[DataRoute]` 的 ERROR 日志。
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
	// 注册拿到 node_uuid 之后,回收重投的成员表才认得出本副本(缺口清单第 25 条);此前成员表若已有别的副本,本副本一片都不做。
	registrar := startNodeRegistration(c, regCli, func(nodeUUID string) {
		svcCtx.BindRecallWorkMember(nodeUUID)
		ks.AddInstance(nodeUUID) // 认 instance/<本进程 uuid>/ 下的热关停规则
	})
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
	fmt.Printf("  name cache:  control cluster %s\n", c.ControlRedis.Describe())
	fmt.Printf("  route:       player_route on the global DB (merged_zone_refresh_sec=%d)\n", c.RouteStore.MergedZoneRefreshSec)
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:     %s/metrics\n", c.MetricsListenAddr)
	}
	if len(c.Kafka.Brokers) > 0 {
		// 打**有效** topic 名(带代号后缀):排障时第一件要核对的就是它与生产者 go/db(AuditOutbox 段,审计发件箱转发)一致。
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
	fmt.Printf("data_service schema migration: %s/%s logical_shards=%d\n", cfg.Host, cfg.DBName, cfg.LogicalShards)
	// 搬库期间(state=MOVING)拒绝 DDL;库搬走之后连当前集群。逐库读一次映射。
	cfg.PoolOptionsFor = func(schema string) ([]authoritysql.OpenOption, error) {
		return dbplacement.ForMigration(context.Background(), c.SnapshotMySQL.DbPlacement, schema)
	}
	if err := store.MigrateSchema(context.Background(), cfg, store.MigrateOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "schema migration FAILED: %v\n", err)
		return migrateExitCode(err)
	}
	fmt.Printf("schema migration OK: directory tables in place on %d shard(s), directory_shard and route_meta seeded; transaction_log, player_snapshot, rollback_audit_log synced from proto on %s\n",
		cfg.LogicalShards, dirshard.LedgerDBName(cfg.DBName))
	return 0
}

// idIssuerToolTimeout 是发号器运维入口的预算:读写三套 etcd 各几次,登记处可能在别的地域,不需要更久。
const idIssuerToolTimeout = 30 * time.Second

// idIssuerFactory 装配运维入口要用的发号器。
type idIssuerFactory func(config.Config) (*idissuer.Issuer, error)

// flagGiven 报告命令行里有没有显式给过这个 flag,给的值恰好等于缺省值也算。
func flagGiven(name string) bool {
	given := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// retireIssuerFactory 选 -id-issuer-retire 用哪种装配。没给 -id-issuer-retire-code:退役配置里的码,它要和部署注入的地域码一致。
// 给了:操作那个码,必须是 1..63。显式给的值不合法一律报错,绝不回落到配置里的码:那是本地域正在用的码,
// 而本地域正是绑定它的环境,退役会直接通过。
func retireIssuerFactory(codeGiven bool, code int) (idIssuerFactory, error) {
	if !codeGiven {
		return svc.NewIdIssuer, nil
	}
	if code < 1 || code > int(idissuer.MaxRegion) {
		return nil, fmt.Errorf("-id-issuer-retire-code must be in 1..%d, got %d", idissuer.MaxRegion, code)
	}
	return func(c config.Config) (*idissuer.Issuer, error) {
		return svc.NewIdIssuerForRegionCode(c, uint32(code))
	}, nil
}

// runIdIssuerTool 用 newIssuer 装配发号器,跑一个运维操作:成功 0,失败 1。输出一行结果给运维核对。
func runIdIssuerTool(c config.Config, newIssuer idIssuerFactory, name string, op func(context.Context, *idissuer.Issuer) (string, error)) int {
	issuer, err := newIssuer(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s FAILED: %v\n", name, err)
		return 1
	}
	defer issuer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), idIssuerToolTimeout)
	defer cancel()
	result, err := op(ctx, issuer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s FAILED: %v\n", name, err)
		return 1
	}
	fmt.Printf("%s OK: %s\n", name, result)
	return 0
}

// idIssuerInitOp 是 -id-issuer-init。这个码已经由这两套 etcd 初始化过时什么也不做,所以重复执行是安全的。
func idIssuerInitOp(ctx context.Context, issuer *idissuer.Issuer) (string, error) {
	created, err := issuer.Init(ctx)
	if err != nil {
		return "", err
	}
	if !created {
		return "this region code is already initialized by this deployment on these etcd clusters, nothing changed", nil
	}
	return "region code claimed in the id registry and both reservation records created", nil
}

// idIssuerRetireOp 是 -id-issuer-retire。已退役时什么也不做。码还在用时只能在绑定它的地域执行;
// unbound(-id-issuer-retire-unbound)是那个地域已经不在时的确认开关。退役哪个码由装配决定(retireIssuerFactory)。
func idIssuerRetireOp(ctx context.Context, issuer *idissuer.Issuer, unbound bool) (string, error) {
	retired, err := issuer.Retire(ctx, unbound)
	if err != nil {
		return "", err
	}
	if !retired {
		return "this region code was already retired, nothing changed", nil
	}
	return "region code retired in the id registry; it can never issue or be claimed again", nil
}

// idRegistryOpenOp 是 -id-registry-open。-id-registry-next 必须显式给 1..64:新世界给 1,
// 结束重建时给登记处里重建中的那个值(对不上就报出它现在的值)。已开放时什么也不做。
func idRegistryOpenOp(ctx context.Context, issuer *idissuer.Issuer, next int) (string, error) {
	if next < 1 || next > int(idissuer.MaxNextRegion) {
		return "", fmt.Errorf("-id-registry-open needs -id-registry-next in 1..%d (1 for a new world, "+
			"the value the registry holds when finishing a rebuild)", idissuer.MaxNextRegion)
	}
	opened, err := issuer.OpenRegistry(ctx, uint32(next))
	if err != nil {
		return "", err
	}
	if !opened {
		return "the id registry is already open, nothing changed", nil
	}
	return fmt.Sprintf("id registry opened, the next claimable region code is %d", next), nil
}

// idRegistryRebuildOp 是 -id-registry-rebuild。-id-registry-next 必须显式给 1..64,取地域码台账里用过的最大码 +1。
// 已经声明过(重建中或已开放)时什么也不做。
func idRegistryRebuildOp(ctx context.Context, issuer *idissuer.Issuer, next int) (string, error) {
	if next < 1 || next > int(idissuer.MaxNextRegion) {
		return "", fmt.Errorf("-id-registry-rebuild needs -id-registry-next in 1..%d (the largest used code in the region code ledger + 1)",
			idissuer.MaxNextRegion)
	}
	declared, err := issuer.BeginRegistryRebuild(ctx, uint32(next))
	if err != nil {
		return "", err
	}
	if !declared {
		return "the id registry already holds a next claimable region code (it is being rebuilt or is open), nothing changed", nil
	}
	return fmt.Sprintf("id registry rebuild started at next claimable region code %d; recover every active region, add the "+
		"tombstones from the ledger, then finish with -id-registry-open (docs/ops/id-issuer-runbook.md §4.3)", next), nil
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

// startAuditRetention 起审计表清理循环,随进程退出。维护排空开始后不再发新的 DELETE。
// 清理只删过期行,不发消息,所以不算 SOURCE 闭合里的后台发行者;排空冻结之后的写入由写者闸门拒绝。
func startAuditRetention(rc config.AuditRetentionConfig, svcCtx *svc.ServiceContext, owner *dskafka.ConsumerOwner) {
	snapshots, snapshotsOK := svcCtx.SnapshotStore.(*store.SnapshotStore)
	txLogs, txLogsOK := svcCtx.TxLogStore.(*store.TransactionLogStore)
	if rc.Disabled || !snapshotsOK || !txLogsOK {
		logx.Infof("[AuditRetention] not started: disabled=%v snapshot_store=%v transaction_log_store=%v", rc.Disabled, snapshotsOK, txLogsOK)
		return
	}
	rc = rc.Normalized()
	opts := store.AuditRetentionOptions{
		SnapshotMaxAge:       time.Duration(rc.SnapshotDays) * 24 * time.Hour,
		TransactionLogMaxAge: time.Duration(rc.TransactionLogDays) * 24 * time.Hour,
		BatchSize:            rc.BatchSize,
		Interval:             time.Duration(rc.IntervalSeconds) * time.Second,
		Paused:               owner.Draining,
		Observe: func(table string, deleted int64, err error) {
			metrics.ObserveAuditRetention(table, deleted, err != nil)
			if err != nil {
				logx.Errorf("[AuditRetention] delete from %s failed: %v", table, err)
			}
		},
	}
	safego.Go("data_service.audit_retention", func() {
		store.RunAuditRetention(context.Background(), opts, snapshots, txLogs)
	})
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
			Owner:                  owner,
			Brokers:                kc.Brokers,
			TxLogTopic:             txTopic,
			TxLogPartitions:        kc.TransactionLogPartitions,
			TxLogGroup:             kc.TransactionLogConsumerGroup,
			SnapshotTopic:          snapshotTopic,
			SnapshotPartitions:     kc.SnapshotPartitions,
			SnapshotGroup:          kc.SnapshotConsumerGroup,
			RetentionMs:            kc.RetentionMs,
			TxLogRetentionBytes:    kc.TxLogRetentionBytes,
			SnapshotRetentionBytes: kc.SnapshotRetentionBytes,
			// kc 已在 main 里 Normalize + ValidateConsumers 过(越界拒启),这里只做单位换算。
			TxLog: dskafka.TxLogConsumerConfig{MaxInflightFlushes: kc.TransactionLogMaxInflightFlushes},
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
			logx.Errorf("[kafka] Start failed attempt=%d, retrying in %s: %v", attempt, kafkaStartRetryInterval, err)
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

	// keepalive:killswitch 的 watch 不会因为半开连接静默停住(etcdwatch 包注释)。
	cli, err := clientv3.New(etcdwatch.WithKeepAlive(c))
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
	// keepalive:租约续约流遇到半开连接时尽快换成员重连,不至于续约静默停住、租约过期(etcdwatch 包注释)。
	cli, err := clientv3.New(etcdwatch.WithKeepAlive(c))
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
		logx.Errorf("node registration shutdown HOLD: %v", err)
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
// onRegistered 可为 nil;注册成功后在注册 goroutine 上收到本进程的 node_uuid(进程内不变),不要在里面阻塞。
// 生产接的是回收重投的成员表(svc.ServiceContext.BindRecallWorkMember,缺口清单第 25 条)。
func startNodeRegistration(c config.Config, cli *clientv3.Client, onRegistered func(nodeUUID string)) *nodeRegistrar {
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
		if onRegistered != nil {
			onRegistered(nr.Info.NodeUuid)
		}
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
		"/data_service.DataService/CheckAccountRoleSlot":          true,
		"/data_service.DataService/GetRoleFenceState":             true,
		"/data_service.DataService/VerifyRoleAdmission":           true,
		"/data_service.DataService/RegisterRoleAdmission":         true,
		"/data_service.DataService/CloseRoleAdmissions":           true,
		"/data_service.DataService/AckRoleTradeDrain":             true,
		"/data_service.DataService/ReleaseDeadPlayerStoreHolder":  true,
		"/data_service.DataService/ReadPlayerStoreHolder":         true,
		"/data_service.DataService/RegisterPlayerZone":            true,
		"/data_service.DataService/GetPlayerRoute":                true,
		"/data_service.DataService/GetPlayerHomeZone":             true,
		"/data_service.DataService/BatchGetPlayerHomeZone":        true,
		"/data_service.DataService/GetMergedZones":                true,
		"/data_service.DataService/CountZoneRoles":                true,
		"/data_service.DataService/ListPlayerSnapshots":           true,
		"/data_service.DataService/QueryTransactionLog":           true,
		"/data_service.DataService/AllocateIdSegment":             true,
		"/data_service.DataService/ReservePlayerName":             true,
		"/data_service.DataService/ReleasePlayerName":             true,
		"/data_service.DataService/BatchGetPlayerName":            true,
		"/data_service.DataService/BatchGetPlayerDisplay":         true,
		"/data_service.DataService/GetPlayerAssetOpLedger":        true,
		"/data_service.DataService/GetPlayerInboxLedger":          true,
		"/data_service.DataService/GetPlayerItemGainBlock":        true,
	}
}
func dataServiceFaultClassifier() serverbase.Classifier {
	return serverbase.FaultCodeSet(
		constants.ErrCodeSnapshotDBError,
		constants.ErrCodeIdSegmentSourceError,
		constants.ErrCodeIdSegmentExhausted,
		constants.ErrCodeIdSegmentUnknownTag,
	)
}

// DS 的 SQL 不定结果常以 error_code 返回；不能将 Go error=nil 当作 SOURCE/RELAY 已成功。
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
