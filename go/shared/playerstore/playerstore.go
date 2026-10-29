// Package playerstore 构造玩家域 Redis 客户端(docs/design/redis-player-store-cluster.md §5.7)。
//
// 玩家域是一切以 player_id 为身份的键所在的存储域,部署形态是 Redis Cluster(开发环境可以是单实例)。
// 本包负责四件事:
//   - 按配置产出 redis.UniversalClient,并把每节点连接池上限显式化(go-redis 缺省每节点 5×GOMAXPROCS,
//     主节点数增长后会把连接数放大到不可控);
//   - 不自动重放「结果未知」的命令:命令发出后遇到的网络错误原样报给调用方,不由客户端重发
//     (见 noreplay.go;与 C++ 路由层「结果未知按空应答上报」同一语义);
//   - 挂发送前守卫:多键命令的键必须同 slot,玩家域的键必须带 hash tag;单实例开发时同样生效,
//     让跨槽缺陷在开发期就暴露,而不是上了 Cluster 才收到 CROSSSLOT;
//   - 提供跨玩家批量读与逐主节点扫描的正确写法(MGetBySlot、ScanEachMaster)。
package playerstore

import (
	"errors"
	"fmt"
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

// Config 是玩家域客户端的配置段(go-zero conf 的 json 标签)。
//
// Addrs 只有一个且 Cluster=false 时是单实例;Cluster=true 时 Addrs 是集群种子(至少一个),客户端按 slot 路由并处理
// MOVED / ASK。玩家域只用 DB 0(Redis Cluster 只有 DB 0),所以没有 DB 字段。
type Config struct {
	Addrs    []string
	Cluster  bool   `json:",optional"`
	Password string `json:",optional"`
	// PoolSizePerNode:每个节点的连接池上限(Cluster 下按节点计,单实例即总上限);0 = DefaultPoolSizePerNode。
	// 单实例阶段并发高的服务(go/db、login)按现值设定。
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
		return errors.New("playerstore: Addrs 不能为空")
	}
	for _, a := range c.Addrs {
		if strings.TrimSpace(a) == "" {
			return errors.New("playerstore: Addrs 含空地址")
		}
	}
	if !c.Cluster && len(c.Addrs) != 1 {
		return fmt.Errorf("playerstore: 单实例模式只能有 1 个地址,实际 %d 个(多地址须 Cluster=true)", len(c.Addrs))
	}
	if c.PoolSizePerNode < 0 || c.PoolSizePerNode > MaxPoolSizePerNode {
		return fmt.Errorf("playerstore: PoolSizePerNode 须在 [0, %d]", MaxPoolSizePerNode)
	}
	if c.ConnMaxIdleSeconds < 0 {
		return errors.New("playerstore: ConnMaxIdleSeconds 不能为负")
	}
	timeouts := []struct {
		name string
		v    int
	}{{"DialTimeoutMs", c.DialTimeoutMs}, {"ReadTimeoutMs", c.ReadTimeoutMs}, {"WriteTimeoutMs", c.WriteTimeoutMs}, {"PoolTimeoutMs", c.PoolTimeoutMs}}
	for _, t := range timeouts {
		if t.v < 0 || t.v > MaxTimeoutMs {
			return fmt.Errorf("playerstore: %s 须在 [0, %d]", t.name, MaxTimeoutMs)
		}
	}
	return nil
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
	if err := c.Validate(); err != nil {
		return nil, err
	}
	pool := orDefault(c.PoolSizePerNode, DefaultPoolSizePerNode)
	idle := time.Duration(orDefault(c.ConnMaxIdleSeconds, DefaultConnMaxIdleSeconds)) * time.Second
	var client redis.UniversalClient
	if c.Cluster {
		cc := redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        c.Addrs,
			Password:     c.Password,
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
			ConnMaxIdleTime:       idle,
			DisableIdentity:       true,
		})
		cc.OnNewNode(func(node *redis.Client) { node.AddHook(noReplayHook{}) })
		client = cc
	} else {
		client = redis.NewClient(&redis.Options{
			Addr:                  c.Addrs[0],
			Password:              c.Password,
			DB:                    0,
			MaxRetries:            -1, // 结果未知的命令不由客户端重发
			DialTimeout:           millis(c.DialTimeoutMs),
			ReadTimeout:           millis(c.ReadTimeoutMs),
			WriteTimeout:          millis(c.WriteTimeoutMs),
			PoolTimeout:           millis(c.PoolTimeoutMs),
			ContextTimeoutEnabled: !c.DisableContextTimeout,
			PoolSize:              pool,
			MaxActiveConns:        pool,
			MinIdleConns:          0,
			ConnMaxIdleTime:       idle,
			DisableIdentity:       true,
		})
	}
	client.AddHook(GuardHook{})
	return client, nil
}
