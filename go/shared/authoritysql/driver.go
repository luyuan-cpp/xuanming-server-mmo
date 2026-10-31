package authoritysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"sync"

	"github.com/go-sql-driver/mysql"
)

// 本文件为隔离候选：未编译、未运行、未接入pool.go。
// 生产writerGate必须使用正规生成的catalog proto；本批不造同义持久化struct。
var (
	ErrOutcomeUnknown    = errors.New("权威SQL执行结果未定，禁止自动重放")
	errGuardClosed       = errors.New("权威SQL连接已关闭或隔离")
	errGuardBusy         = errors.New("权威SQL连接仍持有未关闭的Rows")
	errGuardTxDone       = errors.New("权威SQL事务已结束")
	errConnectorOnly     = errors.New("权威SQL仅允许通过具名connector建立物理连接")
	errUnsupportedDriver = errors.New("权威SQL驱动不支持要求的能力")
)

// unknownError故意没有Unwrap/As；底层ErrBadConn及其Join链均不可达。
// 不保留底层错误文本，避免SQL、参数或连接串进入错误和日志。
type unknownError struct {
	phase    string
	canceled bool
	deadline bool
}

func (e *unknownError) Error() string {
	return "权威SQL结果未定（" + e.phase + "），禁止重放"
}
func (e *unknownError) Is(target error) bool {
	// 取消/超时仅留诊断flags；不能让泛context重试分支把UNKNOWN降类。
	return target == ErrOutcomeUnknown
}

func unknown(phase string, cause error) error {
	return &unknownError{phase: phase, canceled: errors.Is(cause, context.Canceled), deadline: errors.Is(cause, context.DeadlineExceeded)}
}

// writerGate只在本包内部接触raw；没有导出getter，也没有global连接表。
// beforeBegin在同物理连接上强读epoch；enter在BEGIN后首锁own ACTIVE slot，
// CURRENT核epoch/状态，再核meta/registry。实现缺失必须拒绝装配。
type writerGate interface {
	beforeBegin(context.Context, driver.Conn) (gateAttempt, error)
}
type gateAttempt interface {
	enter(context.Context, driver.Conn) error
}

// sessionPolicy负责可信的固定SET和CURRENT_USER/部署身份校验；不得读环境、
// 造UUID、注册或执行DDL。raw ResetSession之后也必须重新执行这些检查。
type sessionPolicy interface {
	pin(context.Context, driver.Conn) error
}

type serverOutcome uint8

const (
	outcomeUnknown serverOutcome = iota
	outcomeStatementRejected
	outcomeTransactionAborted
)

// 分类属于驱动协议，不是领域fault map。没有明确证据的类别不可升级为确定。
type serverOutcomePolicy interface{ classify(error) serverOutcome }
type mysqlStatementPolicy struct{}

func (mysqlStatementPolicy) classify(err error) serverOutcome {
	// 首个候选仅支持直接的确定1062；不通过As拆开混合transport/Join错误。
	// 1213与SQLSTATE40等Tx-abort类别待驱动/实际TiDB证据，不在此冒充已支持。
	if server, ok := err.(*mysql.MySQLError); ok && server.Number == 1062 {
		return outcomeStatementRejected
	}
	return outcomeUnknown
}

type guardedConnector struct {
	permission Permission
	inner      driver.Connector
	gate       writerGate
	session    sessionPolicy
	outcomes   serverOutcomePolicy
}

func newGuardedConnector(inner driver.Connector, gate writerGate, session sessionPolicy, outcomes serverOutcomePolicy, permission Permission) (driver.Connector, error) {
	if err := permission.validate(); err != nil {
		return nil, err
	}
	if inner == nil || gate == nil || session == nil || outcomes == nil {
		return nil, errUnsupportedDriver
	}
	return &guardedConnector{inner: inner, gate: gate, session: session, outcomes: outcomes, permission: permission}, nil
}

type opaqueDriver struct{}

