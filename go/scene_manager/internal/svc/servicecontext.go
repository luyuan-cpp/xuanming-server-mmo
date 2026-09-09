package svc

import (
	"context"
	"fmt"
	"os"
	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shared/generated/table"
	"shared/kafkacmd"
	"shared/snowflake"
	"shared/snowflakealloc"

	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// KafkaWriter 把同步发布与关闭收进同一接口，使 Stop 能统一释放 writer，
// 也让生命周期测试无需连接真实 broker。
type KafkaWriter interface {
	WriteMessages(context.Context, ...kafka.Message) error
	Close() error
}

type ServiceContext struct {
	Config      config.Config
	Redis       *redis.Redis
	Kafka       KafkaWriter
	Etcd        *clientv3.Client
	SceneIDGen  *snowflake.Node
	snowflakeHd *snowflakealloc.Handle // 持有 worker id 的 etcd lease,进程退出时 Close
	stopOnce    sync.Once
	// kafkaDeliveryFailures 统计 writer Completion 给出的终态失败。
	kafkaDeliveryFailures atomic.Uint64
}

func NewServiceContext(c config.Config) *ServiceContext {
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}

	table.LoadTables(c.TableDir, c.UseBinary)

	// 通过 shared/snowflakealloc 申领一个独立于 NodeInfo.NodeId 的 Snowflake 槽位。
	// 关键点:
	//   - kind="scene-manager",槽池按 kind / cluster 隔离;LegacyPrefix="/scene_manager"
	//     让 cluster 0 在滚动升级期仍读旧布局的水位 / 活 id(发布一版后可删)。
	//   - 节点名 = hostname_监听端口,而不是裸 hostname:多副本部署后同一台机器
	//     (本地 dev 双实例、k8s 同节点多 pod 用 hostNetwork 等)会跑多个实例,
	//     端口是实例槽位的稳定标识,同槽位优雅重启仍复用同一个槽,本地缓存也各自一份;
	//     换 key 的首次启动会申领新槽(最久未用 + 隔离期),前任高水位地板保证不会撞 ID。
	//   - 这个槽与 noderegistry 里分配的 NodeInfo.NodeId **解耦**,
	//     reRegister 切换 NodeInfo.NodeId 不会影响 Snowflake ID 生成。
	//   - ClusterId 由运维按集群设定(默认 0),worker id = cluster<<12 | slot。
	host, err := os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("snowflake: failed to get hostname: %v", err))
	}
	nodeName := host
	if i := strings.LastIndex(c.ListenOn, ":"); i >= 0 {
		nodeName = host + "_" + c.ListenOn[i+1:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hd, err := snowflakealloc.AllocateWithKeepAlive(ctx, etcdCli, "scene-manager", nodeName, snowflakealloc.Options{
		LeaseTTL:     60,
		ClusterID:    c.ClusterId,
		LegacyPrefix: "/scene_manager",
		CachePath:    snowflakealloc.DefaultCachePath(c.SnowflakeCacheDir, "scene-manager", nodeName),
	})
	if err != nil {
		panic(fmt.Sprintf("snowflake worker id alloc failed: %v", err))
	}
	logx.Infof("[scene_manager] snowflake %s worker_id=%d (node=%s)", hd.LogFields(), hd.WorkerID, nodeName)

	sc := &ServiceContext{
		Config: c,
		Redis:  redis.MustNewRedis(c.Redis.RedisConf),
		Etcd:   etcdCli,
		// Handle.NewNode 会注入前任高水位作地板(见 snowflakealloc),
		// 裸 snowflake.NewNode 只有"不在构造秒发号"的点排除,顶不住时钟偏斜接管。
		SceneIDGen:  hd.NewNode(),
		snowflakeHd: hd,
	}
	kafkaWriteTimeout := time.Duration(c.KafkaWriteTimeoutSeconds) * time.Second
	if kafkaWriteTimeout <= 0 {
		kafkaWriteTimeout = 5 * time.Second
	}
	sc.Kafka = &kafka.Writer{
		Addr: kafka.TCP(c.Kafka.Brokers...),
		// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
		// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
		// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
		// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer,
		// CommandPartitionBalancer 就是把它接回来的那一环。
		//
		// 其它 topic 仍走 Hash:Key 都是 player_id(项目不变量:kafka key = 业务实体 ID,
		// 同一玩家的事件必须有序),LeastBytes 按字节量选分区、完全忽略 Key ——
		// 玩家连续两次切场景的两条 RoutePlayer 会落到不同分区被乱序消费,最终应用旧路由。
		//
		// 命令 topic 上保序同样成立、而且更强:同一个 gate 的全部命令都在同一个分区里。
		Balancer:               &kafkacmd.CommandPartitionBalancer{Fallback: &kafka.Hash{}},
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
		// WriteMessages 的调用 context 不可用超时取消：kafka-go 明确允许
		// context 返回后批次继续投递，那会让 EnterScene 回滚后 Gate 又收到
		// 旧路由。改由 writer 自身的一次有界 produce 决定终态。
		MaxAttempts:  1,
		WriteTimeout: kafkaWriteTimeout,
		// kafka.Writer 结构体的 RequiredAcks 零值是 RequireNone(协议级
		// fire-and-forget),显式提到 RequireOne:路由命令丢了玩家就进不去
		// 场景,只能靠客户端重试兜底。
		RequiredAcks: kafka.RequireOne,
		// 路由与重定向属于控制面提交点：只有 broker ACK 后 EnterScene 才能
		// 返回并缓存成功。Async=true 会让 WriteMessages 恒返回 nil，导致
		// broker 故障也被去重缓存成成功，故必须同步等待 RequireOne ACK。
		Async:      false,
		Completion: sc.handleKafkaCompletion,
	}
	return sc
}

func (sc *ServiceContext) handleKafkaCompletion(messages []kafka.Message, err error) {
	if err == nil {
		metrics.ObserveKafkaDelivery("acked", len(messages))
		return
	}

	sc.kafkaDeliveryFailures.Add(uint64(len(messages)))
	metrics.ObserveKafkaDelivery("failed", len(messages))
	for _, m := range messages {
		// 同步 writer 会把该错误返给调用方；Completion 额外提供低基数指标
		// 与逐消息日志，但不在这里递归重试，避免 broker 故障形成热循环。
		logx.Errorf("[scene_manager] kafka delivery failed before EnterScene commit: topic=%s key=%s err=%v",
			m.Topic, string(m.Key), err)
	}
}

// KafkaDeliveryFailures 返回进程生命周期内的终态投递失败数。
// 这里只保留低基数计数；player id 进日志，不进入指标 label。
func (sc *ServiceContext) KafkaDeliveryFailures() uint64 {
	return sc.kafkaDeliveryFailures.Load()
}

// SnowflakeLost 在本进程不再持有 Snowflake worker id 的 etcd 租约时关闭。
// 此时 etcd 可以把同一个 worker id 分给别的进程,继续用 SceneIDGen 发号就是确定性撞号,
// 主流程必须停止服务。见 snowflakealloc.Handle.Lost 的说明。
func (sc *ServiceContext) SnowflakeLost() <-chan struct{} {
	if sc.snowflakeHd == nil {
		return nil
	}
	return sc.snowflakeHd.Lost()
}

// Stop 先关闭 Kafka writer并等待 Completion 返回；之后才释放 Snowflake
// worker lease。幂等保证
// 正常 defer 与失租强退路径可以安全共用。
func (sc *ServiceContext) Stop() {
	sc.stopOnce.Do(func() {
		if sc.Kafka != nil {
			if err := sc.Kafka.Close(); err != nil {
				logx.Errorf("[scene_manager] Kafka writer close/flush failed: %v", err)
			}
		}
		if failures := sc.KafkaDeliveryFailures(); failures > 0 {
			logx.Errorf("[scene_manager] stopping with %d terminal Kafka delivery failures", failures)
		}
		if sc.snowflakeHd != nil {
			sc.snowflakeHd.Close()
			sc.snowflakeHd = nil
		}
	})
}
