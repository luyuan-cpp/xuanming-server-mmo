package schemamigrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// advisoryLockPrefix + 库名构成 GET_LOCK 的锁名,与 go/db/internal/migrate 相同:
// 同一个库上本包与 go/db 的 cmd/migrate 也互斥。MySQL 的 GET_LOCK 名字上限 64 字节。
const advisoryLockPrefix = "mmorpg_db_migrate:"

const maxAdvisoryLockNameLen = 64

// MySQL 两个会话锁等待变量的取值上限。
const (
	maxLockWaitTimeoutSeconds       = 31536000
	maxInnodbLockWaitTimeoutSeconds = 1073741824
)

const (
	// lockReleaseTimeout 是 RELEASE_LOCK 的独立超时:调用方 ctx 已取消时锁也要放掉。
	lockReleaseTimeout = 5 * time.Second
	// killQueryTimeout 是旁路 KILL QUERY 自身的超时。
	killQueryTimeout = 10 * time.Second
)

// ledgerDDL 是台账建表语句,与 go/db/internal/migrate/runner.go 的 ensureLedger **逐字一致**,
// 方便 go/db 日后直接换用本包。改动任何一边都要同步另一边(runner_test.go 的 TestLedgerDDLMatchesGoDB 钉住文本)。
var ledgerDDL = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
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

// appliedMigration 是台账里的一行。时间列扫进字符串:不依赖 DSN 是否开 parseTime
// (database/sql 会把 time.Time 与 []byte 都转成字符串),本包只用它们打日志。
type appliedMigration struct {
	Version    int64
	Name       string
	Checksum   string
	Dirty      bool
	StartedAt  string
	FinishedAt sql.NullString
}

// session 是一次 Plan / Up 独占的那条连接。
//
// GET_LOCK、SET SESSION 都是会话级的,DDL 必须和它们在同一条连接上才受保护,
// 所以全部 DDL 与台账写入都经 conn 执行;只有超时后的 KILL QUERY 走连接池里的另一条连接。
type session struct {
	db     *sql.DB
	conn   *sql.Conn
	connID uint64
	opts   Options
}

// openSession 取连接 → 库名断言 → 读 CONNECTION_ID → 收紧会话锁等待。
func openSession(ctx context.Context, db *sql.DB, opts Options) (*session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("schemamigrate: 连接库 %q 失败(库不存在时先按 deploy/mysql-init/00_init_zone_dbs.sql 建库,本包不建库): %w",
			opts.Database, err)
	}
	s := &session{db: db, conn: conn, opts: opts}
	if err := s.prepare(ctx); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) prepare(ctx context.Context) error {
	// 库名断言放在任何写动作之前:DSN 指错库时连锁都不拿、台账都不建。
	var current sql.NullString
	if err := s.conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		return fmt.Errorf("schemamigrate: 读取库 %q 连接的 DATABASE() 失败: %w", s.opts.Database, err)
	}
	if !current.Valid || current.String == "" {
		return fmt.Errorf("%w: 连接未选中任何库,期望 %q(DSN 必须带库名)", ErrDatabaseMismatch, s.opts.Database)
	}
	if current.String != s.opts.Database {
		return fmt.Errorf("%w: 连接实际选中 %q,期望 %q", ErrDatabaseMismatch, current.String, s.opts.Database)
	}

	if err := s.conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&s.connID); err != nil {
		return fmt.Errorf("schemamigrate: 读取库 %q 迁移连接的 CONNECTION_ID() 失败: %w", s.opts.Database, err)
	}

	// 这是「DDL 撞上长事务时快速失败」的开关:默认 lock_wait_timeout 是一年,一条 ALTER 排在
	// 长事务后面会把该表的 MDL 队列堵死,后面所有读写一起挂住。
	lockWait := ceilSeconds(s.opts.SessionLockWait, maxLockWaitTimeoutSeconds)
	innodbWait := ceilSeconds(s.opts.SessionLockWait, maxInnodbLockWaitTimeoutSeconds)
	stmt := fmt.Sprintf("SET SESSION lock_wait_timeout = %d, innodb_lock_wait_timeout = %d", lockWait, innodbWait)
	if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("schemamigrate: 设置库 %q 迁移会话的锁等待超时失败: %w", s.opts.Database, err)
	}
	s.opts.logf("schemamigrate 会话: database=%s connection_id=%d lock_wait_timeout=%ds innodb_lock_wait_timeout=%ds statement_timeout=%s",
		s.opts.Database, s.connID, lockWait, innodbWait, s.opts.StatementTimeout)
	return nil
}

