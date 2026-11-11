package proto_sql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"db/internal/config"
	"db/internal/dbguard"
	"db/internal/migrate"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	_ "proto/common/database"
	"regexp"
	"shared/authoritysql"
	"shared/dbplacement"
	"shared/placement"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// GameDB 是一个已打开、已核对过的存档库:连接池加上注册过全部表的模型。只由 StoreRegistry 按需打开(OpenPlacementStore)。
type GameDB struct {
	SqlModel *proto2mysql.DB
	DB       *sql.DB
	// Connector 是建这个连接池用的连接器。探活(store_health.go)用它绕过连接池新建一条连接:
	// 池里的空闲连接可能早已失效(Windows 上驱动不做空闲连接活性检查),池满时取连接还要排队,
	// 两种情况都会把活着的库误判成不可达。为 nil(单测里只带库名的模型)时退回连接池的 PingContext。
	Connector driver.Connector
}

// storeBoot 是本进程启动时装配一次的写资格与逻辑库映射(docs/design/player-data-tidb-scaling-plan.md §7.2 A17c)。
// 它不对应任何一个库的连接:go/db 不再有「本区主库」,存档库全部由 StoreRegistry 在第一次被指向时按需打开。
type storeBoot struct {
	// writers 由进程唯一启动入口消费一次:每个存档库一份,闸门跟着库走;按需打开时按库名取,不重新登记。
	writers map[string]authoritysql.WriterConfig
	// placement 是逻辑库映射(没配 DbPlacement 时 nil,固定连配置的地址)。
	placement authoritysql.PlacementResolver
}

// boot 由 InitDB 写一次,之后只读。
var boot *storeBoot

// DDL 开关的环境变量覆盖名。
//
// 为什么要有 env 覆盖:业务服务的 yaml 在 K8s 里是 ConfigMap,改它要重新
// 渲染+apply;而「回退到旧的启动期自动建表」是一个必须能在事故中一分钟内
// 做到的动作。env 放在 Deployment 上,改一行即可。取值 1/true/yes 开启,
// 0/false/no 关闭,两个方向都能覆盖 yaml。
const (
	EnvAutoCreateDatabase = "DB_AUTO_CREATE_DATABASE"
	EnvAutoMigrateSchema  = "DB_AUTO_MIGRATE_SCHEMA"
)

// DDLPolicy 是本进程被允许做的 DDL 范围。
//
// 默认全 false —— 业务服务的启动路径不碰 DDL。
//
// 历史行为(等价于全 true)的问题不是「多跑了几条语句」,而是:
//   - 多 zone × 多副本同时启动 ⇒ 对同一张大表并发 ALTER,MDL 阻塞会把整个
//     zone 的存盘一起拖停;
//   - proto2mysql 在列类型不匹配时会自作主张拼 MODIFY COLUMN 并执行,
//     没有版本记录、没有 lock 超时,大表上就是一次无人批准的在线 DDL。
//
// 生产改为在部署阶段跑 `go run ./cmd/migrate -command up`(见 go/db/README.md)。
type DDLPolicy struct {
	// AllowCreateDatabase 允许在 MySQL 报 1049(库不存在)时自动建库。
	AllowCreateDatabase bool
	// AllowCreateOrUpdateTable 允许逐张跑 CreateOrUpdateTable(建表 / 补列)。
	AllowCreateOrUpdateTable bool
	// Source 记录每个开关的最终来源,启动日志要打出来。
	Source string
}

// ResolveDDLPolicy 解出本进程的 DDL 权限:yaml 打底,env 可双向覆盖。
func ResolveDDLPolicy() DDLPolicy {
	dbCfg := config.AppConfig.ServerConfig.Database
	policy := DDLPolicy{
		AllowCreateDatabase:      dbCfg.AutoCreateDatabase,
		AllowCreateOrUpdateTable: dbCfg.AutoMigrateSchema,
	}
	sources := []string{"config"}
	if v, ok := envBool(EnvAutoCreateDatabase); ok {
		policy.AllowCreateDatabase = v
		sources = append(sources, fmt.Sprintf("%s=%v", EnvAutoCreateDatabase, v))
	}
	if v, ok := envBool(EnvAutoMigrateSchema); ok {
		policy.AllowCreateOrUpdateTable = v
		sources = append(sources, fmt.Sprintf("%s=%v", EnvAutoMigrateSchema, v))
	}
	policy.Source = strings.Join(sources, ",")
	return policy
}

