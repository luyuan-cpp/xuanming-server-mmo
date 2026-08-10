package main

import (
	"db/internal/config"
	"db/internal/kafka"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	server "db/internal/server/db"
	"db/internal/svc"
	"flag"
	"fmt"
	"os"
	"os/signal"
	db_grpc "proto/db"
	"shared/grpcstats"
	"shared/kafkautil"
	"syscall"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/db.yaml", "the config file")

func main() {
	flag.Parse()

	// Load config
	conf.MustLoad(*configFile, &config.AppConfig)
	// 回填 optional 段的默认值(迁移超时、大字段闸上限)。见 config.Normalize 注释。
	config.AppConfig.Normalize()

	// Derive zone-specific Kafka topic and MySQL database name from ZoneId
	if config.AppConfig.ZoneId == 0 {
		panic("ZoneId must be set in config (> 0)")
	}
	config.AppConfig.ServerConfig.Kafka.Topic = config.DbTaskTopicForGeneration(
		config.AppConfig.ZoneId, config.AppConfig.ServerConfig.Kafka.TopicGeneration)
	config.AppConfig.ServerConfig.Database.DBName = config.ZoneDBName(config.AppConfig.ZoneId)

	// Ensure the db_task topic exists with the desired partition count BEFORE
	// the sarama consumer joins. If the topic doesn't exist when sarama
	// connects, the broker auto-creates it with num.partitions=1 and sarama
	// permanently assigns this consumer partition 0 only — any later
	// partition expansion strands the other partitions with no consumer until
	// the next process restart. Round 10 of the 45k stress (2026-05-31) caught this with
	// ~19.5k preload tasks stuck on partitions 1..9 while only ~764 on
	// partition 0 drained, capping SceneManager.EnterScene throughput at
	// ~4/s and timing out 78% of robots on "scene ready".
	if err := kafkautil.EnsureTopics(
		config.AppConfig.ServerConfig.Kafka.Brokers,
		[]kafkautil.TopicSpec{{
			Name:        config.AppConfig.ServerConfig.Kafka.Topic,
			Partitions:  config.AppConfig.ServerConfig.Kafka.PartitionCnt,
			RetentionMs: config.AppConfig.ServerConfig.Kafka.RetentionMs,
		}},
	); err != nil {
		panic(fmt.Sprintf("Kafka db-task partition contract rejected: %v", err))
	}

	ctx := svc.NewServiceContext()

	// Initialize database BEFORE the Kafka consumer starts pulling tasks:
	// 消费任务落到 worker 就会碰 proto_sql.DB,若此时还是 nil,启动窗口内的
	// 每条消息都 panic-recover 一次(offset 不提交,重启可恢复,但整个窗口在
	// 空转打错误日志)。先建库连接再放消费者进来,窗口从机制上不存在。
	//
	// InitDB 默认**不跑任何 DDL**,并且会断言实际连上的 DATABASE() 落在外部
	// 注入的白名单里 —— 不在就拒启,而不是静默建库把玩家数据写进去。
	// 建表/补列由部署阶段的 `go run ./cmd/migrate -command up` 负责,
	// 运维步骤见 go/db/README.md。
	if err := proto_sql.InitDB(); err != nil {
		panic(fmt.Sprintf("database init rejected: %v", err))
	}

	// 装配大字段三档闸(必须在消费者启动之前:第一条消息落到 worker 就要过闸)
	kafka.InitBlobGuard(config.AppConfig.ServerConfig.BlobGuard)

	// Initialize Kafka consumer
	kafkaConsumer, err := kafka.NewKeyOrderedKafkaConsumer(
		config.AppConfig,
		ctx.RedisClient,
	)
	if err != nil {
		panic(fmt.Sprintf("failed to init Kafka consumer: %v", err))
	}
	defer kafkaConsumer.Stop()

	// Start Kafka consumer
	if err := kafkaConsumer.Start(); err != nil {
		panic(fmt.Sprintf("failed to start Kafka consumer: %v", err))
	}

	// Start Prometheus /metrics endpoint (no-op when MetricsListenAddr empty).
	metrics.Start(config.AppConfig.MetricsListenAddr)

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		db_grpc.RegisterDbServer(grpcServer, server.NewDbServer(ctx))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	defer s.Stop()

	// Wait for shutdown signal
	fmt.Println("\n=============================================================")
	fmt.Println("  DB SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  ZoneId:      %d\n", config.AppConfig.ZoneId)
	fmt.Printf("  Listen:      %s\n", config.AppConfig.ListenOn)
	fmt.Printf("  Mode:        %s\n", config.AppConfig.Mode)
	if len(config.AppConfig.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", config.AppConfig.Etcd.Hosts)
	}
	fmt.Printf("  redis:       %s\n", config.AppConfig.ServerConfig.RedisClient.Hosts)
	fmt.Printf("  kafka:       %v\n", config.AppConfig.ServerConfig.Kafka.Brokers)
	fmt.Printf("  kafka topic: %s\n", config.AppConfig.ServerConfig.Kafka.Topic)
	fmt.Printf("  mysql:       %s@%s/%s\n", config.AppConfig.ServerConfig.Database.User, config.AppConfig.ServerConfig.Database.Hosts, config.AppConfig.ServerConfig.Database.DBName)
	fmt.Println("=============================================================")
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("Shutting down gracefully...")
}
