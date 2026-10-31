package svc

import (
	"errors"

	"github.com/redis/go-redis/v9"
	"match/internal/config"
	"shared/playerredis"
)

// 与login同一全局命名空间，独立只读池，不回落到匹配缓存或zone在线投影。
func NewLoginSessionClient(c config.Config) (redis.UniversalClient, error) {
	if !c.GlobalLoginSessions || !c.LoginRedis.HasAddr() || c.LoginRedis.DB < 0 || c.LoginRedis.PoolSizePerNode < 1 || c.LoginRedis.PoolSizePerNode > 128 {
		return nil, errors.New("匹配必须显式配置全局login Redis实例/DB/池上限")
	}
	return playerredis.New(c.LoginRedis.ClientConfig(playerredis.Config{DialTimeoutMs: 1000, ReadTimeoutMs: 300, WriteTimeoutMs: 300, PoolTimeoutMs: 300}))
}
