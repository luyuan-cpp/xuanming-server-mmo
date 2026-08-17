package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	base "proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/agones"
	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/logic"
	"scene_manager/internal/metrics"
	"scene_manager/internal/noderegistry"
	"scene_manager/internal/server"
	"scene_manager/internal/svc"
	"shared/generated/table"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/leader"
	"shared/safego"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/scene_manager_service.yaml", "the config file")

// safePointSnowflakeLeaseWatch 是 snowflake worker id 租约丢失守望者的
// safego 点位名(会变成 safego_panic_total 的 label,必须是常量)。
const safePointSnowflakeLeaseWatch = "scene_manager.snowflake_lease_watch"

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	// 再入屏障:进程起来之前先把常数与配置校验一遍(设计文档
	// docs/design/scene-owner-reentry-barrier.md §3.2 要求的"机械校验")。
	//
	// 常数之间劈叉是**代码错误**,而且后果是静默的双写窗口 —— 直接 panic 让
	// Pod 起不来(K8s 会 CrashLoopBackOff 并告警),远好过带着一个假屏障跑。
	if err := constants.ValidateSceneReentryBarrier(); err != nil {
		panic(fmt.Sprintf("再入屏障常数自检失败: %v", err))
	}
	// 配置低于安全下限是**运维错误**,但安全值是已知的:钳回下限并大声报出来,
	// 比让进程起不来更合适(屏障调高永远是安全的,进程停摆不是)。
	reentryBarrier, barrierErr := constants.ResolveSceneReentryBarrier(c.SceneReentryBarrierSeconds)
	if barrierErr != nil {
		logx.Errorf("[ReentryBarrier] 配置不合理: %v", barrierErr)
	}

	svcCtx := svc.NewServiceContext(c)
	defer svcCtx.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register debug HTTP handlers before starting the metrics server so
	// they are available from the moment the port opens. Currently only
	// the rebalance planner is exposed; add more via metrics.RegisterDebugHandler.
	logic.RegisterRebalanceDebugHandler(svcCtx)

	// Start Prometheus metrics endpoint (no-op when MetricsListenAddr is empty).
	metrics.Start(c.MetricsListenAddr)

	// 选主:变更类后台循环(补频道 / rebalance / 死节点孤儿清理 / 空闲副本
	// 销毁 / 自动扩缩容 / Agones 对账)全局只允许一个副本执行;RPC 数据面与
	// etcd watch 内存镜像每个副本照常跑。这是 scene_manager 去单点的关键:
	// 之前 k8s 只能用 replicas:1 + Recreate 兜"无选主的单写者循环"的口子,
	// 现在选主抽到了 shared/leader,副本数已放开(见 scene-manager.yaml)。
	// 必须在所有后台循环启动之前接线,否则首拍可能带着"人人都是领导者"
	// 的缺省闸门跑变更动作。
	leaderLockKey := c.LeaderLockKey
	if leaderLockKey == "" {
		leaderLockKey = "scene_manager:leader:lock"
	}
	leaderLockTTL := time.Duration(c.LeaderLockTTLSeconds) * time.Second
	if leaderLockTTL <= 0 {
		leaderLockTTL = 30 * time.Second
	}
	electorID, _ := os.Hostname()
	elector := leader.New(leader.NewGoZeroStore(svcCtx.Redis), leaderLockKey, leader.Options{
		TTL: leaderLockTTL,
		ID:  electorID,
		OnStateChange: func(leading bool) {
			metrics.SetLeader(leading)
			if !leading {
				// 只有领导者刷新的 gauge(Agones 漂移 / rebalance 积压)
				// 降级后会滞留旧值,直接清掉序列。
				metrics.ResetLeaderGauges()
			}
			// 领导权一变就踢一次 fullSync:新任领导者立即补齐跟随期间
			// 跳过的变更动作(补频道 / rebalance / 清理),不等 watch 中断。
			logic.RequestLoadReporterResync()
		},
	})
	logic.SetLeaderCheck(elector.IsLeader)
	// 先把 gauge 置 0,首轮选举完成前 /metrics 上就能看到本副本
	// (与 login dispatcherIsLeaderGauge 的处理一致)。
	metrics.SetLeader(false)
	if c.LeaderEligible {
		go elector.Run(ctx, nil)
		// 优雅让位:go-zero 的 SIGTERM 走 os.Exit,main 的 defer 不执行,
		// 不主动放锁的话接任者要等满 TTL(30s)—— 每次滚动更新都会多出
		// 一段无领导窗口。挂 shutdown listener 在退出前用属主校验脚本放锁。
		proc.AddShutdownListener(elector.Stop)
	} else {
		// 金丝雀模式:不竞选,IsLeader 恒 false,只服务 RPC 数据面。
		// 流量占比 = 本副本数 / 同 zone 实例总数(etcd 发现 + playerId % N)。
		logx.Info("[scene_manager] LeaderEligible=false: not campaigning for leadership (canary mode, data plane only)")
	}

	// Agones 容量预占。默认关闭 —— 不配 Agones.Enabled 就是接入之前的行为。
	//
	// 构造失败**必须 panic**,不能降级成"当作没开 Agones 继续跑":
	// 配置说要用 Agones 说明运维期望容量由 Agones 权威管理,静默绕过
	// 会让房间被塞进 Agones 认为已经满了的进程,而且没有任何人会发现。
	// 宁可这个 Pod 起不来(K8s 会 CrashLoopBackOff 并告警)。
	if c.Agones.Enabled {
		allocator, err := agones.NewK8sAllocatorInCluster(agones.K8sAllocatorOptions{
			HighDensity:            c.Agones.HighDensityEnabled,
			CounterRollbackRetries: c.Agones.CounterRollbackRetries,
			CounterRollbackBackoff: time.Duration(c.Agones.CounterRollbackBackoffMs) * time.Millisecond,
			RequestTimeout:         time.Duration(c.Agones.RequestTimeoutMs) * time.Millisecond,
		})
		if err != nil {
			panic(fmt.Sprintf("Agones.Enabled=true but allocator init failed: %v", err))
		}
		logic.SetAgonesAllocator(allocator)
		logx.Infof("[Agones] allocator enabled (high_density=%v namespace=%q)",
			c.Agones.HighDensityEnabled, c.Agones.Namespace)

		// 周期性比对 Agones / Redis 计数,发现漂移只告警不自动改写。
		logic.StartAgonesReconcile(ctx, svcCtx)
	}

	// 后台常驻链路一律走 safego.Go:裸 `go` 里 panic 会当场打死进程,日志里
	// 只剩一段 runtime 栈,看不出是哪条链路炸的。safego 兜住之后会带着点位名
	// 进 safego_panic_total{point="..."}(压测期恒 0 是健康判据)。
	//
	// ⚠️ 边界:safego 只保证"panic 不打死进程",不保证这条链路还在跑 ——
	// StartLoadReporter 自己有重试外层循环,但若它整体 panic 退出,SceneManager
	// 就此失去节点感知而进程仍然"健康"。所以 panic 指标必须配告警,不能只看存活。

	// Start load reporter (discovers scene nodes from etcd, inits main scenes for new zones).
	safego.Go(logic.SafePointLoadReporter, func() { logic.StartLoadReporter(ctx, svcCtx) })

	// Start instance lifecycle manager (auto-destroys idle instances).
	safego.Go(logic.SafePointInstanceLifecycle, func() { logic.StartInstanceLifecycleManager(ctx, svcCtx) })

	// 大世界频道按人数自动扩缩容(默认关闭)。
	logic.StartWorldAutoscaler(ctx, svcCtx)

	// RPC 级热关停(shared/killswitch):线上某个方法把依赖打爆时,往 etcd 写一个
	// key 就能秒级把它短路掉,不必走一遍构建-发布-滚动更新。
	//
	// 复用 svcCtx.Etcd(服务上下文里那条已有的连接),不另开第二条 ——
	// 连的是同一个集群,而且它的生存期与进程一致。ctx 是上面那个进程级
	// 后台 ctx,main 返回时 cancel,watch 循环随之收尾。
	//
	// Start 非阻塞:客户端为 nil、etcd 连不上、前缀下没有 key,一律放行
	// (fail-open),因此这里既不需要判错也不需要 panic。
	ks := killswitch.New(killswitch.Config{Prefix: c.KillSwitchPrefix})
	ks.Start(ctx, svcCtx.Etcd)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		scene_manager.RegisterSceneManagerServer(grpcServer, server.NewSceneManagerServer(svcCtx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks)...)
	defer s.Stop()

	// Register with etcd in C++ NodeInfo convention so Scene nodes can discover us.
	host, port := parseListenOn(c.ListenOn)
	nr, err := noderegistry.Register(
		svcCtx.Etcd,
		uint32(base.ENodeType_SceneManagerNodeService),
		c.ZoneId,
		host, port,
		c.LeaseTTL,
	)
	if err != nil {
		panic("failed to register SceneManager node in etcd: " + err.Error())
	}
	nr.KeepAlive()
	defer nr.Close()

	fmt.Println("\n=============================================================")
	fmt.Println("  SCENE_MANAGER SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Mode:        %s\n", c.Mode)
	fmt.Printf("  multi-zone:  true (zone-agnostic)\n")
	fmt.Printf("  main scenes: %d (from World.json)\n", len(table.WorldTableManagerInstance.FindAll()))
	if len(c.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	}
	fmt.Printf("  node_id:     %d (etcd CAS)\n", nr.Info.NodeId)
	fmt.Printf("  node_uuid:   %s\n", nr.Info.NodeUuid)
	fmt.Printf("  kafka:       %v\n", c.Kafka.Brokers)
	// 屏障值打进启动横幅:它是一条跨语言约定(C++ kDrainBudget),排查双写/回档
	// 时第一句要问的就是"这个进程当时的屏障是多少"。
	fmt.Printf("  reentry barrier: %v (C++ drain %v + skew %v)\n",
		reentryBarrier, constants.CppNodeDrainBudget, constants.ReentryBarrierClockSkewMargin)
	fmt.Println("=============================================================")

	// Snowflake worker id 的 etcd 租约丢了 = 本进程不再是这个 worker id 的合法持有者,
	// etcd 随时会把它分给别的进程。再用 SceneIDGen 发一个 scene_id 就是确定性撞号,
	// 所以这里先 fence 发号器、显式 flush Kafka，再退出让编排拉起新进程。
	// os.Exit 不执行 defer，因此 flush 必须在强退分支里直接调用。
	//
	// ⏱ 时间预算(§租约与重启时间预算必须闭合):Lost() 由 snowflakealloc 的**自 fencing**
	// 提前触发 —— 距上次成功续租超过 TTL 的 2/3(TTL=60s ⇒ 40s)就报信,而不是干等
	// KeepAlive channel 关闭(那恒晚于服务端过期点)。收到信号时服务端 lease 通常还有
	// 约 TTL/3(≈20s)才过期,这段余量用来让在途请求干净失败。
	//
	// ⚠️ 顺序不能反,而且**不能只调 s.Stop()**:go-zero 的 zrpc.RpcServer.Stop() 实测
	// (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),既不拒新请求也不排空在途。
	// 更要命的是 scene_id 有一半发号点根本不在 gRPC 请求链上(load_reporter /
	// world_autoscale 的后台 ticker → world_init.go),就算 gRPC 真能停也拦不住它们。
	// 所以正确性只能由 Fence() 保证:此后 Generate 一律 ErrFenced,建场景与铺频道整体失败。
	// scene_id 撞号后果比建帮更重 —— createscenelogic 拿到 id 后是裸 Redis SET
	// (scene:{id}:node 无 CAS),两个场景共用一个 id 会互相覆盖路由且静默。
	if lost := svcCtx.SnowflakeLost(); lost != nil {
		safego.Go(safePointSnowflakeLeaseWatch, func() {
			<-lost
			svcCtx.SceneIDGen.Fence() // ① 先关闸,后台 ticker 也一并失效
			elector.Stop()            // ①' 让位(尽力而为):不放锁接任者要等满 TTL
			logx.Error("[scene_manager] snowflake worker id lease lost; generator fenced, flushing Kafka before exit")
			svcCtx.Stop() // ② Close/flush Async Kafka pending batches,再释放 worker lease
			logx.Close()  // ③ 冲掉日志缓冲
			os.Exit(1)    // ④ 退出;重启后拿新 worker id
		})
	}

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
// 见 scene_manager_service_test.go。
func buildUnaryInterceptors(ks *killswitch.Switch) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计放最外层:被热关停短路掉的请求也必须被统计到。
		//    否则"关停生效后这个方法的 QPS 归零"会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中之后 in-band 定性、handler、
		//    以及 handler 里的 Redis / Kafka / Agones 访问统统不做 —— 止血阀的
		//    全部意义就是"别为一个已经关停的方法做任何无谓的工作"。
		//    它放在 serverbase 之前也意味着被关停的调用不进 rpc_duration_seconds:
		//    那条指标衡量的是业务链路,而关停请求根本没走业务链路,
		//    它们的账记在 killswitch_blocked_total{method} 上。
		//
		//    ⚠️ 关停范围要克制:它只挡得住 gRPC 入口,挡不住后台 ticker
		//    (load_reporter / world_autoscale 也会建场景发 scene_id)。
		//    把 CreateScene 关掉不等于"没有场景再被创建"。
		ks.UnaryServerInterceptor(),

		// ③ in-band 故障拦截器:本服务的失败**几乎全部**塞在响应体的 error_code 里,
		//    handler 返回的 gRPC status 恒 OK。不挂这一层的话,监控看到的成功率永远是
		//    100%,而玩家正在被拒进场。它只观测,不改响应内容、不吞错、不把业务码翻成
		//    gRPC status,挂上对客户端完全无感。
		//
		//    码表是 scene_manager 私有的(internal/constants/errors.go),与 tip 码表
		//    数值区间重叠,所以必须显式给 Classifier —— 留 nil 会把所有非 0 码都记成
		//    "正常业务拒绝",故障就此隐身。
		//
		//    判定原则:码指向**服务端自身或其依赖出错**才算 fault;客户端参数错误、
		//    幂等冲突、以及"这次先别改"的可重试拒绝都不算。
		serverbase.UnaryInterceptor(serverbase.Options{
			ErrorCodeClassifier: serverbase.FaultCodeSet(
				constants.ErrNoAvailableNode,   // 整个 zone 没有可用场景节点 —— 容量/调度故障
				constants.ErrSceneLookupFailed, // 场景映射查不到 —— 服务端状态缺失
				constants.ErrUpdateLocation,    // 写 PlayerLocation 失败 —— 存储依赖
				constants.ErrEncodeEvent,       // 服务端自己序列化不出来
				constants.ErrKafkaRoute,        // 路由命令发不出去 —— 依赖故障
				constants.ErrRedis,             // Redis 不可用
				constants.ErrNoNodeForPurpose,  // 该用途没有任何节点 —— 部署/调度故障
				// 刻意**不算**故障,记在这里免得下个人反复纠结:
				//   ErrInvalidNodeID / ErrInvalidGateID / ErrInvalidSceneType /
				//   ErrNoSceneConfId            请求参数问题
				//   ErrDuplicateScene           幂等冲突,不是故障
				//   ErrSourceSceneGone          源场景已销毁,业务规则拒绝
				//   ErrUnsafeCrossNodeHandoff   安全门禁按设计拒绝,拒得越多越说明它在工作
				//   ErrEnterSceneInProgress / ErrEnterSceneIdempotencyConflict  去重语义
				//   ErrSceneReentryBarrier      屏障未到的可重试拒绝(见再入屏障)
			),
		}),
	}
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