// envBool 解析布尔环境变量;未设置或无法解析时返回 ok=false(不覆盖 yaml)。
func envBool(name string) (bool, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, false
	}
	switch strings.ToLower(raw) {
	case "yes", "on":
		return true, true
	case "no", "off":
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		logx.Errorf("ignoring unparsable boolean env %s=%q: %v", name, raw, err)
		return false, false
	}
	return v, true
}

func newMysqlConfig() *mysql.Config {
	myCnf := mysql.NewConfig()
	myCnf.User = config.AppConfig.ServerConfig.Database.User
	myCnf.Passwd = config.AppConfig.ServerConfig.Database.Passwd
	myCnf.Addr = config.AppConfig.ServerConfig.Database.Hosts
	myCnf.Net = config.AppConfig.ServerConfig.Database.Net
	myCnf.DBName = config.AppConfig.ServerConfig.Database.DBName
	// 强制会话级严格模式:玩家权威数据是 MEDIUMBLOB 里的 protobuf,非严格模式下超长写入
	// 会 err=nil 静默截断,下次 proto.Unmarshal 必失败且无任何日志可追溯,损失面是整个存档库。
	// 用连接参数在每条连接建立时 SET(会话级覆盖 global),而非仅做启动断言——这样即使
	// MySQL 服务端 global 未配严格模式,本服务的写入路径依然是严格的。
	myCnf.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES'"}
	return myCnf
}

// NewMysqlConfig 暴露给迁移入口(cmd/migrate)复用同一套连接参数。
// 它不带业务池的读写超时与会话级锁等待(见 newServiceMysqlConfig):DDL 可能合法地跑几分钟,
// 迁移 runner 也会在自己的会话上单独设 lock_wait_timeout。
func NewMysqlConfig() *mysql.Config { return newMysqlConfig() }

// newServiceMysqlConfig 是业务连接池(按需打开的存档库)的连接参数:在 newMysqlConfig 之上加
// 拨号 / 读 / 写超时,以及会话级的锁等待上限(docs/design/db-task-store-unavailable-deferral.md §8)。
//
// 没有这组超时时,MySQL 卡住(排在一条 DDL 的元数据锁后面、进程被暂停、对端不回包)会让 SQL 既不返回也不报错:
// 子分片协程永久阻塞,通道一路堵到全实例唯一的重试协程,Stop() 也挂住。超时之后驱动返回连接类错误,
// 任务按 L-04 延后。
//
// 会话级锁等待要短于读超时:客户端读超时撤不回已经发出的语句,让服务端先以 1205 中止排队中的语句,
// 超时之后才不会留下事后生效的在途写。这一层只是缩小窗口,不是栅栏;挡旧写要靠条件写。
func newServiceMysqlConfig() *mysql.Config {
	myCnf := newMysqlConfig()
	db := config.AppConfig.ServerConfig.Database
	myCnf.Timeout = positiveSeconds(db.DialTimeoutSeconds)
	myCnf.ReadTimeout = positiveSeconds(db.ReadTimeoutSeconds)
	myCnf.WriteTimeout = positiveSeconds(db.WriteTimeoutSeconds)
	if db.SessionLockWaitSeconds > 0 {
		myCnf.Params["lock_wait_timeout"] = strconv.Itoa(db.SessionLockWaitSeconds)
	}
	if db.SessionInnodbLockWaitSeconds > 0 {
		myCnf.Params["innodb_lock_wait_timeout"] = strconv.Itoa(db.SessionInnodbLockWaitSeconds)
	}
	return myCnf
}

