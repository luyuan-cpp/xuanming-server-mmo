package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"guild/internal/config"
	"guild/internal/data"
	"guild/internal/logic"
	"guild/internal/node"
	"guild/internal/server"
	"guild/internal/svc"
	base "proto/common/base"
	pb "proto/guild"
	"shared/grpcstats"
	"shared/snowflake"
	"shared/snowflakealloc"
)

var configFile = flag.String("f", "etc/guild.yaml", "config file path")

const nodeType = uint32(base.ENodeType_GuildNodeService)

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
	logx.Infof("Guild node registered: id=%d uuid=%s", n.Info.NodeId, n.Info.NodeUuid)

	// 通过 shared/snowflakealloc 独立分配 Snowflake worker id。
	//
	// 注意:**不再用 n.Info.NodeId 当 Snowflake worker id**。
	// 理由:
	//   1. NodeInfo.NodeId 是 C++ 服务发现使用的逻辑节点编号,有可能因为 reRegister
	//      CAS 失败而变化(参考 scene_manager 的 reRegister 设计)。
	//   2. Snowflake worker id 一旦变化,可能在同一毫秒里和上一个 worker id 的 ID 序列冲突
	//      (理论上不会,但毫秒级时钟回退 + worker id 跳变是公认的潜在风险)。
	//   3. 同 hostname 重启复用同 worker id,Snowflake 时间戳单调性更稳。
	//   4. 与 scene_manager 模式一致,降低维护成本。
	//
	// prefix="/guild" 与其他服务的 prefix 不同,worker id 池互相隔离。
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   config.AppConfig.Registry.Etcd.Hosts,
		DialTimeout: config.AppConfig.Registry.Etcd.DialTimeout,
	})
	if err != nil {
		logx.Must(fmt.Errorf("snowflake etcd client: %w", err))
	}
	defer etcdCli.Close()

	host_name, err := os.Hostname()
	if err != nil {
		logx.Must(fmt.Errorf("hostname: %w", err))
	}
	sfCtx, sfCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sfHandle, err := snowflakealloc.AllocateWithKeepAlive(sfCtx, etcdCli, "/guild", host_name, snowflakealloc.Options{LeaseTTL: 60})
	sfCancel()
	if err != nil {
		logx.Must(fmt.Errorf("snowflake worker id alloc: %w", err))
	}
	defer sfHandle.Close()
	logx.Infof("Guild snowflake worker id = %d (host=%s)", sfHandle.WorkerID, host_name)

	sf := snowflake.NewNode(sfHandle.WorkerID)

	// Initialize data repo with singleflight + cache-aside
	repo := data.NewGuildRepo(svcCtx.RedisClient, svcCtx.DB, config.AppConfig.Cache.DefaultTTL)
	onlineResolver := logic.NewOnlineStatusResolver(svcCtx.PlayerLocatorRedisClient)
	guildLogic := logic.NewGuildLogic(repo, sf, onlineResolver)

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterGuildServiceServer(grpcServer, server.NewGuildServer(guildLogic))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	defer s.Stop()

	// Snowflake worker id 的 etcd 租约丢了 = 本进程不再是这个 worker id 的合法持有者,
	// etcd 随时会把它分给别的进程。再用 sf 发一个公会 ID 就是确定性撞号,所以主动停服,
	// 让编排把进程拉起来 —— 重启会拿一个新租约,并被 snowflake 的启动 guard 兜住。
	// s.Stop() 让下面的 s.Start() 返回,defer 链正常收尾。
	go func() {
		<-sfHandle.Lost()
		logx.Error("Guild snowflake worker id lease lost; stopping the server to avoid minting colliding ids")
		s.Stop()
	}()

	logx.Infof("Starting Guild RPC server at %s...", config.AppConfig.ListenOn)
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
