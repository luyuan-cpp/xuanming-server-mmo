// cmd/loginservice/main.go
package main

import (
	"errors"
	"flag"
	"fmt"
	"login/internal/config"
	loginlogic "login/internal/logic/clientplayerlogin"
	loginauth "login/internal/logic/pkg/callerauth"
	"login/internal/logic/pkg/createbudget"
	"login/internal/logic/pkg/node"
	"login/internal/rpclogging"
	loginserver "login/internal/server/clientplayerlogin"
	loginadminserver "login/internal/server/loginadmin"
	loginpregateserver "login/internal/server/loginpregate"
	"login/internal/svc"
	"net"
	"os"
	"os/signal"
	login_proto "proto/common/base"
	databasepb "proto/common/database"
	login_proto_login "proto/login"
	"shared/buildinfo"
	"shared/callerauth"
	"shared/drainproof"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/safego"
	"shared/serverbase"
	"shared/shutdownbudget"
	"shared/tracing"
	"strconv"
	"syscall"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

var configFile = flag.String("loginService", "etc/login.yaml", "the config file path")

var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "正常boot受控排空的私有目录")
var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式typed维护runtime Receipt路径；不serve或声明topic")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有stop/report目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime Receipt原文件SHA256")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

const nodeType = login_proto.ENodeType_LoginNodeService

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("login"))
	if *showVersion {
		return
	}

	// Load config file
	//
	// 刻意不带 conf.UseEnv():它会对整份 yaml 做 os.ExpandEnv,任何含 `$` 的值(Redis 口令等)都会被静默改写。
	// 需要从环境变量取的密钥一律写成 *Env 字段(PasswordAuth.DSNEnv、Secrets.*.Env、AuthProviders.WeChat.AppSecretEnv),
	// 照旧文档写成 "${VAR}" 的字面量由下面的 ResolveSecrets 拒绝启动(config/auth_providers.go)。
	conf.MustLoad(*configFile, &config.AppConfig)
	// 停机强退时限要盖住在途请求与已受理的 EnterGame 后台链(loginlogic.EnterGameShutdownDrain):计划重启时进游戏的玩家
	// 要么进场、要么收到失败提示,而不是被拦腰砍断后干等客户端超时。必须在建 zrpc 服务之前。
	shutdownbudget.ApplyWithBackgroundDrain(&config.AppConfig.RpcServerConf, loginlogic.EnterGameShutdownDrain)
	if err := config.AppConfig.ValidateGlobalAuthority(os.Getenv); err != nil {
		panic(fmt.Errorf("global Login configuration rejected; refusing to start: %w", err))
	}
	// 新角色的存档库(总体方案 §7.2):地域码、存档库数不合法就拒绝启动,不建出落到错误库的角色。
	if err := config.AppConfig.Placement.Validate(); err != nil {
		panic(fmt.Errorf("Placement configuration rejected; refusing to start: %w", err))
	}
	// 建角各跳按 handler 截止分摊(docs/design/client-request-ordering.md §5.3):本地余量 + 各跳上限 + 跳间间隙
	// 放不进 zrpc Timeout 就拒绝启动,不让「名义预算之和超过 handler 预算」再静默上线。
	if err := createbudget.New(config.AppConfig.HomeZone.RegisterTimeout).Validate(
		time.Duration(config.AppConfig.Timeout) * time.Millisecond); err != nil {
		panic(fmt.Errorf("CreatePlayer budget configuration rejected; refusing to start: %w", err))
	}

	// 密钥门禁必须跑在**任何外部连接之前**(Kafka / Redis / etcd 都在下面)。
	// 生产模式下密钥为空、等于占位串、短于 32 字节、或不同用途复用同一把,
	// 一律拒绝启动 —— 一个用占位密钥起来的 login,比起不来危险得多。
	secrets, warnings, err := config.ResolveSecrets(&config.AppConfig, os.LookupEnv)
	for _, w := range warnings {
		logx.Errorf("SECURITY WARNING: %s", w)
	}
	if err != nil {
		panic(fmt.Sprintf("HMAC secret configuration invalid; refusing to start: %v", err))
	}
	config.ActiveSecrets = secrets
	// 共享 Redis 口令(SharedRedisPasswordEnv,K8s 上经 Secret redis-auth 注入):同样要在任何外部连接之前填好。
	if err := config.AppConfig.ResolveSharedRedisPassword(os.LookupEnv); err != nil {
		panic(fmt.Errorf("shared Redis password configuration invalid; refusing to start: %w", err))
	}

	// 控制面命令 topic(gate-cmd_g<N> / scene-cmd_g<N>)**刻意不在这里建**:
	// 它的分区数是不可变的寻址契约(partition = node_id % P),必须由基础设施
	// 引导阶段统一按 256 分区 + 1 小时保留期建好 —— deploy/docker-compose.yml 的
	// kafka-topic-init 或 k8s_deploy.ps1 -Command infra-kafka-topics,
	// 且必须早于任何 gate/scene 启动。任何服务在这里抢先预建都会引入第二个真源,
	// 分区数一旦与契约不符,命令就落到没人 assign 的分区上、静默全丢。
	// 见 docs/design/control-plane-topic-partitioning-20260908.md §3.1 / §7。
	// (老的一节点一 topic gate-<id> / scene-<id> 已停用,login 是最后一个切换的生产者。)
	var maintenanceRuntime *drainproof.Runtime
	if *maintenancePlan != "" {
		maintenanceRuntime, err = drainproof.LoadRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
		if err != nil {
			panic(fmt.Errorf("maintenance runtime rejected: %w", err))
		}
	} else if *maintenanceDir != "" || *maintenanceSHA != "" {
		panic("maintenance parameters must be provided in full")
	}
	var ctx *svc.ServiceContext
	if maintenanceRuntime != nil {
		ctx, err = svc.NewServiceContextForDrain(maintenanceRuntime)
		if err != nil {
			panic(fmt.Errorf("maintenance boot identity rejected: %w", err))
		}
	} else {
		ctx = svc.NewServiceContext()
	}

	defer ctx.Stop()

	if maintenanceRuntime != nil && maintenanceRuntime.Request.RunPhase != databasepb.SqlWriterDrainRun_SQL_RELEASED {
		ctx.StartForDrain()
	} else {
		ctx.Start()
	}
	if maintenanceRuntime != nil && maintenanceRuntime.Request.RunPhase != databasepb.SqlWriterDrainRun_SQL_RELEASED {
		maintenanceCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runMaintenanceLogin(maintenanceCtx, maintenanceRuntime, ctx); err != nil {
			panic(fmt.Errorf("login maintenance drain HOLD: %w", err))
		}
		return
	}

	controlCtx, stopControl := context.WithCancel(context.Background())
	defer stopControl()
	if maintenanceRuntime != nil && maintenanceRuntime.Request.RunPhase == databasepb.SqlWriterDrainRun_SQL_RELEASED {
		safego.Go("login.maintenance_prepared", func() {
			prepareCtx, cancel := context.WithTimeout(controlCtx, 60*time.Second)
			digest, err := ctx.PreparedFrontier(prepareCtx)
			cancel()
			if err != nil {
				logx.Errorf("maintenance new-boot prepare HOLD: %v", err)
				return
			}
			ready, err := maintenanceRuntime.Readiness("SOURCE", digest)
			if err != nil {
				logx.Errorf("maintenance new-boot identity HOLD: %v", err)
				return
			}
			if err = maintenanceRuntime.AwaitOpen(controlCtx, ready, ctx.OpenPreparedRoot); err != nil {
				logx.Errorf("maintenance new-boot open HOLD: %v", err)
			}
		})
	}
	if *maintenanceControlDir != "" {
		safego.Go("login.maintenance_live_control", func() {
			runtime, err := drainproof.WaitRuntime(controlCtx, *maintenanceControlDir)
			if err != nil {
				if controlCtx.Err() == nil {
					ctx.CloseRootAdmission()
					logx.Errorf("maintenance control input HOLD: %v", err)
				}
				return
			}
			if err = runtime.BindBoot(ctx.BootInstance(), nil); err != nil {
				ctx.CloseRootAdmission()
				logx.Errorf("maintenance same-boot identity HOLD: %v", err)
				return
			}
			if err = runMaintenanceLogin(controlCtx, runtime, ctx); err != nil {
				ctx.CloseRootAdmission()
				logx.Errorf("maintenance same-boot drain HOLD: %v", err)
			}
		})
	}
	// Start gRPC service
	if err := startGRPCServer(config.AppConfig, ctx); err != nil {
		logx.Errorf("Failed to start GRPC server: %v", err)
	}
}

