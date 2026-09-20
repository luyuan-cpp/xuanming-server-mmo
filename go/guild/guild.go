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
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"guild/internal/config"
	"guild/internal/constants"
	"guild/internal/data"
	"guild/internal/logic"
	"guild/internal/node"
	"guild/internal/server"
	"guild/internal/session"
	"guild/internal/svc"
	base "proto/common/base"
	pb "proto/guild"
	"schemamigrate"
	"shared/buildinfo"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/safego"
	"shared/serverbase"
	"shared/snowflakealloc"
)

var configFile = flag.String("f", "etc/guild.yaml", "config file path")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

// migrateOnly:`guild -f <yaml> -migrate` 只对 mmorpg_guild 跑一次 schemamigrate.Up 后退出
// (退出码 0 成功 / 1 错误 / 3 锁忙 / 4 需人工),不连 Redis / etcd / data_service,供将来的
// guild-migrate Job 使用(port-decisions D-14 §4,形状照 go/trade)。
var migrateOnly = flag.Bool("migrate", false, "run schemamigrate.Up on mmorpg_guild and exit (0 ok / 1 error / 3 lock busy / 4 manual)")

const migrateRemedyCommand = "guild -f etc/guild.yaml -migrate"

// 启动期等迁移锁:本地 go_services 允许 guild 多开,首次建表时实例间会互等 GET_LOCK。
// 没有编排器替我们重拉,所以在进程内重试。测试里把 lockBusyRetryDelay 置 0。
const lockBusyAttempts = 3

var lockBusyRetryDelay = 2 * time.Second