// close 归还连接。连接断开时服务端会自动释放 GET_LOCK 与会话变量。
func (s *session) close() {
	if err := s.conn.Close(); err != nil {
		s.opts.logf("schemamigrate: 关闭库 %s 的迁移连接失败: %v", s.opts.Database, err)
	}
}

// ceilSeconds 把时长向上取整成秒,夹在 [1, max]。
func ceilSeconds(d time.Duration, max int64) int64 {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	if secs > max {
		return max
	}
	return secs
}

// advisoryLockName 返回库对应的迁移锁名。
//
// 锁名带库名是刻意的:不同库互不阻塞,同一个库的多个副本 / 多次部署必须串行。
// 超过 64 字节时截断,与 go/db 相同(保证两个工具对同一个库算出同一把锁)。
func advisoryLockName(database string) string {
	name := advisoryLockPrefix + database
	if len(name) > maxAdvisoryLockNameLen {
		name = name[:maxAdvisoryLockNameLen]
	}
	return name
}

// acquireLock 在会话连接上取跨实例互斥锁,返回释放函数。
func (s *session) acquireLock(ctx context.Context) (func(), error) {
	name := advisoryLockName(s.opts.Database)
	wait := ceilSeconds(s.opts.AdvisoryLockWait, maxLockWaitTimeoutSeconds)
	var got sql.NullInt64
	if err := s.conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, wait).Scan(&got); err != nil {
		return nil, fmt.Errorf("schemamigrate: 获取库 %q 的迁移锁 %q 失败: %w", s.opts.Database, name, err)
	}
	if !got.Valid {
		// NULL 表示服务端出错(如被 KILL),不是「别人持有」,不按可重试的锁忙处理。
		return nil, fmt.Errorf("schemamigrate: 获取库 %q 的迁移锁 %q 时 GET_LOCK 返回 NULL(服务端出错或会话被 KILL)", s.opts.Database, name)
	}
	if got.Int64 != 1 {
		return nil, fmt.Errorf("%w: 库 %q 锁 %q 等待 %ds 后仍被持有", ErrLockBusy, s.opts.Database, name, wait)
	}
	s.opts.logf("schemamigrate 已持有迁移锁: %s", name)

	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), lockReleaseTimeout)
		defer cancel()
		var released sql.NullInt64
		if err := s.conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil {
			s.opts.logf("schemamigrate: 释放迁移锁 %q 失败(连接断开时服务端会自动释放): %v", name, err)
		}
	}, nil
}

// loadLedger 读台账。
//
// apply=true(Up):先 CREATE TABLE IF NOT EXISTS 台账再读。
// apply=false(Plan):不建表;台账不存在就等价于「一条都没应用过」,返回空集。
func (s *session) loadLedger(ctx context.Context, apply bool) (map[int64]appliedMigration, error) {
	if apply {
		if err := s.exec(ctx, ledgerDDL); err != nil {
			return nil, fmt.Errorf("schemamigrate: 在库 %q 建台账表 %s 失败: %w", s.opts.Database, SchemaMigrationsTable, err)
		}
	} else {
		var count int64
		err := s.conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
			s.opts.Database, SchemaMigrationsTable).Scan(&count)
		if err != nil {
			return nil, fmt.Errorf("schemamigrate: 探测库 %q 的台账表 %s 失败: %w", s.opts.Database, SchemaMigrationsTable, err)
		}
		if count == 0 {
			return map[int64]appliedMigration{}, nil
		}
	}

	rows, err := s.conn.QueryContext(ctx, fmt.Sprintf(
		"SELECT version, name, checksum, dirty, started_at, finished_at FROM %s ORDER BY version",
		quoteIdent(SchemaMigrationsTable)))
	if err != nil {
		return nil, fmt.Errorf("schemamigrate: 读取库 %q 的台账失败: %w", s.opts.Database, err)
	}
	applied := make(map[int64]appliedMigration)
	for rows.Next() {
		var a appliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.Dirty, &a.StartedAt, &a.FinishedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("schemamigrate: 解析库 %q 的台账行失败: %w", s.opts.Database, err)
		}
		applied[a.Version] = a
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("schemamigrate: 遍历库 %q 的台账失败: %w", s.opts.Database, err)
	}
	return applied, nil
}