func (opaqueDriver) Open(string) (driver.Conn, error) { return nil, errConnectorOnly }
func (*guardedConnector) Driver() driver.Driver       { return opaqueDriver{} }
func (c *guardedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, errors.New("权威SQL物理连接建立失败")
	}
	if raw == nil {
		return nil, errUnsupportedDriver
	}
	if err = c.session.pin(ctx, raw); err != nil {
		_ = raw.Close()
		// 档位不符是部署错误,文本不含 DSN,原样带出「连到了从库 / TiDB」,否则运维只看得到下面这句笼统的话。
		if errors.Is(err, ErrBackendMismatch) {
			return nil, err
		}
		return nil, errors.New("权威SQL物理会话或boot身份核验失败")
	}
	return &guardedConn{raw: raw, gate: c.gate, session: c.session, outcomes: c.outcomes, permission: c.permission}, nil
}

// mu只属于一个物理连接，串行化raw调用及Tx/Rows/Close状态迁移。
// 底层没有Context接口时仅保证调用前取消检查；网络上限仍由原socket预算限制。
// 不另启goroutine制造“返回了但SQL还在跑”的伪取消。
type guardedConn struct {
	permission  Permission
	mu          sync.Mutex
	raw         driver.Conn
	gate        writerGate
	session     sessionPolicy
	outcomes    serverOutcomePolicy
	tx          *guardedTx
	rows        *guardedRows
	closed      bool
	quarantined bool
}

type guardedTx struct {
	owner     *guardedConn
	raw       driver.Tx
	ctx       context.Context
	automatic bool
	done      bool
	failure   error
}

func (c *guardedConn) usableLocked() error {
	if c.closed || c.quarantined {
		return errGuardClosed
	}
	if c.rows != nil {
		return errGuardBusy
	}
	return nil
}

func (c *guardedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *guardedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usableLocked(); err != nil {
		return nil, err
	}
	if c.tx != nil {
		return nil, errGuardBusy
	}
	return c.beginLocked(ctx, opts, false)
}

