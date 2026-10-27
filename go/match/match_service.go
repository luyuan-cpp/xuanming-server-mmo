package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/kafkautil"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"match/internal/authority"
	"match/internal/config"
	"match/internal/metrics"
	"match/internal/noderegistry"
	"match/internal/pkg/ctxkeys"
	"match/internal/server"
	"match/internal/svc"
	"match/internal/team"
	"schemamigrate"

	matchkafka "match/internal/kafka"
	matchlogic "match/internal/logic"

	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"
	teampb "proto/team"

	"shared/buildinfo"
	"shared/grpcstats"
	sharedregistry "shared/noderegistry"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var configFile = flag.String("f", "etc/match_service.yaml", "the config file")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

// 显式迁移先于服务装配，只由独立DDL账号运行；常驻形态始终只读核对schema。
var migrateOnly = flag.Bool("migrate", false, "仅迁移匹配与组队SQL表后退出（0成功/1失败/3锁忙/4人工处理）")
var allowModify = flag.Bool("allow-modify", false, "仅与-migrate同时使用：人工审阅后授权MODIFY COLUMN")

const migrationUserEnv = "MATCH_MIGRATION_USER"
const migrationPasswordEnv = "MATCH_MIGRATION_PASSWORD"

const (
	// listenProbeTimeout:等本进程 gRPC 端口可连的上限(照 chat)。先探端口再注册,
	// 是为了不让路由服 / gate 在监听建立前就选中本节点。
	listenProbeTimeout = 30 * time.Second
	// registerTimeout:两次注册整体(探端口 + etcd CAS)的上限,须大于 listenProbeTimeout。
	registerTimeout = 45 * time.Second
	// shutdownDrainTimeout:注销后等 gRPC 服务退出的上限。Linux 上 SIGTERM/SIGINT 会触发
	// go-zero proc 在 1s 后 GracefulStop 并在 5.5s 强杀(core/proc/shutdown.go),5s 足够覆盖
	// 正常排空;Windows 本地 go-zero 不接管信号,等满这段就直接退出。
	shutdownDrainTimeout = 5 * time.Second
)

