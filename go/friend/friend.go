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

	"friend/internal/config"
	"friend/internal/constants"
	"friend/internal/data"
	"friend/internal/logic"
	"friend/internal/node"
	"friend/internal/server"
	"friend/internal/svc"
	base "proto/common/base"
	pb "proto/friend"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/serverbase"
)

var configFile = flag.String("f", "etc/friend.yaml", "config file path")

const nodeType = uint32(base.ENodeType_FriendNodeService)

func main() {
	flag.Parse()
	conf.MustLoad(*configFile, &config.AppConfig)

	svcCtx := svc.NewServiceContext(config.AppConfig)
	defer svcCtx.Stop()

	// friend_capacity 的 DDL 与历史回填不是一个原子动作。只有 durable ready
	// 标记与回填事务一起提交后才能对外注册服务，避免半迁移库把存量容量当 0。
	repo := data.NewFriendRepo(svcCtx.RedisClient, svcCtx.DB, config.AppConfig.Cache.DefaultTTL)
	if err := repo.RequireFriendCapacityReady(context.Background()); err != nil {
		logx.Must(fmt.Errorf("friend capacity migration gate: %w", err))
	}

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
	logx.Infof("Friend node registered: id=%d uuid=%s", n.Info.NodeId, n.Info.NodeUuid)

	// RPC 级热关停(shared/killswitch):线上某个方法把 DB 打爆时,往 etcd 写一个
	// key 就能秒级把它短路掉,不必走一遍构建-发布-滚动更新。
	//
	// 复用 node 的 etcd 客户端,不另开第二条连接 —— 连的是同一个集群。
	// Start 非阻塞:客户端为 nil、etcd 连不上、前缀下没有 key,一律放行(fail-open),
	// 因此这里既不需要判错也不需要 logx.Must。
	ksCtx, ksCancel := context.WithCancel(context.Background())
	// ⚠️ 这个 defer 必须写在 `defer n.Close()` 之后:defer 是后进先出,先取消
	// watch 循环,再关 etcd 客户端,否则 watch 会撞上一个已经 Close 的 client。
	defer ksCancel()
	ks := killswitch.New(killswitch.Config{Prefix: config.AppConfig.KillSwitchPrefix})
	ks.Start(ksCtx, n.EtcdClient())

	// Initialize logic after the durable schema/data gate has passed.
	friendLogic := logic.NewFriendLogic(repo)

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterFriendServiceServer(grpcServer, server.NewFriendServer(friendLogic))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(buildUnaryInterceptors(ks)...)
	defer s.Stop()

	logx.Infof("Starting Friend RPC server at %s...", config.AppConfig.ListenOn)
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
// 见 friend_test.go。
func buildUnaryInterceptors(ks *killswitch.Switch) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		// ① 流量统计放最外层:被热关停短路掉的请求也必须被统计到。
		//    否则"关停生效后这个方法的 QPS 归零"会被误读成客户端不再调用了。
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),

		// ② 热关停紧跟其后,尽早短路:命中之后 in-band 定性、handler、
		//    以及 handler 里的 Redis / MySQL 访问统统不做 —— 止血阀的全部意义
		//    就是"别为一个已经关停的方法做任何无谓的工作"。
		//    它放在 serverbase 之前也意味着被关停的调用不进 rpc_duration_seconds:
		//    那条指标衡量的是业务链路,而关停请求根本没走业务链路,
		//    它们的账记在 killswitch_blocked_total{method} 上。
		ks.UnaryServerInterceptor(),

		// ③ in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把业务失败塞进
		//    响应体的 TipInfoMessage —— gRPC status 恒 OK,go-zero 自带的指标拦截器把每一次
		//    业务拒绝都记成一次成功请求,监控上看不出任何比例变化。这层把响应体里的码读出来
		//    定性后单独计数;真正的依赖故障(Redis / MySQL)在 friend_logic 里是
		//    `return nil, err`,由同一个拦截器记成 transport_error。
		//    好友域没有 in-band 故障码,TipClassifier 直接复用由 Tip.xlsx 生成的
		//    全局段表判定；保留显式注入以固定本服务的定性接缝。
		serverbase.UnaryInterceptor(serverbase.Options{
			TipClassifier: constants.TipClassifier(),
		}),
	}
}

func splitHostPort(address string) (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	p, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return "", 0, err
	}
	return host, uint32(p), nil
}
