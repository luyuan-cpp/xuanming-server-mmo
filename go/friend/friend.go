// friend 服务 F1:好友申请 / 通过 / 拒绝 / 删除 / 列表 / 待处理列表(客户端经 gate → 路由服到达),
// 以及 F2 批将补齐的拉黑与推荐。
//
// 全局一份、多副本、进程无状态:数据在独占库 mmorpg_friend(port-decisions D-14),跨运行时契约 key
// (player:session:{id})读共享 Redis。注册按 C++ NodeInfo 约定写进 etcd(shared/noderegistry),
// 路由服按 FriendNodeService.rpc 前缀发现本服务(契约 microservice-zone-contract §2/§3)。
//
// 业务代码不读 cfg.ZoneId 做分支(契约:全局服务):ZoneId 只影响 etcd 注册路径。
//
// 两种运行形态:
//   - 常驻服务(默认):建表 / 查表 → metrics → killswitch → gRPC → 注册 etcd;
//     本服务不领任何号段、不连 data_service(好友关系的主键就是两个 player_id,没有自增身份要发)。
//   - `-migrate`:只连 MySQL 跑一次 schemamigrate.Up,按 D-14 退出码退出(K8s friend-migrate Job 用),
//     不起 gRPC、不连 etcd。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"friend/internal/config"
	"friend/internal/constants"
	"friend/internal/data"
	"friend/internal/lifecycle"
	"friend/internal/logic"
	"friend/internal/metrics"
	"friend/internal/server"
	"friend/internal/session"
	"friend/internal/svc"

	base "proto/common/base"
	friendpb "proto/friend"

	"schemamigrate"

	"shared/buildinfo"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/noderegistry"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	configFile = flag.String("f", "etc/friend.yaml", "the config file")

	// migrateOnly 是生产用的显式建表入口(D-14 第 4 条):`friend -f <yaml> -migrate` 只对 mmorpg_friend
	// 跑一次 schemamigrate.Up(与 Schema.AutoMigrate=true 的启动路径是同一份表清单与选项),跑完即退出。
	// 退出码按 schemamigrate.ExitCode:0 成功 / 1 错误 / 3 锁被占(K8s Job 按 backoff 重试)/ 4 需人工。
	migrateOnly = flag.Bool("migrate", false, "run schemamigrate.Up on mmorpg_friend and exit (0 ok / 1 error / 3 lock busy / 4 manual)")
	// 改列授权只属于显式迁移,不得被常驻服务启动期的 AutoMigrate 继承。
	allowModify = flag.Bool("allow-modify", false, "仅与 -migrate 一起使用:人工确认影响后允许 MODIFY COLUMN;常驻启动禁止使用")

	showVersion = flag.Bool("version", false, "打印版本信息并退出")
)

// nodeType 本服务的节点类型。FriendNodeService=27 早已在 node.proto 里,proto 不改。
const nodeType = base.ENodeType_FriendNodeService

const (
	// listenProbeTimeout:等本进程 gRPC 端口可连的上限。先探端口再注册,是为了不让路由服
	// 在监听建立前就选中本节点(拨号失败 = 客户端收到 kServiceUnavailable)。与 chat / trade 同值。
	listenProbeTimeout = 30 * time.Second
	// registerTimeout:注册整体(探端口 + etcd CAS)的上限,须大于 listenProbeTimeout。
	registerTimeout = 45 * time.Second
)

// migrateRemedyCommand 是 Schema.AutoMigrate=false 且库与 proto 不一致时打印给运维的补救命令。
const migrateRemedyCommand = "friend -f etc/friend.yaml -migrate"

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。-migrate 形态同样先打,迁移 Job 日志可追溯。
	fmt.Println(buildinfo.StartupLine("friend"))
	if *showVersion {
		return
	}
	if err := validateMigrationFlags(*migrateOnly, *allowModify); err != nil {
		fmt.Fprintf(os.Stderr, "[friend] %v\n", err)
		os.Exit(schemamigrate.ExitFailed)
	}

	var c config.Config
	// conf.MustLoad 会自动调用 (*Config).Validate(go-zero v1.10.0),不合法即 Fatalf 退出,理由同 trade.go。
	conf.MustLoad(*configFile, &c)

	if *migrateOnly {
		os.Exit(runMigration(c, *allowModify, svc.OpenMySQL, schemamigrate.Up))
	}
	if err := runFriend(c); err != nil {
		fmt.Fprintf(os.Stderr, "[friend] %v\n", err)
		os.Exit(1)
	}
}

