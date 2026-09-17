// client_rpc_router:gate 唯一的 gRPC 目标(docs/design/client-rpc-router.md)。
//
// 无状态、无 Redis、无 Kafka:客户端可见的 gRPC 类消息由 gate 原包交给本服务,
// 本服务按生成路由表(generated/pb/game/route_table.go)以原始字节转发到
// login / match / chat / … 的某个实例,响应字节原样回给 gate。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"client_rpc_router/internal/config"
	"client_rpc_router/internal/metrics"
	"client_rpc_router/internal/noderegistry"
	"client_rpc_router/internal/server"
	"client_rpc_router/internal/svc"

	pb "proto/client_rpc_router"
	base "proto/common/base"

	"shared/buildinfo"
	"shared/grpcstats"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/client_rpc_router.yaml", "the config file")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("client_rpc_router"))
	if *showVersion {
		return
	}

	var c config.Config
	conf.MustLoad(*configFile, &c)
	// NewServiceContext 内含 Validate:配置错(如 Timeout ≤ ForwardTimeoutMs、
	// ZoneScopedNodeTypes 名字非法)直接 panic,不带病起服。
	svcCtx := svc.NewServiceContext(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Prometheus /metrics(MetricsListenAddr 为空则不开)。
	metrics.Start(c.MetricsListenAddr)

	// 目标业务服务发现:路由表里每个客户端协议目标类型(非 Battle)一个 etcd
	// list-watch 镜像,连接按 endpoint 缓存、节点消失即清。
	svcCtx.RunWatchers(ctx)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterClientRpcRouterServer(grpcServer, server.NewClientRpcRouterServer(svcCtx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 会话元数据的透传与回写在 Forward 逻辑里按原值处理,本服务不解析它,
	// 所以不像 match 那样挂 sessionInterceptor;只挂流量统计。
	s.AddUnaryInterceptors(
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),
	)
	defer s.Stop()

	// 照 C++ NodeInfo 约定注册进 etcd(PROTOCOL_GRPC),gate 按
	// ClientRpcRouterNodeService.rpc/ 前缀发现本服务。
	// 写进 NodeInfo 的是**对外地址**而不是监听地址:K8s 上 ListenOn 恒为 0.0.0.0,原样注册的话
	// gate 拿到 0.0.0.0 拨不通(本地 ListenOn=127.0.0.1 不受影响)。口径与 chat / data_service 的 advertisedHost 一致。
	listenHost, port := parseListenOn(c.ListenOn)
	nr, err := noderegistry.Register(
		svcCtx.Etcd,
		uint32(base.ENodeType_ClientRpcRouterNodeService),
		c.ZoneId,
		advertisedHost(listenHost), port,
		c.LeaseTTL,
	)
	if err != nil {
		panic("failed to register ClientRpcRouter node in etcd: " + err.Error())
	}
	nr.KeepAlive()
	defer nr.Close()

	targetNames := make([]string, 0, len(svcCtx.Targets))
	for nodeType := range svcCtx.Targets {
		targetNames = append(targetNames, base.ENodeType_name[int32(nodeType)])
	}

	// 「STARTED SUCCESSFULLY」是 tools/scripts/go_services.ps1 判定就绪的关键字,不要改。
	fmt.Println("\n=============================================================")
	fmt.Println("  CLIENT RPC ROUTER STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Mode:        %s\n", c.Mode)
	if len(c.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	}
	fmt.Printf("  node_id:     %d (etcd CAS)\n", nr.Info.NodeId)
	fmt.Printf("  node_uuid:   %s\n", nr.Info.NodeUuid)
	fmt.Printf("  targets:     %s\n", strings.Join(targetNames, ", "))
	fmt.Printf("  zone_scoped: %v\n", c.ZoneScopedNodeTypes)
	fmt.Printf("  fwd_timeout: %d ms\n", c.ForwardTimeoutMs)
	fmt.Println("=============================================================")

	s.Start()
}

// parseListenOn 把 "host:port" 拆成两部分(与 match 同写法)。
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

// advertisedHost 决定写进 NodeInfo 的 IP(与 go/chat/chat.go、go/data_service/data_service.go 同一口径):
//  1. POD_IP 环境变量(K8s Downward API,manifests/go-svc/client-rpc-router.yaml 注入)优先;
//  2. 否则 ListenOn 写了具体 host 就用它(本地 127.0.0.1:50600);
//  3. 否则取本机第一个非 loopback IP;拿不到退回 127.0.0.1(至少本机的 gate 还连得上)。
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
