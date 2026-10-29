// player_inbox 服务:玩家持久收件箱(docs/design/player-inbox-service.md,设计 v1.0;D3 独立进程)。
//
// 全局一份、多副本、进程无状态:数据在独占库 mmorpg_player_inbox(port-decisions D-14)。
// 调用方:
//   - C++ scene:PlayerInbox.FetchItems / AckItems(经 etcd 前缀 PlayerInboxNodeService.rpc 发现);
//   - 运维工具:PlayerInboxAdmin.*(直连,metadata x-admin-token;口令为空 = 运维面停用)。
//
// 两种运行形态(同 go/trade):
//   - 常驻服务(默认):建表 / 查表 → metrics → 依赖(节点镜像、后台循环)→ killswitch → gRPC → 注册 etcd;
//   - `-migrate`:只连 MySQL 跑一次 schemamigrate.Up,按 D-14 退出码退出(K8s player-inbox-migrate Job 用),
//     不起 gRPC、不连 etcd / Redis / Kafka / data_service。
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
	"shared/authoritysql"
	"shared/drainproof"
	"strconv"
	"syscall"
	"time"

	"player_inbox/internal/config"
	"player_inbox/internal/constants"
	"player_inbox/internal/data"
	"player_inbox/internal/inbox"
	"player_inbox/internal/lifecycle"
	"player_inbox/internal/logic"
	"player_inbox/internal/metrics"
	"player_inbox/internal/server"
	"player_inbox/internal/svc"

	base "proto/common/base"
	inboxpb "proto/player_inbox"

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
	configFile = flag.String("f", "etc/player_inbox.yaml", "the config file")

	// migrateOnly 是生产用的显式建表入口(D-14 第 4 条):`player_inbox -f <yaml> -migrate` 只对 mmorpg_player_inbox
	// 用独立 Schema 能力执行一次正式迁移后退出；常驻启动仅核对表结构。
	// 退出码按 schemamigrate.ExitCode:0 成功 / 1 错误 / 3 锁被占(K8s Job 按 backoff 重试)/ 4 需人工。
	migrateOnly = flag.Bool("migrate", false, "run schemamigrate.Up on mmorpg_player_inbox and exit (0 ok / 1 error / 3 lock busy / 4 manual)")
	// 改列授权只属于显式迁移,不得被常驻服务启动期的 AutoMigrate 继承。
	allowModify = flag.Bool("allow-modify", false, "only with -migrate: allow MODIFY COLUMN after manual review; never for resident startup")

	showVersion = flag.Bool("version", false, "print version and exit")
)

// nodeType 本服务的节点类型(proto/common/base/node.proto = 30)。
const nodeType = base.ENodeType_PlayerInboxNodeService

const (
	// listenProbeTimeout:等本进程 gRPC 端口可连的上限。先探端口再注册,scene 才不会在监听建立前选中本节点。
	listenProbeTimeout = 30 * time.Second
	// registerTimeout:注册整体(探端口 + etcd CAS)的上限,须大于 listenProbeTimeout。
	registerTimeout = 45 * time.Second
)

// migrateRemedyCommand 是 Schema.AutoMigrate=false 且库与 proto 不一致时打印给运维的补救命令。
const migrateRemedyCommand = "player_inbox -f etc/player_inbox.yaml -migrate"

var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式 typed 排空 runtime 原件")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有排空原件目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime 原文件 SHA256")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空请求的私有目录")

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"。
	fmt.Println(buildinfo.StartupLine("player_inbox"))
	if *showVersion {
		return
	}
	if err := validateMigrationFlags(*migrateOnly, *allowModify); err != nil {
		fmt.Fprintf(os.Stderr, "[player_inbox] %v\n", err)
		os.Exit(schemamigrate.ExitFailed)
	}

	var c config.Config
	// conf.MustLoad 会自动调用 (*Config).Validate(go-zero v1.10.0),不合法即 Fatalf 退出。
	conf.MustLoad(*configFile, &c)

	if *migrateOnly {
		os.Exit(runMigration(c, *allowModify, svc.OpenSchemaMySQL, schemamigrate.Up, checkRowShardLayout))
	}
	if err := runPlayerInbox(c); err != nil {
		fmt.Fprintf(os.Stderr, "[player_inbox] %v\n", err)
		os.Exit(1)
	}
}