// positiveSeconds 把配置里的秒数转成时长;0 与负数都表示不设(驱动按 0 处理即不加超时)。
func positiveSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// logServicePoolTimeouts 在启动时打印一次业务池的超时口径,并检查「会话级锁等待短于读超时」。
// 不满足时只打 ERROR 不拒启:它削弱的是在途语句窗口的保护,不会造成写错数据,拒启等于停掉整个 zone 的存盘。
func logServicePoolTimeouts() {
	db := config.AppConfig.ServerConfig.Database
	logx.Infof("mysql service pool timeouts: dial=%ds read=%ds write=%ds session lock_wait_timeout=%ds innodb_lock_wait_timeout=%ds (<=0 means unset)",
		db.DialTimeoutSeconds, db.ReadTimeoutSeconds, db.WriteTimeoutSeconds, db.SessionLockWaitSeconds, db.SessionInnodbLockWaitSeconds)
	if db.ReadTimeoutSeconds <= 0 {
		logx.Errorf("mysql service pool has no read timeout: a stalled MySQL will block db workers forever (Database.ReadTimeoutSeconds)")
		return
	}
	for name, wait := range map[string]int{
		"SessionLockWaitSeconds":       db.SessionLockWaitSeconds,
		"SessionInnodbLockWaitSeconds": db.SessionInnodbLockWaitSeconds,
	} {
		if wait <= 0 {
			// 写负数是有意关掉(例如 TiDB 不接受 lock_wait_timeout),只提示,不当错误。
			logx.Infof("Database.%s is disabled (%d): the server-side lock wait stays at its server default", name, wait)
			continue
		}
		if wait >= db.ReadTimeoutSeconds {
			logx.Errorf("Database.%s=%d is not below ReadTimeoutSeconds=%d: a statement queued on a lock can outlive the client timeout and take effect after the task was rescheduled",
				name, wait, db.ReadTimeoutSeconds)
		}
	}
}

// AllowlistSpec 组装白名单的三个外部来源。迁移入口与业务服务共用同一份,
// 保证「迁移能改的库」和「服务能写的库」永远是同一个集合。
func AllowlistSpec() dbguard.AllowlistSpec {
	dbCfg := config.AppConfig.ServerConfig.Database
	return dbguard.AllowlistSpec{
		File:   dbCfg.AllowedDatabasesFile,
		Inline: dbCfg.AllowedDatabases,
	}
}

// RelaxEmptyAllowlist 报告「白名单为空」是否只警告不拒启。
//
// 刻意不看 go-zero 的 Mode:db.yaml 里没写 Mode 时 go-zero 默认是 "pro",
// 本地脚本起的服务因此会被当成生产 —— 用它做判据会把「本地能不能跑」和
// 「有没有写 Mode」绑在一起,踩坑时完全看不出因果。改成显式字段。
func RelaxEmptyAllowlist() bool {
	return strings.EqualFold(
		strings.TrimSpace(config.AppConfig.ServerConfig.Database.AllowlistEnforcement), "warn")
}