func (c *guardedConn) beginLocked(ctx context.Context, opts driver.TxOptions, automatic bool) (*guardedTx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	begin, hasContext := c.raw.(driver.ConnBeginTx)
	if !hasContext && (opts.Isolation != 0 || opts.ReadOnly) {
		return nil, errUnsupportedDriver
	}
	// 必须在BEGIN之前；不能挪入RR快照再读。
	attempt, err := c.gate.beforeBegin(ctx, c.raw)
	if err != nil {
		return nil, c.preBusinessErrorLocked(err)
	}
	if attempt == nil {
		return nil, errUnsupportedDriver
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var rawTx driver.Tx
	if hasContext {
		rawTx, err = begin.BeginTx(ctx, opts)
	} else {
		rawTx, err = c.raw.Begin()
	}
	if err != nil {
		return nil, c.preBusinessErrorLocked(err)
	}
	if rawTx == nil {
		c.quarantined = true
		return nil, unknown("BEGIN缺失事务", nil)
	}
	tx := &guardedTx{owner: c, raw: rawTx, ctx: ctx, automatic: automatic}
	c.tx = tx
	if err = attempt.enter(ctx, c.raw); err != nil {
		if rollbackErr := c.endLocked(tx, false); rollbackErr != nil {
			return nil, rollbackErr
		}
		return nil, c.preBusinessErrorLocked(err)
	}
	return tx, nil
}

// 确定的语句拒绝保留显式Tx及此前业务/slot，交caller继续处理；auto-owned则回滚。
// 确定Tx-abort结束Tx，防下一条SQL脱离slot落入autocommit；未知结果隔离。
// 修正记录：初稿“所有错误终止Tx”破坏既有1062转读语义，已撤销该设计。
func (c *guardedConn) failLocked(tx *guardedTx, cause error) error {
	outcome := outcomeUnknown
	if !errors.Is(cause, driver.ErrBadConn) && !errors.Is(cause, ErrOutcomeUnknown) && c.outcomes != nil {
		outcome = c.outcomes.classify(cause)
	}
	if outcome != outcomeStatementRejected && outcome != outcomeTransactionAborted {
		outcome = outcomeUnknown
	}
	if outcome == outcomeStatementRejected && tx != nil && !tx.automatic {
		return cause
	}
	result := cause
	if outcome == outcomeUnknown {
		c.quarantined = true
		result = unknown("业务发送后", cause)
	}
	if tx != nil && !tx.done {
		if rollbackErr := c.endLocked(tx, false); rollbackErr != nil {
			result = rollbackErr
		}
		tx.failure = result
	}
	return result
}

func (c *guardedConn) preBusinessErrorLocked(cause error) error {
	if errors.Is(cause, driver.ErrBadConn) {
		c.quarantined = true
		return unknown("业务发送前连接失效", cause)
	}
	return cause
}

func (c *guardedConn) endLocked(tx *guardedTx, commit bool) error {
	if tx.done {
		if tx.failure != nil {
			return tx.failure
		}
		return errGuardTxDone
	}
	if c.tx != tx {
		return errGuardTxDone
	}
	if c.rows != nil {
		return errGuardBusy
	}
	var err error
	committing := commit && tx.ctx.Err() == nil
	if committing {
		err = tx.raw.Commit()
	} else {
		err = tx.raw.Rollback()
	}
	tx.done = true
	c.tx = nil
	if err != nil {
		c.quarantined = true
		tx.failure = unknown("事务结束", err)
		return tx.failure
	}
	// 已收到成功COMMIT不能因随后取消而伪报失败；发送前已取消则确认回滚后返回取消。
	if commit && !committing {
		tx.failure = tx.ctx.Err()
		return tx.failure
	}
	return nil
}

func (tx *guardedTx) Commit() error {
	tx.owner.mu.Lock()
	defer tx.owner.mu.Unlock()
	if tx.done || tx.owner.tx != tx {
		if tx.failure != nil {
			return tx.failure
		}
		return errGuardTxDone
	}
	if tx.owner.rows != nil {
		if err := tx.owner.rows.finishLocked(false, errGuardBusy); err != nil {
			return err
		}
		return errGuardBusy
	}
	return tx.owner.endLocked(tx, true)
}
func (tx *guardedTx) Rollback() error {
	tx.owner.mu.Lock()
	defer tx.owner.mu.Unlock()
	if tx.done || tx.owner.tx != tx {
		if tx.failure != nil {
			return tx.failure
		}
		return errGuardTxDone
	}
	if tx.owner.rows != nil {
		_ = tx.owner.rows.finishLocked(false, nil)
	}
	return tx.owner.endLocked(tx, false)
}

func (c *guardedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	var result error
	if c.rows != nil {
		result = c.rows.finishLocked(false, nil)
	}
	if c.tx != nil {
		if err := c.endLocked(c.tx, false); err != nil {
			result = err
		}
	}
	c.closed = true
	if err := c.raw.Close(); err != nil {
		c.quarantined = true
		result = unknown("关闭连接", err)
	}
	return result
}

func (c *guardedConn) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usableLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p, ok := c.raw.(driver.Pinger); ok {
		return c.preBusinessErrorLocked(p.Ping(ctx))
	}
	return nil
}
func (c *guardedConn) ResetSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usableLocked(); err != nil {
		return err
	}
	if c.tx != nil {
		return errGuardBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if resetter, ok := c.raw.(driver.SessionResetter); ok {
		if err := resetter.ResetSession(ctx); err != nil {
			c.quarantined = true
			return unknown("重置会话", err)
		}
	}
	if err := c.session.pin(ctx, c.raw); err != nil {
		c.quarantined = true
		return unknown("重核强会话", err)
	}
	return nil
}
func (c *guardedConn) IsValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.quarantined || c.tx != nil || c.rows != nil {
		return false
	}
	if validator, ok := c.raw.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}