const nodeType = uint32(base.ENodeType_GuildNodeService)

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("guild"))
	if *showVersion {
		return
	}
	conf.MustLoad(*configFile, &config.AppConfig)

	// -migrate:只建表然后退出,不碰 Redis / etcd / Kafka。
	if *migrateOnly {
		os.Exit(runMigration(config.AppConfig))
	}

	svcCtx := svc.NewServiceContext(config.AppConfig)
	defer svcCtx.Stop()

	// 帮会规则表 / 等级表自洽校验(设计 docs/design/guild-phase2/02-management.md §3.3):
	// 缺行或数值越界一律拒启,**不降级成警告**。
	//
	// 理由:成员上限、长老上限、申请上限全部是"用时现查配表",查不到时这些判据会静默取到 0,
	// 表现成"谁都进不去 / 长老名额无限",而不是一条报错 —— 带着错配表活着比崩掉危险得多。
	// 位置:NewServiceContext 刚 LoadTables,所以校验只能在它之后;又放在建表与节点注册之前,
	// 配置不对的实例连 etcd 都不该注册进去,免得流量先被路由过来再出问题。
	if err := logic.ValidateGuildTables(); err != nil {
		logx.Must(fmt.Errorf("guild config tables invalid: %w", err))
	}

	// 表结构不对的实例不能对外服务,也不能注册进 etcd:建表 / 核对放在节点注册之前。
	if err := ensureSchema(context.Background(), svcCtx.DB, config.AppConfig, schemamigrate.Up, schemamigrate.Plan); err != nil {
		logx.Must(err)
	}

	// Register node with etcd
	host, port, err := splitHostPort(config.AppConfig.ListenOn)
	if err != nil {
		logx.Must(fmt.Errorf("parse listen address: %w", err))
	}

	n, err := node.NewNode(nodeType, host, port)
	if err != nil {
		logx.Must(fmt.Errorf("create node: %w", err))
	}
	defer n.Close()

	if err := n.KeepAlive(); err != nil {
		logx.Must(fmt.Errorf("keep alive: %w", err))
	}
	logx.Infof("Guild node registered: id=%d uuid=%s", n.Info.NodeId, n.Info.NodeUuid)

	// 通过 shared/snowflakealloc 独立申领 Snowflake 槽位(worker id = cluster<<12 | slot)。
	//
	// 注意:**不再用 n.Info.NodeId 当 Snowflake worker id**。
	// 理由:
	//   1. NodeInfo.NodeId 是 C++ 服务发现使用的逻辑节点编号,有可能因为 reRegister
	//      CAS 失败而变化(参考 scene_manager 的 reRegister 设计)。
	//   2. Snowflake worker id 一旦变化,可能在同一毫秒里和上一个 worker id 的 ID 序列冲突
	//      (理论上不会,但毫秒级时钟回退 + worker id 跳变是公认的潜在风险)。
	//   3. 同 hostname 优雅重启复用同槽,Snowflake 时间戳单调性更稳。
	//   4. 与 scene_manager 模式一致,降低维护成本。
	//
	// kind="guild" 与其他服务不同,槽池互相隔离;ClusterId 由运维按集群设定(默认 0)。
	// LegacyPrefix="/guild" 让 cluster 0 在滚动升级期仍读旧布局的水位 / 活 id(发布一版后可删)。
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   config.AppConfig.Registry.Etcd.Hosts,
		DialTimeout: config.AppConfig.Registry.Etcd.DialTimeout,
	})
	if err != nil {
		logx.Must(fmt.Errorf("snowflake etcd client: %w", err))
	}
	defer etcdCli.Close()

	host_name, err := os.Hostname()
	if err != nil {
		logx.Must(fmt.Errorf("hostname: %w", err))
	}
	sfCtx, sfCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sfHandle, err := snowflakealloc.AllocateWithKeepAlive(sfCtx, etcdCli, "guild", host_name, snowflakealloc.Options{
		LeaseTTL:     60,
		ClusterID:    config.AppConfig.ClusterId,
		LegacyPrefix: "/guild",
		// 缓存文件名带监听端口:同主机多实例各自一份,不互相覆盖。
		CachePath: snowflakealloc.DefaultCachePath(config.AppConfig.SnowflakeCacheDir, "guild",
			fmt.Sprintf("%s_%d", host_name, port)),
	})
	sfCancel()
	if err != nil {
		logx.Must(fmt.Errorf("snowflake worker id alloc: %w", err))
	}
	defer sfHandle.Close()
	logx.Infof("Guild snowflake %s worker_id=%d (host=%s)", sfHandle.LogFields(), sfHandle.WorkerID, host_name)

	// RPC 级热关停(shared/killswitch):线上某个方法把 DB 打爆时,往 etcd 写一个
	// key 就能秒级把它短路掉,不必走一遍构建-发布-滚动更新。
	//
	// 复用上面这个 etcdCli(snowflake worker id 分配用的那条连接),不另开第三条 ——
	// 连的是同一个集群。Start 非阻塞:客户端为 nil、etcd 连不上、前缀下没有 key,
	// 一律放行(fail-open),因此这里既不需要判错也不需要 logx.Must。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	// ⚠️ 这个 defer 必须写在 `defer etcdCli.Close()` 之后:defer 是后进先出,
	// 先取消 watch 循环,再关 etcd 客户端,否则 watch 会撞上一个已经 Close 的 client。
	defer ksCancel()
	ks := killswitch.New(killswitch.Config{Prefix: config.AppConfig.KillSwitchPrefix})
	ks.Start(ksCtx, etcdCli)

	// 用 Handle.NewNode 而不是裸 snowflake.NewNode:它会把**前任在这个 worker id 上的
	// 高水位**当地板注入(etcd 里的持久水位),顶住跨机时钟偏斜接管、本机时钟回拨、
	// 前任借过逻辑秒这三类"启动 guard 挡不住"的重号。
	sf := sfHandle.NewNode()

	// Initialize data repo with singleflight + cache-aside
	repo := data.NewGuildRepo(svcCtx.RedisClient, svcCtx.DB, config.AppConfig.Cache.DefaultTTL)

	// 每次启动从 MySQL 权威快照重建全局 / 分区榜;失败拒绝启动,避免对外提供不完整榜单。
	if err := repo.RebuildRanks(context.Background()); err != nil {
		panic(fmt.Errorf("rebuild guild ranks from MySQL: %w", err))
	}
	onlineResolver := logic.NewOnlineStatusResolver(svcCtx.PlayerLocatorRedisClient)

	// guild_id 发号策略(设计稿 §6.4):号段(svcCtx.GuildIDSegment,IdSegment.Enabled 时
	// 已在 NewServiceContext 里接好 data_service)优先;是否回退到上面这个 snowflake 节点
	// 由 IdSegment.FallbackToSnowflake 决定,默认不回退。snowflake 节点本身照旧申领 /
	// 写水位 / 失租 fence —— 它既是可选回退,也是解码存量 guild_id 的依据。
	guildIDs := svc.NewGuildIDMinter(config.AppConfig.IdSegment, svcCtx.GuildIDSegment, sf.Generate)
	svcCtx.WarmGuildIDSegment()

	// 合服闸门(可选):没配 MergeMarkerRedis 时 NewRedisMergeFence 返回 nil 指针,
	// 必须显式转成 nil **接口** 再传下去 —— 直接传一个 nil 的具体类型指针,
	// GuildLogic 里的 `l.mergeFence == nil` 会是 false,于是每次建帮都去调一个
	// 空实现,反而把"未配置"变成一条隐蔽的运行期分支。
	var mergeFence logic.MergeFence
	if f := logic.NewRedisMergeFence(svcCtx.MergeMarkerRedisClient); f != nil {
		mergeFence = f
	}
	// 帮会按 zone 隔离:客户端请求的 zone 取 data_service 的玩家归属映射(logic/home_zone.go)。
	// 没配 DataServiceRpc 时保持 nil 接口:内部调用照常,客户端请求一律按服务不可用拒绝。
	var homeZones logic.HomeZoneLookup
	if svcCtx.DataServiceClient != nil {
		homeZones = logic.NewDataServiceHomeZone(svcCtx.DataServiceClient, logic.DefaultHomeZoneLookupTimeout)
	} else {
		logx.Error("Guild: DataServiceRpc 未配置,无法判定玩家归属 zone,所有客户端帮会请求将被拒绝")
	}
	// 推送出口与"申请推送冷却"在这里一次装配(设计 §13.7 / §10.1)。
	//
	// NewGuildNotifier 自己判 nil:没配 Kafka(Brokers 为空 → KafkaWriter 为 nil)时它退回
	// NoopNotifier,所以这里不必再包一层 if。这跟上面 mergeFence 的写法不同不是疏忽 ——
	// 那边要躲的是"具体类型的 nil 指针装进接口后 != nil"这个坑,而这里拿到的已经是接口值。
	//
	// TryMarkApplyPush 是 Redis SetNX 冷却键,挡的是"申请 → 撤回 → 申请"对审批人的刷屏;
	// 不注入 = 总是放行,只有单测才会走那条默认。
	guildLogic := logic.NewGuildLogic(repo, guildIDs, onlineResolver, mergeFence, homeZones,
		logic.WithNotifier(logic.NewGuildNotifier(svcCtx.KafkaWriter, svcCtx.GateCommandBuilder, svcCtx.PlayerLocatorRedisClient)),
		logic.WithApplyPushGate(repo.TryMarkApplyPush))

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterGuildServiceServer(grpcServer, server.NewGuildServer(guildLogic))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks, config.AppConfig.RequestBudget())...)
	defer s.Stop()

	// Lost() 关闭 = 本进程**确认**不再是这个槽的持有者(slots key 被挂到了别的 uuid 上:
	// 运维手动清理 / 水位机制被绕过 / 旧版本二进制)。再用 sf 发一个公会 ID 就是确定性撞号,
	// 所以主动退出,让编排把进程拉起来 —— 重启会申领一个新槽,并以前任水位为地板。
	//
	// ⏱ 时间预算:lease 抖动 / etcd 不可达**不再**触发 Lost()(snowflakealloc 会自己 reclaim);
	// 这段时间的安全由发号器内部的水位年龄自 fence 兜住 —— 距上次水位写成功超过 F(2h)
	// Generate 返回 ErrWatermarkStale(暂态,写成功即恢复),建帮整体失败但进程不退出。
	//
	// ⚠️ 顺序不能反,而且**不能只调 s.Stop()**:go-zero 的 zrpc.RpcServer.Stop() 实测
	// (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),既不拒新请求也不排空在途 ——
	// 靠它"停服"等于什么都没做,进程会带着已失效的 worker id 一直服务下去。
	// 所以正确性由 Fence() 保证(之后 Generate 一律 ErrFenced,建帮整体失败),
	// 可用性由进程退出 + 编排重拉保证。
	//
	// 用 safego.Go 而不是裸 `go func`:这条看门狗一旦 panic(比如 Lost() 通道被
	// 重复关闭),裸 goroutine 会把整个进程当场打死,日志里只剩一段 runtime 栈;
	// safego 兜住后会打稳定事件名 + 计 safego_panic_total{point="guild.snowflake_fence_watch"},
	// 看门狗失效这件事变成可告警的,而不是伪装成一次"正常"的进程退出。
	safego.Go("guild.snowflake_fence_watch", func() {
		<-sfHandle.Lost()
		sf.Fence() // ① 先关闸:此后一个号都发不出去,撞号从机制上不可能
		logx.Error("Guild snowflake worker id lease lost; generator fenced, exiting to let the orchestrator restart " +
			"(zrpc Stop() only closes the logger and cannot stop serving)")
		logx.Close() // ② 冲掉日志缓冲,别把上面这条 ERROR 丢了
		os.Exit(1)   // ③ 退出;重启后拿新租约,并被 snowflake 的启动 guard 兜住
	})

	logx.Infof("Starting Guild RPC server at %s...", config.AppConfig.ListenOn)
	s.Start()
}