// startGRPCServer starts the Login gRPC service and registers it to etcd.
func startGRPCServer(cfg config.Config, ctx *svc.ServiceContext) error {

	host, port, err := splitHostPort(config.AppConfig.ListenOn)
	if err != nil {
		logx.Errorf("Failed to parse listen address: %v", err)
		return err
	}

	// In K8s, 0.0.0.0 is not routable from other pods — use POD_IP if available.
	if podIP := os.Getenv("POD_IP"); podIP != "" && (host == "0.0.0.0" || host == "::") {
		host = podIP
	}

	// Register node to etcd
	loginNode := node.NewNode(uint32(nodeType), host, port, config.AppConfig.Node.LeaseTTL)
	if loginNode == nil {
		err = errors.New("failed to create node")
		logx.Errorf("Failed to create node: %v", err)
		return err
	}

	if err := loginNode.KeepAlive(); err != nil {
		logx.Errorf("Failed to keep node alive: %v", err)
		return err
	}

	defer func() {
		if err := loginNode.Close(); err != nil {
			logx.Errorf("Failed to close node: %v", err)
		}
	}()

	logx.Infof("Login node registered: %+v", loginNode.Info.String())

	// 这条 etcd 连接给热关停规则的 watch 用(newKillSwitch);复用 login 既有的 etcd 客户端工厂,不另拼一份配置。
	// PlayerId 只由发号器发(svc/player_id_minter.go,data_service.AllocateIdSegment):login 不申领 snowflake 槽位、
	// 不写水位,也没有失槽退出(2026-11-12 删)。
	etcdCli, err := node.NewEtcdClient()
	if err != nil {
		logx.Errorf("Failed to create etcd client: %v", err)
		return err
	}
	defer etcdCli.Close()

	// 热关停 watch 的生命周期:随本函数返回而结束。
	// startServer 里的 server.Start() 是阻塞的,它返回就意味着服务在停,
	// watch goroutine 必须跟着退出。
	//
	// defer 是 LIFO,这一句注册在 etcdCli.Close()(上面)之后,所以停机时
	// **先**取消 watch、**后**关 etcd 客户端,不会让 watch 撞上已关闭的连接。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	defer ksCancel()

	// Start gRPC server
	// etcdCli 直接复用上面建的那一个 —— login 到 etcd 只该有这一条业务连接,
	// 再 New 一个既多一份 keepalive 心跳,也多一处会漏关的资源。
	// 节点 uuid 给热关停认 instance/<uuid>/ 下的规则(docs/design/rpc-kill-switch.md §5)。
	if err := startServer(ksCtx, cfg, ctx, etcdCli, loginNode.Info.NodeUuid); err != nil {
		logx.Errorf("Failed to start gRPC server: %v", err)
		return err
	}

	return nil
}

