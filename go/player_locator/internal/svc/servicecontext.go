package svc

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	"player_locator/internal/config"
	smpb "proto/scene_manager"
	"shared/generated/table"
	"shared/kafkacmd"
	"shared/playerredis"
)

// leaseCleanupWakeBuffer 是 LeaseCleanupWake 的容量:满了就丢,常规轮转兜底。
const leaseCleanupWakeBuffer = 1024

type ServiceContext struct {
	Config config.Config
	// RedisClient 是控制域:选主锁、就绪标记、代次、审计时间。
	RedisClient *redis.Client
	// PlayerRedis 是玩家域:会话、断线租约、gate 索引、分片登记、AFK 通行、位置(只读)。
	PlayerRedis        redis.UniversalClient
	KafkaWriter        *kafkago.Writer
	SceneManagerClient smpb.SceneManagerClient
	// LeaseCleanupWake 叫醒本进程的 LeaseMonitor 立刻处理一个 slot(见 wakeLeaseMonitorAt)。nil = 不叫醒,只靠常规轮转。
	LeaseCleanupWake chan uint16
}

func NewServiceContext(c config.Config) *ServiceContext {
	table.LoadTables(c.TableDir, false)

	if c.RedisClient.PoolSize < 0 {
		panic(fmt.Errorf("RedisClient.PoolSize=%d must be >= 0 (0 = go-redis default 10 x GOMAXPROCS)", c.RedisClient.PoolSize))
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     c.RedisClient.Host,
		Password: c.RedisClient.Password,
		DB:       c.RedisClient.DB,
		// 共享 Redis 连接总账按它记账(0 = go-redis 缺省 10 × GOMAXPROCS),见 config.RedisConf.PoolSize。
		PoolSize: c.RedisClient.PoolSize,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect Redis: %w", err))
	}
	// 生效值(缺省时是 go-redis 算出来的 10 × GOMAXPROCS):核对 ConfigMap 镜像的 PoolSize 是否真的被加载。
	logx.Infof("Redis client %s DB=%d pool_size=%d", c.RedisClient.Host, c.RedisClient.DB, rdb.Options().PoolSize)

	playerConf, err := playerRedisConf(c)
	if err != nil {
		panic(err)
	}
	playerRedis, err := playerredis.New(playerConf)
	if err != nil {
		panic(fmt.Errorf("failed to build player redis client: %w", err))
	}
	if err := playerRedis.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect player redis: %w", err))
	}
	logx.Infof("Player redis client %s", playerConf.Describe())

	w := &kafkago.Writer{
		Addr: kafkago.TCP(c.Kafka.Brokers...),
		// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
		// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
		// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
		// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer。
		//
		// 其它 topic 仍走 Hash 按 Key(player_id)选分区,保证同一玩家的事件有序
		// (项目不变量:kafka key = 业务实体 ID)。LeastBytes 忽略 Key。
		Balancer: &kafkacmd.CommandPartitionBalancer{Fallback: &kafkago.Hash{}},
		// 必须显式设置:kafka-go 直接构造 Writer 时 RequiredAcks 零值是
		// RequireNone(fire-and-forget,写进 socket 即返回 nil,broker 端
		// leader 切换/落盘前崩溃全都不可见)。而 LeaseMonitor 的受理协议
		// 恰恰以 WriteMessages 返回 nil 作为"gate 副作用已完成"的凭据去
		// ack claim —— acks=0 时这个凭据是假的,清理命令会静默丢失且无重试。
		// 兄弟服务 scene_manager 的 servicecontext 已修过同一个坑(RequireOne),
		// 这里对齐。
		RequiredAcks: kafkago.RequireOne,
	}

	// SceneManager zrpc client (via etcd discovery).
	var smClient smpb.SceneManagerClient
	if len(c.SceneManagerRpc.Etcd.Hosts) > 0 {
		cli := zrpc.MustNewClient(c.SceneManagerRpc)
		smClient = smpb.NewSceneManagerClient(cli.Conn())
	}

	return &ServiceContext{
		Config:             c,
		RedisClient:        rdb,
		PlayerRedis:        playerRedis,
		KafkaWriter:        w,
		SceneManagerClient: smClient,
		LeaseCleanupWake:   make(chan uint16, leaseCleanupWakeBuffer),
	}
}

// playerRedisConf 是玩家域客户端的配置:没写 PlayerRedis 段时指向 RedisClient 这个单实例,沿用它的连接池上限与
// go-redis 缺省超时,开发环境和还没渲染这一段的 ConfigMap 不用改就能跑。玩家域只用 DB 0,所以 RedisClient.DB 不是 0
// 时不能这样缺省:会话会悄悄换一个 DB,读会话的别的服务看不到。
func playerRedisConf(c config.Config) (playerredis.Config, error) {
	if err := c.PlayerRedis.ValidateSection(); err != nil {
		return playerredis.Config{}, fmt.Errorf("PlayerRedis: %w", err)
	}
	if len(c.PlayerRedis.Addrs) == 0 && c.RedisClient.DB != 0 {
		return playerredis.Config{}, fmt.Errorf("PlayerRedis is not set and RedisClient.DB=%d: the player domain only uses DB 0, set PlayerRedis.Addrs explicitly", c.RedisClient.DB)
	}
	return c.PlayerRedis.OrDefault(playerredis.Config{
		Addrs:           []string{c.RedisClient.Host},
		Password:        c.RedisClient.Password,
		PoolSizePerNode: min(c.RedisClient.PoolSize, playerredis.MaxPoolSizePerNode),
		DialTimeoutMs:   5000,
		ReadTimeoutMs:   3000,
		WriteTimeoutMs:  3000,
		PoolTimeoutMs:   4000,
	}), nil
}

func (s *ServiceContext) Stop() {
	if s.KafkaWriter != nil {
		if err := s.KafkaWriter.Close(); err != nil {
			logx.Errorf("Failed to close Kafka writer: %v", err)
		}
	}
	if s.RedisClient != nil {
		if err := s.RedisClient.Close(); err != nil {
			logx.Errorf("Failed to close Redis client: %v", err)
		}
	}
	if s.PlayerRedis != nil {
		if err := s.PlayerRedis.Close(); err != nil {
			logx.Errorf("Failed to close player redis client: %v", err)
		}
	}
}
