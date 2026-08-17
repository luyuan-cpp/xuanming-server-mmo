package config

import (
	"fmt"

	"github.com/zeromicro/go-zero/zrpc"
)

// Config is the global configuration.
type Config struct {
	zrpc.RpcServerConf
	ZoneId       uint32       `json:"ZoneId"`
	ServerConfig ServerConfig `json:"ServerConfig"`

	// MetricsListenAddr is the bind address for the Prometheus /metrics
	// HTTP endpoint. Leave empty to disable (production default; opt-in
	// via deploy config). Dev default is set in etc/db.yaml so stress
	// runs can scrape per-task stage histograms alongside login :9101
	// and scene_manager :9150.
	MetricsListenAddr string `json:"MetricsListenAddr,optional"`
}

// ServerConfig holds core service settings.
type ServerConfig struct {
	Database    DatabaseConfig  `json:"Database"`
	RedisClient RedisConfig     `json:"RedisClient"`
	JsonPath    string          `json:"JsonPath"`
	Kafka       KafkaConfig     `json:"Kafka"`
	BlobGuard   BlobGuardConfig `json:"BlobGuard,optional"`
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Hosts       string `json:"Hosts"`
	User        string `json:"User"`
	Passwd      string `json:"Passwd"`
	DBName      string `json:"DBName,optional"` // Derived from ZoneId at startup
	MaxOpenConn int    `json:"MaxOpenConn"`
	MaxIdleConn int    `json:"MaxIdleConn"`
	Net         string `json:"Net"`

	// AllowedDatabases 是**外部注入**的库名白名单。
	//
	// 为什么必须外部注入:DBName 由 ZoneId 现拼(ZoneDBName),而 ZoneId 本身
	// 就是最容易填错的那个字段 —— 填错一位(1 → 11)时旧实现会在生产实例上
	// 静默 CREATE DATABASE + 建全套表,把玩家数据写进一个无人知晓的新库,
	// 全程没有任何报错。用「ZoneId 推导出来的名字」自己校验自己是无效的,
	// 所以白名单必须来自部署侧(env / ConfigMap 文件 / yaml),与 ZoneId 无关。
	//
	// 解析优先级:环境变量 DB_ALLOWED_DATABASES > AllowedDatabasesFile > 本字段。
	AllowedDatabases []string `json:"AllowedDatabases,optional"`

	// AllowedDatabasesFile 指向一份「每行一个库名」的白名单文件(K8s ConfigMap
	// 挂载场景)。`#` 开头与空行忽略。
	AllowedDatabasesFile string `json:"AllowedDatabasesFile,optional"`

	// AllowlistEnforcement 控制「白名单三个来源全为空」时的行为:
	//   ""/"strict" —— 拒启(默认,生产语义)
	//   "warn"      —— 打一条显眼的 WARN 后放行,**仅限本地/dev**
	//
	// 刻意不复用 go-zero 的 Mode 做判据:db.yaml 里没写 Mode 时 go-zero 默认
	// 是 "pro",本地脚本起的服务会被当成生产,踩坑时完全看不出因果。
	AllowlistEnforcement string `json:"AllowlistEnforcement,optional,options=|strict|warn"`

	// AutoCreateDatabase 允许启动路径在库不存在(MySQL 1049)时自动建库。
	// 默认 false。生产由部署阶段的迁移 Job 预建;只有 dev/test 模式下本开关
	// 才会被采纳(见 proto_sql.ddlPolicy)。
	AutoCreateDatabase bool `json:"AutoCreateDatabase,optional"`

	// AutoMigrateSchema 允许启动路径逐张跑 CreateOrUpdateTable(建表 / 补列)。
	// 默认 false。历史行为等价于 true —— 多 zone × 多副本同时启动会对同一张
	// 大表并发 ALTER,MDL 阻塞会让整个 zone 的 db_task 消费停摆。生产改为由
	// `go run ./cmd/migrate -command up` 在部署阶段单独执行。
	// 同样只有 dev/test 模式下本开关才会被采纳。
	AutoMigrateSchema bool `json:"AutoMigrateSchema,optional"`

	// Migration 是迁移入口(cmd/migrate)的超时与锁参数,业务服务不读。
	Migration MigrationConfig `json:"Migration,optional"`
}

