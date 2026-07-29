package main

import (
	"context"
	"flag"
	"fmt"
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
	// 所以这里主动停服,让编排把进程拉起来 —— 重启会拿一个新租约,并被 snowflake 的
	// 启动 guard 兜住。s.Stop() 会让下面的 s.Start() 返回,defer 链正常收尾。
	if lost := svcCtx.SnowflakeLost(); lost != nil {
		go func() {
			<-lost
			logx.Error("[scene_manager] snowflake worker id lease lost; stopping the server to avoid minting colliding scene ids")
			s.Stop()
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
