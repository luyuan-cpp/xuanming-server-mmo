// Package playerredis 构造玩家域 Redis 客户端(docs/design/redis-player-store-cluster.md §5.7)。
//
// 玩家域是一切以 player_id 为身份的协调键所在的存储域,部署形态是 Redis Cluster(开发环境可以是单实例)。
// 它和 TiDB 里的 player_store 表、go/db/internal/playerstore 不是一回事:存档不在 Redis,这里只有位置、会话、
// 战斗锁这类协调键。
//
// 本包负责五件事:
//   - 按配置产出 redis.UniversalClient,并把每节点连接池上限显式化(go-redis 缺省每节点 10×GOMAXPROCS,
//     主节点数增长后会把连接数放大到不可控);
//   - 不自动重放「结果未知」的命令:命令发出后遇到的网络错误原样报给调用方,不由客户端重发
//     (见 noreplay.go;与 C++ 路由层「结果未知按空应答上报」同一语义);
//   - 挂发送前守卫:多键命令的键必须同 slot,玩家域的键必须带 hash tag;单实例开发时同样生效,
//     让跨槽缺陷在开发期就暴露,而不是上了 Cluster 才收到 CROSSSLOT;
//   - 提供跨玩家批量读与逐主节点扫描的正确写法(MGetBySlot、ScanEachMaster、ScanPage);
//   - 给「同一节点、同一条连接连发一条 pipeline」的铸造通道提供节点句柄(pinned.go)。
package playerredis

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 缺省值与上界。上界防止配置笔误把连接数或超时放大到失控。
const (
	DefaultPoolSizePerNode    = 4
	MaxPoolSizePerNode        = 128
	DefaultConnMaxIdleSeconds = 60
	DefaultTimeoutMs          = 1000
	MaxTimeoutMs              = 10000
	DefaultMaxRedirects       = 8
)

// Config 是玩家域客户端的配置段,各服务的配置结构统一用它声明 `PlayerRedis`(go-zero conf 的 json 标签)。
//
// Addrs 只有一个且 Cluster=false 时是单实例;Cluster=true 时 Addrs 是集群种子(至少一个),客户端按 slot 路由并处理
// MOVED / ASK。
//
// 整段不写时 Addrs 为空,由服务用 OrDefault 指向它现有的 Redis 单实例:开发环境与还没渲染这一段的 K8s ConfigMap 不用改就能跑。
//
// 登录域(login 与读登录态的服务)用同一个结构,部署上可以和玩家域共用一个 Cluster,也可以拆开(设计 §10.2)。
type Config struct {
	Addrs    []string `json:",optional"`
	Cluster  bool     `json:",optional"`
	Username string   `json:",optional"`
	Password string   `json:",optional"`
	// DB 只对单实例有意义:OrDefault 指向现有实例时要落在它原来的库号上。Redis Cluster 只有 DB 0,Cluster=true 时写别的值拒启。
	DB int `json:",optional"`
	// PasswordEnv:口令所在的环境变量名,非空时优先于 Password。K8s 上口令不进 ConfigMap 时用它。
	PasswordEnv string `json:",optional"`
	// TLS:true = 走 TLS 且不校验证书,与 go-zero redis.RedisConf.Tls 同口径。
	TLS bool `json:",optional"`
	// PoolSizePerNode:每个节点的连接池上限(Cluster 下按节点计,单实例即总上限);0 = DefaultPoolSizePerNode。
	// 单实例阶段并发高的服务按现值设定。
	PoolSizePerNode int `json:",optional"`
	// ConnMaxIdleSeconds:空闲连接回收时间;0 = DefaultConnMaxIdleSeconds。不预留空闲连接(MinIdleConns=0),
	// 不常访问的节点会回落到零连接。
	ConnMaxIdleSeconds int `json:",optional"`
	// 拨号 / 读 / 写 / 取连接超时(毫秒);0 = DefaultTimeoutMs。
	DialTimeoutMs  int `json:",optional"`
	ReadTimeoutMs  int `json:",optional"`
	WriteTimeoutMs int `json:",optional"`
	PoolTimeoutMs  int `json:",optional"`
	// DisableContextTimeout:true = 不用调用方 ctx 的截止时间约束套接字读写(go-redis 缺省行为)。
	// 缺省 false(即开启):player_inbox 500ms、guild 2s 这类调用预算依赖 ctx 截止约束读写。
	DisableContextTimeout bool `json:",optional"`
}