// validateMigrationFlags 在读配置、连接数据库之前拒绝意外的常驻改列授权。
func validateMigrationFlags(migrateOnly, allowModify bool) error {
	if allowModify && !migrateOnly {
		return errors.New("-allow-modify 必须与 -migrate 一起使用;常驻启动不允许 MODIFY COLUMN")
	}
	return nil
}

// runFriend 是常驻服务形态。启动失败与正常退出走同一条收尾(lifecycle.Shutdown),返回的 error 决定退出码。
//
// 停机(契约 §2、§9.1、§16;照 chat / trade 已活体验证的做法):
//   - Linux 上 go-zero proc 在 init 就订阅 SIGTERM/SIGINT,默认收到信号 1s 后自行 GracefulStop、5.5s 强杀,
//     完全不等本服务注销。lifecycle.Configure 把这两个时刻推迟到 24s 硬截止,正常预算内由本函数串行收尾:
//     ① nr.Close() 注销尝试返回 → ② 真实 grpc.Server.GracefulStop 排空(≤5s,超时异步 Stop 并报错)
//     → ③ 停 killswitch watch、关 Kafka / DB / etcd → ④ proc.Shutdown 与等 Start 返回(≤2s)→ 刷日志。
//   - 到 24s 仍未收尾由 watchHardDeadline 强制退出:不保证在途请求完成;注销传输失败仍靠 LeaseTTL 兜底。
func runFriend(c config.Config) (runErr error) {
	// 从启动阶段起接管退出信号:信号会打断注册;启动失败也执行同一条收尾。
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	lifecycle.Configure(&c.RpcServerConf) // 必须早于 zrpc.NewServer(其 SetUp 会再次 proc.Setup 同一份值)
	finished := make(chan struct{})
	defer close(finished)
	go watchHardDeadline(ctx, stopSignals, finished)
	if exitRequested(ctx) {
		return nil // 配置加载期间已收到退出:不建依赖、不注册
	}

	listenHost, port, err := c.ListenHostPort()
	if err != nil {
		return err
	}

	svcCtx := svc.NewServiceContext(c)

	// 收尾所有权从这里开始:之后任何 return 都会 注销(若已注册)→ 排空(若已起服)→ 关资源 → 框架收尾。
	stopKillSwitch := func() {}
	var serverSlot lifecycle.ServerSlot
	shutdown := &lifecycle.Shutdown{
		DrainTimeout:  lifecycle.DrainTimeout,
		FinishTimeout: lifecycle.FinishTimeout,
		// killswitch watch 与注销共用 svcCtx 的 etcd 客户端,必须在 svcCtx.Stop 关连接之前停。
		CloseResources:  func() { stopKillSwitch(); svcCtx.Stop() },
		FinishFramework: proc.Shutdown,
	}
	defer func() {
		stopSignals() // 启动失败时也从这里触发同一个 24s 硬截止
		shutdown.Server = serverSlot.TakeForShutdown()
		if err := shutdown.Stop(); err != nil {
			logx.Errorf("[friend] 停机收尾未按预算完成: %v", err)
			runErr = errors.Join(runErr, err)
		}
		_ = logx.Close()
	}()

	// 建表 / 查表(D-14 第 4 条)。放在起 gRPC 之前:表不对的 friend 一个请求都不该接。
	// 不设整体超时、也不接退出信号:schemamigrate 自带锁等待与每条语句硬超时;中途取消 DDL 可能留下 dirty 台账。
	if err := ensureSchema(context.Background(), svcCtx.DB, c, schemamigrate.Up, schemamigrate.Plan); err != nil {
		return err
	}
	if exitRequested(ctx) {
		return nil
	}

	deps := logic.NewDeps(svcCtx)

	// Prometheus /metrics(MetricsListenAddr 为空则不开)。指标本身是懒注册的,不调 Start 也能安全 Inc。
	metrics.Start(c.MetricsListenAddr)

	// friend_request 终态行的后台清理循环。接法按 internal/logic/sweep.go 顶部注释给的位置:
	// `deps := logic.NewDeps(svcCtx)` 之后、metrics.Start 附近 —— 放在 metrics.Start 之后,
	// 是为了让第一轮刷出的 friend_sweep_pending_rows 一开始就能被抓到。
	//
	// ⚠ 默认 Sweep.Mode = report_only:接上之后**不会删任何数据**,只 COUNT + 打 WARN + 刷 gauge。
	// 真删必须显式把配置改成 delete;当前模式与间隔在下面的启动横幅 "sweep:" 那一行,运维一眼可见。
	//
	// ctx 就是本函数顶部 signal.NotifyContext 的那个:收到 SIGTERM 即 cancel,循环自行退出。
	// **不新建 context.Background()** —— 那样的循环在停机时根本不会退,只能等 24s 硬截止强杀。
	//
	// 停机**不等它退出**(直接丢下):sweep 是幂等的批量 DELETE,中途被打断只是少删一批,
	// 下一个周期接着删;为它加一步停机等待反而要挤占那 24s 硬预算(契约 §9.1),代价与收益不成比例。
	// 代价是收尾关 DB / Redis 时,在途的那一轮可能留下一条查询失败日志 —— 刻意接受的噪声,不是缺陷。
	logic.StartSweep(ctx, deps)

	// RPC 级热关停(shared/killswitch):复用 svcCtx 的 etcd 客户端;Start 非阻塞,etcd 不可达一律放行。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	stopKillSwitch = ksCancel
	ks := killswitch.New(killswitch.Config{Prefix: c.KillSwitchPrefix})
	ks.Start(ksCtx, svcCtx.Etcd)

	serverReady := make(chan struct{})
	s, err := zrpc.NewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		// 只挂一个服务:ClientPlayerFriend 标了 OptionIsClientProtocolService=true,
		// 客户端来源的方法级准入由 internal/session 的白名单负责(S2C 的 NotifyFriendEvent 必须挡住)。
		friendpb.RegisterClientPlayerFriendServer(grpcServer, server.NewFriendServer(deps))
		if config.IsRelaxedMode(c.Mode) {
			reflection.Register(grpcServer)
		}
		// 把真实 grpc.Server 交给退出线程:go-zero 的 RpcServer.Stop() 只 logx.Close(),主动排空只能调它。
		serverSlot.Publish(grpcServer)
		close(serverReady)
	})
	if err != nil {
		return fmt.Errorf("friend RPC 构造失败: %w", err)
	}
	// go-zero 自带拦截器在 NewServer 里已先加入,位于下面这条链的外层。
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks)...)

	// **先起 gRPC,再注册**(契约 §2)。Start 在监听失败时 panic:recover 成错误交回主线程,
	// 走同一条收尾后以非 0 退出(进程拉起方 / K8s 仍可见),不带着已创建的资源直接崩。
	serveDone := make(chan struct{})
	serveErr := make(chan error, 1)
	shutdown.ServeDone = serveDone
	go func() {
		var startErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				startErr = fmt.Errorf("gRPC Start 失败: %v", recovered)
			}
			serveErr <- startErr
			close(serveDone)
		}()
		s.Start()
	}()
	select {
	case <-serverReady:
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		return fmt.Errorf("friend gRPC 监听未就绪: %v", err)
	}

	advertiseHost := advertisedHost(listenHost)
	regCtx, regCancel := context.WithTimeout(ctx, registerTimeout)
	nr, err := noderegistry.RegisterAfterListening(regCtx, svcCtx.Etcd, dialAddress(listenHost, port), listenProbeTimeout,
		noderegistry.Spec{
			// Prefix 由枚举名派生,不手写(契约 §2):"FriendNodeService.rpc"。
			Prefix:     base.ENodeType_name[int32(nodeType)] + ".rpc",
			NodeType:   uint32(nodeType),
			ZoneId:     c.ZoneId,
			LeaseTTL:   c.LeaseTTL,
			BuildValue: nodeInfoValueBuilder(c.ZoneId, advertiseHost, port, uint64(time.Now().Unix())),
			// friend 不以 node_id 派生任何持久身份(好友关系的主键是两个 player_id,没有 snowflake),
			// 失租后换 id 继续服务即可(D-11)。
			OnReclaimFailed: noderegistry.ReallocateNewID,
			OnNodeIDChanged: func(oldID, newID uint32) {
				logx.Errorf("[friend] etcd 失租后原 node_id=%d 未能重夺,已换新 node_id=%d 继续服务", oldID, newID)
			},
			LogPrefix: "[friend]",
		})
	regCancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil // 注册期间收到退出信号:按正常退出收尾
		}
		return fmt.Errorf("friend 节点注册 etcd 失败: %w", err)
	}
	shutdown.Unregister = nr.Close
	nr.KeepAlive()

	fmt.Println("\n=============================================================")
	// 这一行的字面量是 tools/scripts/go_services.ps1 的就绪判据(它在启动日志里找
	// "STARTED SUCCESSFULLY"),改字就等于让启动器永远等不到 friend 就绪。
	fmt.Println("  FRIEND SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:        %s\n", c.ListenOn)
	fmt.Printf("  Advertise:     %s\n", net.JoinHostPort(advertiseHost, strconv.FormatUint(uint64(port), 10)))
	fmt.Printf("  Mode:          %s\n", c.Mode)
	fmt.Printf("  etcd:          %v\n", c.Etcd.Hosts)
	fmt.Printf("  node_id:       %d (etcd CAS)\n", nr.NodeID())
	fmt.Printf("  node_uuid:     %s\n", nr.NodeUUID)
	fmt.Printf("  zone:          %d (只影响注册路径)\n", c.ZoneId)
	// MySQLTarget 只含 host / 库名 / 用户名,绝不含密码。
	fmt.Printf("  mysql:         %s\n", svc.MySQLTarget(c.MySQL))
	fmt.Printf("  schema:        %s\n", schemaModeLabel(c))
	// 两个 Redis 句柄不同源时必须一眼看出来:FriendRedis 未配置会回落到共享库,
	// 那种形态下 friend 的私有 key 与 player:session:{id} 挤在同一个实例上。
	fmt.Printf("  friend_redis:  %s\n", svcCtx.FriendRedisTarget)
	fmt.Printf("  quota:         friends=%d blocks=%d pending_out=%d pending_in=%d\n",
		c.Friend.MaxFriends, c.Friend.MaxBlocks, c.Friend.MaxPendingRequests, c.Friend.MaxIncomingRequests)
	fmt.Printf("  sweep:         mode=%s interval=%v retention=%dd\n",
		c.Friend.Sweep.Mode, c.Friend.Sweep.Interval, c.Friend.Sweep.RetentionDays)
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:       %s\n", c.MetricsListenAddr)
	}
	fmt.Println("=============================================================")

	select {
	case <-ctx.Done():
		logx.Info("[friend] 收到退出信号,先注销再排空 RPC")
	case err := <-serveErr:
		return fmt.Errorf("friend gRPC 服务提前退出: %v", err)
	}
	return nil // 收尾在 defer 里按 注销 → 排空 → 关资源 → 框架收尾 执行
}

