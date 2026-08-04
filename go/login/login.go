// cmd/loginservice/main.go
package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"login/internal/config"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/node"
	loginserver "login/internal/server/clientplayerlogin"
	loginadminserver "login/internal/server/loginadmin"
	loginpregateserver "login/internal/server/loginpregate"
	"login/internal/svc"
	"net"
	"os"
	login_proto "proto/common/base"
	login_proto_login "proto/login"
	"shared/grpcstats"
	"shared/kafkautil"
	"shared/snowflakealloc"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"
)

var configFile = flag.String("loginService", "etc/login.yaml", "the config file path")

const nodeType = login_proto.ENodeType_LoginNodeService

func main() {
	flag.Parse()

	// Load config file
	conf.MustLoad(*configFile, &config.AppConfig)

	// Derive zone-specific Kafka topic from ZoneId
	config.AppConfig.Kafka.Topic = config.DbTaskTopicForGeneration(
		config.AppConfig.Node.ZoneId, config.AppConfig.Kafka.TopicGeneration)

	// Ensure db_task topic exists with configured retention.
	// Ephemeral topics (gate-*, scene-*) use broker default (short retention).
	if err := kafkautil.EnsureTopics(config.AppConfig.Kafka.Brokers, []kafkautil.TopicSpec{
		{
			Name:        config.AppConfig.Kafka.Topic,
			Partitions:  config.AppConfig.Kafka.PartitionCnt,
			RetentionMs: config.AppConfig.Kafka.RetentionMs,
		},
	}); err != nil {
		panic(fmt.Sprintf("Kafka db-task partition contract rejected: %v", err))
	}

	ctx := svc.NewServiceContext()

	defer ctx.Stop()

	ctx.Start()

	// Start gRPC service
	if err := startGRPCServer(config.AppConfig, ctx); err != nil {
		logx.Errorf("Failed to start GRPC server: %v", err)
	}
}

// startGRPCServer starts the Login gRPC service and registers it to etcd.
func startGRPCServer(cfg config.Config, ctx *svc.ServiceContext) error {

	host, port, err := splitHostPort(config.AppConfig.ListenOn)
	if err != nil {
		logx.Errorf("Failed to parse listen address: %v", err)
		return err
	}

	// In K8s, 0.0.0.0 is not routable from other pods — use POD_IP if available.
	if podIP := os.Getenv("POD_IP"); podIP != "" && (host == "0.0.0.0" || host == "::") {
		host = podIP
	}

	// Register node to etcd
	loginNode := node.NewNode(uint32(nodeType), host, port, config.AppConfig.Node.LeaseTTL)
	if loginNode == nil {
		err = errors.New("failed to create node")
		logx.Errorf("Failed to create node: %v", err)
		return err
	}

	if err := loginNode.KeepAlive(); err != nil {
		logx.Errorf("Failed to keep node alive: %v", err)
		return err
	}

	defer func() {
		if err := loginNode.Close(); err != nil {
			logx.Errorf("Failed to close node: %v", err)
		}
	}()

	logx.Infof("Login node registered: %+v", loginNode.Info.String())

	// PlayerId 的机器位由 shared/snowflakealloc 独立分配,**不再用 NodeInfo.NodeId**。
	//
	// 旧做法的问题:NodeInfo.NodeId 与它的 etcd 租约绑在一起,租约一丢 etcd 就把
	// allocKey 删掉、别的副本随时可以抢走同一个号;而 login 这边既不停发也不重夺
	// (reRegister 是空操作,见下),于是会永久用一个已被释放的机器位继续铸 PlayerId。
	// login 是 replicas=2 部署,两副本拿到同一机器位时 bwmarrin 在新毫秒把 step 归零,
	// 同毫秒的首个号逐位相同 —— 而 player_database.player_id 上没有唯一索引兜底。
	//
	// 与 guild / scene_manager 同一套机制:独立 lease + hostname 亲和 + 失租自 fencing。
	// ⚠️ MaxWorkerID 必须显式钳到 bwmarrin 的 node 位宽(13 bit ⇒ 8191):
	// snowflakealloc 默认上界取 shared/snowflake.NodeMask(131071),超出会让
	// snowflake.NewNode 直接失败。
	etcdCli, err := node.NewEtcdClient() // 复用 login 既有的 etcd 客户端工厂,不另拼一份配置
	if err != nil {
		logx.Errorf("Failed to create etcd client for snowflake worker id: %v", err)
		return err
	}
	defer etcdCli.Close()

	hostname, err := os.Hostname()
	if err != nil {
		logx.Errorf("Failed to get hostname: %v", err)
		return err
	}
	maxWorkerID := uint64(1)<<uint(config.AppConfig.Snowflake.NodeBits) - 1
	sfCtx, sfCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sfHandle, err := snowflakealloc.AllocateWithKeepAlive(sfCtx, etcdCli, "/login", hostname,
		snowflakealloc.Options{LeaseTTL: 60, MaxWorkerID: maxWorkerID})
	sfCancel()
	if err != nil {
		logx.Errorf("Failed to allocate snowflake worker id: %v", err)
		return err
	}
	defer sfHandle.Close()
	logx.Infof("Login snowflake worker id = %d (host=%s, max=%d)", sfHandle.WorkerID, hostname, maxWorkerID)

	ctx.SetNodeId(int64(sfHandle.WorkerID))

	// 失租 = 本进程不再拥有这个机器位,必须立刻停止铸 PlayerId。
	// **不能只靠停服**:go-zero 的 zrpc.RpcServer.Stop() 实测只有一行 logx.Close(),
	// 既不拒新请求也不排空在途。所以正确性由 Fence() 保证(之后 Generate 一律失败,
	// 建角整体失败),可用性由进程退出 + 编排重拉保证。
	go func() {
		<-sfHandle.Lost()
		ctx.SnowFlake.Fence() // ① 先关闸
		logx.Error("Login snowflake worker id lease lost; player id generator fenced, exiting to let the " +
			"orchestrator restart (zrpc Stop() only closes the logger and cannot stop serving)")
		logx.Close() // ② 冲掉日志缓冲
		os.Exit(1)   // ③ 退出;重启后拿新 worker id
	}()

	// Start gRPC server
	if err := startServer(cfg, ctx); err != nil {
		logx.Errorf("Failed to start gRPC server: %v", err)
		return err
	}

	return nil
}

func SessionInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals, exists := md["x-session-detail-bin"]; exists && len(vals) > 0 {
			// Decode Base64
			bin, err := base64.StdEncoding.DecodeString(vals[0])
			if err != nil {
				logx.Error("Base64 decode error:", err)
			} else {
				var detail login_proto.SessionDetails
				if err := proto.Unmarshal(bin, &detail); err != nil {
					logx.Error("Protobuf unmarshal error:", err)
				} else {
					ctx = ctxkeys.WithSessionDetails(ctx, &detail)
				}
			}
		}
	}

	// Execute the actual handler
	resp, err := handler(ctx, req)

	// ---- Attach session detail header to response ----
	if detail, ok := ctxkeys.GetSessionDetails(ctx); ok {
		if bin, err := proto.Marshal(detail); err == nil {
			logx.Infof("Session info: %+v", detail)
			val := base64.StdEncoding.EncodeToString(bin)
			header := metadata.Pairs("x-session-detail-bin", val)
			grpc.SendHeader(ctx, header)
		} else {
			logx.Error("Protobuf marshal error:", err)
		}
	}

	return resp, err
}

// startServer configures and starts the gRPC server.
func startServer(cfg config.Config, ctx *svc.ServiceContext) error {
	server := zrpc.MustNewServer(cfg.RpcServerConf, func(grpcServer *grpc.Server) {
		login_proto_login.RegisterClientPlayerLoginServer(grpcServer, loginserver.NewClientPlayerLoginServer(ctx))
		login_proto_login.RegisterLoginAdminServer(grpcServer, loginadminserver.NewLoginAdminServer(ctx))
		login_proto_login.RegisterLoginPreGateServer(grpcServer, loginpregateserver.NewLoginPreGateServer(ctx))

		if cfg.Mode == service.DevMode || cfg.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})

	server.AddUnaryInterceptors(
		SessionInterceptor,
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),
	)

	defer server.Stop()

	// Start the gRPC server
	fmt.Println("\n=============================================================")
	fmt.Println("  LOGIN SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", cfg.ListenOn)
	fmt.Printf("  Mode:        %s\n", cfg.Mode)
	fmt.Printf("  zone_id:     %d\n", cfg.Node.ZoneId)
	if len(cfg.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", cfg.Etcd.Hosts)
	}
	fmt.Printf("  kafka:       %v\n", cfg.Kafka.Brokers)
	fmt.Printf("  redis:       %s\n", cfg.Node.RedisClient.Host)
	fmt.Println("=============================================================")
	server.Start()

	return nil
}

// splitHostPort splits an address into host and uint32 port.
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
