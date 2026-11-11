package config

import (
	"fmt"

	"shared/authoritysql"
	"shared/dbplacement"
	"shared/secretenv"

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

	// Placement 是落点库的放行与连接池参数(docs/design/player-storage-placement.md §6)。
	// 整块可缺省,缺省语义见 PlacementConfig。
	Placement PlacementConfig `json:"Placement,optional"`

	// DataServiceRpc:认领前读玩家落点(GetPlayerRoute),go/db 不连目录。必填,调用方密钥在环境变量 MMORPG_ROLE_FENCE_SECRET_DB。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc,optional"`

	// DbPlacement:逻辑库按映射拨号(docs/design/horizontal-scale-logical-db.md §8.1),没写就固定连 Database.Hosts。
	// 和上面的 Placement 不是一回事:Placement 管玩家落在哪个库,DbPlacement 管库落在哪个物理集群。
	DbPlacement dbplacement.Config `json:"DbPlacement,optional"`

	// MySQLPasswordEnv:MySQL 口令所在的环境变量名(K8s 上经 Secret mysql-auth 注入,docs/design/workload-secrets-via-k8s-secret.md)。
	// 写了它,ServerConfig.Database.Passwd 必须为空,由 ResolveMySQLPassword 在加载配置后填;本机 etc/db.yaml 不写,照用 Passwd。
	MySQLPasswordEnv string `json:",optional"`
}

// PlacementConfig 控制 go/db 能打开哪些存档库、每个库的连接池多大。
//
// go/db 没有「本区主库」,也不再有 zone 家族库(docs/design/player-data-tidb-scaling-plan.md §7.2 A17c):
// 全部玩家存档在本地域的地域存档库 player_store_{>=1000000}_db 里,每个库第一次被指向时按需打开、一个池。
//
// go-zero 对整段 optional 且未出现的嵌套结构不回填内层 default 标签,所以这里不写 default:
//   - AllowStoreFamilies 用 *bool,nil(没写)按 true 读,经 StoreFamiliesAllowed 取值;
//   - 连接池大小由 Normalize 回填。
type PlacementConfig struct {
	// AllowStoreFamilies 为 nil 或 true 时,名字属于存档库家族 player_store_{>=1000000}_db 的库即使不在库名白名单里
	// 也允许按需打开(§6.1)。按需打开只 Ping + 只读 schema 闸,从不建库。显式 false 时只认白名单。
	AllowStoreFamilies *bool `json:"AllowStoreFamilies,optional"`

	// StoreMaxOpenConn / StoreMaxIdleConn 是**每个**存档库连接池的上限与空闲数;0 = 取默认 8 / 2(Normalize)。
	// 服务 yaml 写的是「本地域只有一个库」(K=1)时的预算,K8s 的 ConfigMap 按存档库数 K 摊到每个库
	// (tools/scripts/k8s_deploy.ps1 Get-PlayerStorePoolLimit,每库至少 2 条),连接总账读同一条规则。
	StoreMaxOpenConn int `json:"StoreMaxOpenConn,optional"`
	StoreMaxIdleConn int `json:"StoreMaxIdleConn,optional"`

	// StoreConnMaxIdleSeconds 是存档库池的 SetConnMaxIdleTime(秒);≤0 视为没写,取默认 60。
	StoreConnMaxIdleSeconds int `json:"StoreConnMaxIdleSeconds,optional"`
}

// StoreFamiliesAllowed 返回是否按家族放行落点库;没写(nil)按 true。
func (p PlacementConfig) StoreFamiliesAllowed() bool {
	return p.AllowStoreFamilies == nil || *p.AllowStoreFamilies
}

// ServerConfig holds core service settings.
type ServerConfig struct {
	Database    DatabaseConfig    `json:"Database"`
	JsonPath    string            `json:"JsonPath"`
	PlayerStore PlayerStoreConfig `json:"PlayerStore,optional"`
}