var maintenancePlan = flag.String("maintenance-drain-plan", "", "正式 typed 排空 runtime 原件")
var maintenanceDir = flag.String("maintenance-drain-dir", "", "私有排空原件目录")
var maintenanceSHA = flag.String("maintenance-drain-sha256", "", "runtime 原文件 SHA256")
var maintenanceControlDir = flag.String("maintenance-drain-control-dir", os.Getenv("MMORPG_MAINTENANCE_CONTROL_DIR"), "同一 boot 接收排空请求的私有目录")

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("match"))
	if *showVersion {
		return
	}
	if err := validateMigrationFlags(*migrateOnly, *allowModify); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(schemamigrate.ExitFailed)
	}

	var c config.Config
	conf.MustLoad(*configFile, &c)
	if *migrateOnly {
		os.Exit(runMigration(c, *allowModify, authority.Migrate))
	}
	runtime, root, relay, err := serverbase.LoadPublicationRuntime(*maintenancePlan, *maintenanceDir, *maintenanceSHA)
	if err != nil {
		panic(err)
	}
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(err)
	}
	svcCtx := svc.NewServiceContextWithAdmission(c, writer, root, relay)
	// 独立内部取消证明；未配置时保持nil并在RPC入口拒绝，绝不降级为无签名取消。
	if secret := strings.TrimSpace(os.Getenv("MMORPG_MATCH_INTERNAL_SECRET_GUILD")); secret != "" {
		verifier, err := serviceauth.New(map[string]string{"guild": secret})
		if err != nil {
			logx.Must(fmt.Errorf("活动取消独立签名配置非法"))
		}
		svcCtx.ActivityCancelAuth = verifier
		svcCtx.ActivityCancelSecret = secret
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := svcCtx.BindBackgroundStop(cancel); err != nil {
		panic(err)
	}
	defer func() { root.CloseAdmission(); cancel() }()
	resultConfig := matchkafka.ResultConsumerConfig{
		RelayAdmission: relay, BindOwner: svcCtx.BindConsumerOwner,
		Brokers: c.Kafka.Brokers, Topic: c.ResultTopic,
		GroupID: c.ResultConsumerGroup, Partitions: c.ResultTopicPartitions,
		RequireSQLCommit: c.GlobalMatch.Enabled,
	}
	// 成局之后 battle 会写结果，不能等到可选的评分消费者启动时才发现主题不耐久。
	// 此门禁先于 matcher、节点发现和 RPC 注册；失败直接拒绝本进程启动。
	if c.GlobalMatch.Enabled {
		if err := matchkafka.EnsureResultTopic(resultConfig); err != nil {
			cancel()
			panic(fmt.Errorf("全球匹配结果主题未满足耐久契约，拒绝启动: %w", err))
		}
	}

	// Prometheus /metrics(MetricsListenAddr 为空则不开)。
	metrics.Start(c.MetricsListenAddr)
	// 本实例生效的组队跨区开关;多实例取值不一致 = 滚动切换中(team-system.md J-13a)。
	metrics.SetTeamCrossZoneAllowed(c.Team.AllowCrossZone)

	// scene / battle 节点发现:etcd list-watch 内存镜像
	// (gather 定位 PrepareBattle / CreateBattle 对端用,照 LoadReporter 模式;
	// 组队给 scene 发 PlayerTeamRefreshEvent 也从这里取 NodeUuid)。
	svcCtx.LaunchRelayTask("match.watch.scene", func() { svcCtx.SceneNodes.Run(ctx, svcCtx.Etcd) })
	svcCtx.LaunchRelayTask("match.watch.battle", func() { svcCtx.BattleNodes.Run(ctx, svcCtx.Etcd) })

	// 全球结果来源归档独立于评分开关，PVE与不计分局也必须保全完整个人原件。
	// 启动接线先于matcher与RPC注册；旧兼容模式仍只在启用评分时消费。
	if c.GlobalMatch.Enabled || c.RatingEnabled {
		startResults := func() error {
			var recorder []matchkafka.ResultRecordHandler
			if c.GlobalMatch.Enabled {
				if svcCtx.Authority == nil {
					return fmt.Errorf("全球匹配完整结算来源数据库未装配")
				}
				recorder = append(recorder, svcCtx.Authority.ArchiveCompletion)
			}
			return matchkafka.StartResultConsumer(ctx, resultConfig, func(_ context.Context, event *kafkapb.BattleResultEvent) error {
				if !c.RatingEnabled {
					return nil
				}
				_, err := matchlogic.ApplyBattleResult(svcCtx, event)
				return err
			}, recorder...)
		}
		// 排行榜评分快照 topic(docs/design/leaderboard-system.md §2.4):RatingEnabled=true 时不论发布开关开没开,都由 match
		// 声明分区契约与保留期 —— rank 先起时 topic 以 broker 默认保留期建出(rank Ensure 传 RetentionMs: -1 不改写),
		// match 一起来就按配置写正。
		ensureSnapshotTopic := func() error {
			return matchkafka.EnsureRatingSnapshotTopic(c.Kafka.Brokers, c.RatingSnapshotTopic,
				c.RatingSnapshotTopicPartitions, c.RatingSnapshotRetentionMs())
		}
		// 发布开关打开时,结果消费者必须在快照 topic 声明成功**之后**才起:共享 writer 开着 AllowAutoTopicCreation,
		// 声明失败期间消费到的对局一发布,broker 就会按默认分区数(本地 compose 与 K8s 都是 1)自动建 topic,
		// 3 分区契约从此永久破坏,match 与 rank 的 Ensure 都永远失败,只能手工删 topic(R1 评审)。
		// 所以两步串成一步重试:声明不成功,评分入账就停在当前值(排队 / 凑单照常),日志每 30s 报一次原因。
		// 开关关闭时两者各自重试:快照 topic 的问题(比如分区数与 broker 不一致)不能连带停掉评分入账。
		consumeResults := startResults
		if c.RatingEnabled && !c.GlobalMatch.Enabled && c.RatingSnapshotPublishEnabled {
			consumeResults = func() error {
				if err := ensureSnapshotTopic(); err != nil {
					return fmt.Errorf("快照发布已打开,必须先声明评分快照 topic %s 才能起结果消费者: %w", c.RatingSnapshotTopic, err)
				}
				return startResults()
			}
		}
		// 全球来源归档必须同步装配成功；只有显式旧兼容模式的软评分允许告警后后台重试。
		if err := consumeResults(); err != nil {
			if c.GlobalMatch.Enabled {
				cancel()
				panic(fmt.Errorf("全球匹配完整结算来源消费者未就绪，拒绝启动: %w", err))
			}
			logx.Errorf("[rating] Failed to start battle result consumer; rating updates paused, retrying in the background after 30s: %v", err)
			svcCtx.LaunchRelayTask("match.kafka.results.retry", func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(30 * time.Second):
					}
					if err := consumeResults(); err != nil {
						logx.Errorf("[rating] Failed to restart battle result consumer; retrying after 30s: %v", err)
						continue
					}
					logx.Info("[rating] Battle result consumer restarted successfully")
					return
				}
			})
		}
		// 开关关闭:快照 topic 只在后台声明、失败每 30s 重试(开关打开时已经在 startRating 里先于结果消费者同步声明)。
		// 不在这里同步试第一次:Kafka 能建连却不回包时一次 admin 调用最坏要几十秒,叠在 startRating 之后会把 gRPC 起服推迟一倍,
		// 被 liveness 探针杀掉重启 —— 而开关关着时没有任何东西等这个 topic。
		if c.RatingEnabled && !c.GlobalMatch.Enabled && !c.RatingSnapshotPublishEnabled {
			svcCtx.LaunchRelayTask("match.kafka.rating_snapshot_topic", func() {
				for {
					err := ensureSnapshotTopic()
					if err == nil {
						logx.Infof("[rating] Rating snapshot topic %s declared (publish disabled)", c.RatingSnapshotTopic)
						return
					}
					logx.Errorf("[rating] Failed to declare rating snapshot topic %s (publish disabled); retrying after 30s: %v",
						c.RatingSnapshotTopic, err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(30 * time.Second):
					}
				}
			})
		}
		logx.Infof("[rating] Leaderboard rating snapshot publish enabled=%t topic=%s partitions=%d retention=%dh",
			c.RatingSnapshotPublishEnabled, c.RatingSnapshotTopic, c.RatingSnapshotTopicPartitions, c.RatingSnapshotRetentionHours)
	}

	// 组队(team-system.md §A.3,port-decisions D-2 / feasibility D7):与 match 同进程、同一个
	// zrpc server,协议独立(ClientPlayerTeam service + TeamNodeService 第二次节点注册)。
	//   - home zone 查询走 data_service(DataServiceRpc 未配置时 DataServiceClient 为 nil,
	//     team 需要 home zone 的请求一律回 TeamInternal,不拒绝起服);
	//   - 整队开战的票据域端口由 logic.TeamBattleStarter 实现,这一行同时是
	//     *logic.TeamBattleStarter 满足 team.BattleStarter 的编译期保证(team 与 logic 互不 import)。
	teamService := team.NewService(svcCtx,
		team.NewDataServiceHomeZone(svcCtx.DataServiceClient, team.DefaultHomeZoneLookupTimeout),
		matchlogic.NewTeamBattleStarter(svcCtx))
	svcCtx.SetPartyTeam(teamService)
	teamAuthority, teamAuthorityErr := team.NewAuthorityServer(svcCtx)
	if teamAuthorityErr != nil {
		logx.Errorf("SQL队伍场景读取契约未就绪，拒绝启动: %v", teamAuthorityErr)
		cancel()
		svcCtx.Stop()
		os.Exit(1)
	}
	teamService.StartSQLMaintenance(ctx)
	matchlogic.StartGlobalTeamMatcher(ctx, svcCtx)
	matchlogic.StartGlobalSpectatorRecovery(ctx, svcCtx)
	// 完整结果归档与队伍反向端口均就绪后才开放成局。
	matchlogic.StartMatcherLoop(ctx, svcCtx)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		matchpb.RegisterMatchServiceServer(grpcServer, server.NewMatchServiceServer(svcCtx))
		// 内部服务(帮会同道历练开局,guild-phase2 §6.18.3):只给 go/guild 调;
		// 客户端来源由 sessionInterceptor 拒绝(PermissionDenied)。
		matchpb.RegisterMatchInternalServer(grpcServer, server.NewMatchInternalServer(svcCtx))
		teampb.RegisterClientPlayerTeamServer(grpcServer, team.NewServer(teamService))
		if teamAuthority != nil {
			teampb.RegisterTeamAuthorityServer(grpcServer, teamAuthority)
		}

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 拦截器链作用于同一 server 上的全部 service(MatchService、MatchInternal 与 ClientPlayerTeam),顺序不变:
	// sessionInterceptor 最外层先挡"带会话调 MatchInternal.*"(PermissionDenied),再把 x-session-detail-bin
	// 解进 ctx(team 的调用者身份只从这里取,缺失即 fail-closed 回 4001);grpcstats 在内层计流量。
	// zrpc 顶层 Timeout 保持 5000:改小会截断 WatchBattle 同步链;team 在每个方法入口自设 3500ms 预算(§A.3 第 7 条)。
	s.AddUnaryInterceptors(
		serverbase.PublicationAdmission(root, relay, map[string]bool{
			matchpb.MatchInternal_StartActivityBattle_FullMethodName:  true,
			matchpb.MatchInternal_CancelActivityBattle_FullMethodName: true,
			matchpb.MatchInternal_CommitBattleOutcome_FullMethodName:  true,
		}, svcCtx.RelayIngress),
		sessionInterceptor,
		originalRoleInterceptor(svcCtx),
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),
	)

	// **先起 gRPC,再注册**(§A.3 第 2 条,照 chat):Start 阻塞到服务退出,所以放后台 goroutine;
	// 注册前由 RegisterAfterListening 探到端口可连才写 etcd。刻意用裸 go 而不是 safego.Go:
	// Start 在监听失败时 panic,那就应该让进程死掉,而不是以"已注册但不可达"的状态继续跑。
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		s.Start()
	}()

	// C++ NodeInfo 约定注册进 etcd:路由服按 TeamNodeService.rpc 发现组队,gate 按 MatchNodeService.rpc
	// 发现 match。写进 NodeInfo 的是对外地址(K8s 取 POD_IP),不是监听的 0.0.0.0。
	listenHost, port := parseListenOn(c.ListenOn)
	advertiseHost := advertisedHost(listenHost)
	regs, err := registerNodes(svcCtx.Etcd, c, advertiseHost, port)
	if err != nil {
		// §A.3 第 4 条:不用 logx.Must(直接 os.Exit,defer 不执行)。registerNodes 已把成功的那一次
		// 注册 Close(发现 key 立刻消失),这里再释放 snowflake 租约、刷 Kafka、刷日志后退出。
		logx.Errorf("[match] Failed to register node in etcd; exiting: %v", err)
		cancel()
		svcCtx.Stop()
		s.Stop() // go-zero RpcServer.Stop 只做 logx.Close
		os.Exit(1)
	}

	controlCtx, cancelControl := context.WithCancel(context.Background())
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		prepared := func(prepareCtx context.Context) (*serverbase.PreparedPublication, error) {
			bound := svcCtx.DrainRuntime
			if bound == nil || root.Pending() != 0 || svcCtx.Authority == nil {
				return nil, drainproof.ErrHold
			}
			owner := svcCtx.ConsumerOwner
			if c.GlobalMatch.Enabled || c.RatingEnabled {
				if owner == nil {
					return nil, drainproof.ErrHold
				}
				expected, err := owner.ExpectedPartitions(bound)
				if err != nil {
					return nil, err
				}
				digest, raw, err := owner.PreparedFrontier(prepareCtx, bound, "SOURCE", expected)
				if err != nil {
					return nil, err
				}
				return &serverbase.PreparedPublication{Digest: digest, Raw: raw, WithReady: func(openCtx context.Context, commit func() ([]byte, error)) ([]byte, error) {
					return kafkautil.WithConsumerReadiness(openCtx, raw, []*kafkautil.BoundConsumerGroup{owner}, commit)
				}}, nil
			}
			return &serverbase.PreparedPublication{NoConsumers: true}, nil
		}
		if err := serverbase.DriveDrainRuntime(controlCtx, runtime, *maintenanceControlDir, root, relay, svcCtx.BindDrainRuntime, svcCtx.CloseSource, svcCtx.StopAndReport, prepared); err != nil && controlCtx.Err() == nil {
			logx.Errorf("匹配排空 HOLD: %v", err)
		}
	}()
	defer func() { cancelControl(); <-controlDone }()

	fmt.Println("\n=============================================================")
	fmt.Println("  MATCH SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Advertise:   %s\n", net.JoinHostPort(advertiseHost, strconv.FormatUint(uint64(port), 10)))
	fmt.Printf("  Mode:        %s\n", c.Mode)
	if len(c.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	}
	fmt.Printf("  node_id:     %d (MatchNodeService, etcd CAS)\n", regs.match.Info.NodeId)
	fmt.Printf("  node_uuid:   %s\n", regs.match.Info.NodeUuid)
	fmt.Printf("  team node:   %d (TeamNodeService, uuid=%s)\n", regs.team.NodeID(), regs.team.NodeUUID)
	fmt.Printf("  team cross:  AllowCrossZone=%v\n", c.Team.AllowCrossZone)
	fmt.Printf("  kafka:       %v\n", c.Kafka.Brokers)
	fmt.Println("=============================================================")

	// Snowflake worker id 失租 = 本进程不再是该 worker id 的合法持有者,
	// 继续用 BattleIDGen 发号就是确定性撞号(battle_id/challenge_id/team_id 都会撞)。
	// 与 scene_manager 同口径:先 fence 发号器,再注销两个发现 key(§A.3 第 5 条:os.Exit 不执行 defer,
	// 不主动注销的话路由服 / gate 会在 LeaseTTL 内继续把请求打到将退出的进程),flush Kafka,再退出重启换 id。
	if lost := svcCtx.SnowflakeLost(); lost != nil {
		go func() {
			<-lost
			svcCtx.BattleIDGen.Fence() // 先关闸:此后 Generate 一律 ErrFenced,gather / 建队整体失败
			logx.Error("[match] snowflake worker id lease lost; generator fenced, closing node registrations and flushing Kafka before exit")
			regs.Close()  // 发现 key 立刻消失
			svcCtx.Stop() // flush Kafka 在途批次,再释放 worker lease
			logx.Close()  // 冲掉日志缓冲
			os.Exit(1)    // 退出;重启后拿新 worker id
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logx.Infof("[match] Received signal %v; shutting down", sig)
	case <-serveDone:
		logx.Error("[match] gRPC server exited unexpectedly; shutting down")
	}

	// 退出顺序(§A.3 第 3 条、shared/noderegistry 包注释「使用顺序」):
	//  ① regs.Close():两份注册都删 key + Revoke。必须先于 gRPC 停止 —— 路由服 PickRandom / gate
	//    不看连接状态,只要 key 还在就会继续选中本节点;
	//  ② 等 gRPC 排空:go-zero 的 RpcServer.Stop() 只 logx.Close(),真正的 GracefulStop 由 proc 的
	//    shutdown listener 在收到信号 1s 后执行,所以这里等 serveDone(有上限);
	//  ③ 停后台循环(节点发现 / matcher / 评分消费);
	//  ④ svcCtx.Stop()(flush Kafka、释放 snowflake 租约)→ s.Stop()(logx.Close 放最后,前面的错误日志不丢)。
	// 组队已接纳的异步推送与清锁由同 boot RELAY 等待；TTL 不作为完成证据。
	regs.Close()
	select {
	case <-serveDone:
	case <-time.After(shutdownDrainTimeout):
		logx.Infof("[match] gRPC server did not exit within %v (go-zero does not handle signals on non-Linux platforms); exiting directly", shutdownDrainTimeout)
	}
	cancel()
	svcCtx.Stop()
	s.Stop()
}

