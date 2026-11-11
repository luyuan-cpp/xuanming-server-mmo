// Package redisauth 给共享 Redis 的客户端配置填口令。共享 Redis 指核心实例,以及按用途从它拆出的 queue / ctl 实例,
// 它们在部署上用同一个口令;玩家域 Redis Cluster(playerredis.Config.PasswordEnv)与 match 独占集群不归这里。
//
// K8s 上口令只放在 Secret redis-auth 里(tools/scripts/k8s_deploy.ps1 的 Sync-RedisAuthSecret),经 secretKeyRef 注入容器环境变量。
// ConfigMap 里各 Redis 段的 Pass / Password 写空串,另有一行顶层键 SharedRedisPasswordEnv,写的是那个环境变量的名字
// (docs/handoff/handoff-global-singletons-p0-20261015.md §9)。各服务加载配置之后调 Apply,把变量的值填进本服务所有共享 Redis 段;
// 之后建客户端、玩家域回落(playerredis.Config.OrDefault)读到的都是填好的值。
//
// 填值的规则与「为什么在配置里写变量名、为什么不用 conf.UseEnv()」见 shared/secretenv(本包是它的一个用途:配置键固定为
// SharedRedisPasswordEnv)。这里只多一条取舍:为什么一个服务只有一个键,而不是每段一个 PasswordEnv —— go-zero 的 redis.RedisConf
// (含 zrpc.RpcServerConf 内嵌的 Redis 段)加不了字段;而且 Secret redis-auth 只有一个口令,分段写只会多出写错的机会。
package redisauth

import "shared/secretenv"

// Target 是一个要填口令的配置字段。Field 是它在配置文件里的键路径(如 "Node.RedisClient.Password"),只用于报错。
type Target struct {
	Field    string
	Password *string
}

// Apply 在 envName(配置里 SharedRedisPasswordEnv 的值)非空时,把这个环境变量的值写进每个 target,规则同 secretenv.Fill:
// envName 为空时什么都不做(本机 etc/*.yaml 与 dev 档 ConfigMap,口令通常是空串,即不认证);变量缺席或为空、或某段同时写了字面量,
// 都返回错误且不改任何 target。错误文本只点键名与变量名,不含口令。lookup 通常传 os.LookupEnv,单测传桩。
func Apply(envName string, lookup func(string) (string, bool), targets ...Target) error {
	converted := make([]secretenv.Target, len(targets))
	for i, t := range targets {
		converted[i] = secretenv.Target{Field: t.Field, Value: t.Password}
	}
	return secretenv.Fill("SharedRedisPasswordEnv", envName, lookup, converted...)
}
