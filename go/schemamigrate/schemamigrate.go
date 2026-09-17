// Package schemamigrate 是「每服务一库、表以 proto 为源」的新建表服务共用的 schema 迁移器。
//
// # 来源
//
// 按 docs/design/xuanming-port-decisions-20260910.md D-14 第 3 条,从 go/db/internal/migrate
// (plan.go 的 ProtoSource + runner.go)抽成独立 module,供 trade、mail 等全局服务的
// `-migrate` 入口与 dev 档启动期 AutoMigrate 共用。go/db 本轮不切换,两份代码暂时并存;
// 台账表形状与锁名前缀刻意与 go/db 保持一致,方便 go/db 日后直接换用本包。
//
// 为什么不直接 import go/db:internal 包跨 module 导入不了;为什么不放 go/shared:
// proto2mysql 要求 go 1.26.5,放进 shared 会把所有依赖 shared 的 module 的 go 指令一起抬高。
//
// # 五项保证(与 go/db runner 相同)
//
//  1. 版本台账:schema_migrations(建表语句与 go/db 逐字一致)记录 version / name /
//     checksum / dirty。任何一行 dirty 就拒绝继续(ErrDirty),逼人工核对,而不是在
//     未知结构上继续叠加 DDL。
//  2. 跨实例互斥:GET_LOCK("mmorpg_db_migrate:<库名>")。前缀与 go/db 相同,同一个库上
//     两个工具也互斥;多副本并发启动时只有一个实例在改结构。
//  3. 有超时:会话级 lock_wait_timeout / innodb_lock_wait_timeout(DDL 撞上长事务时秒级
//     失败,不把整张表的 MDL 队列堵死),外加每条 DDL 的硬超时与旁路 KILL QUERY。
//     GET_LOCK、SET SESSION、全部 DDL 与台账写入都在同一条 *sql.Conn 上。
//  4. 库名断言:连接后 SELECT DATABASE() 必须等于 Options.Database,否则 ErrDatabaseMismatch。
//  5. 漂移默认只 ADD COLUMN(外加对缺失的表 CREATE TABLE IF NOT EXISTS,见下方差异):
//     列类型漂移、缺主键、表清单异常只进 Report.Manual;AllowModifyColumn=true 才把类型漂移
//     变成 MODIFY COLUMN;多余列 / 多余表只进 Warnings,永不删除。
//
// # 与 go/db/internal/migrate 的差异
//
//   - 修掉「后加表建不出来」:go/db 的基线(version=1)跑过之后再往表清单里加表,基线
//     checksum 变了也不重跑,漂移阶段只打一句 WARN,新表永远不存在。本包在漂移阶段对
//     information_schema 里查不到的表生成 CREATE TABLE IF NOT EXISTS,与 ADD COLUMN 一起
//     记成一条漂移迁移。
//   - 表名取 OptionTableName(proto2mysql.TableNameFromDescriptor)。proto2mysql v0.1.1 的
//     GetTableName 返回 proto full name(带 package),拿它去比 information_schema 会把每张
//     带 package 的表都当成「不存在」。
//   - 主键必须由 proto 声明,且只能是整数 / 枚举列(D-14 第 2 条),否则表清单校验直接失败。
//     没有 go/db 的 OptionIsPlayerDatabase 回退,也不再「给缺主键的建表语句补主键」——
//     新服务的表一律显式写 OptionPrimaryKey。
//   - 已记账的漂移 checksum 再次出现,说明台账记着执行过但库结构仍不符(表或列被人工删掉 /
//     回退),进 Manual,既不重跑也不静默跳过(go/db 会静默跳过)。
//   - 主键列不符、缺唯一键同样进 Manual(去重语义失效,补建前要先去重,只能人工做);
//     缺普通索引只进 Warnings(只影响性能,也不自动建)。go/db 只查主键是否存在,不看列、不看索引。
//   - 库里有、表清单里没有的表只进 Warnings(多余表),与多余列同一口径:新版本加的表 / 列在
//     回滚到旧版本二进制之后就是「多余」的,当成阻断项会让回滚后的副本起不来。本包永不删表、删列。
//   - 超时 KILL 不止在硬超时触发:调用方 ctx 被取消时同样旁路 KILL QUERY,因为驱动断开
//     连接后服务端那条 DDL 仍会跑完并继续持锁。除到期即杀的守卫 goroutine 外,语句带错误返回且
//     ctx 已到期时再同步补杀一次(最多发一次),不依赖守卫与驱动 watcher 之间的调度顺序。
//   - 没有 SQLSource(D-14:表只以 proto 为源);没有 dbguard 白名单(库名断言自带,见保证 4);
//     不提供建库(建库只登记在 deploy/mysql-init/00_init_zone_dbs.sql);没有 DryRun 开关,
//     改为独立的只读 Plan。
//   - 台账时间列扫进字符串,不依赖 DSN 是否开 parseTime。
//
// # 台账语义
//
//   - version=1、name=0001_baseline_from_proto:首次迁移按表清单建全套表(全部是
//     CREATE TABLE IF NOT EXISTS)。checksum = 表名集合排序后的 sha256,之后表清单再增删
//     都不重跑它,只在日志里提示 checksum 变化。
//   - version>=2、name=auto_proto_sync_<checksum 前 12 位>:每次 Up 按真实库结构算出的漂移
//     (缺表 CREATE TABLE IF NOT EXISTS + 缺列 ADD COLUMN [+ 授权后的 MODIFY COLUMN])。
//     version 取台账最大值 +1,checksum = 语句集合排序后的 sha256。
//   - 执行前先写 dirty=1 行,全部语句成功后改 dirty=0;中途失败保留 dirty,下一次 Up / Plan 拒绝。
//   - 兼容 go/db 已建的台账:台账表结构逐字相同;基线只看 version=1 是否存在,不比 checksum
//     (两边算表名集合的口径不必一致);漂移版本号接在已有最大号后面;go/db 基线漏建的表会在
//     本包的漂移里补建。
//
// # 调用方约束
//
//   - DSN 必须已选中 Options.Database。本包不 USE、不建库;库不存在 / 连不上时返回的错误点名库名。
//   - *sql.DB 连接池至少允许 2 条连接(MaxOpenConns 为 0 或 >=2):1 条持锁执行 DDL,
//     另 1 条留给超时后的 KILL QUERY。MaxOpenConns=1 直接拒绝。
//   - 账号需要 CREATE / ALTER / INSERT / UPDATE 权限,以及对自身连接执行 KILL QUERY 的权限
//     (MySQL 允许用户 KILL 自己的连接,无需 SUPER / CONNECTION_ADMIN)。
//
// # 待验证
//
// 以下语义只在 MySQL 8 上按文档设计,尚未在 TiDB 上验证,待 Codex 在 TiDB 冒烟确认:
// GET_LOCK / RELEASE_LOCK 在目标 TiDB 版本上是否可用及其互斥语义;KILL QUERY 经负载均衡
// 落到另一个 TiDB 实例时能否杀掉本连接上的 DDL(global kill);TiDB 在线 DDL 下
// lock_wait_timeout 是否仍起作用。
package schemamigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
)

