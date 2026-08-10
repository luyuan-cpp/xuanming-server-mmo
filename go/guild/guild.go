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
	"guild/internal/constants"
	"guild/internal/data"
	"guild/internal/logic"
	"guild/internal/node"
	"guild/internal/server"
	"guild/internal/svc"
	base "proto/common/base"
	pb "proto/guild"
	"shared/grpcstats"
	"shared/safego"
	"shared/serverbase"
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

	// 用 Handle.NewNode 而不是裸 snowflake.NewNode:它会把**前任在这个 worker id 上的
	// 高水位**当地板注入(etcd 里的持久水位),顶住跨机时钟偏斜接管、本机时钟回拨、
	// 前任借过逻辑秒这三类"启动 guard 挡不住"的重号。
	sf := sfHandle.NewNode()

	// Initialize data repo with singleflight + cache-aside
	repo := data.NewGuildRepo(svcCtx.RedisClient, svcCtx.DB, config.AppConfig.Cache.DefaultTTL)

	// 先执行一次性的 Redis -> MySQL 存量分数回填，再从 MySQL 权威快照完整重建
	// 全局/分区榜。迁移或重建失败必须阻止服务启动，否则新写会把尚未回填的
	// 历史分数覆盖掉，或继续向不完整榜单提供结果。
	if err := repo.MigrateLegacyRankScores(context.Background()); err != nil {
		panic(fmt.Errorf("migrate legacy guild rank scores: %w", err))
	}
	if err := repo.RebuildRanks(context.Background()); err != nil {
		panic(fmt.Errorf("rebuild guild ranks from MySQL: %w", err))
	}
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
	// in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把失败塞进
	// 响应体的 TipInfoMessage —— gRPC status 恒 OK,go-zero 自带的指标拦截器会把
	// 每一次「发号器被 fence」都记成一次成功请求。这层把响应体里的码读出来定性,
	// 故障打日志 + 计数,业务拒绝只计数。它不改响应内容、不吞错,对客户端无感。
	// TipClassifier 必须传:公会码在 200-219 私有段,serverbase 的全局判定
	// (只认已生成的 0-129 数轴)会把它们全判成 unknown_code。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
	}))
	defer s.Stop()

	// Snowflake worker id 的 etcd 租约丢了 = 本进程不再是这个 worker id 的合法持有者,
	// etcd 随时会把它分给别的进程。再用 sf 发一个公会 ID 就是确定性撞号,所以主动停服,
	// 让编排把进程拉起来 —— 重启会拿一个新租约,并被 snowflake 的启动 guard 兜住。
	// s.Stop() 让下面的 s.Start() 返回,defer 链正常收尾。
	//
	// ⏱ 时间预算(§租约与重启时间预算必须闭合):Lost() 由 snowflakealloc 的**自 fencing**
	// 提前触发 —— 距上次成功续租超过 TTL 的 2/3(TTL=60s ⇒ 40s)就报信,而不是干等
	// KeepAlive channel 关闭(那恒晚于服务端过期点)。收到信号时服务端 lease 通常还有
	// 约 TTL/3(≈20s)才过期,这段余量用来让在途请求干净失败。
	//
	// ⚠️ 顺序不能反,而且**不能只调 s.Stop()**:go-zero 的 zrpc.RpcServer.Stop() 实测
	// (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),既不拒新请求也不排空在途 ——
	// 靠它"停服"等于什么都没做,进程会带着已失效的 worker id 一直服务下去。
	// 所以正确性由 Fence() 保证(之后 Generate 一律 ErrFenced,建帮整体失败),
	// 可用性由进程退出 + 编排重拉保证。
	//
	// 用 safego.Go 而不是裸 `go func`:这条看门狗一旦 panic(比如 Lost() 通道被
	// 重复关闭),裸 goroutine 会把整个进程当场打死,日志里只剩一段 runtime 栈;
	// safego 兜住后会打稳定事件名 + 计 safego_panic_total{point="guild.snowflake_fence_watch"},
	// 看门狗失效这件事变成可告警的,而不是伪装成一次"正常"的进程退出。
	safego.Go("guild.snowflake_fence_watch", func() {
		<-sfHandle.Lost()
		sf.Fence() // ① 先关闸:此后一个号都发不出去,撞号从机制上不可能
		logx.Error("Guild snowflake worker id lease lost; generator fenced, exiting to let the orchestrator restart " +
			"(zrpc Stop() only closes the logger and cannot stop serving)")
		logx.Close() // ② 冲掉日志缓冲,别把上面这条 ERROR 丢了
		os.Exit(1)   // ③ 退出;重启后拿新租约,并被 snowflake 的启动 guard 兜住
	})

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
