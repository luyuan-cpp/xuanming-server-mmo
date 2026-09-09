package config

import (
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	MappingRedis     redis.RedisConf
	Regions          []RegionConfig
	DevRedis         DevRedisConfig `json:",optional"`
	PlayerLockTTLSec int            `json:",default=3"`

	// Snapshot MySQL (for rollback persistence)
	SnapshotMySQL SnapshotMySQLConfig `json:",optional"`

	// Login admin gRPC client (for orphan account cleanup during rollback)
	LoginAdminRpc zrpc.RpcClientConf `json:",optional"`

	// AdminToken 是运维专用 RPC(目前只有 RemapHomeZoneForMerge —— 一次调用能改写
	// 全服玩家的归属 zone)的共享口令,调用方放在 gRPC metadata 的 `x-admin-token` 里。
	//
	// **空 = 该 RPC 整体停用**(fail closed),不是"免鉴权"。理由:data_service 在集群
	// 内网是无鉴权可达的,任何一个能拨到它的进程都能调 remap;默认敞开意味着一次误配
	// 或一个跑错环境的脚本就能把全服玩家送进别的 zone,而这个操作没有反向操作
	// (改回去要先知道谁原本在哪个 zone,而那份信息刚被覆盖掉)。所以默认值只能是
	// "不能用",启用是一次显式的运维动作:合服维护窗口里临时下发,窗口结束就撤掉。
	//
	// 不放进 yaml 明文的话可以用 k8s Secret 挂成环境变量再由 ConfigMap 引用;
	// 无论哪种,它都只与 remap 这一个接口有关,泄露的后果止于"能在已封锁的 zone 上
	// 跑一次 remap"——闸门标记仍是第二道锁(见 RemapHomeZoneForMerge 的前置条件)。
	AdminToken string `json:",optional"`

	// Prometheus /metrics HTTP listen address (host:port). Empty = disabled.
	// Scraped by Prometheus alongside scene_manager. Exposes per-RPC outcome /
	// latency, per-player lock contention, version mismatch, rollback counts.
	// See go/data_service/internal/metrics/metrics.go for the metric list.
	MetricsListenAddr string `json:",optional"`

	// Schema 控制全局库(SnapshotMySQL)四张表的建表/补列路径,见 internal/store/schema.go。
	// 整段可缺失,缺失语义见 SchemaConfig.AutoMigrate 与 ShouldAutoMigrate。
	Schema SchemaConfig `json:",optional"`

	// Kafka 是 C++ scene 产出的 transaction_log_topic / player_snapshot_topic 的落库消费者
	// 配置。Brokers 为空 = 不消费(本地无 Kafka 时的合法形态);消费者启动失败不会拖死
	// data_service(Load/Save 是热路径),只记日志并每 30s 后台重试。
	Kafka KafkaConfig `json:",optional"`

	// IdSegment 控制号段表 id_segment 的**行**生命周期(表结构归 Schema 管),
	// 见 internal/store/id_segment_store.go 与设计 §7.5 第 7 条。整段可缺失,缺失 = 生产安全值。
	IdSegment IdSegmentConfig `json:",optional"`

	// ZoneId 只用于 C++ 约定的 etcd 注册路径
	// (DataServiceNodeService.rpc/zone/<ZoneId>/node_type/26/node_id/<N>,见 internal/noderegistry)。
	// data_service 按 zone namespace 各部署一份,但 DataServiceNodeService **不是** zone-scoped
	// 节点类型(C++ 发现侧只有 Gate/Scene/Login/PlayerLocator 按 zone 过滤),所以填哪个 zone
	// 都能被任何 scene 发现;写成 Pod 所在的 zone 只是为了排障时一眼对得上。
	// optional、默认 0:整段缺失也能注册(与 scene_manager / match 的 default=1 不同 —— 那两个
	// 靠它区分本地 dev-start-zones 每 zone 起的一份)。k8s ConfigMap 写部署 zone(${CurrentZoneId})。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL 是 C++ 约定注册那把 etcd 租约的 TTL(秒),keepalive 按它续租;
	// 与 scene_manager / match 同形(顶层键、默认 60)。租约丢失 = C++ 侧看到 data_service
	// 消失,scene 的号段续段会失败(手里两段能顶 10~20 分钟),noderegistry 会 CAS 重夺 / 重分配 node_id。
	LeaseTTL int64 `json:",default=60"`
}