// PlayerStoreConfig 是存盘接口的配置。
type PlayerStoreConfig struct {
	// MaxRowBytes / MaxRequestBytes 是 payload 预算,0 = 用代码默认值。
	MaxRowBytes     int `json:"MaxRowBytes,optional"`
	MaxRequestBytes int `json:"MaxRequestBytes,optional"`
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Hosts       string `json:"Hosts"`
	User        string `json:"User"`
	Passwd      string `json:"Passwd"`
	// DBName 只给迁移入口用:cmd/migrate 按 -storage-id 派生目标存档库名填进来。常驻进程不连固定的库,
	// 每个存档库按需打开,池参数在 Placement.Store*(A17c 删了本区库和它的 MaxOpenConn / MaxIdleConn)。
	DBName string `json:"DBName,optional"`
	Net    string `json:"Net"`
	// Backend:tidb | mysql,不写就是 tidb(docs/design/player-data-tidb-scaling-plan.md §9);非法值拒启。
	// tidb 档每条玩家库物理连接固定 TiDB leader、当前严格读;mysql 档每条物理连接核对是 MySQL 主库。
	Backend string `json:"Backend,default=tidb"`

	// AllowedDatabases 是**外部注入**的库名白名单。
	//
	// 为什么必须外部注入:能写哪些库是部署的决定(本地域的 K 个存档库,k8s_deploy.ps1 核过恰好是这 K 个),
	// 不能由进程从自己的配置推导 —— 用推导出来的名字自己校验自己是无效的。历史上 DBName 由 ZoneId 现拼,
	// ZoneId 填错一位就会在生产实例上静默建库把玩家数据写进一个无人知晓的新库,白名单就是为此而设。
	// 清单里只许有地域存档库名,zone 库名会让 go/db 拒启(proto_sql.NewStoreRegistryFromConfig)。
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
	// 大表并发 ALTER,MDL 阻塞会让整个 zone 的存盘停摆。生产改为由
	// `go run ./cmd/migrate -command up` 在部署阶段单独执行。
	// 同样只有 dev/test 模式下本开关才会被采纳。
	AutoMigrateSchema bool `json:"AutoMigrateSchema,optional"`

	// Migration 是迁移入口(cmd/migrate)的超时与锁参数,业务服务不读。
	Migration MigrationConfig `json:"Migration,optional"`

	// 以下五个是业务连接池(按需打开的存档库)的驱动级超时与会话级锁等待上限
	// 迁移入口与建库不用这组值:DDL 可能合法地跑几分钟。
	//
	// 写 0 或不写取默认值;写负数表示不设(驱动不加超时 / 不发对应的 SET)。
	//
	// 为什么要求会话级锁等待短于读超时:客户端读超时只关掉自己这一侧的连接,已经发给 MySQL 的语句不会被撤回,
	// 可能在之后才生效。让服务端先以 1205 自行中止排队中的语句,超时之后就不会留下在途语句。
	DialTimeoutSeconds  int `json:"DialTimeoutSeconds,optional"`
	ReadTimeoutSeconds  int `json:"ReadTimeoutSeconds,optional"`
	WriteTimeoutSeconds int `json:"WriteTimeoutSeconds,optional"`
	// SessionLockWaitSeconds 对应 MySQL 的 lock_wait_timeout(元数据锁等待,默认一年)。
	// TiDB 是否接受这个会话变量没有核实;切 TiDB 时若连接因它失败,写 -1 关掉。
	SessionLockWaitSeconds int `json:"SessionLockWaitSeconds,optional"`
	// SessionInnodbLockWaitSeconds 对应 innodb_lock_wait_timeout(行锁等待,MySQL 默认 50 秒)。
	SessionInnodbLockWaitSeconds int `json:"SessionInnodbLockWaitSeconds,optional"`
}

// 业务连接池的超时与锁等待默认值(秒)。会话级锁等待必须短于读超时,理由见 DatabaseConfig 的注释。
const (
	DefaultDatabaseDialTimeoutSeconds           = 5
	DefaultDatabaseReadTimeoutSeconds           = 30
	DefaultDatabaseWriteTimeoutSeconds          = 30
	DefaultDatabaseSessionLockWaitSeconds       = 10
	DefaultDatabaseSessionInnodbLockWaitSeconds = 10
)

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

var AppConfig Config

// 迁移安全阀默认值。
//
// LockWait / InnodbLockWait 取 5s:DDL 撞上长事务时必须**秒级失败**并退出,
// 而不是把 MDL 排到整表后面 —— 后者会让所有存盘一起挂住。
// StatementTimeout 取 300s 是给真正在跑的 ALTER 留出余量(表大时正常耗时),
// 超时后由 KILL QUERY 兜底。AdvisoryLock 取 60s:多副本同时部署时后到的那个
// 应该排队等一会儿,而不是立刻失败重启。
const (
	DefaultMigrationLockWaitSeconds       = 5
	DefaultMigrationInnodbLockWaitSeconds = 5
	DefaultMigrationStatementTimeoutSec   = 300
	DefaultMigrationAdvisoryLockSeconds   = 60
)

