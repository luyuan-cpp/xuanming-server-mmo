// Package config 是 chat 服务的配置结构与启动期校验(契约 zone_contract_v1 §3/§4/§7/§9)。
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// MaxRpcTimeoutMs 是 zrpc 服务端整体超时的上限(毫秒)。
//
// 契约 §3 超时预算:业务服务 Timeout ≤ 路由服 ForwardTimeoutMs(默认 5000)− 1000。
// 为什么要留 1000ms:chat 必须先于路由服超时,把 in-band 结果回给路由服;反过来
// 路由服先超时会把"chat 的正常慢响应"判成 kServiceUnavailable,而 chat 那边的
// 副作用(消息已写入)仍然生效,客户端看到失败再重发 = 重复消息(幂等键只挡 60s)。
const MaxRpcTimeoutMs int64 = 4000

// Config 是 chat 服务的全部配置。
//
// 共享 Redis 库**不在这里重新声明**:嵌入的 zrpc.RpcServerConf 已经带了
// `Redis redis.RedisKeyConf`(yaml 顶层 `Redis:`),再声明一个同名字段会让
// go-zero conf 在加载期报 "conflict key Redis"(core/conf/config.go addOrMergeFields)。
// 所以共享库就用 c.Redis.RedisConf —— 与 match 同一写法。它在 v1 只用于 ChatRedis
// 未配置时的回落;契约 key(player:{id}:location 等)v1 一个都不读。
type Config struct {
	zrpc.RpcServerConf

	// ChatRedis:chat 私有 key(chat:*)的存储,可配 Type: cluster(所有 key 单 key 操作,
	// 集群安全,契约 §4/§9)。留空则回落到共享库 Redis,启动时打 WARN 并打印落点。
	// 历史 LIST 是这类数据的唯一副本:staging/prod 必须独立实例且 maxmemory-policy=noeviction
	// (compose 共享库是 allkeys-lfu,会把聊天记录当缓存淘汰)。
	ChatRedis redis.RedisConf `json:",optional"`

	// ZoneId:只影响 etcd 注册路径 <Prefix>/zone/<z>/...(路由服按 NodeInfo 发现本服务)。
	// 必填且拒 0(契约 §2):chat 是全局服务,业务代码**禁止**读它做分支(契约 §0)。
	// 刻意不给 default:漏配就在 Validate 里 fail-fast,而不是悄悄注册进 zone 1。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL:etcd 节点注册租约 TTL(秒)。它就是崩溃场景的切换窗口 ——
	// 路由服 PickRandom 不看连接状态,进程崩掉后最多这么久还会被选中(契约 §2)。
	LeaseTTL int64 `json:",default=60"`

	// MaxContentBytes:单条内容的最大**字节**数(不是字符数:512 字节 ≈ 170 个汉字)。
	MaxContentBytes int `json:",default=512"`

	// RateLimitPerSecond:每玩家每秒最多发几条(chat:{rl:<pid>},1 秒固定窗口)。
	RateLimitPerSecond int `json:",default=5"`

	// HistoryMaxEntries:每个频道 LIST 保留的最近条数(写入 Lua 里 LTRIM)。
	HistoryMaxEntries int `json:",default=200"`

	// HistoryTTLSeconds:频道 LIST 的 TTL(秒),每次写入续期;默认 7 天。
	// 历史是尽力而为窗口,不是权威账本(契约 §9)。
	HistoryTTLSeconds int `json:",default=604800"`

	// HistoryDefaultLimit:PullChatHistory.limit=0 时取多少条。
	HistoryDefaultLimit int `json:",default=20"`

	// HistoryMaxLimit:PullChatHistory.limit 的上限,超出钳到它。
	HistoryMaxLimit int `json:",default=50"`

	// RequestIdTTLSeconds:SendChat 幂等键 chat:{req:<pid>}:<request_id> 的 TTL(秒)。
	RequestIdTTLSeconds int `json:",default=60"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。
	// 端口分工(契约 §7):9101 login / 9150 scene_manager / 9160 db / 9170 match /
	// 9180 friend / 9190 player_locator / 9200 路由服 / 9210 chat。
	MetricsListenAddr string `json:",optional"`

	// KillSwitchPrefix:RPC 级热关停规则在 etcd 里的前缀,留空用 /mmorpg/killswitch/。
	KillSwitchPrefix string `json:",optional"`
}

// ListenHostPort 把 ListenOn 拆成 host 与端口号。用 net.SplitHostPort 以正确处理 "[::]:50700"。
func (c *Config) ListenHostPort() (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(c.ListenOn)
	if err != nil {
		return "", 0, fmt.Errorf("ListenOn 必须是 host:port,得到 %q: %w", c.ListenOn, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("ListenOn 端口非法: %q", c.ListenOn)
	}
	return host, uint32(port), nil
}

// Validate 做启动期整体校验(fail-fast)。每一条拒绝的都是"进程能起来、但行为静默出错"的配置:
//   - ZoneId=0:注册路径变成 zone/0,路由服按 zone 过滤/排障时看不见本节点;
//   - Timeout 为 0 或 >4000:违反超时预算(见 MaxRpcTimeoutMs),0 = 无上限同样违反;
//   - ListenOn 为空 / 非 host:port:注册进 etcd 的端点无法拨通;
//   - Etcd.Hosts 为空:注册与 killswitch 都没有 etcd 可用;
//   - Etcd.Key 非空:go-zero 会额外把本服务注册成 chat.rpc key,且被 go_services.ps1 -Zone
//     按 .z<N> 切分 —— 契约 D-13 规定全局服务 yaml 的 Etcd.Key 显式留空(不能整行省略:go-zero 的
//     EtcdConf.Key 不是 optional,Etcd 段存在而缺 Key 会在 conf 加载期报错),直到首个 Go 调用方出现;
//   - Redis.Host 为空:共享库是回落目标,也是将来读契约 key 的唯一句柄;
//   - 各上限 ≤0:例如 RateLimitPerSecond=0 会拒绝全部发言、HistoryMaxEntries=0 会让
//     LTRIM 0 -1 保留整条 LIST(无上限增长)。
func (c *Config) Validate() error {
	if c.ZoneId == 0 {
		return errors.New("ZoneId 必须非 0(K8s 全局服务用命令行 / ConfigMap 注入的 CurrentZoneId)")
	}
	if c.ListenOn == "" {
		return errors.New("ListenOn 不能为空")
	}
	if _, _, err := c.ListenHostPort(); err != nil {
		return err
	}
	if c.Timeout <= 0 || c.Timeout > MaxRpcTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须在 (0, %d] 内:业务服务超时须 ≤ 路由服 ForwardTimeoutMs − 1000(契约 §3)",
			c.Timeout, MaxRpcTimeoutMs)
	}
	if len(c.Etcd.Hosts) == 0 {
		return errors.New("Etcd.Hosts 不能为空(节点注册与 killswitch 都依赖 etcd)")
	}
	if c.Etcd.Key != "" {
		return fmt.Errorf("Etcd.Key 必须留空(得到 %q):全局服务不注册 go-zero key,契约 D-13", c.Etcd.Key)
	}
	if c.Redis.Host == "" {
		return errors.New("Redis.Host(共享库)不能为空:ChatRedis 未配置时私有 key 回落到它")
	}

	positives := []struct {
		name  string
		value int64
	}{
		{"LeaseTTL", c.LeaseTTL},
		{"MaxContentBytes", int64(c.MaxContentBytes)},
		{"RateLimitPerSecond", int64(c.RateLimitPerSecond)},
		{"HistoryMaxEntries", int64(c.HistoryMaxEntries)},
		{"HistoryTTLSeconds", int64(c.HistoryTTLSeconds)},
		{"HistoryDefaultLimit", int64(c.HistoryDefaultLimit)},
		{"HistoryMaxLimit", int64(c.HistoryMaxLimit)},
		{"RequestIdTTLSeconds", int64(c.RequestIdTTLSeconds)},
	}
	for _, p := range positives {
		if p.value <= 0 {
			return fmt.Errorf("%s 必须为正数,得到 %d", p.name, p.value)
		}
	}
	if c.HistoryDefaultLimit > c.HistoryMaxLimit {
		return fmt.Errorf("HistoryDefaultLimit(%d)不能大于 HistoryMaxLimit(%d)",
			c.HistoryDefaultLimit, c.HistoryMaxLimit)
	}
	return nil
}
