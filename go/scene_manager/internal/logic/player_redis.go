package logic

import (
	"context"
	"errors"
	"time"

	"scene_manager/internal/svc"

	goredis "github.com/redis/go-redis/v9"
)

// 玩家域句柄 svcCtx.PlayerRedis 的读写(docs/design/redis-player-store-cluster.md §5.7、§5.10)。
//
// 本包的调用点大多不带 ctx,这里统一用 context.Background():与它们换句柄之前(go-zero 的无 ctx 版本)同一语义,
// 单次读写受句柄的读写超时约束。Get 把缺键翻成空串,也是 go-zero 的形状,调用点的判断不用改。
//
// 与 go-zero 句柄的区别:玩家域句柄不自动重发结果未知的命令(MaxRetries=-1),读超时直接报错。本包脚本注释里的「go-redis 原样重发」
// 是换句柄之前的行为,脚本的重放识别留着:调用方(C++、player_locator)重试时走的是同一条路径。

// playerRedisGet 读字符串键,键不存在返回 ("", nil)。
func playerRedisGet(svcCtx *svc.ServiceContext, key string) (string, error) {
	val, err := svcCtx.PlayerRedis.Get(context.Background(), key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", nil
	}
	return val, err
}

// playerRedisExists 报告键是否存在。
func playerRedisExists(svcCtx *svc.ServiceContext, key string) (bool, error) {
	n, err := svcCtx.PlayerRedis.Exists(context.Background(), key).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// playerRedisSetex 写带过期的键。
func playerRedisSetex(svcCtx *svc.ServiceContext, key, value string, seconds int) error {
	return svcCtx.PlayerRedis.Set(context.Background(), key, value, time.Duration(seconds)*time.Second).Err()
}

// playerRedisSetnx 键不存在时写入,不带过期。返回是否写入。
func playerRedisSetnx(svcCtx *svc.ServiceContext, key, value string) (bool, error) {
	return svcCtx.PlayerRedis.SetNX(context.Background(), key, value, 0).Result()
}

// playerRedisSetnxEx 键不存在时写入并带过期。返回是否写入。
func playerRedisSetnxEx(svcCtx *svc.ServiceContext, key, value string, seconds int) (bool, error) {
	return svcCtx.PlayerRedis.SetNX(context.Background(), key, value, time.Duration(seconds)*time.Second).Result()
}

// playerRedisHget 读 hash 字段。字段或键不存在时错误是 goredis.Nil。
func playerRedisHget(svcCtx *svc.ServiceContext, key, field string) (string, error) {
	return svcCtx.PlayerRedis.HGet(context.Background(), key, field).Result()
}

// playerRedisEval 执行一段玩家域脚本。KEYS 必须都在这名玩家的 slot 上,发送前由句柄的守卫核对。
func playerRedisEval(svcCtx *svc.ServiceContext, script string, keys []string, args ...any) (any, error) {
	return svcCtx.PlayerRedis.Eval(context.Background(), script, keys, args...).Result()
}
