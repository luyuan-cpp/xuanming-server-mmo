package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"match/internal/config"
	"match/internal/metrics"
	"match/internal/noderegistry"
	"match/internal/pkg/ctxkeys"
	"match/internal/server"
	"match/internal/svc"

	matchkafka "match/internal/kafka"
	matchlogic "match/internal/logic"

	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"shared/grpcstats"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"
)

var configFile = flag.String("f", "etc/match_service.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
	svcCtx := svc.NewServiceContext(c)
	defer svcCtx.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Prometheus /metrics(MetricsListenAddr 为空则不开)。
	metrics.Start(c.MetricsListenAddr)

	// scene / battle 节点发现:etcd list-watch 内存镜像
	// (gather 定位 PrepareBattle / CreateBattle 对端用,照 LoadReporter 模式)。
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

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		matchpb.RegisterMatchServiceServer(grpcServer, server.NewMatchServiceServer(svcCtx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(
		sessionInterceptor,
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),
	)
	defer s.Stop()

	// C++ NodeInfo 约定注册进 etcd,gate 按 MatchNodeService.rpc 前缀发现本服务。
	host, port := parseListenOn(c.ListenOn)
	nr, err := noderegistry.Register(
		svcCtx.Etcd,
		uint32(base.ENodeType_MatchNodeService),
		c.ZoneId,
		host, port,
		c.LeaseTTL,
	)
	if err != nil {
		panic("failed to register Match node in etcd: " + err.Error())
	}
	nr.KeepAlive()
	defer nr.Close()

	fmt.Println("\n=============================================================")
	fmt.Println("  MATCH SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Mode:        %s\n", c.Mode)
	if len(c.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	}
	fmt.Printf("  node_id:     %d (etcd CAS)\n", nr.Info.NodeId)
	fmt.Printf("  node_uuid:   %s\n", nr.Info.NodeUuid)
	fmt.Printf("  kafka:       %v\n", c.Kafka.Brokers)
	fmt.Println("=============================================================")

	// Snowflake worker id 失租 = 本进程不再是该 worker id 的合法持有者,
	// 继续用 BattleIDGen 发号就是确定性撞号(battle_id/challenge_id 都会撞)。
	// 与 scene_manager 同口径:先 fence 发号器,flush Kafka,再退出重启换 id。
	// os.Exit 不执行 defer,所以 flush 必须在强退分支里直接调用。
	if lost := svcCtx.SnowflakeLost(); lost != nil {
		go func() {
			<-lost
			svcCtx.BattleIDGen.Fence() // 先关闸:此后 Generate 一律 ErrFenced,gather 整体失败
			logx.Error("[match] snowflake worker id lease lost; generator fenced, flushing Kafka before exit")
			svcCtx.Stop() // flush Kafka 在途批次,再释放 worker lease
			logx.Close()  // 冲掉日志缓冲
			os.Exit(1)    // 退出;重启后拿新 worker id
		}()
	}

	s.Start()
}

// sessionInterceptor 解出 gate 附带的 x-session-detail-bin(base64 protobuf
// SessionDetails)放进 ctx —— 客户端直达协议的权威 player_id 从这里取,
// 请求体里的 player_id 仅供内部调用(照 login 的 SessionInterceptor 模式;
// match 不修改会话,无需回写响应 header)。
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
