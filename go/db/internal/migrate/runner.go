package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"db/internal/dbguard"
)

// SchemaMigrationsTable 是版本台账表名。
const SchemaMigrationsTable = "schema_migrations"

// advisoryLockPrefix + 库名构成 GET_LOCK 的锁名。
//
// 锁名带库名是刻意的:不同 zone 是不同的库、不同的表,让它们并行迁移;
// 同一个库的多个副本 / 多次部署则必须串行 —— 那正是「并发 ALTER 同一张大表
// 把整个 zone 的 db_task 消费拖停」的场景。MySQL 的 GET_LOCK 名字上限 64 字节。
const advisoryLockPrefix = "mmorpg_db_migrate:"

// ErrDirtySchema 表示上一次迁移中途失败,库结构处于未知状态。
var ErrDirtySchema = errors.New("schema_migrations has a dirty version; refusing to migrate")

// ErrAdvisoryLockBusy 表示另一个迁移进程正在跑同一个库。
var ErrAdvisoryLockBusy = errors.New("another migration is already running for this database")

// Options 是迁移的安全阀。
type Options struct {
	// LockWaitSeconds 会话级 MDL 等待上限(MySQL lock_wait_timeout)。
	LockWaitSeconds int
	// InnodbLockWaitSeconds 会话级行锁等待上限(innodb_lock_wait_timeout)。
	InnodbLockWaitSeconds int
	// StatementTimeout 单条语句硬超时;超时后从旁路连接 KILL QUERY。
	StatementTimeout time.Duration
	// AdvisoryLockTimeout GET_LOCK 的等待时长。
	AdvisoryLockTimeout time.Duration
	// Allow 是库名白名单,与业务服务共用同一份。
	Allow dbguard.Allowlist
	// RelaxEmptyAllowlist 只在本地 dev 为 true。
	RelaxEmptyAllowlist bool
	// ExpectedDatabase 是调用方按 ZoneId 推导出的库名,非空时必须与
	// 服务端 DATABASE() 一致。
	ExpectedDatabase string
	// DryRun 只打印不执行(台账也不写)。
	DryRun bool
	// Logf 输出执行轨迹,为空时丢弃。
	Logf func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// AppliedMigration 是台账里的一行。
type AppliedMigration struct {
	Version    int64
	Name       string
	Checksum   string
	Dirty      bool
	StartedAt  time.Time
	FinishedAt sql.NullTime
}

// Report 是一次 Up 的结果。
type Report struct {
	Database string
	// Applied 是本次真正执行的迁移。
	Applied []Migration
	// Skipped 是判定为已应用而跳过的迁移。
	Skipped []Migration
	// Warnings 是需要人工过目但不自动处理的项(类型漂移 / 多余列 / 缺主键)。
	Warnings []string
	// DryRun 标记本次是否只是预演。
	DryRun bool
}

// Runner 执行迁移。
type Runner struct {
	db   *sql.DB
	opts Options
}

// New 构造 Runner。opts 里的零值会被补成安全默认值。
func New(db *sql.DB, opts Options) *Runner {
	if opts.LockWaitSeconds <= 0 {
		opts.LockWaitSeconds = 5
	}
	if opts.InnodbLockWaitSeconds <= 0 {
		opts.InnodbLockWaitSeconds = 5
	}
	if opts.StatementTimeout <= 0 {
		opts.StatementTimeout = 5 * time.Minute
	}
	if opts.AdvisoryLockTimeout <= 0 {
		opts.AdvisoryLockTimeout = time.Minute
	}
	return &Runner{db: db, opts: opts}
}

// Up 在一把跨实例互斥锁下执行 baseline + drift。没有 down。
func (r *Runner) Up(ctx context.Context, src Source) (Report, error) {
	report := Report{DryRun: r.opts.DryRun}

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return report, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	database, err := r.assertDatabase(ctx, conn)
	if err != nil {
		return report, err
	}
	report.Database = database

	connID, err := r.connectionID(ctx, conn)
	if err != nil {
		return report, err
	}
	if err := r.prepareSession(ctx, conn); err != nil {
		return report, err
	}

	release, err := r.acquireAdvisoryLock(ctx, conn, database)
	if err != nil {
		return report, err
	}
	defer release()

	if err := r.ensureLedger(ctx, conn, connID); err != nil {
		return report, err
	}
	applied, err := r.loadApplied(ctx, conn)
	if err != nil {
		return report, err
	}
	for _, a := range applied {
		if a.Dirty {
			return report, fmt.Errorf("%w: version=%d name=%s started_at=%s;先人工核对该版本的语句是否已生效,确认后再 DELETE/UPDATE 掉这一行",
				ErrDirtySchema, a.Version, a.Name, a.StartedAt.Format(time.RFC3339))
		}
	}

	baseline, err := src.Baseline()
	if err != nil {
		return report, err
	}
	ran, err := r.applyOne(ctx, conn, connID, baseline, applied)
	if err != nil {
		return report, err
	}
	if ran {
		report.Applied = append(report.Applied, baseline)
	} else {
		report.Skipped = append(report.Skipped, baseline)
	}

	drift, warnings, err := src.Drift(ctx, conn, database)
	if err != nil {
		return report, err
	}
	report.Warnings = warnings
	if drift.Empty() {
		return report, nil
	}
	if drift.Version == 0 {
		drift.Version = nextVersion(applied)
	}
	ran, err = r.applyOne(ctx, conn, connID, drift, applied)
	if err != nil {
		return report, err
	}
	if ran {
		report.Applied = append(report.Applied, drift)
	} else {
		report.Skipped = append(report.Skipped, drift)
	}
	return report, nil
}

// Status 只读地列出台账。
func (r *Runner) Status(ctx context.Context) (string, []AppliedMigration, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	database, err := r.assertDatabase(ctx, conn)
	if err != nil {
		return "", nil, err
	}
	exists, err := r.ledgerExists(ctx, conn, database)
	if err != nil {
		return database, nil, err
	}
	if !exists {
		return database, nil, nil
	}
	applied, err := r.loadApplied(ctx, conn)
	if err != nil {
		return database, nil, err
	}
	out := make([]AppliedMigration, 0, len(applied))
	for _, a := range applied {
		out = append(out, a)
	}
	sortApplied(out)
	return database, out, nil
}

// assertDatabase 复用业务服务那道白名单闸:迁移能改的库 ⊆ 服务能写的库。
func (r *Runner) assertDatabase(ctx context.Context, conn *sql.Conn) (string, error) {
	return dbguard.AssertDatabase(ctx, conn, dbguard.AssertOptions{
		Expected:            r.opts.ExpectedDatabase,
		Allow:               r.opts.Allow,
		RelaxEmptyAllowlist: r.opts.RelaxEmptyAllowlist,
		Warnf:               r.opts.Logf,
	})
}

func (r *Runner) connectionID(ctx context.Context, conn *sql.Conn) (uint64, error) {
	var id uint64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		return 0, fmt.Errorf("read migration CONNECTION_ID(): %w", err)
	}
	return id, nil
}