// watchHardDeadline 从进入退出(本进程收到信号、启动失败触发 stopSignals,或 go-zero proc 先于
// NotifyContext 收到的信号)开始计 lifecycle.HardTimeout,到点仍未收尾就强制退出。
// finished 在 runFriend 返回时关闭。go-zero 在 24s 时会给自己补发一次信号,但本进程的
// NotifyContext 若仍在订阅会吞掉它,所以不能指望框架强杀,必须自己兜底。
func watchHardDeadline(ctx context.Context, stopSignals context.CancelFunc, finished <-chan struct{}) {
	select {
	case <-finished:
		return
	case <-ctx.Done():
	case <-proc.Done():
		// proc 在 init 已接管信号;补收配置加载期发生、NotifyContext 未看到的退出。Windows 上是 nil 通道。
		stopSignals()
	}
	timer := time.NewTimer(lifecycle.HardTimeout)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		fmt.Fprintf(os.Stderr, "[friend] 已到 %v 停机硬截止,强制退出;不能确认在途请求已完成\n", lifecycle.HardTimeout)
		os.Exit(1)
	}
}

// exitRequested 报告是否已经收到退出(本进程 ctx 或 go-zero proc),用于在建依赖 / 起服 / 注册之前短路。
func exitRequested(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-proc.Done():
		return true
	default:
		return false
	}
}

