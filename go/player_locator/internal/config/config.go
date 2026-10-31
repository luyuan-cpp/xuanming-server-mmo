package config

import (
	"time"

	"github.com/zeromicro/go-zero/zrpc"

	"shared/playerredis"
)

type Config struct {
	zrpc.RpcServerConf
	RedisClient RedisConf `json:"RedisClient"` // 控制域:选主锁、就绪标记、代次、审计时间
	// PlayerRedis 是玩家域:会话、断线租约、gate 索引、分片登记,上线后是 Redis Cluster
	// (docs/design/redis-player-store-cluster.md §5.7、§5.9)。整段不写时指向 RedisClient 这个单实例。
	PlayerRedis     playerredis.Config `json:"PlayerRedis,optional"`
	Kafka           KafkaConf          `json:"Kafka"`
	Node            NodeConf           `json:"Node"`
	Registry        RegistryConf       `json:"Registry"`
	Lease           LeaseConf          `json:"Lease"`
	TableDir        string             `json:",default=../../generated/tables"`
	SceneManagerRpc zrpc.RpcClientConf `json:"SceneManagerRpc"` // SceneManager gRPC client (via etcd)

	// KillSwitchPrefix 是 RPC 级热关停规则在 etcd 里的前缀(shared/killswitch)。
	// 留空即用 killswitch.DefaultPrefix(/mmorpg/killswitch/)—— 这是安全默认值:
	// 前缀下没有任何 key 就是全部放行。只有多套环境共用一个 etcd 集群、需要各自
	// 独立的止血阀时才需要改它。
	KillSwitchPrefix string `json:"KillSwitchPrefix,optional"`
}

type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password"`
	DB       int    `json:"DB"`
	// PoolSize 是 go-redis 连接池上限(共享 Redis 连接总账,P0-E 第 3 批)。0 / 不写 = go-redis 缺省 10 × GOMAXPROCS,
	// 随 CPU limit 漂移;负数起服即失败(svc.NewServiceContext)。K8s 上 tools/scripts/k8s_deploy.ps1 从
	// etc/player_locator.yaml 镜像 RedisClient.PoolSize 并要求正整数,Redis 连接总账读同一个键。
	PoolSize int `json:"PoolSize,optional"`
}

type KafkaConf struct {
	Brokers []string `json:"Brokers"`
}

type NodeConf struct {
	ZoneId   uint32 `json:"ZoneId"`
	LeaseTTL int64  `json:"LeaseTTL"` // etcd lease TTL (seconds)
}

type RegistryConf struct {
	Etcd EtcdConf `json:"Etcd"`
}

type EtcdConf struct {
	Hosts       []string      `json:"Hosts"`
	DialTimeout time.Duration `json:"DialTimeout"`
}

type LeaseConf struct {
	DefaultTTLSeconds uint32        `json:"DefaultTTLSeconds"` // disconnect lease, default 30
	PollInterval      time.Duration `json:"PollInterval"`      // lease monitor poll interval, default 1s
	BatchSize         int           `json:"BatchSize"`         // max expired leases per tick, default 100
	// ScanRoundSeconds 是 LeaseMonitor 探测完持有的全部 slot 用多久,0 = DefaultLeaseScanRoundSeconds。全服的探测量是
	// 2 × 16384 / 本值 条 ZCOUNT 每秒,与副本数无关;租约到期后最迟晚这么久才被认领。事实源是代码默认值,dev yaml 与
	// K8s ConfigMap 都不写。
	ScanRoundSeconds int64 `json:"ScanRoundSeconds,optional"`
	// ReconcileIntervalSeconds 是会话对账扫描的周期(默认 60,0=默认,-1=关闭)。
	// 对账扫描兜住"gate 整机崩溃 → 断线回调不执行 → 会话永久 ONLINE"的缺口:
	// State==ONLINE 且 gate_instance_id 连续两轮不在 etcd 存活集内的会话,
	// 补投 DISCONNECTING + 租约,恢复「所有会话终点必经租约链」的闭环。
	//
	// 对账是「按 gate 实例建的会话索引」(docs/design/player-locator-gate-session-index.md):死 gate 清扫分给各副本,
	// 每拍只读自己那些 slot 的登记集与死 gate 的索引,不扫全键空间;写者检查与补建 / 审计由一个 leader 跑。
	// 这个周期同时是清扫的拍长、leader 的拍长、就绪标记租约与让位门槛的基准。
	// -1 关闭整个对账(索引照常随会话写入维护,但没有人处理死 gate、也没有人续就绪标记)。
	// 这一项与下面的 Reconcile 各项**必须全服一致**,否则行为取决于哪个 Pod 当选、哪个副本持段。
	ReconcileIntervalSeconds int64 `json:"ReconcileIntervalSeconds,optional"`
	// Reconcile 是索引对账与补建 / 审计扫描的限速与预算。事实源是代码默认值(Normalized):dev yaml 与 K8s
	// ConfigMap 都不写值,避免两处各写一份后漂移。
	Reconcile ReconcileConf `json:"Reconcile,optional"`
}