// prepareSession 把两种锁等待都收到秒级。
//
// 这一步是「DDL 撞上长事务时快速失败」的唯一开关:默认 lock_wait_timeout
// 是 31536000 秒(一年),一条 ALTER 排在长事务后面会把该表的 MDL 队列整个
// 堵死,后面所有 db_task 的 Save 一起挂住 —— 表现就是整个 zone 消费停摆。
func (r *Runner) prepareSession(ctx context.Context, conn *sql.Conn) error {
	lockWait := clampInt(r.opts.LockWaitSeconds, 1, 31536000)
	innodbWait := clampInt(r.opts.InnodbLockWaitSeconds, 1, 1073741824)
	stmt := fmt.Sprintf("SET SESSION lock_wait_timeout = %d, innodb_lock_wait_timeout = %d", lockWait, innodbWait)
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("set migration session lock timeouts: %w", err)
	}
	r.opts.logf("migration session: lock_wait_timeout=%ds innodb_lock_wait_timeout=%ds statement_timeout=%s",
		lockWait, innodbWait, r.opts.StatementTimeout)
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// acquireAdvisoryLock 取跨实例互斥锁,返回释放函数。
func (r *Runner) acquireAdvisoryLock(ctx context.Context, conn *sql.Conn, database string) (func(), error) {
	name := advisoryLockName(database)
	timeout := int(r.opts.AdvisoryLockTimeout / time.Second)
	if timeout <= 0 {
		timeout = 1
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, timeout).Scan(&got); err != nil {
		return nil, fmt.Errorf("acquire migration advisory lock %q: %w", name, err)
	}
	if !got.Valid || got.Int64 != 1 {
		return nil, fmt.Errorf("%w: lock=%q waited=%ds", ErrAdvisoryLockBusy, name, timeout)
	}
	r.opts.logf("migration advisory lock held: %s", name)
	return func() {
		// 用独立的 context:即使调用方的 ctx 已经取消,锁也必须放掉。
		// (连接断开时 MySQL 会自动释放,这里只是让正常路径干净收尾。)
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released sql.NullInt64
		if err := conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil {
			r.opts.logf("release migration advisory lock %q failed (MySQL 会在连接断开时自动释放): %v", name, err)
		}
	}, nil
}

