package config

import (
	"strconv"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	NodeID string `json:",optional"` // deprecated: kept for backward compat
	Kafka  struct {
		Brokers []string
	}

	// AllowUnsafeCrossNodeHandoff 仅用于开发环境临时复现旧流程。
	// 默认 false：跨节点切场景，以及已有位置记录时的跨区重定向，必须
	// 在具备持久化交接屏障前拒绝，避免新节点加载到旧节点尚未落盘的状态。
	AllowUnsafeCrossNodeHandoff bool `json:",default=false"`

	// ZoneId: zone identifier for this SceneManager instance.
	// Used for C++ convention etcd registration so Scene nodes can discover this service.
	ZoneId uint32 `json:",default=1"`

	// LeaseTTL: etcd lease TTL in seconds for node registration keepalive.
	LeaseTTL int64 `json:",default=60"`

	// LeaderLockTTLSeconds:多副本选主锁的 TTL(秒)。变更类后台循环
	// (补频道 / rebalance / 孤儿与空闲副本清理 / 自动扩缩容 / Agones 对账)
	// 只在领导者上执行;领导者失联后,其余副本最迟 ~TTL 内接管。
	// 心跳续期间隔 = TTL/3。见 internal/logic/leader_gate.go。
	LeaderLockTTLSeconds int64 `json:",default=30"`

	// LeaderLockKey:选主锁的 Redis key,留空用默认 scene_manager:leader:lock。
	// 服务本身 zone-agnostic,所有副本共用同一把锁。
	LeaderLockKey string `json:",optional"`

	// LeaderEligible:本副本是否参与选主。金丝雀副本必须设 false ——
	// 金丝雀靠 etcd 发现天然按实例比例分到 RPC 数据面流量,这正是想灰度的
	// 部分;但领导者只有一个,金丝雀一旦当选,新版本的**编排**逻辑
	// (补频道 / rebalance / 伸缩 / 清理)会作用于全部 zone,爆炸半径失控。
	// 注意:所有副本都设 false 会导致无人编排(scene_manager_is_leader
	// 全体之和为 0,超过锁 TTL 应告警)。默认 true。
	LeaderEligible bool `json:",default=true"`

	// TableDir: directory containing exported table data files (JSON or binary pb).
	// Default assumes the service runs from go/scene_manager/ with repo root two levels up.
	TableDir string `json:",default=../../generated/tables"`

	// UseBinary: if true, load .pb (proto binary) table files; otherwise load .json.
	UseBinary bool `json:",default=false"`

	// WorldChannelCount: default number of channels per world scene.
	// Each channel is a separate ECS scene entity sharing the same config_id.
	// Players are assigned to the least-loaded channel. Default 1 (no split).
	WorldChannelCount int `json:",default=1"`

	// WorldChannelCountByConfId: per-confId override of WorldChannelCount.
	// Use for hot main cities / starter zones that need more copies than the
	// default, or niche maps that only need one. A value of 0 falls back to
	// WorldChannelCount. Example in yaml:
	//   WorldChannelCountByConfId:
	//     1001: 4      # big city — 4 channels
	//     1010: 1      # tutorial — 1 channel
	WorldChannelCountByConfId map[string]int `json:",optional"`

	// StrictNodeTypeSeparation: when true, main-world creation is only routed
	// to nodes whose scene_node_type is kMainSceneNode/kMainSceneCrossNode,
	// and instance creation is only routed to kSceneNode/kSceneSceneCrossNode.
	// Default true — production deployments should dedicate nodes by role.
	// Set false for dev / single-node deployments where one process hosts
	// both kinds (the filter falls back to the full zone pool).
	// Mirror co-location bypasses this filter regardless of the flag: the
	// gain from reusing the source scene's resident map/AI data beats type
	// purity, and operators opt in via source_scene_id explicitly.
	StrictNodeTypeSeparation bool `json:",default=true"`

	// NodeLoadWeightSceneCount / NodeLoadWeightPlayerCount: weights combined
	// into the per-node load score stored in scene_nodes:zone:{zone}:load.
	// score = α·scene_count + β·player_count. Defaults α=1.0, β=0.01 — one
	// scene entity costs the same as ~100 players, reflecting the fact that
	// an empty scene still burns CPU on ticks/AOI scaffolding.
	// Tune β higher if your scenes are cheap and players are expensive
	// (e.g. heavy physics or per-player AI).
	NodeLoadWeightSceneCount  float64 `json:",default=1.0"`
	NodeLoadWeightPlayerCount float64 `json:",default=0.01"`

	// InstanceIdleTimeoutSeconds: seconds an empty instance is kept alive
	// before being auto-destroyed. 0 = never auto-destroy (default).
	InstanceIdleTimeoutSeconds int64 `json:",default=300"`

	// MirrorIdleTimeoutSeconds: seconds an empty *mirror* instance is kept
	// alive before auto-destroy. Mirrors are ephemeral copies of a world
	// scene — NPCs/state are re-initialized on every re-entry — so keeping
	// them around empty is pure waste. 0 = fall back to
	// InstanceIdleTimeoutSeconds (treat mirrors like normal instances).
	// Default 30s: absorbs brief disconnects / loading screens, but doesn't
	// linger. Set to a small positive number to opt into aggressive cleanup.
	MirrorIdleTimeoutSeconds int64 `json:",default=30"`

	// InstanceCheckIntervalSeconds: how often the lifecycle manager scans
	// for idle instances. Default 30s.
	InstanceCheckIntervalSeconds int64 `json:",default=30"`

	// KafkaWriteTimeoutSeconds: writer-side timeout for the single synchronous
	// Gate command produce attempt. EnterScene only commits success after
	// RequireOne broker ACK; the caller context does not cancel an already
	// queued kafka-go batch because that can return before delivery actually ends.
	// Default 5 seconds.
	KafkaWriteTimeoutSeconds int64 `json:",default=5"`

	// MirrorSourceNodeLoadCap: soft cap on the source node's scene_count
	// above which mirror requests fall back to GetBestNode instead of
	// co-locating with the source scene. 0 disables the cap (always
	// co-locate). Default 0 = no cap; operators raise it when one world
	// tends to spawn many mirrors.
	MirrorSourceNodeLoadCap int64 `json:",default=0"`

	// MirrorDedupBySource: when true, CreateScene with source_scene_id > 0
	// returns an EXISTING mirror of that source instead of allocating a new
	// one (if any). Off by default because the typical mirror use case is
	// per-player phasing where independent copies are intentional. Turn it
	// on for "shared instance" semantics (e.g. raid lockouts, world bosses
	// where the whole zone shares one mirror copy). Selection is arbitrary
	// when multiple mirrors exist; do not enable this if your gameplay
	// requires deterministic mirror identity per request.
	MirrorDedupBySource bool `json:",default=false"`

	// GateTokenSecret: HMAC-SHA256 secret shared with Gate nodes for signing
	// connection tokens during cross-zone redirect.
	GateTokenSecret string `json:",optional"`

	// MetricsListenAddr: host:port to serve Prometheus /metrics. Empty
	// disables the scrape endpoint (default). Typical prod value is
	// ":9150" — keep it off the gRPC port and scrape it via ServiceMonitor.
	MetricsListenAddr string `json:",optional"`

	// MaxRebalanceMigrationsPerTick bounds the number of world-channel
	// migrations executed per world-node-set change event. Rebalancing
	// moves empty channels (player_count == 0) toward a more uniform hash
	// distribution; large values can thrash the cluster on scale events,
	// small values slow convergence. Default 10 balances responsiveness
	// with safety. Set to 0 to disable proactive rebalancing (channels on
	// dead nodes are still re-homed by GetBestWorldChannel on demand).
	MaxRebalanceMigrationsPerTick int `json:",default=10"`

	// RebalanceCheckIntervalSeconds: how often the LoadReporter runs a
	// periodic rebalance pass in addition to event-triggered passes.
	// Event-driven rebalance catches join/leave of world-hosting nodes,
	// but a channel can become opportunistic-migratable after its players
	// drain with no corresponding etcd event. The ticker closes that gap.
	// Default 300s (5 min). Set to 0 to disable — event-driven only.
	RebalanceCheckIntervalSeconds int64 `json:",default=300"`

	// Agones: 高密度 GameServer 容量预占。默认整体关闭 —— 不配就是接入
	// Agones 之前的行为(按 Redis 负载分数挑节点)。
	// 设计文档:docs/design/agones-scene-node-high-density.md。
	Agones AgonesConfig `json:",optional"`

	// WorldAutoscale: 大世界频道按人数自动扩缩容。默认关闭。
	// 设计文档:docs/design/world-channel-autoscale.md。
	WorldAutoscale WorldAutoscaleConfig `json:",optional"`

	// CleanupOrphanChannelsOnStartup: when true (default), SceneManager
	// scans Redis on fullSync for world_channels:* sets whose confId is
	// not in World.json and deletes them. This removes drift left by
	// designers dropping a map from the table without a clean tear-down.
	// Set to false in environments where you migrate maps manually or
	// share a Redis cluster across incompatible World.json versions.
	// NB: cleanup is refused when worldConfIds() returns empty (a
	// defensive check against a table-load failure nuking prod data).
	CleanupOrphanChannelsOnStartup bool `json:",default=true"`
}