// buildUnaryInterceptors 组装本服务的一元拦截器链。返回的切片顺序**就是执行顺序**(第一个是最外层)。
// 抽成函数是为了让链本身可测(friend_test.go)——
// main() 无法在单测里跑起来,而"哪次重构顺手把 killswitch / session 那行删了"是最容易发生、
// 又最难被发现的回归:删掉之后一切照常工作,只有真出事那天才发现止血阀 / 准入是假的。
func buildUnaryInterceptors(ks *killswitch.Switch) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计最外层:被热关停短路、被会话准入拒绝的请求也要被统计到。
		//    否则"关停生效后这个方法的 QPS 归零"会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中后会话解码、in-band 定性、Redis / MySQL 访问统统不做。
		//    被关停的调用不进 rpc_duration_seconds —— 它们的账记在 killswitch_blocked_total{method} 上。
		ks.UnaryServerInterceptor(),

		// ③ 会话与方法准入(D-9):无会话 = 内部调用放行;坏会话 → Unauthenticated;
		//    带会话调白名单外方法(S2C 的 NotifyFriendEvent)→ PermissionDenied。放在 serverbase 之前:
		//    被拒绝的调用没进业务链路,不计入业务耗时。
		session.UnaryServerInterceptor(session.ClientMethods),

		// ④ in-band 故障定性:handler 一律 `return resp, nil`,这层读 TipInfoMessage.id 按 Tip.xlsx 的
		//    fault 列定性(kServiceUnavailable = 故障告警,其余 = 业务拒绝)。紧贴 handler。
		serverbase.UnaryInterceptor(serverbase.Options{
			TipClassifier: constants.TipClassifier(),
		}),
	}
}