func (c *guardedConn) CheckNamedValue(v *driver.NamedValue) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if checker, ok := c.raw.(driver.NamedValueChecker); ok {
		return c.preBusinessErrorLocked(checker.CheckNamedValue(v))
	}
	return driver.ErrSkip
}

func (c *guardedConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	return c.ExecContext(context.Background(), query, named(args))
}
func (c *guardedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execLocked(ctx, query, func() (driver.Result, error) { return rawExec(ctx, c.raw, query, args) })
}
func (c *guardedConn) execLocked(ctx context.Context, query string, call func() (driver.Result, error)) (driver.Result, error) {
	if err := c.usableLocked(); err != nil {
		return nil, err
	}
	if err := validatePermissionStatement(query, c.permission.kind); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx := c.tx
	if tx != nil && tx.ctx.Err() != nil {
		if err := c.endLocked(tx, false); err != nil {
			return nil, err
		}
		return nil, tx.ctx.Err()
	}
	if tx == nil && c.permission.kind == permissionWriter {
		var err error
		tx, err = c.beginLocked(ctx, driver.TxOptions{}, true)
		if err != nil {
			return nil, err
		}
	}
	result, err := call()
	if ctx.Err() != nil {
		err = unknown("业务发送后取消", ctx.Err())
	}
	if tx != nil && tx.ctx.Err() != nil {
		err = unknown("事务期间取消", tx.ctx.Err())
	}
	if err != nil {
		return nil, c.failLocked(tx, err)
	}
	if result == nil {
		return nil, c.failLocked(tx, errUnsupportedDriver)
	}
	if tx != nil && tx.automatic {
		if err = c.endLocked(tx, true); err != nil {
			return nil, err
		}
	}
	return guardedResult{inner: result}, nil
}

func (c *guardedConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return c.QueryContext(context.Background(), query, named(args))
}
func (c *guardedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queryLocked(ctx, query, func() (driver.Rows, func() error, error) { return rawQuery(ctx, c.raw, query, args) })
}
func (c *guardedConn) queryLocked(ctx context.Context, query string, call func() (driver.Rows, func() error, error)) (driver.Rows, error) {
	if err := c.usableLocked(); err != nil {
		return nil, err
	}
	if err := validatePermissionStatement(query, c.permission.kind); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx := c.tx
	if tx != nil && tx.ctx.Err() != nil {
		if err := c.endLocked(tx, false); err != nil {
			return nil, err
		}
		return nil, tx.ctx.Err()
	}
	if tx == nil && c.permission.kind == permissionWriter {
		var err error
		tx, err = c.beginLocked(ctx, driver.TxOptions{}, true)
		if err != nil {
			return nil, err
		}
	}
	raw, closeStmt, err := call()
	if ctx.Err() != nil {
		err = unknown("Query发送后取消", ctx.Err())
	}
	if tx != nil && tx.ctx.Err() != nil {
		err = unknown("事务期间取消", tx.ctx.Err())
	}
	if err != nil {
		if raw != nil {
			if closeErr := raw.Close(); closeErr != nil {
				err = unknown("关闭失败Rows", closeErr)
			}
		}
		if closeStmt != nil {
			if closeErr := closeStmt(); closeErr != nil {
				err = unknown("关闭失败Stmt", closeErr)
			}
		}
		return nil, c.failLocked(tx, err)
	}
	if raw == nil {
		failure := unknown("Query缺失Rows", errUnsupportedDriver)
		if closeStmt != nil {
			if closeErr := closeStmt(); closeErr != nil {
				failure = unknown("关闭缺失Rows的Stmt", closeErr)
			}
		}
		return nil, c.failLocked(tx, failure)
	}
	rows := &guardedRows{owner: c, raw: raw, tx: tx, ctx: ctx, closeStmt: closeStmt}
	c.rows = rows
	return rows, nil
}

