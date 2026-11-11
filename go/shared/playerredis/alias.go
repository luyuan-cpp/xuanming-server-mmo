// Package playerredis 是 shared/clusterredis 的别名层,只为现有导入不用改(全部 Redis 上 Cluster 第 1 批,
// docs/design/redis-cluster-all-domains-20261110.md §5.1)。实现 2026-11-11 挪到 shared/clusterredis,这里不再有逻辑。
//
// 这里的构造器与守卫都是玩家域口径:New 用 clusterredis.PlayerDomainKeys,CheckKeys / CheckCommand 按玩家域规则判,
// GuardHook 的零值也是玩家域守卫。控制集群的客户端用 clusterredis.New(c, clusterredis.ControlClusterKeys),
// 新代码一律直接从 shared/clusterredis 导入。
package playerredis

import (
	"github.com/redis/go-redis/v9"

	"shared/clusterredis"
)

type (
	Config             = clusterredis.Config
	GuardHook          = clusterredis.GuardHook
	ResultUnknownError = clusterredis.ResultUnknownError
	NotExecuted        = clusterredis.NotExecuted
	ScanCursor         = clusterredis.ScanCursor
	ScanPageResult     = clusterredis.ScanPageResult
)

const (
	DefaultPoolSizePerNode    = clusterredis.DefaultPoolSizePerNode
	MaxPoolSizePerNode        = clusterredis.MaxPoolSizePerNode
	DefaultConnMaxIdleSeconds = clusterredis.DefaultConnMaxIdleSeconds
	DefaultTimeoutMs          = clusterredis.DefaultTimeoutMs
	MaxTimeoutMs              = clusterredis.MaxTimeoutMs
	DefaultMaxRedirects       = clusterredis.DefaultMaxRedirects

	NotExecutedNo         = clusterredis.NotExecutedNo
	NotExecutedMoved      = clusterredis.NotExecutedMoved
	NotExecutedAsk        = clusterredis.NotExecutedAsk
	NotExecutedRetryLater = clusterredis.NotExecutedRetryLater
)

// 错误与 clusterredis 是同一个值,errors.Is 两边都认。
var (
	ErrTopologyChanged         = clusterredis.ErrTopologyChanged
	ErrUnsupportedClient       = clusterredis.ErrUnsupportedClient
	ErrUntaggedKey             = clusterredis.ErrUntaggedKey
	ErrCrossSlot               = clusterredis.ErrCrossSlot
	ErrNonIdempotentInPipeline = clusterredis.ErrNonIdempotentInPipeline
	ErrInvalidScanCursor       = clusterredis.ErrInvalidScanCursor
)

// 与域无关的函数原样转出。
var (
	MGetBySlot          = clusterredis.MGetBySlot
	ScanEachMaster      = clusterredis.ScanEachMaster
	ScanPage            = clusterredis.ScanPage
	ParseScanCursor     = clusterredis.ParseScanCursor
	SlotKeys            = clusterredis.SlotKeys
	CommandKeys         = clusterredis.CommandKeys
	NewPinned           = clusterredis.NewPinned
	MasterForKey        = clusterredis.MasterForKey
	ForEachMaster       = clusterredis.ForEachMaster
	ReloadTopology      = clusterredis.ReloadTopology
	NewNodeClient       = clusterredis.NewNodeClient
	ClassifyNotExecuted = clusterredis.ClassifyNotExecuted
)

// New 构造玩家域客户端,挂上玩家域守卫与不重放钩子。返回的客户端由调用方 Close。
func New(c Config) (redis.UniversalClient, error) {
	return clusterredis.New(c, clusterredis.PlayerDomainKeys)
}

// CheckKeys 按玩家域规则校验一组将要在同一条命令里出现的键。绕开守卫直接发命令的通道(NewPinned)在发送前自己调它。
func CheckKeys(keys []string) error {
	return clusterredis.PlayerDomainKeys.CheckKeys(keys)
}

// CheckCommand 按玩家域规则检查一条命令。不认识键位置的命令放行。
func CheckCommand(args []interface{}) error {
	return clusterredis.PlayerDomainKeys.CheckCommand(args)
}