// schemaRunner 是 schemamigrate.Up / schemamigrate.Plan 的函数形状;抽出来让启动期建表策略可单测。
type schemaRunner func(ctx context.Context, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error)

// schemaOptions 是启动路径与 -migrate 共用的迁移选项:库名断言用代码常量(Validate 已保证配置与之相等),
// 表清单只来自 data.Tables();锁等待 / 语句超时取 schemamigrate 默认值。
func schemaOptions() schemamigrate.Options {
	return schemamigrate.Options{
		Database: data.DatabaseName,
		Tables:   data.Tables(),
		Logf:     logx.Infof,
	}
}

// ensureSchema 是启动期建表策略(D-14 第 4 条):
//   - Schema.AutoMigrate 没写或 true:跑 Up;err 非 nil 或出现需人工项 → 返回错误(调用方拒绝启动)。
//     多副本同时起:Up 内部 GET_LOCK 串行化;等锁超时返回 ErrLockBusy,本副本拒启动后由进程拉起方重试。
//   - false:只跑只读 Plan;有待执行语句或需人工项 → 返回错误,并带上补救命令。
func ensureSchema(ctx context.Context, db *sql.DB, c config.Config, up, plan schemaRunner) error {
	opts := schemaOptions()
	if c.ShouldAutoMigrate() {
		report, err := up(ctx, db, opts)
		logReport("schemamigrate.Up", report)
		if err != nil {
			return fmt.Errorf("[friend] 启动期建表失败(库 %s),拒绝启动: %w", data.DatabaseName, err)
		}
		if len(report.Manual) > 0 {
			return fmt.Errorf("[friend] 启动期建表发现 %d 项需人工处理(库 %s),拒绝启动: %s",
				len(report.Manual), data.DatabaseName, strings.Join(report.Manual, "; "))
		}
		return nil
	}

	report, err := plan(ctx, db, opts)
	logReport("schemamigrate.Plan", report)
	if err != nil {
		return fmt.Errorf("[friend] Schema.AutoMigrate=false,启动期检查表结构失败(库 %s),拒绝启动;确认库可达后执行 %s: %w",
			data.DatabaseName, migrateRemedyCommand, err)
	}
	if !report.Clean() {
		return fmt.Errorf("[friend] Schema.AutoMigrate=false 且库 %s 与 proto/friend/friend_table.proto 不一致"+
			"(待执行 %d 条、需人工 %d 项),拒绝启动;先执行 %s",
			data.DatabaseName, len(report.Statements), len(report.Manual), migrateRemedyCommand)
	}
	return nil
}