func validateMigrationFlags(migrateOnly, allowModify bool) error {
	if allowModify && !migrateOnly {
		return fmt.Errorf("-allow-modify只能与-migrate同时使用，常驻进程不执行DDL")
	}
	return nil
}

// runMigration 不构造ServiceContext，因此不申请号源、不连接etcd/Redis/Kafka、不监听RPC。
func runMigration(c config.Config, allowModify bool, migrate func(context.Context, config.GlobalMatchConf, bool) (schemamigrate.Report, error)) int {
	if !c.GlobalMatch.Enabled || (c.Mode != service.DevMode && c.Mode != service.TestMode && !c.GlobalMatch.RequireTiDB) {
		fmt.Fprintln(os.Stderr, "匹配迁移必须显式启用全球SQL权威，生产必须使用TiDB")
		return schemamigrate.ExitFailed
	}
	// 迁移凭据不进入常驻配置；缺少任一项都不能拿runtime账号补做DDL。
	user := os.Getenv(migrationUserEnv)
	if strings.TrimSpace(user) == "" || os.Getenv(migrationPasswordEnv) == "" {
		fmt.Fprintln(os.Stderr, "显式匹配迁移需要独立MATCH_MIGRATION_USER和MATCH_MIGRATION_PASSWORD环境变量")
		return schemamigrate.ExitFailed
	}
	c.GlobalMatch.User, c.GlobalMatch.PasswordEnv = user, migrationPasswordEnv
	report, err := migrate(context.Background(), c.GlobalMatch, allowModify)
	for _, statement := range report.Statements {
		fmt.Printf("迁移语句: %s\n", statement)
	}
	for _, warning := range report.Warnings {
		fmt.Printf("迁移告警: %s\n", warning)
	}
	for _, manual := range report.Manual {
		fmt.Printf("需人工处理: %s\n", manual)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "匹配schema迁移失败: %v\n", err)
	}
	return schemamigrate.ExitCode(report, err)
}