// newSessionInterceptor 构造身份声明闸门。
//
// 这里替换掉的旧实现是全仓最严重的 P0:它把 metadata 里的
// x-session-detail-bin 解出来就当可信身份写进 ctx,没有任何校验 ——
// 任何能连到 login gRPC 端口的进程都可以自称是任意会话的任意玩家。
// 现在改成 fail-closed:带了身份声明就必须带 gate 的 Ed25519 签名,验不过直接
// Unauthenticated;不带声明的调用(Java Gateway 新链路)行为不变。
//
// 任何运行模式都强制:gate 已对全部会话身份签名,dev/test 不再保留「告警放行」档,
// 与在线域的 callerauth.GateInterceptor 一致。login 只持公钥(callerauth.PublicKeysEnv),
// 签不出名;公钥缺失或格式不对时拒绝启动(docs/design/gate-online-identity.md §10.1)。
func newSessionInterceptor(cfg config.Config, getenv func(string) string) (grpc.UnaryServerInterceptor, error) {
	keys, err := callerauth.ParsePublicKeys(getenv(callerauth.PublicKeysEnv))
	if err != nil {
		return nil, fmt.Errorf("%s is missing or malformed: %w", callerauth.PublicKeysEnv, err)
	}

	return loginauth.UnaryServerInterceptor(callerauth.NewVerifier(callerauth.Options{
		Keys:            keys,
		Enforce:         true,
		MaxClockSkew:    cfg.InternalAuth.MaxClockSkew,
		MaxNonceEntries: cfg.InternalAuth.MaxNonceEntries,
		AllowedCallers:  cfg.InternalAuth.AllowedCallers,
		OnNonceOverflow: func() {
			loginauth.RecordNonceOverflow()
			logx.Error("callerauth nonce table exceeded MaxNonceEntries and rotated early; replay window temporarily shortened, increase InternalAuth.MaxNonceEntries")
		},
	})), nil
}