// CreateDatabase 建库。**这不是启动路径该做的事**,只保留给迁移入口与
// 显式开了 AutoCreateDatabase 的 dev 环境。
//
// 库名由迁移入口的 -storage-id 经 placement.StoreDBName 派生,编号是最容易填错的参数:
// 填错一位就会在生产实例上静默建出一个没人认领的新库并建全套表。所以
// 这里强制先过外部白名单 —— 白名单不认的库名,一律不建。
func CreateDatabase() error {
	target := config.AppConfig.ServerConfig.Database.DBName
	if target == "" {
		return errors.New("refusing to create database: DBName is empty")
	}
	allow, err := AllowlistSpec().Resolve()
	if err != nil {
		return fmt.Errorf("resolve database allow list: %w", err)
	}
	if allow.Empty() {
		if !RelaxEmptyAllowlist() {
			return fmt.Errorf("%w: refusing to CREATE DATABASE %q without an injected allow list (set %s)",
				dbguard.ErrAllowlistMissing, target, dbguard.AllowedDatabasesEnv)
		}
		logx.Errorf("DB-ALLOWLIST: Allowlist empty; allowing CREATE DATABASE %q under AllowlistEnforcement=warn. Production must inject %s",
			target, dbguard.AllowedDatabasesEnv)
	} else if !allow.Contains(target) {
		return fmt.Errorf("%w: refusing to CREATE DATABASE %q; allowed=%s",
			dbguard.ErrDatabaseNotAllowed, target, allow.String())
	}
	if err := validateDatabaseIdentifier(target); err != nil {
		return err
	}

	mysqlConfig := newMysqlConfig()
	mysqlConfig.DBName = ""

	conn, err := mysql.NewConnector(mysqlConfig)
	if err != nil {
		return fmt.Errorf("error creating MySQL connector: %w", err)
	}

	tempDB := sql.OpenDB(conn)
	defer func() {
		if err := tempDB.Close(); err != nil {
			logx.Errorf("error closing temp database connection: %v", err)
		}
	}()

	if _, err = tempDB.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", target)); err != nil {
		logx.Errorf("error creating database %s: %v", target, err)
		return err
	}
	logx.Infof("database ensured: %s (allowlist=%s)", target, allow.String())
	return nil
}

// validateDatabaseIdentifier 拒绝无法安全反引号转义的库名。
// 库名来自配置拼接而非用户输入,但 CREATE DATABASE 无法参数化,守一道最省事。
func validateDatabaseIdentifier(name string) error {
	if strings.ContainsAny(name, "`\x00") {
		return fmt.Errorf("invalid database identifier %q", name)
	}
	return nil
}

func isUnknownDatabaseError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1049
	}
	return false
}

func newPlayerConnector(cfg *mysql.Config) (driver.Connector, error) {
	if boot == nil {
		return nil, errors.New("player store pool requested before this boot consumed its writer qualifications (call InitDB first)")
	}
	writer, ok := boot.writers[cfg.DBName]
	if !ok {
		return nil, fmt.Errorf("this boot has no writer qualification for player store %s", cfg.DBName)
	}
	backend := authoritysql.Backend(config.AppConfig.ServerConfig.Database.Backend)
	if boot.placement != nil {
		return authoritysql.PinPlacedSession(cfg, boot.placement, backend, authoritysql.Writer(writer))
	}
	inner, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return authoritysql.PinCurrentSession(inner, backend, authoritysql.Writer(writer))
}

// schemaDriftCheckTimeout 是只读 schema 核对的总预算(按需打开存档库时用,见 store_registry.go)。
// 核对只有几条 information_schema / 台账的只读查询,正常是毫秒级;给 30s 是为了实例繁忙时
// 不误拒启,同时不让「库卡住」变成进程无限挂在启动阶段。
const schemaDriftCheckTimeout = 30 * time.Second

// migrateRemedyCommand 是缺列 / 缺表时打印给运维的补救命令(在 go/db 目录下执行)。
// -storage-id 是报错里那个存档库的编号(player_store_<编号>_db),-f 用本进程的同一份配置(库名清单、连接参数)。
const migrateRemedyCommand = "go run ./cmd/migrate -f <本进程使用的 db 配置> -storage-id <存档库编号> -command up"

// addColumnStmtRe 从 migrate.ProtoSource.Drift 产出的补列语句里解出「表、列」,只用于拼报错。
// 语句形状由 internal/migrate/plan.go 的 Drift 固定为 ALTER TABLE `表` ADD COLUMN `列` <类型>;
// 解不出来时原样带上整条语句 —— 报错难看一点可以,漏报不行。
var addColumnStmtRe = regexp.MustCompile("^ALTER TABLE `([^`]+)` ADD COLUMN `([^`]+)`")

