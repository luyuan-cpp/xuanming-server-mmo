package svc

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"

	"guild/internal/config"
	guildkafka "guild/internal/kafka"
	dspb "proto/data_service"
	"shared/generated/table"
	"shared/idsegment"
	"shared/kafkacmd"
	"shared/kafkautil"
)

type ServiceContext struct {
	Config                   config.Config
	RedisClient              *redis.Client
	PlayerLocatorRedisClient *redis.Client
	DB                       *sql.DB
	KafkaWriter              *kafkago.Writer
	GateCommandBuilder       kafkautil.GateCommandBuilder
	// DataServiceClient 是 data_service 的 gRPC 客户端;目前只给 guild_id 号段用,
	// IdSegment.Enabled=false 时为 nil(不拨号)。
	DataServiceClient dspb.DataServiceClient
	// GuildIDSegment 是 guild_id 号段客户端(shared/idsegment,biz_tag="guild");
	// IdSegment.Enabled=false 时为 nil。发号策略(是否回退 snowflake)在 guild.go 里
	// 用 NewGuildIDMinter 组装,因为 snowflake 节点在那里才申领得到。
	GuildIDSegment *idsegment.Client
	// MergeMarkerRedisClient 指向 data_service 的 mapping Redis,只用来读
	// merge:in_progress:{zone}(合服闸门,见 internal/logic/merge_fence.go)。
	// 没配 MergeMarkerRedis 时为 nil = 闸门不生效。
	MergeMarkerRedisClient *redis.Client
}

func NewServiceContext(c config.Config) *ServiceContext {
	table.LoadTables(c.TableDir, false)

	rdb := redis.NewClient(&redis.Options{
		Addr:     c.RedisClient.Host,
		Password: c.RedisClient.Password,
		DB:       c.RedisClient.DB,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect global Redis: %w", err))
	}

	plRedisConf := c.PlayerLocatorRedis
	if plRedisConf.Host == "" {
		plRedisConf = c.RedisClient
		plRedisConf.DB = 0
	}
	plRdb := redis.NewClient(&redis.Options{
		Addr:     plRedisConf.Host,
		Password: plRedisConf.Password,
		DB:       plRedisConf.DB,
	})
	if err := plRdb.Ping(context.Background()).Err(); err != nil {
		panic(fmt.Errorf("failed to connect player locator Redis: %w", err))
	}

	db, err := sql.Open("mysql", c.MySQL.DataSource)
	if err != nil {
		panic(fmt.Errorf("failed to connect MySQL: %w", err))
	}
	if err := db.Ping(); err != nil {
		panic(fmt.Errorf("failed to ping MySQL: %w", err))
	}

	var w *kafkago.Writer
	if len(c.Kafka.Brokers) > 0 {
		w = &kafkago.Writer{
			Addr: kafkago.TCP(c.Kafka.Brokers...),
			// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
			// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
			// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
			// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer。
			//
			// 其它 topic 仍走 Hash 按 Key(player_id/gate_id)选分区,保证同一实体的
			// 推送有序(项目不变量:kafka key = 业务实体 ID)。LeastBytes 忽略 Key。
			Balancer: &kafkacmd.CommandPartitionBalancer{Fallback: &kafkago.Hash{}},
			// kafka-go 的 RequiredAcks 零值是 RequireNone(fire-and-forget):
			// 写进 socket 即返回 nil,broker 端 leader 切换/落盘前崩溃全部不可见,
			// 于是 gate_push 依赖 WriteMessages 返回值的 fail-closed 语义形同虚设。
			// 与 scene_manager / player_locator 的 servicecontext 对齐,显式 RequireOne。
			RequiredAcks: kafkago.RequireOne,
		}
	}

	sc := &ServiceContext{
		Config:                   c,
		RedisClient:              rdb,
		PlayerLocatorRedisClient: plRdb,
		DB:                       db,
		KafkaWriter:              w,
		GateCommandBuilder:       guildkafka.NewGateCommandBuilder(),
		MergeMarkerRedisClient:   newMergeMarkerRedis(c),
	}
	sc.initGuildIDSegment()
	return sc
}

// newMergeMarkerRedis 建合服闸门用的只读 Redis 客户端;没配 = nil = 闸门不生效。
//
// 与上面两个客户端刻意不同:**Ping 失败不 panic**。那两个是 guild 的命脉(缓存 /
// 在场查询),连不上就没有可用形态;这一个只喂一次 EXISTS,而且运行期读失败已经
// fail-closed(见 logic.checkMergeFence:查不到就拒绝建帮)。为一个可选加固的
// 瞬时不可达而拒启,等于把"合服期多一道锁"换成"平时多一个启动失败点"。
func newMergeMarkerRedis(c config.Config) *redis.Client {
	if !c.MergeMarkerRedis.Enabled() {
		logx.Info("[ServiceContext] MergeMarkerRedis 未配置:合服闸门不生效,CreateGuild 不检查 merge:in_progress:{zone}(合服维护窗口内仍可在源 zone 建帮)")
		return nil
	}
	cli := redis.NewClient(&redis.Options{
		Addr:     c.MergeMarkerRedis.Host,
		Password: c.MergeMarkerRedis.Pass,
		DB:       c.MergeMarkerRedis.DB,
	})
	if err := cli.Ping(context.Background()).Err(); err != nil {
		logx.Errorf("[ServiceContext] 合服闸门 Redis %s DB=%d 暂不可达(客户端保留,会自动重连);在它恢复之前 CreateGuild 会 fail-closed 拒绝: %v",
			c.MergeMarkerRedis.Host, c.MergeMarkerRedis.DB, err)
		return cli
	}
	logx.Infof("[ServiceContext] 合服闸门已启用:%s DB=%d,键 merge:in_progress:{zone}", c.MergeMarkerRedis.Host, c.MergeMarkerRedis.DB)
	return cli
}

func (s *ServiceContext) Stop() {
	if s.GuildIDSegment != nil {
		s.GuildIDSegment.Close()
	}
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
	if s.PlayerLocatorRedisClient != nil {
		if err := s.PlayerLocatorRedisClient.Close(); err != nil {
			logx.Errorf("Failed to close player locator Redis client: %v", err)
		}
	}
	if s.MergeMarkerRedisClient != nil {
		if err := s.MergeMarkerRedisClient.Close(); err != nil {
			logx.Errorf("Failed to close merge marker Redis client: %v", err)
		}
	}
	if s.DB != nil {
		if err := s.DB.Close(); err != nil {
			logx.Errorf("Failed to close MySQL: %v", err)
		}
	}
}
