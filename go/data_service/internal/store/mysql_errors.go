package store

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

// InnoDB 锁冲突的两个错误号。本包里**只在这里**写数字:isRetryableMySQL(1213 + 1205)与
// isDeadlockMySQL(只认 1213,见 snapshot_store.go)都引用这两个常量,不再各写一份字面量。
const (
	// mysqlErrDeadlock 1213 ER_LOCK_DEADLOCK:InnoDB 检测到锁等待成环,回滚了本方事务(自动提交 = 本条语句)。
	mysqlErrDeadlock = 1213
	// mysqlErrLockWaitTimeout 1205 ER_LOCK_WAIT_TIMEOUT:等锁超过 innodb_lock_wait_timeout(默认只回滚本条语句)。
	mysqlErrLockWaitTimeout = 1205
)

// isRetryableMySQL 只把死锁(1213)和锁等待超时(1205)视为可重试。
func isRetryableMySQL(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDeadlock || me.Number == mysqlErrLockWaitTimeout
}