// newKillSwitch 构造 RPC 级热关停闸门,并挂上 etcd list-watch。
//
// 返回 nil 表示这一层不挂(配置里显式关掉了),调用方跳过即可。
//
// 三条不变量都由 shared/killswitch 保证,这里只负责别把它们破坏掉:
//   - 非阻塞:Start 内部用 safego.Go 起 watch,绝不拖慢启动;
//   - fail-open:cli 为 nil / etcd 连不上 / 规则值写坏,一律放行;
//   - 绝不 fatal:etcd 拿不到不是启动错误,login 照常起。
//
// 所以这里**不检查 cli 是否为 nil 就直接传下去**是刻意的 —— 库里对 nil
// 的处理(打一条 Info 然后全放行)正是我们要的语义,在外面再补一个
// "拿不到 etcd 就报错返回"只会把 fail-open 改成 fail-closed。
//
// 范围与受保护的方法(docs/design/rpc-kill-switch.md §5、§8):认 type/LoginNodeService/ 与 instance/<nodeUUID>/
// 下的规则;LeaveGame 与 Disconnect 是会话收尾,只认精确规则 —— 挡住它们会让登录会话一直不释放、重登被拒,
// 断线通知(gate 合成的 58 号)也经它们到达。
func newKillSwitch(watchCtx context.Context, cfg config.Config, cli *clientv3.Client, nodeUUID string) *killswitch.Switch {
	if cfg.KillSwitch.Disabled {
		logx.Error("SECURITY/OPS WARNING: RPC kill switch explicitly disabled by configuration (KillSwitch.Disabled=true), " +
			"individual methods cannot be disabled through etcd within seconds during incidents; deployment required")
		return nil
	}

	ks := killswitch.New(killswitch.Config{
		Prefix:        cfg.KillSwitch.Prefix,
		StaleAfter:    cfg.KillSwitch.StaleAfter,
		ResyncBackoff: cfg.KillSwitch.ResyncBackoff,
		NodeTypes:     []string{login_proto.ENodeType_name[int32(nodeType)]},
		Protected: []string{
			login_proto_login.ClientPlayerLogin_LeaveGame_FullMethodName,
			login_proto_login.ClientPlayerLogin_Disconnect_FullMethodName,
		},
	})
	ks.AddInstance(nodeUUID)
	ks.Start(watchCtx, cli)
	return ks
}