// buildUnaryInterceptors 组装本服务的一元拦截器链。
//
// 返回的切片顺序**就是执行顺序**:go-zero 把它们原样交给
// grpc.ChainUnaryInterceptor,第一个是最外层。
//
// 抽成函数而不是直接在 main 里连着调 AddUnaryInterceptors,是为了让链本身可测 ——
// main() 没法在单测里跑起来,而"哪次重构顺手把 killswitch 那行删了"是最容易发生、
// 又最难被发现的回归:开关删掉之后一切照常工作,只有真出事那天才发现止血阀是假的。
// 见 guild_test.go。
func buildUnaryInterceptors(ks *killswitch.Switch, budget time.Duration) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计放最外层:被热关停短路掉的请求也必须被统计到。
		//    否则"关停生效后这个方法的 QPS 归零"会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中之后 in-band 定性、handler、
		//    以及 handler 里的 Redis / MySQL / 发号器访问统统不做 —— 止血阀的全部意义
		//    就是"别为一个已经关停的方法做任何无谓的工作"。
		//    它放在 serverbase 之前也意味着被关停的调用不进 rpc_duration_seconds:
		//    那条指标衡量的是业务链路,而关停请求根本没走业务链路,
		//    它们的账记在 killswitch_blocked_total{method} 上。
		ks.UnaryServerInterceptor(),

		// ③ 会话与方法准入(internal/session):客户端来源的身份只认 gate 注入的会话,
		//    内部方法(UpdateGuildScore)对客户端一律 PermissionDenied。放在热关停之后:
		//    被关停的方法不必解码会话;放在 serverbase 之前:被拒绝的调用没进业务链路,
		//    不该计入 rpc_duration_seconds(grpcstats 仍统计到它们)。
		session.UnaryServerInterceptor(session.ClientMethods),

		// ④ in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把失败塞进
		//    响应体的 TipInfoMessage —— gRPC status 恒 OK,go-zero 自带的指标拦截器会把
		//    每一次「发号器被 fence」都记成一次成功请求。这层把响应体里的码读出来定性,
		//    故障打日志 + 计数,业务拒绝只计数。它不改响应内容、不吞错,对客户端无感。
		//    段归属、已分配范围、哪个码算故障(发号器被 fence)全部由 Tip.xlsx 生成的
		//    全局段表 + 故障表统一判定;TipClassifier 只是本服务固定下来的定性接缝。
		serverbase.UnaryInterceptor(serverbase.Options{
			TipClassifier: constants.TipClassifier(),
		}),

		// ⑤ 整请求业务预算(Timeout − 500ms):让归属区查询、发号、Redis / MySQL 这些 I/O
		//    在 zrpc 服务端超时之前失败,客户端拿到的是 in-band tip 而不是 DeadlineExceeded。
		//    放最内层:前四层都是本地判断,不该占业务预算。
		requestBudgetInterceptor(budget),
	}
}