// validateMigrationFlags 在读配置、连接数据库之前拒绝意外的常驻改列授权。
func validateMigrationFlags(migrateOnly, allowModify bool) error {
	if allowModify && !migrateOnly {
		return errors.New("-allow-modify must be used together with -migrate; resident startup never allows MODIFY COLUMN")
	}
	return nil
}

// runPlayerInbox 是常驻服务形态。启动失败与正常退出走同一条收尾(lifecycle.Shutdown),返回的 error 决定退出码。
// 停机顺序同 go/trade:① 注销 → ② 排空 gRPC(≤5s)→ ③ 停 killswitch / metrics,取消后台循环并有界等待,
// 关 Kafka / Redis / DB / etcd → ④ 框架收尾(≤2s);24s 硬截止兜底。
func runPlayerInbox(c config.Config) (runErr error) {
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	lifecycle.Configure(&c.RpcServerConf) // 必须早于 zrpc.NewServer
	finished := make(chan struct{})
	defer close(finished)
	go watchHardDeadline(ctx, stopSignals, finished)
	if exitRequested(ctx) {
		return nil
	}
	consumerVerifier, err := server.NewConsumerVerifier()
	if err != nil {
		return err
	}

	listenHost, port, err := c.ListenHostPort()
	if err != nil {
		return err
	}
	// 运维口令:字面 AdminToken 或 AdminTokenEnv 指向的环境变量(Secret 注入);空 = 运维面停用(fail-closed)。
	adminToken, err := c.PlayerInbox.ResolveAdminToken(os.Getenv)
	if err != nil {
		return err
	}

	runtime, root, relay, err := serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
	if err != nil {
		return err
	}
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return err
	}
	svcCtx, err := svc.NewServiceContextWithAdmission(c, writer, root, relay)
	if err != nil {
		return err
	}

	stopKillSwitch := func() {}
	stopMetrics := func() {}
	var serverSlot lifecycle.ServerSlot
	shutdown := &lifecycle.Shutdown{
		DrainTimeout:  lifecycle.DrainTimeout,
		FinishTimeout: lifecycle.FinishTimeout,
		// killswitch watch、节点镜像与注销共用 svcCtx 的 etcd 客户端;后台循环跑在 MySQL / 核心 Redis 上:
		// 都必须在 svcCtx.Stop 关连接之前停(Stop 先取消循环并有界等待)。
		CloseAndReport: func() error {
			stopKillSwitch()
			stopMetrics()
			closeCtx, cancel := context.WithTimeout(context.Background(), lifecycle.DrainTimeout)
			defer cancel()
			return svcCtx.StopAndReport(closeCtx)
		},
		FinishFramework: proc.Shutdown,
	}
	defer func() {
		stopSignals()
		root.CloseAdmission()
		shutdown.Server = serverSlot.TakeForShutdown()
		if err := shutdown.Stop(); err != nil {
			logx.Errorf("[player_inbox] Shutdown cleanup exceeded its budget: %v", err)
			runErr = errors.Join(runErr, err)
		}
		_ = logx.Close()
	}()

	// 建表 / 查表(D-14 第 4 条)。放在起 gRPC 之前:表不对的收件箱一个请求都不该接。
	if err := ensureSchema(context.Background(), svcCtx.DB, c, schemamigrate.Up, schemamigrate.Plan, checkRowShardLayout); err != nil {
		return err
	}
	indexCtx, indexCancel := context.WithTimeout(context.Background(), 5*time.Second)
	indexErr := inbox.CheckAuthorityIndexes(indexCtx, svcCtx.DB)
	indexCancel()
	if indexErr != nil {
		return indexErr
	}
	if exitRequested(ctx) {
		return nil
	}

	metrics.Register(svcCtx.DB)
	stopMetrics = metrics.StartHTTP(c.MetricsListenAddr)

	// scene 节点镜像 + 选主后台循环(清理 / 提醒补发 / gauge / 对账)。必须在 ensureSchema 之后(表一定在)。
	svcCtx.Start(ctx)

	// RPC 级热关停(shared/killswitch):复用 svcCtx 的 etcd 客户端;Start 非阻塞,etcd 不可达一律放行。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	stopKillSwitch = ksCancel
	ks := killswitch.New(killswitch.Config{Prefix: c.KillSwitchPrefix})
	ks.Start(ksCtx, svcCtx.Etcd)

	budget := c.RequestBudget()
	sanction := logic.GmSanctionDeps{Store: svcCtx.Store, Audit: inbox.InsertAudit, Now: time.Now}
	serverReady := make(chan struct{})
	s, err := zrpc.NewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		inboxpb.RegisterPlayerInboxServer(grpcServer, server.NewPlayerInboxServer(svcCtx.Store, budget, consumerVerifier))
		inboxpb.RegisterPlayerInboxAdminServer(grpcServer, server.NewAdminServer(svcCtx.Store, sanction, svcCtx.Ledgers, budget))
		if config.IsRelaxedMode(c.Mode) {
			reflection.Register(grpcServer)
		}
		serverSlot.Publish(grpcServer)
		close(serverReady)
	})
	if err != nil {
		return fmt.Errorf("player_inbox RPC construction failed: %w", err)
	}
	s.AddUnaryInterceptors(serverbase.PublicationAdmission(root, relay, map[string]bool{
		inboxpb.PlayerInbox_FetchItems_FullMethodName:              true,
		inboxpb.PlayerInbox_AckItems_FullMethodName:                true,
		inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName: true,
		inboxpb.PlayerInbox_LookupBattleSettlement_FullMethodName:  true,
	}, svcCtx.RelayIngress))
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks, adminToken)...)

	// **先起 gRPC,再注册**(契约 §2)。Start 在监听失败时 panic:recover 成错误交回主线程。
	serveDone := make(chan struct{})
	serveErr := make(chan error, 1)
	shutdown.ServeDone = serveDone
	go func() {
		var startErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				startErr = fmt.Errorf("gRPC Start failed: %v", recovered)
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
		return fmt.Errorf("player_inbox gRPC listener not ready: %v", err)
	}

	advertiseHost := advertisedHost(listenHost)
	regCtx, regCancel := context.WithTimeout(ctx, registerTimeout)
	nr, err := noderegistry.RegisterAfterListening(regCtx, svcCtx.Etcd, dialAddress(listenHost, port), listenProbeTimeout,
		noderegistry.Spec{
			// Prefix 由枚举名派生,不手写:"PlayerInboxNodeService.rpc"。
			Prefix:     base.ENodeType_name[int32(nodeType)] + ".rpc",
			NodeType:   uint32(nodeType),
			ZoneId:     c.ZoneId,
			LeaseTTL:   c.LeaseTTL,
			BuildValue: nodeInfoValueBuilder(c.ZoneId, advertiseHost, port, uint64(time.Now().Unix())),
			// 收件箱不以 node_id 派生任何持久身份(seq 来自流行,不是 snowflake),失租后换 id 继续服务即可。
			OnReclaimFailed: noderegistry.ReallocateNewID,
			OnNodeIDChanged: func(oldID, newID uint32) {
				logx.Errorf("[player_inbox] Could not reclaim original node_id=%d after etcd lease loss; continuing with new node_id=%d", oldID, newID)
			},
			LogPrefix: "[player_inbox]",
		})
	regCancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("player_inbox node registration in etcd failed: %w", err)
	}
	shutdown.Unregister = nr.Close
	nr.KeepAlive()

	controlCtx, cancelControl := context.WithCancel(ctx)
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		if err := serverbase.DriveDrainRuntime(controlCtx, runtime, *maintenanceControlDir, root, relay, svcCtx.BindDrainRuntime, svcCtx.CloseSource, svcCtx.StopAndReport, func(prepareCtx context.Context) (*serverbase.PreparedPublication, error) {
			if err := prepareCtx.Err(); err != nil {
				return nil, err
			}
			if svcCtx.Store == nil || svcCtx.DB == nil || root.Pending() != 0 {
				return nil, drainproof.ErrHold
			}
			return &serverbase.PreparedPublication{NoConsumers: true}, nil
		}); err != nil && controlCtx.Err() == nil {
			logx.Errorf("player_inbox 排空 HOLD: %v", err)
		}
	}()
	defer func() { cancelControl(); <-controlDone }()

	fmt.Println("\n=============================================================")
	fmt.Println("  PLAYER INBOX SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:        %s\n", c.ListenOn)
	fmt.Printf("  Advertise:     %s\n", net.JoinHostPort(advertiseHost, strconv.FormatUint(uint64(port), 10)))
	fmt.Printf("  Mode:          %s\n", c.Mode)
	fmt.Printf("  etcd:          %v\n", c.Etcd.Hosts)
	fmt.Printf("  node_id:       %d (etcd CAS)\n", nr.NodeID())
	fmt.Printf("  node_uuid:     %s\n", nr.NodeUUID)
	fmt.Printf("  zone:          %d (affects registration path only)\n", c.ZoneId)
	// MySQLTarget 只含 host / 库名 / 用户名,绝不含密码。
	fmt.Printf("  mysql:         %s\n", svc.MySQLTarget(c.MySQL))
	fmt.Printf("  schema:        %s\n", schemaModeLabel(c))
	fmt.Printf("  core_redis:    %s (DB %d)\n", c.CoreRedis.Host, c.CoreRedis.DB)
	fmt.Printf("  kafka:         %v\n", c.Kafka.Brokers)
	fmt.Printf("  enqueue_types: %v (empty = all disabled)\n", c.PlayerInbox.EnqueueTypes)
	fmt.Printf("  admin:         %s\n", adminLabel(adminToken != ""))
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:       %s\n", c.MetricsListenAddr)
	}
	fmt.Println("=============================================================")

	select {
	case <-ctx.Done():
		logx.Info("[player_inbox] Received shutdown signal; deregistering before draining RPCs")
	case err := <-serveErr:
		return fmt.Errorf("player_inbox gRPC server exited early: %v", err)
	}
	return nil
}

