// migrate 是 db 的**独立 schema 迁移入口**。
//
// 它把 DDL 从业务服务的启动路径上摘了下来:db 服务启动时不再逐张跑
// CreateOrUpdateTable(那会让多 zone × 多副本并发 ALTER 同一张大表,MDL
// 阻塞把整个 zone 的存盘拖停),改由部署阶段跑一次本命令。
//
// 用法:
//
//	# 看台账(只读)
//	go run ./cmd/migrate -f etc/db.yaml -command status
//
//	# 预演:打印将要执行的语句与人工待办,不动库
//	go run ./cmd/migrate -f etc/db.yaml -command plan
//
//	# 真正执行(建表 + 补列);-storage-id 必填,只收地域存档库编号(player_store_{id}_db,id >= 1000000)
//	go run ./cmd/migrate -f etc/db.yaml -storage-id 1000000 -command up
//
//	# 库还不存在时,额外允许建库(部署首次开服)
//	go run ./cmd/migrate -f etc/db.yaml -storage-id 1000000 -command up -create-database
//
//	# 列类型漂移默认只报告;确认过影响面后才允许生成 MODIFY COLUMN
//	go run ./cmd/migrate -f etc/db.yaml -storage-id 1000000 -command up -allow-modify
//
// go/db 没有「本区主库」,zone 库已经退役(docs/design/player-data-tidb-scaling-plan.md §7.2 A17c),
// 所以不再有「不给 -storage-id 就按 ZoneId 迁 zone_{ZoneId}_db」;一次只迁一个存档库,部署侧逐库调用。
//
// 库名必须先过外部注入的白名单(DB_ALLOWED_DATABASES / AllowedDatabasesFile / AllowedDatabases),
// 或按业务服务的同一判定放行(proto_sql.AdmitStore:白名单,或 Placement.AllowStoreFamilies 且名字属于存档库家族);
// 但**建库**(-create-database)仍只认白名单 —— 家族放行只适用于已存在的库,与业务服务按需打开同口径。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"time"

	"db/internal/config"
	"db/internal/dbguard"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/migrate"
	"shared/authoritysql"
	"shared/dbplacement"
	"shared/placement"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/zeromicro/go-zero/core/conf"
	"google.golang.org/protobuf/proto"
)