// Validate 在构造客户端前校验配置;错误文本不含口令。
func (c Config) Validate() error {
	if len(c.Addrs) == 0 {
		return errors.New("playerredis: Addrs must not be empty")
	}
	for _, a := range c.Addrs {
		if strings.TrimSpace(a) == "" {
			return errors.New("playerredis: Addrs contains an empty address")
		}
	}
	if !c.Cluster && len(c.Addrs) != 1 {
		return fmt.Errorf("playerredis: single-instance mode takes exactly 1 address, got %d (multiple addresses need Cluster=true)", len(c.Addrs))
	}
	if c.DB < 0 {
		return errors.New("playerredis: DB must not be negative")
	}
	if c.Cluster && c.DB != 0 {
		return errors.New("playerredis: Redis Cluster only has DB 0")
	}
	if c.PoolSizePerNode < 0 || c.PoolSizePerNode > MaxPoolSizePerNode {
		return fmt.Errorf("playerredis: PoolSizePerNode must be in [0, %d]", MaxPoolSizePerNode)
	}
	if c.ConnMaxIdleSeconds < 0 {
		return errors.New("playerredis: ConnMaxIdleSeconds must not be negative")
	}
	timeouts := []struct {
		name string
		v    int
	}{{"DialTimeoutMs", c.DialTimeoutMs}, {"ReadTimeoutMs", c.ReadTimeoutMs}, {"WriteTimeoutMs", c.WriteTimeoutMs}, {"PoolTimeoutMs", c.PoolTimeoutMs}}
	for _, t := range timeouts {
		if t.v < 0 || t.v > MaxTimeoutMs {
			return fmt.Errorf("playerredis: %s must be in [0, %d]", t.name, MaxTimeoutMs)
		}
	}
	return nil
}

// ValidateSection 校验配置文件里写出来的这一段:没写 Addrs 合法(由 OrDefault 指向现有实例),其余规则同 Validate。
// Cluster=true 却没写种子地址是配错了,不能静默落到现有实例上。
func (c Config) ValidateSection() error {
	if len(c.Addrs) > 0 {
		return c.Validate()
	}
	if c.Cluster {
		return errors.New("playerredis: Cluster=true needs Addrs")
	}
	c.Addrs = []string{"unset:0"}
	return c.Validate()
}

// OrDefault 在没写 Addrs 时指向 fallback,否则原样返回。fallback 由服务给出:现有 Redis 单实例的地址与口令,加上它现有的连接池和
// 超时口径。这一段里单独写的池与超时优先于 fallback:etc 下的 yaml 不写地址也能把连接池上限写明白。
func (c Config) OrDefault(fallback Config) Config {
	if len(c.Addrs) > 0 {
		return c
	}
	if c.DB != 0 {
		fallback.DB = c.DB
	}
	if c.PoolSizePerNode != 0 {
		fallback.PoolSizePerNode = c.PoolSizePerNode
	}
	if c.ConnMaxIdleSeconds != 0 {
		fallback.ConnMaxIdleSeconds = c.ConnMaxIdleSeconds
	}
	if c.DialTimeoutMs != 0 {
		fallback.DialTimeoutMs = c.DialTimeoutMs
	}
	if c.ReadTimeoutMs != 0 {
		fallback.ReadTimeoutMs = c.ReadTimeoutMs
	}
	if c.WriteTimeoutMs != 0 {
		fallback.WriteTimeoutMs = c.WriteTimeoutMs
	}
	if c.PoolTimeoutMs != 0 {
		fallback.PoolTimeoutMs = c.PoolTimeoutMs
	}
	if c.DisableContextTimeout {
		fallback.DisableContextTimeout = true
	}
	return fallback
}