// watchHardDeadline 从进入退出开始计 lifecycle.HardTimeout,到点仍未收尾就强制退出(同 go/trade)。
func watchHardDeadline(ctx context.Context, stopSignals context.CancelFunc, finished <-chan struct{}) {
	select {
	case <-finished:
		return
	case <-ctx.Done():
	case <-proc.Done():
		stopSignals()
	}
	timer := time.NewTimer(lifecycle.HardTimeout)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		fmt.Fprintf(os.Stderr, "[player_inbox] Shutdown hard deadline of %v reached; forcing exit, completion of in-flight requests cannot be confirmed\n", lifecycle.HardTimeout)
		os.Exit(1)
	}
}

// exitRequested 报告是否已经收到退出(本进程 ctx 或 go-zero proc)。
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
func buildUnaryInterceptors(ks *killswitch.Switch, adminToken string) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计最外层:被热关停短路、被运维鉴权拒绝的请求也要被统计到。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中后鉴权、MySQL / Redis 访问统统不做。
		ks.UnaryServerInterceptor(),

		// ③ 运维面鉴权:只拦 PlayerInboxAdmin.*;口令为空 = 运维面停用,错误 / 缺失 → PermissionDenied。
		//    放在 serverbase 之前:被拒绝的调用没进业务链路,不计入业务耗时。
		server.AdminAuthInterceptor(adminToken),

		// ④ in-band 故障定性:handler 一律 `return resp, nil`,这层读 TipInfoMessage.id 按 Tip.xlsx 的
		//    fault 列定性(kPlayerInboxStoreUnavailable = 故障告警,其余 = 业务拒绝)。紧贴 handler。
		serverbase.UnaryInterceptor(serverbase.Options{
			TipClassifier: constants.TipClassifier(),
		}),
	}
}