func main() {
	var (
		configFile     = flag.String("f", "etc/db.yaml", "db 配置文件")
		command        = flag.String("command", "status", "status | plan | up")
		createDatabase = flag.Bool("create-database", false, "库不存在时先 CREATE DATABASE(仍需过白名单)")
		allowModify    = flag.Bool("allow-modify", false, "允许把列类型漂移变成 MODIFY COLUMN(大表上是重建表级操作,默认只报告)")
		timeout        = flag.Duration("timeout", 30*time.Minute, "整个迁移过程的总超时")
		storageID      = flag.Uint64("storage-id", 0, "必填:地域存档库编号(>= 1000000),迁 player_store_{id}_db")
	)
	flag.Parse()

	conf.MustLoad(*configFile, &config.AppConfig)
	config.AppConfig.Normalize()

	allow, err := proto_sql.AllowlistSpec().Resolve()
	if err != nil {
		log.Fatalf("resolve database allow list: %v", err)
	}
	target, runnerAllow, admittedBy, err := migrationTarget(*storageID, allow,
		config.AppConfig.Placement.StoreFamiliesAllowed(), proto_sql.RelaxEmptyAllowlist())
	if err != nil {
		log.Fatalf("resolve migration target: %v", err)
	}
	config.AppConfig.ServerConfig.Database.DBName = target
	logf("target database=%s storage=%d admitted_by=%s allowlist=%s", target, *storageID, admittedBy, runnerAllow.String())
	if admittedBy == proto_sql.StoreAdmittedByFamily && *createDatabase {
		// CreateDatabase 只认外部白名单;家族放行不延伸到建库,见包注释。提前说清楚,免得以为是 bug。
		logf("Note: %s is admitted only by Placement.AllowStoreFamilies; -create-database still requires it in the external allowlist (%s)",
			target, dbguard.AllowedDatabasesEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// 库搬走之后连当前集群;搬库期间(state=MOVING)拒绝 DDL,只读的 status、plan 照常。
	resolved, err := dbplacement.ResolveOnce(ctx, config.AppConfig.DbPlacement, target)
	if err != nil {
		log.Fatalf("read db placement: %v", err)
	}
	if *command == "up" {
		// 没有 DbPlacement 段就看不到 MOVING。status、plan 只读,不受影响。
		if err := dbplacement.RequireForMigration(config.AppConfig.DbPlacement, os.Getenv); err != nil {
			log.Fatalf("%v", err)
		}
		if err := resolved.RefuseMoving(target); err != nil {
			log.Fatalf("%v", err)
		}
	}
	if resolved.Addr != "" {
		config.AppConfig.ServerConfig.Database.Hosts = resolved.Addr
		logf("db placement: %s is on cluster %s", target, resolved.Placement.Cluster)
	}

	if *createDatabase {
		if *command != "up" {
			log.Fatalf("-create-database can only be used with -command up")
		}
		if err := proto_sql.CreateDatabase(); err != nil {
			log.Fatalf("create database: %v", err)
		}
	}

	db, err := openTarget()
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrateCfg := config.AppConfig.ServerConfig.Database.Migration
	runner := migrate.New(db, migrate.Options{
		LockWaitSeconds:       migrateCfg.LockWaitSeconds,
		InnodbLockWaitSeconds: migrateCfg.InnodbLockWaitSeconds,
		StatementTimeout:      time.Duration(migrateCfg.StatementTimeoutSeconds) * time.Second,
		AdvisoryLockTimeout:   time.Duration(migrateCfg.AdvisoryLockSeconds) * time.Second,
		Allow:                 runnerAllow,
		RelaxEmptyAllowlist:   proto_sql.RelaxEmptyAllowlist(),
		ExpectedDatabase:      target,
		DryRun:                *command == "plan",
		Logf:                  logf,
	})

	switch *command {
	case "status":
		runStatus(ctx, runner)
	case "plan", "up":
		runUp(ctx, runner, db, *allowModify)
	default:
		log.Fatalf("unknown -command %q (want status | plan | up)", *command)
	}
}

// migrationTarget 决定本次迁移的目标库、交给 runner 的有效白名单与放行依据。
//
// 库名 = placement.StoreDBName(storageID),只认地域存档库编号(>= 1000000);0、区号的值域(zone 库已退役,A17c)
// 和超过 uint32 的值都拒绝。放行判定与业务服务按需打开同一个函数(proto_sql.AdmitStore)。家族放行的库不在外部清单里,
// 而 runner 会按清单断言连上的库,所以把它并进一份**仅本次迁移使用**的有效清单;原清单不改,也不回写任何配置。
//
// 外部白名单为空且不是 dev 的 warn 档时照样拒绝:业务服务在这种部署上根本起不来(注册表拒启),
// 家族放行不能替一个没注入白名单的部署兜底。warn 档的空白名单原样交给 runner,由它打 WARN 放行,
// dev 的「宽松」保持看得见。
func migrationTarget(storageID uint64, allow dbguard.Allowlist, familiesAllowed, relaxEmptyAllowlist bool) (string, dbguard.Allowlist, string, error) {
	if storageID > math.MaxUint32 {
		return "", allow, "", fmt.Errorf("-storage-id %d exceeds uint32", storageID)
	}
	name, ok := placement.StoreDBName(uint32(storageID))
	if !ok {
		return "", allow, "", fmt.Errorf("-storage-id %d is not a player store id (want >= %d, player_store_<id>_db); zone databases are retired, see docs/design/player-data-tidb-scaling-plan.md §7.2 A17c",
			storageID, placement.FirstStorageID)
	}
	if allow.Empty() && !relaxEmptyAllowlist {
		return "", allow, "", fmt.Errorf("%w: refusing to migrate %s (-storage-id %d) without an injected allow list (set %s)",
			dbguard.ErrAllowlistMissing, name, storageID, dbguard.AllowedDatabasesEnv)
	}
	admittedBy, ok := proto_sql.AdmitStore(allow, familiesAllowed, name)
	if !ok {
		return "", allow, "", fmt.Errorf("%w: -storage-id %d derives %s, which is neither in the allow list %s nor admitted by Placement.AllowStoreFamilies=%v",
			dbguard.ErrDatabaseNotAllowed, storageID, name, allow.String(), familiesAllowed)
	}
	if admittedBy == proto_sql.StoreAdmittedByAllowlist || allow.Empty() {
		return name, allow, admittedBy, nil
	}
	effective := dbguard.Allowlist{
		Names:  append(append([]string(nil), allow.Names...), name),
		Source: allow.Source + "+placement-family",
	}
	return name, effective, admittedBy, nil
}

// migrationMysqlConfig 复用业务服务的目标与严格模式,只为迁移台账开启时间解析。
// schema_migrations 的 DATETIME 会被扫描到 time.Time / sql.NullTime;
// ParseTime=false 时驱动返回 []byte,首次建台账后的 status/plan 就会失败。
func migrationMysqlConfig() *mysql.Config {
	myCnf := proto_sql.NewMysqlConfig()
	myCnf.ParseTime = true
	return myCnf
}

// openTarget 使用迁移专用的时间解析,保留业务连接的目标和会话约束。
func openTarget() (*sql.DB, error) {
	connector, err := mysql.NewConnector(migrationMysqlConfig())
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	// 迁移是单进程串行动作,连接池给到 4 够用:1 条持锁执行 DDL,
	// 其余留给台账写入与 KILL QUERY 旁路。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func runStatus(ctx context.Context, runner *migrate.Runner) {
	database, applied, err := runner.Status(ctx)
	if err != nil {
		log.Fatalf("status: %v", err)
	}
	if len(applied) == 0 {
		logf("database=%s has no %s ledger yet (or the ledger is empty); run -command up to establish the baseline", database, migrate.SchemaMigrationsTable)
		return
	}
	logf("database=%s applied %d migration(s):", database, len(applied))
	dirty := 0
	for _, a := range applied {
		state := "ok"
		if a.Dirty {
			state = "DIRTY"
			dirty++
		}
		logf("  version=%-4d %-28s %s checksum=%s", a.Version, a.Name, state, a.Checksum[:12])
	}
	if dirty > 0 {
		logf("Found %d dirty record(s): the next up will be rejected; first verify manually whether that version's statements took effect", dirty)
		os.Exit(2)
	}
}

func runUp(ctx context.Context, runner *migrate.Runner, db *sql.DB, allowModify bool) {
	model, tables, err := buildModel(db)
	if err != nil {
		log.Fatalf("build proto model: %v", err)
	}
	src := &migrate.ProtoSource{Model: model, Tables: tables, AllowModifyColumn: allowModify}

	report, err := runner.Up(ctx, src)
	if err != nil {
		if errors.Is(err, migrate.ErrAdvisoryLockBusy) {
			// 另一个副本正在迁移同一个库。这正是本工具存在的目的,退 3 让
			// 部署脚本能区分「真失败」和「别人在跑」。
			logf("%v", err)
			os.Exit(3)
		}
		log.Fatalf("migrate up: %v", err)
	}

	logf("database=%s applied=%d skipped=%d dryRun=%v",
		report.Database, len(report.Applied), len(report.Skipped), report.DryRun)
	for _, m := range report.Applied {
		logf("  applied  version=%d %s (%d statements)", m.Version, m.Name, len(m.Statements))
	}
	for _, m := range report.Skipped {
		logf("  skipped  version=%d %s", m.Version, m.Name)
	}
	for _, w := range report.Warnings {
		logf("  NEEDS-REVIEW %s", w)
	}
	// 闸门跟着逻辑库走:每个落点库自带一份。建表和种行都幂等,已有的一个字节不动。
	if !report.DryRun {
		if err := authoritysql.EnsureCatalogSchema(ctx, db, report.Database); err != nil {
			log.Fatalf("ensure writer gate tables on %s: %v", report.Database, err)
		}
	}
	if len(report.Warnings) > 0 {
		// 退 4 = 迁移本身成功,但有需要人工过目的漂移。部署脚本可以据此告警
		// 而不中断发布。
		os.Exit(4)
	}
}

// buildModel 打开 proto2mysql 模型并注册全部表(纯内存,不产生 SQL)。
func buildModel(db *sql.DB) (*proto2mysql.DB, []proto.Message, error) {
	tables, err := proto_sql.TablesFromJSON()
	if err != nil {
		return nil, nil, err
	}
	model := proto2mysql.NewDB()
	if err := model.OpenDB(db, config.AppConfig.ServerConfig.Database.DBName); err != nil {
		return nil, nil, err
	}
	for _, t := range tables {
		model.RegisterTable(t)
	}
	return model, tables, nil
}

func logf(format string, args ...any) {
	fmt.Printf("[migrate] "+format+"\n", args...)
}