// apply 执行一条迁移:先写 dirty=1 台账行,逐条执行,全部成功后改 dirty=0。
// 返回真正执行成功的语句;中途失败时台账保留 dirty=1。
func (s *session) apply(ctx context.Context, m migration, applied map[int64]appliedMigration) ([]string, error) {
	if m.Empty() {
		return nil, nil
	}
	if m.Version <= 0 {
		return nil, fmt.Errorf("schemamigrate: 内部错误:迁移 %s 未分配版本号", m.Name)
	}
	if prev, ok := applied[m.Version]; ok {
		return nil, fmt.Errorf("schemamigrate: 台账 version=%d 已被 %s 占用,拒绝覆盖", m.Version, prev.Name)
	}

	s.opts.logf("schemamigrate 迁移开始: version=%d name=%s statements=%d", m.Version, m.Name, len(m.Statements))
	claim := fmt.Sprintf("INSERT INTO %s (version, name, checksum, dirty, started_at) VALUES (?, ?, ?, 1, NOW())",
		quoteIdent(SchemaMigrationsTable))
	if err := s.exec(ctx, claim, m.Version, m.Name, m.Checksum); err != nil {
		return nil, fmt.Errorf("schemamigrate: 在库 %q 写入台账 version=%d 失败: %w", s.opts.Database, m.Version, err)
	}

	executed := make([]string, 0, len(m.Statements))
	for _, stmt := range m.Statements {
		s.opts.logf("  version=%d 执行: %s", m.Version, collapse(stmt))
		if err := s.exec(ctx, stmt); err != nil {
			// 刻意保留 dirty=1:下一次 Plan / Up 直接拒绝,逼人工核对,而不是在未知结构上继续叠加 DDL。
			return executed, fmt.Errorf("schemamigrate: 库 %q 迁移 version=%d name=%s 失败,台账保留 dirty=1;人工核对已生效的语句后修正台账再重跑: %w",
				s.opts.Database, m.Version, m.Name, err)
		}
		executed = append(executed, stmt)
	}

	finish := fmt.Sprintf("UPDATE %s SET dirty = 0, finished_at = NOW() WHERE version = ?", quoteIdent(SchemaMigrationsTable))
	if err := s.exec(ctx, finish, m.Version); err != nil {
		return executed, fmt.Errorf("schemamigrate: 库 %q 迁移 version=%d 的语句已全部执行,但清除 dirty 失败(台账仍为 dirty=1,人工确认后 UPDATE): %w",
			s.opts.Database, m.Version, err)
	}
	applied[m.Version] = appliedMigration{Version: m.Version, Name: m.Name, Checksum: m.Checksum}
	s.opts.logf("schemamigrate 迁移完成: version=%d name=%s", m.Version, m.Name)
	return executed, nil
}

