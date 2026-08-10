package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"strconv"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"player_locator/internal/config"
	"player_locator/internal/logic"
	"player_locator/internal/node"
	"player_locator/internal/server"
	"player_locator/internal/svc"
	proto_common "proto/common/base"
	pb "proto/player_locator"
	"shared/grpcstats"
	"shared/safego"
	"shared/serverbase"
)

var configFile = flag.String("f", "etc/player_locator.yaml", "config file path")

const nodeType = uint32(proto_common.ENodeType_PlayerLocatorNodeService)

func main() {
	flag.Parse()
	conf.MustLoad(*configFile, &config.AppConfig)

	svcCtx := svc.NewServiceContext(config.AppConfig)
	defer svcCtx.Stop()

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
	logx.Infof("Node registered: id=%d uuid=%s", n.Info.NodeId, n.Info.NodeUuid)

	// Start background lease monitor
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	defer leaseCancel()

	// 这两条后台链路都用 safego.Go 而不是裸 `go`:它们各自的循环体已经在内部
	// 逐轮 recover,这里的外层只是最后一道兜底 —— 万一循环体外的部分(初始化、
	// ticker、通道收尾)panic,裸 goroutine 会把整个进程当场打死,而租约清理与
	// 会话对账正是"进程活着但后台已停摆"最难被发现的两处。兜住之后至少留下
	// 稳定事件名 + safego_panic_total{point} 可以告警。
	safego.Go("player_locator.lease_monitor", func() {
		logic.StartLeaseMonitor(
			leaseCtx,
			svcCtx,
			config.AppConfig.Lease.PollInterval,
			config.AppConfig.Lease.BatchSize,
		)
	})

	// 会话对账扫描:兜住 gate 整机崩溃(断线回调不执行)导致的永久 ONLINE 会话,
	// 恢复「所有会话终点必经租约链」的闭环。见 session_reconciler.go 顶部注释。
	safego.Go("player_locator.session_reconciler", func() {
		logic.StartSessionReconciler(
			leaseCtx,
			svcCtx,
			config.AppConfig.Registry.Etcd.Hosts,
			config.AppConfig.Registry.Etcd.DialTimeout,
			config.AppConfig.Lease.ReconcileIntervalSeconds,
		)
	})

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterPlayerLocatorServer(grpcServer, server.NewPlayerLocatorServer(svcCtx))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	// in-band 故障拦截器。PlayerLocator 的响应里没有业务码字段(Empty /
	// GetSessionResponse.found / ReconnectResponse.success + 自由文本 error_message),
	// 所以 serverbase 在这里只会把每次调用记进 rpc_duration_seconds:
	// handler 返 error 的记 status=transport_error,否则记 ok。
	// 零值 Options 即可 —— 没有码表就不该配任何 Classifier,免得凭空判故障。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{}))
	defer s.Stop()

	fmt.Println("\n=============================================================")
	fmt.Println("  PLAYER_LOCATOR SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", config.AppConfig.ListenOn)
	fmt.Printf("  Mode:        %s\n", config.AppConfig.Mode)
	fmt.Printf("  zone_id:     %d\n", config.AppConfig.Node.ZoneId)
	if len(config.AppConfig.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", config.AppConfig.Etcd.Hosts)
	}
	fmt.Printf("  kafka:       %v\n", config.AppConfig.Kafka.Brokers)
	fmt.Printf("  redis:       %s\n", config.AppConfig.RedisClient.Host)
	fmt.Println("=============================================================")
	s.Start()
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
