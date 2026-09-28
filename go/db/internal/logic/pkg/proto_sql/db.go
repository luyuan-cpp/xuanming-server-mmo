package proto_sql

import (
	"context"
	"database/sql"
	"db/internal/config"
	"db/internal/dbguard"
	"db/internal/migrate"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	_ "proto/common/database"
	"regexp"
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

type GameDB struct {
	SqlModel *proto2mysql.DB
	DB       *sql.DB
}

var DB *GameDB

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
//     zone 的 db_task 消费一起拖停;
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
	// 会 err=nil 静默截断,下次 proto.Unmarshal 必失败且无任何日志可追溯,损失面是整个 zone。
	// 用连接参数在每条连接建立时 SET(会话级覆盖 global),而非仅做启动断言——这样即使
	// MySQL 服务端 global 未配严格模式,本服务的写入路径依然是严格的。
	myCnf.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES'"}
	return myCnf
}

// NewMysqlConfig 暴露给迁移入口(cmd/migrate)复用同一套连接参数。
func NewMysqlConfig() *mysql.Config { return newMysqlConfig() }

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
// 库名来自 ZoneId 现拼,而 ZoneId 是最容易填错的字段:填错一位就会在生产
// 实例上静默建出一个新库、建全套表、把玩家数据写进去且无人看到报错。所以
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
		logx.Errorf("DB-ALLOWLIST: 白名单为空,按 AllowlistEnforcement=warn 放行 CREATE DATABASE %q。生产必须注入 %s",
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

func openDB(policy DDLPolicy) error {
	mysqlConfig := newMysqlConfig()
	conn, err := mysql.NewConnector(mysqlConfig)
	if err != nil {
		return fmt.Errorf("create MySQL connector: %w", err)
	}

	databaseHandle := sql.OpenDB(conn)
	if err := databaseHandle.Ping(); err != nil {
		_ = databaseHandle.Close()
		if !isUnknownDatabaseError(err) {
			return err
		}
		if !policy.AllowCreateDatabase {
			// 这条 fail-closed 是整个治理的核心:库不存在说明「部署阶段的迁移
			// Job 没跑」或者「ZoneId 填错了」,两种都必须让人看见,而不是顺手
			// 建一个新库把玩家数据倒进去。
			return fmt.Errorf("database %q does not exist and startup-path DDL is disabled (policy=%s): run `go run ./cmd/migrate -command up` in the deploy stage, or set %s=1 to restore the legacy auto-create behaviour",
				config.AppConfig.ServerConfig.Database.DBName, policy.Source, EnvAutoCreateDatabase)
		}
		if err := CreateDatabase(); err != nil {
			return err
		}

		conn, err = mysql.NewConnector(mysqlConfig)
		if err != nil {
			return fmt.Errorf("create MySQL connector after database creation: %w", err)
		}

		databaseHandle = sql.OpenDB(conn)
		if err := databaseHandle.Ping(); err != nil {
			_ = databaseHandle.Close()
			return err
		}
	}

	if err := assertConnectedDatabase(databaseHandle); err != nil {
		_ = databaseHandle.Close()
		return err
	}

	DB = &GameDB{
		DB:       databaseHandle,
		SqlModel: proto2mysql.NewDB(),
	}

	DB.DB.SetMaxOpenConns(config.AppConfig.ServerConfig.Database.MaxOpenConn)
	DB.DB.SetMaxIdleConns(config.AppConfig.ServerConfig.Database.MaxIdleConn)
	DB.DB.SetConnMaxLifetime(5 * time.Minute)

	return DB.SqlModel.OpenDB(DB.DB, mysqlConfig.DBName)
}

// assertConnectedDatabase 在服务碰玩家数据之前断言「我到底连上了哪个库」。
func assertConnectedDatabase(handle *sql.DB) error {
	allow, err := AllowlistSpec().Resolve()
	if err != nil {
		return fmt.Errorf("resolve database allow list: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name, err := dbguard.AssertDatabase(ctx, handle, dbguard.AssertOptions{
		Expected:            config.AppConfig.ServerConfig.Database.DBName,
		Allow:               allow,
		RelaxEmptyAllowlist: RelaxEmptyAllowlist(),
		Warnf:               logx.Errorf,
	})
	if err != nil {
		return err
	}
	logx.Infof("database allowlist check passed: connected=%s allowed=%s", name, allow.String())
	return nil
}

// schemaDriftCheckTimeout 是启动期只读 schema 核对的总预算。
// 核对只有几条 information_schema / 台账的只读查询,正常是毫秒级;给 30s 是为了实例繁忙时
// 不误拒启,同时不让「库卡住」变成进程无限挂在启动阶段。
const schemaDriftCheckTimeout = 30 * time.Second

// migrateRemedyCommand 是缺列 / 缺表拒启时打印给运维的补救命令(在 go/db 目录下执行)。
// -f 必须是本进程启动时用的同一份配置:库名由其中的 ZoneId 现拼,指错就补到别的 zone 库上。
const migrateRemedyCommand = "go run ./cmd/migrate -f <本进程使用的 db 配置> -command up"

// addColumnStmtRe 从 migrate.ProtoSource.Drift 产出的补列语句里解出「表、列」,只用于拼报错。
// 语句形状由 internal/migrate/plan.go 的 Drift 固定为 ALTER TABLE `表` ADD COLUMN `列` <类型>;
// 解不出来时原样带上整条语句 —— 报错难看一点可以,漏报不行。
var addColumnStmtRe = regexp.MustCompile("^ALTER TABLE `([^`]+)` ADD COLUMN `([^`]+)`")

// assertSchemaUpToDate 是启动期 DDL 关闭时的只读 schema 闸:库里缺 proto 声明的列,或缺表且
// `cmd/migrate -command up` 能建出来时,就拒启。
//
// 为什么缺列必须拒启:消费者 Save 按 proto descriptor 写全部列、FindOneByWhereClause 按全部列
// SELECT,库里少一列,该表的每一条 db_task 都是 Error 1054 → 进死信,玩家加载也全部失败。
// 2026-09-22 zone 库缺 profile_component / asset_op_ledger 就是这样:进程照常打印
// STARTED SUCCESSFULLY,故障拖到玩家登录时才以 EnterGame preload failed 暴露。
//
// 缺表(Error 1146,后果同上)按台账分两种:
//   - 台账里没有干净的基线(空库、历史 AutoMigrate 建出来的库、基线跑到一半留了 dirty):
//     up 会先跑基线(全是 CREATE TABLE IF NOT EXISTS,幂等)再补列,所以拒启并给补救命令。
//     zone 库由 deploy/mysql-init 预建成**空库**,本地启动器与 K8s 都不跑迁移,这是最可能的真实故障;
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
			return fmt.Errorf("库 %s 缺 %d 张 proto 声明的表且台账 %s 里没有干净的基线,拒绝启动(启动期 DDL 已关闭,缺表会让对应表的每条 db_task 以 Error 1146 进死信);"+
				"在 go/db 目录执行 `%s`(先跑基线建表再补列;报 dirty 时按它的提示人工核对)后重启: %s",
				database, len(missingTables), migrate.SchemaMigrationsTable, migrateRemedyCommand, strings.Join(missingTables, ", "))
		}
		for _, name := range missingTables {
			logx.Errorf("SCHEMA-DRIFT: 库 %s 缺表 %s,但基线已应用、`cmd/migrate -command up` 不会重建它(不阻断启动);该表的每条 db_task 会以 Error 1146 进死信,需人工建表",
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
		logx.Errorf("SCHEMA-DRIFT: 库 %s 需人工过目(不阻断启动): %s", database, w)
	}
	if drift.Empty() {
		if len(missingTables) > 0 {
			logx.Errorf("schema drift check NOT clean: database=%s tables=%d missing_tables=%s review_warnings=%d(缺表未阻断启动,见上)",
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
	return fmt.Errorf("库 %s 缺 %d 个 proto 声明的列,拒绝启动(启动期 DDL 已关闭,缺列会让对应表的每条 db_task 以 Error 1054 进死信);"+
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

// InitDB 建连接、断言库白名单、注册表映射。
//
// **默认不跑任何 DDL**。历史实现在这里逐张跑 CreateOrUpdateTable,
// 见 DDLPolicy 的注释。DDL 关闭时改为只读核对库结构,缺列、或缺表且迁移能补上时
// 直接返回错误(见 assertSchemaUpToDate),调用方据此拒绝启动。
func InitDB() error {
	policy := ResolveDDLPolicy()
	logx.Infof("db DDL policy: createDatabase=%v createOrUpdateTable=%v source=%s",
		policy.AllowCreateDatabase, policy.AllowCreateOrUpdateTable, policy.Source)

	if err := openDB(policy); err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	// 注册必须无条件执行:Save / FindOneByWhereClause 都从 SqlModel.Tables
	// 取表结构,不注册就是运行期 ErrTableNotFound。历史实现把注册和建表写在
	// 同一个循环里,把 DDL 摘出去时最容易连注册一起摘掉。
	tables, err := RegisterTables()
	if err != nil {
		return err
	}

	if !policy.AllowCreateOrUpdateTable {
		logx.Infof("startup-path DDL disabled; %d tables registered without schema changes", len(tables))
		ctx, cancel := context.WithTimeout(context.Background(), schemaDriftCheckTimeout)
		defer cancel()
		return assertSchemaUpToDate(ctx, DB.DB, DB.SqlModel, config.AppConfig.ServerConfig.Database.DBName, tables)
	}
	logx.Errorf("STARTUP-DDL: 启动路径正在跑建表/补列(policy=%s)。这是 dev/兼容开关,生产应改用 cmd/migrate",
		policy.Source)
	return CreateOrUpdateTables(tables)
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
// 不一致直接拒绝启动(全局数据层决策风险 #3)。挂在 RegisterTables 里,业务服务
// 与迁移入口(internal/migrate)走同一条注册链,自动共享这道守卫。
func assertTableNameLocked(table proto.Message) error {
	md := table.ProtoReflect().Descriptor()
	declared, ok := proto2mysql.TableNameFromDescriptor(md)
	if !ok || declared == "" {
		return fmt.Errorf("表名守卫: 消息 %s 未声明 OptionTableName,拒绝按默认表名建表", md.FullName())
	}
	ddl := DB.SqlModel.GetCreateTableSQL(table)
	m := ddlTableNameRegex.FindStringSubmatch(ddl)
	if m == nil {
		return fmt.Errorf("表名守卫: 无法从 %s 的建表语句中解析表名: %s", md.FullName(), ddl)
	}
	if m[1] != declared {
		return fmt.Errorf("表名守卫: 消息 %s 生成的表名 %q 与 proto 声明 %q 不一致,拒绝启动", md.FullName(), m[1], declared)
	}
	return nil
}

// RegisterTables 把表列表注册进 SqlModel(纯内存,不产生任何 SQL)。
func RegisterTables() ([]proto.Message, error) {
	if DB == nil || DB.SqlModel == nil {
		return nil, errors.New("RegisterTables called before the database handle was opened")
	}
	tables, err := TablesFromJSON()
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		DB.SqlModel.RegisterTable(table)
		if err := assertTableNameLocked(table); err != nil {
			return nil, err
		}
	}
	return tables, nil
}

// CreateOrUpdateTables 逐张建表 / 补列。
//
// 只应由 dev 兼容开关或迁移入口调用 —— 它没有任何跨实例互斥与 lock 超时,
// 真正带这些保护的路径在 internal/migrate。
func CreateOrUpdateTables(tables []proto.Message) error {
	for _, table := range tables {
		if err := DB.SqlModel.CreateOrUpdateTable(table); err != nil {
			return fmt.Errorf("create or update table %s: %w", proto2mysql.GetTableName(table), err)
		}
	}
	return nil
}