func (c *guardedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}
func (c *guardedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usableLocked(); err != nil {
		return nil, err
	}
	if err := validatePermissionStatement(query, c.permission.kind); err != nil {
		return nil, err
	}
	raw, err := rawPrepare(ctx, c.raw, query)
	if err != nil {
		if c.tx != nil {
			return nil, c.failLocked(c.tx, err)
		}
		return nil, c.preBusinessErrorLocked(err)
	}
	if raw == nil {
		return nil, errUnsupportedDriver
	}
	return &guardedStmt{owner: c, raw: raw, query: query}, nil
}

func rawPrepare(ctx context.Context, conn driver.Conn, query string) (driver.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p, ok := conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return conn.Prepare(query)
}
func rawExec(ctx context.Context, conn driver.Conn, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if exec, ok := conn.(driver.ExecerContext); ok {
		result, err := exec.ExecContext(ctx, query, args)
		if err != driver.ErrSkip {
			return result, err
		}
	} else if exec, ok := conn.(driver.Execer); ok {
		values, err := legacyValues(args)
		if err != nil {
			return nil, err
		}
		result, err := exec.Exec(query, values)
		if err != driver.ErrSkip {
			return result, err
		}
	}
	stmt, err := rawPrepare(ctx, conn, query)
	if err != nil {
		return nil, err
	}
	if stmt == nil {
		return nil, errUnsupportedDriver
	}
	result, err := rawStmtExec(ctx, stmt, args)
	if closeErr := stmt.Close(); closeErr != nil {
		err = unknown("关闭临时Stmt", closeErr)
	}
	return result, err
}
func rawQuery(ctx context.Context, conn driver.Conn, query string, args []driver.NamedValue) (driver.Rows, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if queryer, ok := conn.(driver.QueryerContext); ok {
		rows, err := queryer.QueryContext(ctx, query, args)
		if err != driver.ErrSkip {
			return rows, nil, err
		}
	} else if queryer, ok := conn.(driver.Queryer); ok {
		values, err := legacyValues(args)
		if err != nil {
			return nil, nil, err
		}
		rows, err := queryer.Query(query, values)
		if err != driver.ErrSkip {
			return rows, nil, err
		}
	}
	stmt, err := rawPrepare(ctx, conn, query)
	if err != nil {
		return nil, nil, err
	}
	if stmt == nil {
		return nil, nil, errUnsupportedDriver
	}
	rows, err := rawStmtQuery(ctx, stmt, args)
	return rows, stmt.Close, err
}
func rawStmtExec(ctx context.Context, stmt driver.Stmt, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if exec, ok := stmt.(driver.StmtExecContext); ok {
		return exec.ExecContext(ctx, args)
	}
	values, err := legacyValues(args)
	if err != nil {
		return nil, err
	}
	return stmt.Exec(values)
}
func rawStmtQuery(ctx context.Context, stmt driver.Stmt, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query, ok := stmt.(driver.StmtQueryContext); ok {
		return query.QueryContext(ctx, args)
	}
	values, err := legacyValues(args)
	if err != nil {
		return nil, err
	}
	return stmt.Query(values)
}
func named(args []driver.Value) []driver.NamedValue {
	values := make([]driver.NamedValue, len(args))
	for i, v := range args {
		values[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return values
}
func legacyValues(args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for i, v := range args {
		if v.Name != "" || v.Ordinal != i+1 {
			return nil, errUnsupportedDriver
		}
		values[i] = v.Value
	}
	return values, nil
}

type guardedStmt struct {
	owner  *guardedConn
	raw    driver.Stmt
	query  string
	closed bool
}

func (s *guardedStmt) Close() error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.owner.rows != nil {
		return errGuardBusy
	}
	s.closed = true
	if err := s.raw.Close(); err != nil {
		return s.owner.failLocked(s.owner.tx, unknown("关闭Stmt", err))
	}
	return nil
}
func (s *guardedStmt) NumInput() int {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	return s.raw.NumInput()
}
func (s *guardedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}
func (s *guardedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.closed {
		return nil, errGuardClosed
	}
	return s.owner.execLocked(ctx, s.query, func() (driver.Result, error) { return rawStmtExec(ctx, s.raw, args) })
}
func (s *guardedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}
func (s *guardedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.closed {
		return nil, errGuardClosed
	}
	return s.owner.queryLocked(ctx, s.query, func() (driver.Rows, func() error, error) {
		rows, err := rawStmtQuery(ctx, s.raw, args)
		return rows, nil, err
	})
}
func (s *guardedStmt) CheckNamedValue(v *driver.NamedValue) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if checker, ok := s.raw.(driver.NamedValueChecker); ok {
		return s.owner.preBusinessErrorLocked(checker.CheckNamedValue(v))
	}
	if checker, ok := s.owner.raw.(driver.NamedValueChecker); ok {
		return s.owner.preBusinessErrorLocked(checker.CheckNamedValue(v))
	}
	return driver.ErrSkip
}
func (s *guardedStmt) ColumnConverter(index int) driver.ValueConverter {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	inner := driver.ValueConverter(driver.DefaultParameterConverter)
	if converter, ok := s.raw.(driver.ColumnConverter); ok {
		inner = converter.ColumnConverter(index)
	}
	return guardedConverter{owner: s.owner, inner: inner}
}