// AgonesConfig 控制 SceneManager 是否通过 Agones GameServerAllocation
// 预占房间容量。
//
// 两个开关是分开的,不要合并:
//   - Enabled=false        完全不碰 Agones,选节点走原来的 Redis 负载分数。
//   - Enabled=true 且 HighDensityEnabled=false
//     走 GSA 选 GameServer,但不使用 Counters and Lists —— 一个进程一次
//     只接一个房间。可用于在 beta 能力打开之前先验证分配链路。
//   - Enabled=true 且 HighDensityEnabled=true
//     完整高密度:rooms Counter 参与筛选并在 GSA 内原子 +1。
//
// Counters and Lists 在 Agones 里是 **beta** 能力,需要集群侧显式打开
// FeatureGate。所以 HighDensityEnabled 默认 false,由运维在确认 Agones
// 版本与 FeatureGate 之后再开。
type AgonesConfig struct {
	Enabled            bool `json:",default=false"`
	HighDensityEnabled bool `json:",default=false"`

	// Namespace 是 Fleet 所在命名空间。留空则用 POD_NAMESPACE 环境变量,
	// 再不行就用 "default"。
	Namespace string `json:",optional"`

	// RoomCapacity 只用于**生成部署模板时**的参考值,SceneManager 运行时
	// 不依赖它(容量是 GameServer 对象上的 status.counters.rooms.capacity)。
	// 这里保留一个字段是为了让配置与压测结论有个落点。
	//
	// 注意:不要硬编码"一个 Scene Node 能承载多少房间"。C++ Scene Node 是
	// 单 EventLoop,实际容量必须按帧耗时、AOI、玩家数和内存压测确定
	// (压测口径见 CLAUDE.md §6)。
	RoomCapacity int64 `json:",default=0"`

	// BuildLabel 为空表示分配时不按 mmorpg.io/build 过滤。
	// 滚动升级期间通常留空,否则新旧版本会互相看不见。
	BuildLabel string `json:",optional"`

	// CounterRollbackRetries / CounterRollbackBackoffMs 控制回滚 rooms 计数
	// 时的乐观并发重试(409 冲突是正常现象)。
	CounterRollbackRetries   int   `json:",default=5"`
	CounterRollbackBackoffMs int64 `json:",default=100"`

	// RequestTimeoutMs 单次 K8s API 调用超时。
	RequestTimeoutMs int64 `json:",default=5000"`

	// ReconcileIntervalSeconds: 周期性比对 Agones rooms 计数、Redis Scene
	// 映射与节点上报数量。0 = 关闭。
	// 第一版发现不一致**只告警和记指标**,不自动覆盖计数 —— 证据不完整时
	// 自动"修正"很可能把对的一方改错。
	ReconcileIntervalSeconds int64 `json:",default=60"`
}