// exec 在会话连接上带硬超时执行一条语句。
//
// 三层超时缺一不可:
//   - 会话级 lock_wait_timeout:拿不到 MDL 时秒级失败;
//   - context 硬超时:拿到锁但语句本身跑太久时,客户端不再无限等;
//   - 旁路 KILL QUERY:context 到期只让客户端放手,**服务端还在跑**。不 KILL 的话工具已经退出
//     而 ALTER 仍在锁表。调用方 ctx 被取消时同理,所以不论到期原因都 KILL。
//
// KILL 由两处触发,atomic 保证同一条语句最多发一次:
//   - 守卫 goroutine:ctx 一到期就杀,不看语句是否已返回。它兜的是「驱动不响应 ctx、
//     ExecContext 一直阻塞」的情况——服务端被 KILL 后回错误包,阻塞的读才会返回。
//   - 返回后同步补杀:语句带错误返回且 ctx 已到期时一定再杀一次。不能拿「ExecContext 已返回」
//     推断「服务端已跑完」:go-sql-driver 在 ctx 到期时由驱动自己的 watcher 关 socket,
//     ExecContext 立刻返回而 DDL 仍在服务端执行、仍持 MDL;守卫 goroutine 与驱动 watcher
//     被同一次 ctx 关闭唤醒,调度顺序不确定,只靠守卫会漏杀。
//
// 语句成功返回(err==nil)时不补杀。剩下的窄窗口是守卫在语句恰好成功的同一刻到期并发出 KILL:
// 目标连接此时空闲,KILL 最坏打断这条连接上的下一条语句,表现为报错 / 台账留 dirty(fail-closed)。
func (s *session) exec(ctx context.Context, stmt string, args ...any) error {
	execCtx, cancel := context.WithTimeout(ctx, s.opts.StatementTimeout)
	defer cancel()

	var killed atomic.Bool
	kill := func() {
		if killed.CompareAndSwap(false, true) {
			s.killQuery(execCtx.Err())
		}
	}

	done := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-done:
		case <-execCtx.Done():
			kill()
		}
	}()

	_, err := s.conn.ExecContext(execCtx, stmt, args...)
	close(done)
	<-watcherDone

	if err != nil {
		if ctxErr := execCtx.Err(); ctxErr != nil {
			// 同步补杀:守卫可能因为 done 与 ctx 同时就绪而选中 done,这里保证一定发出过 KILL。
			kill()
			return fmt.Errorf("语句未在 %s 硬超时内完成或调用方已取消(%v),已尝试旁路 KILL QUERY %d: %s: %w",
				s.opts.StatementTimeout, ctxErr, s.connID, collapse(stmt), err)
		}
		return fmt.Errorf("执行 %s: %w", collapse(stmt), err)
	}
	return nil
}

// killQuery 从**另一条连接**杀掉会话连接上正在执行的语句。
// KILL 不支持占位符;connID 是从 CONNECTION_ID() 读出的 uint64,直接格式化没有注入面。
func (s *session) killQuery(reason error) {
	killCtx, cancel := context.WithTimeout(context.Background(), killQueryTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(killCtx, fmt.Sprintf("KILL QUERY %d", s.connID)); err != nil {
		s.opts.logf("schemamigrate: KILL QUERY %d 失败(服务端可能仍在执行该语句,需人工确认): %v", s.connID, err)
		return
	}
	s.opts.logf("schemamigrate: 已发送 KILL QUERY %d(原因: %v)", s.connID, reason)
}

// sortedApplied 按版本号升序返回台账行。
func sortedApplied(applied map[int64]appliedMigration) []appliedMigration {
	out := make([]appliedMigration, 0, len(applied))
	for _, a := range applied {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// refuseDirty 台账里任何一行 dirty 就拒绝继续。
func refuseDirty(applied map[int64]appliedMigration) error {
	for _, a := range sortedApplied(applied) {
		if a.Dirty {
			return fmt.Errorf("%w: version=%d name=%s started_at=%s;先人工核对该版本的语句是否已生效,确认后再修正或删除这一行台账",
				ErrDirty, a.Version, a.Name, a.StartedAt)
		}
	}
	return nil
}

// baselineRecorded 报告基线是否已记账。只看 version=1 是否存在,不比 checksum:
// 表清单增删后 checksum 必然变化,新增的表由漂移补建;go/db 建的台账 checksum 口径也不同。
func baselineRecorded(baseline migration, applied map[int64]appliedMigration) bool {
	_, ok := applied[baseline.Version]
	return ok
}

// findChecksum 找台账里 checksum 相同的一行(按版本号升序取第一行)。
func findChecksum(applied map[int64]appliedMigration, sum string) (appliedMigration, bool) {
	for _, a := range sortedApplied(applied) {
		if a.Checksum == sum {
			return a, true
		}
	}
	return appliedMigration{}, false
}

// nextVersion 返回台账最大版本号 +1。
func nextVersion(applied map[int64]appliedMigration) int64 {
	var max int64
	for v := range applied {
		if v > max {
			max = v
		}
	}
	return max + 1
}