type guardedConverter struct {
	owner *guardedConn
	inner driver.ValueConverter
}

func (c guardedConverter) ConvertValue(value any) (driver.Value, error) {
	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	if c.owner.closed || c.owner.quarantined {
		return nil, errGuardClosed
	}
	converted, err := c.inner.ConvertValue(value)
	return converted, c.owner.preBusinessErrorLocked(err)
}

type guardedResult struct{ inner driver.Result }

func (r guardedResult) LastInsertId() (int64, error) {
	value, err := r.inner.LastInsertId()
	if errors.Is(err, driver.ErrBadConn) {
		return 0, unknown("读取结果元数据", err)
	}
	return value, err
}
func (r guardedResult) RowsAffected() (int64, error) {
	value, err := r.inner.RowsAffected()
	if errors.Is(err, driver.ErrBadConn) {
		return 0, unknown("读取结果元数据", err)
	}
	return value, err
}

// Rows拥有自动事务直到全部结果集完成或Close已真实排完底层结果。
// 任何Close/Next/cancel错误都不能吞掉；显式Tx的成功Rows不提前提交。
type guardedRows struct {
	owner     *guardedConn
	raw       driver.Rows
	tx        *guardedTx
	ctx       context.Context
	closeStmt func() error
	done      bool
	terminal  error
}

