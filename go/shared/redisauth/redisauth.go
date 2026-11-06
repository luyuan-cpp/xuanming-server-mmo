// Package redisauth 给共享 Redis 的客户端配置填口令。共享 Redis 指核心实例,以及按用途从它拆出的 queue / ctl 实例,
// 它们在部署上用同一个口令;玩家域 Redis Cluster(playerredis.Config.PasswordEnv)与 match 独占集群不归这里。
//
// K8s 上口令只放在 Secret redis-auth 里(tools/scripts/k8s_deploy.ps1 的 Sync-RedisAuthSecret),经 secretKeyRef 注入容器环境变量。
// ConfigMap 里各 Redis 段的 Pass / Password 写空串,另有一行顶层键 SharedRedisPasswordEnv,写的是那个环境变量的名字
// (docs/handoff/handoff-global-singletons-p0-20261015.md §9)。各服务加载配置之后调 Apply,把变量的值填进本服务所有共享 Redis 段;
// 之后建客户端、玩家域回落(playerredis.Config.OrDefault)读到的都是填好的值。
//
// 三个取舍:
//   - 为什么在配置里写变量名,而不是认一个固定的变量名:仓库里其余从环境变量取的密钥都这么写(PasswordAuth.DSNEnv、
//     PlayerRedis.PasswordEnv、mail 的 SharedRedisPasswordEnv、PlayerInbox.AdminTokenEnv),读配置的人能直接看到口令从哪来;
//     本机 etc/*.yaml 不写这个键,机器上残留的同名环境变量不会悄悄改掉本机的行为。
//   - 为什么不用 go-zero 的 conf.UseEnv():它对整份配置做 os.ExpandEnv,任何值里的 `$` 都会被改写。
//   - 为什么一个服务只有一个键,而不是每段一个 PasswordEnv:go-zero 的 redis.RedisConf(含 zrpc.RpcServerConf 内嵌的 Redis 段)
//     加不了字段;而且 Secret redis-auth 只有一个口令,分段写只会多出写错的机会。
package redisauth

import (
	"errors"
	"fmt"
)

// Target 是一个要填口令的配置字段。Field 是它在配置文件里的键路径(如 "Node.RedisClient.Password"),只用于报错。
type Target struct {
	Field    string
	Password *string
}

// Apply 在 envName 非空时,把环境变量 envName 的值写进每个 target。
//   - envName 为空:什么都不做,各段照用配置里的字面量。本机 etc/*.yaml 与 dev 档的 K8s ConfigMap 都是这种情形,口令通常是空串,即不认证。
//   - 环境变量缺席或为空串:返回错误。配置写明口令在这个变量里,变量却没有值,带着空口令起来只会让每次调用都收 NOAUTH,
//     不如启动即失败。K8s 上 secretKeyRef 不写 optional,Secret 缺失时 Pod 根本起不来;走到这里的是绕过部署脚本的情形。
//   - 某个 target 已有非空字面量:返回错误。口令只能有一个来源,否则换了 Secret 进程却还在用配置里的旧值,排查时看不出来。
//
// 出错时不改任何 target。错误文本只点键名与变量名,不含口令。lookup 通常传 os.LookupEnv,单测传桩。
func Apply(envName string, lookup func(string) (string, bool), targets ...Target) error {
	if envName == "" {
		return nil
	}
	if lookup == nil {
		return errors.New("redisauth: lookup must not be nil")
	}
	for _, t := range targets {
		if t.Password == nil {
			return fmt.Errorf("redisauth: target %q has no password field", t.Field)
		}
		if *t.Password != "" {
			return fmt.Errorf("redisauth: %s is set literally while SharedRedisPasswordEnv=%s; the password must come from one source only", t.Field, envName)
		}
	}
	value, ok := lookup(envName)
	if !ok || value == "" {
		return fmt.Errorf("redisauth: SharedRedisPasswordEnv=%s, but that environment variable is unset or empty", envName)
	}
	for _, t := range targets {
		*t.Password = value
	}
	return nil
}
