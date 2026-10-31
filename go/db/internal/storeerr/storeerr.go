// Package storeerr 给 SQL 调用点返回的错误打「存储侧故障」的来源标记(L-04)。
//
// 设计稿:docs/design/db-task-store-unavailable-deferral.md §3.2。
//
// 为什么只在 SQL 调用点分类:Redis 侧的错误同样可能是 net.Error / io.EOF / 超时。如果在重试豁免、
// 延后计数这些汇总处对任意错误跑类型判断,一次 Redis 抖动就会被算成 MySQL 不可用。所以分类只发生在
// 执行 SQL 的那一行(handler、落点库探活),命中就包成 *Error;其它地方只认这个标记,不再看底层类型。
package storeerr

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"

	"github.com/go-sql-driver/mysql"
)

// Kind 是存储侧故障的类别。零值 KindNone 表示「不是存储侧故障」(数据、表结构、配置一类的问题)。
type Kind uint8

const (
	KindNone Kind = iota
	// KindConnRefused 服务端在应答,只是拒绝新连接(连接数满)。库是活的,池里已建立的连接照常可用,
	// 所以不触发库级冷却,只延后这一条任务。
	KindConnRefused
	// KindConnLost 连接断开或连不上:拨号失败、连接被重置、读写超时、服务端正在关闭。
	// 可能是整库不可达,由注册表做一次探活确认后才进入库级冷却。
	KindConnLost
	// KindReadOnly 库处于只读(主从切换期间的旧主)。
	KindReadOnly
	// KindBusy 锁等待超时、死锁、TiDB 写冲突:只是这一条语句的事,重试即可。
	KindBusy
)

// MySQL / TiDB 错误号。含义来自 MySQL 与 TiDB 的官方错误码表;落码时没有在真库上逐个触发核对,
// 见设计稿 §11「未核实」。
const (
	errConCount              uint16 = 1040 // Too many connections:全局 max_connections 满
	errServerShutdown        uint16 = 1053 // Server shutdown in progress
	errTooManyUserConns      uint16 = 1203 // 全局 max_user_connections 满
	errLockWaitTimeout       uint16 = 1205 // 锁等待超时(含会话级 lock_wait_timeout 触发的元数据锁等待)
	errLockDeadlock          uint16 = 1213 // 死锁,事务被回滚
	errUserLimitReached      uint16 = 1226 // 账号级资源上限(MAX_USER_CONNECTIONS 等);当前部署没有按账号限连接,为将来预留
	errOptionPreventsStmt    uint16 = 1290 // --read-only / super_read_only 下的写
	errReadOnlyMode          uint16 = 1836 // Running in read-only mode
	errTiDBForUpdateConflict uint16 = 8002 // TiDB 乐观事务里 SELECT FOR UPDATE 写冲突
	errTiDBWriteConflict     uint16 = 9007 // TiDB 乐观事务写冲突
)

// 1129(来源因 max_connect_errors 被拉黑)不纳入:K8s 上的 MySQL 开了 skip-name-resolve
// (deploy/k8s/manifests/infra/mysql.yaml),不走主机缓存,不会出现;TiDB 也没有这个机制。

// Error 是带来源标记的存储侧错误。Unwrap 返回原始错误,errors.Is / errors.As 照常穿透。
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Classify 判定一个 SQL 调用返回的错误属于哪一类存储侧故障。只应用在 SQL 调用点(见包注释)。
func Classify(err error) Kind {
	if err == nil {
		return KindNone
	}
	// 上下文被取消(分区被收回、进程退出)不是存储故障,走调用方原有的路径。
	if errors.Is(err, context.Canceled) {
		return KindNone
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case errConCount, errTooManyUserConns, errUserLimitReached:
			return KindConnRefused
		case errServerShutdown:
			return KindConnLost
		case errOptionPreventsStmt, errReadOnlyMode:
			return KindReadOnly
		case errLockWaitTimeout, errLockDeadlock, errTiDBForUpdateConflict, errTiDBWriteConflict:
			return KindBusy
		}
		// 其它服务端错误(1062 主键冲突、1406 数据过长、1146 缺表……)是任务或表结构的问题。
		return KindNone
	}
	switch {
	case errors.Is(err, driver.ErrBadConn),
		errors.Is(err, mysql.ErrInvalidConn),
		errors.Is(err, sql.ErrConnDone),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, context.DeadlineExceeded):
		return KindConnLost
	}
	// 拨号被拒、拨号超时、连接重置、读写超时。context.DeadlineExceeded 本身也实现了 net.Error,上面已先判。
	var ne net.Error
	if errors.As(err, &ne) {
		return KindConnLost
	}
	return KindNone
}

// Tag 对 SQL 调用返回的错误分类,命中就包成 *Error;nil 原样返回,不是存储侧故障的原样返回。
func Tag(err error) error {
	if err == nil {
		return nil
	}
	var tagged *Error
	if errors.As(err, &tagged) {
		return err
	}
	if kind := Classify(err); kind != KindNone {
		return &Error{Kind: kind, Err: err}
	}
	return err
}

// KindOf 返回错误链上的来源标记;没有标记(包括 Redis 侧的网络错误)返回 KindNone。
func KindOf(err error) Kind {
	var tagged *Error
	if errors.As(err, &tagged) {
		return tagged.Kind
	}
	return KindNone
}
