package svc

import (
	"context"
	"fmt"

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

// validateRedisPoolSize 拒绝负的 PoolSize:负数没有合法含义,go-redis 只把 0 当缺省。
// 与连不上 Redis 只记日志不同,这是配置写错,起服即失败。
func validateRedisPoolSize(section string, poolSize int) {
	if poolSize < 0 {
		panic(fmt.Sprintf("%s.PoolSize=%d must be >= 0 (0 = go-redis default 10 x GOMAXPROCS)", section, poolSize))
	}
}

func NewServiceContext() *ServiceContext {
	if err := config.AppConfig.ValidateGlobalAuthority(); err != nil {
		panic(fmt.Errorf("全球DB配置拒绝启动: %w", err))
	}
	redisCfg := config.AppConfig.ServerConfig.RedisClient
	validateRedisPoolSize("ServerConfig.RedisClient", redisCfg.PoolSize)

	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisCfg.Hosts,
		Password: redisCfg.Password,
		DB:       redisCfg.DB,
		// 共享 Redis 连接总账按它记账(0 = go-redis 缺省 10 × GOMAXPROCS),见 config.RedisConfig.PoolSize。
		PoolSize:         redisCfg.PoolSize,
		DisableIndentity: true, // suppress CLIENT SETINFO on Redis < 7.2
	})

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		logx.Errorf("Failed to connect to Redis at %s: %v", redisCfg.Hosts, err)
	}
	// 生效值(缺省时是 go-redis 算出来的 10 × GOMAXPROCS):核对 ConfigMap 镜像的 PoolSize 是否真的被加载。
	logx.Infof("Redis client %s DB=%d pool_size=%d", redisCfg.Hosts, redisCfg.DB, redisClient.Options().PoolSize)

	placementRedis := redisClient
	if pr := config.AppConfig.Placement.Redis; pr != nil {
		validateRedisPoolSize("Placement.Redis", pr.PoolSize)
		placementRedis = redis.NewClient(&redis.Options{
			Addr:             pr.Hosts,
			Password:         pr.Password,
			DB:               pr.DB,
			PoolSize:         pr.PoolSize,
			DisableIndentity: true, // suppress CLIENT SETINFO on Redis < 7.2
		})
		// 与上面 RedisClient 同口径只记日志不拒启:连不上时选库失败,认领回「暂不可用」,不会写错库。
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