// assertSchemaUpToDate 是启动期 DDL 关闭时的只读 schema 闸:库里缺 proto 声明的列,或缺表且
// `cmd/migrate -command up` 能建出来时,就拒启。
//
// 为什么缺列必须拒启:库里少一列,存盘和认领的语句都是 Error 1054,玩家存不了盘,也进不了场。
// 2026-09-22 zone 库缺 profile_component / asset_op_ledger 就是这样:进程照常打印
// STARTED SUCCESSFULLY,故障拖到玩家登录时才以 EnterGame preload failed 暴露。
//
// 缺表(Error 1146,后果同上)按台账分两种:
//   - 台账里没有干净的基线(空库、历史 AutoMigrate 建出来的库、基线跑到一半留了 dirty):
//     up 会先跑基线(全是 CREATE TABLE IF NOT EXISTS,幂等)再补列,所以拒启并给补救命令。
//     存档库由部署阶段预建成**空库**,迁移 Job 没跑或跑漏一个库时,这是最可能的真实故障;
//   - 基线已干净应用:runner 不重跑基线(基线 checksum 只算表名集合,表清单变了也只告警),
//     up 建不出这张表,拦下来给不出能执行的补救命令,所以只打 SCHEMA-DRIFT 错误日志、不阻断。
//
// 其余漂移(类型不一致 / 多余列 / 缺主键)只打 SCHEMA-DRIFT 日志:
//   - 类型漂移有已知的历史存量(user_oauth.provider_id、user_phone.phone),拦下来本地库全起不来;
//   - Drift 只对缺列生成语句,up 不处理这几类,拦下来同样给不出能执行的补救命令。
//
// 全程只读:只跑 information_schema 的 SELECT 与一条台账计数 SELECT,不 SET SESSION、不取 GET_LOCK、
// 不建台账,所以不需要 Migration 段的锁超时配置,多副本同时启动也互不影响。
func assertSchemaUpToDate(ctx context.Context, q migrate.Queryer, model *proto2mysql.DB, database string, tables []proto.Message) error {
	if err := assertDriftKeysArePhysicalNames(tables); err != nil {
		return err
	}
	existing, err := migrate.LoadColumns(ctx, q, database)
	if err != nil {
		return schemaCheckFailed(database, err)
	}
	present := make([]proto.Message, 0, len(tables))
	var missingTables []string
	for _, t := range tables {
		name := proto2mysql.GetTableName(t)
		if _, ok := existing[name]; ok {
			present = append(present, t)
			continue
		}
		missingTables = append(missingTables, name)
	}
	if len(missingTables) > 0 {
		applied, err := cleanBaselineApplied(ctx, q, existing)
		if err != nil {
			return schemaCheckFailed(database, err)
		}
		if !applied {
			return fmt.Errorf("库 %s 缺 %d 张 proto 声明的表且台账 %s 里没有干净的基线,拒绝启动(启动期 DDL 已关闭,缺表会让对应表的每次存盘都以 Error 1146 失败);"+
				"在 go/db 目录执行 `%s`(先跑基线建表再补列;报 dirty 时按它的提示人工核对)后重启: %s",
				database, len(missingTables), migrate.SchemaMigrationsTable, migrateRemedyCommand, strings.Join(missingTables, ", "))
		}
		for _, name := range missingTables {
			logx.Errorf("SCHEMA-DRIFT: database %s missing table %s, but baseline already applied and cmd/migrate -command up will not recreate it (startup allowed); each save for this table will fail, dead-letter with Error 1146, manual table creation required",
				database, name)
		}
	}

	// 缺的表已在上面处理过,只把存在的表交给 Drift,免得它再报一遍含义不同的「查不到表」告警。
	src := &migrate.ProtoSource{Model: model, Tables: present}
	drift, warnings, err := src.Drift(ctx, q, database)
	if err != nil {
		return schemaCheckFailed(database, err)
	}
	for _, w := range warnings {
		logx.Errorf("SCHEMA-DRIFT: database %s requires manual review (startup allowed): %s", database, w)
	}
	if drift.Empty() {
		if len(missingTables) > 0 {
			logx.Errorf("schema drift check NOT clean: database=%s tables=%d missing_tables=%s review_warnings=%d (missing tables do not block startup; see above)",
				database, len(tables), strings.Join(missingTables, ","), len(warnings))
			return nil
		}
		logx.Infof("schema drift check passed: database=%s tables=%d review_warnings=%d",
			database, len(tables), len(warnings))
		return nil
	}

	missing := make([]string, 0, len(drift.Statements))
	for _, stmt := range drift.Statements {
		if m := addColumnStmtRe.FindStringSubmatch(stmt); m != nil {
			missing = append(missing, m[1]+"."+m[2])
			continue
		}
		missing = append(missing, stmt)
	}
	return fmt.Errorf("库 %s 缺 %d 个 proto 声明的列,拒绝启动(启动期 DDL 已关闭,缺列会让对应表的每次存盘都以 Error 1054 失败);"+
		"在 go/db 目录执行 `%s`(不加 -allow-modify)后重启: %s",
		database, len(missing), migrateRemedyCommand, strings.Join(missing, ", "))
}