// 零值 Options 对应的安全默认值。
const (
	// DefaultAdvisoryLockWait 是 GET_LOCK 的等待时长。迁移 Job 由 backoff 重试锁忙(退出码 3),
	// 不需要在一次运行里长时间排队。
	DefaultAdvisoryLockWait = 10 * time.Second
	// DefaultSessionLockWait 同时用于 lock_wait_timeout 与 innodb_lock_wait_timeout。
	// MySQL 默认的 lock_wait_timeout 是一年:一条 ALTER 排在长事务后面会把该表的 MDL 队列堵死。
	DefaultSessionLockWait = 5 * time.Second
	// DefaultStatementTimeout 是单条 DDL 的硬超时。新服务的表小,MySQL 8 的 ADD COLUMN 走
	// INSTANT;超过 60s 说明撞上了意料之外的重建表,宁可 KILL 后留 dirty 交人工。
	DefaultStatementTimeout = 60 * time.Second
)

// ExitCode 的取值(D-14 第 4 条),供 `-migrate` 入口与 K8s Job podFailurePolicy 对齐。
const (
	ExitOK       = 0 // 成功,含只有 Warnings(多余列)的情况
	ExitFailed   = 1 // 失败:连不上 / 库名不符 / dirty / DDL 出错 / 表清单校验失败
	ExitLockBusy = 3 // 锁被其他实例持有,可重试
	ExitManual   = 4 // 迁移本身没出错,但有需人工处理的项
)