// runMigration 是 -migrate 的实现:只连 MySQL,不起 gRPC、不连 etcd。
// allowModify 只接收本次显式迁移的命令行授权,启动期 ensureSchema 不使用它。
// 输出走 stdout / stderr,退出码按 schemamigrate.ExitCode(D-14),部署脚本与 K8s Job 直接读。
func runMigration(c config.Config, allowModify bool, openMySQL func(config.MySQLConf) (*sql.DB, error), up schemaRunner) int {
	fmt.Printf("friend schema migration: %s\n", svc.MySQLTarget(c.MySQL))
	db, err := openMySQL(c.MySQL)
	if err != nil {
		code := schemamigrate.ExitCode(schemamigrate.Report{}, err)
		fmt.Fprintf(os.Stderr, "schema migration FAILED (exit %d): %v\n", code, err)
		return code
	}
	defer func() { _ = db.Close() }()

	opts := schemaOptions()
	opts.AllowModifyColumn = allowModify
	report, err := up(context.Background(), db, opts)
	printReport(os.Stdout, report)
	code := schemamigrate.ExitCode(report, err)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "schema migration FAILED (exit %d): %v\n", code, err)
	case code != 0:
		fmt.Fprintf(os.Stderr, "schema migration needs manual action (exit %d): %d item(s) listed above\n", code, len(report.Manual))
	default:
		fmt.Printf("schema migration OK: %s synced from proto/friend/friend_table.proto (%d statement(s) executed)\n",
			data.DatabaseName, len(report.Statements))
	}
	return code
}

// reportLines 把迁移报告展开成可读行(Statements / Warnings / Manual 各一段)。
func reportLines(r schemamigrate.Report) []string {
	var lines []string
	for _, s := range r.Statements {
		lines = append(lines, "  statement: "+s)
	}
	for _, w := range r.Warnings {
		lines = append(lines, "  warning:   "+w)
	}
	for _, m := range r.Manual {
		lines = append(lines, "  MANUAL:    "+m)
	}
	return lines
}

func printReport(w io.Writer, r schemamigrate.Report) {
	for _, line := range reportLines(r) {
		fmt.Fprintln(w, line)
	}
}

// logReport 把启动期迁移报告写进日志:需人工项与告警用 Error,待执行 / 已执行语句用 Info。
func logReport(stage string, r schemamigrate.Report) {
	for _, s := range r.Statements {
		logx.Infof("[friend] %s statement: %s", stage, s)
	}
	for _, w := range r.Warnings {
		logx.Errorf("[friend] %s warning: %s", stage, w)
	}
	for _, m := range r.Manual {
		logx.Errorf("[friend] %s MANUAL: %s", stage, m)
	}
}

// schemaModeLabel 是横幅里的建表策略描述。
func schemaModeLabel(c config.Config) string {
	if c.ShouldAutoMigrate() {
		return "auto-migrate (schemamigrate.Up at startup)"
	}
	return "plan-only (tables owned by " + migrateRemedyCommand + ")"
}

// nodeInfoValueBuilder 返回 noderegistry 的 BuildValue 回调:按 C++ NodeInfo 约定生成 protojson。
// shared 不 import proto 模块(契约 §2),所以 NodeInfo 由调用方拼;字段填法与 chat.go / trade.go 相同。
// Endpoint 与 GrpcEndpoint **必须都填**:只填 Endpoint 时路由服拨不通(guild 踩过),
// friend_test.go 的 TestNodeInfoValueMatchesRegistryContract 钉住这一点。
func nodeInfoValueBuilder(zoneId uint32, host string, port uint32, launchTime uint64) func(nodeID uint32, nodeUUID string) ([]byte, error) {
	return func(nodeID uint32, nodeUUID string) ([]byte, error) {
		info := &base.NodeInfo{
			NodeId:       nodeID,
			NodeType:     uint32(nodeType),
			ZoneId:       zoneId,
			NodeUuid:     nodeUUID,
			LaunchTime:   launchTime,
			ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC),
			Endpoint:     &base.EndpointComp{Ip: host, Port: port},
			GrpcEndpoint: &base.EndpointComp{Ip: host, Port: port},
		}
		return protojson.Marshal(info)
	}
}

// dialAddress 是注册前"探端口"用的拨号地址:监听 0.0.0.0 / :: 时本机只能拨 loopback。
func dialAddress(listenHost string, port uint32) string {
	host := listenHost
	if isUnspecifiedHost(host) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))
}

// isUnspecifiedHost:""、0.0.0.0、:: 这类"监听所有网卡"的 host,不能原样写进 NodeInfo。
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// advertisedHost 决定写进 NodeInfo 的 IP(与 chat.go / trade.go advertisedHost 同一口径):
// POD_IP 优先 → ListenOn 的具体 host → 本机第一个非 loopback IP → 127.0.0.1。
// 顺序颠倒的后果:K8s 里通告了 Pod 内看到的 0.0.0.0 / 节点 IP,路由服拨到别的副本上。
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