// MigrationConfig 是 DDL 迁移的安全阀参数。
//
// 三层保护缺一不可:
//  1. AdvisoryLockSeconds —— GET_LOCK 跨实例互斥,杜绝多 zone/多副本并发 ALTER;
//  2. LockWaitSeconds / InnodbLockWaitSeconds —— 会话级 MDL / 行锁等待上限,
//     让 DDL 撞上长事务时**快速失败**而不是把整表锁死;
//  3. StatementTimeoutSeconds —— 每条语句的硬超时,超时后从旁路连接 KILL QUERY,
//     防止服务端还在跑而工具已经退出。
type MigrationConfig struct {
	LockWaitSeconds         int `json:"LockWaitSeconds,optional"`
	InnodbLockWaitSeconds   int `json:"InnodbLockWaitSeconds,optional"`
	StatementTimeoutSeconds int `json:"StatementTimeoutSeconds,optional"`
	AdvisoryLockSeconds     int `json:"AdvisoryLockSeconds,optional"`
}

// BlobGuardConfig 是 db_task 落库路径上 pb blob 列的字节上限(大字段三档闸)。
//
// 档位语义:
//   - 正常          —— 只记 histogram,零日志;
//   - >= WarnRatio  —— WARN 日志 + db_blob_guard_total{level="warn"} 计数;
//   - >  上限       —— db_blob_guard_total{level="reject"} 计数,Enforce 时拒写。
//
// 上限**按设计期望定,不是按 MEDIUMBLOB 的 16MB 定**:玩家单个组件 blob 的
// 设计量级是个位数 KB,16MB 的闸等于没设 —— 等它触发时行早就大到把 Save
// 的网络往返和 Redis 写回一起拖垮了。
type BlobGuardConfig struct {
	// MaxColumnBytes 单个 blob 列的**入库字节**上限(0 = 用默认值)。
	// 注意 proto2mysql 把子消息 base64 后再入库,所以入库字节 ≈ 序列化字节 × 4/3,
	// 本字段量的是 base64 之后的那个数(也就是真正占 MEDIUMBLOB 的数)。
	MaxColumnBytes int64 `json:"MaxColumnBytes,optional"`

	// MaxRowBytes 单行所有 blob 列入库字节之和的上限(0 = 用默认值)。
	MaxRowBytes int64 `json:"MaxRowBytes,optional"`

	// WarnRatio 预警水位(0 = 用默认值 0.8)。
	WarnRatio float64 `json:"WarnRatio,optional"`

	// ReportOnly 把超限从「拒写」降级为「只报警」。默认 false = fail-closed 拒写。
	// 之所以用反向开关而不是 Enforce:go-zero 对「整段 optional 且未出现」的
	// 嵌套结构不会回填 default 标签,零值必须直接就是我们要的安全语义。
	// 灰度期可显式设 true 先跑 report-only,盯 histogram 的 p99 定完上限再关掉。
	ReportOnly bool `json:"ReportOnly,optional"`

	// PerTable 按表覆盖上限,键是表名(= proto message 名)。
	// 用于 player_snapshot 这类天然就该大的表。
	PerTable map[string]BlobTableLimit `json:"PerTable,optional"`
}