// schemaCheckFailed 包装「核对本身做不成」的错误。
// 与 friend / trade 的 ensureSchema 同口径:查不了库结构也拒启,不按「没查出问题」放行。
func schemaCheckFailed(database string, err error) error {
	return fmt.Errorf("启动期 schema 核对失败(库 %s),拒绝启动;确认库可达后在 go/db 目录执行 `%s`: %w",
		database, migrateRemedyCommand, err)
}

// cleanBaselineApplied 报告台账里是否有一条干净(dirty=0)的基线记录,即 up 是否还会跑基线建表。
//
// existing 是 LoadColumns 的结果:台账表本身不在里面(空库 / 历史 AutoMigrate 建的库)时
// 直接视为未应用,不去查一张不存在的表。基线 dirty 同样算未应用 —— 那说明建表跑到一半,
// 库结构未知,up 会以 ErrDirtySchema 拒绝并给出人工核对的提示,正是该让人看见的时候。
func cleanBaselineApplied(ctx context.Context, q migrate.Queryer, existing map[string]map[string]string) (bool, error) {
	if _, ok := existing[migrate.SchemaMigrationsTable]; !ok {
		return false, nil
	}
	rows, err := q.QueryContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE version = ? AND dirty = 0", migrate.SchemaMigrationsTable),
		migrate.BaselineVersion)
	if err != nil {
		return false, fmt.Errorf("read baseline from %s: %w", migrate.SchemaMigrationsTable, err)
	}
	defer func() { _ = rows.Close() }()
	var count int64
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			return false, fmt.Errorf("scan baseline count from %s: %w", migrate.SchemaMigrationsTable, err)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate baseline count from %s: %w", migrate.SchemaMigrationsTable, err)
	}
	return count > 0, nil
}

// assertDriftKeysArePhysicalNames 守住 Drift 的查表键,在查库之前执行。
//
// internal/migrate 的 Drift 拿 proto2mysql.GetTableName(= proto full name)去 information_schema
// 里找表,而真实表名是 OptionTableName。两者今天相等,只因为 mysql_database_table.proto 没写
// package;一旦加了 package,每张表都会变成「查不到」,缺列核对对全部表静默失效,这道闸永远放行。
// 所以不一致就拒启,逼改 proto 的人先让 Drift 改按物理表名取表。
func assertDriftKeysArePhysicalNames(tables []proto.Message) error {
	for _, t := range tables {
		key := proto2mysql.GetTableName(t)
		physical, ok := proto2mysql.TableNameFromDescriptor(t.ProtoReflect().Descriptor())
		if !ok {
			// 没声明 OptionTableName 时 proto2mysql 按 full name 建表,与查表键天然一致
			// (RegisterTables 的表名守卫另行拒绝这种表)。
			continue
		}
		if key != physical {
			return fmt.Errorf("表名守卫: 消息 %s 的 proto full name 与 OptionTableName %q 不一致;internal/migrate 的 Drift 按 full name 查 information_schema,"+
				"启动期 schema 核对会对这张表静默失效,拒绝启动(给表 proto 加 package 前,先让 Drift 改按 OptionTableName 取表名)", key, physical)
		}
	}
	return nil
}