// WorldAutoscaleConfig 控制大世界频道按人数自动扩缩容。
//
// 口径全部**按频道**算(`instance:{sceneId}:player_count`),不是按进程。
// 进程数量由 Agones FleetAutoscaler 管,两件事分开。
type WorldAutoscaleConfig struct {
	Enabled bool `json:",default=false"`

	// CheckIntervalSeconds: 多久跑一轮扩缩容决策。
	CheckIntervalSeconds int64 `json:",default=30"`

	// ScaleOutPlayerThreshold: 该地图**所有**频道人数都 >= 这个值才扩容。
	//
	// 要求"所有"而不是"任一":负载不均时(刚扩出来的新频道是空的、老频道
	// 还满着),按"任一"会连续触发扩容,扩出一堆空频道。
	ScaleOutPlayerThreshold int64 `json:",default=2000"`

	// ScaleInPlayerThreshold: 某频道人数 < 这个值就把它排空并销毁,
	// 玩家被强制改派到同图其它频道。
	//
	// 与扩容线之间必须留足够宽的带(默认 100 vs 2000),否则缩容把人并过去
	// 立刻触发扩容,扩容又让某个频道掉到线下 —— 自激振荡,玩家被反复改派。
	ScaleInPlayerThreshold int64 `json:",default=100"`

	// MinChannelsPerMap: 每个大世界地图保留的最小频道数。
	// **硬下限是 1**,配成 0 也会被钳回 1:缩到 0 会让该地图无法进入。
	MinChannelsPerMap int `json:",default=1"`

	// MaxChannelsPerMap: 每个大世界地图的频道数上限,防止异常流量把频道
	// 扩到失控。0 = 不限(不推荐)。到顶时只记 ERROR + 指标,不再扩。
	MaxChannelsPerMap int `json:",default=16"`

	// CooldownSeconds: 一次伸缩之后该 (zone, map) 的静默期。
	// 伸缩的效果(玩家重新分布)需要时间体现,不等就会连续误判。
	CooldownSeconds int64 `json:",default=120"`

	// DrainTimeoutSeconds: 排空标记的 TTL。进程在排空中途挂掉时标记会过期,
	// 下一轮 sweep 重新接手,不会留下永久"半死"频道。
	DrainTimeoutSeconds int64 `json:",default=300"`
}

// ChannelCountFor returns the effective world-channel count for a confId,
// honoring per-confId overrides, clamped to at least 1.
func (c *Config) ChannelCountFor(confId uint64) int {
	if c.WorldChannelCountByConfId != nil {
		for key, value := range c.WorldChannelCountByConfId {
			if value <= 0 {
				continue
			}
			parsed, err := strconv.ParseUint(key, 10, 64)
			if err == nil && parsed == confId {
				return value
			}
		}
	}
	if c.WorldChannelCount < 1 {
		return 1
	}
	return c.WorldChannelCount
}