// Describe 是配置的可读描述(不含口令),供启动日志使用。
func (c Config) Describe() string {
	return fmt.Sprintf("addrs=%s cluster=%v pool_per_node=%d", strings.Join(c.Addrs, ","), c.Cluster, orDefault(c.PoolSizePerNode, DefaultPoolSizePerNode))
}

func (c Config) password() string {
	if c.PasswordEnv != "" {
		return os.Getenv(c.PasswordEnv)
	}
	return c.Password
}

func (c Config) tlsConfig() *tls.Config {
	if !c.TLS {
		return nil
	}
	// 与 go-zero redisclientmanager.go 的 getClient 同口径,同一份配置两边行为一致。
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 与 go-zero 对同一配置项的处理一致
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func millis(v int) time.Duration {
	return time.Duration(orDefault(v, DefaultTimeoutMs)) * time.Millisecond
}

// New 按配置构造玩家域客户端,挂上守卫与不重放钩子。返回的客户端由调用方 Close。
func New(c Config) (redis.UniversalClient, error) {
	client, err := newClient(c)
	if err != nil {
		return nil, err
	}
	client.AddHook(GuardHook{})
	return client, nil
}

func newClient(c Config) (redis.UniversalClient, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Cluster {
		return newClusterClient(c), nil
	}
	return redis.NewClient(nodeOptions(c, c.Addrs[0])), nil
}

func newClusterClient(c Config) *redis.ClusterClient {
	pool := orDefault(c.PoolSizePerNode, DefaultPoolSizePerNode)
	cc := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        c.Addrs,
		Username:     c.Username,
		Password:     c.password(),
		TLSConfig:    c.tlsConfig(),
		MaxRedirects: DefaultMaxRedirects,
		// 节点客户端自己不重试;集群层只重试「未执行」的重定向类错误(网络错误由 noReplayHook 改成不可重试)。
		MaxRetries:            -1,
		DialTimeout:           millis(c.DialTimeoutMs),
		ReadTimeout:           millis(c.ReadTimeoutMs),
		WriteTimeout:          millis(c.WriteTimeoutMs),
		PoolTimeout:           millis(c.PoolTimeoutMs),
		ContextTimeoutEnabled: !c.DisableContextTimeout,
		PoolSize:              pool,
		MaxActiveConns:        pool,
		MinIdleConns:          0,
		ConnMaxIdleTime:       time.Duration(orDefault(c.ConnMaxIdleSeconds, DefaultConnMaxIdleSeconds)) * time.Second,
		DisableIdentity:       true,
	})
	cc.OnNewNode(func(node *redis.Client) { node.AddHook(noReplayHook{}) })
	return cc
}

// nodeOptions 是连单个节点的客户端参数。
func nodeOptions(c Config, addr string) *redis.Options {
	pool := orDefault(c.PoolSizePerNode, DefaultPoolSizePerNode)
	return &redis.Options{
		Addr:                  addr,
		Username:              c.Username,
		Password:              c.password(),
		TLSConfig:             c.tlsConfig(),
		DB:                    c.DB,
		MaxRetries:            -1, // 结果未知的命令不由客户端重发
		DialerRetries:         1,  // 连不上立刻报错,让调用方的时间预算决定下一步
		DialTimeout:           millis(c.DialTimeoutMs),
		ReadTimeout:           millis(c.ReadTimeoutMs),
		WriteTimeout:          millis(c.WriteTimeoutMs),
		PoolTimeout:           millis(c.PoolTimeoutMs),
		ContextTimeoutEnabled: !c.DisableContextTimeout,
		PoolSize:              pool,
		MaxActiveConns:        pool,
		MinIdleConns:          0,
		ConnMaxIdleTime:       time.Duration(orDefault(c.ConnMaxIdleSeconds, DefaultConnMaxIdleSeconds)) * time.Second,
		DisableIdentity:       true,
	}
}