// requestBudgetInterceptor 给 handler 的 ctx 套上整请求业务预算(与 go/trade 的 RequestBudget 同义)。
func requestBudgetInterceptor(budget time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return handler(ctx, req)
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

// runWithLockRetry 在 GET_LOCK 被别的实例占住时重试。本地 go_services 允许 guild 多开,
// 首次建表时两个实例会互等;没有编排器替我们重拉,所以在进程内重试。
// -migrate 入口刻意不用它:退出码 3 交给 K8s Job 的 backoff。
func runWithLockRetry(ctx context.Context, runner schemaRunner, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error) {
	var report schemamigrate.Report
	var err error
	for attempt := 1; attempt <= lockBusyAttempts; attempt++ {
		report, err = runner(ctx, db, opts)
		if !errors.Is(err, schemamigrate.ErrLockBusy) || attempt == lockBusyAttempts {
			return report, err
		}
		logx.Infof("[guild] 迁移锁忙,%v 后重试(%d/%d)", lockBusyRetryDelay, attempt, lockBusyAttempts)
		select {
		case <-time.After(lockBusyRetryDelay):
		case <-ctx.Done():
			return report, ctx.Err()
		}
	}
	return report, err
}

// ensureSchema 是启动期建表策略(port-decisions D-14 第 4 条):
//   - Schema.AutoMigrate 没写或 true:跑 Up;err 非 nil 或出现需人工项 → 返回错误(调用方拒绝启动)。
//   - false:只跑只读 Plan;有待执行语句或需人工项 → 返回错误,并带上补救命令。
//
// 两种模式都额外把"缺普通索引"从 schemamigrate 的 Warning 升级成阻断:它不会自动补建,
// 放过去等于带着缺索引的表对外服务(设计 §6.4)。
func ensureSchema(ctx context.Context, db *sql.DB, c config.Config, up, plan schemaRunner) error {
	opts := schemaOptions()
	if c.ShouldAutoMigrate() {
		report, err := runWithLockRetry(ctx, up, db, opts)
		logReport("schemamigrate.Up", report)
		if err != nil {
			return fmt.Errorf("[guild] 启动期建表失败(库 %s),拒绝启动: %w", data.DatabaseName, err)
		}
		if len(report.Manual) > 0 {
			return fmt.Errorf("[guild] 启动期建表发现 %d 项需人工处理(库 %s),拒绝启动: %s",
				len(report.Manual), data.DatabaseName, strings.Join(report.Manual, "; "))
		}
		return missingIndexError(report)
	}

	report, err := runWithLockRetry(ctx, plan, db, opts)
	logReport("schemamigrate.Plan", report)
	if err != nil {
		return fmt.Errorf("[guild] Schema.AutoMigrate=false,启动期检查表结构失败(库 %s),拒绝启动;确认库可达后执行 %s: %w",
			data.DatabaseName, migrateRemedyCommand, err)
	}
	if !report.Clean() {
		return fmt.Errorf("[guild] Schema.AutoMigrate=false 且库 %s 与 proto/guild/guild_db.proto 不一致"+
			"(待执行 %d 条、需人工 %d 项),拒绝启动;先执行 %s",
			data.DatabaseName, len(report.Statements), len(report.Manual), migrateRemedyCommand)
	}
	return missingIndexError(report)
}

// missingIndexWarnings 挑出"缺索引"告警。前缀取自 go/schemamigrate/plan.go 的文案;
// guild_test.go 的真库用例负责守住两边一致(文案漂移时会红)。
func missingIndexWarnings(r schemamigrate.Report) []string {
	var missing []string
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, "缺索引") {
			missing = append(missing, w)
		}
	}
	return missing
}