func advisoryLockName(database string) string {
	name := advisoryLockPrefix + database
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// ensureLedger 建版本台账表。它本身也是 DDL,同样走带超时的执行路径。
func (r *Runner) ensureLedger(ctx context.Context, conn *sql.Conn, connID uint64) error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  version BIGINT UNSIGNED NOT NULL,
  name VARCHAR(191) NOT NULL,
  checksum CHAR(64) NOT NULL,
  dirty TINYINT(1) NOT NULL DEFAULT 0,
  started_at DATETIME NOT NULL,
  finished_at DATETIME NULL,
  PRIMARY KEY (version),
  KEY idx_schema_migrations_checksum (checksum)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='db schema migration ledger'`,
		quoteIdent(SchemaMigrationsTable))
	// DryRun 也要建台账:没有台账就读不出「已应用哪些版本」,预演会把所有
	// 迁移都报成待执行,失去预演的意义。建空表本身无风险。
	return r.execDDL(ctx, conn, connID, ddl, true)
}

func (r *Runner) ledgerExists(ctx context.Context, conn *sql.Conn, database string) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
		database, SchemaMigrationsTable).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", SchemaMigrationsTable, err)
	}
	return count > 0, nil
}

func (r *Runner) loadApplied(ctx context.Context, conn *sql.Conn) (map[int64]AppliedMigration, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(
		"SELECT version, name, checksum, dirty, started_at, finished_at FROM %s", quoteIdent(SchemaMigrationsTable)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", SchemaMigrationsTable, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]AppliedMigration)
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.Dirty, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan %s: %w", SchemaMigrationsTable, err)
		}
		out[a.Version] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", SchemaMigrationsTable, err)
	}
	return out, nil
}

func nextVersion(applied map[int64]AppliedMigration) int64 {
	var max int64
	for v := range applied {
		if v > max {
			max = v
		}
	}
	return max + 1
}

func sortApplied(list []AppliedMigration) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Version < list[j-1].Version; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// applyOne 执行一条迁移。返回 ran=false 表示判定为已应用而跳过。
func (r *Runner) applyOne(ctx context.Context, conn *sql.Conn, connID uint64, m Migration, applied map[int64]AppliedMigration) (bool, error) {
	if m.Empty() {
		return false, nil
	}
	if prev, ok := applied[m.Version]; ok {
		if prev.Checksum != m.Checksum {
			// 基线的 checksum 只算表名集合,列变更走漂移迁移,所以这里出现
			// 差异意味着「表增删了」。不自动重跑,交人工确认。
			r.opts.logf("WARN: 迁移 version=%d 已应用但 checksum 变了(台账=%s 当前=%s);不重跑,请人工确认表清单变更",
				m.Version, prev.Checksum, m.Checksum)
		}
		return false, nil
	}
	for _, a := range applied {
		if a.Checksum == m.Checksum {
			return false, nil
		}
	}

	if r.opts.DryRun {
		r.opts.logf("[dry-run] version=%d name=%s 共 %d 条语句:", m.Version, m.Name, len(m.Statements))
		for _, s := range m.Statements {
			r.opts.logf("[dry-run]   %s", collapse(s))
		}
		return false, nil
	}

	if err := r.beginLedger(ctx, m); err != nil {
		return false, err
	}
	for _, stmt := range m.Statements {
		r.opts.logf("version=%d 执行: %s", m.Version, collapse(stmt))
		if err := r.execDDL(ctx, conn, connID, stmt, false); err != nil {
			// 刻意保留 dirty=1:下一次 Up 会直接拒绝,逼人工核对而不是在
			// 未知结构上继续叠加 DDL。
			return false, fmt.Errorf("migration version=%d name=%s failed (ledger left dirty): %w", m.Version, m.Name, err)
		}
	}
	if err := r.finishLedger(ctx, m); err != nil {
		return false, err
	}
	applied[m.Version] = AppliedMigration{Version: m.Version, Name: m.Name, Checksum: m.Checksum}
	return true, nil
}

func (r *Runner) beginLedger(ctx context.Context, m Migration) error {
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO %s (version, name, checksum, dirty, started_at) VALUES (?, ?, ?, 1, NOW())",
		quoteIdent(SchemaMigrationsTable)), m.Version, m.Name, m.Checksum)
	if err != nil {
		return fmt.Errorf("claim migration version=%d: %w", m.Version, err)
	}
	return nil
}

func (r *Runner) finishLedger(ctx context.Context, m Migration) error {
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s SET dirty = 0, finished_at = NOW() WHERE version = ?",
		quoteIdent(SchemaMigrationsTable)), m.Version)
	if err != nil {
		return fmt.Errorf("finalize migration version=%d (ledger left dirty): %w", m.Version, err)
	}
	return nil
}

// execDDL 带硬超时地执行一条 DDL。
//
// 三层超时缺一不可:
//   - 会话级 lock_wait_timeout —— 拿不到 MDL 时秒级失败;
//   - context 硬超时 —— 拿到锁但语句本身跑太久时,客户端不再无限等;
//   - 旁路 KILL QUERY —— context 超时只让客户端放手,**服务端还在跑**。
//     不 KILL 的话工具已经退出而 ALTER 仍在锁表,是最难排查的一种事故。
func (r *Runner) execDDL(ctx context.Context, conn *sql.Conn, connID uint64, stmt string, allowInDryRun bool) error {
	if r.opts.DryRun && !allowInDryRun {
		r.opts.logf("[dry-run] %s", collapse(stmt))
		return nil
	}

	execCtx, cancel := context.WithTimeout(ctx, r.opts.StatementTimeout)
	defer cancel()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-done:
		case <-execCtx.Done():
			if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
				r.killQuery(connID)
			}
		}
	}()

	_, err := conn.ExecContext(execCtx, stmt)
	close(done)
	wg.Wait()

	if err != nil {
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("statement exceeded %s and was killed: %s: %w", r.opts.StatementTimeout, collapse(stmt), err)
		}
		return fmt.Errorf("exec %s: %w", collapse(stmt), err)
	}
	return nil
}

// killQuery 从**另一条连接**杀掉超时的语句。KILL 不支持占位符,connID 是
// 从 CONNECTION_ID() 读出来的 uint64,直接格式化没有注入面。
func (r *Runner) killQuery(connID uint64) {
	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(killCtx, fmt.Sprintf("KILL QUERY %d", connID)); err != nil {
		r.opts.logf("KILL QUERY %d failed: %v(服务端可能仍在执行该 DDL,需人工确认)", connID, err)
		return
	}
	r.opts.logf("KILL QUERY %d sent: 语句超过 %s 硬超时", connID, r.opts.StatementTimeout)
}

// collapse 把多行 DDL 压成一行,方便进日志。
func collapse(stmt string) string {
	return strings.Join(strings.Fields(stmt), " ")
}