// Options 是一次迁移的输入与安全阀。
type Options struct {
	Database          string          // 必填;连接后 SELECT DATABASE() 必须等于它,否则 ErrDatabaseMismatch
	Tables            []proto.Message // 必填、非空;每个 message 必须带 OptionTableName,表名不许重复
	AllowModifyColumn bool            // 默认 false:类型漂移只进 Manual
	AdvisoryLockWait  time.Duration   // GET_LOCK 等待,0 → 10s
	SessionLockWait   time.Duration   // SET SESSION lock_wait_timeout / innodb_lock_wait_timeout,0 → 5s
	StatementTimeout  time.Duration   // 每条 DDL 硬超时(超时旁路 KILL QUERY),0 → 60s
	Logf              func(format string, args ...any) // nil → 不打日志
}

// withDefaults 把零值与非法负值补成默认值,不修改调用方的副本。
func (o Options) withDefaults() Options {
	if o.AdvisoryLockWait <= 0 {
		o.AdvisoryLockWait = DefaultAdvisoryLockWait
	}
	if o.SessionLockWait <= 0 {
		o.SessionLockWait = DefaultSessionLockWait
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = DefaultStatementTimeout
	}
	return o
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Report 是 Plan / Up 的结果。
type Report struct {
	// Statements:Plan 为待执行语句(基线 + 漂移);Up 为本次真正执行成功的语句。
	// 台账表自身的建表语句不计入。
	Statements []string
	// Warnings 只报告不阻断(如多余列,本包永不删列)。
	Warnings []string
	// Manual 需人工处理,非空时 ExitCode=4:列类型漂移、缺主键或主键列不符、缺唯一键,
	// 以及表清单异常 —— 台账记着某条漂移(建表 / 加列)已完成,库里却又缺了(表或列被人工删除 / 回退)。
	// 表清单本身写错(缺表名、重名、非整数主键等)不在这里,是 Plan / Up 直接返回错误(ExitCode=1)。
	Manual []string
}

// Clean 表示没有待执行语句、也没有需人工项。
func (r Report) Clean() bool {
	return len(r.Statements) == 0 && len(r.Manual) == 0
}

var (
	// ErrLockBusy 表示迁移锁被其他实例持有(等待 AdvisoryLockWait 后仍拿不到)。
	ErrLockBusy error = errors.New("schemamigrate: 迁移锁被其他实例持有")
	// ErrDatabaseMismatch 表示连接实际选中的库与 Options.Database 不一致(或未选库)。
	ErrDatabaseMismatch error = errors.New("schemamigrate: 连接的库与期望库名不一致")
	// ErrDirty 表示台账存在 dirty 行:上一次迁移中途失败,库结构处于未知状态。
	ErrDirty error = errors.New("schemamigrate: schema_migrations 存在 dirty 行,拒绝继续")
)

// Plan 只读地计算把库对齐到 Tables 需要执行的语句与需人工项。
//
// 不建台账、不执行 DDL、不写台账;为了读到一致的结构,同样会在同一条连接上
// SET SESSION 超时并持有迁移锁(二者都是会话级,断开即消)。台账有 dirty 行时返回 ErrDirty。
// 服务在 Schema.AutoMigrate=false 时用它做启动门禁:!Clean() 就拒绝启动。
func Plan(ctx context.Context, db *sql.DB, opts Options) (Report, error) {
	return run(ctx, db, opts, false)
}

// Up 在迁移锁下执行基线 + 漂移,没有 down。
//
// 可重复执行:库已对齐时 Report.Statements 为空。Manual 非空时仍会执行其余安全的
// 语句(CREATE TABLE IF NOT EXISTS / ADD COLUMN),由调用方按 ExitCode=4 决定是否放行。
// 出错时返回的 Report 里是出错前已成功执行的语句。
func Up(ctx context.Context, db *sql.DB, opts Options) (Report, error) {
	return run(ctx, db, opts, true)
}

// ExitCode 按 D-14:err==nil 且 len(Manual)==0 → 0;errors.Is(err, ErrLockBusy) → 3;其他 err → 1;len(Manual)>0 → 4。
func ExitCode(r Report, err error) int {
	switch {
	case errors.Is(err, ErrLockBusy):
		return ExitLockBusy
	case err != nil:
		return ExitFailed
	case len(r.Manual) > 0:
		return ExitManual
	default:
		return ExitOK
	}
}

// run 是 Plan 与 Up 的共同流程;apply=false 时不产生任何持久写入。
func run(ctx context.Context, db *sql.DB, opts Options, apply bool) (Report, error) {
	var report Report
	if db == nil {
		return report, errors.New("schemamigrate: *sql.DB 为 nil")
	}
	opts = opts.withDefaults()
	if opts.Database == "" {
		return report, errors.New("schemamigrate: Options.Database 必填")
	}
	// 表清单校验是纯内存动作,放在碰库之前:清单写错不应该先拿锁、建台账。
	sch, err := buildSchema(opts.Tables)
	if err != nil {
		return report, err
	}
	// 连接池只允许 1 条连接时,超时后的 KILL QUERY 拿不到旁路连接,只能等到 killTimeout 放弃,
	// 而服务端那条 DDL 仍在锁表 —— 硬超时形同虚设,所以直接拒绝。
	if db.Stats().MaxOpenConnections == 1 {
		return report, fmt.Errorf("schemamigrate: 库 %q 的连接池 MaxOpenConns=1,至少需要 2(1 条执行 DDL,1 条留给超时后的 KILL QUERY)", opts.Database)
	}

	mode := "plan"
	if apply {
		mode = "up"
	}
	opts.logf("schemamigrate %s: database=%s tables=%d allowModifyColumn=%v", mode, opts.Database, len(sch.tables), opts.AllowModifyColumn)

	s, err := openSession(ctx, db, opts)
	if err != nil {
		return report, err
	}
	defer s.close()

	release, err := s.acquireLock(ctx)
	if err != nil {
		return report, err
	}
	defer release()

	applied, err := s.loadLedger(ctx, apply)
	if err != nil {
		return report, err
	}
	if err := refuseDirty(applied); err != nil {
		return report, err
	}

	baseline := sch.baseline()
	baselinePending := !baselineRecorded(baseline, applied)
	if baselinePending {
		if !apply {
			report.Statements = append(report.Statements, baseline.Statements...)
		} else {
			executed, err := s.apply(ctx, baseline, applied)
			report.Statements = append(report.Statements, executed...)
			if err != nil {
				return report, err
			}
		}
	} else if prev, ok := applied[BaselineVersion]; ok && prev.Checksum != baseline.Checksum {
		// 表清单增删过。新增的表由下面的漂移补建,删掉的表由漂移报成 Warnings(多余表,不删),
		// 所以这里只留痕,不重跑基线。
		opts.logf("基线 version=%d 已应用,表清单 checksum 已变化(台账=%s 当前=%s);新增表由漂移迁移补建",
			BaselineVersion, shortSum(prev.Checksum), shortSum(baseline.Checksum))
	}

	live, err := loadLiveSchema(ctx, s.conn, opts.Database)
	if err != nil {
		return report, err
	}
	d := sch.drift(live, driftOptions{
		AllowModifyColumn: opts.AllowModifyColumn,
		// 基线还没执行时(只会出现在 Plan),缺的表由基线的 CREATE TABLE IF NOT EXISTS 建,
		// 漂移里再算一遍会让计划重复。Up 路径上基线已经执行完,此时查不到的表就是真缺。
		CreateMissingTables: apply || !baselinePending,
	})
	report.Warnings = d.warnings
	report.Manual = d.manual

	if !d.migration.Empty() {
		if prev, ok := findChecksum(applied, d.migration.Checksum); ok {
			// 同一组语句台账说执行过,库结构却仍然不符:重跑会掩盖「有人删了表 / 列」的事故,
			// 静默跳过会让服务带着缺表缺列启动,两种都不对,交人工。
			report.Manual = append(report.Manual, fmt.Sprintf(
				"漂移迁移 %s 已在台账 version=%d 记为完成,但库结构仍与 proto 不符(表或列可能被人工删除 / 回退),未重跑;待执行语句:%s",
				d.migration.Name, prev.Version, strings.Join(collapseAll(d.migration.Statements), "; ")))
		} else if !apply {
			report.Statements = append(report.Statements, d.migration.Statements...)
		} else {
			d.migration.Version = nextVersion(applied)
			executed, err := s.apply(ctx, d.migration, applied)
			report.Statements = append(report.Statements, executed...)
			if err != nil {
				return report, err
			}
		}
	}

	opts.logf("schemamigrate %s 完成: database=%s statements=%d warnings=%d manual=%d",
		mode, opts.Database, len(report.Statements), len(report.Warnings), len(report.Manual))
	for _, w := range report.Warnings {
		opts.logf("  WARN %s", w)
	}
	for _, m := range report.Manual {
		opts.logf("  NEEDS-MANUAL %s", m)
	}
	return report, nil
}