func (r *guardedRows) Columns() []string {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	return r.raw.Columns()
}
func (r *guardedRows) Close() error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	return r.finishLocked(true, nil)
}
func (r *guardedRows) Next(values []driver.Value) error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.done {
		if r.terminal != nil {
			return r.terminal
		}
		return io.EOF
	}
	err := r.raw.Next(values)
	if err == nil {
		return nil
	}
	if err == io.EOF {
		if next, ok := r.raw.(driver.RowsNextResultSet); ok && next.HasNextResultSet() {
			return io.EOF
		}
		if err = r.finishLocked(true, nil); err != nil {
			return err
		}
		return io.EOF
	}
	return r.finishLocked(false, err)
}
func (r *guardedRows) HasNextResultSet() bool {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.done {
		return false
	}
	if next, ok := r.raw.(driver.RowsNextResultSet); ok {
		return next.HasNextResultSet()
	}
	return false
}
func (r *guardedRows) NextResultSet() error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.done {
		if r.terminal != nil {
			return r.terminal
		}
		return io.EOF
	}
	if next, ok := r.raw.(driver.RowsNextResultSet); ok {
		err := next.NextResultSet()
		if err == nil {
			return nil
		}
		if err != io.EOF {
			return r.finishLocked(false, err)
		}
	}
	if err := r.finishLocked(true, nil); err != nil {
		return err
	}
	return io.EOF
}
func (r *guardedRows) finishLocked(commit bool, cause error) error {
	if r.done {
		return r.terminal
	}
	r.done = true
	// 清理失败不能被此前确定1062遮住；任何额外失败都升级为opaque UNKNOWN。
	if err := r.raw.Close(); err != nil {
		cause = unknown("关闭Rows", err)
	}
	if r.closeStmt != nil {
		if err := r.closeStmt(); err != nil {
			cause = unknown("关闭Rows临时Stmt", err)
		}
	}
	r.owner.rows = nil
	if r.ctx.Err() != nil {
		cause = unknown("Rows期间取消", r.ctx.Err())
	}
	if r.tx != nil && r.tx.ctx.Err() != nil {
		cause = unknown("事务期间取消", r.tx.ctx.Err())
	}
	if cause != nil {
		r.terminal = r.owner.failLocked(r.tx, cause)
		return r.terminal
	}
	if r.tx != nil && r.tx.automatic {
		r.terminal = r.owner.endLocked(r.tx, commit)
	}
	return r.terminal
}
func (r *guardedRows) ColumnTypeScanType(index int) reflect.Type {
	if rows, ok := r.raw.(driver.RowsColumnTypeScanType); ok {
		return rows.ColumnTypeScanType(index)
	}
	return reflect.TypeOf((*any)(nil)).Elem()
}
func (r *guardedRows) ColumnTypeDatabaseTypeName(index int) string {
	if rows, ok := r.raw.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return rows.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}
func (r *guardedRows) ColumnTypeLength(index int) (int64, bool) {
	if rows, ok := r.raw.(driver.RowsColumnTypeLength); ok {
		return rows.ColumnTypeLength(index)
	}
	return 0, false
}
func (r *guardedRows) ColumnTypeNullable(index int) (bool, bool) {
	if rows, ok := r.raw.(driver.RowsColumnTypeNullable); ok {
		return rows.ColumnTypeNullable(index)
	}
	return false, false
}
func (r *guardedRows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if rows, ok := r.raw.(driver.RowsColumnTypePrecisionScale); ok {
		return rows.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

var (
	_ driver.Connector                      = (*guardedConnector)(nil)
	_ driver.Driver                         = opaqueDriver{}
	_ driver.Conn                           = (*guardedConn)(nil)
	_ driver.ConnBeginTx                    = (*guardedConn)(nil)
	_ driver.ConnPrepareContext             = (*guardedConn)(nil)
	_ driver.Execer                         = (*guardedConn)(nil)
	_ driver.ExecerContext                  = (*guardedConn)(nil)
	_ driver.Queryer                        = (*guardedConn)(nil)
	_ driver.QueryerContext                 = (*guardedConn)(nil)
	_ driver.Pinger                         = (*guardedConn)(nil)
	_ driver.SessionResetter                = (*guardedConn)(nil)
	_ driver.Validator                      = (*guardedConn)(nil)
	_ driver.NamedValueChecker              = (*guardedConn)(nil)
	_ driver.Tx                             = (*guardedTx)(nil)
	_ driver.Stmt                           = (*guardedStmt)(nil)
	_ driver.StmtExecContext                = (*guardedStmt)(nil)
	_ driver.StmtQueryContext               = (*guardedStmt)(nil)
	_ driver.NamedValueChecker              = (*guardedStmt)(nil)
	_ driver.ColumnConverter                = (*guardedStmt)(nil)
	_ driver.Rows                           = (*guardedRows)(nil)
	_ driver.RowsNextResultSet              = (*guardedRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*guardedRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*guardedRows)(nil)
	_ driver.RowsColumnTypeLength           = (*guardedRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*guardedRows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*guardedRows)(nil)
)
