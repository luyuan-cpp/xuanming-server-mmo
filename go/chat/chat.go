// chat 服务 v1:全服世界频道 + 私聊,客户端经 gate → 路由服到达(契约 zone_contract_v1 §3/§9)。
//
// 全局一份、多副本、无状态:所有状态在 ChatRedis;注册按 C++ NodeInfo 约定写进 etcd
// (shared/noderegistry),路由服按 ChatNodeService.rpc 前缀发现本服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"chat/internal/config"
	"chat/internal/constants"
	"chat/internal/lifecycle"
	"chat/internal/server"
	"chat/internal/session"
	"chat/internal/svc"

	chatpb "proto/chat"
	base "proto/common/base"

	"shared/grpcstats"
	"shared/killswitch"
	"shared/noderegistry"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
)

var configFile = flag.String("f", "etc/chat.yaml", "the config file")

// nodeType 本服务的节点类型。ChatNodeService=9 早已在 node.proto 里,proto 不改(契约 §9)。
const nodeType = base.ENodeType_ChatNodeService

const (
	// listenProbeTimeout:等本进程 gRPC 端口可连的上限。先探端口再注册,是为了不让路由服
	// 在监听建立前就选中本节点(拨号失败 = 客户端收到 kServiceUnavailable)。
	listenProbeTimeout = 30 * time.Second
	// registerTimeout:注册整体(探端口 + etcd CAS)的上限,须大于 listenProbeTimeout。
	registerTimeout = 45 * time.Second
)

func main() {
	flag.Parse()

	var c config.Config
	// conf.MustLoad 会自动调用 (*Config).Validate(go-zero v1.10.0 core/conf LoadFromJsonBytes 末尾的
	// validate(v)),不合法即 log.Fatalf("error: config file <path>, <Validate 的错误>") 退出。
	// 所以这里不再显式调 c.Validate():那一段永远走不到,留着只会让人以为错误前缀是它打的。
	conf.MustLoad(*configFile, &c)
	if err := runChat(c); err != nil {
		fmt.Fprintf(os.Stderr, "[chat] %v\n", err)
		os.Exit(1)
	}
}

func runChat(c config.Config) (runErr error) {
	// 从启动阶段起接管取消:信号会打断注册请求,失败路径也执行同一资源收尾。
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	lifecycle.Configure(&c.RpcServerConf)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		case <-proc.Done():
			// proc 在 init 已接管信号；补收配置加载期发生、NotifyContext 未看到的退出。
			stopSignals()
		}
		timer := time.NewTimer(lifecycle.HardTimeout)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			fmt.Fprintln(os.Stderr, "[chat] 已到24s停机硬截止,强制退出;不能确认在途请求已完成")
			os.Exit(1)
		}
	}()
	// 已在配置加载期收到退出时不再建立依赖或注册。proc.Done 在 Windows 为 nil。
	select {
	case <-ctx.Done():
		return nil
	case <-proc.Done():
		return nil
	default:
	}
	listenHost, port, err := c.ListenHostPort()
	if err != nil {
		return err
	}

	svcCtx := svc.NewServiceContext(c)

	// Prometheus /metrics(MetricsListenAddr 为空则不开)。
	stopMetrics := svc.StartMetrics(c.MetricsListenAddr)

	// RPC 级热关停(shared/killswitch,D-1 验收第四条):线上某个方法出事时往 etcd 写一个 key
	// 秒级短路它。复用 svcCtx 的 etcd 客户端;Start 非阻塞,etcd 不可达一律放行(fail-open)。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	ks := killswitch.New(killswitch.Config{Prefix: c.KillSwitchPrefix})
	ks.Start(ksCtx, svcCtx.Etcd)

	var serverSlot lifecycle.ServerSlot
	shutdown := &lifecycle.Shutdown{
		DrainTimeout:    lifecycle.DrainTimeout,
		FinishTimeout:   lifecycle.FinishTimeout,
		CloseResources:  func() { ksCancel(); stopMetrics(); svcCtx.Stop() },
		FinishFramework: proc.Shutdown,
	}
	defer func() {
		stopSignals() // 启动失败也从这里启动同一个24s退出硬截止。
		shutdown.Server = serverSlot.TakeForShutdown()
		runErr = errors.Join(runErr, shutdown.Stop())
		_ = logx.Close()
	}()
	serverReady := make(chan struct{})
	s, err := zrpc.NewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		chatpb.RegisterClientPlayerChatServer(grpcServer, server.NewChatServer(svcCtx))
		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
		serverSlot.Publish(grpcServer)
		close(serverReady)
	})
	if err != nil {
		return fmt.Errorf("chat RPC 构造失败: %w", err)
	}
	// go-zero 自带的 trace / recover / stat / prometheus / breaker / shedding / timeout 拦截器
	// 在 NewServer 里已先加入,位于下面这条链的外层;"grpcstats 最外"指本服务自己的链内。
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks)...)

	// 先启动gRPC再注册。保存真实Server用于主动排空,Start错误经通道交回主线程,
	// 让监听失败、注册失败和收到启动期信号都能清理已创建的资源。
	serveDone := make(chan struct{})
	serveErr := make(chan error, 1)
	shutdown.ServeDone = serveDone
	go func() {
		var startErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				startErr = fmt.Errorf("gRPC Start失败: %v", recovered)
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
		return fmt.Errorf("chat监听未就绪: %v", err)
	}
	advertiseHost := advertisedHost(listenHost)
	regCtx, regCancel := context.WithTimeout(ctx, registerTimeout)
	nr, err := noderegistry.RegisterAfterListening(regCtx, svcCtx.Etcd, dialAddress(listenHost, port), listenProbeTimeout,
		noderegistry.Spec{
			// Prefix 由枚举名派生,不手写(契约 §2):"ChatNodeService.rpc"。
			Prefix:     base.ENodeType_name[int32(nodeType)] + ".rpc",
			NodeType:   uint32(nodeType),
			ZoneId:     c.ZoneId,
			LeaseTTL:   c.LeaseTTL,
			BuildValue: nodeInfoValueBuilder(c.ZoneId, advertiseHost, port, uint64(time.Now().Unix())),
			// chat 不以 node_id 派生任何持久身份 / per-node topic,失租后换 id 继续服务即可。
			OnReclaimFailed: noderegistry.ReallocateNewID,
			OnNodeIDChanged: func(oldID, newID uint32) {
				logx.Errorf("[chat] etcd 失租后原 node_id=%d 未能重夺,已换新 node_id=%d 继续服务", oldID, newID)
			},
			LogPrefix: "[chat]",
		})
	regCancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("chat 节点注册 etcd 失败: %w", err)
	}
	shutdown.Unregister = nr.Close
	nr.KeepAlive()

	fmt.Println("\n=============================================================")
	fmt.Println("  CHAT SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Advertise:   %s\n", net.JoinHostPort(advertiseHost, strconv.FormatUint(uint64(port), 10)))
	fmt.Printf("  Mode:        %s\n", c.Mode)
	fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	fmt.Printf("  node_id:     %d (etcd CAS)\n", nr.NodeID())
	fmt.Printf("  node_uuid:   %s\n", nr.NodeUUID)
	fmt.Printf("  zone:        %d (只影响注册路径)\n", c.ZoneId)
	fmt.Printf("  chat_redis:  %s\n", svcCtx.ChatRedisTarget)
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:     %s\n", c.MetricsListenAddr)
	}
	fmt.Println("=============================================================")

	select {
	case <-ctx.Done():
		logx.Info("[chat] 收到退出信号,先注销再排空RPC")
	case err := <-serveErr:
		return fmt.Errorf("chat gRPC服务提前退出: %v", err)
	}
	return nil
}

