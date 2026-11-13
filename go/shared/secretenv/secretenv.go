// Package secretenv 把配置里「指名环境变量」的密钥填进配置字段。
//
// K8s 上密钥只放 Secret,经 secretKeyRef 注入容器环境变量(tools/scripts/k8s_deploy.ps1)。ConfigMap 里密钥字段写空串,另有一个键写变量的名字
// (如 scene-manager 的 GateTokenSecretEnv: MMORPG_GATE_TOKEN_SECRET)。服务加载配置之后调 Fill,把变量的值填进对应字段;之后建连接、校验读到的都是填好的值。
// 规则与取舍见 docs/design/workload-secrets-via-k8s-secret.md。共享 Redis 口令经 shared/redisauth 走同一条规则。
//
// 两个取舍:
//   - 为什么在配置里写变量名,而不是认一个固定的变量名:读配置的人能直接看到密钥从哪来;本机 etc/*.yaml 不写这个键,
//     机器上残留的同名环境变量不会悄悄改掉本机的行为。
//   - 为什么不用 go-zero 的 conf.UseEnv():它对整份配置做 os.ExpandEnv,任何值里的 `$` 都会被改写。
package secretenv

import (
	"errors"
	"fmt"
)

// Target 是一个要填密钥的配置字段。Field 是它在配置文件里的键路径(如 "GateTokenSecret"),只用于报错。
type Target struct {
	Field string
	Value *string
}

// Fill 在 envName 非空时,把环境变量 envName 的值写进每个 target。envKey 是配置里写变量名的那个键的路径
// (如 "GateTokenSecretEnv"),只用于报错。
//   - envName 为空:什么都不做,各字段照用配置里的字面量(本机 etc/*.yaml 的情形)。
//   - 环境变量缺席或为空串:返回错误。配置写明密钥在这个变量里、变量却没有值,带着空密钥起来只会把错误推迟到第一次连接或验签,
//     不如启动即失败。K8s 上 secretKeyRef 不写 optional,Secret 缺失时 Pod 根本起不来;走到这里的是绕过部署脚本的情形。
//   - 没有 target、或某个 target 没有字段指针:返回错误,这是调用方接线写错了。
//   - 某个 target 已有非空字面量:返回错误。密钥只能有一个来源,否则换了 Secret 进程却还在用配置里的旧值,排查时看不出来。
//
// 值原样写入,不去空白、不展开 `$`。出错时不改任何 target。错误文本只点键名与变量名,不含密钥。lookup 通常传 os.LookupEnv,单测传桩。
func Fill(envKey, envName string, lookup func(string) (string, bool), targets ...Target) error {
	if envName == "" {
		return nil
	}
	if lookup == nil {
		return errors.New("secretenv: lookup must not be nil")
	}
	if len(targets) == 0 {
		return fmt.Errorf("secretenv: %s=%s has no field to fill", envKey, envName)
	}
	for _, t := range targets {
		if t.Value == nil {
			return fmt.Errorf("secretenv: target %q has no field to fill", t.Field)
		}
		if *t.Value != "" {
			return fmt.Errorf("secretenv: %s is set literally while %s=%s; the secret must come from one source only", t.Field, envKey, envName)
		}
	}
	value, ok := lookup(envName)
	if !ok || value == "" {
		return fmt.Errorf("secretenv: %s=%s, but that environment variable is unset or empty", envKey, envName)
	}
	for _, t := range targets {
		*t.Value = value
	}
	return nil
}
