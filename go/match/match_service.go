package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"match/internal/config"
	"match/internal/metrics"
	"match/internal/noderegistry"
	"match/internal/pkg/ctxkeys"
	"match/internal/server"
	"match/internal/svc"
	"match/internal/team"

	matchkafka "match/internal/kafka"
	matchlogic "match/internal/logic"

	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"
	teampb "proto/team"

	"shared/buildinfo"
	"shared/grpcstats"
	sharedregistry "shared/noderegistry"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var configFile = flag.String("f", "etc/match_service.yaml", "the config file")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

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

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置 / 依赖阶段就崩溃时也已留下"跑的是哪一版"
	// (shared/buildinfo;理由同 cpp/nodes/gate/gate_version.h 头注释)。
	fmt.Println(buildinfo.StartupLine("match"))
	if *showVersion {
		return
	}

	var c config.Config
	conf.MustLoad(*configFile, &c)
	svcCtx := svc.NewServiceContext(c)

	ctx, cancel := context.WithCancel(context.Background())

	// Prometheus /metrics(MetricsListenAddr 为空则不开)。
	metrics.Start(c.MetricsListenAddr)
	// 本实例生效的组队跨区开关;多实例取值不一致 = 滚动切换中(team-system.md J-13a)。
	metrics.SetTeamCrossZoneAllowed(c.Team.AllowCrossZone)

	// scene / battle 节点发现:etcd list-watch 内存镜像
	// (gather 定位 PrepareBattle / CreateBattle 对端用,照 LoadReporter 模式;
	// 组队给 scene 发 PlayerTeamRefreshEvent 也从这里取 NodeUuid)。
	safego.Go("match.watch.scene", func() { svcCtx.SceneNodes.Run(ctx, svcCtx.Etcd) })
	safego.Go("match.watch.battle", func() { svcCtx.BattleNodes.Run(ctx, svcCtx.Etcd) })

	// matcher loop:定时扫队列凑单。多实例安全由每个 (mode, config) 队列的
	// Redis SETNX 锁保证,match 服务本身无状态、可水平扩(设计文档 §5.4)。
	matchlogic.StartMatcherLoop(ctx, svcCtx)

	// 对局结果回流(cross-zone-matchmaking.md §11):消费 battle 发的
	// BattleResultEvent 更新 Elo。消费者不 import logic(会成环),入账逻辑
	// 以回调注入;RatingEnabled=false 时不消费,评分停在默认 1500。
	if c.RatingEnabled {
		startResults := func() error {
			return matchkafka.StartResultConsumer(ctx, matchkafka.ResultConsumerConfig{
				Brokers:    c.Kafka.Brokers,
				Topic:      c.ResultTopic,
				GroupID:    c.ResultConsumerGroup,
				Partitions: c.ResultTopicPartitions,
			}, func(_ context.Context, event *kafkapb.BattleResultEvent) error {
				_, err := matchlogic.ApplyBattleResult(svcCtx, event)
				return err
			})
		}
		// 评分是软数据:Kafka 暂不可达(EnsureTopics / 建 reader 失败)不能拖死匹配本身
		// (复审:RatingEnabled=true 让 match 启动硬依赖 Kafka)。降级为告警 + 30s 后台重试,
		// 期间排队/凑单照常、评分停在当前值;重试成功后从最早 offset 补消费,不丢结果。
		if err := startResults(); err != nil {
			logx.Errorf("[rating] 对局结果消费者启动失败,评分暂停更新、30s 后后台重试: %v", err)
			safego.Go("match.kafka.results.retry", func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(30 * time.Second):
					}
					if err := startResults(); err != nil {
						logx.Errorf("[rating] 对局结果消费者重试启动失败,30s 后再试: %v", err)
						continue
					}
					logx.Info("[rating] 对局结果消费者重试启动成功")
					return
				}
			})
		}
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

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		matchpb.RegisterMatchServiceServer(grpcServer, server.NewMatchServiceServer(svcCtx))
		teampb.RegisterClientPlayerTeamServer(grpcServer, team.NewServer(teamService))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 拦截器链作用于同一 server 上的全部 service(MatchService 与 ClientPlayerTeam),顺序不变:
	// sessionInterceptor 最外层把 x-session-detail-bin 解进 ctx(team 的调用者身份只从这里取,
	// 缺失即 fail-closed 回 4001);grpcstats 在内层计流量。
	// zrpc 顶层 Timeout 保持 5000:改小会截断 WatchBattle 同步链;team 在每个方法入口自设 3500ms 预算(§A.3 第 7 条)。
	s.AddUnaryInterceptors(
		sessionInterceptor,
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
		logx.Errorf("[match] 节点注册 etcd 失败,进程退出: %v", err)
		cancel()
		svcCtx.Stop()
		s.Stop() // go-zero RpcServer.Stop 只做 logx.Close
		os.Exit(1)
	}

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
		logx.Infof("[match] 收到信号 %v,开始退出", sig)
	case <-serveDone:
		logx.Error("[match] gRPC 服务意外退出,开始退出")
	}

	// 退出顺序(§A.3 第 3 条、shared/noderegistry 包注释「使用顺序」):
	//  ① regs.Close():两份注册都删 key + Revoke。必须先于 gRPC 停止 —— 路由服 PickRandom / gate
	//    不看连接状态,只要 key 还在就会继续选中本节点;
	//  ② 等 gRPC 排空:go-zero 的 RpcServer.Stop() 只 logx.Close(),真正的 GracefulStop 由 proc 的
	//    shutdown listener 在收到信号 1s 后执行,所以这里等 serveDone(有上限);
	//  ③ 停后台循环(节点发现 / matcher / 评分消费);
	//  ④ svcCtx.Stop()(flush Kafka、释放 snowflake 租约)→ s.Stop()(logx.Close 放最后,前面的错误日志不丢)。
	// 组队的异步副作用(推送、EndMatch 清开战锁)不等:开战锁靠自然过期(5 人 83s),推送至多一次。
	regs.Close()
	select {
	case <-serveDone:
	case <-time.After(shutdownDrainTimeout):
		logx.Infof("[match] gRPC 服务 %v 内未退出(非 Linux 平台 go-zero 不接管信号),直接退出", shutdownDrainTimeout)
	}
	cancel()
	svcCtx.Stop()
	s.Stop()
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
				logx.Errorf("[match.team] etcd 失租后原 TeamNodeService node_id=%d 未能重夺,已换新 node_id=%d 继续服务", oldID, newID)
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

// sessionInterceptor 解出 gate 附带的 x-session-detail-bin(base64 protobuf
// SessionDetails)放进 ctx —— 客户端直达协议的权威 player_id 从这里取,
// 请求体里的 player_id 仅供内部调用(照 login 的 SessionInterceptor 模式;
// match 不修改会话,无需回写响应 header)。ClientPlayerTeam 的请求体里没有 player_id,
// 解不出会话时 team 直接回 4001(team-system.md §D.2)。
func sessionInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals, exists := md["x-session-detail-bin"]; exists && len(vals) > 0 {
			bin, err := base64.StdEncoding.DecodeString(vals[0])
			if err != nil {
				logx.Errorf("[match] session metadata base64 解码失败: %v", err)
			} else {
				var detail base.SessionDetails
				if err := proto.Unmarshal(bin, &detail); err != nil {
					logx.Errorf("[match] SessionDetails 反序列化失败: %v", err)
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