// schemaRunner 是 schemamigrate.Up / schemamigrate.Plan 的函数形状。
type schemaRunner func(ctx context.Context, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error)

// schemaOptions 是启动路径与 -migrate 共用的迁移选项:库名断言用代码常量,表清单只来自 data.Tables()。
func schemaOptions() schemamigrate.Options {
	return schemamigrate.Options{
		Database: data.DatabaseName,
		Tables:   data.Tables(),
		Logf:     logx.Infof,
	}
}

// rowLayoutCheck 是「表结构对齐之后、接请求之前」的数据布局核对,形状同 inbox.CheckRowShards(单测注入假实现)。
type rowLayoutCheck func(ctx context.Context, db *sql.DB) error

// checkRowShardLayout 是生产用的布局核对:shard 列与 RowShard(player_id) 不符的行(典型是 2026-10-16 分片公式改为
// mix64 之前写出的开发库存量)对按玩家的读写静默不可见,会破坏幂等,所以必须在接请求之前拒绝(评审 L5 / R3)。
func checkRowShardLayout(ctx context.Context, db *sql.DB) error {
	return inbox.CheckRowShards(ctx, db)
}

// ensureSchema 只核对常驻进程依赖的表结构；显式请求自动 DDL 时拒绝启动。
// false 只跑只读 Plan,有待执行语句或需人工项即拒启并带补救命令。表结构对齐之后再跑一次行布局核对,不一致即拒启。
func ensureSchema(ctx context.Context, db *sql.DB, c config.Config, up, plan schemaRunner, check rowLayoutCheck) error {
	opts := schemaOptions()
	if c.ShouldAutoMigrate() {
		return errors.New("常驻服务禁止自动DDL，请关闭AutoMigrate并显式运行 -migrate")
	}

	report, err := plan(ctx, db, opts)
	logReport("schemamigrate.Plan", report)
	if err != nil {
		return fmt.Errorf("[player_inbox] Schema.AutoMigrate=false and schema check failed (database %s), refusing to start; "+
			"once the database is reachable run %s: %w", data.DatabaseName, migrateRemedyCommand, err)
	}
	if !report.Clean() {
		return fmt.Errorf("[player_inbox] Schema.AutoMigrate=false and database %s differs from proto/player_inbox/player_inbox_table.proto "+
			"(%d pending statement(s), %d manual item(s)), refusing to start; run %s first",
			data.DatabaseName, len(report.Statements), len(report.Manual), migrateRemedyCommand)
	}
	return checkLayoutBeforeServing(ctx, db, check)
}