// InitDB 装配本进程的写资格与逻辑库映射,不连任何库(docs/design/player-data-tidb-scaling-plan.md §7.2 A17c)。
//
// go/db 没有「本区主库」:存档库全部由 StoreRegistry 在第一次被指向时按需打开,打开时 Ping、核 DATABASE()、
// 过只读 schema 闸(assertSchemaUpToDate),库不存在或缺列就报错延后,绝不建库。
// 常驻进程**不跑任何 DDL**,表由部署阶段的 cmd/migrate 建(见 DDLPolicy 的注释)。
func InitDB() error {
	writers, err := authoritysql.LoadWriterConfigs(os.Getenv)
	if err != nil {
		return err
	}
	return InitDBWithWriters(writers)
}

// InitDBWithWriters 供进程装配与自有 fixture 显式复用已消费的同 boot 资格。
//
// 每份写资格都必须属于一个地域存档库 player_store_<id>_db:zone 库已经退役,清单里还有它说明部署没跟上,拒启并点名。
// 启动时不探活存档库:库在不在、表对不对由部署阶段的迁移 Job 核,运行期由按需打开的那道闸核,
// 一个库坏了只影响落在它上面的玩家,不挡 go/db 起来。
func InitDBWithWriters(writers map[string]authoritysql.WriterConfig) error {
	if boot != nil {
		return errors.New("player SQL boot is already assembled; the boot identity cannot change")
	}
	if len(writers) == 0 {
		return errors.New("player SQL boot has no writer qualification: the boot grant lists no player store database")
	}
	for schema := range writers {
		if _, ok := placement.StorageIDFromDBName(schema); !ok {
			return fmt.Errorf("writer qualification for %q is not a player store database player_store_<id>_db; zone databases are retired "+
				"(player-data-tidb-scaling-plan.md §7.2 A17c), list only the player stores of this region in MMORPG_DB_ALLOWED_DATABASES", schema)
		}
	}
	policy := ResolveDDLPolicy()
	if policy.AllowCreateDatabase || policy.AllowCreateOrUpdateTable {
		return fmt.Errorf("resident db process must not run startup DDL (policy=%s); tables are created by the cmd/migrate schema entry point", policy.Source)
	}
	// 表清单先解析一遍并过表名守卫:清单坏了是部署错误,在启动时拒绝,不拖到第一个玩家进场时才在按需打开里报。
	if _, err := RegisterTables(proto2mysql.NewDB()); err != nil {
		return err
	}
	placementCtx, placementCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer placementCancel()
	dbPlacement, err := dbplacement.Start(placementCtx, config.AppConfig.DbPlacement)
	if err != nil {
		return err
	}
	boot = &storeBoot{writers: writers, placement: dbPlacement.Resolver()}
	logx.Infof("player store boot ready: %d player store writer qualifications, startup DDL disabled (policy=%s), stores open on demand",
		len(writers), policy.Source)
	logServicePoolTimeouts()
	return nil
}

// CurrentWriterConfigs 只返回同一批不可变 normal 能力(每个存档库一份),不含连接或 operator 权限。
func CurrentWriterConfigs() (map[string]authoritysql.WriterConfig, error) {
	if boot == nil || len(boot.writers) == 0 {
		return nil, errors.New("player SQL boot is not assembled yet")
	}
	return boot.writers, nil
}

// Close 的调用前提是全部consumer、outbox、retry及按需开池调用已确定join。
// 关闭本身不证明服务端提交结局；UNKNOWN必须在调用前由生命周期owner保留HOLD。
func (r *StoreRegistry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	pools := make(map[*sql.DB]bool)
	for _, store := range r.stores {
		if store != nil && store.DB != nil {
			pools[store.DB] = true
		}
	}
	r.mu.RUnlock()
	var result error
	for pool := range pools {
		result = errors.Join(result, pool.Close())
	}
	return result
}