// IdSegmentConfig 号段行的种植策略。
//
// 背景(设计 §7.5 第 7 条):id_segment 里每一行都是一个永久身份的发号水位。行没了
// (全局库被 drop 重建、或从旧备份恢复)而消费表(player_database / guild / 玩家 blob 里的
// 物品)还留着已发出的号,下一次发号就会从 1 重来,login 的 INSERT ... ON DUPLICATE KEY
// UPDATE 会**静默覆盖**别人的角色行。所以行只能由迁移显式创建,运行期"缺行就补种"仅限 dev。
type IdSegmentConfig struct {
	// AllowAutoSeed=true:AllocateIdSegment 遇到表里没有的 biz_tag 时自动种一行从 1 起
	// (今天的行为,只给 dev 用);false(生产):返回 ErrCodeIdSegmentUnknownTag,不写任何行,
	// 行必须由 `data_service -migrate` / AutoMigrate 按 BootstrapTags 创建。
	//
	// 这里可以放心用裸 bool:go-zero 不下钻一个"整段 optional 且未出现"的嵌套结构,
	// 里面的 default 标签一个都不会回填 —— 但本键的安全值恰好就是零值 false,块缺失
	// 得到的正是生产语义(与 SchemaConfig.AutoMigrate 需要 *bool 的原因相反)。
	AllowAutoSeed bool `json:",optional"`

	// BootstrapTags 迁移时用 INSERT IGNORE 预建的 biz_tag 清单(max_id=1, step=100,
	// version=0),幂等、绝不降低已有行的 max_id。为空 = DefaultIdSegmentBootstrapTags
	// (原因同上:块缺失时 default 标签不生效,只能在 EffectiveBootstrapTags 里兜)。
	// 新增一种永久身份 = 在这里加一个 tag,而不是靠运行期补种。
	BootstrapTags []string `json:",optional"`
}

// DefaultIdSegmentBootstrapTags 是设计 §6.4 / §7.5 第 1 条里走号段的五种永久身份:
// player / guild(Go login / guild)与 item / txlog / snapshot(C++ scene)。
// 与 store.DefaultIdSegmentBootstrapTags 同一份清单(config 不能 import store)。
var DefaultIdSegmentBootstrapTags = []string{"player", "guild", "item", "txlog", "snapshot"}