// checkLayoutBeforeServing 跑行布局核对;不一致或核对本身失败都拒启(fail-closed:核对不了就不能断言没有隐形行)。
func checkLayoutBeforeServing(ctx context.Context, db *sql.DB, check rowLayoutCheck) error {
	if err := check(ctx, db); err != nil {
		return fmt.Errorf("[player_inbox] row shard layout check failed (database %s), refusing to start: %w", data.DatabaseName, err)
	}
	return nil
}

// runMigration 是 -migrate 的实现:只连 MySQL。输出走 stdout / stderr,退出码按 schemamigrate.ExitCode(D-14)。
// 迁移成功之后再跑行布局核对:不一致按「需人工」退出(ExitManual),让 K8s Job 停下而不是反复重试。
func runMigration(c config.Config, allowModify bool, openMySQL func(config.MySQLConf) (*sql.DB, error), up schemaRunner, check rowLayoutCheck) int {
	fmt.Printf("player_inbox schema migration: %s\n", svc.MySQLTarget(c.MySQL))
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
		fmt.Printf("schema migration OK: %s synced from proto/player_inbox/player_inbox_table.proto (%d statement(s) executed)\n",
			data.DatabaseName, len(report.Statements))
		if err := check(context.Background(), db); err != nil {
			fmt.Fprintf(os.Stderr, "row shard layout check FAILED (exit %d): %v\n", schemamigrate.ExitManual, err)
			return schemamigrate.ExitManual
		}
		fmt.Println("row shard layout check OK")
	}
	return code
}

// reportLines 把迁移报告展开成可读行。
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
		logx.Infof("[player_inbox] %s statement: %s", stage, s)
	}
	for _, w := range r.Warnings {
		logx.Errorf("[player_inbox] %s warning: %s", stage, w)
	}
	for _, m := range r.Manual {
		logx.Errorf("[player_inbox] %s MANUAL: %s", stage, m)
	}
}

// schemaModeLabel 是横幅里的建表策略描述。
func schemaModeLabel(c config.Config) string {
	if c.ShouldAutoMigrate() {
		return "拒绝常驻自动迁移；请先执行独立 -migrate"
	}
	return "plan-only (tables owned by " + migrateRemedyCommand + ")"
}

// adminLabel 是横幅里的运维面状态。**不打印口令**,只说开没开。
func adminLabel(enabled bool) string {
	if enabled {
		return "enabled (PlayerInboxAdmin requires metadata " + server.AdminTokenMetadataKey + ")"
	}
	return "DISABLED (no admin token resolved from PlayerInbox.AdminToken / AdminTokenEnv; every PlayerInboxAdmin call returns PermissionDenied)"
}

// nodeInfoValueBuilder 返回 noderegistry 的 BuildValue 回调:按 C++ NodeInfo 约定生成 protojson(字段填法同 go/trade)。
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

// advertisedHost 决定写进 NodeInfo 的 IP:POD_IP 优先 → ListenOn 的具体 host → 本机第一个非 loopback IP → 127.0.0.1。
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