// 存档库连接池默认值(player-storage-placement.md §6.1)。没写时才用,etc/db.yaml 显式写出 K=1 的预算(60 / 20)。
//
// 每个被按需打开的库各自一个池。预算按「每个 go/db 实例的存档库数 × 每库池大小 × 副本数 × zone 数」计入连接总账
// (tools/scripts/k8s_deploy.ps1 Get-MysqlConnectionLedger)。
//
// StoreConnMaxIdleSeconds 默认 60:没有流量的库的池因此回到 0 条,不按 MaxIdleConn 常驻。
// ConnMaxLifetime 是 5 分钟,本值大于 300 等于不生效(空闲连接本来就会在 5 分钟寿命到期时被回收,本值只是让冷池更早归零)。
const (
	DefaultPlacementStoreMaxOpenConn        = 8
	DefaultPlacementStoreMaxIdleConn        = 2
	DefaultPlacementStoreConnMaxIdleSeconds = 60
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

	// 业务连接池:0 取默认,负数保留(表示不设),所以这里只补 0。
	d := &c.ServerConfig.Database
	if d.DialTimeoutSeconds == 0 {
		d.DialTimeoutSeconds = DefaultDatabaseDialTimeoutSeconds
	}
	if d.ReadTimeoutSeconds == 0 {
		d.ReadTimeoutSeconds = DefaultDatabaseReadTimeoutSeconds
	}
	if d.WriteTimeoutSeconds == 0 {
		d.WriteTimeoutSeconds = DefaultDatabaseWriteTimeoutSeconds
	}
	if d.SessionLockWaitSeconds == 0 {
		d.SessionLockWaitSeconds = DefaultDatabaseSessionLockWaitSeconds
	}
	if d.SessionInnodbLockWaitSeconds == 0 {
		d.SessionInnodbLockWaitSeconds = DefaultDatabaseSessionInnodbLockWaitSeconds
	}

	// 存档库池:0 取默认。不新增拒启校验:idle > open 由 database/sql 静默压到 open。
	p := &c.Placement
	if p.StoreMaxOpenConn <= 0 {
		p.StoreMaxOpenConn = DefaultPlacementStoreMaxOpenConn
	}
	if p.StoreMaxIdleConn <= 0 {
		p.StoreMaxIdleConn = DefaultPlacementStoreMaxIdleConn
	}
	if p.StoreConnMaxIdleSeconds <= 0 {
		p.StoreConnMaxIdleSeconds = DefaultPlacementStoreConnMaxIdleSeconds
	}
}

// Validate 校验配置。conf.Load 在 Normalize 之前自动调用一次,启动路径在 Normalize 之后再调一次,本方法对两者都安全。
//
// Config 内嵌的 zrpc.RpcServerConf 自带 Validate(Auth 打开时校验 Redis 段);本方法会遮蔽它,所以先原样调一次。
func (c *Config) Validate() error {
	if err := c.RpcServerConf.Validate(); err != nil {
		return err
	}
	return c.ValidateGlobalAuthority()
}

// ResolveMySQLPassword 按 MySQLPasswordEnv 把 MySQL 口令填进 ServerConfig.Database.Passwd,规则见 secretenv.Fill
// (变量缺席或为空、或 Passwd 同时写了字面量都返回错误)。主进程与 cmd/migrate 在 conf.MustLoad 之后、连库之前调用;lookup 生产传 os.LookupEnv。
func (c *Config) ResolveMySQLPassword(lookup func(string) (string, bool)) error {
	return secretenv.Fill("MySQLPasswordEnv", c.MySQLPasswordEnv, lookup,
		secretenv.Target{Field: "ServerConfig.Database.Passwd", Value: &c.ServerConfig.Database.Passwd})
}

// ValidateGlobalAuthority 是普通服务启动的配置门禁,不执行 DDL。Mode 不放宽它。
func (c *Config) ValidateGlobalAuthority() error {
	if _, err := authoritysql.ParseBackend(c.ServerConfig.Database.Backend); err != nil {
		return fmt.Errorf("ServerConfig.Database.Backend: %w", err)
	}
	return nil
}
