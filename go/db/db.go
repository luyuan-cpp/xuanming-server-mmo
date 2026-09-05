package main

import (
	"context"
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
	"shared/killswitch"
	"shared/serverbase"
	"syscall"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/db.yaml", "the config file")

// killSwitchEtcdDialTimeout 与 scene_manager 建 etcd 客户端时的取值一致(5s)。
// 只影响 killswitch 自己的 watch 连接,拨不通也只是退回全放行。
const killSwitchEtcdDialTimeout = 5 * time.Second

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

	// ── 热关停(killswitch)────────────────────────────────────────────
	// db 是玩家权威数据的写入方,真出事时最缺的是"秒级止血阀":往 etcd 前缀
	// /mmorpg/killswitch/ 下写一个 key 就能把某个方法立刻短路掉,而不必走一遍
	// 构建-发布-滚动更新。规则布局与优先级见 shared/killswitch 包注释。
	//
	// New 出来立刻可用(规则为空 = 全放行),Start 非阻塞、etcd 为 nil 也合法,
	// 所以整段接线不会让 db 的启动多出任何一个失败点。
	ks := killswitch.New(killswitch.Config{})
	ksCtx, ksCancel := context.WithCancel(context.Background())
	defer ksCancel()
	etcdCli := newKillSwitchEtcdClient(config.AppConfig.Etcd)
	if etcdCli != nil {
		defer etcdCli.Close()
	}
	ks.Start(ksCtx, etcdCli)

	// Start gRPC server
	s := zrpc.MustNewServer(config.AppConfig.RpcServerConf, func(grpcServer *grpc.Server) {
		db_grpc.RegisterDbServer(grpcServer, server.NewDbServer(ctx))
		if config.AppConfig.Mode == service.DevMode || config.AppConfig.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 拦截器顺序是有意的(先加的在外层):
	//   grpcstats → killswitch → serverbase → handler
	// grpcstats 放最外层,被关停的请求也照样计入总量(否则一开闸就像"没人调用");
	// killswitch 命中即短路,handler 根本不会被调用;
	// serverbase 放最内层,只观测真正跑过 handler 的结果,不会把"被人为关停"
	// 误记成一次业务故障。
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	s.AddUnaryInterceptors(ks.UnaryServerInterceptor())
	// in-band 故障拦截器。
	//
	// 这里**刻意不传任何 Classifier**,依据是 proto 事实而不是照抄别的服务:
	// db 的 gRPC 面只有 db.Test 一个方法(见 proto/db/db.proto),它的
	// TestResponse 里既没有 `uint32 error_code`,也没有 `TipInfoMessage
	// error_message` —— 压根没有 in-band 业务码字段,serverbase 会判成
	// SourceNone 一律记成成功。传 Classifier 只会是自欺欺人的装饰。
	//
	// 那为什么还挂?两条真实收益:
	//   1. handler 返回 error 时记一条 transport_error + 耗时,与其他服务同口径;
	//   2. 以后 db 真加出带业务码的 RPC 时,这层已经在链上,不会再漏一遍
	//      "gRPC status 恒 OK、故障全部静默"的老坑。
	// db 真正的业务失败走的是 Kafka db_task 那条链,由 internal/metrics 的
	// db_task_result_total 统计,不在本拦截器的观测范围内。
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{}))
	defer s.Stop()

	// 真正把 gRPC 服务跑起来。fc9377336(asynq→kafka)之后这里只剩 MustNewServer
	// 没有 Start:进程打印 "STARTED SUCCESSFULLY" 却什么端口都不监听,也不向 etcd
	// 注册 db.rpc。本地 compose 没探针所以一直没暴露;K8s(deploy/k8s/manifests/go-svc/db.yaml)
	// 的 grpc readiness/liveness 探 6000 端口永远超时,db Pod 永远 0/1 并被反复重启
	// (2026-09-03 kind 实跑)。go-zero 的 Start() 是阻塞的(内部先注册 etcd 再 Serve,
	// 出错走 logx.Must 直接退出),放到 goroutine 里,主 goroutine 仍按下面的信号退出。
	go s.Start()

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

// newKillSwitchEtcdClient 按 RpcServerConf.Etcd 建一个只给热关停 watch 用的
// etcd 客户端。**返回 nil 是合法结果**,调用方直接把 nil 传给
// killswitch.Start 即可(它会打一条 Info 后全部放行)。
//
// 为什么不复用 go-zero 服务注册内部那个客户端:go-zero 没把它暴露出来。
//
// 为什么任何失败都只记日志不 panic:fail-open 是 killswitch 的铁律 ——
// 管控组件自身故障绝不能拖垮业务。db 是玩家权威数据的写入方,
// 因为一个"止血阀连不上 etcd"就拒启,等于用小故障换一次全服停写。
//
// 注意 clientv3.New 不带 WithBlock,不会在这里真的去拨号,因此也不会拖慢启动;
// 连不通的后果只是 killswitch 的全量同步失败并按 ResyncBackoff 重试(期间放行)。
func newKillSwitchEtcdClient(cfg discov.EtcdConf) *clientv3.Client {
	if len(cfg.Hosts) == 0 {
		logx.Info("[killswitch] db 未配置 etcd Hosts,热关停不生效(全部放行)")
		return nil
	}

	c := clientv3.Config{
		Endpoints:   cfg.Hosts,
		DialTimeout: killSwitchEtcdDialTimeout,
	}
	if cfg.HasAccount() {
		c.Username = cfg.User
		c.Password = cfg.Pass
	}

	cli, err := clientv3.New(c)
	if err != nil {
		logx.Errorf("[killswitch] db 建 etcd 客户端失败,热关停不生效(全部放行): %v", err)
		return nil
	}
	return cli
}