// EffectiveBootstrapTags 是迁移路径实际预建的 tag 清单:没配 = 默认五种。
// 返回副本,免得调用方改到包级默认值。
func (c IdSegmentConfig) EffectiveBootstrapTags() []string {
	src := c.BootstrapTags
	if len(src) == 0 {
		src = DefaultIdSegmentBootstrapTags
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// SchemaConfig 建表策略。
type SchemaConfig struct {
	// AutoMigrate 缺省(nil)= true:启动时用 proto2mysql 按 proto 定义对四张表逐张
	// CreateOrUpdateTable(建表 / 补列 / 补主键),并预建 id_segment。
	//
	// 生产必须显式设 false:多副本同时启动会对同一张表并发 ALTER,MDL 阻塞会让所有
	// GM 回滚/流水查询停摆;改为部署阶段显式跑一次 `data_service -f <yaml> -migrate`
	// (同一段代码、单进程、跑完即退)。设 false 时启动路径不碰任何 DDL。
	//
	// 为什么是 *bool 而不是 `bool json:",default=true"`:go-zero 的 mapping **不下钻**
	// 一个"整段 optional 且未出现"的嵌套结构,里面的 default 标签一个都不会回填
	// (与 go/db 的 BlobGuardConfig、login 的 IdSegment.Enabled 同一个坑)。用裸 bool
	// 时,一份没有 `Schema:` 段的 yaml 会得到 AutoMigrate=false —— 启动路径一条 DDL 都
	// 不跑,三个 store 打在一个没有表的库上,rollback / txlog / AllocateIdSegment 每一次
	// 调用都在运行期炸;而在收口手写 DDL 之前,store 自带的 CREATE TABLE IF NOT EXISTS
	// 还兜着这一层。指针把"没写"与"显式写了 false"区分开:nil = 用安全默认。
	//
	// 键名保持 AutoMigrate(不改成反向的 SkipAutoMigrate):
	// tools/scripts/k8s_deploy.ps1 的 data-service ConfigMap 会写死
	// `Schema: AutoMigrate: <dev 取服务 yaml / staging|prod 固定 false>`,改名会让那份
	// 生产配置变成一个被静默忽略的未知键,反而把生产切回启动期 DDL。
	AutoMigrate *bool `json:",optional"`
}

// ShouldAutoMigrate 是启动路径是否跑 DDL 的唯一判据:没配 = 建表(安全默认)。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
}

// KafkaConfig 两条落库消费者的参数。分区数是不可变契约(kafkautil.EnsureTopics
// 会拒绝与 broker 现状不一致的值),现在就定够:transaction_log 6、snapshot 3。
type KafkaConfig struct {
	Brokers []string `json:",optional"`

	// TopicGeneration 是分区契约的**代号**,进 topic 名(见 EffectiveTransactionLogTopic)。
	// 与 go/db、go/login 的 Kafka.TopicGeneration 同一个机制,但这里**连 1 也带后缀**。
	//
	// 为什么必须带:broker 是 auto.create.topics.enable=true + num.partitions=1
	// (deploy/docker-compose.yml KAFKA_NUM_PARTITIONS=1),而 C++ scene 一发消息就把
	// 裸名字的 topic 自动建成 1 分区。EnsureTopics 拿 6/3 的契约去比,永远得到
	// "partition contract mismatch",两条消费者一条都起不来,唯一症状是每 30s 一条
	// Error 日志 —— 流水/快照全被保留期吃掉。带代号的名字与自动建出来的那两个裸名
	// topic 互不相干,第一次就能按契约建出 6/3 分区。
	//
	// 分区数要改 = 把这个数 +1(换一批新 topic、老 topic 排空后删),**绝不**原地扩分区:
	// 扩分区会重映射 key=player_id 的哈希,同一玩家的流水顺序就断了。
	TopicGeneration uint32 `json:",default=1"`

	// TransactionLogTopic 是**基名**;实际使用的名字是 EffectiveTransactionLogTopic()。
	// 基名必须与 C++ 生产者的 topic 常量一致(全局单 topic,key=player_id)。
	TransactionLogTopic         string `json:",default=transaction_log_topic"`
	TransactionLogPartitions    int32  `json:",default=6"`
	TransactionLogConsumerGroup string `json:",default=data_service-transaction-log"`

	// SnapshotTopic 同上;单条消息几百 KB,消费者不攒批。
	SnapshotTopic         string `json:",default=player_snapshot_topic"`
	SnapshotPartitions    int32  `json:",default=3"`
	SnapshotConsumerGroup string `json:",default=data_service-player-snapshot"`

	// RetentionMs 两个 topic 的保留期,默认 30 天。审计数据宁可积压不许丢:
	// 消费者停下(DB 长时间不可用)期间的消息要在这个窗口内被补消费掉。
	RetentionMs int64 `json:",default=2592000000"`
}

// EffectiveTransactionLogTopic 是消费者与 EnsureTopics 真正使用的 topic 名。
func (k KafkaConfig) EffectiveTransactionLogTopic() string {
	return topicForGeneration(k.TransactionLogTopic, k.TopicGeneration)
}

// EffectiveSnapshotTopic 同上。
func (k KafkaConfig) EffectiveSnapshotTopic() string {
	return topicForGeneration(k.SnapshotTopic, k.TopicGeneration)
}

// topicForGeneration 拼 `<base>_g<N>`。base 为空时返回空,交给 kafka.Start 的 validate
// 报错,而不是拼出一个只有后缀的名字。generation=0 只可能来自"整段 Kafka 未出现"
// (go-zero 不回填未出现结构里的 default),此时按第一代 1 处理:名字保持规范。
func topicForGeneration(base string, generation uint32) string {
	if base == "" {
		return ""
	}
	if generation == 0 {
		generation = 1
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}

type DevRedisConfig struct {
	Host     string `json:",optional"`
	Password string `json:",optional"`
	DB       int    `json:",default=0"`
}

type RegionConfig struct {
	Id    uint32
	Zones []uint32
	Redis RedisClusterConfig
}

type RedisClusterConfig struct {
	Addrs    []string
	Password string `json:",optional"`
}

type SnapshotMySQLConfig struct {
	Host        string `json:",default=127.0.0.1:3306"`
	User        string `json:",default=appuser"`
	Password    string `json:",default=apppass123"`
	DBName      string `json:",default=testdb"`
	MaxOpenConn int    `json:",default=5"`
	MaxIdleConn int    `json:",default=2"`
}