// BlobTableLimit 是单表的上限覆盖,0 表示沿用全局值。
type BlobTableLimit struct {
	MaxColumnBytes int64 `json:"MaxColumnBytes,optional"`
	MaxRowBytes    int64 `json:"MaxRowBytes,optional"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Hosts             string `json:"Hosts"`
	DefaultTTLSeconds int    `json:"DefaultTTLSeconds"`
	Password          string `json:"Password"`
	DB                int    `json:"DB"`
}

// KafkaConfig holds Kafka consumer settings.
type KafkaConfig struct {
	Brokers         []string `json:"Brokers"`                      // Broker addresses
	GroupID         string   `json:"GroupID"`                      // Consumer group ID
	Topic           string   `json:"Topic,optional"`               // Derived from ZoneId at startup
	TopicGeneration uint32   `json:"TopicGeneration,default=1"`    // Immutable routing generation; partition changes require a new topic
	PartitionCnt    int32    `json:"PartitionCnt"`                 // Partition count
	RetentionMs     int64    `json:"RetentionMs,default=86400000"` // Topic retention in ms (default 24h; matches login)
	// P1 数据安全加固 2026-06-03: 旧值 300000 (5min)
	// db service 卡 5min+ 会丢数据,24h 给运维事故充足窗口
	IsOfflineExpand bool `json:"IsOfflineExpand"` // Offline expansion: true = maintenance mode

	// SubShardCount controls intra-partition parallelism. 0 or 1 = legacy
	// behaviour (single goroutine per partition, max parallelism =
	// PartitionCnt). N>1 = each partition's worker becomes a router that
	// fans tasks across N sub-goroutines by hash(task.Key), so per-key
	// ordering is preserved while overall throughput multiplies by N.
	//
	// Why this exists: under MaxOpenConn=30 the MySQL pool was massively
	// underused because partition=10 = strictly 10 in-flight queries.
	// With SubShardCount=4, effective parallelism becomes 10×4=40 and the
	// pool actually fills. See 2026-05-28 stress maxopenconn doc §3.
	SubShardCount int `json:"SubShardCount,optional"`
}

var AppConfig Config

// 迁移安全阀默认值。
//
// LockWait / InnodbLockWait 取 5s:DDL 撞上长事务时必须**秒级失败**并退出,
// 而不是把 MDL 排到整表后面 —— 后者会让所有 db_task 的 Save 一起挂住。
// StatementTimeout 取 300s 是给真正在跑的 ALTER 留出余量(表大时正常耗时),
// 超时后由 KILL QUERY 兜底。AdvisoryLock 取 60s:多副本同时部署时后到的那个
// 应该排队等一会儿,而不是立刻失败重启。
const (
	DefaultMigrationLockWaitSeconds       = 5
	DefaultMigrationInnodbLockWaitSeconds = 5
	DefaultMigrationStatementTimeoutSec   = 300
	DefaultMigrationAdvisoryLockSeconds   = 60
)

// 大字段闸默认值。
//
// 256 KiB / 列:玩家单个组件 blob 的设计量级是个位数 KB,256 KiB 已经留了
// 一到两个数量级的余量,同时比 MEDIUMBLOB 的 16 MiB 低 64 倍 —— 只有这样
// 闸门才会在「异常增长」而不是「已经炸了」的时候响。
// 1 MiB / 行:player_database 有 9 个 blob 列,单行总量到 1 MiB 就意味着
// 每次存盘都要推 1 MiB 过 MySQL 连接并原样写回 Redis,那已经是事故现场。
const (
	DefaultBlobMaxColumnBytes int64   = 256 * 1024
	DefaultBlobMaxRowBytes    int64   = 1024 * 1024
	DefaultBlobWarnRatio      float64 = 0.8
)

// Normalize 回填零值默认项。
//
// go-zero 对「标了 optional 且 yaml 里整段缺失」的嵌套结构不会走 default 标签,
// 所以默认值统一在这里补,而不是散在 tag 里 —— 否则「配了半段」和「完全没配」
// 会走出两套不同的默认值。可重复调用。
func (c *Config) Normalize() {
	m := &c.ServerConfig.Database.Migration
	if m.LockWaitSeconds <= 0 {
		m.LockWaitSeconds = DefaultMigrationLockWaitSeconds
	}
	if m.InnodbLockWaitSeconds <= 0 {
		m.InnodbLockWaitSeconds = DefaultMigrationInnodbLockWaitSeconds
	}
	if m.StatementTimeoutSeconds <= 0 {
		m.StatementTimeoutSeconds = DefaultMigrationStatementTimeoutSec
	}
	if m.AdvisoryLockSeconds <= 0 {
		m.AdvisoryLockSeconds = DefaultMigrationAdvisoryLockSeconds
	}

	b := &c.ServerConfig.BlobGuard
	if b.MaxColumnBytes <= 0 {
		b.MaxColumnBytes = DefaultBlobMaxColumnBytes
	}
	if b.MaxRowBytes <= 0 {
		b.MaxRowBytes = DefaultBlobMaxRowBytes
	}
	if b.WarnRatio <= 0 || b.WarnRatio > 1 {
		b.WarnRatio = DefaultBlobWarnRatio
	}
}

// DbTaskTopic returns the zone-specific Kafka topic for DB tasks.
func DbTaskTopic(zoneId uint32) string {
	return fmt.Sprintf("db_task_zone_%d", zoneId)
}

func DbTaskTopicForGeneration(zoneId, generation uint32) string {
	base := DbTaskTopic(zoneId)
	if generation <= 1 {
		return base
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}

// ZoneDBName returns the zone-specific MySQL database name.
func ZoneDBName(zoneId uint32) string {
	return fmt.Sprintf("zone_%d_db", zoneId)
}
