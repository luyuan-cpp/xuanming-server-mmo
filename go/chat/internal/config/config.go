// Package config 是 chat 服务的配置结构与启动期校验(契约 zone_contract_v1 §3/§4/§7/§9)。
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"

	"chat/internal/constants"
	"chat/internal/textfilter"
	"shared/authoritysql"
	"shared/playerredis"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// MaxRpcTimeoutMs 是 zrpc 服务端整体超时的上限(毫秒)。
//
// 契约 §3 超时预算:业务服务 Timeout ≤ 路由服 ForwardTimeoutMs(默认 5000)− 1000。
// 为什么要留 1000ms:chat 必须先于路由服超时,把 in-band 结果回给路由服;反过来
// 路由服先超时会把"chat 的正常慢响应"判成 kServiceUnavailable,而 chat 那边的
// 副作用(消息已写入)仍然生效；客户端保留原request_id可从永久SQL收据恢复同一决定。
const MaxRpcTimeoutMs int64 = 4000

// Config 是 chat 服务的全部配置。
//
// 共享 Redis 库**不在这里重新声明**:嵌入的 zrpc.RpcServerConf 已经带了
// `Redis redis.RedisKeyConf`(yaml 顶层 `Redis:`),再声明一个同名字段会让
// go-zero conf 在加载期报 "conflict key Redis"(core/conf/config.go addOrMergeFields)。
// 所以共享缓存句柄沿用c.Redis.RedisConf；正式Send/Pull只走SQL，不回落到缓存授权或历史。
type Config struct {
	zrpc.RpcServerConf

	// 聊天原件与角色目录走同一连接池(同一 TiDB 集群或同一 MySQL 主库),目录只做跨库普通读、不同事务；环境变量名不含凭据值。
	DSNEnv string `json:",optional"`
	// Backend:tidb | mysql,不写就是 tidb(docs/design/player-data-tidb-scaling-plan.md §9);非法值拒启。
	Backend           string `json:",default=tidb"`
	MaxOpenConns      int    `json:",default=16"`
	AccountRoleSchema string `json:",optional"`
	// ActorVerifyTTLSeconds:身份校验结果的进程内缓存秒数,0 = 每次都校验。
	// 代价:角色被挂牌或过户之后,旧会话最多还能发这么久的聊天(authority/actor_verify.go)。
	ActorVerifyTTLSeconds int                           `json:",default=30"`
	GlobalLoginSessions   bool                          `json:",default=true"`
	LoginRedis            playerredis.LoginReaderConfig `json:",optional"`

	// ChatRedis保留既有可丢缓存配置；正式Send/Pull只用SQL原件，不读写旧LIST或授权键。
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

	// SensitiveWords 为启动时加载的字面屏蔽词表；空表不过滤。
	// 大小写敏感、按配置顺序替换，一个 Unicode 字符变为一个 *，与原 Go 聊天业务一致。
	SensitiveWords []string `json:",optional"`

	// RateLimitPerSecond:每玩家每秒最多接受几条；按SQL时间固定秒窗口计数,并发在途时可能略超。
	RateLimitPerSecond int `json:",default=5"`

	// HistoryMaxEntries:每个SQL频道窗口最多可见的最近条数。
	HistoryMaxEntries int `json:",default=200"`

	// HistoryTTLSeconds:频道窗口闲置TTL；保持旧LIST续期/过期间隔的可见历史语义。
	// 显示过期不删除永久发送原件，旧UUID不能重新进入。
	HistoryTTLSeconds int `json:",default=604800"`

	// HistoryDefaultLimit:PullChatHistory.limit=0 时取多少条。
	HistoryDefaultLimit int `json:",default=20"`

	// HistoryMaxLimit:PullChatHistory.limit 的配置上限，不得超过MaxHistoryPullLimit。
	HistoryMaxLimit int `json:",default=50"`

	// RequestIdTTLSeconds只保留旧配置兼容；SQL成功收据永久保存，不再按此值删除。
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
//   - Redis.Host为空:既有框架共享缓存句柄配置不完整；业务授权/历史不依赖它;
//   - 各上限 ≤0:例如 RateLimitPerSecond=0 会拒绝全部发言、HistoryMaxEntries=0 会让
//     SQL窗口失去必要上限。
func (c *Config) Validate() error {
	if _, err := authoritysql.ParseBackend(c.Backend); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(c.DSNEnv) || c.MaxOpenConns < 2 || c.MaxOpenConns > 128 || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`).MatchString(c.AccountRoleSchema) || !c.GlobalLoginSessions || !c.LoginRedis.HasAddr() || c.LoginRedis.DB < 0 || c.LoginRedis.PoolSizePerNode < 1 || c.LoginRedis.PoolSizePerNode > 128 {
		return errors.New("聊天必须显式配置SQL原件库、同域角色目录与全局原登录态只读句柄")
	}
	if c.ActorVerifyTTLSeconds < 0 || c.ActorVerifyTTLSeconds > constants.MaxActorVerifyTTLSeconds {
		return fmt.Errorf("ActorVerifyTTLSeconds(%d)必须在 [0, %d] 内", c.ActorVerifyTTLSeconds, constants.MaxActorVerifyTTLSeconds)
	}
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
		return errors.New("Redis.Host不能为空：既有框架缓存句柄配置须完整，SQL授权与历史不向它回落")
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
	if c.HistoryMaxLimit > constants.MaxHistoryPullLimit {
		return fmt.Errorf("HistoryMaxLimit不得超过%d条权威读取预算", constants.MaxHistoryPullLimit)
	}
	if _, err := textfilter.New(c.SensitiveWords); err != nil {
		return err
	}
	return nil
}
