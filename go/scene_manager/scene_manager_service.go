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

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
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
