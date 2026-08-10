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
	"shared/generated/table"
	"shared/kafkautil"
)

type ServiceContext struct {
	Config                   config.Config
	RedisClient              *redis.Client
	PlayerLocatorRedisClient *redis.Client
	DB                       *sql.DB
	KafkaWriter              *kafkago.Writer
	GateCommandBuilder       kafkautil.GateCommandBuilder
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
			// Hash 按 Key(player_id/gate_id)选分区,保证同一实体的推送有序
			// (项目不变量:kafka key = 业务实体 ID)。LeastBytes 忽略 Key。
			Balancer: &kafkago.Hash{},
			// kafka-go 的 RequiredAcks 零值是 RequireNone(fire-and-forget):
			// 写进 socket 即返回 nil,broker 端 leader 切换/落盘前崩溃全部不可见,
			// 于是 gate_push 依赖 WriteMessages 返回值的 fail-closed 语义形同虚设。
			// 与 scene_manager / player_locator 的 servicecontext 对齐,显式 RequireOne。
			RequiredAcks: kafkago.RequireOne,
		}
	}

	return &ServiceContext{
		Config:                   c,
		RedisClient:              rdb,
		PlayerLocatorRedisClient: plRdb,
		DB:                       db,
		KafkaWriter:              w,
		GateCommandBuilder:       guildkafka.NewGateCommandBuilder(),
	}
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
	if s.PlayerLocatorRedisClient != nil {
		if err := s.PlayerLocatorRedisClient.Close(); err != nil {
			logx.Errorf("Failed to close player locator Redis client: %v", err)
		}
	}
	if s.DB != nil {
		if err := s.DB.Close(); err != nil {
			logx.Errorf("Failed to close MySQL: %v", err)
		}
	}
}
