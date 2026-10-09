package svc

import (
	"context"

	"db/internal/config"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

type ServiceContext struct {
	Config      config.Config
	RedisClient *redis.Client
	// PlacementRedis 读落点记录与 home_zone、写能力标记(player-storage-placement.md §6.2 / §4.3)。
	// 它必须指向 data_service 的 mapping Redis(player:zone 所在实例,DB 0);Placement.Redis 缺省时
	// 就是 RedisClient 本身(dev / K8s 两者同实例同 DB)。
	PlacementRedis *redis.Client
}

func NewServiceContext() *ServiceContext {
	redisCfg := config.AppConfig.ServerConfig.RedisClient

	redisClient := redis.NewClient(&redis.Options{
		Addr:             redisCfg.Hosts,
		Password:         redisCfg.Password,
		DB:               redisCfg.DB,
		DisableIndentity: true, // suppress CLIENT SETINFO on Redis < 7.2
	})

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		logx.Errorf("Failed to connect to Redis at %s: %v", redisCfg.Hosts, err)
	}

	placementRedis := redisClient
	if pr := config.AppConfig.Placement.Redis; pr != nil {
		placementRedis = redis.NewClient(&redis.Options{
			Addr:             pr.Hosts,
			Password:         pr.Password,
			DB:               pr.DB,
			DisableIndentity: true, // suppress CLIENT SETINFO on Redis < 7.2
		})
		// 与上面 RedisClient 同口径只记日志不拒启:连不上时每条任务的选库 MGET 都失败,
		// 按 lookup_error 延后重试(fail-closed),不会写错库。
		if err := placementRedis.Ping(context.Background()).Err(); err != nil {
			logx.Errorf("Failed to connect to placement Redis at %s: %v", pr.Hosts, err)
		}
	}

	return &ServiceContext{
		Config:         config.AppConfig,
		RedisClient:    redisClient,
		PlacementRedis: placementRedis,
	}
}
