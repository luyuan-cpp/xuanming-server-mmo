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
	"scene_manager/internal/logic"
	"scene_manager/internal/metrics"
	"scene_manager/internal/noderegistry"
	"scene_manager/internal/server"
	"scene_manager/internal/svc"
	"shared/generated/table"
	"shared/grpcstats"
	"shared/leader"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/scene_manager_service.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
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

	// Start load reporter (discovers scene nodes from etcd, inits main scenes for new zones).
	go logic.StartLoadReporter(ctx, svcCtx)

	// Start instance lifecycle manager (auto-destroys idle instances).
	go logic.StartInstanceLifecycleManager(ctx, svcCtx)

	// 大世界频道按人数自动扩缩容(默认关闭)。
	logic.StartWorldAutoscaler(ctx, svcCtx)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		scene_manager.RegisterSceneManagerServer(grpcServer, server.NewSceneManagerServer(svcCtx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
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
		go func() {
			<-lost
			svcCtx.SceneIDGen.Fence() // ① 先关闸,后台 ticker 也一并失效
			elector.Stop()            // ①' 让位(尽力而为):不放锁接任者要等满 TTL
			logx.Error("[scene_manager] snowflake worker id lease lost; generator fenced, flushing Kafka before exit")
			svcCtx.Stop() // ② Close/flush Async Kafka pending batches,再释放 worker lease
			logx.Close()  // ③ 冲掉日志缓冲
			os.Exit(1)    // ④ 退出;重启后拿新 worker id
		}()
	}

	s.Start()
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