// startServer configures and starts the gRPC server.
func startServer(watchCtx context.Context, cfg config.Config, ctx *svc.ServiceContext,
	etcdCli *clientv3.Client, nodeUUID string) error {
	rpclogging.ProtectCredentials(&cfg.RpcServerConf)
	server := zrpc.MustNewServer(cfg.RpcServerConf, func(grpcServer *grpc.Server) {
		login_proto_login.RegisterClientPlayerLoginServer(grpcServer, loginserver.NewClientPlayerLoginServer(ctx))
		login_proto_login.RegisterLoginAdminServer(grpcServer, loginadminserver.NewLoginAdminServer(ctx))
		login_proto_login.RegisterLoginPreGateServer(grpcServer, loginpregateserver.NewLoginPreGateServer(ctx))

		if cfg.Mode == service.DevMode || cfg.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})

	// 必须在 MustNewServer 之后:它内部的 SetUp → prometheus.StartAgent 才打开 go-zero 指标的
	// 全局开关,在那之前写的样本会被直接丢弃(理由与告警口径见 PrimeCreatePlayerMetrics)。
	loginlogic.PrimeCreatePlayerMetrics()
	loginlogic.PrimeEnterGameMetrics()

	// 拦截器顺序有讲究(go-zero 把它们交给 grpc.ChainUnaryInterceptor,
	// **排在前面的在外层、先执行**;注意 recover/timeout/stat 等 go-zero
	// 内建中间件在 MustNewServer 里就已经排在我们前面了,动不了):
	//   ① 热关停闸门(killswitch)—— 见下面的取舍论证;
	//   ② 身份闸门 —— 后面的观测层不该看到未认证请求带来的身份;
	//   ③ serverbase 把 in-band 业务码翻成日志 + 指标(本仓 handler 一律
	//      `return resp, nil`,失败塞在响应体里,不挂这层监控上全是"成功");
	//   ④ grpcstats 记调用量与耗时。
	//
	// login 的响应用的是 TipInfoMessage 这套 tip 码表,serverbase 默认就按
	// TipVerdict 定性,不需要额外传 ErrorCodeClassifier。
	//
	// ── 为什么热关停放在验签**之前** ──────────────────────────────────
	// 结论:放在最外层。理由与逐条证伪如下(这是安全相关的位置选择,
	// 改动前请先把这三条推翻)。
	//
	// 1) 止血阀挂在负载后面就止不了血。热关停存在的唯一意义是线上出事时
	//    秒级掐掉一个方法;而"出事"往往正是某条链路在烧 CPU / 涨内存。
	//    验签这一层自己就有成本:HMAC 计算 + nonce 表插入(上限
	//    InternalAuth.MaxNonceEntries,默认 50 万条常驻内存)。若把闸门放在
	//    验签之后,那么当 nonce 表正是被打爆的那一块时,关停规则救不了它
	//    —— 每个被"关停"的请求仍然先往表里写一条。
	//
	// 2) 它是**只拒不放**的闸门,不可能削弱后面任何一层。killswitch 的
	//    拦截器只有两种出口:命中 deny 规则 → 直接返回错误;否则原样
	//    handler(ctx, req) 交给下一层。它不改 ctx、不动 metadata、不写
	//    任何身份,后面的验签逻辑收到的东西与没有这一层时逐字节相同。
	//    也就是说,它无法让任何一个本该被②拒掉的请求通过。
	//
	// 3) "未鉴权方能借此探测方法是否存在"这条顾虑在 login 不成立:
	//    - ②不是全局鉴权层。按 loginauth.UnaryServerInterceptor 的不变量①,
	//      它只校验**带了** x-session-detail-bin 的调用;不带身份声明的调用
	//      (Java Gateway 的 /api/login 新链路)本来就原样直达 handler。
	//      login 是登录入口,天然对外接受未鉴权调用 —— 这里没有"先鉴权
	//      才能知道方法存在"这一层遮蔽可言。
	//    - 未注册的方法由 gRPC 运行时在**进入拦截器链之前**就回
	//      Unimplemented(grpc-go server.go: 服务/方法查不到时直接
	//      WriteStatus,processUnaryRPC 根本不会被调用),方法存在性从来
	//      就不是秘密。
	//    - 因此①多暴露的信息只有一条:"这个方法此刻被运维关停了"。而这
	//      恰恰是要主动告诉调用方、好让它别再重试的信息(denyMessage 里
	//      还刻意带上了原因文本)。
	//
	// 代价写在这里备查:被①短路的请求不进③④的指标,关停期间该方法在常规
	// 面板上 QPS 直接归零;想看还有多少流量在撞墙,看 killswitch_blocked_total。
	interceptors := make([]grpc.UnaryServerInterceptor, 0, 5)
	interceptors = append(interceptors, func(callCtx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		done, err := ctx.EnterRoot()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "维护排空入口已关闭")
		}
		defer done()
		return handler(callCtx, req)
	})
	if ks := newKillSwitch(watchCtx, cfg, etcdCli, nodeUUID); ks != nil {
		interceptors = append(interceptors, ks.UnaryServerInterceptor())
	}
	// LoginAdmin 的口令闸门排在身份闸门前面:身份闸门只校验带了身份声明的调用,挡不住运维接口。
	adminToken := os.Getenv(loginadminserver.AdminTokenEnv)
	if adminToken != "" && len(adminToken) < loginadminserver.MinAdminTokenBytes {
		return fmt.Errorf("%s is shorter than %d bytes", loginadminserver.AdminTokenEnv, loginadminserver.MinAdminTokenBytes)
	}
	sessionInterceptor, err := newSessionInterceptor(cfg, os.Getenv)
	if err != nil {
		return err
	}
	interceptors = append(interceptors,
		loginadminserver.AdminAuthInterceptor(adminToken),
		sessionInterceptor,
		// 请求顶层带 player_id 的方法,handler 与 serverbase 的日志都带上它(docs/design/distributed-tracing.md §6.2)。
		tracing.PlayerFieldInterceptor(),
		serverbase.UnaryInterceptor(serverbase.Options{}),
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),
	)
	server.AddUnaryInterceptors(interceptors...)

	defer server.Stop()

	// Start the gRPC server
	fmt.Println("\n=============================================================")
	fmt.Println("  LOGIN SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", cfg.ListenOn)
	fmt.Printf("  Mode:        %s\n", cfg.Mode)
	fmt.Printf("  zone_id:     %d\n", cfg.Node.ZoneId)
	if len(cfg.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", cfg.Etcd.Hosts)
	}
	fmt.Printf("  kafka:       %v\n", cfg.Kafka.Brokers)
	fmt.Printf("  redis:       %s\n", cfg.Node.RedisClient.Host)
	// 控制集群(gate 标记;没配 Node.QueueRedis 时也承载排队与放行锁):没写 ControlRedis.Addrs 时就是上一行那个实例。
	controlRedisConf := cfg.EffectiveControlRedis()
	fmt.Printf("  control redis: %s\n", controlRedisConf.Describe())
	// 排队与放行锁所在实例(G4a 第 1a 批):没配 Node.QueueRedis 时与上一行共用同一个句柄。
	if cfg.Node.QueueRedis != nil {
		fmt.Printf("  queue redis: %s\n", cfg.Node.QueueRedis.Host)
	} else {
		fmt.Printf("  queue redis: shared with control redis\n")
	}
	playerRedisConf, loginRedisConf := cfg.EffectivePlayerRedis(), cfg.EffectiveLoginRedis()
	fmt.Printf("  player redis: %s\n", playerRedisConf.Describe())
	fmt.Printf("  login redis:  %s\n", loginRedisConf.Describe())
	fmt.Println("=============================================================")
	server.Start()

	return nil
}