// nodeRegistrations 本进程在 etcd 的两份节点注册。Close 幂等、可并发调用
// (信号退出与 snowflake 失租强退可能同时走到)。
type nodeRegistrations struct {
	team  *sharedregistry.Registration // TeamNodeService:shared/noderegistry
	match *noderegistry.NodeRegistration

	closeOnce sync.Once
}

// Close 先注销 TeamNodeService 再注销 MatchNodeService(与注册顺序相反)。
func (r *nodeRegistrations) Close() {
	r.closeOnce.Do(func() {
		if r.team != nil {
			r.team.Close()
		}
		if r.match != nil {
			r.match.Close()
		}
	})
}

// registerNodes 按 §A.3 第 3 条的固定顺序注册,两者都在端口可连之后:
//
//  1. TeamNodeService:shared/noderegistry.RegisterAfterListening,先探端口,连不上就拒绝注册。
//     失租策略 ReallocateNewID —— team 不以 node_id 派生任何持久身份或 per-node topic,换号安全;
//  2. MatchNodeService:沿用 match 自己的 internal/noderegistry.Register(上一步已确认端口可连,
//     不另探测;不迁 shared registry,避免顺手改变 match 的 node_id 失租语义)。
//
// 任一步失败返回 error,且已成功的注册已被 Close(撤销租约,发现 key 立刻消失),调用方直接退出即可。
func registerNodes(etcd *clientv3.Client, c config.Config, advertiseHost string, port uint32) (*nodeRegistrations, error) {
	regCtx, regCancel := context.WithTimeout(context.Background(), registerTimeout)
	defer regCancel()

	teamNodeType := base.ENodeType_TeamNodeService
	teamReg, err := sharedregistry.RegisterAfterListening(regCtx, etcd, c.ListenOn, listenProbeTimeout,
		sharedregistry.Spec{
			// Prefix 由枚举名派生,不手写:"TeamNodeService.rpc"。
			Prefix:          base.ENodeType_name[int32(teamNodeType)] + ".rpc",
			NodeType:        uint32(teamNodeType),
			ZoneId:          c.ZoneId,
			LeaseTTL:        c.LeaseTTL,
			BuildValue:      nodeInfoValueBuilder(teamNodeType, c.ZoneId, advertiseHost, port, uint64(time.Now().Unix())),
			OnReclaimFailed: sharedregistry.ReallocateNewID,
			OnNodeIDChanged: func(oldID, newID uint32) {
				logx.Errorf("[match.team] Could not reclaim original TeamNodeService node_id=%d after etcd lease loss; continuing with new node_id=%d", oldID, newID)
			},
			LogPrefix: "[match.team]",
		})
	if err != nil {
		return nil, fmt.Errorf("TeamNodeService 注册失败: %w", err)
	}
	teamReg.KeepAlive()

	matchReg, err := noderegistry.Register(
		etcd,
		uint32(base.ENodeType_MatchNodeService),
		c.ZoneId,
		advertiseHost, port,
		c.LeaseTTL,
	)
	if err != nil {
		teamReg.Close()
		return nil, fmt.Errorf("MatchNodeService 注册失败: %w", err)
	}
	matchReg.KeepAlive()
	return &nodeRegistrations{team: teamReg, match: matchReg}, nil
}