// DefaultLeaseScanRoundSeconds 是 Lease.ScanRoundSeconds 的默认值。推断值([I]),需要 V0 实测后校准。
const DefaultLeaseScanRoundSeconds = 16

// ScanRound 返回 LeaseMonitor 探测完持有 slot 的周期。
func (c LeaseConf) ScanRound() time.Duration {
	if c.ScanRoundSeconds <= 0 {
		return DefaultLeaseScanRoundSeconds * time.Second
	}
	return time.Duration(c.ScanRoundSeconds) * time.Second
}

// ReconcileConf 各字段零值取默认值。默认值都是推断值([I]),需要 V0 实测后校准。
// 限速与预算都按副本计:死 gate 清扫分给了各副本,全服的量 = 持段副本数 × 这些值。
type ReconcileConf struct {
	// AuditIntervalSeconds 是索引就绪后定期审计(全键扫描补漏成员 + 清各 gate 索引里的脏成员)的周期。
	// 0 = 86400;-1 = 就绪后不再定期审计(未就绪时的补建不受影响)。审计时机持久化在 Redis,换主不重置。
	AuditIntervalSeconds int64 `json:"AuditIntervalSeconds,optional"`
	// ScanCount 是补建 / 审计扫描 SCAN 与 SSCAN 每页的 COUNT 提示。0 = 1000。
	ScanCount int64 `json:"ScanCount,optional"`
	// ScanPagesPerSecond 是补建 / 审计扫描每秒最多发出的页数:一页是一次 SCAN 的结果、ScanCount 把索引键,或一把大索引键的续页。
	// 0 = 200;负数 = 不限速(只用于本机与单测,生产不要用:会拉高 Redis 延迟)。
	ScanPagesPerSecond int `json:"ScanPagesPerSecond,optional"`
	// MaxTransitionsPerSecond 是对账把死 gate 上的 ONLINE 会话转成 DISCONNECTING 的速率上限。
	// 0 = 1000;负数 = 不限速。
	// 注意:每拍只用半个拍长去转换(见 DeadGateMembersPerRound),大面积 gate 故障时每个副本的**持续**速率约为
	// 本值 / 2,估恢复时间按「持段副本数 × 这个值」算(设计文档 §7)。
	MaxTransitionsPerSecond int `json:"MaxTransitionsPerSecond,optional"`
	// DeadGateMembersPerRound 是一拍里最多检查的死 gate 索引成员数,超出的留到下一拍继续,
	// 保证大面积 gate 故障时每拍仍能按时完成、新死的 gate 照常被识别。0 = 30000。
	// 实际预算还会被 MaxTransitionsPerSecond × 半个拍长截住,限速调低时一拍不会超时。
	DeadGateMembersPerRound int `json:"DeadGateMembersPerRound,optional"`
}

// Reconcile 的默认值。
const (
	DefaultReconcileAuditIntervalSeconds    int64 = 86400
	DefaultReconcileScanCount               int64 = 1000
	DefaultReconcileScanPagesPerSecond            = 200
	DefaultReconcileMaxTransitionsPerSecond       = 1000
	DefaultReconcileDeadGateMembersPerRound       = 30000
)

// Normalized 把零值换成默认值;负数按各字段注释的语义原样保留(AuditIntervalSeconds 只认 -1 为关闭,
// 其余负数也按关闭处理;ScanCount 不接受负数,按默认值处理)。
func (c ReconcileConf) Normalized() ReconcileConf {
	if c.AuditIntervalSeconds == 0 {
		c.AuditIntervalSeconds = DefaultReconcileAuditIntervalSeconds
	} else if c.AuditIntervalSeconds < 0 {
		c.AuditIntervalSeconds = -1
	}
	if c.ScanCount <= 0 {
		c.ScanCount = DefaultReconcileScanCount
	}
	if c.ScanPagesPerSecond == 0 {
		c.ScanPagesPerSecond = DefaultReconcileScanPagesPerSecond
	}
	if c.MaxTransitionsPerSecond == 0 {
		c.MaxTransitionsPerSecond = DefaultReconcileMaxTransitionsPerSecond
	}
	if c.DeadGateMembersPerRound <= 0 {
		c.DeadGateMembersPerRound = DefaultReconcileDeadGateMembersPerRound
	}
	return c
}

var AppConfig Config