// readJSONFile reads a JSON file and returns the message type names.
func readJSONFile(filePath string) ([]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var jsonData struct {
		Messages []string `json:"messages"`
	}

	err = json.Unmarshal(data, &jsonData)
	if err != nil {
		return nil, err
	}

	return jsonData.Messages, nil
}

// TablesFromJSON 按 JsonPath 列出的顺序解析出表消息原型。
//
// 解析不到的名字**直接报错**,不再 continue:漏掉一张表的后果是该表的
// 每次 Save / Find 在运行期返回 ErrTableNotFound,而启动日志里只有一行
// 被淹没的 Error —— 等于把配置错误推迟到玩家身上。
func TablesFromJSON() ([]proto.Message, error) {
	jsonPath := config.AppConfig.ServerConfig.JsonPath
	messageNames, err := readJSONFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("read table list %q: %w", jsonPath, err)
	}
	if len(messageNames) == 0 {
		return nil, fmt.Errorf("table list %q contains no message", jsonPath)
	}

	messages := make([]proto.Message, 0, len(messageNames))
	var missing []string
	for _, typeName := range messageNames {
		msgType, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(typeName))
		if err != nil {
			missing = append(missing, typeName)
			continue
		}
		messages = append(messages, msgType.New().Interface())
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("table list %q references unknown proto messages %v; rebuild the proto module or fix the list",
			jsonPath, missing)
	}
	return messages, nil
}

// ddlTableNameRegex 从生成的建表语句里解出表名,用于启动期表名守卫。
var ddlTableNameRegex = regexp.MustCompile("CREATE TABLE IF NOT EXISTS `([^`]+)`")

// assertTableNameLocked 表名守卫:proto2mysql 新版默认表名 = proto full name(含点号),
// 一旦 OptionTableName 因任何原因未被读到,schema sync 会静默建出新表,存量数据对业务
// "消失"。这里强校验每张表生成 DDL 的表名与 proto 里声明的 OptionTableName 完全一致,
// 不一致直接拒绝启动(全局数据层决策风险 #3)。挂在 registerTables 里,业务服务、
// 按需打开的落点库(store_registry.go)与迁移入口(internal/migrate)走同一条注册链,自动共享这道守卫。
func assertTableNameLocked(model *proto2mysql.DB, table proto.Message) error {
	md := table.ProtoReflect().Descriptor()
	declared, ok := proto2mysql.TableNameFromDescriptor(md)
	if !ok || declared == "" {
		return fmt.Errorf("表名守卫: 消息 %s 未声明 OptionTableName,拒绝按默认表名建表", md.FullName())
	}
	ddl := model.GetCreateTableSQL(table)
	m := ddlTableNameRegex.FindStringSubmatch(ddl)
	if m == nil {
		return fmt.Errorf("表名守卫: 无法从 %s 的建表语句中解析表名: %s", md.FullName(), ddl)
	}
	if m[1] != declared {
		return fmt.Errorf("表名守卫: 消息 %s 生成的表名 %q 与 proto 声明 %q 不一致,拒绝启动", md.FullName(), m[1], declared)
	}
	return nil
}

// RegisterTables 解析表清单并逐张注册进给定模型(纯内存,不产生任何 SQL),过表名守卫。
func RegisterTables(model *proto2mysql.DB) ([]proto.Message, error) {
	if model == nil {
		return nil, errors.New("RegisterTables needs a proto2mysql model")
	}
	tables, err := TablesFromJSON()
	if err != nil {
		return nil, err
	}
	if err := registerTables(model, tables); err != nil {
		return nil, err
	}
	return tables, nil
}

// registerTables 把表逐张注册进给定模型并过表名守卫(纯内存)。
// 每个落点库有自己的 proto2mysql 模型(见 OpenPlacementStore),注册与守卫必须对每个模型各做一遍。
func registerTables(model *proto2mysql.DB, tables []proto.Message) error {
	for _, table := range tables {
		model.RegisterTable(table)
		if err := assertTableNameLocked(model, table); err != nil {
			return err
		}
	}
	return nil
}
