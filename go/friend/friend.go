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
	"friend/internal/data"
	"friend/internal/logic"
	"friend/internal/node"
	"friend/internal/server"
	"friend/internal/svc"
	base "proto/common/base"
	pb "proto/friend"
	"shared/grpcstats"
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

	// Initialize logic after the durable schema/data gate has passed.
	friendLogic := logic.NewFriendLogic(repo)

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterFriendServiceServer(grpcServer, server.NewFriendServer(friendLogic))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	defer s.Stop()

	logx.Infof("Starting Friend RPC server at %s...", config.AppConfig.ListenOn)
	s.Start()
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