// splitHostPort splits an address into host and uint32 port.
func splitHostPort(address string) (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, uint32(portInt), nil
}

func runMaintenanceLogin(ctx context.Context, runtime *drainproof.Runtime, service *svc.ServiceContext) error {
	source, relay := drainproof.New(), drainproof.New()
	sourceWritten := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		sourceRequested, err := runtime.Requested("SOURCE")
		if err != nil {
			return err
		}
		if sourceRequested && !sourceWritten {
			stopCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			digest, err := service.SourceFrontier(stopCtx)
			cancel()
			if err != nil {
				return err
			}
			if err = source.MergeSettled(digest); err != nil {
				return err
			}
			source.CloseAdmission()
			if err = runtime.Write("SOURCE", source); err != nil {
				return err
			}
			sourceWritten = true
		}
		relayRequested, err := runtime.Requested("RELAY")
		if err != nil {
			return err
		}
		if relayRequested {
			if !sourceWritten {
				return errors.New("login RELAY关闭先于SOURCE")
			}
			stopCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			digest, err := service.RelayFrontier(stopCtx)
			cancel()
			if err != nil {
				return err
			}
			if err = relay.MergeSettled(digest); err != nil {
				return err
			}
			relay.CloseAdmission()
			return runtime.Write("RELAY", relay)
		}
	}
}
