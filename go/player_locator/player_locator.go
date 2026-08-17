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
	"shared/killswitch"
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
	//
	// leaseCtx 是本进程所有后台常驻链路的公共取消信号(租约清理、会话对账、
	// 热关停 watch)。它的 defer 必须排在 `defer n.Close()` 之后 —— defer 是
	// 后进先出,先取消后台链路,再关 etcd 客户端,否则 watch 会撞上一个已经
	// Close 的 client。
	leaseCtx, leaseCancel := context.WithCancel(context.Background())
	defer leaseCancel()

	// RPC 级热关停(shared/killswitch):线上某个方法把依赖打爆时,往 etcd 写一个
	// key 就能秒级把它短路掉,不必走一遍构建-发布-滚动更新。
	//
	// 复用 node 的 etcd 客户端,不另开第二条连接 —— 连的是同一个集群。
	// Start 非阻塞:客户端为 nil、etcd 连不上、前缀下没有 key,一律放行(fail-open),
	// 因此这里既不需要判错也不需要 logx.Must。
	ks := killswitch.New(killswitch.Config{Prefix: config.AppConfig.KillSwitchPrefix})
	ks.Start(leaseCtx, n.EtcdClient())

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
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks)...)
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

// buildUnaryInterceptors 组装本服务的一元拦截器链。
//
// 返回的切片顺序**就是执行顺序**:go-zero 把它们原样交给
// grpc.ChainUnaryInterceptor,第一个是最外层。
//
// 抽成函数而不是直接在 main 里连着调 AddUnaryInterceptors,是为了让链本身可测 ——
// main() 没法在单测里跑起来,而"哪次重构顺手把 killswitch 那行删了"是最容易发生、
// 又最难被发现的回归:开关删掉之后一切照常工作,只有真出事那天才发现止血阀是假的。
// 见 player_locator_test.go。
func buildUnaryInterceptors(ks *killswitch.Switch) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计放最外层:被热关停短路掉的请求也必须被统计到。
		//    否则"关停生效后这个方法的 QPS 归零"会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中之后 in-band 定性、handler、
		//    以及 handler 里的 Redis 访问统统不做 —— 止血阀的全部意义
		//    就是"别为一个已经关停的方法做任何无谓的工作"。
		//    它放在 serverbase 之前也意味着被关停的调用不进 rpc_duration_seconds:
		//    那条指标衡量的是业务链路,而关停请求根本没走业务链路,
		//    它们的账记在 killswitch_blocked_total{method} 上。
		//
		//    ⚠️ 关停 PlayerLocator 的方法要格外克制:SetSession / SetDisconnecting
		//    是"所有会话终点必经租约链"的写入点,把它们关掉会留下永久 ONLINE 的
		//    会话(要靠 session_reconciler 事后补)。真要止血优先关只读方法。
		ks.UnaryServerInterceptor(),

		// ③ in-band 故障拦截器。PlayerLocator 的响应里没有业务码字段(Empty /
		//    GetSessionResponse.found / ReconnectResponse.success + 自由文本 error_message),
		//    所以 serverbase 在这里只会把每次调用记进 rpc_duration_seconds:
		//    handler 返 error 的记 status=transport_error,否则记 ok。
		//    零值 Options 即可 —— 没有码表就不该配任何 Classifier,免得凭空判故障。
		serverbase.UnaryInterceptor(serverbase.Options{}),
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
