// migrate 是 db 的**独立 schema 迁移入口**。
//
// 它把 DDL 从业务服务的启动路径上摘了下来:db 服务启动时不再逐张跑
// CreateOrUpdateTable(那会让多 zone × 多副本并发 ALTER 同一张大表,MDL
// 阻塞把整个 zone 的 db_task 消费拖停),改由部署阶段跑一次本命令。
//
// 用法:
//
//	# 看台账(只读)
//	go run ./cmd/migrate -f etc/db.yaml -command status
//
//	# 预演:打印将要执行的语句与人工待办,不动库
//	go run ./cmd/migrate -f etc/db.yaml -command plan
//
//	# 真正执行(建表 + 补列)
//	go run ./cmd/migrate -f etc/db.yaml -command up
//
//	# 库还不存在时,额外允许建库(部署首次开服)
//	go run ./cmd/migrate -f etc/db.yaml -command up -create-database
//
//	# 列类型漂移默认只报告;确认过影响面后才允许生成 MODIFY COLUMN
//	go run ./cmd/migrate -f etc/db.yaml -command up -allow-modify
//
// 无论哪条路径,库名都必须先过外部注入的白名单(DB_ALLOWED_DATABASES /
// AllowedDatabasesFile / AllowedDatabases),否则拒绝执行。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/migrate"

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
	)
	flag.Parse()

	conf.MustLoad(*configFile, &config.AppConfig)
	config.AppConfig.Normalize()
	if config.AppConfig.ZoneId == 0 {
		log.Fatalf("ZoneId must be set in %s (> 0)", *configFile)
	}
	config.AppConfig.ServerConfig.Database.DBName = config.ZoneDBName(config.AppConfig.ZoneId)

	allow, err := proto_sql.AllowlistSpec().Resolve()
	if err != nil {
		log.Fatalf("resolve database allow list: %v", err)
	}
	target := config.AppConfig.ServerConfig.Database.DBName
	logf("target database=%s zone=%d allowlist=%s", target, config.AppConfig.ZoneId, allow.String())

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *createDatabase {
		if *command != "up" {
			log.Fatalf("-create-database 只能配合 -command up 使用")
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
		Allow:                 allow,
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
		logf("database=%s 尚无 %s 台账(或台账为空);跑 -command up 建立基线", database, migrate.SchemaMigrationsTable)
		return
	}
	logf("database=%s 已应用 %d 条迁移:", database, len(applied))
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
		logf("发现 %d 条 dirty 记录:下一次 up 会被拒绝,请先人工核对该版本的语句是否已生效", dirty)
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