func missingIndexError(r schemamigrate.Report) error {
	missing := missingIndexWarnings(r)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("[guild] 库 %s 缺 %d 个 proto 声明的普通索引,拒绝启动"+
		"(schemamigrate 不自动补建;开发期按 docs/design/guild-phase2/01-storage.md §6.4 删库重建): %s",
		data.DatabaseName, len(missing), strings.Join(missing, "; "))
}

// runMigration 是 -migrate 的实现:只连 MySQL,不起 gRPC、不连 etcd / data_service。
// 输出走 stdout / stderr,退出码按 schemamigrate.ExitCode(D-14),部署脚本与 K8s Job 直接读。
// 只打印库名,不打印 DSN(含口令)。
func runMigration(c config.Config) int {
	fmt.Printf("guild schema migration: database=%s\n", data.DatabaseName)
	db, err := sql.Open("mysql", c.MySQL.DataSource)
	if err == nil {
		var pingCtx context.Context
		var cancel context.CancelFunc
		pingCtx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
	}
	if err != nil {
		code := schemamigrate.ExitCode(schemamigrate.Report{}, err)
		fmt.Fprintf(os.Stderr, "schema migration FAILED (exit %d): %v\n", code, err)
		return code
	}
	defer func() { _ = db.Close() }()

	report, err := schemamigrate.Up(context.Background(), db, schemaOptions())
	printReport(os.Stdout, report)
	code := schemamigrate.ExitCode(report, err)
	if code == 0 {
		if missErr := missingIndexError(report); missErr != nil {
			fmt.Fprintf(os.Stderr, "schema migration needs manual action (exit %d): %v\n", schemamigrate.ExitManual, missErr)
			return schemamigrate.ExitManual
		}
	}
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "schema migration FAILED (exit %d): %v\n", code, err)
	case code != 0:
		fmt.Fprintf(os.Stderr, "schema migration needs manual action (exit %d): %d item(s) listed above\n", code, len(report.Manual))
	default:
		fmt.Printf("schema migration OK: %s synced from proto/guild/guild_db.proto (%d statement(s) executed)\n",
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

func logReport(stage string, r schemamigrate.Report) {
	for _, line := range reportLines(r) {
		logx.Infof("[guild] %s%s", stage, line)
	}
}

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