// buildUnaryInterceptors 组装本服务的一元拦截器链(契约 §3 新口径)。
//
// 返回的切片顺序**就是执行顺序**:go-zero 原样交给 grpc.ChainUnaryInterceptor,第一个是最外层。
// 抽成函数是为了让链本身可测(chat_test.go)——"哪次重构顺手把 killswitch 那行删了"
// 是最容易发生、又最难被发现的回归:开关平时没有任何可观测行为。
func buildUnaryInterceptors(ks *killswitch.Switch) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计最外层:被热关停短路的请求也要被统计到,否则"关停后 QPS 归零"
		//    会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中后会话解码、in-band 定性、Redis 访问统统不做。
		ks.UnaryServerInterceptor(),

		// ③ 会话解码:x-session-detail-bin → ctx。放在 killswitch 之后,被关停的请求不白解码;
		//    放在 serverbase 之前,handler 一定能拿到会话。坏头 / 缺头放行,由逻辑层回 kInvalidParameter。
		session.UnaryServerInterceptor(),

		// ④ in-band 故障定性:handler 一律 `return resp, nil`,gRPC status 恒 OK,
		//    这层把 TipInfoMessage.id 读出来按 Tip.xlsx 的 fault 列定性计数(kServiceUnavailable = 故障告警,
		//    其余 = 业务拒绝)。紧贴 handler,耗时只算业务链路。
		serverbase.UnaryInterceptor(serverbase.Options{
			TipClassifier: constants.TipClassifier(),
		}),
	}
}

// nodeInfoValueBuilder 返回 noderegistry 的 BuildValue 回调:按 C++ NodeInfo 约定生成 protojson。
// shared 不 import proto 模块(契约 §2),所以 NodeInfo 由调用方拼;字段填法照 match registry.go。
// noderegistry 注册前会用镜像 struct 校验 nodeId / zoneId / nodeUuid / 两个 port / protocolType,
// 不合格拒注册 —— chat_test.go 的 TestNodeInfoValueMatchesRegistryContract 钉住这些字段。
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

// dialAddress 是注册前"探端口"用的拨号地址:监听 0.0.0.0 / :: 时本机只能拨 loopback
// (Windows 上拨 0.0.0.0 直接失败)。与写进 NodeInfo 的对外地址是两回事,见 advertisedHost。
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

// advertisedHost 决定写进 NodeInfo 的 IP(与 go/data_service/data_service.go advertisedHost 同一口径):
//  1. POD_IP 环境变量(K8s Downward API)优先 —— 容器里 ListenOn 恒为 0.0.0.0,别的 Pod 连不上;
//  2. 否则 ListenOn 写了具体 host 就用它(本地 127.0.0.1:50700);
//  3. 否则取本机第一个非 loopback IP;拿不到退回 127.0.0.1(至少本机的路由服还连得上)。
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