// nodeInfoValueBuilder 返回 shared/noderegistry 的 BuildValue 回调:按 C++ NodeInfo 约定生成 protojson
// (lowerCamelCase 字段名)。字段填法与 internal/noderegistry.Register、go/chat/chat.go 一致;
// 注册前 shared/noderegistry 会用镜像 struct 校验 nodeId / nodeType / zoneId / nodeUuid / 两个 port /
// protocolType,不合格拒注册 —— match_service_test.go 钉住这些字段。
func nodeInfoValueBuilder(nodeType base.ENodeType, zoneId uint32, host string, port uint32, launchTime uint64) func(nodeID uint32, nodeUUID string) ([]byte, error) {
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

// sessionMetadataKey 是 gate 写入、路由服透传的会话元数据键,值 = base64(proto SessionDetails)。
const sessionMetadataKey = "x-session-detail-bin"

// matchInternalMethodPrefix 是 MatchInternal 全部方法的 FullMethod 前缀("/match.MatchInternal/"),
// 由生成的 ServiceDesc 派生,不手写服务名。
var matchInternalMethodPrefix = "/" + matchpb.MatchInternal_ServiceDesc.ServiceName + "/"

// sessionInterceptor 解出 gate 附带的 x-session-detail-bin(base64 protobuf
// SessionDetails)放进 ctx —— 客户端直达协议的权威 player_id 从这里取,
// 请求体里的 player_id 仅供内部调用(照 login 的 SessionInterceptor 模式;
// match 不修改会话,无需回写响应 header)。ClientPlayerTeam 的请求体里没有 player_id,
// 解不出会话时 team 直接回 4001(team-system.md §D.2)。
//
// MatchInternal 是内部服务(guild-phase2 §6.18.3,照 trade 的 TradeAdmin):带会话 metadata 即客户端来源,
// 不论能否解码一律 PermissionDenied、不进 handler;无会话 = 内部调用(guild 直连)放行。
// v1 只靠这一条 + 部署层 NetworkPolicy 收口,调用方不验签(契约偏差 13)。
func sessionInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok && strings.HasPrefix(info.FullMethod, matchInternalMethodPrefix) && len(md.Get(sessionMetadataKey)) > 0 {
		logx.WithContext(ctx).Errorf("[match] Rejecting internal method %s invoked with a session", info.FullMethod)
		return nil, status.Errorf(codes.PermissionDenied, "%s is not callable by clients", info.FullMethod)
	}
	if ok {
		if vals, exists := md[sessionMetadataKey]; exists && len(vals) > 0 {
			bin, err := base64.StdEncoding.DecodeString(vals[0])
			if err != nil {
				logx.Errorf("[match] Failed to decode session metadata base64: %v", err)
			} else {
				var detail base.SessionDetails
				if err := proto.Unmarshal(bin, &detail); err != nil {
					logx.Errorf("[match] Failed to deserialize SessionDetails: %v", err)
				} else {
					ctx = ctxkeys.WithSessionDetails(ctx, &detail)
				}
			}
		}
	}
	return handler(ctx, req)
}

// parseListenOn splits "host:port" into its components.
func parseListenOn(listenOn string) (string, uint32) {
	parts := strings.SplitN(listenOn, ":", 2)
	host := parts[0]
	port := uint32(0)
	if len(parts) == 2 {
		fmt.Sscanf(parts[1], "%d", &port)
	}
	return host, port
}

// isUnspecifiedHost:""、0.0.0.0、:: 这类"监听所有网卡"的 host,不能原样写进 NodeInfo。
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// advertisedHost 决定写进 NodeInfo 的 IP(照 go/chat/chat.go advertisedHost,§A.3 第 3 条):
//  1. POD_IP 环境变量(K8s Downward API,deploy/k8s/manifests/go-svc/match.yaml 注入)优先 ——
//     容器里 ListenOn 恒为 0.0.0.0,原样注册的话别的 Pod 连不上;
//  2. 否则 ListenOn 写了具体 host 就用它(本地 127.0.0.1:50500);
//  3. 否则取本机第一个非 loopback IP;拿不到退回 127.0.0.1(至少本机的路由服 / gate 还连得上)。
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
